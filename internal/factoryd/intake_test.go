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
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
// SetStateLabel (state mirroring). requests records every path
// hit, so tests can assert a lookup was — or was not — made; a test can read
// back the labels the stub now holds for an issue via the same *Forgejo's
// IssueLabels.
// openIssues are the open issues intakeForgejoStub lists.
var openIssues = []string{
	`{"number":7,"title":"Add the runner","body":"objective body","user":{"login":"sebastian"},"labels":[{"name":"agent"},{"name":"priority/p1"}]}`,
	`{"number":8,"title":"Filed by the bot","user":{"login":"factory-agent"},"labels":[{"name":"agent"}]}`,
	`{"number":10,"title":"Already a task","user":{"login":"sebastian"},"labels":[{"name":"agent"}]}`,
	`{"number":9,"title":"Needs a decision","user":{"login":"sebastian"},"labels":[{"name":"human"}]}`,
	`{"number":11,"title":"Unlabelled","user":{"login":"outsider"},"labels":[]}`,
	`{"number":12,"title":"A wish","user":{"login":"outsider"},"labels":[{"name":"improvement"}]}`,
}

func intakeForgejoStub(t *testing.T) (*Forgejo, *[]string) {
	t.Helper()
	var requests []string
	labels := map[string][]string{} // keyed by the issue's labels path
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch {
		case isWriterRead(r.URL.Path):
			serveWriters(t, w, r)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code":
			io.WriteString(w, `{"default_branch":"main"}`)
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"sha-main-head"}}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/factory-agent/") && strings.Contains(r.URL.Path, "/branches/"):
			if r.Method == http.MethodDelete {
				requests = append(requests, "DELETE "+r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			io.WriteString(w, `{"commit":{"id":"sha-branch-head"}}`)
		case strings.Contains(r.URL.Path, "/pulls/"):
			// pull request 33 is a factory pull request card's: the template
			// update branch on the projects fork, not the agent's factory/<id>
			if strings.HasSuffix(r.URL.Path, "/pulls/33") {
				io.WriteString(w, `{"head":{"ref":"factory/template-update","repo":{"full_name":"factory-projects/ai-software-factory-code"}}}`)
				return
			}
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
		case strings.HasSuffix(r.URL.Path, "/comments") && r.Method == http.MethodPost:
			var payload struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&payload) // #nosec G104 -- test stub
			requests = append(requests, "comment "+r.URL.Path+": "+payload.Body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/ai-software-factory-code/issues/") && strings.Count(r.URL.Path, "/") == 7:
			requests = append(requests, "close "+r.URL.Path)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"state":"closed"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/sf/ai-software-factory-code/issues/") && strings.Count(r.URL.Path, "/") == 7:
			io.WriteString(w, `{"state":"open"}`)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code/issues" && r.Method == http.MethodGet:
			if q := r.URL.Query(); q.Get("state") != "open" || q.Get("type") != "issues" || q.Has("labels") {
				t.Errorf("issue list query %q, want every open issue", r.URL.RawQuery)
			}
			servePage(w, r, 50, openIssues...)
		case strings.HasSuffix(r.URL.Path, "/timeline") && r.Method == http.MethodGet:
			io.WriteString(w, `[]`)
		case strings.Count(r.URL.Path, "/") == 4: // /api/v1/repos/{owner}/{repo}
			io.WriteString(w, `{"default_branch":"main"}`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"}, &requests
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
	started     []string
	signals     []string // "<task> <signal> <author> #<pr>: <body>"
	err         error    // returned by Signal when set
	onStart     func(taskID string)
	routineRuns []RoutineRunInput
}

func (w *fakeWorkflows) Start(ctx context.Context, taskID string) error {
	w.started = append(w.started, taskID)
	if w.onStart != nil {
		w.onStart(taskID)
	}
	return nil
}

func (w *fakeWorkflows) StartRoutineRun(ctx context.Context, in RoutineRunInput) error {
	w.routineRuns = append(w.routineRuns, in)
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

// testBudget is a policy budget whose default differs from the deployed one.
func testBudget() policy.Budget {
	return policy.Budget{TaskDefaultEUR: 1.5, TaskMaxEUR: 4, DayEUR: 20, MonthEUR: 50}
}

// testPolicy is intake's policy: testBudget, and alice the one approver
// (sebastian writes the code repository and approves nothing).
func testPolicy() policy.Tree {
	return policy.Tree{Document: policy.Document{Budget: testBudget(), Approvers: []string{"alice"}}}
}

func newTestIntake(t *testing.T) (*Intake, *fakeStore, *[]string) {
	t.Helper()
	forgejo, requests := intakeForgejoStub(t)
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, Policy: testPolicy}
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
			"number": number, "title": title, "body": body, "created_at": "2026-09-28T09:00:00+02:00",
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
	body := issueOpenedBody(7, "sebastian", []string{"agent", "priority/p1"}, "Add the runner", "objective body\n\n```repro\nmake runner-check\n```")
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
	if want := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC); !task.CreatedAt.Equal(want) {
		t.Fatalf("created = %s, want the issue's %s", task.CreatedAt, want)
	}
	if task.ReproCommand != "make runner-check" {
		t.Fatalf("repro command = %q, want the issue's repro block", task.ReproCommand)
	}
	if task.Objective != "Add the runner\n\nobjective body\n\n```repro\nmake runner-check\n```" {
		t.Fatalf("objective = %q", task.Objective)
	}
	if task.SHA != "sha-main-head" {
		t.Fatalf("sha = %q, want the resolved branch head", task.SHA)
	}
	if task.BudgetEUR != 1.5 {
		t.Fatalf("budget = %v, want the policy's 1.5 default", task.BudgetEUR)
	}
	if !slices.Equal(task.RequiredCapabilities, []string{"coder"}) {
		t.Fatalf("capabilities = %v, want [coder]", task.RequiredCapabilities)
	}
	// default_branch came from the payload itself; RepoDefaultBranch must not
	// have been called, only the branch-head lookup.
	for _, p := range *requests {
		if strings.HasSuffix(p, "/repos/sf/ai-software-factory-code") {
			t.Fatalf("RepoDefaultBranch was called even though the payload carried default_branch: %v", *requests)
		}
	}
	// a webhook-created agent task must carry state/queued
	// immediately, not only once a runner later claims it.
	if labels := issueLabels(t, in.Forgejo, 7); !slices.Contains(labels, "state/queued") {
		t.Fatalf("labels = %v, want state/queued", labels)
	}
	if want := "webhook intake: issue #7 on sf/ai-software-factory-code by sebastian: ready task " + task.ID; !strings.Contains(logs.String(), want) {
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
	if task.Kind != "human" || task.IssueNumber != 9 || task.Status != "waiting-human" || len(task.RequiredCapabilities) != 0 {
		t.Fatalf("task = %+v, want a human-lane task waiting on its human from the start", task)
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
	// the human lane gets its own consistent label
	// (architecture 5.6's "waiting on a human" board column), not
	// state/queued, since no runner will ever claim a human-lane task.
	if labels := issueLabels(t, in.Forgejo, 9); !slices.Contains(labels, "state/waiting-human") {
		t.Fatalf("labels = %v, want state/waiting-human", labels)
	}
	if len(store.intakeEvents) != 1 {
		t.Fatalf("intake events = %+v, want the ask's one", store.intakeEvents)
	}
	if e, q := store.intakeEvents[0], store.intakeEvents[0].Question; e.TaskID != task.ID || e.Kind != IntakeEscalation || q == nil ||
		q.Ask != "Needs a decision" || q.Why != "filed by sebastian into the human lane" || q.Info != "please look" ||
		!slices.Equal(q.Labels(), []string{"Have the factory do it", "Done"}) {
		t.Fatalf("intake event = %+v, want the issue asked as a question", e)
	}
}

// TestIntakeIssueOpenedLabelMirrorFailureIsLogOnly asserts a Forgejo hiccup
// while mirroring state/queued never fails the webhook nor loses the task
// row: mirroring is log-only, same as the runner's and completion's.
func TestIntakeIssueOpenedLabelMirrorFailureIsLogOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isWriterRead(r.URL.Path):
			serveWriters(t, w, r)
		case strings.HasSuffix(r.URL.Path, "/branches/main"):
			io.WriteString(w, `{"commit":{"id":"sha-main-head"}}`)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			http.Error(w, "forgejo unavailable", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	forgejo := &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"}
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, Policy: testPolicy}
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

// Each lane end to end through the webhook, from an outside account and a
// writer: the row's status, kind, author standing and whether its
// workflow starts.
func TestIntakeIssueLanes(t *testing.T) {
	for _, tc := range []struct {
		poster     string
		labels     []string
		status     string
		kind       string
		writer     bool
		started    bool
		stateLabel string
	}{
		{"outsider", nil, "queued", "agent", false, true, "state/queued"},
		{"outsider", []string{"bug"}, "queued", "agent", false, true, "state/queued"},
		{"outsider", []string{"improvement"}, "backlog", "agent", false, false, "state/backlog"},
		{"sebastian", []string{"question"}, "queued", "agent", true, true, "state/queued"},
		{"sebastian", []string{"improvement"}, "backlog", "agent", true, false, "state/backlog"},
		{"sebastian", []string{"human"}, "waiting-human", "human", true, false, "state/waiting-human"},
		// Forgejo applies any label an outsider's new-issue form posts:
		// the writers' lane labels do not count from them
		{"outsider", []string{"human"}, "queued", "agent", false, true, "state/queued"},
		{"outsider", []string{"improvement", "agent"}, "backlog", "agent", false, false, "state/backlog"},
	} {
		t.Run(fmt.Sprintf("%s %v", tc.poster, tc.labels), func(t *testing.T) {
			in, store, _ := newTestIntake(t)
			labels := tc.labels
			if tc.poster == "outsider" {
				labels = append(slices.Clone(labels), "priority/p0")
			}
			// Forgejo holds the labels the form posted
			if err := in.Forgejo.ReplaceIssueLabels(context.Background(), "sf", "ai-software-factory-code", 5, labels); err != nil {
				t.Fatal(err)
			}
			body := issueOpenedBody(5, tc.poster, labels, "It breaks", "body")
			if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
			}
			// a backlog card's second row is its write-up (ruling 185)
			if want := map[bool]int{true: 2, false: 1}[tc.status == "backlog"]; len(store.tasks) != want {
				t.Fatalf("tasks = %d, want %d", len(store.tasks), want)
			}
			task, _, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", 5)
			if task.Status != tc.status || task.Kind != tc.kind || task.Author != tc.poster || task.AuthorWriter != tc.writer || task.Priority != 0 {
				t.Fatalf("task = %+v, want status %s kind %s author %s writer %v", task, tc.status, tc.kind, tc.poster, tc.writer)
			}
			if started := slices.Contains(workflowsOf(in).started, task.ID); started != tc.started {
				t.Fatalf("started = %v, want %v", started, tc.started)
			}
			if tc.status == "backlog" && (task.SHA != "" || task.Gate != "") {
				t.Fatalf("backlog card = %+v, want no base and no gate before it starts", task)
			}
			labels = issueLabels(t, in.Forgejo, 5)
			if !slices.Contains(labels, tc.stateLabel) {
				t.Fatalf("labels = %v, want %s", labels, tc.stateLabel)
			}
			// an outside account's writer labels are off the issue (review M1)
			if tc.poster == "outsider" && slices.ContainsFunc(labels, func(l string) bool { return l == "agent" || l == "human" || l == "priority/p0" }) {
				t.Fatalf("labels = %v, want the outsider's agent, human and priority dropped", labels)
			}
		})
	}
}

// A backlog improvement starts on a writer's agent label, never on an
// outside account's, and the heal pass (no sender) leaves it be.
func TestIntakeBacklogCardStartsOnAWritersAgentLabel(t *testing.T) {
	in, store, _ := newTestIntake(t)
	opened := issueOpenedBody(12, "outsider", []string{"improvement"}, "A wish", "body")
	postWebhook(in, "issues", opened, sign(webhookSecret, opened))
	wish, _, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", 12)
	id := wish.ID
	presort, _, _ := store.ReportOf(context.Background(), id, SourcePresort)
	labelled := func(sender string) []byte {
		var p map[string]any
		json.Unmarshal(issueOpenedBody(12, "outsider", []string{"improvement", "agent"}, "A wish", "body"), &p)
		p["action"], p["sender"] = "label_updated", map[string]string{"login": sender}
		b, _ := json.Marshal(p)
		return b
	}
	// a writer's edit that is no label event starts nothing (review M1)
	edited := labelled("sebastian")
	var ep map[string]any
	json.Unmarshal(edited, &ep)
	ep["action"] = "edited"
	edited, _ = json.Marshal(ep)
	postWebhook(in, "issues", edited, sign(webhookSecret, edited))
	// the bot's own label events (its state mirror) start nothing (re-review m1)
	for _, sender := range []string{"outsider", "", "factory-agent"} {
		b := labelled(sender)
		if rr := postWebhook(in, "issue_label", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
	}
	if store.tasks[id].Status != "backlog" || !slices.Equal(workflowsOf(in).started, []string{presort.ID}) {
		t.Fatalf("task = %+v, started %q: an outside account's or no sender's label starts nothing", store.tasks[id], workflowsOf(in).started)
	}
	b := labelled("sebastian")
	if rr := postWebhook(in, "issue_label", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	got := store.tasks[id]
	if got.Status != "queued" || got.Lane != "ready" || got.SHA != "sha-main-head" || got.Gate != GateIntake || !slices.Equal(workflowsOf(in).started, []string{presort.ID, id}) {
		t.Fatalf("task = %+v, started %q: want queued at the branch head with its workflow started", got, workflowsOf(in).started)
	}
	if labels := issueLabels(t, in.Forgejo, 12); !slices.Contains(labels, "state/queued") {
		t.Fatalf("labels = %v, want state/queued", labels)
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
		"action":     "created",
		"issue":      map[string]any{"number": 12},
		"comment":    map[string]any{"id": 61, "user": map[string]string{"login": "sebastian"}, "body": "looks good"},
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
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", IssueNumber: 12, Status: "running", Kind: "agent"}
	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 12},
		"comment":    map[string]any{"id": 62, "user": map[string]string{"login": "factory-agent"}, "body": "posting status"},
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
	store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "running", Kind: "agent"}
	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 21},
		"comment":    map[string]any{"id": 63, "user": map[string]string{"login": "sebastian"}, "body": "one nit"},
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
	if ev := store.intakeEvents; len(ev) != 1 || ev[0].TaskID != "t-pr" || ev[0].Kind != "pull_request_comment" || ev[0].CommentID != 63 || !ev[0].Trusted || !ev[0].Signalled {
		t.Fatalf("intake events = %+v, want one trusted, signalled pull_request_comment 63 on t-pr", ev)
	}
}

func TestIntakePullRequestCommentOnNonFactoryBranchIgnored(t *testing.T) {
	in, store, _ := newTestIntake(t)
	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 99},
		"comment":    map[string]any{"id": 64, "user": map[string]string{"login": "sebastian"}, "body": "hi"},
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
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "running", Kind: "agent"}
	body := reviewBody("factory/t1", "sebastian", "pull_request_review_approved", "ship it")
	rr := postWebhook(in, "pull_request_review_approved", body, sign(webhookSecret, body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.intakeEvents) != 1 {
		t.Fatalf("intake events = %d, want 1", len(store.intakeEvents))
	}
	ev := store.intakeEvents[0]
	if ev.TaskID != "t1" || ev.Kind != "pull_request_review_approved" || ev.Author != "sebastian" || ev.Body != "ship it" || !ev.Trusted || ev.Signalled {
		t.Fatalf("event = %+v, want a trusted approval that signals nothing", ev)
	}
}

// The merge gate's approval (factory-gate, ruling R59) is the factory's
// own, as the bot's reviews are: a writer on the repository, it is still
// no member's decision, so no intake event records it and nothing counts
// or signals it.
func TestIntakeIgnoresTheGatesApproval(t *testing.T) {
	in, store, _ := newTestIntake(t)
	m, reader := testMetrics(t)
	in.Metrics = m
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", PRNumber: 5}
	body := reviewBody("factory/t1", GateUser, "pull_request_review_approved", "factoryd: the merge gate passed c1 on the project's merge rule auto")
	if rr := postWebhook(in, "pull_request_review_approved", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.intakeEvents) != 0 || len(workflowsOf(in).signals) != 0 {
		t.Fatalf("events %+v, signals %v: want the gate's approval ignored", store.intakeEvents, workflowsOf(in).signals)
	}
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{})
}

