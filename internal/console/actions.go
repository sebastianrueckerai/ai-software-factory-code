package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
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
	// level, mergeRule, chatTask and decider ride the chat's actor: the
	// autonomy state, the rule and the chat task behind its actions, and
	// who decided, the agent unconfirmed or the member (roadmap 239).
	level, mergeRule, chatTask, decider string
	// wake marks the actor of a turn the Waker started (the concept's
	// wake): the member never asked, so a closed intake refuses its own
	// filings while the member's ask still files.
	wake   bool
	thread int64 // the chat thread the action came from, which its event names so the card shows the conversation behind it
}

// attribution is who a's commit is for: the chat or the console, the
// chat task behind it and the member.
func (a actor) attribution() factoryd.Attribution {
	role := "console"
	if a.via == factoryd.ViaChat {
		role = "chat"
	}
	return factoryd.Attribution{Role: role, Task: a.chatTask, Human: a.s.user}
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

// capFacts are what a card waiting on a human waits for: its sessions
// used, allowed and granted, whether it waits at its session cap, and its
// pending merge or plan request. A pending request outranks the cap: a
// card that used every session may still pass its gates.
type capFacts struct {
	attempts, sessions, extra int
	capped, merge, plan       bool
	// rerun: its merge request is a factory pull request's on red checks,
	// which a re-run or a close decides (roadmap 264)
	rerun bool
	// returns: its merge request is an agent's on red checks, which a
	// return to the agent or a close decides
	returns bool
}

// capFactsOf reads t's facts, only for a card waiting on a human or in
// review; a failed
// read leaves its fact unknown, which wrongState reads as a question.
func (c *Console) capFactsOf(ctx context.Context, t factoryd.Task) capFacts {
	if t.Status != "waiting-human" && t.Status != "in-review" {
		return capFacts{}
	}
	sessions := 0
	if v, ok := c.inForce(ctx); ok {
		sessions = v.Document.Limits.Sessions
	}
	bt, _, err := c.cfg.Tasks.BoardTask(ctx, t.ID)
	if err != nil {
		log.Printf("console: task %s's escalation: %v", t.ID, err)
	}
	all, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
	}
	return capFactsFrom(t, sessions, bt.Escalation, all)
}

// capFactsFrom are t's facts from the policy's sessions, its newest
// escalation and the pending approval requests; a card neither waiting on
// a human nor in review has none, and only one waiting on a human is
// capped.
func capFactsFrom(t factoryd.Task, sessions int, escalation string, pending []factoryd.ApprovalRequest) capFacts {
	if t.Status != "waiting-human" && t.Status != "in-review" {
		return capFacts{}
	}
	f := capFacts{attempts: t.Usage.Attempts, sessions: sessions, extra: t.Usage.ExtraAttempts}
	for _, q := range pending {
		f.merge = f.merge || q.TaskID == t.ID && q.Kind == factoryd.ApprovalMerge
		f.plan = f.plan || q.TaskID == t.ID && q.Kind == factoryd.ApprovalPlan
		var spec factoryd.MergeSpec
		if q.TaskID == t.ID && q.Kind == factoryd.ApprovalMerge && json.Unmarshal(q.Spec, &spec) == nil {
			f.rerun = spec.Factory && spec.RedChecks
			f.returns = !spec.Factory && spec.RedChecks
		}
	}
	f.capped = t.Status == "waiting-human" && !f.merge && !f.plan && (factoryd.AtSessionCap(escalation) || f.sessions > 0 && f.attempts >= f.sessions+f.extra)
	return f
}

// wrongState refuses t, which needs another state, with 409: its state and
// the action that fits it, so a wrong call corrects itself (ruling 263).
func wrongState(t factoryd.Task, f capFacts, needs string) error {
	state := factoryd.StatusMeaning(t.Status)
	if t.Status == "in-review" && f.merge {
		state = "waits at its approval gate, where " + factoryd.GateMeaning(factoryd.GateApproval)
	}
	return refuse(http.StatusConflict, fmt.Sprintf("Card #%d %s, not %s: %s.", t.IssueNumber, state, needs, fitting(t, f)))
}

// splitRunning refuses an action on card t's merge request of pull
// request pr while its split runs: the request stays pending meanwhile.
func splitRunning(t factoryd.Task, pr int64) error {
	return refuse(http.StatusConflict, fmt.Sprintf("Card #%d runs the split of pull request #%d: its cards follow, or its findings come to the card; "+
		"approve, deny or split it once the split ends.", t.IssueNumber, pr))
}

// factoryRedFits is what decides a factory pull request's merge request on
// red checks (roadmap 264).
const factoryRedFits = "rerun_checks runs its red checks again, or deny_merge closes it with your words"

// agentRedFits is what decides an agent pull request's merge request on
// red checks.
const agentRedFits = "deny_merge with request_changes returns it to the agent, or with reject closes it"

// fitting is the action that fits t's state.
func fitting(t factoryd.Task, f capFacts) string {
	switch t.Status {
	case "waiting-human":
		switch {
		case t.IssueNumber == 0:
			return "close ends it (close)"
		case t.Kind == "human":
			return "a human does its work and closes its issue"
		case f.capped:
			return "an answer with the findings grants one session (answer); close ends it"
		case f.rerun:
			return factoryRedFits
		case f.returns:
			return agentRedFits
		case f.merge:
			return "approve_merge or deny_merge decide its merge request"
		case f.plan:
			return "accept_plan, request_changes or reject_plan decide the plan"
		}
		return "answer it (answer)"
	case "waiting-poster":
		return "the poster must answer on the issue"
	case "running":
		return "steer sends its session a message; its pull request comes when the session ends"
	case "queued", "waiting-deps":
		return "nothing to do until it starts"
	case "in-review":
		if f.rerun {
			return factoryRedFits
		}
		if f.returns {
			return agentRedFits
		}
		if f.merge {
			return "approve_merge or deny_merge decide its merge request"
		}
		return "its gates run; nothing to do until they end"
	case "backlog":
		return "move_card starts it"
	case "failed":
		if t.IssueNumber == 0 {
			return "close ends it (close)"
		}
		return "the card is final: a new card continues the work"
	case "done", "closed":
		return "the card is final: a new card continues the work"
	}
	return "wait a moment"
}

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

// threadKinds are the events that leave a chat action's thread on its
// card: a thread that files or steers a card (roadmap 239's card link).
var threadKinds = []string{factoryd.EventCardFiled, factoryd.EventCardSteered}

// record writes what a just did as one event row, their login the actor,
// even once they left: the action took effect. A chat actor's row names
// the autonomy state, the decider and the chat task behind it (roadmap
// 239's recording), and a filing's or a steer's thread (its card link).
func (c *Console) record(ctx context.Context, a actor, repository, taskID string, issue int64, kind string, detail map[string]any) {
	c.event(context.WithoutCancel(ctx), c.eventOf(a, repository, taskID, issue, kind, detail))
}

