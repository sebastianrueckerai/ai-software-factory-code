package factoryd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// stepReplan is the timeline step of a replan, on the plan card or on the
// direct card that goes to the planner: it is pending until the planner
// files its revision (replanned) or the switch is refused
// (to_plan_refused).
const stepReplan = "replan"

// replanClosers are the steps that end a pending replan.
var replanClosers = []string{"replanned", "to_plan_refused"}

// The origins of a replan: a person's, or the cut of a running card.
const (
	ReplanPerson = "person"
	ReplanCut    = "cut"
)

// ReplanStep is the replan step's detail. Card is the card whose work goes
// back (the cut card, or a direct card replanned as itself), Stops the
// cards the replan stops, Revision the plan's newest revision at the ask
// (0: none), Time when the step was written.
type ReplanStep struct {
	Origin   string    `json:"origin"`
	By       string    `json:"by,omitempty"`
	Comment  string    `json:"comment,omitempty"`
	Card     string    `json:"card,omitempty"`
	Work     string    `json:"work,omitempty"`
	Stops    []string  `json:"stops,omitempty"`
	Revision int       `json:"revision"`
	Time     time.Time `json:"time"`
}

// replanRuns are the statuses of a plan whose replan runs.
var replanRuns = []string{"queued", "running", "waiting-human"}

// pendingReplan is the last replan step on p's timeline while neither
// closer follows it.
func (r *Runner) pendingReplan(ctx context.Context, p Task) (ReplanStep, bool, error) {
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return ReplanStep{}, false, fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	line := openStep(tl, stepReplan, replanClosers...)
	if line == nil {
		return ReplanStep{}, false, nil
	}
	var step ReplanStep
	if err := json.Unmarshal(line, &step); err != nil {
		return ReplanStep{}, false, fmt.Errorf("the replan step of %s: %w", p.ID, err)
	}
	return step, true, nil
}

// fundPlanner tops card id up to the default card budget left for the
// planner's session; a card with that much left gets nothing. It reads
// the row itself: a caller's copy may predate another replan's funding.
func (r *Runner) fundPlanner(ctx context.Context, id string) error {
	t, _, err := r.task(ctx, id)
	if err != nil {
		return err
	}
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	short := Cents(policy.CardBudget(v.Document.Budget, project.Budget, "", false, 0)) - (Cents(t.BudgetEUR) - Cents(t.SpentEUR))
	if short <= 0 {
		return nil
	}
	_, _, err = r.store.FundCard(ctx, t.ID, float64(short)/100, nil, Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber,
		Kind: EventCardFunded, Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"reason": "the planner's session"}})
	return err
}

// planReplannable says plan p may go back to its planner: done, or failed
// with a revision stored (a replan whose planner failed leaves its cards
// on the board), and not closed; closed tells such a plan whose criteria
// closed it.
func (r *Runner) planReplannable(ctx context.Context, p Task) (replannable, closed bool, err error) {
	switch p.Status {
	case "done":
	case "failed":
		if _, ok, err := r.store.PlanRevision(ctx, p.ID, 0); err != nil || !ok {
			return false, false, err
		}
	default:
		return false, false, nil
	}
	closed, err = r.store.PlanClosed(ctx, p.ID)
	return !closed, closed, err
}

// reopenFrom are the statuses reopenPlan moves a plan from.
var reopenFrom = []string{"done", "failed", "queued"}

// reopenPlan sends plan p back to its planner: queued with its counters at
// zero, money for a session, its issue open and labelled queued, and a
// new run of its workflow. Each step is safe to repeat.
func (r *Runner) reopenPlan(ctx context.Context, p Task) error {
	if err := r.store.TransitionTask(ctx, p.ID, reopenFrom, "queued"); err != nil {
		return err
	}
	if err := r.store.SetTaskUsage(ctx, p.ID, TaskUsage{}); err != nil {
		return err
	}
	if err := r.fundPlanner(ctx, p.ID); err != nil {
		return err
	}
	if err := r.reopenIssue(ctx, p); err != nil {
		return err
	}
	mirrorTaskState(ctx, r.forgejo, p, "queued")
	return r.cfg.Workflows.Reopen(ctx, p.ID, "", TaskSignal{})
}

// reopenIssue opens card t's issue again when it is closed.
func (r *Runner) reopenIssue(ctx context.Context, t Task) error {
	if t.IssueNumber == 0 {
		return nil
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return terminalError("%v", err)
	}
	closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil || !closed {
		return err
	}
	return r.forgejo.ReopenIssue(ctx, repo.Owner, repo.Name, t.IssueNumber)
}

// ReplanChildResult is ReplanChild's answer: the child's closing reason,
// or Refused, which names the plan's state and the fitting action.
type ReplanChildResult struct{ Reason, Refused string }

