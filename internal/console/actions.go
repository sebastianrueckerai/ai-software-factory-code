package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
	wake bool
	// birth is the kind of the wake that started the turn
	birth string
	// reads lists the turn's read tool calls: what a rung 2 answer records
	reads func() []factoryd.CaseRead
	// proposed is set when Act ran nothing and left the action to the human
	proposed *bool
	thread   int64 // the chat thread the action came from, which its event names so the card shows the conversation behind it
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
	// timeCap: the cap it waits at is its agent time's
	timeCap bool
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
	return capFactsFrom(t, sessions, bt.Escalation, bt.Question, all)
}

// capFactsFrom are t's facts from the policy's sessions (its own cap where
// it has one), its newest escalation and the pending approval requests; a
// card neither waiting on a human nor in review has none, and only one
// waiting on a human is capped.
func capFactsFrom(t factoryd.Task, sessions int, escalation string, q *factoryd.HumanQuestion, pending []factoryd.ApprovalRequest) capFacts {
	if t.Status != "waiting-human" && t.Status != "in-review" {
		return capFacts{}
	}
	f := capFacts{attempts: t.Usage.Attempts, sessions: factoryd.SessionCap(t, sessions), extra: t.Usage.SessionsAdded()}
	for _, q := range pending {
		f.merge = f.merge || q.TaskID == t.ID && q.Kind == factoryd.ApprovalMerge
		f.plan = f.plan || q.TaskID == t.ID && q.Kind == factoryd.ApprovalPlan
		var spec factoryd.MergeSpec
		if q.TaskID == t.ID && q.Kind == factoryd.ApprovalMerge && json.Unmarshal(q.Spec, &spec) == nil {
			f.rerun = spec.Factory && spec.RedChecks
			f.returns = !spec.Factory && spec.RedChecks
		}
	}
	waits := t.Status == "waiting-human" && !f.merge && !f.plan
	cap := factoryd.QuestionCap(q, escalation)
	f.timeCap = waits && cap == factoryd.CapTime
	f.capped = waits && (cap != "" || f.timeCap || f.sessions > 0 && f.attempts >= f.sessions+f.extra)
	return f
}

// wrongState refuses t, which needs another state, with 409: its state and
// the action that fits it, so a wrong call corrects itself.
func wrongState(t factoryd.Task, f capFacts, needs string) error {
	state := factoryd.StatusMeaning(t.Status)
	if t.Status == "in-review" && f.merge {
		state = "waits at its approval gate, where " + factoryd.GateMeaning(factoryd.GateApproval)
	}
	return refuse(http.StatusConflict, fmt.Sprintf("Card #%d %s, not %s: %s.", t.IssueNumber, state, needs, fitting(t, f)))
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
			return "one of its choices answers it (answer). Close ends it"
		case f.timeCap:
			return "an answer with the findings grants more agent time (answer). Close ends it"
		case f.capped:
			return "an answer with the findings grants one session (answer). Close ends it"
		case f.rerun:
			return factoryRedFits
		case f.returns:
			return agentRedFits
		case f.merge:
			return "approve_merge or deny_merge decide its merge request"
		case f.plan:
			return "answer its open questions (answer). Its next revision with none files the cards, request_changes sends it back to the planner"
		}
		return "answer it (answer)"
	case "waiting-poster":
		return "the poster must answer on the issue"
	case "running":
		return "steer sends its session a message; its pull request comes when the session ends"
	case "queued", "waiting-deps":
		return "nothing to do until it starts"
	case "waiting-image":
		return "nothing to do until its image is pinned and it restarts; close ends it"
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
	case "failed":
		return "close ends it (close)"
	case "done", "closed":
		if t.Track == policy.TrackPlan && t.Status == "done" {
			return "Close with a note ends a plan that still holds production; a new card continues the work"
		}
		return "a new card continues the work"
	}
	return "wait a moment"
}

// forgejoRefusal is a Forgejo call of the action that failed: what the
// action asked Forgejo for, and say — a %s holding the error's words —
// the site's own refusal sentence, empty for the generic one.
type forgejoRefusal struct {
	what string
	err  error
	say  string
}

func (e *forgejoRefusal) Error() string { return "Forgejo refused to " + e.what + ": " + e.err.Error() }

func (e *forgejoRefusal) Unwrap() error { return e.err }

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
		http.Error(w, "The action failed: "+errText(err)+". Try again in a moment.", http.StatusInternalServerError)
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
// refusal names.
func (c *Console) writes(ctx context.Context, a actor, repository, subject string) error {
	ok, err := c.writesRepo(ctx, a.s.user, repository)
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
// anyone but its approvers.
func (c *Console) approves(ctx context.Context, a actor, repository, what string) error {
	if c.isApprover(ctx, a.s.user, repository) {
		return nil
	}
	log.Printf("console: %s is no approver of %s: %s refused", a.s.user, repository, what)
	return refuse(http.StatusForbidden, fmt.Sprintf("Only the approvers of project %s may %s.", c.projectName(ctx, repository), what))
}

// projectName is the name of the project of repository, the repository's
// own when none is read.
func (c *Console) projectName(ctx context.Context, repository string) string {
	if p, ok, err := c.projectAt(ctx, repository); err == nil && ok {
		return p.Name
	}
	return repository
}

// processOfCard is the name of the process card t follows: its project's
// current one, as intake reads it.
func (c *Console) processOfCard(ctx context.Context, t factoryd.Task) (string, error) {
	ps, err := c.coderProjects(ctx)
	if err != nil {
		log.Printf("console: projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	}
	for _, p := range ps {
		if p.Repository == t.Repository {
			return p.Process, nil
		}
	}
	return "", nil
}

// issuePath is the API path of t's issue, with suffix.
func issuePath(t factoryd.Task, suffix string) (string, error) {
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d%s", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), t.IssueNumber, suffix), nil
}

// trackLabels are the issue labels that file a card on each track
// (factoryd.IssueTrack), human a person's card.
var trackLabels = map[string]string{policy.TrackDirect: factoryd.LabelAgent, policy.TrackPlan: factoryd.LabelPlan,
	factoryd.LabelHuman: factoryd.LabelHuman}

// roadmapRefusal is the refusal of a filing for roadmap item that holder
// holds, naming its state and the way forward by it. A holder that is
// built (done or merged) is no card to refile.
func roadmapRefusal(holder factoryd.Task, item int) string {
	head := fmt.Sprintf("Card %s (#%d, %s) already builds roadmap item %d.", holder.ID, holder.IssueNumber, holder.Status, item)
	switch factoryd.RoadmapHold(holder.Status) {
	case "open":
		return head + fmt.Sprintf(" Wait for it to end, or close it and file again with refile_of %d.", holder.IssueNumber)
	case "ended":
		return strings.Replace(head, "already builds", "still holds", 1) + fmt.Sprintf(" File again with refile_of %d.", holder.IssueNumber)
	}
	return strings.Replace(head, "builds", "built", 1) + " Cite a new roadmap item, or file without roadmap_item and refile_of."
}

// doFileCard files an issue on project's repository as the member, labelled
// with track's label (agent for direct, plan, human for a person's card)
// and priority when given, reason the one line why the card takes the
// track, which its card_filed row records; its body ends, after a blank
// line, with the field lines the intake reads, each when given: "Size: <size>",
// "Rung: <rung>, <reason>", "Image: <name>", an image the project pins,
// "Carries: #<n>", the pull request whose head the card's clone
// supplies as its pull branch, "Roadmap: #<n>", the docs/roadmap.md
// item the card builds, and "Refile of #<n>", the issue of the closed or
// failed card that held the item and which this card replaces; the
// webhook or the heal pass makes it a task. A filing that names a roadmap
// item is the roadmap pull: one card per item, held by any card that cites
// it, open or done, unless a closed or failed card's issue is named as
// refiled, so an item another card holds is refused, naming it, before
// any issue is posted. It answers the repository
// and the issue's number. In a wake the chat bot posts the issue, and the
// card_filed row that names the member is written before the answer: an
// unrecorded filing closes its issue and refuses.
func (c *Console) doFileCard(ctx context.Context, a actor, project, title, body, track, reason, priority, size, rung, rungReason, image string, carry int64, roadmapItem int, refileOf int64) (factoryd.Repo, int64, error) {
	title = strings.TrimSpace(title)
	switch {
	case title == "" || utf8.RuneCountInString(title) > 255:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "a title of 1 to 255 characters is required")
	case len(body) > 64<<10:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "the description is longer than 64 KiB")
	case trackLabels[track] == "":
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "track must be direct, plan or human")
	case priority != "" && !slices.Contains(priorities, priority):
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "unknown priority")
	case size != "" && !slices.Contains(policy.CardSizes, size):
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "size must be small, medium or large")
	case rung != "" && rung != factoryd.RungCheap && rung != factoryd.RungFrontier:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "rung must be cheap or frontier")
	case refileOf > 0 && roadmapItem == 0:
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "refile_of needs roadmap_item: it frees that item from the card it names")
	case rung != "" && strings.TrimSpace(rungReason) == "":
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, "a rung needs its one-line reason")
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
	image, err = pinnedImage(ps[i], image)
	if err != nil {
		return factoryd.Repo{}, 0, err
	}
	if _, ok := ps[i].Tracks.Track(track); !ok && track != factoryd.LabelHuman {
		return factoryd.Repo{}, 0, refuse(http.StatusBadRequest, fmt.Sprintf("Project %s follows process %s, which has no plan track: file the card on track direct.", ps[i].Name, ps[i].Process))
	}
	detail := map[string]any{"track": track, "title": title}
	if reason = strings.TrimSpace(reason); reason != "" {
		detail["reason"] = reason
	}
	var fields []string
	if size != "" {
		fields = append(fields, "Size: "+size)
		detail["size"] = size
	}
	if rung != "" {
		fields = append(fields, "Rung: "+rung+", "+strings.TrimSpace(rungReason))
		detail["rung"] = rung
	}
	if image != "" {
		fields = append(fields, "Image: "+image)
		detail["image"] = image
	}
	if carry > 0 {
		fields = append(fields, fmt.Sprintf("Carries: #%d", carry))
		detail["carry"] = carry
	}
	if roadmapItem > 0 {
		fields = append(fields, fmt.Sprintf("Roadmap: #%d", roadmapItem))
		detail["roadmap_item"] = roadmapItem
		if refileOf > 0 {
			fields = append(fields, fmt.Sprintf("Refile of #%d", refileOf))
			detail["refile_of"] = refileOf
		}
	}
	if len(fields) > 0 {
		body += "\n\n" + strings.Join(fields, "\n")
	}
	if roadmapItem > 0 {
		refile := factoryd.AskedRefile(factoryd.IssueObjective(title, body))
		holder, held, err := c.cfg.Tasks.RoadmapHolder(ctx, ps[i].Repository, roadmapItem, refile)
		if err != nil {
			log.Printf("console: the roadmap items of %s: %v", ps[i].Name, err)
			return factoryd.Repo{}, 0, refuse(http.StatusInternalServerError, "cannot read the roadmap items of "+ps[i].Name+"; try again")
		}
		if held {
			return factoryd.Repo{}, 0, refuse(http.StatusConflict, roadmapRefusal(holder, roadmapItem))
		}
	}
	var check func(policy.Tree) string
	if track != factoryd.LabelHuman {
		check = func(tree policy.Tree) string {
			return factoryd.FileRefusal(ctx, c.cfg.Tasks, tree, ps[i].Repository, title, title, body, size)
		}
	}
	if err := c.chatFiling(ctx, a, ps[i], roadmapItem, check); err != nil {
		return factoryd.Repo{}, 0, err
	}
	repo := ps[i].Repo()
	labels, err := c.labels(ctx, a.s, repo)
	if err != nil {
		return repo, 0, &forgejoRefusal{what: "read the labels", err: err}
	}
	var ids []int64
	for _, name := range []string{trackLabels[track], priority} {
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
		return repo, 0, &forgejoRefusal{what: "file the issue", err: err}
	}
	log.Printf("console: %s filed %s#%d (%s) via %s", a.s.user, repo, issue.Number, track, a.via)
	e := c.eventOf(a, repo.String(), "", issue.Number, factoryd.EventCardFiled, detail)
	if !a.s.fixed {
		c.event(context.WithoutCancel(ctx), e)
		return repo, issue.Number, nil
	}
	// the chat bot posted the issue: the intake takes the card's author from
	// this row (factoryd.Intake.Filings), so without it the issue is no card
	if _, err := c.cfg.Events.Record(context.WithoutCancel(ctx), e); err != nil {
		log.Printf("console: %s's filing of %s#%d not recorded, the issue is closed: %v", a.s.user, repo, issue.Number, err)
		filed := factoryd.Task{Repository: repo.String(), IssueNumber: issue.Number}
		if err := c.comment(ctx, a, filed, "factoryd could not record this filing, so it is no card: the project agent closed it.", nil); err != nil {
			log.Printf("console: comment the unrecorded filing %s#%d: %v", repo, issue.Number, err)
		}
		if err := c.editIssue(ctx, a, filed, map[string]string{"state": "closed"}, nil); err != nil {
			log.Printf("console: close the unrecorded filing %s#%d: %v", repo, issue.Number, err)
		}
		return repo, 0, refuse(http.StatusInternalServerError, fmt.Sprintf(
			"The factory could not record the filing of %s#%d, so it is no card; the issue is closed. File the card again.", repo, issue.Number))
	}
	return repo, issue.Number, nil
}

// chatFiling admits a chat filing by a of a card on project p: a closed
// intake stops exactly the roadmap pull (2026-11-01), so it refuses a
// filing that carries a roadmap item while every other filing — the
// member's ask, a need, a wake's reactive card — still files and starts;
// then check, the ready check's line under the policy in force (""
// passes), judges it, nil for a human card, which meets none. The human's
// buttons and the webhook intake file as they always did.
func (c *Console) chatFiling(ctx context.Context, a actor, p factoryd.Project, roadmapItem int, check func(policy.Tree) string) error {
	if a.via != factoryd.ViaChat {
		return nil
	}
	if roadmapItem > 0 {
		closed, err := c.cfg.Chat.ChatIntakeClosed(ctx, p.Repository)
		if err != nil {
			log.Printf("console: the intake of %s: %v", p.Name, err)
			return refuse(http.StatusServiceUnavailable, "cannot read the intake of "+p.Name+"; try again")
		}
		if closed {
			return refuse(http.StatusForbidden, "The intake of "+p.Name+" is closed: the roadmap is not pulled. Tell the human what needs filing; an ask or a need still files.")
		}
	}
	if v, ok := c.inForce(ctx); ok && check != nil {
		if line := check(v.Tree); line != "" {
			return refuse(http.StatusConflict, "The ready check refuses the filing: "+line)
		}
	}
	return nil
}

