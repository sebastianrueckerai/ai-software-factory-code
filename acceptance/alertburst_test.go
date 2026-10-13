// Package acceptance holds the plan's end-to-end acceptance tests: each
// drives the shipped types (internal/factoryd) through the seam the spec
// names — SigNoz's webhook, the real Temporal activities and the real
// task workflow — with nothing of factoryd's own logic stubbed.
package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"go.temporal.io/sdk/activity"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// TestCIQueueAlertRequiresSustainedHighQueue (roadmap 181, Stage 18 item
// 3.12): the "CI queue p95 above 5 minutes" rule fires only when the queue
// time stayed high over its whole evaluation window — matchType 2 ("all
// the times") with requireMinPoints and a full window's 15 points — so one
// spike at 555 s files no card.
func TestCIQueueAlertRequiresSustainedHighQueue(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "k8s", "alerts", "ci-queue-p95.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rule struct {
		Alert     string `json:"alert"`
		Condition struct {
			RequireMinPoints  bool `json:"requireMinPoints"`
			RequiredNumPoints int  `json:"requiredNumPoints"`
			Thresholds        struct {
				Spec []struct {
					Name      string `json:"name"`
					Target    int    `json:"target"`
					MatchType string `json:"matchType"`
				} `json:"spec"`
			} `json:"thresholds"`
		} `json:"condition"`
		Evaluation struct {
			Spec struct {
				EvalWindow string `json:"evalWindow"`
			} `json:"spec"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal(data, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Alert != "CI queue p95 above 5 minutes" {
		t.Fatalf("alert %q", rule.Alert)
	}
	if len(rule.Condition.Thresholds.Spec) != 1 {
		t.Fatalf("%d thresholds, want the warning one alone", len(rule.Condition.Thresholds.Spec))
	}
	th := rule.Condition.Thresholds.Spec[0]
	if th.MatchType != "2" {
		t.Errorf("matchType %q, want \"2\" (all the times)", th.MatchType)
	}
	if !rule.Condition.RequireMinPoints {
		t.Errorf("requireMinPoints false, want true")
	}
	if rule.Condition.RequiredNumPoints != 15 {
		t.Errorf("requiredNumPoints %d, want 15", rule.Condition.RequiredNumPoints)
	}
	if th.Name != "warning" || th.Target != 300 {
		t.Errorf("threshold %q target %d, want warning at 300", th.Name, th.Target)
	}
	if w, err := time.ParseDuration(rule.Evaluation.Spec.EvalWindow); err != nil || w != 15*time.Minute {
		t.Errorf("evalWindow %q, want 15m (%v)", rule.Evaluation.Spec.EvalWindow, err)
	}
}

// TestCapacityAlertRoutesToHuman (roadmap 181, Stage 18 item 3.12): an
// alert whose rule carries the capacity label files a human card even on a
// project with agents — a coding agent can only raise a limit, the
// decision needs evidence. The webhook is posted as SigNoz posts it; the
// issue, its labels and the task row are read back from the intake's own
// Forgejo and store.
func TestCapacityAlertRoutesToHuman(t *testing.T) {
	rig := newIntakeRig(t)
	rig.post(t, map[string]string{"alertname": "CI queue p95 above 5 minutes", "namespace": "demo-staging",
		"severity": "warning", "service": "factoryd", "capacity": "true"}, "fp-capacity")

	if len(rig.fj.issues) != 1 {
		t.Fatalf("issues %d, want 1", len(rig.fj.issues))
	}
	if want := "alert: CI queue p95 above 5 minutes on factoryd in demo-staging"; !strings.HasPrefix(rig.fj.issues[0], want+"\n") {
		t.Fatalf("issue\n%s\nwant the title %q", rig.fj.issues[0], want)
	}
	if got := rig.fj.labels[1]; !slices.Equal(got, []string{"bug", "human", "state/waiting-human"}) {
		t.Fatalf("issue labels %v, want [bug human state/waiting-human]", got)
	}
	row := rig.store.alertRowByFingerprint("fp-capacity")
	task, ok := rig.store.taskByID(row.TaskID)
	if !ok {
		t.Fatalf("the alert row names no task: %+v", row)
	}
	if task.Kind != "human" {
		t.Errorf("task kind %q, want human", task.Kind)
	}
	if task.Status != "waiting-human" {
		t.Errorf("task status %q, want waiting-human", task.Status)
	}
	if task.Source != factoryd.SourceAlert || task.AlertFingerprint != "fp-capacity" {
		t.Errorf("task source %q fingerprint %q, want alert and fp-capacity", task.Source, task.AlertFingerprint)
	}
	if started := rig.flows.startedTasks(); len(started) != 0 {
		t.Fatalf("a human card started workflows %v, want none", started)
	}
}

// TestAlertPreCheckClosesTransientBurst (roadmap 181, Stage 18 item 3.12):
// the burst came and went. The webhook files its card on a project with
// agents, the card's workflow starts, and before its first model call the
// real pre-check activity asks the real SigNoz client, which answers the
// fingerprint resolved: the workflow closes the card itself, failed, and
// the session that would have called the model never runs.
func TestAlertPreCheckClosesTransientBurst(t *testing.T) {
	signozCalls := 0
	signoz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v2/sessions/context"):
			io.WriteString(w, `{"status":"success","data":{"orgs":[{"id":"org-1"}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/sessions/email_password":
			io.WriteString(w, `{"status":"success","data":{"accessToken":"tok"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/alerts":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			signozCalls++
			out := map[string]any{"status": "success", "data": []map[string]any{
				{"status": map[string]string{"state": "active"},
					"labels": map[string]string{"alertname": "gateway restarts"}, "startsAt": firedAt},
				// this card's alert: resolved again, the burst is over
				{"status": map[string]string{"state": "resolved"}, "fingerprint": "fp-burst",
					"labels": map[string]string{"alertname": "CI queue p95 above 5 minutes"}, "startsAt": firedAt},
			}}
			if err := json.NewEncoder(w).Encode(out); err != nil {
				t.Errorf("signoz answer: %v", err)
			}
		default:
			t.Errorf("unexpected signoz call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(signoz.Close)

	rig := newIntakeRig(t)
	// a project with agents: this burst does file a card, and the intake
	// starts its workflow.
	rig.projects.m["demo-app"] = demoProjectWithAgents()
	rig.in.Projects = rig.projects
	sessions := 0
	rig.flows.onStart = func(taskID string) { sessions++; runCardWorkflow(t, rig, taskID, signoz.URL) }

	rig.post(t, map[string]string{"alertname": "CI queue p95 above 5 minutes", "namespace": "demo-staging",
		"severity": "warning", "service": "factoryd"}, "fp-burst")

	row := rig.store.alertRowByFingerprint("fp-burst")
	task, ok := rig.store.taskByID(row.TaskID)
	if !ok || task.Kind != "agent" || task.Source != factoryd.SourceAlert {
		t.Fatalf("the burst filed no agent card: row %+v task %+v (%v)", row, task, ok)
	}
	if sessions != 1 {
		t.Fatalf("the card ran %d workflow(s), want the one the intake started", sessions)
	}
	if signozCalls == 0 {
		t.Fatal("the pre-check never asked SigNoz")
	}
	if final := rig.store.statusOf(task.ID); final != "failed" {
		t.Fatalf("the task row is %q, want failed", final)
	}
	if got := rig.store.alertRowByFingerprint("fp-burst"); got.Verdict != "" {
		t.Fatalf("the alert row's verdict %q, want open: a pre-check close judged nothing, no model spent", got.Verdict)
	}
}

const firedAt = "2026-10-12T10:00:00Z"

// runCardWorkflow runs the shipped TaskWorkflow against the shipped
// activities of rig, wired to a live-shaped SigNoz at signozURL, and
// reports how many sessions (model calls) the card ran.
func runCardWorkflow(t *testing.T, rig *intakeRig, taskID, signozURL string) int {
	t.Helper()
	deps := newCardDeps(t, rig, signozURL)
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(deps.runner.EnsureIssue, activity.RegisterOptions{Name: factoryd.ActivityEnsureIssue})
	env.RegisterActivityWithOptions(deps.runner.SetState, activity.RegisterOptions{Name: factoryd.ActivitySetState})
	env.RegisterActivityWithOptions(deps.runner.Reconcile, activity.RegisterOptions{Name: factoryd.ActivityReconcile})
	env.RegisterActivityWithOptions(deps.runner.RecordStep, activity.RegisterOptions{Name: factoryd.ActivityRecordStep})
	env.RegisterActivityWithOptions(deps.runner.Finish, activity.RegisterOptions{Name: factoryd.ActivityFinish})
	env.RegisterActivityWithOptions(deps.runner.TaskConfig, activity.RegisterOptions{Name: factoryd.ActivityTaskConfig})
	env.RegisterActivityWithOptions(deps.runner.BranchHead, activity.RegisterOptions{Name: factoryd.ActivityBranchHead})
	env.RegisterActivityWithOptions(deps.runner.Dependencies, activity.RegisterOptions{Name: factoryd.ActivityDependencies})
	env.RegisterActivityWithOptions(deps.runner.EnterQueue, activity.RegisterOptions{Name: factoryd.ActivityEnterQueue})
	env.RegisterActivityWithOptions(deps.runner.LeaveQueue, activity.RegisterOptions{Name: factoryd.ActivityLeaveQueue})
	env.RegisterActivityWithOptions(deps.runner.SyncWithMain, activity.RegisterOptions{Name: factoryd.ActivitySyncWithMain})
	env.RegisterActivityWithOptions(deps.runner.PullRequestMerged, activity.RegisterOptions{Name: factoryd.ActivityPullRequestMerged})
	env.RegisterActivityWithOptions(deps.runner.AlertPreCheck, activity.RegisterOptions{Name: factoryd.ActivityAlertPreCheck})
	env.RegisterActivityWithOptions(func(ctx context.Context, in factoryd.SessionInput) (factoryd.SessionResult, error) {
		deps.sessionRuns++
		return factoryd.SessionResult{}, nil
	}, activity.RegisterOptions{Name: factoryd.ActivityRunSession})
	env.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: deps.runner.WorkflowConfig}.Run,
		workflow.RegisterOptions{Name: "TaskWorkflow"})

	env.ExecuteWorkflow("TaskWorkflow", taskID)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the card's workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("the card's workflow: %v", err)
	}
	var final string
	if err := env.GetWorkflowResult(&final); err != nil {
		t.Fatal(err)
	}
	if final != "failed" {
		t.Fatalf("the card ended %q, want failed", final)
	}
	if deps.sessionRuns != 0 {
		t.Fatalf("sessions %d, want none: the card closed itself before its first model call", deps.sessionRuns)
	}
	return deps.sessionRuns
}

// cardDeps is the runner behind one card's workflow: the shipped activities
// on the intake's own store, Forgejo and object store, with SigNoz wired as
// the pre-check's source.
type cardDeps struct {
	runner      *factoryd.Runner
	sessionRuns int
}

func newCardDeps(t *testing.T, rig *intakeRig, signozURL string) *cardDeps {
	t.Helper()
	d := &cardDeps{}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL:   "http://factory-gateway.factory.svc.cluster.local:8081",
		GitBaseURL:   "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		Supervisor: factoryd.SupervisorConfig{NoProgressAfter: time.Minute, RepetitionEvents: 24},
		Policy:     acceptancePolicyVersion,
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			v := acceptancePolicyVersion()
			return v, id == v.ID, nil
		},
		Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   rig.flows,
		Alerts:      &factoryd.SigNoz{BaseURL: signozURL, Email: "root@example.com", Password: "pw"},
	}
	forgejo := rig.forgejoClient()
	completion := &factoryd.Completion{Store: rig.store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: rig.litellm.URL, MasterKey: "mk"}}
	runner, err := factoryd.NewRunner(cfg, rig.store, &acceptanceKube{}, forgejo, completion, rig.objects,
		&factoryd.SupervisorModel{GatewayURL: rig.gateway.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	d.runner = runner
	return d
}

// acceptancePolicyVersion is the configuration version a card starts under:
// the maintenance process with a coder class on lane ready.
func acceptancePolicyVersion() policy.Version {
	return policy.Version{ID: 7, Commit: "c7", Tree: policy.Tree{
		Document: policy.Document{
			Budget: acceptanceBudget(),
			Limits: policy.Limits{Sessions: 3, AgentTime: 20 * time.Minute, SupervisorCalls: 3, SupervisorTimeout: 6 * time.Minute, StrongerAfter: 1},
			Classes: map[string]policy.Class{policy.ClassCoder: {Session: policy.SessionCode,
				Models: []string{"factory/coding-cheap", "factory/coding-frontier"}}},
		},
		Processes: map[string]policy.Process{"maintenance": {
			Lanes: []policy.Lane{{Name: policy.LaneBacklog, WriteUp: policy.ClassAnalyzer},
				{Name: policy.LaneReady, Class: policy.ClassCoder, MovedBy: policy.MovedByWriters}},
			Stages: []policy.Stage{
				{Name: "screening", OnFindings: "human", Enabled: true, When: "outsider"},
				{Name: "checks", OnFindings: "agent", Enabled: true},
				{Name: "policy", OnFindings: "fail", Enabled: true},
				{Name: "migration", OnFindings: "human", Enabled: true},
				{Name: "reproduction", OnFindings: "agent", Enabled: true},
				{Name: "review", OnFindings: "agent", Enabled: true},
				{Name: "approval", OnFindings: "human", Enabled: true},
				{Name: "merge", OnFindings: "human", Enabled: true},
			},
		}},
		Projects: map[string]policy.ProjectConfig{"demo-app": {Repository: "demo-app/demo-app", Process: "maintenance"}},
	}}
}

func acceptanceBudget() policy.Budget {
	return policy.Budget{TaskDefaultEUR: 1.5, TaskMaxEUR: 4, DayEUR: 20, MonthEUR: 50}
}

// acceptanceLeases leases nothing: no commissioned server joins a card's
// sandbox egress here.
type acceptanceLeases struct{}

func (acceptanceLeases) LiveAddresses(context.Context) (map[string][]string, error) {
	return map[string][]string{}, nil
}

func (acceptanceLeases) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	return nil, nil
}

// acceptanceKube stands where the cluster does: a card that never runs a
// session creates no namespace, so deleting none is all it must answer.
type acceptanceKube struct{ factoryd.Kube }

func (*acceptanceKube) DeleteNamespace(context.Context, string) error { return factoryd.ErrNotFound }

func (*acceptanceKube) PersistentVolumes(context.Context) ([]factoryd.PersistentVolume, error) {
	return nil, nil
}

func (*acceptanceKube) DeletePersistentVolume(context.Context, string) error { return nil }

// intakeRig is the alert intake on its own backends, which the card's
// workflow then shares: one store, one Forgejo, one object store.
type intakeRig struct {
	in       *factoryd.AlertIntake
	store    *acceptanceStore
	fj       *stubForgejo
	ns       acceptanceNamespaces
	projects *memProjectBook
	objects  *stubObjects
	flows    *acceptanceWorkflows
	gateway  *httptest.Server
	litellm  *httptest.Server
}

func newIntakeRig(t *testing.T) *intakeRig {
	t.Helper()
	fj := &stubForgejo{labels: map[int64][]string{}}
	srv := fj.server(t)
	t.Cleanup(srv.Close)
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(quiet.Close)
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list") {
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/key/block" {
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected litellm call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(litellm.Close)
	r := &intakeRig{store: newAcceptanceStore(), fj: fj, objects: &stubObjects{data: map[string][]byte{}},
		flows: &acceptanceWorkflows{}, gateway: quiet, litellm: litellm}
	r.ns = acceptanceNamespaces{"demo-staging": "demo-app", "demo-prod": "demo-app"}
	r.projects = &memProjectBook{m: map[string]factoryd.Project{"demo-app": demoProjectWithoutAgents()}}
	r.in = &factoryd.AlertIntake{User: "signoz", Password: "hook-pw", Namespaces: r.ns, Projects: r.projects,
		Store: r.store, Forgejo: r.forgejoClient(), Workflows: r.flows,
		Timeline: &factoryd.Timeline{Objects: r.objects}, Budget: acceptanceBudget}
	return r
}

func (r *intakeRig) forgejoClient() *factoryd.Forgejo {
	return &factoryd.Forgejo{BaseURL: r.fj.baseURL(), BotUser: "factory-agent", BotPassword: "bot-pw"}
}

// post delivers one firing SigNoz alert as its webhook sends it.
func (r *intakeRig) post(t *testing.T, labels map[string]string, fingerprint string) {
	t.Helper()
	msg, err := json.Marshal(map[string]any{"version": "4", "status": "firing", "receiver": "factoryd",
		"alerts": []map[string]any{{"status": "firing", "labels": labels,
			"annotations": map[string]string{"value": "555"}, "startsAt": firedAt,
			"endsAt": "0001-01-01T00:00:00Z", "fingerprint": fingerprint}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/signoz", strings.NewReader(string(msg)))
	req.SetBasicAuth("signoz", "hook-pw")
	rec := httptest.NewRecorder()
	r.in.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status %d: %s", rec.Code, rec.Body)
	}
}

func demoProjectWithAgents() factoryd.Project {
	return factoryd.Project{Name: "demo-app", Repository: "demo-app/demo-app", Objects: "factory",
		Agents: "coder", File: true, GitToken: "git-tok", Namespaces: []string{"demo-staging", "demo-prod"},
		Process: "maintenance", Lanes: []policy.Lane{
			{Name: policy.LaneBacklog, WriteUp: policy.ClassAnalyzer},
			{Name: policy.LaneReady, Class: policy.ClassCoder, MovedBy: policy.MovedByWriters}}}
}

func demoProjectWithoutAgents() factoryd.Project {
	p := demoProjectWithAgents()
	p.Agents = policy.AgentsNone
	return p
}

type acceptanceNamespaces map[string]string

func (m acceptanceNamespaces) NamespaceLabel(_ context.Context, name, key string) (string, error) {
	if key != factoryd.ProjectLabel {
		return "", fmt.Errorf("label %q, want %q", key, factoryd.ProjectLabel)
	}
	return m[name], nil
}

// memProjectBook is the project rows the intake reads.
type memProjectBook struct {
	mu sync.Mutex
	m  map[string]factoryd.Project
}

func (b *memProjectBook) Project(_ context.Context, name string) (factoryd.Project, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.m[name]
	return p, ok, nil
}

type acceptanceWorkflows struct {
	mu      sync.Mutex
	started []string
	onStart func(taskID string)
}

func (w *acceptanceWorkflows) Start(_ context.Context, taskID string) error {
	w.mu.Lock()
	w.started = append(w.started, taskID)
	on := w.onStart
	w.mu.Unlock()
	if on != nil {
		on(taskID)
	}
	return nil
}

func (w *acceptanceWorkflows) Signal(_ context.Context, taskID, name string, sig factoryd.TaskSignal) error {
	return nil
}

func (w *acceptanceWorkflows) startedTasks() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.started...)
}
