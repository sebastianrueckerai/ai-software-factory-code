package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// timelineLine is the part of a timeline line the digest reads.
type timelineLine struct {
	Step     string                  `json:"step"`
	Time     time.Time               `json:"time"`
	Session  string                  `json:"session"`
	Model    string                  `json:"model"`
	Attempt  int                     `json:"attempt"`
	Outcome  string                  `json:"outcome"`
	Started  string                  `json:"started"`
	Commit   string                  `json:"commit"`
	State    string                  `json:"state"`
	Verdict  string                  `json:"verdict"`
	Findings json.RawMessage         `json:"findings"`
	Decision string                  `json:"decision"`
	Moment   string                  `json:"moment"`
	Verdicts map[string]CheckVerdict `json:"verdicts"`
	Record   *CheckInRecord          `json:"record"`
	Handover string                  `json:"handover"`
}

// digestSession is one session the timeline records: its start, its end
// and its push.
type digestSession struct {
	id, model, outcome, commit string
	attempt, line              int
	start, end                 time.Time
}

// timelineSteps are the steps of the card's timeline tl: its sessions,
// with their cost and calls from the audit rows, check-ins, handovers,
// checks, reviews and audits. A line of a kind the digest does not read,
// or one it cannot parse, is skipped; the asks and approvals there are
// the events' too, and come from them.
func (r *Runner) timelineSteps(ctx context.Context, taskID string, tl []byte) ([]DigestStep, error) {
	out, sessions := lineSteps(tl)
	for i, s := range sessions {
		var next *digestSession
		if i+1 < len(sessions) {
			next = sessions[i+1]
		}
		step, err := r.sessionStep(ctx, taskID, s, next)
		if err != nil {
			return nil, err
		}
		out = append(out, step)
	}
	return out, nil
}

// lineSteps are the steps of timeline tl's lines other than its
// sessions, in line order, and its sessions in the order its lines first
// name them.
func lineSteps(tl []byte) ([]DigestStep, []*digestSession) {
	var out []DigestStep
	var sessions []*digestSession
	byID := map[string]*digestSession{}
	session := func(l timelineLine, line int) *digestSession {
		s := byID[l.Session]
		if s == nil {
			started, _ := time.Parse(time.RFC3339Nano, l.Started)
			s = &digestSession{id: l.Session, start: cmp.Or(started, l.Time), line: line}
			byID[l.Session] = s
			sessions = append(sessions, s)
		}
		s.model, s.attempt = cmp.Or(s.model, l.Model), cmp.Or(s.attempt, l.Attempt)
		return s
	}
	for i, raw := range bytes.Split(tl, []byte("\n")) {
		var l timelineLine
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		step := DigestStep{At: l.Time, Ref: DigestRef{Session: l.Session, Step: strconv.Itoa(i + 1)}, commit: l.Commit}
		switch l.Step {
		case "session_start":
			s := session(l, i+1)
			s.start, s.line = l.Time, i+1
			continue
		case "session_end":
			s := session(l, i+1)
			s.end, s.outcome = l.Time, l.Outcome
			continue
		case "pushed":
			if s := byID[l.Session]; s != nil {
				s.commit = l.Commit
			}
			continue
		case "checkin":
			step = checkinStep(step, l)
		case "handover":
			step.Kind, step.Outcome = "handover", digestText(l.Handover)
		case "checks":
			step.Kind, step.Findings = "checks", findingIDs(l.Findings)
			step.Outcome = withFindings(fmt.Sprintf("%s at %s", l.State, l.Commit[:min(7, len(l.Commit))]), step.Findings)
		case "model_review":
			step.Kind, step.Findings, step.state = "review", findingIDs(l.Findings), l.Verdict
			step.Outcome = withFindings(cmp.Or(l.Verdict, "no verdict"), step.Findings)
		case "audit":
			var found []AuditFinding
			_ = json.Unmarshal(l.Findings, &found)
			items := make([]string, len(found))
			for k, f := range found {
				items[k] = f.Item
			}
			step.Kind, step.Findings = "audit", textIDs(items)
			step.Outcome = cmp.Or(l.Verdict, "no verdict")
			if len(step.Findings) > 0 {
				step.Outcome = "findings " + strings.Join(step.Findings, " ")
			}
		default:
			continue
		}
		out = append(out, step)
	}
	return out, sessions
}

// sessionStep is session s's step: its outcome, with the head it pushed,
// and its cost and calls from the task's audit rows between its start and
// its end. A session with no end ran up to the next session's start, next,
// or runs still when it is the last.
func (r *Runner) sessionStep(ctx context.Context, taskID string, s, next *digestSession) (DigestStep, error) {
	end, outcome := s.end, strings.ReplaceAll(cmp.Or(s.outcome, "ended"), "_", " ")
	switch {
	case !end.IsZero():
	case next != nil:
		end, outcome = next.start, "no end"
	default:
		end, outcome = r.clock(), "running"
	}
	cost, calls, err := r.store.SpendAndCallsBetween(ctx, taskID, s.start, end)
	if err != nil {
		return DigestStep{}, err
	}
	if s.outcome == string(OutcomePushed) && s.commit != "" {
		outcome += " " + s.commit[:min(7, len(s.commit))]
	}
	return DigestStep{At: s.start, Kind: "session", Actor: strings.TrimSpace(fmt.Sprintf("%d %s", s.attempt, aliasOf(s.model))),
		Outcome: outcome, CostEUR: cost, Ref: DigestRef{Session: s.id, Step: strconv.Itoa(s.line)}, state: s.outcome, calls: calls}, nil
}

// checkinStep is a check-in's step: its moment, its verdicts by state
// and its decision, the record's when it has one. A record with no
// decision is a cut conversation: its state stays empty for the flags,
// and its outcome names the cut with the decision the line carries.
func checkinStep(step DigestStep, l timelineLine) DigestStep {
	decision, moment := l.Decision, l.Moment
	if l.Record != nil {
		decision, moment, step.CostEUR = l.Record.Decision, cmp.Or(l.Record.Moment, moment), l.Record.CostEUR
		step.Ref.Session = cmp.Or(step.Ref.Session, l.Record.Session)
	}
	byState := map[string][]string{}
	for text, v := range l.Verdicts {
		byState[v.State] = append(byState[v.State], findingID(text))
	}
	var parts []string
	for _, state := range []string{CheckConfirmed, CheckDropped, CheckRemark} {
		if found := byState[state]; len(found) > 0 {
			slices.Sort(found)
			parts = append(parts, state+" "+strings.Join(found, " "))
		}
	}
	shown := cmp.Or(decision, "no decision")
	if decision == "" && l.Decision != "" {
		shown = "cut, " + l.Decision
	}
	step.Kind, step.Actor, step.Findings, step.state = "checkin", moment, byState[CheckConfirmed], decision
	step.Outcome = strings.Join(append(parts, shown), ", ")
	return step
}

// withFindings is outcome followed by the finding ids, if any.
func withFindings(outcome string, ids []string) string {
	if len(ids) == 0 {
		return outcome
	}
	return outcome + ", findings " + strings.Join(ids, " ")
}

// findingIDs are the sorted ids of the findings raw, a JSON list of
// finding texts.
func findingIDs(raw json.RawMessage) []string {
	var texts []string
	_ = json.Unmarshal(raw, &texts)
	return textIDs(texts)
}

// textIDs are the sorted ids of texts.
func textIDs(texts []string) []string {
	var out []string
	for _, t := range texts {
		out = append(out, findingID(t))
	}
	slices.Sort(out)
	return out
}