// pinnedImage is the image a card of project p names: "" for none and
// default, else a name p pins in force; any other name is refused, naming
// p's pins and request_image.
func pinnedImage(p factoryd.Project, image string) (string, error) {
	if image == "default" {
		return "", nil
	}
	if _, ok := p.Images[image]; ok || image == "" {
		return image, nil
	}
	if len(p.Images) == 0 {
		return "", refuse(http.StatusConflict, "Project "+p.Name+" pins no sandbox images. Leave image out for the default, or add the image with request_image.")
	}
	return "", refuse(http.StatusConflict, fmt.Sprintf("Project %s pins no image %s: its images are %s. Name one of them, leave image out for the default, or add the image with request_image.",
		p.Name, image, strings.Join(slices.Sorted(maps.Keys(p.Images)), ", ")))
}

// doRetry files failed card id again (factoryd's RetryCard): the
// replacement is queued on the direct track and starts once a work slot is
// free. Every card that waits on id at the human, its dependency_failed
// case open, has the retry answer that case through Act: the first files
// the replacement, the next name it, and each card goes back to its wait.
// card_retried is the filing answer's, written as it lands.
func (c *Console) doRetry(ctx context.Context, a actor, id string) (string, error) {
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	waiting, err := c.waitsOnRetry(ctx, t)
	if err != nil {
		return "", err
	}
	if len(waiting) > 0 {
		retry := factoryd.Resolution{Action: factoryd.ActRetry, Args: map[string]any{"task_id": t.ID}}
		var said []string
		for _, w := range waiting {
			out, err := c.actOn(ctx, a, w.card, w.caseID, retry, nil)
			if issue, ok := filedReplacement(out.Did); ok { // the filing stands even when the answer failed after it
				c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardRetried, map[string]any{"replacement": issue})
			}
			var no *refusal
			switch {
			case err != nil && len(said) > 0 && errors.As(err, &no):
				return "", refuse(no.status, strings.Join(said, " ")+" The next answer stopped: "+no.msg)
			case err != nil:
				return "", err
			}
			said = append(said, out.Text)
		}
		return strings.Join(said, " "), nil
	}
	issue, err := c.cfg.RetryCard(ctx, t.ID, a.s.user)
	var no factoryd.ErrNoRetry
	switch {
	case errors.As(err, &no):
		return "", refuse(http.StatusConflict, no.Error())
	case err != nil:
		log.Printf("console: %s retries %s: %v", a.s.user, t.ID, err)
		return "", refuse(http.StatusInternalServerError, "The card could not be retried: "+errText(err)+". Press Retry again, or replan the card when it keeps failing.")
	}
	log.Printf("console: %s retried %s (%s#%d) as #%d via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, issue, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardRetried, map[string]any{"replacement": issue})
	return fmt.Sprintf("Retried #%d as #%d; it starts once a work slot is free.", t.IssueNumber, issue), nil
}

// filedReplacement is the issue of the replacement a retry among did
// filed; false when it filed none, as when it named one a retry filed
// before.
func filedReplacement(did []factoryd.CaseAction) (int64, bool) {
	for _, d := range did {
		if issue, ok := d.Args["replacement"].(int64); ok && d.Action == factoryd.ActRetry {
			return issue, true
		}
	}
	return 0, false
}

// waitingCase is a card that waits on another at the human and its live
// case about it.
type waitingCase struct {
	card   factoryd.Task
	caseID int64
}

// waitsOnRetry are the cards that wait on failed card t at the human, each
// with its live dependency_failed case about t.
func (c *Console) waitsOnRetry(ctx context.Context, t factoryd.Task) ([]waitingCase, error) {
	dependents, err := c.cfg.Tasks.Dependents(ctx, t.ID)
	if err != nil {
		log.Printf("console: the dependents of %s: %v", t.ID, err)
		return nil, refuse(http.StatusInternalServerError, "The cards that wait on this one could not be read, so nothing was retried. Try again.")
	}
	var out []waitingCase
	for _, d := range dependents {
		open, err := c.cfg.OpenCasesOf(ctx, d.TaskID)
		if err != nil {
			log.Printf("console: the cases of %s: %v", d.TaskID, err)
			return nil, refuse(http.StatusInternalServerError, "The cases of the cards that wait on this one could not be read, so nothing was retried. Try again.")
		}
		i := slices.IndexFunc(open, func(k factoryd.Case) bool { return k.Kind == factoryd.CaseDependencyFailed && k.StateKey == t.ID })
		if i < 0 {
			continue
		}
		w, ok, err := c.cfg.Tasks.Task(ctx, d.TaskID)
		if err != nil || !ok {
			log.Printf("console: card %s, which waits on %s: %v", d.TaskID, t.ID, err)
			return nil, refuse(http.StatusInternalServerError, "The card that waits on this one could not be read, so nothing was retried. Try again.")
		}
		out = append(out, waitingCase{w, open[i].ID})
	}
	return out, nil
}

// tailContext is the context of the work after a hand-over: the client
// cannot cancel it, a minute bounds it.
func tailContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
}

// goWork hands the work of human card t, which waits on a person, to the
// factory (factoryd's Go): the card's own words become an agent card and
// the card closes on the note, so its wait ends at once. It answers the
// note's comment.
func (c *Console) goWork(ctx context.Context, a actor, t factoryd.Task) (string, int64, error) {
	issue, err := c.cfg.GoCard(ctx, t.ID, a.s.user)
	var no factoryd.ErrNoGo
	switch {
	case errors.As(err, &no):
		return "", 0, refuse(http.StatusConflict, no.Error())
	case err != nil:
		log.Printf("console: %s hands %s to the factory: %v", a.s.user, t.ID, err)
		return "", 0, refuse(http.StatusInternalServerError, "The card could not be handed to the factory: "+errText(err)+". Try again in a moment.")
	}
	_, posted, err := c.closeHumanCard(ctx, a, t, fmt.Sprintf("The factory runs it as #%d.", issue))
	if err != nil {
		return "", 0, err
	}
	log.Printf("console: %s hands %s (%s#%d) to the factory as #%d via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, issue, a.via)
	return fmt.Sprintf("The factory runs it as #%d; card #%d closes.", issue, t.IssueNumber), posted, nil
}

// planIn is plan task id for a; 409 when it is no plan.
func (c *Console) planIn(ctx context.Context, a actor, id string) (factoryd.Task, error) {
	t, err := c.cardIn(ctx, a, id)
	if err == nil && (t.Track != policy.TrackPlan || t.IssueNumber == 0) {
		err = wrongState(t, c.capFactsOf(ctx, t), "a plan card")
	}
	return t, err
}

// maxPlanAnswer bounds a member's answer to a plan's question, one line.
const maxPlanAnswer = 1000

// doAnswerPlanQuestion answers question idx of revision revision of plan
// id as the member, a writer of its project: factoryd stores the answer
// and signals the plan's workflow, then the answer is posted on the issue
// as theirs (factoryd.PlanAnswerComment), which intake knows for no change
// request.
func (c *Console) doAnswerPlanQuestion(ctx context.Context, a actor, id string, revision, idx int, decision, answer string) (string, error) {
	answer = strings.Join(strings.Fields(answer), " ")
	if decision != factoryd.DecisionConfirm && decision != factoryd.DecisionRevise && decision != "" {
		return "", refuse(http.StatusBadRequest, "a confirmation is decided by confirm or revise.")
	}
	switch {
	case answer == "" && decision == factoryd.DecisionRevise:
		return "", refuse(http.StatusBadRequest, "Request changes needs its words: they go to the next planner round.")
	case answer == "" && decision != factoryd.DecisionConfirm, utf8.RuneCountInString(answer) > maxPlanAnswer:
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("an answer of 1 to %d characters is required", maxPlanAnswer))
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.AnswerPlan(ctx, t.ID, revision, idx, decision, answer, a.s.user); {
	case errors.As(err, &stale):
		return "", refuse(http.StatusConflict, stale.Error())
	case errors.Is(err, factoryd.ErrAnswered):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Question %d of plan #%d is answered already.", idx, t.IssueNumber))
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Plan #%d revision %d has no question %d.", t.IssueNumber, revision, idx))
	case errors.Is(err, factoryd.ErrDecisionRequired):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Question %d of plan #%d is its confirmation: Confirm fixes the spec and the criteria, Request changes sends your words to the planner.", idx, t.IssueNumber))
	case errors.Is(err, factoryd.ErrDecisionReserved):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Question %d of plan #%d takes an answer, not a decision.", idx, t.IssueNumber))
	case errors.Is(err, factoryd.ErrReviseWords):
		return "", refuse(http.StatusBadRequest, "Request changes needs its words: they go to the next planner round.")
	case errors.Is(err, factoryd.ErrHoldoutsOpen):
		return "", refuse(http.StatusConflict, fmt.Sprintf("The hold-outs of plan #%d wait for your okay or drop: decide each, then the confirmation.", t.IssueNumber))
	case errors.Is(err, factoryd.ErrAnswerUnsignalled):
		log.Printf("console: %s answered question %d of revision %d of plan %s: %v", a.s.user, idx, revision, t.ID, err)
		return "", refuse(http.StatusBadGateway, "The answer is recorded, but factoryd could not pass it to the plan; request_changes sends the plan to the planner with the answers.")
	case err != nil:
		log.Printf("console: %s answers question %d of revision %d of plan %s: %v", a.s.user, idx, revision, t.ID, err)
		return "", refuse(http.StatusConflict, fmt.Sprintf("The answer could not be recorded: %s. Read the plan again and answer the open questions of its newest revision.", errText(err)))
	}
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanQuestionAnswered, decisionDetail(revision, idx, decision, answer))
	if err := c.comment(ctx, a, t, factoryd.PlanAnswerComment(idx, factoryd.PlanQuestion{Decision: decision, Answer: answer}), nil); err != nil {
		return "", &forgejoRefusal{what: "post the answer on the issue (the answer is recorded)", err: err}
	}
	log.Printf("console: %s answered question %d of plan %s via %s", a.s.user, idx, t.ID, a.via)
	switch decision {
	case factoryd.DecisionConfirm:
		qs, err := c.cfg.Tasks.PlanQuestions(ctx, t.ID, revision)
		if err != nil {
			log.Printf("console: questions of revision %d of plan %s: %v", revision, t.ID, err)
		}
		if slices.ContainsFunc(qs, func(q factoryd.PlanQuestion) bool { return q.Kind != factoryd.QuestionConfirmation }) {
			return fmt.Sprintf("Confirmed plan #%d; the answers go to the next planner round.", t.IssueNumber), nil
		}
		return fmt.Sprintf("Confirmed plan #%d; its cards file.", t.IssueNumber), nil
	case factoryd.DecisionRevise:
		return fmt.Sprintf("Your changes are on plan #%d; the planner answers a new revision.", t.IssueNumber), nil
	}
	return fmt.Sprintf("Answered question %d of plan #%d.", idx, t.IssueNumber), nil
}

// decisionDetail is the answer event's detail, the decision beside the
// words when there is one.
func decisionDetail(revision, idx int, decision, answer string) map[string]any {
	detail := map[string]any{"revision": revision, "index": idx, "answer": answer}
	if decision != "" {
		detail["decision"] = decision
	}
	return detail
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
		if ans.Decision != factoryd.DecisionConfirm && ans.Decision != factoryd.DecisionRevise && ans.Decision != "" {
			return "", refuse(http.StatusBadRequest, "a confirmation is decided by confirm or revise.")
		}
		switch {
		case text == "" && ans.Decision == factoryd.DecisionRevise:
			return "", refuse(http.StatusBadRequest, "Request changes needs its words: they go to the next planner round.")
		case text == "" && ans.Decision != factoryd.DecisionConfirm, utf8.RuneCountInString(text) > maxPlanAnswer:
			return "", refuse(http.StatusBadRequest, fmt.Sprintf("an answer of 1 to %d characters is required", maxPlanAnswer))
		}
		batch = append(batch, factoryd.PlanAnswer{Idx: ans.Question, Text: text, Decision: ans.Decision})
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.AnswerPlanQuestions(ctx, t.ID, revision, batch, a.s.user); {
	case errors.As(err, &stale):
		return "", refuse(http.StatusConflict, stale.Error())
	case errors.Is(err, factoryd.ErrAnswered):
		return "", refuse(http.StatusConflict, "One of those questions is answered already.")
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Plan #%d revision %d has no such question.", t.IssueNumber, revision))
	case errors.Is(err, factoryd.ErrDecisionRequired):
		return "", refuse(http.StatusConflict, "One of those questions is its confirmation: Confirm fixes the spec and the criteria, Request changes sends your words to the planner.")
	case errors.Is(err, factoryd.ErrDecisionReserved):
		return "", refuse(http.StatusConflict, "One of those questions takes an answer, not a decision.")
	case errors.Is(err, factoryd.ErrReviseWords):
		return "", refuse(http.StatusBadRequest, "Request changes needs its words: they go to the next planner round.")
	case errors.Is(err, factoryd.ErrHoldoutsOpen):
		return "", refuse(http.StatusConflict, fmt.Sprintf("The hold-outs of plan #%d wait for your okay or drop: decide each, then the confirmation.", t.IssueNumber))
	case errors.Is(err, factoryd.ErrAnswerUnsignalled):
		log.Printf("console: %s answered %d questions of revision %d of plan %s: %v", a.s.user, len(batch), revision, t.ID, err)
		return "", refuse(http.StatusBadGateway, "The answers are recorded, but factoryd could not pass them to the plan; request_changes sends the plan to the planner with the answers.")
	case err != nil:
		log.Printf("console: %s answers %d questions of revision %d of plan %s: %v", a.s.user, len(batch), revision, t.ID, err)
		return "", refuse(http.StatusConflict, fmt.Sprintf("The answers could not be recorded: %s. Read the plan again and answer the open questions of its newest revision.", errText(err)))
	}
	for _, ans := range batch {
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanQuestionAnswered,
			decisionDetail(revision, ans.Idx, ans.Decision, ans.Text))
		if err := c.comment(ctx, a, t, factoryd.PlanAnswerComment(ans.Idx, factoryd.PlanQuestion{Decision: ans.Decision, Answer: ans.Text}), nil); err != nil {
			return "", &forgejoRefusal{what: "post the answer on the issue (the answers are recorded)", err: err}
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
		lines[i] = fmt.Sprintf("question %d: %s", ans.Idx, factoryd.PlanQuestion{Decision: ans.Decision, Answer: ans.Text}.Reply())
	}
	return lines
}

