package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// aheadRig is roomRig with running #32 ahead of time, as give_room's test
// holds it: 3 sessions in 42 minutes for 3.60 EUR of 6.00, with 1 session
// and 40 minutes raised before, under its own spend limit of 10.00. The
// console reads the card's room facts from the real runner on the card as
// the store holds it. By hand: a session's worth is 14 minutes and 1.20
// EUR from its 3 sessions, starts left 4 - 3 = 1, time left 60 - 42 = 18
// minutes, money left 6.00 - 3.60 - 0.40 = 2.00 EUR, so the room now is
// min(1, 1, 1) = 1; the project, which sets no month cap of its own, has
// the factory's 50.00 EUR month, nothing spent.
func aheadRig(t *testing.T) (*rig, *realAct) {
	t.Helper()
	r, ra, _ := roomRig(t, policy.AutonomyManual, roomCard{task: factoryd.Task{ID: "t-run", Status: "running", IssueNumber: 32, BudgetEUR: 6, SpentEUR: 3.6,
		CeilingEUR: 10, Usage: factoryd.TaskUsage{RaisedSessions: 1, RaisedTime: 40 * time.Minute}}, attempts: 3, agentTime: 42 * time.Minute})
	r.c.cfg.RoomFacts = storedRoomFacts(ra)
	return r, ra
}

// storedRoomFacts are the real runner's room facts of a card as the store
// holds it, as the console reads its cards from the store.
func storedRoomFacts(ra *realAct) func(context.Context, factoryd.Task) (factoryd.CaseFacts, error) {
	return func(ctx context.Context, t factoryd.Task) (factoryd.CaseFacts, error) {
		k, _, err := ra.pg.Task(ctx, t.ID)
		if err != nil {
			return factoryd.CaseFacts{}, err
		}
		return ra.runner.RoomFacts(ctx, k)
	}
}

// readout is the room readout of card id for the count and the tick the
// press form posts, and the guard it shows by name.
func readout(t *testing.T, r *rig, ck *http.Cookie, id, sessions, money string) (string, url.Values) {
	t.Helper()
	q := url.Values{"extend.sessions": {sessions}}
	if money != "" {
		q.Set("extend.money", money)
	}
	rec := r.do(httptest.NewRequest(http.MethodGet, "/tasks/"+id+"/room?"+q.Encode(), nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the readout of %s at %s = %d: %s", id, sessions, rec.Code, rec.Body)
	}
	guard := url.Values{}
	for _, m := range guardInput.FindAllStringSubmatch(rec.Body.String(), -1) {
		guard.Set(m[1], m[2])
	}
	return rec.Body.String(), guard
}

var guardInput = regexp.MustCompile(`<input type="hidden" name="(extend\.shown_[a-z_]+)" value="([^"]*)">`)

// The readout follows the count and the tick, beside the room of 1 the
// card has. 2 sessions' worth aim at a
// room of 3: 3 - 1 = 2 starts, 3 x 14 - 18 = 24 minutes and 3 x 1.20 -
// 2.00 = 1.60 EUR, the budget to 7.60 under the 10.00 limit. 1 aims at
// 2: 1 start, 2 x 14 - 18 = 10 minutes and 2 x 1.20 - 2.00 = 0.40 EUR.
// Without the money the line says the room adds none.
func TestTheReadoutFollowsTheCount(t *testing.T) {
	r, _ := aheadRig(t)
	ck, _ := r.loggedIn(t)
	for _, tc := range []struct {
		sessions, money string
		lines           []string
		guard           url.Values
	}{
		{"2", "yes", []string{"About 14 min and 1.20 EUR per session, from its 3 sessions so far", "Adds 24 min: 18 min left, then 42 min",
			"Adds 1.60 EUR: 2.00 EUR left, then 3.60 EUR", "It may also start 2 more sessions.", "The project has 50.00 EUR left this month."},
			url.Values{"extend.shown_now": {"1"}, "extend.shown_starts": {"2"}, "extend.shown_agent_time_s": {"1440"}, "extend.shown_eur": {"1.60"}, "extend.shown_limit": {"0.00"}}},
		{"1", "yes", []string{"About 14 min and 1.20 EUR per session, from its 3 sessions so far", "Adds 10 min: 18 min left, then 28 min",
			"Adds 0.40 EUR: 2.00 EUR left, then 2.40 EUR", "It may also start 1 more session.", "The project has 50.00 EUR left this month."},
			url.Values{"extend.shown_now": {"1"}, "extend.shown_starts": {"1"}, "extend.shown_agent_time_s": {"600"}, "extend.shown_eur": {"0.40"}, "extend.shown_limit": {"0.00"}}},
		{"1", "", []string{"Adds 10 min: 18 min left, then 28 min", "Adds no money: 2.00 EUR left", "It may also start 1 more session."},
			url.Values{"extend.shown_now": {"1"}, "extend.shown_starts": {"1"}, "extend.shown_agent_time_s": {"600"}, "extend.shown_eur": {"0.00"}, "extend.shown_limit": {"0.00"}}},
	} {
		body, guard := readout(t, r, ck, "t-run", tc.sessions, tc.money)
		for _, l := range tc.lines {
			if !strings.Contains(body, "<p>"+l+"</p>") {
				t.Errorf("the readout at %s with money %q lacks %q:\n%s", tc.sessions, tc.money, l, body)
			}
		}
		if guard.Encode() != tc.guard.Encode() {
			t.Errorf("the readout at %s with money %q guards %v, want %v", tc.sessions, tc.money, guard, tc.guard)
		}
	}
}

// pressRoom posts the room of sessions with its money and guard on card
// id, as the opened card's form sends it.
func pressRoom(t *testing.T, r *rig, ck *http.Cookie, csrf, id, sessions string, guard url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"csrf": {csrf}, "action": {factoryd.ActExtend}, "extend.sessions": {sessions}, "extend.money": {"yes"}}
	for k, v := range guard {
		form[k] = v
	}
	return r.do(post("/tasks/"+id+"/resolve", form), ck)
}

