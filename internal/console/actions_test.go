package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
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
// posted; "check again" closes the card and touches nothing else;
// "dismiss" dismisses the stall as the member and closes the card. A
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
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-stall",
		detail: map[string]any{"answer": "Re-run again", "value": factoryd.AnswerRerunAgain}})

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
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-stall", detail: map[string]any{"note": "Check again"}})

	r, ev, before, code, body = click(t, factoryd.AnswerDismiss)
	if code != http.StatusSeeOther {
		t.Fatalf("dismiss = %d %q, want 303", code, body)
	}
	if !slices.Equal(r.dismissed, []string{"sf/code@c2c2c2c by sebastian"}) || !slices.Equal(r.fg.patched, closed) || len(r.reran) != 0 {
		t.Fatalf("dismiss: dismissed %v, patched %v, reran %v; want the stall dismissed and the card closed", r.dismissed, r.fg.patched, r.reran)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-stall", detail: map[string]any{"note": "Dismiss"}})

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
// lanes view's escalations and the drawer offer them as buttons. "Retry the roll" posts its exact
// label as the member's comment, the roller's retry, and keeps the card
// in place: the roller closes it and files the new roll. "Park" records
// the answer and posts nothing; "Close" closes the card off the board.
// The human card runs no workflow, so no answer signals one. Words that
// name no choice are refused with the choices.
func TestRollFailedCardAnswers(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/42/comments"
	closed := []string{`/api/v1/repos/sf/code/issues/42 {"state":"closed"}`}
	var whole []string
	click := func(t *testing.T, value, answer string) (*rig, *fakeEvents, int, int, string) {
		t.Helper()
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{rollFailedCard()}
		r.c.cfg.AnswerWhole = func(_ context.Context, taskID string, comment int64, by string) error {
			whole = append(whole, fmt.Sprintf("%s %d %s", taskID, comment, by))
			return nil
		}
		ck, s := r.loggedIn(t)
		before := ev.count()
		rec := r.do(post("/tasks/t-roll/answer", url.Values{"csrf": {s.csrf}, "value": {value}, "answer": {answer}}), ck)
		return r, ev, before, rec.Code, rec.Body.String()
	}

	// the face and the drawer offer the three as buttons; a press of
	// retry or park keeps the card where it is, close takes it off
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
			factoryd.AnswerRetryRoll: `class="inline">` + "\n" + `<input type="hidden" name="csrf"`,
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
			`<button type="submit" aria-label="Retry the roll, posted on the issue as you">Retry the roll</button>`,
			`<button type="submit" aria-label="Park, kept off the issue: the card stays waiting">Park</button>`,
			`<input type="hidden" name="value" value="close">` + "\n" + `<button type="submit" class="deny" aria-label="Close, posted on the issue as you">Close</button>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the %s lacks %q:\n%s", name, want, page)
			}
		}
	}

	r, ev, before, code, body := click(t, factoryd.AnswerRetryRoll, "")
	if code != http.StatusSeeOther {
		t.Fatalf("retry = %d %q, want 303", code, body)
	}
	if r.fg.comments[comments] != "Retry the roll" || len(r.fg.patched) != 0 || len(whole) != 0 {
		t.Fatalf("retry: comment %q, patched %v, whole %v; want the exact label posted, the card kept, no signal", r.fg.comments[comments], r.fg.patched, whole)
	}
	retried := postedDetail(1)
	retried["value"] = factoryd.AnswerRetryRoll
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-roll", detail: retried})

	r, ev, before, code, body = click(t, factoryd.AnswerPark, "")
	if code != http.StatusSeeOther {
		t.Fatalf("park = %d %q, want 303", code, body)
	}
	if r.fg.comments[comments] != "" || len(r.fg.patched) != 0 {
		t.Fatalf("park: comment %q, patched %v; want nothing posted", r.fg.comments[comments], r.fg.patched)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-roll",
		detail: map[string]any{"answer": "Park", "value": factoryd.AnswerPark}})

	r, ev, before, code, body = click(t, factoryd.AnswerClose, "")
	if code != http.StatusSeeOther {
		t.Fatalf("close = %d %q, want 303", code, body)
	}
	if !slices.Equal(r.fg.patched, closed) || r.fg.comments[comments] != "Close" {
		t.Fatalf("close: patched %v, comment %q; want the card closed", r.fg.patched, r.fg.comments[comments])
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-roll", detail: map[string]any{"note": "Close"}})

	r, ev, before, code, body = click(t, "", "retry it next week")
	if code != http.StatusBadRequest || body != "Card #42 takes one of its choices: Retry the roll, Park, Close.\n" || ev.count() != before || len(r.fg.patched) != 0 {
		t.Fatalf("words = %d %q, %d rows, patched %v; want 400 naming the choices, nothing done", code, body, ev.count()-before, r.fg.patched)
	}
}

// The chat's answer row takes the failed-roll card's choice as the
// board's button does: "Retry the roll" posts the exact label the roller
// matches and keeps the card, where the plain human card closes with a
// prefixed note.
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
	equal(t, "the comment", r.fg.commentBodies("/api/v1/repos/sf/code/issues/42/comments"), []string{"Retry the roll"})
	equal(t, "the patches", r.fg.patched, []string(nil))
}

// A human card's answers act by its choices: park keeps the card and
// posts nothing, close ends it on the label, free words are the member's
// comment, signal no workflow, and the card keeps waiting; a value the
// question does not offer is refused with the labels. The go choice is
// TestGoHandsTheWaitingCardOver's.
func TestHumanAnswerParksClosesAndComments(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/51/comments"
	signals := 0
	q := func() factoryd.BoardTask {
		return humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "go", Label: "Have the factory do it"},
			factoryd.QuestionChoice{Value: "park", Label: "Wait for it"}, factoryd.QuestionChoice{Value: "close", Label: "Done"})
	}
	click := func(t *testing.T, form url.Values) (*rig, *fakeEvents, int, int, string) {
		t.Helper()
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{q()}
		r.c.cfg.AnswerWhole = func(context.Context, string, int64, string) error { signals++; return nil }
		ck, s := r.loggedIn(t)
		form.Set("csrf", s.csrf)
		before := ev.count()
		rec := r.do(post("/tasks/t-hq/answer", form), ck)
		return r, ev, before, rec.Code, rec.Body.String()
	}

	r, ev, before, code, _ := click(t, url.Values{"value": {"park"}})
	if code != http.StatusSeeOther || len(r.fg.patched) != 0 || r.fg.comments[comments] != "" {
		t.Errorf("park = %d, patched %v, comment %q; want the card kept and nothing posted", code, r.fg.patched, r.fg.comments[comments])
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-hq", detail: map[string]any{"answer": "Wait for it", "value": "park"}})

	r, _, _, code, _ = click(t, url.Values{"value": {"close"}})
	if code != http.StatusSeeOther || r.fg.comments[comments] != "Done" || !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Errorf("close = %d, comment %q, patched %v; want the label as the note and the issue closed", code, r.fg.comments[comments], r.fg.patched)
	}

	r, ev, before, code, _ = click(t, url.Values{"answer": {"Use Postgres."}})
	if code != http.StatusSeeOther || r.fg.comments[comments] != "Use Postgres." || len(r.fg.patched) != 0 {
		t.Errorf("free text = %d, comment %q, patched %v; want one comment and the card kept", code, r.fg.comments[comments], r.fg.patched)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-hq"})
	if signals != 0 {
		t.Errorf("the human card's words signalled %d workflows, want none", signals)
	}

	_, _, _, code, body := click(t, url.Values{"value": {"nonsense"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "Have the factory do it, Wait for it, Done") {
		t.Errorf("nonsense = %d %q; want a refusal naming the labels", code, body)
	}
}

// The board's answer route takes an agent card's close value as its
// Close: the issue closes with nothing posted on it, and no answer is
// recorded.
func TestAgentCardCloseValueEndsTheCard(t *testing.T) {
	r, ev := newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{agentEndsAsk()}
	ck, s := r.loggedIn(t)
	before := ev.count()
	rec := r.do(post("/tasks/t-a/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerClose}}), ck)
	comments := r.fg.commentBodies("/api/v1/repos/sf/code/issues/15/comments")
	if rec.Code != http.StatusSeeOther || !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/15 {"state":"closed"}`}) || len(comments) != 0 {
		t.Errorf("close = %d, patched %v, comments %q; want the issue closed and nothing posted", rec.Code, r.fg.patched, comments)
	}
	checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-a", detail: map[string]any{"note": ""}})
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
		checkRows(t, ev, before, wantRow{kind: factoryd.EventCardClosed, repo: "sf/code", taskID: "t-hq"})
	}
}

