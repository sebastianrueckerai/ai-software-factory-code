package factoryd

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestRoutineRunWorkflowCallsTheRunOnce(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(RoutineRunWorkflow, workflow.RegisterOptions{Name: "RoutineRunWorkflow"})
	calls := 0
	env.RegisterActivityWithOptions(func(ctx context.Context, in RoutineRunInput) (RoutineRunResult, error) {
		calls++
		return RoutineRunResult{Outcome: "filed"}, nil
	}, activity.RegisterOptions{Name: "RoutineRun"})
	in := RoutineRunInput{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "cron", Due: time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)}
	env.ExecuteWorkflow("RoutineRunWorkflow", in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil || calls != 1 {
		t.Fatalf("completed %v err %v calls %d", env.IsWorkflowCompleted(), env.GetWorkflowError(), calls)
	}
	if got := RoutineRunID(in); got != "routine/ai-software-factory-code/weekly-bump/20261013T040000" {
		t.Fatalf("id %q", got)
	}
	// two Run nows in one minute are two runs, as the store keys them
	in.Due = in.Due.Add(30 * time.Second)
	if got := RoutineRunID(in); got != "routine/ai-software-factory-code/weekly-bump/20261013T040030" {
		t.Fatalf("id %q, want the seconds", got)
	}
}

// A run whose activity fails for good leaves its row failed, with the
// error's words as its note, and the workflow fails: a terminal error
// before the run began gets the run a row, a run that filed its card keeps
// it.
func TestRoutineRunWorkflowRecordsAFailedRun(t *testing.T) {
	r, store, _, _ := routineRunner(t, routineTree(t))
	run := func(in RoutineRunInput, failure error) error {
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		env.RegisterWorkflowWithOptions(RoutineRunWorkflow, workflow.RegisterOptions{Name: "RoutineRunWorkflow"})
		env.RegisterActivityWithOptions(func(context.Context, RoutineRunInput) (RoutineRunResult, error) {
			return RoutineRunResult{}, failure
		}, activity.RegisterOptions{Name: ActivityRoutineRun})
		env.RegisterActivityWithOptions(r.RoutineRunFailed, activity.RegisterOptions{Name: ActivityRoutineRunFailed})
		env.ExecuteWorkflow("RoutineRunWorkflow", in)
		return env.GetWorkflowError()
	}
	in := RoutineRunInput{Project: codeProject, Routine: "weekly-bump", Trigger: "cron", Due: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)}
	var app *temporal.ApplicationError
	if err := run(in, terminalError("routine %s/%s: no project %s in the store", in.Project, in.Routine, in.Project)); !errors.As(err, &app) {
		t.Fatalf("workflow error %v, want the activity's", err)
	}
	runs := store.routineRuns[codeProject+"/weekly-bump"]
	if len(runs) != 1 || runs[0].Run != 1 || runs[0].Outcome != "failed" || runs[0].Ended.IsZero() || !runs[0].Due.Equal(in.Due) ||
		runs[0].Note != "routine ai-software-factory-code/weekly-bump: no project ai-software-factory-code in the store" {
		t.Fatalf("runs %+v, want run 1 failed with the error's words", runs)
	}
	if ran := recordedEvents(r, EventRoutineRan); len(ran) != 1 || ran[0].Detail["outcome"] != "failed" || ran[0].Detail["poke"] != nil {
		t.Fatalf("routine_ran %+v, want one failed and no poke", ran)
	}

	filed := in
	filed.Due = in.Due.Add(7 * 24 * time.Hour)
	began, _, _ := store.BeginRoutineRun(context.Background(), RoutineRun{Project: codeProject, Routine: "weekly-bump", Due: filed.Due, Trigger: "cron"}, true)
	if err := store.RoutineRunFiled(context.Background(), codeProject, "weekly-bump", began.Run, "t-71", 71); err != nil {
		t.Fatal(err)
	}
	if err := run(filed, terminalError("forgejo: 502")); err == nil {
		t.Fatal("the workflow did not fail")
	}
	if got := store.routineRuns[codeProject+"/weekly-bump"][1]; got.Outcome != "filed" || got.TaskID != "t-71" || !got.Ended.IsZero() {
		t.Fatalf("the filed run %+v, want it open with its card", got)
	}
}

