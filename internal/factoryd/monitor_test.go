package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A flag's case keys on the state it fired on. An event flag's detail
// names its event. A flag on the head's state keys on the head, the newest
// judged line's commit, h2. A wait keys on its kind and start: a run quiet
// since 11:00, a queue held since 09:00, whose wait_over_bound is left to
// queued_over_bound. A slot held by a wait and a failed dependency open no
// case: the first is a record, the run's own trip takes the second. An
// ended card opens a case only for what it left open where Finish closes
// it, a merged or closing one none.
func TestMonitorFlagStates(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	held := at.Add(-3 * time.Hour)
	digest := func(h DigestHead) CardDigest {
		return CardDigest{Head: h, Steps: []DigestStep{{commit: "h1"}, {Kind: "session"}, {commit: "h2"}, {Kind: "session"}},
			Waits: policy.DefaultWaits, at: at}
	}
	// closes is d of an end whose issue Finish closes
	closes := func(d CardDigest) CardDigest { d.issueCloses = true; return d }
	running := digest(DigestHead{Status: "running", StatusSince: at.Add(-time.Hour)})
	queued := digest(DigestHead{Status: "queued", StatusSince: at.Add(-4 * time.Hour), QueueReason: "work bound 3 of 3", QueueSince: &held})
	for _, tc := range []struct {
		d            CardDigest
		name, detail string
		want         string // "-" for no case
	}{
		{running, "checkin_cut", "+0:20", "+0:20"},
		{running, "session_no_push", "session 1", "session 1"},
		{running, "grant_bought_nothing", "session 4", "session 4"},
		{running, "finding_confirmed_twice", "0123456789ab", "0123456789ab"},
		{running, "budget_moved_no_event", "3.00 EUR not 5.00 EUR", "3.00 EUR not 5.00 EUR"},
		{running, "answer_changed_nothing", "h1", "h1"},
		{running, "review_refused_not_run", "review: the gateway refused the call", "h2"},
		{running, "money_ask_hides_findings", "", "h2"},
		{running, "slot_held_by_wait", "approval", "-"},
		{running, "dependency_failed", "#5", "-"},
		{running, "wait_over_bound", "run_quiet since +0:00", "run_quiet|2026-11-04T11:00:00Z"},
		{queued, "queued_over_bound", "work bound 3 of 3 since +1:00", "queued|2026-11-04T09:00:00Z"},
		{queued, "wait_over_bound", "queued since +1:00", "-"},
		{closes(digest(DigestHead{Status: "done"})), "issue_open_after_end", "session_cap", "h2"},
		{digest(DigestHead{Status: "failed"}), "issue_open_after_end", "session_cap", "-"},
		{digest(DigestHead{Status: "failed"}), "budget_moved_no_event", "3.00 EUR not 5.00 EUR", "-"},
		{digest(DigestHead{Status: "merged"}), "review_refused_not_run", "review: x", "-"},
		{digest(DigestHead{Status: "closing"}), "checkin_cut", "+0:20", "-"},
	} {
		state, _, ok := flagCase(tc.d, DigestFlag{tc.name, tc.detail})
		if got := map[bool]string{true: state, false: "-"}[ok]; got != tc.want {
			t.Errorf("%s(%s) on a %s card = %q, want %q", tc.name, tc.detail, tc.d.Head.Status, got, tc.want)
		}
	}
}

// A failed dependency is the run's: its own trip retries it. The monitor
// opens no case on the flag and retries nothing.
func TestMonitorLeavesAFailedDependencyToItsRun(t *testing.T) {
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
	m := &Monitor{Store: store, Runner: r, Policy: func() policy.Tree { return r.cfg.Policy().Tree }, Now: r.now}
	if err := m.card(ctx, "t-b"); err != nil {
		t.Fatalf("card: %v", err)
	}
	cases, _ := store.CasesOf(ctx, "t-b")
	if _, retried := store.tasks[RetryID("t-a")]; len(cases) != 0 || retried {
		t.Fatalf("t-b's cases = %+v, t-a retried %v; want none and no retry", cases, retried)
	}
}
