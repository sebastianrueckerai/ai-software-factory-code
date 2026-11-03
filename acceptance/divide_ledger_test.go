package acceptance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// TestDivideConquerLedgerIntegrity holds the ledger's integrity contract
// under the panels' own producers: the spend writer's settlement and the
// Runner's session starts and gate readings write the rows the panels
// count; a retried or replayed write leaves the one row its identity
// keeps, a failed write is the caller's error. (The ReleaseKeeper's hold
// facts stay open work for a later card.)
func TestDivideConquerLedgerIntegrity(t *testing.T) {
	ctx := context.Background()

	// the spend writer's settlement: one row per card however often it
	// runs again, the row the economics panels read.
	t.Run("settlement books once", func(t *testing.T) {
		s, db := divideStore(t)
		at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
		accEconCard(t, s, "t-ledger-1", "small", 100, 3, 411, at)
		accEconCard(t, s, "t-ledger-2", "small", 300, 5, 412, at)
		if again, _ := s.SettleSpendFinal(ctx, "t-ledger-1", 3); again {
			t.Error("retried settlement booked again")
		}
		var rows int
		if err := db.QueryRow(`SELECT count(*) FROM ledger`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Errorf("ledger holds %d rows, want the two settled cards' alone", rows)
		}
		holds(t, newDivideRig(t, s).page(), []string{"€0.0200", "2 cards"}, nil)
	})

	// SessionAnswered writes the session_started row the sessions panel
	// counts: a reattached run replays it, a failed row is the caller's
	// error, the retry lands.
	t.Run("session starts once", func(t *testing.T) {
		ledger := &accLedger{task: factoryd.Task{ID: "t-sess-1", Repository: "sf/code", IssueNumber: 71}}
		r := newLedgerRunner(t, ledger)
		in := factoryd.SessionAnswerInput{TaskID: "t-sess-1", SessionID: "s-1", Model: "factory/small", Attempt: 1}
		for range 2 {
			if ok, err := r.SessionAnswered(ctx, in); err != nil || !ok {
				t.Fatalf("SessionAnswered = (%v, %v), want (true, nil)", ok, err)
			}
		}
		ledger.failNext = errors.New("the store is gone")
		retry := factoryd.SessionAnswerInput{TaskID: "t-sess-1", SessionID: "s-2", Model: "factory/small"}
		if _, err := r.SessionAnswered(ctx, retry); err == nil {
			t.Error("a failed row is the caller's error, not a log line")
		}
		if _, err := r.SessionAnswered(ctx, retry); err != nil {
			t.Fatal(err)
		}
		rows := ledger.rowsOf(factoryd.EventSessionStarted)
		if len(rows) != 2 {
			t.Fatalf("%d session_started rows, want the two sessions' alone", len(rows))
		}
		for _, e := range rows {
			if e.Repository != "sf/code" || e.TaskID != "t-sess-1" {
				t.Errorf("row of %s on %s: the panel counts starts per card", e.TaskID, e.Repository)
			}
		}
	})

	// AutoMergeNote writes the gate_head reading the size-bound panel
	// counts: one row per head, a replay leaves the first standing.
	t.Run("gate readings keep the first", func(t *testing.T) {
		ledger := &accLedger{task: factoryd.Task{ID: "t-gate-1", Repository: "sf/code", IssueNumber: 72,
			Size: "small", Usage: factoryd.TaskUsage{ExtraLines: 10}}}
		r := newLedgerRunner(t, ledger)
		note := func(head string, insertions int) error {
			g := factoryd.GateResults{Policy: factoryd.PolicyGate{Class: "normal", Outcome: policy.OutcomeAuto,
				MergeRule: "auto", LargeLines: 400, PolicyVersion: 7, Files: 3, Insertions: insertions},
				Reproduction: factoryd.ReproResult{Command: "make test"}}
			return r.AutoMergeNote(ctx, "t-gate-1", 7, head, g, factoryd.Review{Verdict: "approve", Summary: "green"})
		}
		for _, h := range [][2]any{{"head-a", 200}, {"head-a", 999}, {"head-b", 350}} {
			if err := note(h[0].(string), h[1].(int)); err != nil {
				t.Fatal(err)
			}
		}
		rows := ledger.rowsOf(factoryd.EventGateHead)
		if len(rows) != 2 {
			t.Fatalf("%d gate_head rows, want one per head", len(rows))
		}
		if rows[0].Detail["insertions"] != 200 || rows[0].IdentityKey == "" {
			t.Errorf("the first reading of a head must stand: detail %v identity %q", rows[0].Detail, rows[0].IdentityKey)
		}
		if rows[0].Detail["gate"] != factoryd.GateMerge || rows[0].Detail["size"] != "small" {
			t.Errorf("the size-bound panel needs gate, bound and size on the row: %v", rows[0].Detail)
		}
	})

}

