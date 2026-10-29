package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"

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
// (0: none).
type ReplanStep struct {
	Origin   string   `json:"origin"`
	By       string   `json:"by,omitempty"`
	Comment  string   `json:"comment,omitempty"`
	Card     string   `json:"card,omitempty"`
	Work     string   `json:"work,omitempty"`
	Stops    []string `json:"stops,omitempty"`
	Revision int      `json:"revision"`
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

// fundPlanner tops card t up to the default card budget left for the
// planner's session; a card with that much left gets nothing.
func (r *Runner) fundPlanner(ctx context.Context, t Task) error {
	v := r.cfg.Policy()
	_, project, _ := v.ProjectOf(t.Repository)
	short := Cents(policy.CardBudget(v.Document.Budget, project.Budget, "", false, 0)) - (Cents(t.BudgetEUR) - Cents(t.SpentEUR))
	if short <= 0 {
		return nil
	}
	_, _, err := r.store.FundCard(ctx, t.ID, float64(short)/100, nil, Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber,
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
	if err := r.fundPlanner(ctx, p); err != nil {
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
	work = sanitize.Inbound(work)
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
// else Open the stopped cards not yet final; By and Comment are the
// person's name and note of their replan.
type ReplanWait struct {
	Stopped     bool
	Open        []string
	By, Comment string
}

// ReplanStopped tells whether the cards plan planID's pending replan stops
// have ended, so its planner may run on what they left.
func (r *Runner) ReplanStopped(ctx context.Context, planID string) (ReplanWait, error) {
	p, _, err := r.task(ctx, planID)
	if err != nil {
		return ReplanWait{}, err
	}
	step, pending, err := r.pendingReplan(ctx, p)
	if err != nil {
		return ReplanWait{}, err
	}
	if !pending {
		return ReplanWait{Stopped: true}, nil
	}
	var w ReplanWait
	if step.Origin == ReplanPerson {
		w.By, w.Comment = step.By, step.Comment
	}
	for _, id := range step.Stops {
		k, ok, err := r.store.Task(ctx, id)
		if err != nil {
			return ReplanWait{}, err
		}
		if ok && !Final(k.Status) {
			w.Open = append(w.Open, id)
		}
	}
	w.Stopped = len(w.Open) == 0
	return w, nil
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
	ask.Comment = textutil.Truncate(sanitize.Inbound(ask.Comment), maxHandoverMessage)
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
// to stop. A failed or closed card is queued again for a new run.
func (r *Runner) replanDirect(ctx context.Context, t Task, ask ReplanAsk) (string, error) {
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	switch {
	case t.MergeCommit != "" || t.Status == "done" || t.Status == "merged":
		return "", ErrNoReplan(mergedRefusal(t))
	case t.Status == "closing":
		return "", ErrNoReplan(card + " is closing: replan it once it has ended.")
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