// eventOf is the row record writes for a's action.
func (c *Console) eventOf(a actor, repository, taskID string, issue int64, kind string, detail map[string]any) factoryd.Event {
	if a.level != "" {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["autonomy"] = a.level
		if a.mergeRule != "" {
			detail["merge_rule"] = a.mergeRule
		}
		if a.decider != "" {
			detail["decider"] = a.decider
		}
		if a.chatTask != "" {
			detail["chat_task"] = a.chatTask
		}
	}
	if a.thread > 0 && slices.Contains(threadKinds, kind) {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["thread"] = a.thread
	}
	return factoryd.Event{Repository: repository, TaskID: taskID, IssueNumber: issue, Kind: kind,
		Actor: a.s.user, Via: a.via, Detail: detail}
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

// approves refuses an approver's action, what, on repository's project to
// anyone but its approvers (rulings 128, 254).
func (c *Console) approves(ctx context.Context, a actor, repository, what string) error {
	if c.isApprover(ctx, a.s.user, repository) {
		return nil
	}
	log.Printf("console: %s is no approver of %s: %s refused", a.s.user, repository, what)
	name := repository
	if p, ok, err := c.projectAt(ctx, repository); err == nil && ok {
		name = p.Name
	}
	return refuse(http.StatusForbidden, fmt.Sprintf("Only the approvers of project %s may %s.", name, what))
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
// lane) and priority when given, its body ending with the line "Size:
// <size>" when given; the webhook or the heal pass makes it a task. It
// answers the repository and the issue's number.
func (c *Console) doFileCard(ctx context.Context, a actor, project, title, body, lane, priority, size, rung, rungReason string) (factoryd.Repo, int64, error) {
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
	case size != "" && !slices.Contains(policy.CardSizes, size):
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "size must be small, medium or large")
	case rung != "" && rung != factoryd.RungCheap && rung != factoryd.RungFrontier:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "rung must be cheap or frontier")
	case rung != "" && strings.TrimSpace(rungReason) == "":
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "a rung needs its one-line reason")
	}
	detail := map[string]any{"lane": lane, "title": title}
	if size != "" {
		body += "\n\nSize: " + size
		detail["size"] = size
	}
	if rung != "" {
		body += "\nRung: " + rung + ", " + strings.TrimSpace(rungReason)
		detail["rung"] = rung
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
	// a closed intake refuses the wake's own filings — the turn the Waker
	// started without the member's ask — while the member's own ask still
	// files and starts
	if a.via == factoryd.ViaChat && a.wake {
		closed, err := c.cfg.Chat.ChatIntakeClosed(ctx, ps[i].Repository)
		if err != nil {
			log.Printf("console: the intake of %s: %v", ps[i].Name, err)
			return factoryd.Repo{}, 0, refuse(http.StatusServiceUnavailable, "cannot read the intake of "+ps[i].Name+"; try again")
		}
		if closed {
			return factoryd.Repo{}, 0, refuse(http.StatusForbidden, "The intake of "+ps[i].Name+" is closed: a wake files nothing. Tell the member what needs filing; their own ask still files and starts.")
		}
	}
	label := factoryd.LaneHuman
	if lane != factoryd.LaneHuman {
		if !slices.ContainsFunc(ps[i].Lanes, func(l policy.Lane) bool { return l.Name == lane }) {
			return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "project "+ps[i].Name+" has no lane "+lane)
		}
		label = laneLabels[lane]
		// the ready check gates the chat agent's filings and starts; the
		// human's buttons and the webhook intake file as they always did
		// (roadmap 239)
		if a.via == factoryd.ViaChat {
			if v, ok := c.inForce(ctx); ok {
				line := factoryd.BacklogRefusal(ctx, c.cfg.Tasks, v.Tree, ps[i].Repository, title)
				if lane != policy.LaneBacklog {
					line = factoryd.FileRefusal(ctx, c.cfg.Tasks, v.Tree, ps[i].Repository, title, title, body, size)
				}
				if line != "" {
					return factoryd.Repo{}, 0, refuse(http.StatusConflict, "The ready check refuses the filing: "+line)
				}
			}
		}
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
	c.record(ctx, a, repo.String(), "", issue.Number, factoryd.EventCardFiled, detail)
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
	why, err := moveRefusal(ctx, t, c.capFactsOf(ctx, t), name, proc, lane, a.s.user, c.cfg.MayMove)
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
	if a.via == factoryd.ViaChat {
		if v, ok := c.inForce(ctx); ok {
			if line := factoryd.StartRefusal(ctx, c.cfg.Tasks, v.Tree, t); line != "" {
				return "", refuse(http.StatusConflict, "The ready check refuses the start: "+line)
			}
		}
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
	if t.Status != "backlog" {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a card still in the backlog")
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
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	accepted, ok := c.awaitAcceptedPlan(ctx, t)
	if !ok {
		if accepted.Lane == policy.LanePlan && accepted.Status != "done" && !factoryd.Final(accepted.Status) {
			return "", refuse(http.StatusConflict, "The plan's cards are still being filed; try again in a moment.")
		}
		return "", wrongState(accepted, c.capFactsOf(ctx, accepted), "an accepted plan")
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
	var backlog, waits []factoryd.Task
	var lines []string
	var tree policy.Tree
	hasTree := false
	if a.via == factoryd.ViaChat {
		if v, ok := c.inForce(ctx); ok {
			tree, hasTree = v.Tree, true
		}
	}
	for _, k := range cards {
		if k.Status != "backlog" || k.IssueNumber == 0 {
			continue
		}
		if hasTree {
			if line := factoryd.StartRefusal(ctx, c.cfg.Tasks, tree, k); line != "" {
				waits = append(waits, k)
				lines = append(lines, fmt.Sprintf("#%d %s", k.IssueNumber, line))
				continue
			}
		}
		backlog = append(backlog, k)
	}
	nums := make([]int64, len(backlog))
	for i, k := range backlog {
		nums[i] = k.IssueNumber
	}
	n := 0
	if len(nums) > 0 {
		if n, err = c.addLabel(ctx, a, t, factoryd.LabelAgent, nums...); err != nil {
			log.Printf("console: %s started plan %s: %d of %d cards labelled", a.s.user, t.ID, n, len(backlog))
			return "", &forgejoRefusal{"label the plan's cards", err}
		}
	}
	log.Printf("console: %s started plan %s via %s: %d cards labelled agent, %d wait", a.s.user, t.ID, a.via, n, len(waits))
	started := map[string]any{"cards": n}
	if len(waits) > 0 {
		started["waiting"] = len(waits)
	}
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanStarted, started)
	var note string
	if len(waits) > 0 {
		note = " The ready check holds " + plural(len(waits), "card") + " in the backlog: " + strings.Join(lines, "; ") + "."
	}
	return fmt.Sprintf("Start plan: %d %s of plan #%d labelled agent.%s", n, plural(n, "card"), t.IssueNumber, note), nil
}

// doRetry files failed plan card id again (factoryd's RetryPlanCard)
// and labels the replacement agent as the member: the label event is its
// move into ready, which intake judges.
func (c *Console) doRetry(ctx context.Context, a actor, id string) (string, error) {
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
	if a.via == factoryd.ViaChat {
		if v, ok := c.inForce(ctx); ok {
			if line := factoryd.StartRefusal(ctx, c.cfg.Tasks, v.Tree, t); line != "" {
				return "", refuse(http.StatusConflict, "The ready check refuses the retry: "+line)
			}
		}
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

// doGo hands the work of card id, a human card that waits on a person,
// to the factory (factoryd's Go): the card's own words become an agent
// card and the card closes on the note, so its wait ends at once.
func (c *Console) doGo(ctx context.Context, a actor, id string) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	switch {
	case t.Kind != "human":
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d is an agent card: the factory runs it.", t.IssueNumber))
	case t.Status != "backlog" && t.Status != "waiting-human":
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d does not wait on you: it is %s.", t.IssueNumber, t.Status))
	}
	bt, _, err := c.cfg.Tasks.BoardTask(ctx, id)
	if err != nil {
		return "", err
	}
	if bt.Escalation != "" {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d waits on your answer, not on the factory.", t.IssueNumber))
	}
	issue, err := c.cfg.GoCard(ctx, t.ID, a.s.user)
	var no factoryd.ErrNoGo
	switch {
	case errors.As(err, &no):
		return "", refuse(http.StatusConflict, no.Error())
	case err != nil:
		log.Printf("console: %s hands %s to the factory: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusInternalServerError, "The card could not be handed to the factory; see the factoryd log.")
	}
	note := fmt.Sprintf("The factory runs it as #%d.", issue)
	if _, err := c.doClose(ctx, a, id, note); err != nil {
		return "", err
	}
	log.Printf("console: %s hands %s (%s#%d) to the factory as #%d via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, issue, a.via)
	return fmt.Sprintf("The factory runs it as #%d; card #%d closes.", issue, t.IssueNumber), nil
}

// planIn is plan task id for a; 409 when it is no plan.
func (c *Console) planIn(ctx context.Context, a actor, id string) (factoryd.Task, error) {
	t, err := c.cardIn(ctx, a, id)
	if err == nil && (t.Lane != policy.LanePlan || t.IssueNumber == 0) {
		err = wrongState(t, c.capFactsOf(ctx, t), "a plan awaiting acceptance")
	}
	return t, err
}

// doAcceptPlan accepts the revision of plan id the approver saw: the
// workflow is signalled, then the acceptance commented as the member (the
// plan's wait reads that comment as no change request, in either order).
// A stale revision answers factoryd.ErrStalePlan; a plan without the
// deploy owner its spec needs answers factoryd's sentence with 409, that
// the planner revises it with no change request (the workflow refuses it
// to the planner), and that the next revision is the one to accept.
func (c *Console) doAcceptPlan(ctx context.Context, a actor, id string, revision int, sha string) (string, error) {
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if revision < 1 || sha == "" {
		return "", refuse(http.StatusBadRequest, "the form names no plan revision; reload the board")
	}
	var stale factoryd.ErrStalePlan
	var open factoryd.ErrOpenQuestions
	if a.via == factoryd.ViaChat {
		if v, ok := c.inForce(ctx); ok {
			if rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, t.ID, revision); err != nil {
				log.Printf("console: the revision %d of plan %s: %v", revision, t.ID, err)
				return "", refuse(http.StatusInternalServerError, "cannot read the plan's revision")
			} else if ok {
				var p struct {
					Cards []json.RawMessage `json:"cards"`
				}
				if err := json.Unmarshal(rev.Plan, &p); err == nil && len(p.Cards) > 0 {
					if n, err := c.cfg.Tasks.CountBacklog(ctx, t.Repository); err != nil {
						log.Printf("console: the backlog of %s: %v", t.Repository, err)
					} else if room := v.Tree.BacklogCapOf(t.Repository) - n; len(p.Cards) > room {
						return "", refuse(http.StatusConflict, fmt.Sprintf("The ready check refuses the acceptance: filing %d cards would pass the backlog cap of %d (%d cards wait now); it waits for room.", len(p.Cards), v.Tree.BacklogCapOf(t.Repository), n))
					}
				}
			}
		}
	}
	switch err := c.cfg.AcceptPlan(ctx, t.ID, revision, sha, a.s.user); {
	case errors.As(err, &stale):
		return "", stale
	case errors.As(err, &open):
		return "", refuse(http.StatusConflict, open.Error())
	case errors.Is(err, factoryd.ErrNoDeployOwner):
		return "", refuse(http.StatusConflict, err.Error()+". The planner revises the plan; no change request is needed. "+
			"The next revision replaces this one: read the plan again before you accept.")
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

// maxPlanAnswer bounds a member's answer to a plan's question, one line.
const maxPlanAnswer = 1000

// doAnswerPlanQuestion answers question idx of revision revision of plan
// id as the member, a writer of its project: factoryd stores the answer
// and signals the plan's workflow, then the answer is posted on the issue
// as theirs (factoryd.PlanAnswerComment), which intake knows for no change
// request.
func (c *Console) doAnswerPlanQuestion(ctx context.Context, a actor, id string, revision, idx int, answer string) (string, error) {
	answer = strings.Join(strings.Fields(answer), " ")
	if answer == "" || utf8.RuneCountInString(answer) > maxPlanAnswer {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("an answer of 1 to %d characters is required", maxPlanAnswer))
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.AnswerPlan(ctx, t.ID, revision, idx, answer, a.s.user); {
	case errors.As(err, &stale):
		return "", refuse(http.StatusConflict, stale.Error())
	case errors.Is(err, factoryd.ErrAnswered):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Question %d of plan #%d is answered already.", idx, t.IssueNumber))
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Plan #%d revision %d has no question %d.", t.IssueNumber, revision, idx))
	case errors.Is(err, factoryd.ErrAnswerUnsignalled):
		log.Printf("console: %s answered question %d of revision %d of plan %s: %v", a.s.user, idx, revision, t.ID, err)
		return "", refuse(http.StatusBadGateway, "The answer is recorded, but factoryd could not pass it to the plan; request_changes sends the plan to the planner with the answers.")
	case err != nil:
		log.Printf("console: %s answers question %d of revision %d of plan %s: %v", a.s.user, idx, revision, t.ID, err)
		return "", refuse(http.StatusConflict, "The answer could not be recorded; see the factoryd log.")
	}
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanQuestionAnswered, map[string]any{"revision": revision, "index": idx, "answer": answer})
	if err := c.comment(ctx, a, t, factoryd.PlanAnswerComment(idx, answer), nil); err != nil {
		return "", &forgejoRefusal{"post the answer on the issue (the answer is recorded)", err}
	}
	log.Printf("console: %s answered question %d of plan %s via %s", a.s.user, idx, t.ID, a.via)
	return fmt.Sprintf("Answered question %d of plan #%d.", idx, t.IssueNumber), nil
}

// doAnswerPlanQuestions answers every question of revision revision of
// plan id that the member names at once, as doAnswerPlanQuestion answers
// one: factoryd stores all answers and signals the plan's workflow, then
// each answer is posted on the issue as theirs (factoryd.
// PlanAnswerComment). An entry without a number or of none, an empty or
// long one, and a repeated number are refused before anything is stored.
func (c *Console) doAnswerPlanQuestions(ctx context.Context, a actor, id string, revision int, answers []PlanAnswerInput) (string, error) {
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	qs, err := c.cfg.Tasks.PlanQuestions(ctx, t.ID, revision)
	if err != nil {
		return "", err
	}
	batch := make([]factoryd.PlanAnswer, 0, len(answers))
	seen := map[int]bool{}
	for _, ans := range answers {
		if ans.Question < 1 || ans.Question > len(qs) {
			return "", refuse(http.StatusBadRequest, fmt.Sprintf("Plan #%d revision %d has no question %d.", t.IssueNumber, revision, ans.Question))
		}
		if seen[ans.Question] {
			return "", refuse(http.StatusBadRequest, fmt.Sprintf("question %d is answered twice", ans.Question))
		}
		seen[ans.Question] = true
		text := strings.Join(strings.Fields(ans.Text), " ")
		if text == "" || utf8.RuneCountInString(text) > maxPlanAnswer {
			return "", refuse(http.StatusBadRequest, fmt.Sprintf("an answer of 1 to %d characters is required", maxPlanAnswer))
		}
		batch = append(batch, factoryd.PlanAnswer{Idx: ans.Question, Text: text})
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.AnswerPlanQuestions(ctx, t.ID, revision, batch, a.s.user); {
	case errors.As(err, &stale):
		return "", refuse(http.StatusConflict, stale.Error())
	case errors.Is(err, factoryd.ErrAnswered):
		return "", refuse(http.StatusConflict, "One of those questions is answered already.")
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Plan #%d revision %d has no such question.", t.IssueNumber, revision))
	case errors.Is(err, factoryd.ErrAnswerUnsignalled):
		log.Printf("console: %s answered %d questions of revision %d of plan %s: %v", a.s.user, len(batch), revision, t.ID, err)
		return "", refuse(http.StatusBadGateway, "The answers are recorded, but factoryd could not pass them to the plan; request_changes sends the plan to the planner with the answers.")
	case err != nil:
		log.Printf("console: %s answers %d questions of revision %d of plan %s: %v", a.s.user, len(batch), revision, t.ID, err)
		return "", refuse(http.StatusConflict, "The answers could not be recorded; see the factoryd log.")
	}
	for _, ans := range batch {
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanQuestionAnswered,
			map[string]any{"revision": revision, "index": ans.Idx, "answer": ans.Text})
		if err := c.comment(ctx, a, t, factoryd.PlanAnswerComment(ans.Idx, ans.Text), nil); err != nil {
			return "", &forgejoRefusal{"post the answer on the issue (the answers are recorded)", err}
		}
	}
	log.Printf("console: %s answered %d questions of plan %s via %s", a.s.user, len(batch), t.ID, a.via)
	note := fmt.Sprintf("Answered %s of plan #%d.", strings.Join(batchLines(batch), ", "), t.IssueNumber)
	return note, nil
}

// batchLines is one answer's sentence part: "question 1: 60 seconds".
func batchLines(batch []factoryd.PlanAnswer) []string {
	lines := make([]string, len(batch))
	for i, ans := range batch {
		lines[i] = fmt.Sprintf("question %d: %s", ans.Idx, ans.Text)
	}
	return lines
}

// doDecideHoldout runs the member's okay or drop of hold-out idx of plan
// id's newest revision: factoryd stores the decision. Nothing is posted
// on the issue: the agent reads it, and the hold-outs stay hidden from
// the agent by routing; the event feed carries the decision.
func (c *Console) doDecideHoldout(ctx context.Context, a actor, id string, revision, idx int, verb string) (string, error) {
	if verb != "okay" && verb != "drop" {
		return "", refuse(http.StatusBadRequest, "okay or drop is required")
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.DecideHoldout(ctx, t.ID, revision, idx, verb, a.s.user); {
	case errors.As(err, &stale):
		return "", refuse(http.StatusConflict, stale.Error())
	case errors.Is(err, factoryd.ErrAnswered):
		return "", refuse(http.StatusConflict, fmt.Sprintf("The confirmation of plan #%d is answered already; its hold-outs are settled.", t.IssueNumber))
	case errors.Is(err, factoryd.ErrHoldoutClick), errors.Is(err, factoryd.ErrNoHoldout):
		return "", refuse(http.StatusConflict, err.Error())
	case err != nil:
		log.Printf("console: %s decides hold-out %d of revision %d of plan %s: %v", a.s.user, idx, revision, t.ID, err)
		return "", refuse(http.StatusConflict, "The decision could not be recorded; see the factoryd log.")
	}
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventHoldoutDecided, map[string]any{"revision": revision, "index": idx, "verb": verb})
	log.Printf("console: %s %s hold-out %d of revision %d of plan %s via %s", a.s.user, verb, idx, revision, t.ID, a.via)
	if verb == "okay" {
		return fmt.Sprintf("Hold-out %d of plan #%d kept out; it runs at the gate, hidden from the agent.", idx, t.IssueNumber), nil
	}
	return fmt.Sprintf("Hold-out %d of plan #%d dropped.", idx, t.IssueNumber), nil
}

// doRequestChanges posts a writer's change request on plan id's issue
// as the member; intake hands it to the planner. The plan's pending
// request is denied as theirs at once: the planner answers a new revision.
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
	pending, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	for _, q := range pending {
		if q.TaskID == t.ID && q.Kind == factoryd.ApprovalPlan {
			err = c.cfg.Approvals.DecideApproval(ctx, q.ID, q.Kind, "denied", a.s.user, "changes requested: "+text)
		}
	}
	if err != nil {
		log.Printf("console: deny the pending request of plan %s: %v", t.ID, err)
	}
	log.Printf("console: %s asked for changes to plan %s via %s", a.s.user, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanChangesRequested, nil)
	return fmt.Sprintf("Your change request is on plan #%d; the planner answers a new revision.", t.IssueNumber), nil
}

// doRejectPlan ends plan task id failed with the writer's reason, which
// is bookkeeping on a final verdict and may be empty.
func (c *Console) doRejectPlan(ctx context.Context, a actor, id, note string) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "a reject reason is at most 64 KiB")
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
// the task, at once since the comment is whole (answerWhole). The event
// names the comment, so intake records no second question_answered when
// its webhook delivers that very comment; at the session cap the answer
// is a grant, and its event session_granted as doGrantSession's.
func (c *Console) doAnswer(ctx context.Context, a actor, id, text string) (string, error) {
	return c.doAnswerValue(ctx, a, id, text, "")
}

// doAnswerValue answers card id with text. value is the clicked choice's
// value when the answer came from a shaped question's button; a typed
// answer's value resolves from the card's question when its words are one
// of the question's labels. A click posts the value alone, no answer
// field, so an empty text with a value takes the choice's own label as
// the answer text; the empty-text refusal then applies only when neither
// text nor value arrived. park and keep waiting record the answer and
// leave the card waiting on its person: no comment is posted, because a
// posted comment would start the attempt the person declined to start.
// fund raises the card's budget by the shortfall its question names while
// the card holds a budget refusal; past a raised cap it posts like any
// answer, the label retrying the card. A worded answer on a running
// agent card steers its session (doSteer) instead of the wrongState
// wall, so the board form and the answer tool, which both ride here,
// reach a session too. A choice card's answer acts by its choice
// (doChoiceAnswer).
func (c *Console) doAnswerValue(ctx context.Context, a actor, id, text, value string) (string, error) {
	text = strings.TrimSpace(text)
	if len(text) > factoryd.MaxAnswer {
		return "", answerFits(text)
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if t.Kind == "agent" && t.Status == "running" && text != "" && c.cfg.Steer != nil {
		return c.doSteer(ctx, a, id, text)
	}
	if choiceCard(t) && t.Status == "waiting-human" && t.IssueNumber != 0 {
		return c.doChoiceAnswer(ctx, a, t, text, value)
	}
	if t.Kind != "agent" || t.Status != "waiting-human" || t.IssueNumber == 0 {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a question or a session cap to answer")
	}
	bt, _, err := c.cfg.Tasks.BoardTask(ctx, id)
	if err != nil {
		return "", err
	}
	if value == "" && bt.Question != nil {
		value = bt.Question.ChoiceValue(text)
	}
	if text == "" && value != "" && bt.Question != nil {
		text = bt.Question.ChoiceLabel(value)
	}
	if err := answerFits(text); err != nil {
		return "", err
	}
	if value == factoryd.AnswerFund && factoryd.BudgetRefusal(c.unfundedSession(ctx, t)) {
		if need := factoryd.BudgetNeed(bt.Escalation); need > 0 {
			if a.via != factoryd.ViaChat {
				return c.doFund(ctx, a, id, need, text, nil)
			}
			p, ok, err := c.projectAt(ctx, t.Repository)
			if err != nil || !ok {
				return "", refuse(http.StatusInternalServerError, "cannot read the projects")
			}
			return c.fundWithinBounds(ctx, a, p.Name, id, need, text)
		}
		return "", refuse(http.StatusConflict, "this answer needs the Fund form on the card: name the amount")
	}
	switch value {
	case factoryd.AnswerPark, factoryd.AnswerKeepWaiting:
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventQuestionAnswered,
			map[string]any{"answer": text, "value": value})
		log.Printf("console: %s answered the escalation of %s via %s: %s, the card stays waiting", a.s.user, t.ID, a.via, value)
		return fmt.Sprintf("Noted: the card keeps waiting; nothing was posted on #%d.", t.IssueNumber), nil
	}
	// the member's own words, matching none of the question's choices, are
	// not an answer: they go to the member's chat, where the agent reads
	// the card and answers with its own judgement. The chat tool's own
	// answers apply as answers; only the card form's words go to the chat.
	// A console without a chat posts the words on the card's issue instead.
	if a.via == factoryd.ViaButton && bt.Question != nil && text != "" && value == "" && c.cfg.Chat != nil && c.cfg.Operator != nil {
		return answerInChat(c, ctx, a, t, text)
	}
	if why := c.unfundedAnswer(ctx, t); why != "" {
		return "", refuse(http.StatusConflict, why)
	}
	if err := c.answerCardValue(ctx, a, t, text, value); err != nil {
		return "", err
	}
	return fmt.Sprintf("Answered #%d.", t.IssueNumber), nil
}

// choiceCard tells a human card factoryd asks on whose choices act: the
// chain stall card and the failed roll card.
func choiceCard(t factoryd.Task) bool {
	return t.Source == factoryd.SourceChainStall || t.Source == factoryd.SourceRollFailed
}

// doChoiceAnswer answers choice card t with the choice value or text
// names, one of its own. The stall card's act on the card's own commit:
// re-run again re-runs the head's stuck runs and keeps the card waiting,
// nothing posted; check again closes the card and lets the watch run on;
// dismiss dismisses the stall until the head changes and closes the card.
// A head that moved on from the card's commit, or concluded, refuses the
// re-run and the dismissal with its own line; check again closes the
// card all the same. The failed roll card's retry posts its label as the
// member's comment, which the roller takes for the retry, and keeps the
// card: the roller closes it and files the new roll; park records the
// answer, nothing posted; close closes the card. Words that name no
// choice are refused with the choices.
func (c *Console) doChoiceAnswer(ctx context.Context, a actor, t factoryd.Task, text, value string) (string, error) {
	bt, _, err := c.cfg.Tasks.BoardTask(ctx, t.ID)
	if err != nil {
		return "", err
	}
	q := bt.Question
	if q == nil {
		q = &factoryd.HumanQuestion{}
	}
	if value == "" {
		value = q.ChoiceValue(text)
	}
	label := q.ChoiceLabel(value)
	if label == "" {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("Card #%d takes one of its choices: %s.", t.IssueNumber, strings.Join(q.Labels(), ", ")))
	}
	var gone factoryd.ErrStallGone
	switch value {
	case factoryd.AnswerRerunAgain:
		switch err := c.cfg.RerunStall(ctx, t.Repository, t.SHA); {
		case errors.As(err, &gone):
			return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d: %s.", t.IssueNumber, gone))
		case errors.Is(err, factoryd.ErrNoRerun):
			return "", refuse(http.StatusConflict, "This Forgejo offers no re-run of an action run (the rerun API came with Forgejo 17): check again or dismiss.")
		case errors.Is(err, factoryd.ErrNothingToRerun):
			return "", refuse(http.StatusConflict, "No run of the head is pending: a skipped run concluded, and a pending required status is its deployer's; neither is re-run. Check again or dismiss.")
		case err != nil:
			log.Printf("console: %s re-runs the stall of %s: %v", a.s.user, t.Repository, err)
			return "", refuse(http.StatusInternalServerError, "The re-run failed; see the factoryd log.")
		}
		log.Printf("console: %s re-ran the stall of %s via %s", a.s.user, t.Repository, a.via)
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventQuestionAnswered, map[string]any{"answer": label, "value": value})
		return fmt.Sprintf("Re-ran the pending run; #%d stays open until the chain moves.", t.IssueNumber), nil
	case factoryd.AnswerDismiss:
		switch err := c.cfg.DismissStall(ctx, t.Repository, t.SHA, a.s.user); {
		case errors.As(err, &gone):
			return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d: %s.", t.IssueNumber, gone))
		case err != nil:
			log.Printf("console: %s dismisses the stall of %s: %v", a.s.user, t.Repository, err)
			return "", refuse(http.StatusInternalServerError, "The stall could not be dismissed; see the factoryd log.")
		}
		return c.doClose(ctx, a, t.ID, label)
	case factoryd.AnswerRetryRoll:
		if err := c.answerCardValue(ctx, a, t, label, value); err != nil {
			return "", err
		}
		return fmt.Sprintf("Answered #%d: the roller starts the roll again and closes the card.", t.IssueNumber), nil
	case factoryd.AnswerPark:
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventQuestionAnswered, map[string]any{"answer": label, "value": value})
		return fmt.Sprintf("Noted: the card keeps waiting; nothing was posted on #%d.", t.IssueNumber), nil
	}
	return c.doClose(ctx, a, t.ID, label) // check again, close
}

