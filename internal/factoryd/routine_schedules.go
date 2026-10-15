package factoryd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// ScheduleSpec is one cron trigger of a routine as a Temporal schedule.
type ScheduleSpec struct {
	ID            string // "routine/" + project + "/" + routine + "/" + the trigger's index
	Cron          string
	TimeZone      string
	CatchupWindow time.Duration
	Input         RoutineRunInput
}

// Schedules is the seam over Temporal Schedules: the routines' clocks.
type Schedules interface {
	// List is every schedule whose ID starts with "routine/".
	List(ctx context.Context) (map[string]ScheduleSpec, error)
	Create(ctx context.Context, s ScheduleSpec) error
	Update(ctx context.Context, s ScheduleSpec) error
	Delete(ctx context.Context, id string) error
	Trigger(ctx context.Context, id string) error
}

// TemporalSchedules is Schedules against a Temporal frontend.
type TemporalSchedules struct{ Client client.Client }

// cronMemo is the action's memo key carrying the cron: Temporal compiles a
// cron into calendars and never describes it back.
const cronMemo = "cron"

func (t TemporalSchedules) List(ctx context.Context) (map[string]ScheduleSpec, error) {
	it, err := t.Client.ScheduleClient().List(ctx, client.ScheduleListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]ScheduleSpec{}
	for it.HasNext() {
		e, err := it.Next()
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(e.ID, "routine/") {
			continue
		}
		d, err := t.Client.ScheduleClient().GetHandle(ctx, e.ID).Describe(ctx)
		var gone *serviceerror.NotFound
		if errors.As(err, &gone) {
			continue // deleted, and the listing has not caught up
		}
		if err != nil {
			return nil, err
		}
		s, err := specOf(e.ID, d)
		if err != nil {
			return nil, fmt.Errorf("schedule %s: %w", e.ID, err)
		}
		out[e.ID] = s
	}
	return out, nil
}

func (t TemporalSchedules) Create(ctx context.Context, s ScheduleSpec) error {
	spec, action := scheduleOf(s)
	_, err := t.Client.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID: s.ID, Spec: spec, Action: action, Overlap: enumspb.SCHEDULE_OVERLAP_POLICY_SKIP, CatchupWindow: s.CatchupWindow,
	})
	return err
}

// Update replaces the schedule's spec, action, overlap and catch-up window
// and keeps its state: a pause set in Temporal's UI stays.
func (t TemporalSchedules) Update(ctx context.Context, s ScheduleSpec) error {
	spec, action := scheduleOf(s)
	return t.Client.ScheduleClient().GetHandle(ctx, s.ID).Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			sch := in.Description.Schedule
			sch.Spec, sch.Action = &spec, action
			sch.Policy.Overlap, sch.Policy.CatchupWindow = enumspb.SCHEDULE_OVERLAP_POLICY_SKIP, s.CatchupWindow
			return &client.ScheduleUpdate{Schedule: &sch}, nil
		},
	})
}

func (t TemporalSchedules) Delete(ctx context.Context, id string) error {
	return t.Client.ScheduleClient().GetHandle(ctx, id).Delete(ctx)
}

func (t TemporalSchedules) Trigger(ctx context.Context, id string) error {
	return t.Client.ScheduleClient().GetHandle(ctx, id).Trigger(ctx, client.ScheduleTriggerOptions{})
}

func scheduleOf(s ScheduleSpec) (client.ScheduleSpec, *client.ScheduleWorkflowAction) {
	return client.ScheduleSpec{CronExpressions: []string{s.Cron}, TimeZoneName: s.TimeZone},
		&client.ScheduleWorkflowAction{ID: routineRunPrefix(s.Input), Workflow: "RoutineRunWorkflow", Args: []any{s.Input},
			TaskQueue: TaskQueue, Memo: map[string]any{cronMemo: s.Cron}}
}

// specOf reads a described schedule back: the action's args and memo
// arrive as payloads.
func specOf(id string, d *client.ScheduleDescription) (ScheduleSpec, error) {
	s := ScheduleSpec{ID: id, TimeZone: d.Schedule.Spec.TimeZoneName, CatchupWindow: d.Schedule.Policy.CatchupWindow}
	a, ok := d.Schedule.Action.(*client.ScheduleWorkflowAction)
	if !ok || len(a.Args) != 1 {
		return s, fmt.Errorf("action %T is not a routine's run", d.Schedule.Action)
	}
	dc := converter.GetDefaultDataConverter()
	if p, ok := a.Memo[cronMemo].(*commonpb.Payload); ok {
		if err := dc.FromPayload(p, &s.Cron); err != nil {
			return s, err
		}
	}
	p, _ := a.Args[0].(*commonpb.Payload)
	return s, dc.FromPayload(p, &s.Input)
}
