// Package acceptance holds the acceptance tests of the fresh per-turn
// context and the aged reads (plan #504, card fresh-context-acceptance):
// each turn's system message holds the project's status line as read at
// the turn's start and no card, which the reads give on demand by the
// card's issue number as well as its task id, every tool result carries
// the time it was read, which GET /projects/{name}/chat answers as
// read_at, and a replayed result older than one minute shows its age
// beside the current state. The rig is the whole chat stack over the
// fixture stores with the operator model scripted: the agent's move_card
// runs through the console's own action, the fixture board takes the
// card into ready as factoryd would, and the next turn's card read
// answers the new state while the conversation still holds the old read
// — which shows its age.
package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const (
	// movedNote is what the console's move answers; the member's undo of
	// it rides the applied event, not the tool block.
	movedNote = "Moved #5 to lane ready; it leaves the backlog once factoryd picks it up."
	// backlogCardHead and queuedCardHead are #5 as the card tool reads it
	// in the backlog and after the move into ready; the fixture files it
	// with a budget of 3 EUR.
	backlogCardHead = `#5 t-m "Fix the cache." backlog (waits in the backlog until a member moves it into a lane), €0.00 of €3.00`
	queuedCardHead  = `#5 t-m "Fix the cache." queued (waits for its session to start), €0.00 of €3.00`
	// statusEnd ends every turn's system message: the code project's
	// status line, #11 running and #5 waiting in the Queued column before
	// and after its move, the chat's task no card.
	statusEnd = "\n\nThe project now:\nStatus: working; 1 working; 0 need you; spend this month €0.00 of €50.00: the cards €0.00, the chat €0.00"
)

// freshRig is the chat rig of these tests: the fixture stores, the code
// project's two cards, the gateway scripting the operator model, and the
// chat block of an empty confirm list, so every tool — move_card
// included — runs at once.
func freshRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.ledger.setChat(&policy.ChatConfig{Confirm: []string{}})
	return r
}

// TestAcceptanceFreshContextPerTurn sends two messages with a card-state
// change between them: the first turn moves #5 from the backlog into
// ready through the agent's own tool and the fixture board, naming it
// "#5", the second reads it as 5, with no board read between. Each
// turn's system message ends with the status line and holds no card; the
// second turn's card read answers the card in its new state.
func TestAcceptanceFreshContextPerTurn(t *testing.T) {
	r := freshRig(t)
	r.gateway.replies = [][]string{
		{call(1, "move_card", `{"task_id":"#5","lane":"ready"}`)},
		{say("moved it.")},
		{call(2, "card", `{"task_id":5}`)},
		{say("it is in ready now.")},
	}
	if code, events := r.post("start #5"); code != http.StatusOK {
		t.Fatalf("the move turn = %d %v", code, events)
	} else if !hasToolResult(events, "move_card", movedNote) {
		t.Fatalf("the move did not run; events:\n%s", strings.Join(events, "\n"))
	}
	if code, events := r.post("and now?"); code != http.StatusOK {
		t.Fatalf("the second turn = %d %v", code, events)
	} else if !hasToolResultPrefix(events, "card", queuedCardHead) {
		t.Fatalf("the card read of 5 did not answer #5 in its new state; events:\n%s", strings.Join(events, "\n"))
	}
	calls := r.gateway.calls()
	if len(calls) != 4 {
		t.Fatalf("%d model calls, want 4", len(calls))
	}
	for i, call := range calls {
		sys, _ := messagesOf(call)[0]["content"].(string)
		if !strings.HasPrefix(sys, "You are the project agent of project ai-software-factory-code") || !strings.HasSuffix(sys, statusEnd) {
			t.Errorf("call %d's system message is not the prompt and the status line alone:\n%s", i, sys)
		}
	}
}

