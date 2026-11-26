package factoryd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
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
// it stands at its limit, so the money stop holds the room, which raises
// the limit.
func TestResumeAppliesOnlyWithRoom(t *testing.T) {
	const capAnswers = " (the card's answers at this stop: `extend`, `close` and `note`)"
	const moneyAnswers = capAnswers
	room := CaseFacts{Attempts: 2, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3, MonthLeftEUR: 40}
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
		{"running", ActResume, with(func(f *CaseFacts) { f.Running = true }), Resolution{}, "a session is running: wait for it to end, then answer again"},
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
		{"green", green, "the checks on the head are green, so there is nothing to run again"},
		{"checks not read", CaseFacts{Head: "h1", PR: 9, PROpen: true}, "the checks on the head are not known yet, so there is nothing to run again"},
	} {
		if got := applies(t, ActRerunChecks, c.f, Resolution{}); got != c.want {
			t.Errorf("%s: rerun_checks applies = %q, want %q", c.name, got, c.want)
		}
	}
}

// A room after a draw that bought nothing (no push since it, the head, the
// checks and the findings as they were) is refused, for an extend and for
// a room beside any action; a push, a new head, new checks or new
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
	extend := extendBy(map[string]any{"sessions": 1})
	if got := refusalOn(t, stuck, 2, extend); got != refused {
		t.Errorf("extend = %q, want %q", got, refused)
	}
	if got := refusalOn(t, stuck, 2, Resolution{Action: ActNote, Note: "one more", Room: &RoomAsk{Sessions: 1, Money: true}}); got != refused {
		t.Errorf("a note with a room beside it = %q, want %q", got, refused)
	}
	unweighed := stuck
	unweighed.DrawFindings, unweighed.Findings = nil, []string{idB}
	if got := refusalOn(t, unweighed, 2, extend); got != refused {
		t.Errorf("a draw that recorded no findings: extend = %q, want %q", got, refused)
	}
	pushed, moved, rechecked, found, unrecorded := stuck, stuck, stuck, stuck, stuck
	pushed.PushedSinceDraw, moved.Head, rechecked.Checks, found.Findings, unrecorded.LastDrawAt = true, "h2", "green", []string{idA, idB}, nil
	for name, f := range map[string]CaseFacts{"a push since": pushed, "a new head": moved, "new checks": rechecked, "a new finding": found,
		"no recorded state": unrecorded} {
		if got := refusalOn(t, f, 2, extend); got != "" {
			t.Errorf("%s: extend = %q, want it taken", name, got)
		}
	}
}

// A give whose session still runs bought nothing yet: the guard weighs a
// draw once a session ended since it. A human gives 1 session's worth
// ahead of time to the ahead-of-time row on h1, red, with finding A, while
// its fourth session runs after 3, and the rows record the head, the
// checks, the finding and the 3 sessions the estimate came from, as
// caseDraw writes them. Before the session pushes, the human's second give
// is taken: 3 attempts are not above the draw's 3. Once the fourth session
// ended (4 attempts) with the head, the checks and the finding unchanged,
// a give is refused as today.
func TestAGiveWhileItsSessionRunsIsWeighedOnceItEnded(t *testing.T) {
	f := aheadOfTime()
	f.Head, f.PR, f.PROpen, f.Checks, f.Findings, f.Running = "h1", 9, true, "red", []string{idA}, true
	r, store, card := roomCardRig(t, 8)
	give := extendBy(map[string]any{"sessions": 1})
	d := r.caseDraw(card, f, Case{}, ActInput{TaskID: "t1", By: "sebastian", Rung: 3, Res: give}, ViaConsole)
	if _, err := store.DrawCard(context.Background(), "t1", *d); err != nil {
		t.Fatal(err)
	}
	rows := slices.Clone(store.events)
	for i := range rows {
		rows[i].At = time.Date(2026, 11, 21, 10, 4, 0, 0, time.UTC)
	}
	slices.Reverse(rows)
	running := f
	running.eventFacts(rows)
	if why := refusalOn(t, running, 3, give); why != "" {
		t.Errorf("a second give while the session runs = %q, want it taken", why)
	}
	ended := running
	ended.Attempts = 4
	const refused = "the last draw at 10:04 UTC bought nothing, with no push since and the head, the checks and the findings unchanged: " +
		"hand over, start a successor, replan or close"
	if got := refusalOn(t, ended, 3, give); got != refused {
		t.Errorf("a give after the session ended with nothing changed = %q, want %q", got, refused)
	}
}

