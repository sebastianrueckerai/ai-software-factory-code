package factoryd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func watchedEnv(store *fakeStore, bound time.Duration) *testsuite.TestActivityEnvironment {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{
		&GateWatch{Store: store, Bound: func(context.Context, string, int64, int) time.Duration { return bound }}}})
	return env
}

// An activity that opens a gate moves the task's row to it, since the
// activity was scheduled; one that opens none leaves the row.
func TestGateWatchMovesTheTaskToTheActivitysGate(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "running", Track: "direct"})
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
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
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

// signalled records the trips GateWatch hands to workflows; onTrip, when
// set, answers each as the gate supervisor would, in process.
type signalled struct {
	mu     sync.Mutex
	trips  []GateTrip
	wfIDs  []string
	onTrip func(workflowID string, trip GateTrip)
}

func (s *signalled) signal(_ context.Context, workflowID string, trip GateTrip) error {
	s.mu.Lock()
	s.trips = append(s.trips, trip)
	s.wfIDs = append(s.wfIDs, workflowID)
	on := s.onTrip
	s.mu.Unlock()
	if on != nil {
		on(workflowID, trip)
	}
	return nil
}

func (s *signalled) got() ([]GateTrip, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.trips), slices.Clone(s.wfIDs)
}

func signallingEnv(g *GateWatch) *testsuite.TestActivityEnvironment {
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{g}})
	return env
}

// A long gate's activity past its bound signals its task's workflow with
// the alert; a short gate, an activity that opens no gate and the human
// gate only alert.
func TestGateWatchSignalsAMachineGatesTrip(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
	sig := &signalled{}
	env := signallingEnv(&GateWatch{Store: store, Bound: func(context.Context, string, int64, int) time.Duration { return 50 * time.Millisecond }, Signal: sig.signal})
	var info activity.Info
	slow := func(ctx context.Context) {
		info = activity.GetInfo(ctx)
		time.Sleep(300 * time.Millisecond)
	}
	env.RegisterActivityWithOptions(func(ctx context.Context, _, _ string) error { slow(ctx); return nil }, activity.RegisterOptions{Name: ActivityWaitChecks})
	env.RegisterActivityWithOptions(func(ctx context.Context, _, _ string) error { slow(ctx); return nil }, activity.RegisterOptions{Name: ActivitySetState})
	env.RegisterActivityWithOptions(func(ctx context.Context, _, _ string) error { slow(ctx); return nil }, activity.RegisterOptions{Name: ActivityEscalate})
	env.RegisterActivityWithOptions(func(ctx context.Context, _, _ string) error { slow(ctx); return nil }, activity.RegisterOptions{Name: ActivityGatePolicy})
	if _, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1"); err != nil {
		t.Fatal(err)
	}
	checks := info
	for _, name := range []string{ActivitySetState, ActivityEscalate, ActivityGatePolicy} {
		if _, err := env.ExecuteActivity(name, "t1", "x"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(store.alertRows()); n != 4 {
		t.Fatalf("alerts = %d, want one per overrun", n)
	}
	trips, wfIDs := sig.got()
	if len(trips) != 1 || wfIDs[0] != checks.WorkflowExecution.ID {
		t.Fatalf("signals = %+v to %v, want the check wait's only, to its workflow %q", trips, wfIDs, checks.WorkflowExecution.ID)
	}
	if tr := trips[0]; tr.Activity != ActivityWaitChecks || tr.ActivityID != checks.ActivityID || tr.Attempt != 1 || tr.AlertID != 1 ||
		tr.Bound != 50*time.Millisecond || tr.Started.IsZero() {
		t.Fatalf("trip = %+v", tr)
	}
}

// wait extends the run's bound once by its own length: a run past twice
// its bound trips a second time, and a second wait extends nothing.
func TestGateWatchWaitExtendsTheBoundOnce(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
	sig := &signalled{}
	g := &GateWatch{Store: store, Bound: func(context.Context, string, int64, int) time.Duration { return 100 * time.Millisecond }, Signal: sig.signal}
	var mu sync.Mutex
	var waits []bool
	sig.onTrip = func(wf string, tr GateTrip) {
		w := g.Wait(wf, tr.ActivityID, tr.Attempt)
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, w)
	}
	env := signallingEnv(g)
	env.RegisterActivityWithOptions(func(context.Context, string, string) error {
		time.Sleep(450 * time.Millisecond)
		return nil
	}, activity.RegisterOptions{Name: ActivityWaitChecks})
	if _, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1"); err != nil {
		t.Fatal(err)
	}
	trips, _ := sig.got()
	mu.Lock()
	defer mu.Unlock()
	if len(trips) != 2 || !slices.Equal(waits, []bool{true, false}) {
		t.Fatalf("trips = %+v, waits = %v; want two trips (at 100 and 200 ms), the first wait taken, the second refused", trips, waits)
	}
	alerts := store.alertRows()
	if len(alerts) != 2 || alerts[0].BoundS != 1 || alerts[1].Detail["extended"] != true || trips[1].AlertID != 2 || trips[1].Bound != 200*time.Millisecond {
		t.Fatalf("alerts = %+v, second trip %+v", alerts, trips[1])
	}
	if g.Running("any", trips[0].ActivityID, 1) || g.Wait("any", "x", 1) || g.Retry("any", "x", 1) {
		t.Fatal("an unknown run is running, or took a wait or a retry")
	}
}

