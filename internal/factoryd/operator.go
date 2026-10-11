package factoryd

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
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

// operatorTimeout bounds one call, its stream included; operatorTokens
// is the answer's max_tokens, which the gateway may clamp further.
const (
	operatorTimeout = 2 * time.Minute
	operatorTokens  = 4096
)

// OperatorMessage is one message of the conversation, in the OpenAI chat
// form: an assistant's may carry tool calls, a tool message answers the
// call ToolCallID.
type OperatorMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []OperatorCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
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
func (m *OperatorModel) Stream(ctx context.Context, taskID, alias string, messages []OperatorMessage, tools []OperatorTool, text func(string)) (OperatorMessage, error) {
	payload, err := json.Marshal(map[string]any{"model": alias, "messages": messages, "tools": tools,
		"stream": true, "max_tokens": operatorTokens})
	if err != nil {
		return OperatorMessage{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, operatorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.GatewayURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return OperatorMessage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	req.Header.Set(gateway.HeaderFactoryPurpose, policy.PurposeOperator)
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
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
				Delta struct {
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
