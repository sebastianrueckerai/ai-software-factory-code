package factoryd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// applies is action name's Applies on f with r.
func applies(t *testing.T, name string, f CaseFacts, r Resolution) string {
	t.Helper()
	a, ok := ActionByName(name)
	if !ok {
		t.Fatalf("no action %s", name)
	}
	r.Action = name
	return a.Applies(f, r)
}

// offeredNames are the names of the actions Offered gives rung on f.
func offeredNames(f CaseFacts, rung int) []string {
	var out []string
	for _, a := range Offered(f, rung) {
		out = append(out, a.Name)
	}
	return out
}

// requireStoredDecisionsOnTheRungsNames guards the stored-decision proofs:
// every name a moment offers is one of the rungs' action names, and the
// names the store wrote before the change — human, dropped_all, message —
// are none of the rungs' own, folding onto hand_up, reverify and continue
// the way the run reads them back, so the stored rows and the rungs hold
// one vocabulary.
// rungReadBackNames are the decision names the run reads back: every name
// the three moments offer, gathered once.
func rungReadBackNames() []string {
	var out []string
	for _, moment := range []string{CheckInSession, CheckInGate, CheckInDead} {
		out = append(out, momentDecisions(moment, true)...)
	}
	return out
}

func requireStoredDecisionsOnTheRungsNames(t *testing.T) {
	t.Helper()
	readBack := rungReadBackNames()
	for _, c := range []struct{ legacy, rung string }{
		{"human", RungHandUp},
		{"dropped_all", ActReverify},
		{"message", ActContinue},
	} {
		if _, ok := ActionByName(c.legacy); ok {
			t.Fatalf("the stored decision %q is still a name on the rungs' action list", c.legacy)
		}
		if !slices.Contains(readBack, c.rung) {
			t.Fatalf("the stored decision %q folds onto %q, which is no name the run reads back", c.legacy, c.rung)
		}
	}
	for _, moment := range []string{CheckInSession, CheckInGate, CheckInDead} {
		for _, d := range momentDecisions(moment, true) {
			if d == RungHandUp {
				continue
			}
			if _, ok := ActionByName(d); !ok {
				t.Fatalf("the %s moment offers %q, which is no name on the rungs' action list", moment, d)
			}
		}
	}
}

// A resume needs no session running, an attempt under the cap, agent time
// a session can start on and the money an opening needs, each refusal with
// the answers its stop's face holds; caps or a floor that could not be read
// refuse. A handover needs the same and names its model. By hand: the card
// has no branch, so no stop holds a successor, and with no spend limit set
// it stands at its limit, so the money stop holds the raise and no fund.
func TestResumeAppliesOnlyWithRoom(t *testing.T) {
	const capAnswers = " (the card's answers at this stop: `extend`, `close` and `note`)"
	const moneyAnswers = " (the card's answers at this stop: `raise_ceiling`, `close` and `note`)"
	room := CaseFacts{Attempts: 2, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3}
	with := func(change func(*CaseFacts)) CaseFacts {
		f := room
		change(&f)
		return f
	}
	for _, c := range []struct {
		name, action string
		f            CaseFacts
		r            Resolution
		want         string
	}{
		{"room", ActResume, room, Resolution{}, ""},
		{"at the cap", ActResume, with(func(f *CaseFacts) { f.Attempts = 3 }), Resolution{}, "the session cap of 3 is reached" + capAnswers},
		{"below the floor", ActResume, with(func(f *CaseFacts) { f.LeftEUR = 0.2 }), Resolution{}, "0.20 EUR is left for a session and an opening needs 0.30 EUR" + moneyAnswers},
		// 0.70 left less the 0.40 gate reserve is 0.30 for a session, under the 0.60 floor
		{"under the reserve", ActResume, with(func(f *CaseFacts) { f.LeftEUR, f.ReserveEUR, f.FloorEUR = 0.7, 0.4, 0.6 }), Resolution{},
			"0.30 EUR is left for a session and an opening needs 0.60 EUR" + moneyAnswers},
		{"running", ActResume, with(func(f *CaseFacts) { f.Running = true }), Resolution{}, "a session is running"},
		// 1200 s cap, 1100 s used: 100 s left, under the 5 minutes a session needs
		{"time short", ActResume, with(func(f *CaseFacts) { f.AgentTimeS = 1100 }), Resolution{},
			"the agent time cap of 20 minutes leaves 2 minutes and a session needs 5 minutes" + capAnswers},
		{"time spent", ActResume, with(func(f *CaseFacts) { f.AgentTimeS = 1200 }), Resolution{}, "the agent time cap of 20 minutes is reached" + capAnswers},
		{"cap unread", ActResume, with(func(f *CaseFacts) { f.AgentTimeCapS, f.Partial = 0, []string{"config"} }), Resolution{},
			"the agent time cap could not be read: try again in a minute, or close the card"},
		{"floor unread", ActResume, with(func(f *CaseFacts) { f.FloorEUR, f.Partial = 0, []string{"floor"} }), Resolution{},
			"what an opening needs could not be read: try again in a minute, or close the card"},
		{"handover same", ActHandover, room, Resolution{Args: map[string]any{"model": "same"}}, ""},
		{"handover at the cap", ActHandover, with(func(f *CaseFacts) { f.Attempts = 3 }), Resolution{Args: map[string]any{"model": "stronger"}},
			"the session cap of 3 is reached" + capAnswers},
		{"handover to no rung", ActHandover, room, Resolution{Args: map[string]any{"model": "best"}},
			`a handover names its model as same, stronger or cheaper, not "best"`},
	} {
		if got := applies(t, c.action, c.f, c.r); got != c.want {
			t.Errorf("%s: %s applies = %q, want %q", c.name, c.action, got, c.want)
		}
	}
}

// A report card's stop offers the human no handover, no extend and no
// replan, which its run takes none of; it offers the resume, the continue
// and the close the run carries out. The same stop on a work card offers
// all three.
func TestReportStopOffersNoHandoverExtendOrReplan(t *testing.T) {
	work := CaseFacts{Kind: CaseGateStop, Gate: ActivityRunSession, Attempts: 1, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3}
	report := work
	report.Gate, report.Report = ActivityRunReport, true
	for _, c := range []struct {
		name    string
		f       CaseFacts
		offered bool
	}{{"work card", work, true}, {"report card", report, false}} {
		got := offeredNames(c.f, 3)
		for _, a := range []string{ActResume, ActContinue, ActClose} {
			if !slices.Contains(got, a) {
				t.Errorf("%s: offered %q, want %s among them", c.name, got, a)
			}
		}
		for _, a := range []string{ActHandover, ActExtend, ActReplan} {
			if slices.Contains(got, a) != c.offered {
				t.Errorf("%s: offered %q, want %s offered %v", c.name, got, a, c.offered)
			}
		}
	}
	for _, a := range []string{ActHandover, ActExtend, ActReplan} {
		if got := applies(t, a, report, Resolution{Args: map[string]any{"model": "stronger", "sessions": 1}}); got != reportRuns {
			t.Errorf("%s on a report card applies = %q, want %q", a, got, reportRuns)
		}
	}
}

// Route to approval takes an open pull request whose head is green and
// approved by its newest review.
func TestRouteApprovalNeedsGreenAndApproved(t *testing.T) {
	ready := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true}
	pending, unapproved, closed := ready, ready, ready
	pending.Checks, unapproved.ReviewApproved, closed.PROpen = "pending", false, false
	for _, c := range []struct {
		name string
		f    CaseFacts
		want string
	}{
		{"ready", ready, ""},
		{"pending", pending, "the checks on the head are pending, not green"},
		{"unapproved", unapproved, "the newest review of the head did not approve it"},
		{"no pull request", closed, "no pull request is open"},
	} {
		if got := applies(t, ActRouteApproval, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: route_approval applies = %q, want %q", c.name, got, c.want)
		}
	}
}

// Re-run checks takes a red head once.
func TestRerunChecksOnceOnAHead(t *testing.T) {
	red := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "red"}
	again, green := red, red
	again.RerunOnHead, green.Checks = true, "green"
	for _, c := range []struct {
		name string
		f    CaseFacts
		want string
	}{
		{"red", red, ""},
		{"run again before", again, "the checks on this head were run again once already"},
		{"green", green, "the checks on the head are green, not red"},
	} {
		if got := applies(t, ActRerunChecks, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: rerun_checks applies = %q, want %q", c.name, got, c.want)
		}
	}
}

// A draw after one that bought nothing (no push since it, the head, the
// checks and the findings as they were) is refused for an extend, a fund
// and a draw beside any action; a push, a new head, new checks or new
// findings lift the refusal. A draw whose row recorded no findings is
// weighed on the head and the checks, and one that recorded no head and
// checks never refuses.
func TestExtendRefusesWhenTheLastDrawBoughtNothing(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	stuck := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "red", Findings: []string{idA}, LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red",
		DrawFindings: []string{idA}, Attempts: 2,
		PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		BudgetEUR: 5, CeilingEUR: 10}
	const refused = "the last draw at 10:04 UTC bought nothing, with no push since and the head, the checks and the findings unchanged: " +
		"hand over, start a successor, replan or close"
	extend := Resolution{Args: map[string]any{"sessions": 1}}
	if got := applies(t, ActExtend, stuck, extend); got != refused {
		t.Errorf("extend applies = %q, want %q", got, refused)
	}
	if got := applies(t, ActFund, stuck, Resolution{Args: map[string]any{"eur": 1.0}}); got != refused {
		t.Errorf("fund applies = %q, want %q", got, refused)
	}
	note, _ := ActionByName(ActNote)
	if got := note.refusal(stuck, 2, Resolution{Action: ActNote, Note: "one more", Fund: &policy.Draw{Sessions: 1}}); got != refused {
		t.Errorf("a note with a draw beside it = %q, want %q", got, refused)
	}
	unweighed := stuck
	unweighed.DrawFindings, unweighed.Findings = nil, []string{idB}
	if got := applies(t, ActExtend, unweighed, extend); got != refused {
		t.Errorf("a draw that recorded no findings: extend applies = %q, want %q", got, refused)
	}
	pushed, moved, rechecked, found, unrecorded := stuck, stuck, stuck, stuck, stuck
	pushed.PushedSinceDraw, moved.Head, rechecked.Checks, found.Findings, unrecorded.LastDrawAt = true, "h2", "green", []string{idA, idB}, nil
	for name, f := range map[string]CaseFacts{"a push since": pushed, "a new head": moved, "new checks": rechecked, "a new finding": found,
		"no recorded state": unrecorded} {
		if got := applies(t, ActExtend, f, extend); got != "" {
			t.Errorf("%s: extend applies = %q, want it to apply", name, got)
		}
	}
}

// Rungs 0 and 1 draw within the card's draw bound and the pool, rung 2
// within the pool, the human past both; a fund stays under the ceiling on
// every rung. Money compares in cents.
func TestDrawWithinByRung(t *testing.T) {
	f := CaseFacts{PoolLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 60 - 59.7}, BudgetEUR: 5, CeilingEUR: 8}
	one := policy.Draw{Sessions: 1}
	if got, want := drawWithin(f, 1, one, policy.Draw{}, 0, false), "the card's draw bound leaves 0 sessions, 0 seconds of agent time and 0.00 EUR: the factory agent or the human may draw more. "+
		"This draw does not fit in sessions (asked 1, left 0), and nothing of it fits: hand the draw up (`hand_up`) with it as the recommendation"; got != want {
		t.Errorf("rung 1 = %q, want %q", got, want)
	}
	if got := drawWithin(f, 2, one, policy.Draw{}, 0, false); got != "" {
		t.Errorf("rung 2 within the pool = %q, want none", got)
	}
	past := policy.Draw{Sessions: 3}
	if got, want := drawWithin(f, 2, past, policy.Draw{}, 0, false), "the escalation pool leaves 2 sessions, 1 hour of agent time and 0.30 EUR this month: only the human draws more. "+
		"This draw does not fit in sessions (asked 3, left 2). The largest draw that fits is 2 sessions, 0 seconds of agent time and 0.00 EUR: ask for that"; got != want {
		t.Errorf("rung 2 past the pool = %q, want %q", got, want)
	}
	if got := drawWithin(f, 3, past, policy.Draw{}, 0, false); got != "" {
		t.Errorf("rung 3 past the pool = %q, want none", got)
	}
	// 60 - 59.7 is 0.29999999999999716 in floats: 30 cents, which 0.30 EUR fits
	if got := drawWithin(f, 2, policy.Draw{EUR: 0.3}, policy.Draw{}, 0, true); got != "" {
		t.Errorf("rung 2 drawing the pool's last 30 cents = %q, want none", got)
	}
	if got, want := drawWithin(f, 3, policy.Draw{EUR: 4}, policy.Draw{}, 0, true), "4.00 EUR more takes the budget of 5.00 EUR past the card's ceiling of 8.00 EUR: raise the ceiling first"; got != want {
		t.Errorf("rung 3 past the ceiling = %q, want %q", got, want)
	}
	if got := drawWithin(f, 3, policy.Draw{EUR: 3}, policy.Draw{}, 0, true); got != "" {
		t.Errorf("rung 3 up to the ceiling = %q, want none", got)
	}
	if got, want := drawWithin(f, 4, one, policy.Draw{}, 0, false), "there is no decider 4"; got != want {
		t.Errorf("rung 4 = %q, want %q", got, want)
	}
}

