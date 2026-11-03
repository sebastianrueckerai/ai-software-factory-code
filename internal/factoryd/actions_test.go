package factoryd

import (
	"slices"
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
// a session can start on and the money an opening needs; a handover needs
// the same and names its model.
func TestResumeAppliesOnlyWithRoom(t *testing.T) {
	room := CaseFacts{Attempts: 2, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, LeftEUR: 1, FloorEUR: 0.3}
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
		{"at the cap", ActResume, with(func(f *CaseFacts) { f.Attempts = 3 }), Resolution{}, "the session cap of 3 is reached"},
		{"below the floor", ActResume, with(func(f *CaseFacts) { f.LeftEUR = 0.2 }), Resolution{}, "0.20 EUR is left and an opening needs 0.30 EUR"},
		{"running", ActResume, with(func(f *CaseFacts) { f.Running = true }), Resolution{}, "a session is running"},
		// 1200 s cap, 1100 s used: 100 s left, under the 5 minutes a session needs
		{"time short", ActResume, with(func(f *CaseFacts) { f.AgentTimeS = 1100 }), Resolution{},
			"the agent time cap of 20 minutes leaves 2 minutes and a session needs 5 minutes"},
		{"time spent", ActResume, with(func(f *CaseFacts) { f.AgentTimeS = 1200 }), Resolution{}, "the agent time cap of 20 minutes is reached"},
		{"handover same", ActHandover, room, Resolution{Args: map[string]any{"model": "same"}}, ""},
		{"handover at the cap", ActHandover, with(func(f *CaseFacts) { f.Attempts = 3 }), Resolution{Args: map[string]any{"model": "stronger"}},
			"the session cap of 3 is reached"},
		{"handover to no rung", ActHandover, room, Resolution{Args: map[string]any{"model": "best"}},
			`a handover names its model as same, stronger or cheaper, not "best"`},
	} {
		if got := applies(t, c.action, c.f, c.r); got != c.want {
			t.Errorf("%s: %s applies = %q, want %q", c.name, c.action, got, c.want)
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

// A draw after one that bought nothing (no push since it, the head and the
// checks as they were) is refused for an extend, a fund and a draw beside
// any action; a push, a new head or new checks lift the refusal, and a
// draw whose row recorded no head and checks never refuses.
func TestExtendRefusesWhenTheLastDrawBoughtNothing(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	stuck := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "red", LastDrawAt: &at, DrawHead: "h1", DrawChecks: "red",
		PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		BudgetEUR: 5, CeilingEUR: 10}
	const refused = "the last draw at 10:04 UTC bought nothing: no push since and the head and the checks are unchanged"
	extend := Resolution{Args: map[string]any{"sessions": 1}}
	if got := applies(t, ActExtend, stuck, extend); got != refused {
		t.Errorf("extend applies = %q, want %q", got, refused)
	}
	if got := applies(t, ActFund, stuck, Resolution{Args: map[string]any{"eur": 1.0}}); got != refused {
		t.Errorf("fund applies = %q, want %q", got, refused)
	}
	note, _ := ActionByName(ActNote)
	if got := note.refusal(stuck, 1, Resolution{Action: ActNote, Note: "one more", Fund: &policy.Draw{Sessions: 1}}); got != refused {
		t.Errorf("a note with a draw beside it = %q, want %q", got, refused)
	}
	pushed, moved, rechecked, unrecorded := stuck, stuck, stuck, stuck
	pushed.PushedSinceDraw, moved.Head, rechecked.Checks, unrecorded.LastDrawAt = true, "h2", "green", nil
	for name, f := range map[string]CaseFacts{"a push since": pushed, "a new head": moved, "new checks": rechecked, "no recorded state": unrecorded} {
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
	if got, want := fund.refusal(room, 0, Resolution{Action: ActFund, Args: map[string]any{"eur": 1.0}}), "rung 0 may not pick fund"; got != want {
		t.Errorf("rung 0's fund = %q, want %q", got, want)
	}
}

// A green approved head at the session cap, its budget at the ceiling and
// the pool spent: the human is offered the routes to merge, an extend
// past the pool, a successor, a replan of the direct card and a close; rung
// 0, bound by the card's draw, gets no extend and withdraws instead of
// closing.
func TestOfferedFollowsTheTable(t *testing.T) {
	f := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Attempts: 3, SessionCap: 3,
		AgentTimeS: 900, AgentTimeCapS: 1800, BudgetEUR: 5, SpentEUR: 5, LeftEUR: 0, FloorEUR: 0.3, CeilingEUR: 5}
	if got, want := offeredNames(f, 3), []string{ActRouteApproval, ActReverify, ActExtend, ActSuccessor, ActReplan, ActClose}; !slices.Equal(got, want) {
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
// fix; a named one is refused with its state.
func TestRetryTakesARetryableFailedDependency(t *testing.T) {
	retried := DepFact{TaskID: "d1", Issue: 20, Status: "failed", Why: "session error: the pod is gone", Replaced: RetryID("d1")}
	forbidden := DepFact{TaskID: "d2", Issue: 21, Status: "failed", Why: "policy version 7: risk class forbidden is forbidden to agents: .forgejo/ci.yaml"}
	running := DepFact{TaskID: "d3", Issue: 22, Status: "running"}
	open := DepFact{TaskID: "d4", Issue: 23, Status: "failed", Why: "checks: timeout"}
	f := CaseFacts{Dependencies: []DepFact{retried, forbidden, running}}
	if got, want := applies(t, ActRetry, f, Resolution{}), "no failed dependency is open to a retry"; got != want {
		t.Errorf("no retryable dependency = %q, want %q", got, want)
	}
	for id, want := range map[string]string{
		"d1": "dependency #20 was retried as " + RetryID("d1") + " already",
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
