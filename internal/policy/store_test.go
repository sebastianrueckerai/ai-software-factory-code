package policy

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// testDB opens a disposable Postgres: FACTORYD_TEST_DATABASE_URL must point
// at a database this test may wipe. Skipped otherwise; CI stays hermetic.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	for range 2 { // idempotent
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	// the tasks table belongs to the binaries; these columns are all the
	// headroom queries read
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tasks (
		id text PRIMARY KEY, budget_eur numeric NOT NULL, spent_eur numeric NOT NULL DEFAULT 0, status text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE policy_activations, policy_versions, ledger, holds, tasks CASCADE`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPolicyVersions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	if _, ok, err := Current(ctx, db); err != nil || ok {
		t.Fatalf("Current on empty = (%v, %v), want (false, nil)", ok, err)
	}
	d1, _ := Parse([]byte(valid))
	d2 := d1
	d2.Budget.DayEUR = 7

	v1, inserted, err := Record(ctx, db, "sha1", d1)
	if err != nil || !inserted {
		t.Fatalf("Record new = (%v, %v)", inserted, err)
	}
	if !reflect.DeepEqual(v1.Document, d1) || v1.SHA256 != "sha1" || v1.LoadedAt.IsZero() || v1.ActivatedAt.Before(v1.LoadedAt) {
		t.Fatalf("recorded version = %+v", v1)
	}
	// the content in force again: no new activation
	if again, _, err := Record(ctx, db, "sha1", d1); err != nil || again.ID != v1.ID || !again.ActivatedAt.Equal(v1.ActivatedAt) {
		t.Fatalf("Record of the current content = (%+v, %v), want version %d activated at %s", again, err, v1.ID, v1.ActivatedAt)
	}
	v2, inserted, err := Record(ctx, db, "sha2", d2)
	if err != nil || !inserted || v2.ID == v1.ID {
		t.Fatalf("Record second = (%+v, %v, %v)", v2, inserted, err)
	}
	cur, ok, err := Current(ctx, db)
	if err != nil || !ok || cur.ID != v2.ID || cur.Document.Budget.DayEUR != 7 {
		t.Fatalf("Current = (%+v, %v, %v), want version %d", cur, ok, err, v2.ID)
	}
	back, inserted, err := Record(ctx, db, "sha1", d1)
	if err != nil || inserted || back.ID != v1.ID || !back.LoadedAt.Equal(v1.LoadedAt) || !back.ActivatedAt.After(v1.ActivatedAt) {
		t.Fatalf("Record revert = (%+v, %v, %v), want version %d, not inserted, activated anew", back, inserted, err, v1.ID)
	}
	var activations []int64
	rows, err := db.QueryContext(ctx, `SELECT version_id FROM policy_activations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		activations = append(activations, id)
	}
	if !reflect.DeepEqual(activations, []int64{v1.ID, v2.ID, v1.ID}) {
		t.Fatalf("activations = %v, want v1 v2 v1", activations)
	}
	for _, stmt := range []string{
		`UPDATE policy_versions SET document = '{}'`,
		`DELETE FROM policy_versions`,
		`UPDATE policy_activations SET version_id = version_id`,
		`DELETE FROM policy_activations`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("%s succeeded, want the immutability trigger to refuse", stmt)
		}
	}
}

