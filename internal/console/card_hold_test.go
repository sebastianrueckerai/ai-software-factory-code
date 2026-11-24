package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	startedCards(r)
	r.holdStopped = true
	args = `{"task_id":"50","reason":"out of the way"}`
	g.replies = [][]string{{call(1, "hold_card", args)}, {say("Stopped.")}}
	_, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"put #50 on hold: out of the way"}})
	stop := "#50 stops its session, keeps its work and waits in Queued until a human releases it."
	equal(t, "stream of a running card", events, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"hold_card","result":"` + stop + `"}`,
		`applied {"result":"` + stop + `","tool":"hold_card"}`, `text {"text":"Stopped."}`, `done {}`})
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
		"release_hold": "Refused: This turn runs on no line the human typed, so it releases no hold: only the human ends one. Give them the choice with ask, its press bound to release_hold, or let them press “Release the hold” on the opened card.",
		"hold_card":    "Refused: This turn runs on no line the human typed, so it sets no hold: a hold is the human's own. Give them the choice with ask, its press bound to hold_card, or let them press “Put on hold” on the opened card.",
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
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-adm", Kind: "agent", Status: "queued", Repository: "sf/code", IssueNumber: 35, AdmittedAt: &now}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-rev", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 36}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-clo", Kind: "agent", Status: "closing", Repository: "sf/code", IssueNumber: 37}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-mer", Kind: "agent", Status: "merged", Repository: "sf/code", IssueNumber: 38}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-wh", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 39}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-wp", Kind: "agent", Status: "waiting-poster", Repository: "sf/code", IssueNumber: 41}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-wi", Kind: "agent", Status: "waiting-image", Repository: "sf/code", IssueNumber: 42}})
	r.tasks.mu.Unlock()
	ck, s := r.loggedIn(t)
	before := r.events.count()
	rec := r.do(post("/tasks/t-k1/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.AnswerHold},
		"hold_card.release": {"#33"}, "hold_card.note": {"needs the lone card"}}), ck)
	equal(t, "the Hold button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	equal(t, "the hold", *holdOf(r, "t-k1"), factoryd.CardHold{Reason: "needs the lone card", Release: "t-d", By: "sebastian"})
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-k1", detail: map[string]any{"reason": "needs the lone card", "release": int64(33)}})

	if err := r.tasks.AddTaskDeps(t.Context(), "t-d2", []string{"t-k2"}); err != nil {
		t.Fatal(err)
	}
	a := actor{s: s, via: factoryd.ViaButton}
	for _, tc := range []struct {
		id, release, reason string
		code                int
		want                string
	}{
		{"t-rev", "", "x", http.StatusConflict,
			"Card #36 is in review: its checks decide it now, and it ends or goes back to work without you. Hold it once it runs again, or end it."},
		{"t-clo", "", "x", http.StatusConflict,
			"Card #37 is closing: its checks decide it now, and it ends or goes back to work without you. Hold it once it runs again, or end it."},
		{"t-mer", "", "x", http.StatusConflict,
			"Card #38 is merged: its checks decide it now, and it ends or goes back to work without you. Hold it once it runs again, or end it."},
		{"t-wh", "", "x", http.StatusConflict, "Card #39 waits for an answer and holds no work slot, so a hold frees nothing: answer it, or end it."},
		{"t-wp", "", "x", http.StatusConflict, "Card #41 waits for an answer and holds no work slot, so a hold frees nothing: answer it, or end it."},
		{"t-wi", "", "x", http.StatusConflict, "Card #42 waits for an answer and holds no work slot, so a hold frees nothing: answer it, or end it."},
		{"t-adm", "", "x", http.StatusConflict, "Card #35 took its turn and starts now: close it, or hold it once it runs."},
		{"t-k2", "", "  ", http.StatusBadRequest, "a reason of up to 300 characters is required: the board shows it on the held card"},
		{"t-k2", "", strings.Repeat("ü", 301), http.StatusBadRequest, "a reason of up to 300 characters is required: the board shows it on the held card"},
		{"t-k2", "30", "x", http.StatusConflict,
			"#30 is a plan, which is done once it files its cards, before their work lands, so it would release the hold too early: name the plan's last card, the one whose merge lands its work."},
		{"t-d", "t-k1", "x", http.StatusConflict,
			"#31 waits on #33, through its dependencies or a hold, so a hold of #33 until #31 lands would keep both from starting: name another card, or hold #33 with none."},
		{"t-k2", "34", "x", http.StatusConflict,
			"#34 waits on #32, through its dependencies or a hold, so a hold of #32 until #34 lands would keep both from starting: name another card, or hold #32 with none."},
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
	if want := "Card #32 is not on hold, so there is nothing to release: `hold_card` puts a queued or running card on hold."; !errors.As(err, &no) || no.msg != want {
		t.Errorf("the release of a card on no hold = %v, want %q", err, want)
	}
	if holdOf(r, "t-k2") != nil || r.events.count() != before+1 {
		t.Fatalf("a refused hold wrote: t-k2's hold %+v, %d rows", holdOf(r, "t-k2"), r.events.count()-before)
	}

	// the board names the answer that fits a held card by its label, and the
	// agent's door by the key it types
	rec = r.do(post("/tasks/t-k1/plan/changes", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "text": {"cut it"}}), ck)
	equal(t, "the board's plan press on a held card", []any{rec.Code, rec.Body.String()}, []any{http.StatusConflict,
		"Card #31 waits for a free work slot, in filing order, not a plan card: it is on hold until a human releases it with “Release the hold”.\n"})
	held, _, _ := r.tasks.Task(t.Context(), "t-k1")
	if err := wrongState(held, capFacts{}, "a running session"); !errors.As(err, &no) ||
		no.msg != "Card #31 waits for a free work slot, in filing order, not a running session: it is on hold until a human releases it with `release_hold`." {
		t.Errorf("the agent's door on a held card = %v", err)
	}

	rec = r.do(post("/tasks/t-k1/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {factoryd.AnswerRelease}}), ck)
	equal(t, "the Release button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	if h := holdOf(r, "t-k1"); h != nil {
		t.Fatalf("the hold stands after the release: %+v", h)
	}
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-k1"},
		wantRow{kind: factoryd.EventCardHoldEnded, repo: "sf/code", taskID: "t-k1", detail: map[string]any{"reason": "needs the lone card"}})

	// a closed card's hold holds nothing, so Release refuses it
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-d2" {
			r.tasks.tasks[i].Status, r.tasks.tasks[i].Hold = "closed", &factoryd.CardHold{Reason: "after the review", By: "sebastian"}
		}
	}
	r.tasks.mu.Unlock()
	if _, err := r.c.doReleaseHold(t.Context(), a, "t-d2"); !errors.As(err, &no) ||
		no.msg != "Card #34 is not on hold, so there is nothing to release: `hold_card` puts a queued or running card on hold." {
		t.Errorf("the release of a closed card = %v, want the refusal", err)
	}
	// the reason counts characters, as the form's maxlength does
	long := strings.Repeat("ü", 300)
	if _, err := r.c.doHoldCard(t.Context(), a, "t-k2", "", long); err != nil || holdOf(r, "t-k2") == nil || holdOf(r, "t-k2").Reason != long {
		t.Fatalf("a hold with 300 characters, 600 bytes = %v, hold %+v; want it taken", err, holdOf(r, "t-k2"))
	}
}

// startedCards adds a running card #50, a card #51 queued to resume and a
// card #52 whose hold the store keeps, all of plan-less project sf/code.
func startedCards(r *rig) {
	then := time.Now().Add(-time.Hour)
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Track: policy.TrackDirect, Process: "spec", Repository: "sf/code", IssueNumber: 50, Objective: "Add the cache."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-res", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "sf/code", IssueNumber: 51,
			Objective: "Add the index.", AdmittedAt: &then, ResumedAt: &then, Hold: &factoryd.CardHold{Reason: "out of the way", By: "sebastian"}}})
}

// A writer's press on a running card hands the seam the reason and the
// writer's login. The session took the stop: the answer says it stops, and
// the row carries stopped.
func TestHoldARunningCardFromTheBoard(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	startedCards(r)
	r.holdStopped = true
	ck, s := r.loggedIn(t)
	before := r.events.count()
	rec := r.do(post("/tasks/t-run/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {"hold_card/started"},
		"hold_card/started.note": {"out of the way"}}), ck)
	equal(t, "the Put on hold button", []any{rec.Code, rec.Header().Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	said, err := r.c.doHoldCard(t.Context(), actor{s: s, via: factoryd.ViaButton}, "t-run", "", "out of the way")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the answer", said, "#50 stops its session, keeps its work and waits in Queued until a human releases it.")
	equal(t, "the seam", r.holdCalls, []string{"t-run||out of the way|sebastian", "t-run||out of the way|sebastian"})
	wantHeld := wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-run", detail: map[string]any{"reason": "out of the way", "stopped": true}}
	checkRows(t, r.events, before, wantHeld, wantHeld)
}

// A press waits for the live session to take its stop: past the server's
// write timeout the press still answers.
func TestAHoldPressOutlivesTheWriteTimeout(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	startedCards(r)
	r.holdStopped = true
	ck, s := r.loggedIn(t)
	hold := r.c.cfg.HoldCard
	r.c.cfg.HoldCard = func(ctx context.Context, id, release, reason, by string) (factoryd.HoldResult, error) {
		time.Sleep(2 * time.Second)
		return hold(ctx, id, release, reason, by)
	}
	srv := httptest.NewUnstartedServer(r.c)
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	form := url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {"hold_card/started"}, "hold_card/started.note": {"out of the way"}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/tasks/t-run/resolve", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the press past the write timeout = %v, want its answer", err)
	}
	resp.Body.Close()
	equal(t, "the Put on hold button", []any{resp.StatusCode, resp.Header.Get("Location")}, []any{http.StatusSeeOther, "/?project=ai-software-factory-code"})
	equal(t, "the seam", r.holdCalls, []string{"t-run||out of the way|sebastian"})
}

// A running card between sessions has no live session to stop: the seam
// answers Held alone, and the answer says the card waits before its next
// session. The row carries no stopped.
func TestHoldARunningCardBetweenSessions(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	startedCards(r)
	_, s := r.loggedIn(t)
	before := r.events.count()
	a := actor{s: s, via: factoryd.ViaButton}
	said, err := r.c.doHoldCard(t.Context(), a, "t-run", "", "out of the way")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the answer", said, "#50 waits in Queued before its next session until a human releases it.")
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-run", detail: map[string]any{"reason": "out of the way"}})
	said, err = r.c.doHoldCard(t.Context(), a, "t-res", "#33", "after the lone card")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the answer with a release card", said, "#51 is on hold until #33 is merged or done, or a human releases it.")
}

// A hold the store kept stands when the stop's send fails: the press
// answers a held card and logs the error. Held false with an error is a
// storing failure.
func TestAHoldKeptBesideAFailedStopStands(t *testing.T) {
	r, _ := newWritesRig(t)
	startedCards(r)
	_, s := r.loggedIn(t)
	a := actor{s: s, via: factoryd.ViaButton}
	r.holdErr = errors.New("the ask failed")
	said, err := r.c.doHoldCard(t.Context(), a, "t-run", "", "out of the way")
	if err != nil {
		t.Fatalf("a kept hold beside a failed stop = %v, want it taken", err)
	}
	equal(t, "the answer", said, "#50 waits in Queued before its next session until a human releases it.")
	if holdOf(r, "t-run") == nil {
		t.Fatal("the hold is not stored")
	}
	r.c.cfg.HoldCard = func(context.Context, string, string, string, string) (factoryd.HoldResult, error) {
		return factoryd.HoldResult{}, errors.New("the store is down")
	}
	_, err = r.c.doHoldCard(t.Context(), a, "t-res", "", "again")
	var no *refusal
	if !errors.As(err, &no) || no.status != http.StatusInternalServerError || no.msg != "The hold could not be stored. Hold the card again." {
		t.Fatalf("a hold the store did not keep = %v, want 500 \"could not be stored\"", err)
	}
}

// A drawer is rendered before the card starts or parks, and the hold's
// started variant follows the card's state: a press that names the other
// variant still holds or releases the card.
func TestAHoldOrReleasePressedWithTheOtherVariantGoesThrough(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	startedCards(r)
	r.holdStopped = true
	ck, s := r.loggedIn(t)
	press := func(id, action string) int {
		return r.do(post("/tasks/"+id+"/resolve", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "action": {action},
			action + ".note": {"out of the way"}}), ck).Code
	}
	before := r.events.count()
	if code := press("t-run", "hold_card"); code != http.StatusSeeOther || holdOf(r, "t-run") == nil {
		t.Fatalf("the fresh key on a running card = %d, hold %+v", code, holdOf(r, "t-run"))
	}
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventCardHeld, repo: "sf/code", taskID: "t-run", detail: map[string]any{"reason": "out of the way", "stopped": true}})
	if code := press("t-d", "hold_card/started"); code != http.StatusSeeOther || holdOf(r, "t-d") == nil {
		t.Fatalf("the started key on a card that has not started = %d, hold %+v", code, holdOf(r, "t-d"))
	}
	if code := press("t-res", "release_hold"); code != http.StatusSeeOther || holdOf(r, "t-res") != nil {
		t.Fatalf("the fresh release of a parked card = %d, hold %+v", code, holdOf(r, "t-res"))
	}
	if code := press("t-d", "release_hold/started"); code != http.StatusSeeOther || holdOf(r, "t-d") != nil {
		t.Fatalf("the started release of a card that has not started = %d, hold %+v", code, holdOf(r, "t-d"))
	}
}

// A plan's planning run takes no hold, queued or running: the refusal says
// so and names what fits.
func TestAPlansPlanningRunTakesNoHold(t *testing.T) {
	r, _ := newWritesRig(t)
	then := time.Now().Add(-time.Hour)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-pq", Kind: "agent", Status: "queued", Track: policy.TrackPlan, Process: "spec", Repository: "sf/code", IssueNumber: 60, AdmittedAt: &then, ResumedAt: &then}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-pr", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "sf/code", IssueNumber: 61}})
	r.tasks.mu.Unlock()
	_, s := r.loggedIn(t)
	for id, n := range map[string]int{"t-pq": 60, "t-pr": 61} {
		_, err := r.c.doHoldCard(t.Context(), actor{s: s, via: factoryd.ViaButton}, id, "", "x")
		var no *refusal
		want := fmt.Sprintf("Card #%d is a plan's planning run: it takes no hold, so a hold keeps nothing. Close it, or leave it to plan.", n)
		if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != want {
			t.Errorf("hold %s = %v, want 409 %q", id, err, want)
		}
	}
}

// The drawer of a running card offers Put on hold with what the hold does
// to a started card; a held card that waits in Queued after its session
// offers Release the hold with the started line. A card that has not
// started keeps the lines it had.
func TestTheDrawerOffersTheStartedVariants(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	startedCards(r)
	ck, _ := r.loggedIn(t)
	drawer := func(id string) string {
		return r.do(httptest.NewRequest(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil), ck).Body.String()
	}
	const (
		holdStarted    = "The session stops now and keeps its work. The card waits in Queued until you release it, or until the card you name is merged or done."
		releaseStarted = "The card goes on from its saved work, ahead of the cards that have not started."
		holdFresh      = "The card does not start until you release it, or until the card you name is merged or done."
		releaseFresh   = "The card takes its turn to start again."
	)
	run := drawer("t-run")
	if !strings.Contains(run, `data-answer="hold_card/started"`) || !strings.Contains(run, "Put on hold") || !strings.Contains(run, holdStarted) || strings.Contains(run, holdFresh) || strings.Contains(run, "release_hold") {
		t.Errorf("a running card's drawer lacks Put on hold with the started line:\n%s", run)
	}
	res := drawer("t-res")
	if !strings.Contains(res, `data-answer="release_hold/started"`) || !strings.Contains(res, "Release the hold") || !strings.Contains(res, releaseStarted) || strings.Contains(res, releaseFresh) {
		t.Errorf("a held parked card's drawer lacks Release the hold with the started line:\n%s", res)
	}
	r.tasks.setHold("t-k1", &factoryd.CardHold{Reason: "wait", By: "sebastian"})
	fresh, free := drawer("t-k1"), drawer("t-d")
	if !strings.Contains(fresh, releaseFresh) || strings.Contains(fresh, releaseStarted) || !strings.Contains(free, holdFresh) || strings.Contains(free, holdStarted) {
		t.Errorf("a card that has not started lost its lines:\n%s\n%s", fresh, free)
	}
}

// rereadFails is the task store whose second read of card id fails: the
// hold's first read finds it, its read after a refused hold does not.
type rereadFails struct {
	Tasks
	id    string
	reads int
}

func (s *rereadFails) Task(ctx context.Context, id string) (factoryd.Task, bool, error) {
	if id == s.id {
		if s.reads++; s.reads > 1 {
			return factoryd.Task{}, false, errors.New("the store is down")
		}
	}
	return s.Tasks.Task(ctx, id)
}

// A hold the store refuses is judged on the card read again; a read that
// fails then refuses naming the card and the read, never a card #0, and
// nothing is held. A card in waiting-deps whose dependencies all ended
// before the store's write reads holdable still: the refusal says the
// hold came too late and that it fits once the card runs.
func TestAHoldWhoseRereadFailsSaysSo(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-wd", Kind: "agent", Status: "waiting-deps", Track: policy.TrackDirect,
		Process: "spec", Repository: "sf/code", IssueNumber: 43}})
	r.tasks.mu.Unlock()
	_, s := r.loggedIn(t)
	r.c.cfg.Tasks = &rereadFails{Tasks: r.c.cfg.Tasks, id: "t-k2"}
	r.c.cfg.HoldCard = func(context.Context, string, string, string, string) (factoryd.HoldResult, error) {
		return factoryd.HoldResult{}, nil
	}
	_, err := r.c.doHoldCard(t.Context(), actor{s: s, via: factoryd.ViaButton}, "t-k2", "", "after the split")
	var no *refusal
	want := "Card #32 could not be read again after the store refused its hold, so nothing was held: hold it again in a minute."
	if !errors.As(err, &no) || no.status != http.StatusServiceUnavailable || no.msg != want {
		t.Fatalf("the hold = %v, want 503 %q", err, want)
	}
	if holdOf(r, "t-k2") != nil {
		t.Fatalf("t-k2 is held: %+v", holdOf(r, "t-k2"))
	}
	_, err = r.c.doHoldCard(t.Context(), actor{s: s, via: factoryd.ViaButton}, "t-wd", "", "after the split")
	want = "Card #43's dependencies have all ended, so it starts or asks the human now: a hold comes too late. Close it, or hold it once it runs."
	if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != want {
		t.Fatalf("the hold of a card whose dependencies ended = %v, want 409 %q", err, want)
	}
}

// The board's face shows a hold in the card's waits line, and a release
// card that ended failed or closed in the line's own words; the opened
// card that has not started offers its writer Put on hold, and a held one
// Release the hold and Hold again; a card that waits for an answer offers neither.
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
			r.tasks.tasks[i].HoldView = factoryd.HoldView{
				Line: "on hold: its release card #2062 (plan #1915) ended closed and releases nothing, so a human releases the hold or holds the card on another card: after #2062"}
		}
	}
	r.tasks.mu.Unlock()
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String()
	lone := `<a class="ref-link" href="#card=t-d">#33</a> <span class="ref-of">(Add the lone card., queued)</span>`
	for id, want := range map[string]string{
		"t-k1": `<span class="it waits">on hold until ` + lone + ` is merged or done: after ` + lone + `</span>`,
		"t-k2": `<span class="it waits">on hold: its release card #2062 (plan #1915) ended closed and releases nothing, so a human releases the hold or holds the card on another card: after #2062</span>`,
	} {
		if face := faceOf(t, body, id); !strings.Contains(face, want) {
			t.Errorf("the face of %s lacks %q:\n%s", id, want, face)
		}
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-k1/drawer", nil), ck).Body.String()
	for _, want := range []string{`data-answer="release_hold"`, `data-answer="hold_card"`, `name="hold_card.release"`, `name="hold_card.note"`, "Release the hold", "Hold again"} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the held card's drawer lacks %q", want)
		}
	}
	if want := "The hold takes your new reason and the card you name in place of the ones it has."; !strings.Contains(drawer, want) ||
		strings.Contains(drawer, "The card does not start until you release it") {
		t.Errorf("Hold again on a held card says what Put on hold does, want %q", want)
	}
	// a held card that also asks keeps both: only a human releases a hold
	r.openCases = map[string][]factoryd.Case{"t-k1": {{ID: 7, TaskID: "t-k1", Kind: factoryd.CaseDependencyFailed, StateKey: "t-d", Status: factoryd.CaseAtHuman}}}
	drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-k1/drawer", nil), ck).Body.String()
	for _, want := range []string{`data-answer="release_hold"`, `data-answer="hold_card"`, "Release the hold", "Hold again"} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the held card that asks lacks %q in its drawer", want)
		}
	}
	r.openCases = nil
	drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-d/drawer", nil), ck).Body.String()
	if !strings.Contains(drawer, `data-answer="hold_card"`) || !strings.Contains(drawer, "Put on hold") || strings.Contains(drawer, "release_hold") {
		t.Errorf("a queued card's drawer offers Put on hold alone: %v %v", strings.Contains(drawer, `data-answer="hold_card"`), strings.Contains(drawer, "release_hold"))
	}
	if drawer = r.do(httptest.NewRequest(http.MethodGet, "/cards/t-r/drawer", nil), ck).Body.String(); strings.Contains(drawer, `data-answer="hold_card"`) || strings.Contains(drawer, "release_hold") {
		t.Error("a card that waits for an answer offers a hold")
	}
}

