package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// CardDigest is a card's whole life read from every row it is written in
// (the task row, its events, its timeline, the gateway's audit rows, its
// cases and its criteria): where it stands, its steps in time order, and
// the patterns the steps show. It is built on demand only, never on a
// board render.
type CardDigest struct {
	Head  DigestHead
	Steps []DigestStep // pre-fold, N is 1-based
	Flags []DigestFlag
}

// DigestHead is where the card stands: its issue, its plan's issue and
// name, its money, whether it holds a work slot, and its status and gate
// since Since.
type DigestHead struct {
	Issue, Plan                   int
	PlanName                      string
	BudgetEUR, SpentEUR, HoldsEUR float64
	SlotHeld                      bool
	Status, Gate                  string
	Since                         time.Time
}

// DigestStep is one step of the card's life. Actor is who acted; for a
// session it is the attempt and the model, for a check-in its moment.
// A session's Outcome leaves out its cost and calls, which Render adds.
// Findings are the ids (findingID) of the findings a review, checks or an
// audit raised, or a check-in confirmed. Count is how many identical steps
// in a row the step stands for: 1 before Folded.
type DigestStep struct {
	N                    int
	At                   time.Time
	Offset               time.Duration
	Kind, Actor, Outcome string
	CostEUR              float64
	Findings             []string
	Ref                  DigestRef
	Count                int
	// state is the word the flags read: a session's outcome, "" while it
	// runs; a review's verdict; a check-in's decision, "" when it was cut
	state string
	// calls are a session's model calls, by the audit rows
	calls int
}

// DigestRef names the rows a step was read from, whichever apply: its
// session, its issue comment, its case, its event and its timeline line
// (1-based).
type DigestRef struct{ Session, Comment, Case, Event, Step string }

// DigestFlag is a pattern the digest found in the card's life, by name,
// with what it points at.
type DigestFlag struct{ Name, Detail string }

// cardDigestEvents bounds the event rows a digest reads, far above what
// one card's life writes.
const cardDigestEvents = 5000

// CardDigest builds task taskID's digest; ErrNotFound when there is no
// such task. A card with no timeline object digests from its rows alone.
func (r *Runner) CardDigest(ctx context.Context, taskID string) (CardDigest, error) {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return CardDigest{}, err
	}
	if !ok {
		return CardDigest{}, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	head, err := r.digestHead(ctx, t)
	if err != nil {
		return CardDigest{}, err
	}
	events, err := r.store.TaskEvents(ctx, taskID, cardDigestEvents)
	if err != nil {
		return CardDigest{}, err
	}
	slices.Reverse(events)
	deps, err := r.store.Dependencies(ctx, taskID)
	if err != nil {
		return CardDigest{}, err
	}
	cases, err := r.store.CasesOf(ctx, taskID)
	if err != nil {
		return CardDigest{}, err
	}
	verdicts, err := r.store.CriteriaVerdicts(ctx, taskID)
	if err != nil {
		return CardDigest{}, err
	}
	tl, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return CardDigest{}, fmt.Errorf("read the timeline: %w", err)
	}
	lines, err := r.timelineSteps(ctx, taskID, tl)
	if err != nil {
		return CardDigest{}, err
	}
	budget := t.BudgetEUR
	if !copiesBudget(t, events) {
		if budget, err = r.expectedBudget(ctx, t, events); err != nil {
			return CardDigest{}, err
		}
	}
	steps := slices.Concat([]DigestStep{filedStep(t, deps)}, eventSteps(t, events, cases, deps), lines, criteriaSteps(verdicts))
	slices.SortStableFunc(steps, func(a, b DigestStep) int { return a.At.Compare(b.At) })
	for i := range steps {
		steps[i].N, steps[i].Offset, steps[i].Count = i+1, steps[i].At.Sub(steps[0].At), 1
	}
	return CardDigest{Head: head, Steps: steps, Flags: digestFlags(t, steps, cases, deps, budget)}, nil
}

