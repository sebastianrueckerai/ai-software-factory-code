package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The console's actions: one function each, which a button's handler and
// the project agent's tool both call, so either leaves the same checks,
// the same Forgejo calls as the member and the same one event row.

// actor is whom an action runs as and how it came: the member's session
// (their login and their Forgejo token), the repository a chat confines
// the action to ("" for a button, which any card's own checks admit), and
// the event's via.
type actor struct {
	s          *session
	repository string
	via        string
}

// button is the actor of the request's button.
func button(r *http.Request) actor { return actor{s: current(r), via: factoryd.ViaButton} }

// refusal is an action's answer other than success: the status a button
// answers and the sentence the button and the chat show.
type refusal struct {
	status int
	msg    string
}

func (e *refusal) Error() string { return e.msg }

func refuse(status int, msg string) error { return &refusal{status, msg} }

// forgejoRefusal is a Forgejo call of the action that failed: what the
// action asked Forgejo for.
type forgejoRefusal struct {
	what string
	err  error
}

func (e *forgejoRefusal) Error() string { return "Forgejo refused to " + e.what + ": " + e.err.Error() }

// fail answers a button whose action failed with err.
func (c *Console) fail(w http.ResponseWriter, r *http.Request, err error) {
	var no *refusal
	var fj *forgejoRefusal
	switch {
	case errors.As(err, &fj):
		c.forgejoFailed(w, r, fj.what, fj.err)
	case errors.As(err, &no) && no.status == http.StatusNotFound:
		http.NotFound(w, r)
	case errors.As(err, &no):
		http.Error(w, no.msg, no.status)
	default:
		log.Printf("console: %s: %v", current(r).user, err)
		http.Error(w, "The action failed; see the factoryd log.", http.StatusInternalServerError)
	}
}

// record writes what a just did as one event row, their login the actor.
func (c *Console) record(ctx context.Context, a actor, repository, taskID string, issue int64, kind string, detail map[string]any) {
	c.event(ctx, factoryd.Event{Repository: repository, TaskID: taskID, IssueNumber: issue, Kind: kind,
		Actor: a.s.user, Via: a.via, Detail: detail})
}

// cardIn is card id for a: 404 when there is none, when it is a chat task,
// or when a's chat is another project's.
func (c *Console) cardIn(ctx context.Context, a actor, id string) (factoryd.Task, error) {
	t, ok, err := c.cfg.Tasks.Task(ctx, id)
	switch {
	case err != nil:
		log.Printf("console: task %s: %v", id, err)
		return t, refuse(http.StatusInternalServerError, "cannot read the task")
	case !ok || t.Kind == factoryd.KindChat || a.repository != "" && t.Repository != a.repository:
		return t, refuse(http.StatusNotFound, "There is no card "+id+" in this project.")
	}
	return t, nil
}

// writes refuses a unless they write repository, whose subject the
// refusal names: the writers moved_by, through the board's cache.
func (c *Console) writes(ctx context.Context, a actor, repository, subject string) error {
	ok, err := c.mayMoveCached(ctx, repository, a.s.user, policy.MovedByWriters)
	switch {
	case err != nil:
		log.Printf("console: may %s write %s: %v", a.s.user, repository, err)
		return refuse(http.StatusServiceUnavailable, "cannot check the project's writers")
	case !ok:
		return refuse(http.StatusForbidden, "you write no project of "+subject)
	}
	return nil
}

// approves refuses a spend action, what, to anyone but the policy's
// approvers (ruling 128).
func (c *Console) approves(ctx context.Context, a actor, what string) error {
	if !c.isApprover(ctx, a.s.user) {
		log.Printf("console: %s is no approver: %s refused", a.s.user, what)
		return refuse(http.StatusForbidden, "Only the policy's approvers may authorize spend.")
	}
	return nil
}

// processOfCard is the name and lanes of the process card t follows.
func (c *Console) processOfCard(ctx context.Context, t factoryd.Task) (string, policy.Process, error) {
	ps, err := c.coderProjects(ctx)
	if err != nil {
		log.Printf("console: projects: %v", err)
		return "", policy.Process{}, refuse(http.StatusInternalServerError, "cannot read the projects")
	}
	byRepo := map[string]factoryd.Project{}
	for _, p := range ps {
		byRepo[p.Repository] = p
	}
	name, proc := processOf(byRepo, t)
	return name, proc, nil
}

