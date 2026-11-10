package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// ActivityOpenTrip is OpenTrip's activity.
const ActivityOpenTrip = "OpenTrip"

// TripInput is what the run knows when it stops: the reason as today's
// Escalate took it, the choices, and the head when it has one.
// The facts are read on the head. The kind comes from the reason as phase
// 1's caseKindOf derives it, the state key from the kind: the head for a
// cap, a floor, a gate stop or a merge matter, the dependency's task id
// for a dependency kind, phase 1's text key for a plan kind and for a kind
// with no head.
type TripInput struct {
	TaskID  string
	Reason  string
	Choices []string
	Head    string
	// CheckinStop is the check-in spend stop the trip is (CheckinStop*),
	// "" for any other stop: its repeat's ask no longer names it.
	CheckinStop string
	// Kind and StateKey are the case's for a caller that knows them, the
	// monitor: with Kind set they stand in for the kind and the key the
	// reason gives. Extra are facts the case carries beside the card's.
	Kind     CaseKind
	StateKey string
	Extra    map[string]any
	// Killed is the memory kill that ended the session before the stop,
	// nil for none: the case's facts carry it.
	Killed *SessionKill `json:",omitempty"`
}

// The check-in's spend stops, which a check-in share case keeps in its
// facts as checkin_stop.
const (
	CheckinStopShare   = "share"
	CheckinStopCallCap = "call_cap"
	CheckinStopTimeCap = "time_cap"
)

// tripAttempt is one attempt of a trip's activity: its id, the run's and
// the activity's, and its number, 1 for the first.
type tripAttempt struct {
	ID string
	N  int32
}

// TripOutcome is what the run gets back: the case, the comment the human
// face posted (0 when rung 0 resolved it and nobody was asked), and the
// resolution when rung 0 resolved it in place. Rung is the rung that holds
// the case: 0 when it resolved it, 3 when the case waits on the human.
type TripOutcome struct {
	CaseID     int64
	Comment    int64
	Resolution *Resolution
	Rung       int
}

// resolver is one row of rung 0: it fires on a case's kind and facts and
// returns the resolution it took, or nil.
type resolver struct {
	Name string
	Fire func(ctx context.Context, r *Runner, c Case, f CaseFacts) (*Resolution, error)
}

// headKinds are the stops keyed on the card's head, whose way on rung 0
// may find in the facts: the caps, the floor, the check-in's share and a
// gate stop.
var headKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor, CaseCheckinShare, CaseCheckinFailed, CaseGateStop}

// rung0 is rung 0's table, in the order its rows are tried. Act carries a
// row's resolution out on rung 0: it checks the action table again, runs
// what lives outside the run and records the step.
var rung0 = []resolver{
	// an open pull request whose head is green and approved goes to the
	// approval again: no session is asked for
	{Name: "route_approval", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		return rungZeroTakes(c, f, headKinds, Resolution{Action: ActRouteApproval}), nil
	}},
	// a red head's checks run again, once per head (RerunOnHead)
	{Name: "rerun_checks", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		return rungZeroTakes(c, f, headKinds, Resolution{Action: ActRerunChecks}), nil
	}},
	// a checks wait's stop with every check green: the wait runs again,
	// once per head (WaitedOnHead)
	{Name: "wait_checks", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		if f.Checks != "green" {
			return nil, nil
		}
		return rungZeroTakes(c, f, []CaseKind{CaseGateStop}, Resolution{Action: ActWaitChecks}), nil
	}},
	// a day's, a month's or a project's cap: the run's own wait (capWait)
	// ends it when the cap turns, and the case waits on the human until then
	{Name: "cap_wait", Fire: func(context.Context, *Runner, Case, CaseFacts) (*Resolution, error) { return nil, nil }},
	// a failed dependency outside a plan is filed again once, and a retry
	// that failed in turn goes to the human
	{Name: "retry_dependency", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		d := caseDep(c, f)
		if f.Plan != "" || d == nil || d.RetryOf != "" {
			return nil, nil
		}
		return rungZeroTakes(c, f, []CaseKind{CaseDependencyFailed}, Resolution{Action: ActRetry, Args: map[string]any{"task_id": d.TaskID}}), nil
	}},
	// inside a plan, the failed dependency's work goes to the plan's planner
	{Name: "impasse", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		d := caseDep(c, f)
		if f.Plan == "" || d == nil {
			return nil, nil
		}
		return rungZeroTakes(c, f, []CaseKind{CaseDependencyFailed}, Resolution{Action: ActReplan, Args: map[string]any{"task_id": d.TaskID}}), nil
	}},
	// the monitor's issue_open_after_end, which the monitor raises only
	// where Finish closes the issue: rung 0 closes it on the ended card, as
	// the heal closes it
	{Name: "close_ended", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		if c.Kind != CaseMonitorFlag || c.Facts["flag"] != "issue_open_after_end" || f.Final == "" {
			return nil, nil
		}
		return rungZeroTakes(c, f, []CaseKind{CaseMonitorFlag}, Resolution{Action: ActClose}), nil
	}},
	// the monitor's wait_over_bound on a rung that did not answer: the
	// card's steps past their bound end as ExpireSteps ends one, and that
	// ends the case, with no face: the steps' hand-up is the record
	{Name: "expire_step", Fire: func(ctx context.Context, r *Runner, c Case, _ CaseFacts) (*Resolution, error) {
		if c.Kind != CaseMonitorFlag || c.Facts["flag"] != "wait_over_bound" || c.Facts["wait"] != "rung_answer" {
			return nil, nil
		}
		ended, err := r.expireCardSteps(ctx, c.TaskID)
		if err != nil || len(ended) == 0 {
			return nil, err
		}
		what := "step"
		if len(ended) > 1 {
			what = "steps"
		}
		return &Resolution{Action: ActWithdraw, Note: "ended the overdue " + what + ": " + textutil.AndList(ended)}, nil
	}},
}

