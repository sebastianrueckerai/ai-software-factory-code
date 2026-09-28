package factoryd

import (
	"context"
	"errors"
	"log"
	"math"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"
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

// StepGates are the timeline steps that end a gate, each carrying the
// gate's start as started: the signal step for an approval, the others
// by name.
var StepGates = map[string]string{
	"intake": GateIntake, "session_end": GateSession, "checks": GateChecks,
	"gate_policy": GatePolicy, "gate_migration": GateMigration, "gate_reproduction": GateReproduction,
	"model_review": GateReview, "signal": GateApproval, "merged": GateMerge,
	"merge_failed": GateMerge, "merge_refused": GateMerge, "commissioned": GateCommissioning,
}

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

// SignalGateBound hands a machine gate's bound trip to its task's
// workflow, which asks the gate supervisor (ruling 141).
const SignalGateBound = "gate_bound"

// GateTrip is one gate_bound alert of a machine gate's activity run: the
// run is the activity the workflow scheduled (its id), across attempts.
type GateTrip struct {
	Activity   string
	ActivityID string
	Attempt    int32
	AlertID    int64
	Started    time.Time     // the attempt's start
	Bound      time.Duration // the bound it passed, a wait's extension included
}

// gateRetryError is the error type of an attempt the gate supervisor
// stopped for a retry: retryable, so the activity's retry policy runs it
// again.
const gateRetryError = "gate_supervisor_retry"

var errGateRetry = errors.New("the gate supervisor stopped this attempt for a retry")

// GateWatch is the worker interceptor that moves a task's row to the gate
// an activity opens, since the activity's first schedule, and raises one
// gate_bound alert for a run still going at its policy bound. A machine
// gate's alert is signalled to its task's workflow; the gate supervisor
// then acts on the attempt running here through Wait and Retry.
type GateWatch struct {
	interceptor.WorkerInterceptorBase
	Store GateStore
	Bound func(activity string) time.Duration
	// Signal delivers a trip to the workflow; nil = the alert only.
	Signal func(ctx context.Context, workflowID string, trip GateTrip) error

	mu   sync.Mutex
	runs map[string]*watchedRun // the attempts running here, by workflow and activity id
}

type watchedRun struct {
	started  time.Time
	bound    time.Duration
	timer    *time.Timer
	extended bool
	raise    func(bound time.Duration, extended bool)
	cancel   context.CancelCauseFunc
}

func runKey(workflowID, activityID string) string { return workflowID + "/" + activityID }

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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	run := &watchedRun{started: time.Now(), bound: i.g.Bound(name), cancel: cancel}
	run.raise = func(bound time.Duration, extended bool) { i.g.raise(info, taskID, run.started, bound, extended) }
	key := runKey(info.WorkflowExecution.ID, info.ActivityID)
	i.g.mu.Lock()
	if i.g.runs == nil {
		i.g.runs = map[string]*watchedRun{}
	}
	i.g.runs[key] = run
	run.timer = time.AfterFunc(run.bound, func() { run.raise(run.bound, false) })
	i.g.mu.Unlock()
	defer func() {
		i.g.mu.Lock()
		defer i.g.mu.Unlock()
		run.timer.Stop()
		if i.g.runs[key] == run {
			delete(i.g.runs, key)
		}
	}()
	out, err := i.Next.ExecuteActivity(ctx, in)
	if err != nil && errors.Is(context.Cause(ctx), errGateRetry) {
		return nil, temporal.NewApplicationError(errGateRetry.Error()+": "+err.Error(), gateRetryError)
	}
	return out, err
}

// raise writes the gate_bound alert and signals a machine gate's trip.
func (g *GateWatch) raise(info activity.Info, taskID string, started time.Time, bound time.Duration, extended bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := info.ActivityType.Name
	detail := map[string]any{"attempt": info.Attempt, "started": rfc3339(started)}
	if extended {
		detail["extended"] = true
	}
	id, err := g.Store.InsertAlert(ctx, Alert{Kind: AlertGateBound, Source: name, TaskID: taskID, BoundS: int(math.Ceil(bound.Seconds())), Detail: detail})
	if err != nil {
		log.Printf("task %s: %s passed its bound of %s; the alert was not stored: %v", taskID, name, bound, err)
		return
	}
	if gate, ok := activityGates[name]; !ok || gate == GateHuman || g.Signal == nil || taskID == "" {
		return
	}
	trip := GateTrip{Activity: name, ActivityID: info.ActivityID, Attempt: info.Attempt, AlertID: id, Started: started, Bound: bound}
	if err := g.Signal(ctx, info.WorkflowExecution.ID, trip); err != nil {
		log.Printf("task %s: signal the %s trip (alert %d): %v", taskID, name, id, err)
	}
}

// Running reports whether the run has an attempt running here.
func (g *GateWatch) Running(workflowID, activityID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.runs[runKey(workflowID, activityID)]
	return ok
}

// Wait extends the running attempt's bound once by its own length; false
// when it runs here no more or was extended already.
func (g *GateWatch) Wait(workflowID, activityID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	run, ok := g.runs[runKey(workflowID, activityID)]
	if !ok || run.extended {
		return false
	}
	run.extended = true
	run.timer.Stop()
	run.timer = time.AfterFunc(time.Until(run.started.Add(2*run.bound)), func() { run.raise(2*run.bound, true) })
	return true
}

// Retry stops the running attempt with a retryable error; false when it
// runs here no more.
func (g *GateWatch) Retry(workflowID, activityID string) bool {
	g.mu.Lock()
	run, ok := g.runs[runKey(workflowID, activityID)]
	g.mu.Unlock()
	if ok {
		run.cancel(errGateRetry)
	}
	return ok
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