// ReplanChild sends the work of plan child childID, cut at its session,
// back to its plan's planner: the plan's cards that wait on the child wait
// on the plan, and the plan replans, or its running replan takes the
// child in. A plan that can take no replan refuses, and nothing is
// recorded or moved. Safe to repeat: the join of a queued plan reopens it
// again, so a reopen that failed before its run started is healed.
func (r *Runner) ReplanChild(ctx context.Context, childID, work string) (ReplanChildResult, error) {
	k, _, err := r.task(ctx, childID)
	if err != nil {
		return ReplanChildResult{}, err
	}
	if k.Source != SourcePlan || k.Parent == "" {
		return ReplanChildResult{}, terminalError("task %s belongs to no plan", childID)
	}
	p, _, err := r.task(ctx, k.Parent)
	if err != nil {
		return ReplanChildResult{}, err
	}
	work = sanitize.Inbound(work)
	// a card the newest revision drops stops for that revision, whose
	// plan may have ended since: it closes, and no second round follows
	if newest, n, err := r.planAt(ctx, p.ID, 0); err != nil {
		return ReplanChildResult{}, err
	} else if newest != nil {
		if drop, err := r.dropped(ctx, p.ID, k, *newest, map[int]*Plan{}); err != nil {
			return ReplanChildResult{}, err
		} else if drop {
			if _, err := r.timeline.AppendOnce(ctx, k.ID, "replan_stop", map[string]any{"work": work}); err != nil {
				return ReplanChildResult{}, fmt.Errorf("record replan_stop: %w", err)
			}
			if err := r.moveDependents(ctx, k, p); err != nil {
				return ReplanChildResult{}, err
			}
			return ReplanChildResult{Reason: droppedReason(n)}, nil
		}
	}
	replannable, closed, err := r.planReplannable(ctx, p)
	if err != nil {
		return ReplanChildResult{}, err
	}
	if !replannable && !slices.Contains(replanRuns, p.Status) {
		status := p.Status
		if closed {
			status = "closed"
		}
		return ReplanChildResult{Refused: fmt.Sprintf("Plan #%d is %s, so this card's work cannot go back to its planner: answer to go on with the card, or close it.",
			p.IssueNumber, status)}, nil
	}
	if _, err := r.timeline.AppendOnce(ctx, k.ID, "replan_stop", map[string]any{"work": work}); err != nil {
		return ReplanChildResult{}, fmt.Errorf("record replan_stop: %w", err)
	}
	answer := ReplanChildResult{Reason: fmt.Sprintf("stopped for the replan of plan #%d", p.IssueNumber)}
	if replannable {
		newest, _, err := r.store.PlanRevision(ctx, p.ID, 0)
		if err != nil {
			return ReplanChildResult{}, err
		}
		wrote, err := r.timeline.AppendUnlessPending(ctx, p.ID, stepReplan, map[string]any{"origin": ReplanCut, "card": k.ID, "work": work,
			"stops": []string{k.ID}, "revision": newest.Revision}, replanClosers...)
		if err != nil {
			return ReplanChildResult{}, fmt.Errorf("record %s: %w", stepReplan, err)
		}
		if wrote {
			answer.Reason = fmt.Sprintf("cut: plan #%d replans its work", p.IssueNumber)
		}
	}
	// the plan is open again before the waits move onto it, or they read
	// as met; a queued plan may be a reopen that failed before its run
	// started
	if replannable || p.Status == "queued" {
		if err := r.reopenPlan(ctx, p); err != nil {
			return ReplanChildResult{}, err
		}
	}
	if err := r.moveDependents(ctx, k, p); err != nil {
		return ReplanChildResult{}, err
	}
	return answer, nil
}

// ReplanWait is ReplanStopped's answer: Stopped when the planner may run,
// else Open the stopped cards whose work still runs (stoppable); By and
// Comment are the person's name and note of their replan, or of the one a
// cut took.
type ReplanWait struct {
	Stopped     bool
	Open        []string
	By, Comment string
}

// ReplanStopped tells whether the cards plan planID's pending replan stops,
// and its started cards its newest revision drops (dropped), have ended,
// so its planner may run on what they left and a revision may file on
// their last pushes.
func (r *Runner) ReplanStopped(ctx context.Context, planID string) (ReplanWait, error) {
	p, _, err := r.task(ctx, planID)
	if err != nil {
		return ReplanWait{}, err
	}
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return ReplanWait{}, err
	}
	var w ReplanWait
	if pending {
		w.By, w.Comment = step.By, step.Comment
	}
	for _, id := range step.Stops {
		k, ok, err := r.store.Task(ctx, id)
		if err != nil {
			return ReplanWait{}, err
		}
		if ok && stoppable(k) {
			w.Open = append(w.Open, id)
		}
	}
	newest, _, err := r.planAt(ctx, p.ID, 0)
	if err != nil {
		return ReplanWait{}, err
	}
	if newest != nil {
		cards, err := r.store.PlanCards(ctx, p.ID)
		if err != nil {
			return ReplanWait{}, err
		}
		revisions := map[int]*Plan{}
		for _, k := range cards {
			if slices.Contains(w.Open, k.ID) {
				continue
			}
			if drop, err := r.dropped(ctx, p.ID, k, *newest, revisions); err != nil {
				return ReplanWait{}, err
			} else if drop {
				w.Open = append(w.Open, k.ID)
			}
		}
	}
	w.Stopped = len(w.Open) == 0
	return w, nil
}

// dropped tells whether plan card k started, still runs (stoppable), and
// has its key in the revision it started against but not in next, as
// RevisionDrops judges a stale card: a delta card, in no revision, is
// never dropped. revisions caches the plan's revisions read, by number.
func (r *Runner) dropped(ctx context.Context, planID string, k Task, next Plan, revisions map[int]*Plan) (bool, error) {
	holds := func(p *Plan) bool {
		return p != nil && slices.ContainsFunc(p.Cards, func(c PlanCard) bool { return c.Key == k.PlanKey })
	}
	if k.PlanKey == "" || k.PlanRevision == 0 || !stoppable(k) || holds(&next) {
		return false, nil
	}
	ran, read := revisions[k.PlanRevision]
	if !read {
		var err error
		if ran, _, err = r.planAt(ctx, planID, k.PlanRevision); err != nil {
			return false, err
		}
		revisions[k.PlanRevision] = ran
	}
	return holds(ran), nil
}

