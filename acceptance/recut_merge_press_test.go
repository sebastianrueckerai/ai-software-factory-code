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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
)

// The re-cut card that reaches its merge press, at the acceptance level:
// a plan whose criteria are unsettled, its re-cut integration card in
// review holding pull request 12, open and green through a fake Forgejo —
// the read the criteria gate does at the merge press. The plan's replan
// press and the card's own check-in cut (ReplanChild) both leave the card
// alive with its pull request open and the merge press reachable; only a
// person's replan naming the card closes it. Kept in step with the
// workflow-level TestRecutCardReachesMergePress in internal/factoryd.

// recutOpenPR is the re-cut integration card's open pull request 12 at
// head c7.
const recutOpenPR = `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t-rc","sha":"c7","repo":{"full_name":"factory-agent/demo-app"}}}`

// recutChangelessPR is pull request 12 in the changeless shape: its head
// c7 is also its base's sha, so the branch carries no change over main —
// and the pull request is still owed, for it is the criteria gate's
// trigger.
const recutChangelessPR = `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t-rc","sha":"c7","repo":{"full_name":"factory-agent/demo-app"}},"base":{"ref":"main","sha":"c7"}}`

// recutPlan is a plan whose criteria are unsettled: a test criterion, a
// human one and an invariant, over a foundation card and the integration
// card that runs the criteria gate at its merge press.
func recutPlan() factoryd.Plan {
	return factoryd.Plan{
		Summary: "the plan",
		Criteria: []factoryd.PlanCriterion{
			{ID: "c1", Kind: factoryd.CriterionTest, Claim: "a link is stored", Inputs: "POST /api/links with a url", Expected: "201"},
			{ID: "c2", Kind: factoryd.CriterionHuman, Claim: "the feature reads well"},
			{ID: "c3", Kind: factoryd.CriterionInvariant, Suite: "./..."},
		},
		Cards: []factoryd.PlanCard{
			{Key: "foundation", Role: "feature", Title: "Foundation", Directories: []string{"a"}},
			{Key: "integration", Role: factoryd.RoleIntegration, Title: "Integration", Directories: []string{"a"}},
		},
	}
}

// recutForgejo serves the reads the replan paths make: pull request 12
// open at green head c7, the issues open, no timeline events. closed
// holds the pull requests a PATCH ends.
type recutForgejo struct {
	mu     sync.Mutex
	body   string  // the body GET /pulls/12 answers
	down   bool    // GET /pulls/12 answers 500
	closed []int64 // PATCH /pulls/{n} closes
}

// prClosed says whether a replan path closed pull request n.
func (g *recutForgejo) prClosed(n int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Contains(g.closed, n)
}

