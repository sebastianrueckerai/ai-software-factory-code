package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
)

// The recorded-answer ledger: each fixture compresses the facts the design's
// table (escalation-ladder-redesign-2026-10-15.md, section 4, the table at
// 244) recorded. #836's TasksByPR exists at store.go:672 on main and the
// checks were green; #838's sync with main turned the checks red and the
// human named the one-line fix; #839's fund died at 0.15 EUR, its two starved
// sessions counted as attempts, and the human named two tests and their fix;
// #854's session burnt 4.89 EUR of the 5 EUR fund and the review returned
// eight findings of which seven were false, four naming code that sat in
// another part of the diff, two contradicting the prototype, one a misread.
// Every model role here is stubbed by its recorded answer and the check-in
// by a scripted verdict, until the live validation
// (escalation-ladder-validation.md, section 2) runs the ladder against real
// cards.
func TestLadderReplaysTheEightProblems(t *testing.T) {
	// #837, design section 4 problem 1 and its replay row: a concerns that
	// carries only an opinion becomes a remark — it blocks nothing, starts
	// no session, and rides onto the approval's card.
	t.Run("#837_concerns_remarks", func(t *testing.T) {
		opinion := "the merge rule reads oddly"
		g := newGateHarness(t, func(g *gateHarness) {
			g.taskConfig = askTaskConfig()
			g.reviews = []Review{
				{Verdict: "concerns", Summary: "a human must merge it", Findings: []string{opinion}},
				{Verdict: "approve", Summary: "the head answers the brief"},
			}
		})
		remark := map[string]CheckVerdict{opinion: {State: CheckRemark, Round: 1}}
		g.onSessions(pushed("s1", "c1"),
			checkinPushed("s2", "c2", &CheckInOutcome{Decision: CheckHandover,
				Handover: "the merge rule is the console's, not this diff's: go on", Verdicts: remark}))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, want done", final)
		}
		if len(g.sessions) != 2 || len(g.approvals) != 1 || len(g.merges) != 1 {
			t.Fatalf("sessions %d, approvals %d, merges %d; want the push and the check-in's pod, one approval, one merge: the remark started no session",
				len(g.sessions), len(g.approvals), len(g.merges))
		}
		if in := g.approvals[0]; len(in.EarlierReviews) != 1 || !maps.Equal(in.EarlierReviews[0].Verdicts, remark) {
			t.Fatalf("the approval's card = %+v, want the remark on the raised round's review", in.EarlierReviews)
		}
		var checkin *recordedStep
		for i := range g.recorded {
			if g.recorded[i].step == "checkin" {
				checkin = &g.recorded[i]
			}
		}
		if checkin == nil || checkin.detail["round"] != float64(1) || checkin.detail["decision"] != CheckHandover ||
			!maps.Equal(remark, checkinVerdictsOf(t, checkin.detail)) {
			t.Fatalf("checkin steps = %+v, want round 1's handover with the remark", g.recorded)
		}
	})

	// #836, design section 4 problem 2 and its replay row: the false
	// finding dies at the check-in with the evidence that killed it, the
	// re-review reads the drop record, and no session answers a dropped
	// claim.
	t.Run("#836_false_finding_dropped", func(t *testing.T) {
		finding := "TasksByPR has no implementation, the package will not compile"
		g := newGateHarness(t, func(g *gateHarness) {
			g.taskConfig = askTaskConfig()
			g.reviews = []Review{
				{Verdict: "concerns", Summary: "the claim", Findings: []string{finding}},
				{Verdict: "approve", Summary: "the head answers the finding"},
			}
		})
		drop := map[string]CheckVerdict{finding: {State: CheckDropped,
			Evidence: "store.go:672 at main: func (s *PGStore) TasksByPR", Round: 1}}
		g.onSessions(pushed("s1", "c1"),
			checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: CheckDroppedAll, Verdicts: drop}))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, want done", final)
		}
		if len(g.earlier) != 2 || !slices.Equal(g.rounds, []int{1, 2}) || len(g.earlier[1]) != 1 || !maps.Equal(g.earlier[1][0].Verdicts, drop) {
			t.Fatalf("review calls: rounds %v, earlier %+v; want the re-review reading the drop record", g.rounds, g.earlier)
		}
		if len(g.sessions) != 2 || len(g.approvals) != 1 || len(g.merges) != 1 {
			t.Fatalf("sessions %d, approvals %d, merges %d; want no session on the dropped round, one approval, one merge",
				len(g.sessions), len(g.approvals), len(g.merges))
		}
		if in := g.approvals[0]; len(in.EarlierReviews) != 1 || !maps.Equal(in.EarlierReviews[0].Verdicts, drop) {
			t.Fatalf("the approval's card = %+v, want the drop evidence on its lines", in.EarlierReviews)
		}
	})

	// #839, design section 4 problem 3 and its replay row: the push left
	// the two new tests red, the fund died, the starved pods took no
	// numbers, the cap card asked once, and the change request that
	// answered it reached the granted session's check-in, whose handover
	// carries the fix.
	t.Run("#839_cap_return_carries_the_handover", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = askTaskConfig()
		tests := "TaskFinishTime and TaskPinnedAt fail: the database check forbids the rows they create on purpose"
		change := "reviewer sebastian requested changes: " + tests
		h.onSessions(
			pushed("s1", "c1"),
			SessionResult{Outcome: OutcomeNotStarted, SessionID: "starved", Summary: "the pod never started: the fund was empty"},
			SessionResult{Outcome: OutcomeNotStarted, SessionID: "starved", Summary: "the pod never started: the fund was empty"},
			SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "the gateway refused a call, 0.15 EUR left"},
			SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Summary: "the gateway refused a call, 0.15 EUR left"},
			checkinPushed("s4", "c4", &CheckInOutcome{Decision: CheckHandover,
				Handover: "stamp the test rows, do not drop the check",
				Verdicts: map[string]CheckVerdict{tests: {State: CheckConfirmed, Round: 1}}}))
		h.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{tests}}, ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: tests})
		h.signalAfter(4*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, escalations = %q", final, h.escalated)
		}
		var attempts []int
		for _, in := range h.sessions {
			attempts = append(attempts, in.Attempt)
		}
		if !slices.Equal(attempts, []int{1, 2, 2, 2, 3, 4}) {
			t.Fatalf("attempts = %v, want the starved pods taking no numbers and the granted session as 4", attempts)
		}
		if len(h.escalated) != 1 || !strings.HasPrefix(h.escalated[0], capReached) {
			t.Fatalf("escalations = %q, want the cap card alone", h.escalated)
		}
		ask := h.sessions[5].CheckInAsk
		if ask == nil || !slices.Equal(ask.Findings, []CheckFinding{{Text: tests, Source: "review"}, {Text: change, Source: "changes"}}) {
			t.Fatalf("the granted session's ask = %+v, want the red tests and the change request", ask)
		}
		var handover *recordedStep
		for i := range h.recorded {
			if h.recorded[i].step == "handover" {
				handover = &h.recorded[i]
			}
		}
		if handover == nil || handover.detail["handover"] != "stamp the test rows, do not drop the check" || handover.detail["session"] != "s4" {
			t.Fatalf("handover steps = %+v, want s4's note", h.recorded)
		}
		if handover.at.Before(h.escalatedAt[0]) {
			t.Fatalf("the cap card at %v must precede the granted session at %v", h.escalatedAt[0], handover.at)
		}
		if h.finals[0] != [2]string{"done", "pull request merged"} {
			t.Fatalf("finals = %v, want the merge", h.finals)
		}
	})

	// #838, design section 4 problem 4 and its replay row: the sync with
	// main turned the checks red at the cap; the red check goes to the
	// granted session's check-in with the change request that named the
	// one-line fix, no approval is asked on a red head, and the fresh
	// session makes the fix.
	t.Run("#838_red_checks_never_approve", func(t *testing.T) {
		red := "TestFinishedPage: the stand-in no longer matches the interface"
		fix := "one line: pass the column to the page stand-in"
		change := "reviewer sebastian requested changes: " + fix
		g := newGateHarness(t, func(g *gateHarness) {
			g.taskConfig = askTaskConfig()
			g.branchHead = "c3" // the sync with main left this head unjudged
		})
		g.onSessions(
			SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "no changes to push"},
			SessionResult{Outcome: OutcomeNoPush, SessionID: "s2", Summary: "no changes to push"},
			SessionResult{Outcome: OutcomeNoPush, SessionID: "s3", Summary: "no changes to push"},
			checkinPushed("s4", "c4", &CheckInOutcome{Decision: CheckHandover, Handover: fix,
				Verdicts: map[string]CheckVerdict{red: {State: CheckConfirmed, Round: 1}}}))
		g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}}, ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9, Body: fix})
		g.signalAfter(4*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, escalations = %q", final, g.escalated)
		}
		if len(g.escalated) != 1 || !strings.HasPrefix(g.escalated[0], capReached) {
			t.Fatalf("escalations = %q, want the cap card holding the red check alone", g.escalated)
		}
		ask := g.sessions[3].CheckInAsk
		if ask == nil || !slices.Equal(ask.Findings, []CheckFinding{{Text: red, Source: "review"}, {Text: change, Source: "changes"}}) {
			t.Fatalf("the ask = %+v, want the red check and the one-line fix it asked for", ask)
		}
		if len(g.approvals) != 1 || g.approvals[0].RedChecks {
			t.Fatalf("approvals = %+v, want one, asked only on the green head", g.approvals)
		}
		if len(g.merges) != 1 || g.steps[len(g.steps)-1] != "merge" {
			t.Fatalf("merges = %q, steps = %q, want the merge last, never on the red head", g.merges, g.steps)
		}
		var checkin *recordedStep
		for i := range g.recorded {
			if g.recorded[i].step == "checkin" {
				checkin = &g.recorded[i]
			}
		}
		if checkin == nil || g.escalatedAt[0].After(checkin.at) || checkin.at.After(g.requested[0]) {
			t.Fatalf("order: card %v, check-in %v, approval %v — want the card, its answer, the session, then the merge",
				g.escalatedAt[0], checkin, g.requested[0])
		}
	})

	// #851, design section 4 problem 5 and its replay row: the planner
	// read its agent time away and filed nothing; the time card asks the
	// human, whose answer starts the filing session — the gate moment does
	// not exist on a card with no review, so no check-in runs.
	t.Run("#851_planner_still_asks_the_human", func(t *testing.T) {
		h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
		h.state = RunState{Status: "running", Usage: TaskUsage{AgentTime: 30 * time.Minute, RetryTime: 10 * time.Minute, Attempts: 1}}
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "file what you have", Comment: 801})
		h.file = map[int]bool{1: true}

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, escalated = %q", final, h.escalated)
		}
		question := "the planner used its agent time and its automatic second session without filing a plan: an answer starts one more planner session with 5 minutes of agent time and the answer among its findings"
		if !slices.Equal(h.escalated, []string{question}) {
			t.Fatalf("escalations = %q, want the time card alone", h.escalated)
		}
		if len(h.plans) != 1 || !slices.Equal(h.plans[0], []string{"writer sebastian asked for changes to the plan: file what you have"}) {
			t.Fatalf("plans = %q, want one filing session with the answer among its findings", h.plans)
		}
		if len(h.sessions) != 0 {
			t.Fatalf("sessions = %d, want none: a plan card runs no coding pod", len(h.sessions))
		}
		for _, r := range h.recorded {
			if r.step == "checkin" || r.step == "handover" {
				t.Fatalf("recorded = %+v, want no check-in without a review", h.recorded)
			}
		}
	})

	// #856, design section 4 problem 6 and its replay row: the waiting
	// cards' workflows were alive by design; the sweep reads liveness and
	// asks nothing — no nudge, no escalation, no check-in.
	t.Run("#856_waiting_stays_waiting", func(t *testing.T) {
		rig := newStaleRig(t, map[string]time.Duration{"t-deps": 5 * time.Hour})
		nudged := map[string]time.Time{"t-deps": time.Now().Add(-StaleWindow - time.Minute)}
		StaleSweep(context.Background(), rig.store, rig.wf, rig.in, workflowsRunning("t-deps"), nudged)

		if task, _, _ := rig.store.Task(context.Background(), "t-deps"); task.Status != "waiting-deps" {
			t.Fatalf("t-deps = %s, want still waiting-deps: its workflow waits by design", task.Status)
		}
		if _, ok := nudged["t-deps"]; ok {
			t.Fatal("the nudge memory did not clear")
		}
		if len(rig.store.intakeEvents) != 0 || len(rig.store.alerts) != 0 || len(*rig.comments) != 0 || len(rig.wf.signals) != 0 {
			t.Fatalf("the sweep acted: %+v %v %q", rig.store.intakeEvents, *rig.comments, rig.wf.signals)
		}
	})

	// #853, design section 4 problem 7 and its replay row: the request for
	// changes stands above the dated cap answer — it reaches the next
	// session's findings and its check-in, and the old answer rides along
	// below it.
	t.Run("#853_change_request_reaches_the_session", func(t *testing.T) {
		change := "reviewer sebastian requested changes: the four findings the lab run named"
		old := "sebastian, to \"" + capReached + "\": change nothing and end the session"
		g := newGateHarness(t, func(g *gateHarness) {
			g.taskConfig = askTaskConfig()
			g.state = RunState{Status: "running", Answers: []string{old}}
		})
		g.onSessions(pushed("s1", "c1"),
			checkinPushed("s2", "c2", &CheckInOutcome{Decision: CheckHandover,
				Handover: "the four findings first, the cap answer stands below them",
				Verdicts: map[string]CheckVerdict{change: {State: CheckConfirmed, Round: 1}}}))
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "sebastian", PR: 9,
			Body: "the four findings the lab run named"})
		g.signalAfter(4*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := g.run(t); final != "done" {
			t.Fatalf("final = %q, escalations = %q", final, g.escalated)
		}
		in := g.sessions[1]
		if !slices.Equal(in.Findings, []string{change}) || in.CheckInAsk == nil ||
			!slices.Equal(in.CheckInAsk.Findings, []CheckFinding{{Text: change, Source: "changes"}}) {
			t.Fatalf("the session = %+v, want the change request as its finding and in its ask", in)
		}
		if !slices.Equal(in.Answers, []string{old}) {
			t.Fatalf("the session's answers = %q, want the dated cap answer still riding along", in.Answers)
		}
		var handover *recordedStep
		for i := range g.recorded {
			if g.recorded[i].step == "handover" {
				handover = &g.recorded[i]
			}
		}
		if handover == nil || handover.detail["session"] != "s2" || len(g.merges) != 1 {
			t.Fatalf("handover = %+v, merges = %q; want s2 acting on the request and one merge", handover, g.merges)
		}
	})

	// #854, design section 4 problem 8 and its replay row: of eight
	// findings seven were false; the check-in drops them with evidence and
	// confirms the one; the confirm buys the fixing session, and the
	// twice-confirmed finding goes to the human with both records on the
	// card.
	t.Run("#854_seven_of_eight_dropped", func(t *testing.T) {
		real := "the save endpoint never checks the editor's lock"
		eight := []string{
			"the card drawer has no focus trap",
			"the editor's lock is never released on close",
			"the documents route skips the auth middleware",
			"the save endpoint has no test",
			"the card drawer opens a modal, not a drawer",
			"the pulse line has no hover state",
			"the save endpoint rewrites the whole document on every keypress",
			real,
		}
		first := map[string]CheckVerdict{real: {State: CheckConfirmed, Round: 1}}
		for f, ev := range map[string]string{
			eight[0]: "the diff's file list carries it: the second part of the diff has it",
			eight[1]: "the diff's file list carries it: the second part of the diff has it",
			eight[2]: "the diff's file list carries it: the second part of the diff has it",
			eight[3]: "the diff's file list carries it: the second part of the diff has it",
			eight[4]: "the prototype's card drawer shows the drawer",
			eight[5]: "the prototype's pulse card shows the hover",
			eight[6]: "the code reads it at console/editor.go: the lock is taken once",
		} {
			first[f] = CheckVerdict{State: CheckDropped, Evidence: ev, Round: 1}
		}
		g := newGateHarness(t, func(g *gateHarness) {
			g.taskConfig = askTaskConfig()
			g.reviews = []Review{
				{Verdict: "concerns", Summary: "eight findings", Findings: eight},
				{Verdict: "concerns", Summary: "the same eight", Findings: eight},
			}
		})
		second := map[string]CheckVerdict{real: {State: CheckConfirmed, Round: 2}}
		g.onSessions(pushed("s1", "c1"),
			checkinPushed("s2", "c2", &CheckInOutcome{Decision: CheckHandover,
				Handover: "fix the lock check alone; the other seven name code that is there",
				Verdicts: first}),
			checkinStopped("s3", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman,
				Reason: "confirmed 1 times: " + real, Verdicts: second}))
		g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})

		if final := g.run(t); final != "closed" {
			t.Fatalf("final = %q, escalations = %q", final, g.escalated)
		}
		if len(g.sessions) != 3 {
			t.Fatalf("sessions = %d, want two coding sessions — the push and the fix the confirm bought — and the check-in's pod", len(g.sessions))
		}
		ask := g.sessions[2].CheckInAsk
		if ask == nil || len(ask.Findings) != 8 {
			t.Fatalf("the ask = %+v, want all eight findings back", ask)
		}
		for _, f := range ask.Findings {
			if f.Text == real {
				if f.PriorConfirmed != 1 || f.PriorDropped != 0 {
					t.Fatalf("the real finding = %+v, want confirmed once", f)
				}
				continue
			}
			if f.PriorDropped != 1 || f.PriorConfirmed != 0 {
				t.Fatalf("the false finding %q = %+v, want dropped once", f.Text, f)
			}
		}
		if !slices.Equal(g.escalated, []string{"check-in: gate: confirmed 1 times: " + real}) {
			t.Fatalf("escalations = %q, want the twice-confirmed finding asked once", g.escalated)
		}
		var checkins []recordedStep
		for _, r := range g.recorded {
			if r.step == "checkin" {
				checkins = append(checkins, r)
			}
		}
		if len(checkins) != 2 || checkins[0].detail["round"] != float64(1) || checkins[1].detail["round"] != float64(2) ||
			checkins[1].detail["decision"] != CheckHuman || !maps.Equal(first, checkinVerdictsOf(t, checkins[0].detail)) ||
			!maps.Equal(second, checkinVerdictsOf(t, checkins[1].detail)) {
			t.Fatalf("checkin steps = %+v, want both records for the human's card", checkins)
		}
		if len(g.approvals) != 0 || len(g.merges) != 0 {
			t.Fatalf("approvals %d, merges %d; want never a merge", len(g.approvals), len(g.merges))
		}
	})
}

