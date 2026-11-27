package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The answer row (roadmap 239, C14): a choice of an escalation row answers
// its card as the board's buttons do, with no model turn, the member's
// row and the factory's row landing in the thread; a second press hears
// the card is answered already, a card that waits no more says so, and a
// human-lane escalation closes with the choice.
func TestChatAnswerRow(t *testing.T) {
	r, chat, _ := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-h", Kind: "human", Status: "waiting-human", Repository: "sf/code",
		IssueNumber: 14, Objective: "needs you: which store?"},
		Question: &factoryd.HumanQuestion{Ask: "Which store?", Choices: []factoryd.QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}, {Value: "say:SQLite", Label: "SQLite"}}}})
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	escalation := func(task, text string, choices ...string) string {
		t.Helper()
		if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
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
	equal(t, "a choice it does not offer", []any{code, body}, []any{http.StatusBadRequest, "the choice is none of the escalation's: press one of its buttons."})
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
	equal(t, "the human card's note", r.fg.commentBodies("/api/v1/repos/sf/code/issues/14/comments"), []string{"Postgres"})

	e11 := escalation("t-r", "#11 needs you: rebase?", "Yes", "No")
	answer(e11, "Yes")
	equal(t, "the inbox", said(), []string{
		"assistant escalation: #13 needs you: Use a replica?",
		"user : Yes",
		"assistant poke: Case 1 took continue.",
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

// agentEndsAsk is an agent card waiting on its agent's question, the
// factory's park and close behind the agent's own choice.
func agentEndsAsk() factoryd.BoardTask {
	return factoryd.BoardTask{Task: factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "Add the replica."},
		Escalation: factoryd.AgentAsks + "Use a replica?",
		Question: &factoryd.HumanQuestion{Ask: "Use a replica?", Why: "The reads grow.", Info: "An answer starts the attempt again.",
			Choices: []factoryd.QuestionChoice{{Value: "say:Yes", Label: "Yes"}, {Value: factoryd.AnswerPark, Label: "Leave it waiting"}, {Value: factoryd.AnswerClose, Label: "End the card"}}}}
}

// "End the card" pressed in the inbox closes the agent card through Act
// on its case: nothing is posted on its issue by the console, and Act's
// answer is the only row.
func TestChatAnswerRowEndsAnAgentCard(t *testing.T) {
	r, chat, _ := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, agentEndsAsk())
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#15 needs you: Use a replica?", Choices: []string{"Yes", "Leave it waiting", "End the card"}, TaskID: "t-a", Conversation: inbox}); err != nil {
		t.Fatal(err)
	}
	before := r.events.count()
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(chat.next, 10)}, "choice": {"End the card"}})
	if code != http.StatusNoContent {
		t.Fatalf("the press = %d %q", code, body)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the close", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-a", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActClose}}, false})
	equal(t, "the comments", r.fg.commentBodies("/api/v1/repos/sf/code/issues/15/comments"), []string(nil))
	checkRows(t, r.events, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-a",
		detail: map[string]any{"action": factoryd.ActClose, "case_id": int64(1), "answer": factoryd.ActClose}})
	rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	equal(t, "the factory's row", rows[len(rows)-1].Content, "Case 1 took close.")
}