// issuePath is the API path of t's issue, with suffix.
func issuePath(t factoryd.Task, suffix string) (string, error) {
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d%s", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), t.IssueNumber, suffix), nil
}

// doFileCard files an issue on project's repository as the member, labelled
// with the lane's label (backlog, plan or agent, or human for the human
// lane) and priority when given; the webhook or the heal pass makes it a
// task. It answers the repository and the issue's number.
func (c *Console) doFileCard(ctx context.Context, a actor, project, title, body, lane, priority string) (factoryd.Repo, int64, error) {
	title = strings.TrimSpace(title)
	switch {
	case title == "" || utf8.RuneCountInString(title) > 255:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "a title of 1 to 255 characters is required")
	case len(body) > 64<<10:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "the description is longer than 64 KiB")
	case laneLabels[lane] == "" && lane != factoryd.LaneHuman:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "lane must be backlog, plan, ready or human")
	case priority != "" && !slices.Contains(priorities, priority):
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "unknown priority")
	}
	ps, err := c.coderProjects(ctx)
	if err != nil {
		log.Printf("console: projects: %v", err)
		return factoryd.Repo{}, 0, refuse(http.StatusInternalServerError, "cannot read the projects")
	}
	i := slices.IndexFunc(ps, func(p factoryd.Project) bool { return p.Name == project })
	if i < 0 || a.repository != "" && ps[i].Repository != a.repository {
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "choose a project agents work on")
	}
	label := factoryd.LaneHuman
	if lane != factoryd.LaneHuman {
		if !slices.ContainsFunc(ps[i].Lanes, func(l policy.Lane) bool { return l.Name == lane }) {
			return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "project "+ps[i].Name+" has no lane "+lane)
		}
		label = laneLabels[lane]
	}
	repo := ps[i].Repo()
	labels, err := c.labels(ctx, a.s, repo)
	if err != nil {
		return repo, 0, &forgejoRefusal{"read the labels", err}
	}
	var ids []int64
	for _, name := range []string{label, priority} {
		if name == "" {
			continue
		}
		id, ok := labels[name]
		if !ok {
			return repo, 0, refuse(http.StatusConflict, "label "+name+" does not exist on "+repo.String())
		}
		ids = append(ids, id)
	}
	var issue struct {
		Number int64 `json:"number"`
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
	if err := c.as(ctx, a.s, http.MethodPost, path, map[string]any{"title": title, "body": body, "labels": ids},
		http.StatusCreated, &issue); err != nil {
		return repo, 0, &forgejoRefusal{"file the issue", err}
	}
	log.Printf("console: %s filed %s#%d (%s) via %s", a.s.user, repo, issue.Number, lane, a.via)
	c.record(ctx, a, repo.String(), "", issue.Number, factoryd.EventCardFiled, map[string]any{"lane": lane, "title": title})
	return repo, issue.Number, nil
}

// doMoveCard moves backlog card id into a starting lane of its process:
// the lane's label added as the member, whose label event intake judges.
func (c *Console) doMoveCard(ctx context.Context, a actor, id, lane string) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	name, proc, err := c.processOfCard(ctx, t)
	if err != nil {
		return "", err
	}
	why, err := moveRefusal(ctx, t, name, proc, lane, a.s.user, c.cfg.MayMove)
	if err != nil {
		log.Printf("console: may %s move %s into %s: %v", a.s.user, t.ID, lane, err)
		return "", refuse(http.StatusServiceUnavailable, "cannot check who may move the card")
	}
	if why == "" && t.IssueNumber == 0 {
		why = "The card has no issue yet."
	}
	if why != "" {
		return "", refuse(http.StatusForbidden, why)
	}
	if _, err := c.addLabel(ctx, a, t, laneLabels[lane], t.IssueNumber); err != nil {
		return "", &forgejoRefusal{"label the issue", err}
	}
	log.Printf("console: %s moved %s (%s#%d) to lane %s via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, lane, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardMoved, map[string]any{"lane": lane})
	return fmt.Sprintf("Moved #%d to lane %s; it leaves the backlog once factoryd picks it up.", t.IssueNumber, lane), nil
}