// The agent's own replan of a held card is refused naming the hold, on a
// card of no plan, which would go to its planner itself, and on a plan's
// card, whose planner could file its work under a new key that no hold
// holds. Neither reaches factoryd's Replan.
func TestTheAgentsReplanOfAHeldCard(t *testing.T) {
	r, _ := newWritesRig(t)
	withPlan(r)
	_, s := r.loggedIn(t)
	var asked []string
	r.c.cfg.Replan = func(_ context.Context, id, by, _ string, _ bool) (string, error) {
		asked = append(asked, id+" by "+by)
		return "Plan #30 goes back to its planner.", nil
	}
	r.tasks.setHold("t-d", &factoryd.CardHold{Reason: "after the review", By: "sebastian"})
	r.tasks.setHold("t-k1", &factoryd.CardHold{Reason: "after the review", By: "sebastian"})
	agent := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: policy.AutonomyAutonomous}
	for id, issue := range map[string]int{"t-d": 33, "t-k1": 31} {
		_, err := r.c.doReplan(t.Context(), agent, id, "too wide", false)
		var no *refusal
		want := fmt.Sprintf("Card #%d is on hold: after the review. Only the human releases, starts or closes a held card, and this turn runs on no line the human typed, so it takes no replan: ask the human with ask, or let them replan it on its card.", issue)
		if !errors.As(err, &no) || no.msg != want {
			t.Errorf("the agent's replan of held #%d = %v, want %q", issue, err, want)
		}
	}
	equal(t, "the replans asked", len(asked), 0)
}