// A press that posts the readout's guard writes exactly the raises it
// showed: 2 starts, 24 minutes and 1.60 EUR, so #32 holds 1 + 2 = 3
// raised sessions, 40 + 24 = 64 minutes raised and 7.60 EUR under its
// 10.00 limit.
func TestThePressAppliesWhatTheReadoutShowed(t *testing.T) {
	r, ra := aheadRig(t)
	ck, s := r.loggedIn(t)
	_, guard := readout(t, r, ck, "t-run", "2", "yes")
	if rec := pressRoom(t, r, ck, s.csrf, "t-run", "2", guard); rec.Code != http.StatusSeeOther {
		t.Fatalf("the press = %d: %s", rec.Code, rec.Body)
	}
	card := ra.stored(t, "t-run")
	if card.Usage.RaisedSessions != 3 || card.Usage.RaisedTime != 64*time.Minute || factoryd.Cents(card.BudgetEUR) != 760 || factoryd.Cents(card.CeilingEUR) != 1000 {
		t.Fatalf("#32 after the press: %d raised sessions, %s raised time, %.2f EUR under %.2f; want 3, 1h4m, 7.60 under 10.00",
			card.Usage.RaisedSessions, card.Usage.RaisedTime, card.BudgetEUR, card.CeilingEUR)
	}
}