// The extend action: one store transaction raises the card's session cap,
// line bound and budget, and the note answers the card so the run takes
// the raise when the answer wakes it.
func TestExtendRaisesTheCapsAndAnswers(t *testing.T) {
	r := fundRig(t)
	_, s := r.loggedIn(t)
	note, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 1, 500, 2, "the restructure is worth it")
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if want := "Extended #13: +1 session, +500 lines, +2.00 EUR (budget 2.00 to 4.00), and posted your note as the answer; the card resumes."; note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
	task, _, _ := r.tasks.Task(t.Context(), "t-e")
	if task.BudgetEUR != 4 || task.Usage.RaisedSessions != 1 || task.Usage.ExtraLines != 500 {
		t.Errorf("the card after the extend = budget %v, usage %+v, want 4 EUR, one extra session, 500 extra lines", task.BudgetEUR, task.Usage)
	}
	var rows []factoryd.Event
	for _, e := range r.events.all() {
		if e.Kind == factoryd.EventCardExtended {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 || rows[0].Actor != "sebastian" || rows[0].Detail["sessions"] != 1 || rows[0].Detail["lines"] != 500 || rows[0].Detail["amount_eur"] != 2.0 {
		t.Errorf("card_extended rows = %+v, want one of sebastian's raise", rows)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if posted := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; posted != "the restructure is worth it" {
		t.Errorf("posted = %q, want the note as the answer", posted)
	}
}

// Nothing extends when the raise names nothing or names a negative: the
// refusal changes no row and posts no answer.
func TestExtendRefusesWhatChangesNothing(t *testing.T) {
	r := fundRig(t)
	_, s := r.loggedIn(t)
	for _, in := range []struct {
		sessions, lines int
		eur             float64
	}{
		{0, 0, 0}, {-1, 0, 0}, {0, -500, 0}, {0, 0, -2}, {1, 0, -5},
	} {
		_, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", in.sessions, in.lines, in.eur, "")
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
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`action="/tasks/t-e/extend"`, `name="sessions"`, `name="lines"`, `name="eur"`, `value="overrule"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %s", want)
		}
	}
}

// setStatus puts card t-e of fundRig in status.
func setStatus(r *rig, status string) {
	r.tasks.set("", factoryd.Task{ID: "t-e", Kind: "agent", Status: status,
		Repository: "sf/code", IssueNumber: 13, Objective: "Use a replica?", BudgetEUR: 2, ModelAlias: "factory/small"})
}

// A live card that is not waiting takes the raise too: the caps move, the
// event is recorded and nothing is posted, for the run reads the raise at
// its next cap check.
func TestExtendRaisesALiveCardWithoutPosting(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		r := fundRig(t)
		setStatus(r, status)
		_, s := r.loggedIn(t)
		note, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 1, 500, 2, "")
		if err != nil {
			t.Fatalf("%s: extend: %v", status, err)
		}
		if want := "Extended #13: +1 session, +500 lines, +2.00 EUR (budget 2.00 to 4.00); the card reads the raise at its next cap check."; note != want {
			t.Errorf("%s: note = %q, want %q", status, note, want)
		}
		task, _, _ := r.tasks.Task(t.Context(), "t-e")
		if task.BudgetEUR != 4 || task.Usage.RaisedSessions != 1 || task.Usage.ExtraAttempts != 0 || task.Usage.ExtraLines != 500 {
			t.Errorf("%s: the card after the extend = budget %v, usage %+v, want 4 EUR, one raised session, 500 extra lines", status, task.BudgetEUR, task.Usage)
		}
		rows := 0
		for _, e := range r.events.all() {
			if e.Kind == factoryd.EventCardExtended {
				rows++
			}
		}
		if rows != 1 {
			t.Errorf("%s: card_extended rows = %d, want 1", status, rows)
		}
		r.fg.mu.Lock()
		if len(r.fg.comments) != 0 {
			t.Errorf("%s: the extend posted %q", status, r.fg.comments)
		}
		r.fg.mu.Unlock()
	}
}

// A card past its end refuses the extend and changes nothing.
func TestExtendRefusesADoneCard(t *testing.T) {
	r := fundRig(t)
	setStatus(r, "done")
	_, s := r.loggedIn(t)
	_, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-e", 1, 500, 2, "")
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
	for _, want := range []string{`action="/tasks/t-e/extend"`, `name="sessions"`, `name="lines"`, `name="eur"`} {
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
	if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerGrant, "land the smaller merge first"); err != nil {
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
	_, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerExtend, "")
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
	if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerGrant, ""); err != nil {
		t.Fatalf("bare grant: %v", err)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if posted := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; posted != "Grant one session: split the migration first" {
		t.Errorf("posted = %q, want the denial's note re-offered", posted)
	}
}

// A plan card that has run no session yet has no alias on its row: the
// grant still asks the floor, which prices the class's session model, and
// the floor's refusal turns the grant away with nothing posted.
func TestUnfundedSessionAsksTheFloorForACardWithNoAlias(t *testing.T) {
	r := fundRig(t)
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-e" {
			r.tasks.tasks[i].ModelAlias = ""
		}
	}
	r.tasks.mu.Unlock()
	const why = "0.10 EUR free, 0.60 needed"
	var asked []string
	r.c.cfg.SessionFunded = func(_ context.Context, taskID, model string) (string, error) {
		asked = append(asked, taskID+":"+model)
		return why, nil
	}
	_, s := r.loggedIn(t)
	_, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", "", factoryd.AnswerGrant, "")
	var no *refusal
	if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != why {
		t.Fatalf("grant on a card with no alias = %v, want the floor's refusal %q", err, why)
	}
	if !slices.Equal(asked, []string{"t-e:"}) {
		t.Errorf("the floor was asked %q, want once for t-e with no alias", asked)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if len(r.fg.comments) != 0 {
		t.Errorf("a refused grant posted %q", r.fg.comments)
	}
}
