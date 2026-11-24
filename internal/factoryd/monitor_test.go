package factoryd

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A flag's case keys on the state it fired on. An event flag's detail
// names its event. A flag on the head's state keys on the head, the newest
// judged line's commit, h2. A wait keys on its kind and start: a run quiet
// since 11:00, a queue held since 09:00, whose wait_over_bound is left to
// queued_over_bound. A failed dependency opens no case: the run's own
// trip takes it. An ended card opens a case only for a case at the human
// no rung judged and for what it left open where Finish closes it, a
// merged or closing one none.
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
		{running, "faced_unjudged", "7", "7"},
		{running, "review_refused_not_run", "review: the gateway refused the call", "h2"},
		{running, "dependency_failed", "#5", "-"},
		{running, "wait_over_bound", "run_quiet since +0:00", "run_quiet|2026-11-04T11:00:00Z"},
		{queued, "queued_over_bound", "work bound 3 of 3 for 3h", "queued|2026-11-04T09:00:00Z"},
		{queued, "wait_over_bound", "queued since +1:00", "-"},
		{closes(digest(DigestHead{Status: "done"})), "issue_open_after_end", "session_cap", "h2"},
		{digest(DigestHead{Status: "failed"}), "issue_open_after_end", "session_cap", "-"},
		{digest(DigestHead{Status: "failed"}), "budget_moved_no_event", "3.00 EUR not 5.00 EUR", "-"},
		{digest(DigestHead{Status: "done"}), "faced_unjudged", "7", "7"},
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

// A flag's issue line names the answers the board shows on its face, as
// every stop's does: the card goes on, and no line sends the human to the
// chat. A quiet run's flag offers the rescue first. An ended card's line
// is AskHuman's, from its row's face (monitor_pg_test.go).
func TestAFlagsIssueLineNamesItsAnswers(t *testing.T) {
	quiet := Case{Kind: CaseMonitorFlag, Facts: map[string]any{"flag": "wait_over_bound", "wait": "run_quiet", "bound": "30m"}}
	want := "The card goes on meanwhile. Press “Rescue the work into a new card”, “It is fine, go on” or “Not now” on the board."
	if got := tripNext(quiet, MonitorFlagReason(DigestFlag{Name: "wait_over_bound", Detail: "run_quiet since +5:19"})); got != want {
		t.Errorf("the issue line %q, want %q", got, want)
	}
}

// The minute's re-take runs rung 0 on a case at the human again, on the
// facts Act reads: t1's checks wait stop on h2, whose checks are green,
// resolves at rung 0 with wait_checks and the finding as its reason, and
// the run hears it. t1 has no issue here, so no comment withdraws the
// question (the Postgres test reads that comment). On facts read partly (t1's timeline unreadable) the
// stop stays at the human, and so does one whose head a human's
// wait_checks already ran the wait on again (case 4, WaitedOnHead): the
// pass writes no step, no event and no signal for either.
func TestRetakeTakesAGreenChecksWaitOnFactsReadWhole(t *testing.T) {
	stop := Case{ID: 7, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseGateStop, Status: CaseAtHuman,
		Facts: map[string]any{"gate": ActivityWaitChecks, "head": "h2"}}
	waited := Case{ID: 4, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseGateStop, Status: CaseResolved,
		Facts: map[string]any{"gate": ActivityWaitChecks, "head": "h2"}, Resolution: &CaseResolution{By: "sebastian", Rung: 3, Action: ActWaitChecks}}
	took := &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWaitChecks, Reason: "every check on head h2 is green, so the factory resumed the checks wait"}
	for name, tc := range map[string]struct {
		setup func(g factsRig)
		want  *CaseResolution
	}{
		"read whole": {func(factsRig) {}, took},
		"timeline unreadable": {func(g factsRig) {
			g.r.objects = unreadable{ObjectStore: g.r.objects, key: "sessions/t1/timeline.jsonl"}
		}, nil},
		"waited on h2": {func(g factsRig) { g.store.cases = append(g.store.cases, waited) }, nil},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			g := newFactsRig(t)
			g.card()
			g.timeline(t)
			card := g.store.tasks["t1"]
			card.IssueNumber = 0
			g.store.tasks["t1"] = card
			g.store.cases = append(g.store.cases, stop)
			g.r.timeline = &Timeline{Objects: g.r.objects, Events: g.store, Store: g.store}
			tc.setup(g)
			events := len(g.store.events)
			m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
			if err := m.Retake(ctx); err != nil {
				t.Fatal(err)
			}
			c, _ := g.store.CaseByID(ctx, stop.ID)
			signals := g.r.cfg.Workflows.(*fakeWorkflows).signals
			if tc.want == nil {
				if c.Status != CaseAtHuman || c.Resolution != nil || len(g.store.caseSteps[stop.ID]) != 0 || len(g.store.events) != events || len(signals) != 0 {
					t.Fatalf("case %+v, steps %+v, events %d (were %d), signals %q; want the stop at the human and nothing written",
						c, g.store.caseSteps[stop.ID], len(g.store.events), events, signals)
				}
				return
			}
			if c.Status != CaseResolved || !reflect.DeepEqual(c.Resolution, tc.want) || !slices.Equal(signals, []string{"t1 case_resolved factoryd #0: "}) {
				t.Fatalf("case %s by %+v, signals %q; want it resolved by %+v and the run told", c.Status, c.Resolution, signals, tc.want)
			}
			row := WithdrawnRow(stop, tc.want.Reason, nil)
			row["action"] = ActWaitChecks
			if i := slices.IndexFunc(g.store.events, func(e Event) bool { return e.Kind == EventQuestionWithdrawn && e.TaskID == "t1" }); i < 0 || !reflect.DeepEqual(g.store.events[i].Detail, row) {
				t.Fatalf("events %+v; want one question_withdrawn row %+v", g.store.events, row)
			}
		})
	}
}

// The re-take reads only the cases at the human of a kind rung 0 tries a
// Fire row on: a human card's case, whose kind names none, costs no read
// of its card, and a day cap's, whose cap_wait row fires on none, costs
// one read of t1's timeline and takes nothing.
func TestRetakeReadsOnlyKindsWithAFiringRow(t *testing.T) {
	ctx := context.Background()
	g := newFactsRig(t)
	reads := &timelineReads{fakeObjectStore: g.r.objects.(*fakeObjectStore), n: map[string]int{}}
	g.r.objects = reads
	m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
	g.store.cases = []Case{{ID: 7, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseHumanCard, Status: CaseAtHuman}}
	if err := m.Retake(ctx); err != nil || reads.n["t1"] != 0 {
		t.Fatalf("a human card's case: %v, t1's timeline read %d times; want no read", err, reads.n["t1"])
	}
	g.store.cases = append(g.store.cases, Case{ID: 8, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseDayCap, Status: CaseAtHuman})
	if err := m.Retake(ctx); err != nil || reads.n["t1"] != 1 {
		t.Fatalf("beside a day cap's case: %v, t1's timeline read %d times; want one read", err, reads.n["t1"])
	}
	for _, c := range g.store.cases {
		if c.Status != CaseAtHuman || c.Resolution != nil {
			t.Fatalf("case %d = %s by %+v; want it at the human", c.ID, c.Status, c.Resolution)
		}
	}
}
