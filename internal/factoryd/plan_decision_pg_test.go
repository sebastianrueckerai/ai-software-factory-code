package factoryd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The database holds the confirmation's decision, not the writers' good
// behavior. The migration, run on rows of the old format, carries history
// under the old rule, a trimmed okay a confirm and any other answer a
// revise, and then refuses a value outside confirm and revise on a
// confirmation, a decision on any other question, and a confirmation that
// holds words without a decision. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestTheDatabaseHoldsThePlanDecision(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "plan_decision"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	migration, err := os.ReadFile(filepath.Join("..", "schema", "migrations", "factoryd", "20261102000000_plan_question_decision.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`TRUNCATE tasks, plan_revisions, plan_questions CASCADE`,
		`ALTER TABLE plan_questions DROP COLUMN decision`, // the table before the migration; its checks go with the column
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, issue_number, finished_at) VALUES
			('t-plan', 1, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', 'plan', 20, NULL),
			('t-old', 1, 0, 'done', 'sf/r', 'agent', 'webhook', 'plan', 21, '2026-11-01T00:00:00Z')`,
		`INSERT INTO plan_revisions (task_id, revision, plan, sha256) VALUES
			('t-plan', 1, '{"summary": "one"}', 's1'),
			('t-old', 1, '{"summary": "one"}', 'o1'),
			('t-old', 2, '{"summary": "two"}', 'o2')`,
		`INSERT INTO plan_questions (task_id, revision, idx, text, choices, kind) VALUES
			('t-plan', 1, 1, 'How often does the checker probe a site?', '[]', 'question'),
			('t-plan', 1, 2, 'Confirm the plan?', '[]', 'confirmation'),
			('t-old', 1, 1, 'Confirm the plan?', '["okay"]', 'confirmation'),
			('t-old', 2, 1, 'Confirm the plan?', '["okay"]', 'confirmation')`,
		`UPDATE plan_questions SET answer = ' okay ', answered_by = 'sebastian', answered_at = now()
			WHERE task_id = 't-old' AND revision = 1`,
		`UPDATE plan_questions SET answer = 'okay — confirmed. Revision 15 is fine.', answered_by = 'sebastian', answered_at = now()
			WHERE task_id = 't-old' AND revision = 2`,
		string(migration),
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
	for _, tc := range []struct {
		revision int
		want     string
	}{{1, "confirm"}, {2, "revise"}} {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT decision FROM plan_questions WHERE task_id = 't-old' AND revision = $1`, tc.revision).Scan(&got); err != nil || got != tc.want {
			t.Errorf("revision %d carries %q, want %q (%v)", tc.revision, got, tc.want, err)
		}
	}
	for _, tc := range []struct{ update, check string }{
		{`UPDATE plan_questions SET decision = 'okay' WHERE task_id = 't-plan' AND idx = 2`, "plan_question_decision_confirmation"},
		{`UPDATE plan_questions SET decision = 'confirm' WHERE task_id = 't-plan' AND idx = 1`, "plan_question_decision_real_question"},
		{`UPDATE plan_questions SET answer = 'It does' WHERE task_id = 't-plan' AND idx = 2`, "plan_question_decision_answered"},
	} {
		if _, err := s.db.ExecContext(ctx, tc.update); err == nil || !strings.Contains(err.Error(), tc.check) {
			t.Errorf("%s: %v, want %s to refuse it", tc.update, err, tc.check)
		}
	}
}

// A batch never overwrites an answer that lands while it runs: its check
// saw the question open, its write waits on the row another answer holds,
// and once that answer commits the batch stores nothing and answers
// ErrAnswered. Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestABatchNeverOverwritesAnAnswer(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "plan_batch_race"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	for _, q := range []string{
		`TRUNCATE tasks, plan_revisions, plan_questions CASCADE`,
		`INSERT INTO tasks (id, budget_eur, spent_eur, status, repository, kind, source, track, issue_number) VALUES
			('t-plan', 1, 0, 'waiting-human', 'sf/r', 'agent', 'webhook', 'plan', 20)`,
		`INSERT INTO plan_revisions (task_id, revision, plan, sha256) VALUES ('t-plan', 1, '{"summary": "one"}', 's1')`,
		`INSERT INTO plan_questions (task_id, revision, idx, text, choices, kind) VALUES
			('t-plan', 1, 1, 'How often does the checker probe a site?', '[]', 'question')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
	lock, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback() // #nosec G104 -- a no-op after Commit
	if _, err := lock.ExecContext(ctx, `SELECT 1 FROM plan_questions WHERE task_id = 't-plan' AND idx = 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.AnswerPlanQuestions(ctx, "t-plan", 1, []PlanAnswer{{Idx: 1, Text: "5 minutes"}}, "bob")
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var waiting int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()
			AND wait_event_type = 'Lock' AND query LIKE 'UPDATE plan_questions%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the batch never reached its write")
		}
	}
	if _, err := lock.ExecContext(ctx, `UPDATE plan_questions SET answer = '60 seconds', answered_by = 'alice', answered_at = now()
		WHERE task_id = 't-plan' AND idx = 1`); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrAnswered) {
		t.Fatalf("the batch behind alice's answer: %v, want ErrAnswered", err)
	}
	var answer, by string
	if err := s.db.QueryRowContext(ctx, `SELECT answer, answered_by FROM plan_questions WHERE task_id = 't-plan' AND idx = 1`).Scan(&answer, &by); err != nil ||
		answer != "60 seconds" || by != "alice" {
		t.Fatalf("the stored answer %q by %q (%v), want alice's 60 seconds", answer, by, err)
	}
}