// doDecideHoldout runs a writer's okay or drop of hold-out idx of plan
// id's newest revision: factoryd stores the decision. Nothing is posted
// on the issue: the agent reads it, and the hold-outs stay hidden from
// the agent by routing; the event feed carries the decision.
func (c *Console) doDecideHoldout(ctx context.Context, a actor, id string, revision, idx int, verb string) error {
	if verb != "okay" && verb != "drop" {
		return refuse(http.StatusBadRequest, "okay or drop is required")
	}
	t, err := c.planIn(ctx, a, id)
	if err != nil {
		return err
	}
	var stale factoryd.ErrStalePlan
	switch err := c.cfg.DecideHoldout(ctx, t.ID, revision, idx, verb, a.s.user); {
	case errors.As(err, &stale):
		return refuse(http.StatusConflict, stale.Error())
	case errors.Is(err, factoryd.ErrAnswered):
		return refuse(http.StatusConflict, fmt.Sprintf("The confirmation of plan #%d is answered already; its hold-outs are settled.", t.IssueNumber))
	case errors.Is(err, factoryd.ErrHoldoutClick), errors.Is(err, factoryd.ErrNoHoldout):
		return refuse(http.StatusConflict, err.Error())
	case err != nil:
		log.Printf("console: %s decides hold-out %d of revision %d of plan %s: %v", a.s.user, idx, revision, t.ID, err)
		return refuse(http.StatusConflict, fmt.Sprintf("The decision could not be recorded: %s. Reload the board and press Okay or Drop again.", errText(err)))
	}
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventHoldoutDecided, map[string]any{"revision": revision, "index": idx, "verb": verb})
	log.Printf("console: %s %s hold-out %d of revision %d of plan %s via %s", a.s.user, verb, idx, revision, t.ID, a.via)
	return nil
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
		return "", &forgejoRefusal{what: "post the change request", err: err}
	}
	pending, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	for _, q := range pending {
		if q.TaskID == t.ID && q.Kind == factoryd.ApprovalPlan {
			err = c.cfg.Approvals.DecideApproval(ctx, q.ID, q.Kind, "denied", a.approver(), "changes requested: "+text)
		}
	}
	if err != nil {
		log.Printf("console: deny the pending request of plan %s: %v", t.ID, err)
	}
	log.Printf("console: %s asked for changes to plan %s via %s", a.s.user, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventPlanChangesRequested, nil)
	return fmt.Sprintf("Your change request is on plan #%d; the planner answers a new revision.", t.IssueNumber), nil
}

// doAnswer answers card id with the human's words: continue, or the
// choice its words name (doAnswerValue).
func (c *Console) doAnswer(ctx context.Context, a actor, id, text string) (string, error) {
	return c.doAnswerValue(ctx, a, id, text, "", "")
}

