package gateway

import (
	"context"
	"os"
	"testing"
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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, audit`); err != nil {
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
		`INSERT INTO tasks (id, budget_eur, spent_eur, status) VALUES ('t1', 10, 2, 'running')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("Task = (%v, %v), want (true, nil)", ok, err)
	}
	want := Task{ID: "t1", BudgetEUR: 10, SpentEUR: 2, Status: "running"}
	if got != want {
		t.Fatalf("task = %+v, want %+v", got, want)
	}

	if total, err := s.AddSpent(ctx, "t1", 0.5); err != nil || total != 2.5 {
		t.Fatalf("AddSpent = (%v, %v), want (2.5, nil)", total, err)
	}
	if total, err := s.AddSpent(ctx, "t1", 0.25); err != nil || total != 2.75 {
		t.Fatalf("second AddSpent = (%v, %v), want (2.75, nil)", total, err)
	}
	got, _, _ = s.Task(ctx, "t1")
	if got.SpentEUR != 2.75 {
		t.Fatalf("spent after adds = %v, want 2.75", got.SpentEUR)
	}
	if _, err := s.AddSpent(ctx, "missing", 1); err == nil {
		t.Fatal("AddSpent on a missing task succeeded, want an error")
	}

	spend := 2.75
	if err := s.InsertAudit(ctx, AuditRow{
		TaskID: "t1", Decision: "allowed", Reason: "", ModelAlias: "coding-cheap", SpendEUR: &spend,
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
}
