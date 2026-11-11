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
	"sync"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	// Model is the model the run's next session opens on, whose opening
	// floor the facts weigh; "" from a run that reads no floor (the plan
	// loop) or from before versionTripModel.
	Model string `json:",omitempty"`
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
// the case: 0 when it resolved it, 1 when rung 1 works on it in the
// background and the run waits on its resolution, 3 when the case waits on
// the human.
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
	expired := []CaseStep{}
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
			err := r.store.EndCaseStep(ctx, CaseStep{ID: st.ID, Outcome: StepHandedUp, Found: stepExpired(st.Found, st.StartedAt, *st.DueAt), Did: st.Did, Decision: st.Decision, Grounds: st.Grounds, CostEUR: st.CostEUR})
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			st.Found = stepExpired(st.Found, st.StartedAt, *st.DueAt)
			expired = append(expired, st)
			ended = append(ended, fmt.Sprintf("rung %d did not answer within %s", st.Rung, durationText(st.DueAt.Sub(st.StartedAt).Round(time.Minute))))
		}
	}
	return ended, r.climbExpired(ctx, expired)
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
// human, or with rung 1 at work on it, is the repeat guard: the stop came
// again, a step says so, and the card waits on that case, with no new face
// and no second conversation. On a fresh case rung 0 runs, and the first
// row that fires resolves the case through Act. With none the case climbs
// (climb): to rung 1 when it judges, else to the human. A
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
	note, err := r.planDraftNote(ctx, t.ID, in.Reason)
	if err != nil {
		return TripOutcome{}, err
	}
	f, err := r.tripFacts(ctx, t, in.Head, in.Model)
	if err != nil {
		return TripOutcome{}, err
	}
	f.Killed = in.Killed
	c, cases, err := r.tripCase(ctx, t, f, in)
	if err != nil {
		return TripOutcome{}, err
	}
	f.atCase(c)
	if note != "" {
		c.Facts["plan_draft"] = note
	}
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
	if !opened {
		if have, _ := c.Facts["plan_draft"].(string); have != note {
			if err := r.store.SetCaseFact(ctx, c.ID, "plan_draft", note); err != nil {
				return TripOutcome{}, err
			}
			if err := r.refreshDraftInfo(ctx, t.ID, have, note); err != nil {
				return TripOutcome{}, err
			}
		}
	}
	if !opened && c.Status == CaseAtHuman {
		return r.waitsAtHuman(ctx, t, c, "the same stop again: "+c.Problem)
	}
	if !opened {
		if out, ok, err := r.waitsOnRung(ctx, c); ok || err != nil {
			return out, err
		}
	}
	res, found, err := r.rungZero(ctx, t, c, f, cases)
	if err != nil || res != nil {
		return TripOutcome{CaseID: c.ID, Resolution: res}, err
	}
	return r.climb(ctx, t, c, f, 0, found)
}

// waitsOnRung is the trip that finds case c open with rung 1 or rung 2 at
// work on it: a step of rung 0 says the same stop came again, and the case
// stays with that rung, which starts no second conversation.
func (r *Runner) waitsOnRung(ctx context.Context, c Case) (TripOutcome, bool, error) {
	rung, err := r.rungWorks(ctx, c)
	if err != nil || rung == 0 {
		return TripOutcome{}, false, err
	}
	if err := r.tripStep(ctx, c.ID, "the same stop again: "+c.Problem); err != nil {
		return TripOutcome{}, false, err
	}
	return TripOutcome{CaseID: c.ID, Rung: rung}, true, nil
}

