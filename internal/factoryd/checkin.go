package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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

// CheckInSession is the check-in's moment inside a running session: a
// detector tripped and the session's next step is the decision.
const CheckInSession = "session"

// CheckInDead is the check-in's moment before a fresh session: the session
// before it ended without a result and its handover is still unwritten.
const CheckInDead = "dead"

// The check-in's verdict on one finding.
const (
	CheckConfirmed = "confirmed"
	CheckDropped   = "dropped"
	CheckRemark    = "remark"
)

// The check-in's decision: hand a note to the next session, ask a human,
// drop every finding and verify the commit again, or inside a session of a
// card that can be cut, have the planner cut its rest.
const (
	CheckHandover   = "handover"
	CheckHuman      = "human"
	CheckDroppedAll = "dropped_all"
	CheckReplan     = "replan"
)

// The reason kinds of a human ask: only a human can act.
const (
	CheckGoalConflict = "goal_conflict"
	CheckSpend        = "spend_above_policy"
	CheckPolicyClass  = "policy_class"
	CheckAccess       = "access_factory_lacks"
)

// The reason kinds of a check-in that failed: the factory's own failure,
// not a stop only a human can act on. Its cap on time or calls, or an
// answer it could not give in a readable shape.
const (
	CheckTimeCap    = "checkin_time_cap"
	CheckCallCap    = "checkin_call_cap"
	CheckUnreadable = "checkin_unreadable"
)

// checkinFailedKind says kind is a check-in that failed.
func checkinFailedKind(kind string) bool {
	return kind == CheckTimeCap || kind == CheckCallCap || kind == CheckUnreadable
}

// stopFamily is the kind the repeat guard compares: a stop recorded before
// the failures had kinds of their own carries spend on a cap and no kind on
// an unreadable answer.
func stopFamily(kind string) string {
	switch kind {
	case CheckTimeCap, CheckCallCap:
		return CheckSpend
	case CheckUnreadable:
		return ""
	}
	return kind
}

// checkinSpendKind says kind is a check-in stop on the card's spend or on a
// cap of the check-in itself, whose answer buys the session or a share: a
// run recorded before the caps had kinds of their own carries CheckSpend
// on them.
func checkinSpendKind(kind string) bool {
	return kind == CheckSpend || kind == CheckTimeCap || kind == CheckCallCap
}

// The reason a CheckSpend stop carries on the budget's share: the share
// of the card's budget the check-in may spend is spent. A human's answer
// to it grants the card one more share of the check-in budget
// (versionCheckinGrant); the call cap carries checkinCallCapStop, whose
// answer buys the session alone (versionCheckinRaise).
const CheckinShareSpent = "the check-in's share of the budget is spent"

// CheckFinding is one finding the check-in judges: its text exactly as the
// review, the session's stop lines or a change request raised it, where it
// came from, and how many earlier check-ins confirmed or dropped it. Its
// place in the ask is its stable short id (findingKey), which the statement
// names beside the text and the answer echoes instead of the wording.
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
	Trip          *TripReport `json:",omitempty"`
	Messages      []string    `json:",omitempty"`
	Stop          string      `json:",omitempty"`
	// Replan: the session's card can be cut (cuttable), so the decision
	// replan is offered
	Replan    bool       `json:",omitempty"`
	Pace      *Pace      `json:"-"`
	CostState *CostState `json:"-"`
}

// Pace is how far the card has come against what it has: its changed lines
// against the bound, its spend against the budget, and its time. Lines < 0
// means not measured.
type Pace struct {
	Lines, BoundLines                            int
	SpentEUR, BudgetEUR                          float64
	ReserveEUR                                   float64 // the gate reserve a session's room holds out
	SpentMinutes, LeftMinutes, Attempt, Sessions int
}

// Line is the pace as the statement's one sentence.
func (p Pace) Line() string {
	lines := "changed lines are not measured at this moment"
	if p.Lines >= 0 {
		lines = fmt.Sprintf("%d changed lines so far against its bound of %d", p.Lines, p.BoundLines)
	}
	return fmt.Sprintf("Pace of the card: %s; %.2f EUR spent, %.2f left; %d minutes of agent time spent, %d left; session %d of %d.",
		lines, p.SpentEUR, max(0, policy.SessionHeadroom(p.BudgetEUR-p.SpentEUR, p.ReserveEUR)), p.SpentMinutes, max(0, p.LeftMinutes), p.Attempt, p.Sessions)
}

