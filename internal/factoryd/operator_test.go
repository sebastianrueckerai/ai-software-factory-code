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

// Two tool calls as LiteLLM streamed them from DashScope's qwen3.8-max:
// each call's first delta carries its id and name, the next an empty id,
// the rest argument fragments with no id; every delta has an empty
// content, and the usage chunk comes after the finish.
func TestOperatorReadsLiteLLMsToolCallChunks(t *testing.T) {
	g := &operatorGateway{chunks: append(strings.Split(`{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","role":"assistant","tool_calls":[{"id":"call_54f7167caa6f46a1b64f0989","function":{"arguments":"","name":"set_accent"},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"","function":{"arguments":""},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"{\"accent\": \"teal"},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\""},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":", \"task_id\": "},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\"t-da71"},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"898f"},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"5152"},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\""},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"}"},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":""},"type":"function","index":0}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_eaf618fe29974f1eb33434ab","function":{"arguments":"","name":"set_summary"},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"{\"summary\": "},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\"smoke test"},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\""},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":", \"task_id\": "},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\"t-da71"},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"898f"},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"5152"},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"\""},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"}"},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":""},"type":"function","index":1}]}}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","object":"chat.completion.chunk","created":1791694136,"model":"operator","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}
{"id":"chatcmpl-f9588acc-4553-9dba-85f5-fe7f9be95564","created":1791694136,"model":"operator","object":"chat.completion.chunk","choices":[{"index":0,"delta":{}}],"usage":{"completion_tokens":103,"prompt_tokens":4799,"total_tokens":4902,"prompt_tokens_details":{"cached_tokens":4608,"text_tokens":4799},"cost":0.0021520000000000003}}`, "\n"), "[DONE]")}
	var pieces []string
	answer, err := g.start(t).Stream(t.Context(), "t-chat", "operator", []OperatorMessage{{Role: "user", Content: "teal and smoke test"}}, nil,
		func(s string) { pieces = append(pieces, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := OperatorMessage{Role: "assistant", ToolCalls: []OperatorCall{
		{ID: "call_54f7167caa6f46a1b64f0989", Name: "set_accent", Arguments: `{"accent": "teal", "task_id": "t-da71898f5152"}`},
		{ID: "call_eaf618fe29974f1eb33434ab", Name: "set_summary", Arguments: `{"summary": "smoke test", "task_id": "t-da71898f5152"}`}}}
	if !reflect.DeepEqual(answer, want) || pieces != nil {
		t.Fatalf("answer = %+v, pieces %q", answer, pieces)
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
