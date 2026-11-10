package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// pgRollStore reads the project from the seed, as the fake does: the rest
// is the real store.
type pgRollStore struct{ *PGStore }

func (s pgRollStore) ProjectByRepository(ctx context.Context, repository string) (Project, bool, error) {
	return seedProject(repository), true, nil
}

func openRetryStore(t *testing.T) *PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE roll_requests, tasks, releases CASCADE`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPGStoreRetryableRoll(t *testing.T) {
	ctx := context.Background()
	s := openRetryStore(t)
	const repo = "sf/ai-software-factory-code"
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	retryable := func(settledBefore time.Time) string {
		t.Helper()
		sha, ok, err := s.RetryableRoll(ctx, repo, settledBefore, rollAttemptBound)
		if err != nil || ok != (sha != "") {
			t.Fatalf("RetryableRoll = %q %v %v", sha, ok, err)
		}
		return sha
	}
	settle := func(id, sha string, run int64, status string) {
		t.Helper()
		if err := s.CreateTask(ctx, Task{ID: id, Status: status, Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertRollRequest(ctx, RollRequest{SHA: sha, Repository: repo, RunID: run, State: RollOpened, TaskID: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleRollRequests(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setSettled := func(sha string, when time.Time) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET settled_at = $2 WHERE sha = $1`, sha, when); err != nil {
			t.Fatal(err)
		}
	}

	settle("t1", "a", 10, "failed")
	setSettled("a", at)
	if got := retryable(at.Add(-time.Second)); got != "" {
		t.Fatalf("retryable before the backoff = %q", got)
	}
	if got := retryable(at); got != "a" {
		t.Fatalf("retryable at the backoff = %q, want a", got)
	}
	var settledAt *time.Time
	settle("t0", "z", 9, "failed") // older than a: never the newest
	if err := s.db.QueryRowContext(ctx, `SELECT settled_at FROM roll_requests WHERE sha = 'z'`).Scan(&settledAt); err != nil || settledAt == nil {
		t.Fatalf("a settled failure has no settled_at: %v %v", settledAt, err)
	}
	setSettled("z", at)

	// the retry writes over a's request and counts the failure
	if err := s.CreateTask(ctx, Task{ID: "t2", Status: "queued", Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollOpened, TaskID: "t2"}); err != nil {
		t.Fatal(err)
	}
	if got := retryable(at.Add(time.Hour)); got != "" {
		t.Fatalf("retryable beside an open roll = %q", got)
	}
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollOpened, TaskID: "t2"}); err == nil {
		t.Fatal("a second insert over an open request succeeded")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE id = 't2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleRollRequests(ctx); err != nil {
		t.Fatal(err)
	}
	setSettled("a", at)
	failed, err := s.FailedRolls(ctx, repo)
	if err != nil || len(failed) != 2 || failed[1].SHA != "a" || failed[1].Retries != 1 {
		t.Fatalf("failed rolls %+v %v, want z, and a with one retry", failed, err)
	}
	// z failed once, a failed twice: the bound of three is reached
	if got := retryable(at.Add(time.Hour)); got != "" {
		t.Fatalf("retryable at the bound = %q", got)
	}

	// a newer request of any state takes the retry's place, and a merged roll resets
	if _, err := s.db.ExecContext(ctx, `DELETE FROM roll_requests WHERE sha = 'z'`); err != nil {
		t.Fatal(err)
	}
	if got := retryable(at.Add(time.Hour)); got != "a" {
		t.Fatalf("retryable below the bound = %q, want a", got)
	}
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "b", Repository: repo, RunID: 11, State: RollSkipped}); err != nil {
		t.Fatal(err)
	}
	if got := retryable(at.Add(time.Hour)); got != "" {
		t.Fatalf("retryable beside a newer head = %q", got)
	}
}

// The roller over the real store: the retry follows the stored failure
// time, a new Roller over the same store retries once, and a newer head
// rolls in its place.
func TestRollerRetriesFromTheStoreAcrossARestart(t *testing.T) {
	ctx := context.Background()
	s := openRetryStore(t)
	f := retryForgejo()
	fj, reg := f.srv(t)
	wf := &fakeWorkflows{}
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	newRoller := func() *Roller {
		return &Roller{Forgejo: fj, Registry: reg, SigningKey: &testCosignKey.PublicKey, Store: pgRollStore{s}, Workflows: wf,
			Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}, Token: "registrar-tok", Budget: testBudget, Scrub: redact.New(),
			Flux: &fakeFluxSwitch{}, Now: now}
	}
	poll := func(r *Roller) {
		t.Helper()
		if err := r.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	roller := newRoller()
	poll(roller)
	if len(wf.started) != 1 {
		t.Fatalf("started %v, want the first roll", wf.started)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE status <> 'failed'`); err != nil {
		t.Fatal(err)
	}
	poll(roller) // settles: the failure time is the store's now()
	// the backoff is read from the stored time, so move it back
	if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET settled_at = $1`, clock); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(rollRetryBackoff - time.Second)
	poll(newRoller())
	if len(wf.started) != 1 {
		t.Fatalf("started %v inside the backoff, want the first roll only", wf.started)
	}
	clock = clock.Add(time.Second)
	poll(newRoller())
	poll(newRoller())
	var retries int
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT retries, state FROM roll_requests WHERE sha = $1`, commitC1).Scan(&retries, &state); err != nil {
		t.Fatal(err)
	}
	if len(wf.started) != 2 || retries != 1 || state != RollOpened {
		t.Fatalf("started %v, request retries %d state %s, want one retry", wf.started, retries, state)
	}

	// it fails again; a new head arrives inside the second backoff
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE status <> 'failed'`); err != nil {
		t.Fatal(err)
	}
	poll(newRoller())
	if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET settled_at = $1`, clock); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(5 * time.Minute)
	f.mu.Lock()
	f.runs = []map[string]any{pushRun(58, commitC2, "success"), pushRun(57, commitC1, "success")}
	f.mu.Unlock()
	poll(newRoller())
	if len(wf.started) != 3 {
		t.Fatalf("started %v, want the new head's roll", wf.started)
	}
	var c1, c2 string
	s.db.QueryRowContext(ctx, `SELECT state FROM roll_requests WHERE sha = $1`, commitC1).Scan(&c1)
	s.db.QueryRowContext(ctx, `SELECT state FROM roll_requests WHERE sha = $1`, commitC2).Scan(&c2)
	if c1 != RollFailed || c2 != RollOpened {
		t.Fatalf("c1 %s, c2 %s, want c1 left failed and c2 rolling", c1, c2)
	}
}