// CheckInOutcome is the check-in's answer: the decision, the reason and its
// kind when only a human can act, the handover note for the next session's
// brief, the verdicts by finding text, the remarks that blocked nothing,
// and the conversation's own record.
type CheckInOutcome struct {
	Decision   string
	ReasonKind string
	Reason     string
	Guidance   string `json:",omitempty"`
	Handover   string
	Verdicts   map[string]CheckVerdict
	Remarks    []string       `json:",omitempty"`
	Record     *CheckInRecord `json:",omitempty"`
	// Findings and Review are the work state a stop carries when no
	// judgment ran: the ask's findings and the last review verdict, so a
	// fabricated stop names the work, not the spend alone
	Findings []CheckFinding `json:",omitempty"`
	Review   *Review        `json:",omitempty"`
	// Share is the card's check-in share in euros when the check-in ran:
	// the policy's share of the budget, widened by every granted share
	Share float64 `json:",omitempty"`
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
// the task, the writing guide, the rules with the evidence a drop stands
// on and the human shapes, the findings with their history, the review
// and the gates that raised them, the tools, the parent session's tail,
// and the budget left.
func checkinStatement(ask *CheckInAsk, tail []byte, callsLeft int, eurLeft float64, handoverCap int) string {
	var b strings.Builder
	switch ask.Moment {
	case CheckInSession:
		fmt.Fprintf(&b, "You are the factory's check-in inside session %s: the detector %s tripped (%s), and you decide what happens before the session continues. You never edit anything; you gather evidence and answer.\n\n", ask.ParentSession, ask.Trip.Detector, ask.Trip.Detail)
		b.WriteString(writingguide.Guide)
		b.WriteString("\n")
		b.WriteString("Rules:\n")
		b.WriteString("- continue: the session runs on.\n")
		b.WriteString("- message: guidance carries the exact instruction the agent should take next.\n")
		fmt.Fprintf(&b, "- handover: the session stops; the note to the next session is at most %d characters.\n", handoverCap)
		b.WriteString("- human: if only a human can act — a goal conflict, spend above policy, a policy class, access the factory lacks — stop and say which, with why.\n")
		if ask.Replan {
			b.WriteString("- replan: the session stops, its work is pushed and kept, and the planner cuts the card's remaining work into smaller cards; the first one lands what the branch holds. Say why in reason; guidance may name the parts.\n")
		}
		if len(ask.Messages) > 0 {
			b.WriteString("Guidance delivered to the agent this session:\n")
			for _, m := range ask.Messages {
				fmt.Fprintf(&b, "- %s\n", m)
			}
		}
		if ask.Trip.Stuck != "" {
			// the kind only: the trip's detail can quote the agent's call
			if k := ask.Trip.Detector; k == TripFlipFlop || k == TripChurn {
				fmt.Fprintf(&b, "Stuck rule: after a message, the %s detector tripped.\n", k)
			} else {
				fmt.Fprintf(&b, "Stuck rule: after the message, the %s detector tripped with no file written since.\n", k)
			}
			if ask.Trip.Climbs {
				b.WriteString("- on this trip a handover moves the work to the stronger model, with your note as its next instruction.\n")
			} else {
				b.WriteString("- on this trip a handover ends the session, and the next session starts from your note.\n")
			}
		}
		if ask.Trip.Detector == TripSize {
			b.WriteString("The size trip asks one question: will the rest of the card fit in its bound, its budget and its time? Answer continue if it will. Answer message if the agent should wrap up, with the instruction.")
			if ask.Replan {
				b.WriteString(" Answer replan if it will not.")
			}
			b.WriteString("\n")
		}
		if ask.Trip.Detector == TripAgentCut {
			b.WriteString("The agent asks to cut the card. Answer replan if the cut holds. Answer continue or message if the card should finish as one card.\n")
		}
		b.WriteString("The agent's report per finding is the last message in its record: what it fixed, disputed or left open. Verify it; never take it on trust.\n\n")
	case CheckInDead:
		fmt.Fprintf(&b, "You are the factory's check-in before a fresh session: the session %s ended without a result: %s. You never edit anything; you gather evidence and answer.\n\n", ask.ParentSession, ask.Stop)
		fmt.Fprintf(&b, "Your handover note to the next session is at most %d characters. It names: what the earlier session did, the state of the branch, each finding with its verdict and the reason, what to do first, and what not to repeat. The agent's report per finding is the last message in its record; verify it, never take it on trust.\n\n", handoverCap)
	default:
		fmt.Fprintf(&b, "You are the factory's check-in at the %s moment: a round came back with findings, and you judge each one before the next session is bought. You never edit anything; you gather evidence and answer.\n\n", ask.Moment)
		b.WriteString(writingguide.Guide)
		b.WriteString("\n")

		b.WriteString("Rules, one verdict per finding:\n")
		b.WriteString("- confirmed: the finding is real and must be fixed.\n")
		b.WriteString("- dropped: only with checkable evidence: a file and line at a ref, or a command and its output.\n")
		b.WriteString("- remark: an opinion: say it, block nothing.\n")
		b.WriteString("- if only a human can act — a goal conflict, spend above policy, a policy class, access the factory lacks — stop and say which, with why.\n")
		fmt.Fprintf(&b, "The handover note to the next session is at most %d characters.\n\n", handoverCap)

		b.WriteString("Findings:\n")
		for i, f := range ask.Findings {
			fmt.Fprintf(&b, "%s: %s (from %s)\n", findingKey(i), f.Text, f.Source)
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

		b.WriteString(checkinToolsLine + "\n")
	}
	if ask.Pace != nil {
		b.WriteString(ask.Pace.Line() + "\n\n")
	}
	if ask.CostState != nil {
		fmt.Fprintf(&b, "Cost and state: %s\n\n", ask.CostState.Line())
	}
	if len(tail) > 0 {
		// the session moment tails the live session's own record, which no
		// push copied into the pod; the other moments' parents sit there
		if ask.Moment == CheckInSession {
			fmt.Fprintf(&b, "The live session's record so far, its tail:\n<<<SESSION-TAIL>>>\n%s\nSESSION-TAIL>>>\n\n", tail)
		} else {
			fmt.Fprintf(&b, "The parent session's whole record sits at /workspace/.factory/sessions/%s.jsonl; its tail:\n<<<SESSION-TAIL>>>\n%s\nSESSION-TAIL>>>\n\n", ask.ParentSession, tail)
		}
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
// persona, the conversation so far as the user turn, the tools, and
// tool_choice when the call forces one of them.
type CheckinRequest struct {
	System     string
	Input      string
	Tools      []CheckinTool
	ToolChoice any
}

// CheckinResponse is the model's answer: its text, or the one tool call it
// wants answered next, and the call's estimated cost.
type CheckinResponse struct {
	Text     string
	ToolName string
	ToolArgs map[string]any
	// ToolErr and ToolRaw are the parse error and the raw text of a tool
	// call whose arguments were not one JSON object; the loop scrubs them
	// before the model or the record sees them.
	ToolErr, ToolRaw string
	UsageEUR         float64
}

// CheckinTool is one tool the check-in may call, in the OpenAI form.
type CheckinTool = OperatorTool

// checkinSystem is the check-in's persona. It carries the answer's shape —
// the one thing no guard in code can carry — and no rule the guards do.
// Findings are named by the id the statement listed them under, so a
// verdict never turns on the wording of the finding it judges.
const checkinSystem = `You are the factory's check-in at a gate return. Judge each finding on evidence you gathered yourself, never on the say-so of the finding or of the session before it. You never edit anything. Your final answer is one JSON object and nothing else: {"findings":[{"id":"f1","verdict":"confirmed|dropped|remark","evidence":"..."}],"decision":"handover|human|dropped_all","reason_kind":"...","reason":"...","handover":"..."}. Each finding is named by the id the findings list gave it, f1 on down, not by its text. When only a human can act, name the reason kind and why, in one sentence, with the state of the work.`

// checkinSystemFor is the moment's persona: the gate's, or the one that
// fits what the moment decides. Inside a session, replan offers the
// decision replan; every other moment ignores it.
func checkinSystemFor(moment string, replan bool) string {
	switch moment {
	case CheckInSession:
		decisions := "continue|message|handover|human"
		if replan {
			decisions += "|" + CheckReplan
		}
		return `You are the factory's check-in inside a running session: its detectors tripped and you decide what happens next, on evidence you gather yourself from the live workspace and the session's record. You never edit anything. Your final answer is one JSON object and nothing else: {"decision":"` + decisions + `","guidance":"...","reason_kind":"...","reason":"...","handover":"..."}. "message" needs guidance: the exact instruction the agent should take next. "handover" stops the session: write the note the next session starts from. When only a human can act, name the reason kind and why, in one sentence, with the state of the work.`
	case CheckInDead:
		return `You are the factory's check-in before a fresh session: the session before you ended without a result, and you write the handover it starts from. Read its record and the workspace as they are; you never edit anything. Your final answer is one JSON object and nothing else: {"decision":"handover|human","reason_kind":"...","reason":"...","handover":"..."}. When only a human can act, name the reason kind and why, in one sentence, with the state of the work.`
	}
	return checkinSystem
}

// checkinToolsLine is the statement's tool line: the evidence tools the
// check-in's calls post beside the answer tool, one line.
const checkinToolsLine = "Your tools: transcript_search (search the parent session's record), transcript_read (read a span of it), card_facts (the card's digest and facts), card_step (one step of the digest in full), gates (the gates' full results), read_file (a file in the workspace), grep (git grep the workspace at a ref), git_log (the workspace's history), run (a command in the workspace)."

// momentDecisions is one moment's closed decision vocabulary: the gate
// judges findings and may drop them all, the session decides the
// session's next step, the dead one writes the note or asks. The same
// schema mechanism as the rung's answerDecisions, on this vocabulary.
func momentDecisions(moment string, replan bool) []string {
	switch moment {
	case CheckInSession:
		ds := []string{"continue", "message", CheckHandover, CheckHuman}
		if replan {
			ds = append(ds, CheckReplan)
		}
		return ds
	case CheckInDead:
		return []string{CheckHandover, CheckHuman}
	}
	return []string{CheckHandover, CheckHuman, CheckDroppedAll}
}

// checkReasonKinds are the moments' closed reason-kind vocabulary: the
// shapes of a stop only a human can act on. An answer that names no
// reason kind answers "", which the enum holds.
var checkReasonKinds = []string{"", CheckGoalConflict, CheckSpend, CheckPolicyClass, CheckAccess}

// checkVerdicts is the closed verdict vocabulary of a finding's verdict.
var checkVerdicts = []string{CheckConfirmed, CheckDropped, CheckRemark}

// enumCheck refuses value unless the closed enum of holds it, naming the
// kind, the value and the allowed values. It closes the moments' answers
// (parseCheckinAnswer).
func enumCheck(kind, value string, of []string) error {
	if slices.Contains(of, value) {
		return nil
	}
	allowed := slices.DeleteFunc(slices.Clone(of), func(v string) bool { return v == "" })
	return fmt.Errorf("unknown %s %q; allowed: %s", kind, value, strings.Join(allowed, ", "))
}

// checkinAnswerTool is the answer tool's name: every check-in call posts
// it beside the evidence tools, and the last call forces it with
// tool_choice, so the answer arrives as its arguments against a schema.
const checkinAnswerTool = "answer"

// forcedToolChoice is tool_choice in its named-function form: the call
// answers through the named tool and through nothing else.
func forcedToolChoice(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
}

// momentAnswerSchema is the moments' answer schema: the same mechanism as
// the rung's AnswerSchema, on the moment's own vocabulary — the decision
// and the reason kind closed enums, the verdicts closed where findings
// are judged, the note, the reason and the guidance as text.
func momentAnswerSchema(ask *CheckInAsk) map[string]any {
	props := map[string]any{
		"decision":    map[string]any{"type": "string", "enum": momentDecisions(ask.Moment, ask.Replan)},
		"reason_kind": map[string]any{"type": "string", "enum": slices.Clone(checkReasonKinds)},
		"reason":      map[string]any{"type": "string"},
		"handover":    map[string]any{"type": "string"},
		"guidance":    map[string]any{"type": "string"},
	}
	if ask.Moment != CheckInSession && ask.Moment != CheckInDead {
		props["findings"] = map[string]any{"type": "array", "items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":       map[string]any{"type": "string"},
				"text":     map[string]any{"type": "string"},
				"verdict":  map[string]any{"type": "string", "enum": slices.Clone(checkVerdicts)},
				"evidence": map[string]any{"type": "string"},
			},
		}}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"decision"}, "additionalProperties": false}
}

// momentAnswerTool is the answer tool the check-in's calls post.
func momentAnswerTool(ask *CheckInAsk) CheckinTool {
	return CheckinTool{Name: checkinAnswerTool, Description: "the check-in's final answer, one JSON object as this tool's arguments",
		Parameters: momentAnswerSchema(ask)}
}

// checkinTools is the tool list the statement names: the transcript and the
// card factoryd answers, the workspace through the pod.
var checkinTools = []CheckinTool{
	{Name: "transcript_search", Description: "search the parent session's record for lines holding a text, exact and case-sensitive",
		Parameters: toolSchema(map[string]any{"query": toolArg("the text to find")}, "query")},
	{Name: "transcript_read", Description: "read a span of the parent session's record by event number",
		Parameters: toolSchema(map[string]any{"from": map[string]any{"type": "integer"}, "to": map[string]any{"type": "integer"}}, "from", "to")},
	{Name: "card_facts", Description: "the card's digest, its steps and flags, then its budget, spend, attempt and sessions, and the interim push's head when one landed", Parameters: toolSchema(nil)},
	{Name: "card_step", Description: "one step of the card's digest in full, by the number its line starts with",
		Parameters: toolSchema(map[string]any{"step": map[string]any{"type": "integer", "minimum": 1, "description": "the step's number"}}, "step")},
	{Name: "gates", Description: "the gates' full results for the round", Parameters: toolSchema(nil)},
	{Name: "read_file", Description: "read a file of the repository by its path, as grep prints it",
		Parameters: toolSchema(map[string]any{"path": toolArg("the file's path in the repository, or absolute under /workspace")}, "path")},
	{Name: "grep", Description: "git grep the repository at a ref for a fixed string as a whole word, no regex",
		Parameters: toolSchema(map[string]any{"pattern": toolArg("the fixed string"), "ref": toolArg("head or main"), "path": toolArg("a path prefix")}, "pattern")},
	{Name: "git_log", Description: "the workspace's history; only -n, --oneline and -- <path> are allowed",
		Parameters: toolSchema(map[string]any{"args": toolArg("the arguments as one string")}, "args")},
	{Name: "run", Description: "run one command in the workspace and read its output",
		Parameters: toolSchema(map[string]any{"command": toolArg("the command")}, "command")},
}

// checkinToolsFor is the moment's tool list: the session's check-in reads
// a live workspace and gets no run.
func checkinToolsFor(moment string) []CheckinTool {
	if moment == CheckInSession {
		return checkinTools[:len(checkinTools)-1]
	}
	return checkinTools
}

// toolShape lists the named tool's parameters, the required ones marked.
func toolShape(tools []CheckinTool, name string) string {
	for _, t := range tools {
		if t.Name != name {
			continue
		}
		params, _ := t.Parameters.(map[string]any)
		props, _ := params["properties"].(map[string]any)
		required, _ := params["required"].([]string)
		names := slices.Sorted(maps.Keys(props))
		for i, n := range names {
			if slices.Contains(required, n) {
				names[i] = n + " (required)"
			}
		}
		if len(names) == 0 {
			return "no parameters"
		}
		return strings.Join(names, ", ")
	}
	return "see the tool list"
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

// checkinAnswerFinding is one finding's verdict as the model answers it:
// named by the finding's id when it echoes one, with the text beside it.
type checkinAnswerFinding struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

// checkinAnswer is the final answer's JSON shape.
type checkinAnswer struct {
	Findings   []checkinAnswerFinding `json:"findings"`
	Decision   string                 `json:"decision"`
	Guidance   string                 `json:"guidance"`
	ReasonKind string                 `json:"reason_kind"`
	Reason     string                 `json:"reason"`
	Handover   string                 `json:"handover"`
}

// findingKey is one finding's stable short id: its place in the ask, f1 on
// down. The statement names it beside the text and the answer echoes it, so
// a verdict never turns on a long finding's wording.
func findingKey(i int) string { return fmt.Sprintf("f%d", i+1) }

// foldFindingText folds a finding's text for the parser's last resort: case
// and every run of whitespace and punctuation become one space, so a
// verdict that re-punctuates or re-spaces the finding still names it. A
// reworded finding still needs its id.
func foldFindingText(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// parseCheckinAnswer reads the model's final answer against the ask: the
// moment's decisions, the reason kinds and every verdict held to their
// closed enums, each refusal naming what broke; at the session moment a
// message's guidance, a handover's note and a replan's reason (replan
// only where the ask offers it), and at the gate a verdict for every
// finding the ask named, by its id, by its text exactly, or by its text
// with whitespace and punctuation folded, none for a finding it did not.
// Every verdict lands keyed by the ask's own text, whatever wording the
// answer carried.
func parseCheckinAnswer(text string, ask *CheckInAsk) (checkinAnswer, error) {
	var a checkinAnswer
	if err := json.Unmarshal([]byte(text), &a); err != nil {
		return a, fmt.Errorf("not one JSON object: %v", err)
	}
	if err := enumCheck("decision", a.Decision, momentDecisions(ask.Moment, ask.Replan)); err != nil {
		return a, err
	}
	if err := enumCheck("reason_kind", a.ReasonKind, checkReasonKinds); err != nil {
		return a, err
	}
	if ask.Moment == CheckInSession {
		if a.Decision == "message" && strings.TrimSpace(a.Guidance) == "" {
			return a, fmt.Errorf("a message needs its guidance")
		}
		if a.Decision == CheckReplan && strings.TrimSpace(a.Reason) == "" {
			return a, fmt.Errorf("a replan needs its reason")
		}
		// a note-less handover is the shape of a refused call (checkinLive)
		if a.Decision == CheckHandover && strings.TrimSpace(a.Handover) == "" {
			return a, fmt.Errorf("a handover needs the note to the next session")
		}
	}
	byID := make(map[string]string, len(ask.Findings))
	byText := make(map[string]bool, len(ask.Findings))
	byFold := make(map[string]string, len(ask.Findings))
	for i, f := range ask.Findings {
		byID[findingKey(i)] = f.Text
		byText[f.Text] = true
		folded := foldFindingText(f.Text)
		if _, dup := byFold[folded]; !dup {
			byFold[folded] = f.Text
		}
	}
	seen := make(map[string]bool, len(a.Findings))
	for i := range a.Findings {
		answered := a.Findings[i]
		canonical, known := byID[strings.ToLower(strings.TrimSpace(answered.ID))]
		if !known && byText[answered.Text] {
			canonical, known = answered.Text, true
		}
		if !known {
			canonical, known = byFold[foldFindingText(answered.Text)]
		}
		if !known {
			name := answered.Text
			if name == "" {
				name = answered.ID
			}
			return a, fmt.Errorf("unknown finding %q", name)
		}
		if err := enumCheck("verdict", answered.Verdict, checkVerdicts); err != nil {
			return a, err
		}
		a.Findings[i].Text = canonical
		seen[canonical] = true
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
// read gets one repair round and then fails the check-in (checkin_unreadable), at once on the last call.
// Every call posts the answer tool beside the evidence tools; the last
// call posts only it and forces it with tool_choice, so the answer
// arrives as its arguments against the moment's schema. A call of the
// answer tool is the answer, parsed wherever it comes; any other tool
// asked for on the last call is the call cap's stop (checkin_call_cap).
// The last call comes at the call cap or once an evidence call as long as
// the longest so far would not end before the answer is due, whichever is
// first, and an evidence call still running when the answer is due is
// cut. An answer call the cap cuts fails it at its time cap
// (checkin_time_cap). Every call lands in the record.
func (r *Runner) runCheckin(ctx context.Context, taskID, alias string, ask *CheckInAsk, parent []byte, chat checkinChat, tools podTools, callsLeft int, eurLeft float64, sessions, handoverCap int, timeCap time.Duration) CheckInOutcome {
	start := time.Now()
	rec := &CheckInRecord{Model: alias, ParentSession: ask.ParentSession, Moment: ask.Moment, TailBytes: len(parent)}
	defer func() { rec.Duration = time.Since(start) }()
	var parsed checkinAnswer
	end := converse(ctx, chat, tools, taskID, alias, conversation{
		System:    checkinSystemFor(ask.Moment, ask.Replan),
		Statement: checkinStatement(ask, parent, callsLeft, eurLeft, handoverCap),
		Evidence:  checkinToolsFor(ask.Moment),
		Answer:    momentAnswerTool(ask),
		Parse: func(text string) error {
			a, err := parseCheckinAnswer(text, ask)
			parsed = a
			return err
		},
		CallCap: callsLeft,
		TimeCap: timeCap,
		Scrub:   r.scrub,
	})
	rec.StatementBytes = end.StatementBytes
	rec.Calls = end.Calls
	rec.ToolCalls = end.ToolCalls
	rec.CostEUR = end.CostEUR
	stop := func(kind, reason string) CheckInOutcome {
		rec.Decision, rec.ReasonKind, rec.Reason = CheckHuman, kind, reason
		return CheckInOutcome{Decision: CheckHuman, ReasonKind: kind, Reason: reason, Record: rec}
	}
	switch end.Kind {
	case convTimeCap:
		return stop(CheckTimeCap, checkinCapStop(checkinTimeCapStop, timeCap.String()))
	case convCallCap:
		return stop(CheckCallCap, checkinCapStop(checkinCallCapStop, fmt.Sprintf("%d calls", callsLeft)))
	case convRefused:
		rec.Decision, rec.Reason = CheckHandover, "check-in calls refused: "+end.Detail
		return CheckInOutcome{Decision: CheckHandover, Record: rec}
	case convUnreadable:
		return stop(CheckUnreadable, "check-in answer unreadable: "+end.Detail)
	}
	return checkinOutcome(rec, ask, parsed, handoverCap, sessions)
}

// conversation is one forced-call conversation: the persona, the
// statement that opens it, the evidence tools every call posts beside the
// answer tool, the parser of the answer's arguments, and the caps.
type conversation struct {
	System, Statement string
	Evidence          []CheckinTool
	Answer            CheckinTool
	Parse             func(text string) error
	CallCap           int
	TimeCap           time.Duration
	Scrub             func(string) string
	// RepairPastCap gives a refusal of the forced last call its one repair
	// call past the call cap.
	RepairPastCap bool
	// DueOwnLine puts the calls left and the answer's due time on a line of
	// their own after the statement, which then ends at its own sentence.
	DueOwnLine bool
}

type convKind int

const (
	convAnswered convKind = iota
	convTimeCap
	convCallCap
	convRefused
	convUnreadable
)

// convEnd is how a conversation ended. Detail is the scrubbed text of the
// last refusal or parse error, Refusals every parse error in order, and
// Evidence the evidence calls among Calls.
type convEnd struct {
	Kind           convKind
	Detail         string
	Refusals       []string
	Calls          []CheckCall
	Evidence       []CheckCall
	ToolCalls      int
	CostEUR        float64
	StatementBytes int
}

// converse drives a conversation: the statement opens it, each model call
// is executed as the tool it asks for until the answer tool's call
// arrives, and the answer is parsed. An answer the parser refuses gets one
// repair round, then ends the conversation unreadable, at once on the last
// call. Every call posts the answer tool beside the evidence tools; the
// last call posts only it and forces it with tool_choice. Any other tool
// asked for on the last call is the call cap's end. The last call comes at
// the call cap or once an evidence call as long as the longest so far
// would not end before the answer is due, whichever is first, and an
// evidence call still running when the answer is due is cut. A call the
// time cap cuts ends it at the time cap. A provider that refuses a call
// ends it refused. Every call lands in the result.
func converse(ctx context.Context, chat checkinChat, tools podTools, taskID, alias string, c conversation) convEnd {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, c.TimeCap)
	defer cancel()
	var end convEnd
	var longest time.Duration
	due := func() time.Time { return start.Add(checkinAnswerDue(c.TimeCap, longest)) }
	dueLine := func() string {
		if c.DueOwnLine {
			return fmt.Sprintf("\nCalls left %d; your answer is due within %s.", c.CallCap, max(0, time.Until(due())).Round(time.Second))
		}
		return checkinDueLine(time.Until(due()))
	}
	conv := c.Statement + dueLine()
	end.StatementBytes = len(conv)
	finish := func(k convKind, detail string) convEnd {
		end.Kind, end.Detail = k, detail
		return end
	}

	calls, repairs := 0, 0
	for {
		if time.Since(start) >= c.TimeCap {
			return finish(convTimeCap, "")
		}
		last := calls >= c.CallCap-1 || time.Until(due()) < longest
		if last {
			conv += "\n\nThis is your last call: answer now through the answer tool."
		}
		req := CheckinRequest{System: c.System, Input: conv}
		callCtx, cancelCall := ctx, context.CancelFunc(func() {})
		if last {
			req.Tools = []CheckinTool{c.Answer}
			req.ToolChoice = forcedToolChoice(c.Answer.Name)
		} else {
			req.Tools = slices.Concat(c.Evidence, []CheckinTool{c.Answer})
			callCtx, cancelCall = context.WithDeadline(ctx, due())
		}
		calls++
		at := time.Now()
		resp, err := chat.Call(callCtx, taskID, alias, req)
		cancelCall()
		d := time.Since(at)
		longest = max(longest, d)
		end.Calls = append(end.Calls, CheckCall{Name: alias, ResultBytes: len(resp.Text), Duration: d})
		end.CostEUR += resp.UsageEUR
		if err != nil {
			if time.Since(start) >= c.TimeCap {
				return finish(convTimeCap, "")
			}
			if !last && ctx.Err() == nil && !time.Now().Before(due()) {
				continue // the evidence call ran into the answer's reserve
			}
			return finish(convRefused, oneLine(c.Scrub(err.Error())))
		}
		answer := resp.Text
		if resp.ToolName != "" {
			if resp.ToolName != c.Answer.Name {
				if last {
					return finish(convCallCap, "")
				}
				args, _ := json.Marshal(resp.ToolArgs)
				at := time.Now()
				var out string
				var cut bool
				var terr error
				if !slices.ContainsFunc(c.Evidence, func(t CheckinTool) bool { return t.Name == resp.ToolName }) {
					terr = fmt.Errorf("%s is not a tool of this conversation; its tools are %s", c.Scrub(resp.ToolName), textutil.AndList(append(toolNames(c.Evidence), c.Answer.Name)))
				} else if resp.ToolErr != "" {
					excerpt := textutil.Truncate(c.Scrub(resp.ToolRaw), 200)
					args = []byte(excerpt)
					terr = fmt.Errorf("the arguments are not one JSON object (%s): %s; call %s again with one JSON object of its parameters: %s",
						c.Scrub(resp.ToolErr), excerpt, resp.ToolName, toolShape(c.Evidence, resp.ToolName))
				} else {
					out, cut, terr = tools.Tool(ctx, resp.ToolName, resp.ToolArgs)
				}
				d := time.Since(at)
				if terr != nil {
					out = "tool error: " + c.Scrub(terr.Error())
				} else if cut {
					out += "\n(output cut)"
				}
				call := CheckCall{Name: resp.ToolName, Args: textutil.Truncate(string(args), 512), ResultBytes: len(out), Duration: d}
				end.Calls = append(end.Calls, call)
				end.Evidence = append(end.Evidence, call)
				end.ToolCalls++
				conv += fmt.Sprintf("\n\ntool %s %s answered:\n%s\n(calls left %d, your answer due within %s)", resp.ToolName, args, out, c.CallCap-calls, max(0, time.Until(due())).Round(time.Second))
				continue
			}
			// the answer tool's arguments are the answer, parsed here and
			// never executed through the pod's tools
			args, _ := json.Marshal(resp.ToolArgs)
			answer = string(args)
		}
		perr := c.Parse(answer)
		if perr == nil {
			return finish(convAnswered, "")
		}
		refusal := oneLine(c.Scrub(perr.Error()))
		end.Refusals = append(end.Refusals, refusal)
		if repairs == 1 || last && !c.RepairPastCap {
			return finish(convUnreadable, refusal)
		}
		repairs++
		conv += "\n\nYour last answer was unreadable: " + c.Scrub(perr.Error()) + ". Answer again through the answer tool."
	}
}

// scrub is s through the runner's scrubber, the generic rules alone when it
// has none.
func (r *Runner) scrub(s string) string {
	if r.Scrub == nil {
		return redact.New().String(s)
	}
	return r.Scrub.String(s)
}

// checkinAnswerDue is how long after the check-in starts its answer call
// is due: the time cap less the answer's reserve, twice the longest call so
// far, at least policy.CheckinCallTime, at most half the cap. The answer
// call reads the whole record and runs up to 1.6 times the longest
// evidence call.
func checkinAnswerDue(timeCap, longest time.Duration) time.Duration {
	return timeCap - min(timeCap/2, max(policy.CheckinCallTime, 2*longest))
}

// checkinDueLine closes the statement with the time left for the answer.
func checkinDueLine(left time.Duration) string {
	return fmt.Sprintf(", your answer due within %s", left.Round(time.Second))
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
	out := CheckInOutcome{Decision: decision, ReasonKind: a.ReasonKind, Reason: a.Reason, Guidance: a.Guidance, Verdicts: verdicts, Remarks: remarks, Record: rec}
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
	if in.ToolChoice != nil {
		body["tool_choice"] = in.ToolChoice
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
				if resp.ToolName != checkinAnswerTool {
					resp.ToolErr, resp.ToolRaw = err.Error(), args
					return resp, nil
				}
				// a malformed answer reaches the parser as text, so it gets
				// its repair round and then fails the check-in
				// (checkin_unreadable)
				resp.ToolName, resp.ToolArgs, resp.Text = "", nil, args
			}
		}
	}
	return resp, nil
}

// toolNames are the names of tools, in order.
func toolNames(tools []CheckinTool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}
