package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// recordingWorkflows records the starts, signals and routines' runs that
// reached the workflow layer.
type recordingWorkflows struct {
	starts      []string
	signals     []string
	routineRuns []RoutineRunInput
}

func (w *recordingWorkflows) Start(ctx context.Context, taskID string) error {
	w.starts = append(w.starts, taskID)
	return nil
}

func (w *recordingWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	w.signals = append(w.signals, taskID)
	return nil
}

func (w *recordingWorkflows) StartRoutineRun(ctx context.Context, in RoutineRunInput) error {
	w.routineRuns = append(w.routineRuns, in)
	return nil
}

// boundedFixture is a store of demo/app cards (bound at work_in_progress
// bound) and the work-bounded workflows over it.
func boundedFixture(t *testing.T, bound int, tasks ...Task) (*fakeStore, *recordingWorkflows, BoundedWorkflows) {
	t.Helper()
	store := newFakeStore()
	for _, task := range tasks {
		store.tasks[task.ID] = task
	}
	rec := &recordingWorkflows{}
	bounded := BoundedWorkflows{Workflows: rec, Store: store,
		Policy: func() policy.Tree {
			return policy.Tree{Projects: map[string]policy.ProjectConfig{
				"demo": {Repository: "demo/app", WorkInProgress: bound}}}
		}}
	return store, rec, bounded
}

func TestBoundedStartStaysQueuedOverTheBound(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued"})
	if err := bounded.Start(ctx, "t-q"); err != nil {
		t.Fatal(err)
	}
	if len(rec.starts) != 0 {
		t.Fatalf("starts = %v, want none over the bound", rec.starts)
	}
	if q, _, _ := store.Task(ctx, "t-q"); q.AdmittedAt != nil {
		t.Fatal("a card over the bound was stamped admitted")
	}
}

func TestBoundedStartAdmitsUnderTheBound(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := boundedFixture(t, 2,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued"})
	if err := bounded.Start(ctx, "t-q"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.starts, []string{"t-q"}) {
		t.Fatalf("starts = %v, want t-q", rec.starts)
	}
	q, _, _ := store.Task(ctx, "t-q")
	if q.AdmittedAt == nil {
		t.Fatal("an admitted card carries no stamp")
	}
	if n, _ := store.CountAdmitted(ctx, "demo/app"); n != 2 {
		t.Fatalf("admitted = %d, want the running card and the stamped one", n)
	}
	// a second start of the same card is the heal path: it goes through
	// and stamps nothing new
	if err := bounded.Start(ctx, "t-q"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.starts, []string{"t-q", "t-q"}) {
		t.Fatalf("starts = %v, want t-q twice", rec.starts)
	}
	again, _, _ := store.Task(ctx, "t-q")
	if !again.AdmittedAt.Equal(*q.AdmittedAt) {
		t.Fatal("the stamp moved on a second start")
	}
}

func TestBoundedStartTakesNoSlotFromALanelessCard(t *testing.T) {
	ctx := context.Background()
	_, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"},
		Task{ID: "t-roll", Repository: "demo/app", Kind: "agent", Lane: "", Status: "queued"})
	if err := bounded.Start(ctx, "t-roll"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.starts, []string{"t-roll"}) {
		t.Fatalf("starts = %v, want the roll: it runs no session and holds no slot", rec.starts)
	}
}

func TestBoundedStartHealsARunningCardOverTheBound(t *testing.T) {
	ctx := context.Background()
	_, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"})
	if err := bounded.Start(ctx, "t-run"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.starts, []string{"t-run"}) {
		t.Fatalf("starts = %v, want the heal: the card's slot is its own", rec.starts)
	}
}

