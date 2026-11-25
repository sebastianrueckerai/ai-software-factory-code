package policy

import (
	"context"
	"database/sql"
	"errors"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
	"math"
	"net/url"
	"os"
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
	// headroom queries and the sweep's audit rows read
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tasks (
		id text PRIMARY KEY, budget_eur numeric NOT NULL, spent_eur numeric NOT NULL DEFAULT 0, status text NOT NULL, repository text NOT NULL DEFAULT '',
		author text NOT NULL DEFAULT '', settled_at timestamptz, finished_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	// the gateway's audit table, the columns the sweep writes
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS audit (
		id bigserial PRIMARY KEY, ts timestamptz NOT NULL DEFAULT now(), task_id text NOT NULL, decision text NOT NULL,
		reason text NOT NULL, model_alias text NOT NULL DEFAULT '', tool text NOT NULL DEFAULT '', spend_eur numeric,
		hold_eur numeric, kind text NOT NULL DEFAULT 'agent', author text NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	// the gateway's cap resets, as its migrations create them
	for _, name := range []string{"20261120130000_cap_resets.sql", "20261124120000_cap_resets_premium.sql", "20261129100600_cap_resets_pool.sql"} {
		resets, err := os.ReadFile("../schema/migrations/gateway/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(resets)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE policy_activations, policy_versions, ledger, holds, tasks, audit, cap_resets CASCADE`); err != nil {
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
		('t1', (now() AT TIME ZONE 'UTC')::date, 'model', 'small', 0.5),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'small', 0.25),
		('t1', (now() AT TIME ZONE 'UTC')::date, 'settlement', 'litellm', 0.125),
		('t1', '2026-01-01', 'model', 'small', 3),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'review/big', 0.0625),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'review/big', 0.0625)`); err != nil {
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
			{TaskID: "t1", EUR: 3.625, Lines: []CostLine{{Kind: "model", Ref: "small", EUR: 3.5}, {Kind: "settlement", Ref: "litellm", EUR: 0.125}}},
			{TaskID: "t2", EUR: 0.375, Lines: []CostLine{{Kind: "model", Ref: "review/big", EUR: 0.125}, {Kind: "model", Ref: "small", EUR: 0.25}}},
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

var testPrice = Price{In: 1e-6, Out: 4e-6, MaxOutput: unbound}

// unbound is an alias's output limit no test's headroom reaches.
const unbound = 1 << 30

func TestHolds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES
		('a', 0.02, 0.01, 'running'), ('b', 1, 0, 'running')`); err != nil {
		t.Fatal(err)
	}
	req := HoldRequest{TaskID: "a", Kind: KindModel, Ref: "small", Bytes: 1000, Price: testPrice, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf}

	// task a: 0.01 free; 1000 input tokens cost 0.001, 0.009 buys 2250 output tokens
	h, err := TakeHold(ctx, db, req)
	if err != nil {
		t.Fatal(err)
	}
	if h.MaxOut != 2250 || !near(h.EUR, 0.01) {
		t.Fatalf("hold = %+v, want 2250 tokens, 0.01 EUR", h)
	}
	// the hold takes all of a's headroom: the next call is refused
	var r *Refusal
	if _, err := TakeHold(ctx, db, req); !errors.As(err, &r) || r.Decision != "refused_budget" {
		t.Fatalf("second hold = %v, want refused_budget", err)
	}
	// released at its real cost, the rest is free again
	cost := 0.004
	b, err := ReleaseHold(ctx, db, h, &cost, Usage{})
	if err != nil || !near(b.Spent, 0.014) || !near(b.Booked, 0.004) || b.OverHold != "" {
		t.Fatalf("release = (%+v, %v), want spent 0.014, booked 0.004, no flag", b, err)
	}
	// released once only
	if b, err := ReleaseHold(ctx, db, h, &cost, Usage{}); err != nil || !near(b.Spent, 0.014) || b.Booked != 0 {
		t.Fatalf("second release = (%+v, %v), want spent unchanged at 0.014, nothing booked", b, err)
	}
	// 0.006 free: 0.005 after the input buys 1250 tokens
	h2, err := TakeHold(ctx, db, req)
	if err != nil || h2.MaxOut != 1250 {
		t.Fatalf("hold after release = (%+v, %v), want 1250 tokens", h2, err)
	}
	// unknown cost: the hold is booked in full
	if b, err := ReleaseHold(ctx, db, h2, nil, Usage{}); err != nil || !near(b.Spent, 0.02) || !near(b.Booked, 0.006) || b.OverHold != "" {
		t.Fatalf("release without a cost = (%+v, %v), want spent 0.02, the hold's 0.006 booked, no flag", b, err)
	}

	// the day: 0.01 booked in the ledger; a day cap of 0.01 leaves task b
	// nothing. A standing resource's hours are its project's monthly line
	// and take nothing of the day.
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur) VALUES ('standing:sf/app', 'standing', 'resource/1', 5)`); err != nil {
		t.Fatal(err)
	}
	bReq := req
	bReq.TaskID, bReq.DayEUR = "b", 0.01
	bReq.Bytes = 0
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
	if b, err := ReleaseHold(ctx, db, hb, &cost, Usage{}); err != nil || !near(b.Spent, 0.003) {
		t.Fatalf("release of a swept hold = (%v, %v), want b's spent 0.003 (the sweep's)", b.Spent, err)
	}
	// a swept hold of 0.001 whose call cost 0.004: the sweep booked 0.001,
	// the release books the 0.003 above it
	swept := Hold{ID: -1, TaskID: "b", Kind: KindModel, Ref: "y", EUR: 0.001}
	over := 0.004
	b, err = ReleaseHold(ctx, db, swept, &over, Usage{})
	if err != nil || !near(b.Spent, 0.006) || !near(b.Booked, 0.003) || b.OverHold != "COST OVER HOLD: model y cost less than 0.01 EUR against a hold of less than 0.01 EUR; booked in full" {
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
		if b, err := ReleaseHold(ctx, db, h, &bad, Usage{}); err != nil || !near(b.Spent, before+0.004) {
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
	want := []string{"a model small 0.004", "a model small 0.006", "standing:sf/app standing resource/1 5", "b model x 0.003", "b model y 0.003",
		"b model z 0.004", "b model z 0.004", "b model z 0.004"}
	if !reflect.DeepEqual(ledger, want) {
		t.Fatalf("ledger = %q, want %q", ledger, want)
	}
}

// An estimate of calls not yet made reads the ref's history: without a
// settled call of its own task on the ref, Earlier is the ref's newest
// settled call of any task (c's, 514 tokens in 2,048 bytes; d's row has no
// usage, another ref's does not count), and the mean output of the ref's
// settled calls is (6,000 + 10,000) / 2 = 8,000. The task's own call then
// calibrates it, and the mean is (6,000 + 10,000 + 2,000) / 3 = 6,000. Of
// 101 calls on audit/big only the newest 100 count: 1,000 each, the
// oldest's 100,000 left out. A ref without a call has neither.
func TestCalibrateEstimateReadsTheRef(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes) VALUES
		('b', 'model', 'review/big', 0.01, 257, 0, 6000, 1024), ('c', 'model', 'review/big', 0.01, 514, 0, 10000, 2048),
		('c', 'model', 'review/small', 0.01, 1, 0, 99999, 1), ('d', 'model', 'review/big', 0.01, 0, 0, 50000, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes)
		SELECT 'e', 'model', 'audit/big', 0.01, 100, 0, CASE WHEN i = 1 THEN 100000 ELSE 1000 END, 400 FROM generate_series(1, 101) i ORDER BY i`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, ref string
		earlier   Usage
		mean      int
	}{
		{"any task's", "review/big", Usage{PromptTokens: 514, BodyBytes: 2048}, 8000},
		{"the newest 100", "audit/big", Usage{PromptTokens: 100, BodyBytes: 400}, 1000},
		{"none", "review/premium", Usage{}, 0},
	} {
		r, mean, err := CalibrateEstimate(ctx, db, HoldRequest{TaskID: "a", Kind: KindModel, Ref: tc.ref})
		if err != nil || r.Earlier != tc.earlier || mean != tc.mean {
			t.Fatalf("%s: earlier %+v, mean %d, err %v; want %+v, %d", tc.name, r.Earlier, mean, err, tc.earlier, tc.mean)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes) VALUES
		('a', 'model', 'review/big', 0.01, 100, 0, 2000, 1000), ('f', 'model', 'review/big', 0.01, 0, 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	r, mean, err := CalibrateEstimate(ctx, db, HoldRequest{TaskID: "a", Kind: KindModel, Ref: "review/big"})
	if want := (Usage{PromptTokens: 100, BodyBytes: 1000}); err != nil || r.Earlier != want || mean != 6000 {
		t.Fatalf("own call: earlier %+v, mean %d, err %v; want %+v, 6000", r.Earlier, mean, err, want)
	}
}

// A model call's ledger row keeps its usage and request bytes, and the
// task's next hold on the same ref calibrates from the newest row with
// usage (the figures of TestFitCalibrated); a row without usage, another
// ref's and another task's do not count. The cache floor is measured on
// the ref's calls that followed an earlier call of their task: none
// measured prices the prefix at the input price, a floor of the whole
// prefix keeps the cache-read price, and one call served half its prefix
// halves it. Another ref's calls measure no floor of this one.
func TestModelHoldCalibrates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status) VALUES ('c', 5, 'running'), ('d', 5, 'running')`); err != nil {
		t.Fatal(err)
	}
	req := HoldRequest{TaskID: "c", Kind: KindModel, Ref: "small", Bytes: 480000, Price: Price{In: 0.30e-6, CacheRead: 0.03e-6, Out: 1.50e-6, MaxOutput: unbound},
		WantOut: 4096, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf}
	// 480,000 × 0.30e-6 + 4,096 × 1.50e-6
	h, err := TakeHold(ctx, db, req)
	if err != nil || !near(h.EUR, 0.150144) {
		t.Fatalf("first hold = (%+v, %v), want 0.150144 EUR", h, err)
	}
	cost := 0.007818
	if _, err := ReleaseHold(ctx, db, h, &cost, Usage{PromptTokens: 135000, CachedTokens: 131600, OutputTokens: 900, BodyBytes: 480000}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes) VALUES
		('c', 'model', 'small', 0.02, 0, 0, 0, 500000), ('c', 'model', 'review/small', 0.01, 1000, 0, 10, 2000),
		('d', 'model', 'small', 0.01, 5000, 0, 10, 100000)`); err != nil {
		t.Fatal(err)
	}
	req.Bytes = 495000
	if h, err := TakeHold(ctx, db, req); err != nil || !near(h.EUR, 0.05835103125) {
		t.Fatalf("calibrated hold without a measured floor = (%+v, %v), want 0.05835103125 EUR", h, err)
	}
	// e's second call found its first call's 1,000 prompt tokens cached;
	// its two calls on review/small found nothing cached
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes) VALUES
		('e', 'model', 'small', 0.01, 1000, 0, 10, 4000), ('e', 'model', 'small', 0.01, 1200, 1000, 10, 4800),
		('e', 'model', 'review/small', 0.01, 1000, 0, 10, 4000), ('e', 'model', 'review/small', 0.01, 1200, 0, 10, 4800)`); err != nil {
		t.Fatal(err)
	}
	if h, err := TakeHold(ctx, db, req); err != nil || !near(h.EUR, 0.02190103125) {
		t.Fatalf("calibrated hold under a floor of the whole prefix = (%+v, %v), want 0.02190103125 EUR", h, err)
	}
	// f's second call found 1,000 of its first call's 2,000 tokens cached
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, eur, prompt_tokens, cached_tokens, output_tokens, body_bytes) VALUES
		('f', 'model', 'small', 0.01, 2000, 0, 10, 8000), ('f', 'model', 'small', 0.01, 3000, 1000, 10, 12000)`); err != nil {
		t.Fatal(err)
	}
	if h, err := TakeHold(ctx, db, req); err != nil || !near(h.EUR, 0.04012603125) {
		t.Fatalf("calibrated hold under a floor of half = (%+v, %v), want 0.04012603125 EUR", h, err)
	}
	req.Ref = "big"
	if h, err := TakeHold(ctx, db, req); err != nil || !near(h.EUR, 0.154644) {
		t.Fatalf("first hold on another ref = (%+v, %v), want the byte bound's 0.154644 EUR", h, err)
	}
	calls, err := ModelCalls(ctx, db, "c")
	if err != nil {
		t.Fatal(err)
	}
	for i := range calls {
		if calls[i].At.IsZero() {
			t.Fatalf("call %d has no time", i)
		}
		calls[i].At = time.Time{}
	}
	want := []ModelCall{{Ref: "small", EUR: 0.007818, Usage: Usage{135000, 131600, 900, 480000}},
		{Ref: "small", EUR: 0.02, Usage: Usage{BodyBytes: 500000}}, {Ref: "review/small", EUR: 0.01, Usage: Usage{1000, 0, 10, 2000}}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("model calls = %+v, want %+v", calls, want)
	}
}