func (r *Runner) digestHead(ctx context.Context, t Task) (DigestHead, error) {
	h := DigestHead{Issue: int(t.IssueNumber), BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR, SlotHeld: holdsSlot(t),
		Status: t.Status, Gate: t.Gate}
	if t.GateSince != nil {
		h.Since = *t.GateSince
	}
	if t.Track == policy.TrackPlan {
		h.Plan, h.PlanName = h.Issue, t.PlanName
	}
	if t.Parent != "" {
		p, ok, err := r.store.Task(ctx, t.Parent)
		if err != nil {
			return DigestHead{}, err
		}
		if ok && p.Track == policy.TrackPlan {
			h.Plan, h.PlanName = int(p.IssueNumber), p.PlanName
		}
	}
	own, _, err := r.store.Free(ctx, policy.HoldRequest{TaskID: t.ID, ProjectEUR: math.Inf(1)})
	if err != nil {
		return DigestHead{}, err
	}
	h.HoldsEUR = own.Held
	return h, nil
}

// holdsSlot is CountAdmitted's rule for one card.
func holdsSlot(t Task) bool {
	if defaultKind(t.Kind) != "agent" || t.Unbound() {
		return false
	}
	switch t.Status {
	case "waiting-deps", "running", "in-review", "closing":
		return true
	}
	return t.Status == "queued" && t.AdmittedAt != nil
}

// filedStep is the card's filing: by its source, against its plan
// revision, on its dependencies with their statuses.
func filedStep(t Task, deps []Dependency) DigestStep {
	var parts []string
	if t.PlanRevision > 0 {
		parts = append(parts, fmt.Sprintf("revision %d", t.PlanRevision))
	}
	if len(deps) > 0 {
		on := make([]string, len(deps))
		for i, d := range deps {
			on[i] = fmt.Sprintf("#%d (%s)", d.Issue, d.Status)
		}
		parts = append(parts, "depends on "+strings.Join(on, ", "))
	}
	by := cmp.Or(t.Source, FactoryDecider)
	if t.Source == SourcePlan {
		by = "planner"
	}
	return DigestStep{At: t.CreatedAt, Kind: "filed", Actor: by, Outcome: cmp.Or(strings.Join(parts, ", "), "direct card")}
}

// eventSteps are the steps the card's events, oldest first, record. A
// status move to waiting-human is the ask's, and a final one the
// task_ended row's when the card has one.
func eventSteps(t Task, events []Event, cases []Case, deps []Dependency) []DigestStep {
	ended := slices.ContainsFunc(events, func(e Event) bool { return e.Kind == EventTaskEnded })
	issueOf := func(id string) string {
		if i := slices.IndexFunc(deps, func(d Dependency) bool { return d.TaskID == id }); i >= 0 {
			return fmt.Sprintf("#%d", deps[i].Issue)
		}
		return id
	}
	var out []DigestStep
	status := ""
	for _, e := range events {
		word := func(k string) string { s, _ := e.Detail[k].(string); return s }
		number := func(k string) int { n, _ := toInt(e.Detail[k]); return n }
		amount, _ := e.Detail["amount_eur"].(float64)
		s := DigestStep{At: e.At, Actor: e.Actor, Ref: DigestRef{Event: strconv.FormatInt(e.ID, 10)}}
		comment := number("comment")
		if comment > 0 {
			s.Ref.Comment = strconv.Itoa(comment)
		}
		switch e.Kind {
		case EventQuestionAsked:
			s.Kind, s.Actor, s.Outcome = "ask", "", askOutcome(t, e)
			if i := slices.IndexFunc(cases, func(c Case) bool { return comment > 0 && c.CommentID == int64(comment) }); i >= 0 {
				s.Ref.Case = strconv.FormatInt(cases[i].ID, 10)
			}
		case EventQuestionAnswered, EventPlanQuestionAnswered:
			s.Kind, s.Outcome = "answer", digestText(cmp.Or(word("answer"), word("decision"), "answered"))
		case EventSessionGranted:
			s.Kind, s.Outcome = "granted", "one more session"
		case EventCardFunded:
			s.Kind, s.Outcome = "funded", fmt.Sprintf("%.2f EUR", amount)
		case EventCardExtended:
			s.Kind, s.Outcome = "extended", fmt.Sprintf("%s, %s, %.2f EUR",
				textutil.Plural(number("sessions"), "session", "sessions"), textutil.Plural(number("lines"), "line", "lines"), amount)
		case EventCardRetried:
			s.Kind, s.Outcome = "retried", fmt.Sprintf("as #%d", number("replacement"))
		case EventCardReplanned:
			s.Kind, s.Outcome = "replanned", "asked for a new plan"
		case EventCut:
			into, _ := e.Detail["into"].([]any)
			issues := make([]string, len(into))
			for i, n := range into {
				v, _ := toInt(n)
				issues[i] = fmt.Sprintf("#%d", v)
			}
			s.Kind, s.Outcome = "cut", fmt.Sprintf("revision %d into %s at session %d", number("revision"), strings.Join(issues, ", "), number("session"))
		case EventDependencySet:
			s.Kind, s.Outcome = "dependency", "depends on "+issueOf(word("depends_on"))
			if removed, _ := e.Detail["removed"].(bool); removed {
				s.Outcome = "no longer " + s.Outcome
			}
		case EventApprovalRequested:
			parts := []string{word("kind")}
			if pr := number("pr"); pr > 0 {
				parts = append(parts, fmt.Sprintf("pull request #%d", pr))
			}
			if rev := number("revision"); rev > 0 {
				parts = append(parts, fmt.Sprintf("revision %d", rev))
			}
			s.Kind, s.Actor, s.Outcome = "approval", "", strings.Join(parts, ", ")
		case EventTaskEnded:
			s.Kind, s.Actor, s.Outcome = "ended", "", word("state")
			if why := word("reason"); why != "" {
				s.Outcome += ": " + digestText(why)
			}
		case EventTaskStatus:
			to := word("status")
			moved := to != status
			status = to
			s.Actor = ""
			switch {
			case !moved || to == "waiting-human" || (Final(to) && ended):
				continue
			case to == "merged":
				s.Kind, s.Outcome = "merged", fmt.Sprintf("pull request #%d", t.PRNumber)
			case Final(to):
				s.Kind, s.Outcome = "ended", to
			default:
				s.Kind, s.Outcome = "status", statusText(to, word("gate"))
			}
		default:
			continue
		}
		out = append(out, s)
	}
	return out
}

