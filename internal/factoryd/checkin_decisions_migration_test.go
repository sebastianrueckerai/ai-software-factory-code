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

// Requires a disposable Postgres. The checkin_decisions migration moves
// the decision values case_steps holds onto the rungs' one action list,
// from the rows the store wrote before the change: the moment's human
// answer reads hand_up and the dropped-all reverify, while rows of other
// decisions and empty decisions stay. The read side is the one reader:
// the migrated hand_up step is the hand-up whose reason is what the
// moment found. A second run changes nothing.
func TestCheckinDecisionsMigrationMovesStoredDecisionsOntoTheRungsNames(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_checkin_decisions"))
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
	const name = "factoryd/20261120120000_checkin_decisions.sql"
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
	// the rows the store wrote before the change: a moment's human answer
	// and its dropped-all beside the rungs' own names, and steps with no
	// decision at all
	exec(
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem) VALUES
		 (1, 't1', 'sf/r', 'checkin_human', 'k1', 'at_human', 'only a human can act'),
		 (2, 't2', 'sf/r', 'gate_stop', 'k2', 'at_human', 'the round came back with findings'),
		 (3, 't3', 'sf/r', 'other', 'k3', 'resolved', 'a card the human closed')`,
		`INSERT INTO case_steps (case_id, rung, actor, outcome, found, decision, grounds) VALUES
		 (1, 0, 'factoryd', 'handed_up', 'no resolver fits', '', ''),
		 (1, 1, 'checkin', 'handed_up', '', 'human', 'a goal conflict: the objective contradicts the review'),
		 (2, 1, 'checkin', 'handed_up', '', 'dropped_all', 'every finding dropped against evidence'),
		 (2, 2, 'factory agent', 'handed_up', 'the findings stand', 'handover', 'the note names what to do first'),
		 (3, 3, 'sebastian', 'resolved', '', 'close', 'done')`,
	)
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	steps := func() []CaseStep {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT case_id, rung, outcome, decision, grounds FROM case_steps ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []CaseStep
		for rows.Next() {
			var s CaseStep
			if err := rows.Scan(&s.CaseID, &s.Rung, &s.Outcome, &s.Decision, &s.Grounds); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
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
		// human reads hand_up, dropped_all reverify; the rungs' own names
		// and the empty decisions stay
		got := steps()
		want := []CaseStep{
			{CaseID: 1, Rung: 0, Outcome: StepHandedUp},
			{CaseID: 1, Rung: 1, Outcome: StepHandedUp, Decision: RungHandUp, Grounds: "a goal conflict: the objective contradicts the review"},
			{CaseID: 2, Rung: 1, Outcome: StepHandedUp, Decision: ActReverify, Grounds: "every finding dropped against evidence"},
			{CaseID: 2, Rung: 2, Outcome: StepHandedUp, Decision: ActHandover, Grounds: "the note names what to do first"},
			{CaseID: 3, Rung: 3, Outcome: StepResolved, Decision: ActClose, Grounds: "done"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: steps = %+v, want %+v", run, got, want)
		}
		// the face reads the migrated row: a moment's hand_up step is the
		// hand-up whose reason is what the moment found, and no action is
		// recommended over it
		up := handedUpBy(got[1:2], 0, "")
		if up == nil || up.Action != "" || up.Found != "a goal conflict: the objective contradicts the review" {
			t.Fatalf("run %d: the moment's hand-up = %+v, want its grounds found and no action", run, up)
		}
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	}
}