// droppedReason closes a card the plan's revision drops.
func droppedReason(revision int) string {
	return fmt.Sprintf("the plan's revision %d drops it", revision)
}

// stopForReplan stops card k for plan p's replan: a live session is asked
// to stop (its workflow then cuts the card through ReplanChild); a card
// with none hands its dependents to p and ends closed with why. stopped
// is false for a session that took a replan already: it stops for it.
func (r *Runner) stopForReplan(ctx context.Context, k, p Task, why string) (stopped bool, err error) {
	err = r.EscalateSession(ctx, k.ID, AgentAsk{Kind: AskReplan, Reason: why})
	switch {
	case errors.Is(err, errReplanAsked):
		return false, nil
	case !errors.Is(err, ErrNoLiveSession):
		return err == nil, err
	}
	if err := r.moveDependents(ctx, k, p); err != nil {
		return false, err
	}
	return true, r.Finish(ctx, k.ID, "closed", why)
}

// stoppable: card k's work still runs, so a replan stops it; a merged
// card's work is on main, and a closing card is ending.
func stoppable(k Task) bool {
	return !Final(k.Status) && k.Status != "merged" && k.Status != "closing"
}

// moveDependents hands the waits of plan p's cards on card k to p: the
// store's edges, then their issues' dependencies, which only mirror them,
// so a Forgejo failure there is logged.
func (r *Runner) moveDependents(ctx context.Context, k, p Task) error {
	moved, err := r.store.MoveDependents(ctx, k.ID, p.ID)
	if err != nil || len(moved) == 0 {
		return err
	}
	repo, err := SplitRepo(p.Repository)
	if err != nil {
		log.Printf("plan %s: the issue dependencies on #%d: %v", p.ID, k.IssueNumber, err)
		return nil
	}
	for _, id := range moved {
		s, ok, err := r.store.Task(ctx, id)
		if err != nil || !ok || s.IssueNumber == 0 {
			if err != nil {
				log.Printf("plan %s: the issue dependencies of %s: %v", p.ID, id, err)
			}
			continue
		}
		for _, dep := range []struct {
			issue  int64
			remove bool
		}{{p.IssueNumber, false}, {k.IssueNumber, true}} {
			if dep.issue == 0 {
				continue
			}
			if err := r.forgejo.SetIssueDependency(ctx, repo, s.IssueNumber, dep.issue, dep.remove); err != nil {
				log.Printf("plan %s: issue #%d on #%d (remove %t): %v", p.ID, s.IssueNumber, dep.issue, dep.remove, err)
			}
		}
	}
	return nil
}

// ReplanAsk is a person's Replan: who asks, and why, for the planner.
type ReplanAsk struct{ By, Comment string }

// ErrNoReplan refuses a person's Replan: it names the state of the card or
// its plan and the fitting action.
type ErrNoReplan string

func (e ErrNoReplan) Error() string { return string(e) }

// ReplanComment is a person's replan comment as the replan step holds it,
// sanitised and cut. The console posts these words on the issue, so the
// plan loop knows the comment for the replan's own note.
func ReplanComment(s string) string {
	return textutil.Truncate(sanitize.Inbound(s), maxHandoverMessage)
}

// MayReplan tells whether a person may send card t back to the planner: an
// agent card in the work bound that is a plan, a plan's child, or a direct
// card that did not merge, where its project's process has a plan track
// (planTrack).
func MayReplan(t Task, planTrack bool) bool {
	return t.Kind == "agent" && !t.Unbound() && (InPlan(t) || planTrack && t.MergeCommit == "" && t.Status != "done" && t.Status != "merged")
}

// Replan sends card taskID's work back to the planner at a person's ask,
// to plan again on top of what was built: a plan, or a plan's child,
// replans its plan, and the plan's started open cards stop; a direct card
// goes to the planner as itself. The answer is the note for the person. A
// card or plan that takes no replan answers ErrNoReplan and changes
// nothing, except that a plan with a replan pending has its reopen healed.
func (r *Runner) Replan(ctx context.Context, taskID string, ask ReplanAsk) (string, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return "", err
	}
	ask.Comment = ReplanComment(ask.Comment)
	switch {
	case t.Kind != "agent":
		return "", ErrNoReplan(personCardRefusal(t))
	case t.Unbound():
		return "", ErrNoReplan(unboundRefusal(t))
	case InPlan(t):
		return r.replanPlan(ctx, GroupOf(t), ask)
	}
	return r.replanDirect(ctx, t, ask)
}

