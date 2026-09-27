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

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// The supervisor (architecture 6.6) is a model factoryd asks after a
// detector trips. It picks one action. No action carries a cap, a budget
// or a model name: retry_stronger means the workflow's next ladder rung.
type SupervisorAction string

const (
	ActionContinue      SupervisorAction = "continue"
	ActionRedirect      SupervisorAction = "redirect"
	ActionRetryStronger SupervisorAction = "retry_stronger"
	ActionEscalate      SupervisorAction = "escalate"
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
// a known action, guidance for redirect, a reason for escalate, no other
// field. A markdown fence around it is tolerated. Anything else escalates.
func parseDecision(content string) Decision {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) >= 6 {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s, "```")[3:], "json"))
	}
	invalid := func(why string) Decision {
		return Decision{Action: ActionEscalate, Reason: "supervisor answer invalid (" + why + "): " + truncate(content, 300)}
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var d Decision
	if err := dec.Decode(&d); err != nil {
		return invalid(err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("text after the JSON object")
	}
	d.Guidance, d.Reason = truncate(strings.TrimSpace(d.Guidance), maxDecisionText), truncate(strings.TrimSpace(d.Reason), maxDecisionText)
	switch d.Action {
	case ActionContinue, ActionRetryStronger:
	case ActionRedirect:
		if d.Guidance == "" {
			return invalid("redirect without guidance")
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
	Brief   harness.TaskBrief `json:"brief"`
	Trip    TripReport        `json:"trip"`
	Digest  ProgressDigest    `json:"digest"`
	Record  string            `json:"record"` // the session record's last events, JSONL
	Model   string            `json:"model"`
	TopRung bool              `json:"top_rung"` // retry_stronger then acts as redirect
	Calls   int               `json:"supervisor_call"`
	MaxCall int               `json:"supervisor_calls_per_session"`
}

// maxRecordTail bounds the session record the supervisor reads.
const maxRecordTail = 24 << 10

const supervisorPrompt = `You supervise an autonomous coding agent that works on one task in a sandbox. A cheap detector tripped: the session may be going in circles. Read the brief, the detector's finding, the progress digest (commands with outcomes over time, files with edit counts, the last error signatures, the diff against the base so far) and the tail of the session record, then choose exactly one action:
- "continue": the agent makes real progress and the trip is a false alarm. The session runs on.
- "redirect": the agent is stuck on a wrong approach. The session stops, its work in progress is kept, and a new session on the same model resumes with your guidance. Give concrete guidance: what to stop doing and what to do instead.
- "retry_stronger": the approach is sound but the model is not capable enough. A new session on the next stronger model resumes from the work in progress. When top_rung is true there is no stronger model and this acts as redirect, so give guidance.
- "escalate": the task cannot be completed as specified (contradictory, impossible, needs a human decision or access the agent does not have). A human is asked. Give the reason.
You cannot raise a time limit, a budget or a model beyond the next rung; only a human can.
Answer with one JSON object and nothing else: {"action": "continue|redirect|retry_stronger|escalate", "guidance": "...", "reason": "..."}. guidance is required for redirect, reason for escalate; each under 1000 characters.`

// SupervisorModel calls the supervisor alias on LiteLLM directly, with a
// virtual key it mints per call: metadata task_id, so the settlement loop
// books the spend onto the task and the session-end reconcile blocks it
// with the task's other keys; max_budget the task's remaining budget; and
// the supervisor alias as its only model.
type SupervisorModel struct {
	BaseURL string // LiteLLM
	Admin   *LiteLLMAdmin
	Alias   string // "supervisor"
	Budget  TokenBudget
	HTTP    *http.Client // nil = supervisorTimeout
}

// supervisorTimeout bounds one call: GLM reasons before it answers.
const supervisorTimeout = 4 * time.Minute

// Decide never fails: a failed or invalid answer is an escalation.
func (m *SupervisorModel) Decide(ctx context.Context, taskID string, budgetEUR, spentEUR float64, in SupervisorInput) Decision {
	d, err := m.decide(ctx, taskID, budgetEUR, spentEUR, in)
	if err != nil {
		return Decision{Action: ActionEscalate, Reason: "supervisor call failed: " + truncate(err.Error(), 500)}
	}
	return d
}

// The key's cap is the budget minus what the task has spent: the settled
// spent_eur, or the spend LiteLLM already shows on the task's keys (the
// running session's key settles only at the task's end), whichever is more.
func (m *SupervisorModel) decide(ctx context.Context, taskID string, budgetEUR, spentEUR float64, in SupervisorInput) (Decision, error) {
	keySpend, err := m.Admin.TaskSpend(ctx, taskID)
	if err != nil {
		return Decision{}, fmt.Errorf("the task's spend is unknown: %w", err)
	}
	remainingEUR := budgetEUR - max(spentEUR, keySpend)
	if remainingEUR <= 0 {
		return Decision{}, errors.New("the task has no budget left")
	}
	limit, err := m.Budget.OutputLimit()
	if err != nil {
		return Decision{}, err
	}
	key, err := m.Admin.MintKey(ctx, taskID, remainingEUR, []string{m.Alias}, "supervisor")
	if err != nil {
		return Decision{}, err
	}
	user, err := json.MarshalIndent(in, "", " ")
	if err != nil {
		return Decision{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"model":           m.Alias,
		"max_tokens":      limit,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": supervisorPrompt},
			{"role": "user", "content": string(user)},
		},
	})
	if err != nil {
		return Decision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: supervisorTimeout}
	}
	res, err := client.Do(req)
	if err != nil {
		return Decision{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Decision{}, err
	}
	if res.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("status %d: %s", res.StatusCode, truncate(string(body), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
		return Decision{}, fmt.Errorf("unreadable completion: %s", truncate(string(body), 300))
	}
	return parseDecision(out.Choices[0].Message.Content), nil
}

// sessionSupervision caps the supervisor calls of one session: the trip
// after the last allowed call escalates without asking.
type sessionSupervision struct {
	max    int
	calls  int
	decide func(ctx context.Context, trip *Trip, call int) Decision
}

func (s *sessionSupervision) onTrip(ctx context.Context, trip *Trip) (Decision, bool) {
	if s.calls >= s.max {
		return Decision{Action: ActionEscalate, Reason: fmt.Sprintf(
			"%s tripped again after %d supervisor calls in this session: %s", trip.Kind, s.calls, trip.Detail)}, false
	}
	s.calls++
	return s.decide(ctx, trip, s.calls), true
}
