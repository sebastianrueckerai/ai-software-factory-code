package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// LastStepAt is the time of the timeline's newest line, zero for a
	// card with none.
	LastStepAt time.Time
	// Waits are the bounds of the card's project.
	Waits policy.Waits
	// lastStep is the step of the timeline's newest line, caseSteps are
	// the steps of the card's live cases, and at is when the digest was
	// built, the time its waits are aged to. A running card's lastCallAt
	// is its newest model call's time, and its retryDue the time the
	// newest provider_refused line's backoff ends. cases are the card's
	// cases, issueCloses whether Finish closes the issue of the card's end
	// (finishClosesIssue), and issueClosed whether its events hold its
	// issue's close, which the webhook records on an ended card.
	lastStep    string
	caseSteps   []CaseStep
	at          time.Time
	lastCallAt  time.Time
	retryDue    time.Time
	cases       []Case
	issueCloses bool
	issueClosed bool
}

// DigestHead is where the card stands: its issue, its plan's issue and
// name, its money, whether it holds a work slot, and its status and gate
// since Since, the gate's start. StatusSince is the newest task_status
// event's time, else the filing's. QueueReason and QueueSince are the
// task row's: why admit last refused the queued card, and since when.
type DigestHead struct {
	Issue, Plan                   int
	PlanName                      string
	BudgetEUR, SpentEUR, HoldsEUR float64
	SlotHeld                      bool
	Status, Gate                  string
	Since, StatusSince            time.Time
	QueueReason                   string
	QueueSince                    *time.Time
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
	// commit is the head a checks, proofs, review or audit line judged
	commit string
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
	rows, err := r.readCard(ctx, taskID)
	if err != nil {
		return CardDigest{}, err
	}
	return r.cardDigest(ctx, rows)
}

// cardRows are the rows a card's digest is read from: the task, its
// events oldest first, its dependencies, its cases, the steps of its live
// cases, its criteria verdicts and its timeline object, nil when it has
// none.
type cardRows struct {
	task      Task
	events    []Event
	deps      []Dependency
	cases     []Case
	caseSteps []CaseStep
	verdicts  []CriterionVerdict
	timeline  []byte
}

// readCard reads task taskID's rows; ErrNotFound when there is no such
// task.
func (r *Runner) readCard(ctx context.Context, taskID string) (cardRows, error) {
	t, ok, err := r.store.Task(ctx, taskID)
	if err != nil {
		return cardRows{}, err
	}
	if !ok {
		return cardRows{}, fmt.Errorf("task %s: %w", taskID, ErrNotFound)
	}
	rows := cardRows{task: t}
	if rows.events, err = r.store.TaskEvents(ctx, taskID, cardDigestEvents); err != nil {
		return cardRows{}, err
	}
	slices.Reverse(rows.events)
	if rows.deps, err = r.store.Dependencies(ctx, taskID); err != nil {
		return cardRows{}, err
	}
	if rows.cases, err = r.store.CasesOf(ctx, taskID); err != nil {
		return cardRows{}, err
	}
	for _, c := range rows.cases {
		if c.Status != CaseOpen && c.Status != CaseAtHuman {
			continue
		}
		steps, err := r.store.CaseSteps(ctx, c.ID)
		if err != nil {
			return cardRows{}, err
		}
		rows.caseSteps = append(rows.caseSteps, steps...)
	}
	if rows.verdicts, err = r.store.CriteriaVerdicts(ctx, taskID); err != nil {
		return cardRows{}, err
	}
	if rows.timeline, err = r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl"); err != nil {
		return cardRows{}, fmt.Errorf("read the timeline: %w", err)
	}
	return rows, nil
}

// cardDigest builds the digest of the card rows hold.
func (r *Runner) cardDigest(ctx context.Context, rows cardRows) (CardDigest, error) {
	t := rows.task
	head, err := r.digestHead(ctx, t, rows.events)
	if err != nil {
		return CardDigest{}, err
	}
	lines, err := r.timelineSteps(ctx, t.ID, rows.timeline)
	if err != nil {
		return CardDigest{}, err
	}
	budget := t.BudgetEUR
	if !copiesBudget(t, rows.events) {
		if budget, err = r.expectedBudget(ctx, t, rows.events); err != nil {
			return CardDigest{}, err
		}
	}
	steps := slices.Concat([]DigestStep{filedStep(t, rows.deps)}, eventSteps(t, rows.events, rows.cases, rows.deps), lines, criteriaSteps(rows.verdicts))
	slices.SortStableFunc(steps, func(a, b DigestStep) int { return a.At.Compare(b.At) })
	for i := range steps {
		steps[i].N, steps[i].Offset, steps[i].Count = i+1, steps[i].At.Sub(steps[0].At), 1
	}
	d := CardDigest{Head: head, Steps: steps, Waits: r.cfg.Policy().WaitsOf(t.Repository), caseSteps: rows.caseSteps, at: r.clock(), cases: rows.cases,
		issueCloses: finishClosesIssue(t, t.Status, mergedBefore(rows.events)),
		issueClosed: slices.ContainsFunc(rows.events, func(e Event) bool { return e.Kind == EventIssueClosed })}
	newest := newestLine(rows.timeline, "")
	d.lastStep, d.LastStepAt = newest.Step, newest.Time
	if t.Status == "running" {
		if d.lastCallAt, err = r.store.NewestCallAt(ctx, t.ID); err != nil {
			return CardDigest{}, err
		}
		if refused := newestLine(rows.timeline, StepProviderRefused); !refused.Time.IsZero() {
			d.retryDue = refused.Time.Add(time.Duration(refused.BackoffS * float64(time.Second)))
		}
	}
	d.Flags = digestFlags(d, t, rows.events, rows.cases, rows.deps, budget)
	return d, nil
}