// replanPlan sends plan planID back to its planner: the replan step with
// the plan's started open cards as its stops, then completeReplan.
func (r *Runner) replanPlan(ctx context.Context, planID string, ask ReplanAsk) (string, error) {
	p, _, err := r.task(ctx, planID)
	if err != nil {
		return "", err
	}
	plan := fmt.Sprintf("Plan #%d", p.IssueNumber)
	switch p.Status {
	case "closed":
		return "", ErrNoReplan(plan + " is closed: a person closed it. File a new card or plan for the change.")
	case "closing":
		return "", ErrNoReplan(plan + " is closing: replan it once it has ended.")
	}
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return "", err
	}
	if pending {
		return r.completeReplan(ctx, p, step)
	}
	replannable, closed, err := r.planReplannable(ctx, p)
	switch {
	case err != nil:
		return "", err
	case closed:
		return "", ErrNoReplan(plan + " is closed: its criteria are met. File a new card or plan for the change.")
	case !replannable:
		return "", ErrNoReplan(plan + " is " + p.Status + ": it has not filed its cards yet. Comment on its issue to change the plan.")
	}
	newest, _, err := r.store.PlanRevision(ctx, p.ID, 0)
	if err != nil {
		return "", err
	}
	cards, err := r.store.PlanCards(ctx, p.ID)
	if err != nil {
		return "", err
	}
	step = ReplanStep{Origin: ReplanPerson, By: ask.By, Comment: ask.Comment, Revision: newest.Revision}
	for _, k := range cards {
		if k.PlanRevision > 0 && stoppable(k) {
			step.Stops = append(step.Stops, k.ID)
		}
	}
	wrote, err := r.timeline.AppendUnlessPending(ctx, p.ID, stepReplan, map[string]any{"origin": step.Origin, "by": step.By, "comment": step.Comment,
		"stops": step.Stops, "revision": step.Revision}, replanClosers...)
	if err != nil {
		return "", fmt.Errorf("record %s: %w", stepReplan, err)
	}
	if !wrote {
		return "", beingReplanned(p)
	}
	return r.completeReplan(ctx, p, step)
}

// beingReplanned refuses a press on plan p, whose replan is pending.
func beingReplanned(p Task) ErrNoReplan {
	return ErrNoReplan(fmt.Sprintf("Plan #%d is being replanned: wait for its revision.", p.IssueNumber))
}

// completeReplan carries out plan p's pending replan step, for the press
// that wrote it and for any later one, so a press heals one that failed
// partway: the plan reopened, then each of the step's stops that still
// runs stopped, so the waits they hand over land on an open plan. A press
// that finds nothing left to do is refused: the plan is being replanned.
func (r *Runner) completeReplan(ctx context.Context, p Task, step ReplanStep) (string, error) {
	plan := fmt.Sprintf("Plan #%d", p.IssueNumber)
	acted := p.Status == "done" || p.Status == "failed"
	var err error
	if slices.Contains(reopenFrom, p.Status) {
		err = r.reopenPlan(ctx, p)
	} else {
		// a run that runs answers it as a no-op
		err = r.cfg.Workflows.Reopen(ctx, p.ID, "", TaskSignal{})
	}
	if err != nil {
		return "", err
	}
	why := fmt.Sprintf("stopped for the replan of plan #%d", p.IssueNumber)
	for _, id := range step.Stops {
		k, ok, err := r.store.Task(ctx, id)
		if err != nil {
			return "", err
		}
		if !ok || !stoppable(k) {
			continue
		}
		stopped, err := r.stopForReplan(ctx, k, p, why)
		if err != nil {
			return "", err
		}
		acted = acted || stopped
	}
	switch {
	case !acted:
		return "", beingReplanned(p)
	case len(step.Stops) == 0:
		return plan + " goes back to the planner: no card stops.", nil
	case len(step.Stops) == 1:
		return plan + " goes back to the planner: 1 card stops, and its work stays on its branch.", nil
	}
	return fmt.Sprintf("%s goes back to the planner: %d cards stop, and their work stays on their branches.", plan, len(step.Stops)), nil
}

// replanDirect sends direct card t to the planner. The person's words go
// first, as the to_plan signal, which the workflow takes at its next step
// and records as the card's replan step; then a running session is asked
// to stop. A failed or closed card is queued again for a new run. A card
// that waits on its screening refuses: the note posted on its issue would
// answer the screening's question.
func (r *Runner) replanDirect(ctx context.Context, t Task, ask ReplanAsk) (string, error) {
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	switch {
	case t.MergeCommit != "" || t.Status == "done" || t.Status == "merged":
		return "", ErrNoReplan(mergedRefusal(t))
	case t.Status == "closing":
		return "", ErrNoReplan(card + " is closing: replan it once it has ended.")
	case t.Status == "waiting-human":
		events, err := r.store.IntakeEvents(ctx, t.ID)
		if err != nil {
			return "", err
		}
		if waitsOnScreening(events) {
			return "", ErrNoReplan(card + " waits for a person to judge its screening: let it run or close its issue, then replan it.")
		}
	}
	project, _, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return "", err
	}
	if _, planTrack := trackClass(project, policy.TrackPlan); !planTrack {
		return "", ErrNoReplan(noPlanTrackRefusal(t))
	}
	sig := TaskSignal{Author: ask.By, Body: ask.Comment}
	if Final(t.Status) {
		if err := r.store.TransitionTask(ctx, t.ID, []string{"failed", "closed"}, "queued"); err != nil {
			return "", err
		}
		if err := r.reopenIssue(ctx, t); err != nil {
			return "", err
		}
		mirrorTaskState(ctx, r.forgejo, t, "queued")
		if err := r.cfg.Workflows.Reopen(ctx, t.ID, SignalToPlan, sig); err != nil {
			return "", err
		}
		return card + " goes to the planner, which plans on top of its branch.", nil
	}
	if err := r.cfg.Workflows.Reopen(ctx, t.ID, SignalToPlan, sig); err != nil {
		return "", err
	}
	switch err := r.EscalateSession(ctx, t.ID, AgentAsk{Kind: AskReplan, Reason: "replanned by " + ask.By + ": " + ask.Comment}); {
	case err == nil || errors.Is(err, errReplanAsked):
		return card + " stops its session and goes to the planner.", nil
	case errors.Is(err, ErrNoLiveSession):
		return card + " goes to the planner at its next step.", nil
	default:
		return "", err
	}
}