// A model review's bound follows the parts its diff reads in
// (boundByParts): until it read the diff it is policy.ReviewParts parts',
// 300 ms at 100 ms a part, so a review that runs 200 ms never trips;
// told one part, it trips at 100 ms, the trip carrying that bound; told
// three and then one it keeps three.
func TestGateWatchReviewBoundFollowsItsParts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts int // 0: the review tells none
		want  []time.Duration
	}{
		{"one part", 1, []time.Duration{100 * time.Millisecond}},
		{"no parts told", 0, nil},
		{"a later tell never lowers", -3, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
			sig := &signalled{}
			env := signallingEnv(&GateWatch{Store: store, Bound: func(_ context.Context, _ string, _ int64, parts int) time.Duration {
				return time.Duration(parts) * 100 * time.Millisecond
			}, Signal: sig.signal})
			env.RegisterActivityWithOptions(func(ctx context.Context, _ string) error {
				if tc.parts > 0 {
					boundByParts(ctx, tc.parts)
				}
				if tc.parts < 0 {
					// three waves told, then one: the bound stays at three
					boundByParts(ctx, -tc.parts)
					boundByParts(ctx, 1)
				}
				time.Sleep(200 * time.Millisecond)
				return nil
			}, activity.RegisterOptions{Name: ActivityModelReview})
			if _, err := env.ExecuteActivity(ActivityModelReview, "t1"); err != nil {
				t.Fatal(err)
			}
			trips, _ := sig.got()
			var bounds []time.Duration
			for _, tr := range trips {
				bounds = append(bounds, tr.Bound)
			}
			if !slices.Equal(bounds, tc.want) {
				t.Fatalf("trips bound %v, want %v", bounds, tc.want)
			}
		})
	}
}

