package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// DeliveryRetention is how long a processed webhook delivery id is kept
// for redelivery dedupe.
const DeliveryRetention = 30 * 24 * time.Hour

// Workflows starts and signals task workflows and starts routines' runs.
// The intake paths depend on this seam, not on the Temporal client.
type Workflows interface {
	Start(ctx context.Context, taskID string) error
	Signal(ctx context.Context, taskID, name string, sig TaskSignal) error
	// Reopen starts a new run of the task's workflow after any end, with
	// signal when it is not "".
	Reopen(ctx context.Context, taskID, signal string, sig TaskSignal) error
	StartRoutineRun(ctx context.Context, in RoutineRunInput) error
}

// TemporalWorkflows is Workflows against a Temporal frontend.
type TemporalWorkflows struct {
	Client client.Client
}

// Start starts the task's workflow. The workflow id is the idempotency key:
// a start while it runs, or after it completed, is refused by Temporal and
// is not an error here. A workflow that failed, timed out or was
// terminated may be started again, so the heal pass can pick up its task
// (terminating a workflow therefore does not cancel a task: closing its
// issue does).
func (w TemporalWorkflows) Start(ctx context.Context, taskID string) error {
	_, err := w.Client.ExecuteWorkflow(ctx, startOptions(taskID), "TaskWorkflow", taskID)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil
	}
	if err == nil {
		log.Printf("task %s: workflow %s started", taskID, WorkflowID(taskID))
	}
	return err
}

// Signal delivers a signal to the task's workflow, starting it when no run
// exists (after a failure or termination), so a signal is never lost; the
// new run reconciles from Forgejo before it acts. A completed workflow has
// nothing left to decide: no-op.
func (w TemporalWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	_, err := w.Client.SignalWithStartWorkflow(ctx, WorkflowID(taskID), name, sig, startOptions(taskID), "TaskWorkflow", taskID)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		log.Printf("task %s: signal %s: workflow already completed", taskID, name)
		return nil
	}
	if err == nil {
		log.Printf("task %s: signal %s from %s delivered", taskID, name, sig.Author)
	}
	return err
}

// Reopen starts a new run of the task's workflow, whatever ended the last
// one: a plan's replan, or a direct card's that already ended. With
// signal, the run starts with it (SignalWithStartWorkflow), and a run
// that runs takes the signal; without, a run that runs is not an error.
func (w TemporalWorkflows) Reopen(ctx context.Context, taskID, signal string, sig TaskSignal) error {
	opts := startOptions(taskID)
	opts.WorkflowIDReusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE
	var err error
	if signal != "" {
		_, err = w.Client.SignalWithStartWorkflow(ctx, WorkflowID(taskID), signal, sig, opts, "TaskWorkflow", taskID)
	} else {
		_, err = w.Client.ExecuteWorkflow(ctx, opts, "TaskWorkflow", taskID)
	}
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil
	}
	if err == nil {
		log.Printf("task %s: workflow %s reopened", taskID, WorkflowID(taskID))
	}
	return err
}

// StartRoutineRun starts a routine's run. A start for a due time whose run
// completed or still runs is refused by Temporal and is not an error here.
// One whose workflow failed starts, but the failed run's row is final: the
// restart returns its outcome. Run now is the human's retry.
func (w TemporalWorkflows) StartRoutineRun(ctx context.Context, in RoutineRunInput) error {
	_, err := w.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                                       RoutineRunID(in),
		TaskQueue:                                TaskQueue,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, "RoutineRunWorkflow", in)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil
	}
	return err
}

// SignalGate hands a machine gate's trip to the workflow running it.
func (w TemporalWorkflows) SignalGate(ctx context.Context, workflowID string, trip GateTrip) error {
	return w.Client.SignalWorkflow(ctx, workflowID, "", SignalGateBound, trip)
}