// oldCards are plan p's record of cards a revision keeps, changes or
// drops: its cards, and p itself while its pending replan is its own (a
// direct card's replan or cut), whose work the findings name by issue.
func (r *Runner) oldCards(ctx context.Context, p Task) ([]Task, error) {
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return nil, err
	}
	return r.oldCardsOf(ctx, p, step, pending)
}

// oldCardsOf is oldCards with p's pending replan step already read.
func (r *Runner) oldCardsOf(ctx context.Context, p Task, step ReplanStep, pending bool) ([]Task, error) {
	cards, err := r.store.PlanCards(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if pending && step.Card == p.ID {
		cards = append(cards, p)
	}
	return cards, nil
}

// replanState is what a revision of plan is judged against: the revision
// before (nil: none) and its number, the old cards, each started open
// card's entry in the revision it started against by key, the pending
// cut's card and whether its branch holds a commit past its base, and the
// old cards with a merge commit by id.
type replanState struct {
	prev     *Plan
	revision int
	old      []Task
	started  map[string]PlanCard
	plan     Task
	cut      string
	cutWork  bool
	merged   map[string]bool
}

// replanState reads plan p's record for replanFindings: its timeline and
// each revision once.
func (r *Runner) replanState(ctx context.Context, p Task) (replanState, error) {
	st := replanState{plan: p, started: map[string]PlanCard{}, merged: map[string]bool{}}
	var err error
	if st.prev, st.revision, err = r.planAt(ctx, p.ID, 0); err != nil {
		return replanState{}, err
	}
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return replanState{}, err
	}
	if st.old, err = r.oldCardsOf(ctx, p, step, pending); err != nil {
		return replanState{}, err
	}
	revisions := map[int]*Plan{st.revision: st.prev}
	for _, k := range st.old {
		if k.MergeCommit != "" {
			st.merged[k.ID] = true
		}
		if k.PlanKey == "" || k.PlanRevision == 0 || !stoppable(k) {
			continue
		}
		at, read := revisions[k.PlanRevision]
		if !read {
			if at, _, err = r.planAt(ctx, p.ID, k.PlanRevision); err != nil {
				return replanState{}, err
			}
			revisions[k.PlanRevision] = at
		}
		if at == nil {
			continue
		}
		if i := slices.IndexFunc(at.Cards, func(c PlanCard) bool { return c.Key == k.PlanKey }); i >= 0 {
			st.started[k.PlanKey] = at.Cards[i]
		}
	}
	if pending && step.Origin == ReplanCut {
		st.cut = step.Card
		head, err := r.BranchHead(ctx, step.Card)
		if err != nil {
			return replanState{}, err
		}
		st.cutWork = head != ""
	}
	return st, nil
}

// planAt is plan task id's revision n decoded, the newest for 0, and its
// number; nil and 0 when there is none.
func (r *Runner) planAt(ctx context.Context, id string, n int) (*Plan, int, error) {
	rev, ok, err := r.store.PlanRevision(ctx, id, n)
	if err != nil || !ok {
		return nil, 0, err
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return nil, 0, terminalError("plan %s revision %d: %v", id, rev.Revision, err)
	}
	return &p, rev.Revision, nil
}

// fromRef is how a revision's from names old card k: its key, or
// #<issue> for a card without one.
func fromRef(k Task) string {
	if k.PlanKey != "" {
		return k.PlanKey
	}
	return fmt.Sprintf("#%d", k.IssueNumber)
}

// replanFindings judges revision next against the plan's record st: a
// card is kept by its key, changed by a new card whose from names it, and
// dropped by leaving its key out. An old key is a key with a card row. A
// from the revision before held for the same key was judged at its
// filing.
func replanFindings(st replanState, next Plan) []string {
	old := map[string]Task{}
	for _, k := range st.old {
		old[fromRef(k)] = k
	}
	prev := map[string]PlanCard{}
	if st.prev != nil {
		for _, c := range st.prev.Cards {
			prev[c.Key] = c
		}
	}
	kept := map[string]PlanCard{}
	adds := false
	for _, c := range next.Cards {
		kept[c.Key] = c
		_, row := old[c.Key]
		adds = adds || !row
	}
	var found []string
	first := map[string]string{}
	for _, c := range next.Cards {
		if c.From == "" {
			continue
		}
		if a, ok := first[c.From]; ok {
			found = append(found, fmt.Sprintf("cards %s and %s: both start from %s; one card carries an old card's work: keep the from on %s and start %s from main",
				a, c.Key, c.From, a, c.Key))
		} else {
			first[c.From] = c.Key
		}
		k, known := old[c.From]
		// a kept card that ended or merged has its own finding
		if _, keeps := kept[c.From]; known && keeps && !st.merged[k.ID] && k.Status != "closed" && k.Status != "failed" {
			found = append(found, fmt.Sprintf("card %s: starts from %s, which this revision keeps; drop %s's key or start %s from main", c.Key, c.From, c.From, c.Key))
		}
		if was, held := prev[c.Key]; held && was.From == c.From {
			continue
		}
		if _, row := old[c.Key]; row {
			found = append(found, fmt.Sprintf("card %s: keeps an old key and names from; give %s a new key, since a card that starts from another card's work takes a new key", c.Key, c.Key))
		}
		switch {
		case !known:
			found = append(found, fmt.Sprintf("card %s: from %s names no card of this plan's record; name an old card's key, or #<issue> of a card the findings list without a key", c.Key, c.From))
		case st.merged[k.ID]:
			found = append(found, fmt.Sprintf("card %s: from %s, which merged: its work is on main, so start from main and leave from out", c.Key, c.From))
		}
	}
	for _, k := range st.old {
		if k.PlanKey == "" {
			continue
		}
		c, keeps := kept[k.PlanKey]
		_, held := prev[k.PlanKey]
		integration := st.prev != nil && roleOf(*st.prev, k.PlanKey) == RoleIntegration
		switch {
		case keeps && (k.Status == "closed" || k.Status == "failed") && k.PlanRevision == 0:
			found = append(found, fmt.Sprintf("card %s ended %s before it started: leave its key out, and add its work under a new key", k.PlanKey, k.Status))
		case keeps && (k.Status == "closed" || k.Status == "failed"):
			found = append(found, fmt.Sprintf(`card %s ended %s: leave its key out, and add a card with "from": "%s" to go on from its work`, k.PlanKey, k.Status, k.PlanKey))
		case st.merged[k.ID] && integration && keeps && adds:
			found = append(found, "card "+k.PlanKey+" is the integration card and merged: leave its key out; the revision's new integration card checks the whole feature again")
		case st.merged[k.ID] && !integration && held && !keeps:
			found = append(found, "card "+k.PlanKey+" merged: keep its key; a later card changes its work")
		}
		if entry, ok := st.started[k.PlanKey]; ok && keeps && !sameEntry(c, entry) {
			found = append(found, fmt.Sprintf("card %s started against revision %d: keep its entry as revision %d has it, or leave its key out to stop it",
				k.PlanKey, k.PlanRevision, k.PlanRevision))
		}
	}
	if st.cutWork {
		if i := slices.IndexFunc(st.old, func(k Task) bool { return k.ID == st.cut }); i >= 0 {
			ref := fromRef(st.old[i])
			if !slices.ContainsFunc(next.Cards, func(c PlanCard) bool { return c.From == ref }) {
				found = append(found, fmt.Sprintf(`the cut card #%d holds work on its branch: name "%s" in the "from" of the card that lands it`, st.old[i].IssueNumber, ref))
			}
		}
	}
	return found
}