// Rungs 0 and 1 draw within the card's draw bound and the pool, rung 2
// within the pool, the human past both. Money compares in cents: a room
// of 1 on the ahead-of-time row takes 1 start, 10 minutes and 0.30 EUR,
// which the pool's 60 - 59.7 EUR holds.
func TestDrawWithinByRung(t *testing.T) {
	f := aheadOfTime()
	f.CardDrawLeft, f.PoolLeft = policy.Draw{}, policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 60 - 59.7}
	one, two := RoomFor(f, 1, true), RoomFor(f, 2, true)
	if got, want := drawWithin(f, 1, one), "the card's draw bound leaves room for no more session's worth: hand it up (`hand_up`) for the factory agent or the human to give more"; got != want {
		t.Errorf("rung 1 = %q, want %q", got, want)
	}
	if got := drawWithin(f, 2, one); got != "" {
		t.Errorf("rung 2 within the pool = %q, want none", got)
	}
	if got, want := drawWithin(f, 2, two), "the escalation pool leaves room for 1 more session's worth this month, about 14 minutes and 1.20 EUR: ask for that, or hand it up (`hand_up`) for the human to give more"; got != want {
		t.Errorf("rung 2 past the pool = %q, want %q", got, want)
	}
	if got := drawWithin(f, 3, two); got != "" {
		t.Errorf("rung 3 past the pool = %q, want none", got)
	}
}

// A rung's refused extend names a count that fits, "ask for that",
// exactly while the extend stays on offer to it: at case 6354's agent
// time cap, where a session's worth is its 7220 seconds and the pool
// holds 924 minutes and no session, a room of 5 needs 3 starts and a room
// of 2 none; on an empty pool nothing fits, nor at the session cap with
// the pool's sessions gone.
func TestARefusedExtendIsOfferedOnlyWhileAFitRemains(t *testing.T) {
	timeCap := CaseFacts{Kind: CaseAgentTimeCap, Attempts: 1, SessionCap: 3, AgentTimeS: 7220, AgentTimeCapS: 10800,
		CardDrawLeft: policy.Draw{Sessions: 3, AgentTime: 90 * time.Minute, EUR: 6}, PoolLeft: policy.Draw{AgentTime: 924*time.Minute + 6*time.Second}}
	emptyPool, sessionCap := timeCap, timeCap
	emptyPool.PoolLeft = policy.Draw{}
	sessionCap.Kind, sessionCap.Attempts, sessionCap.AgentTimeS = CaseSessionCap, 3, 600
	for _, c := range []struct {
		name string
		f    CaseFacts
		n    int
		fit  bool
	}{
		{"6354's room of 5", timeCap, 5, true},
		{"a room of 1 on an empty pool", emptyPool, 1, false},
		{"a room of 1 at the session cap, the pool's sessions gone", sessionCap, 1, false},
	} {
		got := actRefusal(c.f, ActInput{CaseID: 6354, Rung: 2, Res: extendBy(map[string]any{"sessions": c.n})}, "#2109")
		_, list, _ := strings.Cut(got, ", and the factory agent may take ")
		offered := slices.Contains(strings.Split(strings.ReplaceAll(list, " and ", ", "), ", "), "`"+ActExtend+"`")
		if got == "" || strings.Contains(got, ": ask for that") != c.fit || offered != c.fit {
			t.Errorf("%s: %q; want a fit and the extend on offer %v", c.name, got, c.fit)
		}
	}
	got := actRefusal(timeCap, ActInput{CaseID: 6354, Rung: 2, Res: extendBy(map[string]any{"sessions": 5})}, "#2109")
	if want := "the escalation pool leaves room for 2 more sessions' worth this month, about 241 minutes: ask for that"; !strings.HasPrefix(got, want) {
		t.Errorf("6354's refusal = %q, want it to start %q", got, want)
	}
}

// An answer's draw carries the rooms of the counts below its own, with the
// money as asked, so the store's own refusal names a fit as Act does: on
// the ahead-of-time row a room of 1 takes 1 start, 10 minutes and 0.30
// EUR, a room of 2 takes 2 starts, 24 minutes and 1.50 EUR, or no money.
func TestCaseDrawCarriesTheRoomsBelowIt(t *testing.T) {
	g := newFactsRig(t)
	card := g.store.tasks["t1"]
	for _, c := range []struct {
		money bool
		eur   [2]int64
	}{{true, [2]int64{30, 150}}, {false, [2]int64{0, 0}}} {
		d := g.r.caseDraw(card, aheadOfTime(), Case{ID: 7}, ActInput{Rung: 2, Res: extendBy(map[string]any{"sessions": 3, "money": c.money})}, ViaChat)
		if d == nil || len(d.Below) != 2 {
			t.Fatalf("money %v: the draw = %+v, want the rooms of 1 and 2 below it", c.money, d)
		}
		for i, want := range []policy.Draw{{Sessions: 1, AgentTime: 10 * time.Minute}, {Sessions: 2, AgentTime: 24 * time.Minute}} {
			want.EUR = float64(c.eur[i]) / 100
			if got := d.Below[i]; got.N != i+1 || got.Money != c.money || got.draw() != want {
				t.Errorf("money %v: room %d = %+v, want %d sessions' worth drawing %+v", c.money, i+1, got, i+1, want)
			}
		}
	}
}