// doUnmoveCard undoes a move of card id into lane while the card is still
// in the backlog: the lane's label taken off as the member. Once factoryd
// took the card into the lane, nothing moves it back.
func (c *Console) doUnmoveCard(ctx context.Context, a actor, id, lane string) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := c.writes(ctx, a, t.Repository, "task "+t.ID); err != nil {
		return "", err
	}
	if t.Status != "backlog" {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d has left the backlog (it is %s): factoryd took it into the lane, which no undo reverses.", t.IssueNumber, t.Status))
	}
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "the task names no repository")
	}
	labels, err := c.labels(ctx, a.s, repo)
	if err != nil {
		return "", &forgejoRefusal{"read the labels", err}
	}
	label, ok := labels[laneLabels[lane]]
	if !ok {
		return "", refuse(http.StatusConflict, "label "+laneLabels[lane]+" does not exist on "+repo.String())
	}
	path, err := issuePath(t, fmt.Sprintf("/labels/%d", label))
	if err != nil {
		return "", err
	}
	if err := c.as(ctx, a.s, http.MethodDelete, path, nil, http.StatusNoContent, nil); err != nil {
		return "", &forgejoRefusal{"take the label off", err}
	}
	log.Printf("console: %s took %s (%s#%d) back from lane %s via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, lane, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardMoved, map[string]any{"lane": policy.LaneBacklog})
	return fmt.Sprintf("#%d stays in the backlog: label %s is off.", t.IssueNumber, laneLabels[lane]), nil
}

