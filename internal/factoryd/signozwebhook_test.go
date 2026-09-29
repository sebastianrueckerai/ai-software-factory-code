package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// alertForgejo is the project repository demo-app/demo-app as the alert
// intake sees it: issues filed, labels set and comments posted, in order.
type alertForgejo struct {
	mu       sync.Mutex
	issues   []string // "title\n\nbody"
	labels   map[int64][]string
	comments []string // "#n body"
	closed   []int64  // issues closed
	failNext int      // issue creations to answer 500
}

func newAlertForgejo(t *testing.T) (*Forgejo, *alertForgejo) {
	t.Helper()
	fj := &alertForgejo{labels: map[int64][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fj.mu.Lock()
		defer fj.mu.Unlock()
		if u, p, ok := r.BasicAuth(); !ok || u != "factory-agent" || p != "bot-pw" {
			t.Errorf("%s %s not as the bot", r.Method, r.URL.Path)
		}
		const repo = "/api/v1/repos/demo-app/demo-app"
		var n int64
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repo:
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == http.MethodGet && r.URL.Path == repo+"/branches/main":
			io.WriteString(w, `{"commit":{"id":"sha-demo-main"}}`)
		case r.Method == http.MethodPost && r.URL.Path == repo+"/issues":
			var in struct{ Title, Body string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.issues = append(fj.issues, in.Title+"\n\n"+in.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://git.example/demo-app/demo-app/issues/%d"}`, len(fj.issues), len(fj.issues))
		case r.Method == http.MethodPut && fmtScan(r.URL.Path, repo+"/issues/%d/labels", &n):
			var in struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.labels[n] = in.Labels
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && fmtScan(r.URL.Path, repo+"/issues/%d/comments", &n):
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.comments = append(fj.comments, fmt.Sprintf("#%d %s", n, in.Body))
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}, fj
}

func fmtScan(path, format string, n *int64) bool {
	c, err := fmt.Sscanf(path, format, n)
	return err == nil && c == 1 && fmt.Sprintf(format, *n) == path
}

// fakeNamespaces answers the factory/project label of each namespace and
// counts the reads.
type fakeNamespaces struct {
	mu     sync.Mutex
	labels map[string]string
	reads  int
}

func (f *fakeNamespaces) NamespaceLabel(_ context.Context, name, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if key != ProjectLabel {
		return "", fmt.Errorf("label %q, want %q", key, ProjectLabel)
	}
	return f.labels[name], nil
}

type alertRig struct {
	in    *AlertIntake
	store *fakeStore
	fj    *alertForgejo
	ns    *fakeNamespaces
	flows *fakeWorkflows
}

func newAlertRig(t *testing.T) *alertRig {
	t.Helper()
	forgejo, fj := newAlertForgejo(t)
	store := newFakeStore()
	ns := &fakeNamespaces{labels: map[string]string{"demo-staging": "demo-app", "demo-prod": "demo-app", "stray": "gone",
		"foreign": "demo-app", "legacy": "old"}}
	projects := &memProjects{m: map[string]Project{
		"demo-app": {Name: "demo-app", Repository: "demo-app/demo-app", Managed: true, GitToken: "git-tok", Namespaces: []string{"demo-staging", "demo-prod"}},
		"old":      {Name: "old", Repository: "sf/old", GitToken: "git-tok", Namespaces: []string{"legacy"}},
	}}
	flows := &fakeWorkflows{}
	in := &AlertIntake{User: "signoz", Password: "hook-pw", Namespaces: ns, Projects: projects, Store: store,
		Forgejo: forgejo, Workflows: flows, Budget: testBudget}
	return &alertRig{in: in, store: store, fj: fj, ns: ns, flows: flows}
}

func (r *alertRig) post(t *testing.T, user, password string, alerts ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"version": "4", "status": "firing", "receiver": "factoryd", "alerts": alerts})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/signoz", bytes.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	rr := httptest.NewRecorder()
	r.in.ServeHTTP(rr, req)
	return rr
}

// sigAlert is one alert as SigNoz v0.143.0's webhook sends it (the
// Alertmanager v4 message): the rule's labels, the group-by labels and
// SigNoz's own (alertname, ruleId, ruleSource, threshold.name).
func sigAlert(status, fingerprint, namespace, startsAt, endsAt string) map[string]any {
	return map[string]any{
		"status": status,
		"labels": map[string]string{"alertname": "5xx rate above 5 %", "namespace": namespace, "project": "demo-app",
			"severity": "critical", "ruleId": "r-1", "threshold.name": "critical"},
		"annotations":  map[string]string{"value": "14.29", "summary": "5xx share of the requests over 5 minutes"},
		"startsAt":     startsAt,
		"endsAt":       endsAt,
		"generatorURL": "https://signoz.example/alerts/overview?ruleId=r-1",
		"fingerprint":  fingerprint,
	}
}

const (
	firedAt    = "2026-09-28T20:01:29Z"
	neverEnds  = "0001-01-01T00:00:00Z"
	resolvedAt = "2026-09-28T20:40:29Z"
)

func TestAlertIntakeBasicAuth(t *testing.T) {
	for _, c := range []struct {
		name, user, password string
		configured           string
		want                 int
	}{
		{"right", "signoz", "hook-pw", "hook-pw", http.StatusOK},
		{"no credentials", "", "", "hook-pw", http.StatusUnauthorized},
		{"wrong password", "signoz", "nope", "hook-pw", http.StatusUnauthorized},
		{"wrong user", "other", "hook-pw", "hook-pw", http.StatusUnauthorized},
		{"no password configured", "signoz", "", "", http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newAlertRig(t)
			r.in.Password = c.configured
			rr := r.post(t, c.user, c.password, sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
			if rr.Code != c.want {
				t.Fatalf("status %d, want %d", rr.Code, c.want)
			}
			if c.want == http.StatusUnauthorized {
				if rr.Header().Get("WWW-Authenticate") == "" || len(r.fj.issues) != 0 || len(r.store.alertRows()) != 0 {
					t.Fatalf("refused, but WWW-Authenticate %q, %d issues, %d alert rows", rr.Header().Get("WWW-Authenticate"), len(r.fj.issues), len(r.store.alertRows()))
				}
			}
		})
	}
}

func TestAlertIntakeFilesABugIssueThatStartsATask(t *testing.T) {
	r := newAlertRig(t)
	if rr := r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds)); rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(r.fj.issues) != 1 {
		t.Fatalf("issues %d, want 1", len(r.fj.issues))
	}
	want := "alert: 5xx rate above 5 % in demo-staging\n\n" +
		"factoryd: SigNoz fires the alert 5xx rate above 5 % in namespace demo-staging of project demo-app.\n\n" +
		"Firing since 2026-09-28T20:01:29Z. SigNoz: https://signoz.example/alerts/overview?ruleId=r-1\n\n" +
		"Value and annotations:\n\n```\nvalue=14.29\nsummary=5xx share of the requests over 5 minutes\n```\n\n" +
		"Labels:\n\n```\nalertname=5xx rate above 5 %\nnamespace=demo-staging\nproject=demo-app\nruleId=r-1\nseverity=critical\nthreshold.name=critical\n```\n\n" +
		"filed by factoryd from SigNoz alert fp1"
	if r.fj.issues[0] != want {
		t.Fatalf("issue\n%s\nwant\n%s", r.fj.issues[0], want)
	}
	if got := r.fj.labels[1]; !slices.Equal(got, []string{"bug", "state/queued"}) {
		t.Fatalf("labels %v, want [bug state/queued]", got)
	}
	if len(r.store.tasks) != 1 {
		t.Fatalf("tasks %d, want 1", len(r.store.tasks))
	}
	var task Task
	for _, task = range r.store.tasks {
	}
	if task.Repository != "demo-app/demo-app" || task.IssueNumber != 1 || task.Kind != "agent" || task.Status != "queued" ||
		task.SHA != "sha-demo-main" || task.Source != SourceAlert || task.ReproCommand != "" || task.BudgetEUR != 1.5 ||
		task.Author != "factory-agent" || task.AuthorWriter || task.Standing() != harness.StandingTelemetry || task.Objective != want {
		t.Fatalf("task %+v", task)
	}
	if !slices.Equal(r.flows.started, []string{task.ID}) {
		t.Fatalf("started %v, want [%s]", r.flows.started, task.ID)
	}
	rows := r.store.alertRows()
	if len(rows) != 1 {
		t.Fatalf("alert rows %d, want 1", len(rows))
	}
	a := rows[0]
	if a.Kind != AlertSigNoz || a.Source != "5xx rate above 5 %" || a.TaskID != task.ID || a.Fingerprint != "fp1" || a.Verdict != "" ||
		a.Detail["project"] != "demo-app" || a.Detail["namespace"] != "demo-staging" || a.Detail["issue"] != int64(1) ||
		a.Detail["starts_at"] != firedAt || a.Detail["link"] != "https://signoz.example/alerts/overview?ruleId=r-1" {
		t.Fatalf("alert row %+v", a)
	}
}

