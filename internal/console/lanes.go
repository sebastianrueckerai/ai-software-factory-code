package console

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// laneTitles name the lanes as columns and buttons.
var laneTitles = map[string]string{policy.LaneBacklog: "Backlog", policy.LanePlan: "Plan", policy.LaneReady: "Ready"}

// laneLabels are the labels that put an issue into a lane (section 1.1).
var laneLabels = map[string]string{policy.LaneBacklog: factoryd.LabelBacklog, policy.LanePlan: factoryd.LabelPlan,
	policy.LaneReady: factoryd.LabelAgent}

type mayMove func(ctx context.Context, repository, user, movedBy string) (bool, error)

// moveAsk bounds one MayMove on a board render, which must not stall on
// Forgejo.
const moveAsk = 5 * time.Second

// answer is a remembered MayMove answer, a failure included.
type answer struct {
	ok  bool
	err error
	at  time.Time
}

// moveFailTTL is how long a failed MayMove ask is served as the answer:
// a breath, so a render and the polls right after it ask Forgejo once
// each, while a healthy ask follows one broken render within seconds
// instead of a minute of hidden buttons and closed streams.
var moveFailTTL = 2 * time.Second

// mayMoveCached is Config.MayMove for the board's buttons: an answer is
// kept for writersTTL per repository, user and moved_by, so a render and
// the polls after it ask Forgejo once for each; a failure is kept for
// moveFailTTL only, so a canceled ask (a browser abort mid-render) never
// poisons the renders and the stream connects after it.
func (c *Console) mayMoveCached(ctx context.Context, repository, user, movedBy string) (bool, error) {
	if user == "" {
		return false, nil // the pulse's view of the board, no member's
	}
	key := repository + "\x00" + user + "\x00" + movedBy
	c.mu.Lock()
	a, ok := c.moves[key]
	c.mu.Unlock()
	if ok && time.Since(a.at) < writersTTL && (a.err == nil || time.Since(a.at) < moveFailTTL) {
		return a.ok, a.err
	}
	ask, cancel := context.WithTimeout(ctx, moveAsk)
	defer cancel()
	a.ok, a.err = c.cfg.MayMove(ask, repository, user, movedBy)
	a.at = time.Now()
	c.mu.Lock()
	if c.moves == nil {
		c.moves = map[string]answer{}
	}
	c.moves[key] = a
	c.mu.Unlock()
	return a.ok, a.err
}

// processOf is the name and lanes of the process card t follows: its
// project's current one (projects by repository), as intake reads it.
func processOf(projects map[string]factoryd.Project, t factoryd.Task) (string, policy.Process) {
	p := projects[t.Repository]
	return p.Process, policy.Process{Lanes: p.Lanes}
}

// movedByRefusal is the one sentence refusing a card of repository entry
// into lane.
func movedByRefusal(lane policy.Lane, repository string) string {
	who := "writers of " + repository
	if lane.MovedBy == policy.MovedByApprovers {
		who = "the approvers"
	}
	return fmt.Sprintf("Lane %s takes cards from %s.", lane.Name, who)
}

// moveRefusal is why user may not move card t, with facts f, which
// follows process name, into lane, in one sentence; "" allows it. The
// checks are intake's (factoryd lanes.go move), which stays the judge.
func moveRefusal(ctx context.Context, t factoryd.Task, f capFacts, name string, proc policy.Process, lane, user string, may mayMove) (string, error) {
	if t.Kind != "agent" || t.Status != "backlog" {
		return wrongState(t, f, "the backlog").Error(), nil
	}
	l, ok := proc.Lane(lane)
	if !ok || l.Class == "" {
		return fmt.Sprintf("Lane %s is no starting lane of process %s.", lane, name), nil
	}
	if lane == policy.LanePlan && t.Parent != "" {
		return "Lane plan takes no card of a plan: plans do not nest.", nil
	}
	allowed, err := may(ctx, t.Repository, user, l.MovedBy)
	if err != nil || allowed {
		return "", err
	}
	return movedByRefusal(l, t.Repository), nil
}

// planIssue is plan task id's issue: from the board's tasks, else read.
func (c *Console) planIssue(ctx context.Context, byID map[string]factoryd.Task, id string) int64 {
	if t, ok := byID[id]; ok {
		return t.IssueNumber
	}
	t, ok, err := c.cfg.Tasks.Task(ctx, id)
	if err != nil || !ok {
		log.Printf("console: plan %s: found %v, %v", id, ok, err)
		return 0
	}
	byID[id] = t
	return t.IssueNumber
}

// planCards are accepted plan t's cards; Start plan while any is in the
// backlog, for an approver who may move them into ready (mayStart).
func (c *Console) planCards(ctx context.Context, t factoryd.Task, mayStart bool, csrf string) (*planCards, error) {
	cards, err := c.cfg.Tasks.PlanCards(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	p := &planCards{}
	for _, k := range cards {
		p.Cards = append(p.Cards, c.card(k))
		if k.Status == "backlog" {
			p.Backlog++
		}
	}
	if mayStart && p.Backlog > 0 {
		p.CSRF = csrf
	}
	return p, nil
}

// addLabel adds label name to issue numbers of t's repository as a's
// member; it reports how many issues got it.
func (c *Console) addLabel(ctx context.Context, a actor, t factoryd.Task, name string, numbers ...int64) (int, error) {
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return 0, err
	}
	labels, err := c.labels(ctx, a.s, repo)
	if err != nil {
		return 0, err
	}
	id, ok := labels[name]
	if !ok {
		return 0, fmt.Errorf("label %s does not exist on %s", name, repo)
	}
	for i, n := range numbers {
		path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/labels", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), n)
		if err := c.as(ctx, a.s, http.MethodPost, path, map[string][]int64{"labels": {id}}, http.StatusOK, nil); err != nil {
			return i, err
		}
	}
	return len(numbers), nil
}

