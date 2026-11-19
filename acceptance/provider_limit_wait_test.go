package acceptance

// The provider-limit wait's acceptance proof at the compiled factoryd: the
// real TaskWorkflow in Temporal's test environment, every activity the
// real runner's over the real store and the git-target Forgejo, the
// session activity alone scripted as the gateway's refusal reaches it.
// Workflow time skips, so the holds to the resets cost no wall clock.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// plWaitStart is the workflow clock's start: every reset the proof's
// sessions name lies after it.
var plWaitStart = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// plWaitWords is the provider's words the proof's refusals carry.
const plWaitWords = "rate limit exceeded until 2026-10-14 18:04:50 (Asia/Singapore)"

// plLimitRefused is a session the gateway refused with the named limit
// resetting at until, as the session activity reads the gateway's audit.
func plLimitRefused(id string, until time.Time) factoryd.SessionResult {
	reason := "provider limit for class coder: reset " + until.Format(time.RFC3339) + ": " + plWaitWords
	return factoryd.SessionResult{Outcome: factoryd.OutcomeProviderRefused, SessionID: id, Status: http.StatusTooManyRequests,
		Reason: reason, Until: until, Words: plWaitWords, Findings: []string{"previous session ended without a pushed commit: " + reason}}
}

// plWaitPolicy is wipPolicy with the coder class's models as the gateway
// names them; moved runs the class on big alone, a later version.
func plWaitPolicy(moved bool) policy.Version {
	v := wipPolicy()
	models := []string{"small", "big"}
	if moved {
		v.ID, v.Commit, models = v.ID+1, "c-moved", []string{"big"}
	}
	c := v.Tree.Document.Classes[policy.ClassCoder]
	c.Models = models
	v.Tree.Document.Classes[policy.ClassCoder] = c
	return v
}

// plWaitRun is one card's run: its end, the sessions it began and when,
// the provider_refused lines its timeline holds and the asks it opened.
type plWaitRun struct {
	final  string
	inputs []factoryd.SessionInput
	began  []time.Time
	waits  []map[string]any
	asks   []string // the provider_refused cases' problems: the six-refusal asks
}

// plWaitEnv is a Temporal test environment whose clock starts at start,
// with the real runner's activities over store, its timelines in objects,
// and TaskWorkflow registered on it: every activity but the session, which
// the caller scripts, behind the gate watch that moves a card's gate as
// factoryd's worker does. move puts plWaitPolicy's moved version in force.
func plWaitEnv(t *testing.T, store *factoryd.PGStore, objects *stubObjects, start time.Time) (env *testsuite.TestWorkflowEnvironment, move func()) {
	t.Helper()
	git := &gitForgejo{base: wipBase}
	srv := git.server(t)
	quiet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
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
	var mu sync.Mutex
	current := plWaitPolicy(false)
	versions := map[int64]policy.Version{current.ID: current}
	forgejo := &factoryd.Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc", SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test",
		PullSecret: "forgejo-registry", FactoryNamespace: "factory",
		GatewayURL: "http://factory-gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo-http.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor: factoryd.SupervisorConfig{RepetitionEvents: 24},
		Policy: func() policy.Version {
			mu.Lock()
			defer mu.Unlock()
			return current
		},
		PolicyVersion: func(_ context.Context, id int64) (policy.Version, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			v, ok := versions[id]
			return v, ok, nil
		},
		Leases:      &acceptanceLeases{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Workflows:   &acceptanceWorkflows{},
	}
	gates := &factoryd.GateWatch{Store: store, Bound: factoryd.PinnedBound(cfg.Policy, cfg.PolicyVersion)}
	cfg.Gates = gates
	completion := &factoryd.Completion{Store: store, Forgejo: forgejo, LiteLLM: &factoryd.LiteLLMAdmin{BaseURL: litellm.URL, MasterKey: "mk"}}
	runner, err := factoryd.NewRunner(cfg, mergeProject{store}, &acceptanceKube{}, forgejo, completion, objects,
		&factoryd.SupervisorModel{GatewayURL: quiet.URL, Token: "gw-token", Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env = suite.NewTestWorkflowEnvironment()
	env.SetStartTime(start)
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{gates}})
	runner.RegisterActivities(skipping{env: env, skip: factoryd.ActivityRunSession})
	env.RegisterWorkflowWithOptions(factoryd.TaskWorkflow{Config: runner.WorkflowConfig}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	return env, func() {
		mu.Lock()
		defer mu.Unlock()
		current = plWaitPolicy(true)
		versions[current.ID] = current
	}
}

// plWaitHolds are the provider_refused lines of card id's timeline in
// objects, oldest first: the holds its run waited.
func plWaitHolds(t *testing.T, objects *stubObjects, id string) []map[string]any {
	t.Helper()
	body, err := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var holds []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("timeline line %q: %v", line, err)
		}
		if m["step"] == factoryd.StepProviderRefused {
			holds = append(holds, m)
		}
	}
	return holds
}

