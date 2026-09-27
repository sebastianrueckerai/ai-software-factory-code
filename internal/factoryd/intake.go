package factoryd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Intake is the Forgejo webhook endpoint: every issue labelled agent or
// human becomes a task row (an agent task also starts its workflow), every
// PR/issue comment or review becomes an intake event, and the events a task
// workflow waits on (merge, changes requested, issue closed) are signalled
// to it. It implements http.Handler directly; main wires it at POST
// /webhooks/forgejo.
type Intake struct {
	Store     Store
	Forgejo   *Forgejo
	Workflows Workflows
	BotUser   string // factory-agent: its own issues/comments never spawn tasks or events
	Secret    string // HMAC-SHA256 webhook secret, shared with Forgejo's webhook config
	// Budget is the policy in force's budget section: a new task's budget.
	Budget func() policy.Budget
	// Repositories are where the heal pass looks for agent and human
	// issues no webhook turned into a task.
	Repositories []Repo

	trustMu sync.Mutex
	trust   map[string]time.Time // repo + user → trusted until
}

// trustTTL is how long a positive verdict is reused; a denial is checked
// again every time, so a newly granted user is heard at once.
const trustTTL = time.Minute

// trusted reports whether user may steer a task through comments and
// reviews: code write on the repository (RepoWriters). A comment by anyone
// else is recorded, never signalled.
func (in *Intake) trusted(ctx context.Context, repository, user string) (bool, error) {
	key := repository + "\x00" + user
	in.trustMu.Lock()
	until, ok := in.trust[key]
	in.trustMu.Unlock()
	if ok && time.Now().Before(until) {
		return true, nil
	}
	repo, err := SplitRepo(repository)
	if err != nil {
		return false, err
	}
	writers, err := in.Forgejo.RepoWriters(ctx, repo.Owner, repo.Name)
	if err != nil {
		return false, fmt.Errorf("writers of %s: %w", repository, err)
	}
	if !slices.Contains(writers, user) {
		log.Printf("intake: %s cannot write %s: not trusted to steer a task", user, repository)
		return false, nil
	}
	in.trustMu.Lock()
	if in.trust == nil {
		in.trust = map[string]time.Time{}
	}
	in.trust[key] = time.Now().Add(trustTTL)
	in.trustMu.Unlock()
	return true, nil
}

