package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
// on the board), and not closed.
func (r *Runner) planReplannable(ctx context.Context, p Task) (bool, error) {
	switch p.Status {
	case "done":
	case "failed":
		if _, ok, err := r.store.PlanRevision(ctx, p.ID, 0); err != nil || !ok {
			return false, err
		}
	default:
		return false, nil
	}
	closed, err := r.store.PlanClosed(ctx, p.ID)
	return !closed, err
}

// reopenPlan sends plan p back to its planner: queued with its counters at
// zero, money for a session, its issue open and labelled queued, and a
// new run of its workflow. Each step is safe to repeat.
func (r *Runner) reopenPlan(ctx context.Context, p Task) error {
	if err := r.store.TransitionTask(ctx, p.ID, []string{"done", "failed", "queued"}, "queued"); err != nil {
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
	replannable, err := r.planReplannable(ctx, p)
	if err != nil {
		return ReplanChildResult{}, err
	}
	if !replannable && !slices.Contains(replanRuns, p.Status) {
		status := p.Status
		closed, err := r.store.PlanClosed(ctx, p.ID)
		if err != nil {
			return ReplanChildResult{}, err
		}
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
	if err := r.moveDependents(ctx, k, p); err != nil {
		return ReplanChildResult{}, err
	}
	join := ReplanChildResult{Reason: fmt.Sprintf("stopped for the replan of plan #%d", p.IssueNumber)}
	if !replannable {
		// a queued plan may be a reopen that failed before its run started
		if p.Status == "queued" {
			if err := r.reopenPlan(ctx, p); err != nil {
				return ReplanChildResult{}, err
			}
		}
		return join, nil
	}
	newest, _, err := r.store.PlanRevision(ctx, p.ID, 0)
	if err != nil {
		return ReplanChildResult{}, err
	}
	wrote, err := r.timeline.AppendUnlessPending(ctx, p.ID, stepReplan, map[string]any{"origin": ReplanCut, "card": k.ID, "work": work,
		"stops": []string{k.ID}, "revision": newest.Revision}, replanClosers...)
	if err != nil {
		return ReplanChildResult{}, fmt.Errorf("record %s: %w", stepReplan, err)
	}
	if err := r.reopenPlan(ctx, p); err != nil {
		return ReplanChildResult{}, err
	}
	if !wrote {
		return join, nil
	}
	return ReplanChildResult{Reason: fmt.Sprintf("cut: plan #%d replans its work", p.IssueNumber)}, nil
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
// with none hands its dependents to p and ends closed with why.
func (r *Runner) stopForReplan(ctx context.Context, k, p Task, why string) error {
	err := r.EscalateSession(ctx, k.ID, AgentAsk{Kind: AskReplan, Reason: why})
	if !errors.Is(err, ErrNoLiveSession) {
		return err
	}
	if err := r.moveDependents(ctx, k, p); err != nil {
		return err
	}
	return r.Finish(ctx, k.ID, "closed", why)
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
	return t.Kind == "agent" && !t.Unbound() && (t.Source == SourcePlan && t.Parent != "" || t.Track == policy.TrackPlan ||
		planTrack && t.MergeCommit == "" && t.Status != "done" && t.Status != "merged")
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
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	switch {
	case t.Kind != "agent":
		return "", ErrNoReplan(card + " is a person's card" + cannotPlan + "file the work as an issue labelled plan.")
	case t.Unbound():
		return "", ErrNoReplan(card + " is a " + t.Source + " card" + cannotPlan + "let it finish or close it.")
	case t.Source == SourcePlan && t.Parent != "" || t.Track == policy.TrackPlan:
		return r.replanPlan(ctx, GroupOf(t), ask)
	}
	return r.replanDirect(ctx, t, ask)
}

// replanPlan sends plan planID back to its planner: the replan step, the
// plan reopened, then each started open card stopped, so the waits they
// hand over land on a plan that is open again.
func (r *Runner) replanPlan(ctx context.Context, planID string, ask ReplanAsk) (string, error) {
	p, _, err := r.task(ctx, planID)
	if err != nil {
		return "", err
	}
	plan := fmt.Sprintf("Plan #%d", p.IssueNumber)
	if p.Status == "closed" {
		return "", ErrNoReplan(plan + " is closed: a person closed it. File a new card or plan for the change.")
	}
	pending := ErrNoReplan(plan + " is being replanned: wait for its revision.")
	_, ok, err := r.pendingReplan(ctx, p)
	if err != nil {
		return "", err
	}
	if ok {
		// heals a reopen that failed after its step, or a run that never
		// started; a run that runs answers the bare reopen as a no-op
		if slices.Contains([]string{"done", "failed", "queued"}, p.Status) {
			err = r.reopenPlan(ctx, p)
		} else {
			err = r.cfg.Workflows.Reopen(ctx, p.ID, "", TaskSignal{})
		}
		if err != nil {
			return "", err
		}
		return "", pending
	}
	closed, err := r.store.PlanClosed(ctx, p.ID)
	if err != nil {
		return "", err
	}
	if closed {
		return "", ErrNoReplan(plan + " is closed: its criteria are met. File a new card or plan for the change.")
	}
	replannable, err := r.planReplannable(ctx, p)
	if err != nil {
		return "", err
	}
	if !replannable {
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
	// the started open cards; a merged card's work is on main already
	var stops []Task
	var ids []string
	for _, k := range cards {
		if k.PlanRevision > 0 && !Final(k.Status) && k.Status != "merged" {
			stops, ids = append(stops, k), append(ids, k.ID)
		}
	}
	wrote, err := r.timeline.AppendUnlessPending(ctx, p.ID, stepReplan, map[string]any{"origin": ReplanPerson, "by": ask.By, "comment": ask.Comment,
		"stops": ids, "revision": newest.Revision}, replanClosers...)
	if err != nil {
		return "", fmt.Errorf("record %s: %w", stepReplan, err)
	}
	if !wrote {
		return "", pending
	}
	if err := r.reopenPlan(ctx, p); err != nil {
		return "", err
	}
	why := fmt.Sprintf("stopped for the replan of plan #%d", p.IssueNumber)
	for _, k := range stops {
		if err := r.stopForReplan(ctx, k, p, why); err != nil {
			return "", err
		}
	}
	switch len(stops) {
	case 0:
		return plan + " goes back to the planner: no card stops.", nil
	case 1:
		return plan + " goes back to the planner: 1 card stops, and its work stays on its branch.", nil
	}
	return fmt.Sprintf("%s goes back to the planner: %d cards stop, and their work stays on their branches.", plan, len(stops)), nil
}

// replanDirect sends direct card t to the planner. The person's words go
// first, as the to_plan signal, which the workflow takes at its next step
// and records as the card's replan step; then a running session is asked
// to stop. A failed or closed card is queued again for a new run.
func (r *Runner) replanDirect(ctx context.Context, t Task, ask ReplanAsk) (string, error) {
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	if t.MergeCommit != "" || t.Status == "done" || t.Status == "merged" {
		return "", ErrNoReplan(card + " merged, so its work is on main: file a new card for what is still missing.")
	}
	project, _, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return "", err
	}
	if _, planTrack := trackClass(project, policy.TrackPlan); !planTrack {
		return "", ErrNoReplan(card + "'s project follows no process with a plan track" + cannotPlan + "split the work into smaller cards.")
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
	case err == nil:
		return card + " stops its session and goes to the planner.", nil
	case errors.Is(err, ErrNoLiveSession):
		return card + " goes to the planner at its next step.", nil
	default:
		return "", err
	}
}
