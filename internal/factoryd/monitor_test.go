package factoryd

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
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

// A checks wait's stop at the human whose checks are green goes on with no
// human: the pass takes it on rung 0 with wait_checks, the run is told, and
// the timeline holds one question_withdrawn row of the question (the
// Postgres test reads that comment). Only a reader the row needs keeps it
// off: an unreadable timeline does not, nor does a model nobody can price,
// which fails the floor's read (the roll card whose checks turned green
// while LiteLLM priced no model), nor a wait_checks a human answered on the
// head before, nor rung 0's own wait when a runner came. A stop that came
// back after rung 0 ran the wait on green checks stays at the human, and so
// does one whose checks cannot be read: one step says why, however often
// the pass runs.
func TestRetakeTakesAGreenChecksWaitOnTheFactsItsRowNeeds(t *testing.T) {
	stop := Case{ID: 7, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseGateStop, Status: CaseAtHuman,
		Facts: map[string]any{"gate": ActivityWaitChecks, "head": "h2", "model": "small"}}
	waited := Case{ID: 4, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseGateStop, Status: CaseResolved,
		Facts: map[string]any{"gate": ActivityWaitChecks, "head": "h2"}, Resolution: &CaseResolution{By: "sebastian", Rung: 3, Action: ActWaitChecks}}
	rungZeroWaited := func(args map[string]any) Case {
		c := waited
		c.Resolution = &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWaitChecks, Args: args}
		return c
	}
	took := &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWaitChecks, Reason: "every check on head h2 is green, so the factory resumed the checks wait"}
	for name, tc := range map[string]struct {
		setup func(g factsRig)
		want  *CaseResolution
		kept  string
	}{
		"read whole": {setup: func(factsRig) {}, want: took},
		"timeline unreadable": {setup: func(g factsRig) {
			g.r.objects = unreadable{ObjectStore: g.r.objects, key: "sessions/t1/timeline.jsonl"}
		}, want: took},
		"no model priced": {setup: func(g factsRig) { g.r.cfg.Prices = aliasPrices{} }, want: took},
		"waited on h2":    {setup: func(g factsRig) { g.store.cases = append(g.store.cases, waited) }, want: took},
		"rung 0 ran the wait again when a runner came": {setup: func(g factsRig) {
			g.store.cases = append(g.store.cases, rungZeroWaited(map[string]any{"runner": true}))
		}, want: took},
		"rung 0 ran the wait again on green checks": {setup: func(g factsRig) { g.store.cases = append(g.store.cases, rungZeroWaited(nil)) },
			kept: "rung 0 took this stop with wait_checks before, and it came back"},
		"checks unreadable": {setup: func(g factsRig) {
			g.r.forgejo = &Forgejo{BaseURL: "http://127.0.0.1:1", BotUser: "factory-agent", BotPassword: "pw"}
		},
			kept: "facts read partly: checks, which route_approval, rerun_checks and wait_checks would read"},
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
			for range 2 {
				if err := m.Retake(ctx); err != nil {
					t.Fatal(err)
				}
			}
			c, _ := g.store.CaseByID(ctx, stop.ID)
			signals := g.r.cfg.Workflows.(*fakeWorkflows).signals
			if tc.want == nil {
				steps := g.store.caseSteps[stop.ID]
				if c.Status != CaseAtHuman || c.Resolution != nil || len(steps) != 1 || steps[0].Rung != 0 || steps[0].Found != tc.kept ||
					len(g.store.events) != events || len(signals) != 0 {
					t.Fatalf("case %+v, steps %+v, events %d (were %d), signals %q; want the stop at the human and the one step %q",
						c, steps, len(g.store.events), events, signals, tc.kept)
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

// capRetakeRig is the facts rig with card t1 funded (20 EUR, none spent)
// and attempts sessions in, under a version activated at 06:00 with a month
// cap of 800 EUR, and t1's month cap case at the human, opened at 11:03:25,
// its next session on small. The month is spent unless a test frees it.
func capRetakeRig(t *testing.T, attempts int) (factsRig, Case) {
	t.Helper()
	g := newFactsRig(t)
	v := g.r.cfg.Policy()
	v.ActivatedAt, v.Document.Budget.MonthEUR = factsAt(6, 0), 800
	g.r.cfg.Policy = func() policy.Version { return v }
	card := g.store.tasks["t1"]
	card.IssueNumber, card.BudgetEUR, card.SpentEUR, card.HeldEUR, card.Usage = 0, 20, 0, 0, TaskUsage{Attempts: attempts}
	g.store.tasks["t1"] = card
	g.store.capsFree = &policy.Free{Day: math.Inf(1), Month: 0, Project: math.Inf(1)}
	g.r.timeline = &Timeline{Objects: g.r.objects, Events: g.store, Store: g.store}
	c := Case{ID: 9, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseMonthCap, Status: CaseAtHuman,
		OpenedAt: factsAt(11, 3).Add(25 * time.Second), Facts: map[string]any{"model": "small", "head": "h2"}}
	g.store.cases = append(g.store.cases, c)
	return g, c
}

// The re-take resumes a card whose month cap's count a human reset while
// its ask waited: sebastian's reset at 11:30:32 frees the month, and the
// pass at 11:31 resolves the case on rung 0 with resume, its reason the
// reset and its moment, the row records the withdrawal with both, and the
// run hears it. With the month still spent and no reset it takes nothing.
func TestRetakeTakesAMonthCapAskWhoseCountWasReset(t *testing.T) {
	ctx := context.Background()
	g, c := capRetakeRig(t, 0)
	g.r.now = func() time.Time { return factsAt(11, 31) }
	m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
	events := len(g.store.events)
	if err := m.Retake(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.store.CaseByID(ctx, c.ID); got.Status != CaseAtHuman || len(g.store.events) != events {
		t.Fatalf("the month spent: case %s, %d new events; want it at the human and nothing written", got.Status, len(g.store.events)-events)
	}
	reset := factsAt(11, 30).Add(32 * time.Second)
	g.store.capResets = []policy.CapReset{{ID: 1, Cap: policy.CapMonth, At: reset, Login: "sebastian", BeforeEUR: 800}}
	g.store.capsFree.Month = 800
	if err := m.Retake(ctx); err != nil {
		t.Fatal(err)
	}
	const why = "the month cap's count was reset at 11:30 UTC by sebastian"
	said := Moments{{At: reset, Layout: LayoutClock}}
	got, _ := g.store.CaseByID(ctx, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.By != FactoryDecider || got.Resolution.Rung != 0 ||
		got.Resolution.Action != ActResume || got.Resolution.Reason != why {
		t.Fatalf("case %s by %+v; want it resolved on rung 0 with resume for %q", got.Status, got.Resolution, why)
	}
	i := slices.IndexFunc(g.store.events, func(e Event) bool { return e.Kind == EventQuestionWithdrawn && e.TaskID == "t1" })
	if i < 0 || g.store.events[i].Detail["reason"] != why || !reflect.DeepEqual(MomentsOf(g.store.events[i].Detail), said) {
		t.Fatalf("events %+v; want a question_withdrawn row for %q with %+v", g.store.events[events:], why, said)
	}
	if signals := g.r.cfg.Workflows.(*fakeWorkflows).signals; len(signals) != 1 || !strings.HasPrefix(signals[0], "t1 case_resolved factoryd") {
		t.Fatalf("signals %q; want the run told", signals)
	}
}

// A lifted cap on a card whose sessions are spent takes nothing: resume is
// refused at the session cap, so the ask stays with the human with no row
// and no signal, and one step of rung 0 says why, however often the pass
// runs.
func TestRetakeLeavesALiftedCapAtTheSessionCap(t *testing.T) {
	ctx := context.Background()
	g, c := capRetakeRig(t, 10)
	g.r.now = func() time.Time { return factsAt(11, 31) }
	g.store.capsFree.Month = 800
	events := len(g.store.events)
	m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
	for range 2 {
		if err := m.Retake(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := g.store.CaseByID(ctx, c.ID)
	steps := g.store.caseSteps[c.ID]
	if got.Status != CaseAtHuman || len(steps) != 1 || steps[0].Rung != 0 || steps[0].Outcome != StepHandedUp ||
		!strings.HasPrefix(steps[0].Found, "cap_wait was refused: the session cap of 3 is reached") ||
		len(g.store.events) != events || len(g.r.cfg.Workflows.(*fakeWorkflows).signals) != 0 {
		t.Fatalf("case %s, steps %+v, %d new events; want it at the human, one step that names the session cap, and no row", got.Status, steps, len(g.store.events)-events)
	}
}

// A re-take that read the ask before the run's own wait ended it finds the
// case ended: it writes no second row and tells the run nothing.
func TestARetakeAfterTheRunsOwnEndWritesNothing(t *testing.T) {
	ctx := context.Background()
	g, c := capRetakeRig(t, 0)
	g.r.now = func() time.Time { return factsAt(11, 31) }
	g.store.capsFree.Month = 800
	if err := g.store.WithdrawCase(ctx, c.ID, "the month cap of 800.00 EUR in policy version 7 leaves room for a session"); err != nil {
		t.Fatal(err)
	}
	events := len(g.store.events)
	if res, err := g.r.retake(ctx, c); err != nil || res != nil {
		t.Fatalf("retake = %+v, %v; want nothing taken", res, err)
	}
	if len(g.store.events) != events || len(g.r.cfg.Workflows.(*fakeWorkflows).signals) != 0 {
		t.Fatalf("%d new events, signals %q; want none", len(g.store.events)-events, g.r.cfg.Workflows.(*fakeWorkflows).signals)
	}
}

// providerRig is the facts rig with card t1 funded (20 EUR, none spent, no
// session yet) and its provider stop on big at the human, opened at 05:00
// with state key k, the clock at 05:44.
func providerRig(t *testing.T) (factsRig, Case) {
	t.Helper()
	g := newFactsRig(t)
	card := g.store.tasks["t1"]
	card.IssueNumber, card.BudgetEUR, card.SpentEUR, card.HeldEUR, card.Usage = 0, 20, 0, 0, TaskUsage{}
	g.store.tasks["t1"] = card
	g.r.timeline = &Timeline{Objects: g.r.objects, Events: g.store, Store: g.store}
	g.r.now = func() time.Time { return factsAt(5, 44) }
	c := Case{ID: 9, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseProviderRefused, StateKey: "k", Status: CaseAtHuman,
		OpenedAt: factsAt(5, 0), Facts: map[string]any{"model": "big", "head": "h2"}}
	g.store.cases = append(g.store.cases, c)
	return g, c
}

// A provider stop on big resumes once a call on big answers after the stop
// opened, whichever card made it: another card's call at 05:43:10 resolves
// the case on rung 0 with resume, naming the call and its moment, and the
// run hears it. A call on small alone, or one on big before the stop, takes
// nothing.
func TestRetakeResumesAProviderStopOnceItsModelAnswers(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		calls []fakeCall
		took  bool
	}{
		{"big answered after the stop", []fakeCall{{alias: "big", at: factsAt(5, 43).Add(10 * time.Second)}}, true},
		{"small alone answered", []fakeCall{{alias: "small", at: factsAt(5, 30)}}, false},
		{"big answered before the stop", []fakeCall{{alias: "big", at: factsAt(4, 59)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, c := providerRig(t)
			g.store.answeredCalls = tc.calls
			events := len(g.store.events)
			m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
			if err := m.Retake(ctx); err != nil {
				t.Fatal(err)
			}
			got, _ := g.store.CaseByID(ctx, c.ID)
			if !tc.took {
				if got.Status != CaseAtHuman || len(g.store.events) != events {
					t.Fatalf("case %s, %d new events; want it at the human and nothing written", got.Status, len(g.store.events)-events)
				}
				return
			}
			const why = "a call on big answered at 05:43 UTC, so the provider answers again"
			said := Moments{{At: factsAt(5, 43).Add(10 * time.Second), Layout: LayoutClock}}
			if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Rung != 0 || got.Resolution.Action != ActResume || got.Resolution.Reason != why {
				t.Fatalf("case %s by %+v; want it resolved on rung 0 with resume for %q", got.Status, got.Resolution, why)
			}
			i := slices.IndexFunc(g.store.events, func(e Event) bool { return e.Kind == EventQuestionWithdrawn && e.TaskID == "t1" })
			if i < 0 || !reflect.DeepEqual(MomentsOf(g.store.events[i].Detail), said) {
				t.Fatalf("events %+v; want the withdrawal with %+v", g.store.events[events:], said)
			}
			if signals := g.r.cfg.Workflows.(*fakeWorkflows).signals; len(signals) != 1 || !strings.HasPrefix(signals[0], "t1 case_resolved factoryd") {
				t.Fatalf("signals %q; want the run told", signals)
			}
		})
	}
}

// A provider stop that came back after rung 0 resumed the card once stays
// with the human, though its model answers again.
func TestAProviderStopThatCameBackStaysWithTheHuman(t *testing.T) {
	ctx := context.Background()
	g, c := providerRig(t)
	g.store.answeredCalls = []fakeCall{{alias: "big", at: factsAt(5, 43)}}
	g.store.cases = append(g.store.cases, Case{ID: 4, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseProviderRefused, StateKey: "k",
		Status: CaseResolved, Resolution: &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActResume, Reason: "a call on big answered at 03:10 UTC, so the provider answers again"}})
	events := len(g.store.events)
	m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
	if err := m.Retake(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.store.CaseByID(ctx, c.ID); got.Status != CaseAtHuman || len(g.store.events) != events {
		t.Fatalf("case %s, %d new events; want it at the human and nothing written", got.Status, len(g.store.events)-events)
	}
}

// The night of 2026-11-24, on the rig's day: a month cap case opened at
// 11:03:25 went to rung 2, due at 11:38:26, and rung 2 never answered;
// sebastian reset the month's count at 11:30:32. The wake at 11:38:27
// expires the step, and the climb from rung 2 checks the cause before the
// face: the case resolves on rung 0 with resume for the reset, nobody is
// asked, and the run hears it. With the month still spent and no reset,
// the climb asks the human as before.
func TestClimbAfterAnExpiredRungTakesALiftedCap(t *testing.T) {
	ctx := context.Background()
	for _, reset := range []bool{true, false} {
		g, c := capRetakeRig(t, 0)
		card := g.store.tasks["t1"]
		card.Status = "running"
		g.store.tasks["t1"] = card
		g.store.cases[len(g.store.cases)-1].Status = CaseOpen
		due := factsAt(11, 38).Add(26 * time.Second)
		if _, err := g.store.StartRungStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, DueAt: &due}); err != nil {
			t.Fatal(err)
		}
		if reset {
			g.store.capResets = []policy.CapReset{{ID: 1, Cap: policy.CapMonth, At: factsAt(11, 30).Add(32 * time.Second), Login: "sebastian", BeforeEUR: 800}}
			g.store.capsFree.Month = 800
		}
		at := due.Add(time.Second)
		g.r.now = func() time.Time { return at }
		m := &Monitor{Store: g.store, Runner: g.r, Now: func() time.Time { return at }, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
		if err := m.WakeCases(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := g.store.CaseByID(ctx, c.ID)
		asked := slices.ContainsFunc(g.store.events, func(e Event) bool { return e.Kind == EventQuestionAsked && e.TaskID == "t1" })
		if !reset {
			if got.Status != CaseAtHuman || !asked {
				t.Fatalf("the month spent: case %s, asked %v; want it at the human", got.Status, asked)
			}
			continue
		}
		const why = "the month cap's count was reset at 11:30 UTC by sebastian"
		if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Rung != 0 || got.Resolution.Action != ActResume || got.Resolution.Reason != why || asked {
			t.Fatalf("case %s by %+v, asked %v; want it resolved on rung 0 with resume for %q and nobody asked", got.Status, got.Resolution, asked, why)
		}
		if signals := g.r.cfg.Workflows.(*fakeWorkflows).signals; len(signals) != 1 || !strings.HasPrefix(signals[0], "t1 case_resolved factoryd") {
			t.Fatalf("signals %q; want the run told", signals)
		}
	}
}

// A case Act holds while rung 2's proposal climbs it skips the check: the
// climb runs under that lock, which the check would wait on, and the
// retake pass checks the case once it is at the human.
func TestClimbUnderTheCasesLockSkipsTheCheck(t *testing.T) {
	ctx := context.Background()
	g, c := capRetakeRig(t, 0)
	g.r.now = func() time.Time { return factsAt(11, 31) }
	g.store.capsFree.Month = 800
	unlock, ok := g.r.TryLockCase(c.ID)
	if !ok {
		t.Fatal("the case's lock is held")
	}
	res, err := g.r.causeGone(ctx, c)
	unlock()
	if err != nil || res != nil {
		t.Fatalf("causeGone under the lock = %+v, %v; want nothing", res, err)
	}
	if res, err = g.r.causeGone(ctx, c); err != nil || res == nil || res.Action != ActResume {
		t.Fatalf("causeGone = %+v, %v; want the resume once the lock is free", res, err)
	}
}

// The re-take reads a card only for a case at the human of a kind rung 0
// tries a Fire row on: a human card's case, whose kind names none and
// whose steps say no rung missed it, costs no read of its card, and a day
// cap's, opened today, whose cap_wait row finds the card's own budget short
// of a session, costs one read of t1's timeline and takes nothing.
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
	g.store.cases = append(g.store.cases, Case{ID: 8, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseDayCap, Status: CaseAtHuman, OpenedAt: factsAt(11, 0)})
	if err := m.Retake(ctx); err != nil || reads.n["t1"] != 1 {
		t.Fatalf("beside a day cap's case: %v, t1's timeline read %d times; want one read", err, reads.n["t1"])
	}
	for _, c := range g.store.cases {
		if c.Status != CaseAtHuman || c.Resolution != nil {
			t.Fatalf("case %d = %s by %+v; want it at the human", c.ID, c.Status, c.Resolution)
		}
	}
}

// A card with no model answer in three sessions resumes on the fact a
// provider stop resumes on: another card's call on big at 05:43:10, after
// the stop opened at 05:00, says the provider answers again.
func TestRetakeResumesACardWithNoModelAnswerOnceItsModelAnswers(t *testing.T) {
	ctx := context.Background()
	g, c := providerRig(t)
	g.store.cases[len(g.store.cases)-1].Kind = CaseNoAnswer
	g.store.answeredCalls = []fakeCall{{alias: "big", at: factsAt(5, 43).Add(10 * time.Second)}}
	m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
	if err := m.Retake(ctx); err != nil {
		t.Fatal(err)
	}
	const why = "a call on big answered at 05:43 UTC, so the provider answers again"
	if got, _ := g.store.CaseByID(ctx, c.ID); got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Rung != 0 || got.Resolution.Action != ActResume || got.Resolution.Reason != why {
		t.Fatalf("case %s by %+v; want it resolved on rung 0 with resume for %q", got.Status, got.Resolution, why)
	}
}

// A card whose pods did not start, and one whose sessions the platform
// ended, resume once sessions run again: two sessions of other cards
// started at 05:10 and 05:20, after the stop opened at 05:00, and the
// platform ended none since. No session started since the stop takes
// nothing, and neither does a session the platform ended at 05:30.
func TestRetakeResumesAPlatformStopOnceSessionsRunAgain(t *testing.T) {
	ctx := context.Background()
	started := func(g factsRig) {
		g.event("t2", factsAt(4, 50), EventSessionStarted, nil)
		g.event("t2", factsAt(5, 10), EventSessionStarted, nil)
		g.event("t3", factsAt(5, 20), EventSessionStarted, nil)
	}
	for _, kind := range []CaseKind{CasePodNeverStarted, CasePlatformFault} {
		for name, tc := range map[string]struct {
			setup func(g factsRig)
			took  bool
		}{
			"sessions started since": {started, true},
			"none started since":     {func(g factsRig) { g.event("t2", factsAt(4, 50), EventSessionStarted, nil) }, false},
			"the platform ended one since": {func(g factsRig) {
				started(g)
				g.store.platformDeaths = []PodDeath{{Kind: "session", Record: DeathRecord{At: factsAt(5, 30)}}}
			}, false},
		} {
			t.Run(string(kind)+", "+name, func(t *testing.T) {
				g, c := providerRig(t)
				g.store.cases[len(g.store.cases)-1].Kind = kind
				tc.setup(g)
				events := len(g.store.events)
				m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
				if err := m.Retake(ctx); err != nil {
					t.Fatal(err)
				}
				got, _ := g.store.CaseByID(ctx, c.ID)
				if !tc.took {
					if got.Status != CaseAtHuman || len(g.store.events) != events || len(g.store.caseSteps[c.ID]) != 0 {
						t.Fatalf("case %s, %d new events, steps %+v; want it at the human and nothing written", got.Status, len(g.store.events)-events, g.store.caseSteps[c.ID])
					}
					return
				}
				const why = "2 sessions started since the stop and the platform ended none, so sessions run again"
				if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Rung != 0 || got.Resolution.Action != ActResume || got.Resolution.Reason != why {
					t.Fatalf("case %s by %+v; want it resolved on rung 0 with resume for %q", got.Status, got.Resolution, why)
				}
				if signals := g.r.cfg.Workflows.(*fakeWorkflows).signals; len(signals) != 1 || !strings.HasPrefix(signals[0], "t1 case_resolved factoryd") {
					t.Fatalf("signals %q; want the run told", signals)
				}
			})
		}
	}
}

// A checks wait's stop that found no runner on its jobs goes on once a
// runner takes one: the stop's facts say no_runner, the head's checks are
// still pending and one job has started, so the pass runs the wait again. While
// every job still waits for a runner the stop stays, with nothing written.
func TestRetakeResumesAChecksWaitOnceARunnerTakesItsJobs(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		statuses string
		took     bool
	}{
		"a job started": {`{"status":"pending","context":"pr / test","description":"Has started running"},{"status":"pending","context":"pr / vet","description":"Waiting to run"}`, true},
		"none started":  {`{"status":"pending","context":"pr / test","description":"Waiting to run"},{"status":"pending","context":"pr / image-build","description":"Blocked by required conditions"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := newFactsRig(t)
			g.card()
			g.timeline(t)
			forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/repos/demo-app/demo-app/commits/h2/status" {
					fmt.Fprint(w, `{"state":"pending","statuses":[`+tc.statuses+`]}`)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(forge.Close)
			g.r.forgejo = &Forgejo{BaseURL: forge.URL, BotUser: "factory-agent", BotPassword: "pw"}
			card := g.store.tasks["t1"]
			card.IssueNumber = 0
			g.store.tasks["t1"] = card
			stop := Case{ID: 7, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseGateStop, Status: CaseAtHuman,
				Facts: map[string]any{"gate": ActivityWaitChecks, "head": "h2", "no_runner": true}}
			g.store.cases = append(g.store.cases, stop)
			g.r.timeline = &Timeline{Objects: g.r.objects, Events: g.store, Store: g.store}
			m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
			if err := m.Retake(ctx); err != nil {
				t.Fatal(err)
			}
			c, _ := g.store.CaseByID(ctx, stop.ID)
			if !tc.took {
				if c.Status != CaseAtHuman || len(g.store.caseSteps[stop.ID]) != 0 {
					t.Fatalf("case %s, steps %+v; want the stop at the human and nothing written", c.Status, g.store.caseSteps[stop.ID])
				}
				return
			}
			want := &CaseResolution{By: FactoryDecider, Rung: 0, Action: ActWaitChecks, Args: map[string]any{"runner": true}, Reason: "a runner took the jobs of the checks on head h2, so the factory resumed the checks wait"}
			if c.Status != CaseResolved || !reflect.DeepEqual(c.Resolution, want) {
				t.Fatalf("case %s by %+v; want it resolved by %+v", c.Status, c.Resolution, want)
			}
		})
	}
}

// judgeAgainRig is the facts rig with card t1 waiting on its human for case
// 9, an open problem opened at 05:00: the provider refused rung 2's step,
// which ran from 05:01 to 05:05, and the case faced the human marked not
// judged. The clock is at 05:44.
func judgeAgainRig(t *testing.T) (factsRig, Case) {
	t.Helper()
	g := newFactsRig(t)
	g.r.timeline = &Timeline{Objects: g.r.objects, Events: g.store, Store: g.store}
	g.r.now = func() time.Time { return factsAt(5, 44) }
	c := Case{ID: 9, TaskID: "t1", Repository: "demo-app/demo-app", Kind: CaseOther, StateKey: "k", Status: CaseAtHuman, OwnerRung: 3,
		OpenedAt: factsAt(5, 0), Problem: "the run stopped on something no rule names", Facts: map[string]any{"head": "h2"}}
	g.store.cases = append(g.store.cases, c)
	const refusal = "not judged: the model provider refused: the gateway refused the call"
	ended := factsAt(5, 5)
	g.store.caseSteps = map[int64][]CaseStep{c.ID: {
		{ID: 1, CaseID: c.ID, Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp, Found: noRowFits},
		{ID: 2, CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp, StartedAt: factsAt(5, 1), EndedAt: &ended, Found: refusal},
		{ID: 3, CaseID: c.ID, Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp, Found: refusal},
		{ID: 4, CaseID: c.ID, Rung: 3, Actor: FactoryDecider, CommentID: 50},
	}}
	return g, c
}

// retakeTwice runs the retake pass twice on rig g.
func retakeTwice(t *testing.T, g factsRig) {
	t.Helper()
	m := &Monitor{Store: g.store, Runner: g.r, Policy: func() policy.Tree { return g.r.cfg.Policy().Tree }}
	for range 2 {
		if err := m.Retake(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// givenToRungTwo checks that the passes on case c of rig g wrote, past the
// steps and events it held before, one rung 2 step in progress due at the
// rung's bound of 35m past 05:44 and one case_climbed row that names the
// case, and that the case is still at the human, owned by rung 3 with no
// wake.
func givenToRungTwo(t *testing.T, g factsRig, c Case, steps, events int) {
	t.Helper()
	got, _ := g.store.CaseByID(context.Background(), c.ID)
	if got.Status != CaseAtHuman || got.OwnerRung != 3 || got.NextWakeAt != nil {
		t.Fatalf("case %s owned by rung %d waking at %v; want it at the human, owned by rung 3 with no wake", got.Status, got.OwnerRung, got.NextWakeAt)
	}
	added := g.store.caseSteps[c.ID][steps:]
	due := factsAt(6, 19)
	if len(added) != 1 || added[0].Rung != 2 || added[0].Actor != ChatUser || added[0].Outcome != StepInProgress || added[0].DueAt == nil || !added[0].DueAt.Equal(due) {
		t.Fatalf("new steps %+v; want one of rung 2 in progress due at %s", added, due)
	}
	rows := g.store.events[events:]
	if len(rows) != 1 || rows[0].Kind != EventCaseClimbed || rows[0].Detail["case_id"] != c.ID || rows[0].Detail["rung"] != 2 {
		t.Fatalf("new rows %+v; want one case_climbed row naming case %d at rung 2", rows, c.ID)
	}
}

// A case at the human no rung judged, for the provider refused rung 2's
// step at 05:05, goes to rung 2 once more when a call on big answers after
// the refusal, at 05:19, whichever card made it: two passes give it to rung
// 2 once (givenToRungTwo). A call on big during the refused step, or on small
// alone, writes nothing.
func TestRetakeAsksRungTwoAgainOnceTheProviderAnswers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls []fakeCall
		asks  bool
	}{
		{"big answered after the refusal", []fakeCall{{alias: "big", at: factsAt(5, 19)}}, true},
		{"big answered during the refused step", []fakeCall{{alias: "big", at: factsAt(5, 3)}}, false},
		{"small alone answered", []fakeCall{{alias: "small", at: factsAt(5, 30)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, c := judgeAgainRig(t)
			g.store.answeredCalls = tc.calls
			steps, events := len(g.store.caseSteps[c.ID]), len(g.store.events)
			retakeTwice(t, g)
			if tc.asks {
				givenToRungTwo(t, g, c, steps, events)
				return
			}
			got, _ := g.store.CaseByID(context.Background(), c.ID)
			if got.Status != CaseAtHuman || len(g.store.caseSteps[c.ID]) != steps || len(g.store.events) != events {
				t.Fatalf("case %s, steps %+v, %d new events; want it at the human and nothing written", got.Status, g.store.caseSteps[c.ID], len(g.store.events)-events)
			}
		})
	}
}

// A case rung 2 was asked on again, whose second step the provider refused
// too, stays with the human: rung 2 works a case at most twice, so the pass
// writes nothing, though big answered after both refusals.
func TestRetakeAsksRungTwoNoThirdTime(t *testing.T) {
	g, c := judgeAgainRig(t)
	second := factsAt(5, 30)
	g.store.caseSteps[c.ID] = append(g.store.caseSteps[c.ID], CaseStep{ID: 5, CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepHandedUp,
		StartedAt: factsAt(5, 20), EndedAt: &second, Found: "not judged: the model provider refused: the gateway refused the call"})
	g.store.answeredCalls = []fakeCall{{alias: "big", at: factsAt(5, 19)}, {alias: "big", at: factsAt(5, 40)}}
	steps, events := len(g.store.caseSteps[c.ID]), len(g.store.events)
	retakeTwice(t, g)
	if len(g.store.caseSteps[c.ID]) != steps || len(g.store.events) != events {
		t.Fatalf("steps %+v, %d new events; want nothing written", g.store.caseSteps[c.ID][steps:], len(g.store.events)-events)
	}
}

// A case at the human whose rung 2 never ran, for the judgment budget was
// spent, goes to rung 2 once the budget leaves a call room: with the month
// spent the passes write nothing, the step that says so standing; once the
// chat task has spent nothing, two passes give the case to rung 2 once
// (givenToRungTwo).
func TestRetakeAsksRungTwoOnceTheJudgmentBudgetLeavesRoom(t *testing.T) {
	g, c := judgeAgainRig(t)
	g.store.caseSteps[c.ID] = []CaseStep{
		{ID: 1, CaseID: c.ID, Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp, Found: noRowFits},
		{ID: 2, CaseID: c.ID, Rung: 0, Actor: FactoryDecider, Outcome: StepHandedUp, Found: "not judged, judgment budget spent: the chat task t-chat has 0.00 of 10.00 EUR left"},
		{ID: 3, CaseID: c.ID, Rung: 3, Actor: FactoryDecider, CommentID: 50},
	}
	g.store.chatSpent = 1000
	steps, events := len(g.store.caseSteps[c.ID]), len(g.store.events)
	retakeTwice(t, g)
	if len(g.store.caseSteps[c.ID]) != steps || len(g.store.events) != events {
		t.Fatalf("the budget spent: steps %+v, %d new events; want nothing written", g.store.caseSteps[c.ID][steps:], len(g.store.events)-events)
	}
	g.store.chatSpent = 0
	retakeTwice(t, g)
	givenToRungTwo(t, g, c, steps, events)
}

// A due ask a spent budget keeps off leaves one step that says so: big
// answered after rung 2's refusal, but the chat task's month is spent, so
// two passes write one step of rung 0 that names the spent budget and no
// rung 2 step.
func TestRetakeNamesTheBudgetThatKeepsADueAskOff(t *testing.T) {
	g, c := judgeAgainRig(t)
	g.store.answeredCalls = []fakeCall{{alias: "big", at: factsAt(5, 19)}}
	g.store.chatSpent = 1000
	steps, events := len(g.store.caseSteps[c.ID]), len(g.store.events)
	retakeTwice(t, g)
	added := g.store.caseSteps[c.ID][steps:]
	if len(added) != 1 || added[0].Rung != 0 || !strings.HasPrefix(added[0].Found, "not judged, judgment budget spent: the chat task ") || len(g.store.events) != events {
		t.Fatalf("new steps %+v, %d new events; want one step of rung 0 naming the spent budget and no row", added, len(g.store.events)-events)
	}
}
