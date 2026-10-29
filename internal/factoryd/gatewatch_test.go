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
// gate only alert (ruling 162).
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
// told one part, it trips at 100 ms, the trip carrying that bound.
func TestGateWatchReviewBoundFollowsItsParts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts int // 0: the review tells none
		want  []time.Duration
	}{
		{"one part", 1, []time.Duration{100 * time.Millisecond}},
		{"no parts told", 0, nil},
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

// The model review and the audit tell their watch the parts their diff
// reads in before the first call: two files of 30 KiB are two parts, read
// after the watch bound the run at policy.ReviewParts parts; a diff beyond
// three parts makes no call and tells none.
func TestReviewAndAuditBoundThemselvesByTheirParts(t *testing.T) {
	two := diffFile("a.go", 30<<10) + diffFile("b.go", 30<<10)
	four := diffFile("a.go", 40<<10) + diffFile("b.go", 40<<10) + diffFile("c.go", 40<<10) + diffFile("d.go", 40<<10)
	for _, tc := range []struct {
		name, activity, diff, answer string
		want                         []int
	}{
		{"a review of two parts", ActivityModelReview, two, `{"verdict":"approve","summary":"ok"}`, []int{3, 2}},
		{"a review beyond its reach", ActivityModelReview, four, `{"verdict":"approve","summary":"ok"}`, []int{3}},
		{"an audit of two parts", ActivityGateAudit, two, `{"verdict":"clear"}`, []int{3, 2}},
		{"an audit beyond its reach", ActivityGateAudit, four, `{"verdict":"clear"}`, []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newAuditRunner(t, &gateForgejo{diff: tc.diff}, &fakeLiteLLM{content: tc.answer}, true)
			var mu sync.Mutex
			var parts []int
			env := signallingEnv(&GateWatch{Store: r.store, Bound: func(_ context.Context, activity string, _ int64, n int) time.Duration {
				if activity == tc.activity {
					mu.Lock()
					defer mu.Unlock()
					parts = append(parts, n)
				}
				return time.Hour
			}})
			r.RegisterActivities(env)
			if _, err := env.ExecuteActivity(tc.activity, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(parts, tc.want) {
				t.Fatalf("bounds asked for %v parts, want %v", parts, tc.want)
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
// started under, not the current one (ruling 183); pin 0 reads the
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
		{ActivityRunSession, 7, policy.ReviewParts, 31 * time.Minute},
		{ActivitySuperviseGate, 7, policy.ReviewParts, 12 * time.Minute},
		{ActivityRunSession, 0, policy.ReviewParts, 21 * time.Minute},
		// an unreadable pin falls back to the current version: a bound is
		// an alert threshold, never a reason to fail the activity
		{ActivityRunSession, 99, policy.ReviewParts, 21 * time.Minute},
		// a roll's waits end at their own bound, the longest agent time in
		// force and half an hour: the alert only past that and the slack
		{ActivityAwaitSessionGap, 0, policy.ReviewParts, 55 * time.Minute},
		{ActivityAwaitRollHold, 0, policy.ReviewParts, 55 * time.Minute},
		{ActivityModelReview, 7, 1, 12 * time.Minute},
		{ActivityGateAudit, 7, 2, 22 * time.Minute},
		{ActivityModelReview, 7, 3, 32 * time.Minute},
		{ActivityRunSession, 7, 1, 31 * time.Minute},
	} {
		if got := bound(ctx, tc.activity, tc.pin, tc.parts); got != tc.want {
			t.Errorf("bound(%s, pin %d, %d parts) = %s, want %s", tc.activity, tc.pin, tc.parts, got, tc.want)
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