// LastFailure is the last failure of the workflow's pending activity run
// activityID, "" when it has none or is not pending.
func (w TemporalWorkflows) LastFailure(ctx context.Context, workflowID, activityID string) (string, error) {
	res, err := w.Client.DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		return "", err
	}
	for _, a := range res.GetPendingActivities() {
		if a.GetActivityId() == activityID {
			return a.GetLastFailure().GetMessage(), nil
		}
	}
	return "", nil
}

// Alive: the task's workflow is running now. A workflow that is missing,
// completed, failed or terminated is not; a Temporal error is an error.
func (w TemporalWorkflows) Alive(ctx context.Context, taskID string) (bool, error) {
	status, err := w.runStatus(ctx, taskID)
	return status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, err
}

// Completed: the task's last workflow run completed. One that is missing,
// running, failed or terminated did not; a Temporal error is an error.
func (w TemporalWorkflows) Completed(ctx context.Context, taskID string) (bool, error) {
	status, err := w.runStatus(ctx, taskID)
	return status == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, err
}

// runStatus is the status of the task's last workflow run, unspecified
// when it has none.
func (w TemporalWorkflows) runStatus(ctx context.Context, taskID string) (enumspb.WorkflowExecutionStatus, error) {
	res, err := w.Client.DescribeWorkflowExecution(ctx, WorkflowID(taskID), "")
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, nil
	}
	if err != nil {
		return enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, err
	}
	return res.GetWorkflowExecutionInfo().GetStatus(), nil
}

func startOptions(taskID string) client.StartWorkflowOptions {
	return client.StartWorkflowOptions{
		ID:                    WorkflowID(taskID),
		TaskQueue:             TaskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		// without this the SDK hands back the running execution as a success
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}
}

// HealPass starts a workflow for every agent task not yet done or
// failed, except a queued card under the work bound: those start through
// the admission only, one pull per project below in filing order. Main runs it
// at boot and every minute. Starting is idempotent, so
// this heals an intake that stored its task row but failed to start the
// workflow, and a task whose workflow died (in review, or mid-completion)
// before finishing it. For a task waiting on Forgejo (in review or on a
// human) it also re-reads the pull request, issue and comments through
// in.Resync, so a webhook lost while factoryd was down still reaches the
// workflow, and takes in the agent and human issues no webhook turned
// into a task (in.ResyncIssues), and finishes the factory closes it did
// not confirm (in.HealFactoryCloses). It wakes every merge queue's head still
// waiting for its turn, so a lost queue_turn costs at most a pass. A
// project's chat task runs no workflow: the pass leaves it be, and
// neither restarts a merged card, whose workflow ended at its merge: the
// chain poller ends it. An open card whose last run completed (completed)
// gets a new run through Reopen, which Start would refuse: a reopen that
// moved the row but did not start the run.
//
// It also forgets webhook deliveries older than DeliveryRetention.
func HealPass(ctx context.Context, store Store, wf Workflows, in *Intake, completion *Completion, completed func(ctx context.Context, taskID string) (bool, error)) {
	if err := store.PruneDeliveries(ctx, DeliveryRetention); err != nil {
		log.Printf("heal pass: prune deliveries: %v", err)
	}
	if err := in.ResyncIssues(ctx); err != nil {
		log.Printf("heal pass: issues: %v", err)
	}
	if err := in.HealFactoryCloses(ctx); err != nil {
		log.Printf("heal pass: factory closes: %v", err)
	}
	// pulls are the projects with queued cards under the work bound: their
	// oldest queued cards start below, once per project, the sweep that
	// re-derives what a missed pull would have started
	pulls := map[string]bool{}
	for _, status := range []string{"queued", "waiting-deps", "running", "in-review", "waiting-human", "waiting-poster", "waiting-image", "closing"} {
		tasks, err := store.ListTasksByStatus(ctx, status)
		if err != nil {
			log.Printf("start open workflows: list %s: %v", status, err)
			continue
		}
		for _, t := range tasks {
			if t.Kind == KindChat {
				continue
			}
			if t.Kind == "human" {
				// no workflow: only a close to take in
				if status == "waiting-human" {
					if err := in.Resync(ctx, t); err != nil {
						log.Printf("heal pass: resync task %s: %v", t.ID, err)
					}
				}
				continue
			}
			// a task no workflow can run (its repository is not owner/name)
			// is completed failed here like any failure (credentials
			// revoked, keys blocked, the label mirrored where the repository
			// allows), or it would be restarted every pass
			if _, err := SplitRepo(t.Repository); err != nil {
				log.Printf("heal pass: task %s failed: %v", t.ID, err)
				if err := completion.Complete(ctx, t.ID, "failed"); err != nil {
					log.Printf("heal pass: fail task %s: %v", t.ID, err)
					continue
				}
				taskEnded(ctx, in.Timeline, store, in.Forgejo, wf, t, "failed", err.Error())
				continue
			}
			if status == "queued" {
				if moved, err := in.humanLaneWithoutAgents(ctx, t); err != nil || moved {
					if err != nil {
						log.Printf("heal pass: task %s: %v", t.ID, err)
					}
					continue
				}
			}
			if !reopenCompleted(ctx, store, wf, completed, t) {
				if status == "queued" && !t.Unbound() {
					pulls[t.Repository] = true
					continue
				}
				if err := wf.Start(ctx, t.ID); err != nil {
					log.Printf("start open workflows: task %s: %v", t.ID, err)
					continue
				}
			}
			if status == "waiting-deps" || status == "in-review" || status == "waiting-human" || status == "waiting-poster" || status == "waiting-image" {
				if err := in.Resync(ctx, t); err != nil {
					log.Printf("heal pass: resync task %s: %v", t.ID, err)
				}
			}
			if status == "waiting-deps" {
				healDependencyWait(ctx, store, wf, t)
			}
		}
	}
	for repository := range pulls {
		pullQueued(ctx, store, wf, repository)
	}
	signalQueueHeads(ctx, store, wf)
}

