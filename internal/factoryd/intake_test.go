package factoryd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
// handleIssueComment call out to: repo default branch, branch head SHA, a
// pull request's head branch, and the issue-labels GET/PUT pair behind
// SetStateLabel (state mirroring, fix round 1). requests records every path
// hit, so tests can assert a lookup was — or was not — made; a test can read
// back the labels the stub now holds for an issue via the same *Forgejo's
// IssueLabels.
func intakeForgejoStub(t *testing.T) (*Forgejo, *[]string) {
	t.Helper()
	var requests []string
	labels := map[string][]string{} // keyed by the issue's labels path
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/permission"):
			// the registrar token reads permissions; the bot cannot
			if r.Header.Get("Authorization") != "token registrar-tok" {
				t.Errorf("permission read without the registrar token: %q", r.Header.Get("Authorization"))
			}
			switch strings.Split(r.URL.Path, "/")[7] {
			case "sebastian":
				io.WriteString(w, `{"permission":"owner"}`)
			case "reader":
				io.WriteString(w, `{"permission":"read"}`)
			default:
				http.NotFound(w, r)
			}
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code":
			io.WriteString(w, `{"default_branch":"main"}`)
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"sha-main-head"}}`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			io.WriteString(w, `{"head":{"ref":"factory/t-pr","repo":{"full_name":"factory-agent/ai-software-factory-code"}}}`)
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == http.MethodGet:
			names := labels[r.URL.Path]
			out := make([]map[string]string, len(names))
			for i, n := range names {
				out[i] = map[string]string{"name": n}
			}
			json.NewEncoder(w).Encode(out) // #nosec G104 -- best-effort test stub response
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == http.MethodPut:
			var payload struct {
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&payload) // #nosec G104 -- test stub, malformed body just yields nil labels
			labels[r.URL.Path] = payload.Labels
			io.WriteString(w, `[]`)
		case strings.Count(r.URL.Path, "/") == 4: // /api/v1/repos/{owner}/{repo}
			io.WriteString(w, `{"default_branch":"main"}`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", CIRegistrarToken: "registrar-tok"}, &requests
}

// issueLabels reads back the labels a test's Forgejo stub currently holds
// for an issue, exercising the same GET SetStateLabel itself depends on.
func issueLabels(t *testing.T, f *Forgejo, issueNumber int64) []string {
	t.Helper()
	names, err := f.IssueLabels(context.Background(), "sf", "ai-software-factory-code", issueNumber)
	if err != nil {
		t.Fatalf("issue labels: %v", err)
	}
	return names
}

// fakeWorkflows records workflow starts and signals.
type fakeWorkflows struct {
	started []string
	signals []string // "<task> <signal> <author> #<pr>: <body>"
	err     error    // returned by Signal when set
}

func (w *fakeWorkflows) Start(ctx context.Context, taskID string) error {
	w.started = append(w.started, taskID)
	return nil
}

func (w *fakeWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	if w.err != nil {
		return w.err
	}
	comment := ""
	if sig.Comment != 0 {
		comment = fmt.Sprintf(" c%d", sig.Comment)
	}
	w.signals = append(w.signals, fmt.Sprintf("%s %s %s #%d%s: %s", taskID, name, sig.Author, sig.PR, comment, sig.Body))
	return nil
}

func newTestIntake(t *testing.T) (*Intake, *fakeStore, *[]string) {
	t.Helper()
	forgejo, requests := intakeForgejoStub(t)
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, MaxBudgetEUR: 5}
	return in, store, requests
}

func workflowsOf(in *Intake) *fakeWorkflows { return in.Workflows.(*fakeWorkflows) }

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
	logs := captureLog(t)
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
	if task.Kind != "agent" || task.Source != "webhook" || task.IssueNumber != 7 || task.Repository != "sf/ai-software-factory-code" {
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
	// fix round 1: a webhook-created agent task must carry state/queued
	// immediately, not only once a runner later claims it.
	if labels := issueLabels(t, in.Forgejo, 7); !slices.Contains(labels, "state/queued") {
		t.Fatalf("labels = %v, want state/queued", labels)
	}
	if want := "webhook intake: issue #7 on sf/ai-software-factory-code by sebastian: agent task " + task.ID; !strings.Contains(logs.String(), want) {
		t.Fatalf("log = %q, want a line containing %q", logs.String(), want)
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
	// no branch/SHA resolution for the human lane: the only forgejo calls
	// allowed are the state-label mirror's own GET/PUT.
	for _, p := range *requests {
		if strings.HasSuffix(p, "/branches/main") || strings.HasSuffix(p, "/repos/sf/ai-software-factory-code") {
			t.Fatalf("forgejo was called to resolve the agent's branch/SHA for a human-lane issue: %v", *requests)
		}
	}
	// fix round 1: the human lane gets its own consistent label
	// (architecture 5.6's "waiting on a human" board column), not
	// state/queued, since no runner will ever claim a human-lane task.
	if labels := issueLabels(t, in.Forgejo, 9); !slices.Contains(labels, "state/waiting-human") {
		t.Fatalf("labels = %v, want state/waiting-human", labels)
	}
}

// TestIntakeIssueOpenedLabelMirrorFailureIsLogOnly asserts a Forgejo hiccup
// while mirroring state/queued never fails the webhook nor loses the task
// row: mirroring is log-only, same as the runner's and completion's.
func TestIntakeIssueOpenedLabelMirrorFailureIsLogOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"sha-main-head"}}`)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			http.Error(w, "forgejo unavailable", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	forgejo := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, MaxBudgetEUR: 5}
	body := issueOpenedBody(11, "sebastian", []string{"agent"}, "Add Z", "body")
	rr := postWebhook(in, "issues", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200: a label-mirror failure must not fail the webhook", rr.Code, rr.Body)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1: the task must still be created despite the mirror failure", len(store.tasks))
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
		"pull_request": map[string]any{"number": 5, "head": prHead(ref, "factory-agent/ai-software-factory-code")},
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

