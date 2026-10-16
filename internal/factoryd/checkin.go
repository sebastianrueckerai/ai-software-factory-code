package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The check-in at a gate return (the escalation ladder's plan D): one model
// conversation under its own alias judges every finding before the next
// session is bought — confirmed, dropped against checkable evidence, or
// remarked — hands the next session a note at the top of its brief, and
// stops the pod when only a human can act. This file is the vocabulary, the
// evidence rule, the three deterministic guards, and the conversation
// runner with its gateway client.

// CheckInGate is the check-in's moment at a gate return: findings came back
// from a round and the next session is not bought yet. Plan E adds the
// in-session and dead-session moments on the same machinery.
const CheckInGate = "gate"

// The check-in's verdict on one finding.
const (
	CheckConfirmed = "confirmed"
	CheckDropped   = "dropped"
	CheckRemark    = "remark"
)

// The check-in's decision: hand a note to the next session, ask a human, or
// drop every finding and verify the commit again.
const (
	CheckHandover   = "handover"
	CheckHuman      = "human"
	CheckDroppedAll = "dropped_all"
)

// The reason kinds of a human ask: only a human can act.
const (
	CheckGoalConflict = "goal_conflict"
	CheckSpend        = "spend_above_policy"
	CheckPolicyClass  = "policy_class"
	CheckAccess       = "access_factory_lacks"
)

// CheckFinding is one finding the check-in judges: its text exactly as the
// review, the session's stop lines or a change request raised it, where it
// came from, and how many earlier check-ins confirmed or dropped it.
type CheckFinding struct {
	Text           string `json:"text"`
	Source         string `json:"source"`
	PriorConfirmed int    `json:"prior_confirmed,omitempty"`
	PriorDropped   int    `json:"prior_dropped,omitempty"`
}

// CheckVerdict is the check-in's verdict on one finding, keyed by its text
// on the review beside the adjudications. Evidence is what a drop stands
// on; Round is the round of the review the verdict rode.
type CheckVerdict struct {
	State    string `json:"state"`
	Evidence string `json:"evidence,omitempty"`
	Round    int    `json:"round,omitempty"`
}

// CheckInAsk is what the workflow hands the session's activity: the moment
// the check-in runs at, the findings to judge, and the review and the gates
// that raised them. ParentSession is the session whose record the check-in
// reads: its pod copy sits at /workspace/.factory/sessions/<ParentSession>.jsonl.
type CheckInAsk struct {
	Moment        string
	ParentSession string
	Findings      []CheckFinding
	Review        *Review
	Gates         *GateResults
}

// CheckInOutcome is the check-in's answer: the decision, the reason and its
// kind when only a human can act, the handover note for the next session's
// brief, the verdicts by finding text, the remarks that blocked nothing,
// and the conversation's own record.
type CheckInOutcome struct {
	Decision   string
	ReasonKind string
	Reason     string
	Handover   string
	Verdicts   map[string]CheckVerdict
	Remarks    []string       `json:",omitempty"`
	Record     *CheckInRecord `json:",omitempty"`
}

// CheckInRecord is one check-in's record for the timeline: the model under
// its alias, the session and the parent session whose record it read, the
// moment, the sizes of the statement and the tail, every call with its
// result size, and the cost and duration of the whole conversation.
type CheckInRecord struct {
	Model          string        `json:"model"`
	Session        string        `json:"session"`
	ParentSession  string        `json:"parent_session"`
	Moment         string        `json:"moment"`
	StatementBytes int           `json:"statement_bytes"`
	TailBytes      int           `json:"tail_bytes"`
	Calls          []CheckCall   `json:"calls,omitempty"`
	ToolCalls      int           `json:"tool_calls"`
	Decision       string        `json:"decision"`
	ReasonKind     string        `json:"reason_kind"`
	Reason         string        `json:"reason"`
	CostEUR        float64       `json:"cost_eur"`
	Duration       time.Duration `json:"duration"`
}

// CheckCall is one call of the check-in's conversation, a model call or a
// tool's execution, with the size of what came back.
type CheckCall struct {
	Name        string        `json:"name"`
	Args        string        `json:"args,omitempty"`
	ResultBytes int           `json:"result_bytes"`
	Duration    time.Duration `json:"duration"`
}

var evidenceFile = regexp.MustCompile(`\S+:\d+ at (?:main|head|[0-9a-f]{7,40}):`)
var evidenceCommand = regexp.MustCompile(`(?:^|\n)\$ .*\n`)

// evidenceOK: the evidence cites a file and line at a ref, or a command
// and its output. Structure only; the truth is the card's to check.
func evidenceOK(s string) bool {
	return evidenceFile.MatchString(s) || evidenceCommand.MatchString(s)
}

// applyVerdictGuards carries the three guards no prompt holds. A drop
// whose evidence fails evidenceOK is no drop: it becomes a confirm that
// names why. A finding dropped again after an earlier drop, and a finding
// confirmed as many times as sessions grants fresh sessions, go to the
// human — the first upgrade wins, and human names it; ok says the verdicts
// stand.
func applyVerdictGuards(findings []CheckFinding, verdicts map[string]CheckVerdict, sessions int) (human string, ok bool) {
	for _, f := range findings {
		v, answered := verdicts[f.Text]
		if !answered {
			continue
		}
		if v.State == CheckDropped && !evidenceOK(v.Evidence) {
			v.State, v.Evidence = CheckConfirmed, "drop without evidence: "+f.Text
			verdicts[f.Text] = v
		}
		if human != "" {
			continue
		}
		switch {
		case v.State == CheckDropped && f.PriorDropped > 0:
			human = "re-raised after a drop"
		case v.State == CheckConfirmed && f.PriorConfirmed >= sessions:
			human = fmt.Sprintf("confirmed %d times", f.PriorConfirmed)
		}
	}
	return human, human == ""
}

// checkinStatement builds the check-in's opening statement: the moment and
// the task, the rules with the evidence a drop stands on and the human
// shapes, the findings with their history, the review and the gates that
// raised them, the tools, the parent session's tail, and the budget left.
func checkinStatement(ask *CheckInAsk, tail []byte, callsLeft int, eurLeft float64, handoverCap int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the factory's check-in at the %s moment: a round came back with findings, and you judge each one before the next session is bought. You never edit anything; you gather evidence and answer.\n\n", ask.Moment)

	b.WriteString("Rules, one verdict per finding:\n")
	b.WriteString("- confirmed: the finding is real and must be fixed.\n")
	b.WriteString("- dropped: only with checkable evidence: a file and line at a ref, or a command and its output.\n")
	b.WriteString("- remark: an opinion: say it, block nothing.\n")
	b.WriteString("- if only a human can act — a goal conflict, spend above policy, a policy class, access the factory lacks — stop and say which, with why.\n")
	fmt.Fprintf(&b, "The handover note to the next session is at most %d characters.\n\n", handoverCap)

	b.WriteString("Findings:\n")
	for i, f := range ask.Findings {
		fmt.Fprintf(&b, "%d. %s (from %s)\n", i+1, f.Text, f.Source)
		switch {
		case f.PriorConfirmed == 1:
			b.WriteString("   confirmed once before\n")
		case f.PriorConfirmed > 1:
			fmt.Fprintf(&b, "   confirmed %d times before\n", f.PriorConfirmed)
		}
		if f.PriorDropped > 0 {
			b.WriteString("   dropped before: keep it dropped only against new evidence\n")
		}
	}
	b.WriteString("\n")

	if ask.Review != nil {
		fmt.Fprintf(&b, "The review that raised them (verdict %s):\n%s\n\n", ask.Review.Verdict, ask.Review.Summary)
	}
	if ask.Gates != nil {
		g := ask.Gates
		b.WriteString("The gates:\n")
		fmt.Fprintf(&b, "- risk class %s is %s", g.Policy.Class, g.Policy.Outcome)
		if g.Policy.MergeRule != "" {
			fmt.Fprintf(&b, " (merge rule %s)", g.Policy.MergeRule)
		}
		b.WriteString("\n")
		if g.Reproduction.Ran {
			fmt.Fprintf(&b, "- reproduction %q exited %d at the head\n", g.Reproduction.Command, g.Reproduction.Exit)
		} else {
			b.WriteString("- no reproduction command\n")
		}
		if g.ChecksPassed {
			b.WriteString("- checks green\n\n")
		} else {
			b.WriteString("- checks not green\n\n")
		}
	}

	b.WriteString("Your tools: transcript_search (search the parent session's record), transcript_read (read a span of it), card_facts (the card's facts), gates (the gates' full results), read_file (a file in the workspace), grep (git grep the workspace at a ref), git_log (the workspace's history), run (a command in the workspace).\n")
	if len(tail) > 0 {
		fmt.Fprintf(&b, "The parent session's whole record sits at /workspace/.factory/sessions/%s.jsonl; its tail:\n<<<SESSION-TAIL>>>\n%s\nSESSION-TAIL>>>\n\n", ask.ParentSession, tail)
	} else {
		b.WriteString("The parent session's record is absent.\n\n")
	}
	fmt.Fprintf(&b, "calls left %d, %.2f EUR left for the check-in on this card", callsLeft, eurLeft)
	return b.String()
}

// cutHandover keeps at most cap runes of the handover note, cut on a rune
// boundary: a note over the cap is cut, never dropped.
func cutHandover(s string, cap int) string {
	r := []rune(s)
	if len(r) <= cap {
		return s
	}
	return string(r[:cap])
}

// The factoryd side of the check-in's own waits and caps, mirroring the
// worker's (cmd/agent-worker/checkin.go): how long a brief or record
// delivery retries while the worker's endpoint is still coming up, and one
// workspace tool's run cap with the transport's slack. Vars, like the
// worker's checkinWait, so a test shortens them.
var (
	checkinBriefWait   = 10 * time.Minute
	checkinToolTimeout = 15*time.Minute + time.Minute
)

// checkinOutputCap cuts every tool answer the check-in's model sees.
const checkinOutputCap = 32 << 10

// cutTail keeps the record's last n bytes, advanced past the partial line
// the cut lands in, so the tail starts where a line does.
func cutTail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	cut := len(b) - n
	if i := bytes.IndexByte(b[cut:], '\n'); i >= 0 {
		cut += i + 1
	}
	return b[cut:]
}