// The model review and the audit tell their watch the waves their calls
// run in, reviewParallel at once, before the first call: four files of
// 40 KiB are four parts in one wave, told after the watch bound the run
// at policy.ReviewParts; five parts are two waves.
func TestReviewAndAuditBoundThemselvesByTheirWaves(t *testing.T) {
	five := fourParts + diffFile("e.go", 40<<10)
	for _, tc := range []struct {
		name, activity, diff, answer string
		want                         []int
	}{
		{"a review of four parts", ActivityModelReview, fourParts, `{"verdict":"approve","summary":"ok"}`, []int{3, 1}},
		{"a review of five parts", ActivityModelReview, five, `{"verdict":"approve","summary":"ok"}`, []int{3, 2}},
		{"an audit of four parts", ActivityGateAudit, fourParts, `{"verdict":"clear"}`, []int{3, 1}},
		{"an audit of five parts", ActivityGateAudit, five, `{"verdict":"clear"}`, []int{3, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newAuditRunner(t, &gateForgejo{diff: tc.diff}, &fakeLiteLLM{content: tc.answer}, true)
			var mu sync.Mutex
			var waves []int
			env := signallingEnv(&GateWatch{Store: r.store, Bound: func(_ context.Context, activity string, _ int64, n int) time.Duration {
				if activity == tc.activity {
					mu.Lock()
					defer mu.Unlock()
					waves = append(waves, n)
				}
				return time.Hour
			}})
			r.RegisterActivities(env)
			if _, err := env.ExecuteActivity(tc.activity, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(waves, tc.want) {
				t.Fatalf("bounds asked for %v waves, want %v", waves, tc.want)
			}
		})
	}
}

// retry ends the running attempt with a retryable error, so the
// activity's retry policy runs it again.
func TestGateWatchRetryEndsTheAttemptForARetry(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
	sig := &signalled{}
	g := &GateWatch{Store: store, Bound: func(context.Context, string, int64, int) time.Duration { return 50 * time.Millisecond }, Signal: sig.signal}
	var mu sync.Mutex
	running := false
	sig.onTrip = func(wf string, tr GateTrip) {
		mu.Lock()
		// another attempt of the run is not the one that tripped
		running = g.Running(wf, tr.ActivityID, tr.Attempt) && !g.Running(wf, tr.ActivityID, tr.Attempt+1) && !g.Retry(wf, tr.ActivityID, tr.Attempt+1)
		mu.Unlock()
		g.Retry(wf, tr.ActivityID, tr.Attempt)
	}
	env := signallingEnv(g)
	env.RegisterActivityWithOptions(func(ctx context.Context, _, _ string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}, activity.RegisterOptions{Name: ActivityWaitChecks})
	_, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1")
	var app *temporal.ApplicationError
	mu.Lock()
	defer mu.Unlock()
	if !running || !errors.As(err, &app) || app.Type() != gateRetryError || app.NonRetryable() {
		t.Fatalf("running = %v, err = %v; want a retryable %s", running, err, gateRetryError)
	}
}

// Every activity factoryd registers may carry a policy bound or takes the
// default one, and the policy knows no other.
func TestBoundActivitiesAreTheRegisteredOnes(t *testing.T) {
	var names []string
	(&Runner{}).RegisterActivities(nameRecorder(func(n string) { names = append(names, n) }))
	slices.Sort(names)
	want := slices.Sorted(slices.Values(append(slices.Clone(policy.BoundActivities), defaultBoundActivities...)))
	if !slices.Equal(names, want) {
		t.Fatalf("registered %v, policy bounds %v", names, want)
	}
}

type nameRecorder func(string)

func (f nameRecorder) RegisterActivityWithOptions(_ any, o activity.RegisterOptions) { f(o.Name) }

// The supervisor's action is its trip's verdict: only a continue or a
// gate's wait found nothing to act on.
func TestTripVerdict(t *testing.T) {
	for action, want := range map[SupervisorAction]string{
		ActionContinue: VerdictNoise, ActionRedirect: VerdictReal, ActionRetryStronger: VerdictReal, ActionEscalate: VerdictReal,
		ActionWait: VerdictNoise, ActionRetry: VerdictReal, ActionCancel: VerdictReal,
	} {
		if got := tripVerdict(action); got != want {
			t.Errorf("tripVerdict(%s) = %s, want %s", action, got, want)
		}
	}
}

// A session's bound follows the agent time of the version its task
// started under, not the current one; pin 0 reads the
// current version. A model review's or an audit's follows its diff's
// parts: one call's supervisor_timeout of 10 min each, and two minutes.
func TestPinnedBound(t *testing.T) {
	pinned, current := testPolicyVersion(), testPolicyVersion()
	pinned.Document.Limits.AgentTime, pinned.Document.Limits.SupervisorTimeout = 30*time.Minute, 10*time.Minute
	current.ID, current.Document.Limits.AgentTime = 8, 20*time.Minute
	bound := PinnedBound(func() policy.Version { return current },
		func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil })
	ctx := context.Background()
	for _, tc := range []struct {
		activity string
		pin      int64
		parts    int
		want     time.Duration
	}{
		// a session's bound is a large card's agent time and a minute
		{ActivityRunSession, 7, policy.ReviewParts, 46 * time.Minute},
		{ActivitySuperviseGate, 7, policy.ReviewParts, 12 * time.Minute},
		{ActivityRunSession, 0, policy.ReviewParts, 31 * time.Minute},
		// an unreadable pin falls back to the current version: a bound is
		// an alert threshold, never a reason to fail the activity
		{ActivityRunSession, 99, policy.ReviewParts, 31 * time.Minute},
		// a roll's waits end at their own bound, the longest agent time in
		// force as a large card holds it and half an hour: the alert only
		// past that and the slack
		{ActivityAwaitSessionGap, 0, policy.ReviewParts, 65 * time.Minute},
		{ActivityAwaitRollHold, 0, policy.ReviewParts, 65 * time.Minute},
		{ActivityModelReview, 7, 1, 12 * time.Minute},
		{ActivityGateAudit, 7, 2, 22 * time.Minute},
		{ActivityModelReview, 7, 3, 32 * time.Minute},
		{ActivityRunSession, 7, 1, 46 * time.Minute},
	} {
		if got := bound(ctx, tc.activity, tc.pin, tc.parts); got != tc.want {
			t.Errorf("bound(%s, pin %d, %d parts) = %s, want %s", tc.activity, tc.pin, tc.parts, got, tc.want)
		}
	}
}

