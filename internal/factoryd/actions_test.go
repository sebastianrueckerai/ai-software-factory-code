package factoryd

import (
	"context"
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

// A resume needs no session running, an attempt under the cap, agent time
// a session can start on and the money an opening needs, each refusal with
// the action that makes room; caps or a floor that could not be read
// refuse. A handover needs the same and names its model.
func TestResumeAppliesOnlyWithRoom(t *testing.T) {
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
		{"at the cap", ActResume, with(func(f *CaseFacts) { f.Attempts = 3 }), Resolution{}, "the session cap of 3 is reached: extend or close"},
		{"below the floor", ActResume, with(func(f *CaseFacts) { f.LeftEUR = 0.2 }), Resolution{}, "0.20 EUR is left for a session and an opening needs 0.30 EUR: fund"},
		// 0.70 left less the 0.40 gate reserve is 0.30 for a session, under the 0.60 floor
		{"under the reserve", ActResume, with(func(f *CaseFacts) { f.LeftEUR, f.ReserveEUR, f.FloorEUR = 0.7, 0.4, 0.6 }), Resolution{},
			"0.30 EUR is left for a session and an opening needs 0.60 EUR: fund"},
		{"running", ActResume, with(func(f *CaseFacts) { f.Running = true }), Resolution{}, "a session is running"},
		// 1200 s cap, 1100 s used: 100 s left, under the 5 minutes a session needs
		{"time short", ActResume, with(func(f *CaseFacts) { f.AgentTimeS = 1100 }), Resolution{},
			"the agent time cap of 20 minutes leaves 2 minutes and a session needs 5 minutes: extend"},
		{"time spent", ActResume, with(func(f *CaseFacts) { f.AgentTimeS = 1200 }), Resolution{}, "the agent time cap of 20 minutes is reached: extend"},
		{"cap unread", ActResume, with(func(f *CaseFacts) { f.AgentTimeCapS, f.Partial = 0, []string{"config"} }), Resolution{},
			"the agent time cap could not be read: try again in a minute, or close the card"},
		{"floor unread", ActResume, with(func(f *CaseFacts) { f.FloorEUR, f.Partial = 0, []string{"floor"} }), Resolution{},
			"what an opening needs could not be read: try again in a minute, or close the card"},
		{"handover same", ActHandover, room, Resolution{Args: map[string]any{"model": "same"}}, ""},
		{"handover at the cap", ActHandover, with(func(f *CaseFacts) { f.Attempts = 3 }), Resolution{Args: map[string]any{"model": "stronger"}},
			"the session cap of 3 is reached: extend or close"},
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
		DrawFindings: []string{idA},
		PoolLeft:     policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
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
	if got, want := drawWithin(f, 1, one, false), "the card's draw bound leaves 0 sessions, 0 seconds of agent time and 0.00 EUR: rung 2 or the human may draw more"; got != want {
		t.Errorf("rung 1 = %q, want %q", got, want)
	}
	if got := drawWithin(f, 2, one, false); got != "" {
		t.Errorf("rung 2 within the pool = %q, want none", got)
	}
	past := policy.Draw{Sessions: 3}
	if got, want := drawWithin(f, 2, past, false), "the escalation pool leaves 2 sessions, 60 minutes of agent time and 0.30 EUR this month: only the human draws more"; got != want {
		t.Errorf("rung 2 past the pool = %q, want %q", got, want)
	}
	if got := drawWithin(f, 3, past, false); got != "" {
		t.Errorf("rung 3 past the pool = %q, want none", got)
	}
	// 60 - 59.7 is 0.29999999999999716 in floats: 30 cents, which 0.30 EUR fits
	if got := drawWithin(f, 2, policy.Draw{EUR: 0.3}, true); got != "" {
		t.Errorf("rung 2 drawing the pool's last 30 cents = %q, want none", got)
	}
	if got, want := drawWithin(f, 3, policy.Draw{EUR: 4}, true), "4.00 EUR more takes the budget of 5.00 EUR past the card's ceiling of 8.00 EUR: raise the ceiling first"; got != want {
		t.Errorf("rung 3 past the ceiling = %q, want %q", got, want)
	}
	if got := drawWithin(f, 3, policy.Draw{EUR: 3}, true); got != "" {
		t.Errorf("rung 3 up to the ceiling = %q, want none", got)
	}
	if got, want := drawWithin(f, 4, one, false), "there is no rung 4"; got != want {
		t.Errorf("rung 4 = %q, want %q", got, want)
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
	if got, want := fund.refusal(room, 0, Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}}), "rung 0 may not pick fund: rungs 1, 2 and the human may"; got != want {
		t.Errorf("rung 0's fund = %q, want %q", got, want)
	}
	note, _ := ActionByName(ActNote)
	if got, want := note.refusal(room, 0, Resolution{Action: ActNote, Note: "x"}), "rung 0 may not pick note: rung 2 and the human may"; got != want {
		t.Errorf("rung 0's note = %q, want %q", got, want)
	}
	withdraw, _ := ActionByName(ActWithdraw)
	if got, want := withdraw.refusal(room, 0, Resolution{Action: ActWithdraw, Fund: &policy.Draw{EUR: 1}}), "rung 0 may not draw money: rungs 1, 2 and the human may"; got != want {
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
	if got := withdraw.refusal(room, 1, Resolution{Action: ActWithdraw}); !strings.Contains(got, "withdraw ends a monitor flag's case alone") {
		t.Errorf("rung 1's withdraw of a case the run waits on = %q, want it refused: the run would wait for good", got)
	}
	if got := note.refusal(room, 1, Resolution{Action: ActNote, Note: "x"}); got != "rung 1 may not pick note: rung 2 and the human may" {
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

// A retry takes a failed dependency nobody retried whose end a retry can
// fix, and a named one a retry replaced already, whose answer names the
// replacement; any other named one is refused with its state.
func TestRetryTakesARetryableFailedDependency(t *testing.T) {
	retried := DepFact{TaskID: "d1", Issue: 20, Status: "failed", Why: "session error: the pod is gone", Replaced: RetryID("d1")}
	forbidden := DepFact{TaskID: "d2", Issue: 21, Status: "failed", Why: "policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml"}
	running := DepFact{TaskID: "d3", Issue: 22, Status: "running"}
	open := DepFact{TaskID: "d4", Issue: 23, Status: "failed", Why: "checks: timeout"}
	f := CaseFacts{Dependencies: []DepFact{retried, forbidden, running}}
	if got, want := applies(t, ActRetry, f, Resolution{}), "no failed dependency is open to a retry"; got != want {
		t.Errorf("no retryable dependency = %q, want %q", got, want)
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
	const pool = "the escalation pool leaves 5 sessions, 300 minutes of agent time and 10.00 EUR this month: only the human draws more"
	for _, c := range []struct {
		name string
		rung int
		r    Resolution
		want string
	}{
		{"rung 3 raises past the pool", 3, raiseBy(20, nil), ""},
		{"rung 2 raises within it", 2, raiseBy(8, nil), ""},
		{"rung 2 raises within it and funds past it", 2, raiseBy(8, &policy.Draw{EUR: 5}), pool},
		{"rung 2 raises past it", 2, raiseBy(12, nil), pool},
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
	if got, want := extend.refusal(f, 7, Resolution{Action: ActExtend, Args: map[string]any{"sessions": 1}}), "there is no rung 7"; got != want {
		t.Errorf("rung 7: extend = %q, want %q", got, want)
	}
	closing, _ := ActionByName(ActClose)
	if got, want := closing.refusal(f, 1, Resolution{Action: ActClose}), "rung 1 may not pick close: rung 2 and the human may"; got != want {
		t.Errorf("rung 1's close = %q, want %q", got, want)
	}
	withdraw, _ := ActionByName(ActWithdraw)
	if got, want := withdraw.refusal(f, 3, Resolution{Action: ActWithdraw}), "the human may not pick withdraw: rungs 0 and 1 may"; got != want {
		t.Errorf("the human's withdraw = %q, want %q", got, want)
	}
}

// A raise of the ceiling moves no money into the budget, so the bought-
// nothing rule and the reads it is weighed on refuse an extend, a fund and
// a draw beside the raise, never the raise alone.
func TestRaiseCeilingIsNoDrawThatBuys(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	stuck := CaseFacts{Head: "h1", Checks: "red", Findings: []string{idA}, LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red",
		DrawFindings: []string{idA}, PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, BudgetEUR: 5, CeilingEUR: 6}
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
	f := CaseFacts{Head: "h1", Checks: "red", LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red", DrawFindings: []string{idA}}
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
	stuck := CaseFacts{Head: "h1", Checks: "red", LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red"}
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
// card's caps. A stop on money names the fund, any other case asked a
// question, which an extend does not answer, and the human is not offered
// one there.
func TestExtendAnswersACapAlone(t *testing.T) {
	const asked = "this card asked a question, not for a session: answer it, or extend at its cap"
	one := Resolution{Args: map[string]any{"sessions": 1}}
	for kind, want := range map[CaseKind]string{
		CaseSessionCap: "", CaseCheckinShare: "", CaseGateStop: "", CaseFindingsRepeated: "", "": "",
		CaseAgentEscalate: asked, CaseAgentRequestInfo: asked, CaseOther: asked, CaseCheckinHuman: asked,
		CaseBudgetFloor: "this card waits on money, not a session: fund it",
		CaseMonthCap:    "this card waits on the month cap, not a session: raise budget.month_eur or wait for the month to turn, then fund or retry",
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
	for _, tc := range []struct {
		name string
		r    Resolution
		want string
	}{
		{"sessions alone", Resolution{Args: map[string]any{"sessions": 1}},
			"this card stopped for agent time: an extend of its agent time starts the next session; sessions or lines alone buy none"},
		{"lines alone", Resolution{Args: map[string]any{"lines": 200}},
			"this card stopped for agent time: an extend of its agent time starts the next session; sessions or lines alone buy none"},
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

// A card short of money is answered by a fund: an extend there names it.
// At the month cap the gateway refuses every call until the cap moves, so
// the refusal names the cap before the fund.
func TestExtendOnAMoneyStopNamesTheFund(t *testing.T) {
	for kind, want := range map[CaseKind]string{
		CaseBudgetFloor: "this card waits on money, not a session: fund it",
		CaseMonthCap:    "this card waits on the month cap, not a session: raise budget.month_eur or wait for the month to turn, then fund or retry",
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
		facts CaseFacts
	}{{0, CaseFacts{}}, {1, CaseFacts{Final: "done"}}} {
		if got, want := closing.refusal(tc.facts, tc.rung, res), fmt.Sprintf("rung %d may not pick close: rung 2 and the human may", tc.rung); got != want {
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
	if got, want := resume.refusal(f, 3, Resolution{Action: ActResume}), "resume would do nothing on a monitor flag's case: the card's run waits on none"; got != want {
		t.Errorf("a resume of a monitor case = %q, want %q", got, want)
	}
	closing, _ := ActionByName(ActClose)
	if got, want := closing.refusal(f, 3, Resolution{Action: ActClose}),
		"close ends the card, not the flag's case: settle it with settle, which ends the case and the card goes on"; got != want {
		t.Errorf("a close of a live card's monitor case = %q, want %q", got, want)
	}
	ended := f
	ended.Final = "done"
	if got := closing.refusal(ended, 3, Resolution{Action: ActClose}); got != "" {
		t.Errorf("a close of an ended card's monitor case = %q, want it taken", got)
	}
	if got, want := applies(t, ActSettle, CaseFacts{Kind: CaseSessionCap}, Resolution{}),
		"settle ends a monitor flag's case alone, and this case is no monitor flag's"; got != want {
		t.Errorf("a settle of a cap case = %q, want %q", got, want)
	}
}

// A budget floor's money fact reads what a session may spend, the gate
// reserve held out: 0.70 EUR left less the 0.40 reserve is 0.30, and an
// opening of 0.60 needs 0.30 EUR more.
func TestTripMoneyHoldsTheReserveOut(t *testing.T) {
	f := CaseFacts{BudgetEUR: 3, SpentEUR: 2.3, LeftEUR: 0.7, ReserveEUR: 0.4, FloorEUR: 0.6, Model: "factory/small"}
	want := CaseMoney{Kind: "budget", BudgetEUR: 3, SpentEUR: 2.3, NeedEUR: 0.3, Buys: "one session on factory/small"}
	if got := tripMoney(CaseBudgetFloor, f); got == nil || *got != want {
		t.Fatalf("money = %+v, want %+v", got, want)
	}
}

// The money fact rounds up to the cent: an opening of 0.604 against a
// session ceiling of 0.11 - 0.40 = -0.29 needs 0.894, so 0.90, not 0.89.
func TestTripMoneyRoundsTheNeedUp(t *testing.T) {
	f := CaseFacts{BudgetEUR: 3, SpentEUR: 2.89, LeftEUR: 0.11, ReserveEUR: 0.4, FloorEUR: 0.604}
	if got := tripMoney(CaseBudgetFloor, f); got == nil || got.NeedEUR != 0.9 {
		t.Fatalf("money = %+v, want a need of 0.90", got)
	}
}

// A planner never pushes: its draw bought something once it filed a
// revision since. At an agent time stop the draw weighed is the newest that
// raised agent time: a draw of money or sessions beside or after it raised
// nothing the stop names.
func TestBoughtNothingWeighsAPlannerOnItsRevision(t *testing.T) {
	at := time.Date(2026, 11, 10, 11, 29, 0, 0, time.UTC)
	f := CaseFacts{Kind: CaseAgentTimeCap, TimeDraw: &DrawFacts{At: at}}
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
	f.Kind = CaseAgentTimeCap
	if f.LastDrawAt == nil || f.TimeDraw == nil || f.TimeDraw.Revised || !boughtNothing(f) {
		t.Fatalf("facts %+v; want the extend's time draw weighed and bought nothing", f)
	}
	f.Kind = CaseSessionCap
	if !boughtNothing(f) {
		t.Error("the newest draw, the fund, bought nothing at a session cap")
	}
	var after CaseFacts
	after.eventFacts([]Event{revision, fund, extend})
	after.Kind = CaseAgentTimeCap
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
		{"planner at a time stop", CaseFacts{Kind: CaseAgentTimeCap, Planner: true, TimeDraw: &DrawFacts{At: at}},
			"the last agent-time draw at 11:29" + fmt.Sprintf(tail, "plan revision")},
		{"agent at a time stop", CaseFacts{Kind: CaseAgentTimeCap, TimeDraw: &DrawFacts{At: at}},
			"the last agent-time draw at 11:29" + fmt.Sprintf(tail, "push")},
		{"agent at a session cap", CaseFacts{Kind: CaseSessionCap, LastDrawAt: &at},
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
		{"merge stop", merge, "wait_checks runs a stopped checks wait again, and this case is no checks wait's stop"},
		{"other case", quiet, "wait_checks runs a stopped checks wait again, and this case is no checks wait's stop"},
		{"no pull request", noPR, "no pull request is open"},
	} {
		if got := applies(t, ActWaitChecks, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: wait_checks applies = %q, want %q", c.name, got, c.want)
		}
	}
	if !slices.Contains(offeredNames(stop, 2), ActWaitChecks) || slices.Contains(offeredNames(stop, 2), ActResume) {
		t.Errorf("rung 2 is offered %v at the cap, want wait_checks and no resume", offeredNames(stop, 2))
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

// route_approval refuses a checks-wait stop and names wait_checks, whose
// once-per-head limit it would bypass.
func TestRouteApprovalRefusesAChecksWaitStop(t *testing.T) {
	green := CaseFacts{Kind: CaseGateStop, Gate: ActivityWaitChecks, PROpen: true, Checks: "green", ReviewApproved: true}
	merge := green
	merge.Gate = ActivityMerge
	if got, want := applies(t, ActRouteApproval, green, Resolution{}), "a checks wait's stop goes on with wait_checks, not route_approval"; got != want {
		t.Errorf("checks wait: route_approval applies = %q, want %q", got, want)
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
		if got, want := a.refusal(f, 2, Resolution{Action: ActWaitChecks}), "wait_checks runs a stopped checks wait again, and this case is no checks wait's stop"; got != want {
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
		{"pending", pending, "the checks on abcdef1 are pending, not green: approve_merge waits for green checks on the head"},
		{"closed", closed, "the pull request is not open, so there is nothing to approve"},
		{"a gate stop", gate, "approve_merge decides a merge request, and this case is no merge approval"},
	} {
		if got := applies(t, ActApproveMerge, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: approve_merge applies = %q, want %q", c.name, got, c.want)
		}
	}
	a, _ := ActionByName(ActApproveMerge)
	if got, want := a.refusal(ready, 1, Resolution{Action: ActApproveMerge}), "rung 1 may not pick approve_merge: rung 2 and the human may"; got != want {
		t.Errorf("rung 1's approve_merge = %q, want %q", got, want)
	}
	if got := a.refusal(ready, 3, Resolution{Action: ActApproveMerge, Fund: &policy.Draw{EUR: 1}}); got != "approve_merge decides the merge request alone: it draws nothing" {
		t.Errorf("approve_merge with a fund = %q", got)
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

// Every action of the table has its effect line, and the arguments a form
// asks for belong to actions of the table.
func TestEveryActionHasItsEffectLine(t *testing.T) {
	for _, a := range Actions {
		if ActionEffects[a.Name] == "" {
			t.Errorf("action %s has no effect line", a.Name)
		}
	}
	for name := range ActionArgs {
		if _, ok := ActionByName(name); !ok {
			t.Errorf("arguments for %s, which is no action", name)
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

// A case none of whose actions apply names the refusals of the actions the
// human may pick.
func TestActionRefusalsNameWhyTheHumansActionsDoNotApply(t *testing.T) {
	merge := CaseFacts{Kind: CaseMergeApproval, Head: "abcdef123", PR: 9, PROpen: true, Checks: "red"}
	for _, r := range ActionRefusals(merge, 3) {
		if r.Action == ActApproveMerge {
			if want := "the checks on abcdef1 are red, not green: approve_merge waits for green checks on the head"; r.Why != want {
				t.Errorf("approve_merge's refusal = %q, want %q", r.Why, want)
			}
			return
		}
	}
	t.Error("no refusal names approve_merge on red checks")
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
		{"no draw", Resolution{Args: same}, "the session cap of 3 is reached: extend or close"},
		{"agent time alone", Resolution{Args: same, Fund: &policy.Draw{AgentTime: time.Hour}}, "the session cap of 3 is reached: extend or close"},
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
		{"deny with no reason", ActDenyCriteria, CaseFacts{Kind: CasePlanCriteria}, Resolution{}, false},
		{"deny with a reason", ActDenyCriteria, CaseFacts{Kind: CasePlanCriteria}, Resolution{Note: "no"}, true},
		{"deny_merge with no reason", ActDenyMerge, CaseFacts{Kind: CaseMergeApproval, PROpen: true}, Resolution{}, false},
		{"deny_merge with a reason", ActDenyMerge, CaseFacts{Kind: CaseMergeApproval, PROpen: true}, Resolution{Note: "no"}, true},
	} {
		if got := applies(t, c.action, c.f, c.r); (got == "") != c.ok {
			t.Errorf("%s: %s applies = %q, want applies %v", c.name, c.action, got, c.ok)
		}
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
		r.atCaseOf(context.Background(), &f, Case{Kind: CaseMergeApproval, Facts: map[string]any{"class": class}})
		want := ""
		if refused {
			want = "risk class " + class + " keeps a human on its merge, whatever the autonomy: approve_merge is the human's alone, so hand_up with approve_merge as the recommendation"
		}
		if got := a.refusal(f, 2, Resolution{Action: ActApproveMerge}); got != want {
			t.Errorf("class %q: rung 2's approve_merge = %q, want %q", class, got, want)
		}
		if got := slices.ContainsFunc(Offered(f, 2), func(o Action) bool { return o.Name == ActApproveMerge }); got == refused {
			t.Errorf("class %q: approve_merge offered to rung 2 = %v, want %v", class, got, !refused)
		}
		if got := a.refusal(f, 3, Resolution{Action: ActApproveMerge}); got != "" {
			t.Errorf("class %q: the human's approve_merge = %q, want none", class, got)
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