// One issue per fingerprint: a resent notification of the same firing
// (Alertmanager repeats every alert of a group) files nothing and says
// nothing; a new firing of it while its task is open is a comment; a
// firing after the task ended is a new issue.
func TestAlertIntakeDedupesByFingerprint(t *testing.T) {
	r := newAlertRig(t)
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds),
		sigAlert("firing", "fp2", "demo-prod", firedAt, neverEnds))
	if len(r.fj.issues) != 2 || len(r.fj.comments) != 0 || len(r.store.tasks) != 2 {
		t.Fatalf("after a resend and a second fingerprint: %d issues, %d comments, %d tasks; want 2, 0, 2", len(r.fj.issues), len(r.fj.comments), len(r.store.tasks))
	}
	if !strings.HasPrefix(r.fj.issues[1], "alert: 5xx rate above 5 % in demo-prod\n") {
		t.Fatalf("second issue %q", r.fj.issues[1])
	}
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", "2026-09-28T21:00:00Z", neverEnds))
	if want := []string{"#1 factoryd: the alert fired again at 2026-09-28T21:00:00Z.\n\n```\nvalue=14.29\n```"}; !slices.Equal(r.fj.comments, want) {
		t.Fatalf("comments %q, want %q", r.fj.comments, want)
	}
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", "2026-09-28T21:00:00Z", neverEnds))
	if len(r.fj.comments) != 1 || len(r.fj.issues) != 2 {
		t.Fatalf("the re-firing resent: %d comments, %d issues; want 1, 2", len(r.fj.comments), len(r.fj.issues))
	}
	first := r.store.alertRows()[0].TaskID
	task := r.store.tasks[first]
	task.Status = "done"
	r.store.tasks[first] = task
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", "2026-09-28T21:00:00Z", neverEnds))
	if len(r.fj.issues) != 2 {
		t.Fatalf("the same firing resent after its task ended filed issue %d", len(r.fj.issues))
	}
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", "2026-09-28T22:00:00Z", neverEnds))
	if len(r.fj.issues) != 3 || len(r.store.tasks) != 3 || len(r.store.alertRows()) != 3 || len(r.fj.comments) != 1 {
		t.Fatalf("a new firing after the task ended: %d issues, %d tasks, %d rows, %d comments; want 3, 3, 3, 1",
			len(r.fj.issues), len(r.store.tasks), len(r.store.alertRows()), len(r.fj.comments))
	}
}

