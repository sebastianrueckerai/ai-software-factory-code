package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// An answer that approves a merge or accepts a plan needs an approver, as
// the board's buttons and the chat's tools do: a writer on the notify
// list hears the refusal, and both requests stay pending.
func TestChatAnswerNeedsAnApprover(t *testing.T) {
	r, _ := newReadsRig(t)
	r.ledger.approvers = []string{"alice"}
	r.approvals.rows = append(r.approvals.rows,
		factoryd.ApprovalRequest{ID: 6, Kind: factoryd.ApprovalMerge, TaskID: "t-e", Project: "sf/code", PRNumber: 9, Status: "pending",
			Spec: json.RawMessage(`{"risk_class":"low","head_sha":"abc"}`)},
		factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: "t-p", Project: "sf/code", Status: "pending",
			Spec: json.RawMessage(`{"revision":1,"sha256":"5e1f"}`)})
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	inbox, _ := r.chat.EnsureInbox(ctx, "sf/code", "sebastian")
	for _, e := range []struct{ task, text, choice string }{
		{"t-e", "Merge PR #9 of #13 waits for you: risk class low.", "Approve"},
		{"t-p", "Plan #20 waits for your acceptance: revision 1, 3 cards.", "Accept"},
	} {
		if _, err := r.chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
			Content: e.text, Choices: []string{e.choice}, TaskID: e.task, Conversation: inbox, BirthKind: factoryd.EventApprovalRequested}); err != nil {
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
		"The answer did not land: Only the approvers of project ai-software-factory-code may accept a plan.",
	})
	equal(t, "the requests", []string{r.approvals.rows[len(r.approvals.rows)-2].Status, r.approvals.rows[len(r.approvals.rows)-1].Status}, []string{"pending", "pending"})
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
	g.replies = [][]string{{say("Let me look."), call(1, "board", `{}`)}, {refuseContext}, {say("done")}}
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
		"user c1", "assistant Let me look.", "tool " + boardText})
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