// doStartPlan labels each card of accepted plan id still in the backlog
// agent as the member (section 1.5): each label event is a move into
// ready.
func (c *Console) doStartPlan(ctx context.Context, a actor, id string) (string, error) {
	if err := c.approves(ctx, a, "start a plan"); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	accepted, ok := c.awaitAcceptedPlan(ctx, t)
	if !ok {
		msg := "Task " + t.ID + " is no accepted plan."
		if accepted.Lane == policy.LanePlan && accepted.Status != "done" && !factoryd.Final(accepted.Status) {
			msg = "The plan's cards are still being filed; try again in a moment."
		}
		return "", refuse(http.StatusConflict, msg)
	}
	t = accepted
	_, proc, err := c.processOfCard(ctx, t)
	if err != nil {
		return "", err
	}
	ready, _ := proc.Lane(policy.LaneReady)
	allowed, err := c.cfg.MayMove(ctx, t.Repository, a.s.user, ready.MovedBy)
	if err != nil {
		log.Printf("console: may %s start plan %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusServiceUnavailable, "cannot check who may move the cards")
	}
	if !allowed {
		return "", refuse(http.StatusForbidden, movedByRefusal(ready, t.Repository))
	}
	cards, err := c.cfg.Tasks.PlanCards(ctx, t.ID)
	if err != nil {
		log.Printf("console: the cards of plan %s: %v", t.ID, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the plan's cards")
	}
	var backlog []int64
	for _, k := range cards {
		if k.Status == "backlog" && k.IssueNumber > 0 {
			backlog = append(backlog, k.IssueNumber)
		}
	}
	n := 0
	if len(backlog) > 0 {
		if n, err = c.addLabel(ctx, a, t, factoryd.LabelAgent, backlog...); err != nil {
			log.Printf("console: %s started plan %s: %d of %d cards labelled", a.s.user, t.ID, n, len(backlog))
			return "", &forgejoRefusal{"label the plan's cards", err}
		}
	}
	log.Printf("console: %s started plan %s via %s: %d cards labelled agent", a.s.user, t.ID, a.via, n)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanStarted, map[string]any{"cards": n})
	return fmt.Sprintf("Start plan: %d %s of plan #%d labelled agent.", n, plural(n, "card"), t.IssueNumber), nil
}

// doRetry files failed plan card id again (factoryd's RetryPlanCard)
// and labels the replacement agent as the member: the label event is its
// move into ready, which intake judges.
func (c *Console) doRetry(ctx context.Context, a actor, id string) (string, error) {
	if err := c.approves(ctx, a, "retry a card"); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	_, proc, err := c.processOfCard(ctx, t)
	if err != nil {
		return "", err
	}
	ready, _ := proc.Lane(policy.LaneReady)
	allowed, err := c.cfg.MayMove(ctx, t.Repository, a.s.user, ready.MovedBy)
	if err != nil {
		log.Printf("console: may %s retry %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusServiceUnavailable, "cannot check who may move the card")
	}
	if !allowed {
		return "", refuse(http.StatusForbidden, movedByRefusal(ready, t.Repository))
	}
	issue, err := c.cfg.RetryPlanCard(ctx, t.ID, a.s.user)
	var no factoryd.ErrNoRetry
	switch {
	case errors.As(err, &no):
		return "", refuse(http.StatusConflict, no.Error())
	case err != nil:
		log.Printf("console: %s retries %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusInternalServerError, "The card could not be retried; see the factoryd log.")
	}
	if _, err := c.addLabel(ctx, a, t, factoryd.LabelAgent, issue); err != nil {
		return "", &forgejoRefusal{fmt.Sprintf("label #%d agent (the replacement of #%d is filed)", issue, t.IssueNumber), err}
	}
	log.Printf("console: %s retried %s (%s#%d) as #%d via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, issue, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardRetried, map[string]any{"replacement": issue})
	return fmt.Sprintf("Retried #%d as #%d; it starts in lane ready once factoryd picks it up.", t.IssueNumber, issue), nil
}

// planIn is plan task id for a; 409 when it is no plan.
func (c *Console) planIn(ctx context.Context, a actor, id string) (factoryd.Task, error) {
	t, err := c.cardIn(ctx, a, id)
	if err == nil && (t.Lane != policy.LanePlan || t.IssueNumber == 0) {
		err = refuse(http.StatusConflict, "Task "+t.ID+" is no plan.")
	}
	return t, err
}

// doAcceptPlan accepts the revision of plan id the approver saw: the
// workflow is signalled, then the acceptance commented as the member (the
// plan's wait reads that comment as no change request, in either order).
// A stale revision answers factoryd.ErrStalePlan.
func (c *Console) doAcceptPlan(ctx context.Context, a actor, id string, revision int, sha string) (string, error) {
	if err := c.approves(ctx, a, "accept a plan"); err != nil {
		return "", err
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if revision < 1 || sha == "" {
		return "", refuse(http.StatusBadRequest, "the form names no plan revision; reload the board")
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.AcceptPlan(ctx, t.ID, revision, sha, a.s.user); {
	case errors.As(err, &stale):
		return "", stale
	case err != nil:
		log.Printf("console: %s accepts revision %d of plan %s: %v", a.s.user, revision, t.ID, err)
		return "", refuse(http.StatusConflict, "The plan could not be accepted; see the factoryd log.")
	}
	if err := c.comment(ctx, a, t, factoryd.PlanAcceptedComment(revision), nil); err != nil {
		return "", &forgejoRefusal{"comment the acceptance (the plan is accepted)", err}
	}
	log.Printf("console: %s accepted revision %d of plan %s via %s", a.s.user, revision, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanAccepted, map[string]any{"revision": revision})
	return fmt.Sprintf("Accepted revision %d of plan #%d; factoryd files its cards.", revision, t.IssueNumber), nil
}

// doRequestChanges posts a writer's change request on plan id's issue
// as the member; intake hands it to the planner.
func (c *Console) doRequestChanges(ctx context.Context, a actor, id, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "the changes you ask for, up to 64 KiB, are required")
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := c.comment(ctx, a, t, text, nil); err != nil {
		return "", &forgejoRefusal{"post the change request", err}
	}
	log.Printf("console: %s asked for changes to plan %s via %s", a.s.user, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanChangesRequested, nil)
	return fmt.Sprintf("Your change request is on plan #%d; the planner answers a new revision.", t.IssueNumber), nil
}

// doRejectPlan ends plan task id failed with the writer's reason.
func (c *Console) doRejectPlan(ctx context.Context, a actor, id, note string) (string, error) {
	note = strings.TrimSpace(note)
	if note == "" || len(note) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "a reason of up to 64 KiB is required")
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := c.cfg.RejectPlan(ctx, t.ID, a.s.user, note); err != nil {
		log.Printf("console: %s rejects plan %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusConflict, "The plan could not be rejected; see the factoryd log.")
	}
	log.Printf("console: %s rejected plan %s via %s: %s", a.s.user, t.ID, a.via, note)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanRejected, nil)
	return fmt.Sprintf("Rejected plan #%d.", t.IssueNumber), nil
}

// doAnswer posts the member's answer to card id's escalation as an
// issue comment under their own name; intake's human_comment path resumes
// the task. The event names the comment, so intake records no second
// question_answered when its webhook delivers that very comment.
func (c *Console) doAnswer(ctx context.Context, a actor, id, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "an answer of up to 64 KiB is required")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if t.Kind != "agent" || t.Status != "waiting-human" || t.IssueNumber == 0 {
		return "", refuse(http.StatusConflict, "task "+t.ID+" is not waiting for an answer")
	}
	var posted int64
	if err := c.comment(ctx, a, t, text, &posted); err != nil {
		return "", &forgejoRefusal{"post the answer", err}
	}
	log.Printf("console: %s answered the escalation of %s via %s", a.s.user, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventQuestionAnswered, commentDetail(posted))
	return fmt.Sprintf("Answered #%d.", t.IssueNumber), nil
}

// doComment posts text on card id's issue as the member, a writer of its
// project; posted is the comment's id.
func (c *Console) doComment(ctx context.Context, a actor, id, text string) (posted int64, note string, err error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64<<10 {
		return 0, "", refuse(http.StatusBadRequest, "a comment of up to 64 KiB is required")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return 0, "", err
	}
	if err := c.writes(ctx, a, t.Repository, "task "+t.ID); err != nil {
		return 0, "", err
	}
	if t.IssueNumber == 0 {
		return 0, "", refuse(http.StatusConflict, "The card has no issue yet.")
	}
	if err := c.comment(ctx, a, t, text, &posted); err != nil {
		return 0, "", &forgejoRefusal{"post the comment", err}
	}
	log.Printf("console: %s commented on %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardCommented, commentDetail(posted))
	return posted, fmt.Sprintf("Commented on #%d.", t.IssueNumber), nil
}

// doDeleteComment deletes comment posted of card id as the member: the undo
// of doComment.
func (c *Console) doDeleteComment(ctx context.Context, a actor, id string, posted int64) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := c.writes(ctx, a, t.Repository, "task "+t.ID); err != nil {
		return "", err
	}
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "the task names no repository")
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/comments/%d", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), posted)
	if err := c.as(ctx, a.s, http.MethodDelete, path, nil, http.StatusNoContent, nil); err != nil {
		return "", &forgejoRefusal{"delete the comment", err}
	}
	log.Printf("console: %s deleted comment %d of %s via %s", a.s.user, posted, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCommentDeleted, commentDetail(posted))
	return fmt.Sprintf("Deleted the comment on #%d.", t.IssueNumber), nil
}

// doSteer hands the member's text to card id's running session on the
// supervisor's message path, marked as theirs.
func (c *Console) doSteer(ctx context.Context, a actor, id, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 8<<10 {
		return "", refuse(http.StatusBadRequest, "a message of up to 8 KiB is required")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := c.writes(ctx, a, t.Repository, "task "+t.ID); err != nil {
		return "", err
	}
	if t.Status != "running" {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d runs no session (it is %s).", t.IssueNumber, t.Status))
	}
	switch err := c.cfg.Steer(ctx, t.ID, a.s.user, text); {
	case errors.Is(err, factoryd.ErrNoLiveSession), errors.Is(err, harness.ErrNotRunning):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d runs no session right now.", t.IssueNumber))
	case errors.Is(err, harness.ErrNotSupported):
		return "", refuse(http.StatusConflict, fmt.Sprintf("The harness of card #%d takes no messages.", t.IssueNumber))
	case err != nil:
		log.Printf("console: %s steers %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusBadGateway, "The message could not be delivered; see the factoryd log.")
	}
	log.Printf("console: %s steered %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardSteered, nil)
	return fmt.Sprintf("The session of #%d has your message.", t.IssueNumber), nil
}

// doSetSummary is a writer's one-line summary of card id, shown on its face
// instead of the generated title; "" restores the generated one. It
// answers the card as it was.
func (c *Console) doSetSummary(ctx context.Context, a actor, id, summary string) (factoryd.Task, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return t, err
	}
	if err := c.writes(ctx, a, t.Repository, "task "+t.ID); err != nil {
		return t, err
	}
	summary = strings.TrimSpace(summary)
	if utf8.RuneCountInString(summary) > maxSummaryLen {
		return t, refuse(http.StatusBadRequest, "a summary of up to 255 characters")
	}
	if err := c.cfg.Tasks.SetHumanSummary(ctx, t.ID, summary); err != nil {
		if errors.Is(err, factoryd.ErrNotFound) {
			return t, refuse(http.StatusNotFound, "There is no card "+id+" in this project.")
		}
		log.Printf("console: %s summarized %s: %v", a.s.user, t.ID, err)
		return t, refuse(http.StatusInternalServerError, "the summary could not be stored")
	}
	log.Printf("console: %s set the summary of %s (%q) via %s", a.s.user, t.ID, summary, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventSummarySet, map[string]any{"summary": summary})
	return t, nil
}

// doSetAccent is a writer's colour label on card id, one of
// factoryd.Accents or "" to remove it. It answers the card as it was.
func (c *Console) doSetAccent(ctx context.Context, a actor, id, accent string) (factoryd.Task, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return t, err
	}
	if err := c.writes(ctx, a, t.Repository, "task "+t.ID); err != nil {
		return t, err
	}
	if accent != "" && !factoryd.IsAccent(accent) {
		return t, refuse(http.StatusBadRequest, "accent must be one of "+strings.Join(factoryd.Accents, ", "))
	}
	if err := c.cfg.Tasks.SetTaskAccent(ctx, t.ID, accent); err != nil {
		if errors.Is(err, factoryd.ErrNotFound) {
			return t, refuse(http.StatusNotFound, "There is no card "+id+" in this project.")
		}
		log.Printf("console: %s accented %s: %v", a.s.user, t.ID, err)
		return t, refuse(http.StatusInternalServerError, "the accent could not be stored")
	}
	log.Printf("console: %s set the accent of %s (%q) via %s", a.s.user, t.ID, accent, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventAccentSet, map[string]any{"accent": accent})
	return t, nil
}

// pendingRequest is pending approval request id for a: 409 when it is
// not pending (any more), 404 when a's chat is another project's.
func (c *Console) pendingRequest(ctx context.Context, a actor, id int64) (factoryd.ApprovalRequest, error) {
	all, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		return factoryd.ApprovalRequest{}, refuse(http.StatusInternalServerError, "cannot read the approval requests")
	}
	i := slices.IndexFunc(all, func(q factoryd.ApprovalRequest) bool { return q.ID == id })
	switch {
	case i < 0:
		return factoryd.ApprovalRequest{}, refuse(http.StatusConflict, fmt.Sprintf("approval request %d is not pending (any more); reload the board", id))
	case a.repository != "" && all[i].Project != a.repository:
		return factoryd.ApprovalRequest{}, refuse(http.StatusNotFound, fmt.Sprintf("There is no approval request %d in this project.", id))
	}
	return all[i], nil
}

// decide records the member's decision on q; a request the factory
// decided meanwhile (a new attempt superseded it) is logged, not an
// error: Forgejo already carries the member's action.
func (c *Console) decide(ctx context.Context, a actor, q factoryd.ApprovalRequest, status, note string) error {
	err := c.cfg.Approvals.DecideApproval(ctx, q.ID, q.Kind, status, a.s.user, note)
	switch {
	case errors.Is(err, factoryd.ErrNotFound) && q.Kind == factoryd.ApprovalMerge:
		log.Printf("console: approval request %d was decided by the factory before %s's %s was recorded", q.ID, a.s.user, status)
	case err != nil:
		log.Printf("console: decide approval request %d: %v", q.ID, err)
		return refuse(http.StatusInternalServerError, "cannot record the decision; see the factoryd log")
	}
	log.Printf("console: %s %s %s request %d of %s via %s: %s", a.s.user, status, q.Kind, q.ID, q.TaskID, a.via, note)
	return nil
}

// issueOf is task id's issue, 0 when it has none or cannot be read.
func (c *Console) issueOf(ctx context.Context, id string) int64 {
	t, _, err := c.cfg.Tasks.Task(ctx, id)
	if err != nil {
		log.Printf("console: task %s: %v", id, err)
	}
	return t.IssueNumber
}

// doApprove approves approval request id: a resource request for its
// task (one commission of that spec; the project's envelope is its file's,
// ruling 183); a merge request gets the member's APPROVED review on the
// pull request, and only once Forgejo has it is the workflow signalled,
// which merges as factory-merge.
func (c *Console) doApprove(ctx context.Context, a actor, id int64) (string, error) {
	if err := c.approves(ctx, a, "approve"); err != nil {
		return "", err
	}
	q, err := c.pendingRequest(ctx, a, id)
	if err != nil {
		return "", err
	}
	switch q.Kind {
	case factoryd.ApprovalResource:
		if err := c.decide(ctx, a, q, "approved", "approved for the task"); err != nil {
			return "", err
		}
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeApproved, map[string]any{"approval": q.ID, "kind": "resource"})
		return "Approved the resource for its task.", nil
	case factoryd.ApprovalMerge:
		var spec factoryd.MergeSpec
		if err := json.Unmarshal(q.Spec, &spec); err != nil || spec.HeadSHA == "" {
			return "", refuse(http.StatusInternalServerError, "the merge request names no head commit")
		}
		if err := c.review(ctx, a, q, "APPROVED", "Approved in the factory console.", spec.HeadSHA); err != nil {
			return "", &forgejoRefusal{"submit the approving review", err}
		}
		if err := c.cfg.ApproveMerge(ctx, q.TaskID, q.PRNumber, a.s.user); err != nil {
			log.Printf("console: %s approved PR #%d of %s, the signal failed: %v", a.s.user, q.PRNumber, q.TaskID, err)
			return "", refuse(http.StatusBadGateway, "your approving review is on the pull request, but the task's workflow could not be signalled; approve again")
		}
		log.Printf("console: %s approved PR #%d of %s at %s via %s", a.s.user, q.PRNumber, q.TaskID, spec.HeadSHA, a.via)
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeApproved, map[string]any{"pr": q.PRNumber})
		return fmt.Sprintf("Approved pull request #%d; factoryd merges it.", q.PRNumber), nil
	}
	return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which the board decides", q.ID, q.Kind))
}