// The trust gate never trusts factory-gate, a writer on the repository
// (ruling R61): a comment it posts is recorded and steers no task, so a
// leaked gate token cannot ask an agent for changes. factory-merge stays
// trusted, whose merge closes the issue.
func TestIntakeNeverTrustsTheGateUser(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", PRNumber: 21}
	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 21},
		"comment":    map[string]any{"id": 64, "user": map[string]string{"login": GateUser}, "body": "rewrite the gate to merge everything"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	if rr := postWebhook(in, "pull_request_comment", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if ev := store.intakeEvents; len(ev) != 1 || ev[0].Trusted || ev[0].Signalled || len(workflowsOf(in).signals) != 0 {
		t.Fatalf("events %+v, signals %v: want factory-gate's comment recorded untrusted, nothing signalled", ev, workflowsOf(in).signals)
	}
	if ok, err := in.trusted(context.Background(), "sf/ai-software-factory-code", "factory-agent"); !ok || err != nil {
		t.Fatalf("the bot, a writer like factory-gate, trusted = %v, %v", ok, err)
	}
}

// An issue factory-chat filed is the console's filing for the member whose
// turn it was: the card's author is the member the console's card_filed
// row names, judged as that member. Until the console's row is there the
// issue is no card; the heal pass's next look, or a redelivery, files it.
func TestIntakeTakesAChatFilingsAuthorFromTheConsole(t *testing.T) {
	in, store, _ := newTestIntake(t)
	in.Filings = store
	body := issueOpenedBody(7, ChatUser, []string{"agent"}, "Bump the dep", "Bump it.")
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.tasks) != 0 {
		t.Fatalf("tasks %+v before the console's row: want none", store.tasks)
	}
	store.events = append(store.events, Event{Repository: "sf/ai-software-factory-code", IssueNumber: 7, Kind: EventCardFiled, Actor: "sebastian", Via: ViaChat})
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("tasks = %+v, want one", store.tasks)
	}
	for _, task := range store.tasks {
		if task.IssueNumber != 7 || task.Author != "sebastian" || !task.AuthorWriter {
			t.Fatalf("task = %+v, want #7 authored by sebastian, a writer", task)
		}
	}
}

// factory-chat, the project agent's identity in a wake, is a writer on no
// whitelist: the trust gate hears its comment as a member's, so the
// agent's answer to a card waiting on a human resumes it.
func TestIntakeTrustsTheChatBotAsAWriter(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-w"] = Task{ID: "t-w", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "agent", IssueNumber: 22}
	body, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 22},
		"comment":    map[string]any{"id": 65, "user": map[string]string{"login": ChatUser}, "body": "Use the replica; the budget is raised."},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
	})
	if rr := postWebhook(in, "issue_comment", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	want := []string{"t-w human_comment factory-chat #0 c65: Use the replica; the budget is raised."}
	if ev := store.intakeEvents; len(ev) != 1 || !ev[0].Trusted || !slices.Equal(workflowsOf(in).signals, want) {
		t.Fatalf("events %+v, signals %q: want factory-chat's comment trusted and signalled as %q", ev, workflowsOf(in).signals, want)
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

// factoryPRReviewBody is a review on a factory pull request card's pull
// request number: its head is the template update branch on the projects
// fork, not the agent's factory/<task id> (roadmap 275).
func factoryPRReviewBody(number int64, sender, content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{"number": number, "head": prHead("factory/template-update", "factory-projects/ai-software-factory-code")},
		"repository":   map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":       map[string]string{"login": sender},
		"review":       map[string]string{"content": content, "type": "pull_request_review_rejected"},
	})
	return b
}

