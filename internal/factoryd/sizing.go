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

// sizedSources are the sources of the cards a person or the chat files.
var sizedSources = []string{"webhook", "api"}

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
// carries a pull request is not sized: its work exists. A writer's filed
// medium or large goes to the planner without a session; an outsider's
// filed size is no floor. Otherwise the sizing report task sizes it, and
// a sizing with no size keeps the card as filed. The sizing sets the size,
// and the budget of its size when the card still holds its filing's: a
// raise keeps its value. The sizing step is recorded once and first; the
// write follows from it, and a call that finds the step completes that
// write and answers from it.
func (r *Runner) SizeCard(ctx context.Context, taskID string) (SizeResult, error) {
	defer beatUntilDone(ctx, "sizing")()
	step, err := r.sizingStep(ctx, taskID)
	if err != nil {
		return SizeResult{}, err
	}
	if step == nil {
		if step, err = r.recordSizing(ctx, taskID); err != nil {
			return SizeResult{}, err
		}
	}
	return r.sized(ctx, taskID, step)
}

// recordSizing sizes card taskID and records its sizing step, unless a
// parallel attempt recorded one first; it answers the step recorded.
func (r *Runner) recordSizing(ctx context.Context, taskID string) (map[string]any, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return nil, err
	}
	detail, err := r.sizing(ctx, t)
	if err != nil {
		return nil, err
	}
	if _, err := r.timeline.AppendOnce(ctx, taskID, StepSizing, detail); err != nil {
		return nil, fmt.Errorf("record %s: %w", StepSizing, err)
	}
	step, err := r.sizingStep(ctx, taskID)
	if err == nil && step == nil {
		err = fmt.Errorf("the timeline of %s holds no %s step after its record", taskID, StepSizing)
	}
	return step, err
}

// sizingStep is card taskID's sizing step, nil before it is recorded.
func (r *Runner) sizingStep(ctx context.Context, taskID string) (map[string]any, error) {
	tl, err := r.objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return nil, fmt.Errorf("the timeline of %s: %w", taskID, err)
	}
	return lastStep(tl, StepSizing), nil
}

// sizing is card t's sizing step, its gate's end included. Only a step
// whose session answered a size carries it as sized. A sized step that
// changes the card's size carries held_cents, the budget the card holds,
// and budget_eur, the budget the write sets: the sized size's when the
// card asked no budget and holds its filing's, else the one it holds.
func (r *Runner) sizing(ctx context.Context, t Task) (map[string]any, error) {
	if t.CarryPR > 0 {
		return gateEnd(t, GateSizing, map[string]any{"skipped": fmt.Sprintf("the card carries pull request #%d, whose work exists", t.CarryPR)}), nil
	}
	if t.AuthorWriter && (t.Size == "medium" || t.Size == "large") {
		return gateEnd(t, GateSizing, map[string]any{"size": t.Size, "filed": t.Size, "reason": "its filer sized it " + t.Size, "plan": true}), nil
	}
	child, err := r.reportOf(ctx, t, SourceSizing)
	if err != nil {
		return nil, err
	}
	var v sizeVerdict
	ok, reason := false, fmt.Sprintf("the sizing task %s ended %s", child.ID, child.Status)
	if child.Status == "done" {
		if v, ok = parseSizing(child.AgentSummary); !ok {
			reason = "the sizing's answer is no size: " + textutil.Truncate(strings.TrimSpace(child.AgentSummary), maxSizeReason)
		}
	}
	if !ok {
		return gateEnd(t, GateSizing, map[string]any{"size": t.Size, "filed": t.Size, "reason": reason, "report": child.ID, "plan": false}), nil
	}
	detail := map[string]any{"size": v.Size, "filed": t.Size, "sized": v.Size, "reason": v.Reason, "files": v.Files,
		"report": child.ID, "plan": v.Size != "small"}
	if v.Size != t.Size {
		p, _, err := r.store.ProjectByRepository(ctx, t.Repository)
		if err != nil {
			return nil, err
		}
		b, strong := r.cfg.Policy().Document.Budget, t.Rung == RungFrontier
		budget := t.BudgetEUR
		if _, asked := askedBudget(t.Objective); asked == 0 && Cents(budget) == Cents(policy.CardBudget(b, p.Budget, t.Size, strong, 0)) {
			budget = policy.CardBudget(b, p.Budget, v.Size, strong, 0)
		}
		detail["held_cents"], detail["budget_eur"] = Cents(t.BudgetEUR), budget
	}
	return gateEnd(t, GateSizing, detail), nil
}

// sized completes the write of card taskID's sizing step, when the step
// carries one, and answers from the step. A budget that moved since the
// step read it stays.
func (r *Runner) sized(ctx context.Context, taskID string, step map[string]any) (SizeResult, error) {
	if held, ok := step["held_cents"].(float64); ok {
		sized, _ := step["sized"].(string)
		budget, _ := step["budget_eur"].(float64)
		if err := r.store.SetTaskSizeBudget(ctx, taskID, sized, int64(held), budget); err != nil {
			return SizeResult{}, err
		}
	}
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
