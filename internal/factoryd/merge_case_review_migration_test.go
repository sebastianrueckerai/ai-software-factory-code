package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres. The merge_case_review migration gives a
// live merge case opened before RequestCase wrote them the two facts the
// merge's suggestion reads, from its approval's spec: whether the model
// review approved the head (an approve with no note) and the review's
// summary. A case that holds them keeps them; an ended case, a case of
// another kind and a case whose spec holds no review stay as they are; and
// a second run changes nothing.
func TestMergeCaseReviewMigrationGivesOldCasesTheReviewsPick(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_merge_case_review"))
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
	const name = "factoryd/20261124100000_merge_case_review.sql"
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
	exec(
		`INSERT INTO approval_requests (id, kind, task_id, project, spec) VALUES
		 (1, 'merge', 't1', 'sf/r', '{"verdict_summary": "adds the column", "review": {"verdict": "approve", "summary": "adds the column"}}'),
		 (2, 'merge', 't2', 'sf/r', '{"verdict_summary": "the column lacks a default", "review": {"verdict": "concerns"}}'),
		 (3, 'merge', 't3', 'sf/r', '{"verdict_summary": "the review did not run", "review": {"verdict": "approve", "note": "the review did not run"}}'),
		 (4, 'merge', 't4', 'sf/r', '{"verdict_summary": "adds the column", "review": {"verdict": "approve"}}'),
		 (5, 'merge', 't5', 'sf/r', '{"verdict_summary": "adds the column", "review": {"verdict": "approve"}}'),
		 (6, 'merge', 't7', 'sf/r', '{"verdict_summary": "no review"}')`,
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, facts) VALUES
		 (1, 't1', 'sf/r', 'merge_approval', 'k1', 'at_human', 'p', '{"approval": 1}'),
		 (2, 't2', 'sf/r', 'merge_approval', 'k2', 'at_human', 'p', '{"approval": 2}'),
		 (3, 't3', 'sf/r', 'merge_approval', 'k3', 'at_human', 'p', '{"approval": 3}'),
		 (4, 't4', 'sf/r', 'merge_approval', 'k4', 'at_human', 'p', '{"approval": 4, "review_approved": false, "review_summary": ""}'),
		 (5, 't5', 'sf/r', 'merge_approval', 'k5', 'resolved', 'p', '{"approval": 5}'),
		 (6, 't6', 'sf/r', 'other', 'k6', 'open', 'p', '{"approval": 1}'),
		 (7, 't7', 'sf/r', 'merge_approval', 'k7', 'at_human', 'p', '{"approval": 6}')`,
	)
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	read := func() map[int64]map[string]any {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id, facts FROM cases ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]map[string]any{}
		for rows.Next() {
			var id int64
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				t.Fatal(err)
			}
			var facts map[string]any
			if err := json.Unmarshal(raw, &facts); err != nil {
				t.Fatal(err)
			}
			out[id] = facts
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for run := 1; run <= 2; run++ {
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		// case 1's review approved; case 2's had concerns; case 3's did not
		// run; case 4 holds its facts already; case 5 ended; case 6 is no
		// merge approval; case 7's spec holds no review
		want := map[int64]map[string]any{
			1: {"approval": 1.0, "review_approved": true, "review_summary": "adds the column"},
			2: {"approval": 2.0, "review_approved": false, "review_summary": "the column lacks a default"},
			3: {"approval": 3.0, "review_approved": false, "review_summary": "the review did not run"},
			4: {"approval": 4.0, "review_approved": false, "review_summary": ""},
			5: {"approval": 5.0},
			6: {"approval": 1.0},
			7: {"approval": 6.0},
		}
		if got := read(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: facts by case = %v, want %v", run, got, want)
		}
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	}
}
