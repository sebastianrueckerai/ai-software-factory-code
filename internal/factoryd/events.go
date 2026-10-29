package factoryd

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"sync"
)

// The events (stage11-design.md section 2): the fixed list a later triggers
// block reads. Only task_ended drives behaviour.
const (
	// EventAutonomyChanged records a project's autonomy level or queue
	// cap change (roadmap 239): detail {"from", "to", "queue_cap"}.
	EventAutonomyChanged = "autonomy_changed"
	// EventIntakeChanged records a project's intake switch (the chat
	// header's): detail {"from", "to"} with the words open and closed —
	// closed refuses the wake's own filings, the member's ask still files.
	EventIntakeChanged     = "intake_changed"
	EventCardCreated       = "card_created"
	EventTaskEnded         = "task_ended"
	EventPullRequestMerged = "pull_request_merged"
	EventAllChildrenDone   = "all_children_done"
	// the model tier's measurements (roadmap 222, item 5): an answered
	// session's start with its model and attempt, its push with the minutes since
	// the start, and a switch of the running session's model
	EventSessionStarted = "session_started"
	EventSessionPushed  = "session_pushed"
	EventModelSwitched  = "model_switched"
	// EventAcceptancePassed is factoryd's record of a session of a plan's
	// card that ran one of the plan's acceptance commands and it exited 0
	// (roadmap 208): detail {"command", "exit", "session"}. Only green
	// runs are recorded; the trail is the card's evidence.
	EventAcceptancePassed = "acceptance_passed"
	// EventApprovalRequested is factoryd's record of an approval it opened
	// for a card, a merge's or a plan's (roadmap 239): detail {"approval",
	// "kind", and "pr" and "class" for a merge, "revision" and "cards"
	// for a plan}.
	EventApprovalRequested = "approval_requested"
	// EventChatThreadDeleted records a member's hard delete of a chat
	// thread (roadmap 239): the actor is the member, detail {"thread",
	// "messages"}, the thread's number and the rows that went.
	EventChatThreadDeleted = "chat_thread_deleted"
	// EventCriterionDecided records an approver's grant or waiver on a
	// plan's criterion (the spec-intake concept, pieces 7 and 8): detail
	// {"approval", "criteria", "verb"} or {"criterion", "verdict"}.
	EventCriterionDecided = "criterion_decided"
	// EventPlanClosed records a plan's close (the spec-intake concept,
	// piece 8): every criterion met or waived, or, for a plan without
	// criteria, every card done or closed. Detail {"criteria"}, the plan's
	// count, with "backfill": true on a close the schema wrote for a plan
	// that ended before the second rule. A quiet record, no poke.
	EventPlanClosed = "plan_closed"
)

// StepEvent is the timeline step every event is recorded under.
const StepEvent = "event"

// recordEvent appends event with detail to taskID's timeline as one line
// of step event and writes the same event as a row of events, via
// factoryd; a nil tl records nothing. A failed append is logged: an event
// never fails the step it records.
func (tl *Timeline) recordEvent(ctx context.Context, taskID, event string, detail map[string]any) {
	if tl == nil {
		return
	}
	entry := map[string]any{"event": event}
	maps.Copy(entry, detail)
	if err := tl.Append(ctx, taskID, StepEvent, entry); err != nil {
		log.Printf("task %s: event %s: %v", taskID, event, err)
	}
	tl.RecordRow(ctx, taskID, event, FactoryDecider, ViaFactoryd, detail)
}

// RecordRow writes event as an events row: the task's repository and issue
// come from its row, and actor and via say who did it and how it reached
// the factory (factoryd's own events pass FactoryDecider and ViaFactoryd,
// intake an answer's author and ViaWebhook). A Timeline with no recorder or
// store, or a task that cannot be read, leaves no row (logged).
func (tl *Timeline) RecordRow(ctx context.Context, taskID, event, actor, via string, detail map[string]any) {
	if tl == nil || tl.Events == nil || tl.Store == nil {
		return
	}
	t, ok, err := tl.Store.Task(ctx, taskID)
	if err != nil {
		log.Printf("task %s: event %s: the task: %v", taskID, event, err)
		return
	}
	if !ok {
		log.Printf("task %s: event %s: no such task", taskID, event)
		return
	}
	e := Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Kind: event,
		Actor: actor, Via: via, Detail: detail}
	if _, err := tl.Events.Record(ctx, e); err != nil {
		log.Printf("task %s: event %s: the row: %v", taskID, event, err)
	}
}

// RecordProjectRow writes e as a project's own events row, which no task
// carries — an outside push to a document repository (the 2026-10-15
// plan, item 6). e's repository, actor and via stand as given; a Timeline
// with no recorder writes none (a failure is logged).
func (tl *Timeline) RecordProjectRow(ctx context.Context, e Event) {
	if tl == nil || tl.Events == nil {
		return
	}
	if _, err := tl.Events.Record(ctx, e); err != nil {
		log.Printf("event %s: the row: %v", e.Kind, err)
	}
}

// allDoneMu holds taskEnded's check and record of all_children_done
// together: the last two cards ending side by side both find every card
// done (one factoryd replica).
var allDoneMu sync.Mutex

// taskEnded records task t's end in state final, with why when one is
// told (a failure's reason as its issue comment carries it, which its
// poke tells), ends the run of a routine's card and wakes the cards
// waiting on it (wf nil wakes none). When t is the last of a plan's cards
// to turn done, the plan records all_children_done and its issue says so.
func taskEnded(ctx context.Context, tl *Timeline, store Store, forgejo *Forgejo, wf Workflows, t Task, final, why string) {
	detail := map[string]any{"state": final}
	if why != "" {
		detail["reason"] = why
	}
	tl.recordEvent(ctx, t.ID, EventTaskEnded, detail)
	if t.Source == SourceRoutine {
		endRoutineRun(ctx, store, t, final)
	}
	signalDependents(ctx, store, wf, t.ID)
	// the freed slot goes to the oldest queued card of the same project
	pullQueued(ctx, store, wf, t.Repository)
	if t.Source != SourcePlan || t.Parent == "" || final != "done" {
		return
	}
	allDoneMu.Lock()
	defer allDoneMu.Unlock()
	if tl != nil {
		if b, err := tl.Objects.Get(ctx, "sessions/"+t.Parent+"/timeline.jsonl"); err != nil || bytes.Contains(b, []byte(`"event":"`+EventAllChildrenDone+`"`)) {
			if err != nil {
				log.Printf("task %s: timeline of plan %s: %v", t.ID, t.Parent, err)
			}
			return
		}
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