// askOutcome is an ask's case kind and words, as the events writer files
// its case (caseFromAsk).
func askOutcome(t Task, e Event) string {
	c, err := caseFromAsk(t, e, "")
	if err != nil {
		return "an ask with no words"
	}
	return string(c.Kind) + ": " + digestText(c.Problem)
}

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
		step := DigestStep{At: l.Time, Ref: DigestRef{Session: l.Session, Step: strconv.Itoa(i + 1)}}
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
	cost, err := r.store.SpendBetween(ctx, taskID, s.start, end)
	if err != nil {
		return DigestStep{}, err
	}
	calls, err := r.store.CallsBetween(ctx, taskID, s.start, end)
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
// and its decision, the record's when it has one.
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
	step.Kind, step.Actor, step.Findings, step.state = "checkin", moment, byState[CheckConfirmed], decision
	step.Outcome = strings.Join(append(parts, cmp.Or(decision, "no decision")), ", ")
	return step
}

// criteriaSteps are the card's criteria verdicts, one step per write of a
// source: who decided, and each criterion's verdict.
func criteriaSteps(rows []CriterionVerdict) []DigestStep {
	type write struct {
		source string
		at     int64
	}
	var out []DigestStep
	at := map[write]int{}
	for _, v := range rows {
		w := write{v.Source, v.CreatedAt.UnixNano()}
		i, ok := at[w]
		if !ok {
			i, at[w] = len(out), len(out)
			out = append(out, DigestStep{At: v.CreatedAt, Kind: "criteria", Actor: v.DecidedBy})
		} else {
			out[i].Outcome += ", "
		}
		out[i].Outcome += v.Criterion + " " + cmp.Or(v.Verdict, "undecided")
	}
	return out
}

// digestText is the first line of s, cut to fit a digest line.
func digestText(s string) string { return textutil.Truncate(FirstLine(s), 120) }

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

// findingID names a finding by its words alone: the first 12 hex of the
// sha256 of its text lowercased, runs of white space one space, trimmed.
// A review's finding and a check-in's verdict on it get the same id.
func findingID(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(strings.ToLower(text)), " ")))
	return hex.EncodeToString(sum[:])[:12]
}