// A refused extend names the part past the bound, with what it asked and
// what the bound left, and the largest draw that fits every bound of the
// rung: on rungs 0 and 1 the card's draw and the pool together. A fit is
// named only while the extend's smallest answer at the case (its probe: a
// minute at an agent time cap, else a session) fits too, so the extend
// stays on offer; else nothing fits. Lines ride beside the fit, or alone.
// The facts are case 6354's at an agent time cap: the card's draw holds 3
// sessions, 90 minutes and 6 EUR, the pool 924 minutes alone.
func TestDrawBoundNamesThePartAndTheFit(t *testing.T) {
	const pool924 = "the escalation pool leaves 0 sessions, 924 minutes of agent time and 0.00 EUR this month: only the human draws more. "
	const pool5 = "the escalation pool leaves 5 sessions, 0 seconds of agent time and 10.00 EUR this month: only the human draws more. "
	timeCap := CaseFacts{Kind: CaseAgentTimeCap, CardDrawLeft: policy.Draw{Sessions: 3, AgentTime: 90 * time.Minute, EUR: 6},
		PoolLeft: policy.Draw{AgentTime: 924*time.Minute + 6*time.Second}}
	empty, sessionsOnly, gate, odd, sessionCap := timeCap, timeCap, timeCap, timeCap, timeCap
	empty.PoolLeft = policy.Draw{}
	sessionsOnly.PoolLeft = policy.Draw{Sessions: 5, EUR: 10}
	gate.Kind = CaseGateStop
	sessionCap.Kind = CaseSessionCap
	gateSessions, noCase := sessionsOnly, sessionsOnly
	gateSessions.Kind, noCase.Kind = CaseGateStop, ""
	odd.PoolLeft.AgentTime = 924*time.Minute + 40*time.Second
	extend, _ := ActionByName(ActExtend)
	for _, c := range []struct {
		name  string
		f     CaseFacts
		rung  int
		d     policy.Draw
		lines int
		want  string
	}{
		{"rung 2's session past the pool", timeCap, 2, policy.Draw{Sessions: 1, AgentTime: 30 * time.Minute}, 0,
			pool924 + "This draw does not fit in sessions (asked 1, left 0). The largest draw that fits is 0 sessions, 30 minutes of agent time and 0.00 EUR: ask for that"},
		{"rung 2's minutes alone", timeCap, 2, policy.Draw{AgentTime: 30 * time.Minute}, 0, ""},
		// the card's bound fails first, on minutes; the fit takes its minutes
		// from the card's 90 and its sessions from the pool's 0
		{"rung 1 past the card's minutes", timeCap, 1, policy.Draw{Sessions: 1, AgentTime: 120 * time.Minute}, 0,
			"the card's draw bound leaves 3 sessions, 90 minutes of agent time and 6.00 EUR: the factory agent or the human may draw more. " +
				"This draw does not fit in agent time (asked 2 hours, left 90 minutes). The largest draw that fits is 0 sessions, 90 minutes of agent time and 0.00 EUR: ask for that"},
		{"an empty pool", empty, 2, policy.Draw{AgentTime: 30 * time.Minute}, 0,
			"the escalation pool leaves 0 sessions, 0 seconds of agent time and 0.00 EUR this month: only the human draws more. " +
				"This draw does not fit in agent time (asked 30 minutes, left 0 seconds), and nothing of it fits: hand the draw up (`hand_up`) with it as the recommendation"},
		{"a session without minutes at the agent time cap", sessionsOnly, 2, policy.Draw{Sessions: 1, AgentTime: 30 * time.Minute}, 200,
			pool5 + "This draw does not fit in agent time (asked 30 minutes, left 0 seconds), and nothing of it fits: hand the draw up (`hand_up`) with it as the recommendation"},
		// a gate stop's extend is probed with a session, which the pool lacks
		{"lines beside a session at a gate stop", gate, 2, policy.Draw{Sessions: 1}, 200,
			pool924 + "This draw does not fit in sessions (asked 1, left 0), and nothing of it fits: hand the draw up (`hand_up`) with it as the recommendation"},
		{"a session at the session cap", sessionCap, 2, policy.Draw{Sessions: 1, AgentTime: 30 * time.Minute}, 0,
			pool924 + "This draw does not fit in sessions (asked 1, left 0), and nothing of it fits: hand the draw up (`hand_up`) with it as the recommendation"},
		{"lines beside a session and minutes at a gate stop", gateSessions, 2, policy.Draw{Sessions: 1, AgentTime: 30 * time.Minute}, 200,
			pool5 + "This draw does not fit in agent time (asked 30 minutes, left 0 seconds). The largest draw that fits is 1 session, 0 seconds of agent time and 0.00 EUR with 200 lines beside it, as lines ride free: ask for that"},
		{"lines beside minutes at a gate stop", gateSessions, 2, policy.Draw{AgentTime: 30 * time.Minute}, 200,
			pool5 + "This draw does not fit in agent time (asked 30 minutes, left 0 seconds). An extend of the 200 lines alone fits, as lines ride free: ask for that"},
		{"minutes at a gate stop", gateSessions, 2, policy.Draw{AgentTime: 30 * time.Minute}, 0,
			pool5 + "This draw does not fit in agent time (asked 30 minutes, left 0 seconds), and nothing of it fits, but 1 session, 0 seconds of agent time and 0.00 EUR does: ask for that"},
		// 924m40s reads as 925 minutes left, and the fit is cut to 924 whole
		// minutes, which the pool holds
		{"a fit cut to the minute", odd, 2, policy.Draw{AgentTime: 1000 * time.Minute}, 0,
			"the escalation pool leaves 0 sessions, 925 minutes of agent time and 0.00 EUR this month: only the human draws more. " +
				"This draw does not fit in agent time (asked 1000 minutes, left 925 minutes). The largest draw that fits is 0 sessions, 924 minutes of agent time and 0.00 EUR: ask for that"},
		{"two parts past the pool", noCase, 2, policy.Draw{Sessions: 6, EUR: 12}, 0,
			pool5 + "This draw does not fit in sessions (asked 6, left 5) and EUR (asked 12.00, left 10.00). The largest draw that fits is 5 sessions, 0 seconds of agent time and 10.00 EUR: ask for that"},
	} {
		if got := drawWithin(c.f, c.rung, c.d, extend.need(c.f), c.lines, false); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// An extend at the agent time cap that names no minutes is refused with
// the minutes each rung may ask for: rung 1 the least of the card's draw
// and the pool, rung 2 the pool's, each at most 600 at once.
func TestExtendAtATimeCapNamesTheMinutesThatFit(t *testing.T) {
	f := CaseFacts{Kind: CaseAgentTimeCap, CardDrawLeft: policy.Draw{Sessions: 3, AgentTime: 90 * time.Minute, EUR: 6},
		PoolLeft: policy.Draw{AgentTime: 924*time.Minute + 6*time.Second}}
	want := "this card stopped for agent time: an extend of its agent time starts the next session, and sessions or lines alone buy none. " +
		"The card's draw leaves 90 minutes and the pool 924 minutes of agent time, so the check-in may ask for up to 90 minutes, the factory agent for up to 600 minutes and the human for up to 600 minutes"
	if got := applies(t, ActExtend, f, Resolution{Args: map[string]any{"sessions": 1}}); got != want {
		t.Errorf("an extend of a session at the agent time cap:\n got %q\nwant %q", got, want)
	}
}

// A rung's refused extend names a fit, "ask for that", exactly while the
// extend stays on offer to it, and sends it up with hand_up only while it
// does not: at case 6354's agent time cap and at a gate stop, whose extend
// is probed with a session.
func TestARefusedExtendIsOfferedOnlyWhileAFitRemains(t *testing.T) {
	timeCap := CaseFacts{Kind: CaseAgentTimeCap, Attempts: 1, SessionCap: 3, AgentTimeS: 7220, AgentTimeCapS: 10800,
		CardDrawLeft: policy.Draw{Sessions: 3, AgentTime: 90 * time.Minute, EUR: 6}, PoolLeft: policy.Draw{AgentTime: 924*time.Minute + 6*time.Second}}
	emptyPool, gate := timeCap, timeCap
	emptyPool.PoolLeft = policy.Draw{}
	gate.Kind = CaseGateStop
	gateSessions := gate
	gateSessions.PoolLeft = policy.Draw{Sessions: 5, EUR: 10}
	for _, c := range []struct {
		name string
		f    CaseFacts
		args map[string]any
		fit  bool
	}{
		{"6354's session and 30 minutes", timeCap, map[string]any{"sessions": 1, "agent_time_s": 1800}, true},
		{"30 minutes on an empty pool", emptyPool, map[string]any{"agent_time_s": 1800}, false},
		{"a session and lines at a gate stop, the pool's sessions gone", gate, map[string]any{"sessions": 1, "lines": 200}, false},
		{"minutes at a gate stop, the pool's minutes gone", gateSessions, map[string]any{"agent_time_s": 1800}, true},
		{"minutes and lines at a gate stop, the pool's minutes gone", gateSessions, map[string]any{"agent_time_s": 1800, "lines": 200}, true},
	} {
		got := actRefusal(c.f, ActInput{CaseID: 6354, Rung: 2, Res: Resolution{Action: ActExtend, Args: c.args}}, "#2109")
		_, list, _ := strings.Cut(got, ", and the factory agent may take ")
		offered := slices.Contains(strings.Split(strings.ReplaceAll(list, " and ", ", "), ", "), "`"+ActExtend+"`")
		if strings.Contains(got, ": ask for that") != c.fit || strings.Contains(got, "hand_up") == c.fit || offered != c.fit {
			t.Errorf("%s: %q; want a fit and the extend on offer %v", c.name, got, c.fit)
		}
	}
	got := actRefusal(timeCap, ActInput{CaseID: 6354, Rung: 2, Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1, "agent_time_s": 1800}}}, "#2109")
	_, list, _ := strings.Cut(got, "The largest draw that fits is 0 sessions, 30 minutes of agent time and 0.00 EUR: ask for that, and the factory agent may take ")
	if !strings.Contains(list, "`"+ActExtend+"`") {
		t.Errorf("6354's refusal = %q, want the fit and the extend on offer", got)
	}
}

// An answer's draw carries the smallest draw its action takes on the case,
// the extend's probe, so the store's own refusal names a fit by it as
// drawWithin does: a minute at an agent time cap, a session at a gate stop,
// a cent for a fund.
func TestCaseDrawCarriesTheActionsNeed(t *testing.T) {
	g := newFactsRig(t)
	card := g.store.tasks["t1"]
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1, "agent_time_s": 1800}}
	for _, c := range []struct {
		kind CaseKind
		res  Resolution
		want policy.Draw
	}{
		{CaseAgentTimeCap, extend, policy.Draw{AgentTime: time.Minute}},
		{CaseGateStop, extend, policy.Draw{Sessions: 1}},
		{CaseBudgetFloor, Resolution{Action: ActFund, Args: map[string]any{"eur": 2.0}}, policy.Draw{EUR: 0.01}},
	} {
		d := g.r.caseDraw(card, CaseFacts{Kind: c.kind}, Case{ID: 7, Kind: c.kind}, ActInput{Rung: 2, Res: c.res}, ViaChat)
		if d == nil || d.Need != c.want {
			t.Errorf("%s's %s draw = %+v, want the need %+v", c.kind, c.res.Action, d, c.want)
		}
	}
}

// Rung 0 never funds, however much room the card has.
func TestFundIsNeverRungZeros(t *testing.T) {
	fund, ok := ActionByName(ActFund)
	if !ok || fund.Rungs[0] {
		t.Fatalf("fund = %+v, want a row rung 0 may not pick", fund)
	}
	room := CaseFacts{PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		BudgetEUR: 5, CeilingEUR: 10}
	if names := offeredNames(room, 0); slices.Contains(names, ActFund) {
		t.Errorf("rung 0 is offered %v, want no fund", names)
	}
	if names := offeredNames(room, 1); !slices.Contains(names, ActFund) {
		t.Errorf("rung 1 is offered %v, want fund", names)
	}
	if got, want := fund.refusal(room, 0, Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}}), "the factory may not pick `fund`: the check-in, the factory agent and the human may"; got != want {
		t.Errorf("rung 0's fund = %q, want %q", got, want)
	}
	note, _ := ActionByName(ActNote)
	if got, want := note.refusal(room, 0, Resolution{Action: ActNote, Note: "x"}), "the factory may not pick `note`: the factory agent and the human may"; got != want {
		t.Errorf("rung 0's note = %q, want %q", got, want)
	}
	withdraw, _ := ActionByName(ActWithdraw)
	if got, want := withdraw.refusal(room, 0, Resolution{Action: ActWithdraw, Fund: &policy.Draw{EUR: 1}}), "the factory may not draw money: the check-in, the factory agent and the human may"; got != want {
		t.Errorf("rung 0's withdraw with money beside it = %q, want %q", got, want)
	}
	if got := withdraw.refusal(room, 0, Resolution{Action: ActWithdraw, Fund: &policy.Draw{Sessions: 1}}); got != "" {
		t.Errorf("rung 0's withdraw with a session beside it = %q, want none", got)
	}
	flag := room
	flag.Kind = CaseMonitorFlag
	if got := withdraw.refusal(flag, 1, Resolution{Action: ActWithdraw, Fund: &policy.Draw{EUR: 1}}); got != "" {
		t.Errorf("rung 1's withdraw of a monitor flag's case with money beside it = %q, want none", got)
	}
	if got := withdraw.refusal(room, 1, Resolution{Action: ActWithdraw}); !strings.Contains(got, "A withdrawal (`withdraw`) ends a monitor flag's ask alone") {
		t.Errorf("rung 1's withdraw of a case the run waits on = %q, want it refused: the run would wait for good", got)
	}
	if got := note.refusal(room, 1, Resolution{Action: ActNote, Note: "x"}); got != "the check-in may not pick `note`: the factory agent and the human may" {
		t.Errorf("rung 1's note = %q, want it refused: a note resolves nothing", got)
	}
}

