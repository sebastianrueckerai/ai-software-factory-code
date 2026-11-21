package factoryd

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func roomFacts(sessionCap int, capMin float64, leftEUR float64) CaseFacts {
	return CaseFacts{
		Attempts: 3, AgentTimeS: 42 * 60, SpentEUR: 3.60, FloorEUR: 0.50,
		BudgetEUR: 6.00, CeilingEUR: 8.00, ReserveEUR: 0.30,
		SessionCap: sessionCap, AgentTimeCapS: capMin * 60, LeftEUR: leftEUR,
	}
}

func TestRoomForTheSpecRows(t *testing.T) {
	m := time.Minute
	for _, c := range []struct {
		name      string
		f         CaseFacts
		n         int
		now, strt int
		agent     time.Duration
		eur, lim  float64
	}{
		{"ahead of time", roomFacts(4, 60, 2.40), 2, 1, 2, 24 * m, 1.50, 0},
		{"session-cap stop", roomFacts(3, 60, 2.40), 1, 0, 1, 0, 0, 0},
		{"agent-time stop", roomFacts(4, 44, 2.40), 1, 0, 0, 12 * m, 0, 0},
		{"money stop", roomFacts(5, 72, 0.70), 1, 0, 0, 0, 0.80, 0},
	} {
		r := RoomFor(c.f, c.n, true)
		if r.Now != c.now || r.Starts != c.strt || r.AgentTime != c.agent || math.Abs(r.EUR-c.eur) > 1e-9 || math.Abs(r.Limit-c.lim) > 1e-9 {
			t.Errorf("%s: got now %d starts %d time %v eur %v limit %v", c.name, r.Now, r.Starts, r.AgentTime, r.EUR, r.Limit)
		}
	}
}

func TestRoomForRaisesTheLimitFirst(t *testing.T) {
	f := roomFacts(4, 60, 2.40)
	f.CeilingEUR = 7.00
	r := RoomFor(f, 2, true)
	if math.Abs(r.EUR-1.50) > 1e-9 || math.Abs(r.Limit-0.50) > 1e-9 {
		t.Errorf("got eur %v limit %v", r.EUR, r.Limit)
	}
}

