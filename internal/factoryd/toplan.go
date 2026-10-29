package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// PlanSwitch asks for a direct card's work to go to the planner: Reason is
// one line for the card's record, Work the caller's text for the planner.
type PlanSwitch struct{ Reason, Work string }

// and is sw with more's reason and work after its own.
func (sw PlanSwitch) and(more PlanSwitch) PlanSwitch {
	join := func(sep, a, b string) string {
		if a == "" || b == "" {
			return a + b
		}
		return a + sep + b
	}
	return PlanSwitch{Reason: join("; ", sw.Reason, more.Reason), Work: join("\n", sw.Work, more.Work)}
}

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

// BecomePlan turns direct card taskID plan-track. The row moves first, to
// the plan track with the plan class and without its pull request, and
// nothing is written before it: a refusal leaves the card as it was. Then
// each attempt that finds the card plan-track completes what is missing:
// the to_plan step with the planner's block, the session counters at zero
// (the plan loop has caps of its own), the open pull request closed with
// its branch kept, its merge approvals denied, the card out of the merge
// queue and its image park, money for the planner's session, the issue
// labelled plan in place of agent. Its only caller is the direct
// lifecycle, so a card found plan-track is one an earlier attempt moved.
func (r *Runner) BecomePlan(ctx context.Context, taskID string, sw PlanSwitch) (PlanSwitchResult, error) {
	t, repo, err := r.task(ctx, taskID)
	if err != nil {
		return PlanSwitchResult{}, err
	}
	var work string
	var pr int64 // the pull request the switch took off the row
	if t.Track != policy.TrackPlan {
		p, _, err := r.store.ProjectByRepository(ctx, t.Repository)
		if err != nil {
			return PlanSwitchResult{}, err
		}
		class, planTrack := trackClass(p, policy.TrackPlan)
		if why := planRefusal(t, planTrack); why != "" {
			return PlanSwitchResult{Refused: why}, nil
		}
		if work, err = r.workSoFar(ctx, taskID, sw); err != nil {
			return PlanSwitchResult{}, err
		}
		switched, err := r.store.SwitchToPlan(ctx, taskID, []string{class})
		if err != nil {
			return PlanSwitchResult{}, err
		}
		if !switched {
			if t, _, err = r.task(ctx, taskID); err != nil {
				return PlanSwitchResult{}, err
			}
			if t.Track != policy.TrackPlan {
				return PlanSwitchResult{Refused: cmp.Or(planRefusal(t, planTrack), statusRefusal(t))}, nil
			}
		} else {
			pr = t.PRNumber
		}
	}
	tl, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return PlanSwitchResult{}, fmt.Errorf("the timeline of %s: %w", taskID, err)
	}
	if lastStep(tl, stepToPlan) == nil {
		if work == "" {
			if work, err = r.workSoFar(ctx, taskID, sw); err != nil {
				return PlanSwitchResult{}, err
			}
		}
		detail := map[string]any{"reason": sw.Reason, "work_so_far": work}
		if pr != 0 {
			detail["pr"] = pr
		}
		if _, err := r.timeline.AppendOnce(ctx, taskID, stepToPlan, detail); err != nil {
			return PlanSwitchResult{}, fmt.Errorf("record %s: %w", stepToPlan, err)
		}
	}
	if err := r.store.SetTaskUsage(ctx, taskID, TaskUsage{}); err != nil {
		return PlanSwitchResult{}, err
	}
	note := fmt.Sprintf("factoryd: card #%d went back to the planner; its branch stays.", t.IssueNumber)
	if err := cleanup(taskID, "close its pull request", r.closeTaskPullRequest(ctx, repo, t, note)); err != nil {
		return PlanSwitchResult{}, err
	}
	if err := r.store.ResolveApprovals(ctx, taskID, ApprovalMerge, "denied", FactoryDecider, note); err != nil {
		return PlanSwitchResult{}, err
	}
	if err := r.leaveQueue(ctx, taskID, toPlanner); err != nil {
		return PlanSwitchResult{}, err
	}
	if p, ok, err := r.store.ImagePark(ctx, taskID); err != nil {
		return PlanSwitchResult{}, err
	} else if ok && p.ToolTask != "" {
		if err := cleanup(taskID, "leave its image park", r.LeaveImagePark(ctx, taskID, p.ToolTask)); err != nil {
			return PlanSwitchResult{}, err
		}
	}
	if err := r.fundPlanner(ctx, t); err != nil {
		return PlanSwitchResult{}, err
	}
	if t.IssueNumber != 0 {
		if err := r.relabelPlan(ctx, repo, t); err != nil {
			return PlanSwitchResult{}, err
		}
	}
	return PlanSwitchResult{Switched: true}, nil
}

// SupersedeApproval denies card taskID's pending merge approval with
// reason: a replan of the card in review supersedes it, refused or not.
func (r *Runner) SupersedeApproval(ctx context.Context, taskID, reason string) error {
	return r.store.ResolveApprovals(ctx, taskID, ApprovalMerge, "denied", FactoryDecider, reason)
}

// relabelPlan labels card t's issue plan in place of agent. The label only
// mirrors the row, so Forgejo's refusal is logged and other errors retry.
func (r *Runner) relabelPlan(ctx context.Context, repo Repo, t Task) error {
	labels, err := r.forgejo.IssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber)
	if err != nil {
		return cleanup(t.ID, "read the issue's labels", err)
	}
	if slices.Contains(labels, LabelAgent) || !slices.Contains(labels, LabelPlan) {
		next := []string{LabelPlan}
		for _, l := range labels {
			if l != LabelAgent && l != LabelPlan {
				next = append(next, l)
			}
		}
		if err := r.forgejo.ReplaceIssueLabels(ctx, repo.Owner, repo.Name, t.IssueNumber, next); err != nil {
			return cleanup(t.ID, "relabel the issue plan", err)
		}
	}
	return nil
}

// workSoFar is the planner's input block of card taskID: the head of its
// branch past the base, the caller's text and the reason for the switch.
func (r *Runner) workSoFar(ctx context.Context, taskID string, sw PlanSwitch) (string, error) {
	head, err := r.BranchHead(ctx, taskID)
	if err != nil {
		return "", err
	}
	work := "No commit past the base.\n"
	if head != "" {
		work = "Branch head: " + head + "\n"
	}
	return work + sw.Work + "\nReason for the switch: " + sw.Reason, nil
}

// planRefusal is why card t may not turn plan-track, "" when it may;
// planTrack is whether its project's process has a plan track.
func planRefusal(t Task, planTrack bool) string {
	card := fmt.Sprintf("Card #%d", t.IssueNumber)
	switch {
	case t.Kind != "agent":
		return card + " is a person's card" + cannotPlan + "file the work as an issue labelled plan."
	case t.Track != policy.TrackDirect:
		return fmt.Sprintf("%s is on track %q, not the direct track%sfile its work as a direct card first.", card, t.Track, cannotPlan)
	case t.Source == SourcePlan:
		return card + " belongs to a plan" + cannotPlan + "revise the plan instead."
	case t.Unbound():
		return card + " is a " + t.Source + " card" + cannotPlan + "let it finish or close it."
	case t.MergeCommit != "":
		return card + " merged, so its work is on main: file a new card for what is still missing."
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