// The escalate tool files the question and its five parts through the
// factory's escalation and names the card; a question without its ask
// within an issue title's 255 characters, its info or its recommendation
// refuses, as do choices outside the rule. The ask and the why line pass
// the factory's filing door.
func TestChatEscalate(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	_, err := trn.escalate(toolInput{Ask: "Which store?"})
	equal(t, "without the factory's escalation", err.Error(), "this factory files no escalation from the chat")
	var filed []factoryd.HumanQuestion
	var repo, about string
	r.c.cfg.Escalate = func(_ context.Context, repository string, q factoryd.HumanQuestion, by, card string) (int64, error) {
		repo, about, _ = repository, card, by
		filed = append(filed, q)
		return 42, nil
	}
	got, err := trn.escalate(toolInput{Ask: " Which store? ", Why: " Postgres runs out of connections at the measured peak. ", Info: " The counts and the plan section both say so. ",
		Recommendation: " Postgres, plan section 3 ", Choice: " Postgres ", Choices: []string{" Postgres ", "SQLite"}, Act: " psql -c 'SHOW max_connections' ", TaskID: "t-r"})
	equal(t, "the tool's answer", []any{got, err}, []any{"Filed the escalation as #42 in Needs you; the inbox carries it to the project's humans with its choices.", error(nil)})
	equal(t, "what it filed", []any{repo, about, filed}, []any{"sf/code", "t-r", []factoryd.HumanQuestion{{
		Ask:            "Which store?",
		Why:            "Postgres runs out of connections at the measured peak.",
		Info:           "The counts and the plan section both say so.",
		Recommendation: "Postgres, plan section 3",
		Recommended:    "say:Postgres",
		Source:         "the project agent",
		Choices:        []factoryd.QuestionChoice{{Value: "say:Postgres", Label: "Postgres"}, {Value: "say:SQLite", Label: "SQLite"}},
		Act:            "psql -c 'SHOW max_connections'",
	}}})
	two := []string{"a", "b"}
	for _, in := range []struct {
		name, ask, why, info, recommendation, choice string
		choices                                      []string
		want                                         string
		act, card                                    string
	}{
		{"no ask", "", "why", "info", "rec", "", two, "ask: 1 to 255 characters, as the card's title", "", ""},
		{"256 characters", strings.Repeat("x", 256), "why", "info", "rec", "", two, "ask: 1 to 255 characters, as the card's title", "", ""},
		{"no info", "Which?", "why", "", "rec", "", two, "info: what a human should know to answer well, 1 to 2000 characters", "", ""},
		{"no recommendation", "Which?", "why", "info", "", "", two, "recommendation: the answer you would pick and where you read it, 1 to 2000 characters", "", ""},
		{"no choices", "Which?", "why", "info", "rec", "", nil, "choices: 2 to 5 answers of 1 to 60 characters", "", ""},
		{"one choice", "Which?", "why", "info", "rec", "", []string{"one"}, "choices: 2 to 5 answers of 1 to 60 characters", "", ""},
		{"six choices", "Which?", "why", "info", "rec", "", []string{"a", "b", "c", "d", "e", "f"}, "choices: 2 to 5 answers of 1 to 60 characters", "", ""},
		{"empty choice", "Which?", "why", "info", "rec", "", []string{"a", ""}, `choices: "": The answer is empty.`, "", ""},
		{"61 characters", "Which?", "why", "info", "rec", "", []string{"a", strings.Repeat("b", 61)}, `choices: "` + strings.Repeat("b", 61) + `": The answer has 61 characters, at most 60.`, "", ""},
		{"twice", "Which?", "why", "info", "rec", "", []string{"a", " a"}, "choices: each answer once", "", ""},
		{"twice by case and end punctuation", "Which?", "why", "info", "rec", "", []string{"Close it", "close IT."}, "choices: each answer once", "", ""},
		{"a choice of none of them", "Which?", "why", "info", "rec", "c", two, "`choice`: the one of `choices` you would pick, as written there. \"c\" is none of them.", "", ""},
		{"2001 characters of act", "Which?", "why", "info", "rec", "", two, "act: the exact command or press a human does outside the factory, at most 2000 characters", strings.Repeat("x", 2001), ""},
		{"a card of no project's", "Which?", "why", "info", "rec", "", two, "There is no card t-nope in this project.", "", "t-nope"},
	} {
		if _, err := trn.escalate(toolInput{Ask: in.ask, Why: in.why, Info: in.info, Recommendation: in.recommendation, Choice: in.choice, Choices: in.choices,
			Act: in.act, TaskID: in.card}); err == nil || err.Error() != in.want {
			t.Errorf("%s: escalate = %v, want %q", in.name, err, in.want)
		}
	}
	equal(t, "refusals file nothing", len(filed), 1)
}

// A filing the factory's escalation refuses names the error and the two
// remedies, at the gateway's own 502.
func TestChatEscalateRefusalNamesTheError(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	r.c.cfg.Escalate = func(context.Context, string, factoryd.HumanQuestion, string, string) (int64, error) {
		return 0, errors.New("forgejo rejected the card: Needs you is closed for sf/code")
	}
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	_, err := trn.escalate(toolInput{Ask: "Which store?", Why: "the ledger is short", Info: "the pool is dry", Recommendation: "Postgres", Choice: "Postgres", Choices: []string{"Postgres", "SQLite"}})
	refusedAs(t, err, http.StatusBadGateway, "The escalation could not be filed: forgejo rejected the card: Needs you is closed for sf/code. "+
		"Try again, or post the question on the card's issue.")
}