func TestRoomForMoneyLeftOut(t *testing.T) {
	r := RoomFor(roomFacts(5, 72, 0.70), 1, false)
	if r.Now != 2 || r.Starts != 1 || r.AgentTime != 12*time.Minute || math.Abs(r.EUR) > 1e-9 || r.Limit != 0 || math.Abs(r.MoneyLeft-0.40) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestSessionEstimateBeforeTheFirstSession(t *testing.T) {
	got := SessionEstimate(CaseFacts{AgentTimeCapS: 60 * 60, FloorEUR: 0.80})
	if want := (Estimate{15 * time.Minute, 0.80, 0}); got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestSessionEstimateFloors(t *testing.T) {
	got := SessionEstimate(CaseFacts{Attempts: 3, AgentTimeS: 6 * 60, SpentEUR: 0.30, FloorEUR: 0.50})
	if want := (Estimate{5 * time.Minute, 0.50, 3}); got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestRoomForAPriceOfZero(t *testing.T) {
	r := RoomFor(CaseFacts{SessionCap: 1}, 1, true)
	if r.Est != (Estimate{5 * time.Minute, 0, 0}) || r.Now != 0 || r.Starts != 0 || r.AgentTime != 5*time.Minute || math.Abs(r.EUR) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestRoomForMoneySpentIntoTheReserve(t *testing.T) {
	r := RoomFor(roomFacts(5, 72, 0.10), 1, true)
	if math.Abs(r.MoneyLeft+0.20) > 1e-9 || r.Now != 0 || math.Abs(r.EUR-1.40) > 1e-9 {
		t.Errorf("got %+v", r)
	}
}

func TestSessionRoomWithoutMoney(t *testing.T) {
	now, starts, agent := SessionRoom(0, 18*time.Minute, 14*time.Minute, math.MaxInt, 1)
	if now != 0 || starts != 1 || agent != 0 {
		t.Errorf("got %d %d %v", now, starts, agent)
	}
}

func TestRoomForAPriceOfZeroInTheReserve(t *testing.T) {
	r := RoomFor(CaseFacts{SessionCap: 1, ReserveEUR: 0.30}, 1, true)
	if math.Abs(r.EUR) > 1e-9 {
		t.Errorf("got eur %v", r.EUR)
	}
}

func TestSessionRoomRoundsMinutesUp(t *testing.T) {
	now, starts, agent := SessionRoom(1, 10*time.Minute, 14*time.Minute+30*time.Second, math.MaxInt, 1)
	if now != 0 || starts != 0 || agent != 5*time.Minute {
		t.Errorf("got %d %d %v", now, starts, agent)
	}
	r := RoomFor(CaseFacts{Attempts: 2, AgentTimeS: 29 * 60, SpentEUR: 2, FloorEUR: 0.5, SessionCap: 3, AgentTimeCapS: 39 * 60, LeftEUR: 5, ReserveEUR: 0}, 1, false)
	if r.Est.Time != 14*time.Minute+30*time.Second || r.Now != 0 || r.AgentTime != 5*time.Minute {
		t.Errorf("got %+v", r)
	}
}

// aheadOfTime is the ahead-of-time row on a live card, the deciders' bounds
// wide and the project's month holding 41.20 EUR: T = 42 min / 3 = 14 min,
// P = 3.60 / 3 = 1.20 EUR, 1 start, 18 minutes and 2.10 EUR left.
func aheadOfTime() CaseFacts {
	f := roomFacts(4, 60, 2.40)
	f.MonthLeftEUR = 41.20
	f.PoolLeft = policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}
	f.CardDrawLeft = policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 10}
	return f
}

// moneyStop is the money-stop row at its case: 2 starts, 30 minutes and
// 0.40 EUR left, under the 0.50 EUR an opening needs.
func moneyStop() CaseFacts {
	f := aheadOfTime()
	f.Kind, f.SessionCap, f.AgentTimeCapS, f.LeftEUR = CaseBudgetFloor, 5, 72*60, 0.70
	return f
}

func extendBy(args map[string]any) Resolution { return Resolution{Action: ActExtend, Args: args} }

func refusalOn(t *testing.T, f CaseFacts, rung int, r Resolution) string {
	t.Helper()
	a, ok := ActionByName(r.Action)
	if !ok {
		t.Fatalf("no action %s", r.Action)
	}
	return a.refusal(f, rung, r)
}

// roomCardRig is a runner whose card t1 holds the ahead-of-time row: budget
// 6.00 EUR under the limit ceiling.
func roomCardRig(t *testing.T, ceiling float64) (*Runner, *fakeStore, Task) {
	t.Helper()
	store := newFakeStore()
	r := &Runner{store: store, cfg: testRunnerConfig()}
	card := Task{ID: "t1", Kind: "agent", Repository: "demo-app/demo-app", IssueNumber: 12, Status: "running", Size: "medium",
		BudgetEUR: 6, SpentEUR: 3.6, CeilingEUR: ceiling, Usage: TaskUsage{Attempts: 3, AgentTime: 42 * time.Minute}}
	store.tasks["t1"] = card
	return r, store, card
}

// An extend of 2 sessions' worth on the ahead-of-time row by a human:
// target 1 + 2 = 3, so 3 - 1 = 2 starts, 3 x 14 - 18 = 24 minutes and
// 3 x 1.20 - 2.10 = 1.50 EUR. 6.00 + 1.50 stays under the 8.00 EUR limit,
// so no ceiling row. The rows record the count, the money and the
// estimate.
func TestExtendTakesSessionsAndMoney(t *testing.T) {
	f := aheadOfTime()
	res := extendBy(map[string]any{"sessions": 2})
	if why := refusalOn(t, f, 3, res); why != "" {
		t.Fatalf("the human's extend of 2 is refused: %s", why)
	}
	r, store, card := roomCardRig(t, 8)
	d := r.caseDraw(card, f, Case{}, ActInput{TaskID: "t1", By: "sebastian", Rung: 3, Res: res}, ViaConsole)
	if d == nil || d.Raise != 0 || d.Extend != (policy.Draw{Sessions: 2, AgentTime: 24 * time.Minute}) || Cents(d.FundEUR) != 150 {
		t.Fatalf("the draw = %+v, want 2 starts, 24 minutes and 1.50 EUR, no raise", d)
	}
	if _, err := store.DrawCard(context.Background(), "t1", *d); err != nil {
		t.Fatal(err)
	}
	got := store.tasks["t1"]
	if got.Usage.RaisedSessions != 2 || got.Usage.RaisedTime != 1440*time.Second || Cents(got.BudgetEUR) != 750 || got.CeilingEUR != 8 {
		t.Fatalf("the card = usage %+v, budget %v, ceiling %v; want 2 sessions and 1440 s raised and the budget at 7.50", got.Usage, got.BudgetEUR, got.CeilingEUR)
	}
	if len(store.events) != 2 || store.events[0].Kind != EventCardExtended || store.events[1].Kind != EventCardFunded || store.events[1].Detail["ceiling_eur"] != nil {
		t.Fatalf("rows = %+v, want the extend's then the fund's, no ceiling row", store.events)
	}
	ext, fund := store.events[0].Detail, store.events[1].Detail
	if ext["sessions"] != 2 || ext["agent_time_s"] != int64(1440) || fund["amount_eur"] != d.FundEUR {
		t.Errorf("extend row %v, fund row %v; want 2 sessions and 1440 s, then 1.50 EUR", ext, fund)
	}
	for _, row := range []map[string]any{ext, fund} {
		if row["room_sessions"] != 2 || row["room_money"] != true || row["session_time_s"] != int64(840) || row["session_eur"] != 1.2 || row["from_sessions"] != 3 {
			t.Errorf("row %v, want the room of 2 with money and the estimate of 840 s and 1.20 EUR from 3 sessions", row)
		}
	}
}

// The same extend under a limit of 7.00 EUR: 6.00 + 1.50 passes it by 0.50
// EUR, so a human's press raises the limit to 7.50 first, then the caps,
// then the budget to 7.50, all in one draw.
func TestExtendRaisesTheLimitFirst(t *testing.T) {
	f := aheadOfTime()
	f.CeilingEUR = 7
	res := extendBy(map[string]any{"sessions": 2, "money": true})
	if why := refusalOn(t, f, 3, res); why != "" {
		t.Fatalf("the human's extend of 2 is refused: %s", why)
	}
	r, store, card := roomCardRig(t, 7)
	d := r.caseDraw(card, f, Case{}, ActInput{TaskID: "t1", By: "sebastian", Rung: 3, Res: res}, ViaConsole)
	if d == nil || Cents(d.Raise) != 50 || Cents(d.FundEUR) != 150 {
		t.Fatalf("the draw = %+v, want a raise of 0.50 and a fund of 1.50", d)
	}
	if _, err := store.DrawCard(context.Background(), "t1", *d); err != nil {
		t.Fatal(err)
	}
	if got := store.tasks["t1"]; Cents(got.CeilingEUR) != 750 || Cents(got.BudgetEUR) != 750 {
		t.Fatalf("ceiling %v, budget %v; want both at 7.50", got.CeilingEUR, got.BudgetEUR)
	}
	var kinds []string
	for _, e := range store.events {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []string{EventCardFunded, EventCardExtended, EventCardFunded}) || store.events[0].Detail["ceiling_eur"] == nil ||
		store.events[0].Detail["pool"] != nil || store.events[2].Detail["amount_eur"] != d.FundEUR {
		t.Fatalf("rows = %+v, want the ceiling row first, then the extend, then the fund of 1.50", store.events)
	}
}

// The check-in gives money but raises no limit: its extend of 2 under the
// 7.00 EUR limit is refused, and 1 session's worth fits under it (target
// 2, 2.40 - 2.10 = 0.30 EUR, a budget of 6.30).
func TestTheCheckInRaisesNoLimit(t *testing.T) {
	f := aheadOfTime()
	f.CeilingEUR = 7
	if got, want := refusalOn(t, f, 1, extendBy(map[string]any{"sessions": 2})), "only the factory agent or a human raises the spend limit: 1 session's worth fits under it"; got != want {
		t.Errorf("the check-in's extend of 2 = %q, want %q", got, want)
	}
	if got := refusalOn(t, f, 1, extendBy(map[string]any{"sessions": 1})); got != "" {
		t.Errorf("the check-in's extend of 1 = %q, want it taken", got)
	}
}

// The factory's code gives starts and time, never money: at the money stop
// the room of 1 holds 0.80 EUR, which only the check-in, the factory agent
// and the human draw. At the session cap the room holds no money, so the
// same extend with its money passes.
func TestTheFactoryGivesNoMoney(t *testing.T) {
	if got, want := refusalOn(t, moneyStop(), 0, extendBy(map[string]any{"sessions": 1, "money": true})), "the factory may not draw money: the check-in, the factory agent and the human may"; got != want {
		t.Errorf("the factory's extend at the money stop = %q, want %q", got, want)
	}
	capStop := aheadOfTime()
	capStop.Kind, capStop.SessionCap = CaseSessionCap, 3
	if got := refusalOn(t, capStop, 0, extendBy(map[string]any{"sessions": 1, "money": true})); got != "" {
		t.Errorf("the factory's extend at the session cap = %q, want it taken", got)
	}
}

// A room's money is bounded by the project's month for every decider: 1.50
// EUR past 1.20 EUR left is refused, and 1 session's worth (0.30 EUR) fits.
// An overspent month shows as nothing left.
func TestARoomPastTheMonthIsRefused(t *testing.T) {
	f := aheadOfTime()
	f.MonthLeftEUR = 1.20
	for _, rung := range []int{1, 2, 3} {
		if got, want := refusalOn(t, f, rung, extendBy(map[string]any{"sessions": 2})),
			"the project has 1.20 EUR left this month, which pays for 1 session's worth: give that, or raise the month cap by a commit (`budget.month_eur`)"; got != want {
			t.Errorf("rung %d: %q, want %q", rung, got, want)
		}
	}
	f.MonthLeftEUR = -3
	if got, want := refusalOn(t, f, 3, extendBy(map[string]any{"sessions": 1})),
		"the project has 0.00 EUR left this month, which pays for no session's worth: raise the month cap by a commit (`budget.month_eur`)"; got != want {
		t.Errorf("an overspent month: %q, want %q", got, want)
	}
	if got := refusalOn(t, f, 3, extendBy(map[string]any{"sessions": 1, "money": false})); got != "" {
		t.Errorf("a room without money in an overspent month = %q, want it taken", got)
	}
}

// Time and starts alone do not let a card go on that cannot pay to open a
// session: 0.40 EUR left under a 0.50 EUR opening.
func TestMoneyLeftOutOnAnUnfundedCard(t *testing.T) {
	if got, want := refusalOn(t, moneyStop(), 3, extendBy(map[string]any{"sessions": 1, "money": false})),
		"this card lacks the money to open a session: give the room with its money"; got != want {
		t.Errorf("the money stop without money = %q, want %q", got, want)
	}
}

// A count is a whole number from 1 to 5, from a form, the chat or a rung,
// for an extend and for a room beside another action.
func TestARoomCountsOneToFive(t *testing.T) {
	const want = "a room counts 1 to 5 sessions' worth"
	for _, n := range []any{0, 6, 1.5, -1, "2", nil} {
		if got := refusalOn(t, aheadOfTime(), 3, extendBy(map[string]any{"sessions": n})); got != want {
			t.Errorf("a count of %v = %q, want %q", n, got, want)
		}
	}
	capStop := aheadOfTime()
	capStop.Kind, capStop.SessionCap = CaseSessionCap, 3
	for _, n := range []int{0, 6} {
		res := Resolution{Action: ActHandover, Args: map[string]any{"model": "same"}, Room: &RoomAsk{Sessions: n, Money: true}}
		if got := refusalOn(t, capStop, 1, res); got != want {
			t.Errorf("a handover with a room of %d = %q, want %q", n, got, want)
		}
	}
	if got := refusalOn(t, aheadOfTime(), 3, extendBy(map[string]any{"sessions": 5.0})); got != "" {
		t.Errorf("a count of 5 = %q, want it taken", got)
	}
	if got := refusalOn(t, aheadOfTime(), 3, extendBy(map[string]any{"sessions": 1, "money": "no"})); got != "a room's money is true or false" {
		t.Errorf("money of %q = %q, want the refusal of its kind", "no", got)
	}
}

// The factory agent's room is bounded by the pool, 1 session, 1 hour and 5
// EUR left: a room of 3 takes 3 starts, 2 of them 2; a room of 1 takes 1
// start, 28 - 18 = 10 minutes and 0.30 EUR, which fits. The refusal names
// that count with what it is worth: 14 minutes and 1.20 EUR.
func TestAPoolRefusalNamesTheLargestCountThatFits(t *testing.T) {
	f := aheadOfTime()
	f.PoolLeft = policy.Draw{Sessions: 1, AgentTime: time.Hour, EUR: 5}
	if got, want := refusalOn(t, f, 2, extendBy(map[string]any{"sessions": 3})),
		"the escalation pool leaves room for 1 more session's worth this month, about 14 minutes and 1.20 EUR: ask for that, or hand it up (`hand_up`) for the human to give more"; got != want {
		t.Errorf("the factory agent's room of 3 = %q, want %q", got, want)
	}
	f.PoolLeft.Sessions = 0
	if got, want := refusalOn(t, f, 2, extendBy(map[string]any{"sessions": 1})),
		"the escalation pool leaves room for no more session's worth this month: hand it up (`hand_up`) for the human to give more"; got != want {
		t.Errorf("an empty pool = %q, want %q", got, want)
	}
	f = aheadOfTime()
	f.CeilingEUR, f.CardDrawLeft = 18, policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}
	if got, want := refusalOn(t, f, 1, extendBy(map[string]any{"sessions": 3})),
		"the card's draw bound leaves room for 2 more sessions' worth, about 28 minutes and 2.40 EUR: ask for that, or hand it up (`hand_up`) for the factory agent or the human to give more"; got != want {
		t.Errorf("the check-in's room of 3 = %q, want %q", got, want)
	}
	// without its money a room names no EUR: a room of 3 takes 3 starts, a
	// room of 1 the 1 start and 10 minutes the pool holds
	f = aheadOfTime()
	f.PoolLeft = policy.Draw{Sessions: 1, AgentTime: time.Hour, EUR: 5}
	if got, want := refusalOn(t, f, 2, extendBy(map[string]any{"sessions": 3, "money": false})),
		"the escalation pool leaves room for 1 more session's worth this month, about 14 minutes: ask for that, or hand it up (`hand_up`) for the human to give more"; got != want {
		t.Errorf("the factory agent's room of 3 without money = %q, want %q", got, want)
	}
}

// At an agent time overrun, 42 minutes spent of a 40 minute cap, the time
// line shows none left and the real total after the press: a room of 1
// needs 14 - (-2) = 16 minutes, then 14 minutes; the 2.10 EUR left hold
// the 1.20 EUR, and the start left takes the session.
func TestRoomLinesAtATimeOverrunSayNoneLeft(t *testing.T) {
	f := aheadOfTime()
	f.AgentTimeCapS = 40 * 60
	want := []string{
		"About 14 min and 1.20 EUR per session, from its 3 sessions so far",
		"Adds 16 min: none left, then 14 min",
		"Enough: 2.10 EUR left",
		"The project has 41.20 EUR left this month.",
	}
	if got := RoomLines(f, RoomFor(f, 1, true)); !slices.Equal(got, want) {
		t.Errorf("RoomLines = %q, want %q", got, want)
	}
}

// A draw that Act's facts let through meets the store's own weighing: with
// 29 of the pool's 30 sessions drawn after the facts were read, the room of
// 3 on the ahead-of-time row (3 starts) is refused naming the count that
// fits, 1 session's worth with its 1 start, as Act's refusal names it.
func TestARacingDrawIsRefusedInSessionsWorth(t *testing.T) {
	g := newFactsRig(t)
	ctx := context.Background()
	card := g.store.tasks["t1"]
	d := g.r.caseDraw(card, aheadOfTime(), Case{ID: 7}, ActInput{Rung: 2, Res: extendBy(map[string]any{"sessions": 3})}, ViaChat)
	if d == nil {
		t.Fatal("no draw for a room of 3")
	}
	d.Pool = policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}
	if _, err := g.store.Record(ctx, Event{Repository: card.Repository, TaskID: "t2", Kind: EventCardExtended, Actor: ChatUser, Via: ViaChat,
		Detail: map[string]any{"pool": true, "rung": 2, "sessions": 29}}); err != nil {
		t.Fatal(err)
	}
	_, err := g.store.DrawCard(ctx, "t1", *d)
	if want := "the escalation pool leaves room for 1 more session's worth this month, about 14 minutes and 1.20 EUR: ask for that, or hand it up (`hand_up`) for the human to give more"; err == nil || err.Error() != want {
		t.Errorf("the racing draw = %v, want %q", err, want)
	}
}