// cutToolOutput caps a local tool's answer the way the worker caps its
// own: a plain byte cut, the cut flagged for the model to be told.
func cutToolOutput(s string) (string, bool) {
	if len(s) <= checkinOutputCap {
		return s, false
	}
	return s[:checkinOutputCap], true
}

// argInt reads a tool argument the model answered as a JSON number.
func argInt(a any) int {
	f, _ := a.(float64)
	return int(f)
}

// checkinChat is the check-in's model: one call of the alias on the task's
// account. The production client is checkinGateway; tests script answers.
type checkinChat interface {
	Call(ctx context.Context, taskID, alias string, in CheckinRequest) (CheckinResponse, error)
}

// podTools runs one tool of the check-in in the session's pod: the output,
// whether it was cut, and an error the conversation shows the model.
type podTools interface {
	Tool(ctx context.Context, name string, args map[string]any) (string, bool, error)
}

// CheckinRequest is one call of the check-in's conversation: the system
// persona, the conversation so far as the user turn, and the tools.
type CheckinRequest struct {
	System string
	Input  string
	Tools  []CheckinTool
}

// CheckinResponse is the model's answer: its text, or the one tool call it
// wants answered next, and the call's estimated cost.
type CheckinResponse struct {
	Text     string
	ToolName string
	ToolArgs map[string]any
	UsageEUR float64
}