// A wake about a card files no escalation that names no card: the refusal
// names the card the wake's first line names, and the ask that names it
// files with it, so a later stop of that card or its plan waits on the ask.
func TestChatEscalateInAWakeNamesItsCard(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{MonthlyEUR: 4, Confirm: []string{}}
	var about []string
	r.c.cfg.Escalate = func(_ context.Context, _ string, _ factoryd.HumanQuestion, _, card string) (int64, error) {
		about = append(about, card)
		return 42, nil
	}
	conv, err := chat.EnsureInbox(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	const ask = `"ask":"Fill the store?","why":"The build reads its files from the store.","info":"The store answers 404 for four files.",` +
		`"recommendation":"Run the copy at the pull request's head","choices":["Filled","Not now"]`
	g.replies = [][]string{{call(1, "escalate", "{"+ask+"}")}, {call(2, "escalate", `{"task_id":"t-r",`+ask+"}")}, {say("Asked.")}}
	if err := r.c.StartWake(t.Context(), "ai-software-factory-code", "sebastian", conv, factoryd.WakeTimer,
		"Timer: Re-check #11 on head bd1171a. Run the check it stood for."); err != nil {
		t.Fatalf("StartWake: %v", err)
	}
	rows, err := chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 10)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, m := range rows {
		for _, c := range m.ToolCalls {
			results = append(results, c.Result)
		}
	}
	equal(t, "the two calls", results, []string{
		"Refused: `task_id`: this turn is about #11. Name the card or plan the ask is about, so a later stop of that card or its plan waits on this ask and asks no second time.",
		"Filed the escalation as #42 in Needs you; the inbox carries it to the project's humans with its choices. It cannot be undone."})
	equal(t, "the cards the filings are about", about, []string{"t-r"})
}

// In a wake on a case escalate is refused naming the case: the case reaches
// the human by its hand-up, and a card of its own would ask a second time.
// Nothing is filed.
func TestChatEscalateInACaseWakeNamesTheCase(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	filed := 0
	r.c.cfg.Escalate = func(context.Context, string, factoryd.HumanQuestion, string, string) (int64, error) {
		filed++
		return 42, nil
	}
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.a.wake, trn.a.birth = true, factoryd.EventCaseClimbed
	r.c.books.add(trn.key(), []int64{61})
	_, err := trn.escalate(toolInput{Ask: "Which store?", Why: "the ledger is short", Info: "the pool is dry", Recommendation: "Postgres", Choices: []string{"Postgres", "SQLite"}})
	refusedAs(t, err, http.StatusConflict, "You hold case 61: hand it up with `attach_analysis`, its `case_id` and your recommendation, and it reaches the human with your analysis; a second card would ask twice.")
	equal(t, "filings", filed, 0)
}

// An escalation about a card that already asks the human in Needs you
// files no second card: the card's own ask, a helper's on it, or a human
// card the chat filed about it. The refusal names the card that asks and
// its question, and the two ways on. An ask still on its rungs, or one
// about another card, files.
func TestChatEscalateAboutACardThatAsksFilesNone(t *testing.T) {
	r, _, _ := newChatRig(t)
	_, s := r.loggedIn(t)
	var about []string
	r.c.cfg.Escalate = func(_ context.Context, _ string, _ factoryd.HumanQuestion, _, card string) (int64, error) {
		about = append(about, card)
		return 42, nil
	}
	r.tasks.tasks = append(r.tasks.tasks,
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-h", Kind: "human", Source: factoryd.SourceChat, Status: "waiting-human", Repository: "sf/code", IssueNumber: 40}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-p", Kind: "agent", Source: factoryd.SourceProver, Parent: "t-m", Status: "waiting-human", Repository: "sf/code", IssueNumber: 41}})
	r.openCases = map[string][]factoryd.Case{
		"t-e": {{ID: 7, TaskID: "t-e", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman}},
		"t-h": {{ID: 8, TaskID: "t-h", Kind: factoryd.CaseHumanCard, Status: factoryd.CaseAtHuman,
			Facts: map[string]any{"ask": "Which store should the build read?", factoryd.FactAbout: "t-r"}}},
		"t-p": {{ID: 9, TaskID: "t-p", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman}},
	}
	face := func(ask string) factoryd.CaseAsk {
		return factoryd.CaseAsk{At: time.Now(), Detail: map[string]any{"ask": ask, "story": map[string]any{}}}
	}
	r.caseAsks = map[int64]factoryd.CaseAsk{7: face("Use a replica?"), 9: face("Do the proofs hold?")}
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	escalate := func(card string) (string, error) {
		return trn.escalate(toolInput{Ask: "Which store?", Why: "the ledger is short", Info: "the pool is dry", Recommendation: "Postgres",
			Choices: []string{"Postgres", "SQLite"}, TaskID: card})
	}
	for _, in := range []struct{ name, card, want string }{
		{"the card's own ask", "t-e", "#13 already asks the human “Use a replica?” in Needs you: a second card would ask twice. " +
			"Tell the human to answer it there, or, if your question is another one, watch #13 with `watch_card` and ask once it is answered."},
		{"a helper's ask on the card", "t-m", "#5 already asks the human “Do the proofs hold?” in Needs you: a second card would ask twice. " +
			"Tell the human to answer it there, or, if your question is another one, watch #5 with `watch_card` and ask once it is answered."},
		{"the chat's ask about the card", "t-r", "#40 already asks the human “Which store should the build read?” in Needs you, about #11: a second card would ask twice. " +
			"Tell the human to answer it there, or, if your question is another one, watch #40 with `watch_card` and ask once it is answered."},
	} {
		_, err := escalate(in.card)
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != in.want {
			t.Errorf("%s: escalate = %v, want 409 %q", in.name, err, in.want)
		}
	}
	equal(t, "the refusals file nothing", about, []string(nil))
	r.openCases["t-e"][0].Status = factoryd.CaseOpen
	got, err := escalate("t-e")
	equal(t, "an ask on its rungs, beside the chat's ask about another card", []any{got, err, about},
		[]any{"Filed the escalation as #42 in Needs you; the inbox carries it to the project's humans with its choices.", error(nil), []string{"t-e"}})
}

