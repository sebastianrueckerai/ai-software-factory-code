package factoryd

import (
	"context"
	"fmt"
	"slices"
	"strings"
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
	if err := s.CreateTask(ctx, Task{ID: "card", Status: "merged", Repository: repo, Kind: "agent", PRNumber: 7, Track: "direct", FinishedAt: justNow()}); err != nil {
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

// Two green runs wait, the newest fails and the older one rolls: the
// newest's retry waits until that roll settles, then runs as its own third
// card, and the one question after it names three cards, none twice.
func TestARetryWaitsWhileAnOlderCommitsRollIsOpen(t *testing.T) {
	ctx := context.Background()
	s := openRetryStore(t)
	f := retryForgejo()
	f.runs = []map[string]any{pushRun(57, commitC1, "success"), pushRun(56, commitC2, "success")}
	fj, reg := f.srv(t)
	wf := &fakeWorkflows{}
	store := pgRollStore{s}
	poll := func() {
		t.Helper()
		r := &Roller{Forgejo: fj, Registry: reg, SigningKey: &testCosignKey.PublicKey, Store: store, Workflows: wf,
			Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}, Token: "registrar-tok", Budget: testBudget, Scrub: redact.New(),
			Flux: &fakeFluxSwitch{}}
		if err := r.poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	failOpen := func() {
		t.Helper()
		exec(`UPDATE tasks SET status = 'failed', finished_at = now() WHERE source = 'roll' AND status <> 'failed'`)
		poll()
	}
	pastBackoff := func() {
		t.Helper()
		exec(`UPDATE roll_requests SET settled_at = now() - make_interval(secs => $1) WHERE state = 'failed'`, (rollRetryBackoff + time.Minute).Seconds())
	}
	state := func(sha string) string {
		t.Helper()
		var st string
		var kept int
		if err := s.db.QueryRowContext(ctx, `SELECT state, cardinality(failed_tasks) FROM roll_requests WHERE sha = $1`, sha).Scan(&st, &kept); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%d", st, kept)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-prev", Status: "queued", Repository: codeRepo, Kind: "agent", Source: SourceRoll, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE tasks SET status = 'merged', finished_at = now() WHERE id = 't-prev'`)
	if err := s.InsertRollRequest(ctx, RollRequest{SHA: commitC0, Repository: codeRepo, RunID: 50, State: RollMerged, TaskID: "t-prev", Committed: true}); err != nil {
		t.Fatal(err)
	}

	poll() // the newest, c1, rolls and the older c2 waits
	failOpen()
	if len(wf.started) != 2 || state(commitC1) != "failed/0" || state(commitC2) != "opened/0" {
		t.Fatalf("started %d, c1 %s, c2 %s, want c1 failed and the older c2 rolling", len(wf.started), state(commitC1), state(commitC2))
	}
	// c2's roll has its pull request, so a fold into it would run
	var open string
	exec(`UPDATE tasks SET pr_number = 99 WHERE source = 'roll' AND status NOT IN ('failed', 'merged')`)
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE source = 'roll' AND status NOT IN ('failed', 'merged')`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.files[forkRepo+"@factory/"+open] = mainPins()
	f.mu.Unlock()
	pastBackoff()
	poll()
	if len(wf.started) != 2 || state(commitC1) != "failed/0" || len(wf.signals) != 0 {
		t.Fatalf("started %d, c1 %s: the retry ran beside c2's open roll", len(wf.started), state(commitC1))
	}
	failOpen()
	pastBackoff()
	poll()
	if len(wf.started) != 3 || state(commitC1) != "opened/1" {
		t.Fatalf("started %d, c1 %s, want c1 retried as the third card once c2's roll settled", len(wf.started), state(commitC1))
	}
	failOpen()
	for range 3 {
		pastBackoff()
		poll()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT objective FROM tasks WHERE kind = 'human' AND objective LIKE '%' || $1 || '%'`, rollAskMarker)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var asks []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		asks = append(asks, o)
	}
	if len(wf.started) != 3 || len(asks) != 1 {
		t.Fatalf("started %d, %d questions, want 3 cards and one question", len(wf.started), len(asks))
	}
	cards := map[string]bool{}
	for _, line := range strings.Split(asks[0], "\n") {
		if strings.HasPrefix(line, "- run ") {
			_, card, _ := strings.Cut(line, "(card #")
			cards[card] = true
		}
	}
	if n := strings.Count(asks[0], "\n- run "); n != 3 || len(cards) != 3 {
		t.Fatalf("question lists %d attempts with %d distinct cards:\n%s", n, len(cards), asks[0])
	}
}
