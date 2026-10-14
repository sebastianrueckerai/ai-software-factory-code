package console

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The answer row (roadmap 239, C14): a choice of an escalation row answers
// its card as the board's buttons do, with no model turn, the member's
// row and the factory's row landing in the thread; a second press hears
// the card is answered already, a card that waits no more says so, and a
// human-lane escalation closes with the choice.
func TestChatAnswerRow(t *testing.T) {
	r, chat, _ := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/code",
		IssueNumber: 14, Objective: "needs you: which store?"}})
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	escalation := func(task, text string, choices ...string) string {
		t.Helper()
		if err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
			Content: text, Choices: choices, TaskID: task, Conversation: inbox}); err != nil {
			t.Fatal(err)
		}
		return strconv.FormatInt(chat.next, 10)
	}
	answer := func(id, choice string) (int, string) {
		t.Helper()
		code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)}, "message": {id}, "choice": {choice}})
		return code, body[0]
	}
	said := func() []string {
		t.Helper()
		rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 50)
		var out []string
		for _, m := range rows {
			out = append(out, m.Role+" "+m.Kind+": "+m.Content)
		}
		return out
	}

	e13 := escalation("t-e", "#13 needs you: Use a replica?", "Yes", "No")
	code, body := answer(e13, "Maybe")
	equal(t, "a choice it does not offer", []any{code, body}, []any{http.StatusBadRequest, "the choice is none of the escalation's"})
	code, body = answer("999", "Yes")
	equal(t, "no such row", []any{code, body}, []any{http.StatusNotFound, "404 page not found"})
	if code, body := answer(e13, "Yes"); code != http.StatusNoContent {
		t.Fatalf("the answer = %d %q", code, body)
	}
	equal(t, "the comment as the member", r.fg.commentBodies("/api/v1/repos/sf/code/issues/13/comments"), []string{"Yes"})
	answer(e13, "No")
	equal(t, "the second press posts nothing", r.fg.commentBodies("/api/v1/repos/sf/code/issues/13/comments"), []string{"Yes"})

	e14 := escalation("t-h", "#14 needs you: which store?", "Postgres", "SQLite")
	answer(e14, "Postgres")
	equal(t, "the human card's note", r.fg.commentBodies("/api/v1/repos/sf/code/issues/14/comments"), []string{"Answered in the chat: Postgres"})

	e11 := escalation("t-r", "#11 needs you: rebase?", "Yes", "No")
	answer(e11, "Yes")
	equal(t, "the inbox", said(), []string{
		"assistant escalation: #13 needs you: Use a replica?",
		"user : Yes",
		"assistant poke: Answered #13 with “Yes”; the card goes on.",
		"user : No",
		"assistant poke: #13 is answered already: question answered by sebastian.",
		"assistant escalation: #14 needs you: which store?",
		"user : Postgres",
		"assistant poke: Closed #14; factoryd ends the card.",
		"assistant escalation: #11 needs you: rebase?",
		"user : Yes",
		"assistant poke: #11 waits no more: it was answered or moved on elsewhere.",
	})
}

// The escalate tool files the question and its choices through the
// factory's escalation and names the card; a list of choices outside the
// rule refuses with the rule.
func TestChatEscalate(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	_, err := trn.escalate("Which store?", nil)
	equal(t, "without the factory's escalation", err.Error(), "this factory files no escalation from the chat")
	var filed []string
	r.c.cfg.Escalate = func(_ context.Context, repository, question string, choices []string, by string) (int64, error) {
		filed = append(filed, repository+" "+by+": "+question+" "+slices.Concat([]string{"["}, choices, []string{"]"})[1])
		return 42, nil
	}
	got, err := trn.escalate(" Which store? ", []string{" Postgres ", "SQLite"})
	equal(t, "the tool's answer", []any{got, err}, []any{"Filed the escalation as #42 in Needs you; the inbox carries it to the project's members with its choices.", error(nil)})
	equal(t, "what it filed", filed, []string{"sf/code sebastian: Which store? Postgres"})
	for _, choices := range [][]string{{"one"}, {"a", "b", "c", "d", "e", "f"}, {"a", ""}} {
		if _, err := trn.escalate("Which?", choices); err == nil || err.Error() != "choices: 2 to 5 answers of 1 to 60 characters, or none" {
			t.Errorf("choices %q = %v", choices, err)
		}
	}
}