// routineRunEnv runs RoutineRunWorkflow and records the inputs its
// activity receives.
func routineRunEnv() (*testsuite.TestWorkflowEnvironment, *[]RoutineRunInput) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(RoutineRunWorkflow, workflow.RegisterOptions{Name: "RoutineRunWorkflow"})
	var runs []RoutineRunInput
	env.RegisterActivityWithOptions(func(ctx context.Context, in RoutineRunInput) (RoutineRunResult, error) {
		runs = append(runs, in)
		return RoutineRunResult{Outcome: "filed"}, nil
	}, activity.RegisterOptions{Name: "RoutineRun"})
	return env, &runs
}

// A schedule's run is due at the time the schedule set on it, not at the
// time its input carries, whether or not the attribute names its type.
func TestRoutineRunWorkflowTakesTheScheduledTime(t *testing.T) {
	scheduled := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	for name, set := range map[string]func(env *testsuite.TestWorkflowEnvironment) error{
		"typed": func(env *testsuite.TestWorkflowEnvironment) error {
			return env.SetTypedSearchAttributesOnStart(temporal.NewSearchAttributes(
				temporal.NewSearchAttributeKeyTime("TemporalScheduledStartTime").ValueSet(scheduled)))
		},
		"without type metadata": func(env *testsuite.TestWorkflowEnvironment) error {
			return env.SetSearchAttributesOnStart(map[string]any{"TemporalScheduledStartTime": scheduled})
		},
	} {
		env, runs := routineRunEnv()
		if err := set(env); err != nil {
			t.Fatal(err)
		}
		env.ExecuteWorkflow("RoutineRunWorkflow", RoutineRunInput{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "cron",
			Due: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)})
		if env.GetWorkflowError() != nil || len(*runs) != 1 || !(*runs)[0].Due.Equal(scheduled) {
			t.Fatalf("%s: err %v runs %+v, want one due %v", name, env.GetWorkflowError(), *runs, scheduled)
		}
	}
}

// A run with no due time would share its store key with every other such
// run of the routine: it fails before its activity, and is not retried.
func TestRoutineRunWorkflowFailsWithoutADueTime(t *testing.T) {
	env, runs := routineRunEnv()
	env.ExecuteWorkflow("RoutineRunWorkflow", RoutineRunInput{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "cron"})
	var app *temporal.ApplicationError
	if err := env.GetWorkflowError(); !errors.As(err, &app) || !app.NonRetryable() || len(*runs) != 0 {
		t.Fatalf("err %v runs %+v, want a non-retryable failure and no run", err, *runs)
	}
}

// A second start for the same due time meets the first run and is no
// error. A run that failed may start again. Any other refusal is the
// caller's to see.
func TestStartRoutineRunIsKeyedOnTheDueTime(t *testing.T) {
	c := &mocks.Client{}
	var opts client.StartWorkflowOptions
	in := RoutineRunInput{Project: "ai-software-factory-code", Routine: "audit", Trigger: "merges", Due: time.Date(2026, 10, 13, 6, 0, 0, 0, time.FixedZone("CEST", 2*3600))}
	c.On("ExecuteWorkflow", mock.Anything, mock.Anything, "RoutineRunWorkflow", in).
		Run(func(args mock.Arguments) { opts = args.Get(1).(client.StartWorkflowOptions) }).
		Return(nil, serviceerror.NewWorkflowExecutionAlreadyStarted("done already", "", ""))
	if err := (TemporalWorkflows{Client: c}).StartRoutineRun(context.Background(), in); err != nil {
		t.Fatalf("StartRoutineRun = %v, want an already-started refusal to be success", err)
	}
	if opts.ID != "routine/ai-software-factory-code/audit/20261013T040000" || opts.TaskQueue != TaskQueue ||
		opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY || !opts.WorkflowExecutionErrorWhenAlreadyStarted {
		t.Fatalf("options = %+v", opts)
	}

	down := &mocks.Client{}
	down.On("ExecuteWorkflow", mock.Anything, mock.Anything, "RoutineRunWorkflow", in).Return(nil, serviceerror.NewUnavailable("frontend down"))
	var unavailable *serviceerror.Unavailable
	if err := (TemporalWorkflows{Client: down}).StartRoutineRun(context.Background(), in); !errors.As(err, &unavailable) {
		t.Fatalf("StartRoutineRun = %v, want the frontend's refusal", err)
	}
}

