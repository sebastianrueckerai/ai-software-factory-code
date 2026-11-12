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

// Requires a disposable Postgres. The merge_case_policy_version migration
// copies the policy version of a merge approval's spec into the facts of
// its case opened before cases kept it: a case without it gains it, a case
// that holds one keeps it, a case whose approval spec carries none and a
// case of another kind stay as they are, and a second run changes nothing.
func TestMergeCasePolicyVersionMigrationCopiesTheSpecsVersionIntoOldCases(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_merge_case_policy_version"))
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
	const name = "factoryd/20261120090000_merge_case_policy_version.sql"
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
		 (1, 'merge', 't1', 'sf/r', '{"policy_version": 9, "risk_class": "migration"}'),
		 (2, 'merge', 't2', 'sf/r', '{"policy_version": 9}'),
		 (3, 'merge', 't3', 'sf/r', '{"risk_class": "standard"}'),
		 (4, 'merge', 't4', 'sf/r', '{"policy_version": 12}')`,
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, facts) VALUES
		 (1, 't1', 'sf/r', 'merge_approval', 'k1', 'at_human', 'p', '{"approval": 1, "class": "migration"}'),
		 (2, 't2', 'sf/r', 'merge_approval', 'k2', 'at_human', 'p', '{"approval": 2, "policy_version": 7}'),
		 (3, 't3', 'sf/r', 'merge_approval', 'k3', 'at_human', 'p', '{"approval": 3}'),
		 (4, 't4', 'sf/r', 'other', 'k4', 'open', 'p', '{"approval": 4}')`,
	)
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	read := func() map[int64]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id, coalesce(facts->>'policy_version', '-') FROM cases ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]string{}
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatal(err)
			}
			out[id] = v
		}
		return out
	}
	for run := 1; run <= 2; run++ {
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		// case 1 gains its spec's 9; case 2 keeps its 7; case 3's spec has none; case 4 is no merge approval
		if got, want := read(), map[int64]string{1: "9", 2: "7", 3: "-", 4: "-"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: policy_version by case = %v, want %v", run, got, want)
		}
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	}
}
