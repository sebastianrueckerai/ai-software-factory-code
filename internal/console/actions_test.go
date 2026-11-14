package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// stallCard is sf/code's chain stall card, #41, as the poller files it.
func stallCard() factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: "t-stall", Kind: "human", Status: "waiting-human", Source: factoryd.SourceChainStall,
		Repository: "sf/code", IssueNumber: 41, SHA: "c2c2c2c", Objective: "needs you: the chain of sf/code has been pending"},
		Escalation: "The chain of sf/code has been pending. Re-run it, check again or dismiss?",
		Question: &factoryd.HumanQuestion{Ask: "The chain of sf/code has been pending. Re-run it, check again or dismiss?",
			Choices: []factoryd.QuestionChoice{
				{Value: factoryd.AnswerRerunAgain, Label: "Re-run again"},
				{Value: factoryd.AnswerCheckAgain, Label: "Check again"},
				{Value: factoryd.AnswerDismiss, Label: "Dismiss"},
			}}}
}

// The stall card's answers carry the card's commit: "re-run again"
// re-runs the head's pending runs and keeps the card waiting, nothing
// posted, a note on its case; "check again" closes the card and touches
// nothing else; "dismiss" dismisses the stall as the human and closes
// the card; both resolve its case with their value. A
// Forgejo that re-runs nothing refuses the re-run with both causes and
// what still fits;
// a head that moved on or concluded refuses with its own line, the card
// left as it is.
func TestStallCardAnswers(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/41/comments"
	closed := []string{`/api/v1/repos/sf/code/issues/41 {"state":"closed"}`}
	click := func(t *testing.T, value string) (*rig, *fakeEvents, int, int, string) {
		t.Helper()
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{stallCard()}
		ck, s := r.loggedIn(t)
		before := ev.count()
		rec := r.do(post("/tasks/t-stall/answer", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "value": {value}}), ck)
		return r, ev, before, rec.Code, rec.Body.String()
	}

	// the drawer offers the three as buttons, the re-run kept off the issue
	r, _ := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{stallCard()}
	ck, _ := r.loggedIn(t)
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-stall/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{
		`<input type="hidden" name="value" value="rerun_again">` + "\n" + `<button type="submit" aria-label="Re-run again, kept off the issue: the card stays waiting">Re-run again</button>`,
		`<input type="hidden" name="value" value="check_again">` + "\n" + `<button type="submit" aria-label="Check again, posted on the issue as you">Check again</button>`,
		`<input type="hidden" name="value" value="dismiss">` + "\n" + `<button type="submit" aria-label="Dismiss, posted on the issue as you">Dismiss</button>`,
	} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("the drawer lacks %q:\n%s", want, drawer)
		}
	}
	// only its choices: no close of its own, no free answer
	if strings.Contains(drawer, `action="/tasks/t-stall/close"`) || strings.Contains(drawer, `name="answer"`) {
		t.Errorf("the drawer offers a close of its own or a free answer; want the three choices alone:\n%s", drawer)
	}
	// a press moves the card where its answer takes it (C31): the re-run
	// keeps it waiting, check again and dismiss close it, and a closed
	// human card shows in no column
	for value, want := range map[string]string{factoryd.AnswerRerunAgain: `class="inline">`,
		factoryd.AnswerCheckAgain: `class="inline" data-to="off">`, factoryd.AnswerDismiss: `class="inline" data-to="off">`} {
		at := strings.Index(drawer, `name="value" value="`+value+`"`)
		if form := drawer[strings.LastIndex(drawer[:at], "<form"):at]; !strings.Contains(form, `action="/tasks/t-stall/answer" hx-post="/tasks/t-stall/answer" `+want) {
			t.Errorf("the %s form does not end its tag with %s:\n%s", value, want, form)
		}
	}

	r, ev, before, code, body := click(t, factoryd.AnswerRerunAgain)
	if code != http.StatusSeeOther {
		t.Fatalf("re-run again = %d %q, want 303", code, body)
	}
	if !slices.Equal(r.reran, []string{"sf/code@c2c2c2c"}) || len(r.fg.patched) != 0 || r.fg.comments[comments] != "" || len(r.dismissed) != 0 {
		t.Fatalf("re-run again: reran %v, patched %v, comment %q, dismissed %v; want the re-run alone", r.reran, r.fg.patched, r.fg.comments[comments], r.dismissed)
	}
	checkRows(t, ev, before)
	got, _ := actedOnce(t, r)
	equal(t, "the re-run's note", got, factoryd.ActInput{TaskID: "t-stall", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.AnswerRerunAgain, Note: "Re-run again"}})

	// Forgejo re-runs nothing: a 404 (no rerun API before 17) or a 403 (a
	// token without the right) both arrive as ErrNoRerun; the line names
	// both, the cause is logged once
	for _, cause := range []string{"forgejo: not found", "forgejo: refused: status 403: user does not have permission"} {
		r, ev = newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{stallCard()}
		r.rerunErr = fmt.Errorf("run 21 of sf/code: %w: %s", factoryd.ErrNoRerun, cause)
		ck, s := r.loggedIn(t)
		before = ev.count()
		var logs bytes.Buffer
		log.SetOutput(&logs)
		rec := r.do(post("/tasks/t-stall/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerRerunAgain}}), ck)
		log.SetOutput(os.Stderr)
		want := "Forgejo re-runs no action run of sf/code: it has no rerun API (that came with Forgejo 17), or the project's actions token lacks the right. Check again or dismiss."
		if rec.Code != http.StatusConflict || !strings.HasPrefix(rec.Body.String(), want) {
			t.Fatalf("re-run again on %q = %d %q, want 409 %q", cause, rec.Code, rec.Body.String(), want)
		}
		if n := strings.Count(logs.String(), "console: sebastian re-runs the stall of sf/code: run 21 of sf/code: "+factoryd.ErrNoRerun.Error()+": "+cause+"\n"); n != 1 {
			t.Fatalf("the cause logged %d times, want once:\n%s", n, logs.String())
		}
		if ev.count() != before || len(r.fg.patched) != 0 {
			t.Fatalf("the refused re-run recorded %d rows, patched %v; want nothing", ev.count()-before, r.fg.patched)
		}
	}

	r, ev, before, code, body = click(t, factoryd.AnswerCheckAgain)
	if code != http.StatusSeeOther {
		t.Fatalf("check again = %d %q, want 303", code, body)
	}
	if !slices.Equal(r.fg.patched, closed) || r.fg.comments[comments] != "Check again" || len(r.reran) != 0 || len(r.dismissed) != 0 {
		t.Fatalf("check again: patched %v, comment %q, reran %v, dismissed %v; want the card closed alone", r.fg.patched, r.fg.comments[comments], r.reran, r.dismissed)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-stall", detail: map[string]any{"note": "Check again"}},
		wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-stall",
			detail: map[string]any{"action": factoryd.AnswerCheckAgain, "case_id": int64(1), "answer": "Check again", "value": factoryd.AnswerCheckAgain, "comment": int64(1)}})

	r, ev, before, code, body = click(t, factoryd.AnswerDismiss)
	if code != http.StatusSeeOther {
		t.Fatalf("dismiss = %d %q, want 303", code, body)
	}
	if !slices.Equal(r.dismissed, []string{"sf/code@c2c2c2c by sebastian"}) || !slices.Equal(r.fg.patched, closed) || len(r.reran) != 0 {
		t.Fatalf("dismiss: dismissed %v, patched %v, reran %v; want the stall dismissed and the card closed", r.dismissed, r.fg.patched, r.reran)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-stall", detail: map[string]any{"note": "Dismiss"}},
		wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-stall",
			detail: map[string]any{"action": factoryd.AnswerDismiss, "case_id": int64(1), "answer": "Dismiss", "value": factoryd.AnswerDismiss, "comment": int64(1)}})

	for _, tc := range []struct {
		value string
		err   error
		want  string
	}{
		{factoryd.AnswerDismiss, factoryd.ErrStallGone{Head: "e5e5e5e5e5e5"}, "Card #41: the head moved on to e5e5e5e; this stall no longer stands."},
		{factoryd.AnswerRerunAgain, factoryd.ErrStallGone{Head: "c2c2c2c", Verdict: "green"}, "Card #41: the head concluded green; this stall no longer stands."},
		{factoryd.AnswerRerunAgain, factoryd.ErrNothingToRerun, "No run of the head is pending"},
	} {
		r, ev = newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{stallCard()}
		r.rerunErr, r.dismissErr = tc.err, tc.err
		ck, s := r.loggedIn(t)
		before = ev.count()
		rec := r.do(post("/tasks/t-stall/answer", url.Values{"csrf": {s.csrf}, "value": {tc.value}}), ck)
		if rec.Code != http.StatusConflict || !strings.HasPrefix(rec.Body.String(), tc.want) {
			t.Fatalf("%s on %v = %d %q, want 409 %q", tc.value, tc.err, rec.Code, rec.Body.String(), tc.want)
		}
		if ev.count() != before || len(r.fg.patched) != 0 {
			t.Fatalf("%s on %v: %d rows, patched %v; want the card left as it is", tc.value, tc.err, ev.count()-before, r.fg.patched)
		}
	}
}

