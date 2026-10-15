package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// seedTurns stores turns in sebastian's thread 1, each a member's line and
// the agent's answer: a1 a2, b1 b2, …
func seedTurns(t *testing.T, chat *fakeChat, names ...string) {
	t.Helper()
	for _, n := range names {
		for _, m := range []factoryd.ChatMessage{{Role: "user", Content: n + "1"}, {Role: "assistant", Content: n + "2"}} {
			m.Conversation = 1
			if err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", m); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// contents are a model call's messages after the system prompt, as
// "role content".
func contents(t *testing.T, call map[string]any) []string {
	t.Helper()
	var out []string
	for _, m := range messages(t, call)[1:] {
		out = append(out, m["role"].(string)+" "+m["content"].(string))
	}
	return out
}

// systemOf is a model call's system prompt.
func systemOf(t *testing.T, call map[string]any) string {
	t.Helper()
	return messages(t, call)[0]["content"].(string)
}

// compactedRows is each compacted event's folded row count, in order.
func compactedRows(t *testing.T, events []string) []int {
	t.Helper()
	var out []int
	for _, e := range events {
		data, ok := strings.CutPrefix(e, "compacted ")
		if !ok {
			continue
		}
		var c struct{ Messages, Before, After int }
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		if c.After >= c.Before {
			t.Errorf("a compaction kept %d of %d estimated tokens: want fewer", c.After, c.Before)
		}
		out = append(out, c.Messages)
	}
	return out
}

// The forced trigger: the provider refuses the prompt past its window, the
// turn compacts and retries with the summary and its last two turns; a
// second refusal compacts again keeping half the tail, the newest turn;
// the third answer goes through (roadmap 239).
func TestChatCompactsOnTheContextRefusal(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	seedTurns(t, chat, "a", "b", "c")
	g.replies = [][]string{{refuseContext}, {refuseContext}, {say("the answer")}}
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"d1"}})
	equal(t, "the code", code, 200)
	equal(t, "the folded rows", compactedRows(t, events), []int{4, 2})
	equal(t, "the stream's end", events[len(events)-2:], []string{`text {"text":"the answer"}`, `done {}`})
	calls := g.calls()
	equal(t, "the first call", contents(t, calls[0]), []string{"user a1", "assistant " + earlier("a2"), "user b1", "assistant " + earlier("b2"),
		"user c1", "assistant " + earlier("c2"), "user d1"})
	equal(t, "the first retry", contents(t, calls[1]), []string{"user c1", "assistant " + earlier("c2"), "user d1"})
	equal(t, "the second retry", contents(t, calls[2]), []string{"user d1"})
	if !strings.HasSuffix(systemOf(t, calls[1]), "summarised:\nsummary 1") || !strings.HasSuffix(systemOf(t, calls[2]), "summarised:\nsummary 2") {
		t.Errorf("the retries' system prompts do not end with their summaries")
	}
	first, _ := messages(t, g.compacts[0])[1]["content"].(string)
	second, _ := messages(t, g.compacts[1])[1]["content"].(string)
	if !strings.HasPrefix(first, "The summary so far:\n(none yet)\n\nThe turns after it:\nthe member: a1\nthe agent: a2\nthe member: b1\nthe agent: b2\n") {
		t.Errorf("the first compaction read %q", first)
	}
	if !strings.HasPrefix(second, "The summary so far:\nsummary 1\n\nThe turns after it:\nthe member: c1\nthe agent: c2\n") {
		t.Errorf("the second compaction read %q", second)
	}
	th, _, _ := chat.Conversation(t.Context(), "sf/code", "sebastian", 1)
	equal(t, "the thread's summary", []any{th.Summary, th.SummaryUpto, th.Compactions}, []any{"summary 2", int64(6), 2})
}

// A third refusal after two compactions ends the turn with the named
// error.
func TestChatContextRefusedThriceEndsTheTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	seedTurns(t, chat, "a", "b", "c")
	g.replies = [][]string{{refuseContext}, {refuseContext}, {refuseContext}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"d1"}})
	equal(t, "the folded rows", compactedRows(t, events), []int{4, 2})
	equal(t, "the stream's end", events[len(events)-1],
		`error {"message":"The thread is past the model's context window even after compacting it (refused_context): start a new thread to go on."}`)
	equal(t, "the model calls", len(g.calls()), 3)
}

// The preset trigger: a turn projected past 80 percent of the window
// compacts before its call; at 80 percent exactly it does not.
func TestChatCompactsBeforeAProjectedOverflow(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.chat = &policy.ChatConfig{Window: 10000}
	ck, s := r.loggedIn(t)
	seedTurns(t, chat, "a", "b", "c")
	thread := "sf/code:sebastian:1"
	// 7999 tokens and the message's one: 8000, 80 percent exactly
	chat.tokens = map[string]int{thread: 7999}
	g.replies = [][]string{{say("d2")}, {say("e2")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"d1"}})
	equal(t, "the compactions at 80 percent", []any{compactedRows(t, events), len(g.compacts)}, []any{[]int(nil), 0})
	// 8000 and one: past it
	chat.tokens[thread] = 8000
	_, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"e1"}})
	equal(t, "the compaction past 80 percent", compactedRows(t, events), []int{6})
	equal(t, "the call after it", contents(t, g.calls()[1]), []string{"user d1", "assistant " + earlier("d2"), "user e1"})
}