// climb is the one way a case goes from rung from to the next: from rung 0,
// after rung 0 found found and handed the case up, to rung 1, which works
// on it in the background, unless a human alone decides it, the project's
// rungs do not judge at its autonomy, the case is one rung 1 may not
// answer, no check-in model is set, or rung 1 judged the stop before (a
// moment's own human answer is its step); from rung 1, or after any of
// those, to rung 2, the factory agent's wake turn, unless the case is the
// human's alone, the rungs do not judge, the judgment budget is spent or
// rung 2 worked the case already; from rung 2, or after any of those, to
// the human, with the recommendation the rung before made. The card keeps
// its status while a rung works: only the face moves it. A case at the
// human climbs nowhere.
func (r *Runner) climb(ctx context.Context, t Task, c Case, f CaseFacts, from int, found string) (TripOutcome, error) {
	var unjudged *handUp
	if from == 0 {
		if err := r.tripStep(ctx, c.ID, found); err != nil {
			return TripOutcome{}, err
		}
		if r.rungOneJudges(t, c, f) {
			var err error
			if unjudged, err = r.spentJudgment(ctx, t, c); err != nil {
				return TripOutcome{}, err
			}
			if unjudged == nil {
				limit := r.cfg.Policy().EscalationOf(t.Repository).Rung1Runs
				started, atLimit, err := r.startRung1(ctx, t, c, limit)
				if err != nil {
					return TripOutcome{}, err
				}
				if started {
					return TripOutcome{CaseID: c.ID, Rung: 1}, nil
				}
				if atLimit {
					if unjudged, err = r.countReached(ctx, c, limit); err != nil {
						return TripOutcome{}, err
					}
				} else if rung, err := r.rungWorks(ctx, c); err != nil || rung == 1 {
					return TripOutcome{CaseID: c.ID, Rung: 1}, err
				}
			}
		}
		if c.Kind == CaseCheckinHuman {
			if err := r.momentStep(ctx, c); err != nil {
				return TripOutcome{}, err
			}
		}
	}
	live, err := r.store.CaseByID(ctx, c.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	if live.Status != CaseOpen {
		return TripOutcome{CaseID: c.ID, Comment: live.CommentID, Rung: 3}, nil
	}
	if from < 2 && (unjudged == nil || unjudged.Mark != markJudgmentSpent) {
		started, spent, err := r.startRung2(ctx, t, live, f)
		if err != nil || started {
			return TripOutcome{CaseID: c.ID, Rung: 2}, err
		}
		unjudged = cmp.Or(spent, unjudged)
	}
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	reason, choices := faceOf(live)
	up := handedUpBy(steps, from, found)
	switch {
	case unjudged != nil && unjudged.Mark == "" && up != nil:
		// a rung judged the case: the face is its own, with a line for the rung that is missing
		up.Note = unjudged.Note
	case unjudged != nil:
		if up != nil {
			unjudged.Action, unjudged.Grounds, unjudged.ActionRung = up.Action, up.Grounds, up.ActionRung
		}
		up = unjudged
	}
	out, err := r.face(ctx, t, live, reason, tripNext(live, reason), choices, up)
	if err == nil && out.Comment != 0 && live.Kind != CaseMonitorFlag {
		// the trip may have returned without a comment (rung 1 held it):
		// the run learns this face's. A run that has the comment ignores it.
		// off the caller's path: the bounded retries may take long
		r.rungs.Add(1)
		go func() {
			defer r.rungs.Done()
			r.signalFaced(context.WithoutCancel(ctx), t.ID, live.ID, out.Comment)
		}()
	}
	return out, err
}

// spentJudgment is the hand-up of a case that goes to the human with no
// rung 1 conversation because the judgment budget is spent: the one money
// case is filed, the case says so, and a step of rung 0 says why. Nil when
// rung 1 may take the case: money is left, or rung 1 judged it already.
func (r *Runner) spentJudgment(ctx context.Context, t Task, c Case) (*handUp, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 1 }) {
		return nil, err
	}
	if _, _, ok := r.cfg.Policy().ProjectOf(t.Repository); !ok {
		found := r.scrub(fmt.Sprintf("%s: repository %s names no project, so its judgment budget cannot be read", markJudgmentUnread, t.Repository))
		return &handUp{Found: found, Mark: markJudgmentUnread}, r.stepOnce(ctx, c, steps, markJudgmentUnread, found)
	}
	return r.judgmentSpent(ctx, t, c, r.callModel(t, policy.PurposeCheckin))
}

// noJudgmentLine is the face's line for a case a rung judged but the rung
// above could not, for want of money.
const noJudgmentLine = "The factory agent did not judge it: the judgment budget is spent."