// reopenCompleted starts a new run of open card t when its last run
// completed, and tells whether that run completed: Start would refuse it.
// The row is read again after the check, since a run that just ended its
// card leaves it final or merged: a card that moved is left to the next
// pass.
func reopenCompleted(ctx context.Context, store Store, wf Workflows, completed func(context.Context, string) (bool, error), t Task) bool {
	done, err := completed(ctx, t.ID)
	if err != nil {
		log.Printf("heal pass: task %s: its run: %v", t.ID, err)
		return false
	}
	if !done {
		return false
	}
	now, ok, err := store.Task(ctx, t.ID)
	switch {
	case err != nil:
		log.Printf("heal pass: task %s: %v", t.ID, err)
	case ok && now.Status == t.Status:
		if err := wf.Reopen(ctx, t.ID, "", TaskSignal{}); err != nil {
			log.Printf("heal pass: reopen task %s: %v", t.ID, err)
		}
	}
	return true
}

// StaleWindow is how long a live card may stay quiet — no event, no
// timeline step — before the console's ring wears it and the stale sweep
// acts on it.
const StaleWindow = 30 * time.Minute

// StaleCards is what the stale sweep reads and writes: the cards of its
// statuses, their newest events, and the escalation's records.
type StaleCards interface {
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	InsertAlert(ctx context.Context, a Alert) (int64, error)
}

