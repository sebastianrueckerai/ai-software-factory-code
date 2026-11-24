package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"regexp"
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
	// Death is the death record of the session before the stop, nil for
	// none: the case's facts carry it.
	Death *DeathRecord `json:",omitempty"`
	// Model is the model the run's next session opens on, whose opening
	// floor the facts weigh; "" from a run that reads no floor (the plan
	// loop) or from before versionTripModel.
	Model string `json:",omitempty"`
}

// The check-in's spend stops, which a check-in spend case keeps in its
// facts as checkin_stop.
const (
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
// the case: 0 when it resolved it or holds it until the wake it set, 1 when
// rung 1 works on it in the background and the run waits on its
// resolution, 3 when the case waits on the human.
type TripOutcome struct {
	CaseID     int64
	Comment    int64
	Resolution *Resolution
	Rung       int
}

// resolver is one row of rung 0: it fires on a case's kind and facts and
// returns the resolution it took, or nil. Took says what it found and did
// in plain words, for the human whose case it takes (retake). A row that
// holds its case in place of a resolution has Hold instead: it sets the
// case's wake itself and answers held, and its error hands the case up
// with why.
type resolver struct {
	Name string
	Fire func(ctx context.Context, r *Runner, c Case, f CaseFacts) (*Resolution, error)
	Took func(c Case, f CaseFacts, res Resolution) string
	Hold func(ctx context.Context, r *Runner, c Case) (held bool, err error)
}

// headKinds are the stops keyed on the card's head: the caps, the floor, the
// check-in's spend stops and a gate stop.
var headKinds = []CaseKind{CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor, CaseCheckinShare, CaseCheckinFailed, CaseGateStop}

// rung0 is rung 0's table, in the order its rows are tried on a case whose
// kind names them (caseKinds). Act carries a row's resolution out on rung
// 0: it checks the action table again, runs what lives outside the run and
// records the step.
var rung0 = []resolver{
	// an open pull request whose head is green and approved goes to the
	// approval again: no session is asked for
	{Name: "route_approval", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		return rungZeroTakes(f, Resolution{Action: ActRouteApproval}), nil
	}, Took: func(_ Case, f CaseFacts, _ Resolution) string {
		found := "every check on head " + ShortSHA(f.Head) + " is green"
		if f.Kind != CasePolicyClass {
			found += ", its newest review approved it"
		}
		return found + " and it still merges into main, so the factory sent it to the approval"
	}},
	// a red head's checks run again, once per head (RerunOnHead)
	{Name: "rerun_checks", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		return rungZeroTakes(f, Resolution{Action: ActRerunChecks}), nil
	}, Took: func(_ Case, f CaseFacts, _ Resolution) string {
		return "a check on head " + ShortSHA(f.Head) + " failed, so the factory ran the checks again"
	}},
	// a checks wait's stop with every check green: the wait runs again,
	// once per head (WaitedOnHead)
	{Name: "wait_checks", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		if f.Checks != "green" {
			return nil, nil
		}
		return rungZeroTakes(f, Resolution{Action: ActWaitChecks}), nil
	}, Took: func(_ Case, f CaseFacts, _ Resolution) string {
		return "every check on head " + ShortSHA(f.Head) + " is green, so the factory resumed the checks wait"
	}},
	// a day's, a month's or a project's cap: the run's own wait (capWait)
	// ends it once the cap no longer binds a session or its period turns,
	// and the same check (capLift) resumes the card here when it reads so
	// first, its words the check's; a case that names no model has no
	// session to weigh and waits for the run
	{Name: "cap_wait", Fire: func(ctx context.Context, r *Runner, c Case, f CaseFacts) (*Resolution, error) {
		model, _ := c.Facts["model"].(string)
		if model == "" {
			return nil, nil
		}
		var decision string
		for d, k := range capWaitKinds {
			if k == c.Kind {
				decision = d
			}
		}
		lift, err := r.capLift(ctx, CapCheck{TaskID: c.TaskID, Model: model, Decision: decision, Turned: !r.Clock().Before(CapTurn(decision, c.OpenedAt))})
		if err != nil || !lift.Resume {
			return nil, err
		}
		return rungZeroTakes(f, Resolution{Action: ActResume, Note: lift.Why, Moments: lift.Moments}), nil
	}, Took: func(_ Case, _ CaseFacts, res Resolution) string { return res.Note }},
	// a failed dependency outside a plan is filed again once, and a retry
	// that failed in turn goes to the human
	{Name: "retry_dependency", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		d := caseDep(c, f)
		if f.Plan != "" || d == nil || d.RetryOf != "" {
			return nil, nil
		}
		return rungZeroTakes(f, Resolution{Action: ActRetry, Args: map[string]any{"task_id": d.TaskID}}), nil
	}, Took: func(c Case, f CaseFacts, _ Resolution) string {
		return fmt.Sprintf("dependency #%d failed, so the factory filed it again", caseDep(c, f).Issue)
	}},
	// inside a plan, the failed dependency's work goes to the plan's planner
	{Name: "impasse", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		d := caseDep(c, f)
		if f.Plan == "" || d == nil {
			return nil, nil
		}
		return rungZeroTakes(f, Resolution{Action: ActReplan, Args: map[string]any{"task_id": d.TaskID}}), nil
	}, Took: func(c Case, f CaseFacts, _ Resolution) string {
		return fmt.Sprintf("dependency #%d failed, so the factory gave its work to the plan's planner", caseDep(c, f).Issue)
	}},
	// the monitor's issue_open_after_end, which the monitor raises only
	// where Finish closes the issue: rung 0 closes it on the ended card, as
	// the heal closes it
	{Name: "close_ended", Fire: func(_ context.Context, _ *Runner, c Case, f CaseFacts) (*Resolution, error) {
		if c.Facts["flag"] != "issue_open_after_end" || f.Final == "" {
			return nil, nil
		}
		return rungZeroTakes(f, Resolution{Action: ActClose}), nil
	}, Took: func(Case, CaseFacts, Resolution) string { return "the card ended, so the factory closed its issue" }},
	// the monitor's wait_over_bound on a rung that did not answer: the
	// card's steps past their bound end as ExpireSteps ends one, and that
	// ends the case, with no face: the steps' hand-up is the record
	{Name: "expire_step", Fire: func(ctx context.Context, r *Runner, c Case, _ CaseFacts) (*Resolution, error) {
		if c.Facts["flag"] != "wait_over_bound" || c.Facts["wait"] != "rung_answer" {
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
	}, Took: func(_ Case, _ CaseFacts, res Resolution) string { return res.Note }},
	// a plan's criteria case whose gate last judged another head than
	// main's: the gate runs once more on main, and the case waits for it
	{Name: "rerun_criteria_gate", Hold: func(ctx context.Context, r *Runner, c Case) (bool, error) {
		return r.rerunCriteriaGate(ctx, c)
	}},
	// a stop that would ask a human what the chat's open ask about the
	// card or its plan asks already: the card waits on that ask, and goes
	// on with its answer
	{Name: "wait_on_ask", Hold: func(ctx context.Context, r *Runner, c Case) (bool, error) {
		return r.waitOnAsk(ctx, c)
	}},
}

// expireCardSteps ends card taskID's in-progress steps past their due as
// ExpireSteps ends them, handed up with its words, and names each: the
// rung that did not answer and its bound, or the choice it took since,
// after which the card still waits. A step its rung ended meanwhile keeps
// its own end.
func (r *Runner) expireCardSteps(ctx context.Context, taskID string) ([]string, error) {
	expired := []CaseStep{}
	cases, err := r.store.OpenCasesOf(ctx, taskID)
	if err != nil {
		return nil, err
	}
	now := r.Clock()
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
			took := tookSince(steps, st)
			found := stepExpired(st.Found, st.Rung, took, st.StartedAt, *st.DueAt)
			err := r.store.EndCaseStep(ctx, CaseStep{ID: st.ID, Outcome: StepHandedUp, Found: found, Did: st.Did, Decision: st.Decision, Grounds: st.Grounds, CostEUR: st.CostEUR})
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			st.Found = found
			expired = append(expired, st)
			words := fmt.Sprintf("%s did not answer within %s", who(st.Rung), DurationText(st.DueAt.Sub(st.StartedAt).Round(time.Minute)))
			if took != "" {
				words = expiredWords(st.Rung, took, st.DueAt.Sub(st.StartedAt))
			}
			ended = append(ended, words)
		}
	}
	return ended, r.ClimbExpired(ctx, expired)
}

// rungZeroTakes is res when the action table lets rung 0 take res on facts
// f, else nil.
func rungZeroTakes(f CaseFacts, res Resolution) *Resolution {
	a, ok := ActionByName(res.Action)
	if !ok || a.refusal(f, 0, res) != "" {
		return nil
	}
	return &res
}