// judgmentSpent is spentJudgment's path in full, for the rung whose calls run
// on model: nil when the chat task holds the money one call needs. A case no
// rung judged is marked; one a rung judged is not, and the hand-up carries
// noJudgmentLine instead.
func (r *Runner) judgmentSpent(ctx context.Context, t Task, c Case, model string) (*handUp, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	chat, err := r.judgmentTask(ctx, t)
	if err != nil {
		return nil, err
	}
	var need attemptNeed
	if r.cfg.Prices != nil {
		if need, err = r.needOf(ctx, r.cfg.Policy(), t, model, false); err != nil {
			return nil, err
		}
	}
	left := chat.BudgetEUR - chat.EnforcedSpent()
	if left > 0 && left >= need.eur {
		return nil, nil
	}
	r.fileJudgmentCard(ctx, t, chat)
	judged := slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 1 && s.Outcome == StepHandedUp && s.Decision != "" })
	if !judged {
		if err := r.store.AppendCaseProblem(ctx, c.ID, " ("+markJudgmentSpent+")"); err != nil {
			return nil, err
		}
	}
	found := fmt.Sprintf("%s: the chat task %s has %.2f of %.2f EUR left", markJudgmentSpent, chat.ID, left, chat.BudgetEUR)
	if judged {
		stepFound := fmt.Sprintf("%s The chat task %s has %.2f of %.2f EUR left.", noJudgmentLine, chat.ID, left, chat.BudgetEUR)
		return &handUp{Note: noJudgmentLine}, r.stepOnce(ctx, c, steps, noJudgmentLine, stepFound)
	}
	return &handUp{Found: found, Mark: markJudgmentSpent}, r.stepOnce(ctx, c, steps, markJudgmentSpent, found)
}

// stepOnce appends found as a step of rung 0 on case c unless one of steps
// begins with prefix already: a retried trip adds none.
func (r *Runner) stepOnce(ctx context.Context, c Case, steps []CaseStep, prefix, found string) error {
	if slices.ContainsFunc(steps, func(s CaseStep) bool {
		return s.Rung == 0 && (strings.HasPrefix(s.Found, prefix) || strings.HasPrefix(s.Found, markJudgmentSpent) || strings.HasPrefix(s.Found, noJudgmentLine))
	}) {
		return nil
	}
	_, err := r.store.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 0, Actor: FactoryDecider, Found: found, Outcome: StepHandedUp})
	return err
}

// fileJudgmentCard files the judgment budget's money card. A failure does
// not hold the case from its human: the chat's stop at this budget is
// recorded, which the card loop files from on its own.
func (r *Runner) fileJudgmentCard(ctx context.Context, t Task, chat Task) {
	if r.ChatBudget == nil {
		return
	}
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err == nil && !ok {
		err = fmt.Errorf("project of %s: %w", t.Repository, ErrNotFound)
	}
	if err == nil {
		if err = r.ChatBudget.File(ctx, p, chat); err == nil {
			return
		}
	}
	log.Printf("task %s: the judgment budget's card: %s", t.ID, r.scrub(err.Error()))
	stops, serr := r.store.TaskEventsOfKind(ctx, chat.ID, EventChatBudgetSpent, 1)
	if serr == nil && len(stops) > 0 {
		if at, _ := stops[0].Detail["budget_eur"].(float64); at >= chat.BudgetEUR {
			return
		}
	}
	now := r.clock()
	spent7, err := r.store.TaskSpendSince(ctx, chat.ID, now.Add(-7*24*time.Hour))
	if err != nil {
		log.Printf("task %s: read the chat spend: %s", t.ID, r.scrub(err.Error()))
	}
	if _, err := r.store.Record(ctx, Event{Repository: t.Repository, TaskID: chat.ID, Kind: EventChatBudgetSpent, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"budget_eur": chat.BudgetEUR, "spent_eur": chat.EnforcedSpent(), "suggest_eur": SuggestFund(spent7, now)}}); err != nil {
		log.Printf("task %s: record the judgment budget's stop: %s", t.ID, r.scrub(err.Error()))
	}
}

// countReached is the hand-up of a case rung 1 may not take because its
// card has had the per-card count of rung 1 runs.
func (r *Runner) countReached(ctx context.Context, c Case, limit int) (*handUp, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	found := fmt.Sprintf("rung 1 reached its per-card count of %d on this card", limit)
	return &handUp{Found: found, Mark: found}, r.stepOnce(ctx, c, steps, found, found)
}

const (
	markJudgmentSpent  = "not judged, judgment budget spent"
	markJudgmentUnread = "not judged, judgment budget unreadable"
)

// facedTries and facedBackoff bound the retries of a case_faced signal.
const facedTries = 4

var facedBackoff = time.Second

