package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
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
	if want := []string{"chat_token", "created_at", "gate_token", "git_token", "hook_secret_hash", "host_key_hash", "merge_token", "name", "pull_token", "read_token_hash", "ready_at", "staging_hash", "updated_at"}; !slices.Equal(cols, want) {
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

// The chat-intake backfill: a chat-source card still waiting, whose
// question reached only the chat thread, gains the intake escalation row
// the board reads, built from its newest question_asked row. A card that
// already has its row, a settled card and a runner-escalated card gain
// none.
func TestChatIntakeBackfillMigration(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_chat_backfill"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seed := []string{
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, gate, gate_since, finished_at) VALUES
			('t-chat', 1, 0, 'waiting-human', 'sf/r', 'human', 'chat', '', now(), NULL),
			('t-twice', 1, 0, 'waiting-human', 'sf/r', 'human', 'chat', '', now(), NULL),
			('t-runner', 1, 0, 'waiting-human', 'sf/r', 'human', 'api', '', now(), NULL),
			('t-done', 1, 0, 'done', 'sf/r', 'human', 'chat', '', now(), now())`,
		`INSERT INTO events (at, repository, task_id, issue_number, kind, actor, via, detail) VALUES
			(now() - interval '2 hours', 'sf/r', 't-chat', 40, 'question_asked', 'factoryd', 'chat',
			 '{"ask":"Write the acceptance criteria?","why":"the plan needs them","info":"item 12 stops at the goal","recommendation":"Write them","choices":["Write them","Park"],"by":"sebastian"}'),
			(now() - interval '3 hours', 'sf/r', 't-twice', 41, 'question_asked', 'factoryd', 'chat',
			 '{"ask":"Old ask?","why":"old why","info":"old info"}'),
			(now() - interval '1 hour', 'sf/r', 't-twice', 41, 'question_asked', 'factoryd', 'chat',
			 '{"ask":"New ask?","why":"new why","info":"new info"}'),
			(now() - interval '1 hour', 'sf/r', 't-done', 42, 'question_asked', 'factoryd', 'chat',
			 '{"ask":"Settled ask?","why":"settled why","info":"settled info"}'),
			(now() - interval '1 hour', 'sf/r', 't-runner', 43, 'question_asked', 'factoryd', 'factoryd',
			 '{"ask":"Which file?","why":"open","info":"none"}')`,
		`INSERT INTO intake_events (task_id, kind, author, body) VALUES ('t-runner', 'escalation', 'factoryd', 'Which file?')`,
		`DELETE FROM schema_migrations WHERE name = 'factoryd/20261029003000_chat_intake_backfill.sql'`,
	}
	for _, q := range seed {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := schema.Migrate(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_events WHERE kind = 'escalation'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("escalation rows = %d, %v; want 3 (t-chat, t-twice, the seeded t-runner)", n, err)
	}
	var body string
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT body, question FROM intake_events WHERE task_id = 't-chat'`).Scan(&body, &raw); err != nil {
		t.Fatal(err)
	}
	if body != "Write the acceptance criteria?" {
		t.Fatalf("body = %q", body)
	}
	var got HumanQuestion
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := HumanQuestion{
		Ask:            "Write the acceptance criteria?",
		Why:            "the plan needs them",
		Info:           "item 12 stops at the goal",
		Recommendation: "Write them",
		Choices:        []QuestionChoice{{Value: "Write them", Label: "Write them"}, {Value: "Park", Label: "Park"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("question = %+v, want %+v", got, want)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM intake_events WHERE task_id = 't-twice'`).Scan(&body); err != nil || body != "New ask?" {
		t.Fatalf("the twice-asked card's row = %q, %v; want its newest ask", body, err)
	}
}