// answerInChatNote is doAnswerValue's note when the member's own words
// went to the chat: the answer route reads it and lands the member there.
const answerInChatNote = "The words are in the project chat; the agent answers there."

// answerCard posts text as the member's answer to card t's escalation; at the
// session cap the answer is a grant, and its event session_granted. value,
// when set, is the clicked choice's value and rides on the record.
func (c *Console) answerCard(ctx context.Context, a actor, t factoryd.Task, text string) error {
	return c.answerCardValue(ctx, a, t, text, "")
}

func (c *Console) answerCardValue(ctx context.Context, a actor, t factoryd.Task, text, value string) error {
	kind := factoryd.EventQuestionAnswered
	if c.capFactsOf(ctx, t).capped {
		kind = factoryd.EventSessionGranted
	}
	var posted int64
	if err := c.comment(ctx, a, t, text, &posted); err != nil {
		return &forgejoRefusal{"post the answer", err}
	}
	c.answerWhole(ctx, a, t, posted)
	log.Printf("console: %s answered the escalation of %s via %s", a.s.user, t.ID, a.via)
	detail := commentDetail(posted)
	if value != "" {
		detail["value"] = value
	}
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, kind, detail)
	return nil
}

// doGrantSession grants card id, waiting at its session cap, one more
// session: the member's answer to its findings commented on its issue as
// doAnswer does, which the workflow's wait takes as the grant.
func (c *Console) doGrantSession(ctx context.Context, a actor, id, text string) (string, error) {
	text = strings.TrimSpace(text)
	if err := answerFits(text); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if f := c.capFactsOf(ctx, t); !f.capped || t.IssueNumber == 0 {
		return "", wrongState(t, f, "a card at its session cap")
	}
	if why := c.unfundedAnswer(ctx, t); why != "" {
		return "", refuse(http.StatusConflict, why)
	}
	var posted int64
	if err := c.comment(ctx, a, t, text, &posted); err != nil {
		return "", &forgejoRefusal{"post the answer", err}
	}
	c.answerWhole(ctx, a, t, posted)
	log.Printf("console: %s granted %s a session via %s", a.s.user, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventSessionGranted, commentDetail(posted))
	return fmt.Sprintf("Your answer is on #%d; it grants the card one session.", t.IssueNumber), nil
}