// StaleSweep acts on the cards the console's ring names, beside the heal
// pass (which restarts what died but cannot know a restarted run goes on
// reporting nothing): a card of a live status (running, waiting-deps,
// waiting-poster — never the queue, whose wait owes no work) quiet for the
// stale window is nudged — SignalWithStart starts a workflow that died —
// and one still quiet a window after its nudge is escalated to a human
// like any escalation: waiting-human, the question on the card, an event
// row, an alert. A card whose workflow Temporal reports running (alive)
// is never nudged or escalated, in any state, since a quiet wait is by
// design. nudged is the sweep's memory of its signals (task id → when),
// owned by the caller's loop; a card that reports again is forgotten and
// starts over.
func StaleSweep(ctx context.Context, store StaleCards, wf Workflows, in *Intake, alive func(ctx context.Context, taskID string) (bool, error), nudged map[string]time.Time) {
	var objects ObjectStore
	if in != nil && in.Timeline != nil {
		objects = in.Timeline.Objects
	}
	for _, status := range []string{"running", "waiting-deps", "waiting-poster"} {
		tasks, err := store.ListTasksByStatus(ctx, status)
		if err != nil {
			log.Printf("stale sweep: list %s: %v", status, err)
			continue
		}
		ids := make([]string, 0, len(tasks))
		for _, t := range tasks {
			ids = append(ids, t.ID)
		}
		newest, err := store.NewestTaskEvents(ctx, ids)
		if err != nil {
			log.Printf("stale sweep: newest events of %s: %v", status, err)
			newest = map[string]Event{}
		}
		for _, t := range tasks {
			if t.Kind == KindChat || t.Kind == "human" {
				continue
			}
			at := t.CreatedAt
			if e, ok := newest[t.ID]; ok && e.At.After(at) {
				at = e.At
			}
			if stepAt := newestStepAt(ctx, objects, t.ID); stepAt.After(at) {
				at = stepAt
			}
			quiet := time.Since(at)
			if quiet >= StaleWindow {
				up, err := alive(ctx, t.ID)
				if err != nil {
					log.Printf("stale sweep: is the workflow of task %s alive: %v", t.ID, err)
					continue
				}
				if up {
					delete(nudged, t.ID)
					continue
				}
			}
			nudge, ok := nudged[t.ID]
			switch {
			case quiet < StaleWindow:
				// it reported: the ring comes off and the rungs start over
				delete(nudged, t.ID)
			case !ok:
				state, _ := staleNames(t.Status)
				sig := TaskSignal{Author: FactoryDecider, Body: fmt.Sprintf("quiet for %s while %s", staleDuration(quiet), state)}
				if err := wf.Signal(ctx, t.ID, SignalStaleNudge, sig); err != nil {
					log.Printf("stale sweep: nudge task %s: %v", t.ID, err)
					continue
				}
				nudged[t.ID] = time.Now()
			case time.Since(nudge) < StaleWindow:
				// the nudge's grace: the workflow has a window to answer
			default:
				if err := escalateStale(ctx, store, in, t, quiet); err != nil {
					log.Printf("stale sweep: escalate task %s: %v", t.ID, err)
					continue
				}
				delete(nudged, t.ID)
			}
		}
	}
}

// staleNames are the sweep's words for a status: how the card spends its
// wait, and what a human can do about it.
func staleNames(status string) (state, action string) {
	switch status {
	case "waiting-deps":
		return "waiting on its dependencies", "answer on the issues of the cards it waits on, or close this issue to end the wait"
	case "waiting-poster":
		return "waiting on the issue's author", "the issue's author answers on the issue, or close it to end the wait"
	}
	return "running", "answer on the issue to start a new attempt with your comment as guidance, or close the issue to end the card"
}