// doAnswerValue answers card id with text. value is the clicked choice's
// value when the answer came from a shaped question's button; a typed
// answer's value resolves from the card's question when its words are one
// of the question's labels. A click posts the value alone, no answer
// field, so an empty text with a value takes the choice's own label as
// the answer text; the empty-text refusal then applies only when neither
// text nor value arrived. The value maps to its action (answerAction),
// which Act takes on the card's live case: fund raises the budget by the
// amount its value carries, else by the shortfall its budget refusal
// names; close ends the card (doClose); extend names its amounts on the
// Extend form; words alone continue, and at the session cap grant one
// session. A worded answer on a running agent card steers its session
// (doSteer) instead of the wrongState wall, so the board form and the
// answer tool, which both ride here, reach a session too. A human card's
// answer acts by its choice (doHumanAnswer).
func (c *Console) doAnswerValue(ctx context.Context, a actor, id, text, value, instruction string) (string, error) {
	text = strings.TrimSpace(text)
	t, said, done, err := c.routeAnswer(ctx, a, id, text, value, nil)
	if done {
		return said, err
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
	switch value {
	case factoryd.AnswerGrant:
		// a grant carries its one-line instruction, or re-offers the newest
		// denial's note, so the session the press buys is told what the
		// human wants, not only that they want it
		text = c.grantText(ctx, t, text, instruction)
	case factoryd.AnswerClose:
		return c.doClose(ctx, a, id, "")
	case factoryd.AnswerExtend:
		return "", refuse(http.StatusConflict, "an extend names its amounts: use the Extend form on the card, where you give them")
	}
	var eur float64
	if value == factoryd.AnswerFund || strings.HasPrefix(value, factoryd.AnswerFund+":") {
		if eur, err = c.fundShortfall(ctx, t, bt, value); err != nil {
			return "", err
		}
		if eur == 0 {
			value = "" // no budget refusal left: the label answers as words do
		} else if text == "" {
			text = fmt.Sprintf("Fund %.2f EUR and go on.", eur)
		}
	}
	if err := answerFits(text); err != nil {
		return "", err
	}
	// the member's own words, matching none of the question's choices, are
	// not an answer: they go to the member's chat, where the agent reads
	// the card and answers with its own judgement. The chat tool's own
	// answers apply as answers; only the card form's words go to the chat.
	// A console without a chat posts the words on the card's issue instead.
	if a.via == factoryd.ViaButton && bt.Question != nil && text != "" && value == "" && c.cfg.Chat != nil && c.cfg.Operator != nil {
		return answerInChat(c, ctx, a, t, text)
	}
	k, err := c.liveCase(ctx, t)
	if err != nil {
		return "", err
	}
	if value == "" && (k.Kind == factoryd.CaseSessionCap || k.Kind == factoryd.CaseAgentTimeCap) {
		value = factoryd.AnswerGrant // continue finds no room at a cap: words grant, as its form's button says
	}
	res := answerAction(value, text, eur, k, c.cardTime(ctx, t))
	out, err := c.actOn(ctx, a, t, k.ID, res, c.posting(a, t, res))
	if err != nil {
		return "", err
	}
	return out.Text, nil
}

// routeAnswer routes an answer to its work before Act, on every entry (the
// board's button, the chat, the api): a running agent card's words steer its
// session where an answer with an action never does, and a human card's
// choice goes to its work (doHumanAnswer, typed's action its value). It
// refuses nothing about the card's state: Act is the one gate. typed is the
// chat's answer with an action (nil for none). done: routed, with said or err.
func (c *Console) routeAnswer(ctx context.Context, a actor, id, text, value string, typed *factoryd.Resolution) (t factoryd.Task, said string, done bool, err error) {
	if len(text) > factoryd.MaxAnswer {
		return t, "", true, answerFits(text)
	}
	if t, err = c.cardIn(ctx, a, id); err != nil {
		return t, "", true, err
	}
	switch {
	case t.Kind == "agent" && t.Status == "running" && text != "" && typed == nil && c.cfg.Steer != nil:
		said, err = c.doSteer(ctx, a, id, text)
		return t, said, true, err
	case t.Kind == "human" && t.Status == "waiting-human" && t.IssueNumber != 0:
		said, err = c.doHumanAnswer(ctx, a, t, text, value, typed)
		return t, said, true, err
	}
	return t, "", false, nil
}

// answerArgs are the keys of an action's args the answer tool's schema
// names; any other key of the chat's args is dropped.
var answerArgs = []string{"model", "sessions", "agent_time_s", "lines", "checkin_shares", "eur",
	"action", "task_id", "depends_on", "title", "body", "size", "criterion", "verdict",
	"track", "track_reason", "rung", "rung_reason", "image", "carry_pr", "roadmap_item", "refile_of"}

// doAnswerAs answers card id with the typed action the chat names, after
// every answer's gates (routeAnswer): text its note and the comment, args
// passed through to the action, eur the money it draws, the fund's or the
// ceiling raise's own amount (args' eur, which it replaces), else money
// for the budget beside the action. No action is continue.
func (c *Console) doAnswerAs(ctx context.Context, a actor, id, text, action string, args json.RawMessage, eur float64) (string, error) {
	text = strings.TrimSpace(text)
	res, err := typedAnswer(text, action, args, eur)
	if err != nil {
		return "", err
	}
	t, said, done, err := c.routeAnswer(ctx, a, id, text, action, &res)
	if done {
		return said, err
	}
	return c.answerCard(ctx, a, t, res)
}

// doAnswerCase answers case caseID of card id with the typed action the
// chat names, as doAnswerAs answers the card's own ask. A monitor's case
// sits beside the card's run: it is answered whatever the card's status,
// past the run's own ask, and no comment of a's goes on the issue for the
// run to read. The case's answer has one gate, Act: a merge's or a plan's
// criteria case is answered here as any case is. A plan question's case is
// refused with its own door, and a number that names no live case of the card with the card's live
// cases (noLiveCase).
func (c *Console) doAnswerCase(ctx context.Context, a actor, id string, caseID int64, text, action string, args json.RawMessage, eur float64) (string, error) {
	text = strings.TrimSpace(text)
	res, err := typedAnswer(text, action, args, eur)
	if err != nil {
		return "", err
	}
	if len(text) > factoryd.MaxAnswer {
		return "", answerFits(text)
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	open, err := c.cfg.OpenCasesOf(ctx, t.ID)
	if err != nil {
		log.Printf("console: the cases of %s: %v", t.ID, err)
		return "", refuse(http.StatusServiceUnavailable, "The card's cases could not be read, so nothing was answered. Try again.")
	}
	i := slices.IndexFunc(open, func(k factoryd.Case) bool { return k.ID == caseID })
	if i < 0 {
		return "", c.noLiveCase(ctx, t, caseID, open)
	}
	ref := issueRef(t.ID, t.IssueNumber)
	switch k := open[i]; k.Kind {
	case factoryd.CasePlanQuestion, factoryd.CasePlanConfirmation:
		revision, _ := k.Facts["revision"].(float64)
		index, _ := k.Facts["index"].(float64)
		return "", refuse(http.StatusConflict, fmt.Sprintf("Case %d is %s's %s: answer revision %d, question %d without a case number.",
			caseID, ref, strings.ReplaceAll(string(k.Kind), "_", " "), int(revision), int(index)))
	}
	post := c.posting(a, t, res)
	switch {
	case open[i].Kind == factoryd.CaseMonitorFlag:
		post = nil
	case factoryd.ChoiceCase(open[i], t.Kind):
		// a human card's choice is routed to its work here (doHumanAnswer)
		var said string
		var done bool
		if t, said, done, err = c.routeAnswer(ctx, a, id, text, action, &res); done {
			return said, err
		}
	}
	if factoryd.IsBoardAction(res.Action) {
		return c.boardAct(ctx, a, t, open[i], res)
	}
	out, err := c.actOn(ctx, a, t, caseID, res, post)
	if err != nil {
		return "", err
	}
	return out.Text, nil
}

// noLiveCase refuses an answer on case id, which is no live case of card
// t: it says why, a case unknown or another project's, one of t's that
// ended or another card's, and names t's live cases, open, with their
// kinds.
func (c *Console) noLiveCase(ctx context.Context, t factoryd.Task, id int64, open []factoryd.Case) error {
	ref := issueRef(t.ID, t.IssueNumber)
	why := "Card " + ref + " has no case by that number."
	k, err := c.cfg.CaseByID(ctx, id)
	switch {
	case errors.Is(err, factoryd.ErrNotFound):
	case err != nil:
		log.Printf("console: case %d: %v", id, err)
		return refuse(http.StatusServiceUnavailable, "The case could not be read, so nothing was answered. Try again.")
	case k.TaskID == t.ID:
		why = "That case ended."
	case k.Repository == t.Repository:
		other, _, _ := c.cfg.Tasks.Task(ctx, k.TaskID)
		why = "That case is " + issueRef(k.TaskID, other.IssueNumber) + "'s."
	}
	if len(open) == 0 {
		return refuse(http.StatusConflict, why+" Card "+ref+" has no live case.")
	}
	live := make([]string, len(open))
	for i, k := range open {
		live[i] = fmt.Sprintf("%d (%s)", k.ID, k.Kind)
	}
	return refuse(http.StatusConflict, why+" Card "+ref+"'s live cases: "+strings.Join(live, ", ")+".")
}

// typedAnswer is the chat's typed answer: action with its args, continue
// when none, text its note, and eur the money it draws, the fund's or the
// ceiling raise's own amount, else money for the budget beside the action.
func typedAnswer(text, action string, args json.RawMessage, eur float64) (factoryd.Resolution, error) {
	res := factoryd.Resolution{Action: cmp.Or(action, factoryd.ActContinue), Note: text}
	if len(args) > 0 {
		var all map[string]any
		if err := json.Unmarshal(args, &all); err != nil {
			return res, refuse(http.StatusBadRequest, "args is a JSON object of the action's arguments")
		}
		for _, key := range answerArgs {
			if v, ok := all[key]; ok {
				if res.Args == nil {
					res.Args = map[string]any{}
				}
				res.Args[key] = v
			}
		}
	}
	switch {
	case eur < 0 || math.IsNaN(eur) || math.IsInf(eur, 0):
		return res, refuse(http.StatusBadRequest, "fund_eur is an amount in EUR above zero")
	case eur == 0:
	case res.Action == factoryd.ActFund || res.Action == factoryd.ActRaiseCeiling:
		if res.Args == nil {
			res.Args = map[string]any{}
		}
		res.Args["eur"] = eur
	default:
		res.Fund = &policy.Draw{EUR: eur}
	}
	return res, nil
}

// fundShortfall is what a fund choice of card t raises its budget by: the
// amount value carries ("fund:0.73"), else the raise that lifts the
// card's session ceiling (what it has left less the gate reserve) to the
// need its budget refusal names; 0 when the budget refuses nothing any more.
func (c *Console) fundShortfall(ctx context.Context, t factoryd.Task, bt factoryd.BoardTask, value string) (float64, error) {
	if eur := fundAmountOf(value); eur > 0 {
		return eur, nil
	}
	why, err := c.unfundedSession(ctx, t)
	switch {
	case err != nil:
		return 0, unreadBudget()
	case !factoryd.BudgetRefusal(why):
		return 0, nil
	}
	var reserve float64
	if c.cfg.GateReserve != nil {
		reserve = c.cfg.GateReserve()
	}
	if eur := cmp.Or(factoryd.FundRaise(why, t.Left(), reserve), factoryd.FundRaise(bt.Escalation, t.Left(), reserve)); eur > 0 {
		return eur, nil
	}
	return 0, refuse(http.StatusConflict, "this budget refusal names no amount: use the Fund form on the card, where you name it")
}

// answerAction maps a choice value of the five-part question on case k to
// its action, text its note: grant buys what k's stop asks for
// (grantOf), else goes on, fund and fund:N fund eur, go resumes, close
// and successor are their own, extend takes the Extend form's amounts,
// keep checking (and keep waiting on a checks-wait stop) runs the stopped
// checks wait again, park and keep waiting are a note that ends nothing,
// overrule re-verifies with the reason as its note, and words alone
// continue. agentTime is the card draw's agent time, the grant at an agent
// time cap.
func answerAction(value, text string, eur float64, k factoryd.Case, agentTime time.Duration) factoryd.Resolution {
	res := factoryd.Resolution{Action: factoryd.ActContinue, Note: text}
	switch {
	case value == factoryd.AnswerGrant:
		if args, ok := grantOf(k, agentTime); ok {
			res.Action, res.Args = factoryd.ActExtend, args
		}
	case value == factoryd.AnswerFund || strings.HasPrefix(value, factoryd.AnswerFund+":"):
		res.Action, res.Args = factoryd.ActFund, map[string]any{"eur": eur}
	case value == factoryd.AnswerGo:
		res.Action = factoryd.ActResume
	case value == factoryd.AnswerClose, value == factoryd.AnswerSuccessor, value == factoryd.AnswerExtend:
		res.Action = value
	case value == factoryd.AnswerKeepChecking, value == factoryd.AnswerKeepWaiting && k.Kind == factoryd.CaseGateStop && k.Facts["gate"] == factoryd.ActivityWaitChecks:
		res.Action = factoryd.ActWaitChecks
	case value == factoryd.AnswerPark, value == factoryd.AnswerKeepWaiting:
		res.Action = factoryd.ActNote
	case value == factoryd.AnswerOverrule:
		res.Action = factoryd.ActReverify
	}
	return res
}

// grantOf is what a grant on case k buys, the extend's args: one session
// at a session cap, agentTime at an agent time cap, and at a check-in's
// spend stop one more check-in share when the case's checkin_stop names
// the share's, one session when it names the call or time cap's or none.
// ok is false on a stop that asked a question, where a grant goes on.
func grantOf(k factoryd.Case, agentTime time.Duration) (map[string]any, bool) {
	switch {
	case k.Kind == factoryd.CaseAgentTimeCap:
		return map[string]any{"agent_time_s": int(agentTime / time.Second)}, true
	case k.Kind == factoryd.CaseCheckinShare && k.Facts["checkin_stop"] == factoryd.CheckinStopShare:
		return map[string]any{"checkin_shares": 1}, true
	case k.Kind == factoryd.CaseSessionCap, k.Kind == factoryd.CaseCheckinShare:
		return map[string]any{"sessions": 1}, true
	}
	return nil, false
}

// cardTime is card t's draw of agent time: its project's card draw for its
// size in the policy in force, the default card draw's when the policy
// cannot be read.
func (c *Console) cardTime(ctx context.Context, t factoryd.Task) time.Duration {
	if v, ok := c.inForce(ctx); ok {
		return v.EscalationOf(t.Repository).CardDrawOf(t.Size).AgentTime
	}
	return policy.DefaultEscalation.CardDrawOf(t.Size).AgentTime
}

// grantText is a grant press's answer: the instruction its form named, or
// the newest denial's note when the press named none — the session the
// press buys hears what the human wants, not only that they want it.
func (c *Console) grantText(ctx context.Context, t factoryd.Task, label, instruction string) string {
	head := cmp.Or(label, "Grant one session")
	if instruction = strings.TrimSpace(instruction); instruction == "" {
		instruction = c.denialNote(ctx, t)
	}
	if instruction == "" {
		return head
	}
	return head + ": " + instruction
}

// denialNote is the note the newest denied approval of t carries, "" when
// there is none.
func (c *Console) denialNote(ctx context.Context, t factoryd.Task) string {
	if c.cfg.Approvals == nil {
		return ""
	}
	rows, err := c.cfg.Approvals.ApprovalRequests(ctx, "denied")
	if err != nil {
		return ""
	}
	var newest factoryd.ApprovalRequest
	for _, r := range rows {
		if r.TaskID == t.ID && r.DecisionNote != "" && (newest.DecisionNote == "" || r.CreatedAt.After(newest.CreatedAt)) {
			newest = r
		}
	}
	return newest.DecisionNote
}

// doExtend raises card id's caps on any live agent card through Act: its
// sessions, its agent time (minutes of it) and its line bound as an
// extend, its budget as the money drawn beside it, or a fund alone when
// it raises no session, minute or line. On a card waiting at its case
// the note, or when none a line naming the raise, is the answer Act has
// posted as the human's comment; a card waiting on no case reads the
// raise at its next cap check.
func (c *Console) doExtend(ctx context.Context, a actor, id string, sessions, lines, minutes int, eur float64, note string) (string, error) {
	switch {
	case sessions < 0 || lines < 0 || minutes < 0 || eur < 0 || math.IsNaN(eur) || math.IsInf(eur, 0) ||
		(sessions == 0 && lines == 0 && minutes == 0 && !(eur > 0)):
		return "", refuse(http.StatusBadRequest, "an extend raises at least one of the card's numbers: whole numbers of sessions, minutes, lines and an amount in EUR")
	}
	if minutes > factoryd.MaxExtendMinutes {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("an extend raises at most %d minutes of agent time at once", factoryd.MaxExtendMinutes))
	}
	note = cmp.Or(strings.TrimSpace(note), fmt.Sprintf("Extended the card: %s.", raiseLine(sessions, lines, minutes, eur)))
	if err := answerFits(note); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if !extendable(t) {
		return "", wrongState(t, c.capFactsOf(ctx, t), "a live card to extend")
	}
	res := factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": eur}, Note: note}
	if sessions > 0 || lines > 0 || minutes > 0 {
		res.Action, res.Args = factoryd.ActExtend, map[string]any{"sessions": sessions, "lines": lines, "agent_time_s": minutes * 60}
		if eur > 0 {
			res.Fund = &policy.Draw{EUR: eur}
		}
	}
	return c.answerCard(ctx, a, t, res)
}

// extendable: t is an agent card with an issue, in a status short of its end.
func extendable(t factoryd.Task) bool {
	return t.Kind == "agent" && t.IssueNumber != 0 && slices.Contains([]string{"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster", "waiting-image"}, t.Status)
}

// raiseLine names an extend's raise: the sessions, minutes of agent time,
// lines and EUR it adds.
func raiseLine(sessions, lines, minutes int, eur float64) string {
	return fmt.Sprintf("+%d %s, +%d %s of agent time, +%d %s, +%.2f EUR", sessions, plural(sessions, "session"), minutes, plural(minutes, "minute"), lines, plural(lines, "line"), eur)
}

// choiceCard tells a human card factoryd asks on whose choices act: the
// chain stall card, the failed roll card and the held-production ask.
func choiceCard(t factoryd.Task) bool {
	return t.Source == factoryd.SourceChainStall || t.Source == factoryd.SourceRollFailed || t.Source == factoryd.SourceProductionHeld
}

// doHumanAnswer answers human card t with the choice value or text names,
// one of its question's own, and once the choice's work went through
// resolves the card's case through Act with the value. The stall card's
// act on the card's own commit: re-run again re-runs the head's stuck runs
// and keeps the card waiting, nothing posted; check again closes the card
// and lets the watch run on; dismiss dismisses the stall until the head
// changes and closes the card. A head that moved on from the card's
// commit, or concluded, refuses the re-run and the dismissal with its own
// line; check again closes the card all the same. The failed roll card's
// retry resets the failed rolls and closes the card as factoryd's own; the
// roller's next poll files the new roll. park and keep waiting leave the case open with a note,
// nothing posted; go hands the card to the factory; every other choice
// closes the card on its label. The held-production ask's Deploy anyway is
// Act's deploy_now, which the chat's agent may press too; its replans are
// phase 4's doReplan of the plan the value names, and the card closes once
// factoryd accepted. Words that
// name no choice are the human's comment on a card that is no choice
// card, a note on its case, the card still waiting; on a choice card they
// are refused with the choices, as is a value the question does not offer.
func (c *Console) doHumanAnswer(ctx context.Context, a actor, t factoryd.Task, text, value string, typed *factoryd.Resolution) (string, error) {
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
	if label == "" && value == "" && text == "" {
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("Card #%d takes one of its choices: %s. A remark goes on the issue.", t.IssueNumber, strings.Join(q.Labels(), ", ")))
	}
	k, err := c.liveCase(ctx, t)
	if err != nil {
		return "", err
	}
	chosen := factoryd.Resolution{Action: value, Note: label}
	if typed != nil {
		chosen.Fund, chosen.Args = typed.Fund, typed.Args // Act refuses what a choice does not take
	}
	// the choice's work runs inside Act, which accepts the answer first and
	// runs nothing for one it refuses or leaves to the human as a proposal
	var note string
	var work func(context.Context) (int64, error)
	closing := func(ctx context.Context) (int64, error) {
		var posted int64
		var err error
		note, posted, err = c.closeHumanCard(ctx, a, t, label)
		return posted, err
	}
	res, post := chosen, (func(context.Context) (int64, error))(nil)
	var gone factoryd.ErrStallGone
	switch {
	case label == "" && value == "":
		res, note = factoryd.Resolution{Action: factoryd.ActNote, Note: text}, fmt.Sprintf("Commented on #%d; the card keeps waiting.", t.IssueNumber)
		work = func(ctx context.Context) (int64, error) {
			var posted int64
			if err := c.comment(ctx, a, t, text, &posted); err != nil {
				return 0, &forgejoRefusal{what: "post the comment", err: err}
			}
			return posted, nil
		}
	case !choiceCard(t) && slices.Contains([]string{factoryd.AnswerRerunAgain, factoryd.AnswerDismiss, factoryd.AnswerRetryRoll, factoryd.AnswerCheckAgain}, value):
		work = closing
	case t.Source == factoryd.SourceProductionHeld && isReplanChoice(value):
		plan, takeOff, _ := factoryd.ReplanChoice(value)
		work = func(ctx context.Context) (int64, error) {
			var posted int64
			var err error
			note, posted, err = c.answerHoldReplan(ctx, a, t, plan, takeOff, label)
			return posted, err
		}
	case value == factoryd.ActDeployNow:
		if err := c.admit(ctx, accessTable["POST /projects/{name}/deploy-now"], a, t.Repository, ""); err != nil {
			return "", err
		}
		switch _, ok, err := c.projectAt(ctx, t.Repository); {
		case err != nil:
			log.Printf("console: the projects: %v", err)
			return "", refuse(http.StatusInternalServerError, "cannot read the projects")
		case !ok:
			return "", refuse(http.StatusNotFound, "There is no project of "+t.Repository+".")
		}
	case value == factoryd.AnswerRerunAgain:
		work = func(ctx context.Context) (int64, error) {
			switch err := c.cfg.RerunStall(ctx, t.Repository, t.SHA); {
			case errors.As(err, &gone):
				return 0, refuse(http.StatusConflict, fmt.Sprintf("Card #%d: %s.", t.IssueNumber, gone))
			case errors.Is(err, factoryd.ErrNoRerun):
				log.Printf("console: %s re-runs the stall of %s: %v", a.s.user, t.Repository, err)
				return 0, refuse(http.StatusConflict, "Forgejo re-runs no action run of "+t.Repository+
					": it has no rerun API (that came with Forgejo 17), or the project's actions token lacks the right. Check again or dismiss.")
			case errors.Is(err, factoryd.ErrNothingToRerun):
				return 0, refuse(http.StatusConflict, "No run of the head is pending: a skipped run concluded, and a pending required status is its deployer's; neither is re-run. Check again or dismiss.")
			case err != nil:
				log.Printf("console: %s re-runs the stall of %s: %v", a.s.user, t.Repository, err)
				return 0, refuse(http.StatusInternalServerError, "The re-run failed: "+errText(err)+". Try again in a moment.")
			}
			log.Printf("console: %s re-ran the stall of %s via %s", a.s.user, t.Repository, a.via)
			note = fmt.Sprintf("Re-ran the pending run; #%d stays open until the chain moves.", t.IssueNumber)
			return 0, nil
		}
	case value == factoryd.AnswerDismiss:
		work = func(ctx context.Context) (int64, error) {
			switch err := c.cfg.DismissStall(ctx, t.Repository, t.SHA, a.s.user); {
			case errors.As(err, &gone):
				return 0, refuse(http.StatusConflict, fmt.Sprintf("Card #%d: %s.", t.IssueNumber, gone))
			case err != nil:
				log.Printf("console: %s dismisses the stall of %s: %v", a.s.user, t.Repository, err)
				return 0, refuse(http.StatusInternalServerError, "The stall could not be dismissed: "+errText(err)+". Try again in a moment.")
			}
			return closing(ctx)
		}
	case value == factoryd.AnswerRetryRoll:
		work = func(ctx context.Context) (int64, error) {
			if err := c.cfg.RetryRoll(ctx, t); err != nil {
				log.Printf("console: %s retries the roll of %s: %v", a.s.user, t.Repository, scrubText(err.Error()))
				return 0, refuse(http.StatusInternalServerError, "The roll's retry failed: "+errText(err)+". Try again in a moment.")
			}
			return 0, nil
		}
		note = fmt.Sprintf("Answered #%d: the failed rolls are reset, the card is closed and the roller starts the roll again.", t.IssueNumber)
	case value == factoryd.AnswerPark || value == factoryd.AnswerKeepWaiting:
		note = fmt.Sprintf("Noted: the card keeps waiting; nothing was posted on #%d.", t.IssueNumber)
	case value == factoryd.AnswerGo:
		// the agent card is filed with the hand-over: a client that leaves now must not leave the old card waiting
		var stop context.CancelFunc
		ctx, stop = tailContext(ctx)
		defer stop()
		work = func(ctx context.Context) (int64, error) {
			var posted int64
			var err error
			note, posted, err = c.goWork(ctx, a, t)
			return posted, err
		}
	default:
		work = closing // check again, close, any other choice
	}
	out, err := c.actOnWork(ctx, a, t, k.ID, res, post, work)
	if err != nil {
		return "", err
	}
	if out.Proposed || value == factoryd.ActDeployNow {
		return out.Text, nil
	}
	return note, nil
}

// isReplanChoice says choice value names a replan.
func isReplanChoice(value string) bool {
	_, _, ok := factoryd.ReplanChoice(value)
	return ok
}

