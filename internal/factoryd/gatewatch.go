package factoryd

import (
	"context"
	"errors"
	"log"
	"math"
	"slices"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/manual"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The gates a task passes (stage 8 proposal 2.d); the task row's gate and
// the timeline's durations name them alike.
const (
	GateIntake        = manual.GateIntake
	GateSession       = manual.GateSession
	GateChecks        = manual.GateChecks
	GatePolicy        = manual.GatePolicy
	GateMigration     = manual.GateMigration
	GateReproduction  = manual.GateReproduction
	GateCriteria      = manual.GateCriteria
	GateReview        = manual.GateReview
	GateApproval      = manual.GateApproval
	GateMerge         = manual.GateMerge
	GateHuman         = manual.GateHuman
	GateCommissioning = manual.GateCommissioning
	GateScreening     = manual.GateScreening
	GateAudit         = manual.GateAudit
	GateProvider      = manual.GateProvider
	GateDependencies  = manual.GateDependencies
	GateMergeQueue    = manual.GateMergeQueue
	GateSizing        = manual.GateSizing
)

// StepGates are the timeline steps that end a gate, each carrying the
// gate's start as started: the signal step for an approval, the others
// by name.
var StepGates = map[string]string{
	"intake": GateIntake, "session_end": GateSession, "checks": GateChecks,
	"gate_policy": GatePolicy, "gate_migration": GateMigration, "gate_reproduction": GateReproduction, "gate_criteria": GateCriteria,
	"model_review": GateReview, "signal": GateApproval, "merged": GateMerge,
	"merge_failed": GateMerge, "merge_refused": GateMerge, "commissioned": GateCommissioning, "screening": GateScreening,
	"audit": GateAudit, "dependencies_done": GateDependencies, "queue_turn": GateMergeQueue, StepSizing: GateSizing,
}

// activityGates are the gates the activities open: the task's row is in
// the gate from the activity's schedule until the next one opens. A gate
// beside the checks wait (the review, the audit) opens none
// (activityBeside): its stage moves the row to its gate once it awaits
// it (SetGate).
var activityGates = map[string]string{
	ActivityRunSession:        GateSession,
	ActivityWaitChecks:        GateChecks,
	ActivityGatePolicy:        GatePolicy,
	ActivityGateCompanion:     GatePolicy,
	ActivityGateMigration:     GateMigration,
	ActivityGateReproduction:  GateReproduction,
	ActivityGateCriteria:      GateCriteria,
	ActivityModelReview:       GateReview,
	ActivityRequestApproval:   GateApproval,
	ActivityMerge:             GateMerge,
	ActivityOpenTrip:          GateHuman,
	ActivityScreen:            GateScreening,
	ActivityScreenAnswer:      GateScreening,
	ActivitySizeCard:          GateSizing,
	ActivityRunReport:         GateSession,
	ActivityGateAudit:         GateAudit,
	ActivityRunPlan:           GateSession,
	ActivityRequestPlan:       GateApproval,
	ActivityHoldoutSettlement: GateSession,
	ActivityAwaitSessionGap:   GateMerge,
	ActivityProviderWait:      GateProvider,
	ActivityProofsAtMain:      GateSession,
}

// defaultBoundActivities are the activities policy.BoundActivities does
// not name: they take the policy's default bound, RunPlan a session's,
// SizeCard a screening's.
// Naming one there would change the configuration tree's schema, which
// only an infra commit follows.
var defaultBoundActivities = []string{ActivityDependencies, ActivityRunPlan, ActivityRequestPlan,
	ActivityEnterQueue, ActivityAdmitResume, ActivitySyncWithMain, ActivityLeaveQueue, ActivityWithdrawApproval, ActivityBranchHead, ActivityScreenAnswer,
	ActivityAwaitSessionGap, ActivityAwaitRollHold, ActivityClearRollHold, ActivityAwaitRollout, ActivityProviderWait, ActivityOpenPlanQuestions,
	ActivityAlertPreCheck, ActivityAttemptFunded, ActivitySessionFunded, ActivitySessionAnswered, ActivityDayResume, ActivityCapResume, ActivityHeadChecks, ActivitySetGate, ActivityDropTrip,
	ActivityWithdrawQuestion, ActivityRoutineRun, ActivityRoutineRunFailed, ActivityHoldoutSettlement, ActivityParkForTool, ActivityImageArrived, ActivityLeaveImagePark,
	ActivityBecomePlan, ActivitySizeCard, ActivityReplanChild, ActivityReplanStopped, ActivitySupersedeApproval, ActivityReadUsage,
	ActivitySuccessor, ActivityUnjudged, ActivityHoldMerge, ActivityEndCase, ActivityCaseState, ActivityOpenTrip, ActivityRecordGrant, ActivityAuditDraw,
	ActivityRecordNotRun, ActivityCopyUpstreamImages}

// GateStore is what GateWatch writes.
type GateStore interface {
	SetGate(ctx context.Context, id, gate string, since time.Time) error
	InsertAlert(ctx context.Context, a Alert) (int64, error)
	Task(ctx context.Context, id string) (Task, bool, error)
}

// SignalGateBound hands a long gate's bound trip to its task's workflow,
// which asks the gate supervisor.
const SignalGateBound = "gate_bound"

// supervisedGates are the long gates: a short gate's attempt
// times out before any supervisor answer could arrive, so its gate_bound
// alert is its record.
var supervisedGates = []string{ActivityRunSession, ActivityWaitChecks, ActivityGateReproduction, ActivityGateCriteria, ActivityModelReview, ActivityGateAudit, ActivityMerge}

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
// gate_bound alert for a run still going at its policy bound. A long
// gate's alert is signalled to its task's workflow; the gate supervisor
// then acts on the attempt that tripped through Wait and Retry. factoryd
// runs one replica (k8s/factoryd.yaml), so an attempt runs in this
// process or not at all.
type GateWatch struct {
	interceptor.WorkerInterceptorBase
	Store GateStore
	// Bound is activity's bound under the policy version pin, the one its
	// task started under (0: the version in force); waves: the waves of
	// calls a model review or an audit makes, policy.ReviewParts until it
	// read the diff (boundByParts).
	Bound func(ctx context.Context, activity string, pin int64, waves int) time.Duration
	// Signal delivers a trip to the workflow; nil = the alert only.
	Signal func(ctx context.Context, workflowID string, trip GateTrip) error
	// LookupRetry is how long a checks wait whose task could not be read
	// waits before it reads it again; 30s when zero.
	LookupRetry time.Duration

	mu   sync.Mutex
	runs map[string]*watchedRun // the attempt of each run running here, by workflow and activity id
}

type watchedRun struct {
	activity string
	attempt  int32
	started  time.Time
	bound    time.Duration
	told     time.Duration // the largest bound its activity told by waves
	deadline time.Time     // when the timer is armed to fire after a wait
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
	if gate, ok := activityGates[name]; ok && taskID != "" && !activityBeside(in.Args) {
		if err := i.g.Store.SetGate(ctx, taskID, gate, info.ScheduledTime); err != nil {
			log.Printf("task %s: enter gate %s: %v", taskID, gate, err)
		}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	pin := activityPin(in.Args)
	run := &watchedRun{activity: name, attempt: info.Attempt, started: time.Now(), cancel: cancel}
	// a checks wait's bound is its project's, found through its task: while
	// the task is unreadable the wait has no bound to trip at, and the
	// lookup is tried again each retry interval
	lookup := name == ActivityWaitChecks && taskID != ""
	if lookup {
		if t, ok, err := i.g.Store.Task(ctx, taskID); err == nil && ok {
			ctx, lookup = context.WithValue(ctx, repositoryKey{}, t.Repository), false
		} else {
			log.Printf("task %s: the bound of %s waits for its task: %v (found %v)", taskID, name, err, ok)
		}
	}
	if !lookup {
		run.bound = i.g.Bound(ctx, name, pin, policy.ReviewParts)
	}
	run.raise = func(bound time.Duration, extended bool) { i.g.raise(info, taskID, run.started, bound, extended) }
	waves := func(n int) { i.g.rebound(run, i.g.Bound(ctx, name, pin, n)) }
	key := runKey(info.WorkflowExecution.ID, info.ActivityID)
	i.g.mu.Lock()
	if i.g.runs == nil {
		i.g.runs = map[string]*watchedRun{}
	}
	i.g.runs[key] = run
	if lookup {
		run.timer = time.AfterFunc(i.g.lookupRetry(), func() { i.g.retryBound(run, key, ctx, taskID, pin) })
	} else {
		run.timer = time.AfterFunc(run.bound, func() { run.raise(run.bound, false) })
	}
	i.g.mu.Unlock()
	defer func() {
		cancel(nil)
		i.g.mu.Lock()
		defer i.g.mu.Unlock()
		run.timer.Stop()
		if i.g.runs[key] == run {
			delete(i.g.runs, key)
		}
	}()
	out, err := i.Next.ExecuteActivity(context.WithValue(ctx, wavesKey{}, waves), in)
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
	if !slices.Contains(supervisedGates, name) || g.Signal == nil || taskID == "" {
		return
	}
	trip := GateTrip{Activity: name, ActivityID: info.ActivityID, Attempt: info.Attempt, AlertID: id, Started: started, Bound: bound}
	if err := g.Signal(ctx, info.WorkflowExecution.ID, trip); err != nil {
		log.Printf("task %s: signal the %s trip (alert %d): %v", taskID, name, id, err)
	}
}

// run is the attempt that tripped when it still runs here.
func (g *GateWatch) run(workflowID, activityID string, attempt int32) (*watchedRun, bool) {
	run, ok := g.runs[runKey(workflowID, activityID)]
	return run, ok && run.attempt == attempt
}

// Running reports whether the attempt runs here.
func (g *GateWatch) Running(workflowID, activityID string, attempt int32) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.run(workflowID, activityID, attempt)
	return ok
}

// Wait extends the attempt's bound once by its own length; false when it
// runs here no more or was extended already. An answer after that moment
// still gives the attempt half a bound before it trips again.
func (g *GateWatch) Wait(workflowID, activityID string, attempt int32) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	run, ok := g.run(workflowID, activityID, attempt)
	if !ok || run.extended {
		return false
	}
	run.extended = true
	run.timer.Stop()
	if run.told > 2*run.bound {
		run.bound = run.told / 2
	}
	g.armExtended(run, run.started.Add(2*run.bound))
	return true
}