// Two presses on one readout (Review Focus 4): the card's agent time moves
// from 42 to 46 minutes between the readout and the press. A session's
// worth is then 46 / 3 = 15 min 20 s and 14 minutes are left, so the room
// now is min(1, 0, 1) = 0, and 2 sessions' worth need 2 - 1 = 1 start, 2
// x 920 - 840 = 1000 s, 17 minutes rounded up, and 2 x 1.20 - 2.00 = 0.40
// EUR, on a room of 0. The press is refused with those figures, changes nothing, and asks
// the readout to read again. The readout then guards them, and a second
// press with that guard gives them: 1 + 1 = 2 raised sessions, 40 + 17 =
// 57 minutes and 6.40 EUR.
func TestAPressAfterTheCardRanOnIsRefused(t *testing.T) {
	r, ra := aheadRig(t)
	ck, s := r.loggedIn(t)
	_, guard := readout(t, r, ck, "t-run", "2", "yes")
	if _, err := ra.pg.DB().ExecContext(t.Context(), `UPDATE tasks SET agent_time_ns = $2 WHERE id = $1`, "t-run", int64(46*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec := pressRoom(t, r, ck, s.csrf, "t-run", "2", guard)
	const want = "The room changed since this opened: 2 sessions' worth now add 17 min and 0.40 EUR. It may also start 1 more session. Press again to give them."
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want || rec.Header().Get("HX-Trigger") != "room-refused" {
		t.Fatalf("the press after the card ran on = %d %q, trigger %q; want 409 with %q and the readout asked to read again",
			rec.Code, rec.Body, rec.Header().Get("HX-Trigger"), want)
	}
	if card := ra.stored(t, "t-run"); card.Usage.RaisedSessions != 1 || card.Usage.RaisedTime != 40*time.Minute || factoryd.Cents(card.BudgetEUR) != 600 {
		t.Fatalf("the refused press changed #32: %d raised sessions, %s raised time, %.2f EUR", card.Usage.RaisedSessions, card.Usage.RaisedTime, card.BudgetEUR)
	}
	_, guard = readout(t, r, ck, "t-run", "2", "yes")
	if want := (url.Values{"extend.shown_now": {"0"}, "extend.shown_starts": {"1"}, "extend.shown_agent_time_s": {"1020"}, "extend.shown_eur": {"0.40"}, "extend.shown_limit": {"0.00"}}); guard.Encode() != want.Encode() {
		t.Fatalf("the readout after the card ran on guards %v, want %v", guard, want)
	}
	if rec := pressRoom(t, r, ck, s.csrf, "t-run", "2", guard); rec.Code != http.StatusSeeOther {
		t.Fatalf("the second press = %d: %s", rec.Code, rec.Body)
	}
	if card := ra.stored(t, "t-run"); card.Usage.RaisedSessions != 2 || card.Usage.RaisedTime != 57*time.Minute || factoryd.Cents(card.BudgetEUR) != 640 {
		t.Fatalf("#32 after the second press: %d raised sessions, %s raised time, %.2f EUR; want 2, 57m and 6.40", card.Usage.RaisedSessions, card.Usage.RaisedTime, card.BudgetEUR)
	}
}

// A running card's other actions hold "Give more room" opened at 1
// session's worth, its money ticked since the card lacks 0.40 EUR for it,
// the readout of that room under the fields and its guard in the form:
// the room of 1 it has, 1 start, 10 minutes (600 s) and 0.40 EUR. Its
// effect names no count, its count box takes 1 to 5 and no blank, and its
// Send takes one press at a time. No "Add money" is left.
func TestTheOpenedCardOffersRoomAheadOfTime(t *testing.T) {
	r, _ := aheadRig(t)
	ck, _ := r.loggedIn(t)
	body := drawer(t, r, ck, "t-run", "overview")
	if strings.Contains(body, "Add money") {
		t.Errorf("the opened card still offers Add money:\n%s", body)
	}
	more := otherActionsOf(t, body)
	for _, l := range []string{"About 14 min and 1.20 EUR per session, from its 3 sessions so far", "Adds 10 min: 18 min left, then 28 min",
		"Adds 0.40 EUR: 2.00 EUR left, then 2.40 EUR", "It may also start 1 more session.", "The project has 50.00 EUR left this month."} {
		if !strings.Contains(more, "<p>"+l+"</p>") {
			t.Errorf("the other actions' room lacks %q:\n%s", l, more)
		}
	}
	send := answerForm(t, body, factoryd.ActExtend)
	want := url.Values{"action": {factoryd.ActExtend}, "extend.sessions": {"1"}, "extend.money": {"yes"}, "extend.shown_now": {"1"}, "extend.shown_starts": {"1"},
		"extend.shown_agent_time_s": {"600"}, "extend.shown_eur": {"0.40"}, "extend.shown_limit": {"0.00"}}
	for k, v := range want {
		if got := send.Values[k]; strings.Join(got, ",") != strings.Join(v, ",") {
			t.Errorf("the room's form posts %s = %v, want %v", k, got, v)
		}
	}
	if send.Label != "Give more room" {
		t.Errorf("the room's label = %q, want Give more room", send.Label)
	}
	for _, w := range []string{`Gives the card more <span class="term">sessions</span>&#39; worth, and it goes on.`, `hx-disabled-elt="find .send"`,
		`name="extend.sessions" min="1" max="5" step="1" value="1" required`} {
		if !strings.Contains(more, w) {
			t.Errorf("the room's form lacks %q:\n%s", w, more)
		}
	}
}

// A proposal opens the room at its count and its tick, marked with who
// proposes it: the factory agent's 3 sessions' worth without money at the
// session cap. By hand on the cap's facts: a session's worth is the least
// session, 5 minutes (600 s over 3 sessions is less), and the 0.30 EUR
// floor (nothing spent); no start and 110 of 120 minutes left, so the room
// now is 0, and 3 aims at 3: 3 starts, no time, and no money, as asked.
func TestAProposalOpensTheRoomAtItsCount(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	cappedCard(r, t)
	seatAsk(r, "t-e", factoryd.CaseSessionCap, roomyFacts(nil))
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {Detail: map[string]any{"recommendation": "three more sessions finish the importer", "recommended_by": "the factory agent",
		"preselected": map[string]any{"action": factoryd.ActExtend, "args": map[string]any{"sessions": float64(3), "money": false}}}}}
	body := drawer(t, r, ck, "t-e", "overview")
	send := answerForm(t, body, factoryd.ActExtend)
	want := url.Values{"extend.sessions": {"3"}, "extend.shown_now": {"0"}, "extend.shown_starts": {"3"}, "extend.shown_agent_time_s": {"0"}, "extend.shown_eur": {"0.00"}, "extend.shown_limit": {"0.00"}}
	for k, v := range want {
		if got := send.Values[k]; strings.Join(got, ",") != strings.Join(v, ",") {
			t.Errorf("the proposed room posts %s = %v, want %v", k, got, v)
		}
	}
	if got := send.Values["extend.money"]; len(got) != 0 {
		t.Errorf("the proposed room ticks its money (%v), want it clear as proposed", got)
	}
	ask := askBlockOf(t, body)
	for _, l := range []string{"suggested by the factory agent", "Adds no money: 1.50 EUR left", "It may also start 3 more sessions."} {
		if !strings.Contains(ask, l) {
			t.Errorf("the proposed room lacks %q:\n%s", l, ask)
		}
	}
}

// One readout, two presses (Review Focus 4) on a card its starts limit:
// #33 ran 1 session in 14 minutes for 1.20 EUR of 10.00, under a cap of 3
// + 1 = 4 sessions and 20 + 100 = 120 minutes. Starts left 3, time left
// 106 minutes, 7 sessions' worth, money left 10.00 - 1.20 - 0.40 = 8.40
// EUR, 7: the room now is 3, and 1 more needs 1 start alone. The first
// press gives it (cap 5). A second press right after is refused by the
// draw that bought nothing yet. Once the card pushed, the room now is 4
// and 1 more again needs 1 start alone, the raises the first readout
// showed, so its guard refuses the press by the room it showed, and the
// card keeps 2 raised sessions.
func TestASecondPressOnOneReadoutIsRefused(t *testing.T) {
	r, ra, _ := roomRig(t, policy.AutonomyManual, roomCard{task: factoryd.Task{ID: "t-st", Status: "running", IssueNumber: 33, BudgetEUR: 10, SpentEUR: 1.2,
		CeilingEUR: 20, Usage: factoryd.TaskUsage{RaisedSessions: 1, RaisedTime: 100 * time.Minute}}, attempts: 1, agentTime: 14 * time.Minute})
	r.c.cfg.RoomFacts = storedRoomFacts(ra)
	ck, s := r.loggedIn(t)
	_, guard := readout(t, r, ck, "t-st", "1", "yes")
	if want := (url.Values{"extend.shown_now": {"3"}, "extend.shown_starts": {"1"}, "extend.shown_agent_time_s": {"0"}, "extend.shown_eur": {"0.00"}, "extend.shown_limit": {"0.00"}}); guard.Encode() != want.Encode() {
		t.Fatalf("the readout guards %v, want %v", guard, want)
	}
	if rec := pressRoom(t, r, ck, s.csrf, "t-st", "1", guard); rec.Code != http.StatusSeeOther {
		t.Fatalf("the first press = %d: %s", rec.Code, rec.Body)
	}
	if rec := pressRoom(t, r, ck, s.csrf, "t-st", "1", guard); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "bought nothing") {
		t.Fatalf("the second press right after = %d %q, want 409, the draw having bought nothing yet", rec.Code, rec.Body)
	}
	if _, err := ra.pg.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-st", IssueNumber: 33, Kind: factoryd.EventSessionPushed,
		Actor: factoryd.FactoryDecider, Detail: map[string]any{"commit": "c2"}}); err != nil {
		t.Fatal(err)
	}
	rec := pressRoom(t, r, ck, s.csrf, "t-st", "1", guard)
	const want = "The room changed since this opened: 1 session's worth now add no time and no money. It may also start 1 more session. Press again to give them."
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("the second press on one readout = %d %q, want 409 with %q", rec.Code, rec.Body, want)
	}
	if card := ra.stored(t, "t-st"); card.Usage.RaisedSessions != 2 {
		t.Fatalf("#33 after two presses holds %d raised sessions, want 2: the second gave nothing", card.Usage.RaisedSessions)
	}
}

