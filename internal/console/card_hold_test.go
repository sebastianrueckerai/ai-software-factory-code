package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// holdOf is card id's hold as the board's store holds it, nil for none.
func holdOf(r *rig, id string) *factoryd.CardHold {
	k, _, _ := r.tasks.Task(context.Background(), id)
	return k.Hold
}

// The chat holds plan #30's card #31 on the lone card #33, a card of no
// plan, on the human's typed line: the hold names #33 as its release card
// and the human as who held it, and one card_held row records it.
func TestChatHoldsACardOnTheHumansWord(t *testing.T) {
	r, _, g := newChatRig(t)
	withPlan(r)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	args := `{"task_id":"31","release_card":"#33","reason":"it needs the lone card's table"}`
	g.replies = [][]string{{call(1, "hold_card", args)}, {say("Held.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"hold #31 until #33 lands: it needs the lone card's table"}})
	held := "#31 is on hold until #33 is merged or done, or a human releases it."
	equal(t, "stream", events, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"hold_card","result":"` + held + `"}`,
		`applied {"result":"` + held + `","tool":"hold_card"}`, `text {"text":"Held."}`, `done {}`})
	equal(t, "the hold", *holdOf(r, "t-k1"), factoryd.CardHold{Reason: "it needs the lone card's table", Release: "t-d", By: "sebastian"})
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-k1", IssueNumber: 31, Kind: factoryd.EventCardHeld,
		Detail: map[string]any{"reason": "it needs the lone card's table", "release": int64(33)}})
}

// A wake's turn runs on no line the human typed: its hold_card, its
// release_hold and its close of the held card are refused, each naming
// what fits, and the hold stands. The human's typed line then releases
// the hold, and closes the other held card.
func TestChatReleasesAHoldOnlyOnTheHumansLine(t *testing.T) {
	r, chat, g := newChatRig(t)
	withPlan(r)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	r.tasks.setHold("t-k1", &factoryd.CardHold{Reason: "after #33", Release: "t-d", By: "sebastian"})
	r.tasks.setHold("t-d2", &factoryd.CardHold{Reason: "after the review", By: "sebastian"})
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{call(1, "release_hold", `{"task_id":"t-k1"}`, "hold_card", `{"task_id":"t-k2","reason":"x"}`,
		"close", `{"task_id":"t-k1","note":"its dependency closed"}`)}, {say("I asked them.")}}
	before := r.events.count()
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "The hold of #31 waits."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, m := range rows {
		for _, c := range m.ToolCalls {
			results[c.Name] = c.Result
		}
	}
	equal(t, "the wake's results", results, map[string]string{
		"release_hold": "Refused: This turn runs on no line the human typed, so it releases no hold: only the human ends one. Give them the choice with ask, its press bound to release_hold, or let them press Release on the card's drawer.",
		"hold_card":    "Refused: This turn runs on no line the human typed, so it sets no hold: a hold is the human's own. Give them the choice with ask, its press bound to hold_card, or let them press Hold on the card's drawer.",
		"close":        "Refused: Card #31 is on hold: after #33. Only the human releases, starts or closes a held card, and this turn runs on no line the human typed, so it takes no close: ask the human with ask, or let them close it on its card.",
	})
	if h := holdOf(r, "t-k1"); h == nil || holdOf(r, "t-k2") != nil || len(r.fg.patched) != 0 || r.events.count() != before {
		t.Fatalf("the wake changed something: t-k1's hold %+v, t-k2's %+v, patches %v, %d rows", h, holdOf(r, "t-k2"), r.fg.patched, r.events.count()-before)
	}

	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "release_hold", `{"task_id":"t-k1"}`, "close", `{"task_id":"t-d2","note":"superseded"}`)}, {say("Done.")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"release #31, and close #34: it is superseded"}})
	released := "#31's hold is released: it queues for a work slot."
	equal(t, "stream", events, []string{
		`tool {"arguments":{"task_id":"t-k1"},"duration_ms":0,"name":"release_hold","result":"` + released + `"}`,
		`applied {"result":"` + released + `","tool":"release_hold"}`,
		`tool {"arguments":{"task_id":"t-d2","note":"superseded"},"duration_ms":0,"name":"close","result":"Closed #34; factoryd ends the card."}`,
		`applied {"result":"Closed #34; factoryd ends the card.","tool":"close"}`,
		`text {"text":"Done."}`, `done {}`})
	if h := holdOf(r, "t-k1"); h != nil {
		t.Fatalf("the hold stands after the human's release: %+v", h)
	}
	equal(t, "the close", r.fg.patched, []string{`/api/v1/repos/sf/code/issues/34 {"state":"closed"}`})
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-k1", IssueNumber: 31, Kind: factoryd.EventCardHoldEnded, Detail: map[string]any{"reason": "after #33"}},
		factoryd.Event{Repository: "sf/code", TaskID: "t-d2", IssueNumber: 34, Kind: factoryd.EventCardClosed, Detail: map[string]any{"note": "superseded"}})
}

// The drawer's Hold and Release run the same actions as buttons: a hold
// on #33 by its issue number, one card_held row, then the release, one
// card_hold_ended row. Every refusal names the state and what fits.
func TestHoldButtonsAndTheirRefusals(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	now := time.Now()
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-other", Kind: "agent", Status: "queued", Repository: "sf/other", IssueNumber: 3}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-adm", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 35, AdmittedAt: &now}})
	r.tasks.mu.Unlock()
	ck, s := r.loggedIn(t)
	before := r.events.count()
	rec := r.do(post("/tasks/t-k1/hold", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "release": {"#33"}, "reason": {"needs the lone card"}}), ck)
	equal(t, "the Hold button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	equal(t, "the hold", *holdOf(r, "t-k1"), factoryd.CardHold{Reason: "needs the lone card", Release: "t-d", By: "sebastian"})
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-k1", detail: map[string]any{"reason": "needs the lone card", "release": int64(33)}})

	a := actor{s: s, via: factoryd.ViaButton}
	for _, tc := range []struct {
		id, release, reason string
		code                int
		want                string
	}{
		{"t-r", "", "x", http.StatusConflict,
			"Card #11 runs an agent session, not a card that has not started: steer sends its session a message; its pull request comes when the session ends."},
		{"t-adm", "", "x", http.StatusConflict, "Card #35 took a work slot and starts now, not a card that has not started: close it, or steer it once it runs."},
		{"t-k2", "", "  ", http.StatusBadRequest, "a reason of up to 300 bytes is required: the board shows it on the held card"},
		{"t-k2", "t-k2", "x", http.StatusBadRequest, "#32 cannot release its own hold: name another card, or hold it with none."},
		{"t-k2", "7", "x", http.StatusConflict, "#7 is done already, so it would release the hold at once: hold #32 with no release card, or name a card that has not ended."},
		{"t-k2", "t-f", "x", http.StatusConflict, "#31 ended failed, so it would never release the hold: name a card that has not ended, or hold #32 with none."},
		{"t-k2", "99", "x", http.StatusNotFound, "There is no card #99 in this project: a release card is a card of #32's project."},
		{"t-k2", "t-other", "x", http.StatusNotFound, "There is no card t-other in this project: a release card is a card of #32's project."},
	} {
		_, err := r.c.doHoldCard(t.Context(), a, tc.id, tc.release, tc.reason)
		var no *refusal
		if !errors.As(err, &no) || no.status != tc.code || no.msg != tc.want {
			t.Errorf("hold %s on %q = %v, want %d %q", tc.id, tc.release, err, tc.code, tc.want)
		}
	}
	_, err := r.c.doReleaseHold(t.Context(), a, "t-k2")
	var no *refusal
	if want := "Card #32 is not on hold, so there is nothing to release: hold_card, or Hold on its drawer, puts a card that has not started on hold."; !errors.As(err, &no) || no.msg != want {
		t.Errorf("the release of a card on no hold = %v, want %q", err, want)
	}
	if holdOf(r, "t-k2") != nil || r.events.count() != before+1 {
		t.Fatalf("a refused hold wrote: t-k2's hold %+v, %d rows", holdOf(r, "t-k2"), r.events.count()-before)
	}

	rec = r.do(post("/tasks/t-k1/release-hold", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}}), ck)
	equal(t, "the Release button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	if h := holdOf(r, "t-k1"); h != nil {
		t.Fatalf("the hold stands after the release: %+v", h)
	}
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-k1"},
		wantRow{kind: factoryd.EventCardHoldEnded, repo: "sf/code", taskID: "t-k1", detail: map[string]any{"reason": "needs the lone card"}})
}

// The board's face shows a hold in the waiting chip, and a release card
// that ended failed or closed in the blocked chip; the drawer of a card
// that has not started offers its writer the Hold form, and of a held one
// Release; a running card's drawer offers neither.
func TestBoardShowsTheHoldAndTheDrawerOffersIt(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	r.tasks.setHold("t-k1", &factoryd.CardHold{Reason: "after #33", Release: "t-d"})
	r.tasks.setHold("t-k2", &factoryd.CardHold{Reason: "after #2062", Release: "t-x"})
	r.tasks.mu.Lock()
	for i, bt := range r.tasks.tasks {
		switch bt.ID {
		case "t-k1":
			r.tasks.tasks[i].HoldView = factoryd.HoldView{Line: "on hold until #33 is merged or done: after #33"}
		case "t-k2":
			r.tasks.tasks[i].HoldView = factoryd.HoldView{Warn: true,
				Line: "on hold: its release card #2062 (plan #1915) ended closed and releases nothing, so a human releases the hold or holds the card on another card: after #2062"}
		}
	}
	r.tasks.mu.Unlock()
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	for _, want := range []string{`<span class="chip waiting">on hold until #33 is merged or done: after #33</span>`,
		`<span class="chip waiting blocked">on hold: its release card #2062 (plan #1915) ended closed and releases nothing, so a human releases the hold or holds the card on another card: after #2062</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the board lacks %q", want)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-k1/drawer", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-k1/release-hold"`, `action="/tasks/t-k1/hold"`, `name="release"`, `name="reason"`, ">Hold again</button>"} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the held card's drawer lacks %q", want)
		}
	}
	drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-d/drawer", nil), ck).Body.String()
	if !strings.Contains(drawer, `action="/tasks/t-d/hold"`) || strings.Contains(drawer, "release-hold") {
		t.Errorf("a queued card's drawer offers the Hold form alone: %v %v", strings.Contains(drawer, `action="/tasks/t-d/hold"`), strings.Contains(drawer, "release-hold"))
	}
	if drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-r/drawer", nil), ck).Body.String(); strings.Contains(drawer, `action="/tasks/t-r/hold"`) || strings.Contains(drawer, "release-hold") {
		t.Error("a running card's drawer offers a hold")
	}
}