func TestAlertIntakeCommentsTheResolve(t *testing.T) {
	r := newAlertRig(t)
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	first := r.store.alertRows()[0].TaskID
	task := r.store.tasks[first]
	task.Status = "done"
	r.store.tasks[first] = task
	r.post(t, "signoz", "hook-pw", sigAlert("resolved", "fp1", "demo-staging", firedAt, resolvedAt))
	r.post(t, "signoz", "hook-pw", sigAlert("resolved", "fp1", "demo-staging", firedAt, resolvedAt))
	if want := []string{"#1 factoryd: resolved at 2026-09-28T20:40:29Z"}; !slices.Equal(r.fj.comments, want) {
		t.Fatalf("comments %q, want %q (once, on the issue although its task ended)", r.fj.comments, want)
	}
	if got := r.store.alertRows()[0].Detail["resolved_at"]; got != resolvedAt {
		t.Fatalf("resolved_at %v", got)
	}
	r.post(t, "signoz", "hook-pw", sigAlert("resolved", "fp-unknown", "demo-staging", firedAt, resolvedAt))
	if len(r.fj.comments) != 1 || len(r.fj.issues) != 1 {
		t.Fatalf("a resolve without a row: %d comments, %d issues", len(r.fj.comments), len(r.fj.issues))
	}
}

// The namespace label maps to the project through the namespace's
// factory/project label, read once per namespace; an alert without a
// mapped project, of a project that does not list the namespace, or of an
// unmanaged project, is dropped (200, nothing filed).
func TestAlertIntakeMapsNamespaceToProject(t *testing.T) {
	r := newAlertRig(t)
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp2", "demo-staging", "2026-09-28T20:05:00Z", neverEnds))
	if r.ns.reads != 1 || len(r.fj.issues) != 2 {
		t.Fatalf("namespace reads %d, issues %d; want 1, 2", r.ns.reads, len(r.fj.issues))
	}
	prod := sigAlert("firing", "fp3", "demo-prod", firedAt, neverEnds)
	r.post(t, "signoz", "hook-pw", prod)
	if len(r.fj.issues) != 3 || !strings.HasPrefix(r.fj.issues[2], "alert: 5xx rate above 5 % in demo-prod\n") {
		t.Fatalf("demo-prod: %d issues", len(r.fj.issues))
	}
	// a label the rule's query produced never names the namespace: only
	// the job's namespace label does
	grouped := sigAlert("firing", "fp4", "", firedAt, neverEnds)
	delete(grouped["labels"].(map[string]string), "namespace")
	grouped["labels"].(map[string]string)["k8s.namespace.name"] = "demo-prod"
	grouped["labels"].(map[string]string)["k8s_namespace_name"] = "demo-prod"
	r.post(t, "signoz", "hook-pw", grouped)
	if len(r.fj.issues) != 3 {
		t.Fatalf("an alert without the namespace label filed issue %d", len(r.fj.issues))
	}
	for _, ns := range []string{"platform", "stray", "foreign", "legacy", "", "Bad/../ns"} {
		if rr := r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp-"+ns, ns, firedAt, neverEnds)); rr.Code != http.StatusOK {
			t.Fatalf("namespace %q: status %d", ns, rr.Code)
		}
	}
	if len(r.fj.issues) != 3 || len(r.store.tasks) != 3 || len(r.store.alertRows()) != 3 {
		t.Fatalf("unmapped alerts filed: %d issues, %d tasks, %d rows", len(r.fj.issues), len(r.store.tasks), len(r.store.alertRows()))
	}
	if r.ns.reads != 6 {
		t.Fatalf("namespace reads %d, want 6 (demo-staging, demo-prod, platform, stray, foreign, legacy; none for an empty or invalid name)", r.ns.reads)
	}
}