// A count left blank or left out is refused with the range on the card's
// own door and on a case's, never filled in, as one sentence, and Act
// takes nothing.
func TestABlankCountIsRefusedOnBothDoors(t *testing.T) {
	for _, form := range []url.Values{{"extend.sessions": {""}, "extend.money": {"yes"}}, {"extend.money": {"yes"}}} {
		for _, onCase := range []bool{false, true} {
			r := fundRig(t)
			setStatus(r, "running")
			if onCase {
				cappedCard(r, t)
				seatAsk(r, "t-e", factoryd.CaseSessionCap, roomyFacts(nil))
			}
			ck, s := r.loggedIn(t)
			f := url.Values{"csrf": {s.csrf}, "action": {factoryd.ActExtend}}
			for k, v := range form {
				f[k] = v
			}
			if onCase {
				f.Set("case_id", "1")
			}
			rec := r.do(post("/tasks/t-e/resolve", f), ck)
			if rec.Code != http.StatusBadRequest || strings.TrimSpace(rec.Body.String()) != "A room counts 1 to 5 sessions' worth." || len(r.acted) != 0 {
				t.Errorf("%v on a case %v = %d %q with %d Act calls; want 400 with the range and none", form, onCase, rec.Code, rec.Body, len(r.acted))
			}
		}
	}
}

