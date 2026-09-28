package factoryd

import (
	"context"
	"log"
	"math"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
)

// The gates a task passes (stage 8 proposal 2.d); the task row's gate and
// the timeline's durations name them alike.
const (
	GateIntake        = "intake"
	GateSession       = "session"
	GateChecks        = "checks"
	GatePolicy        = "policy"
	GateMigration     = "migration"
	GateReproduction  = "reproduction"
	GateReview        = "review"
	GateApproval      = "approval"
	GateMerge         = "merge"
	GateHuman         = "human"
	GateCommissioning = "commissioning"
)

// activityGates are the gates the activities open: the task's row is in
// the gate from the activity's schedule until the next one opens.
var activityGates = map[string]string{
	ActivityRunSession:       GateSession,
	ActivityWaitChecks:       GateChecks,
	ActivityGatePolicy:       GatePolicy,
	ActivityGateMigration:    GateMigration,
	ActivityGateReproduction: GateReproduction,
	ActivityModelReview:      GateReview,
	ActivityRequestApproval:  GateApproval,
	ActivityMerge:            GateMerge,
	ActivityEscalate:         GateHuman,
}

// GateStore is what GateWatch writes.
type GateStore interface {
	SetGate(ctx context.Context, id, gate string, since time.Time) error
	InsertAlert(ctx context.Context, a Alert) (int64, error)
}

// GateWatch is the worker interceptor that moves a task's row to the gate
// an activity opens, since the activity's first schedule, and raises one
// gate_bound alert for a run still going at its policy bound.
type GateWatch struct {
	interceptor.WorkerInterceptorBase
	Store GateStore
	Bound func(activity string) time.Duration
}

func (g *GateWatch) InterceptActivity(ctx context.Context, next interceptor.ActivityInboundInterceptor) interceptor.ActivityInboundInterceptor {
	return &gateWatchInbound{ActivityInboundInterceptorBase: interceptor.ActivityInboundInterceptorBase{Next: next}, g: g}
}

type gateWatchInbound struct {
	interceptor.ActivityInboundInterceptorBase
	g *GateWatch
}

func (i *gateWatchInbound) ExecuteActivity(ctx context.Context, in *interceptor.ExecuteActivityInput) (any, error) {
	info := activity.GetInfo(ctx)
	name, taskID := info.ActivityType.Name, activityTask(in.Args)
	if gate, ok := activityGates[name]; ok && taskID != "" {
		if err := i.g.Store.SetGate(ctx, taskID, gate, info.ScheduledTime); err != nil {
			log.Printf("task %s: enter gate %s: %v", taskID, gate, err)
		}
	}
	bound := i.g.Bound(name)
	started := time.Now()
	overrun := time.AfterFunc(bound, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a := Alert{Kind: AlertGateBound, Source: name, TaskID: taskID, BoundS: int(math.Ceil(bound.Seconds())),
			Detail: map[string]any{"attempt": info.Attempt, "started": started.UTC().Format(time.RFC3339Nano)}}
		if _, err := i.g.Store.InsertAlert(ctx, a); err != nil {
			log.Printf("task %s: %s passed its bound of %s; the alert was not stored: %v", taskID, name, bound, err)
		}
	})
	defer overrun.Stop()
	return i.Next.ExecuteActivity(ctx, in)
}

// activityTask is the task an activity works on: its first argument.
func activityTask(args []any) string {
	if len(args) == 0 {
		return ""
	}
	switch a := args[0].(type) {
	case string:
		return a
	case SessionInput:
		return a.TaskID
	}
	return ""
}

// gateEnd is the detail of a step that ends gate, with the gate's start
// when the task's row is in it (GateWatch put it there when the gate's
// activity was scheduled).
func gateEnd(t Task, gate string, detail map[string]any) map[string]any {
	if t.Gate == gate && t.GateSince != nil {
		detail["started"] = rfc3339(*t.GateSince)
	}
	return detail
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