// signalFaced tells card taskID's run the comment of case caseID's face: a
// failed send is tried again, with a doubling pause, up to facedTries times
// or until ctx ends, and one that delivers ends the signal's pending row,
// which the heal pass sends again until one does.
func (r *Runner) signalFaced(ctx context.Context, taskID string, caseID, comment int64) {
	sig := facedSignal(caseID, comment)
	pause := facedBackoff
	for try := 1; ; try++ {
		err := r.cfg.Workflows.Signal(ctx, taskID, SignalCaseFaced, sig)
		if err == nil {
			r.settleSignal(ctx, caseID, SignalCaseFaced)
			return
		}
		log.Printf("task %s: signal %s of case %d (try %d): %s", taskID, SignalCaseFaced, caseID, try, r.scrub(err.Error()))
		if try == facedTries {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause *= 2
	}
}

// rungTwoTakes reports whether rung 2 takes the case c, on facts f: the
// rungs judge at the project's autonomy, and a human alone does not decide
// the case.
func (r *Runner) rungTwoTakes(t Task, c Case, f CaseFacts) bool {
	if _, _, ok := r.cfg.Policy().ProjectOf(t.Repository); !ok {
		return false
	}
	if human, _ := humanOnly(c, f); human || c.Kind == CaseHumanCard || c.Kind == CaseJudgmentBudget || c.Kind == CaseMergeApproval {
		return false
	}
	return r.cfg.Policy().EscalationOf(t.Repository).Judges(f.Autonomy)
}

// startRung2 hands case c to rung 2 as a step in progress due after the
// rung's bound, the case's wake on rung 2, and a case_climbed row the Waker
// wakes the factory agent on. It starts nothing, started false, for a case
// rung 2 does not take or one it worked already; a spent judgment budget
// starts nothing either and answers the case's unjudged hand-up (spent).
func (r *Runner) startRung2(ctx context.Context, t Task, c Case, f CaseFacts) (started bool, spent *handUp, err error) {
	if !r.rungTwoTakes(t, c, f) {
		return false, nil, nil
	}
	lock, _ := r.caseLocks.LoadOrStore(c.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 2 }) {
		return false, nil, err
	}
	if spent, err = r.judgmentSpent(ctx, t, c, r.wakeModel(ctx)); err != nil || spent != nil {
		return false, spent, err
	}
	due := r.clock().Add(r.cfg.Policy().WaitsOf(t.Repository).RungAnswer)
	if _, err := r.store.StartRungStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepInProgress, DueAt: &due}); err != nil {
		return false, nil, err
	}
	r.timeline.RecordRow(ctx, t.ID, EventCaseClimbed, FactoryDecider, ViaFactoryd, map[string]any{
		"case_id": c.ID, "rung": 2, "kind": string(c.Kind), "problem": textutil.Truncate(r.scrub(c.Problem), 300)})
	return true, nil, nil
}

// wakeModel is the class rung 2's wake runs on now (WakeClass). A spend that
// cannot be read runs on big.
func (r *Runner) wakeModel(ctx context.Context) string {
	e, err := r.store.Enforced(ctx, r.clock())
	if err != nil {
		log.Printf("premium's month spend: %s", r.scrub(err.Error()))
	}
	return WakeClass(r.cfg.Policy().Document, e, err)
}

// Rung2PremiumShare is the part of premium's month cap that rung 2's wakes
// may spend: premium keeps the rest for the human's own turns.
const Rung2PremiumShare = 0.85

// WakeClass is the class of a wake that names a case: premium while the
// month's premium spend e is under Rung2PremiumShare of premium's cap, big
// from then until the month turns, and big when the spend could not be read
// (err). A premium that has no cap of its own is always premium; one with no
// output limit is not called.
func WakeClass(d policy.Document, e policy.Enforced, err error) string {
	if _, ok := d.Prices.MaxOutputTokens[policy.Premium]; !ok || err != nil {
		return policy.Big
	}
	if cap := d.Budget.PremiumMonthEUR; cap > 0 && e.Premium >= Rung2PremiumShare*cap {
		return policy.Big
	}
	return policy.Premium
}

// facedSignal is the case_faced that tells a run the comment of case
// caseID's face.
func facedSignal(caseID, comment int64) TaskSignal {
	return TaskSignal{Author: FactoryDecider, Comment: comment, Case: &CaseSignal{CaseID: caseID, Rung: 3, By: FactoryDecider, Comment: comment}}
}

