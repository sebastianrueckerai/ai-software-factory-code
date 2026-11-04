package factoryd

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A failed dependency's flag keys on the dependency's task id, matched by
// the flag's issue. With no case on it, rung 0 files the dependency again
// through the retry row, as a trip on it does: one case, resolved by
// factoryd on rung 0 with retry, the replacement queued in its place. The
// card's own dependency_failed case on it, at the human here, is the
// flag's case: nothing opens and nothing is retried.
func TestMonitorRetriesAFailedDependency(t *testing.T) {
	for name, tripped := range map[string]bool{"no case": false, "the trip's case": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, store, _ := outsidePlanRig(t)
			now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
			r.now = func() time.Time { return now }
			b := store.tasks["t-b"]
			b.CreatedAt = now.Add(-time.Hour)
			store.tasks["t-b"] = b
			if _, err := store.Record(ctx, Event{Repository: b.Repository, TaskID: "t-b", Kind: EventCardCreated,
				Detail: map[string]any{"budget_eur": b.BudgetEUR}, At: b.CreatedAt}); err != nil {
				t.Fatal(err)
			}
			if tripped {
				if _, _, err := store.OpenCase(ctx, Case{TaskID: "t-b", Repository: b.Repository, Kind: CaseDependencyFailed, StateKey: "t-a",
					Status: CaseAtHuman, Problem: "dependency #5 failed"}); err != nil {
					t.Fatal(err)
				}
			}
			m := &Monitor{Store: store, Runner: r, Policy: func() policy.Tree { return r.cfg.Policy().Tree }, Now: r.now}
			if err := m.card(ctx, "t-b"); err != nil {
				t.Fatalf("card: %v", err)
			}
			cases, _ := store.CasesOf(ctx, "t-b")
			again := RetryID("t-a")
			if tripped {
				if _, retried := store.tasks[again]; len(cases) != 1 || cases[0].Kind != CaseDependencyFailed || retried {
					t.Fatalf("t-b's cases = %+v, t-a retried %v; want the trip's case alone and no retry", cases, retried)
				}
				return
			}
			retry := &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}
			if len(cases) != 1 || cases[0].Kind != CaseMonitorFlag || cases[0].StateKey != "dependency_failed|t-a" || cases[0].Status != CaseResolved ||
				!reflect.DeepEqual(cases[0].Resolution, retry) {
				t.Fatalf("t-b's cases = %+v, want dependency_failed|t-a resolved by factoryd on rung 0 with retry of t-a", cases)
			}
			if n := store.tasks[again]; n.Status != "queued" || !slices.Equal(store.deps["t-b"], []string{again}) {
				t.Fatalf("replacement %+v, t-b's deps %v; want t-b waiting on the queued replacement", n, store.deps["t-b"])
			}
		})
	}
}