func TestBoundedSignalKeepsAQueuedCardOverTheBound(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued"})
	if err := bounded.Signal(ctx, "t-q", "answer", TaskSignal{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.signals) != 0 {
		t.Fatalf("signals = %v, want none over the bound", rec.signals)
	}
	if q, _, _ := store.Task(ctx, "t-q"); q.Status != "queued" || q.AdmittedAt != nil {
		t.Fatalf("the card moved: %+v", q)
	}
}

func TestPullQueuedAdmitsInFilingOrder(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	// bound 3 with one card running leaves two slots: the two oldest
	// queued cards start, the third stays
	store, rec, bounded := boundedFixture(t, 3,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running", CreatedAt: base},
		Task{ID: "t-q3", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base.Add(3 * time.Minute)},
		Task{ID: "t-q1", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base.Add(1 * time.Minute)},
		Task{ID: "t-q2", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base.Add(2 * time.Minute)})
	pullQueued(ctx, store, bounded, "demo/app")
	if !slices.Equal(rec.starts, []string{"t-q1", "t-q2"}) {
		t.Fatalf("starts = %v, want the two oldest, the third over the bound", rec.starts)
	}
	if q3, _, _ := store.Task(ctx, "t-q3"); q3.AdmittedAt != nil {
		t.Fatal("the third card was stamped over the bound")
	}
}

// Forgejo stamps an issue's time in whole seconds, so a burst of filings
// shares one created_at; the issue number, not the random task id, breaks
// the tie (live 2026-10-14: #652 jumped #651 this way)
func TestPullQueuedAdmitsASameSecondBurstInIssueOrder(t *testing.T) {
	ctx := context.Background()
	base := time.Now().Truncate(time.Second)
	// one slot; both cards share base, and the later issue holds the
	// smaller id, so the id tie-break would take the wrong one
	store, rec, bounded := boundedFixture(t, 2,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running", CreatedAt: base.Add(-time.Minute)},
		Task{ID: "t-later-id", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base, IssueNumber: 652},
		Task{ID: "t-earlier-id", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base, IssueNumber: 651})
	pullQueued(ctx, store, bounded, "demo/app")
	if !slices.Equal(rec.starts, []string{"t-earlier-id"}) {
		t.Fatalf("starts = %v, want the smaller issue number of the same second", rec.starts)
	}
	if later, _, _ := store.Task(ctx, "t-later-id"); later.AdmittedAt != nil {
		t.Fatal("the same-second card with the larger issue took the only slot")
	}
}

func TestTaskEndedHandsTheFreedSlotToTheOldestQueuedCard(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	store, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-done", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "done", CreatedAt: base},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base.Add(time.Minute)})
	taskEnded(ctx, &Timeline{Objects: &fakeObjectStore{}}, store, nil, bounded, store.tasks["t-done"], "done", "")
	if !slices.Equal(rec.starts, []string{"t-q"}) {
		t.Fatalf("starts = %v, want the freed slot to start the queued card", rec.starts)
	}
}

// A card parked on a person holds no work slot (roadmap 233): moved to
// waiting-human it stops counting, so the slot it held frees at once.
func TestCountAdmittedDropsAWaitingHumanCard(t *testing.T) {
	ctx := context.Background()
	store, _, _ := boundedFixture(t, 2,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"},
		Task{ID: "t-wh", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"})
	before, err := store.CountAdmitted(ctx, "demo/app")
	if err != nil || before != 2 {
		t.Fatalf("admitted before = (%d, %v), want 2", before, err)
	}
	if err := store.TransitionTask(ctx, "t-wh", []string{"running"}, "waiting-human"); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountAdmitted(ctx, "demo/app"); n != before-1 {
		t.Fatalf("admitted with a card waiting on a human = %d, want %d (the parked card frees its slot)", n, before-1)
	}
}

// waiting-poster parks on a person too: a fund after a cap. It stops
// counting the same way.
func TestCountAdmittedDropsAWaitingPosterCard(t *testing.T) {
	ctx := context.Background()
	store, _, _ := boundedFixture(t, 2,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"},
		Task{ID: "t-wp", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"})
	before, err := store.CountAdmitted(ctx, "demo/app")
	if err != nil || before != 2 {
		t.Fatalf("admitted before = (%d, %v), want 2", before, err)
	}
	if err := store.TransitionTask(ctx, "t-wp", []string{"running"}, "waiting-poster"); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountAdmitted(ctx, "demo/app"); n != before-1 {
		t.Fatalf("admitted with a card waiting on its poster = %d, want %d (the parked card frees its slot)", n, before-1)
	}
}

// The freed slot admits the next queued card: a queued card starts while
// another card of the project sits in waiting-human.
func TestPullQueuedAdmitsWhileAWaitingHumanCardParks(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	store, rec, bounded := boundedFixture(t, 2,
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running", CreatedAt: base},
		Task{ID: "t-wh", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "waiting-human", CreatedAt: base},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base.Add(time.Minute)})
	pullQueued(ctx, store, bounded, "demo/app")
	if !slices.Equal(rec.starts, []string{"t-q"}) {
		t.Fatalf("starts = %v, want the queued card in the slot the parked card freed", rec.starts)
	}
	if q, _, _ := store.Task(ctx, "t-q"); q.AdmittedAt == nil {
		t.Fatal("the queued card carries no admission stamp")
	}
}

// The answer resumes the parked card as running even at a full bound
// (roadmap 233): the bound admits work, it does not guarantee a
// never-exceeded invariant; the overshoot lasts one gate pass and touches
// only the card the person just unblocked.
func TestWaitingHumanCardAnsweredAtAFullBoundResumesRunning(t *testing.T) {
	ctx := context.Background()
	store, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-wh", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "waiting-human"},
		Task{ID: "t-run", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "running"})
	if n, _ := store.CountAdmitted(ctx, "demo/app"); n != 1 {
		t.Fatalf("admitted = %d, want the bound full at 1", n)
	}
	// the person answers: the signal passes the bound (the card is past
	// its start, not queued), and the session MarkStarted resumes takes
	// the slot back as running, over the bound for this gate pass
	if err := bounded.Signal(ctx, "t-wh", "answer", TaskSignal{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.signals, []string{"t-wh"}) {
		t.Fatalf("signals = %v, want the answered card's workflow", rec.signals)
	}
	if err := store.MarkStarted(ctx, "t-wh"); err != nil {
		t.Fatalf("MarkStarted at a full bound: %v", err)
	}
	if t1, _, _ := store.Task(ctx, "t-wh"); t1.Status != "running" {
		t.Fatalf("status = %q, want running: the answer resumes, never re-queues", t1.Status)
	}
}