// A green approved head at the session cap, its budget at the ceiling and
// the pool spent: the human is offered a re-verify (the approval card is
// where they approve), an extend and a ceiling raise past the pool, a
// successor, a replan of the direct card, a close and a note; rung 0,
// bound by the card's draw, routes to approval, gets no extend and
// withdraws instead of closing.
func TestOfferedFollowsTheTable(t *testing.T) {
	f := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Attempts: 3, SessionCap: 3,
		AgentTimeS: 900, AgentTimeCapS: 1800, BudgetEUR: 5, SpentEUR: 5, LeftEUR: 0, FloorEUR: 0.3, CeilingEUR: 5}
	if got, want := offeredNames(f, 3), []string{ActReverify, ActExtend, ActRaiseCeiling, ActSuccessor, ActReplan, ActClose, ActNote}; !slices.Equal(got, want) {
		t.Errorf("rung 3 is offered %v, want %v", got, want)
	}
	if got, want := offeredNames(f, 0), []string{ActRouteApproval, ActReverify, ActSuccessor, ActReplan, ActWithdraw}; !slices.Equal(got, want) {
		t.Errorf("rung 0 is offered %v, want %v", got, want)
	}
	if got := Offered(f, 4); got != nil {
		t.Errorf("rung 4 is offered %v, want nothing", got)
	}
}

// A card with no open pull request, no push and no head is offered a
// successor while its branch holds work main lacks, which the successor
// continues; with none on its branch the successor is refused.
func TestSuccessorOfferedOnBranchWork(t *testing.T) {
	f := CaseFacts{BranchRead: true, BranchHead: "c9", Autonomy: policy.AutonomyFullAuto}
	if why := applies(t, ActSuccessor, f, Resolution{}); why != "" || !slices.Contains(offeredNames(f, 3), ActSuccessor) {
		t.Errorf("a branch at c9: successor refused %q, offered %v; want it offered", why, offeredNames(f, 3))
	}
	f.BranchHead = ""
	if why, want := applies(t, ActSuccessor, f, Resolution{}), "no pull request is open and the card's branch holds no work"; why != want || slices.Contains(offeredNames(f, 3), ActSuccessor) {
		t.Errorf("no work on the branch: successor refused %q, offered %v; want refused %q", why, offeredNames(f, 3), want)
	}
}

// A retry takes a failed dependency nobody retried whose end a retry can
// fix, and a named one a retry replaced already, whose answer names the
// replacement; any other named one is refused with its state.
func TestRetryTakesARetryableFailedDependency(t *testing.T) {
	retried := DepFact{TaskID: "d1", Issue: 20, Status: "failed", Why: "session error: the pod is gone", Replaced: RetryID("d1")}
	forbidden := DepFact{TaskID: "d2", Issue: 21, Status: "failed", Why: "policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml"}
	running := DepFact{TaskID: "d3", Issue: 22, Status: "running"}
	open := DepFact{TaskID: "d4", Issue: 23, Status: "failed", Why: "checks: timeout"}
	f := CaseFacts{Dependencies: []DepFact{retried, forbidden, running}}
	if got, want := applies(t, ActRetry, f, Resolution{}), "no failed dependency is open to a retry, and no provider freeze holds the card: retry files a failed dependency again or ends a freeze, so pick the action the card's wait needs"; got != want {
		t.Errorf("no retryable dependency = %q, want %q", got, want)
	}
	retry, _ := ActionByName(ActRetry)
	// a card that waits on the provider with no freeze to end is refused
	// with its state, which the facts name
	noFreeze := f
	noFreeze.Stage, noFreeze.NoFreeze = GateProvider, "the freeze of #11 ended, the card is done"
	if got := applies(t, ActRetry, noFreeze, Resolution{}); got != noFreeze.NoFreeze {
		t.Errorf("a provider wait with no freeze = %q, want %q", got, noFreeze.NoFreeze)
	}
	// its timeline unread, whether a freeze holds it is unknown: try again,
	// or a rung below the human hands it up; a dependency open to a retry
	// needs no timeline
	unread := f
	unread.Stage, unread.Partial = GateProvider, []string{partialTimeline}
	for rung, want := range map[int]string{
		2: "the card's timeline could not be read, so it is unknown whether a provider freeze holds the card, which `retry` needs: try again in a minute, or hand it up",
		3: "the card's timeline could not be read, so it is unknown whether a provider freeze holds the card, which `retry` needs: try again in a minute",
	} {
		if got := retry.refusal(unread, rung, Resolution{}); got != want {
			t.Errorf("rung %d's retry with the timeline unread = %q, want %q", rung, got, want)
		}
	}
	unread.Dependencies = append(slices.Clone(unread.Dependencies), open)
	if got := retry.refusal(unread, 3, Resolution{}); got != "" {
		t.Errorf("the retry of an open dependency with the timeline unread = %q, want none", got)
	}
	// the class's freeze cards unread, the same is unknown
	freezeUnread := f
	freezeUnread.Stage, freezeUnread.Partial = GateProvider, []string{partialFreeze}
	if got, want := retry.refusal(freezeUnread, 2, Resolution{}), "the class's freeze cards could not be read, so it is unknown whether a provider freeze holds the card, which `retry` needs: try again in a minute, or hand it up"; got != want {
		t.Errorf("rung 2's retry with the freeze cards unread = %q, want %q", got, want)
	}
	frozen := f
	frozen.FreezeCard = "t-fz"
	if got := applies(t, ActRetry, frozen, Resolution{}); got != "" {
		t.Errorf("a frozen case's retry = %q, want it offered", got)
	}
	// the freeze's end is a human's or the project agent's: rungs 0 and 1
	// are refused it, and so it is on offer to rungs 2 and 3 alone
	if got, want := retry.refusal(frozen, 1, Resolution{}), "the check-in ends no provider freeze, which a human's press or the factory agent's ends: hand it up (`hand_up`) with `retry` as the recommendation"; got != want {
		t.Errorf("rung 1's retry of a frozen case = %q, want %q", got, want)
	}
	for rung, want := range []bool{false, false, true, true} {
		if got := slices.ContainsFunc(Offered(frozen, rung), func(a Action) bool { return a.Name == ActRetry }); got != want {
			t.Errorf("retry on offer to rung %d = %t, want %t", rung, got, want)
		}
	}
	// a failed dependency open to a retry is rung 1's to retry, freeze or none
	withDep := frozen
	withDep.Dependencies = append(slices.Clone(frozen.Dependencies), open)
	if got := retry.refusal(withDep, 1, Resolution{}); got != "" {
		t.Errorf("rung 1's retry of an open dependency beside a freeze = %q, want none", got)
	}
	if got := applies(t, ActRetry, f, Resolution{Args: map[string]any{"task_id": "d1"}}); got != "" {
		t.Errorf("retry d1, retried already = %q, want it to apply", got)
	}
	for id, want := range map[string]string{
		"d2": "dependency #21 ended for good: policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml",
		"d3": "dependency #22 is running, not failed",
		"d9": "the card does not depend on d9",
	} {
		if got := applies(t, ActRetry, f, Resolution{Args: map[string]any{"task_id": id}}); got != want {
			t.Errorf("retry %s = %q, want %q", id, got, want)
		}
	}
	f.Dependencies = append(f.Dependencies, open)
	if got := applies(t, ActRetry, f, Resolution{}); got != "" {
		t.Errorf("a retryable dependency = %q, want it to apply", got)
	}
	if got := applies(t, ActRetry, f, Resolution{Args: map[string]any{"task_id": "d4"}}); got != "" {
		t.Errorf("retry d4 = %q, want it to apply", got)
	}
	for _, why := range []string{ReasonIssueClosed, "alert no longer fires"} {
		if retryableEnd(why) {
			t.Errorf("retryableEnd(%q) = true, want a terminal end", why)
		}
	}
}

// Every money part of an answer draws from the pool together, the ceiling
// bounds only what goes to the card's budget, the human is never refused
// on the pool, and no draw is weighed while the reads it needs failed.
func TestRefusalBoundsEveryMoneyPart(t *testing.T) {
	f := CaseFacts{PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 10}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		BudgetEUR: 5, CeilingEUR: 6}
	raise, _ := ActionByName(ActRaiseCeiling)
	raiseBy := func(eur float64, beside *policy.Draw) Resolution {
		return Resolution{Action: ActRaiseCeiling, Args: map[string]any{"eur": eur}, Fund: beside}
	}
	const pool = "the escalation pool leaves 5 sessions, 5 hours of agent time and 10.00 EUR this month: only the human draws more. This draw does not fit in EUR "
	const fit = ". The largest draw that fits is 0 sessions, 0 seconds of agent time and 10.00 EUR: ask for that"
	for _, c := range []struct {
		name string
		rung int
		r    Resolution
		want string
	}{
		{"rung 3 raises past the pool", 3, raiseBy(20, nil), ""},
		{"rung 2 raises within it", 2, raiseBy(8, nil), ""},
		{"rung 2 raises within it and funds past it", 2, raiseBy(8, &policy.Draw{EUR: 5}), pool + "(asked 13.00, left 10.00)" + fit},
		{"rung 2 raises past it", 2, raiseBy(12, nil), pool + "(asked 12.00, left 10.00)" + fit},
		// budget 5 at a ceiling of 6: the raise of 3 is written first, so a
		// fund beside it meets a ceiling of 9, which 4 more fits and 5 more
		// passes
		{"the human funds past the raised ceiling", 3, raiseBy(3, &policy.Draw{EUR: 5}),
			"5.00 EUR more takes the budget of 5.00 EUR past the card's ceiling of 9.00 EUR: raise the ceiling first"},
		{"the human funds within the raised ceiling", 3, raiseBy(3, &policy.Draw{EUR: 4}), ""},
		{"a negative part beside", 3, raiseBy(3, &policy.Draw{Sessions: -1, EUR: 1}), "the draw has a part below 0 or an endless one"},
	} {
		if got := raise.refusal(f, c.rung, c.r); got != c.want {
			t.Errorf("%s: refusal = %q, want %q", c.name, got, c.want)
		}
	}
	extend, _ := ActionByName(ActExtend)
	for _, reader := range []string{"events", "checks"} {
		blind := f
		blind.Partial = []string{reader}
		want := "the card's " + reader + " could not be read, so a draw cannot be weighed against the last one: try again in a minute, or close the card"
		if got := extend.refusal(blind, 3, Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}); got != want {
			t.Errorf("%s unread: extend = %q, want %q", reader, got, want)
		}
	}
	if got, want := extend.refusal(f, 7, Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}), "there is no decider 7"; got != want {
		t.Errorf("rung 7: extend = %q, want %q", got, want)
	}
	closing, _ := ActionByName(ActClose)
	if got, want := closing.refusal(f, 1, Resolution{Action: ActClose}), "the check-in may not pick `close`: the factory agent and the human may"; got != want {
		t.Errorf("rung 1's close = %q, want %q", got, want)
	}
	withdraw, _ := ActionByName(ActWithdraw)
	if got, want := withdraw.refusal(f, 3, Resolution{Action: ActWithdraw}), "the human may not pick `withdraw`: the factory and the check-in may"; got != want {
		t.Errorf("the human's withdraw = %q, want %q", got, want)
	}
}

// A raise of the ceiling moves no money into the budget, so the bought-
// nothing rule and the reads it is weighed on refuse an extend, a fund and
// a draw beside the raise, never the raise alone.
func TestRaiseCeilingIsNoDrawThatBuys(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	stuck := CaseFacts{Head: "h1", Checks: "red", Findings: []string{idA}, LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red",
		DrawFindings: []string{idA}, Attempts: 2, PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, BudgetEUR: 5, CeilingEUR: 6}
	raise, _ := ActionByName(ActRaiseCeiling)
	byTwo := Resolution{Action: ActRaiseCeiling, Args: map[string]any{"eur": 2.0}}
	beside := byTwo
	beside.Fund = &policy.Draw{EUR: 1}
	const refused = "the last draw at 10:04 UTC bought nothing, with no push since and the head, the checks and the findings unchanged: " +
		"hand over, start a successor, replan or close"
	if got := raise.refusal(stuck, 3, byTwo); got != "" {
		t.Errorf("the human's raise after a draw that bought nothing = %q, want none", got)
	}
	if got := raise.refusal(stuck, 3, beside); got != refused {
		t.Errorf("a fund beside the raise = %q, want %q", got, refused)
	}
	blind := stuck
	blind.LastDrawAt, blind.Partial = nil, []string{"events"}
	if got := raise.refusal(blind, 2, byTwo); got != "" {
		t.Errorf("rung 2's raise with the events unread = %q, want none", got)
	}
	if got, want := raise.refusal(blind, 2, beside), "the card's events could not be read, so a draw cannot be weighed against the last one: try again in a minute, or close the card"; got != want {
		t.Errorf("a fund beside the raise with the events unread = %q, want %q", got, want)
	}
}