// expireCardSteps ends card taskID's in-progress steps past their due as
// ExpireSteps ends them, handed up with its words, and names each: the
// rung that did not answer and its bound. A step its rung ended meanwhile
// keeps its own end.
func (r *Runner) expireCardSteps(ctx context.Context, taskID string) ([]string, error) {
	cases, err := r.store.OpenCasesOf(ctx, taskID)
	if err != nil {
		return nil, err
	}
	now := r.clock()
	var ended []string
	for _, c := range cases {
		steps, err := r.store.CaseSteps(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range steps {
			if st.Outcome != StepInProgress || st.DueAt == nil || !st.DueAt.Before(now) {
				continue
			}
			err := r.store.EndCaseStep(ctx, st.ID, StepHandedUp, stepExpired(st.Found, st.StartedAt, *st.DueAt), st.Did, st.Decision, st.Grounds, st.CostEUR)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			ended = append(ended, fmt.Sprintf("rung %d did not answer within %s", st.Rung, durationText(st.DueAt.Sub(st.StartedAt).Round(time.Minute))))
		}
	}
	return ended, nil
}

// rungZeroTakes is res when case c is of kinds and the action table lets
// rung 0 take res on facts f, else nil.
func rungZeroTakes(c Case, f CaseFacts, kinds []CaseKind, res Resolution) *Resolution {
	a, ok := ActionByName(res.Action)
	if !ok || !slices.Contains(kinds, c.Kind) || a.refusal(f, 0, res) != "" {
		return nil
	}
	return &res
}

// caseDep is the dependency a case of a dependency kind is about, the one
// its state key names, and nil for any other case.
func caseDep(c Case, f CaseFacts) *DepFact {
	if c.Kind != CaseDependencyFailed && c.Kind != CaseDependencyClosed {
		return nil
	}
	i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool { return d.TaskID == c.StateKey })
	if i < 0 {
		return nil
	}
	return &f.Dependencies[i]
}