// A database from before activations keeps its version in force.
func TestMigrateFromPolicyCurrent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	d1, _ := Parse([]byte(valid))
	v1, _, err := Record(ctx, db, "sha1", d1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE policy_activations;
		CREATE TABLE policy_current (singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
		                             version_id bigint NOT NULL REFERENCES policy_versions (id));
		INSERT INTO policy_current (version_id) VALUES (`+itoa(v1.ID)+`)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if cur, ok, err := Current(ctx, db); err != nil || !ok || cur.ID != v1.ID {
		t.Fatalf("Current after the migration = (%+v, %v, %v), want version %d", cur, ok, err, v1.ID)
	}
	var gone bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('policy_current') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("policy_current still exists (%v)", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestLedgerCosts(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
		('t1', (now() AT TIME ZONE 'UTC')::date, 'model', 'coding-cheap', 0.5),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'coding-cheap', 0.25),
		('t1', (now() AT TIME ZONE 'UTC')::date, 'settlement', 'litellm', 0.125),
		('t1', '2026-01-01', 'model', 'coding-cheap', 3)`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO ledger (task_id, kind, eur) VALUES ('t1', 'bogus', 1)`,
		`INSERT INTO ledger (task_id, kind, eur) VALUES ('t1', 'model', -1)`,
		`UPDATE ledger SET eur = 0`,
		`DELETE FROM ledger`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("%s succeeded, want a refusal", stmt)
		}
	}

	today := time.Now().UTC()
	c, err := LedgerCosts(ctx, db, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), today)
	if err != nil {
		t.Fatal(err)
	}
	want := Costs{
		From: "2026-01-01", To: today.Format(DateLayout),
		Days: []DayCost{
			{Day: "2026-01-01", ByKind: map[string]float64{"model": 3}, Total: 3},
			{Day: today.Format(DateLayout), ByKind: map[string]float64{"model": 0.75, "settlement": 0.125}, Total: 0.875},
		},
		Tasks: []TaskCost{{TaskID: "t1", EUR: 3.625}, {TaskID: "t2", EUR: 0.25}},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("costs =\n%+v\nwant\n%+v", c, want)
	}
	c, err = LedgerCosts(ctx, db, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	if err != nil || len(c.Days) != 0 || len(c.Tasks) != 0 {
		t.Fatalf("empty range = (%+v, %v)", c, err)
	}
}

var testPrice = Price{In: 1e-6, Out: 4e-6}

func TestHolds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES
		('a', 0.02, 0.01, 'running'), ('b', 1, 0, 'running')`); err != nil {
		t.Fatal(err)
	}
	req := HoldRequest{TaskID: "a", Kind: KindModel, Ref: "coding-cheap", InTokens: 1000, Price: testPrice, DayEUR: 20}

	// task a: 0.01 free; 1000 input tokens cost 0.001, 0.009 buys 2250 output tokens
	h, err := TakeHold(ctx, db, req)
	if err != nil {
		t.Fatal(err)
	}
	if h.MaxOut != 2250 || !near(h.EUR, 0.01) || !near(h.Headroom, 0.01) {
		t.Fatalf("hold = %+v, want 2250 tokens, 0.01 EUR, headroom 0.01", h)
	}
	// the hold takes all of a's headroom: the next call is refused
	var r *Refusal
	if _, err := TakeHold(ctx, db, req); !errors.As(err, &r) || r.Decision != "refused_budget" {
		t.Fatalf("second hold = %v, want refused_budget", err)
	}
	// released at its real cost, the rest is free again
	cost := 0.004
	spent, err := ReleaseHold(ctx, db, h, &cost)
	if err != nil || !near(spent, 0.014) {
		t.Fatalf("release = (%v, %v), want spent 0.014", spent, err)
	}
	// released once only
	if spent, err := ReleaseHold(ctx, db, h, &cost); err != nil || !near(spent, 0.014) {
		t.Fatalf("second release = (%v, %v), want spent unchanged at 0.014", spent, err)
	}
	// 0.006 free: 0.005 after the input buys 1250 tokens
	h2, err := TakeHold(ctx, db, req)
	if err != nil || h2.MaxOut != 1250 {
		t.Fatalf("hold after release = (%+v, %v), want 1250 tokens", h2, err)
	}
	// unknown cost: the hold is booked in full
	if spent, err := ReleaseHold(ctx, db, h2, nil); err != nil || !near(spent, 0.02) {
		t.Fatalf("release without a cost = (%v, %v), want spent 0.02", spent, err)
	}

	// the day: 0.01 booked in the ledger; a day cap of 0.01 leaves task b nothing
	bReq := req
	bReq.TaskID, bReq.DayEUR = "b", 0.01
	bReq.InTokens = 0
	if _, err := TakeHold(ctx, db, bReq); !errors.As(err, &r) || r.Decision != "refused_day_cap" {
		t.Fatalf("hold under a full day = %v, want refused_day_cap", err)
	}
	bReq.DayEUR = 0.02 // 0.01 free today: 2500 tokens
	hb, err := TakeHold(ctx, db, bReq)
	if err != nil || hb.MaxOut != 2500 {
		t.Fatalf("day-bound hold = (%+v, %v), want 2500 tokens", hb, err)
	}

	// an expired hold is booked in full by the sweep, and its caller's late
	// release books nothing more
	if _, err := db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('b', 'model', 'x', 0.003, now() - interval '1 second'),
		('b', 'resource', 'cx23', 0.0096, now() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	// the expired resource reservation is accrual's to book and drop
	if n, err := SweepHolds(ctx, db); err != nil || n != 1 {
		t.Fatalf("sweep = (%d, %v), want 1", n, err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM holds WHERE kind = 'resource'`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("resource holds left = %d, %v", left, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM holds WHERE kind = 'resource'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM holds WHERE id = $1`, hb.ID); err != nil {
		t.Fatal(err)
	}
	if spent, err := ReleaseHold(ctx, db, hb, &cost); err != nil || !near(spent, 0.003) {
		t.Fatalf("release of a swept hold = (%v, %v), want b's spent 0.003 (the sweep's)", spent, err)
	}
	// a swept hold of 0.001 whose call cost 0.004: the sweep booked 0.001,
	// the release books the 0.003 above it
	swept := Hold{ID: -1, TaskID: "b", Kind: KindModel, Ref: "y", EUR: 0.001}
	over := 0.004
	if spent, err := ReleaseHold(ctx, db, swept, &over); err != nil || !near(spent, 0.006) {
		t.Fatalf("late release above the swept hold = (%v, %v), want b's spent 0.006", spent, err)
	}
	// a cost that is not a finite non-negative number books the hold
	for _, bad := range []float64{math.NaN(), math.Inf(1), -0.5} {
		h, err := TakeHold(ctx, db, HoldRequest{TaskID: "b", Kind: KindModel, Ref: "z", Price: testPrice, WantOut: 1000, DayEUR: 20})
		if err != nil {
			t.Fatal(err)
		}
		before := 0.0
		db.QueryRowContext(ctx, `SELECT spent_eur::float8 FROM tasks WHERE id = 'b'`).Scan(&before)
		if spent, err := ReleaseHold(ctx, db, h, &bad); err != nil || !near(spent, before+0.004) {
			t.Fatalf("release at cost %v = (%v, %v), want the hold 0.004 booked on top of %v", bad, spent, err, before)
		}
	}
	var ledger []string
	rows, err := db.QueryContext(ctx, `SELECT task_id || ' ' || kind || ' ' || ref || ' ' || eur::text FROM ledger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l string
		rows.Scan(&l)
		ledger = append(ledger, l)
	}
	want := []string{"a model coding-cheap 0.004", "a model coding-cheap 0.006", "b model x 0.003", "b model y 0.003",
		"b model z 0.004", "b model z 0.004", "b model z 0.004"}
	if !reflect.DeepEqual(ledger, want) {
		t.Fatalf("ledger = %q, want %q", ledger, want)
	}
}

