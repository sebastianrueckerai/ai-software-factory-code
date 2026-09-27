package policy

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Requires a disposable Postgres: FACTORYD_TEST_DATABASE_URL must point at a
// database this test may wipe. Skipped otherwise; CI stays hermetic.
func TestStore(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for range 2 { // idempotent
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE policy_current, policy_versions, ledger`); err != nil {
		t.Fatal(err)
	}

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
	if !reflect.DeepEqual(v1.Document, d1) || v1.SHA256 != "sha1" || v1.LoadedAt.IsZero() {
		t.Fatalf("recorded version = %+v", v1)
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
	if err != nil || inserted || back.ID != v1.ID {
		t.Fatalf("Record revert = (%+v, %v, %v), want version %d, not inserted", back, inserted, err, v1.ID)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM policy_versions`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("policy_versions rows = %d (%v), want 2", n, err)
	}

	if total, err := DayTotal(ctx, db); err != nil || total != 0 {
		t.Fatalf("DayTotal empty = (%v, %v)", total, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES
		('t1', (now() AT TIME ZONE 'UTC')::date, 'model', 'coding-cheap', 0.5),
		('t2', (now() AT TIME ZONE 'UTC')::date, 'model', 'coding-cheap', 0.25),
		('t1', (now() AT TIME ZONE 'UTC')::date, 'settlement', 'litellm', 0.125),
		('t1', '2026-01-01', 'model', 'coding-cheap', 3)`); err != nil {
		t.Fatal(err)
	}
	if total, err := DayTotal(ctx, db); err != nil || total != 0.875 {
		t.Fatalf("DayTotal = (%v, %v), want 0.875", total, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ledger (task_id, kind, eur) VALUES ('t1', 'bogus', 1)`); err == nil {
		t.Fatal("ledger accepted an unknown kind")
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