// OpenTrip is the one function every trip calls. It reads the card's
// facts and opens the stop's case. A live case with the stop's key at the
// human is the repeat guard: the stop came again, a step says so, and the
// card waits on that case's ask, with no new face. On a fresh case rung 0
// runs, and the first row that fires resolves the case through Act. With
// none, the human is asked (askHuman) and the case waits at the human. A
// restart's trip (healedReason) waits on the card's newest live ask,
// faces it when it has no face yet, and with none opens the stop the card
// asked last as its own case again (healedTrip). An attempt after the
// first takes what the attempts before left (retriedTrip).
func (r *Runner) OpenTrip(ctx context.Context, in TripInput, at tripAttempt) (TripOutcome, error) {
	t, _, err := r.task(ctx, in.TaskID)
	if err != nil {
		return TripOutcome{}, err
	}
	if in.Reason == healedReason && in.Kind == "" {
		return r.healedTrip(ctx, t, in, at.ID)
	}
	f, err := r.tripFacts(ctx, t, in.Head)
	if err != nil {
		return TripOutcome{}, err
	}
	f.Killed = in.Killed
	c, cases, err := r.tripCase(ctx, t, f, in)
	if err != nil {
		return TripOutcome{}, err
	}
	f.atCase(c)
	if at.N > 1 {
		if out, ok, err := r.retriedTrip(ctx, t, cases, at.ID); ok || err != nil {
			return out, err
		}
	}
	if at.ID != "" {
		c.Facts["trip"] = at.ID
	}
	c, opened, err := r.store.OpenCase(ctx, c)
	if err != nil {
		return TripOutcome{}, err
	}
	if !opened && c.Status == CaseAtHuman {
		return r.waitsAtHuman(ctx, t, c, "the same stop again: "+c.Problem)
	}
	res, found, err := r.rungZero(ctx, t, c, f, cases)
	if err != nil || res != nil {
		return TripOutcome{CaseID: c.ID, Resolution: res}, err
	}
	if err := r.tripStep(ctx, c.ID, found); err != nil {
		return TripOutcome{}, err
	}
	comment, err := r.askHuman(ctx, t, c, in.Reason, tripNext(c, in.Reason), in.Choices)
	if err != nil {
		return TripOutcome{}, err
	}
	if err := r.store.SetCaseStatus(ctx, c.ID, CaseAtHuman); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: comment, Rung: 3}, nil
}

// rungZero runs rung 0 on fresh case c of card t: no row on facts read
// partly, which could hide what a row needs (a head a rerun made), none
// when rung 0 resolved a case of c's kind and key before, for the stop
// came back, else the first row of rung0 that fires, which Act carries
// out on rung 0. It answers the resolution taken, or nil and what rung 0
// found: a row Act refused or failed hands the case up with why, so rung
// 0 never keeps the human from being asked.
func (r *Runner) rungZero(ctx context.Context, t Task, c Case, f CaseFacts, cases []Case) (*Resolution, string, error) {
	if len(f.Partial) > 0 {
		return nil, "facts read partly: " + textutil.AndList(f.Partial) + ", so rung 0 took nothing", nil
	}
	if i := slices.IndexFunc(cases, func(e Case) bool {
		return e.ID != c.ID && e.Kind == c.Kind && e.StateKey == c.StateKey && e.Status == CaseResolved && e.Resolution != nil && e.Resolution.Rung == 0
	}); i >= 0 {
		return nil, "rung 0 took this stop with " + cases[i].Resolution.Action + " before, and it came back", nil
	}
	for _, row := range rung0 {
		res, err := row.Fire(ctx, r, c, f)
		if err != nil {
			return nil, "", err
		}
		if res == nil {
			continue
		}
		_, err = r.Act(ctx, ActInput{TaskID: t.ID, CaseID: c.ID, By: FactoryDecider, Via: ViaFactoryd, Rung: 0, Res: *res})
		var refused ErrActRefused
		var noRetry ErrNoRetry
		switch {
		case ctx.Err() != nil:
			return nil, "", ctx.Err()
		case errors.As(err, &refused), errors.As(err, &noRetry):
			return nil, row.Name + " was refused: " + err.Error(), nil
		case err != nil:
			return nil, row.Name + " failed: " + err.Error(), nil
		}
		return res, "", nil
	}
	return nil, "no resolver fits", nil
}

