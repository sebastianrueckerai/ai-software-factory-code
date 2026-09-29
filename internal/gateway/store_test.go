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
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, audit, ledger, holds, policy_activations, policy_versions CASCADE`); err != nil {
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
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, required_capabilities, repository) VALUES ('t1', 10, 2, 'running', '["coder"]', 'sf/app')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, ok, err := s.Task(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("Task = (%v, %v), want (true, nil)", ok, err)
	}
	want := Task{ID: "t1", BudgetEUR: 10, SpentEUR: 2, Status: "running", Classes: []string{"coder"}, Repository: "sf/app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("task = %+v, want %+v", got, want)
	}

	// a hold and its release book the task and the ledger together
	h, err := s.TakeHold(ctx, policy.HoldRequest{TaskID: "t1", Kind: policy.KindModel, Ref: "coding-cheap",
		InTokens: 1000, Price: policy.Price{In: 1e-6, Out: 4e-6}, WantOut: 1024, DayEUR: 20})
	if err != nil || h.MaxOut != 1024 || h.Headroom != 8 {
		t.Fatalf("TakeHold = (%+v, %v), want 1024 tokens under headroom 8", h, err)
	}
	cost := 0.5
	// the hold is 1000 × 1e-6 + 1024 × 4e-6 = 0.005096 EUR: 0.5 is over it
	if b, err := s.ReleaseHold(ctx, h, &cost); err != nil || b.Spent != 2.5 ||
		b.OverHold != "COST OVER HOLD: model coding-cheap cost 0.500000 EUR against a hold of 0.005096 EUR; booked in full" {
		t.Fatalf("ReleaseHold = (%+v, %v), want spent 2.5, flagged", b, err)
	}
	var ledger string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(task_id || ' ' || kind || ' ' || ref || ' ' || eur::text, ';') FROM ledger`).Scan(&ledger); err != nil || ledger != "t1 model coding-cheap 0.5" {
		t.Fatalf("ledger = (%q, %v)", ledger, err)
	}

	if _, ok, err := s.CurrentPolicy(ctx); err != nil || ok {
		t.Fatalf("CurrentPolicy before any load = (%v, %v), want (false, nil)", ok, err)
	}
	doc := testPolicy(0, 20).Document
	v, _, err := policy.Record(ctx, s.db, "abc", policy.Tree{Document: doc})
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
		OutputLimit: &h.MaxOut, HoldEUR: &h.EUR, OverHold: "COST OVER HOLD: x",
	}); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	if err := s.InsertAudit(ctx, AuditRow{
		TaskID: "t1", Decision: "refused_budget", Reason: "budget_exhausted", Tool: "web_scrape",
	}); err != nil {
		t.Fatalf("InsertAudit (no spend): %v", err)
	}

	if err := s.InsertAudit(ctx, AuditRow{TaskID: "t2", Kind: policy.PurposeReview, Decision: "allowed", ModelAlias: "review-frontier"}); err != nil {
		t.Fatalf("InsertAudit (review): %v", err)
	}
	var kinds string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(task_id || '=' || kind, ';' ORDER BY id) FROM audit`).Scan(&kinds); err != nil ||
		kinds != "t1=agent;t1=agent;t2=review" {
		t.Fatalf("audit kinds = (%q, %v), want an agent's two and the review", kinds, err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE task_id = 't1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
	var flags string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(decision || '=' || over_hold, ';' ORDER BY id) FROM audit WHERE task_id = 't1'`).Scan(&flags); err != nil ||
		flags != "allowed=COST OVER HOLD: x;refused_budget=" {
		t.Fatalf("over_hold = (%q, %v)", flags, err)
	}
	var nullSpend any
	if err := s.db.QueryRowContext(ctx,
		`SELECT spend_eur FROM audit WHERE decision = 'refused_budget'`).Scan(&nullSpend); err != nil {
		t.Fatal(err)
	}
	if nullSpend != nil {
		t.Fatalf("spend_eur of refusal = %v, want NULL", nullSpend)
	}
	var tools []string
	rows, err := s.db.QueryContext(ctx, `SELECT tool FROM audit WHERE task_id = 't1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var tool string
		rows.Scan(&tool)
		tools = append(tools, tool)
	}
	if !reflect.DeepEqual(tools, []string{"", "web_scrape"}) {
		t.Fatalf("audit tools = %q, want a model call's empty tool, then web_scrape", tools)
	}
	var auditVersion int64
	var limit int
	var held float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT policy_version_id, output_limit, hold_eur::float8 FROM audit WHERE decision = 'allowed'`).Scan(&auditVersion, &limit, &held); err != nil ||
		auditVersion != v.ID || limit != 1024 || held != h.EUR {
		t.Fatalf("audit = version %d, limit %d, hold %v (%v); want %d, 1024, %v", auditVersion, limit, held, err, v.ID, h.EUR)
	}
}