// answerHoldReplan replans plan on the held-production ask t's answer,
// off main when takeOff, then closes the ask on label. A refusal closes
// nothing. A close that fails after the replan went through is
// named in the note, not answered as an error: a retry would meet ErrNoReplan. The note ends with factoryd's own answer, which says what
// happens next.
func (c *Console) answerHoldReplan(ctx context.Context, a actor, t factoryd.Task, plan string, takeOff bool, label string) (string, int64, error) {
	if err := c.admit(ctx, accessTable["replan"], a, t.Repository, "task "+plan); err != nil {
		return "", 0, err
	}
	pt, err := c.cardIn(ctx, a, plan)
	if err != nil {
		return "", 0, err
	}
	said, err := c.doReplan(ctx, a, plan, fmt.Sprintf("Chosen on #%d, the ask about held production.", t.IssueNumber), takeOff)
	if err != nil {
		return "", 0, err
	}
	what, closed := fmt.Sprintf("Replanning plan #%d on top", pt.IssueNumber), fmt.Sprintf("#%d is closed", t.IssueNumber)
	if takeOff {
		what = fmt.Sprintf("Taking plan #%d off main and replanning", pt.IssueNumber)
	}
	_, posted, err := c.closeIssue(ctx, a, t, label)
	if err != nil {
		log.Printf("console: %s replanned %s on the ask %s, which stays open: %v", a.s.user, plan, t.ID, err)
		closed = fmt.Sprintf("#%d could not be closed, close it by hand", t.IssueNumber)
	}
	return fmt.Sprintf("%s; %s. %s", what, closed, said), posted, nil
}

// answerInChatNote is doAnswerValue's note when the member's own words
// went to the chat: the answer route reads it and lands the member there.
const answerInChatNote = "The words are in the project chat; the agent answers there."

// noCase refuses an answer on a card that waits on a human with no live
// case: a run from before the cases' heal.
const noCase = "this card waits with no open case: the heal pass opens one within a minute"

// caseWake says a's turn is the Waker's wake on a case, rung 2's own turn:
// not the human's thread, which a refill's resume and a retry also run as a
// wake.
func (a actor) caseWake() bool {
	return a.wake && (a.birth == factoryd.WakeAsk || a.birth == factoryd.EventCaseClimbed)
}

// rung is the rung a answers on: the agent's own decision, unconfirmed, is
// rung 2, a human's rung 3.
func (a actor) rung() int {
	if a.decider == "agent" {
		return 2
	}
	return 3
}

// newestCase is the case an answer on card t ends (NewestAskCase); none
// (ID 0) when it has none.
func (c *Console) newestCase(ctx context.Context, t factoryd.Task) (factoryd.Case, error) {
	k, err := c.cfg.NewestAskCase(ctx, t.ID)
	if err != nil {
		log.Printf("console: the cases of %s: %v", t.ID, err)
		return factoryd.Case{}, refuse(http.StatusServiceUnavailable, "The card's cases could not be read, so nothing was answered. Try again.")
	}
	return k, nil
}

// liveCase is newestCase, a card waiting on a human with none refused
// (noCase).
func (c *Console) liveCase(ctx context.Context, t factoryd.Task) (factoryd.Case, error) {
	k, err := c.newestCase(ctx, t)
	if err == nil && k.ID == 0 && t.Status == "waiting-human" {
		err = refuse(http.StatusConflict, noCase)
	}
	return k, err
}

// answerCard answers card t's live case with res through Act, res's note
// posted as a's comment once the answer ends the case.
func (c *Console) answerCard(ctx context.Context, a actor, t factoryd.Task, res factoryd.Resolution) (string, error) {
	k, err := c.liveCase(ctx, t)
	if err != nil {
		return "", err
	}
	if factoryd.IsBoardAction(res.Action) {
		return c.boardAct(ctx, a, t, k, res)
	}
	out, err := c.actOn(ctx, a, t, k.ID, res, c.posting(a, t, res))
	if err != nil {
		return "", err
	}
	return out.Text, nil
}

// posting is the Post that posts a's answer res on card t as a's own
// comment, the one a run from before the cases gate wakes on, as it wakes
// on no comment of factoryd's: res's words, else its sentence. A close
// with no words posts none: its issue's close ends that run.
func (c *Console) posting(a actor, t factoryd.Task, res factoryd.Resolution) func(context.Context) (int64, error) {
	text := res.Note
	if strings.TrimSpace(text) == "" {
		if res.Action == factoryd.ActClose {
			return nil
		}
		text = res.Sentence()
	}
	return func(ctx context.Context) (int64, error) {
		var posted int64
		if err := c.comment(ctx, a, t, text, &posted); err != nil {
			return 0, &forgejoRefusal{what: "post the answer", err: err}
		}
		return posted, nil
	}
}

// actOn answers card t's case caseID (0 for none) with res through Act
// (callAct), its failure as the human reads it (actErr).
func (c *Console) actOn(ctx context.Context, a actor, t factoryd.Task, caseID int64, res factoryd.Resolution, post func(context.Context) (int64, error)) (factoryd.ActResult, error) {
	return c.actOnWork(ctx, a, t, caseID, res, post, nil)
}

// actOnWork is actOn with the work of a human card's choice, which Act runs
// once it accepts the answer (ActInput.Work).
func (c *Console) actOnWork(ctx context.Context, a actor, t factoryd.Task, caseID int64, res factoryd.Resolution, post, work func(context.Context) (int64, error)) (factoryd.ActResult, error) {
	out, err := c.callActWork(ctx, a, t, caseID, res, post, work)
	return out, c.actErr(ctx, a, t, res, err)
}

// callAct answers card t's case caseID (0 for none) with res through Act, as
// a's user on a's rung, the rows carrying a's chat detail and token, post
// the answer's comment (nil for none). Money a human draws is an
// approver's, as the Fund form's. Act's own errors come back as they are.
func (c *Console) callAct(ctx context.Context, a actor, t factoryd.Task, caseID int64, res factoryd.Resolution, post func(context.Context) (int64, error)) (factoryd.ActResult, error) {
	return c.callActWork(ctx, a, t, caseID, res, post, nil)
}

func (c *Console) callActWork(ctx context.Context, a actor, t factoryd.Task, caseID int64, res factoryd.Resolution, post, work func(context.Context) (int64, error)) (factoryd.ActResult, error) {
	if a.rung() == 3 && (res.Action == factoryd.ActFund || res.Action == factoryd.ActRaiseCeiling || res.Fund != nil && res.Fund.EUR > 0) {
		if err := c.admit(ctx, accessTable["fund"], a, t.Repository, "task "+t.ID); err != nil {
			return factoryd.ActResult{}, err
		}
	}
	in := factoryd.ActInput{TaskID: t.ID, CaseID: caseID, By: a.s.user, Via: a.via, Rung: a.rung(), Res: res, Post: post, Work: work,
		Detail: stamped(ctx, c.eventOf(a, t.Repository, t.ID, t.IssueNumber, factoryd.EventQuestionAnswered, nil)).Detail}
	if in.Rung == 2 && a.reads != nil {
		in.Read = a.reads()
	}
	out, err := c.cfg.Act(ctx, in)
	if err == nil && out.Proposed && a.proposed != nil {
		*a.proposed = true
	}
	if err == nil {
		log.Printf("console: %s answered %s with %s on rung %d via %s", a.s.user, t.ID, res.Action, in.Rung, a.via)
	}
	return out, err
}

// actErr is Act's failure of a's answer res on card t as the human reads
// it: Act's refusal, an answer on a case that ended and a retry factoryd
// refuses are a 409 with their words, a ceiling the money passes names the
// raise that makes room, and any other failure is logged and answers a
// 500 that names the log. A console refusal goes as it is.
func (c *Console) actErr(ctx context.Context, a actor, t factoryd.Task, res factoryd.Resolution, err error) error {
	var no *refusal
	var refused factoryd.ErrActRefused
	var ended factoryd.ErrCaseEnded
	var noRetry factoryd.ErrNoRetry
	var capped *factoryd.CardCapError
	var fj *forgejoRefusal
	switch {
	case err == nil:
		return nil
	case errors.As(err, &no), errors.As(err, &fj):
		return err
	case errors.As(err, &ended):
		next := "Reload the board: the card shows what it waits on now."
		if t.Kind == "human" {
			next = fmt.Sprintf("Card #%d has no live case, so a new answer goes on the issue or on a new card.", t.IssueNumber)
		}
		return refuse(http.StatusConflict, err.Error()+". "+next)
	case errors.As(err, &refused), errors.As(err, &noRetry):
		return refuse(http.StatusConflict, err.Error())
	case errors.As(err, &capped):
		return refuse(http.StatusConflict, capSpent(t.IssueNumber, capped.Budget, res, c.ceilingOf(ctx, t), a.via))
	case errors.Is(err, factoryd.ErrNotFound):
		return refuse(http.StatusNotFound, "There is no card "+t.ID+" in this project.")
	}
	log.Printf("console: %s answers %s with %s: %v", a.s.user, t.ID, res.Action, err)
	return refuse(http.StatusInternalServerError, "The answer failed: "+errText(err)+". Reload the board and press the button again.")
}

// ceilingOf is the most card t's budget may hold: its own ceiling, else
// the policy's for its project; 0 when the policy cannot be read.
func (c *Console) ceilingOf(ctx context.Context, t factoryd.Task) float64 {
	if fresh, ok, err := c.cfg.Tasks.Task(ctx, t.ID); err == nil && ok && fresh.CeilingEUR > 0 {
		return fresh.CeilingEUR
	}
	v, ok := c.inForce(ctx)
	if !ok {
		return 0
	}
	_, project, _ := v.ProjectOf(t.Repository)
	return factoryd.CardCeiling(v.Document, project)
}