// CheckinTool is one tool the check-in may call, in the OpenAI form.
type CheckinTool = OperatorTool

// checkinSystem is the check-in's persona. It carries the answer's shape —
// the one thing no guard in code can carry — and no rule the guards do.
const checkinSystem = `You are the factory's check-in at a gate return. Judge each finding on evidence you gathered yourself, never on the say-so of the finding or of the session before it. You never edit anything. Your final answer is one JSON object and nothing else: {"findings":[{"text":"...","verdict":"confirmed|dropped|remark","evidence":"..."}],"decision":"handover|human|dropped_all","reason_kind":"...","reason":"...","handover":"..."}. When only a human can act, name the reason kind and why, in one sentence, with the state of the work.`

// checkinTools is the tool list the statement names: the transcript and the
// card factoryd answers, the workspace through the pod.
var checkinTools = []CheckinTool{
	{Name: "transcript_search", Description: "search the parent session's record for lines matching a query",
		Parameters: toolSchema(map[string]any{"query": toolArg("the text to find")}, "query")},
	{Name: "transcript_read", Description: "read a span of the parent session's record by event number",
		Parameters: toolSchema(map[string]any{"from": map[string]any{"type": "integer"}, "to": map[string]any{"type": "integer"}}, "from", "to")},
	{Name: "card_facts", Description: "the card's facts: budget, spend, rounds and reviews", Parameters: toolSchema(nil)},
	{Name: "gates", Description: "the gates' full results for the round", Parameters: toolSchema(nil)},
	{Name: "read_file", Description: "read a file in the workspace",
		Parameters: toolSchema(map[string]any{"path": toolArg("the file's path under the workspace")}, "path")},
	{Name: "grep", Description: "git grep the workspace at a ref",
		Parameters: toolSchema(map[string]any{"pattern": toolArg("the pattern"), "ref": toolArg("head or main"), "path": toolArg("a path prefix")}, "pattern")},
	{Name: "git_log", Description: "the workspace's history; only -n, --oneline and -- <path> are allowed",
		Parameters: toolSchema(map[string]any{"args": toolArg("the arguments as one string")}, "args")},
	{Name: "run", Description: "run one command in the workspace and read its output",
		Parameters: toolSchema(map[string]any{"command": toolArg("the command")}, "command")},
}