// rungOneJudges reports whether rung 1 takes the case c, on facts f.
func (r *Runner) rungOneJudges(t Task, c Case, f CaseFacts) bool {
	if human, _ := humanOnly(c, f); human || c.Kind == CaseCheckinHuman || c.Kind == CaseHumanCard || c.Kind == CaseJudgmentBudget || c.Kind == CaseMergeApproval {
		return false
	}
	return r.cfg.Policy().EscalationOf(t.Repository).Judges(f.Autonomy) && r.checkin != nil && r.callModel(t, policy.PurposeCheckin) != ""
}

// momentStep records a moment's own human answer on case c as rung 1's
// step handed up, once: the check-in already judged the stop.
func (r *Runner) momentStep(ctx context.Context, c Case) error {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 1 }) {
		return err
	}
	reason, _ := c.Facts["reason"].(string)
	_, err = r.store.AppendCaseStep(ctx, CaseStep{CaseID: c.ID, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp,
		Decision: CheckHuman, Grounds: textutil.Truncate(r.scrub(reason), 1200)})
	return err
}

// faceOf is the reason and the choices a case's face asks, as its trip
// stored them.
func faceOf(c Case) (reason string, choices []string) {
	reason, _ = c.Facts["face"].(string)
	if reason == "" {
		reason, _ = c.Facts["reason"].(string)
	}
	if reason == "" {
		reason = c.Problem
	}
	if raw, ok := c.Facts["choices"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				choices = append(choices, s)
			}
		}
	}
	return reason, choices
}

// handUp is what the rung before the human made of the case: the action it
// recommends with its grounds, and what it found.
type handUp struct {
	Action, Grounds, Found string
	// Rung is the rung that handed the case up, ActionRung the rung whose
	// recommendation Action is
	Rung, ActionRung int
	// Before is what the rung below an expired one found, for the face to
	// carry beside the expiry
	Before string
	// Note is a line the face adds, for a rung that could not judge the case
	Note string
	// Mark, when set, ends the ask itself: a case no rung judged says so
	Mark string
}

// handedUpBy is the hand-up of the rungs from rung from up (from rung 1 for
// a case that climbs from rung 0 after rung 1 judged it already): the
// newest step of one that ended handed up, with found as it climbed; nil for
// a case no rung above 0 handed up.
func handedUpBy(steps []CaseStep, from int, found string) *handUp {
	up := &handUp{Found: found, Rung: from}
	var first *CaseStep
	for k := len(steps) - 1; k >= 0; k-- {
		s := &steps[k]
		if s.Outcome != StepHandedUp || s.Rung < max(from, 1) || s.Rung >= 3 {
			continue
		}
		first = s
		break
	}
	if first == nil {
		if from == 0 {
			return nil
		}
		return up
	}
	up.Rung = first.Rung
	if from == 0 {
		// rung 0 climbs after a rung judged the case already: what that
		// rung found is the face's, not rung 0's
		up.Found = first.Found
	}
	if first.Decision == CheckHuman {
		// a moment's own human answer: its reason is what it found
		up.Found = cmp.Or(first.Found, first.Grounds)
		return up
	}
	// a rung that gave no recommendation (one that did not answer) leaves the
	// newest one the rungs below it made, and what it found
	if strings.Contains(first.Found, expiredMark) {
		for k := len(steps) - 1; k >= 0; k-- {
			s := steps[k]
			if s.Outcome != StepHandedUp || s.Rung != 1 {
				continue
			}
			if s.Decision == CheckHuman {
				up.Before = cmp.Or(s.Found, s.Grounds)
			} else {
				up.Before = s.Found
			}
			if up.Before != "" {
				break
			}
		}
	}
	for k := len(steps) - 1; k >= 0; k-- {
		s := steps[k]
		if s.Outcome == StepHandedUp && s.Rung >= 1 && s.Rung < 3 && s.Decision != "" && s.Decision != CheckHuman {
			up.Action, up.Grounds, up.ActionRung = s.Decision, s.Grounds, s.Rung
			break
		}
	}
	return up
}

// expiredMark is what ExpireSteps writes into the Found of a step whose rung
// did not answer.
const expiredMark = "the rung did not answer within "

// foundLine is what the rung before the human found, as the face reads it:
// an expired step says that its rung did not answer, no finding.
func foundLine(up *handUp) string {
	if _, bound, ok := strings.Cut(up.Found, expiredMark); ok && up.Rung == 2 {
		return "The factory agent did not answer within " + bound + "."
	}
	switch {
	case up.Mark != "":
		return "The factory found: " + up.Found
	case up.Rung == 2:
		return "The factory agent found: " + up.Found
	}
	return "The check-in found: " + up.Found
}