func TestIntakeAgentIssueStartsWorkflowHumanDoesNot(t *testing.T) {
	in, store, _ := newTestIntake(t)
	agent := issueOpenedBody(7, "sebastian", []string{"agent"}, "Add X", "")
	human := issueOpenedBody(8, "sebastian", []string{"human"}, "Decide Y", "")
	postWebhook(in, "issues", agent, sign(webhookSecret, agent))
	postWebhook(in, "issues", human, sign(webhookSecret, human))
	agentTask, _, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", 7)
	if got := workflowsOf(in).started; len(got) != 1 || got[0] != agentTask.ID {
		t.Fatalf("started = %v, want only the agent task %s", got, agentTask.ID)
	}
	// a redelivery while the task is still queued retries the start
	postWebhook(in, "issue_label", agent, sign(webhookSecret, agent))
	if got := workflowsOf(in).started; len(got) != 2 || got[1] != agentTask.ID || len(store.tasks) != 2 {
		t.Fatalf("started = %v, tasks = %d; want a second start for the same task, no new task", got, len(store.tasks))
	}
}

func TestIntakePullRequestCommentSignalsChangesRequested(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "in-review"}
	store.tasks["t-issue"] = Task{ID: "t-issue", Repository: "sf/ai-software-factory-code", Status: "in-review", IssueNumber: 30}
	pr, _ := json.Marshal(map[string]any{
		"issue":      map[string]any{"number": 21},
		"comment":    map[string]any{"user": map[string]string{"login": "sebastian"}, "body": "one nit"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	issue, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 30},
		"comment":    map[string]any{"id": 77, "user": map[string]string{"login": "sebastian"}, "body": "a note on the issue"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
	})
	edited, _ := json.Marshal(map[string]any{
		"action":     "edited",
		"issue":      map[string]any{"number": 30},
		"comment":    map[string]any{"id": 77, "user": map[string]string{"login": "sebastian"}, "body": "a note, edited"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
	})
	postWebhook(in, "pull_request_comment", pr, sign(webhookSecret, pr))
	postWebhook(in, "issue_comment", issue, sign(webhookSecret, issue))
	postWebhook(in, "issue_comment", edited, sign(webhookSecret, edited))
	want := []string{"t-pr changes_requested sebastian #21: one nit", "t-issue human_comment sebastian #0 c77: a note on the issue"}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want %q: the PR comment requests changes, a new issue comment is a human comment", got, want)
	}
}

func TestIntakeReviewRejectedSignalsApprovedDoesNot(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review"}
	rejected := reviewBody("factory/t1", "sebastian", "pull_request_review_rejected", "split the function")
	approved := reviewBody("factory/t1", "sebastian", "pull_request_review_approved", "ship it")
	postWebhook(in, "pull_request_review_rejected", rejected, sign(webhookSecret, rejected))
	postWebhook(in, "pull_request_review_approved", approved, sign(webhookSecret, approved))
	if got := workflowsOf(in).signals; len(got) != 1 || got[0] != "t1 changes_requested sebastian #5: split the function" {
		t.Fatalf("signals = %q", got)
	}
}

func prHead(ref, repo string) map[string]any {
	return map[string]any{"ref": ref, "repo": map[string]string{"full_name": repo}}
}

func prClosedBody(ref string, merged bool) []byte {
	return prClosedBodyFrom(ref, "factory-agent/ai-software-factory-code", merged)
}

func prClosedBodyFrom(ref, headRepo string, merged bool) []byte {
	return prClosedBodyBy(ref, headRepo, "sebastian", merged)
}

func prClosedBodyBy(ref, headRepo, sender string, merged bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"action":       "closed",
		"pull_request": map[string]any{"number": 5, "merged": merged, "head": prHead(ref, headRepo)},
		"repository":   map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":       map[string]string{"login": sender},
	})
	return b
}

