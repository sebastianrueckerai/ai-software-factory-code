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
	if p.IssueNumber != 0 {
		repo, err := SplitRepo(p.Repository)
		if err != nil {
			return terminalError("%v", err)
		}
		closed, err := r.forgejo.IssueClosed(ctx, repo.Owner, repo.Name, p.IssueNumber)
		if err != nil {
			return err
		}
		if closed {
			if err := r.forgejo.ReopenIssue(ctx, repo.Owner, repo.Name, p.IssueNumber); err != nil {
				return err
			}
		}
	}
	mirrorTaskState(ctx, r.forgejo, p, "queued")
	return r.cfg.Workflows.Reopen(ctx, p.ID, "", TaskSignal{})
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