// beforeLine is what rung 1 found under rung 2's expiry: its finding, or that
// it too did not answer.
func beforeLine(found string) string {
	if _, bound, ok := strings.Cut(found, expiredMark); ok {
		return "The check-in did not answer within " + bound + "."
	}
	return "The check-in found: " + found
}

// errNoWait refuses a face of a card in status, which cannot wait for a
// human.
type errNoWait struct{ status string }

func (e errNoWait) Error() string {
	return "the card is " + e.status + ", so no run waits on a human's answer"
}

// face is the human's face of case c: the question asked (askHuman), the
// case at the human and its next move the human's. A card that cannot wait
// for a human gets no face, and the case ends.
func (r *Runner) face(ctx context.Context, t Task, c Case, reason, next string, choices []string, up *handUp) (TripOutcome, error) {
	comment, err := r.askHuman(ctx, t, c, reason, next, choices, up)
	if no := (errNoWait{}); errors.As(err, &no) {
		return TripOutcome{CaseID: c.ID, Rung: 3}, caseEnded(r.store.WithdrawCase(ctx, c.ID, no.Error()))
	}
	if err != nil {
		return TripOutcome{}, err
	}
	if err := r.store.SetCaseStatus(ctx, c.ID, CaseAtHuman); err != nil {
		return TripOutcome{}, err
	}
	if err := r.store.SetCaseWake(ctx, c.ID, 3, c.WakeOn, nil); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: comment, Rung: 3}, nil
}