func TestIntakeMergedPullRequestSignalsWorkflow(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review"}
	// only a head on the bot's fork is the task's PR: a same-named branch on
	// sf or on a stranger's fork is not (ruling 97); the bot's own close
	// (Finish, a .forgejo refusal) is no signal either, Reconcile reads it
	for _, body := range [][]byte{prClosedBody("factory/t1", true), prClosedBody("factory/t1", false), prClosedBody("feature/x", true),
		prClosedBodyFrom("factory/t1", "sf/ai-software-factory-code", true), prClosedBodyFrom("factory/t1", "stranger/ai-software-factory-code", true),
		prClosedBodyBy("factory/t1", "factory-agent/ai-software-factory-code", "factory-agent", false)} {
		if rr := postWebhook(in, "pull_request", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
	}
	if got := workflowsOf(in).signals; len(got) != 2 || got[0] != "t1 pr_merged sebastian #5: " || got[1] != "t1 pr_closed sebastian #5: " {
		t.Fatalf("signals = %q, want pr_merged and pr_closed for the factory PR only", got)
	}
}

func TestIntakeIssueClosedSignalsWorkflow(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 4}
	body, _ := json.Marshal(map[string]any{
		"action":     "closed",
		"issue":      map[string]any{"number": 4, "user": map[string]string{"login": "factory-agent"}},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":     map[string]string{"login": "sebastian"},
	})
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := workflowsOf(in).signals; len(got) != 1 || got[0] != "t1 issue_closed sebastian #0: " {
		t.Fatalf("signals = %q", got)
	}
}

// Forgejo can deliver the same webhook twice; a processed delivery id is a
// no-op, and a delivery whose processing failed is processed again. A
// redelivery that slips through (a crash between processing and marking)
// adds no second intake event.
func TestIntakeRedeliveryIsProcessedOnce(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review"}
	body := reviewBody("factory/t1", "sebastian", "pull_request_review_rejected", "split it")
	post := func(delivery string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/forgejo", bytes.NewReader(body))
		req.Header.Set("X-Forgejo-Event-Type", "pull_request_review_rejected")
		req.Header.Set("X-Forgejo-Signature", sign(webhookSecret, body))
		req.Header.Set("X-Forgejo-Delivery", delivery)
		rr := httptest.NewRecorder()
		in.ServeHTTP(rr, req)
		return rr.Code
	}
	workflowsOf(in).err = errors.New("temporal down")
	if code := post("d-1"); code != http.StatusInternalServerError {
		t.Fatalf("failed delivery = %d, want 500", code)
	}
	workflowsOf(in).err = nil
	for range 2 {
		if code := post("d-1"); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
	}
	if got := workflowsOf(in).signals; len(got) != 1 {
		t.Fatalf("signals = %q, want exactly one across the failed try and two redeliveries", got)
	}
	if len(store.intakeEvents) != 1 || store.intakeEvents[0].DeliveryID != "d-1" {
		t.Fatalf("intake events = %+v, want one, keyed by the delivery", store.intakeEvents)
	}
}

// M2: only people with write access steer a task; anyone else's comment or
// review is recorded and signals nothing. Verdicts are cached.
func TestIntakeSignalsOnlyTrustedCommenters(t *testing.T) {
	in, store, requests := newTestIntake(t)
	store.tasks["t-issue"] = Task{ID: "t-issue", Repository: "sf/ai-software-factory-code", Status: "waiting-human", IssueNumber: 30, Kind: "agent"}
	comment := func(user string, id int) {
		b, _ := json.Marshal(map[string]any{
			"action":     "created",
			"issue":      map[string]any{"number": 30},
			"comment":    map[string]any{"id": id, "user": map[string]string{"login": user}, "body": "do X"},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", user, rr.Code)
		}
	}
	comment("mallory", 1)
	comment("reader", 2)
	comment("reader", 5)
	comment("sebastian", 3)
	comment("sebastian", 4)
	want := []string{"t-issue human_comment sebastian #0 c3: do X", "t-issue human_comment sebastian #0 c4: do X"}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want only the owner's", got)
	}
	if len(store.intakeEvents) != 5 {
		t.Fatalf("intake events = %d, want every comment recorded", len(store.intakeEvents))
	}
	checks := map[string]int{}
	for _, r := range *requests {
		if strings.HasSuffix(r, "/permission") {
			checks[strings.Split(r, "/")[7]]++
		}
	}
	// a grant is reused, a denial is checked again every time
	if checks["sebastian"] != 1 || checks["reader"] != 2 {
		t.Fatalf("permission reads = %v, want sebastian 1 (cached), reader 2", checks)
	}
}

// m5: closing the issue fails the task, so only people with write access,
// or factoryd's bot, can do it through the webhook.
func TestIntakeIssueClosedNeedsWriteAccess(t *testing.T) {
	for closer, want := range map[string]int{"mallory": 0, "reader": 0, "sebastian": 1, "factory-agent": 1} {
		in, store, _ := newTestIntake(t)
		store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "agent", IssueNumber: 4}
		body, _ := json.Marshal(map[string]any{
			"action":     "closed",
			"issue":      map[string]any{"number": 4, "user": map[string]string{"login": "mallory"}},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"sender":     map[string]string{"login": closer},
		})
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", closer, rr.Code)
		}
		if got := len(workflowsOf(in).signals); got != want {
			t.Fatalf("%s closed the issue: %d signals, want %d", closer, got, want)
		}
	}
}