// healedTrip is the trip, of id trip, of a run that restarted while its
// card waited on a human: the card's newest live ask (NewestAskCase) at
// the human waits on with a step and no new face. One with no face yet
// gets it, on its own reason. With none, the stop the card asked last
// (restartedStop) opens its own case again, as its trip opened it. Either
// way the case takes the restart's step of rung 0, a trip under the
// gate's mark (runGrants), and the face names what moves the card on by
// the case's kind and reason.
func (r *Runner) healedTrip(ctx context.Context, t Task, in TripInput, trip string) (TripOutcome, error) {
	const restarted = "asked again after a restart"
	c, err := r.store.NewestAskCase(ctx, t.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	if c.Status == CaseAtHuman {
		return r.waitsAtHuman(ctx, t, c, restarted)
	}
	stop, _ := c.Facts["reason"].(string)
	if c.ID == 0 {
		if stop, err = r.restartedStop(ctx, t); err != nil {
			return TripOutcome{}, err
		}
		f, err := r.tripFacts(ctx, t, in.Head)
		if err != nil {
			return TripOutcome{}, err
		}
		if c, _, err = r.tripCase(ctx, t, f, TripInput{TaskID: t.ID, Reason: stop, Choices: in.Choices, Head: in.Head}); err != nil {
			return TripOutcome{}, err
		}
		if trip != "" {
			c.Facts["trip"] = trip
		}
		if c, _, err = r.store.OpenCase(ctx, c); err != nil {
			return TripOutcome{}, err
		}
	}
	if err := r.tripStep(ctx, c.ID, restarted); err != nil {
		return TripOutcome{}, err
	}
	comment, err := r.askHuman(ctx, t, c, in.Reason, tripNext(c, stop), in.Choices)
	if err != nil {
		return TripOutcome{}, err
	}
	if err := r.store.SetCaseStatus(ctx, c.ID, CaseAtHuman); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: comment, Rung: 3}, nil
}

// restartedStop is the reason of the stop card t asked last, which a
// restart asks again: its newest escalation that is no restart's own,
// healedReason when it has none.
func (r *Runner) restartedStop(ctx context.Context, t Task) (string, error) {
	events, err := r.store.IntakeEvents(ctx, t.ID)
	if err != nil {
		return "", err
	}
	stop := healedReason
	for _, e := range events {
		if e.Kind == IntakeEscalation && e.Body != healedReason && strings.TrimSpace(e.Body) != "" {
			stop = e.Body
		}
	}
	return stop, nil
}

// retriedTrip is what the attempts before trip's left on a case, ok false
// for nothing to take: the case they opened, when rung 0
// resolved it, with its resolution, or when it waits at the human, on
// which the card waits with no new step and no new face. It goes by the
// trip alone, not the key or the kind: rung 0's retry of a dependency moves
// the key the trip reads, and a retry that runs on a newer binary may read
// the stop as another kind than the attempt before it.
func (r *Runner) retriedTrip(ctx context.Context, t Task, cases []Case, trip string) (TripOutcome, bool, error) {
	i := slices.IndexFunc(cases, func(c Case) bool { return c.Facts["trip"] == trip })
	if i < 0 {
		return TripOutcome{}, false, nil
	}
	switch c := cases[i]; {
	case c.Status == CaseResolved && c.Resolution != nil && c.Resolution.Rung == 0:
		return TripOutcome{CaseID: c.ID, Resolution: &Resolution{Action: c.Resolution.Action, Args: c.Resolution.Args, Note: c.Resolution.Reason}}, true, nil
	case c.Status == CaseAtHuman:
		return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, true, r.waitOnHuman(ctx, t)
	}
	return TripOutcome{}, false, nil
}

// tripStep is rung 0's step on case id that found found and handed the
// case up, appended once: not again when the case's last step is rung
// 0's with the same words, as a retried trip would write it.
func (r *Runner) tripStep(ctx context.Context, id int64, found string) error {
	steps, err := r.store.CaseSteps(ctx, id)
	if err != nil {
		return err
	}
	if n := len(steps); n > 0 && steps[n-1].Rung == 0 && steps[n-1].Found == found {
		return nil
	}
	_, err = r.store.AppendCaseStep(ctx, CaseStep{CaseID: id, Rung: 0, Actor: FactoryDecider, Found: found, Outcome: StepHandedUp})
	return err
}

// waitsAtHuman is the trip that finds case c at the human already: a step
// on rung 0 that found found and handed up, the card waiting on the human
// unless c sits beside its run, and the comment of the case's newest ask,
// which no new face follows.
func (r *Runner) waitsAtHuman(ctx context.Context, t Task, c Case, found string) (TripOutcome, error) {
	if err := r.tripStep(ctx, c.ID, found); err != nil {
		return TripOutcome{}, err
	}
	if c.Kind == CaseMonitorFlag {
		return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, nil
	}
	if err := r.waitOnHuman(ctx, t); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, nil
}

