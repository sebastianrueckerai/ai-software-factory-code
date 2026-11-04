package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

// staleRig is one sweep's world: its cards, the workflows it signals and
// the timelines it reads.
type staleRig struct {
	store   *fakeStore
	wf      *fakeWorkflows
	objects *fakeObjectStore
}

func newStaleRig(t *testing.T, ages map[string]time.Duration) *staleRig {
	t.Helper()
	store := newFakeStore()
	for id, age := range ages {
		status := map[string]string{"t-run": "running", "t-wait": "waiting-poster", "t-fresh": "running",
			"t-deps": "waiting-deps", "t-chat": "running", "t-queued": "queued"}[id]
		if status == "" {
			t.Fatalf("unknown stale rig card %s", id)
		}
		store.CreateTask(context.Background(), Task{ID: id, Status: status, Kind: map[string]string{"t-chat": KindChat}[id],
			Repository: "sf/code", IssueNumber: 42, CreatedAt: time.Now().Add(-age), Track: "direct"})
	}
	return &staleRig{store: store, wf: &fakeWorkflows{}, objects: &fakeObjectStore{}}
}

// A live-status card quiet for the window is nudged — the signal starts a
// workflow that died — once, not again on the next pass; the queue and
// the chat task owe no sweep, and a card whose session wrote a fresh
// timeline step is not quiet at all.
func TestStaleSweepNudgesAQuietCardOnce(t *testing.T) {
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 45 * time.Minute, "t-fresh": 45 * time.Minute,
		"t-chat": 45 * time.Minute, "t-queued": 45 * time.Minute})
	// t-fresh's session wrote its start two minutes ago
	line, err := json.Marshal(map[string]any{"step": "session_start", "time": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.objects.Put(context.Background(), "sessions/t-fresh/timeline.jsonl", append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	nudged := map[string]time.Time{}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.objects, workflowsRunning(), nudged)
	if want := []string{"t-run stale_nudge factoryd #0: quiet for 45 min while running"}; !slices.Equal(rig.wf.signals, want) {
		t.Fatalf("signals = %q, want the quiet running card nudged alone", rig.wf.signals)
	}
	if n, ok := nudged["t-run"]; !ok || time.Since(n) > time.Minute {
		t.Fatalf("nudged = %v, want t-run remembered at now", nudged)
	}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.objects, workflowsRunning(), nudged)
	if len(rig.wf.signals) != 1 {
		t.Fatalf("signals = %q, want no second nudge within the grace", rig.wf.signals)
	}
}

// workflowsRunning is the sweep's Temporal read with only ids running.
func workflowsRunning(ids ...string) func(context.Context, string) (bool, error) {
	return func(_ context.Context, id string) (bool, error) { return slices.Contains(ids, id), nil }
}

// A card whose workflow runs is left alone in any state, however quiet:
// a wait on a dependency, a human or a timer is by design. Its nudge
// memory clears. A Temporal error nudges nobody and keeps the memory.
func TestStaleSweepLeavesALiveWorkflowAlone(t *testing.T) {
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 70 * time.Minute, "t-deps": 5 * time.Hour, "t-wait": 70 * time.Minute})
	old := time.Now().Add(-StaleWindow - time.Minute)
	nudged := map[string]time.Time{"t-run": old, "t-deps": old}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.objects, workflowsRunning("t-run", "t-deps", "t-wait"), nudged)
	for _, id := range []string{"t-run", "t-deps"} {
		if _, ok := nudged[id]; ok {
			t.Fatalf("%s still in nudged: a running workflow clears the memory", id)
		}
	}
	if len(rig.wf.signals) != 0 {
		t.Fatalf("the sweep nudged live workflows: %q", rig.wf.signals)
	}

	down := func(context.Context, string) (bool, error) { return false, errors.New("temporal down") }
	nudged = map[string]time.Time{"t-wait": old}
	StaleSweep(context.Background(), rig.store, rig.wf, rig.objects, down, nudged)
	if task, _, _ := rig.store.Task(context.Background(), "t-wait"); task.Status != "waiting-poster" || len(rig.wf.signals) != 0 {
		t.Fatalf("t-wait = %s after %q: an unreadable Temporal must skip the card", task.Status, rig.wf.signals)
	}
	if _, ok := nudged["t-wait"]; !ok {
		t.Fatalf("nudge memory was cleared on a Temporal error")
	}
}