// TestAcceptanceToolResultTimestamps triggers a card tool call and reads
// the conversation back through GET /projects/{name}/chat: the answer's
// tool call carries a read_at that is present and recent.
func TestAcceptanceToolResultTimestamps(t *testing.T) {
	r := freshRig(t)
	start := time.Now().Add(-time.Second)
	r.gateway.replies = [][]string{
		{call(1, "card", `{"task_id":"t-m"}`)},
		{say("#5 waits in the backlog.")},
	}
	if code, events := r.post("what about #5?"); code != http.StatusOK {
		t.Fatalf("chat = %d %v", code, events)
	} else if !hasToolCall(events, "card") {
		t.Fatalf("the card tool was not called; events:\n%s", strings.Join(events, "\n"))
	}
	req, err := http.NewRequest(http.MethodGet, r.srv.URL+chatPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(r.ck)
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", chatPath, resp.StatusCode, body)
	}
	var answer struct {
		Messages []factoryd.ChatMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("GET %s answered %s: %v", chatPath, body, err)
	}
	var raw struct {
		Messages []struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	stamps := 0
	for _, m := range raw.Messages {
		for _, c := range m.ToolCalls {
			if string(c["name"]) != `"card"` {
				continue
			}
			value, ok := c["read_at"]
			if !ok {
				t.Errorf("the card result carries no read_at: %v", c)
				continue
			}
			var s string
			if err := json.Unmarshal(value, &s); err != nil {
				t.Errorf("the card result's read_at %s is no time string: %v", value, err)
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				t.Errorf("the card result's read_at %q parses: %v", s, err)
				continue
			}
			if at.Before(start) || at.After(time.Now().Add(time.Second)) {
				t.Errorf("the card result's read_at = %v, want within [%v, now]", at, start)
			}
			stamps++
		}
	}
	if stamps != 1 {
		t.Fatalf("%d card results in the GET answer, want 1: %s", stamps, body)
	}
}

// TestAcceptanceAgedReadShowsAge reads a card by its issue number,
// mutates it through the agent's own move, then sends another message
// once the read is older than a minute: the replayed tool result carries
// its age prefix — when it was read and how many minutes ago, beside the
// card text it read — while the same turn's fresh read of "#5" already
// shows the new state.
func TestAcceptanceAgedReadShowsAge(t *testing.T) {
	r := freshRig(t)
	r.gateway.replies = [][]string{
		{call(1, "card", `{"task_id":"#5"}`)},
		{say("read it.")},
		{call(2, "move_card", `{"task_id":5,"lane":"ready"}`)},
		{say("moved it.")},
		{call(3, "card", `{"task_id":"#5"}`)},
		{say("it is in ready now.")},
	}
	if code, events := r.post("what about #5?"); code != http.StatusOK {
		t.Fatalf("the read turn = %d %v", code, events)
	} else if !hasToolResultPrefix(events, "card", backlogCardHead) {
		t.Fatalf("the card read did not answer the backlog card; events:\n%s", strings.Join(events, "\n"))
	}
	readAt := cardReadAt(t, r)
	if code, events := r.post("start #5"); code != http.StatusOK {
		t.Fatalf("the move turn = %d %v", code, events)
	} else if !hasToolResult(events, "move_card", movedNote) {
		t.Fatalf("the move did not run; events:\n%s", strings.Join(events, "\n"))
	}
	// the read must be older than a minute at the next turn's fresh read
	if d := time.Until(readAt.Add(61 * time.Second)); d > 0 {
		time.Sleep(d)
	}
	if code, events := r.post("and now?"); code != http.StatusOK {
		t.Fatalf("the third turn = %d %v", code, events)
	}
	calls := r.gateway.calls()
	if len(calls) != 6 {
		t.Fatalf("%d model calls, want 6", len(calls))
	}
	last := messagesOf(calls[len(calls)-1])
	var aged string
	for _, m := range last {
		content, _ := m["content"].(string)
		if m["role"] == "tool" && strings.HasPrefix(content, "[read at ") {
			aged = content
			break
		}
	}
	if aged == "" {
		var all []string
		for _, m := range last {
			all = append(all, fmt.Sprintf("%v %q", m["role"], m["content"]))
		}
		t.Fatalf("no replayed tool result shows its age:\n%s", strings.Join(all, "\n"))
	}
	wantPrefix := fmt.Sprintf("[read at %s, 1 min ago] ", readAt.UTC().Format("2006-01-02 15:04 UTC"))
	if !strings.HasPrefix(aged, wantPrefix) {
		t.Errorf("the aged result = %q, want it to begin %q", aged, wantPrefix)
	}
	if !strings.Contains(aged, backlogCardHead) {
		t.Errorf("the aged result lost the card text it read:\n%s", aged)
	}
	if fresh, _ := last[len(last)-1]["content"].(string); !strings.HasPrefix(fresh, queuedCardHead) {
		t.Errorf("the turn's fresh card read lacks the card's new state %q:\n%s", queuedCardHead, fresh)
	}
}

// cardReadAt is the time the conversation's first card tool call was
// read, as GET /projects/{name}/chat answers it: the turn's fresh read,
// which the store stamps in real time, not the fixture ledger's clock.
func cardReadAt(t *testing.T, r *rig) time.Time {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.srv.URL+chatPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(r.ck)
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", chatPath, resp.StatusCode, body)
	}
	var answer struct {
		Messages []struct {
			ToolCalls []struct {
				Name   string    `json:"name"`
				ReadAt time.Time `json:"read_at"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("GET %s answered %s: %v", chatPath, body, err)
	}
	for _, m := range answer.Messages {
		for _, c := range m.ToolCalls {
			if c.Name == "card" {
				return c.ReadAt
			}
		}
	}
	t.Fatalf("no card tool call in the conversation: %s", body)
	return time.Time{}
}

// hasToolResultPrefix is true when events carry a tool result for tool
// whose result begins with prefix.
func hasToolResultPrefix(events []string, tool, prefix string) bool {
	for _, e := range events {
		if !strings.HasPrefix(e, "tool ") {
			continue
		}
		var payload struct {
			Name   string `json:"name"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "tool ")), &payload); err != nil {
			continue
		}
		if payload.Name == tool && strings.HasPrefix(payload.Result, prefix) {
			return true
		}
	}
	return false
}

// messagesOf are one model call request's messages.
func messagesOf(req map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range req["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

// calls are the model calls' requests so far, oldest first.
func (g *gateway) calls() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// setChat replaces the chat block of the policy in force; a nil one takes
// the policy's default back.
func (l *acceptLedger) setChat(chat *policy.ChatConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chat = chat
}
