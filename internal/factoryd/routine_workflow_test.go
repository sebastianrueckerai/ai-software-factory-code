package factoryd

import (
	"context"
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
	if got := RoutineRunID(in); got != "routine-ai-software-factory-code-weekly-bump-20261013T0400" {
		t.Fatalf("id %q", got)
	}
}

// A schedule's run is due at the time the schedule set on it, not at the
// time its input carries.
func TestRoutineRunWorkflowTakesTheScheduledTime(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(RoutineRunWorkflow, workflow.RegisterOptions{Name: "RoutineRunWorkflow"})
	var due time.Time
	env.RegisterActivityWithOptions(func(ctx context.Context, in RoutineRunInput) (RoutineRunResult, error) {
		due = in.Due
		return RoutineRunResult{Outcome: "filed"}, nil
	}, activity.RegisterOptions{Name: "RoutineRun"})
	scheduled := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	if err := env.SetTypedSearchAttributesOnStart(temporal.NewSearchAttributes(
		temporal.NewSearchAttributeKeyTime("TemporalScheduledStartTime").ValueSet(scheduled))); err != nil {
		t.Fatal(err)
	}
	env.ExecuteWorkflow("RoutineRunWorkflow", RoutineRunInput{Project: "ai-software-factory-code", Routine: "weekly-bump", Trigger: "cron",
		Due: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)})
	if env.GetWorkflowError() != nil || !due.Equal(scheduled) {
		t.Fatalf("err %v due %v, want %v", env.GetWorkflowError(), due, scheduled)
	}
}

// A second start for the same due time meets the first run and is no
// error. A run that failed may start again.
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
	if opts.ID != "routine-ai-software-factory-code-audit-20261013T0400" || opts.TaskQueue != TaskQueue ||
		opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY || !opts.WorkflowExecutionErrorWhenAlreadyStarted {
		t.Fatalf("options = %+v", opts)
	}
}

// Temporal compiles a cron into calendars and never describes it back:
// List reads back what Create and Update wrote, the cron included, and
// an Update keeps a pause someone set in Temporal's UI.
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
	if created.ID != want.ID || created.Overlap != enumspb.SCHEDULE_OVERLAP_POLICY_SKIP || action.ID != "routine-ai-software-factory-code-weekly-bump" ||
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