// checkinVerdictsOf reads a checkin step's verdict map back from the
// activity boundary's JSON round-trip.
func checkinVerdictsOf(t *testing.T, detail map[string]any) map[string]CheckVerdict {
	t.Helper()
	var verdicts map[string]CheckVerdict
	b, err := json.Marshal(detail["verdicts"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &verdicts); err != nil {
		t.Fatal(err)
	}
	return verdicts
}

// liveGateHarness is the gate harness whose sessions are real: every
// SessionInput the workflow builds drives the real RunSession activity —
// the real supervisor, its detectors, the check-in plumbing and the brief
// captures — against a scripted worker endpoint and a scripted check-in
// model, each session reading the pod log sessionBodies hands out in
// order. results keeps each session's own result in the same order.
type liveGateHarness struct {
	*gateHarness
	runner        *Runner
	worker        *fakeWorkerEndpoint
	hub           *forgejoHub
	chat          *scriptedChat
	objects       *fakeObjectStore
	store         *fakeStore
	sessionBodies [][]string
	results       []SessionResult
	beforeSession func(n int) // called before session n runs, none kept
}

func newLiveGateHarness(t *testing.T) *liveGateHarness {
	t.Helper()
	l := &liveGateHarness{gateHarness: newGateHarness(t, func(g *gateHarness) { g.taskConfig = askTaskConfig() })}
	l.objects = &fakeObjectStore{}
	l.runner, l.hub, l.store = newTestRunner(t, supervisedTask("t1"), newFakeKube(), l.objects)
	l.worker = &fakeWorkerEndpoint{status: http.StatusNoContent}
	l.worker.start(t, l.runner)
	l.chat = &scriptedChat{}
	l.runner.checkin = l.chat
	l.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			l.sessions = append(l.sessions, in)
			if l.beforeSession != nil {
				l.beforeSession(len(l.sessions) - 1)
			}
			var bodies []string
			if n := len(l.sessions); n <= len(l.sessionBodies) {
				bodies = l.sessionBodies[n-1]
			}
			res, err := runSession(t, ctx, l.runner, in, nil, bodies...)
			l.results = append(l.results, res)
			return res, err
		})
	return l
}