// The readout of a room of 2 on the ahead-of-time row, word for word, and
// with the limit at 7.00 EUR; the money stop with its money left out:
// target 2 + 1 = 3, 3 - 2 = 1 start and 42 - 30 = 12 minutes; and a card
// before its first session: a quarter of 60 minutes and the 0.80 EUR
// floor, room now for min(3, 60/15, 5.00/0.80) = 3, so the press adds 1
// start alone.
func TestRoomLinesSayWhatThePressGives(t *testing.T) {
	f := aheadOfTime()
	want := []string{
		"About 14 min and 1.20 EUR per session, from its 3 sessions so far",
		"Adds 24 min: 18 min left, then 42 min",
		"Adds 1.50 EUR: 2.10 EUR left, then 3.60 EUR",
		"It may also start 2 more sessions.",
		"The project has 41.20 EUR left this month.",
	}
	if got := RoomLines(f, RoomFor(f, 2, true)); !slices.Equal(got, want) {
		t.Errorf("RoomLines = %q, want %q", got, want)
	}
	f.CeilingEUR = 7
	limited := slices.Insert(slices.Clone(want), 3, "The spend limit rises from 7.00 to 7.50 EUR first.")
	if got := RoomLines(f, RoomFor(f, 2, true)); !slices.Equal(got, limited) {
		t.Errorf("RoomLines under the 7.00 limit = %q, want %q", got, limited)
	}
	f = moneyStop()
	left := []string{
		"About 14 min and 1.20 EUR per session, from its 3 sessions so far",
		"Adds 12 min: 30 min left, then 42 min",
		"Adds no money: 0.40 EUR left",
		"It may also start 1 more session.",
		"The project has 41.20 EUR left this month.",
	}
	if got := RoomLines(f, RoomFor(f, 1, false)); !slices.Equal(got, left) {
		t.Errorf("RoomLines without money = %q, want %q", got, left)
	}
	fresh := CaseFacts{SessionCap: 3, AgentTimeCapS: 60 * 60, FloorEUR: 0.80, BudgetEUR: 5, LeftEUR: 5, CeilingEUR: 18, MonthLeftEUR: -2}
	first := []string{
		"About 15 min and 0.80 EUR per session, the policy's estimate before its first session",
		"Enough: 60 min left",
		"Enough: 5.00 EUR left",
		"It may also start 1 more session.",
		"The project has 0.00 EUR left this month.",
	}
	if got := RoomLines(fresh, RoomFor(fresh, 1, true)); !slices.Equal(got, first) {
		t.Errorf("RoomLines before the first session = %q, want %q", got, first)
	}
}

