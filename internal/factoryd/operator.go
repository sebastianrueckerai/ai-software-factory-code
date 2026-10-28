package factoryd

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// OperatorModel is the project agent's model (architecture 5.7): one
// streamed, tool-calling completion through the gateway on the account of
// a project's chat task, purpose operator.
type OperatorModel struct {
	GatewayURL string
	Token      string       // the gateway-factoryd token
	HTTP       *http.Client // nil = http.DefaultClient
}

// operatorTimeout bounds one call, its stream included; OperatorTokens
// is the answer's max_tokens, which the gateway may clamp further to the
// task's headroom.
const (
	operatorTimeout = 2 * time.Minute
	OperatorTokens  = 16384
)

// OperatorMessage is one message of the conversation, in the OpenAI chat
// form: an assistant's may carry tool calls, a tool message answers the
// call ToolCallID.
type OperatorMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []OperatorCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// Cut says an answer stopped at its max_tokens (finish_reason
	// length); a request never carries it.
	Cut bool `json:"-"`
}

// OperatorCall is one tool call of an answer; Arguments is its JSON text.
type OperatorCall struct {
	ID, Name, Arguments string
}

func (c OperatorCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"id": c.ID, "type": "function",
		"function": map[string]string{"name": c.Name, "arguments": c.Arguments}})
}

// OperatorTool is one tool the model may call: its name, what it does and
// its arguments' JSON Schema.
type OperatorTool struct {
	Name, Description string
	Parameters        any
}

func (t OperatorTool) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"type": "function",
		"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
}

// Stream is one call of alias on task taskID's account with messages and
// tools, cut after operatorTimeout: each piece of the answer's text goes
// to text as it arrives, and the answer comes back with its tool calls in
// their index order. A gateway refusal is a *policy.Refusal in the chain;
// any other status, an unreadable chunk or a stream that ends before its
// [DONE] is an error.
// The call names the chat thread it serves, which its audit row keeps.
func (m *OperatorModel) Stream(ctx context.Context, taskID, alias, thread string, messages []OperatorMessage, tools []OperatorTool, text func(string)) (OperatorMessage, error) {
	return m.stream(ctx, policy.PurposeOperator, OperatorTokens, taskID, alias, thread, messages, tools, text)
}

// compactTokens bounds a compaction's summary (roadmap 239).
const compactTokens = 2048

// Compact is one compaction call of chat thread thread on chat task
// taskID's account: the cheap alias summarises messages, its answer the
// summary text.
func (m *OperatorModel) Compact(ctx context.Context, taskID, thread string, messages []OperatorMessage) (string, error) {
	answer, err := m.stream(ctx, policy.PurposeCompact, compactTokens, taskID, policy.Small, thread, messages, nil, func(string) {})
	return answer.Content, err
}

// Vision is one look at image, ask's answer by the vision alias on chat
// task taskID's account, purpose vision; an empty ask asks what the
// image shows. A gateway refusal is a *policy.Refusal in the chain, as
// Stream's.
func (m *OperatorModel) Vision(ctx context.Context, taskID, alias, thread, ask string, image []byte, contentType string) (string, error) {
	parts := []map[string]any{{"type": "text", "text": cmp.Or(ask, "Describe this image.")},
		{"type": "image_url", "image_url": map[string]any{"url": "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(image)}}}
	answer, err := m.call(ctx, policy.PurposeVision, taskID, thread, map[string]any{
		"model": alias, "stream": true, "max_tokens": OperatorTokens,
		"messages": []map[string]any{{"role": "user", "content": parts}}}, func(string) {})
	return answer.Content, err
}

// titleTokens bounds a thread naming call's answer (roadmap 304): a
// title is a few words.
const titleTokens = 200

// titlePrompt is the naming call's system prompt (roadmap 304): the
// answer must work as a thread title in the switcher.
const titlePrompt = "You name a chat thread from its first message. " +
	"Answer with the title alone: a name, not a sentence, a few words, at most 60 characters, no surrounding quotes. " +
	"Answer in the message's language."

// Title is one naming call of chat thread thread on chat task taskID's
// account (roadmap 304): the cheap alias reads the thread's first
// message's head, its answer is the title before the console cleans it.
func (m *OperatorModel) Title(ctx context.Context, taskID, thread, first string) (string, error) {
	answer, err := m.stream(ctx, policy.PurposeTitle, titleTokens, taskID, policy.Small, thread,
		[]OperatorMessage{{Role: "system", Content: titlePrompt}, {Role: "user", Content: first}}, nil, func(string) {})
	return answer.Content, err
}

// stream is Stream for purpose with an answer of at most maxTokens; no
// tools sends none, no thread names none.
func (m *OperatorModel) stream(ctx context.Context, purpose string, maxTokens int, taskID, alias, thread string, messages []OperatorMessage, tools []OperatorTool, text func(string)) (OperatorMessage, error) {
	body := map[string]any{"model": alias, "messages": messages, "stream": true, "max_tokens": maxTokens}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return m.call(ctx, purpose, taskID, thread, body, text)
}

// call posts body to the gateway's chat door under purpose on task
// taskID's account and reads its one streamed answer.
func (m *OperatorModel) call(ctx context.Context, purpose, taskID, thread string, body map[string]any, text func(string)) (OperatorMessage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return OperatorMessage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operatorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.GatewayURL+"/v1/chat/completions", bytes.NewReader(payload)) // #nosec G704 -- the host is the configured GatewayURL; a caller's words ride only the body
	if err != nil {
		return OperatorMessage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	req.Header.Set(gateway.HeaderFactoryPurpose, purpose)
	if thread != "" {
		req.Header.Set(gateway.HeaderFactoryThread, thread)
	}
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req) // #nosec G704 -- the request built above, to GatewayURL
	if err != nil {
		return OperatorMessage{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		var refused struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if d := res.Header.Get(gateway.HeaderDecision); d != "" && json.Unmarshal(body, &refused) == nil {
			return OperatorMessage{}, fmt.Errorf("gateway %s: %w", d, &policy.Refusal{Decision: d, Reason: refused.Error.Message})
		}
		return OperatorMessage{}, fmt.Errorf("status %d: %s", res.StatusCode, textutil.Truncate(string(bytes.TrimSpace(body)), 300))
	}
	answer := OperatorMessage{Role: "assistant"}
	calls := map[int]*OperatorCall{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		if data = strings.TrimSpace(data); data == "[DONE]" {
			for _, i := range slices.Sorted(maps.Keys(calls)) {
				c := calls[i]
				c.ID = cmp.Or(c.ID, "call_"+strconv.Itoa(i))
				answer.ToolCalls = append(answer.ToolCalls, *c)
			}
			return answer, nil
		}
		var chunk struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
				Delta        struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return answer, fmt.Errorf("unreadable chunk: %s", textutil.Truncate(data, 300))
		}
		if chunk.Error != nil {
			return answer, errors.New("the stream failed: " + chunk.Error.Message)
		}
		for _, ch := range chunk.Choices {
			answer.Cut = answer.Cut || ch.FinishReason == "length"
			if d := ch.Delta.Content; d != "" {
				answer.Content += d
				text(d)
			}
			for _, d := range ch.Delta.ToolCalls {
				c := calls[d.Index]
				if c == nil {
					c = &OperatorCall{}
					calls[d.Index] = c
				}
				c.ID = cmp.Or(d.ID, c.ID)
				c.Name = cmp.Or(d.Function.Name, c.Name)
				c.Arguments += d.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return answer, err
	}
	return answer, errors.New("the stream ended before [DONE]")
}
