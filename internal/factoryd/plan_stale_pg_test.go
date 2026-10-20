package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The plan staleness column round-trips through Postgres (piece 10): the
// migration's own backfill statement derives each card's revision from
// the newest plan revision created before its first session, the stamp
// lands only while the card never started, and the brief update lands.
func TestPGStorePlanStaleness(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, plan_revisions, sessions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seed := Task{Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Lane: "plan"}
	parent := seed
	parent.ID, parent.Status, parent.IssueNumber = "t-plan", "done", 20
	if err := s.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	mid, last, fresh := seed, seed, seed
	mid.ID, mid.Status, mid.IssueNumber, mid.Source, mid.Parent, mid.PlanKey = "t-mid", "backlog", 21, SourcePlan, "t-plan", "foundation"
	last.ID, last.Status, last.IssueNumber, last.Source, last.Parent, last.PlanKey = "t-last", "backlog", 22, SourcePlan, "t-plan", "links-api"
	fresh.ID, fresh.Status, fresh.IssueNumber, fresh.Source, fresh.Parent, fresh.PlanKey = "t-fresh", "backlog", 23, SourcePlan, "t-plan", "redirects"
	for _, task := range []Task{mid, last, fresh} {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"one"}`), "s1"); err != nil {
		t.Fatal(err)
	}
	// t-mid's first session runs against revision 1, before the second
	// revision lands; t-last's runs after it
	if err := s.InsertSession(ctx, Session{ID: "s-mid", TaskID: "t-mid", Model: "factory/big"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "s2"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSession(ctx, Session{ID: "s-last", TaskID: "t-last", Model: "factory/big"}); err != nil {
		t.Fatal(err)
	}

	// the backfill, run again on this data, is the same derivation the
	// migration itself made
	file, err := os.ReadFile("../schema/migrations/factoryd/20261020100000_plan_staleness.sql")
	if err != nil {
		t.Fatal(err)
	}
	from := bytes.Index(file, []byte("UPDATE tasks"))
	to := from + bytes.Index(file[from:], []byte(";\n")) + 1
	if _, err := s.db.ExecContext(ctx, string(file[from:to])); err != nil {
		t.Fatalf("the backfill: %v", err)
	}
	if got, _, _ := s.Task(ctx, "t-mid"); got.PlanRevision != 1 {
		t.Fatalf("the card that ran between the revisions = %d, want 1", got.PlanRevision)
	}
	if got, _, _ := s.Task(ctx, "t-last"); got.PlanRevision != 2 {
		t.Fatalf("the card that ran after revision 2 = %d, want 2", got.PlanRevision)
	}
	// a card with no session stays unstamped: its first session stamps it
	if got, _, _ := s.Task(ctx, "t-fresh"); got.PlanRevision != 0 {
		t.Fatalf("a card that never ran = %d, want 0", got.PlanRevision)
	}

	// the stamp lands only while the card never started
	if err := s.SetTaskPlanRevision(ctx, "t-fresh", 3); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-fresh"); got.PlanRevision != 3 {
		t.Fatalf("the stamp: %d, want 3", got.PlanRevision)
	}
	if err := s.SetTaskPlanRevision(ctx, "t-fresh", 4); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-fresh"); got.PlanRevision != 3 {
		t.Fatalf("a second stamp moved the card to %d, want it pinned at 3", got.PlanRevision)
	}
	if err := s.SetTaskPlanRevision(ctx, "t-mid", 4); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-mid"); got.PlanRevision != 1 {
		t.Fatalf("the backfill's stamp moved: %d, want it pinned at 1", got.PlanRevision)
	}
	if err := s.UpdatePlanCardBrief(ctx, "t-fresh", "Links API\n\nThe amended brief."); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-fresh"); got.Objective != "Links API\n\nThe amended brief." {
		t.Fatalf("the brief = %q", got.Objective)
	}
}
