package factoryd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The database holds the confirmation's decision, not the writers' good
// behavior: a value outside confirm and revise is refused on a
// confirmation row, a decision on any other question's row is refused,
// and the migration's carry reads history under the old rule, a trimmed
// okay a confirm, any other answer a revise. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestTheDatabaseHoldsThePlanDecision(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "plan_decision"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	for _, q := range []string{
		`TRUNCATE tasks, plan_revisions, plan_questions CASCADE`,
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
		`UPDATE plan_questions SET answer = 'okay', answered_by = 'sebastian', answered_at = now()
			WHERE task_id = 't-old' AND revision = 1`,
		`UPDATE plan_questions SET answer = 'okay — confirmed. Revision 15 is fine.', answered_by = 'sebastian', answered_at = now()
			WHERE task_id = 't-old' AND revision = 2`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q[:40], err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE plan_questions SET decision = 'okay' WHERE task_id = 't-plan' AND idx = 2`); err == nil ||
		!strings.Contains(err.Error(), "plan_question_decision_confirmation") {
		t.Errorf("a decision outside the two values: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE plan_questions SET decision = 'confirm' WHERE task_id = 't-plan' AND idx = 1`); err == nil ||
		!strings.Contains(err.Error(), "plan_question_decision_real_question") {
		t.Errorf("a decision on a real question: %v", err)
	}
	// the carry, from the migration's own statement
	raw, err := os.ReadFile(filepath.Join("..", "schema", "migrations", "factoryd", "20261102000000_plan_question_decision.sql"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(raw), "UPDATE plan_questions")
	if i < 0 {
		t.Fatal("the migration carries no history")
	}
	carry := string(raw)[i : i+strings.Index(string(raw)[i:], ";")+1]
	if _, err := s.db.ExecContext(ctx, carry); err != nil {
		t.Fatalf("the carry: %v", err)
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
}
