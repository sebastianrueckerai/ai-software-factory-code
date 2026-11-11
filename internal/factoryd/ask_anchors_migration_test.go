package factoryd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a case
// seeded before the ask_anchors migration with cases.comment_id 27001 and
// no face step reads 27001 through case_face_comment once it has run.
func TestAskAnchorsMigrationKeepsTheFaceCommentOfOldCases(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_ask_anchors"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const name = "factoryd/20261119100000_ask_anchors.sql"
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`,
		`INSERT INTO schema_migrations (name) VALUES ('` + name + `')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO cases (id, task_id, kind, state_key, status, problem, comment_id) VALUES
		 (1, 't1', 'session_cap', 'k1', 'at_human', 'p', 27001), (2, 't1', 'agent_escalate', 'k2', 'at_human', 'p', 27002), (3, 't1', 'other', 'k3', 'open', 'p', 0)`,
		`DELETE FROM schema_migrations WHERE name = '` + name + `'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int]int64{1: 27001, 2: 27002, 3: 0} {
		var got int64
		if err := db.QueryRowContext(ctx, `SELECT case_face_comment($1)`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("case %d: case_face_comment = %d, %v; want %d", id, got, err, want)
		}
	}
	var steps int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM case_steps`).Scan(&steps); err != nil || steps != 2 {
		t.Fatalf("steps = %d, %v; want one face step each for the two cases with a comment", steps, err)
	}
}
