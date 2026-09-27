package gateway

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Requires a disposable Postgres: GATEWAY_TEST_DATABASE_URL must point at a
// database this test may wipe. Skipped otherwise; CI stays hermetic.
func TestPGStore(t *testing.T) {
	dsn := os.Getenv("GATEWAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GATEWAY_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, audit, ledger, policy_current, policy_versions`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Booting twice must be safe: migrations are idempotent.
	s2, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("second OpenStore: %v", err)
	}
	defer s2.db.Close()

	if _, ok, err := s.Task(ctx, "t1"); err != nil || ok {
		t.Fatalf("unknown task = (%v, %v), want (false, nil)", ok, err)
	}

	// factoryd owns task rows; seed one the way it would.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, required_capabilities) VALUES ('t1', 10, 2, 'running', '["coder"]')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("Task = (%v, %v), want (true, nil)", ok, err)
	}
	want := Task{ID: "t1", BudgetEUR: 10, SpentEUR: 2, Status: "running", Classes: []string{"coder"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task = %+v, want %+v", got, want)
	}

	if total, err := s.DayTotal(ctx); err != nil || total != 0 {
		t.Fatalf("DayTotal before spend = (%v, %v), want (0, nil)", total, err)
	}
	if total, err := s.AddSpent(ctx, "t1", "coding-cheap", 0.5); err != nil || total != 2.5 {
		t.Fatalf("AddSpent = (%v, %v), want (2.5, nil)", total, err)
	}
	if total, err := s.AddSpent(ctx, "t1", "coding-frontier", 0.25); err != nil || total != 2.75 {
		t.Fatalf("second AddSpent = (%v, %v), want (2.75, nil)", total, err)
	}
	got, _, _ = s.Task(ctx, "t1")
	if got.SpentEUR != 2.75 {
		t.Fatalf("spent after adds = %v, want 2.75", got.SpentEUR)
	}
	if _, err := s.AddSpent(ctx, "missing", "coding-cheap", 1); err == nil {
		t.Fatal("AddSpent on a missing task succeeded, want an error")
	}
	// the day total is the ledger's: the task's opening spend of 2 was
	// never booked, the missing task's call booked nothing
	if total, err := s.DayTotal(ctx); err != nil || total != 0.75 {
		t.Fatalf("DayTotal = (%v, %v), want (0.75, nil)", total, err)
	}
	var ledger []string
	rows, err := s.db.QueryContext(ctx, `SELECT task_id || ' ' || kind || ' ' || ref || ' ' || eur::text FROM ledger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		ledger = append(ledger, l)
	}
	if want := []string{"t1 model coding-cheap 0.5", "t1 model coding-frontier 0.25"}; !reflect.DeepEqual(ledger, want) {
		t.Fatalf("ledger = %q, want %q", ledger, want)
	}

	if _, ok, err := s.CurrentPolicy(ctx); err != nil || ok {
		t.Fatalf("CurrentPolicy before any load = (%v, %v), want (false, nil)", ok, err)
	}
	doc := testPolicy(0, 20).Document
	v, _, err := policy.Record(ctx, s.db, "abc", doc)
	if err != nil {
		t.Fatal(err)
	}
	cur, ok, err := s.CurrentPolicy(ctx)
	if err != nil || !ok || cur.ID != v.ID || !reflect.DeepEqual(cur.Document, doc) {
		t.Fatalf("CurrentPolicy = (%+v, %v, %v), want version %d", cur, ok, err, v.ID)
	}

	spend := 2.75
	if err := s.InsertAudit(ctx, AuditRow{
		TaskID: "t1", Decision: "allowed", Reason: "", ModelAlias: "coding-cheap", SpendEUR: &spend, PolicyVersion: &v.ID,
	}); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	if err := s.InsertAudit(ctx, AuditRow{
		TaskID: "t1", Decision: "refused_budget", Reason: "budget_exhausted",
	}); err != nil {
		t.Fatalf("InsertAudit (no spend): %v", err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE task_id = 't1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
	var nullSpend any
	if err := s.db.QueryRowContext(ctx,
		`SELECT spend_eur FROM audit WHERE decision = 'refused_budget'`).Scan(&nullSpend); err != nil {
		t.Fatal(err)
	}
	if nullSpend != nil {
		t.Fatalf("spend_eur of refusal = %v, want NULL", nullSpend)
	}
	var auditVersion int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT policy_version_id FROM audit WHERE decision = 'allowed'`).Scan(&auditVersion); err != nil || auditVersion != v.ID {
		t.Fatalf("audit policy_version_id = (%d, %v), want %d", auditVersion, err, v.ID)
	}
}
