package factoryd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The dependency prober (roadmap 299): five probes, each injectable, write
// the latest state per dependency. The flap rule is the heart: one failure
// whispers, two consecutive mark down, the next success marks up. Requires
// a disposable Postgres (FACTORYD_TEST_DATABASE_URL), like every store test.

type fakeControl struct{ err error }

func (f fakeControl) Probe(ctx context.Context) error { return f.err }

// depForgejo answers the LatestCommit read of the forgejo probe, or fails
// it with a 500 while fail is set.
func depForgejo(t *testing.T, fail *atomicBool) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.get() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `[{"sha":"abc123","parents":[{"sha":"def456"}]}]`)
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "pw"}
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// newDepProber wires a prober whose every probe succeeds unless a fake is
// told otherwise. SlowAfter of an hour keeps every fast success up.
func newDepProber(t *testing.T, control ControlPlane, pg, temporal func(context.Context) error, objects ObjectStore) (*DepProber, *PGStore) {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_dephealth"))
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`TRUNCATE dependency_health`); err != nil {
		t.Fatal(err)
	}
	var fjFail atomicBool
	p := &DepProber{Store: s, Objects: objects, Control: control,
		Forgejo: depForgejo(t, &fjFail), Owner: "sf", RepoName: "ai-software-factory-code", Branch: "main",
		PG: pg, Temporal: temporal, Interval: time.Hour, SlowAfter: time.Hour, Cap: 2 * time.Second}
	return p, s
}

func depRows(t *testing.T, s *PGStore) map[string]DependencyState {
	t.Helper()
	rows, err := s.DependencyStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]DependencyState{}
	for _, r := range rows {
		by[r.Name] = r
	}
	return by
}

func TestDepProberAllUp(t *testing.T) {
	p, s := newDepProber(t, fakeControl{}, func(context.Context) error { return nil }, func(context.Context) error { return nil }, &fakeObjectStore{})
	if err := p.probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := depRows(t, s)
	want := []string{"forgejo", "object storage", "postgres", "provider api", "temporal"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d (%v), want %d", len(rows), rows, len(want))
	}
	for _, name := range want {
		r, ok := rows[name]
		if !ok {
			t.Fatalf("no row for %s: %v", name, rows)
		}
		if r.State != "up" || r.Detail != "" {
			t.Fatalf("%s = %s %q, want up with no detail", name, r.State, r.Detail)
		}
	}
}

// Work comes before the first tick: with an hour-long interval no tick can
// fire, and the rows still exist within a second of Run starting.
func TestDepProberRunWorksFirst(t *testing.T) {
	p, s := newDepProber(t, fakeControl{}, func(context.Context) error { return nil }, func(context.Context) error { return nil }, &fakeObjectStore{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		if len(depRows(t, s)) == 5 {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			t.Fatal("no rows within 2 s of Run: the work does not come first")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestDepProberDownAfterTwo(t *testing.T) {
	var fail atomicBool
	fail.set(true)
	boom := errors.New("api is gone")
	p, s := newDepProber(t, fakeControl{boom}, func(context.Context) error { return nil }, func(context.Context) error { return nil }, &fakeObjectStore{})
	ctx := context.Background()
	if err := p.probe(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := depRows(t, s); len(rows) != 4 {
		t.Fatalf("after one failure rows = %v, want the provider absent (the whisper)", rows)
	}
	if err := p.probe(ctx); err != nil {
		t.Fatal(err)
	}
	r := depRows(t, s)["provider api"]
	if r.State != "down" || r.Detail != "api is gone" {
		t.Fatalf("after two failures = %s %q, want down with the error", r.State, r.Detail)
	}
	// the next success lifts it at once, and one later failure does not
	// re-down it: the counter reset
	p.Control = fakeControl{}
	if err := p.probe(ctx); err != nil {
		t.Fatal(err)
	}
	if r := depRows(t, s)["provider api"]; r.State != "up" || r.Detail != "" {
		t.Fatalf("after recovery = %s %q, want up", r.State, r.Detail)
	}
	p.Control = fakeControl{boom}
	if err := p.probe(ctx); err != nil {
		t.Fatal(err)
	}
	if r := depRows(t, s)["provider api"]; r.State != "up" {
		t.Fatalf("one failure after recovery = %s, want still up", r.State)
	}
}

func TestDepProberDegraded(t *testing.T) {
	p, s := newDepProber(t, fakeControl{}, func(context.Context) error { return nil }, func(context.Context) error { return nil }, &fakeObjectStore{})
	p.SlowAfter = time.Millisecond
	p.PG = func(context.Context) error { time.Sleep(30 * time.Millisecond); return nil }
	ctx := context.Background()
	if err := p.probe(ctx); err != nil {
		t.Fatal(err)
	}
	r := depRows(t, s)["postgres"]
	if r.State != "degraded" || r.Detail == "" {
		t.Fatalf("a slow answer = %s %q, want degraded naming the slowness", r.State, r.Detail)
	}
	if r.LatencyMS < 20 {
		t.Fatalf("latency = %d ms, want the measured 30 ms", r.LatencyMS)
	}
	// fast again: up; and a slow answer that also fails counts as a
	// failure, not a degradation
	p.PG = func(context.Context) error { return nil }
	if err := p.probe(ctx); err != nil {
		t.Fatal(err)
	}
	if r := depRows(t, s)["postgres"]; r.State != "up" {
		t.Fatalf("a fast answer after = %s, want up", r.State)
	}
}

// corruptGet serves a different body than the one put, so the round trip
// fails even though both calls succeed.
type corruptGet struct{ fakeObjectStore }

func (c *corruptGet) Get(ctx context.Context, key string) ([]byte, error) {
	return []byte("something else"), nil
}

func TestDepProberS3RoundTrip(t *testing.T) {
	objects := &corruptGet{}
	p, s := newDepProber(t, fakeControl{}, func(context.Context) error { return nil }, func(context.Context) error { return nil }, objects)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := p.probe(ctx); err != nil {
			t.Fatal(err)
		}
	}
	r := depRows(t, s)["object storage"]
	if r.State != "down" || r.Detail == "" {
		t.Fatalf("a corrupted read back = %s %q, want down naming it", r.State, r.Detail)
	}
	objects.mu.Lock()
	last := objects.puts[len(objects.puts)-1]
	objects.mu.Unlock()
	if last.key != healthProbeKey {
		t.Fatalf("the probe wrote %q, want the fixed key %q", last.key, healthProbeKey)
	}
}
