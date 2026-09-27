package factoryd

import (
	"context"
	"errors"
	"log"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

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
	_, err := w.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:                    WorkflowID(taskID),
		TaskQueue:             TaskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}, "TaskWorkflow", taskID)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return nil
	}
	return err
}

// Signal delivers a signal to the task's running workflow. A task without
// one (finished, or never started) has nothing left to decide: no-op.
func (w TemporalWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	err := w.Client.SignalWorkflow(ctx, WorkflowID(taskID), "", name, sig)
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		log.Printf("task %s: signal %s: no running workflow", taskID, name)
		return nil
	}
	return err
}

// StartOpenWorkflows starts a workflow for every agent task not yet done or
// failed; main runs it at boot and every minute. Starting is idempotent, so
// this heals an intake that stored its task row but failed to start the
// workflow, and a task whose workflow died (in review, or mid-completion)
// before finishing it.
func StartOpenWorkflows(ctx context.Context, store Store, wf Workflows) {
	for _, status := range []string{"queued", "running", "in-review", "closing"} {
		tasks, err := store.ListTasksByStatus(ctx, status)
		if err != nil {
			log.Printf("start open workflows: list %s: %v", status, err)
			continue
		}
		for _, t := range tasks {
			if t.Kind == "human" {
				continue
			}
			if err := wf.Start(ctx, t.ID); err != nil {
				log.Printf("start open workflows: task %s: %v", t.ID, err)
			}
		}
	}
}
