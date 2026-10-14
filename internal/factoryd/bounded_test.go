package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// recordingWorkflows records the starts and signals that reached the
// workflow layer.
type recordingWorkflows struct {
	starts  []string
	signals []string
}

func (w *recordingWorkflows) Start(ctx context.Context, taskID string) error {
	w.starts = append(w.starts, taskID)
	return nil
}

func (w *recordingWorkflows) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	w.signals = append(w.signals, taskID)
	return nil
}

// boundedFixture is a store of demo/app cards (bound at work_in_progress
// bound) and the work-bounded workflows over it.
func boundedFixture(t *testing.T, bound int, tasks ...Task) (*fakeStore, *recordingWorkflows, BoundedWorkflows) {
	t.Helper()
	store := &fakeStore{tasks: map[string]Task{}}
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

func TestTaskEndedHandsTheFreedSlotToTheOldestQueuedCard(t *testing.T) {
	ctx := context.Background()
	base := time.Now()
	store, rec, bounded := boundedFixture(t, 1,
		Task{ID: "t-done", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "done", CreatedAt: base},
		Task{ID: "t-q", Repository: "demo/app", Kind: "agent", Lane: "ready", Status: "queued", CreatedAt: base.Add(time.Minute)})
	taskEnded(ctx, &Timeline{Objects: &fakeObjectStore{}}, store, nil, bounded, store.tasks["t-done"], "done")
	if !slices.Equal(rec.starts, []string{"t-q"}) {
		t.Fatalf("starts = %v, want the freed slot to start the queued card", rec.starts)
	}
}
