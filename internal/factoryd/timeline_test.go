package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimelineAppendsLinesAcrossWriters(t *testing.T) {
	objects := &fakeObjectStore{}
	ctx := context.Background()
	if err := (&Timeline{Objects: objects}).Append(ctx, "t1", "intake", map[string]any{"source": "api", "issue": 4}); err != nil {
		t.Fatal(err)
	}
	// a second writer (another activity, maybe another factoryd pod) keeps the first line
	if err := (&Timeline{Objects: objects}).Append(ctx, "t1", "pr_opened", map[string]any{"number": 9}); err != nil {
		t.Fatal(err)
	}
	body, _ := objects.Get(ctx, "sessions/t1/timeline.jsonl")
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want 2", lines)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first["step"] != "intake" || first["source"] != "api" || first["issue"] != float64(4) || first["time"] == nil {
		t.Fatalf("first = %v", first)
	}
	if second["step"] != "pr_opened" || second["number"] != float64(9) {
		t.Fatalf("second = %v", second)
	}
}

// race() runs two activities of one task at once and both may record;
// concurrent appends to one task's timeline lose no line.
func TestTimelineSerializesConcurrentAppends(t *testing.T) {
	objects := &fakeObjectStore{}
	tl := &Timeline{Objects: objects}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tl.Append(context.Background(), "t1", "step", map[string]any{"i": i}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	body, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if lines := strings.Split(strings.TrimSpace(string(body)), "\n"); len(lines) != 32 {
		t.Fatalf("lines = %d, want all 32 appends kept", len(lines))
	}
}

// No lock outlives its appends: the map does not grow with every task
// factoryd ever ran.
func TestTimelineKeepsNoIdleLock(t *testing.T) {
	tl := &Timeline{Objects: &fakeObjectStore{}}
	ctx := context.Background()
	for _, step := range []string{"intake", "final"} {
		if err := tl.Append(ctx, "t1", step, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(tl.locks) != 0 {
		t.Fatalf("locks = %v, want none once no append runs", tl.locks)
	}
}

// gatedObjectStore holds every Get until the test lets it through, and
// counts the appends inside their read-append-write at once.
type gatedObjectStore struct {
	fakeObjectStore
	gate    chan struct{}
	entered chan struct{}
	inside  atomic.Int32
	most    atomic.Int32
}

func (s *gatedObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	n := s.inside.Add(1)
	if n > s.most.Load() {
		s.most.Store(n)
	}
	s.entered <- struct{}{}
	<-s.gate
	return s.fakeObjectStore.Get(ctx, key)
}

func (s *gatedObjectStore) Put(ctx context.Context, key string, body []byte) error {
	defer s.inside.Add(-1)
	return s.fakeObjectStore.Put(ctx, key, body)
}

// The final line must not hand a waiter and a newcomer two different
// locks: the waiter queued on the finished append's lock and a caller
// arriving after it would both read-append-write at once, losing a line.
func TestTimelineFinalLineKeepsItsLockForWaiters(t *testing.T) {
	store := &gatedObjectStore{gate: make(chan struct{}), entered: make(chan struct{}, 3)}
	tl := &Timeline{Objects: store}
	ctx := context.Background()
	var wg sync.WaitGroup
	appendStep := func(step string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tl.Append(ctx, "t1", step, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	appendStep("final")
	<-store.entered // the final append holds the lock
	appendStep("waiter")
	time.Sleep(50 * time.Millisecond) // the waiter queues on the lock
	store.gate <- struct{}{}          // the final append completes
	<-store.entered                   // the waiter holds the lock
	appendStep("newcomer")
	select {
	case <-store.entered:
		t.Fatal("a newcomer entered while the waiter held the task's lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(store.gate)
	wg.Wait()
	body, _ := store.fakeObjectStore.Get(ctx, "sessions/t1/timeline.jsonl")
	if lines := strings.Split(strings.TrimSpace(string(body)), "\n"); len(lines) != 3 || store.most.Load() != 1 {
		t.Fatalf("lines = %d, most at once = %d, want 3 lines, one append at a time", len(lines), store.most.Load())
	}
}

// A step carrying started emits its gate's duration once; one without
// emits nothing.
func TestAppendEmitsTheGateDurationOnlyWithStarted(t *testing.T) {
	m, reader := testMetrics(t)
	now := time.Date(2026, 9, 28, 10, 33, 9, 0, time.UTC)
	tl := &Timeline{Objects: &fakeObjectStore{}, Metrics: m, now: func() time.Time { return now }}
	ctx := context.Background()
	if err := tl.Append(ctx, "t1", "pushed", map[string]any{"commit": "c1"}); err != nil {
		t.Fatal(err)
	}
	if got := histograms(t, reader, "factory.task.gate.duration"); len(got) != 0 {
		t.Fatalf("emitted %v without started", got)
	}
	// typed as WaitChecks and RunSession record them
	if err := tl.Append(ctx, "t1", "checks", map[string]any{"state": ChecksSuccess, "started": "2026-09-28T10:28:38Z"}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Append(ctx, "t1", "session_end", map[string]any{"outcome": OutcomePushed, "model": "factory/coding-cheap",
		"started": "2026-09-28T10:23:09.5Z"}); err != nil {
		t.Fatal(err)
	}
	// commissioning ends at the server's live, 95 s after the request
	if err := tl.Append(ctx, "t1", "commissioned", map[string]any{"server_type": "cx23",
		"started": "2026-09-28T10:20:00Z", "live": "2026-09-28T10:21:35Z"}); err != nil {
		t.Fatal(err)
	}
	// a step that ends no gate emits nothing, started or not
	if err := tl.Append(ctx, "t1", "pr_opened", map[string]any{"started": "2026-09-28T10:20:00Z"}); err != nil {
		t.Fatal(err)
	}
	got := histograms(t, reader, "factory.task.gate.duration")
	want := map[string]histPoint{
		// 10:28:38 to 10:33:09
		"gate=checks,model_alias=,outcome=success":                     {1, 271},
		"gate=session,model_alias=factory/coding-cheap,outcome=pushed": {1, 599.5},
		"gate=commissioning,model_alias=,outcome=":                     {1, 95},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gates %v, want %v", got, want)
	}
}