func (in *Intake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	eventType := r.Header.Get("X-Forgejo-Event-Type")
	if !validWebhookSignature(in.Secret, body, r.Header.Get("X-Forgejo-Signature")) {
		log.Printf("webhook intake: rejected %s: bad signature", strconv.Quote(eventType))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	// A delivery is marked processed only after it was: a crash in between
	// makes a redelivery process it again, which every step tolerates (task
	// creation is keyed by issue, intake events by delivery id, workflow
	// starts by workflow id, and the workflow coalesces signals).
	delivery := r.Header.Get("X-Forgejo-Delivery")
	if delivery != "" {
		seen, err := in.Store.DeliverySeen(ctx, delivery)
		if err != nil {
			log.Printf("webhook intake: delivery lookup: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if seen {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	var procErr error
	switch eventType {
	case "issues", "issue_assign", "issue_label", "issue_milestone":
		procErr = in.handleIssue(ctx, body, eventType)
	case "issue_comment", "pull_request_comment":
		procErr = in.handleIssueComment(ctx, body, eventType, delivery)
	case "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment":
		procErr = in.handleReview(ctx, body, eventType, delivery)
	case "pull_request":
		procErr = in.handlePullRequest(ctx, body)
	default:
		// an event type this factory does not act on yet: accepted so
		// Forgejo's delivery list reads all 2xx, nothing to process
	}
	if procErr != nil {
		log.Printf("webhook intake: %s: %v", strconv.Quote(eventType), procErr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if delivery != "" {
		if err := in.Store.RecordDelivery(ctx, delivery); err != nil {
			log.Printf("webhook intake: record delivery: %v", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// validWebhookSignature verifies X-Forgejo-Signature: hex(HMAC-SHA256(body,
// secret)), no "sha256=" prefix. Checked before any parsing of body.
func validWebhookSignature(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(signature), []byte(want)) == 1
}

type webhookUser struct {
	UserName string `json:"login"`
}

type webhookLabel struct {
	Name string `json:"name"`
}

type webhookRepository struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

type webhookIssue struct {
	Index  int64          `json:"number"`
	Title  string         `json:"title"`
	Body   string         `json:"body"`
	Poster webhookUser    `json:"user"`
	Labels []webhookLabel `json:"labels"`
}

type issuesPayload struct {
	Action     string            `json:"action"`
	Issue      webhookIssue      `json:"issue"`
	Repository webhookRepository `json:"repository"`
	Sender     webhookUser       `json:"sender"`
}

type webhookComment struct {
	ID     int64       `json:"id"`
	Poster webhookUser `json:"user"`
	Body   string      `json:"body"`
}

type issueCommentPayload struct {
	Action     string            `json:"action"`
	Issue      webhookIssue      `json:"issue"`
	Comment    webhookComment    `json:"comment"`
	Repository webhookRepository `json:"repository"`
	IsPull     bool              `json:"is_pull"`
}

type prBranchInfo struct {
	Ref  string `json:"ref"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type webhookPullRequest struct {
	Index  int64        `json:"number"`
	Head   prBranchInfo `json:"head"`
	Merged bool         `json:"merged"`
}

type reviewPayload struct {
	Content string `json:"content"`
}

type pullRequestPayload struct {
	Action      string             `json:"action"`
	PullRequest webhookPullRequest `json:"pull_request"`
	Repository  webhookRepository  `json:"repository"`
	Sender      webhookUser        `json:"sender"`
	Review      *reviewPayload     `json:"review"`
}

func labelNames(labels []webhookLabel) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}

// priorityLabelWeight maps a label to its priority-int contribution. A
// task's priority is the highest weight among the labels actually present
// (the brief: "priority from priority/p0..p3 and severity/critical|high|
// medium|low labels, higher of the two mappings"); absent either kind of
// label, the default is 0.
var priorityLabelWeight = map[string]int{
	"priority/p0": 3, "priority/p1": 2, "priority/p2": 1, "priority/p3": 0,
	"severity/critical": 3, "severity/high": 2, "severity/medium": 1, "severity/low": 0,
}

func priorityFromLabels(names []string) int {
	best := 0
	for _, n := range names {
		if w, ok := priorityLabelWeight[n]; ok && w > best {
			best = w
		}
	}
	return best
}

func objectiveFromIssue(issue webhookIssue) string {
	if issue.Body == "" {
		return issue.Title
	}
	return issue.Title + "\n\n" + issue.Body
}

// AgentCapabilities are the capability classes every agent task gets at
// intake, through POST /tasks or an issue alike.
var AgentCapabilities = []string{"coder"}

// handleIssue hands an issue event to intakeIssue; a close goes to the
// task the issue is linked to.
func (in *Intake) handleIssue(ctx context.Context, body []byte, eventType string) error {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Action == "closed" {
		return in.handleIssueClosed(ctx, p)
	}
	return in.intakeIssue(ctx, "webhook intake", p.Repository.FullName, p.Repository.DefaultBranch, p.Issue)
}

// ResyncIssues takes in the open agent and human issues of in.Repositories
// that no task is linked to yet: Forgejo never retries a failed delivery,
// so an issue filed or labelled while factoryd was down is otherwise never
// seen. Each goes through the webhook's own intake.
func (in *Intake) ResyncIssues(ctx context.Context) error {
	var errs []error
	for _, repo := range in.Repositories {
		for _, label := range []string{"agent", "human"} {
			issues, err := in.Forgejo.OpenIssuesLabelled(ctx, repo.Owner, repo.Name, label)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s issues of %s: %w", label, repo, err))
				continue
			}
			for _, issue := range issues {
				_, linked, err := in.Store.TaskByIssue(ctx, repo.String(), issue.Index)
				if err != nil {
					errs = append(errs, fmt.Errorf("task by issue: %w", err))
					continue
				}
				if linked {
					continue
				}
				if err := in.intakeIssue(ctx, "heal pass", repo.String(), "", issue); err != nil {
					errs = append(errs, fmt.Errorf("issue #%d of %s: %w", issue.Index, repo, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// intakeIssue turns an issue of repo carrying the agent or human label
// into a task row, idempotent per (repository, issue number): a
// redelivery, a later label event or a heal pass finds the task already
// linked and no-ops. defaultBranch is the repository's when the caller
// knows it (a webhook payload), else read. via names the path in the log.
func (in *Intake) intakeIssue(ctx context.Context, via, repo, defaultBranch string, issue webhookIssue) error {
	if issue.Poster.UserName == in.BotUser {
		return nil // the bot's own issues never spawn a second task
	}
	names := labelNames(issue.Labels)
	hasAgent := slices.Contains(names, "agent")
	hasHuman := slices.Contains(names, "human")
	if !hasAgent && !hasHuman {
		return nil
	}
	if t, ok, err := in.Store.TaskByIssue(ctx, repo, issue.Index); err != nil {
		return fmt.Errorf("task by issue: %w", err)
	} else if ok {
		// a redelivery retries a workflow start that failed the first time
		if t.Kind == "agent" && t.Status == "queued" {
			return in.Workflows.Start(ctx, t.ID)
		}
		return nil
	}

	kind := "agent"
	if hasHuman {
		kind = "human"
	}
	id := NewTaskID()
	task := Task{
		ID:          id,
		Priority:    priorityFromLabels(names),
		RiskClass:   "standard",
		BudgetEUR:   in.Budget().ClampBudget(0),
		Status:      "queued",
		Objective:   objectiveFromIssue(issue),
		Repository:  repo,
		Kind:        kind,
		IssueNumber: issue.Index,
		Source:      "webhook",
	}
	if kind == "agent" {
		task.RequiredCapabilities = AgentCapabilities
		repoParts, err := SplitRepo(repo)
		if err != nil {
			return err
		}
		if task.SHA, err = in.Forgejo.DefaultBranchHeadSHA(ctx, repoParts.Owner, repoParts.Name, defaultBranch); err != nil {
			return fmt.Errorf("base sha: %w", err)
		}
	} else {
		// no runner ever claims the human lane: it waits on a human from
		// the start (architecture 5.6's board column) and closing its
		// issue finishes it
		task.Status = "waiting-human"
	}
	if err := in.Store.CreateTask(ctx, task); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	log.Printf("%s: issue #%d on %s by %s: %s task %s", via, issue.Index, repo, issue.Poster.UserName, kind, id)
	mirrorTaskState(ctx, in.Forgejo, task, task.Status)
	if kind == "agent" {
		return in.Workflows.Start(ctx, id)
	}
	return nil
}

func (in *Intake) handleIssueClosed(ctx context.Context, p issuesPayload) error {
	t, ok, err := in.Store.TaskByIssue(ctx, p.Repository.FullName, p.Issue.Index)
	if err != nil {
		return fmt.Errorf("task by issue: %w", err)
	}
	if !ok {
		return nil
	}
	return in.issueClosed(ctx, t, p.Sender.UserName)
}

// issueClosed acts on task t's issue closed by closer, from the webhook or
// the heal pass alike: the task's workflow is signalled (it asks Forgejo
// whether a merge closed it), a human-lane task is simply done. An issue's
// author can close it without any repository access, so only the bot
// (closing what factoryd filed) and a user trusted to steer count.
func (in *Intake) issueClosed(ctx context.Context, t Task, closer string) error {
	if closer != in.BotUser {
		if ok, err := in.trusted(ctx, t.Repository, closer); err != nil || !ok {
			return err
		}
	}
	if t.Kind == "human" {
		return in.finishHumanTask(ctx, t)
	}
	return in.decide(ctx, t.ID, "issue_closed", closer, t.IssueNumber)
}

// decide records a decision — issue number closed, pull request number
// merged or closed — once and signals it once: between the webhook and
// the heal passes that read the same state off Forgejo, and not again on
// every pass while the workflow winds the task down.
func (in *Intake) decide(ctx context.Context, taskID, kind, author string, number int64) error {
	e, err := in.record(ctx, IntakeEvent{TaskID: taskID, Kind: kind, Author: author, CommentID: number, Trusted: true})
	if err != nil {
		return err
	}
	return in.signalEvent(ctx, taskID, e, number)
}

// finishHumanTask: a human-lane task has no workflow; its closed issue is
// its end.
func (in *Intake) finishHumanTask(ctx context.Context, t Task) error {
	if t.Status != "waiting-human" {
		return nil
	}
	if err := in.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
		return err
	}
	mirrorTaskState(ctx, in.Forgejo, t, "done")
	return nil
}

// handlePullRequest signals a closed factory pull request to its task's
// workflow: merged, or closed without a merge (the reviewer rejected it).
func (in *Intake) handlePullRequest(ctx context.Context, body []byte) error {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode pull_request: %w", err)
	}
	// the bot's own close (Finish, a .forgejo refusal) lands on a workflow
	// that already knows; Reconcile reads the state for a resumed one
	if p.Action != "closed" || p.Sender.UserName == in.BotUser {
		return nil
	}
	taskID, ok := in.taskOfHead(p.Repository.FullName, p.PullRequest.Head.Repo.FullName, p.PullRequest.Head.Ref)
	if !ok {
		return nil
	}
	if _, ok, err := in.agentTask(ctx, taskID); err != nil || !ok {
		return err
	}
	kind := "pull_request_closed"
	if p.PullRequest.Merged {
		kind = "pull_request_merged"
	}
	return in.decide(ctx, taskID, kind, p.Sender.UserName, p.PullRequest.Index)
}

// agentTask is task id when it exists and runs a workflow to signal: a
// human-lane task has none.
func (in *Intake) agentTask(ctx context.Context, id string) (Task, bool, error) {
	t, ok, err := in.Store.Task(ctx, id)
	return t, ok && t.Kind == "agent", err
}

// taskOfHead is the task whose pull request has head ref in headRepo: a
// factory/<id> branch on the bot's fork of repo. The same branch name on
// repo itself or on anyone else's fork is not the task's (ruling 97).
func (in *Intake) taskOfHead(repo, headRepo, ref string) (string, bool) {
	base, err := SplitRepo(repo)
	if err != nil || headRepo != in.Forgejo.Fork(base).String() {
		return "", false
	}
	return strings.CutPrefix(ref, "factory/")
}

// handleIssueComment records a new comment as an intake event against the
// task it belongs to: directly by issue number for an issue comment, or by
// resolving the pull request's head branch (factory/<task id>) for a pull
// request comment. A comment on anything else the factory did not file
// (no matching task) is silently dropped, and so are edits and deletions:
// the workflow saw the comment when it was created.
func (in *Intake) handleIssueComment(ctx context.Context, body []byte, eventType, delivery string) error {
	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Action != "created" || p.Comment.Poster.UserName == in.BotUser {
		return nil
	}
	repo := p.Repository.FullName

	var taskID string
	if p.IsPull {
		repoParts, err := SplitRepo(repo)
		if err != nil {
			return err
		}
		pr, err := in.Forgejo.PullRequestState(ctx, repoParts.Owner, repoParts.Name, p.Issue.Index)
		if err != nil {
			return fmt.Errorf("pull request head: %w", err)
		}
		id, ok := in.taskOfHead(repo, pr.HeadRepo, pr.Head)
		if !ok {
			return nil
		}
		taskID = id
	} else {
		t, ok, err := in.Store.TaskByIssue(ctx, repo, p.Issue.Index)
		if err != nil {
			return fmt.Errorf("task by issue: %w", err)
		}
		if !ok {
			return nil
		}
		taskID = t.ID
	}
	t, ok, err := in.agentTask(ctx, taskID)
	if err != nil || !ok {
		return err
	}
	return in.recordComment(ctx, t, p.IsPull, p.Issue.Index, IssueComment{ID: p.Comment.ID, Author: p.Comment.Poster.UserName, Body: p.Comment.Body}, delivery)
}

// recordComment records comment c on task t's issue (onPR false) or pull
// request number and signals it to the workflow when its author is
// trusted: an issue comment may answer an escalation, a pull request
// comment asks for changes.
func (in *Intake) recordComment(ctx context.Context, t Task, onPR bool, number int64, c IssueComment, delivery string) error {
	kind := "issue_comment"
	if onPR {
		kind = "pull_request_comment"
	}
	trusted, err := in.trusted(ctx, t.Repository, c.Author)
	if err != nil {
		return err
	}
	e, err := in.record(ctx, IntakeEvent{
		TaskID: t.ID, Kind: kind, Author: c.Author, Body: c.Body, DeliveryID: delivery, CommentID: c.ID, Trusted: trusted,
	})
	if err != nil {
		return err
	}
	return in.signalEvent(ctx, t.ID, e, number)
}

// record stores e once and returns the stored row: the one just inserted,
// or the one an earlier delivery of the same webhook or a heal-pass
// re-read of the same comment left, so that a signal that failed then is
// sent now.
func (in *Intake) record(ctx context.Context, e IntakeEvent) (IntakeEvent, error) {
	id, err := in.Store.InsertIntakeEvent(ctx, e)
	if err != nil {
		return IntakeEvent{}, err
	}
	if id != 0 {
		e.ID = id
		return e, nil
	}
	events, err := in.Store.IntakeEvents(ctx, e.TaskID)
	if err != nil {
		return IntakeEvent{}, err
	}
	i := slices.IndexFunc(events, func(have IntakeEvent) bool {
		return (e.DeliveryID != "" && have.DeliveryID == e.DeliveryID) ||
			(e.CommentID != 0 && have.Kind == e.Kind && have.CommentID == e.CommentID)
	})
	if i < 0 {
		return IntakeEvent{}, fmt.Errorf("intake event for task %s (%s, delivery %q, comment %d) recorded before but not found", e.TaskID, e.Kind, e.DeliveryID, e.CommentID)
	}
	return events[i], nil
}

// signalEvent delivers a trusted event not yet signalled to the task's
// workflow, once: a pull request comment or rejecting review on pull
// request pr asks for changes, an issue comment may answer an escalation,
// a decision ends the task, an approval is only recorded. The event is
// claimed before the signal, so the webhook path and a heal pass holding
// the same one send it once between them; a failed signal releases the
// claim.
func (in *Intake) signalEvent(ctx context.Context, taskID string, e IntakeEvent, pr int64) error {
	if !e.Trusted || e.Signalled {
		return nil
	}
	name, sig, _ := signalOf(e, pr)
	if name == "" {
		return nil
	}
	if claimed, err := in.Store.SetSignalled(ctx, e.ID, true); err != nil || !claimed {
		return err
	}
	if err := in.Workflows.Signal(ctx, taskID, name, sig); err != nil {
		if _, release := in.Store.SetSignalled(ctx, e.ID, false); release != nil {
			log.Printf("intake: event %d of task %s: signal failed (%v) and its claim stays: %v; nothing will resend it", e.ID, taskID, err, release)
		}
		return err
	}
	return nil
}

// signalOf is the signal event e sends, "" for one that sends none, and
// whether it is a decision that ends the task.
func signalOf(e IntakeEvent, pr int64) (name string, sig TaskSignal, decision bool) {
	sig = TaskSignal{Author: e.Author, Body: e.Body}
	switch e.Kind {
	case "issue_comment":
		name, sig.Comment = SignalHumanComment, e.CommentID
	case "pull_request_comment", "pull_request_review_rejected":
		name, sig.PR = SignalChangesRequested, pr
	case "issue_closed":
		name, decision = SignalIssueClosed, true
	case "pull_request_merged":
		name, sig.PR, decision = SignalPRMerged, pr, true
	case "pull_request_closed":
		name, sig.PR, decision = SignalPRClosed, pr, true
	}
	return name, sig, decision
}

// decisionResignalAfter is how old a signalled decision must be before
// the heal pass sends it again to a task still in review or waiting on a
// human: longer than a workflow takes to wind a decided task down.
const decisionResignalAfter = 15 * time.Minute

// resignalDecision sends decision e again: claimed but never delivered
// (factoryd died between the claim and the signal), it would otherwise
// strand the task. The workflow ignores a decision it already acted on.
func (in *Intake) resignalDecision(ctx context.Context, t Task, e IntakeEvent) error {
	name, sig, decision := signalOf(e, t.PRNumber)
	if !decision || !e.Trusted || !e.Signalled || time.Since(e.CreatedAt) < decisionResignalAfter ||
		(t.Status != "in-review" && t.Status != "waiting-human") {
		return nil
	}
	log.Printf("intake: task %s still %s after decision %d (%s): signalling it again", t.ID, t.Status, e.ID, e.Kind)
	return in.Workflows.Signal(ctx, t.ID, name, sig)
}

// handleReview records a pull request review as an intake event, resolved to
// its task directly from the payload's own head branch (no extra API call:
// unlike a comment, a review payload already carries the pull request).
func (in *Intake) handleReview(ctx context.Context, body []byte, eventType, delivery string) error {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Sender.UserName == in.BotUser {
		return nil
	}
	taskID, ok := in.taskOfHead(p.Repository.FullName, p.PullRequest.Head.Repo.FullName, p.PullRequest.Head.Ref)
	if !ok {
		return nil
	}
	if _, ok, err := in.agentTask(ctx, taskID); err != nil || !ok {
		return err
	}
	reviewBody := ""
	if p.Review != nil {
		reviewBody = p.Review.Content
	}
	trusted, err := in.trusted(ctx, p.Repository.FullName, p.Sender.UserName)
	if err != nil {
		return err
	}
	e, err := in.record(ctx, IntakeEvent{
		TaskID: taskID, Kind: eventType, Author: p.Sender.UserName, Body: reviewBody, DeliveryID: delivery, Trusted: trusted,
	})
	if err != nil {
		return err
	}
	return in.signalEvent(ctx, taskID, e, p.PullRequest.Index)
}

// healAuthor names the heal pass as the author of a state change it read
// from Forgejo rather than received as a webhook.
const healAuthor = "factoryd heal pass"

// Resync re-reads what Forgejo holds for task t and signals the workflow
// what it may have missed: a webhook is delivered once, so a merge, close
// or comment made while factoryd was down or erroring is otherwise lost.
// Terminal states (pull request merged or closed, issue closed) are
// recorded and signalled once, like comments; a decision that has not
// moved the task out of review or the human wait within
// decisionResignalAfter is signalled again on every pass (the claim may
// have outlived a factoryd that died before the signal). A closed issue
// is credited to its last closer per the timeline and gated like a
// webhook's sender. A trusted event recorded but not signalled (factoryd
// died between the two) is signalled now. Reviews carry no id
// in their webhook payload and are not re-read.
func (in *Intake) Resync(ctx context.Context, t Task) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	if t.Kind == "human" {
		if t.IssueNumber == 0 {
			return nil
		}
		closer, err := in.Forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
		if err != nil || closer == "" {
			return err
		}
		return in.issueClosed(ctx, t, closer)
	}
	events, err := in.Store.IntakeEvents(ctx, t.ID)
	if err != nil {
		return err
	}
	if t.PRNumber != 0 {
		pr, err := in.Forgejo.PullRequestState(ctx, repo.Owner, repo.Name, t.PRNumber)
		if err != nil {
			return fmt.Errorf("pull request state: %w", err)
		}
		switch {
		case pr.Merged:
			err = in.decide(ctx, t.ID, "pull_request_merged", healAuthor, t.PRNumber)
		case pr.State == "closed":
			err = in.decide(ctx, t.ID, "pull_request_closed", healAuthor, t.PRNumber)
		}
		if err != nil {
			return err
		}
		if err := in.resyncComments(ctx, t, true, t.PRNumber, events); err != nil {
			return err
		}
	}
	// an issue whose closer cannot be read still has its comments re-read
	var closeErr error
	if t.IssueNumber != 0 {
		closer, err := in.Forgejo.IssueClosedBy(ctx, repo.Owner, repo.Name, t.IssueNumber)
		switch {
		case err != nil:
			closeErr = fmt.Errorf("issue state: %w", err)
		case closer != "":
			if err := in.issueClosed(ctx, t, closer); err != nil {
				return err
			}
		}
		if err := in.resyncComments(ctx, t, false, t.IssueNumber, events); err != nil {
			return err
		}
	}
	for _, e := range events {
		if err := in.signalEvent(ctx, t.ID, e, t.PRNumber); err != nil {
			return err
		}
		if err := in.resignalDecision(ctx, t, e); err != nil {
			return err
		}
	}
	return closeErr
}

// resyncComments records and signals the comments on issue or pull request
// number that none of the task's known events cover. A comment event
// recorded before comment ids were (migration 0010) has id 0 and matches
// no comment, so any comment no newer than the latest such event counts
// as covered rather than recorded and signalled twice.
func (in *Intake) resyncComments(ctx context.Context, t Task, onPR bool, number int64, known []IntakeEvent) error {
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return err
	}
	comments, err := in.Forgejo.ListIssueComments(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return fmt.Errorf("comments: %w", err)
	}
	kind := "issue_comment"
	if onPR {
		kind = "pull_request_comment"
	}
	var idless time.Time
	for _, e := range known {
		if e.CommentID == 0 && (e.Kind == "issue_comment" || e.Kind == "pull_request_comment") && e.CreatedAt.After(idless) {
			idless = e.CreatedAt
		}
	}
	for _, c := range comments {
		if c.Author == in.BotUser || !c.CreatedAt.After(idless) || slices.ContainsFunc(known, func(e IntakeEvent) bool { return e.Kind == kind && e.CommentID == c.ID }) {
			continue
		}
		if err := in.recordComment(ctx, t, onPR, number, c, ""); err != nil {
			return err
		}
	}
	return nil
}