// A review of 25 waves at a supervisor_timeout of 5 min is bound at
// 25 x 5 + 2 = 127 minutes.
func TestPinnedBoundFollowsManyWaves(t *testing.T) {
	v := testPolicyVersion()
	v.Document.Limits.SupervisorTimeout = 5 * time.Minute
	bound := PinnedBound(func() policy.Version { return v },
		func(context.Context, int64) (policy.Version, bool, error) { return v, false, nil })
	if got := bound(context.Background(), ActivityModelReview, 0, 25); got != 127*time.Minute {
		t.Fatalf("bound = %s, want 2h7m0s", got)
	}
}

// A review's or an audit's bound stays ten minutes under the six hours
// Temporal gives its attempt, so the watch trips and the supervisor
// decides before the attempt is ended: 100 waves at 5 min would be
// 502 minutes, and are 350. Sixty waves, 302 minutes, fit; a session's
// bound is its own.
func TestPinnedBoundStaysUnderTheFanOutsAttempt(t *testing.T) {
	v := testPolicyVersion()
	v.Document.Limits.SupervisorTimeout, v.Document.Limits.AgentTime = 5*time.Minute, 400*time.Minute
	bound := PinnedBound(func() policy.Version { return v },
		func(context.Context, int64) (policy.Version, bool, error) { return v, false, nil })
	for _, tc := range []struct {
		activity string
		waves    int
		want     time.Duration
	}{
		{ActivityModelReview, 100, 350 * time.Minute},
		{ActivityGateAudit, 100, 350 * time.Minute},
		{ActivityModelReview, 60, 302 * time.Minute},
		{ActivityRunSession, 100, 601 * time.Minute},
	} {
		if got := bound(context.Background(), tc.activity, 0, tc.waves); got != tc.want {
			t.Errorf("bound(%s, %d waves) = %s, want %s", tc.activity, tc.waves, got, tc.want)
		}
	}
}

