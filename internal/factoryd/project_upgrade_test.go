package factoryd

import (
	"context"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Review m7: migration 0032 on a database in the state before it (the
// settings columns of 0024, managed, table envelopes, a registered row)
// keeps every minted token and drops the rest.
func TestMigration0032KeepsTheMintedTokens(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_upgrade"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`ALTER TABLE projects ADD COLUMN repository text NOT NULL DEFAULT '', ADD COLUMN merge_rule text NOT NULL DEFAULT 'human',
			ADD COLUMN notify text[] NOT NULL DEFAULT '{}', ADD COLUMN namespaces text[] NOT NULL DEFAULT '{}',
			ADD COLUMN environments jsonb NOT NULL DEFAULT '[]', ADD COLUMN quota_cpu text NOT NULL DEFAULT '',
			ADD COLUMN quota_memory text NOT NULL DEFAULT '', ADD COLUMN quota_storage text NOT NULL DEFAULT '',
			ADD COLUMN secrets text[] NOT NULL DEFAULT '{}', ADD COLUMN managed boolean NOT NULL DEFAULT true`,
		`CREATE TABLE envelopes (project text PRIMARY KEY, monthly_eur numeric NOT NULL, set_by text NOT NULL)`,
		`INSERT INTO envelopes VALUES ('demo-app/demo-app', 30, 'sebastian')`,
		`INSERT INTO projects (name, repository, merge_rule, notify, namespaces, quota_cpu, managed, read_token_hash, hook_secret_hash,
			host_key_hash, pull_token, git_token, merge_token)
			VALUES ('demo-app', 'demo-app/demo-app', 'auto', '{alice}', '{demo-staging}', '2', true, 'rh', 'hh', 'kh', 'pt', 'gt', 'mt')`,
		`DELETE FROM schema_migrations WHERE name = 'factoryd/0032_projects_from_files.sql'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := schema.Migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	var cols []string
	rows, err := s.db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'projects' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if want := []string{"created_at", "git_token", "hook_secret_hash", "host_key_hash", "merge_token", "name", "pull_token", "read_token_hash", "ready_at", "updated_at"}; !slices.Equal(cols, want) {
		t.Fatalf("columns after 0032 = %v", cols)
	}
	var tokens string
	if err := s.db.QueryRowContext(ctx, `SELECT concat_ws(' ', read_token_hash, hook_secret_hash, host_key_hash, pull_token, git_token, merge_token)
		FROM projects WHERE name = 'demo-app'`).Scan(&tokens); err != nil || tokens != "rh hh kh pt gt mt" {
		t.Fatalf("tokens after 0032 = %q, %v", tokens, err)
	}
	var envelopes bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass('envelopes') IS NOT NULL`).Scan(&envelopes); err != nil || envelopes {
		t.Fatalf("envelopes after 0032: %v, %v", envelopes, err)
	}

	// ToHumanLane on Postgres: a queued agent task only, which leaves its
	// lane and process
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, gate, gate_since)
		VALUES ('t-q', 1, 0, 'queued', 'sf/r', 'agent', 'intake', now()), ('t-r', 1, 0, 'running', 'sf/r', 'agent', 'checks', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET lane = 'ready', process = 'maintenance'`); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.ToHumanLane(ctx, "t-q"); !moved || err != nil {
		t.Fatalf("queued: %v, %v", moved, err)
	}
	if moved, err := s.ToHumanLane(ctx, "t-r"); moved || err != nil {
		t.Fatalf("running: %v, %v", moved, err)
	}
	var lane string
	if err := s.db.QueryRowContext(ctx, `SELECT kind || ' ' || status || ' ' || gate || ' ' || (gate_since IS NULL) || ' ' || COALESCE(lane, '-') || ' ' || COALESCE(process, '-')
		FROM tasks WHERE id = 't-q'`).Scan(&lane); err != nil ||
		lane != "human waiting-human  true - -" {
		t.Fatalf("t-q = %q, %v", lane, err)
	}
}
