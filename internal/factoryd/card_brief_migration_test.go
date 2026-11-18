package factoryd

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres. The card_brief migration moves a
// human's summary into the summary, so no text a human wrote is lost,
// keeps its column for the factoryd of the release before, and gives every row an empty why line and a checked
// brief. A second run changes nothing.
func TestCardBriefMigrationKeepsTheHumansSummary(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_card_brief"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(stmts ...string) {
		t.Helper()
		for _, stmt := range stmts {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	const name = "factoryd/20261121100000_card_brief.sql"
	entries, err := os.ReadDir("../schema/migrations/factoryd")
	if err != nil {
		t.Fatal(err)
	}
	exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	held := 0
	for _, e := range entries {
		if n := "factoryd/" + e.Name(); n >= name {
			exec(`INSERT INTO schema_migrations (name) VALUES ('` + n + `')`)
			held++
		}
	}
	if held == 0 {
		t.Fatalf("no migration file is named %s", name)
	}
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, human_summary) VALUES
		('t-set', 1, 0, 'queued', 'sf/r', 'agent', 'webhook', 'direct', 'the cache, at last'),
		('t-null', 1, 0, 'queued', 'sf/r', 'agent', 'webhook', 'direct', NULL),
		('t-empty', 1, 0, 'queued', 'sf/r', 'agent', 'webhook', 'direct', '')`)
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	type row struct {
		ID, Why, Summary string
		Unchecked        bool
	}
	for run := 1; run <= 2; run++ {
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT id, why, summary, brief_unchecked FROM tasks ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ID, &r.Why, &r.Summary, &r.Unchecked); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		want := []row{{ID: "t-empty"}, {ID: "t-null"}, {ID: "t-set", Summary: "the cache, at last"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: rows = %+v, want %+v", run, got, want)
		}
		var kept bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'tasks' AND column_name = 'human_summary')`).Scan(&kept); err != nil || !kept {
			t.Fatalf("run %d: human_summary is gone (%v), want it kept for the factoryd of the release before", run, err)
		}
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	}
}