// answerFits refuses an answer the sessions would get cut: each answer
// reaches them as at most factoryd.MaxAnswer bytes, so a longer one is
// refused here, where its author sees why, and the issue never holds more
// than the sessions read.
func answerFits(text string) error {
	if text == "" || len(text) > factoryd.MaxAnswer {
		return refuse(http.StatusBadRequest, fmt.Sprintf("an answer of 1 to %d bytes is required; this one has %d", factoryd.MaxAnswer, len(text)))
	}
	return nil
}

// answerWhole tells card t's workflow that comment posted, the member's
// answer, is whole (Config.AnswerWhole). The answer is on the issue
// either way: a failed signal costs only the quiet window.
func (c *Console) answerWhole(ctx context.Context, a actor, t factoryd.Task, posted int64) {
	if c.cfg.AnswerWhole == nil || posted == 0 || t.Kind == "human" {
		return // a human card runs no workflow: a signal would start one
	}
	if err := c.cfg.AnswerWhole(ctx, t.ID, posted, a.s.user); err != nil {
		log.Printf("console: %s's answer %d to %s not signalled whole, the card resumes after the quiet window: %v", a.s.user, posted, t.ID, err)
	}
}

// unfundedAnswer is the answer path's floor: the refusal a card's remaining
// budget owes an answer, the session it would start being unfunded
// (SessionFunded), "" when one can run. A card with no session yet (no model
// to price) and a failed read answer as before: the workflow's floor refuses
// the session anyway. An answer at the day cap is taken: its session waits
// for the day on its own.
func (c *Console) unfundedAnswer(ctx context.Context, t factoryd.Task) string {
	if why := c.unfundedSession(ctx, t); !factoryd.DayCapped(why) {
		return why
	}
	return ""
}

