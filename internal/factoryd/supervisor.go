package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Supervision (architecture 6.6) answers a detector trip in code first:
// a message into the session, a redirect when a message or nudge cannot
// be delivered or the session loops, retry_stronger (the workflow's next
// ladder rung) at the cheap cap. What code cannot answer, the check-in
// conversation decides; the actions below are the shared vocabulary.
type SupervisorAction string

const (
	ActionContinue      SupervisorAction = "continue"
	ActionMessage       SupervisorAction = "message"
	ActionEscalate      SupervisorAction = "escalate"
	ActionRedirect      SupervisorAction = "redirect"
	ActionRetryStronger SupervisorAction = "retry_stronger"
)

type Decision struct {
	Action   SupervisorAction `json:"action"`
	Guidance string           `json:"guidance,omitempty"`
	Reason   string           `json:"reason,omitempty"`
	// Choices are the answers the agent offered with its own escalation,
	// Tool and Need its request_tool's; never fields of the supervisor
	// model's answer.
	Choices    []string `json:"-"`
	Tool, Need string   `json:"-"`
}

// maxDecisionText caps guidance and reason; they become findings and
// issue comments.
const maxDecisionText = 2000

// decodeAnswer decodes a model's answer into v: exactly one JSON object
// with no unknown field, a markdown fence around it tolerated.
func decodeAnswer(content string, v any) error {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) >= 6 {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s, "```")[3:], "json"))
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("text after the JSON object")
	}
	return nil
}

// ProgressDigest is what a human reviewer would read to see a circle: the
// commands with their outcomes over time, the files touched with edit
// counts, the last error signatures and the diff so far.
type ProgressDigest struct {
	Commands          []DigestCommand `json:"commands"`
	Files             map[string]int  `json:"files_edited"`
	ToolCalls         int             `json:"other_tool_calls"`
	SubagentToolCalls int             `json:"subagent_tool_calls"` // the part of ToolCalls subagents made
	Errors            []DigestError   `json:"last_error_signatures"`
	Diff              DigestDiff      `json:"diff"`
}

type DigestCommand struct {
	At        string `json:"at"` // since the session started
	Command   string `json:"command"`
	Exit      *int   `json:"exit,omitempty"`
	Signature string `json:"signature,omitempty"` // failures only
}

type DigestError struct {
	Command   string `json:"command"`
	Signature string `json:"signature"`
	Count     int    `json:"count"`
	Output    string `json:"output"` // the output tail of its latest occurrence
}