// An answer that approves a merge needs an approver, as the board's
// button and the chat's tool do: a writer on the notify list who presses
// Approve on the merge case's face hears the refusal, and the request
// stays pending.
func TestChatAnswerNeedsAnApprover(t *testing.T) {
	r, _ := newReadsRig(t)
	r.ledger.approvers = []string{"alice"}
	r.approvals.rows = append(r.approvals.rows,
		factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-e", Project: "sf/code", PRNumber: 9, Status: "pending",
			Spec: json.RawMessage(`{"risk_class":"low","head_sha":"abc"}`)})
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := r.chat.EnsureInbox(ctx, "sf/code", "sebastian")
	const question = "Merge PR #9, low risk."
	if _, err := r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-e", IssueNumber: 13, Kind: factoryd.EventQuestionAsked,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"question": question, "case_kind": string(factoryd.CaseMergeApproval)}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct{ task, text, choice string }{
		{"t-e", "#13 needs you. The factory's rules let only a human merge this change.", "Merge it"},
	} {
		if _, err := r.chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
			Content: e.text, Choices: []string{e.choice}, TaskID: e.task, Conversation: inbox, BirthKind: factoryd.EventQuestionAsked}); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
			"message": {strconv.FormatInt(r.chat.next, 10)}, "choice": {e.choice}}); code != http.StatusNoContent {
			t.Fatalf("the answer = %d %q", code, body)
		}
	}
	rows, _ := r.chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 50)
	var notes []string
	for _, m := range rows {
		if m.Kind == factoryd.MessagePoke {
			notes = append(notes, m.Content)
		}
	}
	equal(t, "the notes", notes, []string{
		"The answer did not land: Only the approvers of project ai-software-factory-code may authorize spend.",
	})
	equal(t, "the request", r.approvals.rows[len(r.approvals.rows)-1].Status, "pending")
}

// The answer row of a question factoryd withdrew answers nothing (roadmap
// 246): card #13 waits on a later question, and a press on the withdrawn
// one's row says it needs no answer and posts no comment, where the card
// waiting on a human again would take it as the later question's answer.
func TestChatAnswerRowOfAWithdrawnQuestion(t *testing.T) {
	r, chat, _ := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#13 needs you: Use a replica?", Choices: []string{"Yes", "No"}, TaskID: "t-e", Conversation: inbox}); err != nil {
		t.Fatal(err)
	}
	rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	asked := rows[len(rows)-1]
	if _, err := r.events.Record(ctx, factoryd.Event{At: asked.At.Add(time.Minute), Repository: "sf/code", TaskID: "t-e", IssueNumber: 13,
		Kind: factoryd.EventQuestionWithdrawn, Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd,
		Detail: map[string]any{"question": "Use a replica?", "comment": 601, "reason": "the checks failed"}}); err != nil {
		t.Fatal(err)
	}
	code, _, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(asked.ID, 10)}, "choice": {"Yes"}})
	if code != http.StatusNoContent {
		t.Fatalf("the answer = %d", code)
	}
	if posted := r.fg.commentBodies("/api/v1/repos/sf/code/issues/13/comments"); len(posted) != 0 {
		t.Fatalf("posted %q on #13, want nothing", posted)
	}
	rows, _ = chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	if last := rows[len(rows)-1]; last.Content != "#13 needs no answer: its question is withdrawn because the checks failed." {
		t.Fatalf("the factory's row = %q", last.Content)
	}
}

