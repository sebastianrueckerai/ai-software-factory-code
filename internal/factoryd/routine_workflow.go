package factoryd

import (
	"errors"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ActivityRoutineRun is the routine run's activity, and
// ActivityRoutineRunFailed the one that records the run as failed when it
// fails for good.
const (
	ActivityRoutineRun       = "RoutineRun"
	ActivityRoutineRunFailed = "RoutineRunFailed"
)

// RoutineRunInput is one run of a routine: the project's name ("" for the
// factory's own routines) and the routine's.
type RoutineRunInput struct {
	Project, Routine string
	Trigger          string    // cron, merges, configuration-version, push-main, run-now
	Due              time.Time // the scheduled time, or now for the others
}

// RoutineRunResult is what a run did: its outcome, a note, and the card
// it filed.
type RoutineRunResult struct {
	Outcome, Note, TaskID string
	Issue                 int64
}

// RoutineRunWorkflow is one run of a routine (roadmap 267): the schedule,
// the merge pass or Run now starts it, its activity files the card and
// writes the run, and the ID keyed on the due time makes a second start a
// no-op. An activity that fails after its retries, or for good, leaves the
// run's row failed with the error's words, and the workflow fails.
func RoutineRunWorkflow(ctx workflow.Context, in RoutineRunInput) error {
	// a schedule sets the time the run was due on the workflow it starts,
	// read untyped: the typed read drops a payload without type metadata
	if p, ok := workflow.GetInfo(ctx).SearchAttributes.GetIndexedFields()["TemporalScheduledStartTime"]; ok {
		if err := converter.GetDefaultDataConverter().FromPayload(p, &in.Due); err != nil {
			return terminalError("routine %s/%s: the scheduled start time: %v", in.Project, in.Routine, err)
		}
	}
	if in.Due.IsZero() {
		return terminalError("routine %s/%s: the run has no due time", in.Project, in.Routine)
	}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    5 * time.Minute,
		ScheduleToCloseTimeout: 24 * time.Hour,
		RetryPolicy:            &temporal.RetryPolicy{InitialInterval: 10 * time.Second, BackoffCoefficient: 2, MaximumInterval: 10 * time.Minute, NonRetryableErrorTypes: []string{errTerminal}},
	})
	err := workflow.ExecuteActivity(ctx, ActivityRoutineRun, in).Get(ctx, nil)
	if err == nil {
		return nil
	}
	fctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: time.Hour,
		RetryPolicy:            &temporal.RetryPolicy{InitialInterval: 10 * time.Second, BackoffCoefficient: 2, MaximumInterval: 10 * time.Minute},
	})
	if ferr := workflow.ExecuteActivity(fctx, ActivityRoutineRunFailed, in, failureWords(err)).Get(fctx, nil); ferr != nil {
		workflow.GetLogger(ctx).Error("routine run: record the failure", "project", in.Project, "routine", in.Routine, "error", ferr)
	}
	return err
}

// failureWords are an activity's failure as a run's note: the
// application error's message, else the error's text.
func failureWords(err error) string {
	if app := (*temporal.ApplicationError)(nil); errors.As(err, &app) {
		return app.Message()
	}
	return err.Error()
}

// routineRunPrefix is the workflow ID of a routine's runs without the due
// time: a schedule's action, to which Temporal appends the scheduled time.
// The joiner is the schedule IDs' "/", which no project or routine name
// holds, so no two routines share a prefix.
func routineRunPrefix(in RoutineRunInput) string { return "routine/" + in.Project + "/" + in.Routine }

// RoutineRunID is a run's workflow ID: Temporal refuses a second start for
// the same due time.
func RoutineRunID(in RoutineRunInput) string {
	return routineRunPrefix(in) + "/" + in.Due.UTC().Format("20060102T150405")
}
