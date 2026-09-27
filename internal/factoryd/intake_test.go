package factoryd

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const webhookSecret = "test-webhook-secret"

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// intakeForgejoStub answers the handful of read endpoints handleIssue and
// handleIssueComment call out to: repo default branch, branch head SHA, and
// a pull request's head branch. requests records every path hit, so tests
// can assert a lookup was — or was not — made.
func intakeForgejoStub(t *testing.T) (*Forgejo, *[]string) {
	t.Helper()
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"sha-main-head"}}`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			io.WriteString(w, `{"head":{"ref":"factory/t-pr"}}`)
		case strings.Count(r.URL.Path, "/") == 4: // /api/v1/repos/{owner}/{repo}
			io.WriteString(w, `{"default_branch":"main"}`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}, &requests
}

func newTestIntake(t *testing.T) (*Intake, *fakeStore, *[]string) {
	t.Helper()
	forgejo, requests := intakeForgejoStub(t)
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, BotUser: "factory-agent", Secret: webhookSecret, MaxBudgetEUR: 5}
	return in, store, requests
}

func postWebhook(in *Intake, eventType string, body []byte, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(body))
	req.Header.Set("X-Forgejo-Event-Type", eventType)
	if signature != "" {
		req.Header.Set("X-Forgejo-Signature", signature)
	}
	rr := httptest.NewRecorder()
	in.ServeHTTP(rr, req)
	return rr
}

func issueOpenedBody(number int64, poster string, labels []string, title, body string) []byte {
	var lbls []map[string]string
	for _, l := range labels {
		lbls = append(lbls, map[string]string{"name": l})
	}
	b, _ := json.Marshal(map[string]any{
		"action": "opened",
		"issue": map[string]any{
			"number": number, "title": title, "body": body,
			"user":   map[string]string{"login": poster},
			"labels": lbls,
		},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code", "default_branch": "main"},
	})
	return b
}

func TestIntakeSignatureValid(t *testing.T) {
	in, _, _ := newTestIntake(t)
	body := issueOpenedBody(1, "sebastian", []string{"agent"}, "Add X", "do the thing")
	rr := postWebhook(in, "issues", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
}

func TestIntakeSignatureMissing(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := issueOpenedBody(1, "sebastian", []string{"agent"}, "Add X", "do the thing")
	rr := postWebhook(in, "issues", body, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("a task was created despite the missing signature")
	}
}

func TestIntakeSignatureInvalid(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := issueOpenedBody(1, "sebastian", []string{"agent"}, "Add X", "do the thing")
	rr := postWebhook(in, "issues", body, sign("wrong-secret", body))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("a task was created despite the wrong secret")
	}
}

func TestIntakeSignatureTampered(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := issueOpenedBody(1, "sebastian", []string{"agent"}, "Add X", "do the thing")
	validSig := sign(webhookSecret, body)
	tampered := append(bytes.Clone(body), '\n') // the delivered body no longer matches what was signed
	rr := postWebhook(in, "issues", tampered, validSig)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("a task was created despite the tampered body")
	}
}

func TestIntakeSignatureNeverReadsBodyBeforeVerifying(t *testing.T) {
	// A malformed body must still 401, never a 400 or 500 from JSON decoding:
	// the brief requires rejection strictly before parsing.
	in, _, _ := newTestIntake(t)
	rr := postWebhook(in, "issues", []byte("not json at all"), "0000")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (rejected before parsing)", rr.Code)
	}
}

func TestIntakeIssueOpenedAgentLabelCreatesTask(t *testing.T) {
	in, store, requests := newTestIntake(t)
	body := issueOpenedBody(7, "sebastian", []string{"agent", "priority/p1"}, "Add the runner", "objective body")
	rr := postWebhook(in, "issues", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(store.tasks))
	}
	var task Task
	for _, tk := range store.tasks {
		task = tk
	}
	if task.Kind != "agent" || task.IssueNumber != 7 || task.Repository != "sf/ai-software-factory-code" {
		t.Fatalf("task = %+v", task)
	}
	if task.Priority != 2 {
		t.Fatalf("priority = %d, want 2 (priority/p1)", task.Priority)
	}
	if task.Objective != "Add the runner\n\nobjective body" {
		t.Fatalf("objective = %q", task.Objective)
	}
	if task.SHA != "sha-main-head" {
		t.Fatalf("sha = %q, want the resolved branch head", task.SHA)
	}
	if task.Branch != "factory/"+task.ID {
		t.Fatalf("branch = %q", task.Branch)
	}
	if task.BudgetEUR != 2.0 {
		t.Fatalf("budget = %v, want the 2.0 default", task.BudgetEUR)
	}
	// default_branch came from the payload itself; RepoDefaultBranch must not
	// have been called, only the branch-head lookup.
	for _, p := range *requests {
		if strings.HasSuffix(p, "/repos/sf/ai-software-factory-code") {
			t.Fatalf("RepoDefaultBranch was called even though the payload carried default_branch: %v", *requests)
		}
	}
}

func TestIntakeIssueHumanLabelSkipsAgentWork(t *testing.T) {
	in, store, requests := newTestIntake(t)
	body := issueOpenedBody(9, "sebastian", []string{"human"}, "Needs a decision", "please look")
	rr := postWebhook(in, "issues", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	var task Task
	for _, tk := range store.tasks {
		task = tk
	}
	if task.Kind != "human" || task.IssueNumber != 9 {
		t.Fatalf("task = %+v", task)
	}
	if task.SHA != "" {
		t.Fatalf("sha = %q, want empty: the human lane never runs the agent", task.SHA)
	}
	if len(*requests) != 0 {
		t.Fatalf("forgejo was called for a human-lane issue: %v", *requests)
	}
}

func TestIntakeIssueRedeliveryIsIdempotent(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := issueOpenedBody(3, "sebastian", []string{"agent"}, "Add Y", "body")
	sig := sign(webhookSecret, body)
	postWebhook(in, "issues", body, sig)
	postWebhook(in, "issue_label", body, sig) // a second event on the same issue: labelled again
	if len(store.tasks) != 1 {
		t.Fatalf("tasks = %d, want exactly 1 across both deliveries", len(store.tasks))
	}
}

func TestIntakeIssueBotAuthoredIsIgnored(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := issueOpenedBody(4, "factory-agent", []string{"agent"}, "self-filed", "body")
	rr := postWebhook(in, "issues", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("a bot-authored issue spawned a task")
	}
}

func TestIntakeIssueWithoutAgentOrHumanLabelIgnored(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := issueOpenedBody(5, "sebastian", []string{"question"}, "just asking", "body")
	rr := postWebhook(in, "issues", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("an unlabelled issue spawned a task")
	}
}

func TestIntakePriorityFromLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   int
	}{
		{nil, 0},
		{[]string{"priority/p0"}, 3},
		{[]string{"priority/p3"}, 0},
		{[]string{"severity/critical"}, 3},
		{[]string{"severity/low"}, 0},
		{[]string{"priority/p2", "severity/high"}, 2}, // higher of the two mappings
		{[]string{"priority/p3", "severity/critical"}, 3},
	}
	for _, c := range cases {
		if got := priorityFromLabels(c.labels); got != c.want {
			t.Fatalf("priorityFromLabels(%v) = %d, want %d", c.labels, got, c.want)
		}
	}
}

func TestIntakeIssueCommentRecordsEvent(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", IssueNumber: 12, Status: "running", Kind: "agent"}
	body, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 12},
		"comment":    map[string]any{"user": map[string]string{"login": "sebastian"}, "body": "looks good"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    false,
	})
	rr := postWebhook(in, "issue_comment", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.intakeEvents) != 1 {
		t.Fatalf("intake events = %d, want 1", len(store.intakeEvents))
	}
	ev := store.intakeEvents[0]
	if ev.TaskID != "t1" || ev.Kind != "issue_comment" || ev.Author != "sebastian" || ev.Body != "looks good" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestIntakeIssueCommentBotAuthoredIgnored(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", IssueNumber: 12, Status: "running"}
	body, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 12},
		"comment":    map[string]any{"user": map[string]string{"login": "factory-agent"}, "body": "posting status"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    false,
	})
	postWebhook(in, "issue_comment", body, sign(webhookSecret, body))
	if len(store.intakeEvents) != 0 {
		t.Fatalf("a bot-authored comment was recorded")
	}
}

func TestIntakePullRequestCommentResolvesByHeadBranch(t *testing.T) {
	in, store, requests := newTestIntake(t)
	store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "running"}
	body, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 21},
		"comment":    map[string]any{"user": map[string]string{"login": "sebastian"}, "body": "one nit"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	rr := postWebhook(in, "pull_request_comment", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	found := false
	for _, p := range *requests {
		if strings.Contains(p, "/pulls/21") {
			found = true
		}
	}
	if !found {
		t.Fatalf("PullRequestHeadBranch was not called: %v", *requests)
	}
	if len(store.intakeEvents) != 1 || store.intakeEvents[0].TaskID != "t-pr" {
		t.Fatalf("intake events = %+v", store.intakeEvents)
	}
}

func TestIntakePullRequestCommentOnNonFactoryBranchIgnored(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 99},
		"comment":    map[string]any{"user": map[string]string{"login": "sebastian"}, "body": "hi"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	// the stub always answers pulls/* with head ref factory/t-pr, which has
	// no matching task row here, so nothing should be recorded
	postWebhook(in, "pull_request_comment", body, sign(webhookSecret, body))
	if len(store.intakeEvents) != 0 {
		t.Fatalf("an event was recorded for a PR with no matching task")
	}
}

func reviewBody(ref, sender, reviewType, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{"number": 5, "head": map[string]string{"ref": ref}},
		"repository":   map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":       map[string]string{"login": sender},
		"review":       map[string]string{"content": content, "type": reviewType},
	})
	return b
}

func TestIntakeReviewRecordsEvent(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "running"}
	body := reviewBody("factory/t1", "sebastian", "pull_request_review_approved", "ship it")
	rr := postWebhook(in, "pull_request_review_approved", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.intakeEvents) != 1 {
		t.Fatalf("intake events = %d, want 1", len(store.intakeEvents))
	}
	ev := store.intakeEvents[0]
	if ev.TaskID != "t1" || ev.Kind != "pull_request_review_approved" || ev.Author != "sebastian" || ev.Body != "ship it" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestIntakeReviewOnUnknownTaskIgnored(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := reviewBody("factory/nope", "sebastian", "pull_request_review_rejected", "changes needed")
	postWebhook(in, "pull_request_review_rejected", body, sign(webhookSecret, body))
	if len(store.intakeEvents) != 0 {
		t.Fatalf("an event was recorded for an unknown task")
	}
}

func TestIntakeUnknownEventTypeAccepted(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body := []byte(`{"ref":"refs/heads/main"}`)
	rr := postWebhook(in, "push", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unrecognized events are accepted, not processed)", rr.Code)
	}
	if len(store.tasks) != 0 || len(store.intakeEvents) != 0 {
		t.Fatalf("a push event was processed")
	}
}

// TestIntakeBodySizeIsBounded documents the MaxBytesReader bound: a body
// past the limit fails at the read, before signature verification even runs.
func TestIntakeBodySizeIsBounded(t *testing.T) {
	in, _, _ := newTestIntake(t)
	huge := bytes.Repeat([]byte("a"), (1<<20)+1)
	rr := postWebhook(in, "issues", huge, sign(webhookSecret, huge))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
}