// doDeny denies approval request id with the member's reason, a
// writer of its project. A resource request is denied (the agent is told
// the reason). A merge request gets a REQUEST_CHANGES review as the
// member, which sends the task back to its agent (action
// request_changes), or is rejected: the reason commented and the pull
// request closed as the member, which fails the task (action reject).
func (c *Console) doDeny(ctx context.Context, a actor, id int64, action, note string) (string, error) {
	note = strings.TrimSpace(note)
	if note == "" || len(note) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "a reason of up to 64 KiB is required")
	}
	q, err := c.pendingRequest(ctx, a, id)
	if err != nil {
		return "", err
	}
	if err := c.writes(ctx, a, q.Project, fmt.Sprintf("approval request %d", q.ID)); err != nil {
		return "", err
	}
	switch q.Kind {
	case factoryd.ApprovalResource:
		if err := c.decide(ctx, a, q, "denied", note); err != nil {
			return "", err
		}
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeDenied, map[string]any{"approval": q.ID, "kind": "resource"})
		return "Denied the resource.", nil
	case factoryd.ApprovalMerge:
	default:
		return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which the board decides", q.ID, q.Kind))
	}
	repo, err := factoryd.SplitRepo(q.Project)
	if err != nil {
		log.Printf("console: approval request %d: %v", q.ID, err)
		return "", refuse(http.StatusInternalServerError, "the request names no repository")
	}
	switch action {
	case "request_changes":
		var spec factoryd.MergeSpec
		if err := json.Unmarshal(q.Spec, &spec); err != nil {
			return "", refuse(http.StatusInternalServerError, "cannot read the merge request")
		}
		if err := c.review(ctx, a, q, "REQUEST_CHANGES", note, spec.HeadSHA); err != nil {
			return "", &forgejoRefusal{"submit the change request", err}
		}
		if err := c.decide(ctx, a, q, "denied", "changes requested: "+note); err != nil {
			return "", err
		}
	case "reject":
		pr := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
		if err := c.as(ctx, a.s, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", pr, q.PRNumber),
			map[string]string{"body": "Rejected in the factory console: " + note}, http.StatusCreated, nil); err != nil {
			return "", &forgejoRefusal{"comment the rejection", err}
		}
		if err := c.as(ctx, a.s, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", pr, q.PRNumber),
			map[string]string{"state": "closed"}, http.StatusCreated, nil); err != nil {
			return "", &forgejoRefusal{"close the pull request", err}
		}
		if err := c.decide(ctx, a, q, "denied", "rejected: "+note); err != nil {
			return "", err
		}
	default:
		return "", refuse(http.StatusBadRequest, "choose request changes or reject")
	}
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeDenied, map[string]any{"pr": q.PRNumber, "action": action})
	return fmt.Sprintf("Denied pull request #%d (%s).", q.PRNumber, strings.ReplaceAll(action, "_", " ")), nil
}