// An answer while a turn runs in its thread waits behind the turn as a
// second message does (roadmap 263): its rows are stored queued at once,
// the turn's retry after a forced compaction does not read them as its
// own, and they land after the turn's rows when it ends.
func TestChatAnswerWaitsBehindTheTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	seedTurns(t, chat, "a", "b")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#11 needs you: rebase?", Choices: []string{"Yes", "No"}, TaskID: "t-r", Conversation: 1}); err != nil {
		t.Fatal(err)
	}
	escalation := strconv.FormatInt(chat.next, 10)
	said := func() []string {
		t.Helper()
		rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", 1, 50)
		var out []string
		for _, m := range rows {
			out = append(out, fmt.Sprintf("%s %s: %s (queued %t)", m.Role, m.Kind, m.Content, m.Queued))
		}
		return out
	}

	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 4)
	g.replies = [][]string{{say("Let me look."), fetchCall(1, "board", `{}`)}, {refuseContext}, {say("done")}}
	turn := make(chan error, 1)
	go func() {
		_, _, _, err := postStream(r, ck, s, chatPath, url.Values{"conversation": {"1"}, "message": {"c1"}})
		turn <- err
	}()
	<-g.arrived
	code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {"1"}, "message": {escalation}, "choice": {"Yes"}})
	during := said()
	close(g.hold)
	if err := <-turn; err != nil {
		t.Fatal(err)
	}
	equal(t, "the answer", code, http.StatusNoContent)
	equal(t, "the thread while the turn runs "+body[0], during, []string{
		"user : a1 (queued false)", "assistant : a2 (queued false)", "user : b1 (queued false)", "assistant : b2 (queued false)",
		"assistant escalation: #11 needs you: rebase? (queued false)",
		"user : c1 (queued false)",
		"user : Yes (queued true)",
		"assistant poke: #11 waits no more: it was answered or moved on elsewhere. (queued true)",
	})
	calls := g.calls()
	if len(calls) != 3 {
		t.Fatalf("%d model calls, want 3", len(calls))
	}
	equal(t, "the retry", contents(t, calls[2]), []string{"user b1", "assistant " + earlier("b2"), "assistant " + earlier("#11 needs you: rebase?"),
		"user c1", "assistant Let me look.", "tool " + schemaResult(t, r, "ai-software-factory-code", "board"), "tool " + boardText})
	waitUntil(t, "the answer's rows landed", func() bool { return !strings.Contains(strings.Join(said(), "\n"), "queued true") })
	equal(t, "the thread", said()[5:], []string{
		"user : c1 (queued false)",
		"assistant : Let me look. (queued false)",
		"assistant : done (queued false)",
		"user : Yes (queued false)",
		"assistant poke: #11 waits no more: it was answered or moved on elsewhere. (queued false)",
	})
	if r.c.turns.working(threadKey("sf/code", "sebastian", 1)) {
		t.Fatal("the thread still shows a writer in its line after the answer landed")
	}
}

// answerPress is a press through /chat/answer in one of sebastian's
// threads: the thread's number, the press's status and stream, the
// store's TakeProposal calls, the store and the gateway.
type answerPress struct {
	conv   int64
	code   int
	events []string
	takes  int
	chat   *fakeChat
	g      *chatGateway
}

// pressAnswer seeds m in sebastian's inbox, or with inbox false in his
// own thread 1, and presses choice on it through /chat/answer, the model
// answering reply.
func pressAnswer(t *testing.T, inbox bool, m factoryd.ChatMessage, choice, reply string) answerPress {
	t.Helper()
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	m.Conversation = 1
	if inbox {
		var err error
		if m.Conversation, err = chat.EnsureInbox(t.Context(), "sf/code", "sebastian"); err != nil {
			t.Fatal(err)
		}
	}
	id, err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", m)
	if err != nil {
		t.Fatal(err)
	}
	g.replies = [][]string{{say(reply)}}
	code, events, _ := chatPost(t, r, ck, s, chatPath+"/answer",
		url.Values{"conversation": {strconv.FormatInt(m.Conversation, 10)}, "message": {strconv.FormatInt(id, 10)}, "choice": {choice}})
	chat.mu.Lock()
	defer chat.mu.Unlock()
	return answerPress{conv: m.Conversation, code: code, events: events, takes: chat.takes, chat: chat, g: g}
}