// The chat's answer row takes a stall card's choice as the board's button
// does: dismiss dismisses and closes, instead of the plain human card's
// close with the choice.
func TestStallCardAnswersInTheChat(t *testing.T) {
	r, chat, _ := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, stallCard())
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#41 needs you: the chain is pending", Choices: []string{"Re-run again", "Check again", "Dismiss"}, TaskID: "t-stall", Conversation: inbox}); err != nil {
		t.Fatal(err)
	}
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(chat.next, 10)}, "choice": {"Dismiss"}})
	if code != http.StatusNoContent {
		t.Fatalf("the press = %d %q", code, body)
	}
	if !slices.Equal(r.dismissed, []string{"sf/code@c2c2c2c by sebastian"}) {
		t.Fatalf("dismissed %v, want the stall dismissed", r.dismissed)
	}
	equal(t, "the note", r.fg.commentBodies("/api/v1/repos/sf/code/issues/41/comments"), []string{"Dismiss"})
}

// rollFailedCard is the roll ladder's failed-roll card, #42 on sf/code, as
// the roller files it.
func rollFailedCard() factoryd.BoardTask {
	q := &factoryd.HumanQuestion{Ask: "3 rolls failed since the last one merged: main's newest images are not live. Retry the roll, park it or close it?",
		Choices: []factoryd.QuestionChoice{
			{Value: factoryd.AnswerRetryRoll, Label: "Retry the roll"},
			{Value: factoryd.AnswerPark, Label: "Park"},
			{Value: factoryd.AnswerClose, Label: "Close"},
		}}
	return factoryd.BoardTask{Task: factoryd.Task{ID: "t-roll", Kind: "human", Status: "waiting-human", Source: factoryd.SourceRollFailed,
		Repository: "sf/code", IssueNumber: 42, Objective: "needs you: 3 rolls failed since the last one merged"},
		Escalation: q.Ask, Question: q}
}