// sameEntry tells whether a and b are one card entry; a list left out is
// an empty one.
func sameEntry(a, b PlanCard) bool {
	if !slices.Equal(a.Directories, b.Directories) || !slices.Equal(a.DependsOn, b.DependsOn) || !slices.Equal(a.Acceptance, b.Acceptance) {
		return false
	}
	a.Directories, a.DependsOn, a.Acceptance = nil, nil, nil
	b.Directories, b.DependsOn, b.Acceptance = nil, nil, nil
	return reflect.DeepEqual(a, b)
}

// recordReplan writes the record of plan p's pending replan step, whose
// revision filed next, the cards' issues by key: each old card kept by
// its key, changed into the new card whose from names it, or dropped,
// and the new cards added. The old cards are oldCards that existed at
// the step, without those closed before it; the cards the replan itself
// stops count whatever the clocks say. It comments the counts and who
// asked on the plan's issue and steps each old card; a cut says on its
// card's issue what it was cut into, at which session and spend, and
// steps it. The plan's replanned step comes last, as it ends the pending
// replan: a failure before it fails the filing, which runs again, and
// writes the comments, the cut and each old card's step once.
func (r *Runner) recordReplan(ctx context.Context, p Task, repo Repo, revision int, next Plan, step ReplanStep, issues map[string]int64) error {
	cards, err := r.oldCardsOf(ctx, p, step, true)
	if err != nil {
		return err
	}
	keys, into := map[string]bool{}, map[string]string{}
	for _, c := range next.Cards {
		keys[c.Key] = true
		if c.From != "" {
			into[c.From] = c.Key
		}
	}
	type fate struct {
		k       Task
		verdict string
		into    int64
	}
	var fates []fate
	count, held, named := map[string]int{}, map[string]bool{}, map[string]bool{}
	for _, k := range cards {
		stopped := k.ID == step.Card || slices.Contains(step.Stops, k.ID)
		if k.CreatedAt.After(step.Time) || k.Status == "closed" && k.FinishedAt != nil && k.FinishedAt.Before(step.Time) && !stopped {
			continue
		}
		f := fate{k: k, verdict: "dropped"}
		switch ref := fromRef(k); {
		case k.PlanKey != "" && keys[k.PlanKey]:
			f.verdict = "kept"
		case into[ref] != "":
			f.verdict, f.into, named[ref] = "changed", issues[into[ref]], true
		}
		held[k.PlanKey] = k.PlanKey != ""
		count[f.verdict]++
		fates = append(fates, f)
	}
	added := 0
	var cutInto []int64
	for _, c := range planOrder(next, planEdges(next)) {
		if held[c.Key] {
			continue
		}
		cutInto = append(cutInto, issues[c.Key])
		if !named[c.From] {
			added++
		}
	}
	note := fmt.Sprintf("replanned at revision %d: kept %d, changed %d, dropped %d, added %d.", revision, count["kept"], count["changed"], count["dropped"], added)
	i := slices.IndexFunc(cards, func(k Task) bool { return k.ID == step.Card })
	switch {
	case step.Origin == ReplanPerson:
		note += fmt.Sprintf("\nAsked by %s: %s", cmp.Or(step.By, "a person"), sanitize.Inbound(step.Comment))
	case step.Origin == ReplanCut && i >= 0:
		note += fmt.Sprintf("\nThe cut of #%d: %s", cards[i].IssueNumber, sanitize.Inbound(step.Work))
	}
	if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, p.IssueNumber, "factoryd: "+r.untrusted(p, note)); err != nil {
		return fmt.Errorf("comment the replan on #%d: %w", p.IssueNumber, err)
	}
	if step.Origin == ReplanCut && i >= 0 {
		cut := cards[i]
		session, spent, err := r.cutAt(ctx, p, cut)
		if err != nil {
			return err
		}
		refs := make([]string, len(cutInto))
		for j, n := range cutInto {
			refs[j] = fmt.Sprintf("#%d", n)
		}
		body := fmt.Sprintf("factoryd: cut into %s at session %d, %.2f EUR spent.", strings.Join(refs, ", "), session, spent)
		if err := r.forgejo.CommentOnce(ctx, repo.Owner, repo.Name, cut.IssueNumber, body); err != nil {
			return fmt.Errorf("comment the cut on #%d: %w", cut.IssueNumber, err)
		}
		if _, err := r.timeline.AppendOnce(ctx, cut.ID, "cut", map[string]any{"into": cutInto, "session": session, "spent": spent}); err != nil {
			return fmt.Errorf("record cut: %w", err)
		}
	}
	for _, f := range fates {
		if f.k.ID == p.ID {
			continue // the plan's own step records it
		}
		d := map[string]any{"revision": revision, "verdict": f.verdict}
		if f.into != 0 {
			d["into"] = f.into
		}
		if _, err := r.timeline.AppendOnceFor(ctx, f.k.ID, "replanned", revision, d); err != nil {
			return fmt.Errorf("record replanned on %s: %w", f.k.ID, err)
		}
	}
	detail := map[string]any{"revision": revision, "kept": count["kept"], "changed": count["changed"], "dropped": count["dropped"], "added": added,
		"origin": step.Origin, "by": step.By, "comment": step.Comment}
	if err := r.timeline.Append(ctx, p.ID, "replanned", detail); err != nil {
		return fmt.Errorf("record replanned: %w", err)
	}
	r.timeline.RecordRow(ctx, p.ID, EventPlanReplanned, FactoryDecider, ViaFactoryd, detail)
	return nil
}