// The readout shows the refusal a press would meet: on a card whose money
// cannot open a session (0.20 EUR left against the 0.30 EUR floor), a
// room without its money is Act's to refuse, and a blank count is the
// range's.
func TestTheReadoutShowsTheRefusalAPressMeets(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	cappedCard(r, t)
	seatAsk(r, "t-e", factoryd.CaseBudgetFloor, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.LeftEUR = 1, 0.2 }))
	for sessions, want := range map[string]string{"1": "<p>This card lacks the money to open a session: give the room with its money.</p>",
		"": "<p>A room counts 1 to 5 sessions&#39; worth.</p>"} {
		body, guard := readout(t, r, ck, "t-e", sessions, "")
		if strings.TrimSpace(body) != want || len(guard) != 0 {
			t.Errorf("the readout at %q without money = %q, guard %v; want %q alone", sessions, body, guard, want)
		}
	}
}

// Two humans press one readout's room at once on a card that waits on no
// ask (Review Focus 4): Act takes a card's own presses one at a time, so
// the second reads the room the first gave and is refused, and #32 holds
// one give, 2 starts, 24 minutes and 1.60 EUR: 3 raised sessions, 64
// minutes and 7.60 EUR.
func TestTwoPressesAtOnceGiveOnce(t *testing.T) {
	r, ra := aheadRig(t)
	ck, s := r.loggedIn(t)
	_, guard := readout(t, r, ck, "t-run", "2", "yes")
	start, codes := make(chan struct{}), make(chan int, 2)
	for range 2 {
		go func() {
			<-start
			codes <- pressRoom(t, r, ck, s.csrf, "t-run", "2", guard).Code
		}()
	}
	close(start)
	got := []int{<-codes, <-codes}
	slices.Sort(got)
	if !slices.Equal(got, []int{http.StatusSeeOther, http.StatusConflict}) {
		t.Errorf("two presses at once answered %v, want one given (303) and one refused (409)", got)
	}
	if card := ra.stored(t, "t-run"); card.Usage.RaisedSessions != 3 || card.Usage.RaisedTime != 64*time.Minute || factoryd.Cents(card.BudgetEUR) != 760 {
		t.Fatalf("#32 after two presses at once: %d raised sessions, %s raised time, %.2f EUR; want one give: 3, 1h4m, 7.60",
			card.Usage.RaisedSessions, card.Usage.RaisedTime, card.BudgetEUR)
	}
}