// The watch hands the bound the pin of the activity's input.
func TestActivityPin(t *testing.T) {
	for _, tc := range []struct {
		args []any
		want int64
	}{
		{[]any{SessionInput{TaskID: "t1", PolicyVersion: 7}}, 7},
		{[]any{"t1", GateSupervision{PolicyVersion: 5}}, 5},
		{[]any{"t1", PullRequest{Number: 9}, "c1", GateResults{Pin: 4}}, 4},
		{[]any{"t1", int64(9), "c1"}, 0},
		{nil, 0},
	} {
		if got := activityPin(tc.args); got != tc.want {
			t.Errorf("activityPin(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}

// [no_trip_while_ci_runs] The checks wait's bound is its project's waits.checks
// under the pinned version, never the old code default of 10 min: a wait of
// a project that sets 12m is bound at 12m, so no alert has been written at
// 10m, and the one the bound's timer writes carries Bound 12m0s and trips
// the workflow; a project that sets none is bound at policy.yaml's 40m.
func TestGateWatchChecksWaitTripsAtTheProjectBound(t *testing.T) {
	pinned := fixtureVersion(t)
	demo := pinned.Projects["demo-app"]
	demo.Waits = &policy.Waits{Checks: 12 * time.Minute}
	pinned.Projects["demo-app"] = demo
	for repository, want := range map[string]time.Duration{"demo-app/demo-app": 12 * time.Minute, "linkshort/linkshort": 40 * time.Minute} {
		store := newFakeStore()
		store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct", Repository: repository})
		sig := &signalled{}
		g := &GateWatch{Store: store, Signal: sig.signal, Bound: PinnedBound(func() policy.Version { return pinned },
			func(context.Context, int64) (policy.Version, bool, error) { return pinned, false, nil })}
		env := signallingEnv(g)
		var bound time.Duration
		var alertsBefore int
		env.RegisterActivityWithOptions(func(ctx context.Context, _, _ string) error {
			info := activity.GetInfo(ctx)
			g.mu.Lock()
			run := g.runs[runKey(info.WorkflowExecution.ID, info.ActivityID)]
			g.mu.Unlock()
			bound, alertsBefore = run.bound, len(store.alertRows())
			run.raise(run.bound, false)
			return nil
		}, activity.RegisterOptions{Name: ActivityWaitChecks})
		if _, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1"); err != nil {
			t.Fatal(err)
		}
		if bound != want || alertsBefore != 0 {
			t.Fatalf("%s: bound %s with %d alerts before it, want %s and none (10m is inside it)", repository, bound, alertsBefore, want)
		}
		trips, _ := sig.got()
		rows := store.alertRows()
		if len(trips) != 1 || trips[0].Bound != want || len(rows) != 1 || rows[0].BoundS != int(want.Seconds()) {
			t.Fatalf("%s: trips %+v, alerts %+v, want one of each at %s", repository, trips, rows, want)
		}
	}
}

// PinnedBound reads the checks wait's bound from the pinned version's
// waits row: 12m under waits.checks 12m, the 50m default unset.
func TestPinnedBoundOfTheChecksWaitIsTheWaitsRow(t *testing.T) {
	pinned, current := testPolicyVersion(), testPolicyVersion()
	pinned.Document.Waits.Checks = 12 * time.Minute
	current.ID = 8
	bound := PinnedBound(func() policy.Version { return current },
		func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil })
	ctx := context.Background()
	if got := bound(ctx, ActivityWaitChecks, 7, policy.ReviewParts); got != 12*time.Minute {
		t.Fatalf("pinned waits.checks 12m: bound = %s, want 12m0s", got)
	}
	if got := bound(ctx, ActivityWaitChecks, 0, policy.ReviewParts); got != 50*time.Minute {
		t.Fatalf("waits.checks unset: bound = %s, want 50m0s", got)
	}
}

// flakyTasks is a store whose task read fails its first n calls.
type flakyTasks struct {
	*fakeStore
	mu    sync.Mutex
	n     int
	calls int
}

func (f *flakyTasks) Task(ctx context.Context, id string) (Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.n > 0 {
		f.n--
		return Task{}, false, errors.New("store down")
	}
	return f.fakeStore.Task(ctx, id)
}

// A checks wait whose task cannot be read has no bound to trip at: it does
// not trip on the policy's, and each retry tick reads the task again. A
// task that stays unreadable never trips; one that reads on the third tick
// arms the project's bound (60 ms here) from the attempt's start.
func TestGateWatchChecksWaitRetriesItsTaskLookup(t *testing.T) {
	for name, tc := range map[string]struct {
		fails int
		trips int
	}{"never readable": {1 << 30, 0}, "readable on the third try": {2, 1}} {
		base := newFakeStore()
		base.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct", Repository: "demo-app/demo-app"})
		store := &flakyTasks{fakeStore: base, n: tc.fails}
		sig := &signalled{}
		bound := func(ctx context.Context, _ string, _ int64, _ int) time.Duration {
			if repo, _ := ctx.Value(repositoryKey{}).(string); repo != "demo-app/demo-app" {
				t.Errorf("%s: bound asked without the task's repository: %q", name, repo)
			}
			return 60 * time.Millisecond
		}
		env := signallingEnv(&GateWatch{Store: store, Bound: bound, Signal: sig.signal, LookupRetry: 20 * time.Millisecond})
		env.RegisterActivityWithOptions(func(context.Context, string, string) error { time.Sleep(300 * time.Millisecond); return nil },
			activity.RegisterOptions{Name: ActivityWaitChecks})
		if _, err := env.ExecuteActivity(ActivityWaitChecks, "t1", "c1"); err != nil {
			t.Fatal(err)
		}
		if trips, _ := sig.got(); len(trips) != tc.trips || len(base.alertRows()) != tc.trips {
			t.Fatalf("%s: trips %+v, alerts %d, want %d", name, trips, len(base.alertRows()), tc.trips)
		}
	}
}

// A retry tick arms only the run that is the table's: a later attempt
// took the same key while the old run's context is still live, and the old
// run's tick must not arm anything.
func TestGateWatchLookupTickArmsOnlyTheRunInTheTable(t *testing.T) {
	base := newFakeStore()
	base.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct", Repository: "demo-app/demo-app"})
	g := &GateWatch{Store: base, Bound: func(context.Context, string, int64, int) time.Duration { return time.Hour }}
	gone := &watchedRun{activity: ActivityWaitChecks, started: time.Now()}
	g.retryBound(gone, "wf/1", context.Background(), "t1", 0)
	if gone.timer != nil || gone.bound != 0 {
		t.Fatalf("a run out of the table was armed: bound %s, timer %v", gone.bound, gone.timer)
	}
	live := &watchedRun{activity: ActivityWaitChecks, started: time.Now()}
	g.runs = map[string]*watchedRun{"wf/1": live}
	g.retryBound(live, "wf/1", context.Background(), "t1", 0)
	if live.bound != time.Hour || live.timer == nil {
		t.Fatalf("the run in the table was not armed: bound %s, timer %v", live.bound, live.timer)
	}
	live.timer.Stop()
}

// A run past a trip and a wait takes a later tell that is more than twice
// its bound: a review told one wave of 100 ms trips at 100 ms, its wait
// extends to 200 ms, and a settle that tells ten waves (1 s) is not tripped
// again at 200 ms.
func TestGateWatchExtendedRunTakesALargerTell(t *testing.T) {
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
	sig := &signalled{}
	g := &GateWatch{Store: store, Bound: func(_ context.Context, _ string, _ int64, parts int) time.Duration {
		return time.Duration(parts) * 100 * time.Millisecond
	}, Signal: sig.signal}
	sig.onTrip = func(wf string, tr GateTrip) { g.Wait(wf, tr.ActivityID, tr.Attempt) }
	env := signallingEnv(g)
	env.RegisterActivityWithOptions(func(ctx context.Context, _ string) error {
		boundByParts(ctx, 1)
		time.Sleep(150 * time.Millisecond)
		boundByParts(ctx, 10)
		time.Sleep(300 * time.Millisecond)
		return nil
	}, activity.RegisterOptions{Name: ActivityModelReview})
	if _, err := env.ExecuteActivity(ActivityModelReview, "t1"); err != nil {
		t.Fatal(err)
	}
	if trips, _ := sig.got(); len(trips) != 1 {
		t.Fatalf("trips = %+v, want the one at 100 ms and none at 200 ms", trips)
	}
}

// watchRun runs a model review of the tells and sleeps it makes at 100 ms
// (scale) a wave, a wait taken waitAfter after each trip, and answers the
// trips and alerts.
func watchRun(t *testing.T, scale, waitAfter time.Duration, steps func(ctx context.Context)) ([]GateTrip, []Alert) {
	t.Helper()
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", Status: "in-review", Track: "direct"})
	sig := &signalled{}
	g := &GateWatch{Store: store, Bound: func(_ context.Context, _ string, _ int64, parts int) time.Duration {
		return time.Duration(parts) * scale
	}, Signal: sig.signal}
	sig.onTrip = func(wf string, tr GateTrip) {
		time.Sleep(waitAfter)
		g.Wait(wf, tr.ActivityID, tr.Attempt)
	}
	env := signallingEnv(g)
	env.RegisterActivityWithOptions(func(ctx context.Context, _ string) error { steps(ctx); return nil }, activity.RegisterOptions{Name: ActivityModelReview})
	if _, err := env.ExecuteActivity(ActivityModelReview, "t1"); err != nil {
		t.Fatal(err)
	}
	trips, _ := sig.got()
	return trips, store.alertRows()
}

// After a wait at the trip, a tell of at most twice the bound leaves the
// extended deadline and the timer armed: one wave of 100 ms trips at 100
// ms, the wait extends to 200 ms, a two-wave tell at 150 ms keeps 200 ms,
// and a run to 350 ms trips again at 200 ms with that bound.
func TestGateWatchSmallTellAfterAWaitKeepsTheExtendedTrip(t *testing.T) {
	trips, alerts := watchRun(t, 100*time.Millisecond, 0, func(ctx context.Context) {
		boundByParts(ctx, 1)
		time.Sleep(150 * time.Millisecond)
		boundByParts(ctx, 2)
		time.Sleep(200 * time.Millisecond)
	})
	if len(trips) != 2 || trips[1].Bound != 200*time.Millisecond || len(alerts) != 2 || alerts[1].Detail["extended"] != true {
		t.Fatalf("trips = %+v, alerts = %+v; want a second, extended trip at 200 ms", trips, alerts)
	}
}

// A tell of ten waves after the wait moves the deadline out to 1 s, and
// the trip there carries that bound, extended: the bound stored is half of
// it, so the doubled extended bound is the told one.
func TestGateWatchLargeTellAfterAWaitTripsAtItsBound(t *testing.T) {
	trips, alerts := watchRun(t, 100*time.Millisecond, 0, func(ctx context.Context) {
		boundByParts(ctx, 1)
		time.Sleep(150 * time.Millisecond)
		boundByParts(ctx, 10)
		time.Sleep(1050 * time.Millisecond)
	})
	if len(trips) != 2 || trips[1].Bound != time.Second || len(alerts) != 2 || alerts[1].Detail["extended"] != true {
		t.Fatalf("trips = %+v, alerts = %+v; want a second, extended trip at 1 s", trips, alerts)
	}
}

// A tell between the trip and the wait is kept for the wait: one wave
// trips at 200 ms (scale), the wait comes 160 ms later, ten waves were
// told in between, so the wait arms the told 2 s, not 400 ms.
func TestGateWatchTellBetweenTripAndWaitIsKept(t *testing.T) {
	trips, _ := watchRun(t, 200*time.Millisecond, 160*time.Millisecond, func(ctx context.Context) {
		boundByParts(ctx, 1)
		time.Sleep(250 * time.Millisecond)
		boundByParts(ctx, 10)
		time.Sleep(300 * time.Millisecond)
	})
	if len(trips) != 1 {
		t.Fatalf("trips = %+v, want the one at 200 ms", trips)
	}
}

// A wait taken late arms its half bound beyond the doubled bound; a small
// tell after it never moves the trip earlier: trip at 200 ms, wait at
// 360 ms arms 460 ms, a two-wave tell at 380 ms would give 400 ms, and the
// run ends at 440 ms untripped.
func TestGateWatchTellNeverMovesTheTripInward(t *testing.T) {
	trips, _ := watchRun(t, 200*time.Millisecond, 160*time.Millisecond, func(ctx context.Context) {
		boundByParts(ctx, 1)
		time.Sleep(380 * time.Millisecond)
		boundByParts(ctx, 2)
		time.Sleep(60 * time.Millisecond)
	})
	if len(trips) != 1 {
		t.Fatalf("trips = %+v, want the one at 200 ms", trips)
	}
}

// The stored deadline holds: trip at 100 ms, the wait extends to 200 ms, a
// one-wave tell at 110 ms would arm 100 ms or, by the half-bound floor, 160
// ms, and the run ends at 180 ms untripped.
func TestGateWatchTellKeepsTheExtendedDeadline(t *testing.T) {
	trips, _ := watchRun(t, 100*time.Millisecond, 0, func(ctx context.Context) {
		boundByParts(ctx, 1)
		time.Sleep(110 * time.Millisecond)
		boundByParts(ctx, 1)
		time.Sleep(70 * time.Millisecond)
	})
	if len(trips) != 1 {
		t.Fatalf("trips = %+v, want the one at 100 ms", trips)
	}
}