const (
	// recordFileBytes bounds one old card's record file.
	recordFileBytes = 16 << 10
	// recordSessions is how many of an old card's newest sessions its
	// record reads for the working state of its last session.
	recordSessions = 3
)

// replanRecord is a replan's record for its planner. findings are
// factoryd's lines: a person's opener and one line per old card; work are
// the agent's words, the cut's opener, which stand in the brief's fence;
// branches are the fork's branches of the old cards that hold work and
// did not merge, each <fork branch>:<local branch>; files are one record
// per old card.
type replanRecord struct {
	findings, branches []string
	files              []harness.ReportFile
	work               []string
}

// replanRecord is plan p's record for the planner session of its pending
// replan, and whether one is pending.
func (r *Runner) replanRecord(ctx context.Context, p Task) (replanRecord, bool, error) {
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil || !pending {
		return replanRecord{}, false, err
	}
	cards, err := r.oldCardsOf(ctx, p, step, true)
	if err != nil {
		return replanRecord{}, false, err
	}
	var rec replanRecord
	if step.Origin == ReplanPerson {
		opener := fmt.Sprintf("This is a replan of plan #%d, asked by %s", p.IssueNumber, cmp.Or(step.By, "a person"))
		if comment := sanitize.Inbound(step.Comment); comment != "" {
			opener += ": " + comment
		}
		rec.findings = append(rec.findings, opener)
	}
	if i := slices.IndexFunc(cards, func(k Task) bool { return k.ID == step.Card }); step.Origin == ReplanCut && i >= 0 {
		session, spent, err := r.cutAt(ctx, p, cards[i])
		if err != nil {
			return replanRecord{}, false, err
		}
		rec.work = append(rec.work, fmt.Sprintf("This is the cut of #%d at session %d, %.2f EUR spent: %s", cards[i].IssueNumber, session, spent, sanitize.Inbound(step.Work)))
	}
	roles := map[string]string{}
	if newest, _, err := r.planAt(ctx, p.ID, 0); err != nil {
		return replanRecord{}, false, err
	} else if newest != nil {
		for _, c := range newest.Cards {
			roles[c.Key] = c.Role
		}
	}
	revisions := map[int]*Plan{}
	for _, k := range cards {
		var head string
		if k.MergeCommit == "" {
			if head, err = r.BranchHead(ctx, k.ID); err != nil {
				return replanRecord{}, false, err
			}
		}
		branch := fmt.Sprintf("card/%d", k.IssueNumber)
		work, onBranch := "no commit past its base", ""
		if head != "" {
			work, onBranch = "its work on branch "+branch, ", its work on branch "+branch
			rec.branches = append(rec.branches, taskBranch(k)+":"+branch)
		}
		tl, err := r.objects.Get(ctx, "sessions/"+k.ID+"/timeline.jsonl")
		if err != nil {
			return replanRecord{}, false, fmt.Errorf("the timeline of %s: %w", k.ID, err)
		}
		// said is the file's state line where it differs: a final reason
		// may be the agent's words, which the fenced file holds and no
		// finding does
		var state, said string
		switch {
		case k.MergeCommit != "":
			state = "merged as " + k.MergeCommit[:min(8, len(k.MergeCommit))]
		case k.ID == p.ID:
			state = work
		case k.ID == step.Card || slices.Contains(step.Stops, k.ID):
			state = "stopped for this replan, " + work
		case Final(k.Status):
			state = "ended " + k.Status + ", " + work
			if reason, _ := lastStep(tl, "final")["reason"].(string); stopLine(reason) != "" {
				said = "ended " + k.Status + ": " + stopLine(reason) + ", " + work
			}
		case k.PlanRevision == 0:
			state = "never started" + onBranch
		default:
			state = "running" + onBranch
		}
		title, _, _ := strings.Cut(k.Objective, "\n")
		name := fmt.Sprintf("card-%d.md", k.IssueNumber)
		role, spend := roles[k.PlanKey], ""
		if k.ID == p.ID {
			rec.findings = append(rec.findings, fmt.Sprintf("#%d (this card, no key): %s; its record is %s", k.IssueNumber, state, name))
		} else {
			if _, held := roles[k.PlanKey]; !held && k.PlanRevision > 0 {
				at, read := revisions[k.PlanRevision]
				if !read {
					if at, _, err = r.planAt(ctx, p.ID, k.PlanRevision); err != nil {
						return replanRecord{}, false, err
					}
					revisions[k.PlanRevision] = at
				}
				if at != nil {
					role = roleOf(*at, k.PlanKey)
				}
			}
			role = cmp.Or(role, RoleFeature)
			spend = fmt.Sprintf("; spent %.2f of %.2f EUR in %d sessions", k.SpentEUR, k.BudgetEUR, k.Usage.Attempts)
			rec.findings = append(rec.findings, fmt.Sprintf("#%d %s (%s, key %s): %s%s; its record is %s", k.IssueNumber, title, role, k.PlanKey, state, spend, name))
		}
		// its last session is one from before the replan: a direct card's
		// later ones are its planner's
		text, err := r.oldCardText(ctx, k, tl, fmt.Sprintf("# #%d %s", k.IssueNumber, title), cmp.Or(said, state)+spend, role, cmp.Or(head, k.MergeCommit, k.SHA), step.Time)
		if err != nil {
			return replanRecord{}, false, err
		}
		rec.files = append(rec.files, harness.ReportFile{Name: name, Text: textutil.Truncate(sanitize.Inbound(text), recordFileBytes)})
	}
	return rec, true, nil
}

