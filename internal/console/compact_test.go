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
	equal(t, "the first call", contents(t, calls[0]), []string{"user a1", "assistant a2", "user b1", "assistant b2", "user c1", "assistant c2", "user d1"})
	equal(t, "the first retry", contents(t, calls[1]), []string{"user c1", "assistant c2", "user d1"})
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
	equal(t, "the call after it", contents(t, g.calls()[1]), []string{"user d1", "assistant d2", "user e1"})
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
