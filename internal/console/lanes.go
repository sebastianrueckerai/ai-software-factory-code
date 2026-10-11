package console

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// mayMoveCached is Config.MayMove for the board's buttons: an answer, a
// failure too, is kept for writersTTL per repository, user and moved_by,
// so a render and the polls after it ask Forgejo once for each.
func (c *Console) mayMoveCached(ctx context.Context, repository, user, movedBy string) (bool, error) {
	key := repository + "\x00" + user + "\x00" + movedBy
	c.mu.Lock()
	a, ok := c.moves[key]
	c.mu.Unlock()
	if ok && time.Since(a.at) < writersTTL {
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

// cardProcess is the process card t follows; false once the answer is
// written.
func (c *Console) cardProcess(w http.ResponseWriter, r *http.Request, t factoryd.Task) (string, policy.Process, bool) {
	ps, err := c.coderProjects(r)
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return "", policy.Process{}, false
	}
	byRepo := map[string]factoryd.Project{}
	for _, p := range ps {
		byRepo[p.Repository] = p
	}
	name, proc := processOf(byRepo, t)
	return name, proc, true
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

// moveRefusal is why user may not move card t, which follows process
// name, into lane, in one sentence; "" allows it. The checks are intake's
// (factoryd lanes.go move), which stays the judge.
func moveRefusal(ctx context.Context, t factoryd.Task, name string, proc policy.Process, lane, user string, may mayMove) (string, error) {
	if t.Kind != "agent" || t.Status != "backlog" {
		return fmt.Sprintf("Card #%d is not in the backlog.", t.IssueNumber), nil
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

// taskOf is the task the path names; false once the answer is written.
func (c *Console) taskOf(w http.ResponseWriter, r *http.Request) (factoryd.Task, bool) {
	t, ok, err := c.cfg.Tasks.Task(r.Context(), r.PathValue("id"))
	if err != nil {
		log.Printf("console: task %s: %v", r.PathValue("id"), err)
		http.Error(w, "cannot read the task", http.StatusInternalServerError)
		return t, false
	}
	if !ok {
		http.NotFound(w, r)
	}
	return t, ok
}

// addLabel adds label name to issue numbers of t's repository as the
// human; it reports how many issues got it.
func (c *Console) addLabel(r *http.Request, t factoryd.Task, name string, numbers ...int64) (int, error) {
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return 0, err
	}
	labels, err := c.labels(r, repo)
	if err != nil {
		return 0, err
	}
	id, ok := labels[name]
	if !ok {
		return 0, fmt.Errorf("label %s does not exist on %s", name, repo)
	}
	for i, n := range numbers {
		path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/labels", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), n)
		if err := c.asUser(r, http.MethodPost, path, map[string][]int64{"labels": {id}}, http.StatusOK, nil); err != nil {
			return i, err
		}
	}
	return len(numbers), nil
}

// comment posts text on t's issue as the human.
func (c *Console) comment(r *http.Request, t factoryd.Task, text string) error {
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), t.IssueNumber)
	return c.asUser(r, http.MethodPost, path, map[string]string{"body": text}, http.StatusCreated, nil)
}

// moveCard moves a backlog card into a starting lane of its process: the
// lane's label added as the human, whose label event intake judges.
func (c *Console) moveCard(w http.ResponseWriter, r *http.Request) {
	t, ok := c.taskOf(w, r)
	if !ok {
		return
	}
	user, lane := current(r).user, r.PostFormValue("lane")
	name, proc, ok := c.cardProcess(w, r, t)
	if !ok {
		return
	}
	why, err := moveRefusal(r.Context(), t, name, proc, lane, user, c.cfg.MayMove)
	if err != nil {
		log.Printf("console: may %s move %s into %s: %v", user, t.ID, lane, err)
		http.Error(w, "cannot check who may move the card", http.StatusServiceUnavailable)
		return
	}
	if why == "" && t.IssueNumber == 0 {
		why = "The card has no issue yet."
	}
	if why != "" {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	if _, err := c.addLabel(r, t, laneLabels[lane], t.IssueNumber); err != nil {
		c.forgejoFailed(w, r, "label the issue", err)
		return
	}
	log.Printf("console: %s moved %s (%s#%d) to lane %s", user, t.ID, t.Repository, t.IssueNumber, lane)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardMoved, map[string]any{"lane": lane})
	c.showBoard(w, r, http.StatusOK, fmt.Sprintf("Moved #%d to lane %s; it leaves the backlog once factoryd picks it up.", t.IssueNumber, lane), nil)
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

// startPlan labels each card of an accepted plan still in the backlog
// agent as the human (section 1.5): each label event is a move into ready.
func (c *Console) startPlan(w http.ResponseWriter, r *http.Request) {
	t, ok := c.taskOf(w, r)
	if !ok {
		return
	}
	accepted, ok := c.awaitAcceptedPlan(r.Context(), t)
	if !ok {
		msg := "Task " + t.ID + " is no accepted plan."
		if accepted.Lane == policy.LanePlan && accepted.Status != "done" && !factoryd.Final(accepted.Status) {
			msg = "The plan's cards are still being filed; try again in a moment."
		}
		http.Error(w, msg, http.StatusConflict)
		return
	}
	t = accepted
	_, proc, ok := c.cardProcess(w, r, t)
	if !ok {
		return
	}
	ready, _ := proc.Lane(policy.LaneReady)
	allowed, err := c.cfg.MayMove(r.Context(), t.Repository, current(r).user, ready.MovedBy)
	if err != nil {
		log.Printf("console: may %s start plan %s: %v", current(r).user, t.ID, err)
		http.Error(w, "cannot check who may move the cards", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		http.Error(w, movedByRefusal(ready, t.Repository), http.StatusForbidden)
		return
	}
	cards, err := c.cfg.Tasks.PlanCards(r.Context(), t.ID)
	if err != nil {
		log.Printf("console: the cards of plan %s: %v", t.ID, err)
		http.Error(w, "cannot read the plan's cards", http.StatusInternalServerError)
		return
	}
	var backlog []int64
	for _, k := range cards {
		if k.Status == "backlog" && k.IssueNumber > 0 {
			backlog = append(backlog, k.IssueNumber)
		}
	}
	n := 0
	if len(backlog) > 0 {
		if n, err = c.addLabel(r, t, factoryd.LabelAgent, backlog...); err != nil {
			log.Printf("console: %s started plan %s: %d of %d cards labelled", current(r).user, t.ID, n, len(backlog))
			c.forgejoFailed(w, r, "label the plan's cards", err)
			return
		}
	}
	log.Printf("console: %s started plan %s: %d cards labelled agent", current(r).user, t.ID, n)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanStarted, map[string]any{"cards": n})
	c.showBoard(w, r, http.StatusOK, fmt.Sprintf("Start plan: %d %s of plan #%d labelled agent.", n, plural(n, "card"), t.IssueNumber), nil)
}

// retryCard files a failed card of a plan again (factoryd's
// RetryPlanCard) and labels the replacement agent as the human: the label
// event is its move into ready, which intake judges.
func (c *Console) retryCard(w http.ResponseWriter, r *http.Request) {
	t, ok := c.taskOf(w, r)
	if !ok {
		return
	}
	_, proc, ok := c.cardProcess(w, r, t)
	if !ok {
		return
	}
	user := current(r).user
	ready, _ := proc.Lane(policy.LaneReady)
	allowed, err := c.cfg.MayMove(r.Context(), t.Repository, user, ready.MovedBy)
	if err != nil {
		log.Printf("console: may %s retry %s: %v", user, t.ID, err)
		http.Error(w, "cannot check who may move the card", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		http.Error(w, movedByRefusal(ready, t.Repository), http.StatusForbidden)
		return
	}
	issue, err := c.cfg.RetryPlanCard(r.Context(), t.ID, user)
	var no factoryd.ErrNoRetry
	switch {
	case errors.As(err, &no):
		http.Error(w, no.Error(), http.StatusConflict)
		return
	case err != nil:
		log.Printf("console: %s retries %s: %v", user, t.ID, err)
		http.Error(w, "The card could not be retried; see the factoryd log.", http.StatusInternalServerError)
		return
	}
	if _, err := c.addLabel(r, t, factoryd.LabelAgent, issue); err != nil {
		c.forgejoFailed(w, r, fmt.Sprintf("label #%d agent (the replacement of #%d is filed)", issue, t.IssueNumber), err)
		return
	}
	log.Printf("console: %s retried %s (%s#%d) as #%d", user, t.ID, t.Repository, t.IssueNumber, issue)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardRetried, map[string]any{"replacement": issue})
	c.showBoard(w, r, http.StatusOK, fmt.Sprintf("Retried #%d as #%d; it starts in lane ready once factoryd picks it up.", t.IssueNumber, issue), nil)
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// planTask is the plan task the path names; false once the answer is
// written.
func (c *Console) planTask(w http.ResponseWriter, r *http.Request) (factoryd.Task, bool) {
	t, ok := c.taskOf(w, r)
	if ok && (t.Lane != policy.LanePlan || t.IssueNumber == 0) {
		http.Error(w, "Task "+t.ID+" is no plan.", http.StatusConflict)
		return t, false
	}
	return t, ok
}

// acceptPlan accepts the plan revision the approver saw: the workflow is
// signalled, then the acceptance commented as the human (the plan's wait
// reads that comment as no change request, in either order).
func (c *Console) acceptPlan(w http.ResponseWriter, r *http.Request) {
	t, ok := c.planTask(w, r)
	if !ok {
		return
	}
	revision, err := strconv.Atoi(r.PostFormValue("revision"))
	sha := r.PostFormValue("sha256")
	if err != nil || revision < 1 || sha == "" {
		http.Error(w, "the form names no plan revision; reload the board", http.StatusBadRequest)
		return
	}
	user := current(r).user
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.AcceptPlan(r.Context(), t.ID, revision, sha, user); {
	case errors.As(err, &stale):
		c.showBoard(w, r, http.StatusConflict, "", map[string]string{t.ID: stale.Error()})
		return
	case err != nil:
		log.Printf("console: %s accepts revision %d of plan %s: %v", user, revision, t.ID, err)
		http.Error(w, "The plan could not be accepted; see the factoryd log.", http.StatusConflict)
		return
	}
	if err := c.comment(r, t, factoryd.PlanAcceptedComment(revision)); err != nil {
		c.forgejoFailed(w, r, "comment the acceptance (the plan is accepted)", err)
		return
	}
	log.Printf("console: %s accepted revision %d of plan %s", user, revision, t.ID)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanAccepted, map[string]any{"revision": revision})
	c.showBoard(w, r, http.StatusOK, fmt.Sprintf("Accepted revision %d of plan #%d; factoryd files its cards.", revision, t.IssueNumber), nil)
}

// planChanges posts a writer's change request on the plan's issue as the
// human; intake hands it to the planner.
func (c *Console) planChanges(w http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.PostFormValue("text"))
	if text == "" || len(text) > 64<<10 {
		http.Error(w, "the changes you ask for, up to 64 KiB, are required", http.StatusBadRequest)
		return
	}
	t, ok := c.planTask(w, r)
	if !ok {
		return
	}
	if err := c.comment(r, t, text); err != nil {
		c.forgejoFailed(w, r, "post the change request", err)
		return
	}
	log.Printf("console: %s asked for changes to plan %s", current(r).user, t.ID)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanChangesRequested, nil)
	c.showBoard(w, r, http.StatusOK, fmt.Sprintf("Your change request is on plan #%d; the planner answers a new revision.", t.IssueNumber), nil)
}

// rejectPlan ends the plan task failed with the writer's reason.
func (c *Console) rejectPlan(w http.ResponseWriter, r *http.Request) {
	note := strings.TrimSpace(r.PostFormValue("note"))
	if note == "" || len(note) > 64<<10 {
		http.Error(w, "a reason of up to 64 KiB is required", http.StatusBadRequest)
		return
	}
	t, ok := c.planTask(w, r)
	if !ok {
		return
	}
	user := current(r).user
	if err := c.cfg.RejectPlan(r.Context(), t.ID, user, note); err != nil {
		log.Printf("console: %s rejects plan %s: %v", user, t.ID, err)
		http.Error(w, "The plan could not be rejected; see the factoryd log.", http.StatusConflict)
		return
	}
	log.Printf("console: %s rejected plan %s: %s", user, t.ID, note)
	c.act(r, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanRejected, nil)
	c.showBoard(w, r, http.StatusOK, fmt.Sprintf("Rejected plan #%d.", t.IssueNumber), nil)
}