func factoryPRCommentBody(number int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": number},
		"comment":    map[string]any{"id": 71, "user": map[string]string{"login": "sebastian"}, "body": "one nit"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	return b
}

// A review or a comment written in Forgejo on a factory pull request
// card's pull request reaches that card as it reaches an agent card's
// (roadmap 275): the card records the pull request's number, which
// resolves it, for its head is not the agent's factory/<task id>.
func TestIntakeReviewOnFactoryPRCardReachesTheCard(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-tmpl"] = Task{ID: "t-tmpl", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", Source: SourceTemplateUpdate, PRNumber: 33}
	body := factoryPRReviewBody(33, "sebastian", "redo the workflow file")
	if rr := postWebhook(in, "pull_request_review_rejected", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if got := workflowsOf(in).signals; len(got) != 1 || got[0] != "t-tmpl changes_requested sebastian #33: redo the workflow file" {
		t.Fatalf("signals = %q, want the card's workflow asked for changes", got)
	}
	if ev := store.intakeEvents; len(ev) != 1 || ev[0].TaskID != "t-tmpl" || ev[0].Kind != "pull_request_review_rejected" || !ev[0].Trusted || !ev[0].Signalled {
		t.Fatalf("intake events = %+v, want one trusted, signalled rejection on t-tmpl", ev)
	}
}

func TestIntakeCommentOnFactoryPRCardReachesTheCard(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-tmpl"] = Task{ID: "t-tmpl", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", Source: SourceTemplateUpdate, PRNumber: 33}
	body := factoryPRCommentBody(33)
	if rr := postWebhook(in, "pull_request_comment", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if ev := store.intakeEvents; len(ev) != 1 || ev[0].TaskID != "t-tmpl" || ev[0].Kind != "pull_request_comment" || ev[0].CommentID != 71 || !ev[0].Trusted || !ev[0].Signalled {
		t.Fatalf("intake events = %+v, want one trusted, signalled pull_request_comment 71 on t-tmpl", ev)
	}
}

// A pull request whose number matches no card, or only a card already
// ended, is dropped as before: the number resolves an open factory pull
// request card or nothing.
func TestIntakeOnFactoryPRWithoutOpenCardDropped(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-done"] = Task{ID: "t-done", Repository: "sf/ai-software-factory-code", Status: "done", Kind: "agent", Source: SourceTemplateUpdate, PRNumber: 34}
	review := factoryPRReviewBody(34, "sebastian", "changes")
	if rr := postWebhook(in, "pull_request_review_rejected", review, sign(webhookSecret, review)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	comment := factoryPRCommentBody(35)
	if rr := postWebhook(in, "pull_request_comment", comment, sign(webhookSecret, comment)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	if len(store.intakeEvents) != 0 || len(workflowsOf(in).signals) != 0 {
		t.Fatalf("events %+v, signals %q: want a pull request with no open card dropped", store.intakeEvents, workflowsOf(in).signals)
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

// A push to master of a project's document repository raises one
// doc_written row per changed path under the document roots, via
// webhook, for every project naming the repository as its chat's, the
// commit that changed the path its actor; a commit carrying the door's
// attribution trailers raised its event at the door and none here, and a
// path outside the roots raises none (the 2026-10-15 plan, item 6).
func TestPushRaisesDocWritten(t *testing.T) {
	in, store, _ := newTestIntake(t)
	in.Timeline = &Timeline{Events: store}
	in.Policy = func() policy.Tree {
		return policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: "sf/code", Chat: &policy.ChatConfig{DocRepo: "sf/docs"}},
			"demo": {Repository: "sf/demo", Chat: &policy.ChatConfig{DocRepo: "sf/demo-docs"}},
		}}
	}
	body, _ := json.Marshal(map[string]any{
		"ref":        "refs/heads/master",
		"repository": map[string]any{"full_name": "sf/docs"},
		"commits": []map[string]any{
			{"id": "d00d1", "message": "docs: the roadmap's fifth round",
				"author":   map[string]string{"name": "Sebastian", "username": "sebastian"},
				"added":    []string{"docs/board/night.md"},
				"modified": []string{"docs/roadmap.md", "README.md"}},
			{"id": "d00d2", "message": "docs/roadmap.md: edited in the console\n\nFactory-Role: console\nRequested-by: sebastian\n",
				"author":   map[string]string{"name": "sebastian", "username": "sebastian"},
				"modified": []string{"docs/roadmap.md"}},
		},
	})
	if rr := postWebhook(in, "push", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	rows := store.events
	if len(rows) != 2 {
		t.Fatalf("doc_written rows = %+v, want two", rows)
	}
	for i, want := range []Event{
		{Repository: "sf/code", Kind: EventDocWritten, Actor: "sebastian", Via: ViaWebhook,
			Detail: map[string]any{"path": "docs/board/night.md", "commit": "d00d1"}},
		{Repository: "sf/code", Kind: EventDocWritten, Actor: "sebastian", Via: ViaWebhook,
			Detail: map[string]any{"path": "docs/roadmap.md", "commit": "d00d1"}},
	} {
		got := rows[i]
		if got.Repository != want.Repository || got.Kind != want.Kind || got.Actor != want.Actor || got.Via != want.Via ||
			got.Detail["path"] != want.Detail["path"] || got.Detail["commit"] != want.Detail["commit"] {
			t.Errorf("row %d = %+v, want %+v", i, got, want)
		}
	}
	// a push to master of a repository no project names as its own
	// raises nothing
	other, _ := json.Marshal(map[string]any{
		"ref":        "refs/heads/master",
		"repository": map[string]any{"full_name": "sf/other-docs"},
		"commits":    []map[string]any{{"id": "x1", "message": "docs", "added": []string{"docs/other.md"}}},
	})
	if rr := postWebhook(in, "push", other, sign(webhookSecret, other)); rr.Code != http.StatusOK {
		t.Fatalf("the other push's status = %d", rr.Code)
	}
	if len(store.events) != 2 {
		t.Fatalf("a push to another repository's master raised %d more rows", len(store.events)-2)
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
	store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent"}
	store.tasks["t-issue"] = Task{ID: "t-issue", Repository: "sf/ai-software-factory-code", Status: "in-review", IssueNumber: 30, Kind: "agent"}
	store.tasks["t-human"] = Task{ID: "t-human", Repository: "sf/ai-software-factory-code", Status: "waiting-human", IssueNumber: 31, Kind: "human"}
	pr, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 21},
		"comment":    map[string]any{"id": 70, "user": map[string]string{"login": "sebastian"}, "body": "one nit"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	prEdited, _ := json.Marshal(map[string]any{
		"action":     "edited",
		"issue":      map[string]any{"number": 21},
		"comment":    map[string]any{"id": 70, "user": map[string]string{"login": "sebastian"}, "body": "one nit, edited"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"is_pull":    true,
	})
	human, _ := json.Marshal(map[string]any{
		"action":     "created",
		"issue":      map[string]any{"number": 31},
		"comment":    map[string]any{"id": 78, "user": map[string]string{"login": "sebastian"}, "body": "on the human lane"},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
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
	postWebhook(in, "pull_request_comment", prEdited, sign(webhookSecret, prEdited))
	postWebhook(in, "issue_comment", issue, sign(webhookSecret, issue))
	postWebhook(in, "issue_comment", edited, sign(webhookSecret, edited))
	postWebhook(in, "issue_comment", human, sign(webhookSecret, human))
	want := []string{"t-pr changes_requested sebastian #21: one nit", "t-issue human_comment sebastian #0 c77: a note on the issue"}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want %q: a new PR comment requests changes, a new issue comment is a human comment, edits and the human lane signal nothing", got, want)
	}
	if ev := store.intakeEvents; len(ev) != 2 || ev[0].CommentID != 70 || ev[1].CommentID != 77 || !ev[0].Signalled || !ev[1].Signalled {
		t.Fatalf("intake events = %+v, want the two created comments, both signalled", ev)
	}
}

// resyncForgejoStub is Forgejo as the heal pass re-reads it for task t1:
// pull request 5 merged, issue 30 closed by closer (per its timeline),
// comments on both from the bot, the owner and a stranger.
func resyncForgejoStub(t *testing.T, closer string) (*Forgejo, *[]string) {
	t.Helper()
	var requests []string
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			io.WriteString(w, `[]`)
		case isWriterRead(r.URL.Path):
			serveWriters(t, w, r)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code/pulls/5":
			io.WriteString(w, `{"number":5,"state":"closed","merged":true,"head":{"ref":"factory/t1","sha":"c7","repo":{"full_name":"factory-agent/ai-software-factory-code"}}}`)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code/issues/30":
			io.WriteString(w, `{"state":"closed"}`)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code/issues/30/timeline":
			// reopened once by the owner, then closed by closer: the last close counts
			io.WriteString(w, `[{"type":"comment","user":{"login":"sebastian"}},{"type":"close","user":{"login":"sebastian"}},{"type":"reopen","user":{"login":"sebastian"}},{"type":"close","user":{"login":"`+closer+`"}}]`)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code/issues/30/comments":
			io.WriteString(w, `[{"id":79,"user":{"login":"factory-agent"},"body":"escalation","created_at":"2026-09-20T10:00:00Z"},{"id":80,"user":{"login":"sebastian"},"body":"answer: do X","created_at":"2026-09-20T10:01:00Z"},{"id":81,"user":{"login":"reader"},"body":"drive-by","created_at":"2026-09-20T10:02:00Z"}]`)
		case r.URL.Path == "/api/v1/repos/sf/ai-software-factory-code/issues/5/comments":
			io.WriteString(w, `[{"id":90,"user":{"login":"sebastian"},"body":"please rename","created_at":"2026-09-20T10:05:00Z"}]`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"}, &requests
}

// isWriterRead is one of RepoWriters' reads.
func isWriterRead(path string) bool {
	return strings.HasSuffix(path, "/assignees") || strings.HasSuffix(path, "/collaborators") ||
		strings.HasPrefix(path, "/api/v1/orgs/") || strings.HasPrefix(path, "/api/v1/teams/")
}

// serveWriters answers RepoWriters' reads for a code repository whose
// Owners team holds sebastian and whose bot, merge gate user and chat user
// are write collaborators, each read on its token.
func serveWriters(t *testing.T, w http.ResponseWriter, r *http.Request) {
	want := "token registrar-tok"
	if strings.HasPrefix(r.URL.Path, "/api/v1/orgs/") || strings.HasPrefix(r.URL.Path, "/api/v1/teams/") {
		want = "token org-read-tok"
	}
	if got := r.Header.Get("Authorization"); got != want {
		t.Errorf("%s read with %q, want %q", r.URL.Path, got, want)
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/orgs/"):
		servePage(w, r, 50, `{"id":1,"permission":"owner","includes_all_repositories":true,"units_map":{"repo.code":"owner"}}`)
	case r.URL.Path == "/api/v1/teams/1/members":
		servePage(w, r, 50, `{"login":"sebastian"}`)
	case strings.HasSuffix(r.URL.Path, "/collaborators"):
		servePage(w, r, 50, `{"login":"factory-agent"}`, `{"login":"factory-gate"}`, `{"login":"factory-chat"}`)
	case strings.HasSuffix(r.URL.Path, "/assignees"):
		io.WriteString(w, `[{"login":"sebastian"},{"login":"factory-agent"},{"login":"factory-gate"},{"login":"factory-chat"}]`)
	default:
		http.NotFound(w, r)
	}
}

func writerReads(requests []string) int {
	n := 0
	for _, p := range requests {
		if strings.HasSuffix(p, "/assignees") {
			n++
		}
	}
	return n
}

// A webhook Forgejo delivered while factoryd was down is gone; the heal
// pass re-reads the task's pull request, issue and comments. A terminal
// state and a comment alike are recorded and signalled once, whichever of
// the webhook or a re-read saw it first, and a trusted event recorded but
// not yet signalled is signalled now.
func TestIntakeResyncSignalsWhatTheWebhooksMissed(t *testing.T) {
	forgejo, requests := resyncForgejoStub(t, "sebastian")
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
	task := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 30, PRNumber: 5}
	store.tasks["t1"] = task
	// the webhook recorded comment 80 but factoryd died before signalling it
	store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "answer: do X", CommentID: 80, Trusted: true})

	if err := in.Resync(context.Background(), task); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	want := []string{
		"t1 pr_merged factoryd heal pass #5: ",
		"t1 changes_requested sebastian #5: please rename",
		"t1 issue_closed sebastian #0: ",
		"t1 human_comment sebastian #0 c80: answer: do X",
	}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want %q", got, want)
	}
	var recorded []string
	for _, e := range store.intakeEvents {
		recorded = append(recorded, fmt.Sprintf("%s %d %s trusted=%v signalled=%v", e.Kind, e.CommentID, e.Author, e.Trusted, e.Signalled))
	}
	wantRecorded := []string{
		"issue_comment 80 sebastian trusted=true signalled=true",
		"pull_request_merged 5 factoryd heal pass trusted=true signalled=true",
		"pull_request_comment 90 sebastian trusted=true signalled=true",
		"issue_closed 30 sebastian trusted=true signalled=true",
		"issue_comment 81 reader trusted=false signalled=false",
	}
	if !slices.Equal(recorded, wantRecorded) {
		t.Fatalf("intake events = %q, want %q", recorded, wantRecorded)
	}
	if n := writerReads(*requests); n != 2 {
		t.Fatalf("permission reads = %d, want one per new author (sebastian, reader)", n)
	}

	// the next pass, while the workflow still winds the task down: nothing
	// twice, no permission read for an author already granted
	*requests = nil
	if err := in.Resync(context.Background(), task); err != nil {
		t.Fatalf("second Resync: %v", err)
	}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) {
		t.Fatalf("signals after the second pass = %q, want %q", got, want)
	}
	if len(store.intakeEvents) != 5 || writerReads(*requests) != 0 {
		t.Fatalf("second pass: %d events, %d permission reads; want 5 and 0", len(store.intakeEvents), writerReads(*requests))
	}
}

// A decision the webhook delivered is not sent again by the heal pass that
// reads the same closed pull request or issue off Forgejo: the store row
// claims it for whichever path came first.
func TestIntakeSignalsADecisionOnce(t *testing.T) {
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
	task := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 30, PRNumber: 5}
	store.tasks["t1"] = task
	closed, _ := json.Marshal(map[string]any{
		"action":     "closed",
		"issue":      map[string]any{"number": 30, "user": map[string]string{"login": "factory-agent"}},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":     map[string]string{"login": "sebastian"},
	})
	for _, d := range []struct {
		event string
		body  []byte
	}{{"pull_request", prClosedBody("factory/t1", true)}, {"issues", closed}} {
		if rr := postWebhook(in, d.event, d.body, sign(webhookSecret, d.body)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", d.event, rr.Code)
		}
	}
	if err := in.Resync(context.Background(), task); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	var got []string
	for _, sig := range workflowsOf(in).signals {
		if strings.Contains(sig, " pr_merged ") || strings.Contains(sig, " issue_closed ") {
			got = append(got, sig)
		}
	}
	if want := []string{"t1 pr_merged sebastian #5: ", "t1 issue_closed sebastian #0: "}; !slices.Equal(got, want) {
		t.Fatalf("decision signals = %q, want %q", got, want)
	}
}

// A decision claimed but never signalled (factoryd died between the two)
// would strand the task: the heal pass signals a claimed decision again
// while the task still waits in review or on a human, once the decision
// is older than a workflow takes to wind a task down; a fresh one is
// left to that wind-down.
func TestIntakeResyncResignalsAStrandedDecision(t *testing.T) {
	for name, tc := range map[string]struct {
		age  time.Duration
		want []string
	}{
		"stranded": {decisionResignalAfter + time.Minute, []string{"t1 pr_merged sebastian #5: ", "t1 issue_closed sebastian #0: "}},
		"fresh":    {time.Minute, nil},
	} {
		forgejo, _ := resyncForgejoStub(t, "sebastian")
		store := newFakeStore()
		in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
		task := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 30, PRNumber: 5}
		store.tasks["t1"] = task
		at := time.Now().Add(-tc.age)
		store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: "pull_request_merged", Author: "sebastian", CommentID: 5, Trusted: true, Signalled: true, CreatedAt: at})
		store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: "issue_closed", Author: "sebastian", CommentID: 30, Trusted: true, Signalled: true, CreatedAt: at})
		if err := in.Resync(context.Background(), task); err != nil {
			t.Fatalf("%s: Resync: %v", name, err)
		}
		var got []string
		for _, sig := range workflowsOf(in).signals {
			if strings.Contains(sig, " pr_merged ") || strings.Contains(sig, " issue_closed ") {
				got = append(got, sig)
			}
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("%s: decision signals = %q, want %q", name, got, tc.want)
		}
	}
}

// A pull request decision names its own pull request: one on another PR
// of the task branch, which the workflow ignored, is never sent again as
// a decision on the task's PR, and one not yet signalled goes out with
// its own number.
func TestIntakeResyncKeepsADecisionOnItsOwnPR(t *testing.T) {
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
	task := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", PRNumber: 5}
	store.tasks["t1"] = task
	at := time.Now().Add(-decisionResignalAfter - time.Minute)
	store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: "pull_request_closed", Author: "sebastian", CommentID: 4, Trusted: true, Signalled: true, CreatedAt: at})
	store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: "pull_request_closed", Author: "sebastian", CommentID: 3, Trusted: true, CreatedAt: at})
	if err := in.Resync(context.Background(), task); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	var got []string
	for _, sig := range workflowsOf(in).signals {
		if strings.Contains(sig, " pr_closed ") {
			got = append(got, sig)
		}
	}
	if want := []string{"t1 pr_closed sebastian #3: "}; !slices.Equal(got, want) {
		t.Fatalf("pr_closed signals = %q, want %q: #3 once on its own number, #4 not again", got, want)
	}
}

