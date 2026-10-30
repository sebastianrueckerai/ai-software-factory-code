package factoryd

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): revisions
// stored with plain-string acceptance read as items once the migration
// has run, and a revision already in items stays as it was.
func TestPlanAcceptanceItemsMigration(t *testing.T) {
	ctx := context.Background()
	dsn := schematest.DSN(t, "factoryd_plan_acceptance")
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, id := range []string{"t-old", "t-new"} {
		if err := s.CreateTask(ctx, Task{ID: id, Status: "running", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Track: "plan", IssueNumber: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	const newPlan = `{"summary": "n", "cards": [{"key": "k", "acceptance": [{"criterion": "c1", "text": "a"}, {"text": "b"}]}]}`
	for _, stmt := range []string{
		`INSERT INTO plan_revisions (task_id, revision, plan, sha256) VALUES
		 ('t-old', 1, '{"summary": "o", "cards": [{"key": "k", "acceptance": ["a", "b"]}, {"key": "j", "acceptance": [{"criterion": "c2", "text": "x"}, "y"]}]}', 's-old'),
		 ('t-new', 1, '` + newPlan + `', 's-new')`,
		`DELETE FROM schema_migrations WHERE name = 'factoryd/20261105120000_plan_acceptance_items.sql'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	old, _, err := planOfStore(ctx, s, "t-old", "c")
	if err != nil {
		t.Fatalf("old revision: %v", err)
	}
	if len(old.Cards) != 2 ||
		!slices.Equal(old.Cards[0].Acceptance, []PlanItem{{Text: "a"}, {Text: "b"}}) ||
		!slices.Equal(old.Cards[1].Acceptance, []PlanItem{{Criterion: "c2", Text: "x"}, {Text: "y"}}) {
		t.Fatalf("old cards %+v", old.Cards)
	}
	cur, _, err := planOfStore(ctx, s, "t-new", "c")
	if err != nil {
		t.Fatalf("new revision: %v", err)
	}
	if len(cur.Cards) != 1 || !slices.Equal(cur.Cards[0].Acceptance, []PlanItem{{Criterion: "c1", Text: "a"}, {Text: "b"}}) {
		t.Fatalf("new cards %+v", cur.Cards)
	}
	var sha string
	if err := db.QueryRowContext(ctx, `SELECT sha256 FROM plan_revisions WHERE task_id = 't-old'`).Scan(&sha); err != nil || sha != "s-old" {
		t.Fatalf("sha256 %q %v", sha, err)
	}
}
