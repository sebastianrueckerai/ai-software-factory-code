package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
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
	// closed stops exactly the roadmap pull, every other filing still files.
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
	// EventCompacted is the harness's own record that it compacted the
	// running session's conversation: detail {"session"}. The bus carries
	// no token figures, so the row is the audit fact alone.
	EventCompacted = "compacted"
	// EventApprovalRequested is factoryd's record of an approval it opened
	// for a card, a merge's or a plan's (roadmap 239): detail {"approval",
	// "kind", and "pr" and "class" for a merge, "revision" and "cards"
	// for a plan}.
	EventApprovalRequested = "approval_requested"
	// EventChatThreadDeleted records a member's hard delete of a chat
	// thread (roadmap 239): the actor is the member, detail {"thread",
	// "messages"}, the thread's number and the rows that went.
	EventChatThreadDeleted = "chat_thread_deleted"
	// EventChatStopped records a member's stop of a chat turn in flight:
	// the actor is the member, detail {"thread"}, the stopped turn's
	// thread.
	EventChatStopped = "chat_stopped"
	// EventCriterionDecided records an approver's decision on a plan's
	// criteria (the spec-intake concept, pieces 7 and 8): detail
	// {"approval", "criteria", "verb"} for a grant (granted) or a
	// confirmation (met), {"approval", "verb"} for a denial (denied), or
	// {"approval", "criterion", "verb", "reason"} for one row: met,
	// not_met or waived.
	EventCriterionDecided = "criterion_decided"
	// EventPlanClosed records a plan's close (the spec-intake concept,
	// piece 8): every criterion met or waived, or, for a plan without
	// criteria, every card done or closed. Detail {"criteria"}, the plan's
	// count, with "backfill": true on a close the schema wrote for a plan
	// that ended before the second rule. A quiet record, no poke.
	EventPlanClosed = "plan_closed"
	// EventCriteriaGateRun records a run of the criteria gate on a done
	// plan whose criteria held no verdict row, on the plan task: detail
	// {"task", "pr", "commit", "outcome", "attempt"}, the outcome ran, cut
	// no_room or started (written before the run), the attempt "1" or "2" (a cut run runs once more). A
	// quiet record, no poke.
	EventCriteriaGateRun = "criteria_gate_run"
	// EventProductionReleased records the end of a project's production
	// hold (roadmap 336), a row of the project: detail {"how", "held_since",
	// "hours"}, how closed when no plan is open on main any more (by
	// factoryd), deploy_now when an approver chose Deploy now (by them, with
	// "commit" and "decider"). A quiet record, no poke.
	EventProductionReleased = "production_released"
	// EventPoolChanged records a project's escalation pool money set or
	// topped up, escalation.pool.eur of its project file: detail {"action",
	// "confirmed_eur", "from_eur", "to_eur", "reason", "pr"}, action set
	// or top_up and pr the pull request that landed it, namespaced by the
	// configuration repository. The set_pool action that
	// writes it takes action, eur and reason; the server owns the
	// baseline — from_eur and to_eur are the pool in force and after,
	// confirmed_eur the amount the member confirmed, a
	// correction of a confirmation carries only the amount — and it
	// reaches the policy through the proposal routes as every write does.
	EventPoolChanged = "pool_changed"
	// EventProofsFiled records one file_proofs call of a plan's prover, on
	// the plan task: detail {"prover", "filing", "proofs": [{"criterion",
	// "path", "suite", "name", "outcome"}], "refusals"}, the outcome red,
	// green, unrunnable or invalid; a hold-out's entry is {"criterion":
	// "holdout-<idx>", "outcome"} alone, and a refusal names it so; an entry
	// that names no criterion is {"outcome"} alone. Never a test's code.
	EventProofsFiled = "proofs_filed"
	// EventProofsStored records the dry run of the last filing on main and
	// the rows it stored, on the plan task: detail {"prover", "commit",
	// "stored": [ids], "flagged": [{"criterion", "flag"}]}, a hold-out by
	// holdoutEntry.
	EventProofsStored = "proofs_stored"
	// EventProofOpened records a stored proof the gate opened, on the gated
	// card and on the plan task: its blind try did not pass, or wrote no
	// artifact even where its test passed, or its dry run flagged it and it
	// had no try. Detail {"criterion", "head", "outcome" (a proof* word,
	// "no artifact", or "flagged"), "message" (the output the builder got,
	// or the flag), "path", "suite", "name", "code", and "holdout", the
	// claim with its inputs and expected numbers, for a hold-out}. The test
	// is public from here on.
	EventProofOpened = "proof_opened"
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

// hasEvent tells whether taskID's timeline holds event on a line that
// holds every value of match; a nil match takes any line of the event. An
// unreadable timeline reads as no.
func (tl *Timeline) hasEvent(ctx context.Context, taskID, event string, match map[string]string) bool {
	body, err := tl.Objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) != nil || e["step"] != StepEvent || e["event"] != event {
			continue
		}
		held := true
		for k, v := range match {
			held = held && e[k] == v
		}
		if held {
			return true
		}
	}
	return false
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

// RecordFact writes a fact payload's row (the metrics contracts): its
// IdentityKey makes it one row per fact, whichever retry records it
// first. A failed write is logged — a fact never fails the decision it
// records, and the decision is never retried for the fact's sake.
func (tl *Timeline) RecordFact(ctx context.Context, e Event) {
	if tl == nil || tl.Events == nil {
		return
	}
	if _, err := tl.Events.Record(ctx, e); err != nil {
		log.Printf("task %s: event %s: the fact row: %v", e.TaskID, e.Kind, err)
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
// to turn done and the plan's newest revision leaves no part to a later
// round, the plan records all_children_done and its issue says so.
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
	switch later, err := leavesRounds(ctx, store, t.Parent); {
	case err != nil:
		log.Printf("task %s: the newest revision of plan %s: %v", t.ID, t.Parent, err)
		return
	case later:
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