// The heal pass reads who closed the issue off its timeline and applies
// the webhook path's gate: the bot and a user with write access close a
// task (the agent lane is signalled, the human lane is done), a stranger's
// close is not acted on.
func TestIntakeResyncTrustsTheIssueCloser(t *testing.T) {
	for closer, want := range map[string]bool{"reader": false, "sebastian": true, "factory-agent": true} {
		forgejo, _ := resyncForgejoStub(t, closer)
		store := newFakeStore()
		in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
		agent := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 30}
		human := Task{ID: "t-human", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "human", IssueNumber: 30}
		store.tasks["t1"], store.tasks["t-human"] = agent, human
		// factoryd's own closes of both issues (m8)
		store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: agent.ID, Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: agent.IssueNumber})
		store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: human.ID, Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: human.IssueNumber})
		for _, task := range []Task{agent, human} {
			if err := in.Resync(context.Background(), task); err != nil {
				t.Fatalf("%s: Resync %s: %v", closer, task.ID, err)
			}
		}
		closedSignals := 0
		for _, sig := range workflowsOf(in).signals {
			if strings.HasPrefix(sig, "t1 issue_closed "+closer+" ") {
				closedSignals++
			}
		}
		if (closedSignals == 1) != want || (store.tasks["t-human"].Status == "done") != want {
			t.Fatalf("closed by %s: %d issue_closed signals, human task %s, want acted on = %v", closer, closedSignals, store.tasks["t-human"].Status, want)
		}
	}
}

// A closed issue whose timeline names no closer is not acted on, but the
// pass still re-reads and signals its comments; the lookup error is
// returned after them.
func TestIntakeResyncReadsCommentsPastAnUnknownCloser(t *testing.T) {
	forgejo, _ := resyncForgejoStub(t, "")
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
	task := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "agent", IssueNumber: 30}
	store.tasks["t1"] = task
	if err := in.Resync(context.Background(), task); !errors.Is(err, errNoCloser) {
		t.Fatalf("Resync = %v, want errNoCloser", err)
	}
	if got, want := workflowsOf(in).signals, []string{"t1 human_comment sebastian #0 c80: answer: do X"}; !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want %q", got, want)
	}
}

// Forgejo never retries a failed delivery, so an agent or human issue
// filed while factoryd was down would never become a task: the heal pass
// lists the open issues so labelled on the intake's repositories, every
// page, and runs each through the webhook's own intake. An unlinked one
// becomes a task; a linked one and a bot-authored one do not, on this pass
// or the next.
func TestIntakeResyncIssuesTakesInWhatTheWebhooksMissed(t *testing.T) {
	in, store, _ := newTestIntake(t)
	in.Projects = codeRepoOnly{}
	store.tasks["t-10"] = Task{ID: "t-10", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 10}
	for range 2 {
		if err := in.ResyncIssues(context.Background()); err != nil {
			t.Fatalf("ResyncIssues: %v", err)
		}
	}
	byIssue := map[int64]Task{}
	for _, tk := range store.tasks {
		byIssue[tk.IssueNumber] = tk
	}
	agent, human, bug, wish := byIssue[7], byIssue[9], byIssue[11], byIssue[12]
	presort, _, _ := store.ReportOf(context.Background(), wish.ID, SourcePresort)
	if len(store.tasks) != 6 || presort.ID == "" || agent.Kind != "agent" || agent.Status != "queued" || agent.Priority != 2 || agent.SHA != "sha-main-head" ||
		agent.Objective != "Add the runner\n\nobjective body" || human.Kind != "human" || human.Status != "waiting-human" ||
		bug.Status != "queued" || bug.Author != "outsider" || bug.AuthorWriter || wish.Status != "backlog" {
		t.Fatalf("tasks = %+v, want #10's plus agent tasks for #7 and the unlabelled #11, a human one for #9, #12 in the backlog with its presort", store.tasks)
	}
	// the second pass starts the still-queued workflows again, as the
	// webhook's redelivery would (Temporal refuses a running one); #12's
	// presort starts once, when it opens (the heal pass restarts it)
	if got := workflowsOf(in).started; !slices.Equal(got, []string{agent.ID, bug.ID, presort.ID, agent.ID, bug.ID}) {
		t.Fatalf("started = %q, want #7's and #11's workflows once a pass, #12's presort once", got)
	}
}

// Comment events recorded before comment ids were (migration 0010) carry
// id 0 and match no re-read comment: the heal pass would record and signal
// those comments a second time. A comment no newer than the task's latest
// such event is taken as covered.
func TestIntakeResyncSkipsCommentsOlderThanAnIdlessEvent(t *testing.T) {
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
	task := Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 30, PRNumber: 5}
	store.tasks["t1"] = task
	store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "answer: do X", Trusted: true, Signalled: true,
		CreatedAt: time.Date(2026, 9, 20, 10, 1, 30, 0, time.UTC)})
	if err := in.Resync(context.Background(), task); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	var recorded []string
	for _, e := range store.intakeEvents {
		if strings.HasSuffix(e.Kind, "_comment") {
			recorded = append(recorded, fmt.Sprintf("%s %d %s", e.Kind, e.CommentID, e.Author))
		}
	}
	want := []string{"issue_comment 0 sebastian", "pull_request_comment 90 sebastian", "issue_comment 81 reader"}
	if !slices.Equal(recorded, want) {
		t.Fatalf("intake events = %q, want %q: comment 80 predates the idless event", recorded, want)
	}
	if got := workflowsOf(in).signals; slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "c80") }) {
		t.Fatalf("signals = %q, want comment 80 not signalled again", got)
	}
}

// The webhook path and the heal pass can both hold the same unsignalled
// event; whoever claims it first signals it, and a claim whose signal
// failed is released for the next try.
func TestIntakeSignalsAClaimedEventOnce(t *testing.T) {
	in, store, _ := newTestIntake(t)
	ctx := context.Background()
	id, _ := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "do X", CommentID: 80, Trusted: true})
	e := IntakeEvent{ID: id, TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "do X", CommentID: 80, Trusted: true}
	workflowsOf(in).err = errors.New("temporal down")
	if err := in.signalEvent(ctx, "t1", e, 0); err == nil || store.intakeEvents[0].Signalled {
		t.Fatalf("failed signal: err = %v, signalled = %v; want the error and the claim released", err, store.intakeEvents[0].Signalled)
	}
	workflowsOf(in).err = nil
	for i := 0; i < 2; i++ {
		if err := in.signalEvent(ctx, "t1", e, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := workflowsOf(in).signals; len(got) != 1 || !store.intakeEvents[0].Signalled {
		t.Fatalf("signals = %q, signalled = %v; want one signal, claimed", got, store.intakeEvents[0].Signalled)
	}
	// claimed by the other path between the read and the claim
	store.intakeEvents[0].Signalled = false
	if _, err := store.SetSignalled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := in.signalEvent(ctx, "t1", e, 0); err != nil || len(workflowsOf(in).signals) != 1 {
		t.Fatalf("claimed elsewhere: err = %v, signals = %q; want none added", err, workflowsOf(in).signals)
	}
}

func TestIntakeReviewRejectedSignalsApprovedDoesNot(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent"}
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
		"pull_request": map[string]any{"number": 5, "merged": merged, "merge_commit_sha": map[bool]string{true: "m-5", false: ""}[merged], "head": prHead(ref, headRepo)},
		"repository":   map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":       map[string]string{"login": sender},
	})
	return b
}

func TestIntakeMergedPullRequestSignalsWorkflow(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent"}
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
	// a human's merge records the merge commit the webhook carries, so the
	// card's lead time reads like a queue merge's
	if got := store.tasks["t1"].MergeCommit; got != "m-5" {
		t.Fatalf("merge commit = %q, want m-5 from the merged webhook", got)
	}
}

// prCardRig is an intake on human card t-pr of a factory pull request on
// a project without agents (ruling R62): linked to pull request #5, its
// issue #9 on the code repository. Its Forgejo answers the pull request
// with pr ("" fails the read), the issue's state with closedBy and the
// pull request's as an issue with prClosedBy ("" open), fails the issue's
// close closeFails times and a comment's post commentFails times first,
// and counts the closes and the comments posted.
type prCardRig struct {
	t                        *testing.T
	in                       *Intake
	store                    *fakeStore
	rows                     *fakeEventRecorder
	mu                       sync.Mutex
	pr, closedBy, prClosedBy string
	closeFails, commentFails int
	closes, comments         int
	posted                   []string
}