// doGrantSession grants card id what its cap asks for (grantOf), an
// extend through Act on its live case, the human's answer to its findings
// the note. On a case that asked a question it is an extend of one
// session, which Act's table refuses there, naming the question. A card
// with no case is at no cap, which a grant needs: the Extend form raises a
// running card.
func (c *Console) doGrantSession(ctx context.Context, a actor, id, text string) (string, error) {
	text = strings.TrimSpace(text)
	if err := answerFits(text); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	k, err := c.liveCase(ctx, t)
	switch {
	case err != nil:
		return "", err
	case k.ID == 0:
		return "", refuse(http.StatusConflict, "this card is not at a cap, so there is nothing to grant: the Extend form raises a running card")
	}
	args, ok := grantOf(k, c.cardTime(ctx, t))
	if !ok {
		args = map[string]any{"sessions": 1}
	}
	res := factoryd.Resolution{Action: factoryd.ActExtend, Args: args, Note: text}
	out, err := c.actOn(ctx, a, t, k.ID, res, c.posting(a, t, res))
	if err != nil {
		return "", err
	}
	return out.Text, nil
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

// unfundedSession is the refusal a card's remaining budget owes a session's
// opening calls (SessionFunded), "" when they can run or the card cannot be
// judged; the error is the failed read the caller answers with a verdict. A
// card with no alias yet is priced on its class's session model.
func (c *Console) unfundedSession(ctx context.Context, t factoryd.Task) (string, error) {
	if c.cfg.SessionFunded == nil {
		return "", nil
	}
	return c.cfg.SessionFunded(ctx, t.ID, t.ModelAlias)
}

// unreadBudget is the verdict of a budget read that failed: the answer
// names the state and the fitting action instead of dying mid-request.
func unreadBudget() error {
	return refuse(http.StatusServiceUnavailable, "The card's budget could not be read; the answer was not posted. Try again.")
}

// fundAmountOf reads the EUR a fund choice's value carries ("fund:0.73");
// 0 when it carries none.
func fundAmountOf(value string) float64 {
	s, ok := strings.CutPrefix(value, factoryd.AnswerFund+":")
	if !ok {
		return 0
	}
	eur, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(eur) || math.IsInf(eur, 0) || !(eur > 0) {
		return 0
	}
	return eur
}

// doFund raises card id's budget by eur through Act, a fund on the card's
// live case, or on no case when it waits on none: an approver's, or the
// agent's own from the pool on rung 2. The note is the answer Act has
// posted as the human's comment when the fund ends the case, so the raise
// resumes the card in one step. raise lifts the card's ceiling first, the fund
// drawn beside it (0 for none): the Fund form's ceiling field.
func (c *Console) doFund(ctx context.Context, a actor, id string, eur float64, note string, raise float64) (string, error) {
	note = strings.TrimSpace(note)
	switch {
	case !(eur > 0) || math.IsInf(eur, 0):
		return "", refuse(http.StatusBadRequest, "an amount in EUR above zero is required")
	case !(raise >= 0) || math.IsInf(raise, 0):
		return "", refuse(http.StatusBadRequest, "the ceiling's raise is an amount in EUR, 0 for none")
	}
	if err := answerFits(note); err != nil {
		return "", err
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	res := factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": eur}, Note: note}
	if raise > 0 {
		res = factoryd.Resolution{Action: factoryd.ActRaiseCeiling, Args: map[string]any{"eur": raise}, Fund: &policy.Draw{EUR: eur}, Note: note}
	}
	return c.answerCard(ctx, a, t, res)
}

// doChatFund raises the project's chat task's monthly budget by eur, from
// before to after: the approver's answer to the chat's budget stop, or a
// raise before the money runs out. One store transaction raises
// budget_eur, and the event row is the raise's audit trail as a card's
// is. Every turn the stop ended then goes on (resumeStopped), thread
// skip's aside (a thread key, "" for none). The next month's chat task
// starts at the policy's monthly budget again, so the raise is this
// month's alone.
func (c *Console) doChatFund(ctx context.Context, a actor, name string, eur float64, note, skip string) (from, to float64, err error) {
	note = strings.TrimSpace(note)
	switch {
	case !(eur > 0) || math.IsNaN(eur) || math.IsInf(eur, 0):
		return 0, 0, refuse(http.StatusBadRequest, "an amount in EUR above zero is required")
	}
	if err := answerFits(note); err != nil {
		return 0, 0, err
	}
	p, ok, err := c.projectOf(ctx, name)
	switch {
	case err != nil:
		log.Printf("console: the projects: %v", err)
		return 0, 0, refuse(http.StatusInternalServerError, "cannot read the projects")
	case !ok:
		return 0, 0, refuse(http.StatusNotFound, "There is no project "+name+".")
	}
	t, ok, err := c.cfg.Tasks.Task(ctx, factoryd.ChatTaskID(p.Name, time.Now()))
	switch {
	case err != nil:
		log.Printf("console: %s funds the chat of %s: %v", a.s.user, p.Repository, err)
		return 0, 0, refuse(http.StatusInternalServerError, "cannot read the chat task")
	case !ok || t.Kind != factoryd.KindChat || t.Status != "running":
		return 0, 0, refuse(http.StatusConflict, "the project's chat has spent no budget: its task starts with the chat's first turn of the month")
	}
	from, to, err = c.cfg.Tasks.RaiseTaskBudget(ctx, t.ID, eur)
	switch {
	case errors.Is(err, factoryd.ErrNotFound):
		return 0, 0, refuse(http.StatusNotFound, "There is no project "+name+".")
	case err != nil:
		log.Printf("console: %s funds the chat of %s: %v", a.s.user, p.Repository, err)
		return 0, 0, refuse(http.StatusInternalServerError, "The budget could not be raised: "+errText(err)+". Try again in a moment.")
	}
	log.Printf("console: %s raised the chat of %s's budget by %.2f EUR (%.2f to %.2f) via %s: %s", a.s.user, p.Repository, eur, from, to, a.via, note)
	c.record(ctx, a, p.Repository, t.ID, 0, factoryd.EventChatFunded,
		map[string]any{"from_eur": from, "to_eur": to, "amount_eur": eur, "reason": note})
	c.resumeStopped(context.WithoutCancel(ctx), p, skip)
	return from, to, nil
}

// doClose closes card id as the human, its note commented first when
// given (closeIssue); a failed card closes too. A plan at rest (done,
// failed or closed) closes through closePlan: the note becomes the reason
// of each open criterion's waiver. A
// card waiting on a live case closes through Act: an agent card's Act
// closes its issue as factoryd's recorded close and has the note posted
// after it as the human's, and a human card's close resolves its case once
// its own close went through (choiceDone).
func (c *Console) doClose(ctx context.Context, a actor, id, note string) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > 64<<10 {
		return "", refuse(http.StatusBadRequest, "a note of up to 64 KiB")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	if factoryd.ClosablePlan(t) {
		return c.closePlan(ctx, a, t, note)
	}
	if factoryd.Final(t.Status) && t.Status != "failed" {
		return "", refuse(http.StatusConflict, fmt.Sprintf("Card #%d %s, so there is nothing to close: %s.", t.IssueNumber, factoryd.StatusMeaning(t.Status), fitting(t, capFacts{})))
	}
	k, err := c.newestCase(ctx, t)
	if err != nil {
		return "", err
	}
	closing := factoryd.Resolution{Action: factoryd.ActClose, Note: note}
	if k.ID != 0 && t.Kind != "human" {
		out, err := c.actOn(ctx, a, t, k.ID, closing, c.posting(a, t, closing))
		if err != nil {
			return "", err
		}
		return out.Text, nil
	}
	if k.ID == 0 {
		said, _, err := c.closeIssue(ctx, a, t, note)
		return said, err
	}
	var said string
	out, err := c.actOnWork(ctx, a, t, k.ID, closing, nil, func(ctx context.Context) (int64, error) {
		var posted int64
		var err error
		said, posted, err = c.closeHumanCard(ctx, a, t, note)
		return posted, err
	})
	if err != nil {
		return "", err
	}
	return cmp.Or(said, out.Text), nil
}

// closePlan closes plan t at rest as a. Factoryd checks first what could
// refuse the Close (the role, the note, the plan's state), so a refused Close
// changes nothing; then the issue's own dependencies come off
// (freeForClose), as Forgejo refuses to close over them; then factoryd
// waives every open criterion with the note as the reason, ends the plan's
// criteria asks and, once every card is final, records plan_closed, in one
// write, an approver's act as the waive press is; then the issue hears the
// closing words and closes as a, so the card ends closed. An issue close
// that fails after the write is finished by the sweep.
func (c *Console) closePlan(ctx context.Context, a actor, t factoryd.Task, note string) (string, error) {
	approver := c.isApprover(ctx, a.s.user, t.Repository)
	if err := c.cfg.CheckClosePlan(ctx, t.ID, note, approver); err != nil {
		return "", c.planCloseRefusal(ctx, a, t, err)
	}
	freed := ""
	if t.Status != "closed" {
		var err error
		if freed, err = c.freeForClose(ctx, a, t, ""); err != nil {
			return "", err
		}
	}
	n, words, merged, err := c.cfg.CloseHeldPlan(ctx, t.ID, a.s.user, note, approver)
	if err != nil {
		return "", c.planCloseRefusal(ctx, a, t, err)
	}
	if n > 0 {
		log.Printf("console: %s waived %d criteria of %s via %s", a.s.user, n, t.ID, a.via)
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCriterionDecided, map[string]any{"criteria": n, "verb": "waived", "reason": note})
	}
	if t.IssueNumber != 0 {
		if err := c.commentOnce(ctx, a, t, words); err != nil {
			return "", &forgejoRefusal{what: "post the closing words", err: err}
		}
		if freed != "" {
			if err := c.comment(ctx, a, t, freed, nil); err != nil {
				return "", &forgejoRefusal{what: "post the removed dependencies", err: err}
			}
		}
	}
	if _, err := c.closeIssueNoted(ctx, a, t, words); err != nil {
		return "", err
	}
	card := "plan " + t.ID
	if t.IssueNumber != 0 {
		card = fmt.Sprintf("plan #%d", t.IssueNumber)
	}
	switch {
	case n > 0 && merged:
		return fmt.Sprintf("Closed %s: its open criteria are waived, and its merged cards stay on main.", card), nil
	case n > 0:
		return fmt.Sprintf("Closed %s: its open criteria are waived.", card), nil
	case merged:
		return fmt.Sprintf("Closed %s: its merged cards stay on main.", card), nil
	}
	return fmt.Sprintf("Closed %s.", card), nil
}

// planCloseRefusal is the refusal of a plan's Close that factoryd refused:
// it names the state and the action that fits.
func (c *Console) planCloseRefusal(ctx context.Context, a actor, t factoryd.Task, err error) error {
	switch {
	case errors.Is(err, factoryd.ErrNotApprover):
		return refuse(http.StatusForbidden, fmt.Sprintf("Only the approvers of project %s may close plan #%d while it has open criteria, because Close waives each of them: ask an approver to close it.", c.projectName(ctx, t.Repository), t.IssueNumber))
	case errors.Is(err, factoryd.ErrNoReason):
		return refuse(http.StatusBadRequest, fmt.Sprintf("Plan #%d has open criteria, and Close waives each with your note as the reason: write the note and close again.", t.IssueNumber))
	case errors.Is(err, factoryd.ErrNoteTooLong):
		return refuse(http.StatusBadRequest, fmt.Sprintf("The note of a plan's Close is at most %d bytes, as it becomes each waiver's reason: shorten it.", factoryd.MaxWaiverNote))
	case errors.Is(err, factoryd.ErrPlanClosed):
		return refuse(http.StatusConflict, fmt.Sprintf("Plan #%d is closed already: reload the board.", t.IssueNumber))
	case errors.Is(err, factoryd.ErrNotDonePlan):
		return refuse(http.StatusConflict, fmt.Sprintf("Plan #%d is no longer at rest, so Close no longer fits: reload the board.", t.IssueNumber))
	}
	log.Printf("console: %s closes plan %s: %v", a.s.user, t.ID, err)
	text, _, _ := strings.Cut(errText(err), " (type: ")
	return refuse(http.StatusInternalServerError, "The plan could not be closed: "+text+". Reload the board and press Close again, or ask an approver.")
}

// closeIssue closes card t's issue as a, its note commented first when
// given, and answers the note's comment (0 for none): intake ends the card
// as on any writer's close. A card with no issue factoryd closes alone, its
// note on the row, and so a merged card: its merge closed its issue
// already, and closing a closed issue sends no webhook.
func (c *Console) closeIssue(ctx context.Context, a actor, t factoryd.Task, note string) (string, int64, error) {
	return c.closeIssueOrdered(ctx, a, t, note, false)
}

// closeHumanCard is closeIssue for a human card's choice, which Act answers
// once the work went through: the issue closes first, so a close Forgejo
// refuses posts no note and the case stays at the human with its issue
// open. A note that then fails to post is logged, for the close stands.
func (c *Console) closeHumanCard(ctx context.Context, a actor, t factoryd.Task, note string) (string, int64, error) {
	return c.closeIssueOrdered(ctx, a, t, note, true)
}

func (c *Console) closeIssueOrdered(ctx context.Context, a actor, t factoryd.Task, note string, closeFirst bool) (string, int64, error) {
	var posted int64
	note, err := c.freeForClose(ctx, a, t, note)
	if err != nil {
		return "", 0, err
	}
	if closeFirst {
		said, err := c.closeIssueNoted(ctx, a, t, note)
		if err != nil {
			return "", 0, err
		}
		if t.IssueNumber != 0 && note != "" {
			if err := c.comment(ctx, a, t, note, &posted); err != nil {
				log.Printf("console: %s closed %s, and its note did not post: %v", a.s.user, t.ID, err)
			}
		}
		return said, posted, nil
	}
	if t.IssueNumber != 0 && note != "" {
		if err := c.comment(ctx, a, t, note, &posted); err != nil {
			return "", 0, &forgejoRefusal{what: "post the note", err: err}
		}
	}
	said, err := c.closeIssueNoted(ctx, a, t, note)
	return said, posted, err
}

// commentOnce posts text on card t's issue as a unless a comment with that
// body stands already: whichever of the console and the sweep posts a
// plan's closing words first, the issue carries them once.
func (c *Console) commentOnce(ctx context.Context, a actor, t factoryd.Task, text string) error {
	path, err := issuePath(t, "/comments")
	if err != nil {
		return err
	}
	var have []struct {
		Body string `json:"body"`
	}
	if err := c.as(ctx, a.s, http.MethodGet, path, nil, http.StatusOK, &have); err != nil {
		return err
	}
	if slices.ContainsFunc(have, func(h struct {
		Body string `json:"body"`
	}) bool {
		return h.Body == text
	}) {
		return nil
	}
	return c.comment(ctx, a, t, text, nil)
}

// closeIssueNoted is closeIssue after its note was posted: it closes the
// issue, or the card alone, and records note on the close.
func (c *Console) closeIssueNoted(ctx context.Context, a actor, t factoryd.Task, note string) (string, error) {
	if t.IssueNumber == 0 || t.Status == "merged" {
		if err := c.cfg.CloseCard(ctx, t.ID, a.s.user); err != nil {
			log.Printf("console: %s closes %s: %v", a.s.user, t.ID, err)
			return "", refuse(http.StatusConflict, "The card could not be closed: "+errText(err)+". Reload the board and press the button again.")
		}
		log.Printf("console: %s closed %s (%s, %s) via %s", a.s.user, t.ID, t.Repository, t.Status, a.via)
		c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardClosed, map[string]any{"note": note})
		if t.IssueNumber != 0 {
			return fmt.Sprintf("Closed #%d.", t.IssueNumber), nil
		}
		return "Closed card " + t.ID + ".", nil
	}
	if err := c.editIssue(ctx, a, t, map[string]string{"state": "closed"}, nil); err != nil {
		return "", &forgejoRefusal{what: "close the issue", err: err}
	}
	log.Printf("console: %s closed %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardClosed, map[string]any{"note": note})
	return fmt.Sprintf("Closed #%d; factoryd ends the card.", t.IssueNumber), nil
}

// freeForClose removes the open dependencies card t's issue holds before
// its close (factoryd.DropOpenDependencies, through the human's session) and
// answers note with the sentence naming them added; a merged card or one
// without an issue is left as it is. A failed read or removal refuses the
// close. The note names what this call removed: a retry after a failed
// comment or a partly done removal does not name the edges gone before, and
// the issue's timeline in Forgejo shows each removal.
func (c *Console) freeForClose(ctx context.Context, a actor, t factoryd.Task, note string) (string, error) {
	if t.IssueNumber == 0 || t.Status == "merged" {
		return note, nil
	}
	repo, err := factoryd.SplitRepo(t.Repository)
	if err != nil {
		return "", err
	}
	freed, err := factoryd.DropOpenDependencies(ctx, sessionEdges{c, a}, repo, t.IssueNumber)
	if err != nil {
		return "", dependencyRefused(t, err)
	}
	if len(freed) == 0 {
		return note, nil
	}
	return strings.TrimSpace(note + "\n\n" + factoryd.NoLongerWaits(freed)), nil
}

// dependencyRefused is the refusal of a close whose dependency read or
// removal failed. A removal Forgejo refuses for good (a 4xx: no right to the
// edge, a cross-repository edge it will not touch) names the edge and the
// action that fits, as retrying changes nothing; any other failure is a
// Forgejo refusal to retry.
func dependencyRefused(t factoryd.Task, err error) error {
	var dr *factoryd.DependencyRefusal
	if !errors.As(err, &dr) {
		return &forgejoRefusal{what: fmt.Sprintf("read the dependencies of #%d", t.IssueNumber), err: err}
	}
	edge := fmt.Sprintf("%s/%s#%d", dr.Dep.Repo.Owner, dr.Dep.Repo.Name, dr.Dep.Number)
	var ae *apiError
	if errors.As(dr.Err, &ae) && ae.status >= 400 && ae.status < 500 {
		return refuse(http.StatusBadGateway, fmt.Sprintf("Forgejo refused to remove #%d's dependency on %s: %s. Remove that dependency in Forgejo, then press Close again.",
			t.IssueNumber, edge, forgejoCause(dr.Err)))
	}
	return &forgejoRefusal{what: fmt.Sprintf("remove #%d's dependency on %s", t.IssueNumber, edge), err: dr.Err}
}

// sessionEdges is the actor's Forgejo session as factoryd.DependencyEdges.
type sessionEdges struct {
	c *Console
	a actor
}

func (e sessionEdges) IssueDependencies(ctx context.Context, owner, name string, number int64) ([]factoryd.IssueDependency, error) {
	got, err := factoryd.GetPages[struct {
		Number     int64  `json:"number"`
		State      string `json:"state"`
		Repository struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		} `json:"repository"`
	}](ctx, e.c.userCall(e.a.s), fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies?", url.PathEscape(owner), url.PathEscape(name), number))
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return nil, factoryd.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make([]factoryd.IssueDependency, len(got))
	for i, d := range got {
		out[i] = factoryd.IssueDependency{
			Repo:   factoryd.Repo{Owner: cmp.Or(d.Repository.Owner, owner), Name: cmp.Or(d.Repository.Name, name)},
			Number: d.Number, State: d.State}
	}
	return out, nil
}

func (e sessionEdges) RemoveIssueDependency(ctx context.Context, repo factoryd.Repo, number int64, dep factoryd.IssueDependency) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/dependencies", url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number)
	return e.c.as(ctx, e.a.s, http.MethodDelete, path, map[string]any{"owner": dep.Repo.Owner, "repo": dep.Repo.Name, "index": dep.Number}, anyStatus, nil)
}