// An unread timeline leaves the findings unknown: a draw is weighed on the
// head and the checks alone, never taken as one whose findings went.
func TestBoughtNothingWeighsHeadAndChecksWhenTheTimelineIsUnread(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	f := CaseFacts{Head: "h1", Checks: "red", LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red", DrawFindings: []string{idA}, Attempts: 1}
	if boughtNothing(f) {
		t.Error("the timeline read with no findings against idA at the draw: the draw bought something")
	}
	f.Partial = []string{"timeline"}
	if !boughtNothing(f) {
		t.Error("the timeline unread, head and checks as at the draw: want bought nothing")
	}
	f.Checks = "green"
	if boughtNothing(f) {
		t.Error("the timeline unread, the checks turned green: the draw bought something")
	}
}

// A raise of the ceiling is a whole cent or more, as the store keeps it.
func TestRaiseCeilingNeedsAWholeCent(t *testing.T) {
	for _, eur := range []float64{0.004, 0, -1} {
		want := fmt.Sprintf("a raise of the ceiling by %v EUR is no amount above zero: raise it by at least 0.01 EUR", eur)
		if got := applies(t, ActRaiseCeiling, CaseFacts{}, Resolution{Args: map[string]any{"eur": eur}}); got != want {
			t.Errorf("a raise of %v = %q, want %q", eur, got, want)
		}
	}
	if got := applies(t, ActRaiseCeiling, CaseFacts{}, Resolution{Args: map[string]any{"eur": 0.01}}); got != "" {
		t.Errorf("a raise of a cent = %q, want it to apply", got)
	}
}

// An extend's lines may not lower the card's line bound.
func TestExtendRefusesLinesBelowZero(t *testing.T) {
	if got, want := applies(t, ActExtend, CaseFacts{}, Resolution{Args: map[string]any{"sessions": 1, "lines": -5}}), "an extend takes no part below 0"; got != want {
		t.Errorf("an extend of -5 lines = %q, want %q", got, want)
	}
}

// An extend of lines alone applies: it raises the card's line bound and
// draws nothing, so the bought-nothing rule, which weighs a draw, lets it
// through after a draw that bought nothing. An extend of nothing names
// every part it may take.
func TestExtendOfLinesAloneApplies(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	stuck := CaseFacts{Head: "h1", Checks: "red", LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red", Attempts: 1}
	lines := Resolution{Action: ActExtend, Args: map[string]any{"lines": 200}}
	if got := applies(t, ActExtend, stuck, lines); got != "" {
		t.Errorf("an extend of 200 lines after a draw that bought nothing = %q, want it to apply", got)
	}
	extend, _ := ActionByName(ActExtend)
	if got := extend.refusal(stuck, 3, lines); got != "" {
		t.Errorf("the human's extend of 200 lines = %q, want it to apply", got)
	}
	if got, want := applies(t, ActExtend, CaseFacts{}, Resolution{}), "an extend needs sessions, agent time or lines above 0"; got != want {
		t.Errorf("an extend of nothing = %q, want %q", got, want)
	}
}

// An extend answers a cap: a session cap, a check-in's spend stop (an agent time
// cap takes agent time, see TestExtendAtATimeCapTakesAgentTime), and the owner exits of a gate stop and of findings that repeat,
// whose questions offer "Extend the card"; with no case it raises a live
// card's caps. A stop on money says it waits on money, with the answers its
// face holds (on facts with no amount, neither a fund nor a raise), any other case asked a
// question, which an extend does not answer, and the human is not offered
// one there.
func TestExtendAnswersACapAlone(t *testing.T) {
	const asked = "this card asked a question, not for a session: answer it, or extend at its cap"
	one := Resolution{Args: map[string]any{"sessions": 1}}
	for kind, want := range map[CaseKind]string{
		CaseSessionCap: "", CaseCheckinShare: "", CaseGateStop: "", CaseFindingsRepeated: "", "": "",
		CaseAgentEscalate: asked, CaseAgentRequestInfo: asked, CaseOther: asked, CaseCheckinHuman: asked,
		CaseBudgetFloor: "this card waits on money, not a session (the card's answers at this stop: `close` and `note`)",
		CaseMonthCap:    "this card waits on the month cap, not a session: raise the month cap in policy.yaml or wait for the month to turn, then `fund` or `retry`",
	} {
		if got := applies(t, ActExtend, CaseFacts{Kind: kind}, one); got != want {
			t.Errorf("an extend on %q = %q, want %q", kind, got, want)
		}
	}
	if got := offeredNames(CaseFacts{Kind: CaseAgentEscalate, SessionCap: 3, AgentTimeCapS: 7200}, 3); slices.Contains(got, ActExtend) {
		t.Errorf("the human on an ask is offered %v, want no extend", got)
	}
}

// An agent time cap is answered by agent time: an extend that raises none
// of the time its stop names, sessions or lines alone, is refused with the
// stop's own words before it draws, so it leaves the card's last draw as
// it stands and the next extend is not already refused for having bought
// nothing. The extend stays offered there, probed on the agent time it
// takes.
func TestExtendAtATimeCapTakesAgentTime(t *testing.T) {
	timeCap := CaseFacts{Kind: CaseAgentTimeCap, SessionCap: 5, AgentTimeCapS: 1200, AgentTimeS: 1200}
	const noMinutes = "this card stopped for agent time: an extend of its agent time starts the next session, and sessions or lines alone buy none. " +
		"The card's draw leaves 0 minutes and the pool 0 minutes of agent time, so the check-in may ask for none, the factory agent for none and the human for up to 600 minutes"
	for _, tc := range []struct {
		name string
		r    Resolution
		want string
	}{
		{"sessions alone", Resolution{Args: map[string]any{"sessions": 1}}, noMinutes},
		{"lines alone", Resolution{Args: map[string]any{"lines": 200}}, noMinutes},
		{"agent time", Resolution{Args: map[string]any{"agent_time_s": 600}}, ""},
		{"agent time past the bound", Resolution{Args: map[string]any{"agent_time_s": 36001}}, "an extend raises at most 600 minutes of agent time at once"},
		{"agent time and sessions", Resolution{Args: map[string]any{"agent_time_s": 600, "sessions": 1}}, ""},
	} {
		if got := applies(t, ActExtend, timeCap, tc.r); got != tc.want {
			t.Errorf("%s at the agent time cap = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := offeredNames(timeCap, 3); !slices.Contains(got, ActExtend) {
		t.Errorf("the human at the agent time cap is offered %v, want the extend", got)
	}
}

// A fund on a card that ended buys nothing: it is refused naming the end.
func TestFundRefusesAnEndedCard(t *testing.T) {
	if got, want := applies(t, ActFund, CaseFacts{Final: "done"}, Resolution{Args: map[string]any{"eur": 1.0}}),
		"this card ended done: a fund buys nothing, file a new card"; got != want {
		t.Errorf("a fund on a done card = %q, want %q", got, want)
	}
}

// A card that ended takes nothing that buys: an extend and a raise of its
// ceiling are refused naming the end, as a fund is, and the human is
// offered none of the three there.
func TestEndedCardTakesNoExtendOrRaise(t *testing.T) {
	ended := CaseFacts{Final: "done"}
	for _, tc := range []struct {
		action string
		r      Resolution
		want   string
	}{
		{ActExtend, Resolution{Args: map[string]any{"sessions": 1}}, "this card ended done: an extend buys nothing, file a new card"},
		{ActRaiseCeiling, Resolution{Args: map[string]any{"eur": 1.0}}, "this card ended done: a raise of its ceiling buys nothing, file a new card"},
	} {
		if got := applies(t, tc.action, ended, tc.r); got != tc.want {
			t.Errorf("%s on a done card = %q, want %q", tc.action, got, tc.want)
		}
	}
	if got := offeredNames(ended, 3); slices.Contains(got, ActExtend) || slices.Contains(got, ActRaiseCeiling) || slices.Contains(got, ActFund) {
		t.Errorf("the human on a done card is offered %v, want no extend, raise or fund", got)
	}
}

// An extend on a card short of money names the stop and the answers its
// face holds: under an 18.00 EUR spend limit and with no branch, the fund,
// the close and the note. At the month cap the gateway refuses every call
// until the cap moves, so the refusal names the cap before the fund. Act's
// door adds every answer the human may take: with 0.00 EUR left of a 2.00
// EUR opening and no pull request, the table's fund, raise_ceiling, replan,
// close and note, in the table's order.
func TestExtendOnAMoneyStopNamesTheStop(t *testing.T) {
	floor := CaseFacts{Kind: CaseBudgetFloor, Attempts: 1, SessionCap: 3, BudgetEUR: 4, SpentEUR: 4, LeftEUR: 0, FloorEUR: 2, CeilingEUR: 18,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	got := actRefusal(floor, ActInput{CaseID: 7, Rung: 3, Res: Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}}, "#5")
	if want := "this card waits on money, not a session (the card's answers at this stop: `fund`, `close` and `note`)," +
		" and the human may take `fund`, `raise_ceiling`, `replan`, `close` and `note`"; got != want {
		t.Errorf("the human's extend at the budget floor = %q, want %q", got, want)
	}
	for kind, want := range map[CaseKind]string{
		CaseBudgetFloor: "this card waits on money, not a session (the card's answers at this stop: `close` and `note`)",
		CaseMonthCap:    "this card waits on the month cap, not a session: raise the month cap in policy.yaml or wait for the month to turn, then `fund` or `retry`",
	} {
		if got := applies(t, ActExtend, CaseFacts{Kind: kind}, Resolution{Args: map[string]any{"sessions": 1}}); got != want {
			t.Errorf("an extend on %s = %q, want %q", kind, got, want)
		}
	}
}

// A withdrawal through Act sends the run no signal, so withdraw is the
// factory's own (EndCase and the heal pass): neither the chat's agent on
// rung 2 nor the human is offered it, on a card with no case, at a cap,
// on red checks or with a failed dependency.
func TestWithdrawIsOfferedToNoHumanAndNoChat(t *testing.T) {
	for _, f := range []CaseFacts{
		{},
		{Kind: CaseSessionCap, Attempts: 3, SessionCap: 3, AgentTimeCapS: 1800, LeftEUR: 1, FloorEUR: 0.3, CeilingEUR: 5},
		{Kind: CaseGateStop, Head: "h1", PR: 9, PROpen: true, Checks: "red", SessionCap: 3, AgentTimeCapS: 1800},
		{Kind: CaseDependencyFailed, Dependencies: []DepFact{{TaskID: "d4", Issue: 23, Status: "failed", Why: "checks: timeout"}}},
	} {
		for _, rung := range []int{2, 3} {
			if got := offeredNames(f, rung); slices.Contains(got, ActWithdraw) {
				t.Errorf("rung %d on %+v is offered %v, want no withdraw", rung, f, got)
			}
		}
	}
}

// Rung 0 closes only what an ended card left open: it may close a card
// that ended, and is refused on a live one, which rung 2 or the human
// closes. Rung 1 closes neither.
func TestRungZeroClosesOnlyAnEndedCard(t *testing.T) {
	closing, _ := ActionByName(ActClose)
	res := Resolution{Action: ActClose}
	if got := closing.refusal(CaseFacts{Final: "done"}, 0, res); got != "" {
		t.Errorf("rung 0's close of a done card = %q, want it taken", got)
	}
	for _, tc := range []struct {
		rung  int
		who   string
		facts CaseFacts
	}{{0, "the factory", CaseFacts{}}, {1, "the check-in", CaseFacts{Final: "done"}}} {
		if got, want := closing.refusal(tc.facts, tc.rung, res), tc.who+" may not pick `close`: the factory agent and the human may"; got != want {
			t.Errorf("rung %d's close of %+v = %q, want %q", tc.rung, tc.facts, got, want)
		}
	}
	if got := offeredNames(CaseFacts{Final: "done"}, 0); !slices.Contains(got, ActClose) {
		t.Errorf("rung 0 on a done card is offered %v, want close among them", got)
	}
}

// A monitor flag's case sits beside the card's run, which carries out no
// answer to it: the human is offered only what Act does itself, here the
// raise of the ceiling, the replan, the settle of the flag and a note,
// and a resume is refused. Close is refused on the live card, since it would
// end the card, and taken on an ended one, whose issue it may close; a
// settle of any other case is refused.
func TestMonitorCaseOffersOnlyWhatActDoes(t *testing.T) {
	f := CaseFacts{Kind: CaseMonitorFlag, Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Attempts: 3, SessionCap: 3,
		AgentTimeS: 900, AgentTimeCapS: 1800, BudgetEUR: 5, SpentEUR: 5, LeftEUR: 0, FloorEUR: 0.3, CeilingEUR: 5}
	if got, want := offeredNames(f, 3), []string{ActRaiseCeiling, ActReplan, ActSettle, ActNote}; !slices.Equal(got, want) {
		t.Errorf("rung 3 on a monitor case is offered %v, want %v", got, want)
	}
	resume, _ := ActionByName(ActResume)
	if got, want := resume.refusal(f, 3, Resolution{Action: ActResume}), "`resume` would do nothing on a monitor flag's ask: the card's run waits on none"; got != want {
		t.Errorf("a resume of a monitor case = %q, want %q", got, want)
	}
	closing, _ := ActionByName(ActClose)
	if got, want := closing.refusal(f, 3, Resolution{Action: ActClose}),
		"`close` ends the card, not the flag's ask: answer it with `settle`, which ends the ask, and the card goes on"; got != want {
		t.Errorf("a close of a live card's monitor case = %q, want %q", got, want)
	}
	ended := f
	ended.Final = "done"
	if got := closing.refusal(ended, 3, Resolution{Action: ActClose}); got != "" {
		t.Errorf("a close of an ended card's monitor case = %q, want it taken", got)
	}
	if got, want := applies(t, ActSettle, CaseFacts{Kind: CaseSessionCap}, Resolution{}),
		"settle ends a monitor flag's ask alone, a held plan's ask, or the ask a park settles, and this ask is none of them"; got != want {
		t.Errorf("a settle of a cap case = %q, want %q", got, want)
	}
}

// No answer leads a stop's list: at a budget floor, a session cap and an
// agent time cap the human is offered the action table's rows in the
// table's order. The table holds reverify before extend and extend before
// fund, so on an open pull request reverify stands ahead of the fund at the
// floor and ahead of the extend at both caps.
func TestOfferedListsTheTableInItsOrderAtEveryStop(t *testing.T) {
	base := CaseFacts{Head: "abcdef123", PR: 9, PROpen: true, Pushed: true, Checks: "green", ReviewApproved: true,
		Attempts: 1, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, SessionLeastS: 300,
		BudgetEUR: 4, SpentEUR: 1, LeftEUR: 3, FloorEUR: 2, CeilingEUR: 18,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	for _, tc := range []struct {
		kind CaseKind
		at   func(*CaseFacts)
		cure string
	}{
		{CaseBudgetFloor, func(f *CaseFacts) { f.SpentEUR, f.LeftEUR = 4, 0 }, ActFund},
		{CaseSessionCap, func(f *CaseFacts) { f.Attempts = 3 }, ActExtend},
		{CaseAgentTimeCap, func(f *CaseFacts) { f.AgentTimeS = 7200 }, ActExtend},
	} {
		f := base
		f.Kind = tc.kind
		tc.at(&f)
		var names []string
		for _, a := range Offered(f, 3) {
			names = append(names, a.Name)
		}
		var table []string
		for _, a := range Actions {
			if slices.Contains(names, a.Name) {
				table = append(table, a.Name)
			}
		}
		if !slices.Equal(names, table) {
			t.Errorf("%s: offered %v, want the table's order %v", tc.kind, names, table)
		}
		if r, c := slices.Index(names, ActReverify), slices.Index(names, tc.cure); r < 0 || c < 0 || r > c {
			t.Errorf("%s: offered %v, want reverify ahead of %s", tc.kind, names, tc.cure)
		}
	}
}

// A budget floor's amount reads what a session may spend, the gate
// reserve held out: 0.70 EUR left less the 0.40 reserve is 0.30, and an
// opening of 0.60 needs 0.30 EUR more. Its money fact names the budget
// and what one more session buys.
func TestShortfallHoldsTheReserveOut(t *testing.T) {
	f := CaseFacts{BudgetEUR: 3, SpentEUR: 2.3, LeftEUR: 0.7, ReserveEUR: 0.4, FloorEUR: 0.6, Model: "factory/small"}
	if got := shortfall(f); got != 0.3 {
		t.Errorf("shortfall = %v, want 0.30", got)
	}
	want := CaseMoney{Kind: "budget", BudgetEUR: 3, SpentEUR: 2.3, Buys: "one session on factory/small"}
	if got := tripMoney(CaseBudgetFloor, f); got == nil || *got != want {
		t.Fatalf("money = %+v, want %+v", got, want)
	}
}

// The amount rounds up to the cent: an opening of 0.604 against a session
// ceiling of 0.11 - 0.40 = -0.29 needs 0.894, so 0.90, not 0.89.
func TestShortfallRoundsUp(t *testing.T) {
	f := CaseFacts{BudgetEUR: 3, SpentEUR: 2.89, LeftEUR: 0.11, ReserveEUR: 0.4, FloorEUR: 0.604}
	if got := shortfall(f); got != 0.9 {
		t.Fatalf("shortfall = %v, want 0.90", got)
	}
}

// A planner never pushes: its draw bought something once it filed a
// revision since. At an agent time stop the draw weighed is the newest that
// raised agent time: a draw of money or sessions beside or after it raised
// nothing the stop names.
func TestBoughtNothingWeighsAPlannerOnItsRevision(t *testing.T) {
	at := time.Date(2026, 11, 10, 11, 29, 0, 0, time.UTC)
	f := CaseFacts{Kind: CaseAgentTimeCap, Attempts: 1, TimeDraw: &DrawFacts{At: at}}
	if !boughtNothing(f) {
		t.Error("a draw of agent time, no push and no revision since: want bought nothing")
	}
	f.TimeDraw.Revised = true
	if boughtNothing(f) {
		t.Error("a revision filed since the draw: the draw bought something")
	}
	f.TimeDraw = nil
	f.LastDrawAt, f.DrawHead = &at, ""
	if boughtNothing(f) {
		t.Error("only a draw that raised no agent time at a time stop: no draw to weigh")
	}
	f.Kind = CaseSessionCap
	if !boughtNothing(f) {
		t.Error("the same draw at a session cap is weighed as before")
	}
}

// An extend of agent time with money beside it is two rows, the extend
// then the fund, both of the pool; newest first the fund leads. At a time
// stop the draw weighed is the extend's, so with no push or revision since
// it bought nothing, and a revision filed after both is what it bought.
func TestEventFactsWeighTheTimeDrawBesideAFund(t *testing.T) {
	detail := func(m map[string]any) map[string]any {
		d := map[string]any{"pool": true, "head": "", "checks": "", "sessions": 0.0, "agent_time_s": 0.0, "amount_eur": 0.0}
		maps.Copy(d, m)
		return d
	}
	extend := Event{At: factsAt(11, 29), Kind: EventCardExtended, Detail: detail(map[string]any{"agent_time_s": 300.0})}
	fund := Event{At: factsAt(11, 29), Kind: EventCardFunded, Detail: detail(map[string]any{"amount_eur": 1.0})}
	revision := Event{At: factsAt(11, 40), Kind: EventPlanRevision}
	var f CaseFacts
	f.eventFacts([]Event{fund, extend})
	f.Kind, f.Attempts = CaseAgentTimeCap, 1
	if f.LastDrawAt == nil || f.TimeDraw == nil || f.TimeDraw.Revised || !boughtNothing(f) {
		t.Fatalf("facts %+v; want the extend's time draw weighed and bought nothing", f)
	}
	f.Kind = CaseSessionCap
	if !boughtNothing(f) {
		t.Error("the newest draw, the fund, bought nothing at a session cap")
	}
	var after CaseFacts
	after.eventFacts([]Event{revision, fund, extend})
	after.Kind, after.Attempts = CaseAgentTimeCap, 1
	if !after.TimeDraw.Revised || boughtNothing(after) {
		t.Errorf("a revision filed after both rows: facts %+v, want the draw bought", after)
	}
	var before CaseFacts
	before.eventFacts([]Event{fund, extend, revision})
	if before.TimeDraw.Revised {
		t.Error("a revision filed before the draw is not one since")
	}
}

// At a planner's time stop rungs 1 and 2 are offered the extend only when
// the pool holds agent time: sessions without time start no session.
func TestExtendAtATimeCapNeedsPoolTime(t *testing.T) {
	f := CaseFacts{Kind: CaseAgentTimeCap, SessionCap: 5, AgentTimeCapS: 1200, AgentTimeS: 1200,
		PoolLeft: policy.Draw{Sessions: 5, EUR: 20}, CardDrawLeft: policy.Draw{Sessions: 5, AgentTime: time.Hour, EUR: 20}}
	if got := offeredNames(f, 2); slices.Contains(got, ActExtend) {
		t.Errorf("rung 2 on a pool with sessions and no agent time is offered %v, want no extend", got)
	}
	f.PoolLeft.AgentTime = time.Hour
	if got := offeredNames(f, 2); !slices.Contains(got, ActExtend) {
		t.Errorf("rung 2 on a pool with agent time is offered %v, want the extend", got)
	}
}

// At a time stop the refusal names the agent-time draw it weighs, not the
// card's last draw of any kind, and a planner's is measured on its revision
// where an agent's is on its push.
func TestBoughtNothingTextNamesTheDrawAndWhatItWasMeasuredOn(t *testing.T) {
	at := time.Date(2026, 11, 10, 11, 29, 0, 0, time.UTC)
	tail := " UTC bought nothing, with no %s since and the head, the checks and the findings unchanged: hand over, start a successor, replan or close"
	for _, tc := range []struct {
		name string
		f    CaseFacts
		want string
	}{
		{"planner at a time stop", CaseFacts{Kind: CaseAgentTimeCap, Planner: true, Attempts: 1, TimeDraw: &DrawFacts{At: at}},
			"the last agent-time draw at 11:29" + fmt.Sprintf(tail, "plan revision")},
		{"agent at a time stop", CaseFacts{Kind: CaseAgentTimeCap, Attempts: 1, TimeDraw: &DrawFacts{At: at}},
			"the last agent-time draw at 11:29" + fmt.Sprintf(tail, "push")},
		{"agent at a session cap", CaseFacts{Kind: CaseSessionCap, Attempts: 1, LastDrawAt: &at},
			"the last draw at 11:29" + fmt.Sprintf(tail, "push")},
	} {
		if got := boughtNothingText(tc.f); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// wait_checks applies to a checks wait's stop with a pull request open,
// whatever the checks and the agent time left, and to no other case.
func TestWaitChecksAppliesToAChecksWaitStop(t *testing.T) {
	stop := CaseFacts{Kind: CaseGateStop, Gate: ActivityWaitChecks, PROpen: true, Checks: "green", SessionCap: 3, AgentTimeCapS: 600, AgentTimeS: 600}
	merge, noPR, quiet := stop, stop, stop
	merge.Gate, noPR.PROpen, quiet.Kind = ActivityMerge, false, CaseSessionCap
	for _, c := range []struct {
		name string
		f    CaseFacts
		want string
	}{
		{"checks wait at the agent time cap", stop, ""},
		{"merge stop", merge, "`wait_checks` runs a stopped checks wait again, and this ask is no checks wait's stop"},
		{"other case", quiet, "`wait_checks` runs a stopped checks wait again, and this ask is no checks wait's stop"},
		{"no pull request", noPR, "no pull request is open"},
	} {
		if got := applies(t, ActWaitChecks, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: `wait_checks` applies = %q, want %q", c.name, got, c.want)
		}
	}
	if !slices.Contains(offeredNames(stop, 2), ActWaitChecks) || slices.Contains(offeredNames(stop, 2), ActResume) {
		t.Errorf("rung 2 is offered %v at the cap, want `wait_checks` and no resume", offeredNames(stop, 2))
	}
}

// Rungs 0 to 2 take wait_checks once per head, as rerun_checks; a human
// may press it again.
func TestWaitChecksIsOncePerHeadBelowTheHuman(t *testing.T) {
	stop := CaseFacts{Kind: CaseGateStop, Gate: ActivityWaitChecks, PROpen: true, Head: "h1", Checks: "green"}
	waited := stop
	waited.WaitedOnHead = true
	a, _ := ActionByName(ActWaitChecks)
	for _, c := range []struct {
		rung   int
		f      CaseFacts
		refuse bool
	}{{0, stop, false}, {1, stop, false}, {2, stop, false}, {0, waited, true}, {1, waited, true}, {2, waited, true}, {3, waited, false}} {
		got := a.refusal(c.f, c.rung, Resolution{Action: ActWaitChecks})
		if (got != "") != c.refuse {
			t.Errorf("rung %d, waited %v: refusal %q, want refused %v", c.rung, c.f.WaitedOnHead, got, c.refuse)
		}
	}
	if got := a.refusal(waited, 2, Resolution{Action: ActWaitChecks}); got != "the checks wait on this head was run again once already" {
		t.Errorf("refusal = %q", got)
	}
}

// route_approval refuses a checks-wait stop, says that it is the wrong action
// and names wait_checks, whose once-per-head limit it would bypass. A human
// reads the answer's label and no key.
func TestRouteApprovalRefusesAChecksWaitStop(t *testing.T) {
	green := CaseFacts{Kind: CaseGateStop, Gate: ActivityWaitChecks, PROpen: true, Checks: "green", ReviewApproved: true}
	merge := green
	merge.Gate = ActivityMerge
	got := applies(t, ActRouteApproval, green, Resolution{})
	if want := "a checks wait's stop does not go to the approval (`route_approval`): it goes on with `wait_checks`"; got != want {
		t.Errorf("checks wait: route_approval applies = %q, want %q", got, want)
	}
	if got, want := ForHuman(got), "a checks wait's stop does not go to the approval: it goes on with “Wait for the checks”"; got != want {
		t.Errorf("a human reads %q, want %q", got, want)
	}
	if got := applies(t, ActRouteApproval, merge, Resolution{}); got != "" {
		t.Errorf("merge stop: route_approval applies = %q, want it to apply", got)
	}
}

// A wait_checks on a stop that is no checks wait's gets that refusal, also
// where the head was waited on.
func TestWaitChecksRefusalNamesTheStopBeforeTheLimit(t *testing.T) {
	merge := CaseFacts{Kind: CaseGateStop, Gate: ActivityMerge, PROpen: true, WaitedOnHead: true}
	capped := CaseFacts{Kind: CaseAgentTimeCap, PROpen: true, WaitedOnHead: true}
	a, _ := ActionByName(ActWaitChecks)
	for name, f := range map[string]CaseFacts{"merge": merge, "cap": capped} {
		if got, want := a.refusal(f, 2, Resolution{Action: ActWaitChecks}), "`wait_checks` runs a stopped checks wait again, and this ask is no checks wait's stop"; got != want {
			t.Errorf("%s: refusal = %q, want %q", name, got, want)
		}
	}
}

// approve_merge takes an open pull request whose head's checks are green,
// and rung 1 may not pick it at all.
func TestApproveMergeApplies(t *testing.T) {
	ready := CaseFacts{Kind: CaseMergeApproval, Head: "abcdef123", PR: 9, PROpen: true, Checks: "green"}
	pending, closed, gate := ready, ready, ready
	pending.Checks, closed.PROpen, gate.Kind = "pending", false, CaseGateStop
	for _, c := range []struct {
		name string
		f    CaseFacts
		want string
	}{
		{"ready", ready, ""},
		{"pending", pending, "the checks on abcdef1 are pending, not green: `approve_merge` waits for green checks on the head"},
		{"closed", closed, "the pull request is not open, so there is nothing to approve"},
		{"a gate stop", gate, "`approve_merge` decides a merge request, and this ask is no merge approval"},
	} {
		if got := applies(t, ActApproveMerge, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: `approve_merge` applies = %q, want %q", c.name, got, c.want)
		}
	}
	a, _ := ActionByName(ActApproveMerge)
	if got, want := a.refusal(ready, 1, Resolution{Action: ActApproveMerge}), "the check-in may not pick `approve_merge`: the factory agent and the human may"; got != want {
		t.Errorf("rung 1's `approve_merge` = %q, want %q", got, want)
	}
	if got := a.refusal(ready, 3, Resolution{Action: ActApproveMerge, Fund: &policy.Draw{EUR: 1}}); got != "`approve_merge` decides the merge request alone: it draws nothing" {
		t.Errorf("`approve_merge` with a fund = %q", got)
	}
}

// A merge case offers the human the decision, the denial with a note, the
// close and a note, none of the dependency, filing, restart or criteria rows;
// no board row applies to a gate stop.
func TestOfferedOnAMergeCaseAndNoBoardRowOnAGateStop(t *testing.T) {
	merge := CaseFacts{Kind: CaseMergeApproval, Head: "h1", PR: 9, PROpen: true, Checks: "green"}
	got := offeredNames(merge, 3)
	for _, name := range []string{ActApproveMerge, ActDenyMerge, ActClose, ActNote} {
		if !slices.Contains(got, name) {
			t.Errorf("a merge case offers %v to the human, want %s among them", got, name)
		}
	}
	for _, name := range []string{ActSetDependency, ActRemoveDependency, ActFileCard, ActRestartPlan, ActAnswerCriteria} {
		if slices.Contains(got, name) {
			t.Errorf("a merge case offers %s", name)
		}
	}
	gate := answerFacts
	gate.Kind = CaseGateStop
	for _, name := range offeredNames(gate, 3) {
		if slices.Contains(boardActions, name) {
			t.Errorf("a gate stop offers the board's %s", name)
		}
	}
}

// The human's buttons on the check-in's seed gate stop: rung 3 adds
// raise_ceiling and close to the seed's rung 1 list of resume, continue,
// handover, reverify, extend, fund, successor and replan, and its note
// closes the list; withdraw is the factory's own, and no row of the board
// applies to a gate stop.
func TestHumanButtonsOnTheSeedGateStop(t *testing.T) {
	want := []string{ActResume, ActContinue, ActHandover, ActReverify, ActExtend, ActFund, ActRaiseCeiling, ActSuccessor, ActReplan, ActClose, ActNote}
	if got := offeredNames(rungFacts, 3); !slices.Equal(got, want) {
		t.Errorf("rung 3 is offered %v, want %v", got, want)
	}
}

// A policy-class case on an open pull request routes to the approval
// whatever its newest review says: the gates the re-verify runs handle the
// review's concerns. Any other case still needs the approval.
func TestRouteApprovalAtAPolicyClassNeedsNoApprovingReview(t *testing.T) {
	concerns := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: false, Kind: CasePolicyClass}
	if got := applies(t, ActRouteApproval, concerns, Resolution{}); got != "" {
		t.Errorf("policy class, review with concerns: route_approval applies = %q, want it to apply", got)
	}
	cap := concerns
	cap.Kind = CaseSessionCap
	if got, want := applies(t, ActRouteApproval, cap, Resolution{}), "the newest review of the head did not approve it"; got != want {
		t.Errorf("session cap, review with concerns: route_approval applies = %q, want %q", got, want)
	}
	red := concerns
	red.Checks = "red"
	if got, want := applies(t, ActRouteApproval, red, Resolution{}), "the checks on the head are red, not green"; got != want {
		t.Errorf("policy class, red checks: route_approval applies = %q, want %q", got, want)
	}
}

// A pull request that no longer merges into main refuses the answers that
// only run the gates again into the same refusal, each naming the conflict
// and what it needs by the stop's kind: a session that merges main at a cap
// and at the budget floor, with the answers the stop's face holds (the card
// has a pull request, so a successor is one, and the floor's facts hold no
// amount to fund or raise), the cap's turn at a cap wait, else a continue. With no
// conflict each applies, and an extend at the cap applies with the
// conflict too.
func TestAMergeConflictRefusesTheAnswersThatRerunTheGates(t *testing.T) {
	ready := CaseFacts{Kind: CaseSessionCap, Head: "abcdef123", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true,
		Attempts: 3, SessionCap: 3, AuditFindings: []string{idA}, AuditConfirmable: []string{idA}, FindingTexts: map[string]string{idA: "the migration runs outside its transaction"}}
	conflict := ready
	conflict.MergeConflict = "the branch conflicts with its base: status 409: merge failed because of conflict"
	// the clearing of the audit names the finding it clears
	note := Resolution{Note: "a fixture the human judged safe", Args: map[string]any{"findings": idA}}
	for _, name := range []string{ActRouteApproval, ActReverify, ActConfirmAudit} {
		if got := applies(t, name, ready, note); got != "" {
			t.Errorf("no conflict: %s applies = %q, want it to apply", name, got)
		}
		want := "the pull request no longer merges into main (Forgejo refused to merge main into abcdef1), so `" + name +
			"` only runs the gates into the same refusal at the merge queue: the card needs a session that merges main into the branch" +
			" (the card's answers at this stop: `extend`, `successor`, `close` and `note`), or the card closes as superseded when main already carries its work"
		if got := applies(t, name, conflict, note); got != want {
			t.Errorf("conflict: %s applies = %q, want %q", name, got, want)
		}
	}
	for kind, fit := range map[CaseKind]string{
		CaseAgentTimeCap:  "the card needs a session that merges main into the branch (the card's answers at this stop: `extend`, `successor`, `close` and `note`)",
		CaseBudgetFloor:   "the card needs a session that merges main into the branch (the card's answers at this stop: `successor`, `close` and `note`)",
		CaseGateStop:      "the card needs a session that merges main into the branch, which `extend` buys",
		CaseMonthCap:      "the card's next session, once the cap turns, merges main into the branch",
		CaseAgentEscalate: "continue the card with a note for a session that merges main into the branch",
	} {
		f := conflict
		f.Kind = kind
		want := "the pull request no longer merges into main (Forgejo refused to merge main into abcdef1), so `reverify`" +
			" only runs the gates into the same refusal at the merge queue: " + fit + ", or the card closes as superseded when main already carries its work"
		if got := applies(t, ActReverify, f, note); got != want {
			t.Errorf("conflict at %s: reverify applies = %q, want %q", kind, got, want)
		}
	}
	if got := applies(t, ActExtend, conflict, Resolution{Args: map[string]any{"sessions": 1}}); got != "" {
		t.Errorf("conflict: an extend of one session at the cap applies = %q, want it to apply", got)
	}
}

// A timeline that could not be read leaves a merge conflict and the head's
// audit unknown: route_approval and reverify, which would run the gates on
// a head that may not merge, and confirm_audit, which judges the audit's
// findings, are refused naming the unread timeline and what fits, never a
// reverify that runs the audit again. A rung below the human may hand the
// case up; the human, with no rung above, tries again. Rung 0's
// route_approval does not fire then; read, it does.
func TestAnUnreadTimelineRefusesTheGatesReruns(t *testing.T) {
	ready := CaseFacts{Kind: CaseSessionCap, Head: "abcdef123", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Partial: []string{"timeline"}}
	const unread = "the card's timeline could not be read, so it is unknown whether the pull request still merges into main, which "
	for _, tc := range []struct {
		action string
		rung   int
		r      Resolution
		want   string
	}{
		{ActRouteApproval, 2, Resolution{}, unread + "`route_approval` needs: try again in a minute, or hand it up"},
		{ActReverify, 1, Resolution{}, unread + "`reverify` needs: try again in a minute, or hand it up"},
		{ActReverify, 3, Resolution{}, unread + "`reverify` needs: try again in a minute"},
		{ActConfirmAudit, 3, Resolution{Note: "fixtures"}, "the card's timeline could not be read, so the head's audit findings and whether the pull request still merges into main are unknown: try again in a minute"},
	} {
		a, _ := ActionByName(tc.action)
		tc.r.Action = tc.action
		if got := a.refusal(ready, tc.rung, tc.r); got != tc.want {
			t.Errorf("rung %d's %s = %q, want %q", tc.rung, tc.action, got, tc.want)
		}
	}
	c := Case{Kind: CaseSessionCap}
	if res, _ := rung0[0].Fire(context.Background(), nil, c, ready); res != nil {
		t.Errorf("rung 0 fired %+v on an unread timeline", res)
	}
	read := ready
	read.Partial = nil
	if res, _ := rung0[0].Fire(context.Background(), nil, c, read); res == nil || res.Action != ActRouteApproval {
		t.Errorf("rung 0 fired %+v on a read timeline, want route_approval", res)
	}
}

// Rung 0 takes nothing on facts read in part, except where a fact's own
// actions refuse while it is unread: a hold that could not be read refuses
// the starts and closes it guards (holdRefusal), so rung 0 goes on to its
// rows, and an unread timeline stops it.
func TestRungZeroGoesOnPastAnUnreadHold(t *testing.T) {
	c := Case{Kind: CaseDependencyFailed}
	for _, tc := range []struct {
		partial []string
		want    string
	}{
		{[]string{partialHold, partialPlanHold}, "no resolver fits"},
		{[]string{partialHold, "timeline"}, "facts read partly: timeline, so rung 0 took nothing"},
	} {
		res, found, err := new(Runner).rungZero(context.Background(), Task{ID: "t1"}, c, CaseFacts{Kind: CaseDependencyFailed, Partial: tc.partial}, nil)
		if err != nil || res != nil || found != tc.want {
			t.Errorf("partial %v: rung 0 = %+v, %q, %v; want nothing, %q", tc.partial, res, found, err, tc.want)
		}
	}
	// its retry of a failed dependency stands on an unread hold
	i := slices.IndexFunc(rung0, func(r resolver) bool { return r.Name == "retry_dependency" })
	failed := CaseFacts{Kind: CaseDependencyFailed, Partial: []string{partialHold, partialPlanHold}, Dependencies: []DepFact{{TaskID: "t-a", Status: "failed"}}}
	if res, _ := rung0[i].Fire(context.Background(), nil, Case{Kind: CaseDependencyFailed, StateKey: "t-a"}, failed); res == nil || res.Action != ActRetry {
		t.Errorf("rung 0's retry on an unread hold = %+v, want the retry of t-a", res)
	}
}

// A decision's riding draw counts before the cap check. At a cap of 3
// reached with 3 attempts, a handover that draws one session applies (the
// cap becomes 4), one that draws nothing does not, and a draw of agent time
// alone does not buy a session. By hand: 3 attempts against 3 + 1 = 4.
func TestHandoverAtTheCapAppliesWithItsRidingDraw(t *testing.T) {
	atCap := CaseFacts{Attempts: 3, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3}
	same := map[string]any{"model": "same"}
	for _, c := range []struct {
		name string
		r    Resolution
		want string
	}{
		{"one session beside it", Resolution{Args: same, Fund: &policy.Draw{Sessions: 1}}, ""},
		{"no draw", Resolution{Args: same}, "the session cap of 3 is reached (the card's answers at this stop: `extend`, `close` and `note`)"},
		{"agent time alone", Resolution{Args: same, Fund: &policy.Draw{AgentTime: time.Hour}}, "the session cap of 3 is reached (the card's answers at this stop: `extend`, `close` and `note`)"},
	} {
		if got := applies(t, ActHandover, atCap, c.r); got != c.want {
			t.Errorf("%s: handover applies = %q, want %q", c.name, got, c.want)
		}
	}
	if got := applies(t, ActResume, atCap, Resolution{Fund: &policy.Draw{Sessions: 1}}); got != "" {
		t.Errorf("resume beside a draw of one session applies = %q, want it to apply", got)
	}
}

// The offered list shows a handover at the cap to rung 1 alone, whose answer
// carries a session draw, while the card's draw bound holds a session: not to
// rung 0, rung 2 (its answer sends money alone) or the human, and not to
// rung 1 once the draw bound is spent. Rung 1's statement says the draw.
func TestOfferedListsAHandoverAtTheCapToRung1WhileTheDrawBoundHoldsASession(t *testing.T) {
	atCap := CaseFacts{Kind: CaseSessionCap, Attempts: 3, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3,
		PROpen: true, Head: "h1", Checks: "green",
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}, PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}}
	for rung, want := range map[int]bool{0: false, 1: true, 2: false, 3: false} {
		if got := slices.Contains(offeredNames(atCap, rung), ActHandover); got != want {
			t.Errorf("rung %d is offered %v, handover offered = %v, want %v", rung, offeredNames(atCap, rung), got, want)
		}
	}
	for _, name := range []string{ActResume, ActContinue} {
		if slices.Contains(offeredNames(atCap, 1), name) {
			t.Errorf("rung 1 is offered %s at the cap: a resume buys nothing, a continue needs room", name)
		}
	}
	noRoom := atCap
	noRoom.CardDrawLeft = policy.Draw{}
	if got := offeredNames(noRoom, 1); slices.Contains(got, ActHandover) {
		t.Errorf("rung 1 is offered %v with no draw left on the card, want no handover", got)
	}
	c := Case{Kind: CaseSessionCap, Problem: "the cap"}
	text := new(Runner).rungStatement(c, atCap, "", nil, 1)
	if !strings.Contains(text, "- handover, with a draw of at least one session") {
		t.Errorf("rung 1's statement does not name the handover's draw:\n%s", text)
	}
	if text := new(Runner).rungStatement(c, atCap, "", nil, 2); strings.Contains(text, "- handover\n") || strings.Contains(text, "- handover,") {
		t.Errorf("rung 2's statement offers a handover it cannot fund:\n%s", text)
	}
}

// The board rows' Applies hold each precondition: a dependency edge needs a
// card that has not started, file_card a case whose cause may lie outside the
// card, restart_plan a plan at rest, answer_criteria a criteria case or a merge
// case of a card holding criteria, and the criteria request's decisions a
// plan_criteria case.
func TestBoardRowsApplyOnTheirPreconditions(t *testing.T) {
	started := CaseFacts{Kind: CaseDependencyFailed, Attempts: 1, Dependencies: []DepFact{{TaskID: "d", Status: "failed"}}}
	fresh := CaseFacts{Kind: CaseDependencyFailed, Dependencies: []DepFact{{TaskID: "d", Status: "failed"}}}
	dep := Resolution{Args: map[string]any{"depends_on": "d"}}
	for _, c := range []struct {
		name, action string
		f            CaseFacts
		r            Resolution
		ok           bool
	}{
		{"remove on a started card", ActRemoveDependency, started, dep, false},
		{"remove on a fresh card", ActRemoveDependency, fresh, dep, true},
		{"set on a started card", ActSetDependency, started, dep, false},
		{"set on a fresh card", ActSetDependency, fresh, dep, true},
		{"file on a dependency case", ActFileCard, fresh, Resolution{Args: map[string]any{"title": "x"}}, true},
		{"file on a session cap", ActFileCard, CaseFacts{Kind: CaseSessionCap}, Resolution{Args: map[string]any{"title": "x"}}, false},
		{"file with no title", ActFileCard, fresh, Resolution{}, false},
		{"restart a plan at rest", ActRestartPlan, CaseFacts{Plan: "p", PlanStatus: "done"}, Resolution{Note: "again"}, true},
		{"restart a running plan", ActRestartPlan, CaseFacts{Plan: "p", PlanStatus: "running"}, Resolution{Note: "again"}, false},
		{"restart a direct card", ActRestartPlan, CaseFacts{}, Resolution{Note: "again"}, false},
		{"restart with no note", ActRestartPlan, CaseFacts{Plan: "p", PlanStatus: "done"}, Resolution{}, false},
		{"verdict on a criteria case", ActAnswerCriteria, CaseFacts{Kind: CasePlanCriteria}, Resolution{Args: map[string]any{"criterion": "k1", "verdict": "met"}}, true},
		{"verdict on a merge of a criteria card", ActAnswerCriteria, CaseFacts{Kind: CaseMergeApproval, Criteria: true}, Resolution{Args: map[string]any{"criterion": "k1", "verdict": "met"}}, true},
		{"verdict on a merge of another card", ActAnswerCriteria, CaseFacts{Kind: CaseMergeApproval}, Resolution{Args: map[string]any{"criterion": "k1", "verdict": "met"}}, false},
		{"waive with no reason", ActAnswerCriteria, CaseFacts{Kind: CasePlanCriteria}, Resolution{Args: map[string]any{"criterion": "k1", "verdict": "waive"}}, false},
		{"grant on a criteria case", ActGrantCriteria, CaseFacts{Kind: CasePlanCriteria}, Resolution{}, true},
		{"grant on a merge case", ActGrantCriteria, CaseFacts{Kind: CaseMergeApproval}, Resolution{}, false},
		{"confirm on a criteria case", ActConfirmCriteria, CaseFacts{Kind: CasePlanCriteria}, Resolution{}, true},
		{"grant on a never-judged criteria case", ActGrantCriteria, CaseFacts{Kind: CasePlanCriteria, ApprovalKind: ApprovalCriteriaRerun}, Resolution{}, true},
		{"run on criteria never judged", ActRunCriteria, CaseFacts{Kind: CasePlanCriteria, ApprovalKind: ApprovalCriteriaRerun, Runnable: []string{"k1"}}, Resolution{}, true},
		{"run where no test can run", ActRunCriteria, CaseFacts{Kind: CasePlanCriteria, ApprovalKind: ApprovalCriteriaRerun}, Resolution{}, false},
		{"run on a criteria gap", ActRunCriteria, CaseFacts{Kind: CasePlanCriteria, ApprovalKind: ApprovalCriteria}, Resolution{}, false},
		{"run on the unproven flag", ActRunCriteria, CaseFacts{Kind: CaseMonitorFlag, Flag: "criterion_unproven"}, Resolution{}, true},
		{"run on another flag", ActRunCriteria, CaseFacts{Kind: CaseMonitorFlag, Flag: "wait_over_bound"}, Resolution{}, false},
		{"deny_merge with no reason", ActDenyMerge, CaseFacts{Kind: CaseMergeApproval, PROpen: true}, Resolution{}, false},
		{"deny_merge with a reason", ActDenyMerge, CaseFacts{Kind: CaseMergeApproval, PROpen: true}, Resolution{Note: "no"}, true},
	} {
		if got := applies(t, c.action, c.f, c.r); (got == "") != c.ok {
			t.Errorf("%s: %s applies = %q, want applies %v", c.name, c.action, got, c.ok)
		}
	}
}

// Settling a plan's criteria by word is a human's act. On a criteria case
// and on the merge of a criteria card, rung 2 takes no verdict (waive, met,
// not_met), no confirmation and no denial: each is refused with what rung 2
// may do instead, which on a criteria case is the grant or the hand-up with
// the word as its recommendation, and on a merge the hand-up alone; its
// statement lists each as not offered with that reason. The human takes
// all of them, and rung 2 keeps the grant, which buys a card and waives
// nothing.
func TestSettlingCriteriaByWordIsTheHumans(t *testing.T) {
	const want = "settling a plan's criteria by word is the human's: take `grant_criteria` for one more bounded card, or hand the case up (`hand_up`) with `answer_criteria` (waive) or `confirm_criteria` as the recommendation"
	const onMerge = "settling a plan's criteria by word is the human's: hand the case up (`hand_up`) with `answer_criteria` as the recommendation"
	criteria := CaseFacts{Kind: CasePlanCriteria}
	merge := CaseFacts{Kind: CaseMergeApproval, Criteria: true, Head: "h1", PR: 9, PROpen: true, Checks: "green"}
	verdict := func(v, note string) Resolution {
		return Resolution{Args: map[string]any{"criterion": "k1", "verdict": v}, Note: note}
	}
	for _, c := range []struct {
		name, action string
		f            CaseFacts
		r            Resolution
		want         string
	}{
		{"waive", ActAnswerCriteria, criteria, verdict("waive", "out of scope"), want},
		{"met on a merge", ActAnswerCriteria, merge, verdict("met", ""), onMerge},
		{"not_met on a merge", ActAnswerCriteria, merge, verdict("not_met", "the page breaks"), onMerge},
		{"confirm", ActConfirmCriteria, criteria, Resolution{Note: "main holds every page"}, want},
	} {
		a, _ := ActionByName(c.action)
		c.r.Action = c.action
		if got := a.refusal(c.f, 2, c.r); got != c.want {
			t.Errorf("%s: rung 2's refusal = %q, want %q", c.name, got, c.want)
		}
		if got := a.refusal(c.f, 3, c.r); got != "" {
			t.Errorf("%s: the human's refusal = %q, want none", c.name, got)
		}
	}
	// over a closed card rung 2 hears at once that the answer is the human's,
	// with a note or with none; the human hears that the note is missing
	closed := CaseFacts{Kind: CasePlanCriteria, ClosedCard: true}
	confirm, _ := ActionByName(ActConfirmCriteria)
	for _, note := range []string{"", "main holds every page"} {
		if got := confirm.refusal(closed, 2, Resolution{Action: ActConfirmCriteria, Note: note}); got != want {
			t.Errorf("rung 2's confirmation over a closed card with the note %q = %q, want %q", note, got, want)
		}
	}
	if got := confirm.refusal(closed, 3, Resolution{Action: ActConfirmCriteria}); got != "a card of the plan ended closed, so the work is enough only with your evidence: the note" {
		t.Errorf("the human's confirmation with no note over a closed card = %q", got)
	}
	grant, _ := ActionByName(ActGrantCriteria)
	if got := grant.refusal(criteria, 2, Resolution{Action: ActGrantCriteria}); got != "" {
		t.Errorf("rung 2's grant = %q, want it taken", got)
	}
	text := new(Runner).rungStatement(Case{Kind: CasePlanCriteria, Problem: "criteria not met: k1"}, criteria, "", nil, 2)
	for _, name := range []string{ActAnswerCriteria, ActConfirmCriteria} {
		if line := "- " + name + ": not offered: " + want + "\n"; !strings.Contains(text, line) {
			t.Errorf("rung 2's statement lacks %q:\n%s", line, text)
		}
	}
	if !strings.Contains(text, "- "+ActGrantCriteria+"\n") {
		t.Errorf("rung 2's statement does not offer the grant:\n%s", text)
	}
}

// A merge approval whose change is a migration, the runner's or a class the
// policy marks human keeps a human on its merge at full auto: rung 2's
// approve_merge is refused naming the class and the hand-up, and not
// offered, while the human may pick it and a change of another class stays
// rung 2's. The facts come from the policy through the case's own reading,
// so the test fails when class policy drops human.
func TestApproveMergeOfAHumanClassIsTheHumansAlone(t *testing.T) {
	var v policy.Version
	v.Document.Risk = policy.Risk{Classes: []policy.RiskClass{
		{Name: "migration", Outcome: policy.OutcomeRequiresApproval, Migration: true},
		{Name: "policy", Outcome: policy.OutcomeRequiresApproval, Human: true},
		{Name: "high", Outcome: policy.OutcomeRequiresApproval},
		{Name: "normal", Outcome: policy.OutcomeAuto},
	}}
	cfg := testRunnerConfig()
	cfg.Policy = func() policy.Version { return v }
	r := &Runner{cfg: cfg}
	a, _ := ActionByName(ActApproveMerge)
	for class, refused := range map[string]bool{"migration": true, "runner": true, "policy": true, "high": false, "normal": false} {
		f := CaseFacts{Kind: CaseMergeApproval, Head: "abcdef123", PR: 9, PROpen: true, Checks: "green", Autonomy: policy.AutonomyFullAuto}
		r.atCaseOf(context.Background(), &f, Task{}, Case{Kind: CaseMergeApproval, Facts: map[string]any{"class": class}})
		want := ""
		if refused {
			want = "risk class " + class + " keeps a human on its merge, whatever the autonomy: `approve_merge` is the human's alone, so hand it up (`hand_up`) with `approve_merge` as the recommendation"
		}
		if got := a.refusal(f, 2, Resolution{Action: ActApproveMerge}); got != want {
			t.Errorf("class %q: rung 2's `approve_merge` = %q, want %q", class, got, want)
		}
		if got := slices.ContainsFunc(Offered(f, 2), func(o Action) bool { return o.Name == ActApproveMerge }); got == refused {
			t.Errorf("class %q: `approve_merge` offered to rung 2 = %v, want %v", class, got, !refused)
		}
		if got := a.refusal(f, 3, Resolution{Action: ActApproveMerge}); got != "" {
			t.Errorf("class %q: the human's `approve_merge` = %q, want none", class, got)
		}
	}
}

// Rung 2 decides a merge at full auto alone: below it the merge rule is
// human, and rung 2's approve_merge and deny_merge are refused naming the
// state and the hand-up, and not offered. The human takes both at every
// level.
func TestRung2DecidesAMergeAtFullAutoAlone(t *testing.T) {
	for _, state := range []string{policy.AutonomyManual, policy.AutonomyAssisted, policy.AutonomyAutonomous, policy.AutonomyFullAuto} {
		f := CaseFacts{Kind: CaseMergeApproval, Head: "abcdef123", PR: 9, PROpen: true, Checks: "green", Autonomy: state}
		want := ""
		if state != policy.AutonomyFullAuto {
			want = "the merge rule is human at `" + state + "`: hand the case up (`hand_up`) with `approve_merge` or `deny_merge` as the recommendation"
		}
		for _, res := range []Resolution{{Action: ActApproveMerge}, {Action: ActDenyMerge, Note: "the rename is untested"}} {
			a, _ := ActionByName(res.Action)
			if got := a.refusal(f, 2, res); got != want {
				t.Errorf("%s: rung 2's %s = %q, want %q", state, res.Action, got, want)
			}
			if got := slices.ContainsFunc(Offered(f, 2), func(o Action) bool { return o.Name == res.Action }); got != (want == "") {
				t.Errorf("%s: %s offered to rung 2 = %v, want %v", state, res.Action, got, want == "")
			}
			if got := a.refusal(f, 3, res); got != "" {
				t.Errorf("%s: the human's %s = %q, want none", state, res.Action, got)
			}
		}
	}
}

// Rung 2 approves a head once: on a case whose head rung 2 approved on an
// earlier case of the same key, case 41, its approve_merge is refused
// naming that case and what fits, and not offered, while its deny_merge
// and the human's approve stay. Earlier cases that could not be read
// refuse rung 2's approve too, naming the read.
func TestRung2ApprovesAHeadOnceOnTheFacts(t *testing.T) {
	approved := CaseFacts{Kind: CaseMergeApproval, Head: "abcdef123", PR: 9, PROpen: true, Checks: "green", Autonomy: policy.AutonomyFullAuto, ApprovedOn: 41}
	unread := approved
	unread.ApprovedOn, unread.Partial = 0, []string{partialApprovedCases + ": connection reset"}
	approve, _ := ActionByName(ActApproveMerge)
	deny, _ := ActionByName(ActDenyMerge)
	for _, tc := range []struct {
		name string
		f    CaseFacts
		want string
	}{
		{"approved on case 41", approved, "rung 2 approved head `abcdef1` of pull request #9 on case 41 and its merge did not land: hand the case up (`hand_up`) with what the merge said"},
		{"unread", unread, "the earlier cases of this merge could not be read (connection reset), so rung 2 cannot tell whether it approved head `abcdef1` before: hand the case up (`hand_up`) with `approve_merge` as the recommendation"},
	} {
		if got := approve.refusal(tc.f, 2, Resolution{Action: ActApproveMerge}); got != tc.want {
			t.Errorf("%s: rung 2's approve_merge = %q, want %q", tc.name, got, tc.want)
		}
		if slices.ContainsFunc(Offered(tc.f, 2), func(o Action) bool { return o.Name == ActApproveMerge }) {
			t.Errorf("%s: approve_merge is offered to rung 2", tc.name)
		}
		if got := deny.refusal(tc.f, 2, Resolution{Action: ActDenyMerge, Note: "the merge failed twice"}); got != "" {
			t.Errorf("%s: rung 2's deny_merge = %q, want none", tc.name, got)
		}
		if got := approve.refusal(tc.f, 3, Resolution{Action: ActApproveMerge}); got != "" {
			t.Errorf("%s: the human's approve_merge = %q, want none", tc.name, got)
		}
	}
}

// Rung 0 extends only through the audit's own draw at a cap, which writes
// through the store and not through Act's table: no case offers rung 0 the
// extend, at a session cap or an agent time cap, while rungs 1 and 2 and the
// human keep it.
func TestRungZeroIsOfferedNoExtend(t *testing.T) {
	pool := policy.Draw{Sessions: 5, AgentTime: time.Hour, EUR: 20}
	for _, f := range []CaseFacts{
		{Kind: CaseSessionCap, SessionCap: 3, Attempts: 3, AgentTimeCapS: 7200, PoolLeft: pool, CardDrawLeft: pool},
		{Kind: CaseAgentTimeCap, SessionCap: 5, AgentTimeCapS: 1200, AgentTimeS: 1200, PoolLeft: pool, CardDrawLeft: pool},
	} {
		if got := offeredNames(f, 0); slices.Contains(got, ActExtend) {
			t.Errorf("%s: rung 0 is offered %v, want no extend", f.Kind, got)
		}
		for _, rung := range []int{1, 2, 3} {
			if got := offeredNames(f, rung); !slices.Contains(got, ActExtend) {
				t.Errorf("%s: rung %d is offered %v, want the extend", f.Kind, rung, got)
			}
		}
	}
}

// Money for the card lifts no spending limit of the factory or the
// project, and a case that holds none of the card's money has no limit to
// raise: neither answer is on offer there.
func TestMoneyLiftsNoCapAndRaisesNoLimitWithoutMoney(t *testing.T) {
	room := CaseFacts{Attempts: 1, SessionCap: 3, BudgetEUR: 2, SpentEUR: 1, LeftEUR: 1, CeilingEUR: 10,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	press := func(f CaseFacts, name string) string {
		a, _ := ActionByName(name)
		return a.refusal(f, 3, Resolution{Action: name, Args: map[string]any{"eur": 1.0}})
	}
	for _, kind := range []CaseKind{CaseDayCap, CaseMonthCap, CaseProjectCap} {
		f := room
		f.Kind = kind
		for _, name := range []string{ActFund, ActRaiseCeiling} {
			if press(f, name) == "" {
				t.Errorf("%s at %s is on offer", name, kind)
			}
		}
	}
	for _, kind := range []CaseKind{CaseMergeApproval, CasePlanQuestion, CasePlanConfirmation, CasePlanCriteria, CaseHumanCard, CaseJudgmentBudget, CaseRedMain, CaseRollHeld, CaseConfigRefused} {
		f := room
		f.Kind = kind
		if press(f, ActRaiseCeiling) == "" {
			t.Errorf("a raise of the spend limit on %s is on offer", kind)
		}
	}
	f := room
	f.ApprovalKind = ApprovalResource
	if press(f, ActRaiseCeiling) == "" {
		t.Error("a raise of the spend limit on a resource request is on offer")
	}
	f = room
	f.Kind = CaseBudgetFloor
	if why := press(f, ActRaiseCeiling); why != "" {
		t.Errorf("a raise at the budget floor is refused: %s", why)
	}
}

// A finding the review confirmed twice gets a fresh start: the human may
// rescue the work into a new card from the flag.
func TestSalvageTakesAFindingConfirmedTwice(t *testing.T) {
	a, _ := ActionByName(ActSalvage)
	if why := a.Applies(CaseFacts{Kind: CaseMonitorFlag, Flag: "finding_confirmed_twice"}, Resolution{Action: ActSalvage}); why != "" {
		t.Errorf("salvage on finding_confirmed_twice: %s", why)
	}
	if why := a.Applies(CaseFacts{Kind: CaseMonitorFlag, Flag: "session_no_push"}, Resolution{Action: ActSalvage}); why == "" {
		t.Error("salvage on session_no_push applies")
	}
}

// A draw on an agent's question that waits at the human is weighed at the
// cap the question waits at, by the chat's rung 2 as by the human: both
// doors give one verdict on the same facts. While the question sits on a
// rung, the rung's draw is refused, and the refusal names the draw only
// where the draw lifts the question's cap.
func TestADrawOnAWaitingQuestionIsWeighedAtItsCap(t *testing.T) {
	room := CaseFacts{Kind: CaseAgentEscalate, AtHuman: true, Attempts: 1, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 7200, SessionLeastS: 300,
		BudgetEUR: 2, SpentEUR: 1, LeftEUR: 1, FloorEUR: 0.3, CeilingEUR: 10,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	sessionCap := func(f *CaseFacts) { f.Attempts = f.SessionCap }
	timeCap := func(f *CaseFacts) { f.AgentTimeS = f.AgentTimeCapS }
	floor := func(f *CaseFacts) { f.SpentEUR, f.LeftEUR = 1.9, 0.1 }
	dayCap := func(f *CaseFacts) {
		f.CapWait, _, _ = strings.Cut(CapWaitReason(policy.RefusedDayCap, "the gateway refused the spend", "state", time.Now().Add(time.Hour)), "\n")
	}
	extend := Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}
	extendTime := Resolution{Action: ActExtend, Args: map[string]any{"agent_time_s": 600}}
	fund := Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}}
	for _, c := range []struct {
		name string
		at   func(*CaseFacts)
		r    Resolution
		want string
	}{
		{"extend at the session cap", sessionCap, extend, ""},
		{"extend at the agent time cap", timeCap, extendTime, ""},
		{"fund at the budget floor", floor, fund, ""},
		{"fund at the day cap", dayCap, fund, capLiftsNot},
		// 0.10 EUR left under a 10.00 EUR limit: the floor's face holds the fund, and no branch a successor
		{"extend at the budget floor", floor, extend, "this card waits on money, not a session (the card's answers at this stop: `fund`, `close` and `note`)"},
	} {
		f := room
		c.at(&f)
		a, _ := ActionByName(c.r.Action)
		human, chat := a.refusal(f, 3, c.r), a.refusal(f, 2, c.r)
		if human != c.want || chat != c.want {
			t.Errorf("%s: the human's press = %q, the chat's = %q; want both %q", c.name, human, chat, c.want)
		}
	}
	waits := ". A human's draw waits until the question reaches the human"
	hand := "this ask is the agent's question, which a draw does not answer: hand it up with the draw as your recommendation" + waits
	answer := "this ask is the agent's question, which a draw does not answer: answer it, or hand it up" + waits
	for _, c := range []struct {
		name string
		at   func(*CaseFacts)
		r    Resolution
		want string
	}{
		{"extend at the session cap", sessionCap, extend, hand},
		{"extend of agent time at the agent time cap", timeCap, extendTime, hand},
		{"fund at the budget floor", floor, fund, hand},
		{"extend of agent time at the session cap", sessionCap, extendTime, answer},
		{"extend of sessions at the agent time cap", timeCap, extend, answer},
		{"extend at the budget floor", floor, extend, answer},
		{"fund at the session cap", sessionCap, fund, answer},
		{"fund at no cap", func(*CaseFacts) {}, fund, answer},
		{"fund at the day cap", dayCap, fund, answer},
	} {
		f := room
		f.AtHuman = false
		c.at(&f)
		a, _ := ActionByName(c.r.Action)
		if got := a.refusal(f, 2, c.r); got != c.want {
			t.Errorf("on the rung, %s = %q, want %q", c.name, got, c.want)
		}
	}
}

// The author's answer to an information request buys the session it starts
// while the run still grants one (grantPoster): the session cap and the
// agent time cap keep neither the request off its face nor its answer
// off the card. Money still binds, and once the author's grants are
// spent the cap comes first.
func TestTheAuthorsAnswerBuysItsSession(t *testing.T) {
	asked := CaseFacts{Kind: CaseAgentRequestInfo, AtHuman: true, PosterGrant: true, Attempts: 3, SessionCap: 3, AgentTimeS: 7200, AgentTimeCapS: 7200,
		SessionLeastS: 300, BudgetEUR: 2, SpentEUR: 1, LeftEUR: 1, FloorEUR: 0.3, CeilingEUR: 10}
	answer := Resolution{Action: ActContinue, Note: "fsn1"}
	a, _ := ActionByName(ActContinue)
	if k := asked.atCap().Kind; k != CaseAgentRequestInfo {
		t.Errorf("at both caps with the author's grant left the face is %s's, want the request's", k)
	}
	if why := a.refusal(asked, 3, answer); why != "" {
		t.Errorf("the human's answer is refused: %s", why)
	}
	spent := asked
	spent.PosterGrant = false
	if k := spent.atCap().Kind; k != CaseSessionCap {
		t.Errorf("with the author's grants spent the face is %s's, want the session cap's", k)
	}
	broke := asked
	broke.SpentEUR, broke.LeftEUR = 1.9, 0.1
	if k := broke.atCap().Kind; k != CaseBudgetFloor {
		t.Errorf("out of money the face is %s's, want the budget floor's", k)
	}
}

// A plan with open criteria is restarted by no rung 2: the restart's close
// waives them, and a waiver drops a requirement of the spec, the human's.
// Rung 2's restart_plan is refused naming the hand-up and is not offered,
// and so it is when the count could not be read; the human may pick it,
// and a plan with none open stays rung 2's.
func TestRestartPlanOverOpenCriteriaIsTheHumansAlone(t *testing.T) {
	a, _ := ActionByName(ActRestartPlan)
	const why = ", which a restart's close waives, and a waiver drops a requirement of the spec, which is the human's: hand it up (`hand_up`) with `restart_plan` as the recommendation"
	for _, c := range []struct {
		name    string
		open    int
		partial []string
		want    string
	}{
		{"none open", 0, nil, ""},
		{"one open", 1, nil, "the plan holds 1 open criterion" + why},
		{"three open", 3, nil, "the plan holds 3 open criteria" + why},
		{"the count unread", 0, []string{"open criteria"},
			"the plan's open criteria could not be read, so the factory agent cannot tell whether a restart's close waives any, and a waiver is the human's: hand it up (`hand_up`) with `restart_plan` as the recommendation"},
	} {
		f := CaseFacts{Kind: CaseDependencyFailed, Plan: "p", PlanStatus: "done", OpenCriteria: c.open, Partial: c.partial}
		if got := a.refusal(f, 2, Resolution{Action: ActRestartPlan, Note: "one card"}); got != c.want {
			t.Errorf("%s: rung 2's restart_plan = %q, want %q", c.name, got, c.want)
		}
		if got := slices.ContainsFunc(Offered(f, 2), func(o Action) bool { return o.Name == ActRestartPlan }); got != (c.want == "") {
			t.Errorf("%s: restart_plan offered to rung 2 = %v, want %v", c.name, got, c.want == "")
		}
		if got := a.refusal(f, 3, Resolution{Action: ActRestartPlan, Note: "one card"}); got != "" {
			t.Errorf("%s: the human's restart_plan = %q, want none", c.name, got)
		}
	}
}

// The case facts of the successor that carries a plan card's key name its
// plan, so a replan while it runs goes to that plan, and the integration
// key's successor answers the plan's criteria on its merge approval.
func TestCaseFactsOfAPlanCardsSuccessorNameItsPlan(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "failed")
	succ := SuccessorID("card-1")
	s.tasks[succ] = Task{ID: succ, Kind: "agent", Repository: "linkshort/linkshort", Parent: "t-plan", Source: SourceSuccessor, PlanKey: "card-1",
		Status: "running", Track: policy.TrackDirect}
	f, err := r.tripFacts(ctx, s.tasks[succ], "", "")
	if err != nil || f.Plan != "t-plan" || !f.Criteria {
		t.Fatalf("the successor's facts: plan %q, criteria %v, %v; want t-plan and true", f.Plan, f.Criteria, err)
	}
	a, _ := ActionByName(ActReplan)
	if why := a.refusal(f, 2, Resolution{Action: ActReplan}); why != "" {
		t.Fatalf("replan of the running successor = %q, want it offered", why)
	}
}

// The case facts of a plan and of its card count the plan's open criteria
// by its Close's rule. On the plan's wait on its failed card, Act refuses
// rung 2's restart_plan over them with the table's reason; once every
// criterion is waived the count is 0.
func TestCaseFactsCountThePlansOpenCriteria(t *testing.T) {
	ctx := context.Background()
	r, s, _ := tenCriteriaPlan(t, "done", "failed")
	for _, id := range []string{"t-plan", "card-1"} {
		if f, err := r.tripFacts(ctx, s.tasks[id], "", ""); err != nil || f.OpenCriteria != 10 {
			t.Fatalf("tripFacts of %s = %d open criteria, %v; want 10", id, f.OpenCriteria, err)
		}
	}
	s.cases = []Case{{ID: 9, TaskID: "t-plan", Kind: CaseDependencyFailed, Status: CaseOpen}}
	_, err := r.Act(ctx, ActInput{TaskID: "t-plan", CaseID: 9, By: "sebastian", Rung: 2, Res: Resolution{Action: ActRestartPlan, Note: "one card"}})
	want := "the plan holds 10 open criteria, which a restart's close waives, and a waiver drops a requirement of the spec, which is the human's: hand it up (`hand_up`) with `restart_plan` as the recommendation"
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("rung 2's restart_plan = %v, want the refusal %q", err, want)
	}
	var waivers []CriterionVerdict
	for i := 1; i <= 10; i++ {
		waivers = append(waivers, CriterionVerdict{Criterion: fmt.Sprintf("k%d", i), Verdict: "waived", Evidence: "x", Kind: CriterionInvariant})
	}
	if err := s.PutCriteriaVerdicts(ctx, "card-1", CriteriaSourceApprover, "sebastian", waivers); err != nil {
		t.Fatal(err)
	}
	if f, err := r.tripFacts(ctx, s.tasks["card-1"], "", ""); err != nil || f.OpenCriteria != 0 {
		t.Fatalf("tripFacts after the waivers = %d open criteria, %v; want 0", f.OpenCriteria, err)
	}
}
