package factoryd

import (
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// gapClock is the roll gap's fake clock: each sleep advances it by the
// sleep's length and then runs onSleep with the number of sleeps so far.
type gapClock struct {
	mu      sync.Mutex
	at      time.Time
	sleeps  int
	onSleep func(n int)
}

var gapStart = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func newGapClock(r *Runner) *gapClock {
	c := &gapClock{at: gapStart}
	r.booted = gapStart.Add(-rollBoot)
	r.now = func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.at
	}
	r.sleep = func(ctx context.Context, d time.Duration) error {
		c.mu.Lock()
		c.at = c.at.Add(d)
		c.sleeps++
		n, f := c.sleeps, c.onSleep
		c.mu.Unlock()
		if f != nil {
			f(n)
		}
		return ctx.Err()
	}
	return c
}

func (c *gapClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// runActivity runs one of r's activities in a Temporal test activity
// environment.
func runActivity(t *testing.T, r *Runner, name string, args ...any) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	r.RegisterActivities(env)
	_, err := env.ExecuteActivity(name, args...)
	return err
}

// stepsOf are task id's timeline steps, each its name and the detail keys
// in keys.
func stepsOf(t *testing.T, r *Runner, id string, keys ...string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range timelineLines(t, r, id) {
		m := map[string]any{"step": line["step"]}
		for _, k := range keys {
			if v, ok := line[k]; ok {
				m[k] = v
			}
		}
		out = append(out, m)
	}
	return out
}

func rollGapRunner(t *testing.T, task Task) (*Runner, *fakeStore, *gapClock) {
	t.Helper()
	r, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	return r, store, newGapClock(r)
}

var rollTask = Task{ID: "r1", Kind: "agent", Source: SourceRoll, Status: "in-review", Repository: "sf/repo", Track: "direct"}

// The bound is the longest agent time in force, at any level a task's
// agent time can come from, plus half an hour.
func TestGapBound(t *testing.T) {
	v := testPolicyVersion()
	if got := gapBound(v); got != 40*time.Minute {
		t.Fatalf("bound of limits.agent_time 10m alone = %s, want 40m", got)
	}
	coder := v.Document.Classes["coder"]
	coder.Time = &policy.Time{AgentTime: 40 * time.Minute}
	v.Document.Classes = map[string]policy.Class{"coder": coder}
	p := v.Processes["maintenance"]
	p.Time = &policy.Time{AgentTime: time.Hour}
	v.Processes = map[string]policy.Process{"maintenance": p}
	v.Projects = map[string]policy.ProjectConfig{"a": {Time: &policy.Time{AgentTime: 2 * time.Hour}}, "b": {}}
	if got := gapBound(v); got != 2*time.Hour+30*time.Minute {
		t.Fatalf("bound with a 2h project = %s, want 2h30m", got)
	}
	v.Projects = nil
	if got := gapBound(v); got != time.Hour+30*time.Minute {
		t.Fatalf("bound with a 1h process = %s, want 1h30m", got)
	}
}

// The gap waits while any session runs here, polling every 15 s, and ends
// at the first poll after the last one ended; the roll's own task does not
// count. The hold stands from the wait's start to its bound.
func TestAwaitSessionGapWaitsForTheLastSession(t *testing.T) {
	r, store, c := rollGapRunner(t, rollTask)
	endA, endB := r.sessionRuns("a"), r.sessionRuns("b")
	defer r.sessionRuns("r1")()
	c.onSleep = func(n int) {
		switch n {
		case 2:
			endA()
		case 4:
			endB()
		}
	}
	if err := runActivity(t, r, ActivityAwaitSessionGap, "r1"); err != nil {
		t.Fatal(err)
	}
	if c.sleeps != 4 || !c.at.Equal(gapStart.Add(time.Minute)) {
		t.Fatalf("slept %d times, until %s; want 4 polls of 15s", c.sleeps, c.at)
	}
	want := []map[string]any{{"step": "roll_waiting", "live": []any{"a", "b"}}, {"step": "roll_gap", "waited_s": float64(60)}}
	if got := stepsOf(t, r, "r1", "live", "waited_s"); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if h := store.holds["r1"]; !h.Since.Equal(gapStart) || !h.Until.Equal(gapStart.Add(40*time.Minute)) {
		t.Fatalf("hold = %+v, want 12:00 to 12:40 (agent time 10m + 30m)", h)
	}
}

