package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
	retryable := func() string {
		t.Helper()
		sha, ok, err := s.RetryableRoll(ctx, repo, rollRetryBackoff, rollAttemptBound)
		if err != nil || ok != (sha != "") {
			t.Fatalf("RetryableRoll = %q %v %v", sha, ok, err)
		}
		return sha
	}
	task := func(id, status string) {
		t.Helper()
		finished := "NULL"
		if status != "queued" {
			finished = "now()"
		}
		if err := s.CreateTask(ctx, Task{ID: id, Status: "queued", Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = $2, finished_at = `+finished+` WHERE id = $1`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	// an attempt: its task, its request over the failed one, settled
	attempt := func(id, sha string, run int64, status string) {
		t.Helper()
		task(id, status)
		if err := s.InsertRollRequest(ctx, RollRequest{SHA: sha, Repository: repo, RunID: run, State: RollOpened, TaskID: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleRollRequests(ctx); err != nil {
			t.Fatal(err)
		}
	}
	settledAgo := func(sha string, d time.Duration) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET settled_at = now() - make_interval(secs => $2) WHERE sha = $1`, sha, d.Seconds()); err != nil {
			t.Fatal(err)
		}
	}

	attempt("z1", "z", 5, "failed")
	attempt("t1", "a", 10, "failed")
	var settledAt *time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT settled_at FROM roll_requests WHERE sha = 'a'`).Scan(&settledAt); err != nil || settledAt == nil {
		t.Fatalf("a settled failure has no settled_at: %v %v", settledAt, err)
	}
	if got := retryable(); got != "" {
		t.Fatalf("retryable right after the failure = %q", got)
	}
	settledAgo("a", rollRetryBackoff-time.Minute)
	if got := retryable(); got != "" {
		t.Fatalf("retryable before the backoff = %q", got)
	}
	settledAgo("a", rollRetryBackoff+time.Minute)
	if got := retryable(); got != "a" {
		t.Fatalf("retryable after the backoff = %q, want a", got)
	}

	// the retry writes over a's request and keeps the failed attempt's task
	task("t2", "queued")
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollOpened, TaskID: "t2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollOpened, TaskID: "t2"}); err == nil {
		t.Fatal("a second insert over an open request succeeded")
	}
	if got := retryable(); got != "" {
		t.Fatalf("retryable beside an open roll = %q", got)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE id = 't2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleRollRequests(ctx); err != nil {
		t.Fatal(err)
	}
	settledAgo("a", rollRetryBackoff+time.Minute)
	failed, err := s.FailedRolls(ctx, repo)
	if err != nil || len(failed) != 2 || failed[1].SHA != "a" || !slices.Equal(failed[1].FailedTasks, []string{"t1"}) || failed[1].TaskID != "t2" {
		t.Fatalf("failed rolls %+v %v, want z, and a with t1 kept and t2 current", failed, err)
	}
	// z failed once, a twice: the bound of three is reached
	if got := retryable(); got != "" {
		t.Fatalf("retryable at the bound = %q", got)
	}

	// a merged roll between z and a resets the count to a's two failures
	task("m1", "done")
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "m", Repository: repo, RunID: 7, State: RollMerged, TaskID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if got := retryable(); got != "a" {
		t.Fatalf("retryable after a merged roll reset the count = %q, want a", got)
	}
	// a newer request of any state takes the retry's place
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "b", Repository: repo, RunID: 11, State: RollSkipped}); err != nil {
		t.Fatal(err)
	}
	if got := retryable(); got != "" {
		t.Fatalf("retryable beside a newer head = %q", got)
	}
}

// A retry's request that is forgotten, by a filing error or by the settle
// after a crash, goes back to the failed attempt it overwrote; a first
// attempt's request goes.
func TestPGStoreForgetsARetryBackToItsFailure(t *testing.T) {
	ctx := context.Background()
	s := openRetryStore(t)
	const repo = "sf/ai-software-factory-code"
	if err := s.CreateTask(ctx, Task{ID: "t1", Status: "queued", Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	insert := func(task string) {
		t.Helper()
		if err := s.InsertRollRequest(ctx, RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollOpened, TaskID: task}); err != nil {
			t.Fatal(err)
		}
	}
	insert("t1")
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleRollRequests(ctx); err != nil {
		t.Fatal(err)
	}
	row := func() (state, task string, kept []string) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT state, task_id, failed_tasks FROM roll_requests WHERE sha = 'a'`).Scan(&state, &task, pgtype.NewMap().SQLScanner(&kept)); err != nil {
			t.Fatal(err)
		}
		return
	}
	for name, forget := range map[string]func(){
		"a filing error": func() {
			if err := s.ForgetRollRequest(ctx, "a"); err != nil {
				t.Fatal(err)
			}
		},
		"a settle after a crash": func() {
			if _, err := s.SettleRollRequests(ctx); err != nil {
				t.Fatal(err)
			}
		},
	} {
		insert("no-such-task")
		forget()
		if state, task, kept := row(); state != RollFailed || task != "t1" || len(kept) != 0 {
			t.Fatalf("%s: row %s %s %v, want failed under t1", name, state, task, kept)
		}
		if sha, ok, err := s.RetryableRoll(ctx, repo, 0, rollAttemptBound); err != nil || !ok || sha != "a" {
			t.Fatalf("%s: RetryableRoll = %q %v %v, want a with its settle time stamped", name, sha, ok, err)
		}
	}
	// a first attempt's request is deleted
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: "n", Repository: repo, RunID: 11, State: RollOpened, TaskID: "no-such-task"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetRollRequest(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if seen, err := s.RollRequestSeen(ctx, "n"); err != nil || seen {
		t.Fatalf("a forgotten first attempt is still seen: %v %v", seen, err)
	}
}