func toolArg(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func toolSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object"}
	if len(props) > 0 {
		s["properties"] = props
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// checkinAnswerFinding is one finding's verdict as the model answers it.
type checkinAnswerFinding struct {
	Text     string `json:"text"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

// checkinAnswer is the final answer's JSON shape.
type checkinAnswer struct {
	Findings   []checkinAnswerFinding `json:"findings"`
	Decision   string                 `json:"decision"`
	ReasonKind string                 `json:"reason_kind"`
	Reason     string                 `json:"reason"`
	Handover   string                 `json:"handover"`
}

// parseCheckinAnswer reads the model's final answer against the ask: a
// verdict for every finding the ask named, none for a finding it did not,
// and states and decision among the known ones.
func parseCheckinAnswer(text string, ask *CheckInAsk) (checkinAnswer, error) {
	var a checkinAnswer
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		return a, fmt.Errorf("not one JSON object: %v", err)
	}
	switch a.Decision {
	case CheckHandover, CheckHuman, CheckDroppedAll:
	default:
		return a, fmt.Errorf("unknown decision %q", a.Decision)
	}
	known := make(map[string]bool, len(ask.Findings))
	for _, f := range ask.Findings {
		known[f.Text] = true
	}
	seen := make(map[string]bool, len(a.Findings))
	for _, f := range a.Findings {
		if !known[f.Text] {
			return a, fmt.Errorf("unknown finding %q", f.Text)
		}
		switch f.Verdict {
		case CheckConfirmed, CheckDropped, CheckRemark:
		default:
			return a, fmt.Errorf("unknown verdict %q", f.Verdict)
		}
		seen[f.Text] = true
	}
	for _, f := range ask.Findings {
		if !seen[f.Text] {
			return a, fmt.Errorf("no verdict for %q", f.Text)
		}
	}
	return a, nil
}

// runCheckin drives the check-in's conversation: the statement opens it,
// each model call is executed as the tool it asks for until a final answer
// arrives, and the guards turn that into the outcome. A provider that
// refuses every call ends it as a handover with no note and no verdicts —
// the round runs as if no check-in existed. An answer the runner cannot
// read gets one repair round and then a human. The call cap is a human ask
// under spend. Every call lands in the record.
func (r *Runner) runCheckin(ctx context.Context, taskID, alias string, ask *CheckInAsk, parent []byte, chat checkinChat, tools podTools, callsLeft int, eurLeft float64, sessions, handoverCap int) CheckInOutcome {
	start := time.Now()
	rec := &CheckInRecord{Model: alias, ParentSession: ask.ParentSession, Moment: ask.Moment, TailBytes: len(parent)}
	defer func() { rec.Duration = time.Since(start) }()
	conv := checkinStatement(ask, parent, callsLeft, eurLeft, handoverCap)
	rec.StatementBytes = len(conv)

	calls, repairs := 0, 0
	for {
		if calls >= callsLeft {
			const reason = "check-in call cap reached"
			rec.Decision, rec.ReasonKind, rec.Reason = CheckHuman, CheckSpend, reason
			return CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: reason, Record: rec}
		}
		calls++
		at := time.Now()
		resp, err := chat.Call(ctx, taskID, alias, CheckinRequest{System: checkinSystem, Input: conv, Tools: checkinTools})
		rec.Calls = append(rec.Calls, CheckCall{Name: alias, ResultBytes: len(resp.Text), Duration: time.Since(at)})
		rec.CostEUR += resp.UsageEUR
		if err != nil {
			rec.Decision, rec.Reason = CheckHandover, "check-in calls refused: "+err.Error()
			return CheckInOutcome{Decision: CheckHandover, Record: rec}
		}
		if resp.ToolName != "" {
			args, _ := json.Marshal(resp.ToolArgs)
			at := time.Now()
			out, cut, terr := tools.Tool(ctx, resp.ToolName, resp.ToolArgs)
			d := time.Since(at)
			if terr != nil {
				out = "tool error: " + terr.Error()
			} else if cut {
				out += "\n(output cut)"
			}
			rec.Calls = append(rec.Calls, CheckCall{Name: resp.ToolName, Args: textutil.Truncate(string(args), 512), ResultBytes: len(out), Duration: d})
			rec.ToolCalls++
			conv += fmt.Sprintf("\n\ntool %s %s answered:\n%s", resp.ToolName, args, out)
			continue
		}
		a, perr := parseCheckinAnswer(resp.Text, ask)
		if perr == nil {
			return checkinOutcome(rec, ask, a, handoverCap, sessions)
		}
		if repairs == 1 {
			reason := "check-in answer unreadable: " + perr.Error()
			rec.Decision, rec.Reason = CheckHuman, reason
			return CheckInOutcome{Decision: CheckHuman, Reason: reason, Record: rec}
		}
		repairs++
		conv += "\n\nYour last answer was unreadable: " + perr.Error() + " Answer again as one JSON object."
	}
}

// checkinOutcome turns a readable final answer into the outcome: the
// verdicts and remarks as answered, the guards' upgrade to a human first,
// a dropped_all with a finding left standing a handover, and the handover
// note cut at its cap.
func checkinOutcome(rec *CheckInRecord, ask *CheckInAsk, a checkinAnswer, handoverCap, sessions int) CheckInOutcome {
	verdicts := make(map[string]CheckVerdict, len(a.Findings))
	var remarks []string
	for _, f := range a.Findings {
		verdicts[f.Text] = CheckVerdict{State: f.Verdict, Evidence: f.Evidence}
		if f.Verdict == CheckRemark {
			remarks = append(remarks, f.Text)
		}
	}
	decision := a.Decision
	if human, _ := applyVerdictGuards(ask.Findings, verdicts, sessions); human != "" && decision != CheckHuman {
		rec.Decision, rec.ReasonKind, rec.Reason = CheckHuman, "", human
		return CheckInOutcome{Decision: CheckHuman, Reason: human, Verdicts: verdicts, Remarks: remarks, Record: rec}
	}
	if decision == CheckDroppedAll {
		for _, v := range verdicts {
			if v.State != CheckDropped {
				decision = CheckHandover
				break
			}
		}
	}
	out := CheckInOutcome{Decision: decision, ReasonKind: a.ReasonKind, Reason: a.Reason, Verdicts: verdicts, Remarks: remarks, Record: rec}
	if decision == CheckHandover {
		out.Handover = cutHandover(a.Handover, handoverCap)
	}
	rec.Decision, rec.ReasonKind, rec.Reason = out.Decision, out.ReasonKind, out.Reason
	return out
}

// checkinGateway is the production checkinChat: the gateway's chat
// completions under purpose checkin, unstreamed, the answer's first tool
// call read beside its text. The cost is the response's own figure when the
// upstream echoes one — the ledger books the true cost.
type checkinGateway struct {
	URL, Token string
	HTTP       *http.Client
}

func (g *checkinGateway) Call(ctx context.Context, taskID, alias string, in CheckinRequest) (CheckinResponse, error) {
	body := map[string]any{
		"model":    alias,
		"messages": []map[string]string{{"role": "system", "content": in.System}, {"role": "user", "content": in.Input}},
		"stream":   false,
	}
	if len(in.Tools) > 0 {
		body["tools"] = in.Tools
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return CheckinResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return CheckinResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.HeaderFactoryTask, taskID)
	req.Header.Set(gateway.HeaderFactoryPurpose, policy.PurposeCheckin)
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return CheckinResponse{}, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return CheckinResponse{}, err
	}
	if res.StatusCode != http.StatusOK {
		var refused struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if d := res.Header.Get(gateway.HeaderDecision); d != "" && json.Unmarshal(raw, &refused) == nil {
			return CheckinResponse{}, fmt.Errorf("gateway %s: %w", d, &policy.Refusal{Decision: d, Reason: refused.Error.Message})
		}
		return CheckinResponse{}, fmt.Errorf("status %d: %s", res.StatusCode, textutil.Truncate(string(bytes.TrimSpace(raw)), 300))
	}
	var out struct {
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
		} `json:"choices"`
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return CheckinResponse{}, fmt.Errorf("unreadable completion: %s", textutil.Truncate(string(raw), 300))
	}
	msg := out.Choices[0].Message
	resp := CheckinResponse{Text: msg.Content, UsageEUR: out.Usage.Cost}
	if len(msg.ToolCalls) > 0 {
		resp.ToolName = msg.ToolCalls[0].Function.Name
		if args := msg.ToolCalls[0].Function.Arguments; args != "" {
			if err := json.Unmarshal([]byte(args), &resp.ToolArgs); err != nil {
				return CheckinResponse{}, fmt.Errorf("unreadable tool arguments: %w", err)
			}
		}
	}
	return resp, nil
}