// The failed-roll card's three choices act from the board: the face, the
// lanes view's escalations and the drawer offer them as buttons. "Retry the roll" runs the
// roller's retry as the answer's work, which closes the card; it posts no
// comment of the human's. "Park" leaves a
// note on the card's case and posts nothing; "Close" closes the card off
// the board. Each resolves the card's case through Act with its value,
// park aside. Words that name no choice are refused with the choices.
func TestRollFailedCardAnswers(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/42/comments"
	closed := []string{`/api/v1/repos/sf/code/issues/42 {"state":"closed"}`}
	click := func(t *testing.T, value, answer string) (*rig, *fakeEvents, int, int, string) {
		t.Helper()
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{rollFailedCard()}
		ck, s := r.loggedIn(t)
		before := ev.count()
		rec := r.do(post("/tasks/t-roll/answer", url.Values{"csrf": {s.csrf}, "value": {value}, "answer": {answer}}), ck)
		return r, ev, before, rec.Code, rec.Body.String()
	}

	// the face and the drawer offer the three as buttons; a press of
	// park keeps the card where it is, retry and close take it off
	r, _ := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{rollFailedCard()}
	ck, _ := r.loggedIn(t)
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-roll/drawer?tab=overview", nil), ck).Body.String()
	face := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	lanes := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String(), "Escalations")
	// on the face and in the lanes view, the card's own escalation article
	// (its face's card is an article inside it)
	need := func(page string) string {
		at := strings.Index(page, `id="need-t-roll"`)
		if at < 0 {
			t.Fatalf("no escalation article of t-roll:\n%s", page)
		}
		end := at + strings.Index(page[at:], "</article>") + len("</article>")
		return page[at : end+strings.Index(page[end:], "</article>")]
	}
	face, lanes = need(face), need(lanes)
	for name, page := range map[string]string{"drawer": drawer, "face": face, "lanes view": lanes} {
		// only its choices: the question's Close alone, no free answer
		if strings.Contains(page, `action="/tasks/t-roll/close"`) || strings.Contains(page, `name="answer"`) {
			t.Errorf("the %s has a close form of its own or a free answer form; want the choices alone:\n%s", name, page)
		}
		for value, want := range map[string]string{
			factoryd.AnswerRetryRoll: `class="inline" data-to="off">` + "\n" + `<input type="hidden" name="csrf"`,
			factoryd.AnswerPark:      `class="inline">` + "\n" + `<input type="hidden" name="csrf"`,
		} {
			at := strings.Index(page, `name="value" value="`+value+`"`)
			if at < 0 {
				t.Fatalf("the %s lacks the %s button:\n%s", name, value, page)
			}
			if form := page[strings.LastIndex(page[:at], "<form"):at]; !strings.Contains(form, `action="/tasks/t-roll/answer" hx-post="/tasks/t-roll/answer" `+want) {
				t.Errorf("the %s's %s form does not end its tag with %s:\n%s", name, value, want, form)
			}
		}
		for _, want := range []string{
			`<button type="submit" aria-label="Retry the roll, resets the failed rolls and closes the card">Retry the roll</button>`,
			`<button type="submit" aria-label="Park, kept off the issue: the card stays waiting">Park</button>`,
			`<input type="hidden" name="value" value="close">` + "\n" + `<button type="submit" class="deny" aria-label="Close, posted on the issue as you">Close</button>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the %s lacks %q:\n%s", name, want, page)
			}
		}
	}

	act := func(action, note string) factoryd.ActInput {
		return factoryd.ActInput{TaskID: "t-roll", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
			Res: factoryd.Resolution{Action: action, Note: note}}
	}
	r, _, _, code, body := click(t, factoryd.AnswerRetryRoll, "")
	if code != http.StatusSeeOther {
		t.Fatalf("retry = %d %q, want 303", code, body)
	}
	if r.fg.comments[comments] != "" || len(r.fg.patched) != 0 || !slices.Equal(r.rollRetried, []string{"t-roll"}) {
		t.Fatalf("retry: comment %q, patched %v, retried %v; want no comment posted and the roller's retry run for the card", r.fg.comments[comments], r.fg.patched, r.rollRetried)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the retry", []any{got, posts}, []any{act(factoryd.AnswerRetryRoll, "Retry the roll"), true})

	r, _, _, code, body = click(t, factoryd.AnswerPark, "")
	if code != http.StatusSeeOther {
		t.Fatalf("park = %d %q, want 303", code, body)
	}
	if r.fg.comments[comments] != "" || len(r.fg.patched) != 0 {
		t.Fatalf("park: comment %q, patched %v; want nothing posted", r.fg.comments[comments], r.fg.patched)
	}
	got, posts = actedOnce(t, r)
	equal(t, "the park", []any{got, posts}, []any{act(factoryd.AnswerPark, "Park"), false})

	r, ev, before, code, body := click(t, factoryd.AnswerClose, "")
	if code != http.StatusSeeOther {
		t.Fatalf("close = %d %q, want 303", code, body)
	}
	if !slices.Equal(r.fg.patched, closed) || r.fg.comments[comments] != "Close" {
		t.Fatalf("close: patched %v, comment %q; want the card closed", r.fg.patched, r.fg.comments[comments])
	}
	got, posts = actedOnce(t, r)
	equal(t, "the close", []any{got, posts}, []any{act(factoryd.AnswerClose, "Close"), true})
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-roll", detail: map[string]any{"note": "Close"}},
		wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-roll",
			detail: map[string]any{"action": factoryd.AnswerClose, "case_id": int64(1), "answer": "Close", "value": factoryd.AnswerClose, "comment": int64(1)}})

}

// The chat's answer row takes the failed-roll card's choice as the
// board's button does: "Retry the roll" runs the roller's retry and posts
// no comment, where the plain human card closes with a prefixed note.
func TestRollFailedCardAnswersInTheChat(t *testing.T) {
	r, chat, _ := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, rollFailedCard())
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#42 needs you: 3 rolls failed", Choices: []string{"Retry the roll", "Park", "Close"}, TaskID: "t-roll", Conversation: inbox}); err != nil {
		t.Fatal(err)
	}
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(chat.next, 10)}, "choice": {"Retry the roll"}})
	if code != http.StatusNoContent {
		t.Fatalf("the press = %d %q", code, body)
	}
	equal(t, "the comment", r.fg.commentBodies("/api/v1/repos/sf/code/issues/42/comments"), []string(nil))
	equal(t, "the retry", r.rollRetried, []string{"t-roll"})
	equal(t, "the patches", r.fg.patched, []string(nil))
}

// A human card's answers act by its choices: park keeps the card, posts
// nothing and leaves a note on its case, close ends it on the label and
// resolves its case with the value, free words on a card that offers no
// choice are the human's comment and a note, and the card keeps waiting;
// on a card that offers choices, words that match none and a value the
// question does not offer are refused with the labels. The go choice is
// TestGoHandsTheWaitingCardOver's.
func TestHumanAnswerParksClosesAndComments(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/51/comments"
	choices := []factoryd.QuestionChoice{{Value: "go", Label: "Have the factory do it"},
		{Value: "park", Label: "Wait for it"}, {Value: "close", Label: "Done"}}
	clickOn := func(t *testing.T, form url.Values, choices ...factoryd.QuestionChoice) (*rig, *fakeEvents, int, int, string) {
		t.Helper()
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, choices...)}
		ck, s := r.loggedIn(t)
		form.Set("csrf", s.csrf)
		before := ev.count()
		rec := r.do(post("/tasks/t-hq/answer", form), ck)
		return r, ev, before, rec.Code, rec.Body.String()
	}
	click := func(t *testing.T, form url.Values) (*rig, *fakeEvents, int, int, string) {
		return clickOn(t, form, choices...)
	}
	act := func(action, note string) factoryd.ActInput {
		return factoryd.ActInput{TaskID: "t-hq", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
			Res: factoryd.Resolution{Action: action, Note: note}}
	}

	r, _, _, code, _ := click(t, url.Values{"value": {"park"}})
	if code != http.StatusSeeOther || len(r.fg.patched) != 0 || r.fg.comments[comments] != "" {
		t.Errorf("park = %d, patched %v, comment %q; want the card kept and nothing posted", code, r.fg.patched, r.fg.comments[comments])
	}
	got, _ := actedOnce(t, r)
	equal(t, "the park", got, act(factoryd.AnswerPark, "Wait for it"))

	r, _, _, code, _ = click(t, url.Values{"value": {"close"}})
	if code != http.StatusSeeOther || r.fg.comments[comments] != "Done" || !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Errorf("close = %d, comment %q, patched %v; want the label as the note and the issue closed", code, r.fg.comments[comments], r.fg.patched)
	}
	got, _ = actedOnce(t, r)
	equal(t, "the close", got, act(factoryd.AnswerClose, "Done"))

	r, _, _, code, _ = clickOn(t, url.Values{"answer": {"Use Postgres."}})
	if code != http.StatusSeeOther || r.fg.comments[comments] != "Use Postgres." || len(r.fg.patched) != 0 {
		t.Errorf("free text = %d, comment %q, patched %v; want one comment and the card kept", code, r.fg.comments[comments], r.fg.patched)
	}
	got, _ = actedOnce(t, r)
	equal(t, "the words", got, act(factoryd.ActNote, "Use Postgres."))

	// a value the question does not offer is Act's to refuse: it reaches Act as it came
	r, _, _, _, _ = click(t, url.Values{"value": {"nonsense"}})
	if got, _ := actedOnce(t, r); got.Res.Action != "nonsense" {
		t.Errorf("Act took %+v; want the value as it came, for Act to refuse", got.Res)
	}
}

// The board's answer route takes an agent card's close value as its
// Close: Act closes the card on its case, the console posts nothing and
// patches nothing, and Act's answer is the one row.
func TestAgentCardCloseValueEndsTheCard(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{agentEndsAsk()}
	ck, s := r.loggedIn(t)
	before := ev.count()
	rec := r.do(post("/tasks/t-a/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerClose}}), ck)
	comments := r.fg.commentBodies("/api/v1/repos/sf/code/issues/15/comments")
	if rec.Code != http.StatusSeeOther || len(r.fg.patched) != 0 || len(comments) != 0 {
		t.Errorf("close = %d, patched %v, comments %q; want Act's close alone", rec.Code, r.fg.patched, comments)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the close", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-a", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActClose}}, false})
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-a",
		detail: map[string]any{"action": factoryd.ActClose, "case_id": int64(1), "answer": factoryd.ActClose}})
}

// A chat escalation's choices are their own labels as values: one that
// reads like a stall or roll value closes its card on the label, and
// calls no stall action.
func TestChatChoiceNamedLikeAStallValueClosesTheCard(t *testing.T) {
	for _, word := range []string{"dismiss", "go"} {
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "say:" + word, Label: word}, factoryd.QuestionChoice{Value: "say:retry", Label: "retry"})}
		ck, s := r.loggedIn(t)
		before := ev.count()
		rec := r.do(post("/tasks/t-hq/answer", url.Values{"csrf": {s.csrf}, "value": {"say:" + word}}), ck)
		if rec.Code != http.StatusSeeOther || len(r.dismissed) != 0 || r.fg.comments["/api/v1/repos/sf/code/issues/51/comments"] != word ||
			!slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
			t.Errorf("%s = %d, dismissed %v, comment %q, patched %v; want a close on the label", word, rec.Code, r.dismissed, r.fg.comments["/api/v1/repos/sf/code/issues/51/comments"], r.fg.patched)
		}
		checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-hq"},
			wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-hq",
				detail: map[string]any{"action": "say:" + word, "case_id": int64(1), "answer": word, "value": "say:" + word, "comment": int64(1)}})
	}
}

// The extend action is an extend through Act on the card's live case:
// its sessions and lines, the money drawn beside it, and the note Act has
// posted as the answer. A raise of money alone is a fund.
func TestExtendRaisesTheCapsAndAnswers(t *testing.T) {
	r := fundRig(t)
	_, s := r.loggedIn(t)
	if _, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 1, 500, 0, 2, "the restructure is worth it"); err != nil {
		t.Fatalf("extend: %v", err)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the extend", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-e", CaseID: 1, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1, "lines": 500, "agent_time_s": 0}, Fund: &policy.Draw{EUR: 2},
			Note: "the restructure is worth it"}}, true})
	r.fg.mu.Lock()
	posted := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]
	r.fg.mu.Unlock()
	equal(t, "the answer posted", posted, "the restructure is worth it")

	// the Extend form's minutes field raises the card's agent time, the
	// chat's extend's agent_time_s by its own door
	r = fundRig(t)
	if _, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 0, 0, 20, 0, ""); err != nil {
		t.Fatalf("extend by minutes: %v", err)
	}
	got, _ = actedOnce(t, r)
	equal(t, "the minutes alone", got.Res, factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 0, "lines": 0, "agent_time_s": 1200},
		Note: "Extended the card: +0 sessions, +20 minutes of agent time, +0 lines, +0.00 EUR."})

	r = fundRig(t)
	if _, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 0, 0, 0, 1.5, ""); err != nil {
		t.Fatalf("extend by money alone: %v", err)
	}
	got, _ = actedOnce(t, r)
	equal(t, "the money alone", got.Res, factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": 1.5},
		Note: "Extended the card: +0 sessions, +0 minutes of agent time, +0 lines, +1.50 EUR."})
}

// Nothing extends when the raise names nothing or names a negative: the
// refusal changes no row and posts no answer.
func TestExtendRefusesWhatChangesNothing(t *testing.T) {
	r := fundRig(t)
	_, s := r.loggedIn(t)
	for _, in := range []struct {
		sessions, lines, minutes int
		eur                      float64
	}{
		{0, 0, 0, 0}, {-1, 0, 0, 0}, {0, -500, 0, 0}, {0, 0, -20, 0}, {0, 0, 0, -2}, {1, 0, 0, -5},
	} {
		_, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", in.sessions, in.lines, in.minutes, in.eur, "")
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusBadRequest {
			t.Errorf("extend %+v = %v, want a 400 refusal", in, err)
		}
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 || task.Usage != (factoryd.TaskUsage{}) {
		t.Errorf("a refused extend moved the card: budget %v, usage %+v", task.BudgetEUR, task.Usage)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if len(r.fg.comments) != 0 {
		t.Errorf("a refused extend posted %q", r.fg.comments)
	}
}

// The extend route refuses a non-numeric field with its own line and
// changes nothing.
func TestExtendRouteRefusesNonNumeric(t *testing.T) {
	r := fundRig(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-e/extend", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"},
		"sessions": {"many"}, "lines": {"500"}, "eur": {"2"}}), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "whole number") {
		t.Fatalf("extend = %d: %s", rec.Code, rec.Body)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 {
		t.Errorf("a refused extend raised the budget to %v", task.BudgetEUR)
	}
}

// The Extend form shows beside the answer box where the card's question
// offers the exit, its fields naming what they raise.
func TestExtendFormShowsWhereTheAskOffersIt(t *testing.T) {
	r := fundRig(t)
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-e" {
			r.tasks.tasks[i].Question = factoryd.ShapedQuestion("check-in: spend_above_policy: No reply can change this: the head a1b2c3d and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting.", factoryd.QuestionFacts{}, nil)
		}
	}
	r.tasks.mu.Unlock()
	seatAsk(r, "t-e", factoryd.CaseGateStop, roomyFacts(func(f *factoryd.CaseFacts) {
		f.Head, f.PR, f.PROpen, f.Checks, f.Findings = "a1b2c3d", 9, true, "green", []string{"f1"}
	}))
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-e/resolve"`, `name="extend.minutes"`, `name="extend.sessions"`, `name="extend.lines"`, `name="fund_eur"`, `name="action" value="reverify"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %s", want)
		}
	}
}

// A planner's time stop shows the Extend form's minutes field first: the
// stop asks agent time, and the form's own order says so.
func TestExtendFormLeadsWithMinutesAtAPlannerTimeStop(t *testing.T) {
	r := fundRig(t)
	r.tasks.set(factoryd.PlannerOutOfTime+" and its automatic second session without filing a plan: an extend of its agent time starts one more planner session, with the answer among its findings",
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13,
			Objective: "Plan the API.", BudgetEUR: 2, ModelAlias: "factory/small"})
	seatAsk(r, "t-e", factoryd.CaseAgentTimeCap, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.AgentTimeS, f.Planner = 1, 7200, true }))
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	form := body
	minutes, sessions := strings.Index(form, `name="extend.minutes"`), strings.Index(form, `name="extend.sessions"`)
	if minutes < 0 || sessions < 0 || minutes > sessions {
		t.Fatalf("the extend form at the planner's stop leads with %q; want the minutes field before the sessions", form[:min(len(form), 300)])
	}
}

// The extend route reads the minutes field into the card's agent time, as
// the chat's extend carries it, and bounds it: 20 minutes act as 1200
// seconds, 601 are refused naming the bound and draw nothing.
func TestExtendRouteTakesMinutes(t *testing.T) {
	r := fundRig(t)
	ck, s := r.loggedIn(t)
	form := func(minutes string) url.Values {
		return url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "sessions": {"0"}, "minutes": {minutes}, "lines": {"0"}, "eur": {"0"}}
	}
	if rec := r.do(post("/tasks/t-e/extend", form("20")), ck); rec.Code >= 400 {
		t.Fatalf("extend = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the posted minutes", got.Res.Args, map[string]any{"sessions": 0, "lines": 0, "agent_time_s": 1200})
	r = fundRig(t)
	ck, s = r.loggedIn(t)
	rec := r.do(post("/tasks/t-e/extend", form("601")), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at most 600 minutes") {
		t.Fatalf("extend of 601 minutes = %d: %s", rec.Code, rec.Body)
	}
	if n := len(r.acted); n != 0 {
		t.Errorf("a refused extend acted %d times", n)
	}
	if rec := r.do(post("/tasks/t-e/extend", url.Values{"csrf": {s.csrf}, "project": {"ai-software-factory-code"}, "sessions": {"1"}, "lines": {"0"}, "eur": {"0"}}), ck); rec.Code >= 400 {
		t.Errorf("an extend with no minutes field = %d: %s", rec.Code, rec.Body)
	}
}

// setStatus puts card t-e of fundRig in status.
func setStatus(r *rig, status string) {
	r.tasks.set("", factoryd.Task{ID: "t-e", Kind: "agent", Status: status,
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"})
}

// A live card that is not waiting takes the raise too: an extend on no
// case, nothing posted, for the run reads the raise at its next cap check.
func TestExtendRaisesALiveCardWithoutPosting(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		r := fundRig(t)
		setStatus(r, status)
		_, s := r.loggedIn(t)
		if _, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 1, 500, 0, 2, ""); err != nil {
			t.Fatalf("%s: extend: %v", status, err)
		}
		got, _ := actedOnce(t, r)
		if got.CaseID != 0 || got.Res.Action != factoryd.ActExtend {
			t.Errorf("%s: Act took %+v, want an extend on no case", status, got)
		}
		r.fg.mu.Lock()
		comments := len(r.fg.comments)
		r.fg.mu.Unlock()
		if comments != 0 {
			t.Errorf("%s: the extend of a live card posted %d comments", status, comments)
		}
	}
}

// A card past its end refuses the extend and changes nothing.
func TestExtendRefusesADoneCard(t *testing.T) {
	r := fundRig(t)
	setStatus(r, "done")
	_, s := r.loggedIn(t)
	_, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 1, 500, 0, 2, "")
	var no *refusal
	if !errors.As(err, &no) || no.status != http.StatusConflict {
		t.Fatalf("extend of a done card = %v, want a 409 refusal", err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 || task.Usage != (factoryd.TaskUsage{}) {
		t.Errorf("a refused extend moved the card: budget %v, usage %+v", task.BudgetEUR, task.Usage)
	}
}

// A running card's drawer offers the Extend form to an approver, its
// status left where it is, and offers none to a viewer who is not one.
func TestExtendFormShowsOnALiveCardToApprovers(t *testing.T) {
	r := fundRig(t)
	setStatus(r, "running")
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-e/extend"`, `name="minutes"`, `name="sessions"`, `name="lines"`, `name="eur"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the running card's drawer lacks %s", want)
		}
	}
	form := body[strings.Index(body, `action="/tasks/t-e/extend"`):]
	if form = form[:strings.Index(form, ">")]; strings.Contains(form, "data-to") {
		t.Errorf("the extend form of a running card moves it: %s", form)
	}

	r.repoWriters["sf/code"] = append(r.repoWriters["sf/code"], "morgan")
	r.forgetWriters()
	mk, _ := memberLogin(t, r, "morgan")
	if body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer?tab=overview", nil), mk).Body.String(); strings.Contains(body, `/extend"`) {
		t.Error("a non-approver's drawer offers the extend form")
	}
}

