package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// ErrNoStep refuses a step number the card's digest lacks.
type ErrNoStep string

func (e ErrNoStep) Error() string { return string(e) }

// TranscriptPointer names the read of session's transcript that a session
// step's detail ends with, n the session's place among the card's session
// starts, oldest first, 0 when the timeline holds no start for it; ""
// leaves the line out. Each caller names a tool its own agent holds.
type TranscriptPointer func(session string, n int) string

// CardStep returns one step of the card's digest in full: a session's steps, a gate's full result,
// a check-in's record with its calls, an ask's five parts and its answer, a prover's filing or
// store, a funding event's detail.
// n is the step's number before the fold; one the digest lacks is ErrNoStep, which names cardRead,
// the caller's tool that reads the digest with its numbers.
func (r *Runner) CardStep(ctx context.Context, taskID string, n int, cardRead string, transcript TranscriptPointer) (string, error) {
	rows, err := r.readCard(ctx, taskID)
	if err != nil {
		return "", err
	}
	d, err := r.cardDigest(ctx, rows)
	if err != nil {
		return "", err
	}
	if n < 1 || n > len(d.Steps) {
		return "", ErrNoStep(fmt.Sprintf("#%d has steps 1 to %d; %s numbers them", d.Head.Issue, len(d.Steps), cardRead))
	}
	s := d.Steps[n-1]
	var b strings.Builder
	fmt.Fprintf(&b, "#%d step %d of %d at %s\n%s", d.Head.Issue, n, len(d.Steps), s.At.UTC().Format("2006-01-02 15:04:05 UTC"), digestLine(s))
	lines := bytes.Split(rows.timeline, []byte("\n"))
	line, _ := strconv.Atoi(s.Ref.Step)
	switch {
	case s.Kind == "filed":
		filedDetail(&b, s.Actor, rows.task, d.Head, rows.deps)
	case s.Kind == "criteria":
		criteriaDetail(&b, s, rows.verdicts)
	case s.Kind == "session" && rows.task.Source == SourceProver:
		// a prover's transcript holds its tests: no read points at it
		sessionDetail(&b, s.Ref.Session, lines, d.Steps[0].At, func(string, int) string { return "" })
	case s.Kind == "session":
		sessionDetail(&b, s.Ref.Session, lines, d.Steps[0].At, transcript)
	case s.Kind == "ask":
		if err := r.askDetail(ctx, &b, s, rows); err != nil {
			return "", err
		}
	case s.Kind == "proofs" && s.Ref.Event != "":
		if i := eventIndex(rows.events, s.Ref.Event); i >= 0 {
			proofsEventDetail(&b, rows.events[i])
		}
	case s.Ref.Event != "":
		if i := eventIndex(rows.events, s.Ref.Event); i >= 0 {
			writeFields(&b, rows.events[i].Detail)
		}
	case s.Kind == "checkin":
		checkinDetail(&b, lines[line-1])
	case s.Kind == "proofs":
		proofsDetail(&b, lines[line-1])
	default:
		writeFields(&b, lineFields(lines[line-1]))
	}
	return b.String(), nil
}

// filedDetail is the card's filing: by whom, its track, size, rung and
// budget, its plan, its dependencies and its objective.
func filedDetail(b *strings.Builder, by string, t Task, h DigestHead, deps []Dependency) {
	parts := []string{"filed by " + by}
	for _, f := range [][2]string{{"track", t.Track}, {"size", t.Size}, {"rung", t.Rung}} {
		if f[1] != "" {
			parts = append(parts, f[0]+" "+f[1])
		}
	}
	fmt.Fprintf(b, "%s, budget %.2f EUR\n", strings.Join(parts, ", "), t.BudgetEUR)
	var plan []string
	if h.Plan > 0 {
		plan = append(plan, strings.TrimSpace(fmt.Sprintf("plan #%d %s", h.Plan, h.PlanName)))
	}
	if t.PlanRevision > 0 {
		plan = append(plan, fmt.Sprintf("revision %d", t.PlanRevision))
	}
	if len(plan) > 0 {
		b.WriteString(strings.Join(plan, ", ") + "\n")
	}
	if len(deps) > 0 {
		on := make([]string, len(deps))
		for i, d := range deps {
			on[i] = fmt.Sprintf("#%d (%s)", d.Issue, d.Status)
		}
		b.WriteString("depends on " + strings.Join(on, ", ") + "\n")
	}
	if t.Objective != "" {
		b.WriteString("objective:\n" + t.Objective + "\n")
	}
}