// Rung 0 never draws money, however much room the card has: a withdraw
// with a room beside it that holds 0.50 EUR is refused, one without its
// money is taken. A note is no answer of rung 0's, and rung 1 withdraws a
// monitor flag's ask alone.
func TestTheFactoryNeverDrawsMoney(t *testing.T) {
	room := CaseFacts{PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4},
		BudgetEUR: 5, CeilingEUR: 10, FloorEUR: 0.5, MonthLeftEUR: 40}
	note, _ := ActionByName(ActNote)
	if got, want := note.refusal(room, 0, Resolution{Action: ActNote, Note: "x"}), "the factory may not pick `note`: the factory agent and the human may"; got != want {
		t.Errorf("rung 0's note = %q, want %q", got, want)
	}
	withdraw, _ := ActionByName(ActWithdraw)
	if got, want := withdraw.refusal(room, 0, Resolution{Action: ActWithdraw, Room: &RoomAsk{Sessions: 1, Money: true}}), "the factory may not draw money: the check-in, the factory agent and the human may"; got != want {
		t.Errorf("rung 0's withdraw with money beside it = %q, want %q", got, want)
	}
	if got := withdraw.refusal(room, 0, Resolution{Action: ActWithdraw, Room: &RoomAsk{Sessions: 1}}); got != "" {
		t.Errorf("rung 0's withdraw with a room without money beside it = %q, want none", got)
	}
	flag := room
	flag.Kind = CaseMonitorFlag
	if got := withdraw.refusal(flag, 1, Resolution{Action: ActWithdraw, Room: &RoomAsk{Sessions: 1, Money: true}}); got != "" {
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
// where they approve), an extend that raises the limit past the pool, a
// successor, a replan of the direct card, a close and a note; rung 0,
// which draws no money, routes to approval, gets no extend and withdraws
// instead of closing.
func TestOfferedFollowsTheTable(t *testing.T) {
	f := CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Attempts: 3, SessionCap: 3,
		AgentTimeS: 900, AgentTimeCapS: 1800, BudgetEUR: 5, SpentEUR: 5, LeftEUR: 0, FloorEUR: 0.3, CeilingEUR: 5, MonthLeftEUR: 40}
	if got, want := offeredNames(f, 3), []string{ActReverify, ActExtend, ActSuccessor, ActReplan, ActClose, ActNote}; !slices.Equal(got, want) {
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

// A room's money and its raise of the spend limit draw from the pool
// together, the human is never refused on the pool, and no room is
// weighed while the reads it needs failed. A room of 2 on the
// ahead-of-time row under a 7.00 EUR limit takes 1.50 EUR and raises the
// limit by 0.50: 2.00 EUR, past a pool of 1.90 and within one of 2.00.
func TestRefusalBoundsEveryMoneyPart(t *testing.T) {
	f := aheadOfTime()
	f.CeilingEUR, f.PoolLeft = 7, policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 1.9}
	extend, _ := ActionByName(ActExtend)
	two := extendBy(map[string]any{"sessions": 2})
	if got, want := extend.refusal(f, 2, two), "the escalation pool leaves room for 1 more session's worth this month, about 14 minutes and 1.20 EUR: ask for that, or hand it up (`hand_up`) for the human to give more"; got != want {
		t.Errorf("rung 2's room past the pool = %q, want %q", got, want)
	}
	if got := extend.refusal(f, 3, two); got != "" {
		t.Errorf("the human's room past the pool = %q, want none", got)
	}
	f.PoolLeft.EUR = 2
	if got := extend.refusal(f, 2, two); got != "" {
		t.Errorf("rung 2's room within the pool = %q, want none", got)
	}
	for _, reader := range []string{"events", "checks"} {
		blind := f
		blind.Partial = []string{reader}
		want := "the card's " + reader + " could not be read, so a draw cannot be weighed against the last one: try again in a minute, or close the card"
		if got := extend.refusal(blind, 3, extendBy(map[string]any{"sessions": 1})); got != want {
			t.Errorf("%s unread: extend = %q, want %q", reader, got, want)
		}
	}
	blind := f
	blind.Partial = []string{"month"}
	if got, want := extend.refusal(blind, 3, extendBy(map[string]any{"sessions": 1})), "the project's money left this month could not be read, so a room's money cannot be weighed against it: try again in a minute, or give the room without its money"; got != want {
		t.Errorf("the month unread: extend = %q, want %q", got, want)
	}
	if got := extend.refusal(blind, 3, extendBy(map[string]any{"sessions": 1, "money": false})); got != "" {
		t.Errorf("the month unread: extend without money = %q, want none", got)
	}
	if got, want := extend.refusal(f, 7, extendBy(map[string]any{"sessions": 1})), "there is no decider 7"; got != want {
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

// An extend answers a cap: the session cap, the agent time cap, the money
// stop, a check-in's spend stop, and the owner exits of a gate stop and of
// findings that repeat, whose questions offer more room; with no case it
// gives a live card room. A spending limit of the factory or the project
// is no room of the card's, and any other case asked a question, which
// room does not answer, and the human is not offered one there.
func TestExtendAnswersACapAlone(t *testing.T) {
	const asked = "this card asked a question, not for room: answer it, or give room at its cap"
	one := Resolution{Args: map[string]any{"sessions": 1}}
	for kind, want := range map[CaseKind]string{
		CaseSessionCap: "", CaseAgentTimeCap: "", CaseBudgetFloor: "", CaseCheckinShare: "", CaseGateStop: "", CaseFindingsRepeated: "", "": "",
		CaseAgentEscalate: asked, CaseAgentRequestInfo: asked, CaseOther: asked, CaseCheckinHuman: asked, CaseMergeApproval: asked, CaseFactoryMonthCap: asked,
		CaseDayCap: capLiftsNot, CaseMonthCap: capLiftsNot, CaseProjectCap: capLiftsNot,
	} {
		if got := applies(t, ActExtend, CaseFacts{Kind: kind}, one); got != want {
			t.Errorf("an extend on %q = %q, want %q", kind, got, want)
		}
	}
	if got := offeredNames(CaseFacts{Kind: CaseAgentEscalate, SessionCap: 3, AgentTimeCapS: 7200}, 3); slices.Contains(got, ActExtend) {
		t.Errorf("the human on an ask is offered %v, want no extend", got)
	}
}

// An agent time cap is answered by the time a room lacks: on the
// agent-time stop row, 2 minutes left of a 14-minute session's worth, a
// room of 1 adds 12 minutes and no start, and every decider is offered it.
func TestExtendAtATimeCapGivesItsTime(t *testing.T) {
	timeCap := aheadOfTime()
	timeCap.Kind, timeCap.AgentTimeCapS = CaseAgentTimeCap, 44*60
	if r := RoomFor(timeCap, 1, true); r.Starts != 0 || r.AgentTime != 12*time.Minute || r.EUR != 0 {
		t.Errorf("the room of 1 = %+v, want 12 minutes alone", r)
	}
	for rung := range 4 {
		if got := offeredNames(timeCap, rung); !slices.Contains(got, ActExtend) {
			t.Errorf("rung %d at the agent time cap is offered %v, want the extend", rung, got)
		}
	}
}

// A card that ended takes no room: an extend is refused naming the end,
// and the human is offered none there.
func TestEndedCardTakesNoExtend(t *testing.T) {
	ended := CaseFacts{Final: "done"}
	if got, want := applies(t, ActExtend, ended, Resolution{Args: map[string]any{"sessions": 1}}), "this card ended done: more room buys nothing, file a new card"; got != want {
		t.Errorf("an extend on a done card = %q, want %q", got, want)
	}
	if got := offeredNames(ended, 3); slices.Contains(got, ActExtend) {
		t.Errorf("the human on a done card is offered %v, want no extend", got)
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
// replan, the settle of the flag and a note,
// and a resume is refused. Close is refused on the live card, since it would
// end the card, and taken on an ended one, whose issue it may close; a
// settle of any other case is refused.
func TestMonitorCaseOffersOnlyWhatActDoes(t *testing.T) {
	f := CaseFacts{Kind: CaseMonitorFlag, Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Attempts: 3, SessionCap: 3,
		AgentTimeS: 900, AgentTimeCapS: 1800, BudgetEUR: 5, SpentEUR: 5, LeftEUR: 0, FloorEUR: 0.3, CeilingEUR: 5}
	if got, want := offeredNames(f, 3), []string{ActReplan, ActSettle, ActNote}; !slices.Equal(got, want) {
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

// While the checks on the head of an open pull request run, the check-in
// and the factory agent take no replan of the card and are not offered one;
// a human may, and the replan of a failed dependency's work, which closes
// no pull request of this card, goes on. Checks that could not be read
// refuse it as unknown. Settled checks, green or red, refuse nothing.
func TestAReplanWaitsForTheHeadsChecks(t *testing.T) {
	replan, _ := ActionByName(ActReplan)
	f := CaseFacts{Kind: CaseMonitorFlag, Head: "1a2b3c4d5e", PR: 9, PROpen: true, Checks: "pending"}
	const pending = "the checks on 1a2b3c4 are pending: a replan would close pull request #9 before they judge its work, so %s takes no `replan` until they settle. " +
		"Once they settle red on a test outside the diff, `rerun_checks` runs them again"
	for rung, who := range map[int]string{1: "the check-in", 2: "the factory agent"} {
		if got, want := replan.refusal(f, rung, Resolution{Action: ActReplan}), fmt.Sprintf(pending, who); got != want {
			t.Errorf("rung %d's replan = %q, want %q", rung, got, want)
		}
		if slices.Contains(offeredNames(f, rung), ActReplan) {
			t.Errorf("rung %d is offered a replan while the checks run", rung)
		}
	}
	if got := replan.refusal(f, 3, Resolution{Action: ActReplan}); got != "" {
		t.Errorf("the human's replan = %q, want it taken", got)
	}
	dep := f
	dep.Kind, dep.Plan, dep.Dependencies = CaseDependencyFailed, "p", []DepFact{{TaskID: "d", Issue: 5, Status: "failed"}}
	if got := replan.refusal(dep, 1, Resolution{Action: ActReplan, Args: map[string]any{"task_id": "d"}}); got != "" {
		t.Errorf("rung 1's replan of a failed dependency = %q, want it taken", got)
	}
	unread := f
	unread.Checks, unread.Partial = "", []string{"checks"}
	if got, want := replan.refusal(unread, 1, Resolution{Action: ActReplan}),
		"the checks on the head could not be read, so it is unknown whether they still run, which `replan` needs: try again in a minute, or hand it up"; got != want {
		t.Errorf("rung 1's replan with the checks unread = %q, want %q", got, want)
	}
	for _, checks := range []string{"green", "red"} {
		settled := f
		settled.Checks = checks
		if got := replan.refusal(settled, 1, Resolution{Action: ActReplan}); got != "" {
			t.Errorf("rung 1's replan on %s checks = %q, want it taken", checks, got)
		}
	}
}

// No answer leads a stop's list: at a budget floor, a session cap and an
// agent time cap the human is offered the action table's rows in the
// table's order. The table holds reverify before extend, so on an open
// pull request reverify stands ahead of the extend at every stop.
func TestOfferedListsTheTableInItsOrderAtEveryStop(t *testing.T) {
	base := CaseFacts{Head: "abcdef123", PR: 9, PROpen: true, Pushed: true, Checks: "green", ReviewApproved: true,
		Attempts: 1, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, SessionLeastS: 300,
		BudgetEUR: 4, SpentEUR: 1, LeftEUR: 3, FloorEUR: 2, CeilingEUR: 18, MonthLeftEUR: 40,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	for _, tc := range []struct {
		kind CaseKind
		at   func(*CaseFacts)
		cure string
	}{
		{CaseBudgetFloor, func(f *CaseFacts) { f.SpentEUR, f.LeftEUR = 4, 0 }, ActExtend},
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

// A room's money reads what a session may spend, the gate reserve held
// out: 0.70 EUR left less the 0.40 reserve is 0.30, and before its first
// session a session's worth is the 0.60 EUR opening, so a room of 1 needs
// 0.30 EUR more. Its money fact names the budget and what one more
// session buys.
func TestARoomsMoneyHoldsTheReserveOut(t *testing.T) {
	f := CaseFacts{BudgetEUR: 3, SpentEUR: 2.3, LeftEUR: 0.7, ReserveEUR: 0.4, FloorEUR: 0.6, Model: "factory/small"}
	if got := RoomFor(f, 1, true).EUR; got != 0.3 {
		t.Errorf("the room's money = %v, want 0.30", got)
	}
	want := CaseMoney{Kind: "budget", BudgetEUR: 3, SpentEUR: 2.3, Buys: "one session on factory/small"}
	if got := tripMoney(CaseBudgetFloor, f); got == nil || *got != want {
		t.Fatalf("money = %+v, want %+v", got, want)
	}
}

// The amount rounds up to the cent: an opening of 0.604 against a session
// ceiling of 0.11 - 0.40 = -0.29 needs 0.894, so 0.90, not 0.89.
func TestARoomsMoneyRoundsUp(t *testing.T) {
	f := CaseFacts{BudgetEUR: 3, SpentEUR: 2.89, LeftEUR: 0.11, ReserveEUR: 0.4, FloorEUR: 0.604}
	if got := RoomFor(f, 1, true).EUR; got != 0.9 {
		t.Fatalf("the room's money = %v, want 0.90", got)
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

// Rungs 0 to 2 take wait_checks once per head while its checks are not
// green, as rerun_checks; a human may press it again. On green checks the
// wait ends at once, so no rung is held to once, and neither is one whose
// cases could not be read.
func TestWaitChecksIsOncePerHeadBelowTheHuman(t *testing.T) {
	stop := CaseFacts{Kind: CaseGateStop, Gate: ActivityWaitChecks, PROpen: true, Head: "h1", Checks: "pending"}
	waited := stop
	waited.WaitedOnHead = true
	green := waited
	green.Checks = "green"
	unread := stop
	unread.Partial = []string{"cases"}
	greenUnread := unread
	greenUnread.Checks = "green"
	a, _ := ActionByName(ActWaitChecks)
	for _, c := range []struct {
		rung   int
		f      CaseFacts
		refuse bool
	}{{0, stop, false}, {1, stop, false}, {2, stop, false}, {0, waited, true}, {1, waited, true}, {2, waited, true}, {3, waited, false},
		{0, green, false}, {1, green, false}, {2, green, false}, {0, unread, true}, {3, unread, false}, {0, greenUnread, false}} {
		got := a.refusal(c.f, c.rung, Resolution{Action: ActWaitChecks})
		if (got != "") != c.refuse {
			t.Errorf("rung %d, checks %s, waited %v, partial %v: refusal %q, want refused %v", c.rung, c.f.Checks, c.f.WaitedOnHead, c.f.Partial, got, c.refuse)
		}
	}
	if got := a.refusal(waited, 2, Resolution{Action: ActWaitChecks}); got != "the checks wait on this head was run again once already, and its checks are not green" {
		t.Errorf("refusal = %q", got)
	}
}

// A re-run of a red head's checks is refused while the record of an earlier
// re-run could not be read: the timeline or the card's cases.
func TestRerunChecksIsRefusedOnAnUnreadRecord(t *testing.T) {
	a, _ := ActionByName(ActRerunChecks)
	red := CaseFacts{Kind: CaseSessionCap, PROpen: true, Head: "h1", Checks: "red"}
	for _, tc := range []struct {
		partial []string
		refuse  bool
	}{{nil, false}, {[]string{"floor"}, false}, {[]string{partialTimeline}, true}, {[]string{"cases"}, true}} {
		f := red
		f.Partial = tc.partial
		if got := a.refusal(f, 0, Resolution{Action: ActRerunChecks}); (got != "") != tc.refuse {
			t.Errorf("partial %v: refusal %q, want refused %v", tc.partial, got, tc.refuse)
		}
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
	if got := a.refusal(ready, 3, Resolution{Action: ActApproveMerge, Room: &RoomAsk{Sessions: 1, Money: true}}); got != "`approve_merge` decides the merge request alone: it draws nothing" {
		t.Errorf("`approve_merge` with a room = %q", got)
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

// The human's buttons on the check-in's seed gate stop: rung 3 adds close
// to the seed's rung 1 list of resume, continue, handover, reverify,
// extend, successor and replan, and its note closes the list; withdraw is
// the factory's own, and no row of the board applies to a gate stop.
func TestHumanButtonsOnTheSeedGateStop(t *testing.T) {
	want := []string{ActResume, ActContinue, ActHandover, ActReverify, ActExtend, ActSuccessor, ActReplan, ActClose, ActNote}
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
		CaseBudgetFloor:   "the card needs a session that merges main into the branch (the card's answers at this stop: `extend`, `successor`, `close` and `note`)",
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

// A row of rung 0 is kept from a case only by a reader it needs: a hold
// that could not be read refuses the starts and closes it guards itself
// (holdRefusal), an unread timeline is nothing a failed dependency's rows
// read, and unread dependencies keep both of them off.
func TestRungZeroGoesOnPastAnUnreadHold(t *testing.T) {
	c := Case{Kind: CaseDependencyFailed}
	for _, tc := range []struct {
		partial []string
		want    string
	}{
		{[]string{partialHold, partialPlanHold}, "no resolver fits"},
		{[]string{partialHold, "timeline"}, "no resolver fits"},
		{[]string{partialHold, "dependencies"}, "facts read partly: dependencies, which retry_dependency and impasse would read, so rung 0 took nothing"},
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

// A decision's riding room counts before the cap check. At a cap of 3
// reached with 3 attempts, a handover with a room of 1 beside it applies
// (no start left, so the cap becomes 4), one with no room does not. By
// hand: 3 attempts against 3 + 1 = 4.
func TestHandoverAtTheCapAppliesWithItsRidingDraw(t *testing.T) {
	atCap := CaseFacts{Attempts: 3, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 1200, SessionLeastS: 300, LeftEUR: 1, FloorEUR: 0.3}
	same := map[string]any{"model": "same"}
	for _, c := range []struct {
		name string
		r    Resolution
		want string
	}{
		{"a room of 1 beside it", Resolution{Args: same, Room: &RoomAsk{Sessions: 1, Money: true}}, ""},
		{"no room", Resolution{Args: same}, "the session cap of 3 is reached (the card's answers at this stop: `extend`, `close` and `note`)"},
	} {
		if got := applies(t, ActHandover, atCap, c.r); got != c.want {
			t.Errorf("%s: handover applies = %q, want %q", c.name, got, c.want)
		}
	}
	if got := applies(t, ActResume, atCap, Resolution{Room: &RoomAsk{Sessions: 1}}); got != "" {
		t.Errorf("resume beside a room of 1 applies = %q, want it to apply", got)
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
	if !strings.Contains(text, "- handover, with a draw of 1 session's worth") {
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

// The factory's code gives starts and time through the extend, never
// money: at a session cap and an agent time cap whose rooms need no money
// every decider is offered it; at the money stop, whose room of 1 adds
// 0.50 EUR, rung 0 is not.
func TestRungZeroIsOfferedAnExtendWithoutMoney(t *testing.T) {
	pool := policy.Draw{Sessions: 5, AgentTime: time.Hour, EUR: 20}
	for _, f := range []CaseFacts{
		{Kind: CaseSessionCap, SessionCap: 3, Attempts: 3, AgentTimeCapS: 7200, PoolLeft: pool, CardDrawLeft: pool},
		{Kind: CaseAgentTimeCap, SessionCap: 5, AgentTimeCapS: 1200, AgentTimeS: 1200, PoolLeft: pool, CardDrawLeft: pool},
	} {
		for rung := range 4 {
			if got := offeredNames(f, rung); !slices.Contains(got, ActExtend) {
				t.Errorf("%s: rung %d is offered %v, want the extend", f.Kind, rung, got)
			}
		}
	}
	floor := CaseFacts{Kind: CaseBudgetFloor, SessionCap: 3, AgentTimeCapS: 7200, FloorEUR: 0.5, CeilingEUR: 10, MonthLeftEUR: 40, PoolLeft: pool, CardDrawLeft: pool}
	if got := offeredNames(floor, 0); slices.Contains(got, ActExtend) {
		t.Errorf("rung 0 at the money stop is offered %v, want no extend", got)
	}
	for _, rung := range []int{1, 2, 3} {
		if got := offeredNames(floor, rung); !slices.Contains(got, ActExtend) {
			t.Errorf("rung %d at the money stop is offered %v, want the extend", rung, got)
		}
	}
}

// Room for the card lifts no spending limit of the factory or the
// project, and a case that holds none of the card's money takes none: the
// extend is on offer at neither, and it is at the budget floor.
func TestRoomLiftsNoCap(t *testing.T) {
	room := CaseFacts{Attempts: 1, SessionCap: 3, BudgetEUR: 2, SpentEUR: 1, LeftEUR: 1, CeilingEUR: 10, MonthLeftEUR: 40,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	extend, _ := ActionByName(ActExtend)
	press := func(f CaseFacts) string { return extend.refusal(f, 3, extendBy(map[string]any{"sessions": 1})) }
	for _, kind := range []CaseKind{CaseDayCap, CaseMonthCap, CaseProjectCap} {
		f := room
		f.Kind = kind
		if got := press(f); got != capLiftsNot {
			t.Errorf("an extend at %s = %q, want %q", kind, got, capLiftsNot)
		}
	}
	for _, kind := range []CaseKind{CaseMergeApproval, CasePlanQuestion, CasePlanConfirmation, CasePlanCriteria, CaseHumanCard, CaseJudgmentBudget, CaseRedMain, CaseRollHeld, CaseConfigRefused} {
		f := room
		f.Kind = kind
		if press(f) == "" {
			t.Errorf("an extend on %s is on offer", kind)
		}
	}
	f := room
	f.Kind = CaseBudgetFloor
	if why := press(f); why != "" {
		t.Errorf("an extend at the budget floor is refused: %s", why)
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

// A room on an agent's question that waits at the human is weighed at the
// cap the question waits at, by the chat's rung 2 as by the human: both
// doors give one verdict on the same facts. While the question sits on a
// rung, the rung's room is refused, and the refusal names the room only
// where the room lifts the question's cap: a start at the session cap,
// time at the agent time cap, money at the budget floor.
func TestADrawOnAWaitingQuestionIsWeighedAtItsCap(t *testing.T) {
	room := CaseFacts{Kind: CaseAgentEscalate, AtHuman: true, Attempts: 1, SessionCap: 3, AgentTimeS: 300, AgentTimeCapS: 7200, SessionLeastS: 300,
		BudgetEUR: 2, SpentEUR: 1, LeftEUR: 1, FloorEUR: 0.3, CeilingEUR: 10, MonthLeftEUR: 40,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	sessionCap := func(f *CaseFacts) { f.Attempts = f.SessionCap }
	timeCap := func(f *CaseFacts) { f.AgentTimeS = f.AgentTimeCapS }
	floor := func(f *CaseFacts) { f.SpentEUR, f.LeftEUR = 1.9, 0.1 }
	dayCap := func(f *CaseFacts) {
		f.CapWait, _, _ = strings.Cut(CapWaitReason(policy.RefusedDayCap, "the gateway refused the spend", "state", time.Now().Add(time.Hour)), "\n")
	}
	extend := extendBy(map[string]any{"sessions": 1})
	timeOnly := extendBy(map[string]any{"sessions": 1, "money": false})
	for _, c := range []struct {
		name string
		at   func(*CaseFacts)
		want string
	}{
		{"at the session cap", sessionCap, ""},
		{"at the agent time cap", timeCap, ""},
		{"at the budget floor", floor, ""},
		{"at the day cap", dayCap, capLiftsNot},
	} {
		f := room
		c.at(&f)
		a, _ := ActionByName(ActExtend)
		human, chat := a.refusal(f, 3, extend), a.refusal(f, 2, extend)
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
		{"a room at the session cap", sessionCap, extend, hand},
		{"a room at the agent time cap", timeCap, extend, hand},
		{"a room at the budget floor", floor, extend, hand},
		{"a room without money at the budget floor", floor, timeOnly, answer},
		{"a room at no cap", func(*CaseFacts) {}, extend, answer},
		{"a room at the day cap", dayCap, extend, answer},
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

// A refusal lists the answers of the stop its sentence names. By hand: a
// card that waits at its budget floor (0.10 EUR left, an opening needs 0.30
// EUR) has also used its 20 minutes of agent time. The resume is refused at
// the agent time, which an extend buys and money does not, so the list holds
// the extend and no fund.
func TestARefusalListsTheAnswersOfTheStopItNames(t *testing.T) {
	f := CaseFacts{Kind: CaseBudgetFloor, Attempts: 1, SessionCap: 3, AgentTimeS: 1200, AgentTimeCapS: 1200, SessionLeastS: 300,
		BudgetEUR: 2, SpentEUR: 1.9, LeftEUR: 0.1, FloorEUR: 0.3, CeilingEUR: 10, MonthLeftEUR: 40,
		PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}}
	resume, _ := ActionByName(ActResume)
	want := "the agent time cap of 20 minutes is reached (the card's answers at this stop: `extend`, `close` and `note`)"
	if got := resume.refusal(f, 3, Resolution{Action: ActResume}); got != want {
		t.Errorf("the resume's refusal = %q, want %q", got, want)
	}
}

// A refusal on a card's facts names the cap's turn and the last draw's
// time, which it keeps beside their words: a winter turn at 00:00 UTC as a
// stamp, a summer draw at 12:30 CEST as 10:30 UTC.
func TestARefusalSaysItsInstants(t *testing.T) {
	turn, drew := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	line, _, _ := strings.Cut(CapWaitReason(policy.RefusedDayCap, "the gateway", "the day's cap", turn), "\n")
	f := CaseFacts{Kind: CaseSessionCap, CapWait: line, CapTurn: turn, LastDrawAt: &drew}
	if want := (Moments{{At: turn, Layout: LayoutRFC3339}, {At: drew.UTC(), Layout: LayoutClock}}); !reflect.DeepEqual(f.Said(), want) {
		t.Errorf("Said() = %+v, want %+v", f.Said(), want)
	}
	if !strings.Contains(line, "turns at 2026-11-12T00:00:00Z.") || !strings.Contains(boughtNothingText(f), "the last draw at 10:30 UTC bought nothing") {
		t.Errorf("the words %q and %q, want the turn and the draw in UTC", line, boughtNothingText(f))
	}
	if said := (CaseFacts{}).Said(); said != nil {
		t.Errorf("facts with no wait and no draw say %+v", said)
	}
}

// A cap that binds before a session can start refuses with the action that
// fits beside the state: the wait for the cap's turn, or its lift, which the
// cap's own line names with the time.
func TestACapWaitRefusalNamesTheWaitAndTheLift(t *testing.T) {
	line, _, _ := strings.Cut(CapWaitReason(policy.RefusedDayCap, "the gateway", "the day's cap", time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)), "\n")
	f := CaseFacts{Kind: CaseSessionCap, FloorEUR: 1, LeftEUR: 0.2, SessionCap: 3, Attempts: 1, AgentTimeS: 100, AgentTimeCapS: 1200, SessionLeastS: 300, CapWait: line}
	want := "the day cap is reached: the card resumes on its own once a session fits under the cap, " +
		"at the latest when the UTC day turns at 2026-11-12T00:00:00Z. Reset the day's count in the console's status panel, or raise the day cap in policy.yaml, to resume it sooner, or close the card."
	if got := sessionRoom(f); got != want || !strings.Contains(line, "turns at 2026-11-12T00:00:00Z") || !strings.Contains(line, "Reset the day's count in the console's status panel, or raise the day cap in policy.yaml") {
		t.Errorf("the refusal = %q, want %q, whose line names the turn and the lift", got, want)
	}
}