// An ask that binds nothing answers through /chat/answer in the inbox,
// which takes presses only, and in a thread of the human's own alike: the
// press lands the human's row with the choice, and the agent's turn reads
// it as their answer. A row with no binding asks the proposals' store
// nothing.
func TestChatAnswerPressesAnUnboundAsk(t *testing.T) {
	for _, inbox := range []bool{true, false} {
		p := pressAnswer(t, inbox, factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageAsk,
			Content: "One snapshot a night, or one an hour?", Choices: []string{"One a night", "One an hour"}}, "One a night", "One a night it is.")
		where := fmt.Sprint("thread ", p.conv)
		equal(t, "the press in "+where, []any{p.code, p.events, p.takes}, []any{http.StatusOK, []string{`text {"text":"One a night it is."}`, `done {}`}, 0})
		equal(t, where, rowsOf(t, p.chat, "sebastian", p.conv),
			[]string{"assistant: One snapshot a night, or one an hour?", "user: One a night", "assistant: One a night it is."})
		if got := messages(t, p.g.calls()[0]); got[len(got)-1]["content"] != "One a night" {
			t.Fatalf("the model read %+v in %s, want the choice", got[len(got)-1], where)
		}
	}
}

// A poke's choices answer through /chat/answer as an ask's do: a press
// lands the human's row and the agent's turn reads it, the proposals'
// store asked nothing; a choice the poke does not offer is refused,
// nothing stored.
func TestChatAnswerPressesAPokeChoiceInTheInbox(t *testing.T) {
	poke := failedPoke("t-r", 11)
	p := pressAnswer(t, true, poke, "Try the failed card again", "I filed it again.")
	equal(t, "the press", []any{p.code, p.events, p.takes}, []any{http.StatusOK, []string{`text {"text":"I filed it again."}`, `done {}`}, 0})
	equal(t, "the inbox", rowsOf(t, p.chat, "sebastian", p.conv),
		[]string{"assistant: #11 failed: the checks failed on c1.", "user: Try the failed card again", "assistant: I filed it again."})
	if got := messages(t, p.g.calls()[0]); got[len(got)-1]["content"] != "Try the failed card again" {
		t.Fatalf("the model read %+v, want the choice", got[len(got)-1])
	}

	p = pressAnswer(t, true, poke, "Maybe", "")
	equal(t, "a choice the poke does not offer", []any{p.code, p.events}, []any{http.StatusBadRequest, []string{"the choice is none of the poke's: press one of its buttons."}})
	equal(t, "the inbox", rowsOf(t, p.chat, "sebastian", p.conv), []string{"assistant: #11 failed: the checks failed on c1."})
	if n := len(p.g.calls()); n != 0 {
		t.Fatalf("%d model calls, want none", n)
	}
}