// sessionDetail is session id's timeline lines, one each, at their
// offsets from the card's start, and the read of its transcript that
// transcript names: n is its place among the card's session starts,
// oldest first, as the chat's session_steps counts them.
func sessionDetail(b *strings.Builder, id string, lines [][]byte, start time.Time, transcript TranscriptPointer) {
	var out []string
	starts, n := 0, 0
	for i, raw := range lines {
		m := lineFields(raw)
		if m != nil && m["step"] == "session_start" {
			starts++
			if n == 0 && m["session"] == id {
				n = starts
			}
		}
		if m == nil || m["session"] != id {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(m["time"]))
		text := fmt.Sprintf(" %-6s  line %d  %v", offsetText(at.Sub(start)), i+1, m["step"])
		var fields []string
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if k != "step" && k != "time" && k != "session" {
				fields = append(fields, k+" "+strings.Join(strings.Fields(valueText(m[k])), " "))
			}
		}
		if len(fields) > 0 {
			text += ": " + strings.Join(fields, ", ")
		}
		out = append(out, text)
	}
	fmt.Fprintf(b, "session %s, %s:\n", id, textutil.Plural(len(out), "timeline line", "timeline lines"))
	for _, l := range out {
		b.WriteString(l + "\n")
	}
	if read := transcript(id, n); read != "" {
		b.WriteString("transcript: " + read + "\n")
	}
}

// askDetail is the ask's five parts, the answer that came before the next
// ask, and its case with the case's steps.
func (r *Runner) askDetail(ctx context.Context, b *strings.Builder, s DigestStep, rows cardRows) error {
	i := eventIndex(rows.events, s.Ref.Event)
	if i < 0 {
		return nil
	}
	e := rows.events[i]
	word := func(k string) string { v, _ := e.Detail[k].(string); return cmp.Or(v, "none") }
	var choices []string
	switch c := e.Detail["choices"].(type) {
	case []string:
		choices = c
	case []any:
		for _, v := range c {
			choices = append(choices, fmt.Sprint(v))
		}
	}
	fmt.Fprintf(b, "Ask: %s\nWhy: %s\nInfo: %s\nRecommendation: %s\nChoices: %s\n", cmp.Or(askSentence(e), "none"), word("why"), word("info"),
		word("recommendation"), cmp.Or(strings.Join(choices, "; "), "none"))
	b.WriteString(askAnswer(e, rows.events[i+1:]) + "\n")
	c := slices.IndexFunc(rows.cases, func(c Case) bool { return strconv.FormatInt(c.ID, 10) == s.Ref.Case })
	if c < 0 {
		return nil
	}
	k := rows.cases[c]
	fmt.Fprintf(b, "case %d %s, %s", k.ID, k.Kind, k.Status)
	if res := k.Resolution; res != nil {
		b.WriteString(" by " + res.By)
		if res.Action != "" {
			b.WriteString(": " + res.Action)
		}
	}
	b.WriteString("\n")
	steps, err := r.store.CaseSteps(ctx, k.ID)
	if err != nil {
		return err
	}
	for _, st := range steps {
		fmt.Fprintf(b, " rung %d by %s → %s", st.Rung, st.Actor, st.Outcome)
		if st.Decision != "" {
			b.WriteString(", " + st.Decision)
		}
		b.WriteString("\n")
	}
	return nil
}