// A factory pull request card's red checks run again from the chat as from
// its card (roadmap 264): the inbox's Re-run the checks and the
// rerun_checks tool signal the workflow as the member.
func TestChatRerunsAFactoryPullRequestsChecks(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	ctx := t.Context()
	r.approvals.rows[1].Spec = json.RawMessage(`{"risk_class":"standard","head_sha":"abc123","factory":true,"red_checks":true}`)
	inbox, _ := chat.EnsureInbox(ctx, "sf/code", "sebastian")
	if _, err := chat.AddChatMessage(ctx, "sf/code", "sebastian", factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageEscalation,
		Content: "PR #9 of #5 waits for you: its checks are red, so it cannot merge; run them again, or close it with your words.",
		Choices: []string{factoryd.ChoiceRerun}, TaskID: "t-m", Conversation: inbox, BirthKind: factoryd.EventApprovalRequested}); err != nil {
		t.Fatal(err)
	}
	r.fg.calls = nil
	if code, body, _ := chatPost(t, r, ck, s, chatPath+"/answer", url.Values{"conversation": {strconv.FormatInt(inbox, 10)},
		"message": {strconv.FormatInt(chat.next, 10)}, "choice": {factoryd.ChoiceRerun}}); code != http.StatusNoContent {
		t.Fatalf("the answer = %d %q", code, body)
	}
	rows, _ := chat.ChatMessages(ctx, "sf/code", "sebastian", inbox, 10)
	equal(t, "the inbox's poke", rows[len(rows)-1].Content, "Running the checks of pull request #9 again; the card asks again once they settle.")

	g.replies = [][]string{{call(1, "rerun_checks", `{"task_id":"t-m"}`)}, {say("They run again.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"run #5's checks again"}})
	equal(t, "the calls", r.fg.calls, []string{"SIGNAL rerun_checks t-m #9 by sebastian", "SIGNAL rerun_checks t-m #9 by sebastian"})
}

// The settle rule's table (the notification-settling concept, section 3):
// the first settling event at or after an ask row settles it with its
// word, actor and time, the wait guard settles it as moved on, a newer
// request supersedes an approval but not a question, and a repeat's
// bumped clock sets a re-asked row open again.
func TestSettleOf(t *testing.T) {
	at := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	ask := factoryd.ChatMessage{ID: 10, At: at, Kind: factoryd.MessageEscalation, TaskID: "t-e", Content: "#13 needs you: Use a replica?"}
	approval := factoryd.ChatMessage{ID: 10, At: at, Kind: factoryd.MessageEscalation, TaskID: "t-m", BirthKind: factoryd.EventApprovalRequested}
	ev := func(id int64, min int, kind, actor string) factoryd.Event {
		return factoryd.Event{ID: id, At: at.Add(time.Duration(min) * time.Minute), Kind: kind, Actor: actor}
	}
	open := pokeSettle{Open: true}
	for _, tt := range []struct {
		name                 string
		m                    factoryd.ChatMessage
		events               []factoryd.Event
		cardWaits, apprWaits bool
		want                 pokeSettle
	}{
		{"an open question", ask, nil, true, false, open},
		{"a newer request leaves the question open", ask, []factoryd.Event{ev(12, 1, factoryd.EventApprovalRequested, factoryd.FactoryDecider)}, true, true, open},
		{"an answered question", ask, []factoryd.Event{ev(12, 1, factoryd.EventQuestionAnswered, "sebastian")}, true, false,
			pokeSettle{Word: "answered", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventQuestionAnswered, "sebastian")}},
		{"an answer before the row settles nothing", ask, []factoryd.Event{ev(5, -1, factoryd.EventQuestionAnswered, "bob")}, true, false, open},
		{"an answer on the row's own minute settles", ask, []factoryd.Event{ev(7, 0, factoryd.EventQuestionAnswered, "bob")}, false, false,
			pokeSettle{Word: "answered", By: "bob", At: at, Event: ev(7, 0, factoryd.EventQuestionAnswered, "bob")}},
		{"a withdrawn question", ask, []factoryd.Event{ev(12, 1, factoryd.EventQuestionWithdrawn, factoryd.FactoryDecider)}, false, false,
			pokeSettle{Word: "withdrawn", By: factoryd.FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventQuestionWithdrawn, factoryd.FactoryDecider)}},
		{"an answer on another venue", ask, []factoryd.Event{ev(12, 1, factoryd.EventCardClosed, "sebastian")}, false, false,
			pokeSettle{Word: "answered", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventCardClosed, "sebastian")}},
		{"a granted session", ask, []factoryd.Event{ev(12, 1, factoryd.EventSessionGranted, factoryd.FactoryDecider)}, false, false,
			pokeSettle{Word: "answered", By: factoryd.FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventSessionGranted, factoryd.FactoryDecider)}},
		{"a task ended", ask, []factoryd.Event{ev(12, 1, factoryd.EventTaskEnded, factoryd.FactoryDecider)}, false, false,
			pokeSettle{Word: "moved on", By: factoryd.FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventTaskEnded, factoryd.FactoryDecider)}},
		{"the wait guard moves a row on", ask, []factoryd.Event{ev(11, 1, factoryd.EventCardMoved, "the agent")}, false, false,
			pokeSettle{Word: "moved on", At: at.Add(time.Minute)}},
		{"the wait guard with no event at all", ask, nil, false, false, pokeSettle{Word: "moved on", At: at}},
		{"the first settling event wins", ask, []factoryd.Event{ev(14, 2, factoryd.EventMergeApproved, "sebastian"), ev(12, 1, factoryd.EventQuestionAnswered, "bob")}, false, false,
			pokeSettle{Word: "answered", By: "bob", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventQuestionAnswered, "bob")}},
		{"a tie reads the smaller id first", ask, []factoryd.Event{ev(13, 1, factoryd.EventQuestionAnswered, "bob"), ev(12, 1, factoryd.EventMergeDenied, "sebastian")}, false, false,
			pokeSettle{Word: "rejected", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventMergeDenied, "sebastian")}},
		{"a bumped row asks again", factoryd.ChatMessage{ID: 10, At: at.Add(3 * time.Minute), Kind: factoryd.MessageEscalation, TaskID: "t-e", Repeats: 1},
			[]factoryd.Event{ev(12, 1, factoryd.EventQuestionAnswered, "bob")}, true, false, open},
		{"an open approval on its pending request", approval, nil, false, true, open},
		{"an approval whose request is gone moved on", approval, nil, false, false, pokeSettle{Word: "moved on", At: at}},
		{"an approved merge", approval, []factoryd.Event{ev(12, 1, factoryd.EventMergeApproved, "sebastian")}, false, true,
			pokeSettle{Word: "approved", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventMergeApproved, "sebastian")}},
		{"a denied merge", approval, []factoryd.Event{ev(12, 1, factoryd.EventMergeDenied, "sebastian")}, false, true,
			pokeSettle{Word: "rejected", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventMergeDenied, "sebastian")}},
		{"plan changes requested", approval, []factoryd.Event{ev(12, 1, factoryd.EventPlanChangesRequested, "sebastian")}, false, true,
			pokeSettle{Word: "rejected", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventPlanChangesRequested, "sebastian")}},
		{"a superseding request", approval, []factoryd.Event{ev(12, 1, factoryd.EventApprovalRequested, factoryd.FactoryDecider)}, false, true,
			pokeSettle{Word: "superseded", By: factoryd.FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, factoryd.EventApprovalRequested, factoryd.FactoryDecider)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := settleOf(tt.m, tt.events, tt.cardWaits, tt.apprWaits)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("settleOf =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
	equal(t, "an escalation's family is a question whatever the join found", askFamily(ask), "question")
	equal(t, "an approval row's family", askFamily(approval), "approval")
	equal(t, "a poke-kind row takes its family from the event",
		askFamily(factoryd.ChatMessage{Kind: factoryd.MessagePoke, BirthKind: factoryd.EventQuestionAsked}), "question")
	equal(t, "an inform's family", askFamily(factoryd.ChatMessage{Kind: factoryd.MessagePoke, BirthKind: factoryd.EventTaskEnded}), "")
	equal(t, "a row with no birth kind is an inform", askFamily(factoryd.ChatMessage{Kind: factoryd.MessagePoke}), "")
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
		{Repository: "sf/code", TaskID: "t-r", Kind: factoryd.EventCardMoved, Actor: "the agent", Via: factoryd.ViaFactoryd, Detail: map[string]any{"lane": "done"}},
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
	read := func() []factoryd.ChatMessage {
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
	var reason = regexp.MustCompile(`^(answered by sebastian|moved on) · \d\d:\d\d$`)
	msgs := read()
	equal(t, "the settled asks and the open one", []bool{msgs[0].Settled, msgs[1].Settled, msgs[2].Settled, msgs[3].Settled},
		[]bool{true, true, false, false})
	for i, want := range []string{"answered by sebastian", "moved on", "", ""} {
		if got := msgs[i].Reason; want == "" && got != "" || want != "" && (!reason.MatchString(got) || !strings.HasPrefix(got, want)) {
			t.Errorf("row %d's reason = %q, want %q at HH:MM", i, got, want)
		}
	}
	equal(t, "the inbox's split after the read", []int64{list().NeedsYou, list().NewRows, list().Unread}, []int64{1, 0, 0})
	msgs = read()
	equal(t, "the read inform recedes with its word, the open ask stays bare",
		[]bool{msgs[2].Settled, msgs[2].Reason == "seen", msgs[3].Settled, msgs[3].Reason == ""}, []bool{true, true, false, true})
}
