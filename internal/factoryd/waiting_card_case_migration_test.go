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

// Requires a disposable Postgres. The waiting_card_case_at_human migration
// puts the live ask case of an agent card that waits on a human with no case
// at the human there: the card's newest live case an answer on the card
// ends. A card with a case at the human, a card that waits on no human, a
// case of its own answer, a human card and a case a rung holds with a wake,
// which climbs to its face, stay as they are, and a second run changes
// nothing.
func TestWaitingCardCaseMigrationPutsTheLiveAskAtTheHuman(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", schematest.DSN(t, "factoryd_waiting_card_case_at_human"))
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
	const name = "factoryd/20261124110000_waiting_card_case_at_human.sql"
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
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track) VALUES
		 ('t1', 5, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', 'direct'),
		 ('t2', 5, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', 'direct'),
		 ('t3', 5, 0, 'running', 'sf/r', 'agent', 'webhook', 'direct'),
		 ('t4', 5, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', 'direct'),
		 ('t5', 5, 0, 'waiting-human', 'sf/r', 'human', 'webhook', 'direct'),
		 ('t6', 5, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', 'direct')`,
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, facts) VALUES
		 (1, 't1', 'sf/r', 'session_cap', 'k1', 'open', 'p', '{}'),
		 (2, 't1', 'sf/r', 'agent_escalate', 'k2', 'open', 'p', '{}'),
		 (3, 't1', 'sf/r', 'monitor_flag', 'k3', 'open', 'p', '{}'),
		 (4, 't2', 'sf/r', 'session_cap', 'k4', 'open', 'p', '{}'),
		 (5, 't2', 'sf/r', 'agent_escalate', 'k5', 'at_human', 'p', '{}'),
		 (6, 't3', 'sf/r', 'session_cap', 'k6', 'open', 'p', '{}'),
		 (7, 't4', 'sf/r', 'merge_approval', 'k7', 'open', 'p', '{}'),
		 (8, 't5', 'sf/r', 'human_card', 'k8', 'open', 'p', '{}')`,
		`INSERT INTO cases (id, task_id, repository, kind, state_key, status, problem, facts, owner_rung, next_wake_at) VALUES
		 (9, 't6', 'sf/r', 'session_cap', 'k9', 'open', 'p', '{}', 1, now())`,
	)
	exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	read := func() map[int64]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT id, status FROM cases ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]string{}
		for rows.Next() {
			var id int64
			var status string
			if err := rows.Scan(&id, &status); err != nil {
				t.Fatal(err)
			}
			out[id] = status
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
		// t1's newest live ask, case 2, goes to the human, not its older
		// case 1 nor its monitor flag; t2 holds one at the human already; t3
		// waits on no human; t4's only live case is its merge's; t5 is a
		// human card; t6's case is the check-in's, due now
		want := map[int64]string{1: "open", 2: "at_human", 3: "open", 4: "open", 5: "at_human", 6: "open", 7: "open", 8: "open", 9: "open"}
		if got := read(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: status by case = %v, want %v", run, got, want)
		}
		exec(`DELETE FROM schema_migrations WHERE name >= '` + name + `' AND name LIKE 'factoryd/%'`)
	}
}