// Every failed attempt's card stays related to the request in the console.
func TestRollCarriesKeepsEveryAttemptsCard(t *testing.T) {
	ctx := context.Background()
	s := openRetryStore(t)
	const repo = "sf/ai-software-factory-code"
	if err := s.CreateTask(ctx, Task{ID: "card", Status: "merged", Repository: repo, Kind: "agent", PRNumber: 7, Track: "direct", FinishedAt: ptrTime(time.Now())}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"r1", "r2"} {
		if err := s.CreateTask(ctx, Task{ID: id, Status: "queued", Repository: repo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
			t.Fatal(err)
		}
		if err := s.InsertRollRequest(ctx, RollRequest{SHA: "a", Repository: repo, RunID: 10, State: RollOpened, TaskID: id, PRs: []int64{7}}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed', finished_at = now() WHERE id = 'r1'`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SettleRollRequests(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, id := range []string{"r1", "r2"} {
		if got, err := s.RollCarries(ctx, id); err != nil || len(got) != 1 || got[0].ID != "card" {
			t.Fatalf("RollCarries(%s) = %+v %v, want the card", id, got, err)
		}
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
	settledAgo := func(d time.Duration) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, `UPDATE roll_requests SET settled_at = now() - make_interval(secs => $1)`, d.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	newRoller := func() *Roller {
		return &Roller{Forgejo: fj, Registry: reg, SigningKey: &testCosignKey.PublicKey, Store: pgRollStore{s}, Workflows: wf,
			Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}, Token: "registrar-tok", Budget: testBudget, Scrub: redact.New(),
			Flux: &fakeFluxSwitch{}}
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
	poll(roller)                               // settles: the failure time is the store's now()
	settledAgo(rollRetryBackoff - time.Minute) // a restart in the backoff
	poll(newRoller())
	if len(wf.started) != 1 {
		t.Fatalf("started %v inside the backoff, want the first roll only", wf.started)
	}
	settledAgo(rollRetryBackoff + time.Minute)
	poll(newRoller())
	poll(newRoller())
	var retries int
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT cardinality(failed_tasks), state FROM roll_requests WHERE sha = $1`, commitC1).Scan(&retries, &state); err != nil {
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
	settledAgo(5 * time.Minute)
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

func ptrTime(t time.Time) *time.Time { return &t }