// A plan's close waives its open criteria, a requirement of the spec the
// human drops: at autonomous, plan #51 with two criteria open, a typed line
// that asks nothing of the kind and the model's close of the plan yield a
// proposal and no close, as a wake's close does; the human's press closes
// it as the approver. Plan #50, which holds no open criterion, closes at
// once.
func TestAPlansCloseWaivesOnlyOnTheHumansPress(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", Autonomy: policy.AutonomyAutonomous}}
	crit := factoryd.Plan{Round: 2, Criteria: []factoryd.PlanCriterion{{ID: "k1", Kind: factoryd.CriterionInvariant},
		{ID: "k2", Kind: factoryd.CriterionInvariant}, {ID: "k3", Kind: factoryd.CriterionInvariant}},
		Cards: []factoryd.PlanCard{{Key: "int", Role: factoryd.RoleIntegration}}}
	critJSON, err := json.Marshal(crit)
	if err != nil {
		t.Fatal(err)
	}
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-lr", Kind: "agent", Status: "done", Track: policy.TrackPlan, Repository: "sf/code", IssueNumber: 51, Objective: "Build the links."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-lr-int", Kind: "agent", Status: "merged", MergeCommit: "abc", Source: factoryd.SourcePlan, Parent: "t-lr", PlanKey: "int",
			Repository: "sf/code", IssueNumber: 52, Objective: "Integrate."}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-br", Kind: "agent", Status: "done", Track: policy.TrackPlan, Repository: "sf/code", IssueNumber: 50, Objective: "Build the stats."}})
	r.tasks.plans = map[string][]factoryd.PlanRevision{
		"t-lr": {{TaskID: "t-lr", Revision: 1, Plan: critJSON}},
		"t-br": {{TaskID: "t-br", Revision: 1, Plan: json.RawMessage(`{"round": 1, "outline": ["the console"]}`)}}}
	r.tasks.criteria = map[string][]factoryd.CriterionVerdict{"t-lr-int": {{Criterion: "k1", Verdict: "met", Source: "review", Kind: factoryd.CriterionInvariant}}}
	r.tasks.mu.Unlock()
	// the integration card merged at the head its criteria gate judged
	r.events.mu.Lock()
	r.events.rows = append(r.events.rows,
		factoryd.Event{TaskID: "t-lr-int", Kind: factoryd.EventCriteriaJudged, Detail: map[string]any{"head": "h-lr"}},
		factoryd.Event{TaskID: "t-lr-int", Kind: factoryd.EventPullRequestMerged, Detail: map[string]any{"by": factoryd.MergeUser, "head": "h-lr"}})
	r.events.mu.Unlock()
	if open, err := factoryd.PlanOpenCriteria(t.Context(), planReader{r.c.cfg.Tasks, r.c.cfg.Events}, "t-lr", crit); err != nil || len(open) != 2 {
		t.Fatalf("plan #51's open criteria = %v, %v; want k2 and k3", open, err)
	}
	var closes []string
	r.c.cfg.CheckClosePlan = func(_ context.Context, id, _, _ string, approver bool) error {
		if id == "t-lr" && !approver {
			return factoryd.ErrNotApprover
		}
		return nil
	}
	r.c.cfg.CloseHeldPlan = func(_ context.Context, id, by, note string, approver bool) (int, string, bool, error) {
		waived := map[string]int{"t-lr": 2}[id]
		if waived > 0 && !approver {
			return 0, "", false, factoryd.ErrNotApprover
		}
		closes = append(closes, fmt.Sprintf("%s by %s: %d waived with %q", id, by, waived, note))
		return waived, factoryd.PlanCloseWords(by, note, false), false, nil
	}
	const closeLR = `{"task_id":"t-lr","note":"moved to the rebuild"}`

	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{call(1, "close", closeLR)}, {say("I asked them.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, "wake_ask", "Plan #51 failed."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	for _, m := range rows {
		for _, c := range m.ToolCalls {
			result = c.Result
		}
	}
	if !strings.HasPrefix(result, "Proposed to sebastian: ") || len(closes) != 0 {
		t.Fatalf("the wake's close = %q, closes %q; want a proposal and no close", result, closes)
	}

	ck, s := r.loggedIn(t)
	g.replies = [][]string{{call(1, "close", closeLR)}, {say("I proposed it.")}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"how is plan #51?"}})
	if len(tokens) != 1 || len(closes) != 0 || !strings.HasPrefix(events[0], `proposal {"arguments":`+closeLR) {
		t.Fatalf("the typed line's close: stream %q, closes %q; want one proposal and no close", events, closes)
	}
	_, events, _ = chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	closed := "Closed plan #51: its open criteria are waived."
	equal(t, "the press", events, ran("close", closeLR, closed))
	equal(t, "the closes", closes, []string{`t-lr by sebastian: 2 waived with "moved to the rebuild"`})

	const closeBR = `{"task_id":"t-br","note":"done"}`
	g.replies = [][]string{{call(1, "close", closeBR)}, {say("Closed.")}}
	_, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"close plan #50"}})
	equal(t, "the close of a plan with none open", events, []string{
		`tool {"arguments":` + closeBR + `,"duration_ms":0,"name":"close","result":"Closed plan #50."}`,
		`applied {"result":"Closed plan #50.","tool":"close"}`, `text {"text":"Closed."}`, `done {}`})
	equal(t, "the closes", closes, []string{`t-lr by sebastian: 2 waived with "moved to the rebuild"`, `t-br by sebastian: 0 waived with "done"`})
}