// tripCase is the case trip in makes on card t with facts f, open until
// its face is written, with the card's cases, for OpenCase to open. The kind is caseKindOf's
// of the reason, or in's own with its key. The problem is the shaped
// question's ask where a template names the decision, else the reason's
// first line. The facts are f with the reason, its source, what the reason
// names and in's extra facts, the money the card's for a stop on its own
// budget, with what the next opening needs at the floor. The case follows
// the card's newest ended one.
func (r *Runner) tripCase(ctx context.Context, t Task, f CaseFacts, in TripInput) (Case, []Case, error) {
	cases, err := r.store.CasesOf(ctx, t.ID)
	if err != nil {
		return Case{}, nil, err
	}
	source := escalationSource(in.Reason)
	kind, named := caseKindOf(source, in.Reason)
	if in.Kind != "" {
		kind, named = in.Kind, nil
	}
	if kind == CaseOther && t.Kind == "human" {
		kind = CaseHumanCard
	}
	problem := ShapedQuestion(in.Reason, r.questionFacts(ctx, t, in.Reason), in.Choices).Ask
	if problem == plainAsk {
		problem = escalationQuestion(in.Reason)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return Case{}, nil, err
	}
	var facts map[string]any
	if err := json.Unmarshal(b, &facts); err != nil {
		return Case{}, nil, err
	}
	maps.Copy(facts, named)
	maps.Copy(facts, in.Extra)
	facts["source"], facts["reason"] = source, textutil.Truncate(in.Reason, 300)
	if in.CheckinStop != "" {
		facts["checkin_stop"] = in.CheckinStop
	}
	key := tripKey(kind, problem, f.Head, f)
	if in.Kind != "" {
		key = in.StateKey
	}
	c := Case{TaskID: t.ID, Repository: t.Repository, Kind: kind, StateKey: key,
		Status: CaseOpen, Problem: problem, Facts: facts, Money: tripMoney(kind, f)}
	for _, e := range cases {
		if e.Status != CaseOpen && e.Status != CaseAtHuman {
			c.Follows = max(c.Follows, e.ID)
		}
	}
	return c, cases, nil
}

// tripKey is the state key of a trip's case of kind: head for a stop at
// the head, the dependency's task id for a dependency's, else the
// problem's words (caseStateKey), as for a kind with no head.
func tripKey(kind CaseKind, problem, head string, f CaseFacts) string {
	switch {
	case slices.Contains(headKinds, kind) && head != "":
		return head
	case kind == CaseDependencyFailed || kind == CaseDependencyClosed:
		if d := tripDep(kind, f); d != nil {
			return d.TaskID
		}
	}
	return caseStateKey(kind, problem, "")
}

// tripDep is the dependency a trip of a dependency kind stops on, as the
// Dependencies activity names it: the first failed one with no
// replacement, the first closed one.
func tripDep(kind CaseKind, f CaseFacts) *DepFact {
	i := slices.IndexFunc(f.Dependencies, func(d DepFact) bool {
		if kind == CaseDependencyFailed {
			return d.Status == "failed" && d.Replaced == ""
		}
		return d.Status == "closed"
	})
	if i < 0 {
		return nil
	}
	return &f.Dependencies[i]
}

// tripMoney is the money fact of a case of kind on facts f, nil for none:
// a budget floor's names what the next opening needs past what a session may spend, in
// cents rounded up, and what that buys. The other stops on
// the card's own budget name the budget and what is spent.
func tripMoney(kind CaseKind, f CaseFacts) *CaseMoney {
	switch {
	case kind == CaseBudgetFloor:
		buys := "one session"
		if f.Model != "" {
			buys += " on " + f.Model
		}
		var need float64
		if f.SessionLeft() < f.FloorEUR {
			need = fundFor(f.FloorEUR, f.SessionLeft())
		}
		return &CaseMoney{Kind: "budget", BudgetEUR: f.BudgetEUR, SpentEUR: f.SpentEUR, NeedEUR: need, Buys: buys}
	case slices.Contains(cardMoneyKinds, kind):
		return &CaseMoney{Kind: string(kind), BudgetEUR: f.BudgetEUR, SpentEUR: f.SpentEUR}
	}
	return nil
}