// Temporal compiles a cron into calendars and never describes it back:
// List reads back what Create and Update wrote, the cron included, and
// an Update keeps a pause someone set in Temporal's UI and restores the
// overlap policy.
func TestTemporalSchedulesReadBackWhatTheyWrote(t *testing.T) {
	ctx := context.Background()
	want := ScheduleSpec{ID: "routine/ai-software-factory-code/weekly-bump/0", Cron: "0 6 * * 1", TimeZone: "Europe/Berlin", CatchupWindow: 24 * time.Hour,
		Input: RoutineRunInput{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "cron"}}
	sc, h := &mocks.ScheduleClient{}, &mocks.ScheduleHandle{}
	c := &mocks.Client{}
	c.On("ScheduleClient").Return(sc)
	sched := TemporalSchedules{Client: c}
	var created client.ScheduleOptions
	sc.On("Create", mock.Anything, mock.Anything).Run(func(a mock.Arguments) { created = a.Get(1).(client.ScheduleOptions) }).Return(nil, nil)
	if err := sched.Create(ctx, want); err != nil {
		t.Fatal(err)
	}
	action := created.Action.(*client.ScheduleWorkflowAction)
	if created.ID != want.ID || created.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP || action.ID != "routine/ai-software-factory-code/weekly-bump" ||
		action.Workflow != "RoutineRunWorkflow" || action.TaskQueue != TaskQueue || !reflect.DeepEqual(created.Spec.CronExpressions, []string{"0 6 * * 1"}) {
		t.Fatalf("created %+v action %+v", created, action)
	}
	// described as the frontend answers: the spec without its cron, the
	// action's args and memo as payloads
	described := func(s client.Schedule) *client.ScheduleDescription {
		a := s.Action.(*client.ScheduleWorkflowAction)
		arg, err := converter.GetDefaultDataConverter().ToPayload(a.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		memo := map[string]any{}
		for k, v := range a.Memo {
			if memo[k], err = converter.GetDefaultDataConverter().ToPayload(v); err != nil {
				t.Fatal(err)
			}
		}
		return &client.ScheduleDescription{Schedule: client.Schedule{
			Action: &client.ScheduleWorkflowAction{ID: a.ID, Workflow: a.Workflow, TaskQueue: a.TaskQueue, Args: []any{arg}, Memo: memo},
			Spec:   &client.ScheduleSpec{Calendars: []client.ScheduleCalendarSpec{{}}, TimeZoneName: s.Spec.TimeZoneName},
			Policy: s.Policy, State: s.State}}
	}
	desc := described(client.Schedule{Action: created.Action, Spec: &created.Spec,
		Policy: &client.SchedulePolicies{Overlap: created.Overlap, CatchupWindow: created.CatchupWindow}, State: &client.ScheduleState{}})
	sc.On("GetHandle", mock.Anything, want.ID).Return(h)
	h.On("Describe", mock.Anything).Return(func(context.Context) (*client.ScheduleDescription, error) { return desc, nil })
	sc.On("List", mock.Anything, mock.Anything).Return(func(context.Context, client.ScheduleListOptions) (client.ScheduleListIterator, error) {
		return &scheduleList{{ID: "nightly-backup"}, {ID: want.ID}}, nil
	})
	if got, err := sched.List(ctx); err != nil || !reflect.DeepEqual(got, map[string]ScheduleSpec{want.ID: want}) {
		t.Fatalf("List = %+v, %v", got, err)
	}

	desc.Schedule.State = &client.ScheduleState{Paused: true, Note: "held for the release"}
	desc.Schedule.Policy.Overlap = enumspb.SCHEDULE_OVERLAP_POLICY_ALLOW_ALL
	h.On("Update", mock.Anything, mock.Anything).Run(func(a mock.Arguments) {
		up, err := a.Get(1).(client.ScheduleUpdateOptions).DoUpdate(client.ScheduleUpdateInput{Description: *desc})
		if err != nil {
			t.Fatal(err)
		}
		desc = described(*up.Schedule)
	}).Return(nil)
	want.Cron, want.CatchupWindow = "0 7 * * 1", 10*time.Second
	if err := sched.Update(ctx, want); err != nil {
		t.Fatal(err)
	}
	if !desc.Schedule.State.Paused || desc.Schedule.Policy.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP {
		t.Fatalf("after the update: state %+v policy %+v", desc.Schedule.State, desc.Schedule.Policy)
	}
	if got, err := sched.List(ctx); err != nil || !reflect.DeepEqual(got, map[string]ScheduleSpec{want.ID: want}) {
		t.Fatalf("List after the update = %+v, %v", got, err)
	}
}

// scheduleList is a schedule listing's iterator over its entries.
type scheduleList []*client.ScheduleListEntry

func (l *scheduleList) HasNext() bool { return len(*l) > 0 }

func (l *scheduleList) Next() (*client.ScheduleListEntry, error) {
	e := (*l)[0]
	*l = (*l)[1:]
	return e, nil
}