// With no session running the merge follows at once.
func TestAwaitSessionGapWithoutSessions(t *testing.T) {
	r, store, c := rollGapRunner(t, rollTask)
	if err := runActivity(t, r, ActivityAwaitSessionGap, "r1"); err != nil {
		t.Fatal(err)
	}
	if c.sleeps != 0 {
		t.Fatalf("slept %d times, want none", c.sleeps)
	}
	if got, want := stepsOf(t, r, "r1", "waited_s"), []map[string]any{{"step": "roll_gap", "waited_s": float64(0)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if _, ok := store.holds["r1"]; !ok {
		t.Fatal("no hold set")
	}
}

// At the bound the merge goes ahead with the sessions still running, the
// log says so, and the hold has lapsed.
func TestAwaitSessionGapMergesAtTheBound(t *testing.T) {
	r, store, c := rollGapRunner(t, rollTask)
	defer r.sessionRuns("a")()
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	if err := runActivity(t, r, ActivityAwaitSessionGap, "r1"); err != nil {
		t.Fatal(err)
	}
	if c.sleeps != 160 || !c.at.Equal(gapStart.Add(40*time.Minute)) {
		t.Fatalf("slept %d times, until %s; want 160 polls to the 40m bound", c.sleeps, c.at)
	}
	if !strings.Contains(logs.String(), "roll r1: no session gap within 40m0s: merging with [a] running") {
		t.Fatalf("log = %q", logs.String())
	}
	if h, ok, _ := store.RollHold(context.Background(), c.at); ok {
		t.Fatalf("hold %+v still in force at the bound", h)
	}
	want := []map[string]any{{"step": "roll_waiting", "live": []any{"a"}}, {"step": "roll_gap", "waited_s": float64(2400)}}
	if got := stepsOf(t, r, "r1", "live", "waited_s"); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

// A session waits while a hold is in force, uncounted by the roll's gap,
// with one held step; once the hold is cleared it starts and counts as
// live from then, until its RunSession takes over or a minute passes.
func TestAwaitRollHoldWaitsWhileAHoldIsInForce(t *testing.T) {
	r, store, c := rollGapRunner(t, Task{ID: "t1", Kind: "agent", Status: "running", Repository: "sf/repo", Track: "direct"})
	ctx := context.Background()
	if err := store.SetRollHold(ctx, "r1", gapStart, gapStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c.onSleep = func(n int) {
		if live := r.liveSessions("r1"); len(live) > 0 {
			t.Errorf("poll %d: live %v while held, want none", n, live)
		}
		if n == 3 {
			if err := store.ClearRollHold(ctx, "r1"); err != nil {
				t.Error(err)
			}
		}
	}
	if err := runActivity(t, r, ActivityAwaitRollHold, "t1"); err != nil {
		t.Fatal(err)
	}
	if c.sleeps != 3 {
		t.Fatalf("slept %d times, want 3", c.sleeps)
	}
	if got, want := stepsOf(t, r, "t1", "roll"), []map[string]any{{"step": "held", "roll": "r1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if live := r.liveSessions("r1"); !slices.Equal(live, []string{"t1"}) {
		t.Fatalf("live after the hold = %v, want t1", live)
	}
	c.advance(time.Minute)
	if live := r.liveSessions("r1"); len(live) > 0 {
		t.Fatalf("live a minute after the admission = %v, want none", live)
	}
}

// Neither a chat task nor the roll holding sessions waits.
func TestAwaitRollHoldPassesChatAndTheRollItself(t *testing.T) {
	for _, task := range []Task{
		{ID: "t1", Kind: KindChat, Status: "running", Repository: "sf/repo"},
		{ID: "r1", Kind: "agent", Source: SourceRoll, Status: "running", Repository: "sf/repo", Track: "direct"},
	} {
		r, store, c := rollGapRunner(t, task)
		if err := store.SetRollHold(context.Background(), "r1", gapStart, gapStart.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := runActivity(t, r, ActivityAwaitRollHold, task.ID); err != nil {
			t.Fatal(err)
		}
		if c.sleeps != 0 {
			t.Fatalf("%s %s waited %d polls, want none", task.Kind, task.ID, c.sleeps)
		}
		if b, err := r.timeline.Objects.Get(context.Background(), "sessions/"+task.ID+"/timeline.jsonl"); err != nil || len(b) > 0 {
			t.Fatalf("%s %s: timeline %q %v, want none", task.Kind, task.ID, b, err)
		}
	}
}

// A session counts as live from its RunSession's start, before its pod,
// to its end, and replaces its admission.
func TestRunSessionCountsAsLiveUntilItEnds(t *testing.T) {
	task := Task{ID: "t1", Status: "running", Objective: "fix it", Repository: "sf/repo", SHA: "base", Track: "direct"}
	kube := newFakeKube()
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	newGapClock(runner)
	if err := runActivity(t, runner, ActivityAwaitRollHold, "t1"); err != nil {
		t.Fatal(err)
	}
	var atPod []string
	kube.onCreatePod = func() { atPod = runner.liveSessions("r1") }
	if _, err := runSession(t, nil, runner, SessionInput{TaskID: "t1", Attempt: 1, Model: "factory/small"}, nil,
		pushedLine("c0ffee")+"\n"+resultLine(true, "done")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(atPod, []string{"t1"}) {
		t.Fatalf("live at the pod's creation = %v, want t1", atPod)
	}
	if live := runner.liveSessions("r1"); len(live) > 0 {
		t.Fatalf("live after the session = %v, want none", live)
	}
}

// A roll that ends unmerged ends its hold. (Its repository is no
// owner/name: Finish then skips the pull request, which the test hub does
// not serve.)
func TestFinishOfAnUnmergedRollEndsItsHold(t *testing.T) {
	task := rollTask
	task.Repository = "malformed"
	r, store, _ := rollGapRunner(t, task)
	ctx := context.Background()
	if err := store.SetRollHold(ctx, "r1", gapStart, gapStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "r1", "failed", "pull request closed without merge"); err != nil {
		t.Fatal(err)
	}
	if h, ok := store.holds["r1"]; ok {
		t.Fatalf("hold %+v after the roll failed", h)
	}
}

// A roll that ends merged holds sessions five minutes more, until the
// restart its merge brings (Flux reads main once a minute).
func TestFinishOfAMergedRollSettlesItsHold(t *testing.T) {
	task := rollTask
	task.PRNumber = 9
	r, store, _ := rollGapRunner(t, task)
	ctx := context.Background()
	if err := store.SetRollHold(ctx, "r1", gapStart.Add(-time.Hour), gapStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(ctx, "r1", "done", "pull request merged"); err != nil {
		t.Fatal(err)
	}
	if h := store.holds["r1"]; !h.Since.Equal(gapStart) || !h.Until.Equal(gapStart.Add(5*time.Minute)) {
		t.Fatalf("hold %+v after the roll merged, want 12:00 to 12:05", h)
	}
}

// The merge's end clears the hold, or after a merge that merged keeps it
// from now for five minutes.
func TestClearRollHold(t *testing.T) {
	r, store, _ := rollGapRunner(t, rollTask)
	ctx := context.Background()
	for _, merged := range []bool{false, true} {
		if err := store.SetRollHold(ctx, "r1", gapStart.Add(-time.Hour), gapStart.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := runActivity(t, r, ActivityClearRollHold, "r1", merged); err != nil {
			t.Fatal(err)
		}
		h, ok := store.holds["r1"]
		switch {
		case !merged && ok:
			t.Fatalf("hold %+v after an unmerged merge, want none", h)
		case merged && (!h.Since.Equal(gapStart) || !h.Until.Equal(gapStart.Add(5*time.Minute))):
			t.Fatalf("hold %+v after a merge, want 12:00 to 12:05", h)
		}
	}
}

// A factoryd just started knows no session yet: the ones the restart
// stopped register when Temporal retries them, so the gap waits three
// minutes from the start before it believes it.
func TestAwaitSessionGapRightAfterTheStart(t *testing.T) {
	r, _, c := rollGapRunner(t, rollTask)
	r.booted = gapStart
	if err := runActivity(t, r, ActivityAwaitSessionGap, "r1"); err != nil {
		t.Fatal(err)
	}
	if c.sleeps != 12 || !c.at.Equal(gapStart.Add(3*time.Minute)) {
		t.Fatalf("slept %d times, until %s; want 12 polls of 15s", c.sleeps, c.at)
	}
	want := []map[string]any{{"step": "roll_waiting", "live": []any{}}, {"step": "roll_gap", "waited_s": float64(180)}}
	if got := stepsOf(t, r, "r1", "live", "waited_s"); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}