// /compact by hand folds the thread before its last two turns and calls
// no model for an answer; with nothing before them it says so. The thread's
// read carries the summary.
func TestChatCompactByHand(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	seedTurns(t, chat, "a", "b", "c")
	g.summaries = []string{"the cache was fixed"}
	code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"/compact"}})
	equal(t, "the stream", []any{code, compactedRows(t, events), events[len(events)-1]}, []any{200, []int{2}, `done {}`})
	equal(t, "the model calls for an answer", len(g.calls()), 0)
	_, events, _ = chatPost(t, r, ck, s, chatPath, url.Values{"message": {"/compact"}})
	equal(t, "the second", events, []string{`text {"text":"Nothing to compact: the thread holds no turns before its last two."}`, `done {}`})
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 100)
	equal(t, "the thread's rows", len(rows), 6)
	rec := r.do(httptest.NewRequest(http.MethodGet, chatPath, nil), ck)
	var got chatGetAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	equal(t, "the thread's read", []any{got.Thread.Summary, got.Thread.SummaryUpto, got.Thread.Compactions}, []any{"the cache was fixed", int64(2), 1})
}

// The summariser reads a write's result and a read's call alone, so the
// summary carries no old read as the project now.
func TestChatCompactionLeavesReadsOut(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	for _, m := range []factoryd.ChatMessage{{Role: "user", Content: "a1"},
		{Role: "assistant", ToolCalls: []factoryd.ChatToolCall{
			{ID: "call_1", Name: "board", Arguments: `{}`, Result: "Running (1):\n  #11 t-r"},
			{ID: "call_2", Name: "set_accent", Arguments: `{"task_id":"t-m","accent":"red"}`, Result: "The accent of #5 is set."}}},
		{Role: "assistant", Content: "a2"}} {
		m.Conversation = 1
		if err := chat.AddChatMessage(t.Context(), "sf/code", "sebastian", m); err != nil {
			t.Fatal(err)
		}
	}
	seedTurns(t, chat, "b", "c")
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"/compact"}})
	if len(g.compacts) != 1 {
		t.Fatalf("%d compaction calls, want 1", len(g.compacts))
	}
	equal(t, "the summariser's turns", messages(t, g.compacts[0])[1]["content"], "The summary so far:\n(none yet)\n\nThe turns after it:\n"+
		"the member: a1\nthe agent read board {}\n"+`the agent called set_accent {"task_id":"t-m","accent":"red"}: The accent of #5 is set.`+"\nthe agent: a2\n")
}

// A forced compaction weighs the rows it keeps as the context carries
// them: the turn's own board read whole, its call's name and arguments,
// four characters a token.
func TestChatCompactionWeighsTheTurnsReads(t *testing.T) {
	r, chat, g := newChatRig(t)
	ck, s := r.loggedIn(t)
	seedTurns(t, chat, "a", "b")
	g.replies = [][]string{{call(1, "board", `{}`)}, {refuseContext}, {refuseContext}, {say("done")}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"c1"}})
	equal(t, "the folded rows", compactedRows(t, events), []int{2, 2})
	var last struct{ After int }
	for _, e := range events {
		if data, ok := strings.CutPrefix(e, "compacted "); ok {
			if err := json.Unmarshal([]byte(data), &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	// the second summary, the member's c1, the call's board and {}, and
	// its result
	equal(t, "the kept rows' estimate", last.After, (len("summary 2"+"c1"+"board"+"{}"+boardText)+3)/4)
}

// A command of the config tree's sends its message: the turn stores and
// sends the message, not the name; any other /word is text (roadmap 239).
func TestChatCommandSendsItsMessage(t *testing.T) {
	r, chat, g := newChatRig(t)
	r.ledger.commands = policy.Commands{Commands: []policy.Command{{Name: "standup", Words: "what moved", Message: "What moved since yesterday?"}}}
	ck, s := r.loggedIn(t)
	g.replies = [][]string{{say("Two cards.")}, {say("No such command.")}}
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"/standup"}})
	chatPost(t, r, ck, s, chatPath, url.Values{"message": {"/standupx"}})
	equal(t, "the first call", contents(t, g.calls()[0]), []string{"user What moved since yesterday?"})
	rows, _ := chat.ChatMessages(t.Context(), "sf/code", "sebastian", 1, 10)
	var said []string
	for _, m := range rows {
		said = append(said, m.Role+" "+m.Content)
	}
	equal(t, "the thread", said, []string{"user What moved since yesterday?", "assistant Two cards.", "user /standupx", "assistant No such command."})
}