// digestHead is card t's head. Its events, oldest first, give its
// status's since.
func (r *Runner) digestHead(ctx context.Context, t Task, events []Event) (DigestHead, error) {
	h := DigestHead{Issue: int(t.IssueNumber), BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR, SlotHeld: holdsSlot(t),
		Status: t.Status, Gate: t.Gate, StatusSince: t.CreatedAt, QueueReason: t.QueueReason, QueueSince: t.QueueSince}
	if t.GateSince != nil {
		h.Since = *t.GateSince
	}
	for _, e := range events {
		if e.Kind == EventTaskStatus {
			h.StatusSince = e.At
		}
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
	case "running", "closing":
		return true
	case "in-review":
		return t.Gate != GateApproval
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
			if i := askCaseIndex(t, e, cases); i >= 0 {
				s.Ref.Case = strconv.FormatInt(cases[i].ID, 10)
			}
		case EventQuestionAnswered, EventPlanQuestionAnswered:
			s.Kind, s.Outcome = "answer", digestText(cmp.Or(word("answer"), word("decision"), "answered"))
		case EventSessionGranted:
			s.Kind, s.Outcome = "granted", "one more session"
		case EventCardFunded:
			s.Kind, s.Outcome = "funded", fmt.Sprintf("%.2f EUR", amount)
			if ceiling, ok := e.Detail["ceiling_eur"].(float64); ok {
				s.Kind, s.Outcome = "ceiling", fmt.Sprintf("raised to %.2f EUR", ceiling)
			}
		case EventCardExtended:
			parts := []string{textutil.Plural(number("sessions"), "session", "sessions")}
			if seconds := number("agent_time_s"); seconds > 0 {
				parts = append(parts, minutesWord(time.Duration(seconds)*time.Second)+" of agent time")
			}
			if shares := number("checkin_shares"); shares > 0 {
				parts = append(parts, textutil.Plural(shares, "check-in share", "check-in shares"))
			}
			s.Kind, s.Outcome = "extended", strings.Join(append(parts, textutil.Plural(number("lines"), "line", "lines"), fmt.Sprintf("%.2f EUR", amount)), ", ")
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

// askCaseIndex is the index among cases of the case ask e opened or
// asked again, -1 for none: the case it names, else the case of its
// comment when it has one, else the newest of its kind and words opened by
// the time it was asked, as a plan question's or a human card's ask, which
// have no comment.
func askCaseIndex(t Task, e Event, cases []Case) int {
	if id, _ := toInt(e.Detail["case_id"]); id > 0 {
		return slices.IndexFunc(cases, func(c Case) bool { return c.ID == int64(id) })
	}
	if comment, _ := toInt(e.Detail["comment"]); comment > 0 {
		return slices.IndexFunc(cases, func(c Case) bool { return c.CommentID == int64(comment) })
	}
	want, err := caseFromAsk(t, e, "")
	if err != nil {
		return -1
	}
	for i, c := range slices.Backward(cases) {
		if c.Kind == want.Kind && c.Problem == want.Problem && !c.OpenedAt.After(e.At) {
			return i
		}
	}
	return -1
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
			out = append(out, DigestStep{At: v.CreatedAt, Kind: "criteria", Actor: criteriaActor(v)})
		} else {
			out[i].Outcome += ", "
		}
		out[i].Outcome += v.Criterion + " " + cmp.Or(v.Verdict, "undecided")
	}
	return out
}

// criteriaActor is who wrote a verdict row as the digest names it: the
// source for the factory's rows, else the human.
func criteriaActor(v CriterionVerdict) string {
	if v.DecidedBy == FactoryDecider || v.DecidedBy == "" {
		return v.Source
	}
	return v.DecidedBy
}

// digestText is the first line of s, cut to fit a digest line.
func digestText(s string) string { return textutil.Truncate(FirstLine(s), 120) }

// findingID names a finding by its words alone: the first 12 hex of the
// sha256 of its text lowercased, runs of white space one space, trimmed.
// A review's finding and a check-in's verdict on it get the same id.
func findingID(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(strings.ToLower(text)), " ")))
	return hex.EncodeToString(sum[:])[:12]
}