// oldCardText is old card k's record file under title: its state line,
// then each section that has lines: the reviews' findings and the stages'
// from its timeline tl, an integration card's criteria verdicts, the
// check-in's notes, and the working state of its newest session created
// before before that left one, at branch head commit, with how that
// session ended.
func (r *Runner) oldCardText(ctx context.Context, k Task, tl []byte, title, state, role, commit string, before time.Time) (string, error) {
	parts := []string{title, state}
	section := func(heading string, lines []string) {
		if len(lines) > 0 {
			parts = append(parts, "## "+heading+"\n"+strings.Join(lines, "\n"))
		}
	}
	var st RunState
	stageMemoryOf(tl, &st)
	var lines []string
	for i, rv := range st.Reviews {
		for _, f := range rv.Findings {
			lines = append(lines, fmt.Sprintf("- round %d, %s: %s", i+1, rv.Verdict, f))
		}
	}
	section("Review findings", lines)
	lines = nil
	for _, h := range st.History {
		lines = append(lines, "- "+h)
	}
	section("Stage findings", lines)
	if role == RoleIntegration {
		rows, err := r.store.CriteriaVerdicts(ctx, k.ID)
		if err != nil {
			return "", err
		}
		lines = nil
		for _, v := range rows {
			line := "- " + v.Criterion
			for _, s := range []string{v.Verdict, v.Evidence} {
				if s != "" {
					line += ": " + s
				}
			}
			lines = append(lines, line)
		}
		section("Criteria verdicts", lines)
	}
	lines = nil
	ends := map[string]carryFrom{}
	for line := range strings.SplitSeq(string(tl), "\n") {
		var s struct{ Step, Reason, Handover, Session, Outcome, Summary string }
		if json.Unmarshal([]byte(line), &s) != nil {
			continue
		}
		switch {
		case s.Step == "checkin" && s.Reason != "":
			lines = append(lines, "- check-in: "+s.Reason)
		case s.Step == "handover" && s.Handover != "":
			lines = append(lines, "- handover: "+s.Handover)
		case s.Step == "session_end":
			ends[s.Session] = carryFrom{Outcome: SessionOutcome(s.Outcome), Stop: s.Summary}
		}
	}
	section("Check-in notes", lines)
	sessions, err := r.store.RecentSessions(ctx, k.ID, before, recordSessions)
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		rec, err := r.objects.Get(ctx, "sessions/"+k.ID+"/"+s.ID+".jsonl")
		if err != nil {
			return "", fmt.Errorf("the record of session %s: %w", s.ID, err)
		}
		from := ends[s.ID]
		from.Session, from.Commit = s.ID, commit
		if last := workingState(rec, from); last != nil {
			section("Its last session", []string{last.Prior.Heading + "\n\n" + last.Prior.Body})
			break
		}
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}

// cutAt is the session and the spend at which old card cut of plan p was
// cut. A direct card's switch set its counters to zero; its to_plan step
// kept them.
func (r *Runner) cutAt(ctx context.Context, p, cut Task) (session int, spent float64, err error) {
	if cut.ID != p.ID {
		return cut.Usage.Attempts, cut.SpentEUR, nil
	}
	tl, err := r.timeline.Objects.Get(ctx, "sessions/"+p.ID+"/timeline.jsonl")
	if err != nil {
		return 0, 0, fmt.Errorf("the timeline of %s: %w", p.ID, err)
	}
	sw := lastStep(tl, stepToPlan)
	n, _ := sw["session"].(float64)
	spent, _ = sw["spent"].(float64)
	return int(n), spent, nil
}