func newPRCardRig(t *testing.T, pr string) *prCardRig {
	t.Helper()
	g := &prCardRig{t: t, pr: pr, store: newFakeStore(), rows: &fakeEventRecorder{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		const issue = "/api/v1/repos/sf/ai-software-factory-code/issues/9"
		switch p := r.URL.Path; {
		case isWriterRead(p):
			serveWriters(t, w, r)
		case r.Method == http.MethodGet && p == "/api/v1/repos/sf/ai-software-factory-code/pulls/5":
			if g.pr == "" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			io.WriteString(w, g.pr)
		case r.Method == http.MethodGet && p == issue:
			fmt.Fprintf(w, `{"number":9,"state":%q}`, map[bool]string{true: "open", false: "closed"}[g.closedBy == ""])
		case r.Method == http.MethodGet && p == issue+"/timeline":
			servePage(w, r, 50, fmt.Sprintf(`{"type":"close","user":{"login":%q}}`, g.closedBy))
		case r.Method == http.MethodGet && p == "/api/v1/repos/sf/ai-software-factory-code/issues/5":
			fmt.Fprintf(w, `{"number":5,"state":%q}`, map[bool]string{true: "open", false: "closed"}[g.prClosedBy == ""])
		case r.Method == http.MethodGet && p == "/api/v1/repos/sf/ai-software-factory-code/issues/5/timeline":
			servePage(w, r, 50, fmt.Sprintf(`{"type":"close","user":{"login":%q}}`, g.prClosedBy))
		case r.Method == http.MethodGet && p == issue+"/comments":
			out := []map[string]any{}
			for i, b := range g.posted {
				out = append(out, map[string]any{"id": i + 1, "body": b, "user": map[string]string{"login": "factory-agent"}})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPatch && p == issue:
			if g.closeFails > 0 {
				g.closeFails--
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			g.closes++
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && p == issue+"/comments":
			if g.commentFails > 0 {
				g.commentFails--
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			var c struct{ Body string }
			json.NewDecoder(r.Body).Decode(&c)
			g.comments++
			g.posted = append(g.posted, c.Body)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case strings.HasSuffix(p, "/labels"):
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, p)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	g.in = &Intake{Store: g.store, Forgejo: &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"},
		Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, Policy: testPolicy,
		Timeline: &Timeline{Objects: &fakeObjectStore{}, Store: g.store, Events: g.rows}}
	g.store.tasks["t-pr"] = Task{ID: "t-pr", Repository: "sf/ai-software-factory-code", Kind: "human", Status: "waiting-human",
		Source: SourceSchemaCopy, PRNumber: 5, IssueNumber: 9}
	return g
}

// post delivers webhook event with body and wants it answered status.
func (g *prCardRig) post(event string, body []byte, status int) {
	g.t.Helper()
	if rr := postWebhook(g.in, event, body, sign(webhookSecret, body)); rr.Code != status {
		g.t.Fatalf("%s: status %d, want %d: %s", event, rr.Code, status, rr.Body)
	}
}

// ended checks the card ended final once with reason, settled by one row
// of kind by actor via via, its issue closed with one comment, and no
// workflow signalled.
func (g *prCardRig) ended(final, reason, kind, actor, via string) {
	g.t.Helper()
	if got := g.store.tasks["t-pr"].Status; got != final {
		g.t.Fatalf("status %q, want %q", got, final)
	}
	var ended, settled []Event
	for _, e := range g.rows.rows {
		switch e.Kind {
		case EventTaskEnded:
			ended = append(ended, e)
		case EventQuestionAnswered, EventQuestionWithdrawn:
			settled = append(settled, e)
		}
	}
	if len(ended) != 1 || ended[0].Detail["state"] != final || ended[0].Detail["reason"] != reason {
		g.t.Fatalf("task_ended %+v, want one %s naming %q", ended, final, reason)
	}
	if len(settled) != 1 || settled[0].Kind != kind || settled[0].Actor != actor || settled[0].Via != via {
		g.t.Fatalf("settling rows %+v, want one %s by %s via %s", settled, kind, actor, via)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closes == 0 || g.comments != 1 || len(workflowsOf(g.in).signals) != 0 {
		g.t.Fatalf("issue closes %d, comments %d, signals %v; want #9 closed with one comment and nothing signalled", g.closes, g.comments, workflowsOf(g.in).signals)
	}
}

// A human card linked to a pull request ends with its pull request
// (ruling R63). A merge is its answer: the card ends done, naming the merge
// and the merger. A close without a merge ends it closed. Either way the
// person's answer settles the ask, one task_ended names why, and factoryd
// closes the card's issue as its own close. A redelivery changes nothing.
// Another pull request of the card's head ends nothing.
func TestAPullRequestEndsItsHumanCard(t *testing.T) {
	for _, tc := range []struct {
		merged        bool
		final, reason string
	}{
		{true, "done", "pull request #5 merged by sebastian"},
		{false, "closed", "pull request #5 closed without a merge by sebastian"},
	} {
		g := newPRCardRig(t, "")
		body := prClosedBodyFrom("factory/schema-0123456789ab", "factory-agent/ai-software-factory-code", tc.merged)
		g.post("pull_request", body, http.StatusOK)
		g.post("pull_request", body, http.StatusOK)
		g.ended(tc.final, tc.reason, EventQuestionAnswered, "sebastian", ViaWebhook)
		if a := g.rows.rows[slices.IndexFunc(g.rows.rows, func(e Event) bool { return e.Kind == EventQuestionAnswered })]; a.Detail["answer"] != tc.reason {
			t.Fatalf("answer %+v, want %q", a.Detail, tc.reason)
		}
	}
	g := newPRCardRig(t, "")
	card := g.store.tasks["t-pr"]
	card.PRNumber = 7
	g.store.tasks["t-pr"] = card
	g.post("pull_request", prClosedBodyFrom("factory/t-pr", "factory-agent/ai-software-factory-code", true), http.StatusOK)
	if got := g.store.tasks["t-pr"].Status; got != "waiting-human" || len(g.rows.rows) != 0 || g.closes != 0 {
		t.Fatalf("status %q, rows %+v, closes %d: pull request #5 is not the card's #7, want nothing", got, g.rows.rows, g.closes)
	}
}

// factoryd closes the card's issue before the card ends: a close Forgejo
// fails leaves the card waiting with nothing recorded, so the heal pass
// ends it later with one answer row and one comment. The bot's recorded
// close of the issue, whose webhook may come before the card's own end,
// ends nothing: the pull request ends the card.
func TestAFailedIssueCloseOfAPullRequestsCardHeals(t *testing.T) {
	g := newPRCardRig(t, `{"number":5,"state":"closed","merged":true,"merged_by":{"login":"sebastian"}}`)
	g.closeFails = 1
	g.post("pull_request", prClosedBodyFrom("factory/schema-0123456789ab", "factory-agent/ai-software-factory-code", true), http.StatusInternalServerError)
	if got := g.store.tasks["t-pr"].Status; got != "waiting-human" || len(g.rows.rows) != 0 {
		t.Fatalf("after the failed close: status %q, rows %+v; want the card waiting, nothing recorded", got, g.rows.rows)
	}
	closed, _ := json.Marshal(map[string]any{"action": "closed", "issue": map[string]any{"number": 9, "user": map[string]string{"login": "factory-agent"}},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"}, "sender": map[string]string{"login": "factory-agent"}})
	g.post("issues", closed, http.StatusOK)
	if got := g.store.tasks["t-pr"].Status; got != "waiting-human" {
		t.Fatalf("after the bot's close of #9: status %q, want waiting-human", got)
	}
	if err := g.in.Resync(context.Background(), g.store.tasks["t-pr"]); err != nil {
		t.Fatal(err)
	}
	g.ended("done", "pull request #5 merged by sebastian", EventQuestionAnswered, "sebastian", ViaFactoryd)
}

// The card's explanatory comment is posted when it is missing, not only
// by the call that recorded factoryd's close: a post Forgejo fails is
// posted on the redelivery, once, and the card ends.
func TestAFailedCommentOfAPullRequestsCardIsPostedAgain(t *testing.T) {
	g := newPRCardRig(t, "")
	g.commentFails = 1
	body := prClosedBodyFrom("factory/schema-0123456789ab", "factory-agent/ai-software-factory-code", true)
	g.post("pull_request", body, http.StatusInternalServerError)
	if got := g.store.tasks["t-pr"].Status; got != "waiting-human" || g.comments != 0 || g.closes != 0 {
		t.Fatalf("after the failed comment: status %q, comments %d, closes %d; want the card waiting, #9 open", got, g.comments, g.closes)
	}
	g.post("pull_request", body, http.StatusOK)
	g.ended("done", "pull request #5 merged by sebastian", EventQuestionAnswered, "sebastian", ViaWebhook)
	if !slices.Equal(g.posted, []string{"factoryd: pull request #5 merged by sebastian; this card ends with it."}) {
		t.Fatalf("comments %q, want the one", g.posted)
	}
}

// The heal pass ends a pull request's human card whose closing webhook
// was lost, as the webhook does but via factoryd: a merge credited to its
// merger, a close without a merge to its closer, read from the pull
// request's timeline (a pull request is an issue). The bot's close is
// factoryd's own: the ask is withdrawn, as the copier's own close does. A
// card whose pull request is open stays waiting. A pull request Forgejo
// cannot read falls through to the issue: a writer's close of the card's
// issue ends it done.
func TestResyncEndsAPullRequestsHumanCard(t *testing.T) {
	ctx := context.Background()
	g := newPRCardRig(t, `{"number":5,"state":"closed","merged":true,"merged_by":{"login":"sebastian"}}`)
	if err := g.in.Resync(ctx, g.store.tasks["t-pr"]); err != nil {
		t.Fatal(err)
	}
	g.ended("done", "pull request #5 merged by sebastian", EventQuestionAnswered, "sebastian", ViaFactoryd)

	g = newPRCardRig(t, `{"number":5,"state":"closed","merged":false}`)
	g.prClosedBy = "sebastian"
	if err := g.in.Resync(ctx, g.store.tasks["t-pr"]); err != nil {
		t.Fatal(err)
	}
	g.ended("closed", "pull request #5 closed without a merge by sebastian", EventQuestionAnswered, "sebastian", ViaFactoryd)

	g = newPRCardRig(t, `{"number":5,"state":"closed","merged":false}`)
	g.prClosedBy = "factory-agent"
	if err := g.in.Resync(ctx, g.store.tasks["t-pr"]); err != nil {
		t.Fatal(err)
	}
	g.ended("closed", "factoryd closed pull request #5: the heal pass found it closed by factoryd", EventQuestionWithdrawn, FactoryDecider, ViaFactoryd)

	g = newPRCardRig(t, `{"number":5,"state":"open","merged":false}`)
	if err := g.in.Resync(ctx, g.store.tasks["t-pr"]); err != nil {
		t.Fatal(err)
	}
	if got := g.store.tasks["t-pr"].Status; got != "waiting-human" || len(g.rows.rows) != 0 || g.closes != 0 {
		t.Fatalf("an open pull request: status %q, rows %+v, closes %d; want nothing", got, g.rows.rows, g.closes)
	}

	g = newPRCardRig(t, "")
	g.closedBy = "sebastian"
	if err := g.in.Resync(ctx, g.store.tasks["t-pr"]); err != nil {
		t.Fatal(err)
	}
	if got := g.store.tasks["t-pr"].Status; got != "done" {
		t.Fatalf("pull request unread, issue closed by sebastian: status %q, want done", got)
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

// A human-lane task has no workflow: its issue closing by someone trusted
// finishes it as done, mirrored on the label; a stranger's close is
// ignored.
func TestIntakeIssueClosedFinishesHumanTask(t *testing.T) {
	for _, tc := range []struct{ sender, want string }{{"reader", "waiting-human"}, {"sebastian", "done"}} {
		in, store, _ := newTestIntake(t)
		store.tasks["t-human"] = Task{ID: "t-human", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "human", IssueNumber: 9}
		body, _ := json.Marshal(map[string]any{
			"action":     "closed",
			"issue":      map[string]any{"number": 9, "user": map[string]string{"login": "sebastian"}},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"sender":     map[string]string{"login": tc.sender},
		})
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
		if got := store.tasks["t-human"].Status; got != tc.want {
			t.Fatalf("closed by %s: status = %q, want %q", tc.sender, got, tc.want)
		}
		if len(workflowsOf(in).signals) != 0 {
			t.Fatalf("signals = %q, want none: the human lane has no workflow", workflowsOf(in).signals)
		}
		if tc.want == "done" {
			if labels := issueLabels(t, in.Forgejo, 9); !slices.Contains(labels, "state/done") {
				t.Fatalf("labels = %v, want state/done", labels)
			}
		}
	}
}

// Forgejo can deliver the same webhook twice; a processed delivery id is a
// no-op, and a delivery whose processing failed is processed again. A
// redelivery that slips through (a crash between processing and marking)
// adds no second intake event.
func TestIntakeRedeliveryIsProcessedOnce(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent"}
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

// Only people with write access steer a task; anyone else's comment or
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
	// a grant is reused, a denial is checked again every time: mallory 1,
	// reader 2, sebastian 1
	if n := writerReads(*requests); n != 4 {
		t.Fatalf("writer reads = %d, want 4", n)
	}
}

// Closing the issue fails the task, so only people with write access,
// or factoryd's bot where factoryd recorded the close, can do it through
// the webhook.
func TestIntakeIssueClosedNeedsWriteAccess(t *testing.T) {
	for closer, want := range map[string]int{"mallory": 0, "reader": 0, "sebastian": 1, "factory-agent": 1} {
		in, store, _ := newTestIntake(t)
		store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "agent", IssueNumber: 4}
		store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 4})
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

// A trusted close of a failed card's issue deletes the card's branch on the
// bot's fork, which Finish keeps for a retry, and signals nothing: the
// workflow is over. A done card's branch went at its end; a running card's
// close is its workflow's signal, as before.
func TestClosingAFailedCardDeletesItsBranch(t *testing.T) {
	branch := "DELETE /api/v1/repos/factory-agent/ai-software-factory-code/branches/factory/t1"
	for _, tc := range []struct {
		status, closer string
		recorded       bool
		deletes        []string
		signals        []string
	}{
		{status: "failed", closer: "sebastian", deletes: []string{branch}},
		{status: "failed", closer: "factory-agent", recorded: true, deletes: []string{branch}},
		{status: "failed", closer: "factory-agent"},
		{status: "failed", closer: "mallory"},
		{status: "done", closer: "sebastian", signals: []string{"t1 issue_closed sebastian #0: "}},
		{status: "running", closer: "sebastian", signals: []string{"t1 issue_closed sebastian #0: "}},
	} {
		in, store, requests := newTestIntake(t)
		store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: tc.status, Kind: "agent", IssueNumber: 4}
		if tc.recorded {
			store.InsertIntakeEvent(context.Background(), IntakeEvent{TaskID: "t1", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 4})
		}
		body, _ := json.Marshal(map[string]any{
			"action":     "closed",
			"issue":      map[string]any{"number": 4, "user": map[string]string{"login": "sebastian"}},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"sender":     map[string]string{"login": tc.closer},
		})
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("%s closed by %s: status = %d", tc.status, tc.closer, rr.Code)
		}
		var deletes []string
		for _, r := range *requests {
			if strings.HasPrefix(r, "DELETE ") {
				deletes = append(deletes, r)
			}
		}
		if !slices.Equal(deletes, tc.deletes) || !slices.Equal(workflowsOf(in).signals, tc.signals) {
			t.Errorf("%s task closed by %s (recorded %v): deletes %q, signals %q; want %q, %q",
				tc.status, tc.closer, tc.recorded, deletes, workflowsOf(in).signals, tc.deletes, tc.signals)
		}
	}
}

func TestReproCommand(t *testing.T) {
	for body, want := range map[string]string{
		"":         "",
		"no block": "",
		"Fix it.\n\n```repro\ngo test ./x\n```\n":             "go test ./x",
		"```sh\nls\n```\n```repro\r\nmake a\r\nmake b\r\n```": "make a\nmake b",
		"```repro\nnever closed":                              "",
		"```repro\n```":                                       "",
		"```reproduce\nx\n```":                                "",
		"  ```repro  \n  cmd\n  ```  ":                        "cmd",
	} {
		if got := ReproCommand(body); got != want {
			t.Errorf("ReproCommand(%q) = %q, want %q", body, got, want)
		}
	}
}

// ValidateReproCommand is the static check on a reproduction command at
// filing: one that chains cd or creates a git worktree returns the refusal
// reason, which names the sandbox rule; a plain command returns empty.
func TestValidateReproCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd    string
		refuse bool
	}{
		{"", false},
		{"make repro", false},
		{"go test ./...", false},
		{"git status && make test", false},
		{"echo cd to a directory", false},
		{"cd /tmp/x && make repro", true},
		{"make repro && cd other", true},
		{"make a; cd b; make c", true},
		{"cd /tmp/x || make repro", true},
		{"make repro\ncd dir\nmake b", true},
		{"cd x && cmd && cd -", false},
		{"cd /tmp/x && make repro && cd -", false},
		{"cd a && cmd && cd - && cd b && make c && cd -", false},
		{"cd x && cmd && cd - && cd y", true},
		{"cd a && cmd && cd - && cd b && make c", true},
		{"cd x && cmd && cd ..", true},
		{"cd - && cd x && cmd && cd -", true},
		{"cd x && cmd && cd - && cd -", true},
		{"cd - && make repro", true},
		{"git worktree add /tmp/w HEAD", true},
		{"make repro && GIT=1 git worktree list", true},
		{"CDIR=/tmp/x cd-helper", false},
	} {
		reason := ValidateReproCommand(tc.cmd)
		if (reason != "") != tc.refuse {
			t.Errorf("ValidateReproCommand(%q) = %q, refusal wanted %v", tc.cmd, reason, tc.refuse)
		}
		if reason != "" && !strings.Contains(reason, sandboxCheckoutRule) {
			t.Errorf("ValidateReproCommand(%q) = %q, refusal does not name the sandbox rule %q", tc.cmd, reason, sandboxCheckoutRule)
		}
	}
}

// The filing refusal of a refused reproduction command names the sandbox
// rule and the backlog.
func TestReproRefusalText(t *testing.T) {
	got := reproRefusal(ValidateReproCommand("cd /tmp/x && make repro"))
	want := "factoryd: the reproduction command chains cd out of the checked-out worktree: the sandbox has one checkout, the command runs in place. The card stays in backlog."
	if got != want {
		t.Errorf("reproRefusal = %q, want %q", got, want)
	}
	if got := reproRefusal(ValidateReproCommand("git worktree add /tmp/w HEAD")); !strings.Contains(got, "creates a git worktree: "+sandboxCheckoutRule) {
		t.Errorf("worktree reproRefusal = %q, want the sandbox rule named", got)
	}
}

// A writer's agent issue whose repro block chains cd is refused at filing:
// the card lands in the backlog with no reproduction command and one
// comment naming the sandbox rule; its coder workflow never starts (the
// backlog presort write-up does) and the labels that asked for the entry
// stay, so a later label event judges the fixed command again.
func TestIntakeRefusesCdChainRepro(t *testing.T) {
	in, store, requests := laneIntake(t, "maintenance", maintenanceLanes)
	body := "Fix it.\n\n```repro\ncd /tmp/x && make repro\n```\n"
	post(t, in, "issues", issueOpenedBody(21, "sebastian", []string{"agent"}, "A card", body))
	got := card(store, 21)
	if got.Status != "backlog" || got.Lane != "backlog" || got.ReproCommand != "" {
		t.Fatalf("card = %+v, want a backlog card with no reproduction command", got)
	}
	presort, _, _ := store.ReportOf(context.Background(), got.ID, SourcePresort)
	if started := workflowsOf(in).started; !slices.Equal(started, []string{presort.ID}) {
		t.Fatalf("started %q, want the presort alone", started)
	}
	if labels := issueLabels(t, in.Forgejo, 21); !slices.Contains(labels, "agent") {
		t.Fatalf("labels = %v, want the agent label kept", labels)
	}
	want := "factoryd: the reproduction command chains cd out of the checked-out worktree: the sandbox has one checkout, the command runs in place. The card stays in backlog."
	cs := comments(*requests)
	if len(cs) != 1 || cs[0] != want {
		t.Fatalf("comments = %q, want only %q", cs, want)
	}
}

// A writer's agent issue whose repro block creates a git worktree is
// refused at filing the same way.
func TestIntakeRefusesGitWorktreeRepro(t *testing.T) {
	in, store, requests := laneIntake(t, "maintenance", maintenanceLanes)
	body := "Fix it.\n\n```repro\ngit worktree add /tmp/w HEAD\n```\n"
	post(t, in, "issues", issueOpenedBody(22, "sebastian", []string{"agent"}, "A card", body))
	got := card(store, 22)
	if got.Status != "backlog" || got.Lane != "backlog" || got.ReproCommand != "" {
		t.Fatalf("card = %+v, want a backlog card with no reproduction command", got)
	}
	presort, _, _ := store.ReportOf(context.Background(), got.ID, SourcePresort)
	if started := workflowsOf(in).started; !slices.Equal(started, []string{presort.ID}) {
		t.Fatalf("started %q, want the presort alone", started)
	}
	if labels := issueLabels(t, in.Forgejo, 22); !slices.Contains(labels, "agent") {
		t.Fatalf("labels = %v, want the agent label kept", labels)
	}
	want := "factoryd: the reproduction command creates a git worktree: the sandbox has one checkout, the command runs in place. The card stays in backlog."
	cs := comments(*requests)
	if len(cs) != 1 || cs[0] != want {
		t.Fatalf("comments = %q, want only %q", cs, want)
	}
}

// An issue's body asks for its card's budget by a line "Size: <size>" or
// "Budget: <n> [EUR]" (policy budget 5 by default, 15 at most): a size
// gives its budget, an asked one wins over it, at most the project's card
// maximum, else a large card's 10. The last valid line of each key wins.
func TestIntakeBudgetBySize(t *testing.T) {
	for _, tc := range []struct {
		body    string
		project float64
		want    float64
	}{
		{"Build it.\n\nSize: large", 0, 10},
		{"Build it.\nBudget: 15 EUR\n", 15, 15},
		{"Build it.\nBudget: 15 EUR\n", 0, 10},
		{"budget: 40", 15, 15},
		{"Size: giant", 0, 5},
		{"Build it.", 0, 5},
		{"Size: small\nBUDGET: 7", 0, 7},
		{"Size: large\nSize: small", 0, 2},
		{"Size: small\n\nSize: large", 0, 10},
		{"notes\nsize: 100MB\n\nSize: large", 0, 10},
		{"Budget: abc\nBudget: 7", 0, 7},
		{"Budget: 7\nBudget: abc", 0, 7},
		{"The size: large is not a line of its own", 0, 5},
		{"Size: small\nRung: frontier, the queue code is subtle", 0, 5},
		{"Size: small\nRung: cheap, a rename", 0, 2},
		{"Size: small\nFix internal/factoryd/rolltask.go", 0, 5},
		{"Size: small\nRung: frontier, subtle\nBudget: 1", 0, 1},
	} {
		in, store, _ := newTestIntake(t)
		in.Policy = func() policy.Tree {
			return policy.Tree{Document: policy.Document{Budget: policy.Budget{TaskDefaultEUR: 5, TaskMaxEUR: 15, DayEUR: 20, MonthEUR: 50}}}
		}
		p := seedProject("sf/ai-software-factory-code")
		if tc.project > 0 {
			p.Budget = &policy.ProjectBudget{MonthEUR: 50, TaskMaxEUR: tc.project}
		}
		store.projects = []Project{p}
		body := issueOpenedBody(7, "sebastian", []string{"agent"}, "Add X", tc.body)
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, body = %s", tc.body, rr.Code, rr.Body)
		}
		if len(store.tasks) != 1 {
			t.Fatalf("%q: tasks = %d, want 1", tc.body, len(store.tasks))
		}
		for _, task := range store.tasks {
			if task.BudgetEUR != tc.want {
				t.Errorf("%q with project maximum %g: budget = %g, want %g", tc.body, tc.project, task.BudgetEUR, tc.want)
			}
		}
	}
}

// A push to main reaches MainPushed with the repository; a push to any
// other branch does not, and its failure fails the delivery.
func TestIntakePushToMain(t *testing.T) {
	in, _, _ := newTestIntake(t)
	var got []string
	fail := false
	in.MainPushed = func(_ context.Context, repository string) error {
		got = append(got, repository)
		if fail {
			return errors.New("forgejo down")
		}
		return nil
	}
	push := func(ref string) int {
		body, _ := json.Marshal(map[string]any{"ref": ref, "repository": map[string]any{"full_name": "demo-app/demo-app"}})
		return postWebhook(in, "push", body, sign(webhookSecret, body)).Code
	}
	if c := push("refs/heads/main"); c != http.StatusOK {
		t.Fatalf("status %d", c)
	}
	if c := push("refs/heads/task-1"); c != http.StatusOK {
		t.Fatalf("status %d", c)
	}
	fail = true
	if c := push("refs/heads/main"); c != http.StatusInternalServerError {
		t.Fatalf("failed reconcile: status %d", c)
	}
	if !slices.Equal(got, []string{"demo-app/demo-app", "demo-app/demo-app"}) {
		t.Fatalf("MainPushed got %v", got)
	}
}

// codeRepoOnly is a heal pass over the code repository alone.
type codeRepoOnly struct{}

func (codeRepoOnly) Projects(context.Context) ([]Project, error) {
	return []Project{seedProject("sf/ai-software-factory-code")}, nil
}

// A factory project's trust gate reads its writers with the project's own
// factory-merge token (ruling 164): collaborators on the assignee list,
// no org or team read; without the token it fails closed.
func TestTrustedOnAProjectUsesItsOwnToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token demo-merge-tok" {
			t.Errorf("%s read with %q, want the project's token", r.URL.Path, got)
		}
		switch r.URL.Path {
		case "/api/v1/repos/demo-app/demo-app/collaborators":
			servePage(w, r, 50, `{"login":"sebastian"}`, `{"login":"reader"}`, `{"login":"factory-merge"}`)
		case "/api/v1/repos/demo-app/demo-app/assignees":
			io.WriteString(w, `[{"login":"sebastian"},{"login":"factory-merge"}]`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	store := newFakeStore()
	store.projects = []Project{{Name: "demo-app", Repository: "demo-app/demo-app", MergeRule: "auto", Objects: "factory", Agents: "coder", File: true, GitToken: "g", MergeToken: "demo-merge-tok"}}
	in := &Intake{Store: store, Forgejo: &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", Seeded: testSeeded, CIOrgToken: "org-read-tok"}}
	for user, want := range map[string]bool{"sebastian": true, "reader": false, "outsider": false} {
		if got, err := in.trusted(context.Background(), "demo-app/demo-app", user); err != nil || got != want {
			t.Errorf("trusted(%s) = %v, %v; want %v", user, got, err, want)
		}
	}
	store.projects[0].MergeToken = ""
	if _, err := in.trusted(context.Background(), "demo-app/demo-app", "someone"); err == nil || !strings.Contains(err.Error(), "project demo-app has no read token") {
		t.Fatalf("unminted project: err = %v, want it to fail closed", err)
	}
}

// Ruling 150's resume rule: the issue author's comment is signalled as
// information whatever their access (poster_comment), a stranger's is
// only recorded, a writer's stays a human comment; the heal pass does not
// record the author's comment a second time.
func TestIntakePosterCommentIsSignalledAsInformation(t *testing.T) {
	in, store, _ := newTestIntake(t)
	task := Task{ID: "t-p", Repository: "sf/ai-software-factory-code", Status: "waiting-poster", IssueNumber: 30, Kind: "agent", Author: "visitor"}
	store.tasks["t-p"] = task
	comment := func(user string, id int) {
		b, _ := json.Marshal(map[string]any{
			"action":     "created",
			"issue":      map[string]any{"number": 30, "user": map[string]string{"login": "visitor"}},
			"comment":    map[string]any{"id": id, "user": map[string]string{"login": user}, "body": "Firefox 131"},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", user, rr.Code)
		}
	}
	comment("stranger", 1)
	comment("visitor", 2)
	comment("sebastian", 3)
	want := []string{"t-p poster_comment visitor #0 c2: Firefox 131", "t-p human_comment sebastian #0 c3: Firefox 131"}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want %q", got, want)
	}
	var kinds []string
	for _, e := range store.intakeEvents {
		kinds = append(kinds, fmt.Sprintf("%s %s trusted=%v", e.Kind, e.Author, e.Trusted))
	}
	if wantKinds := []string{"issue_comment stranger trusted=false", "poster_comment visitor trusted=false", "issue_comment sebastian trusted=true"}; !slices.Equal(kinds, wantKinds) {
		t.Fatalf("events = %q, want %q", kinds, wantKinds)
	}
	// the author's comment never counts as an escalation's answer
	if answers, _, _ := answersFrom(append([]IntakeEvent{{Kind: IntakeEscalation, CommentID: 1}}, store.intakeEvents...)); len(answers) != 1 || !strings.Contains(answers[0], "sebastian") {
		t.Fatalf("answers = %q, want only the writer's", answers)
	}
}

// A backlog card takes only a writer's agent label as its start (review
// I1): comments by its author or a writer are neither recorded nor
// signalled (a signal would start its workflow), and a writer's close
// ends it closed without one.
func TestIntakeBacklogCardRunsNothingUntilItStarts(t *testing.T) {
	in, store, _ := newTestIntake(t)
	store.tasks["t-s"] = Task{ID: "t-s", Repository: "sf/ai-software-factory-code", Status: "backlog", IssueNumber: 30, Kind: "agent", Author: "visitor"}
	for i, user := range []string{"visitor", "sebastian"} {
		b, _ := json.Marshal(map[string]any{
			"action":     "created",
			"issue":      map[string]any{"number": 30, "user": map[string]string{"login": "visitor"}},
			"comment":    map[string]any{"id": i + 1, "user": map[string]string{"login": user}, "body": "+1"},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", user, rr.Code)
		}
	}
	closed := func(sender string) []byte {
		b, _ := json.Marshal(map[string]any{"action": "closed", "issue": map[string]any{"number": 30, "user": map[string]string{"login": "visitor"}},
			"repository": map[string]any{"full_name": "sf/ai-software-factory-code"}, "sender": map[string]string{"login": sender}})
		return b
	}
	b := closed("visitor")
	postWebhook(in, "issues", b, sign(webhookSecret, b))
	if got := store.tasks["t-s"].Status; got != "backlog" {
		t.Fatalf("after the author's close: %s, want backlog (an outsider's close decides nothing)", got)
	}
	b = closed("sebastian")
	if rr := postWebhook(in, "issues", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
		t.Fatalf("close: status %d", rr.Code)
	}
	if w := workflowsOf(in); len(w.signals) != 0 || len(w.started) != 0 || len(store.intakeEvents) != 0 {
		t.Fatalf("signals %q, starts %q, events %d: want none for a backlog card", w.signals, w.started, len(store.intakeEvents))
	}
	if got := store.tasks["t-s"]; got.Status != "closed" || got.FinishedAt == nil {
		t.Fatalf("task = %+v, want closed", got)
	}
	if labels := issueLabels(t, in.Forgejo, 30); !slices.Contains(labels, "state/closed") {
		t.Fatalf("labels = %v", labels)
	}
}

// The heal pass takes in a backlog card's close the webhook missed, and
// starts nothing.
func TestHealPassClosesABacklogCard(t *testing.T) {
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	store := newFakeStore()
	in := &Intake{Store: store, Forgejo: forgejo, Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret}
	store.tasks["t-s"] = Task{ID: "t-s", Repository: "sf/ai-software-factory-code", Status: "backlog", Kind: "agent", IssueNumber: 30}
	HealPass(context.Background(), store, in.Workflows, in, &Completion{Store: store})
	if got := store.tasks["t-s"].Status; got != "closed" || len(workflowsOf(in).started) != 0 || len(workflowsOf(in).signals) != 0 {
		t.Fatalf("status %s, started %q, signals %q", got, workflowsOf(in).started, workflowsOf(in).signals)
	}
}

// An improvement issue stays in the backlog and opens an analyzer report task
// on it at the branch head, whose workflow starts (ruling 185); a
// redelivery or heal pass opens no second one.
func TestIntakeImprovementIsPresorted(t *testing.T) {
	in, store, _ := newTestIntake(t)
	opened := issueOpenedBody(12, "outsider", []string{"improvement"}, "A wish", "body")
	for range 2 {
		if rr := postWebhook(in, "issues", opened, sign(webhookSecret, opened)); rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
	}
	wish, ok, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", 12)
	if !ok || wish.Status != "backlog" {
		t.Fatalf("wish = %+v", wish)
	}
	presort, ok, _ := store.ReportOf(context.Background(), wish.ID, SourcePresort)
	if !ok || presort.Status != "queued" || presort.SHA != "sha-main-head" || presort.IssueNumber != 0 ||
		!slices.Equal(presort.RequiredCapabilities, []string{policy.ClassAnalyzer}) || presort.Objective != wish.Objective ||
		presort.Author != "outsider" || presort.AuthorWriter || len(store.tasks) != 2 {
		t.Fatalf("presort = %+v, %d tasks", presort, len(store.tasks))
	}
	if started := workflowsOf(in).started; !slices.Equal(started, []string{presort.ID}) {
		t.Fatalf("started = %v, want the presort's workflow once", started)
	}
}

// Ruling 179: every issue of a project on agents none lands in the human
// lane, whatever its labels, and runs nothing; the heal pass ends it when a
// writer closes it, the writers read with the project's own read token
// (the infra repository's factory-config, the addendum to Task 73). An
// issue of a repository no project names is ignored.
func TestIntakeAgentsNoneIsTheHumanLane(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		const infra = "/api/v1/repos/sf/ai-software-factory-infra"
		switch p := r.URL.Path; {
		case strings.HasPrefix(p, "/api/v1/orgs/") || strings.HasPrefix(p, "/api/v1/teams/"):
			serveWriters(t, w, r)
		case p == infra+"/collaborators" || p == infra+"/assignees":
			auth = append(auth, r.Header.Get("Authorization"))
			if r.Header.Get("Authorization") != "token config-tok" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, `[{"login":"factory-agent"}]`)
		case strings.HasSuffix(p, "/labels"):
			// the bot reads the infra repository and writes nothing on it
			w.WriteHeader(http.StatusForbidden)
		case p == infra+"/issues/21/timeline":
			io.WriteString(w, `[{"type":"close","user":{"login":"sebastian"}}]`)
		case p == infra+"/issues/21":
			io.WriteString(w, `{"state":"closed"}`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	store := newFakeStore()
	store.projects = []Project{{Name: "ai-software-factory-infra", Repository: "sf/ai-software-factory-infra", MergeRule: "human", Objects: "seed",
		Agents: "none", File: true, Tokens: policy.SeedTokens{Read: policy.SecretConfig}}}
	in := &Intake{Store: store, Forgejo: &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"},
		Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, Policy: testPolicy}
	on := func(repo string, number int64, labels ...string) []byte {
		return bytes.Replace(issueOpenedBody(number, "sebastian", labels, "Rotate the key", "please"), []byte("sf/ai-software-factory-code"), []byte(repo), 1)
	}
	for _, body := range [][]byte{on("sf/ai-software-factory-infra", 21, "agent", "bug"), on("sf/elsewhere", 22, "agent")} {
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body)
		}
	}
	if len(store.tasks) != 1 {
		t.Fatalf("tasks = %+v, want the infra issue's alone", store.tasks)
	}
	var task Task
	for _, tk := range store.tasks {
		task = tk
	}
	if task.Kind != "human" || task.Status != "waiting-human" || task.IssueNumber != 21 || task.SHA != "" || len(task.RequiredCapabilities) != 0 ||
		len(workflowsOf(in).started) != 0 || !task.AuthorWriter {
		t.Fatalf("task = %+v, started %v; want a human-lane task, its writer author, no workflow", task, workflowsOf(in).started)
	}
	if err := in.Resync(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if got := store.tasks[task.ID].Status; got != "done" || len(auth) == 0 || slices.ContainsFunc(auth, func(a string) bool { return a != "token config-tok" }) {
		t.Fatalf("after a writer's close: %s, writers read with %v", got, auth)
	}
}

// projectList is a heal pass's projects.
type projectList []Project

func (l projectList) Projects(context.Context) ([]Project, error) { return l, nil }

// The heal pass reads the open issues of every active project row and of
// nothing else: a factory project before register-project and a row
// without a file have no repository factoryd works on.
func TestResyncIssuesOverTheProjectRows(t *testing.T) {
	in, store, requests := newTestIntake(t)
	in.Projects = projectList{seedProject("sf/ai-software-factory-code"),
		{Name: "waiting", Repository: "waiting/waiting", Objects: "factory", Agents: "coder", File: true},
		{Name: "gone", Repository: "gone/gone", GitToken: "g"}}
	if err := in.ResyncIssues(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range *requests {
		if strings.Contains(p, "waiting/waiting") || strings.Contains(p, "gone/gone") {
			t.Fatalf("read %s", p)
		}
	}
	// five issues (all but the bot's) and the improvement's presort (ruling 185)
	if len(store.tasks) != 6 {
		t.Fatalf("tasks %d, want one per open issue of the code repository but the bot's, and a presort", len(store.tasks))
	}
}

// Review m8: a close by the bot account counts only where factoryd
// recorded that it closed the issue itself (IntakeFactoryClosed); any other
// bot close, from the webhook or the heal pass, leaves the task as it was.
func TestIntakeBotCloseCountsOnlyWhenFactorydRecordedIt(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		in, store, _ := newTestIntake(t)
		store.tasks["t-human"] = Task{ID: "t-human", Repository: "sf/ai-software-factory-code", Status: "waiting-human", Kind: "human", IssueNumber: 9}
		store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", IssueNumber: 4}
		if recorded {
			for _, e := range []IntakeEvent{{TaskID: "t-human", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 9},
				{TaskID: "t1", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 4}} {
				if _, err := store.InsertIntakeEvent(context.Background(), e); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, n := range []int64{9, 4} {
			body, _ := json.Marshal(map[string]any{
				"action":     "closed",
				"issue":      map[string]any{"number": n, "user": map[string]string{"login": "sebastian"}},
				"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
				"sender":     map[string]string{"login": "factory-agent"},
			})
			if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
		}
		human, signals := store.tasks["t-human"].Status, workflowsOf(in).signals
		if recorded && (human != "done" || len(signals) != 1 || !strings.HasPrefix(signals[0], "t1 issue_closed factory-agent ")) {
			t.Fatalf("recorded bot close: human task %s, signals %q", human, signals)
		}
		if !recorded && (human != "waiting-human" || len(signals) != 0) {
			t.Fatalf("unrecorded bot close acted: human task %s, signals %q", human, signals)
		}
	}
}

// Review m3: a project switched to agents none restarts none of its queued
// agent tasks, not on a redelivery and not in the heal pass: they move to
// the human lane, asking their issue's title as the question.
func TestQueuedTasksOfAProjectWithoutAgentsGoToTheHumanLane(t *testing.T) {
	none := seedProject("sf/ai-software-factory-code")
	none.Agents = "none"
	want := HumanQuestion{Ask: "Add the runner", Why: "filed by sebastian for an agent; the project runs no agents now", Info: "objective body",
		Choices: []QuestionChoice{{Value: AnswerClose, Label: "Done"}}}
	for _, via := range []string{"redelivery", "heal pass"} {
		in, store, _ := newTestIntake(t)
		in.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: store, Store: store}
		store.projects = []Project{none}
		store.tasks["t-q"] = Task{ID: "t-q", Repository: "sf/ai-software-factory-code", Status: "queued", Kind: "agent", IssueNumber: 7,
			Objective: "Add the runner\n\nobjective body", Author: "sebastian", Source: "webhook"}
		if via == "redelivery" {
			body := issueOpenedBody(7, "sebastian", []string{"agent"}, "Add the runner", "objective body")
			if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
				t.Fatalf("status %d", rr.Code)
			}
		} else {
			in.Projects = projectList{none}
			HealPass(context.Background(), store, in.Workflows, in, &Completion{Store: store})
		}
		if got := store.tasks["t-q"]; got.Kind != "human" || got.Status != "waiting-human" || slices.Contains(workflowsOf(in).started, "t-q") {
			t.Fatalf("%s: task %+v, started %v", via, got, workflowsOf(in).started)
		}
		var asks []IntakeEvent
		for _, e := range store.intakeEvents {
			if e.TaskID == "t-q" && e.Kind == IntakeEscalation {
				asks = append(asks, e)
			}
		}
		if len(asks) != 1 || asks[0].Body != "Add the runner" || asks[0].Question == nil || !reflect.DeepEqual(*asks[0].Question, want) ||
			!slices.Equal(asks[0].Choices, []string{"Done"}) {
			t.Fatalf("%s: intake asks %+v, want the issue's title asked", via, asks)
		}
		var asked []Event
		for _, e := range store.events {
			if e.TaskID == "t-q" && e.Kind == EventQuestionAsked {
				asked = append(asked, e)
			}
		}
		if len(asked) != 1 || asked[0].Via != ViaFactoryd || asked[0].Detail["ask"] != "Add the runner" ||
			asked[0].Detail["why"] != "filed by sebastian for an agent; the project runs no agents now" || asked[0].Detail["source"] != "webhook" ||
			fmt.Sprint(asked[0].Detail["choices"]) != "[Done]" {
			t.Fatalf("%s: question_asked rows %+v", via, asked)
		}
	}
}

// Ruling 180: an alert filed into the human lane of a project on agents
// none is judged by its close: real when a writer closes it as fixed,
// noise when they label it noise first.
func TestHumanLaneAlertVerdictFollowsTheClose(t *testing.T) {
	const infra = "/api/v1/repos/sf/ai-software-factory-infra"
	labels := map[string]string{
		infra + "/issues/31/labels": `[{"name":"bug"},{"name":"human"},{"name":"state/waiting-human"}]`,
		infra + "/issues/32/labels": `[{"name":"bug"},{"name":"human"},{"name":"noise"},{"name":"state/waiting-human"}]`,
	}
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case strings.HasPrefix(p, "/api/v1/orgs/") || strings.HasPrefix(p, "/api/v1/teams/"):
			serveWriters(t, w, r)
		case p == infra+"/collaborators" || p == infra+"/assignees":
			io.WriteString(w, `[{"login":"sebastian"}]`)
		case r.Method == http.MethodGet && labels[p] != "":
			io.WriteString(w, labels[p])
		case strings.HasSuffix(p, "/labels"):
		case strings.HasSuffix(p, "/timeline"):
			io.WriteString(w, `[{"type":"close","user":{"login":"sebastian"}}]`)
		case strings.HasPrefix(p, infra+"/issues/"):
			io.WriteString(w, `{"state":"closed"}`)
		default:
			t.Fatalf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	store := newFakeStore()
	store.projects = []Project{{Name: "ai-software-factory-infra", Repository: "sf/ai-software-factory-infra", MergeRule: "human", Objects: "seed",
		Agents: "none", File: true, Tokens: policy.SeedTokens{Read: policy.SecretConfig}}}
	in := &Intake{Store: store, Forgejo: &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded, CIOrgToken: "org-read-tok"},
		Workflows: &fakeWorkflows{}, BotUser: "factory-agent", Secret: webhookSecret, Policy: testPolicy}
	ctx := context.Background()
	for _, n := range []int64{31, 32} {
		task := Task{ID: fmt.Sprintf("t-%d", n), Kind: "human", Status: "waiting-human", Repository: "sf/ai-software-factory-infra",
			IssueNumber: n, Source: SourceAlert}
		if err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, TaskID: task.ID, Fingerprint: fmt.Sprintf("fp%d", n)}); err != nil {
			t.Fatal(err)
		}
		if err := in.Resync(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]string{}
	for _, a := range store.alertRows() {
		got[a.TaskID] = a.Verdict + " by " + a.ResolvedBy
	}
	want := map[string]string{"t-31": "real by sebastian", "t-32": "noise by sebastian"}
	if !maps.Equal(got, want) || store.tasks["t-31"].Status != "done" || store.tasks["t-32"].Status != "done" {
		t.Fatalf("verdicts %v, want %v; tasks %s, %s", got, want, store.tasks["t-31"].Status, store.tasks["t-32"].Status)
	}
}

// The console's Close of a card without an issue (roadmap 170) ends it in
// the store alone, with no Forgejo call: a backlog or failed card closed;
// one waiting on a human through its workflow, signalled as by its
// issue's close.
func TestIntakeCloseCardWithoutAnIssue(t *testing.T) {
	in, store, requests := newTestIntake(t)
	ctx := context.Background()
	for status, want := range map[string]string{"backlog": "closed", "failed": "closed", "waiting-human": "waiting-human"} {
		store.CreateTask(ctx, Task{ID: "t-" + status, Kind: "agent", Status: status, Repository: "sf/ai-software-factory-code"})
		err := in.CloseCard(ctx, "t-"+status, "sebastian")
		if got, _, _ := store.Task(ctx, "t-"+status); err != nil || got.Status != want {
			t.Fatalf("close %s: %v, status %q, want %q", status, err, got.Status, want)
		}
	}
	if signals := workflowsOf(in).signals; !slices.Equal(signals, []string{"t-waiting-human issue_closed sebastian #0: "}) || len(*requests) != 0 {
		t.Fatalf("signals %q, Forgejo requests %v; want the waiting card's issue_closed alone", signals, *requests)
	}
}

// A merged card's issue closes with its own merge (Closes #N), and a
// webhook of that close looks like a human's: the issue close path leaves
// a merged card merged, nothing signalled to the workflow that ended at
// the merge. The console's Close of a merged card (CloseCard) ends it
// through the runner's CloseMerged with the closer.
func TestClosingAMergedCardEndsItClosed(t *testing.T) {
	in, store, _ := newTestIntake(t)
	var closed []string
	in.CloseMerged = func(ctx context.Context, id, closer string) error {
		closed = append(closed, id+" by "+closer)
		return nil
	}
	store.tasks["t1"] = Task{ID: "t1", Repository: "sf/ai-software-factory-code", Status: "merged", Kind: "agent", IssueNumber: 4, PRNumber: 7, FinishedAt: justNow()}
	store.tasks["t2"] = Task{ID: "t2", Repository: "sf/ai-software-factory-code", Status: "merged", Kind: "agent", IssueNumber: 5, PRNumber: 8, FinishedAt: justNow()}
	body, _ := json.Marshal(map[string]any{
		"action":     "closed",
		"issue":      map[string]any{"number": 4, "user": map[string]string{"login": "sebastian"}},
		"repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
		"sender":     map[string]string{"login": "sebastian"},
	})
	if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(closed) != 0 || len(workflowsOf(in).signals) != 0 || store.tasks["t1"].Status != "merged" {
		t.Fatalf("the issue's close: closed %q, signals %q, status %s; want the card left merged", closed, workflowsOf(in).signals, store.tasks["t1"].Status)
	}
	if err := in.CloseCard(context.Background(), "t2", "alice"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"t2 by alice"}; !slices.Equal(closed, want) || len(workflowsOf(in).signals) != 0 {
		t.Fatalf("closed %q, signals %q; want %q and no signal", closed, workflowsOf(in).signals, want)
	}
}

// A card's rung comes from its body's Rung line, else from its size and
// its text: large, or factoryd's own package, start on frontier; the rest
// on cheap (roadmap 222, item 5). The size is kept on the row.
func TestIntakeReadsAndDerivesTheRung(t *testing.T) {
	for _, tc := range []struct {
		body       string
		rung, size string
	}{
		{"Build it.\n\nSize: small", RungCheap, "small"},
		{"Build it.\n\nSize: medium", RungCheap, "medium"},
		{"Build it.\n\nSize: large", RungFrontier, "large"},
		{"Build it.\n\nSize: small\nRung: frontier, the queue code is subtle", RungFrontier, "small"},
		{"Build it.\n\nSize: large\nRung: cheap, a rename", RungCheap, "large"},
		{"Fix the roll in internal/factoryd/rolltask.go.", RungFrontier, ""},
		{"Build it.\nDirectories: internal/factoryd, cmd/factoryd", RungFrontier, ""},
		{"Build it.\nDirectories: internal/console", RungCheap, ""},
		{"rung: FRONTIER, said so", RungFrontier, ""},
		{"Rung: giant, nope", RungCheap, ""},
		{"Build it.", RungCheap, ""},
	} {
		in, store, _ := newTestIntake(t)
		in.Policy = func() policy.Tree {
			return policy.Tree{Document: policy.Document{Budget: policy.Budget{TaskDefaultEUR: 5, TaskMaxEUR: 15, DayEUR: 20, MonthEUR: 50}}}
		}
		store.projects = []Project{seedProject("sf/ai-software-factory-code")}
		body := issueOpenedBody(7, "sebastian", []string{"agent"}, "Add X", tc.body)
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("%q: status = %d, body = %s", tc.body, rr.Code, rr.Body)
		}
		var got Task
		for _, task := range store.tasks {
			got = task
		}
		if got.Rung != tc.rung || got.Size != tc.size {
			t.Fatalf("%q: rung %q size %q, want %q %q", tc.body, got.Rung, got.Size, tc.rung, tc.size)
		}
	}
}

// A card's image comes from its body's Image line, the last one, key and
// name in any case, where a writer filed it and the project pins that
// name in the version in force; default, a name it does not pin, an
// outsider's line and no line leave the card on its project's default
// image.
func TestIntakeReadsTheImage(t *testing.T) {
	for _, tc := range []struct{ poster, body, image string }{
		{"sebastian", "Build it.\n\nSize: small\nImage: chromium", "chromium"},
		{"sebastian", "Build it.\nimage: Chromium", "chromium"},
		{"sebastian", "Build it.\nImage: video\nImage: chromium", "chromium"},
		{"sebastian", "Build it.\nImage: default", ""},
		{"sebastian", "Build it.\nImage: browser", ""},
		{"sebastian", "Image: the screenshot is broken", ""},
		{"sebastian", "Build it.", ""},
		{"outsider", "Build it.\n\nImage: chromium", ""},
	} {
		in, store, _ := newTestIntake(t)
		p := seedProject("sf/ai-software-factory-code")
		p.Images = map[string]policy.ImagePin{"chromium": {Tools: []string{"chromium"}}, "video": {Tools: []string{"ffmpeg"}}}
		store.projects = []Project{p}
		body := issueOpenedBody(7, tc.poster, []string{"agent"}, "Add X", tc.body)
		if rr := postWebhook(in, "issues", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("%s %q: status = %d, body = %s", tc.poster, tc.body, rr.Code, rr.Body)
		}
		got, ok, _ := store.TaskByIssue(context.Background(), "sf/ai-software-factory-code", 7)
		if !ok || got.Image != tc.image {
			t.Fatalf("%s %q: image %q (%v), want %q", tc.poster, tc.body, got.Image, ok, tc.image)
		}
	}
}

// A finished run's webhook nudges the poller with the run's repository,
// whichever workflow-run event it is; a bad signature nudges nothing.
func TestAWorkflowRunEventNudgesThePoller(t *testing.T) {
	in, _, _ := newTestIntake(t)
	var got []string
	in.Nudge = func(repository string) { got = append(got, repository) }
	body := []byte(`{"action":"success","run":{"status":"success","repository":{"full_name":"sf/demo-app"}}}`)
	if rr := postWebhook(in, "workflow_run_success", body, "bad"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", rr.Code)
	}
	if len(got) != 0 {
		t.Fatalf("nudged by an unsigned delivery: %v", got)
	}
	for _, event := range []string{"workflow_run_success", "workflow_run_failure", "workflow_run_running"} {
		if rr := postWebhook(in, event, body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("%s: %d", event, rr.Code)
		}
	}
	if !slices.Equal(got, []string{"sf/demo-app", "sf/demo-app", "sf/demo-app"}) {
		t.Fatalf("nudges = %v", got)
	}
}

// An event type the intake does not handle is a naming mismatch, since
// hooks deliver only subscribed events: logged once per type, still 2xx.
func TestAnUnknownEventTypeLogsOnce(t *testing.T) {
	logs := captureLog(t)
	in, _, _ := newTestIntake(t)
	body := []byte(`{"run":{"repository":{"full_name":"sf/demo-app"}}}`)
	for range 2 {
		if rr := postWebhook(in, "workflow_run_sucess", body, sign(webhookSecret, body)); rr.Code != http.StatusOK {
			t.Fatalf("unknown type: %d", rr.Code)
		}
	}
	if n := strings.Count(logs.String(), "unhandled event type"); n != 1 {
		t.Fatalf("unknown type logged %d times, want 1:\n%s", n, logs.String())
	}
	logs.Reset()
	postWebhook(in, "workflow_run_success", body, sign(webhookSecret, body))
	if strings.Contains(logs.String(), "unhandled") {
		t.Fatalf("a known type logged: %s", logs.String())
	}
}
