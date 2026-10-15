package factoryd

import (
	"context"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ActivityRoutineRun is the routine run's one activity.
const ActivityRoutineRun = "RoutineRun"

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

// scheduledStart is the search attribute a Temporal schedule sets on the
// workflow it starts: the time the run was due.
var scheduledStart = temporal.NewSearchAttributeKeyTime("TemporalScheduledStartTime")

// RoutineRunWorkflow is one run of a routine (roadmap 267): the schedule, the merge pass or Run now starts it,
// its one activity files the card and writes the run, and the ID keyed on the due time makes a second start a no-op.
func RoutineRunWorkflow(ctx workflow.Context, in RoutineRunInput) error {
	if due, ok := workflow.GetTypedSearchAttributes(ctx).GetTime(scheduledStart); ok {
		in.Due = due
	}
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    5 * time.Minute,
		ScheduleToCloseTimeout: 24 * time.Hour,
		RetryPolicy:            &temporal.RetryPolicy{InitialInterval: 10 * time.Second, BackoffCoefficient: 2, MaximumInterval: 10 * time.Minute, NonRetryableErrorTypes: []string{errTerminal}},
	})
	return workflow.ExecuteActivity(ctx, ActivityRoutineRun, in).Get(ctx, nil)
}

// routineRunPrefix is the workflow ID of a routine's runs without the due
// time: a schedule's action, to which Temporal appends the scheduled time.
func routineRunPrefix(in RoutineRunInput) string { return "routine-" + in.Project + "-" + in.Routine }

// RoutineRunID is a run's workflow ID: Temporal refuses a second start for
// the same due time.
func RoutineRunID(in RoutineRunInput) string {
	return routineRunPrefix(in) + "-" + in.Due.UTC().Format("20060102T1504")
}

// RoutineRun is a routine run's activity. It files nothing yet.
func (r *Runner) RoutineRun(ctx context.Context, in RoutineRunInput) (RoutineRunResult, error) {
	return RoutineRunResult{Outcome: "stub"}, nil
}
