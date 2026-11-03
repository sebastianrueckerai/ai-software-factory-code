package factoryd

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Outside a plan rung 0 files a failed dependency again, once: card B's
// trip on A runs RetryCard and its case resolves with retry. When A's
// replacement fails too, B's trip keys on the replacement, rung 0 retries
// no retry, and the human is asked.
func TestOpenTripRetriesAFailedDependencyOnce(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	for _, id := range []string{"t-a", "t-b"} {
		c := store.tasks[id]
		c.Source, c.Parent, c.PlanKey = "webhook", "", ""
		store.tasks[id] = c
	}
	first, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})})
	if err != nil {
		t.Fatal(err)
	}
	retry := Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}
	if first.Resolution == nil || !reflect.DeepEqual(*first.Resolution, retry) || first.Comment != 0 || first.Rung != 0 {
		t.Fatalf("first outcome = %+v, want rung 0's retry of t-a", first)
	}
	c, _ := store.CaseByID(ctx, first.CaseID)
	if c.Kind != CaseDependencyFailed || c.StateKey != "t-a" || c.Status != CaseResolved ||
		c.Resolution == nil || c.Resolution.By != FactoryDecider || c.Resolution.Rung != 0 || c.Resolution.Action != ActRetry {
		t.Fatalf("case = %+v, resolution %+v; want t-a's failure resolved by factoryd on rung 0 with retry", c, c.Resolution)
	}
	again := RetryID("t-a")
	n, ok := store.tasks[again]
	if !ok || n.Status != "queued" || !slices.Equal(store.deps["t-b"], []string{again}) || fj.issues[5].state != "closed" {
		t.Fatalf("replacement %+v (found %v), B's deps %v, #5 %s; want B waiting on the queued replacement and #5 closed",
			n, ok, store.deps["t-b"], fj.issues[5].state)
	}

	n.Status = "failed"
	store.tasks[again] = n
	second, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: again, Issue: n.IssueNumber, Status: "failed"})})
	if err != nil {
		t.Fatal(err)
	}
	if second.Resolution != nil || second.Comment == 0 || second.Rung != 3 {
		t.Fatalf("second outcome = %+v, want the human asked", second)
	}
	c, _ = store.CaseByID(ctx, second.CaseID)
	steps, _ := store.CaseSteps(ctx, c.ID)
	if c.StateKey != again || c.Status != CaseAtHuman || len(steps) != 1 || steps[0].Found != "no resolver fits" || steps[0].Outcome != StepHandedUp {
		t.Fatalf("case %+v, steps %+v; want the replacement's failure at the human, rung 0 finding no resolver", c, steps)
	}
	if _, ok := store.tasks[RetryID(again)]; ok {
		t.Fatal("the replacement was retried in turn")
	}
}

// Rung 0's rows on a case's kind and facts, the first that fires winning:
// a green approved head goes to its approval, a red one's checks run once,
// a cap of the day waits, and a failed dependency is retried outside a plan
// and sent to its planner inside one, unless it is a retry itself.
func TestRungZeroRowsFireOnTheirFacts(t *testing.T) {
	room := CaseFacts{SessionCap: 3, AgentTimeCapS: 3600}
	head := func(checks string, approved bool) CaseFacts {
		f := room
		f.Head, f.PR, f.PROpen, f.Checks, f.ReviewApproved = "h2", 9, true, checks, approved
		return f
	}
	failed := func(plan, retryOf string) CaseFacts {
		f := room
		f.Plan, f.Dependencies = plan, []DepFact{{TaskID: "t-a", Issue: 5, Status: "failed", RetryOf: retryOf}}
		return f
	}
	for _, tc := range []struct {
		name string
		kind CaseKind
		f    CaseFacts
		want *Resolution
	}{
		{"green and approved", CaseSessionCap, head("green", true), &Resolution{Action: ActRouteApproval}},
		{"green, not approved", CaseGateStop, head("green", false), nil},
		{"red", CaseBudgetFloor, head("red", false), &Resolution{Action: ActRerunChecks}},
		{"red, run again", CaseCheckinShare, func() CaseFacts { f := head("red", false); f.RerunOnHead = true; return f }(), nil},
		{"green at a stop of no head", CaseAgentEscalate, head("green", true), nil},
		{"the day cap", CaseDayCap, head("green", true), nil},
		{"a failed dependency", CaseDependencyFailed, failed("", ""), &Resolution{Action: ActRetry, Args: map[string]any{"task_id": "t-a"}}},
		{"a failed retry", CaseDependencyFailed, failed("", "t-old"), nil},
		{"a failed dependency in a plan", CaseDependencyFailed, failed("t-plan", ""), &Resolution{Action: ActReplan, Args: map[string]any{"task_id": "t-a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Case{Kind: tc.kind, StateKey: tripKey(tc.kind, "the stop", tc.f.Head, tc.f)}
			var got *Resolution
			for _, row := range rung0 {
				if res, err := row.Fire(context.Background(), nil, c, tc.f); err != nil || res != nil {
					got = res
					break
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rung 0 took %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A trip's state key: the head for a stop at it, the dependency's id for a
// dependency's, else the problem's words.
func TestTripKeyByKind(t *testing.T) {
	f := CaseFacts{Dependencies: []DepFact{{TaskID: "t-x", Status: "running"}, {TaskID: "t-c", Status: "closed"}, {TaskID: "t-r", Status: "failed", Replaced: "t-r2"},
		{TaskID: "t-a", Status: "failed"}}}
	for _, tc := range []struct {
		kind CaseKind
		head string
		want string
	}{
		{CaseSessionCap, "h2", "h2"},
		{CaseGateStop, "h2", "h2"},
		{CaseSessionCap, "", caseStateKey(CaseSessionCap, "the stop", "")},
		{CaseAgentEscalate, "h2", caseStateKey(CaseAgentEscalate, "the stop", "")},
		{CaseDependencyFailed, "h2", "t-a"},
		{CaseDependencyClosed, "h2", "t-c"},
	} {
		if got := tripKey(tc.kind, "the stop", tc.head, f); got != tc.want {
			t.Errorf("tripKey(%s, %q) = %q, want %q", tc.kind, tc.head, got, tc.want)
		}
	}
}

// A row whose action fails part way hands the case up with why: the close
// of the failed card's issue fails after the replacement is filed, and the
// human is asked.
func TestOpenTripHandsUpARowThatFails(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	for _, id := range []string{"t-a", "t-b"} {
		c := store.tasks[id]
		c.Source, c.Parent, c.PlanKey = "webhook", "", ""
		store.tasks[id] = c
	}
	fj.failCall = "PATCH issues/5"
	out, err := r.OpenTrip(ctx, TripInput{TaskID: "t-b", Reason: depFailedReason(Dependency{TaskID: "t-a", Issue: 5, Status: "failed"})})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := store.CaseByID(ctx, out.CaseID)
	steps, _ := store.CaseSteps(ctx, c.ID)
	if out.Resolution != nil || out.Comment == 0 || c.Status != CaseAtHuman || len(steps) != 1 ||
		!strings.HasPrefix(steps[0].Found, "retry_dependency failed: ") || steps[0].Outcome != StepHandedUp {
		t.Fatalf("outcome %+v, case %+v, steps %+v; want the retry's failure handed up and the human asked", out, c, steps)
	}
}