// A factory pull request card's red checks run again from the chat as from
// its card (roadmap 264): the inbox's "Run the failed checks again" and
// the rerun_checks tool signal the workflow as the member.
func TestChatRerunsAFactoryPullRequestsChecks(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","factory":true,"red_checks":true}`)
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "#5 (Fix the cache. It drops entries.) needs you. The change is of normal risk, and the factory's rules let only a human merge it.",
		Choices: []string{"Run the failed checks again"}, TaskID: "t-m", Conversation: inbox, BirthKind: factoryd.EventApprovalRequested}); err != nil {
		t.Fatal(err)
	}
	r.fg.calls = nil
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(chat.next, 10)}, "choice": {"Run the failed checks again"}}); code != http.StatusNoContent {
		t.Fatalf("the answer = %d %q", code, body)
	}
	rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	equal(t, "the inbox's poke", rows[len(rows)-1].Content, "Running the checks of pull request #9 again; the card asks again once they settle.")

	g.replies = [][]string{{fetchCall(1, "rerun_checks", `{"task_id":"t-m"}`)}, {say("They run again.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"run #5's checks again"}})
	// the card's read of the head's current checks lands between: the tool describes the
	// card before it acts. The tool runs unconfirmed, so the agent decided
	// the re-run: factory-chat's, the inbox's answer the member's
	equal(t, "the calls", r.fg.calls, []string{"SIGNAL rerun_checks t-m #9 by sebastian",
		"GET /api/v1/repos/sf/code/commits/abc123/status Bearer access-1", "SIGNAL rerun_checks t-m #9 by factory-chat"})
}

// A chat turn's board read carries a red-checks merge request's healed
// head (#1013) read as the chat's own actor: the turn that opens the
// conversation reads the head's current checks as the member who chats,
// the one forge call the turn makes.
func TestChatTurnReadsAHealedHeadAsItsOwnActor(t *testing.T) {
	r, _, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	r.approvals.mu.Lock()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","red_checks":true,"why_human":["session cap reached"]}`)
	r.approvals.mu.Unlock()
	r.fg.statuses = map[string]string{"abc123": `{"state":"success","statuses":[{"context":"ci / test","status":"success"}]}`}
	r.fg.calls = nil
	g.replies = [][]string{{say("Its checks are green again.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"is #5 still red?"}})
	equal(t, "the calls", r.fg.calls, []string{"GET /api/v1/repos/sf/code/commits/abc123/status Bearer access-1"})
}

// The chat renders each poke row's derived state (the
// notification-settling concept): a settled ask carries its reason line,
// an inform recedes once the thread was read, an open ask stays bare, and
// the switcher's badge splits into the asks that need the member and the
// rows that are new.
func TestChatSettlesRows(t *testing.T) {
	r, chat, _ := newChatRig(t)
	ck, _ := r.loggedIn(t)
	ctx := t.Context()
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-w", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 15, Objective: "Which port?"}})
	r.tasks.mu.Unlock()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	say := func(m factoryd.ChatMessage) {
		t.Helper()
		if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", m); err != nil {
			t.Fatal(err)
		}
	}
	say(factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#13 needs you: Use a replica?",
		Choices: []string{"Yes", "No"}, TaskID: "t-e", Conversation: inbox})
	say(factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#11 needs you: rebase?",
		Choices: []string{"Yes", "No"}, TaskID: "t-r", Conversation: inbox})
	say(factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessagePoke, Content: "#11 started.", TaskID: "t-r", Conversation: inbox})
	say(factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#15 needs you: Which port?",
		Choices: []string{"8080", "80"}, TaskID: "t-w", Conversation: inbox})
	for _, e := range []factoryd.Event{
		{Repository: "sf/code", TaskID: "t-r", Kind: factoryd.EventCardRetried, Actor: "the agent", Via: factoryd.ViaFactoryd, Detail: map[string]any{"replacement": 9}},
		{Repository: "sf/code", TaskID: "t-e", Kind: factoryd.EventQuestionAnswered, Actor: "sebastian", Via: factoryd.ViaButton},
	} {
		if _, err := r.events.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	list := func() factoryd.ChatConversation {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, chatPath+"/conversations", nil), ck)
		var out []factoryd.ChatConversation
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(rec.Code, err)
		}
		i := slices.IndexFunc(out, func(th factoryd.ChatConversation) bool { return th.Inbox })
		if i < 0 {
			t.Fatalf("the list = %+v, want the inbox", out)
		}
		return out[i]
	}
	equal(t, "the unread inbox's split", []int64{list().NeedsYou, list().NewRows, list().Unread}, []int64{1, 3, 4})
	read := func() []chatRow {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation="+strconv.FormatInt(inbox, 10), nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		var answer chatGetAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer.Messages
	}
	msgs := read()
	equal(t, "the settled asks and the open one", []bool{msgs[0].Settled, msgs[1].Settled, msgs[2].Settled, msgs[3].Settled},
		[]bool{true, true, false, false})
	for i, want := range []string{"answered by sebastian", "moved on", "", ""} {
		if got := msgs[i].Reason; got != want || (want != "") == msgs[i].SettledAt.IsZero() {
			t.Errorf("row %d's reason = %q at %s, want %q at a time", i, got, msgs[i].SettledAt, want)
		}
	}
	equal(t, "the inbox's split after the read", []int64{list().NeedsYou, list().NewRows, list().Unread}, []int64{1, 0, 0})
	msgs = read()
	equal(t, "the read inform recedes with its word, the open ask stays bare",
		[]bool{msgs[2].Settled, msgs[2].Reason == "seen", msgs[3].Settled, msgs[3].Reason == ""}, []bool{true, true, false, true})
}

