package factoryd

import (
	"context"
	"fmt"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// PlanSwitch asks for a direct card's work to go to the planner: Reason is
// one line for the card's record, Work the caller's text for the planner.
type PlanSwitch struct{ Reason, Work string }

// PlanSwitchResult is BecomePlan's answer: the card is plan-track
// (Switched), or Refused names its state and the fitting action.
type PlanSwitchResult struct {
	Switched bool
	Refused  string
}

// stepToPlan is the timeline step of a card's switch to the plan track;
// its work_so_far is the planner's input block (RunPlan).
const stepToPlan = "to_plan"

// workSoFarFinding opens the finding that carries a switched card's
// work_so_far to its planner.
const workSoFarFinding = "work so far on this card, which ran as a direct card before it came to you:\n"

// switchableStatuses are the statuses a direct card may turn plan-track in.
var switchableStatuses = []string{"queued", "waiting-deps", "running"}

// BecomePlan turns direct card taskID plan-track: the to_plan step with
// the planner's block, the card's session counters at zero (the plan loop
// has caps of its own), its issue labelled plan in place of agent, then
// the row on the plan track with the plan class. The row moves last: an
// attempt that stopped before it is repeated whole, one after it answers
// switched.
func (r *Runner) BecomePlan(ctx context.Context, taskID string, sw PlanSwitch) (PlanSwitchResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanSwitchResult{}, err
	}
	if t.Track == policy.TrackPlan {
		tl, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
		if err != nil {
			return PlanSwitchResult{}, fmt.Errorf("the timeline of %s: %w", taskID, err)
		}
		if lastStep(tl, stepToPlan) != nil {
			return PlanSwitchResult{Switched: true}, nil
		}
	}
	p, _, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return PlanSwitchResult{}, err
	}
	class, planTrack := trackClass(p, policy.TrackPlan)
	if why := planRefusal(t, planTrack); why != "" {
		return PlanSwitchResult{Refused: why}, nil
	}
	head, err := r.BranchHead(ctx, taskID)
	if err != nil {
		return PlanSwitchResult{}, err
	}
	work := "No commit past the base.\n"
	if head != "" {
		work = "Branch head: " + head + "\n"
	}
	work += sw.Work + "\nReason for the switch: " + sw.Reason
	// the planner reads this step: a lost append fails the attempt, not the planner's input
	if err := r.timeline.Append(ctx, taskID, stepToPlan, map[string]any{"reason": sw.Reason, "work_so_far": work}); err != nil {
		return PlanSwitchResult{}, fmt.Errorf("record %s: %w", stepToPlan, err)
	}
	if err := r.store.SetTaskUsage(ctx, taskID, TaskUsage{}); err != nil {
		return PlanSwitchResult{}, err
	}
	if t.IssueNumber != 0 {
		labels, err := r.forgejo.IssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber)
		if err != nil {
			return PlanSwitchResult{}, err
		}
		next := []string{LabelPlan}
		for _, l := range labels {
			if l != LabelAgent && l != LabelPlan {
				next = append(next, l)
			}
		}
		if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber, next); err != nil {
			return PlanSwitchResult{}, err
		}
	}
	switched, err := r.store.SwitchToPlan(ctx, taskID, []string{class})
	if err != nil || switched {
		return PlanSwitchResult{Switched: switched}, err
	}
	if t, _, err = r.task(ctx, taskID); err != nil {
		return PlanSwitchResult{}, err
	}
	if t.Track == policy.TrackPlan {
		return PlanSwitchResult{Switched: true}, nil
	}
	return PlanSwitchResult{Refused: statusRefusal(t)}, nil
}

// planRefusal is why card t may not turn plan-track, "" when it may;
// planTrack is whether its project's process has a plan track.
func planRefusal(t Task, planTrack bool) string {
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	switch {
	case t.Kind != "agent":
		return card + " is a person's card" + cannotPlan + "file the work as an issue labelled plan."
	case t.Track == policy.TrackPlan:
		return card + " is on the plan track already, so the planner has its work: wait for its plan."
	case t.Source == SourcePlan:
		return card + " belongs to a plan" + cannotPlan + "revise the plan instead."
	case t.Unbound():
		return card + " is a " + t.Source + " card" + cannotPlan + "let it finish or close it."
	case t.PRNumber != 0:
		return card + " has an open pull request" + cannotPlan + "close the pull request first."
	case !slices.Contains(switchableStatuses, t.Status):
		return statusRefusal(t)
	case !planTrack:
		return card + "'s project follows no process with a plan track" + cannotPlan + "split the work into smaller cards."
	}
	return ""
}

const cannotPlan = ", so its work cannot go back to the planner: "

// statusRefusal refuses card t, whose status allows no switch.
func statusRefusal(t Task) string {
	return fmt.Sprintf("Card #%d is %s%sswitch it while it is queued, waiting on its dependencies or running.", t.IssueNumber, t.Status, cannotPlan)
}