// replyNext is the last line of the face of reason when a reply moves the
// card on, as it does in a run from before the cases gate: a reply starts
// an attempt, except at a budget floor no answer can fund, and at a cap
// that turns on its own it starts one sooner.
func replyNext(reason string) string {
	switch {
	case strings.HasPrefix(reason, leftQueue):
		return "Reply on this issue to put the pull request back at the end of the merge queue, request changes on it to send the task back to the agent, or close the issue to end the task."
	case capWaitDecision(reason) != "":
		return "The task resumes on its own as above. Reply on this issue to start a new attempt sooner with your comment as guidance, or close the issue to end the task."
	case floorRefused(reason):
		// no answer can run: the card names why and the way out
		return "An answer starts an attempt only once the budget can fund a call, as above. Close this issue to end the task."
	}
	return "Reply on this issue to start a new attempt with your comment as guidance, or close the issue to end the task."
}

// tripNext is the last line of a trip's face for case c: at a cap, where
// a reply is a continue that asks again, the Extend form or the chat's
// extend start the next attempt, at a budget floor the Fund form or the
// chat's fund, for a monitor flag, whose card goes on, the case by its
// number in the chat, and elsewhere a reply (replyNext).
func tripNext(c Case, reason string) string {
	switch c.Kind {
	case CaseMonitorFlag:
		return fmt.Sprintf("The card goes on meanwhile. Answer case %d in the project chat.", c.ID)
	case CaseSessionCap, CaseAgentTimeCap:
		return "Use Extend on the board, or ask the project chat to extend the card, to start a new attempt. A reply alone starts none. Close this issue to end the task."
	case CaseCheckinFailed:
		return "The factory's own check-in failed, not the card's work. Reply on this issue to start a new attempt with your comment as guidance, or close the issue to end the task."
	case CaseBudgetFloor:
		return "Use the Fund form on the board, or ask the project chat to fund the card, to start a new attempt. A reply alone starts none. Close this issue to end the task."
	}
	return replyNext(reason)
}

