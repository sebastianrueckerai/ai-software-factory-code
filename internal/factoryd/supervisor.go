package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The supervisor (architecture 6.6) is a model factoryd asks after a
// detector trips or at a check-in. It picks continue, message or escalate.
// redirect and retry_stronger are factoryd's own decisions, never the
// model's: redirect stops the session when a message or nudge cannot be
// delivered, retry_stronger (the workflow's next ladder rung) ends a
// session stuck after a message.
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
}

// maxDecisionText caps guidance and reason; they become findings and
// issue comments.
const maxDecisionText = 2000

// parseDecision validates the model's answer: exactly one JSON object with
// a known action, guidance for message, a reason for escalate, no other
// field. A markdown fence around it is tolerated. Anything else escalates.
func parseDecision(content string) Decision {
	invalid := func(why string) Decision {
		return Decision{Action: ActionEscalate, Reason: "supervisor answer invalid (" + why + "): " + textutil.Truncate(content, 300)}
	}
	var d Decision
	if err := decodeAnswer(content, &d); err != nil {
		return invalid(err.Error())
	}
	d.Guidance, d.Reason = textutil.Truncate(strings.TrimSpace(d.Guidance), maxDecisionText), textutil.Truncate(strings.TrimSpace(d.Reason), maxDecisionText)
	switch d.Action {
	case ActionContinue:
	case ActionMessage:
		if d.Guidance == "" {
			return invalid("message without guidance")
		}
	case ActionEscalate:
		if d.Reason == "" {
			return invalid("escalate without a reason")
		}
	default:
		return invalid(fmt.Sprintf("unknown action %q", d.Action))
	}
	return d
}

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
	Commands  []DigestCommand `json:"commands"`
	Files     map[string]int  `json:"files_edited"`
	ToolCalls int             `json:"other_tool_calls"`
	Errors    []DigestError   `json:"last_error_signatures"`
	Diff      DigestDiff      `json:"diff"`
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

const (
	digestCommands = 40
	digestErrors   = 5
)

type digestBuilder struct {
	start time.Time
	g     ProgressDigest
}

func newDigestBuilder(start time.Time) *digestBuilder {
	return &digestBuilder{start: start, g: ProgressDigest{Files: map[string]int{}}}
}

func (b *digestBuilder) add(ev harness.Event, at time.Time) {
	switch ev.Kind {
	case harness.EventCommand:
		c := DigestCommand{At: at.Sub(b.start).Round(time.Second).String(), Command: detailString(ev, "command")}
		if exit, ok := detailInt(ev, "exit"); ok {
			c.Exit = &exit
			if exit != 0 {
				out := detailString(ev, "output")
				c.Signature = ErrorSignature(c.Command, out)
				b.noteError(c.Command, c.Signature, out)
			}
		}
		b.g.Commands = append(b.g.Commands, c)
		if len(b.g.Commands) > digestCommands {
			b.g.Commands = b.g.Commands[len(b.g.Commands)-digestCommands:]
		}
	case harness.EventFileChange:
		b.g.Files[detailString(ev, "path")]++
	case harness.EventToolCall:
		b.g.ToolCalls++
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
	s := fmt.Sprintf("%d commands (%d failed), %d files changed %d times, %d other tool calls, diff %d files +%d/-%d",
		len(g.Commands), failed, len(g.Files), edits, g.ToolCalls, g.Diff.Files, g.Diff.Insertions, g.Diff.Deletions)
	if n := len(g.Errors); n > 0 {
		s += fmt.Sprintf("; last error: `%s` ×%d", g.Errors[n-1].Command, g.Errors[n-1].Count)
	}
	return s
}

// TripReport is the detector's side of the supervisor input.
type TripReport struct {
	Detector TripKind `json:"detector"`
	Detail   string   `json:"detail"`
}

// SupervisorInput is everything the supervisor reads: the brief, the trip,
// the digest and the tail of the session record.
type SupervisorInput struct {
	Brief  harness.TaskBrief `json:"brief"`
	Trip   TripReport        `json:"trip"`
	Digest ProgressDigest    `json:"digest"`
	Record string            `json:"record"` // the session record's last events, JSONL
	Model  string            `json:"model"`
	Calls  int               `json:"supervisor_call"`
	// Messages: the guidance this session's earlier messages delivered
	Messages []string `json:"messages,omitempty"`
	MaxCall  int      `json:"supervisor_calls_per_session"`
}

// maxRecordTail bounds the session record the supervisor reads.
const maxRecordTail = 24 << 10

const supervisorPrompt = `You supervise an autonomous coding agent that works on one task in a sandbox. You are asked when a cheap detector tripped (the session may be going in circles) or at a scheduled check-in (detector "check_in": no detector tripped; judge whether the session is on track). Read the brief, the trip, the progress digest (commands with outcomes over time, files with edit counts, the last error signatures, the diff against the base so far) and the tail of the session record, then choose exactly one action:
- "continue": the agent makes real progress; the trip is a false alarm or the check-in finds the session on track. The session runs on untouched.
- "message": the agent is off track: circling, stuck on a wrong approach, or missing something the brief or the record shows. Your guidance goes into the running session as a message the agent reads next; it keeps its context and its work. Give concrete guidance: what to stop doing and what to do instead. If the agent writes no file after your message before a detector trips again, or writes in circles, the session ends and a new session resumes from the work in progress. Your earlier messages in this session are in messages: do not repeat one that did not work.
- "escalate": the task cannot be completed as specified (contradictory, impossible, needs a human decision or access the agent does not have). A human is asked. Give the reason.
You cannot raise a time limit, a budget or a model; only a human can.
Answer with one JSON object and nothing else: {"action": "continue|message|escalate", "guidance": "...", "reason": "..."}. guidance is required for message, reason for escalate; each under 1000 characters.`

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

// Decide never fails: a failed or invalid answer, or a call the gateway
// refuses, is an escalation.
func (m *SupervisorModel) Decide(ctx context.Context, taskID, alias string, timeout time.Duration, in SupervisorInput) Decision {
	content, _, err := m.Chat(ctx, taskID, alias, policy.PurposeSupervisor, timeout, supervisorPrompt, in)
	if err != nil {
		return Decision{Action: ActionEscalate, Reason: "supervisor call failed: " + textutil.Truncate(err.Error(), 500)}
	}
	return parseDecision(content)
}

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
// indented JSON, as the user message, cut after timeout (the task's
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
	user, err := json.MarshalIndent(input, "", " ")
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

// sessionSupervision caps the supervisor calls of one session: the trip
// after the last allowed call escalates without asking. Check-ins are
// asked always and not counted.
type sessionSupervision struct {
	max    int
	calls  int
	decide func(ctx context.Context, trip *Trip, call int) Decision
}

func (s *sessionSupervision) onTrip(ctx context.Context, trip *Trip) (Decision, bool) {
	if trip.Kind == TripCheckIn {
		return s.decide(ctx, trip, s.calls), true
	}
	if s.calls >= s.max {
		return Decision{Action: ActionEscalate, Reason: fmt.Sprintf(
			"%s tripped again after %d supervisor calls in this session: %s", trip.Kind, s.calls, trip.Detail)}, false
	}
	s.calls++
	return s.decide(ctx, trip, s.calls), true
}