// Tool holds count the tool's requests and euros per task, holds and
// ledger rows alike, and fit the task's and the day's headroom.
func TestToolHolds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status) VALUES ('a', 0.1, 'running'), ('b', 1, 'running')`); err != nil {
		t.Fatal(err)
	}
	req := HoldRequest{TaskID: "a", Kind: KindTool, Ref: ToolWebScrape, EUR: 0.02, DayEUR: 20,
		Cap: ToolCap{MaxRequests: 2, MaxEUR: 0.05}}
	var r *Refusal
	h1, err := TakeHold(ctx, db, req)
	if err != nil || h1.EUR != 0.02 {
		t.Fatalf("first hold = (%+v, %v)", h1, err)
	}
	// the open hold counts: a second request fits, a third does not
	h2, err := TakeHold(ctx, db, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TakeHold(ctx, db, req); !errors.As(err, &r) || r.Decision != "refused_tool_requests" {
		t.Fatalf("third hold = %v, want refused_tool_requests", err)
	}
	// a free call still books a row: it was a request
	if spent, err := ReleaseHold(ctx, db, h1, &zeroEUR); err != nil || spent != 0 {
		t.Fatalf("free release = (%v, %v)", spent, err)
	}
	cost := 0.005
	if _, err := ReleaseHold(ctx, db, h2, &cost); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeHold(ctx, db, req); !errors.As(err, &r) || r.Decision != "refused_tool_requests" {
		t.Fatalf("hold after two booked requests = %v, want refused_tool_requests", err)
	}
	// euros: 0.005 used, 0.02 + 0.02 more fit 0.05, a third 0.02 does not
	req.Cap.MaxRequests = 10
	h3, err := TakeHold(ctx, db, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TakeHold(ctx, db, req); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeHold(ctx, db, req); !errors.As(err, &r) || r.Decision != "refused_tool_eur" {
		t.Fatalf("hold past 0.05 EUR = %v, want refused_tool_eur", err)
	}
	// an unknown cost books the hold; the caps count it
	if spent, err := ReleaseHold(ctx, db, h3, nil); err != nil || !near(spent, 0.025) {
		t.Fatalf("release without a cost = (%v, %v), want spent 0.025", spent, err)
	}
	// another tool has its own caps
	other := req
	other.Ref = ToolWebSearch
	if _, err := TakeHold(ctx, db, other); err != nil {
		t.Fatalf("web_search hold = %v", err)
	}
	// task headroom: 0.1 - 0.025 spent - 0.04 held = 0.035; 0.04 does not fit
	big := req
	big.Ref, big.EUR, big.Cap = ToolReplicate, 0.04, ToolCap{MaxRequests: 5, MaxEUR: 1}
	if _, err := TakeHold(ctx, db, big); !errors.As(err, &r) || r.Decision != "refused_budget" {
		t.Fatalf("hold past the task headroom = %v, want refused_budget", err)
	}
	// the day: 0.025 booked, 0.04 held; a day cap of 0.1 leaves b 0.035
	big.TaskID, big.DayEUR = "b", 0.1
	if _, err := TakeHold(ctx, db, big); !errors.As(err, &r) || r.Decision != "refused_day_cap" {
		t.Fatalf("hold past the day = %v, want refused_day_cap", err)
	}
	var rows []string
	res, err := db.QueryContext(ctx, `SELECT kind || ' ' || ref || ' ' || eur::text FROM ledger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
		var l string
		res.Scan(&l)
		rows = append(rows, l)
	}
	want := []string{"tool web_scrape 0", "tool web_scrape 0.005", "tool web_scrape 0.02"}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ledger = %q, want %q", rows, want)
	}
}

