package factoryd

import (
	"context"
	"fmt"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// StepSizing is the timeline step of a card's sizing: the size it goes on
// with, the filer's and the sizing's, the reason and whether it goes to
// the planner (plan).
const StepSizing = "sizing"

// SizeResult is SizeCard's answer: ToPlan is the switch of a card that is
// not small, nil when the card stays direct.
type SizeResult struct{ ToPlan *PlanSwitch }

// sizedSwitch sends a card of size, which is not small, to the planner
// with the files the sizing expects the change to touch.
func sizedSwitch(size, reason string, files []string) *PlanSwitch {
	sw := &PlanSwitch{Reason: size + ", not small: " + reason}
	if len(files) > 0 {
		lines := []string{"The sizing step expects the change to touch these files:"}
		for _, f := range files {
			lines = append(lines, "- "+f)
		}
		sw.Work = strings.Join(lines, "\n")
	}
	return sw
}

// SizeCard sizes card taskID before its first session. A card that
// carries a pull request is not sized: its work exists. A filer's medium
// or large goes to the planner without a session. Otherwise the sizing
// report task sizes it; the filer's small, or none, is a floor no size
// goes under. A sized card that holds its filing's budget gets its new
// size's. A sizing with no size keeps the card as filed. The sizing step
// is written last, so a retry that finds it answers from it.
func (r *Runner) SizeCard(ctx context.Context, taskID string) (SizeResult, error) {
	defer beatUntilDone(ctx, "sizing")()
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return SizeResult{}, err
	}
	tl, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return SizeResult{}, fmt.Errorf("the timeline of %s: %w", taskID, err)
	}
	if step := lastStep(tl, StepSizing); step != nil {
		if plan, _ := step["plan"].(bool); !plan {
			return SizeResult{}, nil
		}
		size, _ := step["size"].(string)
		reason, _ := step["reason"].(string)
		var files []string
		listed, _ := step["files"].([]any)
		for _, f := range listed {
			if f, ok := f.(string); ok {
				files = append(files, f)
			}
		}
		return SizeResult{ToPlan: sizedSwitch(size, reason, files)}, nil
	}
	record := func(detail map[string]any) error {
		if err := r.timeline.Append(ctx, taskID, StepSizing, gateEnd(t, GateSizing, detail)); err != nil {
			return fmt.Errorf("record %s: %w", StepSizing, err)
		}
		return nil
	}
	if t.CarryPR > 0 {
		return SizeResult{}, record(map[string]any{"skipped": fmt.Sprintf("the card carries pull request #%d, whose work exists", t.CarryPR)})
	}
	if t.Size == "medium" || t.Size == "large" {
		reason := "its filer sized it " + t.Size
		if err := record(map[string]any{"size": t.Size, "filed": t.Size, "reason": reason, "plan": true}); err != nil {
			return SizeResult{}, err
		}
		return SizeResult{ToPlan: sizedSwitch(t.Size, reason, nil)}, nil
	}
	child, err := r.reportOf(ctx, t, SourceSizing)
	if err != nil {
		return SizeResult{}, err
	}
	var v sizeVerdict
	ok, reason := false, fmt.Sprintf("the sizing task %s ended %s", child.ID, child.Status)
	if child.Status == "done" {
		if v, ok = parseSizing(child.AgentSummary); !ok {
			reason = "the sizing's answer is no size: " + textutil.Truncate(strings.TrimSpace(child.AgentSummary), maxSizeReason)
		}
	}
	if !ok {
		return SizeResult{}, record(map[string]any{"size": t.Size, "filed": t.Size, "reason": reason, "report": child.ID, "plan": false})
	}
	p, _, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return SizeResult{}, err
	}
	b, strong := r.cfg.Policy().Document.Budget, t.Rung == RungFrontier
	_, eur := askedBudget(t.Objective)
	budget := t.BudgetEUR
	if Cents(t.BudgetEUR) == Cents(policy.CardBudget(b, p.Budget, t.Size, strong, eur)) {
		budget = policy.CardBudget(b, p.Budget, v.Size, strong, eur)
	}
	detail := map[string]any{"size": v.Size, "filed": t.Size, "sized": v.Size, "reason": v.Reason, "files": v.Files, "report": child.ID, "plan": v.Size != "small"}
	if Cents(budget) != Cents(t.BudgetEUR) {
		detail["budget_eur"] = budget
	}
	if v.Size != t.Size || Cents(budget) != Cents(t.BudgetEUR) {
		if err := r.store.SetTaskSize(ctx, taskID, v.Size, budget); err != nil {
			return SizeResult{}, err
		}
	}
	if err := record(detail); err != nil {
		return SizeResult{}, err
	}
	if v.Size == "small" {
		return SizeResult{}, nil
	}
	return SizeResult{ToPlan: sizedSwitch(v.Size, v.Reason, v.Files)}, nil
}
