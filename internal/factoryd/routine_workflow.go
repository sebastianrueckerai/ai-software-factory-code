package factoryd

import (
	"time"

	"go.temporal.io/sdk/converter"
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

// RoutineRunWorkflow is one run of a routine (roadmap 267): the schedule, the merge pass or Run now starts it,
// its one activity files the card and writes the run, and the ID keyed on the due time makes a second start a no-op.
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
	return workflow.ExecuteActivity(ctx, ActivityRoutineRun, in).Get(ctx, nil)
}

// routineRunPrefix is the workflow ID of a routine's runs without the due
// time: a schedule's action, to which Temporal appends the scheduled time.
// The joiner is the schedule IDs' "/", which no project or routine name
// holds, so no two routines share a prefix.
func routineRunPrefix(in RoutineRunInput) string { return "routine/" + in.Project + "/" + in.Routine }

// RoutineRunID is a run's workflow ID: Temporal refuses a second start for
// the same due time.
func RoutineRunID(in RoutineRunInput) string {
	return routineRunPrefix(in) + "/" + in.Due.UTC().Format("20060102T1504")
}
