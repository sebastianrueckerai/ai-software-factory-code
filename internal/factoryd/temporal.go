package factoryd

import (
	"context"
	"errors"
	"log"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// DeliveryRetention is how long a processed webhook delivery id is kept
// for redelivery dedupe.
const DeliveryRetention = 30 * 24 * time.Hour

// Workflows starts and signals task workflows; the intake paths depend on
// this seam, not on the Temporal client.
type Workflows interface {
	Start(ctx context.Context, taskID string) error
	Signal(ctx context.Context, taskID, name string, sig TaskSignal) error
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
// failed; main runs it at boot and every minute. Starting is idempotent, so
// this heals an intake that stored its task row but failed to start the
// workflow, and a task whose workflow died (in review, or mid-completion)
// before finishing it. For a task waiting on Forgejo (in review or on a
// human) it also re-reads the pull request, issue and comments through
// in.Resync, so a webhook lost while factoryd was down still reaches the
// workflow.
//
// It also forgets webhook deliveries older than DeliveryRetention.
func HealPass(ctx context.Context, store Store, wf Workflows, in *Intake) {
	if err := store.PruneDeliveries(ctx, DeliveryRetention); err != nil {
		log.Printf("heal pass: prune deliveries: %v", err)
	}
	for _, status := range []string{"queued", "running", "in-review", "waiting-human", "closing"} {
		tasks, err := store.ListTasksByStatus(ctx, status)
		if err != nil {
			log.Printf("start open workflows: list %s: %v", status, err)
			continue
		}
		for _, t := range tasks {
			if t.Kind == "human" {
				continue
			}
			// a task no workflow can run (its repository is not owner/name)
			// is failed here, or it would be restarted every pass
			if _, err := SplitRepo(t.Repository); err != nil {
				log.Printf("start open workflows: task %s failed: %v", t.ID, err)
				if err := store.TransitionTask(ctx, t.ID, []string{status}, "failed"); err != nil {
					log.Printf("start open workflows: fail task %s: %v", t.ID, err)
				}
				continue
			}
			if err := wf.Start(ctx, t.ID); err != nil {
				log.Printf("start open workflows: task %s: %v", t.ID, err)
				continue
			}
			if status == "in-review" || status == "waiting-human" {
				if err := in.Resync(ctx, t); err != nil {
					log.Printf("heal pass: resync task %s: %v", t.ID, err)
				}
			}
		}
	}
}