// doReplan sends card id, or the plan it belongs to, back to the planner
// for the member, note saying why; offMain takes the plan's merged cards
// off main first. Only once factoryd accepts does the note go on the issue
// of the card, or of the plan a child replans, as the member, in the words
// the replan step holds: the plan loop knows that comment for the replan's
// own, not a writer's ask. factoryd carries the words, so a failed read or
// post is logged.
func (c *Console) doReplan(ctx context.Context, a actor, id, note string, offMain bool) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > 64<<10 || strings.TrimSpace(factoryd.ReplanComment(note)) == "" {
		return "", refuse(http.StatusBadRequest, "A replan needs a comment saying why: the planner reads it.")
	}
	t, err := c.cardIn(ctx, a, id)
	if err != nil {
		return "", err
	}
	said, err := c.cfg.Replan(ctx, t.ID, a.s.user, note, offMain)
	var no factoryd.ErrNoReplan
	switch {
	case errors.As(err, &no):
		return "", refuse(http.StatusConflict, no.Error())
	case err != nil:
		log.Printf("console: %s replans %s: %v", a.s.user, t.ID, err)
		card := "Card " + t.ID
		if t.IssueNumber != 0 {
			card = fmt.Sprintf("Card #%d", t.IssueNumber)
		}
		return "", refuse(http.StatusInternalServerError, card+" could not go back to the planner: "+errText(err)+". Try again in a moment, then replan it again.")
	}
	comment := factoryd.ReplanComment(note)
	on := t
	if g := factoryd.GroupOf(t); g != t.ID {
		if on, err = c.cardIn(ctx, a, g); err != nil {
			log.Printf("console: %s replanned %s, its note not posted: plan %s: %v", a.s.user, t.ID, g, err)
		}
	}
	if err == nil && on.IssueNumber != 0 {
		if err := c.comment(ctx, a, on, comment, nil); err != nil {
			log.Printf("console: %s replanned %s, its note not posted on #%d: %v", a.s.user, t.ID, on.IssueNumber, err)
		}
	}
	log.Printf("console: %s replanned %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardReplanned, map[string]any{"comment": comment})
	return said, nil
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
		return "", &forgejoRefusal{what: "edit the issue", err: err}
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
	k, err := c.newestCase(ctx, t)
	if err != nil {
		return "", err
	}
	action := factoryd.ActSetDependency
	if remove {
		action = factoryd.ActRemoveDependency
	}
	return c.boardAct(ctx, a, t, k, factoryd.Resolution{Action: action, Args: map[string]any{"task_id": t.ID, "depends_on": dependsOn}})
}

// setDependency is the work of the dependency edge Act runs for a.
func (c *Console) setDependency(ctx context.Context, a actor, id, dependsOn string, remove bool) (string, error) {
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
	case t.Status != "queued" && t.Status != "waiting-deps":
		return "", wrongState(t, c.capFactsOf(ctx, t), "a card that has not started")
	case !remove && factoryd.Final(d.Status):
		return "", wrongState(d, c.capFactsOf(ctx, d), "a card that is not final")
	case t.IssueNumber == 0 || d.IssueNumber == 0:
		return "", refuse(http.StatusConflict, "The card has no issue yet.")
	}
	if !remove {
		if factoryd.GroupOf(t) != factoryd.GroupOf(d) {
			return "", refuse(http.StatusConflict, c.crossGroup(ctx, t, d))
		}
		name, err := c.processOfCard(ctx, t)
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
		return "", &forgejoRefusal{what: fmt.Sprintf("%s the dependency of #%d on #%d", verb, t.IssueNumber, d.IssueNumber), err: err}
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

// groupPlan is the issue of the plan card that holds t's group, 0 when t
// is a group of one.
func (c *Console) groupPlan(ctx context.Context, t factoryd.Task) int64 {
	g := factoryd.GroupOf(t)
	if g == t.ID && t.Track != policy.TrackPlan {
		return 0
	}
	p, ok, err := c.cfg.Tasks.Task(ctx, g)
	if err != nil {
		log.Printf("console: the plan card %s: %v", g, err)
	}
	if err != nil || !ok {
		return 0
	}
	return p.IssueNumber
}

// crossGroup is the refusal of a dependency across two groups. Its action
// follows their shape: a card of a plan changes through the plan, and a
// direct card either waits or joins a plan.
func (c *Console) crossGroup(ctx context.Context, t, d factoryd.Task) string {
	label := func(n int64) string {
		if n == 0 {
			return "no plan"
		}
		return fmt.Sprintf("plan #%d", n)
	}
	pt, pd := c.groupPlan(ctx, t), c.groupPlan(ctx, d)
	var action string
	switch {
	case pt != 0:
		action = fmt.Sprintf("Ask for the change in plan #%d.", pt)
	case pd != 0:
		action = fmt.Sprintf("Ask for #%d in plan #%d.", t.IssueNumber, pd)
	default:
		action = fmt.Sprintf("Wait for #%d to finish and file #%d again after it, or file the work of both as one plan.", d.IssueNumber, t.IssueNumber)
	}
	return fmt.Sprintf("#%d is in %s and #%d is in %s. A dependency joins cards of one plan only. %s",
		t.IssueNumber, label(pt), d.IssueNumber, label(pd), action)
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
		return 0, "", &forgejoRefusal{what: "post the comment", err: err}
	}
	log.Printf("console: %s commented on %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardCommented, commentDetail(posted))
	return posted, fmt.Sprintf("Commented on #%d.", t.IssueNumber), nil
}

// waitsOnAnswer tells a card on which a writer's comment is an answer the
// sessions get (factoryd's awaitHuman, waitPoster): one waiting on a
// human or the issue's author, off the plan track, whose comments go to
// the planner whole as change requests.
func waitsOnAnswer(t factoryd.Task) bool {
	return (t.Status == "waiting-human" || t.Status == "waiting-poster") && t.Track != policy.TrackPlan
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
		return "", &forgejoRefusal{what: "delete the comment", err: err}
	}
	log.Printf("console: %s deleted comment %d of %s via %s", a.s.user, posted, t.ID, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCommentDeleted, commentDetail(posted))
	return fmt.Sprintf("Deleted the comment on #%d.", t.IssueNumber), nil
}

// doSteer hands the member's text to card id's running session on the
// supervisor's message path, marked as theirs, every GitHub token in it
// scrubbed before it is sent or recorded.
func (c *Console) doSteer(ctx context.Context, a actor, id, text string) (string, error) {
	if c.cfg.Steer == nil {
		return "", refuse(http.StatusNotImplemented, "This console cannot message a running session: it runs without factoryd's steer.")
	}
	text, github, chat := scrubTokens(strings.TrimSpace(text))
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
		return "", refuse(http.StatusBadGateway, "The message could not be delivered: "+errText(err)+". Try again in a moment.")
	}
	log.Printf("console: %s steered %s (%s#%d) via %s", a.s.user, t.ID, t.Repository, t.IssueNumber, a.via)
	c.record(ctx, a, t.Repository, t.ID, t.IssueNumber, factoryd.EventCardSteered, map[string]any{"text": text})
	note := fmt.Sprintf("The session of #%d has your message.", t.IssueNumber)
	if github || chat {
		note += " I removed " + removedTokens(github, chat) + " from it; it was not stored or sent."
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

// approver is who decided a's action on an approval request, a plan or a
// factory pull request: factoryd.ChatUser when the agent decided it (in a
// wake, or unconfirmed in a member's turn), the factory's own decision,
// else the member. Its approval rows, plan decisions and signal authors
// carry it.
func (a actor) approver() string {
	if a.decider == "agent" {
		return factoryd.ChatUser
	}
	return a.s.user
}

// decide records a's decision on q under a.approver(); a request the
// factory decided meanwhile (a new attempt superseded it) is logged, not
// an error: Forgejo already carries the member's action.
func (c *Console) decide(ctx context.Context, a actor, q factoryd.ApprovalRequest, status, note string) error {
	err := c.cfg.Approvals.DecideApproval(ctx, q.ID, q.Kind, status, a.approver(), note)
	switch {
	case errors.Is(err, factoryd.ErrNotFound) && q.Kind == factoryd.ApprovalMerge:
		log.Printf("console: approval request %d was decided by the factory before %s's %s was recorded", q.ID, a.s.user, status)
	case err != nil:
		log.Printf("console: decide approval request %d: %v", q.ID, err)
		return refuse(http.StatusInternalServerError, "cannot record the decision: "+errText(err)+". Try again in a moment.")
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
// task (one commission of that spec; the project's envelope is its
// file's); a merge request gets the member's APPROVED review on the
// pull request, and only once Forgejo has it is the workflow signalled,
// which merges as factory-merge. An approval the agent decided is the
// factory's own, decided as factoryd.ChatUser (a.approver), whoever is
// online: no review is posted, and the merge posts the merge gate's
// approval on the merge rule auto and refuses on any other. A plan's
// integration or delta card merges once every human criterion row has a
// verdict. A red-checks merge request merges only once the same head's
// checks turned green again (#1013: a re-run); still red, its approval
// refuses naming what fits the card. The agent never approves a GitHub
// sync's pull request: a human approves the GitHub commits it carries.
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
	case factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		return c.answerRequest(ctx, a, q, factoryd.Resolution{Action: factoryd.ActGrantCriteria})
	case factoryd.ApprovalMerge:
		return c.answerRequest(ctx, a, q, factoryd.Resolution{Action: factoryd.ActApproveMerge})
	}
	return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which the board decides", q.ID, q.Kind))
}

// approveMerge is the work of the merge approval Act runs for a: the member's
// APPROVED review on q's pull request, and only once Forgejo has it is the
// workflow signalled, which merges as factory-merge. An approval the agent
// decided is the factory's own, decided as factoryd.ChatUser (a.approver),
// whoever is online: no review is posted, and the merge posts the merge
// gate's approval on the merge rule auto and refuses on any other. A plan's
// integration or delta card merges once every human criterion row has a
// verdict. A red-checks merge request merges only once the same head's
// checks turned green again (#1013: a re-run); still red, its approval
// refuses naming what fits the card. The agent never approves a GitHub
// sync's pull request: a human approves the GitHub commits it carries.
func (c *Console) approveMerge(ctx context.Context, a actor, q factoryd.ApprovalRequest) (string, error) {
	t, _, err := c.cfg.Tasks.Task(ctx, q.TaskID)
	if err != nil {
		log.Printf("console: task %s: %v", q.TaskID, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the task")
	}
	agent := a.approver() == factoryd.ChatUser
	if agent && t.Source == factoryd.SourceGitHubSync {
		return "", refuse(http.StatusForbidden, factoryMergeRefusal)
	}
	var spec factoryd.MergeSpec
	if err := json.Unmarshal(q.Spec, &spec); err != nil || spec.HeadSHA == "" {
		return "", refuse(http.StatusInternalServerError, "the merge request names no head commit")
	}
	// a card the newest revision drops cannot merge (roadmap 291): the
	// request opened before the revision landed
	if t.Source == factoryd.SourcePlan && t.Parent != "" && t.PlanRevision > 0 {
		drops, newest, err := c.revisionDrops(ctx, t)
		if err != nil {
			log.Printf("console: the plan revisions of %s: %v", t.ID, err)
			return "", refuse(http.StatusInternalServerError, "Cannot read the plan's revisions. Approve again; if it fails again, replan the plan.")
		}
		if drops {
			return "", refuse(http.StatusConflict, fmt.Sprintf("The plan's revision %d drops the card %s, so it cannot merge: deny the merge (deny_merge).", newest, t.PlanKey))
		}
	}
	_, open, _, _, err := c.planVerdicts(ctx, t)
	switch {
	case err != nil:
		log.Printf("console: the criterion rows of %s: %v", t.ID, err)
		return "", refuse(http.StatusInternalServerError, "Cannot read the card's criterion rows. Approve again.")
	case len(open) == 1:
		return "", refuse(http.StatusConflict, fmt.Sprintf("Criterion %s has no verdict yet: settle its row on the card first.", open[0]))
	case len(open) > 1:
		return "", refuse(http.StatusConflict, fmt.Sprintf("Criteria %s have no verdict yet: settle each row on the card first.", textutil.AndList(open)))
	}
	if err := c.cfg.CheckMerge(ctx, q.TaskID, spec.PolicyVersion, a.approver()); err != nil {
		if refusal, ok := unreviewedRefusal(err, t, spec); ok {
			return "", refusal
		}
		log.Printf("console: the review and audit rows of %s: %v", q.TaskID, err)
		return "", refuse(http.StatusInternalServerError, "Cannot read the card's review and audit rows. Approve again.")
	}
	unsignalled := "the task's workflow could not be signalled; approve again"
	if !agent {
		if err := c.review(ctx, a, q, "APPROVED", "Approved in the factory console.", spec.HeadSHA); err != nil {
			return "", &forgejoRefusal{what: "submit the approving review", err: err}
		}
		unsignalled = "your approving review is on the pull request, but " + unsignalled
	}
	if err := c.cfg.ApproveMerge(ctx, q.TaskID, q.PRNumber, a.approver()); err != nil {
		if refusal, ok := unreviewedRefusal(err, t, spec); ok {
			return "", refusal
		}
		log.Printf("console: %s approved PR #%d of %s, the signal failed: %v", a.s.user, q.PRNumber, q.TaskID, err)
		return "", refuse(http.StatusBadGateway, unsignalled)
	}
	log.Printf("console: %s approved PR #%d of %s at %s via %s", a.s.user, q.PRNumber, q.TaskID, spec.HeadSHA, a.via)
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeApproved, map[string]any{"pr": q.PRNumber})
	return fmt.Sprintf("Approved pull request #%d; factoryd merges it.", q.PRNumber), nil
}

// grantCriteria is the work of the grant Act runs for a on criteria request q.
func (c *Console) grantCriteria(ctx context.Context, a actor, q factoryd.ApprovalRequest) (string, error) {
	// the grant runs first and reports what it answered: one more
	// card for an open criterion, nothing when every criterion is met
	// or waived already — a request that closes on the next sweep
	// takes no approved grant
	n, err := c.cfg.GrantCriteria(ctx, q.TaskID, a.approver(), q.ID)
	switch {
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no plan %s.", q.TaskID))
	case err != nil:
		log.Printf("console: %s granted the gap of %s, the dues failed: %v", a.s.user, q.TaskID, err)
		return "", refuse(http.StatusInternalServerError, fmt.Sprintf("The grant could not be recorded: %s. Grant again, or Fund the card when the refusal names money.", errText(err)))
	case n == 0:
		return "", refuse(http.StatusConflict, "Cannot grant one more card: every criterion is met or waived already, and the request closes on the next sweep.")
	}
	if err := c.decide(factoryd.LeaveCaseToAct(ctx), a, q, "approved", "granted: one more bounded card"); err != nil {
		return "", err
	}
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"approval": q.ID, "criteria": n, "verb": "granted"})
	return fmt.Sprintf("Granted one more bounded card for %d criteria; the planner drafts it and the board shows it.", n), nil
}

// unreviewedRefusal turns factoryd's refusal of the agent's approval into
// the 409 that names the fitting action: a request for changes goes back to
// an agent, which no factory pull request and no roll has (a change request
// closes the one and fails the other).
func unreviewedRefusal(err error, t factoryd.Task, spec factoryd.MergeSpec) (error, bool) {
	var unreviewed *factoryd.UnreviewedMergeError
	if !errors.As(err, &unreviewed) {
		return nil, false
	}
	remedy := " Leave the call to a human, or deny the merge with a request for changes (deny_merge with request_changes), which sends the card back so the gates run on its next head."
	if spec.Factory || t.Source == factoryd.SourceRoll {
		remedy = " Leave the call to a human: no agent works on this pull request."
	}
	return refuse(http.StatusConflict, unreviewed.Error()+remedy), true
}