// staleDuration is a quiet span in the ring's words.
func staleDuration(d time.Duration) string {
	m := int(d.Minutes())
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

// staleEscalation is the stale sweep's record of its escalation: the wait
// it ended, in the timeline step's keys.
type staleEscalation struct {
	Time   time.Time `json:"time"`
	State  string    `json:"state"`
	Quiet  string    `json:"quiet"`
	Action string    `json:"action"`
}

// escalateStale moves a still-quiet card to waiting-human the way the
// runner's Escalate does: the state mirrored on the issue, the reason as
// a comment whose first line is the question the card shows, the intake
// event the answers reconcile from, a question_asked row and an
// escalation alert. Its errors are the intake event and the transition:
// the rest is logged, never a reason to stop half-way.
func escalateStale(ctx context.Context, store StaleCards, in *Intake, t Task, quiet time.Duration) error {
	state, action := staleNames(t.Status)
	reason := fmt.Sprintf("The card was quiet for %s while %s: is its run gone?\nWhy: nothing reached its timeline or events for that long, and a nudge signal went unanswered.\nRecommendation: %s.",
		staleDuration(quiet), state, action)
	var forgejo *Forgejo
	var timeline *Timeline
	if in != nil {
		forgejo, timeline = in.Forgejo, in.Timeline
	}
	next := "Reply on this issue to start a new attempt with your comment as guidance, or close the issue to end the task."
	var comment int64
	if forgejo != nil && t.IssueNumber != 0 {
		if repo, err := SplitRepo(t.Repository); err != nil {
			log.Printf("task %s: the stale escalation's comment: %v", t.ID, err)
		} else if comment, err = forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber,
			"factoryd: task "+t.ID+" needs a human.\n\n"+fenced(reason)+"\n\n"+next); err != nil {
			log.Printf("task %s: the stale escalation's comment: %v", t.ID, err)
			comment = 0
		}
	}
	if timeline != nil {
		detail := staleDetail(time.Now().UTC(), state, quiet, action)
		detail["reason"], detail["comment"] = reason, comment
		if err := timeline.Append(ctx, t.ID, "escalated", detail); err != nil {
			log.Printf("task %s: timeline escalated: %v", t.ID, err)
		}
	}
	shaped := ShapedQuestion(reason, QuestionFacts{PlanTrack: t.Track == policy.TrackPlan}, nil)
	// the intake row is the ask's durable anchor, written before the wait:
	// a card in waiting-human without its row has no ask
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeEscalation, Author: FactoryDecider, Body: reason, CommentID: comment,
		Question: shaped}); err != nil {
		return fmt.Errorf("task %s: record the stale escalation: %w", t.ID, err)
	}
	if err := store.TransitionTask(ctx, t.ID, []string{"running", "waiting-deps", "waiting-poster"}, "waiting-human"); err != nil {
		return fmt.Errorf("task %s is %s, cannot wait for a human: %w", t.ID, t.Status, err)
	}
	mirrorTaskState(ctx, forgejo, t, "waiting-human")
	if in != nil {
		pullQueued(ctx, in.Store, in.Workflows, t.Repository)
	}
	asked := map[string]any{"question": textutil.Truncate(escalationQuestion(reason), 300), "comment": comment}
	if shaped != nil {
		asked["question"], asked["ask"], asked["why"], asked["info"] = shaped.Ask, shaped.Ask, shaped.Why, shaped.Info
		asked["choices"] = shaped.Labels()
		if shaped.Recommendation != "" {
			asked["recommendation"] = shaped.Recommendation
		}
	}
	if timeline != nil {
		timeline.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaFactoryd, asked)
	}
	if _, err := store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: FactoryDecider, TaskID: t.ID,
		Detail: map[string]any{"comment": comment, "reason": textutil.Truncate(reason, 2000)}}); err != nil {
		log.Printf("task %s: record the stale escalation's alert: %v", t.ID, err)
	}
	return nil
}

// staleDetail is the sweep's record of its escalation as a timeline
// step's detail keys.
func staleDetail(at time.Time, state string, quiet time.Duration, action string) map[string]any {
	b, err := json.Marshal(staleEscalation{Time: at, State: state, Quiet: staleDuration(quiet), Action: action})
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// newestStepAt is taskID's newest timeline step's time, the card's own
// work as the sweep reads it; an unreadable timeline, or one whose lines
// carry no time, leaves the zero time.
func newestStepAt(ctx context.Context, objects ObjectStore, taskID string) time.Time {
	if objects == nil {
		return time.Time{}
	}
	body, err := objects.Get(ctx, "sessions/"+taskID+"/timeline.jsonl")
	if err != nil {
		log.Printf("stale sweep: timeline of %s: %v", taskID, err)
		return time.Time{}
	}
	lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if len(lines[i]) == 0 || json.Unmarshal(lines[i], &m) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, fmt.Sprint(m["time"]))
		if err != nil {
			continue // a line without a time cannot feed the ring
		}
		return at
	}
	return time.Time{}
}
