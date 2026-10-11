package factoryd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// operatorGateway answers one streamed completion with chunks, each a
// data line, and records the request it got.
type operatorGateway struct {
	status  int
	header  map[string]string
	body    string // a non-200 answer's body
	chunks  []string
	headers http.Header
	request map[string]any
}

func (g *operatorGateway) start(t *testing.T) *OperatorModel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.headers = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &g.request); err != nil {
			t.Errorf("request body: %v", err)
		}
		for k, v := range g.header {
			w.Header().Set(k, v)
		}
		if g.status != 0 {
			w.WriteHeader(g.status)
			io.WriteString(w, g.body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range g.chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
	}))
	t.Cleanup(srv.Close)
	return &OperatorModel{GatewayURL: srv.URL, Token: "gw-token"}
}

// One call goes to the gateway on the chat task's account with purpose
// operator, the alias, the tools, max_tokens and the conversation in the
// OpenAI form; the answer's text streams piece by piece, and two tool
// calls whose deltas interleave come back whole, in index order.
func TestOperatorStreamsTextAndParallelToolCalls(t *testing.T) {
	g := &operatorGateway{chunks: []string{
		`{"choices":[{"delta":{"role":"assistant","content":"Two "}}]}`,
		`{"choices":[{"delta":{"content":"cards."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"card","arguments":"{\"task_"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"board","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"id\":\"t-1\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`[DONE]`,
	}}
	m := g.start(t)
	var pieces []string
	answer, err := m.Stream(t.Context(), "t-chat-linkshort-2026-10", "operator", []OperatorMessage{
		{Role: "system", Content: "rules"},
		{Role: "assistant", ToolCalls: []OperatorCall{{ID: "call_0", Name: "costs", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "call_0", Content: "EUR 1.20"},
		{Role: "user", Content: "what is open?"},
	}, []OperatorTool{{Name: "board", Description: "the board", Parameters: map[string]any{"type": "object"}}},
		func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := OperatorMessage{Role: "assistant", Content: "Two cards.", ToolCalls: []OperatorCall{
		{ID: "call_a", Name: "board", Arguments: "{}"}, {ID: "call_b", Name: "card", Arguments: `{"task_id":"t-1"}`}}}
	if !reflect.DeepEqual(answer, want) || !reflect.DeepEqual(pieces, []string{"Two ", "cards."}) {
		t.Fatalf("answer = %+v, pieces %q", answer, pieces)
	}
	if g.headers.Get("Authorization") != "Bearer gw-token" || g.headers.Get(gateway.HeaderFactoryTask) != "t-chat-linkshort-2026-10" ||
		g.headers.Get(gateway.HeaderFactoryPurpose) != policy.PurposeOperator {
		t.Fatalf("headers = %v", g.headers)
	}
	b, _ := json.Marshal(g.request)
	wantBody := `{"max_tokens":4096,"messages":[{"content":"rules","role":"system"},` +
		`{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"costs"},"id":"call_0","type":"function"}]},` +
		`{"content":"EUR 1.20","role":"tool","tool_call_id":"call_0"},{"content":"what is open?","role":"user"}],` +
		`"model":"operator","stream":true,"tools":[{"function":{"description":"the board","name":"board","parameters":{"type":"object"}},"type":"function"}]}`
	if string(b) != wantBody {
		t.Fatalf("request = %s\nwant      %s", b, wantBody)
	}
}

// A refusal comes back as the gateway's decision, any other status, a
// failed stream or one cut before [DONE] as an error naming it.
func TestOperatorStreamFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		g    operatorGateway
		want string
	}{
		"refused": {operatorGateway{status: http.StatusPaymentRequired, header: map[string]string{gateway.HeaderDecision: "refused_task_budget"},
			body: `{"error":{"message":"the task's budget is spent"}}`}, "gateway refused_task_budget: the task's budget is spent"},
		"bad gateway": {operatorGateway{status: http.StatusBadGateway, body: "upstream down"}, "status 502: upstream down"},
		"failed":      {operatorGateway{chunks: []string{`{"error":{"message":"overloaded"}}`}}, "the stream failed: overloaded"},
		"cut":         {operatorGateway{chunks: []string{`{"choices":[{"delta":{"content":"Hal"}}]}`}}, "the stream ended before [DONE]"},
	} {
		t.Run(name, func(t *testing.T) {
			m := tc.g.start(t)
			_, err := m.Stream(t.Context(), "t-chat-x-2026-10", "operator", nil, nil, func(string) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			var refusal *policy.Refusal
			if errors.As(err, &refusal) != (name == "refused") {
				t.Fatalf("refusal in %v: %v", err, refusal)
			}
		})
	}
}
