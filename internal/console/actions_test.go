package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

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
// Forgejo without the rerun API refuses the re-run with what still fits;
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
	ck, s := r.loggedIn(t)
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
		if form := drawer[strings.LastIndex(drawer[:at], "<form"):at]; !strings.Contains(form, `action="/tasks/t-stall/answer" hx-post="/tasks/t-stall/answer" hx-target="#board" hx-swap="outerHTML" `+want) {
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

	r, ev = newRigWithEvents(t)
	r.tasks.tasks = []factoryd.BoardTask{stallCard()}
	r.rerunErr = factoryd.ErrNoRerun
	ck, s = r.loggedIn(t)
	before = ev.count()
	rec := r.do(post("/tasks/t-stall/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerRerunAgain}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Forgejo 17") || !strings.Contains(rec.Body.String(), "check again or dismiss") {
		t.Fatalf("re-run again without the API = %d %q, want 409 naming Forgejo 17 and what fits", rec.Code, rec.Body.String())
	}
	if ev.count() != before || len(r.fg.patched) != 0 {
		t.Fatalf("the refused re-run recorded %d rows, patched %v; want nothing", ev.count()-before, r.fg.patched)
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
		ck, s = r.loggedIn(t)
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
		if n := strings.Count(page, `action="/tasks/t-roll/close"`); n != 1 || strings.Contains(page, `name="answer"`) {
			t.Errorf("the %s has %d close forms and a free answer form %t; want the question's Close alone:\n%s", name, n, strings.Contains(page, `name="answer"`), page)
		}
		for value, want := range map[string]string{
			factoryd.AnswerRetryRoll: `class="inline">` + "\n" + `<input type="hidden" name="csrf"`,
			factoryd.AnswerPark:      `class="inline">` + "\n" + `<input type="hidden" name="csrf"`,
		} {
			at := strings.Index(page, `name="value" value="`+value+`"`)
			if at < 0 {
				t.Fatalf("the %s lacks the %s button:\n%s", name, value, page)
			}
			if form := page[strings.LastIndex(page[:at], "<form"):at]; !strings.Contains(form, `action="/tasks/t-roll/answer" hx-post="/tasks/t-roll/answer" hx-target="#board" hx-swap="outerHTML" `+want) {
				t.Errorf("the %s's %s form does not end its tag with %s:\n%s", name, value, want, form)
			}
		}
		for _, want := range []string{
			`<button type="submit" aria-label="Retry the roll, posted on the issue as you">Retry the roll</button>`,
			`<button type="submit" aria-label="Park, kept off the issue: the card stays waiting">Park</button>`,
			`<form method="post" action="/tasks/t-roll/close" hx-post="/tasks/t-roll/close" hx-target="#board" hx-swap="outerHTML" class="inline close" data-to="off">`,
			`<button type="submit" class="deny" aria-label="Close">Close</button>`,
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
// posts nothing, close ends it on the label, the factory choice hands it
// over, free words are the member's comment and the card keeps waiting; a
// value the question does not offer is refused with the labels.
func TestHumanAnswerParksClosesAndComments(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/51/comments"
	q := func() factoryd.BoardTask {
		return humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: "go", Label: "Have the factory do it"},
			factoryd.QuestionChoice{Value: "park", Label: "Wait for it"}, factoryd.QuestionChoice{Value: "close", Label: "Done"})
	}
	click := func(t *testing.T, form url.Values) (*rig, *fakeEvents, int, int, string) {
		t.Helper()
		r, ev := newRigWithEvents(t)
		r.tasks.tasks = []factoryd.BoardTask{q()}
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

	_, _, _, code, body := click(t, url.Values{"value": {"nonsense"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "Have the factory do it, Wait for it, Done") {
		t.Errorf("nonsense = %d %q; want a refusal naming the labels", code, body)
	}
}