// A grant press carries its one-line instruction into the posted answer,
// so the session the press buys is told what the human wants (the parked
// spec's fix 3: grants carry instruction).
func TestGrantCarriesItsInstruction(t *testing.T) {
	r := fundRig(t)
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return "", nil }
	_, s := r.loggedIn(t)
	if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerGrant, "land the smaller merge first", 0); err != nil {
		t.Fatalf("grant: %v", err)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if posted := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; posted != "Grant one session: land the smaller merge first" {
		t.Errorf("posted = %q", posted)
	}
}

// The Extend exit's bare press answers nothing: an extend names its
// amounts, and only the form carries them. A bare press would post
// "Extend the card" as an ordinary answer and buy the one session every
// answer buys, without raising anything — the card would stop at the
// same place and ask again.
func TestExtendPressNamesTheForm(t *testing.T) {
	r := fundRig(t)
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-e" {
			r.tasks.tasks[i].Question = factoryd.ShapedQuestion("check-in: spend_above_policy: No reply can change this: the head a1b2c3d and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting.", factoryd.QuestionFacts{}, nil)
		}
	}
	r.tasks.mu.Unlock()
	_, s := r.loggedIn(t)
	_, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerExtend, "", 0)
	var no *refusal
	if !errors.As(err, &no) || no.status != http.StatusConflict || !strings.Contains(err.Error(), "Extend form") {
		t.Fatalf("a bare extend press = %v, want the refusal that names the form", err)
	}
	if task, _, _ := r.tasks.Task(t.Context(), "t-e"); task.BudgetEUR != 2 || task.Usage != (factoryd.TaskUsage{}) {
		t.Errorf("a refused extend press moved the card: budget %v, usage %+v", task.BudgetEUR, task.Usage)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if len(r.fg.comments) != 0 {
		t.Errorf("a refused extend press posted %q", r.fg.comments)
	}
}

