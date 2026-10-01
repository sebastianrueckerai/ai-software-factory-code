package policy

import (
	"context"
	"database/sql"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// testDB opens a disposable Postgres: FACTORYD_TEST_DATABASE_URL must point
// at a database this test may wipe. Skipped otherwise; CI stays hermetic.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := schematest.DSN(t, "policy")
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
		id text PRIMARY KEY, budget_eur numeric NOT NULL, spent_eur numeric NOT NULL DEFAULT 0, status text NOT NULL, repository text NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	// the gateway's audit table, the columns the sweep writes
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS audit (
		id bigserial PRIMARY KEY, ts timestamptz NOT NULL DEFAULT now(), task_id text NOT NULL, decision text NOT NULL,
		reason text NOT NULL, model_alias text NOT NULL DEFAULT '', tool text NOT NULL DEFAULT '', spend_eur numeric,
		hold_eur numeric, kind text NOT NULL DEFAULT 'agent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE policy_activations, policy_versions, ledger, holds, tasks, audit CASCADE`); err != nil {
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
	d1, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	d2 := d1
	d2.Document.Budget.DayEUR = 7

	v1, inserted, err := Record(ctx, db, "c1", d1)
	if err != nil || !inserted {
		t.Fatalf("Record new = (%v, %v)", inserted, err)
	}
	if !reflect.DeepEqual(v1.Tree, d1) || v1.Commit != "c1" || v1.LoadedAt.IsZero() || v1.ActivatedAt.Before(v1.LoadedAt) {
		t.Fatalf("recorded version = %+v", v1)
	}
	// the content in force again: no new activation
	if again, _, err := Record(ctx, db, "c1", d1); err != nil || again.ID != v1.ID || !again.ActivatedAt.Equal(v1.ActivatedAt) {
		t.Fatalf("Record of the current content = (%+v, %v), want version %d activated at %s", again, err, v1.ID, v1.ActivatedAt)
	}
	v2, inserted, err := Record(ctx, db, "c2", d2)
	if err != nil || !inserted || v2.ID == v1.ID {
		t.Fatalf("Record second = (%+v, %v, %v)", v2, inserted, err)
	}
	cur, ok, err := Current(ctx, db)
	if err != nil || !ok || cur.ID != v2.ID || cur.Document.Budget.DayEUR != 7 {
		t.Fatalf("Current = (%+v, %v, %v), want version %d", cur, ok, err, v2.ID)
	}
	back, inserted, err := Record(ctx, db, "c1", d1)
	if err != nil || inserted || back.ID != v1.ID || !back.LoadedAt.Equal(v1.LoadedAt) || !back.ActivatedAt.After(v1.ActivatedAt) {
		t.Fatalf("Record revert = (%+v, %v, %v), want version %d, not inserted, activated anew", back, inserted, err, v1.ID)
	}
	// a task's pinned version, whatever is current
	pinned, ok, err := VersionByID(ctx, db, v2.ID)
	if err != nil || !ok || pinned.Commit != "c2" || pinned.Document.Budget.DayEUR != 7 || !reflect.DeepEqual(pinned.Projects, d1.Projects) {
		t.Fatalf("VersionByID(%d) = (%+v, %v, %v)", v2.ID, pinned, ok, err)
	}
	if _, ok, err := VersionByID(ctx, db, v2.ID+100); err != nil || ok {
		t.Fatalf("VersionByID of no version = (%v, %v), want (false, nil)", ok, err)
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
	d1, _ := ParseTree(fixtureTree(t))
	v1, _, err := Record(ctx, db, "c1", d1)
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

// A database from before the tree keeps its rows under commit_sha; their
// policy-file documents decode to no valid tree.
func TestMigrateFromFileVersions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `ALTER TABLE policy_versions RENAME COLUMN commit_sha TO sha256;
		INSERT INTO policy_versions (sha256, document) VALUES ('e3b0c442', '{"version": 1, "budget": {"day_eur": 20}}');
		INSERT INTO policy_activations (version_id) SELECT id FROM policy_versions`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	cur, ok, err := Current(ctx, db)
	if err != nil || !ok || cur.Commit != "e3b0c442" {
		t.Fatalf("Current after the migration = (%+v, %v, %v), want the old row", cur, ok, err)
	}
	if err := cur.Validate(); err == nil {
		t.Fatal("a policy-file document validates as a tree")
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
		('t1', '2026-01-01', 'model', 'coding-cheap', 3),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'review/review-frontier', 0.0625),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'review/review-frontier', 0.0625)`); err != nil {
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
			{Day: today.Format(DateLayout), ByKind: map[string]float64{"model": 0.875, "settlement": 0.125}, Total: 1},
		},
		Tasks: []TaskCost{
			{TaskID: "t1", EUR: 3.625, Lines: []CostLine{{Kind: "model", Ref: "coding-cheap", EUR: 3.5}, {Kind: "settlement", Ref: "litellm", EUR: 0.125}}},
			{TaskID: "t2", EUR: 0.375, Lines: []CostLine{{Kind: "model", Ref: "coding-cheap", EUR: 0.25}, {Kind: "model", Ref: "review/review-frontier", EUR: 0.125}}},
		},
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
	req := HoldRequest{TaskID: "a", Kind: KindModel, Ref: "coding-cheap", InTokens: 1000, Price: testPrice, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf}

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
	b, err := ReleaseHold(ctx, db, h, &cost)
	if err != nil || !near(b.Spent, 0.014) || !near(b.Booked, 0.004) || b.OverHold != "" {
		t.Fatalf("release = (%+v, %v), want spent 0.014, booked 0.004, no flag", b, err)
	}
	// released once only
	if b, err := ReleaseHold(ctx, db, h, &cost); err != nil || !near(b.Spent, 0.014) || b.Booked != 0 {
		t.Fatalf("second release = (%+v, %v), want spent unchanged at 0.014, nothing booked", b, err)
	}
	// 0.006 free: 0.005 after the input buys 1250 tokens
	h2, err := TakeHold(ctx, db, req)
	if err != nil || h2.MaxOut != 1250 {
		t.Fatalf("hold after release = (%+v, %v), want 1250 tokens", h2, err)
	}
	// unknown cost: the hold is booked in full
	if b, err := ReleaseHold(ctx, db, h2, nil); err != nil || !near(b.Spent, 0.02) || !near(b.Booked, 0.006) || b.OverHold != "" {
		t.Fatalf("release without a cost = (%+v, %v), want spent 0.02, the hold's 0.006 booked, no flag", b, err)
	}

	// the day: 0.01 booked in the ledger; a day cap of 0.01 leaves task b
	// nothing. A standing resource's hours are its project's monthly line
	// and take nothing of the day (ruling 147).
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('standing:sf/app', 'standing', 'resource/1', 5)`); err != nil {
		t.Fatal(err)
	}
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
	if b, err := ReleaseHold(ctx, db, hb, &cost); err != nil || !near(b.Spent, 0.003) {
		t.Fatalf("release of a swept hold = (%v, %v), want b's spent 0.003 (the sweep's)", b.Spent, err)
	}
	// a swept hold of 0.001 whose call cost 0.004: the sweep booked 0.001,
	// the release books the 0.003 above it
	swept := Hold{ID: -1, TaskID: "b", Kind: KindModel, Ref: "y", EUR: 0.001}
	over := 0.004
	b, err = ReleaseHold(ctx, db, swept, &over)
	if err != nil || !near(b.Spent, 0.006) || !near(b.Booked, 0.003) || b.OverHold != "COST OVER HOLD: model y cost 0.004000 EUR against a hold of 0.001000 EUR; booked in full" {
		t.Fatalf("late release above the swept hold = (%+v, %v), want b's spent 0.006, flagged", b, err)
	}
	// a cost that is not a finite non-negative number books the hold
	for _, bad := range []float64{math.NaN(), math.Inf(1), -0.5} {
		h, err := TakeHold(ctx, db, HoldRequest{TaskID: "b", Kind: KindModel, Ref: "z", Price: testPrice, WantOut: 1000, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf})
		if err != nil {
			t.Fatal(err)
		}
		before := 0.0
		db.QueryRowContext(ctx, `SELECT spent_eur::float8 FROM tasks WHERE id = 'b'`).Scan(&before)
		if b, err := ReleaseHold(ctx, db, h, &bad); err != nil || !near(b.Spent, before+0.004) {
			t.Fatalf("release at cost %v = (%v, %v), want the hold 0.004 booked on top of %v", bad, b.Spent, err, before)
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
	want := []string{"a model coding-cheap 0.004", "a model coding-cheap 0.006", "standing:sf/app standing resource/1 5", "b model x 0.003", "b model y 0.003",
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
	req := HoldRequest{TaskID: "a", Kind: KindTool, Ref: ToolWebScrape, EUR: 0.02, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf,
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
	if b, err := ReleaseHold(ctx, db, h1, &zeroEUR); err != nil || b.Spent != 0 {
		t.Fatalf("free release = (%v, %v)", b.Spent, err)
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
	if b, err := ReleaseHold(ctx, db, h3, nil); err != nil || !near(b.Spent, 0.025) {
		t.Fatalf("release without a cost = (%v, %v), want spent 0.025", b.Spent, err)
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

// Many concurrent calls of two tasks against a day, then a month, with
// room for exactly four: exactly four get a hold, the rest the cap's
// refusal.
func TestHoldsConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name             string
		dayEUR, monthEUR float64
		decision         string
	}{
		{"day", 0.032, 50, "refused_day_cap"},
		{"month", 20, 0.032, "refused_month_cap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := context.Background()
			if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status) VALUES ('a', 1, 'running'), ('b', 1, 'running')`); err != nil {
				t.Fatal(err)
			}
			// each call: 0 input, 2000 tokens at 4e-6 = 0.008; the cap has 0.032
			var mu sync.Mutex
			var wg sync.WaitGroup
			granted, refused := 0, 0
			for i := range 16 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					task := []string{"a", "b"}[i%2]
					_, err := TakeHold(ctx, db, HoldRequest{TaskID: task, Kind: KindModel, Ref: "m", Price: testPrice, WantOut: 2000, DayEUR: tc.dayEUR, MonthEUR: tc.monthEUR, ProjectEUR: inf})
					mu.Lock()
					defer mu.Unlock()
					var r *Refusal
					switch {
					case err == nil:
						granted++
					case errors.As(err, &r) && r.Decision == tc.decision:
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
		})
	}
}

// The month is the UTC calendar month over the ledger, standing lines left
// out, less every outstanding hold. Hand-derived: 30 EUR on the previous
// month's last day and 10 EUR of a standing line do not count; 0.03 on the
// 1st and 0.01 today do, so a cap of 0.05 leaves 0.01, and another task's
// hold of 0.005 leaves 0.005 free: 1250 output tokens at 4e-6.
func TestMonthHeadroom(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('m', 5, 0, 'running'), ('o', 5, 0, 'running');
		INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
			('old', date_trunc('month', now() AT TIME ZONE 'UTC')::date - 1, 'model', 'x', 30),
			('first', date_trunc('month', now() AT TIME ZONE 'UTC')::date, 'model', 'x', 0.03),
			('standing:sf/app', (now() AT TIME ZONE 'UTC')::date, 'standing', 'resource/1', 10),
			('today', (now() AT TIME ZONE 'UTC')::date, 'model', 'x', 0.01);
		INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('o', 'model', 'x', 0.005, now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	req := HoldRequest{TaskID: "m", Kind: KindModel, Ref: "m", Price: testPrice, DayEUR: 20, MonthEUR: 0.05, ProjectEUR: inf}
	h, err := TakeHold(ctx, db, req)
	if err != nil || h.MaxOut != 1250 || !near(h.EUR, 0.005) || !near(h.Headroom, 0.01) || h.Bound != RefusedMonthCap {
		t.Fatalf("month-bound hold = (%+v, %v), want 1250 tokens, 0.005 EUR, headroom 0.01 bound by the month", h, err)
	}
	var r *Refusal
	if _, err := TakeHold(ctx, db, req); !errors.As(err, &r) || r.Decision != RefusedMonthCap || !strings.Contains(r.Reason, "budget.month_eur") {
		t.Fatalf("hold past the month = %v, want refused_month_cap naming budget.month_eur", err)
	}
	// a resource reservation and a tool call meet the same month
	res := HoldRequest{TaskID: "m", Kind: KindResource, Ref: "cx23", EUR: 0.0001, DayEUR: 20, MonthEUR: 0.05, ProjectEUR: inf}
	if _, err := TakeHold(ctx, db, res); !errors.As(err, &r) || r.Decision != RefusedMonthCap {
		t.Fatalf("reservation past the month = %v, want refused_month_cap", err)
	}
	// 0 stops all spend
	if _, err := TakeHold(ctx, db, HoldRequest{TaskID: "o", Kind: KindModel, Ref: "m", Price: testPrice, WantOut: 1, DayEUR: 20, MonthEUR: 0, ProjectEUR: inf}); !errors.As(err, &r) || r.Decision != RefusedMonthCap {
		t.Fatalf("hold under a month cap of 0 = %v, want refused_month_cap", err)
	}
}

// A project's month cap (budget.month_eur in its file) bounds its tasks
// together: the cap less the month's ledger rows of the tasks of its
// repository, earlier months left out, less their outstanding holds; a
// standing line (task id standing:<repository>) joins no task. Hand-derived:
// p/p's cap 1.00, its task o spent 0.90
// this month (and 5 last month); a has budget 2, the day cap is 20 and the
// month 50, so a's headroom is task 2, day 19.10, month 49.10, project
// 0.10. Task b of q/q has no cap and the same numbers.
func TestProjectCapBoundsHeadroom(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, budget_eur, spent_eur, status, repository) VALUES
			('a', 2, 0, 'running', 'p/p'), ('o', 2, 0.9, 'running', 'p/p'), ('b', 2, 0, 'running', 'q/q');
		INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
			('o', date_trunc('month', now() AT TIME ZONE 'UTC')::date - 1, 'model', 'x', 5),
			('o', (now() AT TIME ZONE 'UTC')::date, 'model', 'x', 0.9),
			('standing:p/p', (now() AT TIME ZONE 'UTC')::date, 'standing', 'resource/1', 3)`); err != nil {
		t.Fatal(err)
	}
	free := func(r HoldRequest) Free {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		f, err := FreeHeadroom(ctx, tx, r)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	tool := HoldRequest{TaskID: "a", Kind: KindTool, Ref: ToolWebScrape, EUR: 0.2, Cap: ToolCap{MaxRequests: 3, MaxEUR: 1},
		DayEUR: 20, MonthEUR: 50, Project: "p", ProjectEUR: 1}
	f := free(tool)
	if !near(f.TaskHeadroom, 2) || !near(f.DayHeadroom, 19.1) || !near(f.MonthHeadroom, 49.1) || !near(f.ProjectHeadroom, 0.1) ||
		!near(f.Task, 2) || !near(f.Day, 19.1) || !near(f.Month, 49.1) || !near(f.Project, 0.1) {
		t.Fatalf("a's headroom = %+v, want task 2, day 19.10, month 49.10, project 0.10", f)
	}
	if got, bound := HeadroomBound(f.Task, f.Day, f.Month, f.Project); !near(got, 0.1) || bound != RefusedProjectCap {
		t.Fatalf("a's free headroom = (%g, %s), want 0.10 bound by the project's cap", got, bound)
	}
	var r *Refusal
	if _, err := TakeHold(ctx, db, tool); !errors.As(err, &r) || r.Decision != RefusedProjectCap ||
		r.Reason != "web_scrape: worst case 0.200000 EUR exceeds the free headroom 0.100000 EUR (task 2.000000, day 19.100000, month 49.100000, project 0.100000); "+
			"the tool call's worst case exceeds the project's remaining month headroom (budget.month_eur in projects/p.yaml), and only a human raises it, by a commit" {
		t.Fatalf("a's call with a worst case of 0.20 = %v, want refused_project_cap", err)
	}
	other := tool
	other.TaskID, other.Project, other.ProjectEUR = "b", "", inf
	h, err := TakeHold(ctx, db, other)
	if err != nil || !near(h.EUR, 0.2) || !near(h.Headroom, 2) || h.Bound != "refused_budget" {
		t.Fatalf("b's call = (%+v, %v), want 0.20 held under headroom 2 bound by the task", h, err)
	}
	// o's hold counts against p/p's cap, b's (q/q) only against the day and
	// month: 1000 input tokens and 1024 output tokens hold 0.005096
	model := HoldRequest{TaskID: "o", Kind: KindModel, Ref: "m", InTokens: 1000, Price: testPrice, WantOut: 1024, DayEUR: 20, MonthEUR: 50, Project: "p", ProjectEUR: 1}
	h, err = TakeHold(ctx, db, model)
	if err != nil || h.MaxOut != 1024 || !near(h.EUR, 0.005096) || !near(h.Headroom, 0.1) || h.Bound != RefusedProjectCap {
		t.Fatalf("o's hold = (%+v, %v), want 1024 tokens, 0.005096 EUR, headroom 0.10 bound by the project's cap", h, err)
	}
	if f := free(tool); !near(f.ProjectHeadroom, 0.1) || !near(f.Project, 0.094904) || !near(f.Day, 18.894904) || !near(f.Month, 48.894904) {
		t.Fatalf("a's headroom after both holds = %+v, want project 0.094904, day 18.894904, month 48.894904", f)
	}
	// a cap of 0.90 is spent: a's model call is refused
	model.TaskID, model.ProjectEUR = "a", 0.9
	if _, err := TakeHold(ctx, db, model); !errors.As(err, &r) || r.Decision != RefusedProjectCap ||
		!strings.HasSuffix(r.Reason, "; the project cap budget.month_eur in projects/p.yaml is spent, and only a human raises it, by a commit") {
		t.Fatalf("a's model call under a spent cap = %v, want refused_project_cap naming projects/p.yaml", err)
	}
}

// A project without a cap costs its holds no project query: with
// tasks.repository gone inside the transaction, the uncapped headroom still
// reads, and only a capped one fails on the column.
func TestNoProjectQueryWithoutACap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository) VALUES ('a', 2, 0, 'running', 'p/p')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `ALTER TABLE tasks DROP COLUMN repository`); err != nil {
		t.Fatal(err)
	}
	req := HoldRequest{TaskID: "a", Kind: KindModel, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf}
	if f, err := FreeHeadroom(ctx, tx, req); err != nil || !math.IsInf(f.ProjectHeadroom, 1) || !math.IsInf(f.Project, 1) || !near(f.Task, 2) {
		t.Fatalf("uncapped headroom without tasks.repository = (%+v, %v), want task 2 and the project's +Inf", f, err)
	}
	req.Project, req.ProjectEUR = "p", 1
	if _, err := FreeHeadroom(ctx, tx, req); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("capped headroom without tasks.repository = %v, want the project query to fail on the column", err)
	}
}

// The day and the month are UTC whatever the session's zone. Hand-derived
// at 2026-10-01 05:00 UTC, 2026-09-30 22:00 in Los Angeles: 30 EUR on
// 2026-09-30 is neither today nor this month; 0.04 on 2026-10-01 is both,
// so a month cap of 0.05 leaves 0.01, 2500 tokens at 4e-6. Read in the
// session's zone, the 30 EUR would count and refuse the call.
func TestHeadroomIsUTC(t *testing.T) {
	testDB(t)
	u, err := url.Parse(schematest.DSN(t, "policy"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("timezone", "America/Los_Angeles")
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var zone string
	if err := db.QueryRowContext(ctx, `SHOW TIME ZONE`).Scan(&zone); err != nil || zone != "America/Los_Angeles" {
		t.Fatalf("session zone = %q, %v", zone, err)
	}
	at := time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)
	defer func(was func() time.Time) { now = was }(now)
	now = func() time.Time { return at.In(time.FixedZone("UTC-7", -7*3600)) }
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('z', 5, 0, 'running');
		INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ('sep', '2026-09-30', 'model', 'x', 30), ('oct', '2026-10-01', 'model', 'x', 0.04)`); err != nil {
		t.Fatal(err)
	}
	h, err := TakeHold(ctx, db, HoldRequest{TaskID: "z", Kind: KindModel, Ref: "m", Price: testPrice, DayEUR: 20, MonthEUR: 0.05, ProjectEUR: inf})
	if err != nil || h.MaxOut != 2500 || h.Bound != RefusedMonthCap {
		t.Fatalf("hold at 2026-10-01 05:00 UTC = (%+v, %v), want 2500 tokens bound by the month", h, err)
	}
	// the day: a cap of 1 leaves 0.96 of 2026-10-01; 2026-09-30's 30 is not today
	h, err = TakeHold(ctx, db, HoldRequest{TaskID: "z", Kind: KindModel, Ref: "m", Price: testPrice, WantOut: 1024, DayEUR: 1, MonthEUR: 50, ProjectEUR: inf})
	if err != nil || !near(h.Headroom, 0.96) || h.Bound != "refused_day_cap" {
		t.Fatalf("day-bound hold = (%+v, %v), want headroom 0.96 bound by the day", h, err)
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
			req := HoldRequest{TaskID: id, Kind: kind, Ref: "coding-cheap", InTokens: 10, Price: testPrice, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf,
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

// A call cut by a restart never releases its hold: the sweep books it and
// writes the call's audit row, decision swept, so every booked call has
// one (#276's security-audit call, cut by the roll of 28afe1f at 23:18:15Z,
// had none). Kind and alias come from the hold's ref: purpose/alias for a
// factoryd call, the alias for an agent's, the tool's name for a tool call.
func TestSweepWritesAnAuditRowPerHold(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status) VALUES ('s', 5, 'failed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES
		('s', 'model', 'audit/coding-frontier', 0.146422, now() - interval '1 second'),
		('s', 'model', 'coding-cheap', 0.004, now() - interval '1 second'),
		('s', 'tool', 'web_scrape', 0.02, now() - interval '1 second'),
		('s', 'model', 'coding-cheap', 0.5, now() + interval '1 hour'),
		('s', 'resource', 'cx23', 0.0096, now() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	if n, err := SweepHolds(ctx, db); err != nil || n != 3 {
		t.Fatalf("sweep = (%d, %v), want 3: the open hold and the resource reservation stay", n, err)
	}
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT kind || ' ' || decision || ' ' || model_alias || ' ' || tool || ' ' || spend_eur::text || ' ' || hold_eur::text || ' ' || (reason <> '')::text
		FROM audit WHERE task_id = 's' ORDER BY spend_eur`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		got = append(got, l)
	}
	want := []string{
		"agent swept coding-cheap  0.004 0.004 true",
		"agent swept  web_scrape 0.02 0.02 true",
		"audit swept coding-frontier  0.146422 0.146422 true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit rows = %q, want %q", got, want)
	}
	var spent float64
	if err := db.QueryRowContext(ctx, `SELECT spent_eur::float8 FROM tasks WHERE id = 's'`).Scan(&spent); err != nil || !near(spent, 0.170422) {
		t.Fatalf("spent = %v (%v), want 0.170422, the three swept holds", spent, err)
	}
}