// askAnswer is ask's answer among events, those after it. A plan
// question's is the plan answer of its revision and index, past the
// questions asked with it, until a later revision asks again; any other
// ask's is the answer or grant before the next such ask.
func askAnswer(ask Event, events []Event) string {
	revision, _ := toInt(ask.Detail["revision"])
	index, _ := toInt(ask.Detail["index"])
	for _, e := range events {
		word := func(k string) string { v, _ := e.Detail[k].(string); return v }
		answer := func() string {
			return fmt.Sprintf("Answer by %s: %s", e.Actor, cmp.Or(word("answer"), word("decision"), "answered"))
		}
		rev, _ := toInt(e.Detail["revision"])
		idx, _ := toInt(e.Detail["index"])
		switch {
		case revision > 0 && e.Kind == EventQuestionAsked && rev > revision:
			return "Answer: none"
		case revision > 0 && e.Kind == EventPlanQuestionAnswered && rev == revision && idx == index:
			return answer()
		case revision > 0:
		case e.Kind == EventQuestionAsked && rev == 0:
			return "Answer: none"
		case e.Kind == EventQuestionAnswered:
			return answer()
		case e.Kind == EventSessionGranted:
			return fmt.Sprintf("Answer by %s: %s", e.Actor, cmp.Or(word("answer"), "one more session granted"))
		case e.Kind == EventQuestionWithdrawn:
			return "Withdrawn: " + cmp.Or(word("reason"), "no reason given")
		}
	}
	return "Answer: none yet"
}

// checkinDetail is a check-in's line: its moment and decision, its calls
// and cost when the line has a record, its reason, its verdict on each
// finding with the evidence, and the calls its record holds.
func checkinDetail(b *strings.Builder, raw []byte) {
	var l timelineLine
	_ = json.Unmarshal(raw, &l)
	moment, decision := l.Moment, l.Decision
	var rec CheckInRecord
	if l.Record != nil {
		rec = *l.Record
		moment, decision = cmp.Or(rec.Moment, moment), cmp.Or(rec.Decision, decision)
	}
	fmt.Fprintf(b, "moment %s, decision %s", moment, cmp.Or(decision, "none"))
	if l.Record != nil {
		fmt.Fprintf(b, ", %s, %.2f EUR", textutil.Plural(rec.ToolCalls, "tool call", "tool calls"), rec.CostEUR)
	}
	b.WriteString("\n")
	if rec.Reason != "" {
		reason := rec.Reason
		if rec.ReasonKind != "" {
			reason = rec.ReasonKind + ": " + reason
		}
		b.WriteString("reason: " + reason + "\n")
	}
	texts := slices.SortedFunc(maps.Keys(l.Verdicts), func(a, c string) int { return strings.Compare(findingID(a), findingID(c)) })
	for _, text := range texts {
		v := l.Verdicts[text]
		fmt.Fprintf(b, "- %s %s: %s\n", findingID(text), v.State, text)
		if v.Evidence != "" {
			b.WriteString("  evidence: " + v.Evidence + "\n")
		}
	}
	if len(rec.Calls) > 0 {
		b.WriteString("calls:\n")
	}
	for _, c := range rec.Calls {
		fmt.Fprintf(b, "- %s, %d bytes, %s\n", strings.TrimSpace(c.Name+" "+c.Args), c.ResultBytes, c.Duration)
	}
}