// armExtended fires the extended run's trip at the later of at and half a
// bound from now, never before the deadline it holds.
func (g *GateWatch) armExtended(run *watchedRun, at time.Time) {
	for _, t := range []time.Time{run.deadline, time.Now().Add(run.bound / 2)} {
		if t.After(at) {
			at = t
		}
	}
	run.deadline = at
	extended, deadline := 2*run.bound, run.deadline
	run.timer = time.AfterFunc(time.Until(deadline), func() { run.raise(extended, true) })
}

func (g *GateWatch) lookupRetry() time.Duration {
	if g.LookupRetry > 0 {
		return g.LookupRetry
	}
	return 30 * time.Second
}

// retryBound is the tick of a checks wait whose task was unreadable at its
// start: it reads the task again and arms the bound from the attempt's
// start, or waits for the next tick.
func (g *GateWatch) retryBound(run *watchedRun, key string, ctx context.Context, taskID string, pin int64) {
	t, ok, err := g.Store.Task(ctx, taskID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx.Err() != nil || g.runs[key] != run {
		return
	}
	if err != nil || !ok {
		log.Printf("task %s: the bound of %s still waits for its task: %v (found %v)", taskID, run.activity, err, ok)
		run.timer = time.AfterFunc(g.lookupRetry(), func() { g.retryBound(run, key, ctx, taskID, pin) })
		return
	}
	run.bound = g.Bound(context.WithValue(ctx, repositoryKey{}, t.Repository), run.activity, pin, policy.ReviewParts)
	bound := run.bound
	run.timer = time.AfterFunc(max(time.Until(run.started.Add(bound)), 0), func() { run.raise(bound, false) })
}

// rebound moves run's bound to bound, from the attempt's start: a model
// review's or an audit's once it knows its waves of calls. The first tell
// may lower the bound the run began with; a later one never goes below the
// largest told before it, and one that finds the timer fired is kept for
// the wait. A run that tripped and waited keeps its deadline unless the
// bound reaches past it: the timer only moves outward.
func (g *GateWatch) rebound(run *watchedRun, bound time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !run.timer.Stop() {
		run.told = max(run.told, bound)
		return
	}
	if run.extended {
		if bound > 2*run.bound {
			run.bound = bound / 2
		}
		run.told = max(run.told, bound)
		g.armExtended(run, run.started.Add(bound))
		return
	}
	bound = max(bound, run.told)
	run.bound, run.told = bound, bound
	run.timer = time.AfterFunc(time.Until(run.started.Add(bound)), func() { run.raise(bound, false) })
}

// repositoryKey carries the repository of the task a checks wait runs for
// to the bound: a project may shorten the wait's bound.
type repositoryKey struct{}

// wavesKey carries an activity's call to rebound in its context.
type wavesKey struct{}

// boundByParts bounds the running model review or audit by the waves its
// calls run in, one call's timeout each (policy.Document.PartsBound):
// callAll tells it before the first call and before its retries.
func boundByParts(ctx context.Context, waves int) {
	if f, ok := ctx.Value(wavesKey{}).(func(int)); ok {
		f(waves)
	}
}

// Retry stops the attempt with a retryable error; false when it runs here
// no more.
func (g *GateWatch) Retry(workflowID, activityID string, attempt int32) bool {
	g.mu.Lock()
	run, ok := g.run(workflowID, activityID, attempt)
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

// activityPin is the policy version an activity's input carries: a
// session's, a gate supervisor call's, a model review's, an audit's; 0
// for the rest.
func activityPin(args []any) int64 {
	for _, a := range args {
		switch a := a.(type) {
		case SessionInput:
			return a.PolicyVersion
		case GateSupervision:
			return a.PolicyVersion
		case GateResults:
			return a.Pin
		}
	}
	return 0
}

// activityBeside: the activity is a gate beside the checks wait
// (GateResults.Beside), which the workflow does not await at its schedule.
func activityBeside(args []any) bool {
	for _, a := range args {
		if g, ok := a.(GateResults); ok {
			return g.Beside
		}
	}
	return false
}

// PinnedBound is GateWatch.Bound: the bound of the version pin (read with
// read), the current version for 0 or a version it cannot read, since a
// bound only decides when to alert. A roll's waits end at their own bound
// (gapBound), so theirs is that and rollWaitSlack. A review's or an
// audit's stays fanOutTripMargin under its attempt's reviewActivityMax, so
// the watch trips, and the supervisor decides, before Temporal ends it.
func PinnedBound(current func() policy.Version, read func(ctx context.Context, id int64) (policy.Version, bool, error)) func(ctx context.Context, activity string, pin int64, waves int) time.Duration {
	return func(ctx context.Context, activity string, pin int64, waves int) time.Duration {
		if activity == ActivityAwaitSessionGap || activity == ActivityAwaitRollHold {
			return gapBound(current()) + rollWaitSlack
		}
		tree := current().Tree
		if pin != 0 {
			v, ok, err := read(ctx, pin)
			if err == nil && ok {
				tree = v.Tree
			} else {
				log.Printf("bound of %s: policy version %d unreadable (%v, found %v): the current one's", activity, pin, err, ok)
			}
		}
		bound := tree.Document.PartsBound(activity, waves)
		if activity == ActivityWaitChecks {
			repository, _ := ctx.Value(repositoryKey{}).(string)
			bound = tree.ChecksBound(repository)
		}
		if activity == ActivityModelReview || activity == ActivityGateAudit {
			bound = min(bound, reviewActivityMax-fanOutTripMargin)
		}
		return bound
	}
}

// fanOutTripMargin is how long before its attempt's end a review's or an
// audit's gate watch trips at the latest.
const fanOutTripMargin = 10 * time.Minute

// gateEnd is the detail of a step that ends gate, with the gate's start
// when the task's row is in it (GateWatch put it there when the gate's
// activity was scheduled).
func gateEnd(t Task, gate string, detail map[string]any) map[string]any {
	if t.Gate == gate && t.GateSince != nil {
		detail["started"] = rfc3339(*t.GateSince)
	}
	return detail
}

// besideEnd is gateEnd for a gate that can run beside the checks wait
// (GateResults.Beside): that one opened no gate on the task's row, so its
// own schedule starts it.
func besideEnd(ctx context.Context, t Task, gate string, beside bool, detail map[string]any) map[string]any {
	if !beside {
		return gateEnd(t, gate, detail)
	}
	detail["started"] = rfc3339(activity.GetInfo(ctx).ScheduledTime)
	return detail
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