// askHuman is a case's human face (architecture 6.6): state
// waiting-human, mirrored on the issue, a comment with the reason and
// next, the line that says what moves the card on, and a question_asked
// row with its question and case c (none for the zero Case); the agent's
// choices, if any, ride on that row, the timeline's step and the intake
// event the card reads. A monitor flag's case sits beside the card's run:
// its face is the comment, the row and the alert, and moves no status,
// writes no ask the run reads and no timeline line. It returns the
// comment's id: only later comments answer it.
func (r *Runner) askHuman(ctx context.Context, t Task, c Case, reason, next string, choices []string) (int64, error) {
	beside := c.Kind == CaseMonitorFlag
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return 0, terminalError("%v", err)
	}
	// the reason in a code block: a mention the agent wrote rings no bell
	safe := r.untrusted(t, reason)
	body := "factoryd: task " + t.ID + " needs a human.\n\n" + fenced(safe)
	if links := sanitize.Links(safe, r.linkHosts(t)); len(links) > 0 {
		// the fence renders no link: the ones the sanitiser keeps follow it
		body += "\n\nLinks:\n- " + strings.Join(links, "\n- ")
	}
	body += "\n\n" + next
	if strings.HasPrefix(reason, screeningAsks) {
		// the reasons go to the card only: they would teach the author
		body = screenedLine
	}
	var id int64
	if t.IssueNumber != 0 {
		// a task without one (a report task) has its question on the card alone
		if id, err = r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body); err != nil {
			return 0, err
		}
	}
	// a clicked choice is posted as the member's comment: agent text,
	// sanitised like the reason
	choices = slices.Clone(choices)
	for i, c := range choices {
		choices[i] = r.untrusted(t, c)
	}
	question := escalationQuestion(reason)
	var shaped *HumanQuestion
	if reason != healedReason && !strings.HasPrefix(reason, screeningAsks) {
		// the deterministic stops shape their five-part question from the
		// stop's own numbers; every other ask gets the same five parts
		// from its reason, so no card asks with words alone
		if beside {
			shaped = monitorQuestion(reason, c.ID)
		} else {
			shaped = ShapedQuestion(reason, r.questionFacts(ctx, t, reason), choices)
		}
		question = shaped.Ask
		// the refusal the newest session ended on rides with the ask,
		// verbatim: the blocker reaches the human with no agent-side
		// reporting to lose it (card #1345). A prover's refusals name its
		// hold-outs, so its ask is factoryd's words alone.
		if t.Source != SourceProver {
			if tool, refusal, ok := r.lastToolRefusal(ctx, t.ID); ok {
				shaped.Info += "\n\nThe session's last tool call (" + tool + ") was refused:\n\n" + fenced(refusal)
			}
		}
	}
	if reason == healedReason {
		// the restart's sentence asks nothing: the row names the question it asks again
		events, err := r.store.IntakeEvents(ctx, t.ID)
		if err != nil {
			log.Printf("task %s: the question a healed run asks again: %v", t.ID, err)
		}
		_, again, _ := answersFrom(events)
		question = cmp.Or(again, question)
	}
	if !beside {
		// the durable anchor Reconcile rebuilds the answers from; a retry
		// with the same comment records nothing twice. It is written before
		// the wait: a card in waiting-human without its row has no ask
		if _, err := r.store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeEscalation, Author: "factoryd", Body: reason, CommentID: id,
			Choices: choices, Question: shaped}); err != nil {
			return 0, fmt.Errorf("task %s: record escalation comment %d: %w", t.ID, id, err)
		}
		if err := r.waitOnHuman(ctx, t); err != nil {
			return 0, err
		}
	}
	// the shaped question's buttons are not these choices: they carry
	// values, and park and keep waiting record an answer without posting
	// one, so the console renders them from the question itself
	step := map[string]any{"reason": reason, "comment": id}
	asked := map[string]any{"question": textutil.Truncate(question, 300), "comment": id, "source": escalationSource(reason),
		"reason": textutil.Truncate(reason, 300)}
	if c.ID != 0 {
		asked["case_id"] = c.ID
	}
	if beside {
		// the card's own ask stays the card's: its thread seeds from that.
		// The flag tells the settle the ask of an ended card's open issue
		asked["case_kind"], asked["flag"] = string(c.Kind), c.Facts["flag"]
	}
	if shaped != nil {
		asked["ask"], asked["why"], asked["info"] = shaped.Ask, shaped.Why, shaped.Info
		if shaped.Recommendation != "" {
			asked["recommendation"] = shaped.Recommendation
		}
		asked["choices"] = shaped.Labels()
	}
	if len(choices) > 0 {
		step["choices"] = choices
		if shaped == nil {
			asked["choices"] = choices
		}
	}
	if !beside {
		r.record(ctx, t.ID, "escalated", step)
	}
	r.timeline.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd, asked)
	if reason == healedReason || strings.HasPrefix(reason, screeningAsks) {
		return id, nil // the question asked again, or the screening's: its alert is open already
	}
	if _, err := r.store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: escalationSource(reason), TaskID: t.ID,
		Detail: map[string]any{"comment": id, "reason": textutil.Truncate(reason, 2000)}}); err != nil {
		log.Printf("task %s: record the escalation alert: %v", t.ID, err)
	}
	return id, nil
}

// waitOnHuman moves card t to waiting-human, mirrored on its issue, unless
// it waits there already.
func (r *Runner) waitOnHuman(ctx context.Context, t Task) error {
	if t.Status == "waiting-human" {
		return nil
	}
	// queued: the screening stage's suspect verdict, before any session;
	// waiting-deps: a dependency ended failed or closed; waiting-poster:
	// the author's answer screened suspect, or a cap reached on it;
	// waiting-image: the image did not come within imageWait
	if err := r.store.TransitionTask(ctx, t.ID, []string{"queued", "waiting-deps", "running", "in-review", "waiting-poster", "waiting-image"}, "waiting-human"); err != nil {
		return terminalError("task %s is %s, cannot wait for a human: %v", t.ID, t.Status, err)
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	// a parked card holds no slot: the next queued card takes it
	pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	return nil
}

// openTrip is OpenTrip's activity, on the activity's attempt.
func (r *Runner) openTrip(ctx context.Context, in TripInput) (TripOutcome, error) {
	info := activity.GetInfo(ctx)
	return r.OpenTrip(ctx, in, tripAttempt{ID: info.WorkflowExecution.RunID + "/" + info.ActivityID, N: info.Attempt})
}