type DigestDiff struct {
	Files      int    `json:"files"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Patch      string `json:"patch,omitempty"`
}

// The digest keeps the last digestCommands commands, the last digestErrors
// error signatures and the digestFiles files edited last.
const (
	digestCommands = 40
	digestErrors   = 5
	digestFiles    = 40
)

type digestBuilder struct {
	start time.Time
	g     ProgressDigest
	files []string // g.Files' paths, the one edited last last
}

func newDigestBuilder(start time.Time) *digestBuilder {
	return &digestBuilder{start: start, g: ProgressDigest{Files: map[string]int{}}}
}

func (b *digestBuilder) add(ev harness.Event, at time.Time) {
	switch ev.Kind {
	case harness.EventCommand:
		cmd := detailString(ev, "command")
		c := DigestCommand{At: at.Sub(b.start).Round(time.Second).String(), Command: CommandLine(cmd)}
		if exit, ok := detailInt(ev, "exit"); ok {
			c.Exit = &exit
			if exit != 0 {
				out := detailString(ev, "output")
				c.Signature = ErrorSignature(cmd, out)
				b.noteError(c.Command, c.Signature, out)
			}
		}
		b.g.Commands = append(b.g.Commands, c)
		if len(b.g.Commands) > digestCommands {
			b.g.Commands = b.g.Commands[len(b.g.Commands)-digestCommands:]
		}
	case harness.EventFileChange:
		path := detailString(ev, "path")
		b.g.Files[path]++
		b.files = append(slices.DeleteFunc(b.files, func(p string) bool { return p == path }), path)
		if len(b.files) > digestFiles {
			delete(b.g.Files, b.files[0])
			b.files = b.files[1:]
		}
	case harness.EventToolCall:
		b.g.ToolCalls++
		if detailBool(ev, "subagent") {
			b.g.SubagentToolCalls++
		}
	case harness.EventCheckpoint:
		diff, ok := ev.Detail["diff"].(map[string]any)
		if !ok {
			return
		}
		files, _ := toInt(diff["files"])
		ins, _ := toInt(diff["insertions"])
		del, _ := toInt(diff["deletions"])
		b.g.Diff = DigestDiff{Files: files, Insertions: ins, Deletions: del, Patch: detailString(ev, "patch")}
	}
}

func (b *digestBuilder) noteError(cmd, sig, out string) {
	for i, e := range b.g.Errors {
		if e.Signature == sig {
			e.Count++
			e.Output = out
			b.g.Errors = append(append(b.g.Errors[:i:i], b.g.Errors[i+1:]...), e)
			return
		}
	}
	b.g.Errors = append(b.g.Errors, DigestError{Command: cmd, Signature: sig, Count: 1, Output: out})
	if len(b.g.Errors) > digestErrors {
		b.g.Errors = b.g.Errors[len(b.g.Errors)-digestErrors:]
	}
}

// digest returns a copy safe to hand out while the builder keeps adding.
func (b *digestBuilder) digest() ProgressDigest {
	g := b.g
	g.Commands = append([]DigestCommand(nil), g.Commands...)
	g.Errors = append([]DigestError(nil), g.Errors...)
	g.Files = make(map[string]int, len(b.g.Files))
	for k, v := range b.g.Files {
		g.Files[k] = v
	}
	return g
}

// Summary is the digest in one line, for the record, the timeline and the
// issue comment.
func (g ProgressDigest) Summary() string {
	failed, edits := 0, 0
	for _, c := range g.Commands {
		if c.Exit != nil && *c.Exit != 0 {
			failed++
		}
	}
	for _, n := range g.Files {
		edits += n
	}
	calls := fmt.Sprintf("%d other tool calls", g.ToolCalls)
	if g.SubagentToolCalls > 0 {
		calls += fmt.Sprintf(" (%d by subagents)", g.SubagentToolCalls)
	}
	s := fmt.Sprintf("%d commands (%d failed), %d files changed %d times, %s, diff %d files +%d/-%d",
		len(g.Commands), failed, len(g.Files), edits, calls, g.Diff.Files, g.Diff.Insertions, g.Diff.Deletions)
	if n := len(g.Errors); n > 0 {
		s += fmt.Sprintf("; last error: `%s` ×%d", g.Errors[n-1].Command, g.Errors[n-1].Count)
	}
	return s
}

// TripReport is the detector's side of the check-in's input.
type TripReport struct {
	Detector TripKind `json:"detector"`
	Detail   string   `json:"detail"`
	Stuck    string   `json:"stuck,omitempty"`
	// Climbs: a handover on a stuck trip moves the work to the stronger
	// model (the session is not on it yet)
	Climbs bool `json:"climbs,omitempty"`
}

// SupervisorModel is factoryd's model client: every call it makes (the
// session supervisor, the gate supervisor, the model review) goes to the
// gateway on the task's account, like an agent's. It presents the
// gateway-factoryd token with the task and the purpose; the gateway admits
// only the alias the task's class names for the purpose, clamps the answer
// limit to the free headroom, holds that worst case, books the call's
// cost to the task and audits it with the purpose as its kind.
type SupervisorModel struct {
	GatewayURL string
	Token      string // the gateway-factoryd token
	Budget     TokenBudget
	HTTP       *http.Client // nil = http.DefaultClient
}

// chatPayload builds one JSON-mode call's body.
func chatPayload(alias, system string, user []byte, limit int) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model":           alias,
		"max_tokens":      limit,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": string(user)},
		},
	})
}

// Chat is one JSON-mode call of alias through the gateway on the task's
// account for purpose, with system as the system prompt and input,
// compact JSON, as the user message, cut after timeout (the task's
// limits.supervisor_timeout: a reasoning model thinks before it answers);
// deployment is the one that answered, as the gateway names it. A gateway
// refusal is a *policy.Refusal in the chain; alias "" (the policy names
// none) makes no call.
func (m *SupervisorModel) Chat(ctx context.Context, taskID, alias, purpose string, timeout time.Duration, system string, input any) (content, deployment string, err error) {
	if alias == "" {
		return "", "", fmt.Errorf("the policy names no %s model for the task's capability classes", purpose)
	}
	limit, err := m.Budget.OutputLimit()
	if err != nil {
		return "", "", err
	}
	user, err := json.Marshal(input)
	if err != nil {
		return "", "", err
	}
	payload, err := chatPayload(alias, system, user, limit)
	if err != nil {
		return "", "", err
	}
	if timeout <= 0 {
		return "", "", fmt.Errorf("no timeout for the %s call", purpose)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.GatewayURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	req.Header.Set(gateway.HeaderFactoryPurpose, purpose)
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if res.StatusCode != http.StatusOK {
		var refused struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if d := res.Header.Get(gateway.HeaderDecision); d != "" && json.Unmarshal(body, &refused) == nil {
			return "", "", fmt.Errorf("gateway %s: %w", d, &policy.Refusal{Decision: d, Reason: refused.Error.Message})
		}
		return "", "", fmt.Errorf("status %d: %s", res.StatusCode, textutil.Truncate(string(body), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
		return "", "", fmt.Errorf("unreadable completion: %s", textutil.Truncate(string(body), 300))
	}
	return out.Choices[0].Message.Content, res.Header.Get(gateway.HeaderDeployment), nil
}