// unfundedSession is the refusal a card's remaining budget owes a session's
// opening calls (SessionFunded), "" when they can run or the card cannot be
// judged.
func (c *Console) unfundedSession(ctx context.Context, t factoryd.Task) string {
	if c.cfg.SessionFunded == nil || t.ModelAlias == "" {
		return ""
	}
	why, err := c.cfg.SessionFunded(ctx, t.ID, t.ModelAlias)
	if err != nil {
		log.Printf("console: task %s's remaining budget: %v", t.ID, err)
		return ""
	}
	return why
}

// doFund raises card id's budget by eur, an approver's funding of a card
// waiting on a human after a budget refusal (roadmap 210): one store
// transaction raises budget_eur, and the event row is the raise's audit
// trail — the actor, the card, the old and the new budget and the stated
// reason. The note is the member's answer to the card's question, posted as
// doAnswer posts it, so the raise resumes the card in one step. When the
// floor still refuses a session after the raise, the raise and the note
// stand and the response is a refusal naming the floor's reason, so the
// member learns the amount now, not on the card's next question. The raise
// and its card_funded row commit together; a non-nil pool is the month's
// pool in EUR, which the store checks under a lock before it raises.
func (c *Console) doFund(ctx context.Context, a actor, id string, eur float64, note string, pool *float64) (string, error) {
	note = strings.TrimSpace(note)
	switch {
	case !(eur > 0) || math.IsNaN(eur) || math.IsInf(eur, 0):
		return "", refuse(http.StatusBadRequest, "an amount in EUR above zero is required")
	}
	if err := answerFits(note); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if t.Kind != "agent" || t.Status != "waiting-human" || t.IssueNumber == 0 {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a card waiting on a human after a budget refusal")
	}
	switch why := c.unfundedSession(ctx, t); {
	case why == "":
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d's remaining budget can fund a session's opening calls: it needs no raise; answer it (answer).", t.IssueNumber))
	case !factoryd.BudgetRefusal(why):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d needs no raise: no budget lets it start. %s", t.IssueNumber, why))
	}
	from, to, err := c.cfg.Tasks.FundCard(ctx, t.ID, eur, pool,
		c.eventOf(a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardFunded, map[string]any{"reason": note}))
	var spent *factoryd.PoolSpentError
	switch {
	case errors.As(err, &spent):
		return "", refuse(http.StatusConflict, poolSpent(*pool, spent.Funded, eur))
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusNotFound, "There is no card "+id+" in this project.")
	case err != nil:
		log.Printf("console: %s funds %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusInternalServerError, "The budget could not be raised; see the factoryd log.")
	}
	log.Printf("console: %s raised %s's budget by %.2f EUR (%.2f to %.2f) via %s: %s", a.s.user, t.ID, eur, from, to, a.via, note)
	short := c.unfundedSession(ctx, t)
	if err := c.answerCard(ctx, a, t, note); err != nil {
		log.Printf("console: %s funded %s, the answer failed: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusBadGateway, fmt.Sprintf("Raised #%d's budget by %.2f EUR to %.2f, but Forgejo refused the note as the answer; answer the card on the board.", t.IssueNumber, eur, to))
	}
	if short != "" {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Raised #%d's budget by %.2f EUR to %.2f and posted your note as the answer, but a session still cannot start: %s", t.IssueNumber, eur, to, short))
	}
	return fmt.Sprintf("Raised #%d's budget by %.2f EUR to %.2f and posted your note as the answer; the card resumes.", t.IssueNumber, eur, to), nil
}

// doChatFund raises the project's chat task's monthly budget by eur, the
// approver's answer to the chat-budget-spent ask: the pulse and the chat
// stand still until the raise or the month's turn, and one store
// transaction raises budget_eur — the event row is the raise's audit
// trail as a card's is. The next month's chat task starts at the
// policy's monthly budget again, so the raise is this month's alone.
func (c *Console) doChatFund(ctx context.Context, a actor, name string, eur float64, note string) error {
	note = strings.TrimSpace(note)
	switch {
	case !(eur > 0) || math.IsNaN(eur) || math.IsInf(eur, 0):
		return refuse(http.StatusBadRequest, "an amount in EUR above zero is required")
	}
	if err := answerFits(note); err != nil {
		return err
	}
	p, ok, err := c.projectOf(ctx, name)
	switch {
	case err != nil:
		log.Printf("console: the projects: %v", err)
		return refuse(http.StatusInternalServerError, "cannot read the projects")
	case !ok:
		return refuse(http.StatusNotFound, "There is no project "+name+".")
	}
	t, ok, err := c.cfg.Tasks.Task(ctx, factoryd.ChatTaskID(p.Name, time.Now()))
	switch {
	case err != nil:
		log.Printf("console: %s funds the chat of %s: %v", a.s.user, p.Repository, err)
		return refuse(http.StatusInternalServerError, "cannot read the chat task")
	case !ok || t.Kind != factoryd.KindChat || t.Status != "running":
		return refuse(http.StatusConflict, "the project's chat has spent no budget: its task starts with the chat's first turn of the month")
	case t.SpentEUR < t.BudgetEUR:
		return refuse(http.StatusConflict, fmt.Sprintf("the chat's budget of €%.2f is not spent: it needs no raise", t.BudgetEUR))
	}
	from, to, err := c.cfg.Tasks.RaiseTaskBudget(ctx, t.ID, eur)
	switch {
	case errors.Is(err, factoryd.ErrNotFound):
		return refuse(http.StatusNotFound, "There is no project "+name+".")
	case err != nil:
		log.Printf("console: %s funds the chat of %s: %v", a.s.user, p.Repository, err)
		return refuse(http.StatusInternalServerError, "The budget could not be raised; see the factoryd log.")
	}
	log.Printf("console: %s raised the chat of %s's budget by %.2f EUR (%.2f to %.2f) via %s: %s", a.s.user, p.Repository, eur, from, to, a.via, note)
	c.record(ctx, a, p.Repository, t.ID, 0, factoryd.EventChatFunded,
		map[string]any{"from_eur": from, "to_eur": to, "amount_eur": eur, "reason": note})
	return nil
}

// doClose closes card id's issue as the member, its note commented first
// when given; intake ends the card as on any writer's close. A card with
// no issue factoryd closes alone, a failed one too, its note on the row,
// and so a merged card: its merge closed its issue already, and closing
// a closed issue sends no webhook.
func (c *Console) doClose(ctx context.Context, a actor, id, note string) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "a note of up to 64 KiB")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if factoryd.Final(t.Status) && (t.IssueNumber != 0 || t.Status != "failed") {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a card that is not final")
	}
	if t.IssueNumber != 0 && note != "" {
		if err := c.comment(ctx, a, t, note, nil); err != nil {
			return "", &forgejoRefusal{"post the note", err}
		}
	}
	if t.IssueNumber == 0 || t.Status == "merged" {
		if err := c.cfg.CloseCard(ctx, t.ID, a.s.user); err != nil {
			log.Printf("console: %s closes %s: %v", a.s.user, t.ID, err)
			return "", refuse(http.StatusConflict, "The card could not be closed; see the factoryd log.")
		}
		log.Printf("console: %s closed %s (%s, %s) via %s", a.s.user, t.ID, t.Repository, t.Status, a.via)
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardClosed, map[string]any{"note": note})
		if t.IssueNumber != 0 {
			return fmt.Sprintf("Closed #%d.", t.IssueNumber), nil
		}
		return "Closed card " + t.ID + ".", nil
	}
	if err := c.editIssue(ctx, a, t, map[string]string{"state": "closed"}, nil); err != nil {
		return "", &forgejoRefusal{"close the issue", err}
	}
	log.Printf("console: %s closed %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardClosed, map[string]any{"note": note})
	return fmt.Sprintf("Closed #%d; factoryd ends the card.", t.IssueNumber), nil
}

