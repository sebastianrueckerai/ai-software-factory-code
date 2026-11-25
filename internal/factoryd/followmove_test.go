package factoryd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

const checkContext = "config-check / check (pull_request)"

// fakeAlertRows are the alert rows the judgment reads.
type fakeAlertRows struct {
	mu   sync.Mutex
	rows []Alert
}

func (f *fakeAlertRows) NewestAlertRows(context.Context, string, string) ([]Alert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Alert(nil), f.rows...), nil
}

// moveClock is the runner's clock of a judgment: a pause moves it, so the
// waits cost no time.
type moveClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *moveClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *moveClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

// moveRun is a follow run with its pull request opened by the pass, the
// gateway of the model call, the alerts and the probes of the judgment, and
// a clock of its own.
type moveRun struct {
	*followRun
	in      FollowMoveInput
	clock   *moveClock
	alerts  *fakeAlertRows
	probe   []string
	gateway int
}

func newMoveRun(t *testing.T) *moveRun {
	t.Helper()
	quickMergeWaits(t)
	m := &moveRun{followRun: newFollowRun(t, behindTree()), clock: &moveClock{now: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}, alerts: &fakeAlertRows{}, gateway: http.StatusOK}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(m.gateway)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\": true}"}}]}`))
	}))
	t.Cleanup(gw.Close)
	m.r.supervisor = &SupervisorModel{GatewayURL: gw.URL, Budget: briefTestBudget}
	m.r.now, m.r.sleep = m.clock.Now, m.clock.sleep
	m.r.Follow.Alerts = m.alerts
	m.r.Follow.Probe = func(context.Context) []string { return m.probe }
	m.run(t)
	if len(m.moves.started) != 1 {
		t.Fatalf("the pass started %v", m.moves.started)
	}
	m.in = m.moves.started[0]
	m.forge.statuses["head40"] = []CommitStatus{{State: "success", Context: checkContext}}
	m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "success", Context: "flux/platform"}}
	return m
}

// workflow runs the move's workflow over the runner's activities.
func (m *moveRun) workflow(t *testing.T) {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(FollowMoveWorkflow, workflow.RegisterOptions{Name: "FollowMove"})
	m.r.RegisterActivities(env)
	env.ExecuteWorkflow("FollowMove", m.in)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("completed %v, err %v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
}

// judge runs the judgment of the merge commit the move made.
func (m *moveRun) judge(t *testing.T) FollowVerdict {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(m.r.FollowJudge, activity.RegisterOptions{Name: ActivityFollowJudge})
	val, err := env.ExecuteActivity(ActivityFollowJudge, m.in, FollowLanded{Commit: "merge40", At: m.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var v FollowVerdict
	if err := val.Get(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (m *moveRun) lastRun(t *testing.T) RoutineRun {
	t.Helper()
	runs, _ := m.store.RoutineRuns(context.Background(), infraName, "follow", 1)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	return runs[0]
}

func (m *moveRun) steps() []string {
	var out []string
	for _, e := range recordedEvents(m.r, EventFollowStep) {
		out = append(out, e.Detail["step"].(string))
	}
	return out
}

// A move of lane alone: the checks are green, the bot merges, Flux's statuses
// are success on the merge commit, the dependencies and the small class
// answer, nothing alerts for the settle time, and the next run starts at
// once. Each step is a comment on the pull request and an event.
func TestFollowMoveAloneGoodPath(t *testing.T) {
	m := newMoveRun(t)
	m.workflow(t)
	if pr := m.forge.pulls[40]; !pr.Merged {
		t.Fatalf("pull request #40 = %+v, want merged", pr)
	}
	want := []string{
		"the checks of pull request head head40 are green.",
		"merged as merge40.",
		"Flux applied merge40: flux/flux-system, flux/platform are success.",
		"the dependencies answer and the small model class answers.",
		"no alert of the project's namespaces fired in the 10m0s after the merge: the move is settled.",
		"done: moved: ghcr.io/berriai/litellm: 1.102.4 to 1.108.17",
	}
	if got := m.steps(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps = %q, want %q", got, want)
	}
	for i, c := range m.forge.comments[40] {
		if c != "Follow routine: "+want[i] {
			t.Errorf("comment %d = %q", i, c)
		}
	}
	run := m.lastRun(t)
	if run.Outcome != "done" || run.Note != "part litellm: moved: ghcr.io/berriai/litellm: 1.102.4 to 1.108.17" {
		t.Fatalf("run = %+v", run)
	}
	if len(m.wf.routineRuns) != 1 || m.wf.routineRuns[0].Trigger != TriggerRunNow || m.wf.routineRuns[0].Routine != "follow" {
		t.Fatalf("next runs = %+v, want one run-now of the follow routine", m.wf.routineRuns)
	}
}

// A red check before the merge ends the move: nothing merges, the run says
// which check and its state.
func TestFollowMoveRedCheckBeforeTheMerge(t *testing.T) {
	m := newMoveRun(t)
	m.forge.statuses["head40"] = []CommitStatus{{State: "failure", Context: checkContext}}
	m.workflow(t)
	if m.forge.pulls[40].Merged {
		t.Fatal("a red pull request merged")
	}
	run := m.lastRun(t)
	if run.Outcome != "failed" || run.Note != "part litellm: the check “config-check / check (pull_request)” of its head head40 has the state failure" {
		t.Fatalf("run = %+v", run)
	}
	if len(m.wf.routineRuns) != 0 {
		t.Fatalf("a failed move started a run: %+v", m.wf.routineRuns)
	}
}

// A check the project names that has not reported is waited for: a head with
// no status yet does not merge unchecked, and one that reports in time
// does. Past the bound the missing check is the reason.
func TestFollowChecksHoldUntilTheNamedCheckReports(t *testing.T) {
	m := newMoveRun(t)
	delete(m.forge.statuses, "head40")
	landChecksBound, landChecksPoll, landNoStatusGrace = 2*time.Second, time.Millisecond, time.Millisecond
	go func() {
		time.Sleep(30 * time.Millisecond)
		m.forge.mu.Lock()
		m.forge.statuses["head40"] = []CommitStatus{{State: "success", Context: checkContext}}
		m.forge.mu.Unlock()
	}()
	if v, err := m.r.FollowChecks(context.Background(), m.in); err != nil || !v.Ok {
		t.Fatalf("a check that reports in time: %+v, %v", v, err)
	}
	delete(m.forge.statuses, "head40")
	m.forge.statuses["head40"] = []CommitStatus{{State: "success", Context: "other"}}
	landChecksBound = 20 * time.Millisecond
	v, err := m.r.FollowChecks(context.Background(), m.in)
	if err != nil || v.Ok || v.Why != "the check “config-check” of its head head40 has not reported within 20ms" {
		t.Fatalf("a check that never reports: %+v, %v", v, err)
	}
}

// Each way a judgment fails names what failed with its text.
func TestFollowJudgeFailures(t *testing.T) {
	at := func(m *moveRun, d time.Duration) string { return m.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	for name, tc := range map[string]struct {
		setup func(m *moveRun)
		want  string
	}{
		"a failed status": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "failure", Context: "flux/flux-system", Description: "HelmRelease platform/litellm: upgrade retries exhausted"}, {State: "success", Context: "flux/platform"}}
		}, "the status “flux/flux-system” of the merge commit merge40 is failure: HelmRelease platform/litellm: upgrade retries exhausted"},
		"an errored status": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "error", Context: "flux/platform", Description: "health check failed"}}
		}, "the status “flux/platform” of the merge commit merge40 is error: health check failed"},
		"a status that never reports": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}}
		}, "the merge commit merge40 has no success status within 30m0s: flux/platform has not reported"},
		"a status that stays pending": {func(m *moveRun) {
			m.forge.mergeStatus = []CommitStatus{{State: "success", Context: "flux/flux-system"}, {State: "pending", Context: "flux/platform"}}
		}, "the merge commit merge40 has no success status within 30m0s: flux/platform is pending"},
		"a dependency probe": {func(m *moveRun) { m.probe = []string{"openbao: sealed"} },
			"a dependency does not answer: openbao: sealed"},
		"the model call": {func(m *moveRun) { m.gateway = http.StatusServiceUnavailable },
			"the small model class does not answer through the gateway: "},
		"an alert in the settle time": {func(m *moveRun) {
			m.alerts.rows = []Alert{{Kind: AlertSigNoz, Source: "5xx rate above 5 %", Detail: map[string]any{"namespace": "platform", "starts_at": at(m, 5*time.Minute), "summary": "5xx at 7 %"}}}
		}, "the alert “5xx rate above 5 %” of namespace platform fired after the merge: 5xx at 7 %"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMoveRun(t)
			m.forge.main = "merge40"
			tc.setup(m)
			m.forge.statuses["merge40"] = m.forge.mergeStatus
			if v := m.judge(t); v.Ok || !strings.HasPrefix(v.Why, tc.want) {
				t.Fatalf("verdict = %+v, want %q", v, tc.want)
			}
		})
	}
	t.Run("alerts that do not count", func(t *testing.T) {
		m := newMoveRun(t)
		m.forge.statuses["merge40"] = m.forge.mergeStatus
		m.alerts.rows = []Alert{
			{Kind: AlertSigNoz, Source: "before the merge", Detail: map[string]any{"namespace": "platform", "starts_at": at(m, -time.Minute)}},
			{Kind: AlertSigNoz, Source: "another namespace", Detail: map[string]any{"namespace": "factory", "starts_at": at(m, time.Minute)}},
			{Kind: AlertSigNoz, Source: "noise", Verdict: VerdictNoise, Detail: map[string]any{"namespace": "platform", "starts_at": at(m, time.Minute)}},
		}
		if v := m.judge(t); !v.Ok {
			t.Fatalf("verdict = %+v, want a good move", v)
		}
	})
	t.Run("a held row counts", func(t *testing.T) {
		m := newMoveRun(t)
		m.forge.statuses["merge40"] = m.forge.mergeStatus
		m.alerts.rows = []Alert{{Kind: AlertSigNoz, Source: "restarts", Detail: map[string]any{"namespace": "forgejo", "starts_at": at(m, time.Minute), "roll_window": true, "summary": "pods restart"}}}
		if v := m.judge(t); v.Ok || !strings.Contains(v.Why, "“restarts” of namespace forgejo") {
			t.Fatalf("verdict = %+v, want the held alert named", v)
		}
	})
}
