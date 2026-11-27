package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
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
	// ActionReplan stops the session, its work pushed and kept, and the
	// planner cuts the card's remaining work into smaller cards.
	ActionReplan SupervisorAction = "replan"
	// ActionHold stops the session, its work pushed and kept, and the card
	// waits in Queued until its hold ends.
	ActionHold SupervisorAction = "hold"
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
	// Judged: a check-in model chose this action on the trip (a stuck
	// trip's handover), not a fixed rule or a fallback.
	Judged bool `json:"-"`
}

// maxDecisionText caps guidance and reason; they become findings and
// issue comments.
const maxDecisionText = 2000

// CostState is the deterministic cost and state block factoryd injects into
// its supervisor inputs: the task row's settled figures (budget, spent,
// remaining), its model calls and their tokens from the audit table, and
// the task's status. factoryd assembles it from its store, never from the
// agent.
type CostState struct {
	BudgetEUR    float64 `json:"budget_eur"`
	SpentEUR     float64 `json:"spent_eur"`
	HeldEUR      float64 `json:"held_eur,omitempty"`
	RemainingEUR float64 `json:"remaining_eur"`
	ReserveEUR   float64 `json:"reserve_eur,omitempty"`
	Calls        int     `json:"calls"`
	PromptTokens int     `json:"prompt_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	OutputTokens int     `json:"output_tokens"`
	State        string  `json:"state"`
}

// Line renders the cost and state block as the one JSON object every
// supervisor input carries it as.
func (c CostState) Line() string {
	b, _ := json.Marshal(c) // #nosec G104 -- plain numbers and a status word
	return string(b)
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
	// running are the bash calls started and not yet completed, oldest
	// first: a call the worker died in never completes
	running []runningCall
}

type runningCall struct{ id, command string }

// lastCommand is the command the session was in when it ended: the newest
// bash call still running, else the last one that completed, "" for none.
func (b *digestBuilder) lastCommand() string {
	if n := len(b.running); n > 0 {
		return b.running[n-1].command
	}
	if n := len(b.g.Commands); n > 0 {
		return b.g.Commands[n-1].Command
	}
	return ""
}

func newDigestBuilder(start time.Time) *digestBuilder {
	return &digestBuilder{start: start, g: ProgressDigest{Files: map[string]int{}}}
}

func (b *digestBuilder) add(ev harness.Event, at time.Time) {
	switch ev.Kind {
	case harness.EventCommand:
		id := detailString(ev, "call_id")
		b.running = slices.DeleteFunc(b.running, func(c runningCall) bool { return c.id == id })
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
		b.noteFile(detailString(ev, "path"), 1)
	case harness.EventToolCall:
		id := detailString(ev, "call_id")
		if status := detailString(ev, "status"); status != "running" {
			b.running = slices.DeleteFunc(b.running, func(c runningCall) bool { return c.id == id })
		} else if args, ok := ev.Detail["args"].(map[string]any); ok && detailString(ev, "tool") == "bash" {
			if cmd, _ := args["command"].(string); cmd != "" && !slices.ContainsFunc(b.running, func(c runningCall) bool { return c.id == id }) {
				b.running = append(b.running, runningCall{id, CommandLine(cmd)})
			}
		}
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
		// a file written through a shell shows only in the session's diff
		names, _ := ev.Detail["files"].([]any)
		for _, n := range names {
			if path, _ := n.(string); path != "" {
				b.noteFile(path, 0)
			}
		}
	}
}

// noteFile counts n edits of path. A file the checkpoint's diff names that
// no event did is added with no edits at the front of the list, where the
// bound drops from, so edited files outlive the names a checkpoint adds; an
// edit moves its file to the end, the one edited last last.
func (b *digestBuilder) noteFile(path string, n int) {
	_, known := b.g.Files[path]
	if known && n == 0 {
		return
	}
	b.g.Files[path] += n
	b.files = slices.DeleteFunc(b.files, func(p string) bool { return p == path })
	if n == 0 {
		b.files = append([]string{path}, b.files...)
	} else {
		b.files = append(b.files, path)
	}
	if len(b.files) > digestFiles {
		delete(b.g.Files, b.files[0])
		b.files = b.files[1:]
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
		s += fmt.Sprintf(", last error: `%s` ×%d", g.Errors[n-1].Command, g.Errors[n-1].Count)
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

// ChatInputLead opens every JSON-mode call's user message: OpenAI refuses
// JSON mode when no input message names JSON, and an input's own JSON
// does not always hold the word.
const ChatInputLead = "The input, as JSON:\n"

// chatPayload builds one JSON-mode call's body; low asks the model to
// think at low depth (reasoning_effort low).
func chatPayload(alias, system string, user []byte, limit int, low bool) ([]byte, error) {
	body := map[string]any{
		"model":           alias,
		"max_tokens":      limit,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": ChatInputLead + string(user)},
		},
	}
	if low {
		body["reasoning_effort"] = thinkingLow
	}
	return json.Marshal(body)
}

// chatBody is the body of Chat's call: input, compact JSON, as the user
// message.
func chatBody(alias, system string, input any, limit int) ([]byte, error) {
	user, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return chatPayload(alias, system, user, limit, false)
}

// Chat is one JSON-mode call of alias through the gateway on the task's
// account for the purpose p's profile names, with p's text as the system
// prompt and input, compact JSON, as the user message, cut after timeout (the task's
// limits.supervisor_timeout: a reasoning model thinks before it answers);
// deployment is the one that answered, as the gateway names it. A gateway
// refusal is a *policy.Refusal in the chain; alias "" (the policy names
// none) makes no call. A reasoning model's thinking counts against the
// answer limit: an answer cut at its limit is asked once more at the same
// limit and low thinking depth, under its own timeout, a minute clear of
// ctx's deadline. A second cut answer, or a first with no time left for
// another call, is a *cutAnswerError; a call its timeout cut is an
// answerTimeout.
func (m *SupervisorModel) Chat(ctx context.Context, taskID, alias string, timeout time.Duration, p prompts.Prompt, input any) (content, deployment string, err error) {
	content, deployment, _, err = m.chat(ctx, taskID, alias, timeout, p, input)
	return content, deployment, err
}

// thinkingLow is the reasoning_effort of the call once more, and the
// depth a gate records beside its model for an answer that call gave.
const thinkingLow = "low"

// chat is Chat with low: the answer came from the call once more at low
// thinking depth, which a gate records beside its model.
func (m *SupervisorModel) chat(ctx context.Context, taskID, alias string, timeout time.Duration, p prompts.Prompt, input any) (content, deployment string, low bool, err error) {
	purpose, system := p.Purpose(), p.Text()
	if alias == "" {
		return "", "", false, fmt.Errorf("the policy names no %s model for the task's capability classes", purpose)
	}
	limit, err := m.Budget.OutputLimit()
	if err != nil {
		return "", "", false, err
	}
	user, err := json.Marshal(input)
	if err != nil {
		return "", "", false, err
	}
	if timeout <= 0 {
		return "", "", false, fmt.Errorf("no timeout for the %s call", purpose)
	}
	c, deployment, err := m.call(ctx, taskID, alias, purpose, timeout, system, user, limit, false)
	if err != nil {
		return "", "", false, err
	}
	if !c.cut() {
		return c.Choices[0].Message.Content, deployment, false, nil
	}
	wait := timeout
	if d, ok := ctx.Deadline(); ok {
		wait = callTimeout(time.Now(), d, timeout)
	}
	if wait <= 0 {
		return "", "", false, &cutAnswerError{c.limit(limit)}
	}
	oneMoreCall(ctx, wait)
	if c, deployment, err = m.call(ctx, taskID, alias, purpose, wait, system, user, limit, true); err != nil {
		return "", "", false, err
	}
	if c.cut() {
		return "", "", false, &cutAnswerError{c.limit(limit)}
	}
	return c.Choices[0].Message.Content, deployment, true, nil
}

// call is one call of Chat asking limit output tokens, low at low
// thinking depth, cut after timeout.
func (m *SupervisorModel) call(ctx context.Context, taskID, alias, purpose string, timeout time.Duration, system string, user []byte, limit int, low bool) (completion, string, error) {
	payload, err := chatPayload(alias, system, user, limit, low)
	if err != nil {
		return completion{}, "", err
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, deployment, err := postChat(callCtx, m.HTTP, m.GatewayURL, m.Token, taskID, purpose, payload)
	if err != nil && ctx.Err() == nil && callCtx.Err() == context.DeadlineExceeded {
		return completion{}, "", answerTimeout(timeout)
	}
	return c, deployment, err
}

// completion is an unstreamed answer of the gateway's chat door: its
// first choice and why it stopped, the call's cost, and the output limit
// the gateway gave the call (gateway.HeaderOutputLimit), 0 when unnamed.
type completion struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		Cost float64 `json:"cost"`
	} `json:"usage"`
	outputLimit int
}

// cut says the answer stopped at its output limit (finish_reason length).
func (c completion) cut() bool { return c.Choices[0].FinishReason == "length" }

// limit is the output limit the call got, asked when the gateway named
// none.
func (c completion) limit(asked int) int { return cmp.Or(c.outputLimit, asked) }

// postChat posts payload to the chat door of the gateway at url as token,
// on task taskID's account under purpose, and reads its one unstreamed
// completion; deployment is the one that answered, as the gateway names
// it. A gateway refusal is a *policy.Refusal in the chain.
func postChat(ctx context.Context, client *http.Client, url, token, taskID, purpose string, payload []byte) (completion, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return completion{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	req.Header.Set(gateway.HeaderFactoryPurpose, purpose)
	setCard(ctx, req)
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return completion{}, "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return completion{}, "", err
	}
	if res.StatusCode != http.StatusOK {
		if err := gatewayRefused(res.Header, body); err != nil {
			return completion{}, "", err
		}
		return completion{}, "", fmt.Errorf("status %d: %s", res.StatusCode, textutil.Truncate(string(bytes.TrimSpace(body)), 300))
	}
	var c completion
	if err := json.Unmarshal(body, &c); err != nil || len(c.Choices) == 0 {
		return completion{}, "", fmt.Errorf("unreadable completion: %s", textutil.Truncate(string(body), 300))
	}
	c.outputLimit, _ = strconv.Atoi(res.Header.Get(gateway.HeaderOutputLimit))
	return c, res.Header.Get(gateway.HeaderDeployment), nil
}

// cutAnswerError is a model call whose answer stopped at its output limit:
// a reasoning model can think the whole limit away before it answers.
type cutAnswerError struct{ limit int }

func (e *cutAnswerError) Error() string {
	return fmt.Sprintf("the model used its whole answer limit of %d tokens before it answered", e.limit)
}

// answerTimeout is a model call its timeout cut; it is a
// context.DeadlineExceeded to the callers that read one.
type answerTimeout time.Duration

func (d answerTimeout) Error() string {
	return "the model did not answer within " + DurationText(time.Duration(d))
}

func (answerTimeout) Unwrap() error { return context.DeadlineExceeded }

// cardClassesKey carries the classes of the card a call on the project's
// chat task runs for.
type cardClassesKey struct{}

// forCard marks ctx's gateway calls as made for card t: a brief or a
// check-in on the project's chat task names t's classes, so the gateway's
// provider-limit hold holds it under t's class.
func forCard(ctx context.Context, t Task) context.Context {
	return context.WithValue(ctx, cardClassesKey{}, t.RequiredCapabilities)
}

// cardHeader is the X-Factory-Card value of the card ctx carries: its
// classes, comma-separated, "" for none.
func cardHeader(ctx context.Context) string {
	classes, _ := ctx.Value(cardClassesKey{}).([]string)
	return strings.Join(classes, ",")
}

// setCard names the card ctx carries on req, when it carries one.
func setCard(ctx context.Context, req *http.Request) {
	if h := cardHeader(ctx); h != "" {
		req.Header.Set(gateway.HeaderFactoryCard, h)
	}
}
