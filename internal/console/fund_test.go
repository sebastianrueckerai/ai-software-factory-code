package console

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Roadmap 210: a card that outlives its budget has a human's path: an
// approver's "Give more room" on the card, with its money, which raises
// budget_eur through Act and lands in the event spine as its own row; on
// the card's case its press is the answer, so the card resumes at once.

// budgetRefusal is what the floor answers for a card whose remaining
// budget cannot fund a call (factoryd's Runner.SessionFunded).
const budgetRefusal = "the remaining budget cannot fund a call: 0.17 EUR of the 2.00 EUR budget remains, and the cheapest call needs more. Raise the budget (Fund on the board) and answer, or close the card."

// cappedCard is a waiting card of sf/code whose remaining budget of 2 EUR
// cannot fund a call.
func cappedCard(r *rig, t *testing.T) {
	t.Helper()
	r.tasks.set(budgetRefusal, factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"})
}

// Card #704 (roadmap 243): its remaining budget cannot fund a session's
// opening calls, so its opened card offers "Give more room" with the
// money ticked, and a press is a room through Act on the card's case, the
// human's on rung 3, which posts its answer. By hand: 0.20 EUR left against
// a session's worth of 0.30 EUR, the floor, leaves room for none, so 1
// session's worth needs 0.10 EUR more, and no start or time, of which the
// card holds 2 starts and 110 minutes: the press posts that readout's
// guard.
func TestTheMoneyStopOpensGiveMoreRoom(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	cappedCard(r, t)
	seatAsk(r, "t-e", factoryd.CaseBudgetFloor, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.LeftEUR = 1, 0.2 }))
	room := answerForm(t, drawer(t, r, ck, "t-e", "overview"), factoryd.ActExtend)
	if room.Action != "/tasks/t-e/resolve" || room.Label != "Give more room" || room.Values.Get("extend.sessions") != "1" || room.Values.Get("extend.money") != "yes" {
		t.Fatalf("the card's room is %+v, want Give more room at 1 session's worth with its money on the case's resolve", room)
	}
	rec := r.do(htmxPost(room.Action, room.Values), ck)
	if !pressRefreshes(rec) {
		t.Fatalf("the room by htmx = %d (%q): %s", rec.Code, rec.Header().Get("HX-Trigger"), rec.Body)
	}
	got, posts := actedOnce(t, r)
	want := roomOf(1, true, "", 3, factoryd.ViaButton)
	maps.Copy(want.Res.Args, map[string]any{"shown_starts": "0", "shown_agent_time_s": "0", "shown_eur": "0.10", "shown_limit": "0.00"})
	equal(t, "the room", []any{got, posts}, []any{want, true})
	if got, want := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"], "“Give more room”: 1 session's worth"; got != want {
		t.Fatalf("the room posted %q on the issue, want the answer in its words %q", got, want)
	}
}

// Room on a card that waits on no case, as a running one, is given on no
// case, ahead of time, with the guard of the readout the form opened with:
// 1 session's worth on a record that names no agent-time cap and no price
// is the least session, 5 minutes (300 s), and no start or money. A count
// that is no whole number never reaches Act, and no card is 404.
func TestGiveMoreRoomOnARunningCardDrawsOnNoCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-r", Kind: "agent", Status: "running", Repository: "sf/code",
		IssueNumber: 14, Objective: "Speed up the board.", BudgetEUR: 2, ModelAlias: "factory/small"})
	room := answerForm(t, drawer(t, r, ck, "t-r", "overview"), factoryd.ActExtend)
	room.Values.Set("extend.sessions", "2")
	room.Values.Set("extend.money", "yes")
	if rec := r.do(post(room.Action, room.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the room = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if got.TaskID != "t-r" || got.CaseID != 0 || got.Res.Action != factoryd.ActExtend || !reflect.DeepEqual(got.Res.Args, map[string]any{"sessions": 2, "money": true,
		"shown_starts": "0", "shown_agent_time_s": "300", "shown_eur": "0.00", "shown_limit": "0.00"}) {
		t.Fatalf("Act took %+v, want a room of 2 sessions' worth with its money for t-r on no case", got)
	}
	if rec := r.do(post("/tasks/t-missing/resolve", url.Values{"csrf": {s.csrf}, "action": {factoryd.ActExtend}, "extend.sessions": {"1"}}), ck); rec.Code != http.StatusNotFound {
		t.Fatalf("a room for no card = %d: %s", rec.Code, rec.Body)
	}
	for _, n := range []string{"two", "-1", "1.5"} {
		if rec := r.do(post("/tasks/t-r/resolve", url.Values{"csrf": {s.csrf}, "action": {factoryd.ActExtend}, "extend.sessions": {n}}), ck); rec.Code != http.StatusBadRequest {
			t.Fatalf("a room of %q = %d: %s", n, rec.Code, rec.Body)
		}
	}
	if len(r.acted) != 1 {
		t.Fatalf("the refused rooms reached Act: %+v", r.acted)
	}
}

// Forgejo refusing the answer's post answers 502 naming the post: Act
// stops there, before its case ends and its draw is written.
func TestARoomWhoseNoteForgejoRefusesAnswers502(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	cappedCard(r, t)
	seatAsk(r, "t-e", factoryd.CaseBudgetFloor, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.LeftEUR = 1, 0.2 }))
	r.fg.failAPI = true
	rec := r.do(post("/tasks/t-e/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"1"}, "action": {factoryd.ActExtend},
		"extend.sessions": {"1"}, "extend.money": {"yes"}}), ck)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "Forgejo refused to post the answer") {
		t.Fatalf("the room = %d: %s, want 502 naming the post", rec.Code, rec.Body)
	}
	if len(r.events.rows) != 0 {
		t.Fatalf("event rows = %+v, want none", r.events.rows)
	}
}

// Room on a merge card is given through Act on no case, the merge case
// aside: it neither answers nor moves the card.
func TestARoomOnAMergeCardLeavesItsCaseOpen(t *testing.T) {
	r := newRig(t)
	_, s := r.loggedIn(t)
	r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10,
		BudgetEUR: 2, SpentEUR: 1.99})
	r.openCases = map[string][]factoryd.Case{"t-m": {{ID: 4, TaskID: "t-m", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman, Facts: map[string]any{"approval": 6.0}}}}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-m", Project: "sf/code",
		PRNumber: 10, Status: "pending",
		Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	if _, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-m", map[string]any{"sessions": 1, "money": true}, "for the brief"); err != nil {
		t.Fatalf("the room: %v", err)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the room", got, factoryd.ActInput{TaskID: "t-m", By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1, "money": true}, Note: "for the brief"}})
	if a := r.approvals.get(6); a.Status != "pending" {
		t.Fatalf("the request after the room = %+v, want it pending", a)
	}
	if len(r.fg.comments) != 0 {
		t.Fatalf("the room posted %v", r.fg.comments)
	}
}
