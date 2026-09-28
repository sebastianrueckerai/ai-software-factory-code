package factoryd

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func watchedEnv(store *fakeStore, bound time.Duration) *testsuite.TestActivityEnvironment {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{
		&GateWatch{Store: store, Bound: func(string) time.Duration { return bound }}}})
	return env
}

// An activity that opens a gate moves the task's row to it, since the
// activity was scheduled; one that opens none leaves the row.
func TestGateWatchMovesTheTaskToTheActivitysGate(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "running"})
	env := watchedEnv(store, time.Minute)
	env.RegisterActivityWithOptions(func(context.Context, SessionInput) error { return nil }, activity.RegisterOptions{Name: ActivityRunSession})
	env.RegisterActivityWithOptions(func(context.Context, string, string) error { return nil }, activity.RegisterOptions{Name: ActivityWaitChecks})
	env.RegisterActivityWithOptions(func(context.Context, string, string) error { return nil }, activity.RegisterOptions{Name: ActivitySetState})
	before := time.Now()
	for _, run := range []func() error{
		func() error {
			_, err := env.ExecuteActivity(ActivityRunSession, SessionInput{TaskID: "t1", Attempt: 1})
			return err
		},
		func() error { _, err := env.ExecuteActivity(ActivitySetState, "t1", "in-review"); return err },
		func() error { _, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1"); return err },
	} {
		if err := run(); err != nil {
			t.Fatal(err)
		}
	}
	var gates []string
	for _, g := range store.gates {
		gates = append(gates, strings.Join(strings.Fields(g)[:2], " "))
	}
	got, _, _ := store.Task(context.Background(), "t1")
	if !slices.Equal(gates, []string{"t1 session", "t1 checks"}) || got.Gate != GateChecks || got.GateSince.Before(before) || got.GateSince.After(time.Now()) {
		t.Fatalf("gates set %q; task in %q since %v, want session then checks, since a moment of the run", store.gates, got.Gate, got.GateSince)
	}
	if len(store.alertRows()) != 0 {
		t.Fatalf("alerts = %+v, want none within the bound", store.alertRows())
	}
}

// An activity still running at its bound raises one gate_bound alert,
// however long it runs past it.
func TestGateWatchAlertsOncePerOverrun(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review"})
	env := watchedEnv(store, 50*time.Millisecond)
	env.RegisterActivityWithOptions(func(context.Context, string, string) error {
		time.Sleep(300 * time.Millisecond)
		return nil
	}, activity.RegisterOptions{Name: ActivityWaitChecks})
	env.RegisterActivityWithOptions(func(context.Context, string) error { return nil }, activity.RegisterOptions{Name: ActivityEnsureIssue})
	if _, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.ExecuteActivity(ActivityEnsureIssue, "t1"); err != nil {
		t.Fatal(err)
	}
	alerts := store.alertRows()
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want one for the overrun", alerts)
	}
	// 50 ms is reported in whole seconds, rounded up
	if a := alerts[0]; a.Kind != AlertGateBound || a.Source != ActivityWaitChecks || a.TaskID != "t1" || a.BoundS != 1 || a.Verdict != "" || a.Detail["attempt"] != int32(1) {
		t.Fatalf("alert = %+v", a)
	}
}

// Every activity factoryd registers may carry a policy bound, and the
// policy knows no other.
func TestBoundActivitiesAreTheRegisteredOnes(t *testing.T) {
	var names []string
	(&Runner{}).RegisterActivities(nameRecorder(func(n string) { names = append(names, n) }))
	slices.Sort(names)
	want := slices.Sorted(slices.Values(policy.BoundActivities))
	if !slices.Equal(names, want) {
		t.Fatalf("registered %v, policy bounds %v", names, want)
	}
}

type nameRecorder func(string)

func (f nameRecorder) RegisterActivityWithOptions(_ any, o activity.RegisterOptions) { f(o.Name) }