func (g *recutForgejo) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		const repo = "/api/v1/repos/demo-app/demo-app"
		p := r.URL.Path
		switch {
		case g.down && p == repo+"/pulls/12":
			http.Error(w, "down", http.StatusInternalServerError)
		case r.Method == http.MethodGet && p == repo+"/pulls/12":
			io.WriteString(w, g.body)
		case r.Method == http.MethodPatch && strings.HasPrefix(p, repo+"/pulls/"):
			var n int64
			fmt.Sscanf(strings.TrimPrefix(p, repo+"/pulls/"), "%d", &n)
			g.closed = append(g.closed, n)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && p == repo+"/commits/c7/status":
			io.WriteString(w, `{"state":"success","total_count":1,"statuses":[{"status":"success","context":"pr / test"}]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/issues/") && strings.HasSuffix(p, "/timeline"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(p, "/labels") || strings.HasSuffix(p, "/dependencies"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, repo+"/issues/"):
			io.WriteString(w, `{"state":"open"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v1/users/factory-agent/tokens"):
			io.WriteString(w, `[]`)
		case strings.Contains(p, "/git/commits/"):
			io.WriteString(w, `{"commit":{"committer":{"email":"factory-agent@factory.local"}},"parents":[]}`)
		default:
			t.Logf("recutForgejo: unexpected %s %s", r.Method, p)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recutRig is the plan p (issue 10, done, one revision of recutPlan
// stored) with its re-cut integration card t-rc (issue 11, revision 1) in
// review holding pull request 12, over a real PostgreSQL store and the
// fake Forgejo. Each call seeds its own database.
func recutRig(t *testing.T, fj *recutForgejo) (*factoryd.Runner, *factoryd.PGStore, *stubObjects) {
	t.Helper()
	ctx := context.Background()
	dsn := accDatabase(t, "acceptance_recut")
	store, err := factoryd.OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected gateway call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(quiet.Close)
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/key/list"):
			io.WriteString(w, `{"keys":[],"total_pages":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/key/block":
		default:
			t.Errorf("unexpected litellm call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(litellm.Close)
	objects := &stubObjects{data: map[string][]byte{}}
	forge := &factoryd.Forgejo{BaseURL: fj.server(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw",
		Seeded: map[string]string{policy.SecretMerge: "9a8b7c6d5e4f30211203f4e5d6c7b8a998877665"}}
	v := acceptancePolicyVersion()
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc", SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL: "http://factory-gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor: factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy:     func() policy.Version { return v },
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			return v, id == v.ID, nil
		},
		Leases: &acceptanceLeases{}, Gates: &factoryd.GateWatch{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &acceptanceWorkflows{},
	}
	completion := &factoryd.Completion{Store: store, Forgejo: forge, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(os.Stderr, nil))))
	runner, err := factoryd.NewRunner(cfg, mergeProject{store}, &acceptanceKube{}, forge, completion, objects,
		&factoryd.SupervisorModel{GatewayURL: quiet.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, factoryd.Task{ID: "p", Kind: "agent", Track: policy.TrackPlan, Source: "webhook",
		Repository: "demo-app/demo-app", IssueNumber: 10, Status: "done", BudgetEUR: 5, Usage: factoryd.TaskUsage{Attempts: 1}}); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(recutPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "p", b, "sha-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, factoryd.Task{ID: "t-rc", Kind: "agent", Track: policy.TrackDirect, Source: factoryd.SourcePlan,
		Parent: "p", Repository: "demo-app/demo-app", PlanKey: "integration", PlanRevision: 1, IssueNumber: 11,
		Status: "in-review", PRNumber: 12, SHA: "c7", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	// insertTask does not carry plan_revision; the card's start records it
	if err := store.SetTaskPlanRevision(ctx, "t-rc", 1); err != nil {
		t.Fatal(err)
	}
	return runner, store, objects
}

// recutTimeline is task id's timeline as the rig's objects hold it.
func recutTimeline(t *testing.T, objects *stubObjects, id string) string {
	t.Helper()
	b, err := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatalf("the timeline of %s: %v", id, err)
	}
	return string(b)
}

// TestRecutCardReachesMergePress: end to end over the real store and the
// fake Forgejo, a re-cut card with an open green pull request reaches its
// merge press — the plan's replan press and the card's own check-in cut
// both leave it alive with the pull request open, no replan_stop strand
// without a revision, and only a person's replan naming the card closes
// it, pressed third on the same run.
func TestRecutCardReachesMergePress(t *testing.T) {
	ctx := context.Background()
	fj := &recutForgejo{body: recutOpenPR}
	r, store, objects := recutRig(t, fj)

	// the plan's replan press spares the gate-trigger card
	note, err := r.Replan(ctx, "p", factoryd.ReplanAsk{By: "sebastian", Comment: "the round changed"})
	if err != nil {
		t.Fatalf("the plan's replan press: %v", err)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the plan's replan answer lacks %q: %s", want, note)
		}
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "in-review" {
		t.Fatalf("the card after the plan's replan = (%s, %v), want still in review", c.Status, err)
	}
	if fj.prClosed(12) {
		t.Fatal("the plan's replan press closed pull request 12")
	}

	// the check-in's cut through ReplanChild is refused the same way, and
	// no replan_stop strand lands on the card
	cut, err := r.ReplanChild(ctx, "t-rc", "the change must be cut smaller")
	if err != nil {
		t.Fatalf("the check-in's cut: %v", err)
	}
	if cut.Reason != "" {
		t.Fatalf("the check-in's cut = %q, want a refusal", cut.Reason)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(cut.Refused, want) {
			t.Fatalf("the cut refusal lacks %q: %s", want, cut.Refused)
		}
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "in-review" {
		t.Fatalf("the card after the check-in's cut = (%s, %v), want still in review", c.Status, err)
	}
	if fj.prClosed(12) {
		t.Fatal("the check-in's cut closed pull request 12")
	}
	if tl := recutTimeline(t, objects, "t-rc"); strings.Contains(tl, "replan_stop") {
		t.Fatalf("the card's timeline holds a replan_stop without the revision that names the change:\n%s", tl)
	}

	// the plan's replan lands before the person's press, as its run would
	// see it through: its planner files the next revision naming the
	// change — the card's key stays in it — the filing ends the pending
	// replan, and the plan ends done. The rig drives the activities, so
	// it stands in for the plan's run here; the person's press then meets
	// the world it would meet, with the first two presses behind it
	b, err := json.Marshal(recutPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "p", b, "sha-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionTask(ctx, "p", []string{"queued"}, "done"); err != nil {
		t.Fatal(err)
	}
	prev, err := objects.Get(ctx, "sessions/p/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(ctx, "sessions/p/timeline.jsonl",
		slices.Concat(prev, []byte(`{"step":"replanned","revision":2}`+"\n"))); err != nil {
		t.Fatal(err)
	}

	// the third press on the same run: a person's replan naming the card
	// still stops it, though the card holds the gate-trigger pull request
	note, err = r.Replan(ctx, "t-rc", factoryd.ReplanAsk{By: "sebastian", Comment: "stop this card for the new round"})
	if err != nil {
		t.Fatalf("the person's replan naming the card: %v", err)
	}
	if !strings.Contains(note, "criteria-gate trigger") {
		t.Fatalf("the person's stop note does not name the gate: %s", note)
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "closed" {
		t.Fatalf("the card after the person's replan = (%s, %v), want closed", c.Status, err)
	}
}

// TestRecutChangelessCardStillOwesItsPR: the changeless shape — a card
// whose branch carries no change over main still owes its pull request,
// for the pull request is the criteria gate's trigger. The replan
// answers name the owed pull request and its merge press exactly as for
// the changed branch, and neither press closes the card or its pull
// request. The statement's own changeless-round sentence is #1997's
// TestCheckinStatementNamesGateTriggerPR, not repeated here.
func TestRecutChangelessCardStillOwesItsPR(t *testing.T) {
	ctx := context.Background()
	fj := &recutForgejo{body: recutChangelessPR}
	r, store, objects := recutRig(t, fj)

	note, err := r.Replan(ctx, "p", factoryd.ReplanAsk{By: "sebastian", Comment: "the round changed"})
	if err != nil {
		t.Fatalf("the plan's replan press: %v", err)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the plan's replan answer on the changeless branch lacks %q: %s", want, note)
		}
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "in-review" {
		t.Fatalf("the changeless card after the plan's replan = (%s, %v), want still in review owing its pull request", c.Status, err)
	}
	if fj.prClosed(12) {
		t.Fatal("the plan's replan press closed the changeless card's pull request")
	}

	cut, err := r.ReplanChild(ctx, "t-rc", "the change must be cut smaller")
	if err != nil {
		t.Fatalf("the check-in's cut: %v", err)
	}
	if cut.Reason != "" {
		t.Fatalf("the check-in's cut = %q, want a refusal", cut.Reason)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(cut.Refused, want) {
			t.Fatalf("the cut refusal on the changeless branch lacks %q: %s", want, cut.Refused)
		}
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "in-review" {
		t.Fatalf("the changeless card after the check-in's cut = (%s, %v), want still in review owing its pull request", c.Status, err)
	}
	if fj.prClosed(12) {
		t.Fatal("the check-in's cut closed the changeless card's pull request")
	}
	if tl := recutTimeline(t, objects, "t-rc"); strings.Contains(tl, "replan_stop") {
		t.Fatalf("the changeless card's timeline holds a replan_stop without the revision that names the change:\n%s", tl)
	}
}

// TestRecutForgeReadRefusesTheReplan: the forge read failing mid-replan
// refuses the replan until the read lands rather than stopping the card —
// the guard's fail direction, kept in step at the acceptance level.
func TestRecutForgeReadRefusesTheReplan(t *testing.T) {
	ctx := context.Background()
	fj := &recutForgejo{body: recutOpenPR}
	r, store, objects := recutRig(t, fj)
	fj.mu.Lock()
	fj.down = true
	fj.mu.Unlock()

	if _, err := r.Replan(ctx, "p", factoryd.ReplanAsk{By: "sebastian", Comment: "the round changed"}); err == nil ||
		!strings.Contains(err.Error(), "pull request #12") {
		t.Fatalf("the plan's replan on a failed read = %v, want the refusal naming the unreadable pull request", err)
	}
	cut, err := r.ReplanChild(ctx, "t-rc", "the change must be cut smaller")
	if err != nil {
		t.Fatalf("the check-in's cut on a failed read: %v", err)
	}
	if cut.Reason != "" || !strings.Contains(cut.Refused, "pull request #12") {
		t.Fatalf("the cut on a failed read = %+v, want a refusal naming the unreadable pull request", cut)
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "in-review" {
		t.Fatalf("the card on a failed read = (%s, %v), want still in review", c.Status, err)
	}
	if tl := recutTimeline(t, objects, "t-rc"); strings.Contains(tl, "replan_stop") {
		t.Fatalf("a failed read left replan_stop steps:\n%s", tl)
	}

	fj.mu.Lock()
	fj.down = false
	fj.mu.Unlock()
	note, err := r.Replan(ctx, "p", factoryd.ReplanAsk{By: "sebastian", Comment: "the round changed"})
	if err != nil || !strings.Contains(note, "pull request #12") || !strings.Contains(note, "merge press") {
		t.Fatalf("the retry = %q, %v; want the spare note naming the pull request and the gate", note, err)
	}
	if c, ok, err := store.Task(ctx, "t-rc"); err != nil || !ok || c.Status != "in-review" {
		t.Fatalf("the card after the retry = (%s, %v), want still in review", c.Status, err)
	}
}
