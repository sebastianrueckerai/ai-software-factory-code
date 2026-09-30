package factoryd

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
)

// The events (stage11-design.md section 2): the fixed list a later triggers
// block reads. Only card_entered_lane and task_ended drive behaviour.
const (
	EventCardCreated       = "card_created"
	EventCardEnteredLane   = "card_entered_lane"
	EventTaskEnded         = "task_ended"
	EventPullRequestMerged = "pull_request_merged"
	EventAllChildrenDone   = "all_children_done"
)

// StepEvent is the timeline step every event is recorded under.
const StepEvent = "event"

// recordEvent appends event with detail to taskID's timeline as one line
// of step event; a nil tl records nothing. A failed append is logged: an
// event never fails the step it records.
func (tl *Timeline) recordEvent(ctx context.Context, taskID, event string, detail map[string]any) {
	if tl == nil {
		return
	}
	entry := map[string]any{"event": event}
	maps.Copy(entry, detail)
	if err := tl.Append(ctx, taskID, StepEvent, entry); err != nil {
		log.Printf("task %s: event %s: %v", taskID, event, err)
	}
}

// enterLane records card t's entry into its lane by user by and starts
// what the lane starts: a queued card's workflow, or a backlog card's
// write-up (writeUp, nil where the caller opens none). A task with no lane
// (a roll) records nothing and only starts.
func enterLane(ctx context.Context, tl *Timeline, wf Workflows, t Task, by string, writeUp func() error) error {
	if t.Lane != "" {
		tl.recordEvent(ctx, t.ID, EventCardEnteredLane, map[string]any{"lane": t.Lane, "by": by})
	}
	switch {
	case t.Status == "queued":
		return wf.Start(ctx, t.ID)
	case t.Status == "backlog" && writeUp != nil:
		return writeUp()
	}
	return nil
}

// taskEnded records task t's end in state final, the hook the cards that
// depend on it wait for; when t is the last of a plan's cards to turn done,
// the plan records all_children_done and its issue says so.
func taskEnded(ctx context.Context, tl *Timeline, store Store, forgejo *Forgejo, t Task, final string) {
	tl.recordEvent(ctx, t.ID, EventTaskEnded, map[string]any{"state": final})
	if t.Source != SourcePlan || t.Parent == "" || final != "done" {
		return
	}
	cards, err := store.PlanCards(ctx, t.Parent)
	if err != nil {
		log.Printf("task %s: the cards of plan %s: %v", t.ID, t.Parent, err)
		return
	}
	if slices.ContainsFunc(cards, func(c Task) bool { return c.Status != "done" }) {
		return
	}
	tl.recordEvent(ctx, t.Parent, EventAllChildrenDone, map[string]any{"cards": len(cards)})
	plan, ok, err := store.Task(ctx, t.Parent)
	if err != nil || !ok || plan.IssueNumber == 0 {
		log.Printf("task %s: plan %s has no issue to tell (%v)", t.ID, t.Parent, err)
		return
	}
	repo, err := SplitRepo(plan.Repository)
	if err == nil {
		_, err = forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, plan.IssueNumber, fmt.Sprintf("factoryd: all %d cards of this plan are done.", len(cards)))
	}
	if err != nil {
		log.Printf("task %s: comment all_children_done on plan %s: %v", t.ID, t.Parent, err)
	}
}