// comment posts text on t's issue as a's member; posted, when not nil,
// gets the comment's id.
func (c *Console) comment(ctx context.Context, a actor, t factoryd.Task, text string, posted *int64) error {
	path, err := issuePath(t, "/comments")
	if err != nil {
		return err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.as(ctx, a.s, http.MethodPost, path, map[string]string{"body": text}, http.StatusCreated, &out); err != nil {
		return err
	}
	if posted != nil {
		*posted = out.ID
	}
	return nil
}

// moveCard moves a backlog card into a starting lane of its process
// (doMoveCard).
func (c *Console) moveCard(w http.ResponseWriter, r *http.Request) {
	note, err := c.doMoveCard(r.Context(), button(r), r.PathValue("id"), r.PostFormValue("lane"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, note, nil)
}

// Item 141: the wait's length and poll interval, settable for the tests.
// Accepting a plan only signals its workflow; the plan task reaches its
// final status seconds later, once factoryd has filed the cards. A Start
// plan posted right after the accept waits for that instead of losing
// the race with a 409.
var (
	startPlanWait = 15 * time.Second
	startPlanPoll = 500 * time.Millisecond
)

// awaitAcceptedPlan is plan task t once it is final: t itself when it
// already is, else re-read every startPlanPoll until startPlanWait runs
// out. It answers false when the task never turned final, or vanished.
func (c *Console) awaitAcceptedPlan(ctx context.Context, t factoryd.Task) (factoryd.Task, bool) {
	if t.Lane != policy.LanePlan || factoryd.Final(t.Status) {
		return t, t.Lane == policy.LanePlan && t.Status == "done"
	}
	deadline := time.Now().Add(startPlanWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return t, false
		case <-time.After(startPlanPoll):
		}
		next, ok, err := c.cfg.Tasks.Task(ctx, t.ID)
		if err != nil {
			log.Printf("console: task %s while waiting for its plan: %v", t.ID, err)
			return t, false
		}
		if !ok {
			return next, false
		}
		if next.Lane != policy.LanePlan || factoryd.Final(next.Status) {
			return next, next.Lane == policy.LanePlan && next.Status == "done"
		}
		t = next
	}
	return t, false
}

// startPlan starts an accepted plan's backlog cards (doStartPlan).
func (c *Console) startPlan(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doStartPlan)
}

// retryCard files a failed card of a plan again (doRetry).
func (c *Console) retryCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doRetry)
}

// goCard is the face's Have the factory do it (doGo).
func (c *Console) goCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doGo)
}

// closeCard is the face's Close (doClose, with no note).
func (c *Console) closeCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, func(ctx context.Context, a actor, id string) (string, error) { return c.doClose(ctx, a, id, "") })
}

// acceptanceCloseCard is the face's Close: acceptance green
// (doCloseAcceptance).
func (c *Console) acceptanceCloseCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doCloseAcceptance)
}

// splitCard is the approval card's Split (doSplit).
func (c *Console) splitCard(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, c.doSplit)
}

// showNote runs a button's action on the card the path names and shows
// the board with its note.
func (c *Console) showNote(w http.ResponseWriter, r *http.Request, action func(ctx context.Context, a actor, id string) (string, error)) {
	note, err := action(r.Context(), button(r), r.PathValue("id"))
	if err != nil {
		c.fail(w, r, err)
		return
	}
	c.showBoard(w, r, http.StatusOK, note, nil)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// acceptPlan accepts the plan revision the approver saw (doAcceptPlan);
// a stale one shows the board with the newer revision's problem.
func (c *Console) acceptPlan(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	note, err := c.doAcceptPlan(r.Context(), button(r), r.PathValue("id"), revision, r.PostFormValue("sha256"))
	var stale factoryd.ErrStalePlan
	switch {
	case errors.As(err, &stale):
		c.showBoard(w, r, http.StatusConflict, "", map[string]string{r.PathValue("id"): stale.Error()})
	case err != nil:
		c.fail(w, r, err)
	default:
		c.showBoard(w, r, http.StatusOK, note, nil)
	}
}

// planAnswer posts a writer's answer to a question of the plan
// (doAnswerPlanQuestion).
func (c *Console) planAnswer(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	if _, err := c.doAnswerPlanQuestion(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("answer")); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
}

// planHoldout posts a writer's okay or drop of one hold-out of the plan
// (doDecideHoldout).
func (c *Console) planHoldout(w http.ResponseWriter, r *http.Request) {
	revision, _ := strconv.Atoi(r.PostFormValue("revision"))
	idx, _ := strconv.Atoi(r.PostFormValue("index"))
	if _, err := c.doDecideHoldout(r.Context(), button(r), r.PathValue("id"), revision, idx, r.PostFormValue("verb")); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+formFilter(r).query(), http.StatusSeeOther)
}

// planChanges posts a writer's change request on the plan
// (doRequestChanges).
func (c *Console) planChanges(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doRequestChanges(ctx, a, id, r.PostFormValue("text"))
	})
}

// rejectPlan ends the plan task failed with the writer's reason
// (doRejectPlan).
func (c *Console) rejectPlan(w http.ResponseWriter, r *http.Request) {
	c.showNote(w, r, func(ctx context.Context, a actor, id string) (string, error) {
		return c.doRejectPlan(ctx, a, id, r.PostFormValue("note"))
	})
}