// The three check-in moments the eight do not carry, replayed beside them:
// the in-session trip (plan E's session moment), the spend trip's
// handover, and the dead session's ask. Unlike the eight, whose sessions
// are recorded results, these run the real RunSession activity inside the
// gate harness, so every ask, answer, step and session count below is the
// landed code's own.
func TestLadderReplaysTheCheckinMoments(t *testing.T) {
	// plan E's session moment, the churn round: a session circles writes —
	// a.go returns to a content it had while `go test ./...` fails again
	// (flip_flop), and after the delivered message the circle goes on
	// without growing the diff (churn).
	//
	// The ledger: one session; two detector trips, each asked: flip_flop
	// answered message with the correction, delivered through the worker's
	// /message; churn after it, stuck, its statement carrying the
	// correction and the stuck line, answered handover, which on a stuck
	// trip is the stuck rule's climb; the climb in place put model_switched
	// on the timeline and the judge's note in the worker's message; the
	// climbed session itself pushed c1; one merge, no escalation.
	//
	// Reshape against the letter: the letter's stuck trip climbs with no
	// second conversation; here the second conversation says the session is
	// stuck and code climbs, so the guidance is in the second ask, as the
	// letter's assert wants. And the letter's "retried session pushes" is
	// the same session: a climb needs canClimb, and the climb in place
	// (roadmap 222) is what records the letter's "the climb in the
	// timeline"; a stop without it records no climb step at all.
	t.Run("churn_round_in_session", func(t *testing.T) {
		correction := "the flip is the test's own clock: stub it and do not wait"
		note := "the circle on a.go went on after the correction; stub the clock in a.go first and leave f0.go to f11.go alone"
		l := newLiveGateHarness(t)
		l.chat.answers = []CheckinResponse{
			{Text: `{"decision":"message","guidance":"` + correction + `"}`},
			{Text: `{"decision":"handover","handover":"` + note + `"}`},
		}
		logs := envLine(fileEvent("a.go", "h1")) + "\n" +
			envLine(cmdEvent("go test ./...", 1, "FAIL: TestLinks")) + "\n" +
			envLine(fileEvent("a.go", "h2")) + "\n" +
			envLine(fileEvent("a.go", "h1")) + "\n" +
			envLine(cmdEvent("go test ./...", 1, "FAIL: TestLinks"))
		for i := range 12 {
			logs += "\n" + envLine(fileEvent(fmt.Sprintf("f%d.go", i), fmt.Sprintf("k%d", i)))
		}
		l.sessionBodies = [][]string{{logs + "\n" + pushedLine("c1") + "\n" + resultLine(true, "pushed branch factory/t1 at c1")}}
		l.onChecks(ChecksResult{State: ChecksSuccess})
		l.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := l.run(t); final != "done" {
			t.Fatalf("final = %q, escalations = %q", final, l.escalated)
		}
		if len(l.sessions) != 1 || l.chat.calls != 2 {
			t.Fatalf("sessions = %d, conversations = %d; want the one session the climb kept, one conversation per trip", len(l.sessions), l.chat.calls)
		}
		if res := l.results[0]; res.Outcome != OutcomePushed || res.Commit != "c1" || res.Model != "factory/big" ||
			res.Summary != "pushed branch factory/t1 at c1" {
			t.Fatalf("the session's result = %+v, want the climbed session's own push", res)
		}
		if !strings.Contains(l.chat.gots[0].Input, "flip_flop") || strings.Contains(l.chat.gots[0].Input, "Stuck rule:") {
			t.Fatalf("the first ask's statement = %.80s, want the flip_flop trip named, not stuck", l.chat.gots[0].Input)
		}
		// 15 file changes, the default churn: a.go three times, f0.go to f11.go
		tripped := "the detector churn tripped (15 file changes since the diff against the base last grew (largest: 0 lines), and no command newly passes)"
		if !strings.Contains(l.chat.gots[1].Input, tripped) || !strings.Contains(l.chat.gots[1].Input, "Guidance delivered to the agent this session:\n- "+correction+"\n"+
			"Stuck rule: after a message, the churn detector tripped.\n- on this trip a handover moves the work to the stronger model, with your note as its next instruction.\n") {
			t.Fatalf("the second ask's statement = %s, want the churn trip, the correction and the stuck rule", l.chat.gots[1].Input)
		}
		l.worker.mu.Lock()
		defer l.worker.mu.Unlock()
		climb := "POST /message factoryd: this session continues on the stronger model, big: " + note + ". Carry on from where you are."
		if len(l.worker.texts) != 3 || l.worker.texts[0] != "POST /brief " ||
			l.worker.texts[1] != "POST /message "+correction || l.worker.texts[2] != climb {
			t.Fatalf("worker got %q, want the brief, the correction, the climb with the judge's note", l.worker.texts)
		}
		tl := string(objGet(l.objects, "sessions/t1/timeline.jsonl"))
		if !strings.Contains(tl, `"step":"model_switched"`) || !strings.Contains(tl, `"to":"big"`) ||
			!strings.Contains(tl, `"trip":"churn"`) {
			t.Fatalf("timeline = %s, want the climb in place", tl)
		}
		if !strings.Contains(tl, `"guidance":"`+correction+`"`) {
			t.Fatalf("timeline = %s, want the first ask's correction on its checkin step", tl)
		}
		step := timelineStep(t, l.objects, "t1", "checkin")
		if step["decision"] != CheckHandover || step["detector"] != "churn" || step["called"] != true || step["reason"] != note {
			t.Fatalf("last checkin step = %v, want the judge's handover on churn", step)
		}
		if len(l.merges) != 1 || len(l.escalated) != 0 {
			t.Fatalf("merges %d, escalations %q; want the card merged, nothing asked of the human", len(l.merges), l.escalated)
		}
	})

	// plan E's session moment, the spend round: the card is past the
	// no-push share of its budget with nothing pushed, the trip's check-in
	// reads the workspace, finds the work uncommittable, and answers
	// handover; the next session starts from the note and the round
	// verifies.
	//
	// The ledger: no asks of the human; one trip (spend_no_push, 3.50 of
	// 5.00 EUR spent, the line 60 percent); one conversation, two answers —
	// the workspace read through the worker's /tool, then the handover; the
	// session stopped with the note riding the result's check-in; the
	// workflow wrote the checkin and handover steps and put the note in the
	// next session's brief; that session pushed c2; one merge.
	t.Run("spend_without_push", func(t *testing.T) {
		note := "the workspace holds the parser rewrite; nothing here is committable until vendoring lands"
		l := newLiveGateHarness(t)
		l.chat.answers = []CheckinResponse{
			{ToolName: "read_file", ToolArgs: map[string]any{"path": "/workspace/parser/parser.go"}},
			{Text: `{"decision":"handover","handover":"` + note + `"}`},
		}
		l.sessionBodies = [][]string{
			{hangLogs},
			{pushedLine("c2") + "\n" + resultLine(true, "pushed branch factory/t1 at c2")},
		}
		tk := l.store.tasks["t1"]
		tk.BudgetEUR, tk.SpentEUR = 5, 3.5 // 70 percent, past the default 60
		l.store.tasks["t1"] = tk
		l.runner.tick = 50 * time.Millisecond
		l.runner.cfg.Supervisor.NoProgressAfter = time.Hour
		l.onChecks(ChecksResult{State: ChecksSuccess})
		l.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := l.run(t); final != "done" {
			t.Fatalf("final = %q, escalations = %q", final, l.escalated)
		}
		if len(l.sessions) != 2 || l.chat.calls != 2 {
			t.Fatalf("sessions = %d, conversations = %d; want the spend trip's session and the note's, the one conversation's two answers", len(l.sessions), l.chat.calls)
		}
		res := l.results[0]
		if res.Outcome != OutcomeNoPush || res.Trip != string(TripSpendNoPush) || res.Commit != "" ||
			res.Summary != "check-in: handover after spend_no_push" {
			t.Fatalf("the trip's outcome = %+v, want the handover stop with nothing pushed", res)
		}
		if res.CheckIn == nil || res.CheckIn.Decision != CheckHandover || res.CheckIn.Handover != note ||
			res.CheckIn.Record == nil || res.CheckIn.Record.Moment != CheckInSession ||
			res.CheckIn.Record.Session != res.SessionID {
			t.Fatalf("the riding check-in = %+v, want the note and the session's own record", res.CheckIn)
		}
		if !strings.Contains(l.chat.gots[0].Input, "spend_no_push") {
			t.Fatalf("the ask's statement = %.80s, want the spend trip named", l.chat.gots[0].Input)
		}
		if len(l.worker.tools) != 1 || !strings.HasPrefix(l.worker.tools[0], "read_file ") {
			t.Fatalf("tool calls = %q, want the workspace read", l.worker.tools)
		}
		l.worker.mu.Lock()
		noteBriefs := 0
		for _, got := range l.worker.texts {
			noteBriefs += min(strings.Count(got, note), 1)
		}
		l.worker.mu.Unlock()
		if l.worker.texts[0] != "POST /brief " || noteBriefs != 1 {
			t.Fatalf("worker got %q, want the empty start brief alone and the note on the next session's brief", l.worker.texts)
		}
		in := l.sessions[1]
		if in.Handover != note || in.CheckInAsk != nil || in.ParentSessionID != res.SessionID {
			t.Fatalf("the next session's input = %+v, want the note handed over, no ask", in)
		}
		var checkin, handover *recordedStep
		for i := range l.recorded {
			switch l.recorded[i].step {
			case "checkin":
				checkin = &l.recorded[i]
			case "handover":
				handover = &l.recorded[i]
			}
		}
		if checkin == nil || checkin.detail["decision"] != CheckHandover || handover == nil ||
			handover.detail["handover"] != note || handover.detail["session"] != res.SessionID {
			t.Fatalf("recorded = %+v, want the checkin and handover steps carrying the note", l.recorded)
		}
		if len(l.merges) != 1 {
			t.Fatalf("merges = %d, want the note's work verified and merged", len(l.merges))
		}
	})

	// plan E's dead moment: a session runs out its time with work in
	// progress and no push, the cap grants it no extra time, its stop ends
	// the round dead, and the next session's check-in reads the dead
	// session's record and writes the handover the following work starts
	// from.
	//
	// The ledger: one dead session — the start rung's cheap time cap passed
	// without a push, its climb undeliverable, so the session stopped with
	// no check-in and the round turned dead; no conversation in it; the
	// next session asked at the dead moment, parent the dead session's id,
	// stop its summary line; the dead record went to the pod through
	// /records and its ask named the stop; the answer's handover rode the
	// brief and the reading session pushed c3; the handover step names that
	// reader; one merge.
	//
	// Reshape against the letter: the letter's dead session runs to its
	// wall clock, but through landed code a wall-clock session never
	// reaches a next session's dead ask — with nothing pushed the keep-work
	// time cap fails the card at once, and anything pushed verifies or
	// parks at a cap card whose findings make the next ask a gate ask. The
	// replay's dead session therefore runs out the start rung's cheap time
	// cap instead — the same ran-out-of-time, work in progress, nothing
	// pushed shape — and stops because the climb its cap decided cannot be
	// delivered.
	t.Run("dead_session", func(t *testing.T) {
		note := "the dead session left the parser half rewritten; take its record and finish the vendor fix"
		l := newLiveGateHarness(t)
		l.chat.answers = []CheckinResponse{{Text: `{"decision":"handover","handover":"` + note + `"}`}}
		l.sessionBodies = [][]string{
			{hangLogs},
			{pushedLine("c3") + "\n" + resultLine(true, "pushed branch factory/t1 at c3")},
		}
		l.runner.cheapCap = 150 * time.Millisecond
		l.runner.tick = 50 * time.Millisecond
		l.runner.cfg.Supervisor.NoProgressAfter = time.Hour
		l.worker.status = http.StatusNotImplemented // the climb its cap decided cannot be delivered
		l.beforeSession = func(n int) {
			if n == 1 {
				l.worker.mu.Lock()
				l.worker.status = http.StatusNoContent
				l.worker.mu.Unlock()
			}
		}
		l.onChecks(ChecksResult{State: ChecksSuccess})
		l.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})

		if final := l.run(t); final != "done" {
			t.Fatalf("final = %q, escalations = %q", final, l.escalated)
		}
		if len(l.sessions) != 2 || l.chat.calls != 1 {
			t.Fatalf("sessions = %d, conversations = %d; want the dead session without a conversation and the reader with one", len(l.sessions), l.chat.calls)
		}
		dead := l.results[0]
		stop := "check-in: retry_stronger after cheap_cap: the cheap rung's 150ms passed without a push"
		if dead.Outcome != OutcomeRetryStronger || dead.Trip != string(TripCheapCap) || dead.CheckIn != nil ||
			dead.Commit != "" || dead.Summary != stop {
			t.Fatalf("the dead session = %+v, want the cheap cap's stop with no check-in", dead)
		}
		failed := "The switch to the stronger model failed (the harness takes no message into a running session): the session stopped, and the next one starts on it."
		l.hub.mu.Lock()
		comments := slices.Clone(l.hub.comments)
		l.hub.mu.Unlock()
		if !slices.ContainsFunc(comments, func(c string) bool { return strings.Contains(c, failed) }) {
			t.Fatalf("comments = %q, want the cheap cap's stop explained: %q", comments, failed)
		}
		ask := l.sessions[1].CheckInAsk
		if ask == nil || ask.Moment != CheckInDead || ask.ParentSession != dead.SessionID || ask.Stop != stop {
			t.Fatalf("the dead ask = %+v, want the dead moment, the dead session as parent, its stop", ask)
		}
		if l.sessions[1].ParentSessionID != dead.SessionID || l.sessions[1].Model != "factory/big" {
			t.Fatalf("the reader's input = %+v, want the dead session as parent and the stronger rung", l.sessions[1])
		}
		if !strings.Contains(l.chat.gots[0].Input, stop) || !strings.Contains(l.chat.gots[0].Input, dead.SessionID) {
			t.Fatalf("the statement = %.120s, want the stop line and the dead session named", l.chat.gots[0].Input)
		}
		l.worker.mu.Lock()
		records, noteBriefs, climbs := 0, 0, 0
		for _, got := range l.worker.texts {
			switch {
			case strings.HasPrefix(got, "POST /records"):
				records++
				if !strings.Contains(got, dead.SessionID) || !strings.Contains(got, `"detector":"cheap_cap"`) {
					t.Fatalf("the record delivery = %.160s, want the dead session's record with its trip", got)
				}
			case strings.HasPrefix(got, "POST /brief"):
				noteBriefs += min(strings.Count(got, note), 1)
			case strings.Contains(got, "continues on the stronger model"):
				climbs++
			}
		}
		l.worker.mu.Unlock()
		if records != 1 || noteBriefs != 1 || climbs != 1 {
			t.Fatalf("records %d, note briefs %d, climbs %d; want the dead record delivered once, the note on the reader's brief, the refused climb once", records, noteBriefs, climbs)
		}
		var handover *recordedStep
		for i := range l.recorded {
			if l.recorded[i].step == "handover" {
				handover = &l.recorded[i]
			}
		}
		if handover == nil || handover.detail["handover"] != note || handover.detail["session"] != l.results[1].SessionID {
			t.Fatalf("handover steps = %+v, want the reader's note", l.recorded)
		}
		if res := l.results[1]; res.Outcome != OutcomePushed || res.Commit != "c3" || res.CheckIn == nil ||
			res.CheckIn.Decision != CheckHandover || res.CheckIn.Record.Moment != CheckInDead {
			t.Fatalf("the reader's result = %+v, want the push with the dead check-in riding", res)
		}
		if len(l.merges) != 1 {
			t.Fatalf("merges = %d, want the record's work verified and merged", len(l.merges))
		}
	})
}