// tries says row is tried on case c: the kind table names it for c's kind.
func (row resolver) tries(c Case) bool { return slices.Contains(kindRow(c.Kind).Rung0, row.Name) }

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
// climbs it when it has no face yet, and with none opens the stop the card
// asked last as its own case again, which climbs (healedTrip). An attempt
// after the first takes what the attempts before left (retriedTrip).
func (r *Runner) OpenTrip(ctx context.Context, in TripInput, at tripAttempt) (TripOutcome, error) {
	t, _, err := r.task(ctx, in.TaskID)
	if err != nil {
		return TripOutcome{}, err
	}
	if in.Reason == HealedReason && in.Kind == "" {
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
	f.Death = in.Death
	c, cases, err := r.tripCase(ctx, t, f, in)
	if err != nil {
		return TripOutcome{}, err
	}
	r.atCaseOf(ctx, &f, t, c)
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
	if err != nil || res != nil || found == "" {
		// found "": a row holds the case at rung 0 until the wake it set
		return TripOutcome{CaseID: c.ID, Resolution: res}, err
	}
	return r.Climb(ctx, t, c, f, 0, found)
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

// Climb is the one way a case goes from rung from to the next: from rung 0,
// after rung 0 found found and handed the case up, to rung 1, which works
// on it in the background, unless a human alone decides it, the project's
// rungs do not judge at its autonomy, the case is one rung 1 may not
// answer, no check-in model is set, or rung 1 judged the stop before (a
// moment's own human answer is its step); from rung 1, or after any of
// those, to rung 2, the factory agent's wake turn, at every autonomy
// level, unless the case is the human's alone, the judgment budget is
// spent or unreadable or rung 2 worked the case already; from rung 2, or
// after any of those, to the human, with the recommendation the rung
// before made, and a case no rung judged with the agent's view saying why.
// The card keeps its status while a rung works: only the face moves it. A
// case at the human climbs nowhere.
func (r *Runner) Climb(ctx context.Context, t Task, c Case, f CaseFacts, from int, found string) (TripOutcome, error) {
	var unjudged *HandUp
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
					if unjudged, err = r.CountReached(ctx, c, limit); err != nil {
						return TripOutcome{}, err
					}
				} else if rung, err := r.rungWorks(ctx, c); err != nil || rung == 1 {
					return TripOutcome{CaseID: c.ID, Rung: 1}, err
				}
			}
		}
		if kindRow(c.Kind).NoRung1 == checkinJudged {
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
		started, spent, err := r.startRung2(ctx, t, live)
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
	up := HandedUpBy(steps, from, found)
	if unjudged == nil {
		if unjudged, up, err = r.refusedUp(ctx, live, steps, up); err != nil {
			return TripOutcome{}, err
		}
	}
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
	out, err := r.Face(ctx, t, live, reason, tripNext(live, reason), choices, up)
	if err == nil && out.Comment != 0 && runHears(t, live) && !caseBesideRun(live, t) {
		// the trip may have returned without a comment (rung 1 held it):
		// the run learns this face's. A run that has the comment ignores it.
		// off the caller's path: the bounded retries may take long
		r.rungs.Add(1)
		go func() {
			defer r.rungs.Done()
			r.signalFaced(context.WithoutCancel(ctx), t.ID, live, out.Comment)
		}()
	}
	return out, err
}

// spentJudgment is the hand-up of a case that goes to the human with no
// rung 1 conversation because the judgment budget is spent: the one money
// case is filed, the case says so, and a step of rung 0 says why. Nil when
// rung 1 may take the case: money is left, or rung 1 judged it already.
func (r *Runner) spentJudgment(ctx context.Context, t Task, c Case) (*HandUp, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 1 }) {
		return nil, err
	}
	if _, _, ok := r.cfg.Policy().ProjectOf(t.Repository); !ok {
		return r.unreadJudgment(ctx, t, c, steps)
	}
	return r.judgmentSpent(ctx, t, c, r.callModel(t, policy.PurposeCheckin))
}

// unreadJudgment is the hand-up of case c of card t, whose repository names
// no project, so no judgment budget can be read and no rung judges it.
func (r *Runner) unreadJudgment(ctx context.Context, t Task, c Case, steps []CaseStep) (*HandUp, error) {
	found := r.scrub(fmt.Sprintf("%s: repository %s names no project, so its judgment budget cannot be read", markJudgmentUnread, t.Repository))
	seen := r.scrub(fmt.Sprintf("The factory agent did not judge it: %s names no project, so the factory cannot read its judgment budget.", t.Repository))
	return r.notJudged(ctx, c, steps, markJudgmentUnread, found, seen)
}

// notJudged is the hand-up of case c, which no rung judged, with mark,
// which says why: a step of rung 0 found found, and the agent's view on
// the face says seen.
func (r *Runner) notJudged(ctx context.Context, c Case, steps []CaseStep, mark, found, seen string) (*HandUp, error) {
	return &HandUp{Found: seen, Mark: mark}, r.stepOnce(ctx, c, steps, mark, found)
}

// refusedUp is the hand-up of case c, whose rung steps are among steps,
// after the model provider refused a rung's calls: with every rung 1 and
// rung 2 step refused, no rung judged it and the case is marked (unjudged);
// with a rung that judged beside a refused one, the face is the judging
// rung's work with a line for the refused rung. up, the hand-up the steps
// make, stays with no refused step or none that judged.
func (r *Runner) refusedUp(ctx context.Context, c Case, steps []CaseStep, up *HandUp) (unjudged, face *HandUp, err error) {
	var refused []CaseStep
	kept := slices.DeleteFunc(slices.Clone(steps), func(s CaseStep) bool {
		if (s.Rung == 1 || s.Rung == 2) && strings.HasPrefix(s.Found, markProviderRefused) {
			refused = append(refused, s)
			return true
		}
		return false
	})
	switch {
	case len(refused) == 0:
		return nil, up, nil
	case !slices.ContainsFunc(kept, func(s CaseStep) bool { return s.Rung == 1 || s.Rung == 2 }):
		last := refused[len(refused)-1]
		// the provider's own words stay in the steps, which the card's life shows
		seen := rungName(last.Rung) + " did not judge it: the model provider refused. The provider's words are in the card's life."
		unjudged, err = r.notJudged(ctx, c, steps, markProviderRefused, last.Found, seen)
		return unjudged, up, err
	case !slices.ContainsFunc(kept, func(s CaseStep) bool {
		return (s.Rung == 1 || s.Rung == 2) && s.Outcome == StepHandedUp && !strings.Contains(s.Found, expiredMark)
	}):
		return nil, up, nil
	}
	face = HandedUpBy(kept, 0, "")
	face.Note = rungName(refused[len(refused)-1].Rung) + " did not judge it: the model provider refused."
	return nil, face, nil
}

// rungName is how a face names the rung that works a case below the human.
func rungName(rung int) string {
	if rung == 2 {
		return "The factory agent"
	}
	return "The check-in"
}

// noJudgmentLine is the face's line for a case a rung judged but the rung
// above could not, for want of money.
const noJudgmentLine = "The factory agent did not judge it: the judgment budget is spent."

// judgmentSpent is spentJudgment's path in full, for the rung whose calls run
// on model: nil when the chat task holds the money one call needs. A case no
// rung judged is marked; one a rung judged is not, and the hand-up carries
// noJudgmentLine instead.
func (r *Runner) judgmentSpent(ctx context.Context, t Task, c Case, model string) (*HandUp, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	chat, err := r.JudgmentTask(ctx, t)
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
	r.FileJudgmentCard(ctx, t, chat)
	if slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 1 && s.Outcome == StepHandedUp && s.Decision != "" }) {
		stepFound := fmt.Sprintf("%s The chat task %s has %s of %s EUR left.", noJudgmentLine, chat.ID, textutil.EUR(left), textutil.EUR(chat.BudgetEUR))
		return &HandUp{Note: noJudgmentLine}, r.stepOnce(ctx, c, steps, noJudgmentLine, stepFound)
	}
	found := fmt.Sprintf("%s: the chat task %s has %s of %s EUR left", markJudgmentSpent, chat.ID, textutil.EUR(left), textutil.EUR(chat.BudgetEUR))
	seen := fmt.Sprintf("%s The project's judgment budget has %s of %s EUR left this month.", noJudgmentLine, textutil.EUR(left), textutil.EUR(chat.BudgetEUR))
	return r.notJudged(ctx, c, steps, markJudgmentSpent, found, seen)
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

// FileJudgmentCard files the judgment budget's money card. A failure does
// not hold the case from its human: the chat's stop at this budget is
// recorded, which the card loop files from on its own.
func (r *Runner) FileJudgmentCard(ctx context.Context, t Task, chat Task) {
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
	now := r.Clock()
	spent7, err := r.store.TaskSpendSince(ctx, chat.ID, now.Add(-7*24*time.Hour))
	if err != nil {
		log.Printf("task %s: read the chat spend: %s", t.ID, r.scrub(err.Error()))
	}
	if _, err := r.store.Record(ctx, Event{Repository: t.Repository, TaskID: chat.ID, Kind: EventChatBudgetSpent, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"budget_eur": chat.BudgetEUR, "spent_eur": chat.EnforcedSpent(), "suggest_eur": SuggestFund(spent7, now)}}); err != nil {
		log.Printf("task %s: record the judgment budget's stop: %s", t.ID, r.scrub(err.Error()))
	}
}