// A session's opening is every model call that started before its first
// one ended (roadmap 243): s-a's three calls, its side call on
// small on its own alias, not a4, started after a1 ended; s-b's
// one. s-c's first call was on small, so its call after
// the climb opens nothing on big; s-d's first call, a swept
// hold, has no measure, so s-d counts none; factoryd's calls, rows from
// before the session and a late release without a hold count none either.
// The window reads o's own rows (ReadOpening scopes the task: another
// task's openings are no hold of its). Newest first, by the first call.
// Without an opening on the ref every ref's count of the task's own
// sessions, a call on its session's first call's alias read as on the
// ref; on a task's ledger without one there is none. Of the newest
// openingSessions sessions alone: s-old counts until a twentieth newer
// session opens. A released call's row keeps its hold's session, start and
// asked output, and each call carries its alias's calibration for the task.
func TestOpeningIsWhatASessionStartsBeforeItsFirstCallEnds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status) VALUES ('o', 5, 'running')`); err != nil {
		t.Fatal(err)
	}
	opening := func(ref string, want Opening) {
		t.Helper()
		if o, err := ReadOpening(ctx, db, "o", ref); err != nil || !reflect.DeepEqual(o, want) {
			t.Fatalf("opening on %s = (%+v, %v), want %+v", ref, o, err, want)
		}
	}
	opening("big", Opening{AnyRef: true})

	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, session, held_at, created_at, want_out, eur, prompt_tokens, body_bytes) VALUES
		('o', 'model', 'big', 's-a', '2026-10-15T10:00:00Z', '2026-10-15T10:00:08Z', 24576, 0.01, 1607, 7031),
		('o', 'model', 'big', 's-a', '2026-10-15T10:00:01Z', '2026-10-15T10:00:09Z', 24576, 0.02, 10144, 44342),
		('o', 'model', 'small', 's-a', '2026-10-15T10:00:02Z', '2026-10-15T10:00:04Z', 4096, 0.001, 300, 1200),
		('o', 'model', 'big', 's-a', '2026-10-15T10:00:09Z', '2026-10-15T10:00:15Z', 24576, 0.02, 12000, 50000),
		('o', 'model', 'big', 's-b', '2026-10-15T10:00:20Z', '2026-10-15T10:00:25Z', 24576, 0.02, 9946, 43683),
		('o', 'model', 'small', 's-c', '2026-10-15T10:00:30Z', '2026-10-15T10:00:35Z', 24576, 0.01, 11000, 50000),
		('o', 'model', 'big', 's-c', '2026-10-15T10:00:40Z', '2026-10-15T10:01:40Z', 24576, 0.30, 97021, 368195),
		('o', 'model', 'big', 's-d', '2026-10-15T10:00:50Z', '2026-10-15T11:00:50Z', 24576, 0.15, 0, 0),
		('o', 'model', 'big', 's-d', '2026-10-15T10:00:51Z', '2026-10-15T10:00:55Z', 24576, 0.02, 2000, 90000),
		('o', 'model', 'review/big', '', '2026-10-15T10:01:00Z', '2026-10-15T10:01:05Z', 4096, 0.02, 20000, 95000),
		('o', 'model', 'big', '', NULL, '2026-10-15T09:00:00Z', 0, 0.40, 140000, 600000),
		('o', 'model', 'big', 's-b', NULL, '2026-10-15T10:00:26Z', 0, 0.01, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	big := Usage{PromptTokens: 140000, BodyBytes: 600000}
	small := Usage{PromptTokens: 11000, BodyBytes: 50000}
	side := OpeningCall{Ref: "small", Bytes: 1200, WantOut: 4096, Earlier: small}
	a := []OpeningCall{{Ref: "big", Bytes: 7031, WantOut: 24576, Earlier: big}, {Ref: "big", Bytes: 44342, WantOut: 24576, Earlier: big}, side}
	b := []OpeningCall{{Ref: "big", Bytes: 43683, WantOut: 24576, Earlier: big}}
	opening("big", Opening{Sessions: [][]OpeningCall{b, a}})
	opening("coding-strong", Opening{Sessions: [][]OpeningCall{{{Ref: "coding-strong", Bytes: 50000, WantOut: 24576}}, {{Ref: "coding-strong", Bytes: 43683, WantOut: 24576}},
		{{Ref: "coding-strong", Bytes: 7031, WantOut: 24576}, {Ref: "coding-strong", Bytes: 44342, WantOut: 24576}, side}}, AnyRef: true})

	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, session, held_at, created_at, eur, prompt_tokens, body_bytes) VALUES
		('o', 'model', 'coding-window', 's-old', '2026-10-15T11:00:00Z', '2026-10-15T11:00:05Z', 0.01, 20000, 90000);
		INSERT INTO ledger (task_id, kind, ref, session, held_at, created_at, eur, prompt_tokens, body_bytes)
		SELECT 'o', 'model', 'coding-window', 's-w' || g, '2026-10-15T12:00:00Z'::timestamptz + g * interval '1 minute',
		       '2026-10-15T12:00:30Z'::timestamptz + g * interval '1 minute', 0.01, 300, 1000 FROM generate_series(1, 19) g`); err != nil {
		t.Fatal(err)
	}
	window := Usage{PromptTokens: 300, BodyBytes: 1000}
	o, err := ReadOpening(ctx, db, "o", "coding-window")
	if err != nil || len(o.Sessions) != 20 || !reflect.DeepEqual(o.Sessions[19], []OpeningCall{{Ref: "coding-window", Bytes: 90000, Earlier: window}}) {
		t.Fatalf("window = (%+v, %v), want 20 openings, s-old's the oldest", o, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, session, held_at, created_at, eur, prompt_tokens, body_bytes)
		VALUES ('o', 'model', 'coding-window', 's-w20', '2026-10-15T13:00:00Z', '2026-10-15T13:00:30Z', 0.01, 300, 1000)`); err != nil {
		t.Fatal(err)
	}
	if o, err := ReadOpening(ctx, db, "o", "coding-window"); err != nil || len(o.Sessions) != 20 || !reflect.DeepEqual(o.Sessions[19], []OpeningCall{{Ref: "coding-window", Bytes: 1000, Earlier: window}}) {
		t.Fatalf("window = (%+v, %v), want 20 openings without s-old's", o, err)
	}

	h, err := TakeHold(ctx, db, HoldRequest{TaskID: "o", Kind: KindModel, Ref: "coding-held", Session: "s-h", Bytes: 44342,
		Price: Price{In: 3e-6, Out: 1.5e-5, MaxOutput: unbound}, WantOut: 24576, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf})
	if err != nil || h.Session != "s-h" || h.WantOut != 24576 {
		t.Fatalf("hold = (%+v, %v), want s-h's asking 24576", h, err)
	}
	cost := 0.05
	if _, err := ReleaseHold(ctx, db, h, &cost, Usage{PromptTokens: 10144, OutputTokens: 50, BodyBytes: 44342}); err != nil {
		t.Fatal(err)
	}
	var booked string
	if err := db.QueryRowContext(ctx, `SELECT session || ' ' || (held_at < created_at)::text || ' ' || want_out FROM ledger WHERE ref = 'coding-held'`).Scan(&booked); err != nil ||
		booked != "s-h true 24576" {
		t.Fatalf("the booked row = (%q, %v), want s-h's, held before it ended, asking 24576", booked, err)
	}
	opening("coding-held", Opening{Sessions: [][]OpeningCall{{{Ref: "coding-held", Bytes: 44342, WantOut: 24576, Earlier: Usage{PromptTokens: 10144, BodyBytes: 44342}}}}})
}

// The window reads the task's own sessions alone (roadmap 243): a task's
// first session prices on its own openings, never another task's, in the
// ref-scoped window and the any-ref fallback alike. own-a's two cheap
// sessions on the ref beside own-b's three newer expensive ones on the
// same ref: own-a reads its two alone, own-b its three, newest first, and
// own-c, with no row of its own, reads none of them — on the ref and on
// one it never called, where the fallback stays inside the task's own
// rows too.
func TestReadOpeningWindowsTheTasksOwnSessions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, ref, session, held_at, created_at, want_out, eur, prompt_tokens, body_bytes) VALUES
		('own-a', 'model', 'own-window', 's-a1', '2026-10-15T10:00:00Z', '2026-10-15T10:00:02Z', 4096, 0.001, 300, 1000),
		('own-a', 'model', 'own-window', 's-a2', '2026-10-15T10:05:00Z', '2026-10-15T10:05:02Z', 4096, 0.002, 600, 2000),
		('own-b', 'model', 'own-window', 's-b1', '2026-10-15T11:00:00Z', '2026-10-15T11:00:02Z', 24576, 0.05, 11000, 50000),
		('own-b', 'model', 'own-window', 's-b2', '2026-10-15T11:05:00Z', '2026-10-15T11:05:02Z', 24576, 0.06, 12000, 60000),
		('own-b', 'model', 'own-window', 's-b3', '2026-10-15T11:10:00Z', '2026-10-15T11:10:02Z', 24576, 0.07, 13000, 70000)`); err != nil {
		t.Fatal(err)
	}
	cheap := Usage{PromptTokens: 600, BodyBytes: 2000}
	dear := Usage{PromptTokens: 13000, BodyBytes: 70000}
	if o, err := ReadOpening(ctx, db, "own-a", "own-window"); err != nil ||
		!reflect.DeepEqual(o, Opening{Sessions: [][]OpeningCall{
			{{Ref: "own-window", Bytes: 2000, WantOut: 4096, Earlier: cheap}},
			{{Ref: "own-window", Bytes: 1000, WantOut: 4096, Earlier: cheap}}}}) {
		t.Fatalf("own-a's opening = (%+v, %v), want its own 2 cheap sessions, newest first", o, err)
	}
	if o, err := ReadOpening(ctx, db, "own-b", "own-window"); err != nil ||
		!reflect.DeepEqual(o, Opening{Sessions: [][]OpeningCall{
			{{Ref: "own-window", Bytes: 70000, WantOut: 24576, Earlier: dear}},
			{{Ref: "own-window", Bytes: 60000, WantOut: 24576, Earlier: dear}},
			{{Ref: "own-window", Bytes: 50000, WantOut: 24576, Earlier: dear}}}}) {
		t.Fatalf("own-b's opening = (%+v, %v), want its own 3 expensive sessions, newest first", o, err)
	}
	if o, err := ReadOpening(ctx, db, "own-c", "own-window"); err != nil || !reflect.DeepEqual(o, Opening{AnyRef: true}) {
		t.Fatalf("own-c's opening on the ref = (%+v, %v), want none", o, err)
	}
	// the any-ref fallback stays inside the task's own sessions: own-a's
	// two, read on a ref it never called, own-c's none
	if o, err := ReadOpening(ctx, db, "own-a", "own-other"); err != nil ||
		!reflect.DeepEqual(o, Opening{Sessions: [][]OpeningCall{
			{{Ref: "own-other", Bytes: 2000, WantOut: 4096}},
			{{Ref: "own-other", Bytes: 1000, WantOut: 4096}}}, AnyRef: true}) {
		t.Fatalf("own-a's any-ref opening = (%+v, %v), want its own 2 sessions alone", o, err)
	}
	if o, err := ReadOpening(ctx, db, "own-c", "own-other"); err != nil || !reflect.DeepEqual(o, Opening{AnyRef: true}) {
		t.Fatalf("own-c's any-ref opening = (%+v, %v), want none", o, err)
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
	if b, err := ReleaseHold(ctx, db, h1, &zeroEUR, Usage{}); err != nil || b.Spent != 0 {
		t.Fatalf("free release = (%v, %v)", b.Spent, err)
	}
	cost := 0.005
	if _, err := ReleaseHold(ctx, db, h2, &cost, Usage{}); err != nil {
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
	if b, err := ReleaseHold(ctx, db, h3, nil, Usage{}); err != nil || !near(b.Spent, 0.025) {
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
	if err != nil || h.MaxOut != 1250 || !near(h.EUR, 0.005) {
		t.Fatalf("month-bound hold = (%+v, %v), want 1250 tokens, 0.005 EUR", h, err)
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

// LedgerEnforced reads what the caps count: the day's and the month's
// ledger spend with every outstanding hold, standing lines left out, the
// premium figure on Premium rows alone. Hand-derived: 30 EUR on the
// previous month's last day and a 10 EUR standing line count nowhere,
// today's 0.01 counts once, and the holds 0.005 and a Premium 0.02 raise
// the day and the month to 0.035, the premium to 0.02 alone — the figures
// a cap stops at. Earlier days of the month count too (TestMonthHeadroom).
func TestLedgerEnforcedCountsTheHolds(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('m', 5, 0, 'running'), ('o', 5, 0, 'running');
		INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
			('old', date_trunc('month', now() AT TIME ZONE 'UTC')::date - 1, 'model', 'x', 30),
			('standing:sf/app', (now() AT TIME ZONE 'UTC')::date, 'standing', 'resource/1', 10),
			('today', (now() AT TIME ZONE 'UTC')::date, 'model', 'x', 0.01);
		INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES
			('o', 'model', 'x', 0.005, now() + interval '1 hour'),
			('o', 'model', 'factory/premium', 0.02, now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	e, err := LedgerEnforced(ctx, db, time.Now())
	if err != nil || !near(e.Day, 0.035) || !near(e.Month, 0.035) || !near(e.Premium, 0.02) {
		t.Fatalf("enforced = (%+v, %v), want day and month 0.035, premium 0.02", e, err)
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
	if !near(f.Task, 2) || !near(f.Day, 19.1) || !near(f.Month, 49.1) || !near(f.Project, 0.1) {
		t.Fatalf("a's headroom = %+v, want task 2, day 19.10, month 49.10, project 0.10", f)
	}
	if got, bound := HeadroomBound(f.Task, f.Day, f.Month, f.Project); !near(got, 0.1) || bound != RefusedProjectCap {
		t.Fatalf("a's free headroom = (%g, %s), want 0.10 bound by the project's cap", got, bound)
	}
	var r *Refusal
	if _, err := TakeHold(ctx, db, tool); !errors.As(err, &r) || r.Decision != RefusedProjectCap ||
		r.Reason != "web_scrape: worst case 0.20 EUR exceeds the free headroom 0.10 EUR by 0.10 EUR (task 2.00, day 19.10, month 49.10, project 0.10); "+
			"the tool call's worst case exceeds the project's remaining month headroom (budget.month_eur in projects/p.yaml), and only a human resets its count in the console's status panel or raises it by a commit" {
		t.Fatalf("a's call with a worst case of 0.20 = %v, want refused_project_cap", err)
	}
	other := tool
	other.TaskID, other.Project, other.ProjectEUR = "b", "", inf
	h, err := TakeHold(ctx, db, other)
	if err != nil || !near(h.EUR, 0.2) {
		t.Fatalf("b's call = (%+v, %v), want 0.20 held", h, err)
	}
	// o's hold counts against p/p's cap, b's (q/q) only against the day and
	// month: 1000 input tokens and 1024 output tokens hold 0.005096
	model := HoldRequest{TaskID: "o", Kind: KindModel, Ref: "m", Bytes: 1000, Price: testPrice, WantOut: 1024, DayEUR: 20, MonthEUR: 50, Project: "p", ProjectEUR: 1}
	h, err = TakeHold(ctx, db, model)
	if err != nil || h.MaxOut != 1024 || !near(h.EUR, 0.005096) {
		t.Fatalf("o's hold = (%+v, %v), want 1024 tokens, 0.005096 EUR", h, err)
	}
	if f := free(tool); !near(f.Project, 0.094904) || !near(f.Day, 18.894904) || !near(f.Month, 48.894904) {
		t.Fatalf("a's headroom after both holds = %+v, want project 0.094904, day 18.894904, month 48.894904", f)
	}
	// a cap of 0.90 is spent: a's model call is refused
	model.TaskID, model.ProjectEUR = "a", 0.9
	if _, err := TakeHold(ctx, db, model); !errors.As(err, &r) || r.Decision != RefusedProjectCap ||
		!strings.HasSuffix(r.Reason, "; the project cap budget.month_eur in projects/p.yaml is spent, and only a human resets its count in the console's status panel or raises it by a commit") {
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
	if f, err := FreeHeadroom(ctx, tx, req); err != nil || !math.IsInf(f.Project, 1) || !near(f.Task, 2) {
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
	if err != nil || h.MaxOut != 2500 {
		t.Fatalf("hold at 2026-10-01 05:00 UTC = (%+v, %v), want 2500 tokens, the month's 0.01", h, err)
	}
	// the day: a cap of 1 leaves 0.96 of 2026-10-01, and the month-bound
	// hold's 0.01 leaves 0.95 free, 237,500 tokens at 4e-6; 2026-09-30's 30
	// is not today
	h, err = TakeHold(ctx, db, HoldRequest{TaskID: "z", Kind: KindModel, Ref: "m", Price: testPrice, DayEUR: 1, MonthEUR: 50, ProjectEUR: inf})
	if err != nil || h.MaxOut != 237500 || !near(h.EUR, 0.95) {
		t.Fatalf("day-bound hold = (%+v, %v), want 237500 tokens, the day's free 0.95", h, err)
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
			req := HoldRequest{TaskID: id, Kind: kind, Ref: "small", Bytes: 10, Price: testPrice, DayEUR: 20, MonthEUR: 50, ProjectEUR: inf,
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

// A finished plan's own review (its criteria card, the delta round) takes a
// hold on a done task and reopens its final settlement, which then books the
// call; no other purpose, the brief included, no sandbox call and no other
// finished status does, and a refused hold leaves the settlement as it was.
func TestHoldsForTheCallsOnADoneTask(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, settled_at) VALUES
		('done', 1, 0, 'done', now()), ('done2', 1, 0, 'done', now()), ('closed', 1, 0, 'closed', now()), ('broke', 0, 0, 'done', now())`); err != nil {
		t.Fatal(err)
	}
	take := func(task, ref, session string) error {
		_, err := TakeHold(ctx, db, HoldRequest{TaskID: task, Kind: KindModel, Ref: ref, Session: session, Bytes: 10, Price: testPrice,
			DayEUR: 20, MonthEUR: 50, ProjectEUR: inf})
		return err
	}
	settled := func(task string) bool {
		var s bool
		if err := db.QueryRowContext(ctx, `SELECT settled_at IS NOT NULL FROM tasks WHERE id = $1`, task).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, c := range []struct{ task, ref, session string }{
		{"done", "supervisor/small", ""},
		{"done", "brief/small", ""},
		{"done", "review/small", "s-1"},
		{"closed", "brief/small", ""},
		{"broke", "brief/small", ""},
	} {
		var r *Refusal
		if err := take(c.task, c.ref, c.session); !errors.As(err, &r) {
			t.Errorf("%s %s %q: err = %v, want a refusal", c.task, c.ref, c.session, err)
		}
		if !settled(c.task) {
			t.Errorf("%s %s %q: a refused hold reopened the settlement", c.task, c.ref, c.session)
		}
	}
	if err := take("done2", "review/small", ""); err != nil {
		t.Fatalf("review on a done task: %v", err)
	}
	if settled("done2") {
		t.Error("the review's hold left the final settlement closed")
	}
}

// A call cut by a restart never releases its hold: the sweep books it and
// writes the call's audit row, decision swept, so every booked call has
// one (#276's security-audit call, cut by the roll of 0de8a61 at 23:18:15Z,
// had none). Kind and alias come from the hold's ref: purpose/alias for a
// factoryd call, the alias for an agent's, the tool's name for a tool call;
// the author is the task's issue author (roadmap 41). Each booked row
// keeps its hold's session, the agent's model and tool calls theirs,
// factoryd's none, and its start.
func TestSweepWritesAnAuditRowPerHold(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, status, author) VALUES ('s', 5, 'failed', 'alice')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO holds (task_id, kind, ref, session, eur, expires_at) VALUES
		('s', 'model', 'audit/big', '', 0.146422, now() - interval '1 second'),
		('s', 'model', 'small', 's-1', 0.004, now() - interval '1 second'),
		('s', 'tool', 'web_scrape', 's-1', 0.02, now() - interval '1 second'),
		('s', 'model', 'small', 's-1', 0.5, now() + interval '1 hour'),
		('s', 'resource', 'cx23', '', 0.0096, now() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	if n, err := SweepHolds(ctx, db); err != nil || n != 3 {
		t.Fatalf("sweep = (%d, %v), want 3: the open hold and the resource reservation stay", n, err)
	}
	var got []string
	rows, err := db.QueryContext(ctx, `SELECT kind || ' ' || decision || ' ' || model_alias || ' ' || tool || ' ' || spend_eur::text || ' ' || hold_eur::text || ' ' || (reason <> '')::text
		|| ' ' || author FROM audit WHERE task_id = 's' ORDER BY spend_eur`)
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
		"agent swept small  0.004 0.004 true alice",
		"agent swept  web_scrape 0.02 0.02 true alice",
		"audit swept big  0.146422 0.146422 true alice",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit rows = %q, want %q", got, want)
	}
	var booked string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(ref || '@' || session || '@' || (held_at IS NOT NULL)::text, ' ' ORDER BY eur) FROM ledger WHERE task_id = 's'`).Scan(&booked); err != nil ||
		booked != "small@s-1@true web_scrape@s-1@true audit/big@@true" {
		t.Fatalf("ledger = (%q, %v), want each swept hold's session and start", booked, err)
	}
	var spent float64
	if err := db.QueryRowContext(ctx, `SELECT spent_eur::float8 FROM tasks WHERE id = 's'`).Scan(&spent); err != nil || !near(spent, 0.170422) {
		t.Fatalf("spent = %v (%v), want 0.170422, the three swept holds", spent, err)
	}
}

// A cap is hit when the gateway refused a call at it in the cap's current
// UTC period. Hand-derived: yesterday's day refusal is outside today, so
// DayHit is false and then true once today's row lands; last month's month
// refusal never counts, this month's does; the premium cap has no row.
func TestLedgerEnforcedHits(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	hits := func() Enforced {
		t.Helper()
		e, err := LedgerEnforced(ctx, db, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	insert := func(decision, ts string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO audit (ts, task_id, decision, reason) VALUES (`+ts+`, 't', $1, 'x')`, decision); err != nil {
			t.Fatal(err)
		}
	}
	today := `date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`
	insert(RefusedDayCap, today+` - interval '1 second'`)
	insert(RefusedMonthCap, `date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '1 second'`)
	if e := hits(); e.DayHit || e.MonthHit || e.PremiumHit {
		t.Fatalf("refusals before the period: %+v, want no hit", e)
	}
	insert(RefusedDayCap, today)
	if e := hits(); !e.DayHit || e.MonthHit || e.PremiumHit {
		t.Fatalf("a day refusal at 00:00 UTC today: %+v, want DayHit alone", e)
	}
	insert(RefusedMonthCap, today)
	insert(RefusedPremiumCap, today)
	if e := hits(); !e.DayHit || !e.MonthHit || !e.PremiumHit {
		t.Fatalf("all three refused this period: %+v, want all hit", e)
	}
}

// clock sets the headroom's clock to at, a UTC "2006-01-02 15:04", for the
// rest of the test.
func clock(t *testing.T, at string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02 15:04", at)
	if err != nil {
		t.Fatal(err)
	}
	was := now
	now = func() time.Time { return tm }
	t.Cleanup(func() { now = was })
	return tm
}

// book writes a model row of eur for task, booked at at (UTC) on its UTC
// day.
func book(t *testing.T, db *sql.DB, task, at, eur string) { bookOn(t, db, task, "x", at, eur) }

// bookOn is book of a call on ref.
func bookOn(t *testing.T, db *sql.DB, task, ref, at, eur string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO ledger (task_id, day, kind, ref, eur, created_at)
		VALUES ($1, ($2::timestamptz AT TIME ZONE 'UTC')::date, 'model', $3, $4::numeric, $2::timestamptz)`, task, at+"+00", ref, eur); err != nil {
		t.Fatal(err)
	}
}

// resetCap is sebastian's press on cap's count as it stands at the
// headroom's clock: it saw the cap's newest reset of the period.
func resetCap(t *testing.T, db *sql.DB, cap, repository string) CapReset {
	t.Helper()
	ctx := context.Background()
	var seen int64
	if n, err := NewestCapReset(ctx, db, cap, repository, now()); err != nil {
		t.Fatal(err)
	} else if n != nil {
		seen = n.ID
	}
	r, err := ResetCap(ctx, db, cap, repository, "sebastian", seen)
	if err != nil {
		t.Fatalf("reset %s %q: %v", cap, repository, err)
	}
	return r
}

func enforcedNow(t *testing.T, db *sql.DB) Enforced {
	t.Helper()
	e, err := LedgerEnforced(context.Background(), db, now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func readFree(t *testing.T, db *sql.DB, r HoldRequest) Free {
	t.Helper()
	_, f, err := ReadFree(context.Background(), db, r)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func sameReset(a, b CapReset) bool {
	return a.ID == b.ID && a.Cap == b.Cap && a.Repository == b.Repository && a.At.Equal(b.At) && a.Login == b.Login && near(a.BeforeEUR, b.BeforeEUR)
}

func runningTasks(t *testing.T, db *sql.DB, repositories map[string]string) {
	t.Helper()
	for id, repository := range repositories {
		if _, err := db.Exec(`INSERT INTO tasks (id, budget_eur, status, repository) VALUES ($1, 5, 'running', $2)`, id, repository); err != nil {
			t.Fatal(err)
		}
	}
}

// A day reset restarts the day's count at the press; DayAll keeps the
// whole day. Hand-derived: 12.00 at 09:00 and 7.62 at 13:00 are the count
// the 14:02 press ends, 19.62; the 0.40 booked at 14:10 is the count after
// it, 20.02 the day's whole spend and the month's count, and a day cap of
// 100 leaves 99.60.
func TestResetRestartsTheDayCount(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": ""})
	book(t, db, "t", "2026-11-20 09:00", "12.00")
	book(t, db, "t", "2026-11-20 13:00", "7.62")
	press := clock(t, "2026-11-20 14:02")
	r := resetCap(t, db, CapDay, "")
	if r.ID == 0 || r.Cap != CapDay || r.Repository != "" || !r.At.Equal(press) || r.Login != "sebastian" || !near(r.BeforeEUR, 19.62) {
		t.Fatalf("reset = %+v, want the day's at 14:02 by sebastian, 19.62 EUR before", r)
	}
	book(t, db, "t", "2026-11-20 14:10", "0.40")
	clock(t, "2026-11-20 14:30")
	e := enforcedNow(t, db)
	if !near(e.Day, 0.40) || !near(e.DayAll, 20.02) || !near(e.Month, 20.02) || !near(e.MonthAll, 20.02) {
		t.Fatalf("enforced = %+v, want day 0.40 of 20.02, month 20.02 of 20.02", e)
	}
	if e.DayReset == nil || !sameReset(*e.DayReset, r) || e.DayReset.Login != "sebastian" || !near(e.DayReset.BeforeEUR, 19.62) || e.MonthReset != nil {
		t.Fatalf("resets = day %+v, month %+v; want the day's press %+v alone", e.DayReset, e.MonthReset, r)
	}
	if f := readFree(t, db, HoldRequest{TaskID: "t", DayEUR: 100, MonthEUR: 1000, ProjectEUR: inf}); !near(f.Day, 99.60) || !near(f.Month, 979.98) {
		t.Fatalf("free = %+v, want day 99.60, month 979.98", f)
	}
}

// A reset forgives the ledger's rows, never an outstanding hold: the hold
// is spend still to come. Hand-derived: 5.00 booked at 10:00 and a hold of
// 3.00 make the count 8.00 at the press; after it the hold alone counts,
// 3.00, and a day cap of 100 leaves 97.00.
func TestResetKeepsHolds(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": ""})
	book(t, db, "t", "2026-11-20 10:00", "5.00")
	if _, err := db.Exec(`INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('t', 'model', 'x', 3, now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	clock(t, "2026-11-20 14:02")
	if r := resetCap(t, db, CapDay, ""); !near(r.BeforeEUR, 8) {
		t.Fatalf("reset = %+v, want 8.00 EUR before, the hold included", r)
	}
	if e := enforcedNow(t, db); !near(e.Day, 3) || !near(e.DayAll, 8) {
		t.Fatalf("enforced = %+v, want day 3.00 of 8.00", e)
	}
	if f := readFree(t, db, HoldRequest{TaskID: "t", DayEUR: 100, MonthEUR: 1000, ProjectEUR: inf}); !near(f.Day, 97) {
		t.Fatalf("free = %+v, want day 97.00", f)
	}
}

// A premium reset restarts the premium count alone, which the gateway's
// premium headroom reads too. Hand-derived: an agent's 30.00 at 09:00 and
// factoryd's review 12.00 at 13:00 on Premium, 5.00 on another alias at
// 10:00 and a premium hold of 2.00: the 14:02 press ends a count of 44.00.
// The 3.00 on Premium at 14:10 and the hold are the count after it, 5.00,
// of the month's 47.00 on Premium; the month counts all 50.00 and the
// hold, 52.00, and has no reset. A premium cap of 50 leaves 45.00, a
// month cap of 1000 leaves 948.00.
func TestResetRestartsThePremiumCount(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": ""})
	bookOn(t, db, "t", Premium, "2026-11-20 09:00", "30.00")
	book(t, db, "t", "2026-11-20 10:00", "5.00")
	bookOn(t, db, "t", PurposeReview+"/"+Premium, "2026-11-20 13:00", "12.00")
	if _, err := db.Exec(`INSERT INTO holds (task_id, kind, ref, eur, expires_at) VALUES ('t', 'model', $1, 2, now() + interval '1 hour')`, Premium); err != nil {
		t.Fatal(err)
	}
	press := clock(t, "2026-11-20 14:02")
	r := resetCap(t, db, CapPremium, "")
	if r.Cap != CapPremium || r.Repository != "" || !r.At.Equal(press) || !near(r.BeforeEUR, 44) {
		t.Fatalf("reset = %+v, want the premium's at 14:02, 44.00 EUR before", r)
	}
	bookOn(t, db, "t", Premium, "2026-11-20 14:10", "3.00")
	clock(t, "2026-11-20 14:30")
	e := enforcedNow(t, db)
	if !near(e.Premium, 5) || !near(e.PremiumAll, 47) || !near(e.Month, 52) || !near(e.MonthAll, 52) {
		t.Fatalf("enforced = %+v, want premium 5.00 of 47.00, month 52.00 of 52.00", e)
	}
	if e.PremiumReset == nil || !sameReset(*e.PremiumReset, r) || e.MonthReset != nil || e.DayReset != nil {
		t.Fatalf("resets = premium %+v, month %+v, day %+v; want the premium's press %+v alone", e.PremiumReset, e.MonthReset, e.DayReset, r)
	}
	f := readFree(t, db, HoldRequest{TaskID: "t", DayEUR: 100, MonthEUR: 1000, ProjectEUR: inf, PremiumEUR: 50})
	if f.Premium == nil || !near(*f.Premium, 45) || !near(f.Month, 948) {
		t.Fatalf("free = %+v, premium %v; want premium 45.00, month 948.00", f, f.Premium)
	}
}

// A reset of an earlier period moves no count of the current one.
// Hand-derived at 2026-11-20 14:02: the day reset of 2026-11-19 23:00 and
// the month and premium resets of 2026-10-31 18:00 are of earlier periods; today's 1.50
// is the day, and with 2.00 of 2026-11-19 and 4.00 of 2026-11-03 the
// month is 7.50; 8.00 of 2026-10-31 is October's. The day refusal of
// 2026-11-19 23:30, after yesterday's reset, does not hit today.
func TestResetOfAnEarlierPeriod(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": ""})
	clock(t, "2026-10-31 18:00")
	resetCap(t, db, CapMonth, "")
	resetCap(t, db, CapPremium, "")
	clock(t, "2026-11-19 23:00")
	resetCap(t, db, CapDay, "")
	book(t, db, "t", "2026-10-31 20:00", "8.00")
	book(t, db, "t", "2026-11-03 10:00", "4.00")
	book(t, db, "t", "2026-11-19 22:30", "2.00")
	book(t, db, "t", "2026-11-20 09:00", "1.50")
	if _, err := db.Exec(`INSERT INTO audit (ts, task_id, decision, reason) VALUES ('2026-11-19 23:30+00', 't', $1, 'x')`, RefusedDayCap); err != nil {
		t.Fatal(err)
	}
	clock(t, "2026-11-20 14:02")
	e := enforcedNow(t, db)
	if !near(e.Day, 1.5) || !near(e.DayAll, 1.5) || !near(e.Month, 7.5) || !near(e.MonthAll, 7.5) || e.DayReset != nil || e.MonthReset != nil || e.PremiumReset != nil || e.DayHit {
		t.Fatalf("enforced = %+v, want day 1.50, month 7.50, no reset of this period, no hit today", e)
	}
}

// The newest reset of the period starts the count. Hand-derived: presses
// at 10:00 and 13:00 around rows of 1.00 at 09:00, 2.00 at 11:00 and 4.00
// at 13:30: the count is 4.00 of the day's 7.00, and the 13:00 press ended
// the count of 2.00 since 10:00.
func TestNewestResetCounts(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": ""})
	book(t, db, "t", "2026-11-20 09:00", "1.00")
	clock(t, "2026-11-20 10:00")
	if r := resetCap(t, db, CapDay, ""); !near(r.BeforeEUR, 1) {
		t.Fatalf("the 10:00 reset = %+v, want 1.00 EUR before", r)
	}
	book(t, db, "t", "2026-11-20 11:00", "2.00")
	second := clock(t, "2026-11-20 13:00")
	resetCap(t, db, CapDay, "")
	book(t, db, "t", "2026-11-20 13:30", "4.00")
	clock(t, "2026-11-20 14:02")
	e := enforcedNow(t, db)
	if !near(e.Day, 4) || !near(e.DayAll, 7) || e.DayReset == nil || !e.DayReset.At.Equal(second) || !near(e.DayReset.BeforeEUR, 2) {
		t.Fatalf("enforced = %+v, day reset %+v; want day 4.00 of 7.00 from the 13:00 reset, 2.00 EUR before it", e, e.DayReset)
	}
}

// A cap is hit by a refusal since its count started: a reset clears the
// day's and the premium's hit, the month's stays. Hand-derived: refusals
// at 11:00 and the day's and the premium's resets at 12:00 leave the
// month hit alone; the day's and the premium's refusals at 12:30 hit them
// again.
func TestResetMovesTheHit(t *testing.T) {
	db := testDB(t)
	refuse := func(decision, at string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO audit (ts, task_id, decision, reason) VALUES ($1::timestamptz, 't', $2, 'x')`, at+"+00", decision); err != nil {
			t.Fatal(err)
		}
	}
	refuse(RefusedDayCap, "2026-11-20 11:00")
	refuse(RefusedMonthCap, "2026-11-20 11:00")
	refuse(RefusedPremiumCap, "2026-11-20 11:00")
	clock(t, "2026-11-20 12:00")
	resetCap(t, db, CapDay, "")
	resetCap(t, db, CapPremium, "")
	if e := enforcedNow(t, db); e.DayHit || !e.MonthHit || e.PremiumHit {
		t.Fatalf("after the day's and the premium's resets: %+v, want the month hit alone", e)
	}
	refuse(RefusedDayCap, "2026-11-20 12:30")
	refuse(RefusedPremiumCap, "2026-11-20 12:30")
	clock(t, "2026-11-20 14:02")
	if e := enforcedNow(t, db); !e.DayHit || !e.MonthHit || !e.PremiumHit {
		t.Fatalf("refusals after the resets: %+v, want all three hit", e)
	}
}

// A project reset moves its repository's count alone. Hand-derived: a's
// task booked 3.00 and b's 5.00 at 09:00; the reset of a at 14:02 ends a
// count of 3.00, and a's 0.50 at 14:10 leaves a cap of 10 at 9.50 for a,
// 5.00 for b; the factory's month counts all 8.50 and has no reset.
func TestProjectResetScope(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"ta": "a", "tb": "b"})
	book(t, db, "ta", "2026-11-20 09:00", "3.00")
	book(t, db, "tb", "2026-11-20 09:00", "5.00")
	clock(t, "2026-11-20 14:02")
	if r := resetCap(t, db, CapProject, "a"); r.Repository != "a" || !near(r.BeforeEUR, 3) {
		t.Fatalf("reset = %+v, want a's, 3.00 EUR before", r)
	}
	book(t, db, "ta", "2026-11-20 14:10", "0.50")
	capped := HoldRequest{TaskID: "ta", DayEUR: 100, MonthEUR: 100, Project: "a", ProjectEUR: 10}
	if f := readFree(t, db, capped); !near(f.Project, 9.5) || !near(f.Month, 91.5) || !near(f.Day, 91.5) {
		t.Fatalf("a's free = %+v, want project 9.50, month 91.50, day 91.50", f)
	}
	capped.TaskID, capped.Project = "tb", "b"
	if f := readFree(t, db, capped); !near(f.Project, 5) || !near(f.Month, 91.5) {
		t.Fatalf("b's free = %+v, want project 5.00, month 91.50", f)
	}
	if e := enforcedNow(t, db); !near(e.Month, 8.5) || e.MonthReset != nil || e.DayReset != nil {
		t.Fatalf("enforced = %+v, want the month's 8.50 and no day or month reset", e)
	}
}

// Only the day's, the month's, the premium's and a project's cap reset, a
// project's with its repository and the others without one; a refused
// reset writes no row.
func TestResetCapRefuses(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, tc := range []struct{ cap, repository string }{{"chat", ""}, {CapProject, ""}, {CapPool, ""}, {CapDay, "a"}, {CapMonth, "a"}, {CapPremium, "a"}} {
		if r, err := ResetCap(ctx, db, tc.cap, tc.repository, "sebastian", 0); err == nil {
			t.Errorf("reset %s %q = %+v, want it refused", tc.cap, tc.repository, r)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM cap_resets`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cap_resets holds %d rows (%v), want none", n, err)
	}
}

// A press names the newest reset of its cap it saw: a second press on the
// same view is refused with the reset that came first and writes no row,
// and a press that saw it passes. Another cap's reset, another
// repository's and one of an earlier period stay out of the check.
func TestResetRefusesAPressOnAnOlderCount(t *testing.T) {
	db, ctx := testDB(t), context.Background()
	rows := func(cap string) (n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT count(*) FROM cap_resets WHERE cap = $1`, cap).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	clock(t, "2026-11-20 14:02")
	first, err := ResetCap(ctx, db, CapDay, "", "sebastian", 0)
	if err != nil {
		t.Fatal(err)
	}
	clock(t, "2026-11-20 14:03")
	var stale *StaleResetError
	if _, err := ResetCap(ctx, db, CapDay, "", "alice", 0); !errors.As(err, &stale) || !sameReset(stale.Newest, first) || rows(CapDay) != 1 {
		t.Fatalf("a second press on the same view = %v, %d day rows; want refused with %+v and one row", err, rows(CapDay), first)
	}
	for _, other := range []struct{ cap, repository string }{{CapMonth, ""}, {CapPremium, ""}, {CapProject, "a"}, {CapProject, "b"}} {
		if _, err := ResetCap(ctx, db, other.cap, other.repository, "alice", 0); err != nil {
			t.Fatalf("the first press on %s %q = %v, want it to pass", other.cap, other.repository, err)
		}
	}
	second, err := ResetCap(ctx, db, CapDay, "", "alice", first.ID)
	if err != nil || second.ID == first.ID || second.Login != "alice" || rows(CapDay) != 2 {
		t.Fatalf("a press that saw %d = (%+v, %v), %d day rows; want alice's new reset, two rows", first.ID, second, err, rows(CapDay))
	}
	clock(t, "2026-11-21 00:05")
	if _, err := ResetCap(ctx, db, CapDay, "", "alice", 0); err != nil {
		t.Fatalf("the first press of a new day = %v, want it to pass", err)
	}
}

// CapResets are the resets with at in [from, to), newest first.
func TestCapResets(t *testing.T) {
	db := testDB(t)
	clock(t, "2026-11-18 10:00")
	day := resetCap(t, db, CapDay, "")
	clock(t, "2026-11-19 10:00")
	project := resetCap(t, db, CapProject, "a")
	to := clock(t, "2026-11-20 10:00")
	resetCap(t, db, CapMonth, "")
	got, err := CapResets(context.Background(), db, day.At, to)
	if err != nil || len(got) != 2 || !sameReset(got[0], project) || !sameReset(got[1], day) {
		t.Fatalf("resets = (%+v, %v), want a's of 11-19 and the day's of 11-18", got, err)
	}
}

// NewestCapReset is the newest reset of the cap, and of a project's
// repository, in the cap's UTC period: yesterday's day reset and last
// month's month and premium resets are none, the newer of two counts, and
// another repository's stays out. A cap without a reset is an error.
func TestNewestCapReset(t *testing.T) {
	db, ctx := testDB(t), context.Background()
	clock(t, "2026-10-31 18:00")
	resetCap(t, db, CapMonth, "")
	resetCap(t, db, CapPremium, "")
	clock(t, "2026-11-19 23:00")
	resetCap(t, db, CapDay, "")
	clock(t, "2026-11-20 09:00")
	resetCap(t, db, CapProject, "a")
	premium := resetCap(t, db, CapPremium, "")
	clock(t, "2026-11-20 14:02")
	newer := resetCap(t, db, CapProject, "a")
	other := resetCap(t, db, CapProject, "b")
	at := clock(t, "2026-11-20 14:10")
	for _, tc := range []struct {
		cap, repository string
		want            *CapReset
	}{
		{CapDay, "", nil}, {CapMonth, "", nil}, {CapPremium, "", &premium}, {CapProject, "a", &newer}, {CapProject, "b", &other}, {CapProject, "c", nil},
	} {
		got, err := NewestCapReset(ctx, db, tc.cap, tc.repository, at)
		if err != nil || (got == nil) != (tc.want == nil) || got != nil && !sameReset(*got, *tc.want) {
			t.Errorf("%s %q = (%+v, %v), want %+v", tc.cap, tc.repository, got, err, tc.want)
		}
	}
	if _, err := NewestCapReset(ctx, db, "chat", "", at); err == nil {
		t.Error("the chat's newest reset is no error")
	}
}

// settle books task's final settlement of eur at at on day, its finish
// day, as factoryd's final settlement does; finished stamps its finish.
func settle(t *testing.T, db *sql.DB, task, finished, at, eur string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE tasks SET finished_at = $2::timestamptz WHERE id = $1`, task, finished+"+00"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ledger (task_id, day, kind, ref, eur, created_at)
		VALUES ($1, ($2::timestamptz AT TIME ZONE 'UTC')::date, 'settlement', 'litellm-final', $3::numeric, $4::timestamptz)`,
		task, finished+"+00", eur, at+"+00"); err != nil {
		t.Fatal(err)
	}
}

// A reset forgives the rows of a task that finished before it, so the
// final settlement that corrects them, booked after the press, stays out
// of the new count; a count from the period's start keeps it. Hand-
// derived: t booked 12.00 at 09:00 and finished at 12:00; the 14:02 press
// ends a count of 12.00; u's 0.40 at 14:10 is the day's count, and t's
// -1.00 settled at 14:30 leaves the month 11.40.
func TestResetForgivesAnEarlierTasksSettlement(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": "", "u": ""})
	book(t, db, "t", "2026-11-20 09:00", "12.00")
	clock(t, "2026-11-20 14:02")
	resetCap(t, db, CapDay, "")
	book(t, db, "u", "2026-11-20 14:10", "0.40")
	settle(t, db, "t", "2026-11-20 12:00", "2026-11-20 14:30", "-1.00")
	clock(t, "2026-11-20 15:00")
	if e := enforcedNow(t, db); !near(e.Day, 0.4) || !near(e.DayAll, 11.4) || !near(e.Month, 11.4) {
		t.Fatalf("enforced = %+v, want day 0.40, the day's and the month's whole 11.40", e)
	}
}

// A negative settlement of a task that booked before the press corrects
// rows the press forgave, so it stays out of the new count. Hand-derived:
// t booked 2.00 at 13:00, the 14:02 press ends a count of 2.00, t books
// 1.50 at 14:10, finishes at 14:20 and settles -1.00 at 14:40: the day's
// count is 1.50 of the day's 2.50.
func TestResetLeavesOutANegativeSettlementAcrossIt(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": ""})
	book(t, db, "t", "2026-11-20 13:00", "2.00")
	clock(t, "2026-11-20 14:02")
	resetCap(t, db, CapDay, "")
	book(t, db, "t", "2026-11-20 14:10", "1.50")
	settle(t, db, "t", "2026-11-20 14:20", "2026-11-20 14:40", "-1.00")
	clock(t, "2026-11-20 15:00")
	if e := enforcedNow(t, db); !near(e.Day, 1.5) || !near(e.DayAll, 2.5) {
		t.Fatalf("enforced = %+v, want day 1.50 of 2.50", e)
	}
}

// A task that finishes after the press settles into the new count, but
// for a negative settlement of a task that booked before it; the day's,
// the month's and the project's counts agree. Hand-derived, all in a's
// tasks, the three caps reset at 14:02: t books 2.00 at 13:00 and 1.50 at
// 14:10 and settles -1.00, out; u books 0.80 at 14:15 alone and settles
// -0.30, in; v books 1.00 at 12:00 and 0.50 at 14:12 and settles +0.20,
// in. The count is 1.50 + 0.80 - 0.30 + 0.50 + 0.20 = 2.70 of the whole
// 4.70, and a project cap of 10 leaves 7.30.
func TestResetCountsTheSettlementsOfItsCount(t *testing.T) {
	db := testDB(t)
	runningTasks(t, db, map[string]string{"t": "a", "u": "a", "v": "a"})
	book(t, db, "v", "2026-11-20 12:00", "1.00")
	book(t, db, "t", "2026-11-20 13:00", "2.00")
	clock(t, "2026-11-20 14:02")
	for _, cap := range []struct{ cap, repository string }{{CapDay, ""}, {CapMonth, ""}, {CapProject, "a"}} {
		resetCap(t, db, cap.cap, cap.repository)
	}
	book(t, db, "t", "2026-11-20 14:10", "1.50")
	book(t, db, "v", "2026-11-20 14:12", "0.50")
	book(t, db, "u", "2026-11-20 14:15", "0.80")
	settle(t, db, "t", "2026-11-20 14:20", "2026-11-20 14:40", "-1.00")
	settle(t, db, "u", "2026-11-20 14:25", "2026-11-20 14:45", "-0.30")
	settle(t, db, "v", "2026-11-20 14:30", "2026-11-20 14:50", "0.20")
	clock(t, "2026-11-20 15:00")
	if e := enforcedNow(t, db); !near(e.Day, 2.7) || !near(e.Month, 2.7) || !near(e.DayAll, 4.7) || !near(e.MonthAll, 4.7) {
		t.Fatalf("enforced = %+v, want day and month 2.70 of 4.70", e)
	}
	if f := readFree(t, db, HoldRequest{TaskID: "u", DayEUR: 100, MonthEUR: 100, Project: "a", ProjectEUR: 10}); !near(f.Project, 7.3) {
		t.Fatalf("free = %+v, want project 7.30", f)
	}
}
