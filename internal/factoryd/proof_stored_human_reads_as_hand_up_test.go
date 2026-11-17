package factoryd

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// [stored_human_reads_as_hand_up] The schema migration moves the stored
// decision values of the moments' case steps onto the rungs' names — human
// to hand_up, dropped_all to reverify — so one reader stays: after it a
// step the store wrote before the change reads as the hand-up, with the
// moment's grounds as its found, and a second run of the migration changes
// nothing. Requires a disposable Postgres.
func TestProofStoredHumanReadsAsHandUp(t *testing.T) {
	ctx := context.Background()
	entries, err := os.ReadDir("../schema/migrations/factoryd")
	if err != nil {
		t.Fatal(err)
	}
	var file string
	for _, e := range entries {
		b, err := os.ReadFile("../schema/migrations/factoryd/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "case_steps") && strings.Contains(s, "decision") &&
			(strings.Contains(s, "hand_up") || strings.Contains(s, "reverify")) {
			file = e.Name()
			break
		}
	}
	if file == "" {
		t.Fatal("no migration moves the stored decision values of case_steps onto the rungs' names (human to hand_up, dropped_all to reverify)")
	}
	name := "factoryd/" + file
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_proof_stored_human"))
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
	exec(`CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	for _, e := range entries {
		if n := "factoryd/" + e.Name(); n >= name {
			exec(`INSERT INTO schema_migrations (name) VALUES ('` + n + `')`)
		}
	}
	if err := schema.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec(
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, facts) VALUES
		 (1, 't1', 'sf/r', 'gate_stop', 'k1', 'at_human', 'the round raised findings', '{}')`,
		`INSERT INTO case_steps (case_id, rung, actor, outcome, decision, grounds) VALUES
		 (1, 1, 'checkin', 'handed_up', 'human', 'the merge class keeps a human on this change'),
		 (1, 1, 'checkin', 'handed_up', 'dropped_all', 'every finding fell against checkable evidence'),
		 (1, 1, 'checkin', 'handed_up', 'hand_up', 'a person must answer this one'),
		 (1, 2, 'member', 'handed_up', '', '')`,
	)
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	for run := 1; run <= 2; run++ {
		if err := schema.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, `SELECT decision, grounds FROM case_steps WHERE case_id = 1 ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		var steps []CaseStep
		for rows.Next() {
			var s CaseStep
			if err := rows.Scan(&s.Decision, &s.Grounds); err != nil {
				t.Fatal(err)
			}
			s.Rung, s.Actor, s.Outcome = 1, CheckinDecider, StepHandedUp
			steps = append(steps, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		want := []string{RungHandUp, ActReverify, RungHandUp, ""}
		got := make([]string, len(steps))
		for i, s := range steps {
			got[i] = s.Decision
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: decisions = %v, want %v", run, got, want)
		}
		up := HandedUpBy(steps, 1, "")
		if up == nil || up.Found != "the merge class keeps a human on this change" || up.Action != "" {
			t.Fatalf("run %d: the stored human step reads as %+v, want the hand-up with the moment's grounds as its found", run, up)
		}
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	}
}