// Label and annotation values are the project's and its telemetry's text:
// one line each, inside a fence longer than any backtick run they carry.
func TestAlertIssueBodyFencesTheValues(t *testing.T) {
	a := SigNozAlert{Labels: map[string]string{"alertname": "x\ny @sebastian", "namespace": "demo-staging", "route": "a```b\n```repro\nrm -rf /\n```"},
		Annotations: map[string]string{"value": "1"}, StartsAt: at(firedAt), GeneratorURL: "javascript:alert(1)", Fingerprint: "fp"}
	title, body := alertIssue(a, "demo-app", "demo-staging")
	if title != "alert: x y @\u200bsebastian in demo-staging" || strings.Contains(body, "@sebastian") {
		t.Fatalf("title %q", title)
	}
	if ReproCommand(body) != "" || strings.Contains(body, "javascript:") || !strings.Contains(body, "````\nalertname=x y @\u200bsebastian\nnamespace=demo-staging\nroute=a```b ```repro rm -rf / ```\n````") {
		t.Fatalf("body %q", body)
	}
}

func TestAlertVerdict(t *testing.T) {
	for _, c := range []struct{ final, reason, want string }{
		{"done", "pull request merged by factory-merge on the merge rule auto", VerdictReal},
		{"done", "pull request merged", VerdictReal},
		{"failed", "issue closed without merge", VerdictNoise},
		{"failed", "issue closed while no pull request of this workflow was open", VerdictNoise},
		{"failed", "issue closed; merge state unknown: boom", ""},
		{"failed", "attempts exhausted", ""},
		{"failed", "pull request closed without merge", ""},
		{"closed", "not reproducible, no answer in 14 days", ""},
	} {
		if got := alertVerdict(c.final, c.reason); got != c.want {
			t.Errorf("alertVerdict(%q, %q) = %q, want %q", c.final, c.reason, got, c.want)
		}
	}
}

// A fix that did not fix: the same firing still resent 30 minutes after
// its task merged is filed once more, and only once; after a human closed
// the issue without a merge it is not.
func TestAlertIntakeRefilesAFiringItsTaskDidNotEnd(t *testing.T) {
	r := newAlertRig(t)
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	first := r.store.alertRows()[0].TaskID
	ended := time.Now().Add(-31 * time.Minute)
	task := r.store.tasks[first]
	task.Status, task.FinishedAt = "failed", &ended
	r.store.tasks[first] = task
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	if len(r.fj.issues) != 1 {
		t.Fatalf("31 minutes after a human closed it: %d issues, want 1", len(r.fj.issues))
	}
	ended = time.Now().Add(-10 * time.Minute)
	task.Status, task.FinishedAt = "done", &ended
	r.store.tasks[first] = task
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	if len(r.fj.issues) != 1 {
		t.Fatalf("10 minutes after the task ended: %d issues, want 1", len(r.fj.issues))
	}
	ended = time.Now().Add(-31 * time.Minute)
	task.FinishedAt = &ended
	r.store.tasks[first] = task
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	if len(r.fj.issues) != 2 || !strings.Contains(r.fj.issues[1], "\n\nfactoryd: the alert still fires from the same start 30m0s after the fix of its last issue merged.\n\nfactoryd: SigNoz fires") {
		t.Fatalf("31 minutes after: issues %q", r.fj.issues)
	}
	rows := r.store.alertRows()
	second := rows[1].TaskID
	if len(rows) != 2 || rows[1].Detail["refiled"] != true || rows[0].Detail["refiled"] != false {
		t.Fatalf("rows %+v", rows)
	}
	task2 := r.store.tasks[second]
	task2.Status, task2.FinishedAt = "done", &ended
	r.store.tasks[second] = task2
	r.post(t, "signoz", "hook-pw", sigAlert("firing", "fp1", "demo-staging", firedAt, neverEnds))
	if len(r.fj.issues) != 2 {
		t.Fatalf("a refiled firing was filed again: %d issues", len(r.fj.issues))
	}
}
