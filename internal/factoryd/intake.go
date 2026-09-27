package factoryd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
)

// Intake is the Forgejo webhook endpoint (task 42): every issue labelled
// agent or human becomes a task row, and every PR/issue comment or review
// becomes an intake event for task 43's workflow to consume. It implements
// http.Handler directly; main wires it at POST /webhooks/forgejo.
type Intake struct {
	Store        Store
	Forgejo      *Forgejo
	BotUser      string // factory-agent: its own issues/comments never spawn tasks or events
	Secret       string // HMAC-SHA256 webhook secret, shared with Forgejo's webhook config
	MaxBudgetEUR float64
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
		log.Printf("webhook intake: rejected %s: bad signature", eventType)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	var procErr error
	switch eventType {
	case "issues", "issue_assign", "issue_label", "issue_milestone":
		procErr = in.handleIssue(ctx, body, eventType)
	case "issue_comment", "pull_request_comment":
		procErr = in.handleIssueComment(ctx, body, eventType)
	case "pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment":
		procErr = in.handleReview(ctx, body, eventType)
	default:
		// an event type this factory does not act on yet: accepted so
		// Forgejo's delivery list reads all 2xx, nothing to process
	}
	if procErr != nil {
		log.Printf("webhook intake: %s: %v", eventType, procErr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
	Issue      webhookIssue      `json:"issue"`
	Repository webhookRepository `json:"repository"`
}

type webhookComment struct {
	Poster webhookUser `json:"user"`
	Body   string      `json:"body"`
}

type issueCommentPayload struct {
	Issue      webhookIssue      `json:"issue"`
	Comment    webhookComment    `json:"comment"`
	Repository webhookRepository `json:"repository"`
	IsPull     bool              `json:"is_pull"`
}

type prBranchInfo struct {
	Ref string `json:"ref"`
}

type webhookPullRequest struct {
	Index int64        `json:"number"`
	Head  prBranchInfo `json:"head"`
}

type reviewPayload struct {
	Content string `json:"content"`
}

type pullRequestPayload struct {
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

// ClampBudget defaults an unset or non-positive requested budget to 2.0 and
// caps it at max. Shared by POST /tasks and the issue-opened intake path so
// the two task-creation entry points never drift apart on this rule.
func ClampBudget(requested, max float64) float64 {
	if requested <= 0 {
		requested = 2.0
	}
	return min(requested, max)
}

// handleIssue turns an issue carrying the agent or human label into a task
// row, idempotent per (repository, issue number): a redelivery, or a later
// label event on the same issue, finds the task already linked and no-ops.
func (in *Intake) handleIssue(ctx context.Context, body []byte, eventType string) error {
	var p issuesPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Issue.Poster.UserName == in.BotUser {
		return nil // the bot's own issues never spawn a second task
	}
	names := labelNames(p.Issue.Labels)
	hasAgent := slices.Contains(names, "agent")
	hasHuman := slices.Contains(names, "human")
	if !hasAgent && !hasHuman {
		return nil
	}
	repo := p.Repository.FullName
	if _, ok, err := in.Store.TaskByIssue(ctx, repo, p.Issue.Index); err != nil {
		return fmt.Errorf("task by issue: %w", err)
	} else if ok {
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
		BudgetEUR:   ClampBudget(0, in.MaxBudgetEUR),
		Status:      "queued",
		Objective:   objectiveFromIssue(p.Issue),
		Repository:  repo,
		Branch:      "factory/" + id,
		Kind:        kind,
		IssueNumber: p.Issue.Index,
	}
	if kind == "agent" {
		repoParts, err := SplitRepo(repo)
		if err != nil {
			return err
		}
		branch := p.Repository.DefaultBranch
		if branch == "" {
			branch, err = in.Forgejo.RepoDefaultBranch(ctx, repoParts.Owner, repoParts.Name)
			if err != nil {
				return fmt.Errorf("repo default branch: %w", err)
			}
		}
		sha, err := in.Forgejo.BranchHeadSHA(ctx, repoParts.Owner, repoParts.Name, branch)
		if err != nil {
			return fmt.Errorf("branch head sha: %w", err)
		}
		task.SHA = sha
	}
	if err := in.Store.CreateTask(ctx, task); err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	// state/queued for the agent lane; the human lane has no runner to claim
	// it, so it gets its own terminal-looking label, state/waiting-human,
	// per architecture 5.6's "waiting on a human" board column.
	label := "queued"
	if kind == "human" {
		label = "waiting-human"
	}
	mirrorTaskState(ctx, in.Forgejo, task, label)
	return nil
}

// handleIssueComment records a comment as an intake event against the task
// it belongs to: directly by issue number for an issue comment, or by
// resolving the pull request's head branch (factory/<task id>) for a pull
// request comment. A comment on anything else the factory did not file
// (no matching task) is silently dropped.
func (in *Intake) handleIssueComment(ctx context.Context, body []byte, eventType string) error {
	var p issueCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Comment.Poster.UserName == in.BotUser {
		return nil
	}
	repo := p.Repository.FullName

	var taskID string
	if p.IsPull {
		repoParts, err := SplitRepo(repo)
		if err != nil {
			return err
		}
		ref, err := in.Forgejo.PullRequestHeadBranch(ctx, repoParts.Owner, repoParts.Name, p.Issue.Index)
		if err != nil {
			return fmt.Errorf("pull request head branch: %w", err)
		}
		id, ok := strings.CutPrefix(ref, "factory/")
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
	if _, ok, err := in.Store.Task(ctx, taskID); err != nil {
		return err
	} else if !ok {
		return nil
	}
	return in.Store.InsertIntakeEvent(ctx, IntakeEvent{
		TaskID: taskID, Kind: eventType, Author: p.Comment.Poster.UserName, Body: p.Comment.Body,
	})
}

// handleReview records a pull request review as an intake event, resolved to
// its task directly from the payload's own head branch (no extra API call:
// unlike a comment, a review payload already carries the pull request).
func (in *Intake) handleReview(ctx context.Context, body []byte, eventType string) error {
	var p pullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("decode %s: %w", eventType, err)
	}
	if p.Sender.UserName == in.BotUser {
		return nil
	}
	taskID, ok := strings.CutPrefix(p.PullRequest.Head.Ref, "factory/")
	if !ok {
		return nil
	}
	if _, ok, err := in.Store.Task(ctx, taskID); err != nil {
		return err
	} else if !ok {
		return nil
	}
	reviewBody := ""
	if p.Review != nil {
		reviewBody = p.Review.Content
	}
	return in.Store.InsertIntakeEvent(ctx, IntakeEvent{
		TaskID: taskID, Kind: eventType, Author: p.Sender.UserName, Body: reviewBody,
	})
}