var zeroEUR = 0.0

// Many concurrent calls of two tasks against a day with room for exactly
// four: exactly four get a hold.
func TestHoldsConcurrent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status) VALUES ('a', 1, 'running'), ('b', 1, 'running')`); err != nil {
		t.Fatal(err)
	}
	// each call: 0 input, 2000 tokens at 4e-6 = 0.008; the day has 0.032
	var mu sync.Mutex
	var wg sync.WaitGroup
	granted, refused := 0, 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := []string{"a", "b"}[i%2]
			_, err := TakeHold(ctx, db, HoldRequest{TaskID: task, Kind: KindModel, Ref: "m", Price: testPrice, WantOut: 2000, DayEUR: 0.032})
			mu.Lock()
			defer mu.Unlock()
			var r *Refusal
			switch {
			case err == nil:
				granted++
			case errors.As(err, &r):
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if granted != 4 || refused != 12 {
		t.Fatalf("granted %d, refused %d, want 4 and 12", granted, refused)
	}
}

func near(a, b float64) bool { return a-b < 1e-12 && b-a < 1e-12 }

// A task that is not working (queued, waiting-human, closing, done, failed)
// takes no hold, even if a caller forgot to check: settlement relies on
// finished tasks taking none. running and in-review (the gates' model
// review) do.
func TestHoldsOnlyForWorkingTasks(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES
		('run', 1, 0, 'running'), ('rev', 1, 0, 'in-review'), ('done', 1, 0, 'done'), ('failed', 1, 0, 'failed'),
		('closing', 1, 0, 'closing'), ('wait', 1, 0, 'waiting-human'), ('queued', 1, 0, 'queued')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"run", "rev", "done", "failed", "closing", "wait", "queued"} {
		for _, kind := range []string{KindTool, KindModel} {
			req := HoldRequest{TaskID: id, Kind: kind, Ref: "coding-cheap", InTokens: 10, Price: testPrice, DayEUR: 20,
				EUR: 0.001, Cap: ToolCap{MaxRequests: 5, MaxEUR: 1}}
			_, err := TakeHold(ctx, db, req)
			var r *Refusal
			working := id == "run" || id == "rev"
			if working && err != nil || !working && (!errors.As(err, &r) || r.Decision != "refused_task_status") {
				t.Errorf("%s %s: err = %v", id, kind, err)
			}
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM holds WHERE task_id NOT IN ('run', 'rev')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("holds of non-working tasks = %d (%v)", n, err)
	}
}