// Clear old (the chat's sweep): the marker hides the thread's old rows
// from the human's view, the record keeps them, and an open ask or a
// poke whose buttons still apply stays whatever the marker says, so
// nothing that needs the human disappears; once its card moved on, the
// poke goes with the rest.
func TestChatClearsOldRows(t *testing.T) {
	r, chat, _ := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	r.tasks.mu.Lock()
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-w", Kind: "agent", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 15, Objective: "Which port?"}},
		factoryd.BoardTask{Task: factoryd.Task{ID: "t-f", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 21}})
	r.tasks.mu.Unlock()
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	say := func(m factoryd.ChatMessage) {
		t.Helper()
		if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", m); err != nil {
			t.Fatal(err)
		}
	}
	say(factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#14 needs you: rebase?",
		Choices: []string{"Yes", "No"}, TaskID: "t-r", Conversation: inbox})
	say(factoryd.ChatMessage{Role: "assistant", Content: "#15 closed.", Conversation: inbox})
	say(factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation, Content: "#15 needs you: Which port?",
		Choices: []string{"8080", "80"}, TaskID: "t-w", Conversation: inbox})
	failed := failedPoke("t-f", 21)
	failed.Conversation = inbox
	say(failed)
	if _, err := r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-r", Kind: factoryd.EventQuestionAnswered,
		Actor: "sebastian", Via: factoryd.ViaButton}); err != nil {
		t.Fatal(err)
	}
	read := func() chatGetAnswer {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation="+strconv.FormatInt(inbox, 10), nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		var answer chatGetAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	answer := read()
	answer = read() // the seen marker passes the plain row, it settles too
	equal(t, "before the sweep", []int{len(answer.Messages), int(answer.Thread.Cleared), int(answer.Thread.Unswept)}, []int{4, 0, 4})
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/clear", url.Values{"conversation": {strconv.FormatInt(inbox, 10)}}); code != http.StatusNoContent {
		t.Fatalf("POST clear = %d: %s", code, body)
	}
	answer = read()
	equal(t, "the sweep keeps the open ask and the open choices", []int{len(answer.Messages), int(answer.Thread.Cleared), int(answer.Thread.Unswept)}, []int{2, 2, 0})
	equal(t, "the kept rows", []any{answer.Messages[0].TaskID, answer.Messages[0].Settled, answer.Messages[1].TaskID, answer.Messages[1].Settled, answer.Messages[1].Choices},
		[]any{"t-w", false, "t-f", false, []string{"Try the failed card again", "Not now"}})
	if _, err := r.events.Record(ctx, factoryd.Event{Repository: "sf/code", TaskID: "t-f", Kind: factoryd.EventCardRetried,
		Actor: "sebastian", Via: factoryd.ViaButton, Detail: map[string]any{"replacement": 22}}); err != nil {
		t.Fatal(err)
	}
	answer = read()
	equal(t, "the retried card's poke goes with the sweep", []int{len(answer.Messages), int(answer.Thread.Cleared), int(answer.Thread.Unswept)}, []int{1, 3, 0})
	say(factoryd.ChatMessage{Role: "assistant", Content: "#16 merged.", Conversation: inbox})
	answer = read()
	equal(t, "a row after the sweep is the next sweep's", []int{len(answer.Messages), int(answer.Thread.Unswept)}, []int{2, 1})
}

// Clear old past the compaction fold: the sweep's read answers no
// summary, so the chat shows no summary card over rows the member swept;
// the stored thread keeps its summary and the turns still read it.
func TestChatClearPastTheSummary(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	seedTurns(t, chat, "a", "b")
	if err := chat.SaveSummary(ctx, "sf/code", "sebastian", 1, factoryd.ThreadSummary{Text: "the cache was fixed", Upto: 2}); err != nil {
		t.Fatal(err)
	}
	read := func() chatGetAnswer {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, chatPath+"?conversation=1", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		var answer chatGetAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer
	}
	answer := read()
	equal(t, "before the sweep", []any{len(answer.Messages), answer.Thread.Summary, answer.Thread.SummaryUpto},
		[]any{4, "the cache was fixed", int64(2)})
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/clear", url.Values{"conversation": {"1"}}); code != http.StatusNoContent {
		t.Fatalf("POST clear = %d: %s", code, body)
	}
	answer = read()
	equal(t, "the sweep past the fold answers no summary", []any{len(answer.Messages), int(answer.Thread.Cleared), answer.Thread.Summary, answer.Thread.SummaryUpto},
		[]any{0, 4, "", int64(0)})
	th, ok, err := chat.Conversation(ctx, "sf/code", "sebastian", 1)
	if err != nil || !ok {
		t.Fatalf("the thread: %v, %v", ok, err)
	}
	equal(t, "the stored thread keeps its summary", []any{th.Summary, th.SummaryUpto, th.Compactions},
		[]any{"the cache was fixed", int64(2), 1})
	g.replies = [][]string{{say("done")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"c1"}})
	if !strings.HasSuffix(systemOf(t, g.calls()[0]), "This thread before its recent turns, summarised:\nthe cache was fixed") {
		t.Errorf("the turn after the sweep reads no summary: %q", systemOf(t, g.calls()[0]))
	}
}
