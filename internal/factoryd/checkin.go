package factoryd

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The check-in at a gate return (the escalation ladder's plan D): one model
// conversation under its own alias judges every finding before the next
// session is bought — confirmed, dropped against checkable evidence, or
// remarked — hands the next session a note at the top of its brief, and
// stops the pod when only a human can act. This file is the vocabulary, the
// evidence rule and the three deterministic guards; the conversation runner
// joins the same file.

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
// that raised them.
type CheckInAsk struct {
	Moment   string
	Findings []CheckFinding
	Review   *Review
	Gates    *GateResults
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
		fmt.Fprintf(&b, "The parent session's whole record sits at /workspace/.factory/sessions/<parent>.jsonl; its tail:\n<<<SESSION-TAIL>>>\n%s\nSESSION-TAIL>>>\n\n", tail)
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