// A bare grant on a card whose newest denial carries a note re-offers
// that note as its instruction.
func TestBareGrantReoffersTheDenialsNote(t *testing.T) {
	r := fundRig(t)
	r.c.cfg.SessionFunded = func(context.Context, string, string) (string, error) { return "", nil }
	r.approvals.mu.Lock()
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 7, Kind: factoryd.ApprovalMerge, TaskID: "t-e",
		Status: "denied", DecisionNote: "split the migration first", CreatedAt: time.Now().Add(-time.Hour)})
	r.approvals.mu.Unlock()
	_, s := r.loggedIn(t)
	if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerGrant, "", 0); err != nil {
		t.Fatalf("bare grant: %v", err)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if posted := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; posted != "Grant one session: split the migration first" {
		t.Errorf("posted = %q, want the denial's note re-offered", posted)
	}
}

// The agent never approves a GitHub sync's pull request: a human approves
// the GitHub commits it carries. At merge rule auto, in a wake and
// unconfirmed in a human's turn alike, the agent's approval refuses before
// any call and the request stays pending. The human's own approval of it
// passes, and so does the agent's of a factory pull request factoryd wrote
// whole.
func TestAgentCannotApproveAFactoryPullRequest(t *testing.T) {
	factoryRig := func(t *testing.T, source string) *rig {
		t.Helper()
		r := newRig(t)
		r.tasks.set("", factoryd.Task{ID: "t-m", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 5, PRNumber: 9, Source: source})
		r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"high","head_sha":"abc123","factory":true}`)
		return r
	}
	at := func(s *session, wake bool, decider string) actor {
		return actor{s: s, repository: "sf/code", via: factoryd.ViaChat, wake: wake, decider: decider, level: policy.AutonomyAutonomous, mergeRule: policy.MergeAuto}
	}
	inWake := func(t *testing.T, r *rig) actor {
		t.Helper()
		s, err := r.c.wakeSession(factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}, "sebastian")
		if err != nil {
			t.Fatal(err)
		}
		return at(s, true, "agent")
	}
	inTurn := func(t *testing.T, r *rig, decider string) actor {
		t.Helper()
		_, s := r.loggedIn(t)
		r.fg.calls = nil
		return at(s, false, decider)
	}
	for _, tc := range []struct {
		name  string
		actor func(*testing.T, *rig) actor
	}{
		{"in a wake", inWake},
		{"in a human's turn the agent decided", func(t *testing.T, r *rig) actor { return inTurn(t, r, "agent") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := factoryRig(t, factoryd.SourceGitHubSync)
			_, err := r.c.doApprove(t.Context(), tc.actor(t, r), 4, "", 0)
			var no *refusal
			if !errors.As(err, &no) || no.status != http.StatusForbidden || no.msg != "A human approves a factory pull request that carries GitHub commits: record what you found with attach_analysis." {
				t.Fatalf("the agent's approval = %v, want 403 naming the human", err)
			}
			if q := r.approvals.get(4); q.Status != "pending" || len(r.fg.calls) != 0 {
				t.Fatalf("request 4 %s, calls %q: want it pending and no call", q.Status, r.fg.calls)
			}
		})
	}
	t.Run("the human", func(t *testing.T) {
		r := factoryRig(t, factoryd.SourceGitHubSync)
		note, err := r.c.doApprove(t.Context(), inTurn(t, r, "member"), 4, "", 0)
		if err != nil || note != "Approved pull request #9; factoryd merges it." ||
			!slices.Equal(r.fg.calls, []string{"POST /api/v1/repos/sf/code/pulls/9/reviews Bearer access-1", "SIGNAL approved t-m #9 by sebastian"}) {
			t.Fatalf("the human's approval = %q, %v, calls %q: want their review and their approval", note, err, r.fg.calls)
		}
	})
	t.Run("a template update in a wake", func(t *testing.T) {
		r := factoryRig(t, factoryd.SourceTemplateUpdate)
		note, err := r.c.doApprove(t.Context(), inWake(t, r), 4, "", 0)
		if err != nil || note != "Approved pull request #9; factoryd merges it." || !slices.Equal(r.fg.calls, []string{"SIGNAL approved t-m #9 by factory-chat"}) {
			t.Fatalf("the agent's approval = %q, %v, calls %q: want it signalled as factory-chat's", note, err, r.fg.calls)
		}
	})
}

// overruleCard seats fundRig's t-e at the deadlock ask: its question offers
// the exits, and its live case 1 holds two review findings and a red
// check's on head a1b2c3d.
func overruleCard(t *testing.T) *rig {
	t.Helper()
	r := fundRig(t)
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-e" {
			r.tasks.tasks[i].Question = factoryd.ShapedQuestion("check-in: spend_above_policy: No reply can change this: the head a1b2c3d and the card's limits are unchanged since the last stop. Extend the card, overrule the review, end the card, or leave it waiting.", factoryd.QuestionFacts{}, nil)
		}
	}
	r.tasks.mu.Unlock()
	seatAsk(r, "t-e", factoryd.CaseFindingsRepeated, roomyFacts(func(f *factoryd.CaseFacts) {
		f.Head, f.PR, f.PROpen, f.Checks = "a1b2c3d", 9, true, "red"
		f.Findings, f.ReviewFindings = []string{"c1", "r1", "r2"}, []string{"r1", "r2"}
		f.FindingTexts = map[string]string{"c1": "checks: TestFlag failed", "r1": "rename the flag", "r2": "the flag lacks a test"}
	}))
	return r
}

// overruleOf is the resolution an overrule of overruleCard's review with
// reason takes: a re-verify with a drop verdict on each review finding, the
// reason the human's, and none on the red check's.
func overruleOf(reason string) factoryd.Resolution {
	drop := factoryd.CheckVerdict{State: factoryd.CheckDropped, Evidence: "the human overruled the review: " + reason}
	return factoryd.Resolution{Action: factoryd.ActReverify, Note: reason,
		Verdicts: map[string]factoryd.CheckVerdict{"rename the flag": drop, "the flag lacks a test": drop}}
}

// Overrule the review on the deadlock ask re-verifies the head with a drop
// verdict on each finding the head's newest review raised, the human's
// reason its evidence and named as theirs, so the review on the same head
// stands on the drop record instead of raising them again; the red check's
// finding is no review's and keeps no verdict. The board's button carries
// the reason in its own field.
func TestOverruleTheReviewCarriesDropVerdicts(t *testing.T) {
	const reason = "the flag's name is the spec's, see spec.md section 2"
	for name, form := range map[string]url.Values{
		"the answer's words": {"value": {factoryd.AnswerOverrule}, "answer": {reason}},
		"the reason field":   {"value": {factoryd.AnswerOverrule}, "instruction": {reason}},
	} {
		t.Run(name, func(t *testing.T) {
			r := overruleCard(t)
			ck, s := r.loggedIn(t)
			form.Set("csrf", s.csrf)
			if rec := r.do(post("/tasks/t-e/answer", form), ck); rec.Code != http.StatusSeeOther {
				t.Fatalf("overrule = %d: %s", rec.Code, rec.Body)
			}
			got, _ := actedOnce(t, r)
			if want := overruleOf(reason); got.CaseID != 1 || got.Rung != 3 || !reflect.DeepEqual(got.Res, want) {
				t.Fatalf("Act took %+v, want %+v on case 1 by the human", got, want)
			}
		})
	}
	// a card whose ask no case decides shows the question's own buttons
	r := overruleCard(t)
	r.openCases = map[string][]factoryd.Case{}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-e/drawer?tab=overview", nil), ck).Body.String()
	if !strings.Contains(body, `<input type="hidden" name="value" value="overrule">
<input type="text" name="instruction" maxlength="4000" required aria-label="your reason for overruling the review, also the card's answer" placeholder="reason (required)">`) {
		t.Errorf("the board's overrule button has no required reason field:\n%s", body)
	}
}

// An overrule with no words is refused before Act, naming the reason
// field, on the board, in the inbox and on a case's form; the inbox's
// press with its reason and the chat's "Overrule the review: <reason>"
// take the overrule.
func TestOverruleTheReviewNeedsItsReason(t *testing.T) {
	const reason = "the spec names the flag"
	r := overruleCard(t)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerOverrule}}), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reason field") || len(r.acted) != 0 {
		t.Fatalf("a wordless overrule = %d %q with %d Act calls; want 400 naming the reason field, none", rec.Code, rec.Body, len(r.acted))
	}
	a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat}
	ask := factoryd.ChatMessage{Kind: factoryd.MessageEscalation, TaskID: "t-e", At: time.Now(), Content: "#13 needs you: No reply can change this"}
	if _, err := r.c.resolveChoice(t.Context(), a, "t-e", "Overrule the review", "", ask); err == nil || !strings.Contains(err.Error(), "reason field") || len(r.acted) != 0 {
		t.Fatalf("the inbox's wordless overrule = %v with %d Act calls; want the refusal naming the reason field, none", err, len(r.acted))
	}
	if _, err := r.c.resolveChoice(t.Context(), a, "t-e", "Overrule the review", reason, ask); err != nil {
		t.Fatal(err)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, overruleOf(reason)) {
		t.Fatalf("the inbox's overrule took %+v, want %+v", got.Res, overruleOf(reason))
	}
	r.acted = nil
	if _, err := r.c.doAnswer(t.Context(), a, "t-e", "Overrule the review: "+reason); err != nil {
		t.Fatal(err)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, overruleOf(reason)) {
		t.Fatalf("the chat's worded overrule took %+v, want %+v", got.Res, overruleOf(reason))
	}
}

// A case's form posts the shaped question's labels as premade answers: one
// that is a choice answers as its button does, the note its words. The
// overrule re-verifies with its drops, End the card closes with the note,
// and an overrule with no note is refused naming the field. Any other
// premade answer goes on as a continue.
func TestAPremadeChoiceAnswersAsItsButton(t *testing.T) {
	press := func(t *testing.T, premade, note string) (*rig, *httptest.ResponseRecorder) {
		t.Helper()
		r := overruleCard(t)
		ck, s := r.loggedIn(t)
		return r, r.do(post("/tasks/t-e/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"1"}, "premade": {premade}, "note": {note}}), ck)
	}
	r, rec := press(t, "Overrule the review", "the spec names the flag")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the premade overrule = %d: %s", rec.Code, rec.Body)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, overruleOf("the spec names the flag")) {
		t.Fatalf("the premade overrule took %+v", got.Res)
	}
	r, rec = press(t, "End the card", "done here")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the premade close = %d: %s", rec.Code, rec.Body)
	}
	if got, _ := actedOnce(t, r); got.Res.Action != factoryd.ActClose || got.Res.Note != "done here" {
		t.Fatalf("the premade close took %+v, want a close with the note", got.Res)
	}
	r, rec = press(t, "Overrule the review", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reason field") || len(r.acted) != 0 {
		t.Fatalf("a premade overrule with no note = %d %q with %d Act calls; want 400 naming the field, none", rec.Code, rec.Body, len(r.acted))
	}
	r, rec = press(t, "Keep the flag", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a premade answer = %d: %s", rec.Code, rec.Body)
	}
	if got, _ := actedOnce(t, r); got.Res.Action != factoryd.ActContinue || got.Res.Note != "Keep the flag" {
		t.Fatalf("a premade answer that is no choice took %+v, want a continue with its words", got.Res)
	}
}

// The checks-wait stop's Keep waiting runs the wait again; the quiet
// stop's keeps its note.
func TestAnswerActionKeepCheckingRunsTheChecksWaitAgain(t *testing.T) {
	for value, want := range map[string]string{
		factoryd.AnswerKeepChecking: factoryd.ActWaitChecks,
		factoryd.AnswerKeepWaiting:  factoryd.ActNote,
		factoryd.AnswerPark:         factoryd.ActNote,
	} {
		if got := answerAction(value, "Keep waiting", 0, factoryd.Case{}, time.Hour).Action; got != want {
			t.Errorf("%s answers with %q, want %q", value, got, want)
		}
	}
}

// A checks-wait stop opened before keep_checking existed still carries
// keep_waiting on its button: on that stop it runs the wait again too.
func TestAnswerActionKeepWaitingOnAChecksWaitStopRunsTheWait(t *testing.T) {
	stop := factoryd.Case{Kind: factoryd.CaseGateStop, Facts: map[string]any{"gate": factoryd.ActivityWaitChecks}}
	merge := factoryd.Case{Kind: factoryd.CaseGateStop, Facts: map[string]any{"gate": factoryd.ActivityMerge}}
	for name, c := range map[string]struct {
		k    factoryd.Case
		want string
	}{"checks wait": {stop, factoryd.ActWaitChecks}, "merge": {merge, factoryd.ActNote}, "no case": {factoryd.Case{}, factoryd.ActNote}} {
		if got := answerAction(factoryd.AnswerKeepWaiting, "Keep waiting", 0, c.k, time.Hour).Action; got != c.want {
			t.Errorf("%s: keep_waiting answers with %q, want %q", name, got, c.want)
		}
	}
}

// planWaitReason is the plan's wait on its failed card #5, as
// planCardFailedReason writes it.
const planWaitReason = "A dependency failed: #5 (links-api), card t-a, ended failed with no replacement, " +
	"and this plan's close and its next round wait on it. Retry the card or close it."

// planWaitCard seats plan t-p's wait on its failed card: the done plan
// holds its dependency_failed case on t-a at the human, its question the
// wait's own face.
func planWaitCard(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.tasks.set(planWaitReason, factoryd.Task{ID: "t-p", Kind: "agent", Status: "done", Track: policy.TrackPlan,
		Repository: "sf/code", IssueNumber: 1, Objective: "Link shortener"})
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-p" {
			r.tasks.tasks[i].Question = factoryd.ShapedQuestion(planWaitReason, factoryd.QuestionFacts{}, nil)
		}
	}
	r.tasks.mu.Unlock()
	r.openCases = map[string][]factoryd.Case{"t-p": {{ID: 1, TaskID: "t-p", Kind: factoryd.CaseDependencyFailed, StateKey: "t-a",
		Status: factoryd.CaseAtHuman, Facts: roomyFacts(func(f *factoryd.CaseFacts) { f.Final = "done" })}}}
	return r
}

// The plan's wait on its failed card answers on its own face: the case
// form's premade Retry files the replacement and its Close closes the
// failed card, the form's Close button closes the card too, and the
// inbox's press answers the same on the done plan.
func TestThePlansWaitAnswersOnItsOwnFace(t *testing.T) {
	press := func(t *testing.T, form url.Values) factoryd.ActInput {
		t.Helper()
		r := planWaitCard(t)
		ck, s := r.loggedIn(t)
		form.Set("csrf", s.csrf)
		form.Set("case_id", "1")
		if rec := r.do(post("/tasks/t-p/resolve", form), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("the press %q = %d: %s", form, rec.Code, rec.Body)
		}
		got, _ := actedOnce(t, r)
		if got.TaskID != "t-p" || got.CaseID != 1 || got.Rung != 3 {
			t.Fatalf("Act took %+v, want the plan's wait case by the human", got)
		}
		return got
	}
	if got := press(t, url.Values{"premade": {"Retry #5 (links-api)"}}); got.Res.Action != factoryd.ActRetry ||
		got.Res.Args["task_id"] != "t-a" || got.Res.Note != "Retry #5 (links-api)" {
		t.Fatalf("the premade Retry took %+v, want the failed card's retry", got.Res)
	}
	if got := press(t, url.Values{"premade": {"Close #5 (links-api)"}}); got.Res.Action != factoryd.ActClose ||
		got.Res.Note != "Close #5 (links-api)" {
		t.Fatalf("the premade Close took %+v, want the failed card's close", got.Res)
	}
	if got := press(t, url.Values{"action": {factoryd.ActClose}, "note": {"not wanted"}}); got.Res.Action != factoryd.ActClose ||
		got.Res.Note != "not wanted" {
		t.Fatalf("the form's Close took %+v, want the failed card's close with the note", got.Res)
	}

	r := planWaitCard(t)
	_, s := r.loggedIn(t)
	if _, err := r.events.Record(t.Context(), factoryd.Event{Repository: "sf/code", TaskID: "t-p",
		Kind: factoryd.EventQuestionAsked, At: time.Now().Add(-time.Minute),
		Detail: map[string]any{"question": planWaitReason, "case_id": int64(1), "case_kind": string(factoryd.CaseDependencyFailed)}}); err != nil {
		t.Fatal(err)
	}
	ask := factoryd.ChatMessage{Kind: factoryd.MessageEscalation, BirthKind: factoryd.EventQuestionAsked, TaskID: "t-p",
		At: time.Now(), Content: "#1 needs you: " + planWaitReason}
	a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat}
	if _, err := r.c.resolveChoice(t.Context(), a, "t-p", "Retry #5 (links-api)", "", ask); err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	if got.Res.Action != factoryd.ActRetry || got.Res.Args["task_id"] != "t-a" {
		t.Fatalf("the inbox's Retry took %+v, want the failed card's retry", got.Res)
	}
}