// acceptanceEvidence is card id's newest acceptance green (roadmap 208):
// the row factoryd recorded when one of its sessions ran one of its
// plan's acceptance commands and it exited 0, as AcceptanceEvidence says
// it, and whether there is one. Only green runs are recorded, so a green
// row is evidence; the newest by id wins, whatever order the store reads
// its rows back in.
func (c *Console) acceptanceEvidence(ctx context.Context, id string) (string, bool) {
	rows, err := c.cfg.Events.TaskEvents(ctx, id, 200)
	if err != nil {
		log.Printf("console: task %s's events: %v", id, err)
		return "", false
	}
	var best *factoryd.Event
	for i := range rows {
		e := &rows[i]
		if e.Kind != factoryd.EventAcceptancePassed {
			continue
		}
		if exit, _ := e.Detail["exit"].(float64); exit != 0 {
			continue
		}
		if best == nil || e.ID > best.ID {
			best = e
		}
	}
	if best == nil {
		return "", false
	}
	command, _ := best.Detail["command"].(string)
	session, _ := best.Detail["session"].(string)
	if command == "" {
		return "", false
	}
	return factoryd.AcceptanceEvidence(command, session), true
}

// doCloseAcceptance closes card id done on its acceptance evidence
// (roadmap 208): a card waiting on a human whose event trail shows one of
// its plan's acceptance commands run green in a session needs no answer,
// no further session and no merge — its work is its own evidence. The
// route's access rule admits only the project's approvers, as Fund's does.
func (c *Console) doCloseAcceptance(ctx context.Context, a actor, id string) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if t.Status != "waiting-human" {
		return "", wrongState(t, c.capFactsOf(ctx, t), "closed on its acceptance evidence")
	}
	evidence, ok := c.acceptanceEvidence(ctx, t.ID)
	if !ok {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d shows no acceptance green: no session of it ran one of its plan's acceptance commands with exit 0.", t.IssueNumber))
	}
	if c.cfg.CloseCardDone == nil {
		return "", refuse(http.StatusConflict, "The acceptance close is not wired; see the factoryd log.")
	}
	if err := c.cfg.CloseCardDone(ctx, t.ID, a.s.user, evidence); err != nil {
		log.Printf("console: %s closes %s on its acceptance: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusConflict, "The card could not be closed; see the factoryd log.")
	}
	log.Printf("console: %s closed %s done on %s via %s", a.s.user, t.ID, evidence, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardClosed, map[string]any{"evidence": evidence})
	return fmt.Sprintf("Closed #%d done: %s.", t.IssueNumber, evidence), nil
}