// The limit rises only by what the room's money passes it: a budget of
// 10.00 above a limit of 5.00, as a budget moved by hand leaves it, with
// 6.40 left (6.10 past the reserve), needs no raise for a room of 1, whose
// 2 x 1.20 EUR the money holds; a room of 5 needs 6 x 1.20 - 6.10 = 1.10
// EUR, which raises the limit to 10.00 + 1.10, by 6.10.
func TestRoomForRaisesNoLimitWithoutMoney(t *testing.T) {
	f := aheadOfTime()
	f.BudgetEUR, f.LeftEUR, f.CeilingEUR = 10, 6.4, 5
	if r := RoomFor(f, 1, true); r.EUR != 0 || r.Limit != 0 {
		t.Errorf("a room of 1 = %v EUR, limit %v; want neither", r.EUR, r.Limit)
	}
	if r := RoomFor(f, 5, true); Cents(r.EUR) != 110 || Cents(r.Limit) != 610 {
		t.Errorf("a room of 5 = %v EUR, limit %v; want 1.10 and 6.10", r.EUR, r.Limit)
	}
}

// The check-in may give a room without its money where its money would
// pass the spend limit: under a limit of 6.00 at a budget of 6.00, a room
// of 1 on the ahead-of-time row adds 0.30 EUR and so needs the limit
// raised, which the check-in may not, but 1 start and 10 minutes without
// money it may. The extend stays on offer, and the statement says to send
// it with money false and why.
func TestAnExtendIsOfferedWithoutItsMoney(t *testing.T) {
	f := aheadOfTime()
	f.CeilingEUR = 6
	if got := offeredNames(f, 1); !slices.Contains(got, ActExtend) {
		t.Errorf("the check-in is offered %v, want the extend", got)
	}
	got := (&Runner{}).rungStatement(Case{Problem: "P"}, f, "D", nil, 1)
	if want := "- extend, with money false: with money true, only the factory agent or a human raises the spend limit: no session's worth fits under it\n"; !strings.Contains(got, want) {
		t.Errorf("the statement lacks %q:\n%s", want, got)
	}
}

// A handover at the cap rides a room of 1 without money where the money
// would need the limit raised: 1.00 EUR left less the 0.30 reserve opens a
// session (0.70 over the 0.50 floor) but lacks 0.50 of a 1.20 EUR
// session's worth, which passes the 6.00 limit, so the check-in's handover
// is on offer with the start alone.
func TestAHandoverAtTheCapTakesARoomWithoutMoney(t *testing.T) {
	f := aheadOfTime()
	f.Kind, f.SessionCap, f.LeftEUR, f.CeilingEUR = CaseSessionCap, 3, 1, 6
	if got := offeredNames(f, 1); !slices.Contains(got, ActHandover) {
		t.Errorf("the check-in is offered %v at the cap, want the handover", got)
	}
	if got := (&Runner{}).rungStatement(Case{Kind: CaseSessionCap, Problem: "P"}, f, "D", nil, 1); !strings.Contains(got, "- handover, with a draw of 1 session's worth and money false: the cap is reached\n") {
		t.Errorf("the statement does not name the handover's draw:\n%s", got)
	}
}