// A dead card still quiet a window after its nudge is nudged again, and
// the sweep asks no human: each card keeps its status, and no ask or
// alert is written. The monitor's run_quiet flag faces the quiet running
// card and its dependency bound the waiting-deps card. The waiting-poster
// card has no wait the monitor owns: its nudge and the board's stale ring
// face it. The next pass, within the new nudges' grace, sends nothing.
func TestStaleSweepNudgesAndAsksNothing(t *testing.T) {
	ctx := context.Background()
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 70 * time.Minute, "t-wait": 70 * time.Minute, "t-deps": 5 * time.Hour})
	old := time.Now().Add(-StaleWindow - time.Minute)
	nudged := map[string]time.Time{"t-run": old, "t-wait": old, "t-deps": old}
	StaleSweep(ctx, rig.store, rig.wf, rig.objects, workflowsRunning(), nudged)
	want := []string{
		"t-deps stale_nudge factoryd #0: quiet for 5 h 0 min while waiting on its dependencies",
		"t-run stale_nudge factoryd #0: quiet for 1 h 10 min while running",
		"t-wait stale_nudge factoryd #0: quiet for 1 h 10 min while waiting on the issue's author",
	}
	if got := slices.Sorted(slices.Values(rig.wf.signals)); !slices.Equal(got, want) {
		t.Fatalf("signals = %q, want %q", got, want)
	}
	for id, status := range map[string]string{"t-run": "running", "t-wait": "waiting-poster", "t-deps": "waiting-deps"} {
		if task, _, _ := rig.store.Task(ctx, id); task.Status != status {
			t.Fatalf("%s = %s, want %s: the sweep asks no human", id, task.Status, status)
		}
		if at, ok := nudged[id]; !ok || time.Since(at) > time.Minute {
			t.Fatalf("%s's nudge = %v (%v), want now", id, at, ok)
		}
	}
	if len(rig.store.intakeEvents) != 0 || len(rig.store.alerts) != 0 {
		t.Fatalf("the sweep asked: intake events %+v, alerts %+v", rig.store.intakeEvents, rig.store.alerts)
	}
	StaleSweep(ctx, rig.store, rig.wf, rig.objects, workflowsRunning(), nudged)
	if len(rig.wf.signals) != 3 {
		t.Fatalf("signals = %q, want no nudge within the grace", rig.wf.signals)
	}
}

// failingList is a sweep's store whose list of one status fails.
type failingList struct {
	*fakeStore
	status string
}

func (f failingList) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	if status == f.status {
		return nil, errors.New("list down")
	}
	return f.fakeStore.ListTasksByStatus(ctx, status)
}

// The nudge memory keeps only the cards the sweep lists: t-queued left
// the swept statuses and t-gone is no card at all, so both are forgotten,
// while t-run, nudged within its grace, is kept. A sweep whose list fails
// forgets nobody, since a card it could not read may still be listed.
func TestStaleSweepForgetsACardItNoLongerLists(t *testing.T) {
	ctx := context.Background()
	rig := newStaleRig(t, map[string]time.Duration{"t-run": 70 * time.Minute, "t-queued": 70 * time.Minute})
	recent := time.Now().Add(-time.Minute)
	nudged := map[string]time.Time{"t-run": recent, "t-queued": recent, "t-gone": recent}
	StaleSweep(ctx, failingList{rig.store, "waiting-poster"}, rig.wf, rig.objects, workflowsRunning(), nudged)
	if got := slices.Sorted(maps.Keys(nudged)); !slices.Equal(got, []string{"t-gone", "t-queued", "t-run"}) {
		t.Fatalf("nudged after a failed list = %q, want every key kept", got)
	}
	StaleSweep(ctx, rig.store, rig.wf, rig.objects, workflowsRunning(), nudged)
	if got := slices.Sorted(maps.Keys(nudged)); !slices.Equal(got, []string{"t-run"}) {
		t.Fatalf("nudged = %q, want only the listed t-run", got)
	}
	if !nudged["t-run"].Equal(recent) || len(rig.wf.signals) != 0 {
		t.Fatalf("t-run's nudge = %v, signals %q: within its grace it is kept and not sent again", nudged["t-run"], rig.wf.signals)
	}
}