// climbExpired climbs the case of each step that ended expired, from the
// step's rung, when its case is still open: the rung did not answer.
func (r *Runner) climbExpired(ctx context.Context, steps []CaseStep) error {
	var errs []error
	for _, st := range steps {
		c, err := r.store.CaseByID(ctx, st.CaseID)
		if err != nil || c.Status != CaseOpen {
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		t, _, err := r.task(ctx, c.TaskID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if c.Kind != CaseMonitorFlag && !slices.Contains(openStatuses, t.Status) {
			// no run waits on a human here: the heal pass ends the case
			continue
		}
		head, _ := c.Facts["head"].(string)
		model, _ := c.Facts["model"].(string)
		f, err := r.tripFacts(ctx, t, head, model)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		f.atCase(c)
		if _, err := r.climb(ctx, t, c, f, st.Rung, st.Found); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rungZero runs rung 0 on fresh case c of card t: no row on facts read
// partly, which could hide what a row needs (a head a rerun made), none
// when rung 0 resolved a case of c's kind and key before, for the stop
// came back, else the first row of rung0 that fires, which Act carries
// out on rung 0. It answers the resolution taken, or nil and what rung 0
// found: a row Act refused or failed hands the case up with why, so rung
// 0 never keeps the human from being asked.
func (r *Runner) rungZero(ctx context.Context, t Task, c Case, f CaseFacts, cases []Case) (*Resolution, string, error) {
	if partial := slices.DeleteFunc(slices.Clone(f.Partial), func(n string) bool { return n == "siblings" || n == "head_commits" || n == "checks_log" }); len(partial) > 0 {
		return nil, "facts read partly: " + textutil.AndList(partial) + ", so rung 0 took nothing", nil
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
		f, err := r.tripFacts(ctx, t, in.Head, in.Model)
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
	if out, ok, err := r.waitsOnRung(ctx, c); ok || err != nil {
		return out, err
	}
	return r.face(ctx, t, c, in.Reason, tripNext(c, stop), in.Choices, nil)
}

// refreshDraftInfo swaps the draft note have in the info of the question
// the card's newest escalation asked for note, so the face shows the draft
// as it is now.
func (r *Runner) refreshDraftInfo(ctx context.Context, taskID, have, note string) error {
	events, err := r.store.IntakeEvents(ctx, taskID)
	if err != nil {
		return err
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind != IntakeEscalation || e.Question == nil || e.CommentID == 0 {
			continue
		}
		info := e.Question.Info
		if have != "" {
			info = strings.Replace(info, "\n\n"+have, "", 1)
		}
		if note != "" {
			info += "\n\n" + note
		}
		return r.store.SetQuestionInfo(ctx, taskID, e.CommentID, info)
	}
	return nil
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
	case c.Status == CaseOpen:
		rung, err := r.rungWorks(ctx, c)
		return TripOutcome{CaseID: c.ID, Rung: rung}, rung != 0, err
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
	facts["source"], facts["reason"] = source, textutil.Truncate(r.Scrub.String(in.Reason), 300)
	if in.Reason != healedReason {
		facts["face"] = textutil.Truncate(r.Scrub.String(in.Reason), 4000)
	}
	if len(in.Choices) > 0 {
		facts["choices"] = in.Choices
	}
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

// noRoom is the face's line at a stop of case c whose card has no room
// for a session by its facts (CaseFacts.noSession, refund and waiting as
// there): a reply starts none, the task waits on a cap that binds first or
// asks the stop next, which a fund or an extend answers. With a limit
// unread a reply may start none. "" when a session fits.
func noRoom(c Case, refund, waiting bool) string {
	const extend = "Extend on the board or the project chat's extend"
	const fund = "the Fund form on the board or the project chat's fund"
	f := factsOf(c)
	var lead, asked, answer string
	switch kind, read := f.noSession(refund, waiting); {
	case !read:
		return "The card's limits could not be read, so a reply alone may start no attempt: " + extend + " buys sessions and time, the Fund form on the board or the chat's fund buys budget. Close this issue to end the task."
	case kind == CaseDayCap, kind == CaseMonthCap, kind == CaseProjectCap:
		// the cap wait's own question line: when it lifts, what raises it
		line := strings.ToUpper(f.CapWait[:1]) + f.CapWait[1:] + " A reply alone starts no attempt while the cap holds."
		if f.unfunded() {
			line += " Once it lifts, the task asks the budget floor, which " + fund + " answers."
		}
		return line
	case kind == CaseBudgetFloor:
		lead, asked, answer = "The card's budget no longer funds a session.", "budget floor", fund
	case kind == CaseSessionCap:
		lead, asked, answer = "The card's sessions are used up.", "session cap", extend
	case kind == CaseAgentTimeCap:
		lead, asked, answer = "The card's agent time is spent.", "agent-time cap", extend
	default:
		return ""
	}
	return lead + " A reply alone starts no attempt: the task asks the " + asked + " next, which " + answer + " answers. Close this issue to end the task."
}

// factsOf is case c's facts; a fact of the wrong type stays zero: unread.
func factsOf(c Case) CaseFacts {
	var f CaseFacts
	if b, err := json.Marshal(c.Facts); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	return f
}

// monitorFace is the face of a monitor flag on t: the reason's flag text
// is agent-derived, so it is fenced and scrubbed as every text a human
// reads.
func (r *Runner) monitorFace(t Task, reason string) *HumanQuestion {
	return monitorQuestion(r.Scrub.String(r.untrusted(t, reason)))
}

// tripNext is the last line of a trip's face for case c: at a cap, where
// a reply is a continue that asks again, the Extend form or the chat's
// extend start the next attempt, at a budget floor the Fund form or the
// chat's fund, for a monitor flag, whose card goes on, the flag in the
// chat, and elsewhere a reply (replyNext).
func tripNext(c Case, reason string) string {
	switch c.Kind {
	case CaseMonitorFlag:
		return "The card goes on meanwhile. Answer the flag in the project chat."
	case CaseSessionCap, CaseAgentTimeCap:
		return "Use Extend on the board, or ask the project chat to extend the card, to start a new attempt. A reply alone starts none. Close this issue to end the task."
	case CaseCheckinFailed:
		const failed = "The factory's own check-in failed, not the card's work. "
		if no := noRoom(c, false, false); no != "" {
			return failed + no
		}
		return failed + "Reply on this issue to start a new attempt with your comment as guidance, or close the issue to end the task."
	case CaseCheckinShare:
		if f := factsOf(c); f.PROpen && f.Head != "" {
			// a spend stop's reply verifies the open head, no session
			return "Reply on this issue to take the pull request's head through the gates again with your comment as guidance, or close the issue to end the task."
		}
		if no := noRoom(c, false, false); no != "" {
			return no
		}
	case CaseCheckinHuman, CasePodNeverStarted, CaseProviderRefused,
		CaseContentFilter, CaseAgentEscalate, CaseOther:
		if no := noRoom(c, false, false); no != "" {
			return no
		}
	case CaseNoAnswer, CaseImageWait:
		if no := noRoom(c, true, false); no != "" {
			return no
		}
	case CaseDayCap, CaseMonthCap, CaseProjectCap:
		if no := noRoom(c, false, true); capWaitDecision(reason) != "" && no != "" {
			return "The task resumes on its own as above. " + no
		}
	case CaseGateStop:
		if !strings.HasPrefix(reason, leftQueue) {
			return "Reply on this issue to run the gate again with your comment as guidance, or close the issue to end the task."
		}
	case CaseFindingsRepeated:
		if no := noRoom(c, false, false); no != "" {
			return no
		}
		return "A reply alone changes nothing: the stop repeats until the card's limits or the review's word change. Use the exits above (extend the card, overrule the review, end the card, or close into a successor), or close this issue to end the task."
	case CaseScreeningSuspect:
		return "A human reads the screening first: reply on this issue to let the agent work on it, or close the issue to end the task."
	case CaseHumanCard, CaseJudgmentBudget:
		return "This card is yours: answer with a choice on the card or reply to leave a note. No attempt starts. Close this issue to end it."
	case CaseWaitExpired, CaseMergeApproval, CaseAgentRequestInfo, CasePlanQuestion, CasePlanConfirmation:
		return "This stop is answered on its own form, not by a reply on this issue. Close this issue to end the task."
	case CaseDependencyClosed, CaseDependencyFailed:
		return "Reply on this issue to start the card anyway with your comment as guidance, or close the issue to end the task."
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
func (r *Runner) askHuman(ctx context.Context, t Task, c Case, reason, next string, choices []string, up *handUp) (int64, error) {
	beside := c.Kind == CaseMonitorFlag
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return 0, terminalError("%v", err)
	}
	if !beside {
		// a card that cannot wait for a human (final, merged, closing) has
		// no run to hear an answer: no comment asks one
		cur, ok, err := r.store.Task(ctx, t.ID)
		if err != nil {
			return 0, err
		}
		if ok && !slices.Contains(openStatuses, cur.Status) {
			return 0, errNoWait{cur.Status}
		}
	}
	// the reason in a code block: a mention the agent wrote rings no bell
	safe := r.untrusted(t, reason)
	body := "factoryd: task " + t.ID + " needs a human.\n\n" + fenced(safe)
	if links := sanitize.Links(safe, r.linkHosts(t)); len(links) > 0 {
		// the fence renders no link: the ones the sanitiser keeps follow it
		body += "\n\nLinks:\n- " + strings.Join(links, "\n- ")
	}
	body += "\n\n" + next
	stop := reason
	if reason == healedReason {
		stop, _ = c.Facts["reason"].(string)
	}
	note, err := r.planDraftNote(ctx, t.ID, stop)
	if err != nil {
		return 0, err
	}
	if note != "" {
		body += "\n\n" + note
	}
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
			shaped = r.monitorFace(t, reason)
		} else {
			shaped = ShapedQuestion(reason, r.questionFacts(ctx, t, reason), choices)
		}
		// the refusal the newest session ended on rides with the ask,
		// verbatim: the blocker reaches the human with no agent-side
		// reporting to lose it (card #1345). A prover's refusals name its
		// hold-outs, so its ask is factoryd's words alone.
		if note != "" {
			shaped.Info += "\n\n" + note
		}
		if up != nil {
			if up.Action != "" && up.ActionRung == 2 {
				shaped.Recommendation = r.untrusted(t, r.scrub(fmt.Sprintf("%s: %s (the factory agent)", up.Action, up.Grounds)))
			} else if up.Action != "" {
				shaped.Recommendation = r.untrusted(t, r.scrub(fmt.Sprintf("The check-in recommends %s: %s", up.Action, up.Grounds)))
			}
			if up.Mark != "" {
				shaped.Ask += " (" + up.Mark + ")"
			}
			if up.Found != "" {
				shaped.Info += "\n\n" + r.untrusted(t, r.scrub(foundLine(up)))
				if up.Before != "" {
					shaped.Info += "\n\n" + r.untrusted(t, r.scrub(beforeLine(up.Before)))
				}
			}
			if up.Note != "" {
				shaped.Info += "\n\n" + up.Note
			}
		}
		if t.Source != SourceProver {
			if tool, refusal, ok := r.lastToolRefusal(ctx, t.ID); ok {
				shaped.Info += "\n\nThe session's last tool call (" + tool + ") was refused:\n\n" + fenced(refusal)
			}
		}
		question = shaped.Ask
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
		"reason": textutil.Truncate(r.Scrub.String(reason), 300)}
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
		Detail: map[string]any{"comment": id, "reason": textutil.Truncate(r.Scrub.String(reason), 2000)}}); err != nil {
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
