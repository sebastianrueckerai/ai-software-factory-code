package factoryd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). The
// criteria_ask_ids migration gives each older criteria ask the criteria
// it named: a gap and a never-judged ask from their approval_requested
// rows, a gap with no such row from its reason; a never-judged ask with
// neither keeps its spec, as do an ask that names its criteria already and
// a merge ask. A second run changes nothing.
func TestCriteriaAskIDsMigration(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_criteria_ask_ids"))
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
	const name = "factoryd/20261117153526_criteria_ask_ids.sql"
	migrate := func() {
		t.Helper()
		exec(`DELETE FROM schema_migrations WHERE name = '` + name + `'`)
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO approval_requests (id, kind, task_id, project, spec, reason, status) VALUES
		(1, 'criteria', 'p1', 'sf/r', '{"task": "p1-int"}', 'criteria not met with the plan''s cards done: c2, c3', 'denied'),
		(2, 'criteria_rerun', 'p1', 'sf/r', '{"task": "p1-int"}', 'criteria never judged: the criteria gate has not settled c1', 'denied'),
		(3, 'criteria', 'p2', 'sf/r', '{"task": "p2-int"}', 'criteria not met with the plan''s cards done: c4', 'denied'),
		(4, 'criteria_rerun', 'p2', 'sf/r', '{"task": "p2-int"}', 'criteria never judged: the criteria gate has not settled c5', 'pending'),
		(5, 'criteria', 'p3', 'sf/r', '{"task": "p3-int", "criteria": ["c9"]}', 'criteria not met once every card of the plan ended: c9', 'denied'),
		(6, 'merge', 'p3', 'sf/r', '{}', 'merge', 'pending')`,
		`INSERT INTO events (at, repository, task_id, kind, actor, via, detail) VALUES
		('2026-11-01 10:00:00+00', 'sf/r', 'p1', 'approval_requested', 'factoryd', 'factoryd', '{"approval": 1, "kind": "criteria", "criteria": ["c3", "c2"]}'),
		('2026-11-01 10:00:00+00', 'sf/r', 'p1', 'approval_requested', 'factoryd', 'factoryd', '{"approval": 2, "kind": "criteria_rerun", "criteria": ["c1"]}'),
		('2026-11-01 10:00:00+00', 'sf/r', 'p3', 'approval_requested', 'factoryd', 'factoryd', '{"approval": 5, "kind": "criteria", "criteria": ["c8"]}')`)
	want := map[int64]string{
		1: `{"task": "p1-int", "criteria": ["c3", "c2"]}`,
		2: `{"task": "p1-int", "criteria": ["c1"]}`,
		3: `{"task": "p2-int", "criteria": ["c4"]}`,
		4: `{"task": "p2-int"}`,
		5: `{"task": "p3-int", "criteria": ["c9"]}`,
		6: `{}`,
	}
	for range 2 {
		migrate()
		for id, spec := range want {
			var same bool
			if err := db.QueryRowContext(ctx, `SELECT spec = $2::jsonb FROM approval_requests WHERE id = $1`, id, spec).Scan(&same); err != nil || !same {
				var got string
				_ = db.QueryRowContext(ctx, `SELECT spec::text FROM approval_requests WHERE id = $1`, id).Scan(&got)
				t.Fatalf("ask %d: spec %s (%v), want %s", id, got, err, spec)
			}
		}
	}
}