// proofsDetail is a gate_criteria line in full: the pull request and the
// head, every criterion's line with its message, the findings no line
// holds, the start failure. Never the hold-outs.
func proofsDetail(b *strings.Builder, raw []byte) {
	var l struct {
		PR          int64       `json:"pr"`
		Commit      string      `json:"commit"`
		Proofs      []gateProof `json:"proofs"`
		Findings    []string    `json:"findings"`
		StartFailed string      `json:"start_failed"`
	}
	_ = json.Unmarshal(raw, &l)
	fmt.Fprintf(b, "pull request #%d at %s\n", l.PR, l.Commit)
	for _, p := range l.Proofs {
		verdict := "unmet"
		if p.Met {
			verdict = "met"
		}
		fmt.Fprintf(b, "%s %s: %s\n", p.ID, verdict, p.Line)
		for line := range strings.Lines(p.Message) {
			b.WriteString("  " + strings.TrimSuffix(line, "\n") + "\n")
		}
	}
	for _, f := range l.Findings {
		if !slices.ContainsFunc(l.Proofs, func(p gateProof) bool { return p.Line == f }) {
			fmt.Fprintf(b, "- %s %s\n", findingID(f), f)
		}
	}
	if l.StartFailed != "" {
		b.WriteString("start failed: " + l.StartFailed + "\n")
	}
}

// proofsEventDetail writes a proofs event's lines in full: a filing's
// entries, each as the event logs it, a hold-out's by its id and outcome
// alone, and its refusals; the store's ids, each with its flag; one line
// for a detail that does not decode.
func proofsEventDetail(b *strings.Builder, e Event) {
	d, ok := readProofsEvent(e)
	if !ok {
		b.WriteString("unreadable detail\n")
		return
	}
	if e.Kind == EventProofsStored {
		fmt.Fprintf(b, "prover %s at %s\n", d.Prover, d.Commit)
		flags := map[string]string{}
		for _, f := range d.Flagged {
			flags[f.Criterion] = f.Flag
		}
		for _, id := range d.Stored {
			line := id + " stored"
			if f := flags[id]; f != "" {
				line += ", flagged " + f
			}
			b.WriteString(line + "\n")
		}
		return
	}
	fmt.Fprintf(b, "prover %s, filing %d\n", d.Prover, d.Filing)
	for _, p := range d.Proofs {
		line := cmp.Or(p.Criterion, "(no criterion)") + " " + p.Outcome + " on main"
		if p.Path != "" {
			line += fmt.Sprintf(": %s in %s at %s", p.Name, p.Suite, p.Path)
		}
		b.WriteString(line + "\n")
	}
	for _, r := range d.Refusals {
		b.WriteString("refused: " + r + "\n")
	}
}

// criteriaDetail is the criteria verdicts of step s's write: each
// criterion's verdict, evidence and fix.
func criteriaDetail(b *strings.Builder, s DigestStep, verdicts []CriterionVerdict) {
	for _, v := range verdicts {
		if !v.CreatedAt.Equal(s.At) || criteriaActor(v) != s.Actor {
			continue
		}
		fmt.Fprintf(b, "%s %s, source %s\n", v.Criterion, cmp.Or(v.Verdict, "undecided"), v.Source)
		for _, f := range [][2]string{{"evidence", v.Evidence}, {"fix", v.Fix}} {
			if f[1] != "" {
				b.WriteString("  " + f[0] + ": " + f[1] + "\n")
			}
		}
	}
}

// writeFields writes m's fields but step and time, by key, one per line:
// a string as it stands, a list of findings one per line with its id, any
// other value as JSON.
func writeFields(b *strings.Builder, m map[string]any) {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if k == "step" || k == "time" {
			continue
		}
		items, isList := m[k].([]any)
		if k != "findings" || !isList {
			b.WriteString(k + ": " + valueText(m[k]) + "\n")
			continue
		}
		b.WriteString("findings:\n")
		for _, f := range items {
			text, ok := f.(string)
			shown := text
			if !ok {
				item, _ := f.(map[string]any)
				text, shown = fmt.Sprint(item["item"]), valueText(f)
			}
			fmt.Fprintf(b, "- %s %s\n", findingID(text), shown)
		}
	}
}

// lineFields is a timeline line's fields, nil when it does not parse.
func lineFields(raw []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// eventIndex is the index of the event of id among events, -1 for none.
func eventIndex(events []Event, id string) int {
	return slices.IndexFunc(events, func(e Event) bool { return strconv.FormatInt(e.ID, 10) == id })
}

// valueText is v as text: a string as it stands, anything else as JSON.
func valueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}