// accLedger is the events table as PostgreSQL enforces it: a row whose
// identity another row already holds drops without an error (PGStore's
// ON CONFLICT DO NOTHING). One write can be made to fail.
type accLedger struct {
	factoryd.Store
	mu       sync.Mutex
	rows     []factoryd.Event
	failNext error
	task     factoryd.Task
}

func (s *accLedger) Record(_ context.Context, e factoryd.Event) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return 0, err
	}
	for _, held := range s.rows {
		if held.IdentityKey != "" && held.IdentityKey == e.IdentityKey {
			return 0, nil
		}
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	s.rows = append(s.rows, e)
	return int64(len(s.rows)), nil
}

func (s *accLedger) rowsOf(kind string) []factoryd.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []factoryd.Event
	for _, e := range s.rows {
		if e.Kind == kind {
			rows = append(rows, e)
		}
	}
	return rows
}

func (s *accLedger) Task(context.Context, string) (factoryd.Task, bool, error) {
	return s.task, true, nil
}

func (s *accLedger) ModelAnswered(context.Context, string, time.Time) (bool, error) {
	return true, nil
}

// accObjects is the object store the timeline appends its steps to.
type accObjects struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (o *accObjects) Put(_ context.Context, key string, body []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[key] = body
	return nil
}

func (o *accObjects) Get(_ context.Context, key string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.files[key], nil
}

func (o *accObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.files, key)
	return nil
}

// accLeases and accWorkflows stand for collaborators NewRunner insists
// on; no fact this test drives touches them.
type accLeases struct{}

func (accLeases) LiveAddresses(context.Context) (map[string][]string, error) { return nil, nil }
func (accLeases) ClaimCommissioned(context.Context) ([]cloudres.Commissioned, error) {
	return nil, nil
}

type accWorkflows struct{}

func (accWorkflows) Start(context.Context, string) error { return nil }
func (accWorkflows) Signal(context.Context, string, string, factoryd.TaskSignal) error {
	return nil
}
func (accWorkflows) Reopen(context.Context, string, string, factoryd.TaskSignal) error {
	return nil
}
func (accWorkflows) StartRoutineRun(context.Context, factoryd.RoutineRunInput) error { return nil }

// accPolicyTree is the tree the runner validates, its limits the agent
// time the supervisor's wall clock needs.
func accPolicyTree(t *testing.T) policy.Tree {
	t.Helper()
	files := map[string][]byte{"policy.yaml": []byte(policy.Header("policy.yaml") + `
version: 1
budget: {task_default_eur: 2, task_max_eur: 5, day_eur: 20, month_eur: 50}
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder: {session: code, models: [small, big], tools: {}, review_model: big, supervisor_model: supervisor}
prices:
  max_output_tokens: {small: 32768, big: 32768, supervisor: 32768}
risk:
  classes: [{name: normal, outcome: auto}]
  rules: []
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`)}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// newLedgerRunner wires the real Runner over the fake ledger; the
// timeline records into the ledger itself.
func newLedgerRunner(t *testing.T, ledger *accLedger) *factoryd.Runner {
	t.Helper()
	objects := &accObjects{files: map[string][]byte{}}
	version := policy.Version{ID: 7, Commit: "c7", Tree: accPolicyTree(t)}
	cfg := factoryd.RunnerConfig{
		AgentNamespacePrefix: "factory-agents-", WorkerImage: "reg/worker@sha256:abc",
		SandboxBaseImage: "reg/sandbox-base@sha256:bcd", RegistryHost: "store.test", FactoryNamespace: "factory",
		GatewayURL: "http://gateway.factory.svc.cluster.local:8081", GitBaseURL: "http://forgejo.forgejo.svc.cluster.local:3000",
		Models:       factoryd.ModelLadder{Start: "factory/small", Stronger: "factory/big"},
		AnswerTokens: 8192, HeadroomTokens: 16384, ChecksPoll: time.Millisecond,
		EgressCanary: "example.com:443", EgressDirectIP: "192.0.2.7:443",
		Supervisor:    factoryd.SupervisorConfig{NoProgressAfter: time.Minute, RepetitionEvents: 24},
		Policy:        func() policy.Version { return version },
		PolicyVersion: func(context.Context, int64) (policy.Version, bool, error) { return version, true, nil },
		Leases:        accLeases{}, Gates: &factoryd.GateWatch{}, Workflows: accWorkflows{},
		LastFailure: func(context.Context, string, string) (string, error) { return "", nil },
		Timeline:    &factoryd.Timeline{Objects: objects, Events: ledger, Store: ledger},
	}
	r, err := factoryd.NewRunner(cfg, ledger, nil, nil, nil, objects, &factoryd.SupervisorModel{
		GatewayURL: "http://127.0.0.1:1", Token: "gw-token",
		Budget: factoryd.TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