// doSplit has a planner split card id's large pull request into 2 or 3
// cards, which factoryd files and starts once its check passes: the
// member's split is their acceptance (ruling 277).
func (c *Console) doSplit(ctx context.Context, a actor, id string) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	all, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		log.Printf("console: approvals: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the approval requests")
	}
	i := slices.IndexFunc(all, func(q factoryd.ApprovalRequest) bool {
		var spec factoryd.MergeSpec
		return q.TaskID == t.ID && q.Kind == factoryd.ApprovalMerge && json.Unmarshal(q.Spec, &spec) == nil && spec.RiskClass == policy.ClassLarge
	})
	switch {
	case i < 0:
		return "", wrongState(t, c.capFactsOf(ctx, t), "a large pull request at its approval gate")
	case t.Status == "running":
		return "", splitRunning(t, all[i].PRNumber)
	}
	if err := c.cfg.SplitCard(ctx, t.ID, a.s.user); err != nil {
		log.Printf("console: %s splits %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusBadGateway, "The card's workflow could not be signalled; split again.")
	}
	log.Printf("console: %s split %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardSplit, map[string]any{"pr": all[i].PRNumber})
	return fmt.Sprintf("Split #%d: a planner splits pull request #%d into 2 or 3 cards, which start once factoryd's check passes; the card then closes.",
		t.IssueNumber, all[i].PRNumber), nil
}

// editIssue patches card t's issue with fields as the member; out takes
// the issue as Forgejo answers it.
func (c *Console) editIssue(ctx context.Context, a actor, t factoryd.Task, fields map[string]string, out any) error {
	path, err := issuePath(t, "")
	if err != nil {
		return err
	}
	return c.as(ctx, a.s, http.MethodPatch, path, fields, http.StatusCreated, out)
}

// doEdit edits card id's issue as the member, its title, its body or both,
// and makes the card's objective the edited issue's as intake builds it,
// which the next session's brief reads; a running session keeps its
// brief, so a running card refuses.
func (c *Console) doEdit(ctx context.Context, a actor, id, title, body string) (string, error) {
	title = strings.TrimSpace(title)
	if strings.TrimSpace(body) == "" {
		body = ""
	}
	switch {
	case title == "" && body == "":
		return "", refuse(http.StatusBadRequest, "a title or a body is required")
	case utf8.RuneCountInString(title) > 255:
		return "", refuse(http.StatusBadRequest, "a title of 1 to 255 characters is required")
	case len(body) > 64<<10:
		return "", refuse(http.StatusBadRequest, "the description is longer than 64 KiB")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if t.Status == "running" {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a card not in a session")
	}
	if t.IssueNumber == 0 {
		return "", refuse(http.StatusConflict, "The card has no issue yet.")
	}
	fields, detail := map[string]string{}, map[string]any{}
	if title != "" {
		fields["title"], detail["title"] = title, title
	}
	if body != "" {
		fields["body"], detail["body_bytes"] = body, len(body)
	}
	var issue struct{ Title, Body string }
	if err := c.editIssue(ctx, a, t, fields, &issue); err != nil {
		return "", &forgejoRefusal{"edit the issue", err}
	}
	if err := c.cfg.Tasks.SetObjective(ctx, t.ID, factoryd.IssueObjective(issue.Title, issue.Body)); err != nil {
		log.Printf("console: %s edited %s, its objective: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusInternalServerError, "The issue is edited, but the card's objective could not be stored; edit it again.")
	}
	log.Printf("console: %s edited %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardEdited, detail)
	return fmt.Sprintf("Edited #%d; the card's next session reads the edit.", t.IssueNumber), nil
}

// doSetDependency makes card id, which has not started, wait for card
// dependsOn of its project until that one is done, or with remove no
// longer: on the issues as the bot, then in the store, whose edge the
// dependency wait reads. A cycle refuses, and so does an edge in a
// process without the dependencies block, where nothing waits.
func (c *Console) doSetDependency(ctx context.Context, a actor, id, dependsOn string, remove bool) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	d, err := c.cardIn(ctx, a, dependsOn)
	if err == nil && d.Repository != t.Repository {
		err = refuse(http.StatusNotFound, "There is no card "+dependsOn+" in this project.")
	}
	if err != nil {
		return "", err
	}
	switch {
	case t.ID == d.ID:
		return "", refuse(http.StatusBadRequest, "a card cannot depend on itself")
	case !slices.Contains([]string{"backlog", "queued", "waiting-deps"}, t.Status):
		return "", wrongState(t, c.capFactsOf(ctx, t), "a card that has not started")
	case !remove && factoryd.Final(d.Status):
		return "", wrongState(d, c.capFactsOf(ctx, d), "a card that is not final")
	case t.IssueNumber == 0 || d.IssueNumber == 0:
		return "", refuse(http.StatusConflict, "The card has no issue yet.")
	}
	if !remove {
		name, _, err := c.processOfCard(ctx, t)
		if err != nil {
			return "", err
		}
		name = cmp.Or(t.Process, name)
		if v, ok := c.inForce(ctx); !ok || v.Tree.Processes[name].Dependencies == nil {
			return "", refuse(http.StatusConflict, "Process "+name+" has no dependencies block: its cards do not wait on others.")
		}
	}
	deps, err := c.cfg.Tasks.Dependencies(ctx, t.ID)
	if err != nil {
		log.Printf("console: the dependencies of %s: %v", t.ID, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the card's dependencies")
	}
	has := slices.ContainsFunc(deps, func(x factoryd.Dependency) bool { return x.TaskID == d.ID })
	switch {
	case remove && !has:
		return "", refuse(http.StatusConflict, fmt.Sprintf("#%d does not depend on #%d.", t.IssueNumber, d.IssueNumber))
	case has && !remove:
		return "", refuse(http.StatusConflict, fmt.Sprintf("#%d already depends on #%d.", t.IssueNumber, d.IssueNumber))
	case !remove:
		cycle, err := c.reaches(ctx, d.ID, t.ID)
		if err != nil {
			log.Printf("console: the dependencies of %s: %v", d.ID, err)
			return "", refuse(http.StatusInternalServerError, "cannot read the card's dependencies")
		}
		if cycle {
			return "", refuse(http.StatusConflict, fmt.Sprintf("#%d already depends on #%d, so #%d cannot depend on #%d.",
				d.IssueNumber, t.IssueNumber, t.IssueNumber, d.IssueNumber))
		}
	}
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "the task names no repository")
	}
	store, verb := c.cfg.Tasks.AddTaskDeps, "add"
	if remove {
		store, verb = c.cfg.Tasks.RemoveTaskDeps, "remove"
	}
	if err := c.cfg.SetIssueDependency(ctx, repo, t.IssueNumber, d.IssueNumber, remove); err != nil {
		return "", &forgejoRefusal{fmt.Sprintf("%s the dependency of #%d on #%d", verb, t.IssueNumber, d.IssueNumber), err}
	}
	if err := store(ctx, t.ID, []string{d.ID}); err != nil {
		log.Printf("console: %s the dependency of %s on %s: %v", verb, t.ID, d.ID, err)
		return "", refuse(http.StatusInternalServerError, "The issue's dependency is set, but the card's could not be stored; set it again.")
	}
	log.Printf("console: %s set %s on %s (removed %t) via %s", a.s.user, t.ID, d.ID, remove, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventDependencySet, map[string]any{"depends_on": d.ID, "removed": remove})
	if remove {
		return fmt.Sprintf("#%d no longer depends on #%d.", t.IssueNumber, d.IssueNumber), nil
	}
	return fmt.Sprintf("#%d depends on #%d now.", t.IssueNumber, d.IssueNumber), nil
}

// reaches: card from depends on card to, through dependencies not done.
func (c *Console) reaches(ctx context.Context, from, to string) (bool, error) {
	seen := map[string]bool{from: true}
	for next := []string{from}; len(next) > 0; next = next[1:] {
		deps, err := c.cfg.Tasks.Dependencies(ctx, next[0])
		if err != nil {
			return false, err
		}
		for _, x := range deps {
			if x.TaskID == to && x.Status != "done" {
				return true, nil
			}
			if x.Status != "done" && !seen[x.TaskID] {
				seen[x.TaskID] = true
				next = append(next, x.TaskID)
			}
		}
	}
	return false, nil
}

// doComment posts text on card id's issue as the member, a writer of its
// project; posted is the comment's id. On a card waiting for an answer the
// comment is one, bounded as the answer box's.
func (c *Console) doComment(ctx context.Context, a actor, id, text string) (posted int64, note string, err error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64<<10 {
		return 0, "", refuse(http.StatusBadRequest, "a comment of up to 64 KiB is required")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return 0, "", err
	}
	if t.IssueNumber == 0 {
		return 0, "", refuse(http.StatusConflict, "The card has no issue yet.")
	}
	if waitsOnAnswer(t) {
		if err := answerFits(text); err != nil {
			return 0, "", err
		}
	}
	if err := c.comment(ctx, a, t, text, &posted); err != nil {
		return 0, "", &forgejoRefusal{"post the comment", err}
	}
	log.Printf("console: %s commented on %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardCommented, commentDetail(posted))
	return posted, fmt.Sprintf("Commented on #%d.", t.IssueNumber), nil
}

// waitsOnAnswer tells a card on which a writer's comment is an answer the
// sessions get (factoryd's awaitHuman, waitPoster): one waiting on a
// human or the issue's author, outside the plan lane, whose comments go to
// the planner whole as change requests.
func waitsOnAnswer(t factoryd.Task) bool {
	return (t.Status == "waiting-human" || t.Status == "waiting-poster") && t.Lane != policy.LanePlan
}

// doDeleteComment deletes comment posted of card id as the member: the undo
// of doComment.
func (c *Console) doDeleteComment(ctx context.Context, a actor, id string, posted int64) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
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
// supervisor's message path, marked as theirs, every GitHub token in it
// scrubbed before it is sent or recorded.
func (c *Console) doSteer(ctx context.Context, a actor, id, text string) (string, error) {
	text, scrubbed := scrubTokens(strings.TrimSpace(text))
	if text == "" || len(text) > 8<<10 {
		return "", refuse(http.StatusBadRequest, "a message of up to 8 KiB is required")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if t.Status != "running" {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a running session")
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
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardSteered, map[string]any{"text": text})
	note := fmt.Sprintf("The session of #%d has your message.", t.IssueNumber)
	if scrubbed {
		note += " I removed a GitHub token from it; it was not stored or sent."
	}
	return note, nil
}

// doSetSummary is a writer's one-line summary of card id, shown on its face
// instead of the generated title; "" restores the generated one. It
// answers the card as it was.
func (c *Console) doSetSummary(ctx context.Context, a actor, id, summary string) (factoryd.Task, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
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
// which merges as factory-merge. A red-checks merge request merges only
// once the same head's checks turned green again (#1013: a re-run); still
// red, its approval refuses naming what fits the card.
func (c *Console) doApprove(ctx context.Context, a actor, id int64) (string, error) {
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
	case factoryd.ApprovalCriteria:
		if err := c.decide(ctx, a, q, "approved", "granted: one more bounded card"); err != nil {
			return "", err
		}
		n, err := c.cfg.GrantCriteria(ctx, q.TaskID, a.s.user, q.ID)
		if err != nil {
			log.Printf("console: %s granted the gap of %s, the dues failed: %v", a.s.user, q.TaskID, err)
			return "", refuse(http.StatusInternalServerError, "the grant could not be recorded; see the factoryd log")
		}
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"approval": q.ID, "criteria": n, "verb": "granted"})
		return fmt.Sprintf("Granted one more bounded card for %d criteria; the planner drafts it and the board shows it.", n), nil
	case factoryd.ApprovalMerge:
		// a running card with a pending merge request runs its split
		t, _, err := c.cfg.Tasks.Task(ctx, q.TaskID)
		if err != nil {
			log.Printf("console: task %s: %v", q.TaskID, err)
			return "", refuse(http.StatusInternalServerError, "cannot read the task")
		}
		if t.Status == "running" {
			return "", splitRunning(t, q.PRNumber)
		}
		var spec factoryd.MergeSpec
		if err := json.Unmarshal(q.Spec, &spec); err != nil || spec.HeadSHA == "" {
			return "", refuse(http.StatusInternalServerError, "the merge request names no head commit")
		}
		if spec.RedChecks {
			// the flag froze the head red at open; green at the same head
			// now (a re-run) heals the request (#1013), and so does green
			// at the current head after the branch moved on — the head
			// the merge takes, so the review pins it
			green, sha, err := c.mergeHeadGreen(ctx, a, q.Project, spec.HeadSHA, q.PRNumber)
			if err != nil {
				return "", err
			}
			if !green {
				remedy := "return it to the agent with what to change (deny_merge with request_changes, the card's Return) or close it (deny_merge with reject, the card's Close)"
				if spec.Factory {
					remedy = "run them again (rerun_checks, the card's Re-run the checks) or close it with your words (deny_merge, the card's Close)"
				}
				return "", refuse(http.StatusConflict, fmt.Sprintf("Pull request #%d's checks were red when its request opened, so it cannot merge: %s.", q.PRNumber, remedy))
			}
			spec.HeadSHA = sha
		}
		// a card the plan has passed cannot merge (roadmap 291): the
		// request opened before the revision landed
		if t.Source == factoryd.SourcePlan && t.Parent != "" && t.PlanRevision > 0 {
			if rev, ok, err := c.cfg.Tasks.PlanRevision(ctx, t.Parent, 0); err == nil && ok && rev.Revision > t.PlanRevision {
				return "", refuse(http.StatusConflict, fmt.Sprintf("The plan moved to revision %d, the card ran against revision %d, so it cannot merge: deny the merge (deny_merge) and let the planner re-file the plan.", rev.Revision, t.PlanRevision))
			}
		}
		if err := c.review(ctx, a, q, "APPROVED", "Approved in the factory console.", spec.HeadSHA); err != nil {
			return "", &forgejoRefusal{"submit the approving review", err}
		}
		if err := c.cfg.ApproveMerge(ctx, q.TaskID, q.PRNumber, a.s.user); err != nil {
			log.Printf("console: %s approved PR #%d of %s, the signal failed: %v", a.s.user, q.PRNumber, q.TaskID, err)
			return "", refuse(http.StatusBadGateway, "your approving review is on the pull request, but the task's workflow could not be signalled; approve again")
		}
		if c.cfg.RecordHumanCriteria != nil {
			if n, err := c.cfg.RecordHumanCriteria(ctx, q.TaskID, a.s.user); err != nil {
				log.Printf("console: %s approved PR #%d of %s, the human criteria failed to record: %v", a.s.user, q.PRNumber, q.TaskID, err)
			} else if n > 0 {
				c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"pr": q.PRNumber, "criteria": n, "verb": "met"})
			}
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
// request closed as the member, which fails the task (action reject). A
// factory pull request's has no agent to send it back to: either action
// closes the card and its pull request with the reason (roadmap 264).
func (c *Console) doDeny(ctx context.Context, a actor, id int64, action, note string) (string, error) {
	note = strings.TrimSpace(note)
	if note == "" || len(note) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "a reason of up to 64 KiB is required")
	}
	q, err := c.pendingRequest(ctx, a, id)
	if err != nil {
		return "", err
	}
	switch q.Kind {
	case factoryd.ApprovalResource:
		if err := c.decide(ctx, a, q, "denied", note); err != nil {
			return "", err
		}
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeDenied, map[string]any{"approval": q.ID, "kind": "resource"})
		return "Denied the resource.", nil
	case factoryd.ApprovalCriteria:
		if err := c.decide(ctx, a, q, "denied", note); err != nil {
			return "", err
		}
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"approval": q.ID, "verb": "denied"})
		return "Denied the grant; waive a criterion instead if it should not hold.", nil
	case factoryd.ApprovalMerge:
	default:
		return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which the board decides", q.ID, q.Kind))
	}
	var spec factoryd.MergeSpec
	if err := json.Unmarshal(q.Spec, &spec); err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the merge request")
	}
	if spec.Factory {
		if err := c.cfg.CloseFactoryPR(ctx, q.TaskID, q.PRNumber, a.s.user, note); err != nil {
			log.Printf("console: %s closed PR #%d of %s, the signal failed: %v", a.s.user, q.PRNumber, q.TaskID, err)
			return "", refuse(http.StatusBadGateway, "the task's workflow could not be signalled; close it again")
		}
		c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeDenied, map[string]any{"pr": q.PRNumber, "action": "close"})
		return fmt.Sprintf("Closing pull request #%d and its card with your words: no agent works on a pull request factoryd opened.", q.PRNumber), nil
	}
	repo, err := factoryd.SplitRepo(q.Project)
	if err != nil {
		log.Printf("console: approval request %d: %v", q.ID, err)
		return "", refuse(http.StatusInternalServerError, "the request names no repository")
	}
	switch action {
	case "request_changes":
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

// doWaive records the member's waiver of criterion cid of the plan of
// criteria approval request id, with their one-line reason: the criterion
// no longer holds the plan open.
func (c *Console) doWaive(ctx context.Context, a actor, id int64, cid, reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2000 {
		return "", refuse(http.StatusBadRequest, "a one-line reason of up to 2000 characters is required")
	}
	q, err := c.pendingRequest(ctx, a, id)
	if err != nil {
		return "", err
	}
	if q.Kind != factoryd.ApprovalCriteria {
		return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which waives nothing", q.ID, q.Kind))
	}
	switch err := c.cfg.DecideCriterion(ctx, q.TaskID, cid, "waived", a.s.user, reason); {
	case errors.Is(err, factoryd.ErrNoCriterion):
		return "", refuse(http.StatusConflict, fmt.Sprintf("The plan holds no criterion %s; reload the board.", cid))
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no plan %s.", q.TaskID))
	case err != nil:
		log.Printf("console: %s waives %s of %s: %v", a.s.user, cid, q.TaskID, err)
		return "", refuse(http.StatusInternalServerError, "the waiver could not be recorded; see the factoryd log")
	}
	log.Printf("console: %s waived criterion %s of %s via %s", a.s.user, cid, q.TaskID, a.via)
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"approval": q.ID, "criterion": cid, "verdict": "waived"})
	return fmt.Sprintf("Criterion %s waived.", cid), nil
}