// factoryMergeRefusal answers the agent's approval of a GitHub sync's
// pull request.
const factoryMergeRefusal = "A human approves a factory pull request that carries GitHub commits: record what you found with attach_analysis."

// revisionDrops tells whether plan card t's newest plan revision drops
// its key, and that revision's number.
func (c *Console) revisionDrops(ctx context.Context, t factoryd.Task) (bool, int, error) {
	started, ok, err := c.cfg.Tasks.PlanRevision(ctx, t.Parent, t.PlanRevision)
	if err != nil || !ok {
		return false, 0, err
	}
	newest, ok, err := c.cfg.Tasks.PlanRevision(ctx, t.Parent, 0)
	if err != nil || !ok {
		return false, 0, err
	}
	drops, err := factoryd.RevisionDrops(started, newest, t.PlanKey)
	return drops, newest.Revision, err
}

// doDeny denies approval request id with the member's reason, a
// writer of its project. A resource request is denied (the agent is told
// the reason). A merge request gets a REQUEST_CHANGES review (review: the
// member's, or the chat identity's when the agent decided), which sends
// the task back to its agent (action request_changes), or is rejected:
// the reason commented and the pull
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
	case factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		return c.answerRequest(ctx, a, q, factoryd.Resolution{Action: factoryd.ActDenyCriteria, Note: note})
	case factoryd.ApprovalMerge:
		return c.answerRequest(ctx, a, q, factoryd.Resolution{Action: factoryd.ActDenyMerge, Args: map[string]any{"action": action}, Note: note})
	}
	return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which the board decides", q.ID, q.Kind))
}

// denyMerge is the work of the merge denial Act runs for a: a
// REQUEST_CHANGES review (the member's, or the chat identity's when the
// agent decided), which sends the task back to its agent (action
// request_changes), or a rejection: the reason commented and the pull
// request closed as the member, which fails the task (action reject). A
// factory pull request's has no agent to send it back to: either action
// closes the card and its pull request with the reason (roadmap 264).
func (c *Console) denyMerge(ctx context.Context, a actor, q factoryd.ApprovalRequest, action, note string) (string, error) {
	var spec factoryd.MergeSpec
	if err := json.Unmarshal(q.Spec, &spec); err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the merge request")
	}
	if spec.Factory {
		if err := c.cfg.CloseFactoryPR(ctx, q.TaskID, q.PRNumber, a.approver(), note); err != nil {
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
			return "", &forgejoRefusal{what: "submit the change request", err: err}
		}
		if err := c.decide(ctx, a, q, "denied", "changes requested: "+note); err != nil {
			return "", err
		}
	case "reject":
		pr := fmt.Sprintf("/api/v1/repos/%s/%s", url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
		if err := c.as(ctx, a.s, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", pr, q.PRNumber),
			map[string]string{"body": "Rejected in the factory console: " + note}, http.StatusCreated, nil); err != nil {
			return "", &forgejoRefusal{what: "comment the rejection", err: err}
		}
		if err := c.as(ctx, a.s, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", pr, q.PRNumber),
			map[string]string{"state": "closed"}, http.StatusCreated, nil); err != nil {
			return "", &forgejoRefusal{what: "close the pull request", err: err}
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

// noRows refuses a criterion press on a request whose card holds no
// criterion rows.
const noRows = "This card has no criterion rows."

// doDecide records a's verb on criterion cid of the pending request id: on
// the merge request of a plan's integration or delta card (planRows), met
// on a human criterion with a's words beside the artifact they saw,
// not_met on a human criterion, which sends the card back to its agent
// with a's words, or waived on any row with a's one-line reason; on a
// criteria or criteria_rerun request, waived alone.
func (c *Console) doDecide(ctx context.Context, a actor, id int64, cid, verb, reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case verb != "met" && verb != "not_met" && verb != "waived":
		return "", refuse(http.StatusBadRequest, "Choose Met, Not met or Waive: the verb is met, not_met or waived.")
	case verb != "met" && (reason == "" || len(reason) > 2000):
		return "", refuse(http.StatusBadRequest, "a one-line reason of up to 2000 characters is required for Not met and Waive")
	}
	q, err := c.pendingRequest(ctx, a, id)
	switch {
	case err != nil:
		return "", err
	case !slices.Contains([]string{factoryd.ApprovalMerge, factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun}, q.Kind):
		return "", refuse(http.StatusConflict, noRows)
	}
	return c.answerRequest(ctx, a, q, factoryd.Resolution{Action: factoryd.ActAnswerCriteria,
		Args: map[string]any{"criterion": cid, "verdict": criteriaVerdicts[verb]}, Note: reason})
}

// criteriaVerdicts are the verdicts of answer_criteria by the verbs of the
// criterion rows' forms.
var criteriaVerdicts = map[string]string{"met": "met", "not_met": "not_met", "waived": "waive"}

// criterionVerdict is the work of a criterion verdict Act runs for a on
// request q.
func (c *Console) criterionVerdict(ctx context.Context, a actor, q factoryd.ApprovalRequest, cid, verb, reason string) (string, error) {
	plan := q.TaskID
	var row *criterionRow
	switch q.Kind {
	case factoryd.ApprovalCriteria, factoryd.ApprovalCriteriaRerun:
		if verb != "waived" {
			return "", refuse(http.StatusConflict, noRows)
		}
	case factoryd.ApprovalMerge:
		t, _, err := c.cfg.Tasks.Task(ctx, q.TaskID)
		if err != nil {
			log.Printf("console: task %s: %v", q.TaskID, err)
			return "", refuse(http.StatusInternalServerError, "cannot read the task")
		}
		rows, _, ok, err := c.planRows(ctx, t)
		switch {
		case err != nil:
			log.Printf("console: the criterion rows of %s: %v", t.ID, err)
			return "", refuse(http.StatusInternalServerError, "Cannot read the card's criterion rows. Press again.")
		case !ok:
			return "", refuse(http.StatusConflict, noRows)
		}
		i := slices.IndexFunc(rows, func(r criterionRow) bool { return r.ID == cid })
		switch {
		case i < 0:
			return "", refuse(http.StatusConflict, fmt.Sprintf("The plan holds no criterion %s. Reload the board.", cid))
		case verb != "waived" && rows[i].Kind != factoryd.CriterionHuman:
			return "", refuse(http.StatusConflict, fmt.Sprintf("Criterion %s is the gate's to judge.", cid))
		case rows[i].Verdict == "met":
			return "", refuse(http.StatusConflict, fmt.Sprintf("Criterion %s is met already. Reload the board.", cid))
		}
		plan, row = t.Parent, &rows[i]
	default:
		return "", refuse(http.StatusConflict, noRows)
	}
	if verb == "met" {
		seen := "the criterion's evidence"
		if row.Artifact != nil {
			seen = row.Artifact.Name + " at " + row.Artifact.Head[:min(7, len(row.Artifact.Head))]
		}
		reason = fmt.Sprintf("%s (%s)", cmp.Or(reason, "met"), seen)
		if len(reason) > 2000 {
			return "", refuse(http.StatusBadRequest, fmt.Sprintf("Met takes up to %d characters of words beside what you saw.", 2000-len(seen)-3))
		}
	}
	if verb == "not_met" {
		if _, err := c.denyMerge(ctx, a, q, "request_changes", fmt.Sprintf("criterion %s not met: %s", cid, reason)); err != nil {
			return "", err
		}
	} else {
		switch err := c.cfg.DecideCriterion(ctx, plan, cid, verb, a.s.user, reason); {
		case errors.Is(err, factoryd.ErrNoCriterion):
			return "", refuse(http.StatusConflict, fmt.Sprintf("The plan holds no criterion %s. Reload the board.", cid))
		case errors.Is(err, factoryd.ErrCriterionMet):
			return "", refuse(http.StatusConflict, fmt.Sprintf("Criterion %s is met already. Reload the board.", cid))
		case errors.Is(err, factoryd.ErrNotHuman):
			return "", refuse(http.StatusConflict, fmt.Sprintf("Criterion %s is the gate's to judge.", cid))
		case errors.Is(err, factoryd.ErrNotFound):
			return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no plan %s.", plan))
		case err != nil:
			log.Printf("console: %s decides %s of %s %s: %v", a.s.user, cid, plan, verb, err)
			return "", refuse(http.StatusInternalServerError, "The decision could not be recorded: "+errText(err)+". Try again in a moment.")
		}
	}
	log.Printf("console: %s decided criterion %s of %s %s via %s", a.s.user, cid, plan, verb, a.via)
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided,
		map[string]any{"approval": q.ID, "criterion": cid, "verb": verb, "reason": reason})
	return fmt.Sprintf(map[string]string{
		"met":     "Criterion %s met.",
		"not_met": "Criterion %s is not met. The card goes back to its agent with your words.",
		"waived":  "Criterion %s waived.",
	}[verb], cid), nil
}

// denyCriteria is the work of the denial Act runs for a on criteria request q.
func (c *Console) denyCriteria(ctx context.Context, a actor, q factoryd.ApprovalRequest, note string) (string, error) {
	if err := c.decide(ctx, a, q, "denied", note); err != nil {
		return "", err
	}
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"approval": q.ID, "verb": "denied"})
	if q.Kind == factoryd.ApprovalCriteriaRerun {
		return "Denied the ask; waive a criterion instead if it should not hold.", nil
	}
	return "Denied the grant; waive a criterion instead if it should not hold.", nil
}

// doConfirmCriteria records the human's confirmation that the plan's
// delivered cards meet its criteria: every open criterion met at once,
// their line the evidence, on the human's own word when it arrives via
// chat. The sweep that reads the rows ends the request and closes the
// plan; a card of the plan not done or merged refuses, naming it.
func (c *Console) doConfirmCriteria(ctx context.Context, a actor, id int64, line string) (string, error) {
	q, err := c.pendingRequest(ctx, a, id)
	if err != nil {
		return "", err
	}
	if q.Kind != factoryd.ApprovalCriteria && q.Kind != factoryd.ApprovalCriteriaRerun {
		return "", refuse(http.StatusConflict, fmt.Sprintf("approval request %d is a %s request, which confirms nothing", q.ID, q.Kind))
	}
	return c.answerRequest(ctx, a, q, factoryd.Resolution{Action: factoryd.ActConfirmCriteria, Note: line})
}

// confirmCriteria is the work of the confirmation Act runs for a on request q.
func (c *Console) confirmCriteria(ctx context.Context, a actor, q factoryd.ApprovalRequest, line string) (string, error) {
	n, err := c.cfg.ConfirmDeliveredCriteria(ctx, q.TaskID, a.s.user, line)
	switch {
	case errors.Is(err, factoryd.ErrChildOpen):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Cannot confirm the delivered cards: %v.", err))
	case errors.Is(err, factoryd.ErrNotFound):
		return "", refuse(http.StatusNotFound, fmt.Sprintf("There is no plan %s.", q.TaskID))
	case err != nil:
		log.Printf("console: %s confirmed the criteria of %s: %v", a.s.user, q.TaskID, err)
		return "", refuse(http.StatusInternalServerError, "the confirmation could not be recorded: "+errText(err)+". Try again in a moment.")
	}
	log.Printf("console: %s confirmed the delivered cards of %s via %s", a.s.user, q.TaskID, a.via)
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventCriterionDecided, map[string]any{"approval": q.ID, "criteria": n, "verb": "met"})
	if n == 0 {
		return "Every criterion of the plan is met or waived already; the request closes on the next sweep.", nil
	}
	return fmt.Sprintf("Confirmed %d criteria met on the delivered cards; the request closes on the next sweep.", n), nil
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
	if err := c.cfg.RerunChecks(ctx, q.TaskID, q.PRNumber, a.approver()); err != nil {
		log.Printf("console: %s re-ran the checks of PR #%d of %s, the signal failed: %v", a.s.user, q.PRNumber, q.TaskID, err)
		return "", refuse(http.StatusBadGateway, "the task's workflow could not be signalled; run the checks again once more")
	}
	c.record(ctx, a, q.Project, q.TaskID, c.issueOf(ctx, q.TaskID), factoryd.EventMergeDenied, map[string]any{"pr": q.PRNumber, "action": "rerun_checks"})
	return fmt.Sprintf("Running the checks of pull request #%d again; the card asks again once they settle.", q.PRNumber), nil
}

// doInvite makes login a member of the chat's project, an approver's
// action: a write collaborator of its repository, added with the token
// registration keeps project repositories with, since the approver's own
// may not administer it. A seed project's collaborators are
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
		return "", &forgejoRefusal{what: "add " + login + " as a collaborator", err: err}
	}
	log.Printf("console: %s invited %s to %s via %s", a.s.user, login, p.Repository, a.via)
	c.record(ctx, a, p.Repository, "", 0, factoryd.EventMemberInvited, map[string]any{"login": login})
	return login + " has write access; the console admits them within a minute.", nil
}

// doDeployNow releases main of the project's held production, an
// approver's call (access rule "deploy a held production"), decided by
// the member.
func (c *Console) doDeployNow(ctx context.Context, a actor, name string) (string, error) {
	p, ok, err := c.projectOf(ctx, name)
	switch {
	case err != nil:
		log.Printf("console: the projects: %v", err)
		return "", refuse(http.StatusInternalServerError, "cannot read the projects")
	case !ok:
		return "", refuse(http.StatusNotFound, "There is no project "+name+".")
	}
	commit, err := c.cfg.DeployNow(ctx, p.Repository, a.s.user, a.via, cmp.Or(a.decider, "member"))
	var red factoryd.ErrMainRed
	switch {
	case errors.Is(err, factoryd.ErrNotHeld):
		return "", refuse(http.StatusConflict, "Production of "+name+" is not held: every merge goes to production on its own.")
	case errors.As(err, &red):
		return "", refuse(http.StatusConflict, fmt.Sprintf("Main of %s at %s is red: Deploy now waits for a green run on main. Press it again once main is green.",
			name, red.Commit[:min(7, len(red.Commit))]))
	case err != nil:
		log.Printf("console: %s deploys %s now: %v", a.s.user, p.Repository, err)
		return "", refuse(http.StatusInternalServerError, "Production of "+name+" could not be released: "+errText(err)+". Try again in a moment, then press Deploy now again.")
	}
	return fmt.Sprintf("Deploy now: main of %s at %s goes to production. A merge after it waits for the open plans again.", name, commit[:min(7, len(commit))]), nil
}