// plWaitCard runs card id's workflow to its end on the store: the sessions
// answer results in order (the last repeats), the policy in force moves to
// the moved version at moveAt (zero: never), signals arrive at their
// workflow times, and preGate replays the run as a history from before the
// provider-freeze gate.
func plWaitCard(t *testing.T, store *factoryd.PGStore, id string, preGate bool, moveAt time.Duration,
	signals map[time.Duration]string, results ...factoryd.SessionResult) plWaitRun {
	t.Helper()
	ctx := context.Background()
	objects := &stubObjects{data: map[string][]byte{}}
	env, move := plWaitEnv(t, store, objects, plWaitStart)
	if err := store.CreateTask(ctx, factoryd.Task{ID: id, Status: "queued", Objective: "wait out the provider's limit", Repository: "demo-app/demo-app",
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: 4, SHA: wipBase, RequiredCapabilities: []string{policy.ClassCoder}}); err != nil {
		t.Fatal(err)
	}

	var run plWaitRun
	session := func(ctx context.Context, in factoryd.SessionInput) (factoryd.SessionResult, error) {
		run.inputs, run.began = append(run.inputs, in), append(run.began, env.Now().UTC())
		return results[min(len(run.inputs), len(results))-1], nil
	}
	env.RegisterActivityWithOptions(session, activity.RegisterOptions{Name: factoryd.ActivityRunSession})
	if preGate {
		env.OnGetVersion("provider-freeze", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	}
	if moveAt > 0 {
		env.RegisterDelayedCallback(move, moveAt-time.Second)
	}
	for at, name := range signals {
		env.RegisterDelayedCallback(func() { env.SignalWorkflow(name, factoryd.TaskSignal{Author: "sebastian"}) }, at)
	}
	env.ExecuteWorkflow("TaskWorkflow", id)
	if !env.IsWorkflowCompleted() {
		t.Fatalf("card %s's workflow did not complete", id)
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("card %s's workflow: %v", id, err)
	}
	if err := env.GetWorkflowResult(&run.final); err != nil {
		t.Fatal(err)
	}
	run.waits = plWaitHolds(t, objects, id)
	cases, err := store.CasesOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Kind == factoryd.CaseProviderRefused {
			reason, _ := c.Facts["reason"].(string)
			run.asks = append(run.asks, reason)
		}
	}
	return run
}

// TestProviderLimitWaitToReset: a session refused with the named limit
// waits to its reset — the provider_refused line's BackoffS the seconds to
// it, no ladder climb, no attempt and no session-cap burn; a policy load
// that leaves the class's model keeps the hold, the session after the
// hold starts at the reset, the next refusal's later reset re-freezes the
// card, and a load whose policy moves the class's model ends the wait
// before that reset. The same refusal on a history from before the gate
// walks today's ladder (2, 4, 8, 16, 30 minutes) and its six-refusal
// escalation. [card_waits_to_reset]
func TestProviderLimitWaitToReset(t *testing.T) {
	store, _ := accReceiptStore(t, "provider_limit_wait")
	at := func(m int) time.Time { return plWaitStart.Add(time.Duration(m) * time.Minute) }

	run := plWaitCard(t, store, "t-limit", false, 35*time.Minute, map[time.Duration]string{
		10 * time.Minute: factoryd.SignalPolicyLoaded, // before the 09:30 reset, the class's model unmoved: the hold keeps
		35 * time.Minute: factoryd.SignalPolicyLoaded, // before the 09:55 reset, the class's model moved: the wait ends
		40 * time.Minute: factoryd.SignalIssueClosed,
	}, plLimitRefused("s-1", at(30)), plLimitRefused("s-2", at(55)), plLimitRefused("s-3", at(80)))
	if run.final != "closed" {
		t.Fatalf("final = %q, want closed", run.final)
	}
	if want := []time.Time{at(0), at(30), at(35)}; !slices.EqualFunc(run.began, want, time.Time.Equal) {
		t.Fatalf("sessions began at %v, want 09:00, the reset at 09:30 and the load at 09:35", run.began)
	}
	for i, in := range run.inputs {
		if in.Attempt != 1 || in.ParentSessionID != "" || in.Sessions != run.inputs[0].Sessions || in.Model != "factory/small" {
			t.Fatalf("session %d = attempt %d, parent %q, cap %d, model %s; want attempt 1, no parent, the first's cap and no climb",
				i+1, in.Attempt, in.ParentSessionID, in.Sessions, in.Model)
		}
	}
	if len(run.waits) != 3 {
		t.Fatalf("provider_refused lines = %v, want the three holds", run.waits)
	}
	for i, want := range []float64{1800, 1500, 2700} {
		if w := run.waits[i]; w["backoff_s"] != want || w["words"] != plWaitWords || w["status"] != float64(429) {
			t.Errorf("hold %d = %v, want backoff_s %v to the reset, the words and status 429", i+1, w, want)
		}
	}
	if len(run.asks) != 0 {
		t.Fatalf("the holds asked %q, want no six-refusal ask", run.asks)
	}

	// the same refusal on a history from before the gate: the ladder and
	// its escalation, as today
	pre := plWaitCard(t, store, "t-pregate", true, 0, map[time.Duration]string{3 * time.Hour: factoryd.SignalIssueClosed},
		plLimitRefused("s-1", at(30)))
	if pre.final != "closed" || len(pre.inputs) != 6 {
		t.Fatalf("a pre-gate run: final %q after %d sessions, want closed after 6", pre.final, len(pre.inputs))
	}
	var backoffs []float64
	for _, w := range pre.waits {
		backoffs = append(backoffs, w["backoff_s"].(float64))
	}
	if !slices.Equal(backoffs, []float64{120, 240, 480, 960, 1800}) {
		t.Fatalf("a pre-gate run's backoffs = %v, want the ladder 120 240 480 960 1800", backoffs)
	}
	if want := []string{"the model provider refused the first call of 6 sessions in a row (status 429): " +
		plLimitRefused("s-1", at(30)).Reason}; !slices.Equal(pre.asks, want) {
		t.Fatalf("a pre-gate run's asks = %q, want the six-refusal ask %q", pre.asks, want)
	}
}