// doRerun runs the red checks of a factory pull request card again
// (roadmap 264), a writer's act: its merge request, whose head cannot
// merge, is decided as the member's, and the card asks again once the
// checks settle. Any other request refuses, naming what decides it.
func (c *Console) doRerun(ctx context.Context, a actor, id int64) (string, error) {
	q, err := c.pendingRequest(ctx, a, id)
	if err != nil {
		return "", err
	}
	var spec factoryd.MergeSpec
	if q.Kind == factoryd.ApprovalMerge {
		if err := json.Unmarshal(q.Spec, &spec); err != nil {
			return "", refuse(http.StatusInternalServerError, "cannot read the merge request")
		}
	}
	switch {
	case !spec.Factory:
		return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is no merge request of a pull request factoryd opened: "+
			"its checks run again by a push to its branch, and approve and deny decide it", q.ID))
	case !spec.RedChecks:
		return "", refuse(http.StatusConflict, fmt.Sprintf("Pull request #%d's checks are green: approve it to merge (approve_merge) or close it (deny_merge).", q.PRNumber))
	}
	if err := c.cfg.RerunChecks(ctx, q.TaskID, q.PRNumber, a.s.user); err != nil {
		log.Printf("console: %s re-ran the checks of PR #%d of %s, the signal failed: %v", a.s.user, q.PRNumber, q.TaskID, err)
		return "", refuse(http.StatusBadGateway, "the task's workflow could not be signalled; run the checks again once more")
	}
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeDenied, map[string]any{"pr": q.PRNumber, "action": "rerun_checks"})
	return fmt.Sprintf("Running the checks of pull request #%d again; the card asks again once they settle.", q.PRNumber), nil
}

// doInvite makes login a member of the chat's project, an approver's
// action: a write collaborator of its repository, added with the token
// registration keeps project repositories with, since the approver's own
// may not administer it (ruling 254). A seed project's collaborators are
// Forgejo's to keep.
func (c *Console) doInvite(ctx context.Context, a actor, login string) (string, error) {
	if !policy.ForgejoLogin(login) {
		return "", refuse(http.StatusBadRequest, "a Forgejo user name is required: 1 to 40 letters, digits, dots, dashes and underscores, starting and ending with a letter or digit")
	}
	p, ok, err := c.projectAt(ctx, a.repository)
	switch {
	case err != nil:
		log.Printf("console: projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	case !ok:
		return "", refuse(http.StatusNotFound, "There is no project "+a.repository+".")
	}
	if p.Objects == policy.ObjectsSeed {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Project %s is a seed project, whose collaborators Forgejo keeps: invite %s there.", p.Name, login))
	}
	if err := c.cfg.AddCollaborator(ctx, p.Repo(), login, "write"); err != nil {
		return "", &forgejoRefusal{"add " + login + " as a collaborator", err}
	}
	log.Printf("console: %s invited %s to %s via %s", a.s.user, login, p.Repository, a.via)
	c.record(ctx, a, p.Repository, "", 0, factoryd.EventMemberInvited, map[string]any{"login": login})
	return login + " is a member; the console admits them within a minute.", nil
}