// CountReached is the hand-up of a case rung 1 may not take because its
// card has had the per-card count of rung 1 runs.
func (r *Runner) CountReached(ctx context.Context, c Case, limit int) (*HandUp, error) {
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	found := fmt.Sprintf("rung 1 reached its per-card count of %d on this card", limit)
	seen := fmt.Sprintf("The check-in did not look at this ask: it already ran %d times on this card, the most it may.", limit)
	return &HandUp{Found: seen, Mark: found}, r.stepOnce(ctx, c, steps, found, found)
}

const (
	markJudgmentSpent   = "not judged, judgment budget spent"
	markJudgmentUnread  = "not judged, judgment budget unreadable"
	markProviderRefused = "not judged: the model provider refused"
)

// FacedTries and facedBackoff bound the retries of a case_faced signal.
const FacedTries = 4

var FacedBackoff = time.Second

// signalFaced tells card taskID's run the comment of case c's face: a
// failed send is tried again, with a doubling pause, up to facedTries times
// or until ctx ends, and one that delivers ends the signal's pending row,
// which the heal pass sends again until one does.
func (r *Runner) signalFaced(ctx context.Context, taskID string, c Case, comment int64) {
	sig := facedSignal(c, comment)
	pause := FacedBackoff
	for try := 1; ; try++ {
		err := r.cfg.Workflows.Signal(ctx, taskID, SignalCaseFaced, sig)
		if err == nil {
			r.settleSignal(ctx, c.ID, SignalCaseFaced)
			return
		}
		log.Printf("task %s: signal %s of case %d (try %d): %s", taskID, SignalCaseFaced, c.ID, try, r.scrub(err.Error()))
		if try == FacedTries {
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

// ChoiceKind reports whether a case of kind k is answered with a value of
// its question's choices: the kind table's choice column.
func ChoiceKind(k CaseKind) bool { return kindRow(k).Choices }

// boardFlags are the monitor flags about the board, not one card's work:
// they skip rung 1 and go to rung 2.
var boardFlags = []string{"queued_over_bound"}

// boardFlag says case c is a monitor flag about the board.
func boardFlag(c Case) bool {
	flag, _ := c.Facts["flag"].(string)
	return c.Kind == CaseMonitorFlag && slices.Contains(boardFlags, flag)
}

// rungTwoTakes reports whether rung 2 takes case c: at every autonomy
// level, where below autonomous its answer is a proposal the human
// confirms (Proposes), every kind the kind table gives rung 2 a part in. A
// case only a human decides is taken for its analysis alone.
func rungTwoTakes(c Case) bool {
	return kindRow(c.Kind).Rung2 != rung2Nothing
}

// startRung2 hands case c to rung 2 as a step in progress due after the
// rung's bound, the case's wake on rung 2, and a case_climbed row the Waker
// wakes the factory agent on. It starts nothing, started false, for a case
// rung 2 does not take or one it worked already; a judgment budget that is
// spent, or unreadable on a repository that names no project, starts
// nothing either and answers the case's unjudged hand-up (spent).
func (r *Runner) startRung2(ctx context.Context, t Task, c Case) (started bool, spent *HandUp, err error) {
	if !rungTwoTakes(c) {
		return false, nil, nil
	}
	lock, _ := r.caseLocks.LoadOrStore(c.ID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil || slices.ContainsFunc(steps, func(s CaseStep) bool { return s.Rung == 2 }) {
		return false, nil, err
	}
	if _, _, ok := r.cfg.Policy().ProjectOf(t.Repository); !ok {
		spent, err = r.unreadJudgment(ctx, t, c, steps)
		return false, spent, err
	}
	if spent, err = r.judgmentSpent(ctx, t, c, policy.Big); err != nil || spent != nil {
		return false, spent, err
	}
	due := r.Clock().Add(r.cfg.Policy().WaitsOf(t.Repository).RungAnswer)
	if _, err := r.store.StartRungStep(ctx, CaseStep{CaseID: c.ID, Rung: 2, Actor: ChatUser, Outcome: StepInProgress, DueAt: &due}); err != nil {
		return false, nil, err
	}
	r.timeline.RecordRow(ctx, t.ID, EventCaseClimbed, FactoryDecider, ViaFactoryd, map[string]any{
		"case_id": c.ID, "rung": 2, "kind": string(c.Kind), "problem": textutil.Truncate(r.scrub(c.Problem), 300)})
	return true, nil, nil
}

// facedSignal is the case_faced that tells a run the comment of case c's
// face.
func facedSignal(c Case, comment int64) TaskSignal {
	return TaskSignal{Author: FactoryDecider, Comment: comment, Case: &CaseSignal{CaseID: c.ID, Rung: 3, By: FactoryDecider, Comment: comment, Again: asksAgain(c)}}
}

// asksAgain is the case whose question case c asks again (its fact
// asks_again), 0 for none.
func asksAgain(c Case) int64 {
	id, _ := toInt(c.Facts["asks_again"])
	return int64(id)
}

// rungOneJudges reports whether rung 1 takes the case c, on facts f.
func (r *Runner) rungOneJudges(t Task, c Case, f CaseFacts) bool {
	if human, _ := humanOnly(c, f); human || kindRow(c.Kind).NoRung1 != "" || boardFlag(c) {
		return false
	}
	return r.cfg.Policy().EscalationOf(t.Repository).Judges(f.Autonomy) && r.Checkin != nil && r.callModel(t, policy.PurposeCheckin) != ""
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
		Decision: RungHandUp, Grounds: textutil.Truncate(r.scrub(reason), 1200)})
	return err
}

// faceOf is the reason and the choices a case's face asks, as its trip
// stored them.
func faceOf(c Case) (reason string, choices []string) {
	for _, k := range []string{"face", "reason"} {
		if s, _ := c.Facts[k].(string); strings.TrimSpace(s) != "" {
			reason = s
			break
		}
	}
	if reason == "" {
		reason = c.Problem
	}
	if v, ok := c.Facts["choices"].([]string); ok {
		return reason, v
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

// HandUp is what the rung before the human made of the case: the action it
// recommends with its grounds, and what it found.
type HandUp struct {
	Action, Grounds, Found string
	// Rung is the rung that handed the case up, ActionRung the rung whose
	// recommendation Action is
	Rung, ActionRung int
	// Before is what the rung below an expired one found, for the face to
	// carry beside the expiry
	Before string
	// Note is a line the face adds, for a rung that could not judge the case
	Note string
	// Mark, when set, says why no rung judged the case: the agent's view
	// names what the factory found
	Mark string
	// Preselected is the action rung 2 proposed, with its arguments and
	// fund: the face offers it preselected
	Preselected *CaseAction
}

// recommendation is the face's recommendation line for hand-up up, masked
// and fenced as agent text: "" when the rung recommended no action or gave
// no grounds. recommendedBy names its writer.
func (r *Runner) recommendation(t Task, up *HandUp) string {
	return up.recommendation(func(s string) string { return r.untrusted(t, s) }, r.scrub)
}

// recommendation is up's recommendation line: its grounds, scrubbed and
// fenced, "" when the rung recommended no action or gave no grounds. The
// face names the action by its answer's label.
func (up *HandUp) recommendation(fence, scrub func(string) string) string {
	if up.Action == "" || strings.TrimSpace(up.Grounds) == "" {
		return ""
	}
	return fence(scrub(up.Grounds))
}

// recommendedBy is who wrote the recommendation of hand-up up.
func recommendedBy(up *HandUp) string {
	if up.ActionRung == 2 {
		return "the factory agent"
	}
	return "the check-in"
}

// preselected is the action as the question_asked row carries it for the
// face to preselect.
func (a CaseAction) preselected() map[string]any {
	return map[string]any{"action": a.Action, "args": a.Args, "room": a.Room}
}

// storedDecision is a case step's decision as the run reads it back: the
// migration moved the stored values onto the rungs' names, and a row the
// store wrote before the change under the old name human reads through
// the same reader as one written hand_up.
func storedDecision(d string) string {
	if d == "human" {
		return RungHandUp
	}
	return d
}

// HandedUpBy is the hand-up of the rungs from rung from up (from rung 1 for
// a case that climbs from rung 0 after rung 1 judged it already): the
// newest step of one that ended handed up, with found as it climbed; nil for
// a case no rung above 0 handed up.
func HandedUpBy(steps []CaseStep, from int, found string) *HandUp {
	up := &HandUp{Found: found, Rung: from}
	// a moment's own hand-up answers the case: the first checkin step that
	// stored the hand-up (under the rungs' name, or the old name human the
	// migration moved) is the hand-up — its grounds the found, no action of
	// its own — outranking the action rows around it
	for k := 0; k < len(steps); k++ {
		s := &steps[k]
		if s.Outcome != StepHandedUp || s.Rung < max(from, 1) || s.Rung >= 3 || storedDecision(s.Decision) != RungHandUp {
			continue
		}
		up.Rung = s.Rung
		up.Found = cmp.Or(s.Found, s.Grounds)
		return up
	}
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
	if storedDecision(first.Decision) == RungHandUp {
		// a moment's own human answer: its reason is what it found
		up.Found = cmp.Or(first.Found, first.Grounds)
		return up
	}
	// a rung that gave no recommendation (one that did not answer) leaves the
	// newest one the rungs below it made, and what it found
	if first.Found == RungTwoStopped || strings.Contains(first.Found, expiredMark) {
		for k := len(steps) - 1; k >= 0; k-- {
			s := steps[k]
			if s.Outcome != StepHandedUp || s.Rung != 1 {
				continue
			}
			if s.Decision == RungHandUp {
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
		if s.Outcome == StepHandedUp && s.Rung >= 1 && s.Rung < 3 && storedDecision(s.Decision) != RungHandUp && s.Decision != "" {
			up.Action, up.Grounds, up.ActionRung = s.Decision, s.Grounds, s.Rung
			if i := slices.IndexFunc(s.Did, func(a CaseAction) bool { return a.Result == resultProposed || a.Result == resultRecommended }); i >= 0 {
				up.Preselected = &s.Did[i]
			}
			break
		}
	}
	return up
}

// expiredMark is what ExpireSteps writes into the Found of a step whose rung
// did not answer.
const expiredMark = "the rung did not answer within "

// RungTwoStopped is the Found of rung 2's step when the factory agent's
// turn stopped on its refused actions with the case unanswered.
const RungTwoStopped = "The factory agent did not answer: the factory refused every action it tried."

// foundLine is what the rung before the human found, as the face reads it:
// an expired step and a stopped turn say that the rung did not answer, no
// finding.
func foundLine(up *HandUp) string {
	if up.Rung == 2 && up.Found == RungTwoStopped {
		return RungTwoStopped
	}
	if _, bound, ok := strings.Cut(up.Found, expiredMark); ok && up.Rung == 2 {
		return "The factory agent did not answer within " + bound + "."
	}
	if waits := stillWaits(up.Found); waits != "" && up.Rung == 2 {
		return "The factory agent " + waits
	}
	switch {
	case up.Mark != "":
		return up.Found
	case up.Rung == 2:
		return "The factory agent found:\n\n" + Quoted(up.Found)
	}
	return "The check-in found:\n\n" + Quoted(up.Found)
}

// tookAndWaits is what expiredWords writes for a rung whose choice kept the
// card waiting.
var tookAndWaits = regexp.MustCompile(`rung \d took (\S+) and the card still waits after (\S+)$`)

// stillWaits says, of a step that found found, that its rung took a choice
// and the card still waits, the choice by its label; "" for any other step.
func stillWaits(found string) string {
	m := tookAndWaits.FindStringSubmatch(found)
	if m == nil {
		return ""
	}
	return "took “" + cmp.Or(LabelOf(m[1]), strings.ReplaceAll(m[1], "_", " ")) + "”, and the card still waits after " + m[2] + "."
}

// Quoted is a writer's text as a quote in markup, so a reader tells it
// from the factory's words.
func Quoted(s string) string {
	return "> " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n> ")
}

// StepWords is case step s as the card's life names it: who took it and
// what came of it, and the words it found, which are its writer's; an
// expired step says its decider did not answer.
func StepWords(s CaseStep) (lead, found string) {
	if s.Found == RungTwoStopped {
		return RungTwoStopped, ""
	}
	who := cmp.Or(deciderLead(s.Actor), "A human")
	if _, bound, ok := strings.Cut(s.Found, expiredMark); ok {
		return who + " did not answer within " + bound + ".", ""
	}
	if waits := stillWaits(s.Found); waits != "" {
		return who + " " + waits, ""
	}
	next := []string{"the check-in", "the factory agent", "a human"}[min(max(s.Rung, 0), 2)]
	did := map[string]string{StepInProgress: "is weighing it", StepResolved: "answered it", StepHandedUp: "passed it to " + next,
		StepWithdrawn: "stopped when the ask was withdrawn"}[s.Outcome]
	lead = who + " " + cmp.Or(did, "took a step")
	if s.Found = strings.TrimSpace(s.Found); s.Found != "" {
		return lead + ", and found: ", s.Found
	}
	return lead + ".", ""
}

// beforeLine is what rung 1 found under rung 2's expiry: its finding, or that
// it too did not answer.
func beforeLine(found string) string {
	if _, bound, ok := strings.Cut(found, expiredMark); ok {
		return "The check-in did not answer within " + bound + "."
	}
	return "The check-in found:\n\n" + Quoted(found)
}

// errNoWait refuses a face of a card in status, which cannot wait for a
// human.
type errNoWait struct{ status string }

func (e errNoWait) Error() string {
	return "the card is " + e.status + ", so no run waits on a human's answer"
}

// Face is the human's Face of case c: the question asked (askHuman), the
// case at the human through the door (ToHuman) and its next move the
// human's. The door is asked first, on the case as it stands now, so a case
// it refuses gets no comment. A card that cannot wait for a human gets no
// Face, and the case ends.
func (r *Runner) Face(ctx context.Context, t Task, c Case, reason, next string, choices []string, up *HandUp) (TripOutcome, error) {
	live, err := r.store.CaseByID(ctx, c.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	steps, err := r.store.CaseSteps(ctx, c.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	if err := doorRefusal(live, climbed(steps)); err != nil {
		return TripOutcome{}, err
	}
	comment, err := r.AskHuman(ctx, t, c, reason, next, choices, up)
	if no := (errNoWait{}); errors.As(err, &no) {
		return TripOutcome{CaseID: c.ID, Rung: 3}, caseEnded(r.store.WithdrawCase(ctx, c.ID, no.Error()))
	}
	if err != nil {
		return TripOutcome{}, err
	}
	if err := r.store.ToHuman(ctx, c.ID); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: comment, Rung: 3}, nil
}

// ClimbExpired climbs the case of each step that ended expired, from the
// step's rung, when its case is still open: the rung did not answer.
func (r *Runner) ClimbExpired(ctx context.Context, steps []CaseStep) error {
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
		if !slices.Contains(openStatuses, t.Status) && !outlivesCard(c, t) {
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
		r.atCaseOf(ctx, &f, t, c)
		if _, err := r.Climb(ctx, t, c, f, st.Rung, st.Found); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// rungZero runs rung 0 on fresh case c of card t: first a row that holds
// its case, which reads what it needs itself; then the first row that
// fires (firing), which Act carries out on rung 0. It answers the
// resolution taken, or nil and what rung 0 found, "" for a case a row
// holds: a row Act refused or failed, or a hold that failed, hands the
// case up with why, so rung 0 never keeps the human from being asked.
func (r *Runner) rungZero(ctx context.Context, t Task, c Case, f CaseFacts, cases []Case) (*Resolution, string, error) {
	for _, row := range rung0 {
		if row.Hold == nil || !row.tries(c) {
			continue
		}
		held, err := row.Hold(ctx, r, c)
		switch {
		case ctx.Err() != nil:
			return nil, "", ctx.Err()
		case err != nil:
			return nil, row.Name + " failed: " + r.scrub(err.Error()), nil
		case held:
			return nil, "", nil
		}
	}
	row, res, found, err := r.firing(ctx, c, f, cases)
	if err != nil || res == nil {
		return nil, found, err
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

// firing is the first row of rung0 tried on case c that fires on facts f,
// with its resolution: none on facts read partly, which could hide what a
// row needs (a head a rerun made), and none when rung 0 resolved a case of
// c's kind and key before, for the stop came back. With none it answers
// what rung 0 found.
func (r *Runner) firing(ctx context.Context, c Case, f CaseFacts, cases []Case) (resolver, *Resolution, string, error) {
	// an unread hold refuses the answers it guards itself (holdRefusal)
	if partial := slices.DeleteFunc(slices.Clone(f.Partial), func(n string) bool {
		return slices.Contains([]string{"siblings", "head_commits", "checks_log", partialHold, partialPlanHold}, n)
	}); len(partial) > 0 {
		return resolver{}, nil, "facts read partly: " + textutil.AndList(partial) + ", so rung 0 took nothing", nil
	}
	if i := slices.IndexFunc(cases, func(e Case) bool {
		return e.ID != c.ID && e.Kind == c.Kind && e.StateKey == c.StateKey && e.Status == CaseResolved && e.Resolution != nil && e.Resolution.Rung == 0
	}); i >= 0 {
		return resolver{}, nil, "rung 0 took this stop with " + cases[i].Resolution.Action + " before, and it came back", nil
	}
	for _, row := range rung0 {
		if row.Fire == nil || !row.tries(c) {
			continue
		}
		if res, err := row.Fire(ctx, r, c, f); err != nil || res != nil {
			return row, res, "", err
		}
	}
	return resolver{}, nil, "no resolver fits", nil
}

// firesOn says rung 0 tries a row that fires on a case of kind k.
func firesOn(k CaseKind) bool {
	return slices.ContainsFunc(rung0, func(row resolver) bool { return row.Fire != nil && row.tries(Case{Kind: k}) })
}

// retake runs rung 0 again on case c at the human, on the facts Act reads:
// the first row that fires (firing) takes the case from the human through
// Act on rung 0, its note the row's words (Took). Nothing that fires, and
// an answer Act refuses or a human's that came first, write nothing.
func (r *Runner) retake(ctx context.Context, c Case) error {
	t, _, err := r.task(ctx, c.TaskID)
	if err != nil {
		return err
	}
	f, err := r.FactsAtCase(ctx, t, c)
	if err != nil {
		return err
	}
	cases, err := r.store.CasesOf(ctx, t.ID)
	if err != nil {
		return err
	}
	row, res, _, err := r.firing(ctx, c, f, cases)
	if err != nil || res == nil {
		return err
	}
	res.Note = row.Took(c, f, *res)
	_, err = r.Act(ctx, ActInput{TaskID: t.ID, CaseID: c.ID, By: FactoryDecider, Via: ViaFactoryd, Rung: 0, Res: *res})
	var refused ErrActRefused
	var noRetry ErrNoRetry
	var ended ErrCaseEnded
	if errors.As(err, &refused) || errors.As(err, &noRetry) || errors.As(err, &ended) {
		return nil
	}
	return err
}

// askedAgain says reason asks a card's standing question again: a run's
// restart (HealedReason) or the heal pass's find of a wait with no live
// ask (AsklessReason).
func askedAgain(reason string) bool { return reason == HealedReason || reason == AsklessReason }

// askedAgainFound is the step a case takes when reason, one askedAgain
// matches, asks it again.
func askedAgainFound(reason string) string {
	if reason == HealedReason {
		return "asked again after a restart"
	}
	return "asked again: " + reason
}

// healedTrip is the trip, of id trip, that asks card t's standing question
// again for in's reason (askedAgain): the card's newest live ask
// (NewestAskCase) at the human waits on with a step and no new face. One
// with no face yet climbs on the facts it was opened with. With none, the
// stop the card asked last (restartedStop) opens its own case again, as
// its trip opened it, and the case names the case whose question it asks
// again (asks_again): that ask's, else the card's newest ask case. Either
// way the case takes the reason's step of rung 0, a trip under the gate's
// mark (runGrants), and climbs from rung 0 with no resolver: the stop's
// own rung 0 ran when it was first asked.
func (r *Runner) healedTrip(ctx context.Context, t Task, in TripInput, trip string) (TripOutcome, error) {
	found := askedAgainFound(in.Reason)
	c, err := r.store.NewestAskCase(ctx, t.ID)
	if err != nil {
		return TripOutcome{}, err
	}
	if c.Status == CaseAtHuman {
		return r.waitsAtHuman(ctx, t, c, found)
	}
	head, model := in.Head, in.Model
	if c.ID != 0 {
		head, _ = c.Facts["head"].(string)
		model, _ = c.Facts["model"].(string)
	}
	f, err := r.tripFacts(ctx, t, head, model)
	if err != nil {
		return TripOutcome{}, err
	}
	if c.ID == 0 {
		stop, again, err := r.restartedStop(ctx, t, in.Reason)
		if err != nil {
			return TripOutcome{}, err
		}
		var cases []Case
		if c, cases, err = r.tripCase(ctx, t, f, TripInput{TaskID: t.ID, Reason: stop, Choices: in.Choices, Head: in.Head,
			Extra: map[string]any{"asked_again": in.Reason}}); err != nil {
			return TripOutcome{}, err
		}
		if again == 0 {
			for _, e := range cases {
				if AnsweredOnCard(e.Kind) {
					again = max(again, e.ID)
				}
			}
		}
		if again != 0 {
			c.Facts["asks_again"] = again
		}
		if trip != "" {
			c.Facts["trip"] = trip
		}
		if c, _, err = r.store.OpenCase(ctx, c); err != nil {
			return TripOutcome{}, err
		}
	}
	r.atCaseOf(ctx, &f, t, c)
	if err := r.tripStep(ctx, c.ID, found); err != nil {
		return TripOutcome{}, err
	}
	if out, ok, err := r.waitsOnRung(ctx, c); ok || err != nil {
		return out, err
	}
	return r.Climb(ctx, t, c, f, 0, found)
}

// refreshDraftInfo swaps the draft note have in the info of the question
// the card's newest ask put for note, so the face shows the draft as it is
// now.
func (r *Runner) refreshDraftInfo(ctx context.Context, taskID, have, note string) error {
	asks, err := r.store.AskAnchors(ctx, taskID)
	if err != nil {
		return err
	}
	for i := len(asks) - 1; i >= 0; i-- {
		if asks[i].Comment == 0 || asks[i].Shaped == nil {
			continue
		}
		var paras []string
		if info := asks[i].Shaped.Info; info != "" {
			paras = strings.Split(info, "\n\n")
		}
		if at := slices.Index(paras, have); have != "" && at >= 0 {
			paras = slices.Delete(paras, at, at+1)
			if note != "" {
				paras = slices.Insert(paras, at, note)
			}
		} else if note != "" {
			paras = append(paras, note)
		}
		return r.store.SetQuestionInfo(ctx, taskID, asks[i].Comment, strings.Join(paras, "\n\n"))
	}
	return nil
}

// restartedStop is the reason of the stop card t asked last, which reason
// asks again, and the case that asked it: its newest ask's words and case,
// reason itself and 0 when it has none.
func (r *Runner) restartedStop(ctx context.Context, t Task, reason string) (string, int64, error) {
	asks, err := r.store.AskAnchors(ctx, t.ID)
	if err != nil {
		return "", 0, err
	}
	if i := len(asks) - 1; i >= 0 && strings.TrimSpace(asks[i].Body) != "" {
		return asks[i].Body, asks[i].CaseID, nil
	}
	return reason, 0, nil
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
	if caseBesideRun(c, t) {
		return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, nil
	}
	if err := r.waitOnHuman(ctx, t); err != nil {
		return TripOutcome{}, err
	}
	return TripOutcome{CaseID: c.ID, Comment: c.CommentID, Rung: 3}, nil
}

// tripCase is the case trip in makes on card t with facts f, open until
// its face is written, with the card's cases, for OpenCase to open. The kind is caseKindOf's
// of the reason, or in's own with its key. The problem is the register's
// ask. The facts are f with the reason, its source, what the reason
// names and in's extra facts, the money the card's for a stop on its own
// budget, with what the next opening needs at the floor. The case follows
// the card's newest ended one.
func (r *Runner) tripCase(ctx context.Context, t Task, f CaseFacts, in TripInput) (Case, []Case, error) {
	cases, err := r.store.CasesOf(ctx, t.ID)
	if err != nil {
		return Case{}, nil, err
	}
	c, err := TripCase(t, f, in, r.Scrub.String)
	if err != nil {
		return Case{}, nil, err
	}
	for _, e := range cases {
		if e.Status != CaseOpen && e.Status != CaseAtHuman {
			c.Follows = max(c.Follows, e.ID)
		}
	}
	return c, cases, nil
}

// TripCase is the case trip in opens on card t with facts f, before it
// follows any case: tripCase's, its texts masked by scrub.
func TripCase(t Task, f CaseFacts, in TripInput, scrub func(string) string) (Case, error) {
	source := EscalationSource(in.Reason)
	kind, named := CaseKindOf(source, in.Reason)
	if in.Kind != "" {
		kind, named = in.Kind, nil
	}
	kind = HumanCardKind(kind, t)
	b, err := json.Marshal(f)
	if err != nil {
		return Case{}, err
	}
	var facts map[string]any
	if err := json.Unmarshal(b, &facts); err != nil {
		return Case{}, err
	}
	maps.Copy(facts, named)
	maps.Copy(facts, in.Extra)
	if d := tripDep(kind, f); kind == CaseDependencyClosed && d != nil && d.Why != "" {
		facts["closed_why"] = d.Why
	}
	facts["source"], facts["reason"] = source, textutil.Truncate(scrub(in.Reason), 300)
	if !askedAgain(in.Reason) {
		facts["face"] = textutil.Truncate(scrub(in.Reason), 4000)
	}
	if len(in.Choices) > 0 {
		facts["choices"] = in.Choices
	}
	if in.CheckinStop != "" {
		facts["checkin_stop"] = in.CheckinStop
	}
	c := Case{TaskID: t.ID, Repository: t.Repository, Kind: kind, Status: CaseOpen, Facts: facts, Money: tripMoney(kind, f)}
	c.Problem = ownAsk(c, t)
	c.StateKey = tripKey(kind, in.Reason, f.Head, f)
	if in.Kind != "" {
		c.StateKey = in.StateKey
	}
	return c, nil
}

// tripKey is the state key of a trip's case of kind at reason: head for a
// stop at the head, the dependency's task id for a dependency's, else the
// first line of the stop's reason (caseStateKey), never the face's words,
// so a change of the register's copy opens no second case.
func tripKey(kind CaseKind, reason, head string, f CaseFacts) string {
	switch {
	case slices.Contains(headKinds, kind) && head != "":
		return head
	case kind == CaseDependencyFailed || kind == CaseDependencyClosed:
		if d := tripDep(kind, f); d != nil {
			return d.TaskID
		}
	}
	return caseStateKey(kind, EscalationQuestion(reason), "")
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
// the card's budget and what is spent, and at a budget floor what one
// more session buys. The face reads the amount it names from the live
// facts (shortfall).
func tripMoney(kind CaseKind, f CaseFacts) *CaseMoney {
	switch {
	case kind == CaseBudgetFloor:
		buys := "one session"
		if f.Model != "" {
			buys += " on " + f.Model
		}
		return &CaseMoney{Kind: "budget", BudgetEUR: f.BudgetEUR, SpentEUR: f.SpentEUR, Buys: buys}
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
// there): a reply starts none, the task waits on a cap that binds first,
// or the line names the stop asked next and the answers c's face holds
// (boardMove). With a limit unread a reply may start none. "" when a
// session fits.
func noRoom(c Case, refund, waiting bool) string {
	f := factsOf(c)
	var lead, asked string
	switch kind, read := f.noSession(refund, waiting); {
	case !read:
		return "The card's limits could not be read, so a reply alone may start no attempt. " + boardMove(c) + " Close this issue to end the task."
	case kind == CaseDayCap, kind == CaseMonthCap, kind == CaseProjectCap:
		// the cap wait's own question line: when it lifts, what raises it
		line := strings.ToUpper(f.CapWait[:1]) + f.CapWait[1:] + " A reply alone starts no attempt while the cap holds."
		if f.unfunded() {
			line += " Once it lifts, the task asks the budget floor next."
		}
		return line
	case kind == CaseBudgetFloor:
		lead, asked = "The card's budget no longer funds a session.", "budget floor"
	case kind == CaseSessionCap:
		lead, asked = "The card's sessions are used up.", "session cap"
	case kind == CaseAgentTimeCap:
		lead, asked = "The card's agent time is spent.", "agent-time cap"
	default:
		return ""
	}
	return lead + " A reply alone starts no attempt: the task asks the " + asked + " next. " + boardMove(c) + " Close this issue to end the task."
}

// boardMove is the issue's sentence of how case c is answered: every
// answer its face holds, by label, and the project chat.
func boardMove(c Case) string {
	if press := boardAnswers(c); press != "" {
		return "Press " + press + " on the board, or tell the project chat how the card goes on."
	}
	return "Tell the project chat how the card goes on."
}

// boardAnswers names, for the issue, the answers the board shows on case
// c's face whose action is one of actions, every answer when actions
// names none: each by its label, with the amount its money field asks. ""
// when the face shows none of them. No issue line names an answer by hand.
func boardAnswers(c Case, actions ...string) string {
	f := Facts{CaseFacts: FactsOf(c)}
	face := FaceOf(c, f)
	_, f = facing(c, f)
	var names []string
	for _, a := range face.Answers {
		if len(actions) > 0 && !slices.Contains(actions, a.Action) {
			continue
		}
		name := "“" + a.Label + "”"
		for _, field := range a.Fields {
			if field.Kind != FieldMoney || field.Default == nil {
				continue
			}
			if eur := field.Default(f); eur != "" && eur != textutil.EUR(0) {
				name += " (" + field.Label + ": " + eur + ")"
			}
		}
		names = append(names, name)
	}
	return orJoin(names)
}

// orJoin is names as one list: "a, b or c".
func orJoin(names []string) string {
	if n := len(names); n > 1 {
		return strings.Join(names[:n-1], ", ") + " or " + names[n-1]
	}
	return strings.Join(names, "")
}

// flagNext is the issue's line of monitor flag c: its card goes on beside
// it, or has ended, and offers, the answers on its face, by label.
func flagNext(c Case, offers []AskOffer) string {
	lead := "The card goes on meanwhile."
	if c.Facts["flag"] == "issue_open_after_end" {
		lead = "This card has ended."
	}
	names := make([]string, len(offers))
	for i, o := range offers {
		names[i] = "“" + o.Label + "”"
	}
	return lead + " Press " + orJoin(names) + " on the board."
}

// FactsOf is case c's facts, as its record holds them.
func FactsOf(c Case) CaseFacts {
	f := factsOf(c)
	f.atCase(c)
	return f
}

// factsOf is case c's facts; a fact of the wrong type stays zero: unread.
func factsOf(c Case) CaseFacts {
	var f CaseFacts
	if b, err := json.Marshal(c.Facts); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	return f
}

// tripNext is the last line of a trip's face for case c: at a cap or a
// budget floor, where a reply is a continue that asks again, every answer
// its face holds and the project chat (boardMove), for a
// monitor flag the answers on its face (flagNext; AskHuman names those of
// the face its row stores), and elsewhere a reply (replyNext).
func tripNext(c Case, reason string) string {
	if ChoiceKind(c.Kind) {
		return "This card is yours: answer with a choice on the card or reply to leave a note. No attempt starts. Close this issue to end it."
	}
	switch c.Kind {
	case CaseMonitorFlag:
		return flagNext(c, offersOfAnswers(FaceOf(c, Facts{CaseFacts: FactsOf(c)}).Answers))
	case CaseSessionCap, CaseAgentTimeCap, CaseBudgetFloor:
		return boardMove(c) + " A reply alone starts no attempt. Close this issue to end the task."
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
	case CaseCheckinHuman, CasePolicyClass, CasePodNeverStarted, CaseProviderRefused, CasePlatformFault,
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
		exits := "Answer on the board"
		if press := boardAnswers(c, ActExtend, ActReverify, ActSuccessor); press != "" {
			exits = "Press " + press + " on the board"
		}
		return "A reply alone changes nothing: the stop repeats until the card's limits or the review's word change. " + exits + ", or close this issue to end the task."
	case CaseScreeningSuspect:
		return "A human reads the screening first: reply on this issue to let the agent work on it, or close the issue to end the task."
	case CaseWaitExpired, CaseMergeApproval, CasePlanCriteria, CasePlanHeld, CaseAgentRequestInfo:
		return "This stop is answered on its own form, not by a reply on this issue. Close this issue to end the task."
	case CasePlanQuestion, CasePlanConfirmation:
		return "A reply on this issue asks the planner for a new revision instead. Closing the issue ends the plan."
	case CaseDependencyClosed, CaseDependencyFailed:
		return "Reply on this issue to start the card anyway with your comment as guidance, or close the issue to end the task."
	}
	return replyNext(reason)
}

// healedLine opens the comment of an ask the factory puts again after a
// restart: its face stands on the board as it was.
const healedLine = "The factory restarted while this card waited for a human. Its question on the board still stands."

// asklessLine opens the comment of an ask the heal pass puts again on a
// card that waited with no live ask.
const asklessLine = "This card waited for a human with no question open. The factory asks its question on the board again."

// askedAgainLine opens the comment of an ask the factory puts again for reason
// (askedAgain), "" for any other reason.
func askedAgainLine(reason string) string {
	switch reason {
	case HealedReason:
		return healedLine
	case AsklessReason:
		return asklessLine
	}
	return ""
}

// IssueAsk opens the comment of ask row face on its card's issue with the
// face as the board shows it: the ask, the why line and the story, each
// in its paragraph, a writer's words quoted.
func IssueAsk(face map[string]any) string {
	ask, _ := face["ask"].(string)
	why, _ := face["why"].(string)
	story, _ := face["story"].(Story)
	var paras []string
	for _, l := range []string{ask, why, story.Planned, story.Happened, story.Left} {
		switch lead, words, ok := QuotedLine(l); {
		case ok:
			paras = append(paras, strings.TrimSpace(lead), Quoted(words))
		case l != "":
			paras = append(paras, l)
		}
	}
	return strings.Join(paras, "\n\n")
}

// quotedLead are the leads of a story line that quotes a writer's words.
var quotedLead = regexp.MustCompile(`^(The agent asks: |(?:The |)[A-Z][\w -]*'s view: )`)

// QuotedLine splits a story line that quotes a writer into its lead and
// the words.
func QuotedLine(line string) (lead, words string, ok bool) {
	m := quotedLead.FindString(line)
	if m == "" {
		return "", "", false
	}
	return m, line[len(m):], true
}

// PlanWaitCase says case c is the case of plan t's wait on its failed card
// (ownFailedCard): it sits beside the plan, which no run holds.
func PlanWaitCase(c Case, t Task) bool {
	return c.Kind == CaseDependencyFailed && t.Track == policy.TrackPlan
}

// EndedCardCase says case c is the open-issue flag of ended card t
// (issue_open_after_end): it asks about the issue the card left open, so
// it stands beside the card at rest.
func EndedCardCase(c Case, t Task) bool {
	return Final(t.Status) && c.Kind == CaseMonitorFlag && c.Facts["flag"] == "issue_open_after_end"
}

// caseBesideRun says no run of card t waits on case c's face: a monitor
// flag's case, beside the card's run, a plan's wait on its failed card, a
// plan's criteria case, opened on the done plan, a merge approval, whose
// run waits on its request's decision, and a plan's question or
// confirmation, whose run waits on its answer and moved the card itself.
// Its face moves no card, writes no escalated step and sends no
// case_faced.
func caseBesideRun(c Case, t Task) bool {
	return c.Kind == CaseMonitorFlag || c.Kind == CasePlanCriteria || c.Kind == CaseMergeApproval || planAsk(c) || PlanWaitCase(c, t)
}

// outlivesCard says case c stays live after card t ended: it is woken,
// climbed and faced on a card that cannot wait for a human.
func outlivesCard(c Case, t Task) bool {
	return c.Kind == CaseMonitorFlag || c.Kind == CasePlanCriteria || PlanWaitCase(c, t)
}

// askHuman is a case's human face (architecture 6.6): state
// waiting-human, mirrored on the issue, a comment with the reason and
// next, the line that says what moves the card on, and a question_asked
// row, written before the wait, with its question and case c (none for the
// zero Case); the agent's choices, if any, ride on that row and the
// timeline's step. A monitor flag's case and a merge approval sit beside
// the card's run, a plan's question and confirmation beside the plan's
// wait for its answers, and a plan's case on its failed card and its
// criteria case beside the plan at rest: its face is the comment, the row
// and the alert, and moves no status, writes no ask the run reads and no
// timeline line. A criteria case's comment is its request's words; a
// merge's face posts none; a plan question's is its revision's one face
// comment (planFaceComment), and its row the planner's again. A case that
// asks its card's stop again (asked_again) says so above the stop. An
// asked-again reason is a stop of its own on a case that has no face yet,
// and on a case at the human repeats the face it had (repeatFace). It
// returns the comment's id: only later comments answer it.
func (r *Runner) AskHuman(ctx context.Context, t Task, c Case, reason, next string, choices []string, up *HandUp) (int64, error) {
	beside := caseBesideRun(c, t)
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return 0, terminalError("%v", err)
	}
	if !outlivesCard(c, t) {
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
	body := askedAgainLine(reason)
	stop := reason
	if askedAgain(reason) {
		stop, _ = c.Facts["reason"].(string)
	}
	note, err := r.planDraftNote(ctx, t.ID, stop)
	if err != nil {
		return 0, err
	}
	question := EscalationQuestion(reason)
	held := r.heldAsk(ctx, t, c)
	var face map[string]any
	whole := (!askedAgain(reason) || c.Status == CaseOpen) && !strings.HasPrefix(reason, screeningAsks)
	if whole {
		// the register writes the face on the facts Act judges, as the case
		// reaches the human; a render reads it as stored
		f, err := r.FactsAtCase(ctx, t, c)
		if err != nil {
			log.Printf("task %s: the facts of case %d: %v", t.ID, c.ID, err)
			f = FactsOf(c)
		}
		fence := func(s string) string { return r.untrusted(t, s) }
		face = FaceRow(r.fencedChoices(t, c), t, f, note, up, fence, r.scrub)
		if slices.Contains(f.Partial, partialTimeline) {
			face[faceUnweighed] = true
		}
		question, _ = face["ask"].(string)
		// the face carries the agent's own words (a why line read from the
		// stop's reason): the comment is sanitised as every outbound text
		body = r.untrusted(t, IssueAsk(face))
		if again := askedAgainLine(factString(c, "asked_again")); again != "" && !askedAgain(reason) {
			// a case that asks its card's stop again says so above the stop
			body = again + "\n\n" + body
		}
		// the stop's evidence follows the face as the agent wrote it, fenced
		// so a mention rings no bell; the fence renders no link, so the
		// links the sanitiser keeps follow it
		safe := r.untrusted(t, reason)
		if _, details, ok := strings.Cut(safe, "\n\n"); ok && strings.TrimSpace(details) != "" {
			body += "\n\nWhat the factory saw:\n\n" + fenced(strings.TrimSpace(details))
		}
		if links := sanitize.Links(safe, r.linkHosts(t)); len(links) > 0 {
			body += "\n\nLinks:\n- " + strings.Join(links, "\n- ")
		}
		// the refusal the newest session ended on rides with the ask,
		// verbatim: the blocker reaches the human with no agent-side
		// reporting to lose it (card #1345). A prover's refusals name its
		// hold-outs, so its ask is factoryd's words alone.
		if t.Source != SourceProver && !planAsk(c) {
			if tool, refusal, ok := r.lastToolRefusal(ctx, t.ID); ok {
				view, _ := face["info"].(string)
				face["info"] = strings.TrimSpace(view + "\n\nThe session's last tool call (" + tool + ") was refused:\n\n" + fenced(refusal))
			}
		}
		if offers, ok := face["offers"].([]AskOffer); ok && c.Kind == CaseMonitorFlag {
			// the issue names the answers the inbox and the board offer
			next = flagNext(c, offers)
		}
	}
	body += "\n\n" + next
	if note != "" {
		body += "\n\n" + note
	}
	if strings.HasPrefix(reason, screeningAsks) {
		// the reasons go to the card only: they would teach the author
		body = screenedLine
	}
	if c.Kind == CasePlanCriteria {
		// the plan's criteria in factoryd's own words, as its request wrote them
		body = reason
	}
	var id int64
	switch {
	case held != nil && whole && t.IssueNumber == 0:
		// factoryd's own human card: its issue is the face, the body asks,
		// and no comment repeats it
		if t, err = r.fileFace(ctx, t, repo, held, face); err != nil {
			return 0, err
		}
	case facesByComment(t, c):
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
	step := map[string]any{"reason": reason, "comment": id}
	if askedAgain(reason) {
		// the sentence asks nothing: the row names the question it asks again
		asks, err := r.store.AskAnchors(ctx, t.ID)
		if err != nil {
			log.Printf("task %s: the question a healed run asks again: %v", t.ID, err)
		}
		if len(asks) > 0 {
			question = cmp.Or(asks[len(asks)-1].Question, question)
		}
	}
	if len(choices) > 0 {
		step["choices"] = choices
	}
	asked := AskRow(c, t, reason, question, id, face, choices, up, r.Scrub.String)
	for _, k := range []string{"source", "decider_line", "labels"} {
		// the held row's own: the poke's line, and the asker its refreshes find
		if v, ok := held[k]; ok {
			asked[k] = v
		}
	}
	if planAsk(c) && t.IssueNumber != 0 {
		if id, err = r.planFaceComment(ctx, t, repo, c, asked, next); err != nil {
			return 0, err
		}
		asked["comment"], step["comment"] = id, id
	}
	if !whole {
		r.repeatFace(ctx, t, c, asked)
	}
	// the ask's record comes before the wait: a card that waits on a human
	// has its ask row, so a failed write fails the step
	if err := r.timeline.WriteRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd, asked); err != nil {
		return 0, err
	}
	if !beside {
		if err := r.waitOnHuman(ctx, t); err != nil {
			return 0, err
		}
		r.record(ctx, t.ID, "escalated", step)
	}
	return id, nil
}

// AskRow is the question_asked row of case c on card t: the comment id
// that asks, the source and the reason, scrubbed by scrub, the case, the
// face the register wrote (none for a healed or screened ask), the
// question, the agent's choices and the answer a rung preselected. A case
// beside the run, and a closed plan's ask, names its kind and flag: the
// card's own ask stays the card's, its thread seeds from that, and the
// settle reads the ask of an ended card's open issue and the closed
// plan's as open.
func AskRow(c Case, t Task, reason, question string, comment int64, face map[string]any, choices []string, up *HandUp, scrub func(string) string) map[string]any {
	asked := map[string]any{"comment": comment, "source": EscalationSource(reason), "reason": textutil.Truncate(scrub(reason), 300)}
	if c.ID != 0 {
		asked["case_id"] = c.ID
	}
	if caseBesideRun(c, t) || c.Kind == CasePlanHeld {
		asked["case_kind"], asked["flag"] = string(c.Kind), c.Facts["flag"]
	}
	switch {
	case c.Kind == CasePlanCriteria:
		asked["approval"] = c.Facts["approval"]
	case c.Kind == CaseMergeApproval:
		// the promises a human judges first, which the inbox's message opens on
		asked["approval"], asked["open"] = c.Facts["approval"], c.Facts["open"]
	case planAsk(c):
		// the planner's row again, which the plan's answer settles by its
		// revision and index
		for _, k := range []string{"revision", "index", "kind", "recommended", "grounds"} {
			if v, ok := c.Facts[k]; ok {
				asked[k] = v
			}
		}
	}
	maps.Copy(asked, face)
	asked["question"] = textutil.Truncate(question, 300)
	if len(choices) > 0 {
		asked["choices"] = choices
	}
	if up != nil && up.Preselected != nil {
		asked["preselected"] = up.Preselected.preselected()
	}
	return asked
}

// FaceRow is the face an ask row of case c on card t stores: the
// register's face on facts f with up's recommendation, and as its info the
// agent's view, note before it. fence fences agent text, scrub masks
// secrets.
func FaceRow(c Case, t Task, f CaseFacts, note string, up *HandUp, fence, scrub func(string) string) map[string]any {
	rec := up.recommend(fence, scrub)
	if c.Kind == CasePlanQuestion && rec.Action != "" {
		// a rung recommends a plan question's choice by its words
		rec.Action = planPrefix + rec.Action
	}
	row := FaceKeys(FaceOf(c, Facts{CaseFacts: f, Card: t, Recommend: rec}))
	if view := agentView(note, up, fence, scrub); view != "" {
		row["info"] = view
	}
	return row
}

// InfoRequestRow is the ask row of the agent's request for information
// reason on card t, asked by comment at asked, with the register's face of
// its case on facts f: the row opens the agent_request_info case at the
// human (askCase). fence fences agent text, scrub masks secrets.
func InfoRequestRow(t Task, f CaseFacts, reason string, comment int64, asked time.Time, fence, scrub func(string) string) map[string]any {
	at := asked.UTC().Format(time.RFC3339)
	c := Case{TaskID: t.ID, Repository: t.Repository, Kind: CaseAgentRequestInfo, Status: CaseAtHuman, Facts: map[string]any{"reason": reason, "asked_at": at}}
	f.atCase(c)
	face := FaceRow(c, t, f, "", nil, fence, scrub)
	ask, _ := face["ask"].(string)
	row := AskRow(c, t, reason, ask, comment, face, nil, nil, scrub)
	row["asked_at"] = at
	return row
}

// fencedChoices is case c with the agent's choices as a face shows them:
// sanitised as every agent text, since a press posts one as the human's
// answer.
func (r *Runner) fencedChoices(t Task, c Case) Case {
	raw := factStrings(c, "choices")
	if len(raw) == 0 {
		return c
	}
	safe := make([]string, len(raw))
	for i, s := range raw {
		safe[i] = r.untrusted(t, s)
	}
	c.Facts = maps.Clone(c.Facts)
	c.Facts["choices"] = safe
	return c
}

// recommend is up's recommendation as a face takes it: the action rung 2
// proposed, else the one the rung below recommended, with its grounds;
// none for no hand-up. fence fences agent text, scrub masks secrets.
func (up *HandUp) recommend(fence, scrub func(string) string) Recommendation {
	if up == nil {
		return Recommendation{}
	}
	rec := Recommendation{Action: up.Action, By: recommendedBy(up), Grounds: up.recommendation(fence, scrub)}
	if up.Preselected != nil {
		rec.Action, rec.Args = verdictKey(up.Preselected.Action, up.Preselected.Args), up.Preselected.Args
	}
	if rec.Action == "" || rec.Action == RungHandUp {
		return Recommendation{}
	}
	return rec
}

// agentView is what the rungs below the human found on the case, the
// planner's note before it: the agent's view beside the face. fence fences
// agent text, scrub masks secrets.
func agentView(note string, up *HandUp, fence, scrub func(string) string) string {
	parts := []string{note}
	if up != nil {
		if up.Found != "" {
			parts = append(parts, fence(scrub(foundLine(up))))
			if up.Before != "" {
				parts = append(parts, fence(scrub(beforeLine(up.Before))))
			}
		}
		parts = append(parts, up.Note)
	}
	return strings.TrimSpace(strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), "\n\n"))
}

// facesByComment says case c's face asks on card t's issue by a comment
// of its own: a merge's face posts none, a plan's ask has its revision's
// one face comment, and a card without an issue (a report task) has its
// question on the card alone.
func facesByComment(t Task, c Case) bool {
	return t.IssueNumber != 0 && c.Kind != CaseMergeApproval && !planAsk(c)
}

// heldAsk is the held question_asked row of case c of card t: factoryd's
// own human card's ask, which its face asks again; nil for any other case.
func (r *Runner) heldAsk(ctx context.Context, t Task, c Case) map[string]any {
	if t.Kind != "human" || c.ID == 0 {
		return nil
	}
	events, err := r.store.TaskEventsOfKind(ctx, t.ID, EventQuestionAsked, 50)
	if err != nil {
		log.Printf("case %d: its held ask: %v", c.ID, err)
		return nil
	}
	for _, e := range events {
		if id, _ := toInt(e.Detail["case_id"]); int64(id) == c.ID && e.Detail["held"] == true && questionOfAsk(e.Detail) != nil {
			return e.Detail
		}
	}
	return nil
}

// fileFace files the issue of factoryd's own human card t, which has none,
// at its face: the held row's question, the card's own, with the rungs'
// view under its info, the face's answers by their labels and the face's
// recommendation, which names one of them, labelled with the row's labels
// and the wait on a human. A face that failed after its issue
// was filed takes that issue on again. A routine's run follows its card to
// the issue.
func (r *Runner) fileFace(ctx context.Context, t Task, repo Repo, held, face map[string]any) (Task, error) {
	q := *questionOfAsk(held)
	str := func(k string) string { s, _ := face[k].(string); return s }
	q.Info = strings.TrimSpace(q.Info + "\n\n" + str("info"))
	offers, _ := face["offers"].([]AskOffer)
	if len(offers) > 0 {
		// the issue lists the answers as the card labels them
		q.Choices = nil
		for _, o := range offers {
			q.Choices = append(q.Choices, QuestionChoice{Value: o.Value, Label: o.Label})
		}
	}
	if rec := str("recommendation"); rec != "" {
		// the answer the face suggests, by its label, then its grounds
		q.Recommendation = rec
		pick, _ := face["preselected"].(map[string]any)
		if i := slices.IndexFunc(offers, func(o AskOffer) bool { return o.Value == pick["action"] }); i >= 0 {
			q.Recommendation = "\u201c" + offers[i].Label + "\u201d: " + rec
		}
	}
	b := HumanCardText(q, "", false)
	reuse, err := strandedIssue(ctx, r.store, r.forgejo, repo, b.Title)
	if err != nil {
		return t, err
	}
	asking := t
	asking.Status = "waiting-human"
	filed, _, err := fileOnIssue(ctx, r.forgejo, repo, b, stringsOf(held["labels"]), asking, reuse, func(f Task) (Task, error) {
		return f, r.store.SetTaskIssue(ctx, f.ID, f.IssueNumber)
	})
	if err != nil {
		return t, err
	}
	t.IssueNumber = filed.IssueNumber
	if t.Source == SourceRoutine && r.Routines != nil {
		if err := r.Routines.RepointRoutineRun(ctx, t.ID, t.ID, t.IssueNumber); err != nil {
			log.Printf("task %s: the routine run's issue #%d: %v", t.ID, t.IssueNumber, err)
		}
	}
	return t, nil
}

// faceKeys are the keys of a question_asked row that make its face.
var faceKeys = []string{"ask", "why", "story", "info", "recommendation", "recommended_by", "choices", "offers", "preselected", "findings"}

// repeatFace fills the face keys asked lacks from the newest ask of case c
// that has them, so an ask repeated (a restart's) is a whole face too.
func (r *Runner) repeatFace(ctx context.Context, t Task, c Case, asked map[string]any) {
	events, err := r.store.TaskEventsOfKind(ctx, t.ID, EventQuestionAsked, 20)
	if err != nil {
		log.Printf("task %s: the ask a repeated ask repeats: %v", t.ID, err)
		return
	}
	for _, e := range events {
		if id, _ := toInt(e.Detail["case_id"]); c.ID != 0 && int64(id) != c.ID {
			continue
		}
		if _, whole := e.Detail["story"]; !whole {
			continue
		}
		for _, k := range faceKeys {
			if v, ok := e.Detail[k]; ok {
				if _, have := asked[k]; !have {
					asked[k] = v
				}
			}
		}
		return
	}
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
	// waiting-image: the image did not come within imageWait;
	// waiting-agent: factoryd's own human card at its face
	if err := r.store.TransitionTask(ctx, t.ID, []string{"queued", "waiting-deps", "running", "in-review", "waiting-poster", "waiting-image", "waiting-agent"}, "waiting-human"); err != nil {
		return terminalError("task %s is %s, cannot wait for a human: %v", t.ID, t.Status, err)
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-human")
	if t.Kind != "human" {
		// a parked card holds no slot: the next queued card takes it; a
		// human card never held one
		pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	}
	return nil
}

// openTrip is OpenTrip's activity, on the activity's attempt.
func (r *Runner) openTrip(ctx context.Context, in TripInput) (TripOutcome, error) {
	info := activity.GetInfo(ctx)
	return r.OpenTrip(ctx, in, tripAttempt{ID: info.WorkflowExecution.RunID + "/" + info.ActivityID, N: info.Attempt})
}
