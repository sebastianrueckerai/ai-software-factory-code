package factoryd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestZZDebugAnswerBatch(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_zzdebug20"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{ID: "t-plan", Status: "waiting-human", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Lane: "plan", IssueNumber: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary": "one"}`), "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions); err != nil {
		t.Fatal(err)
	}
	tx, _ := s.db.BeginTx(ctx, nil)
	defer tx.Rollback() // #nosec G104
	b, _ := json.Marshal([]PlanAnswer{{Idx: 2, Text: "30 days"}, {Idx: 1, Text: "60 seconds"}})
	var asked, answered bool
	err = tx.QueryRowContext(ctx, `WITH want (idx, text) AS (SELECT (e->>'idx')::int, e->>'text' FROM jsonb_array_elements($3::jsonb) AS a(e))
 		SELECT EXISTS (SELECT 1 FROM want w WHERE NOT EXISTS (SELECT 1 FROM plan_questions WHERE task_id = $1 AND revision = $2 AND idx = w.idx)),
 			EXISTS (SELECT 1 FROM want w WHERE EXISTS (SELECT 1 FROM plan_questions WHERE task_id = $1 AND revision = $2 AND idx = w.idx AND answer IS NOT NULL))`,
		"t-plan", 1, string(b)).Scan(&asked, &answered)
	t.Logf("copied -> asked %v (%v)", asked, err)
	err = s.AnswerPlanQuestions(ctx, "t-plan", 1, []PlanAnswer{{Idx: 2, Text: "30 days"}, {Idx: 1, Text: "60 seconds"}}, "alice")
	t.Logf("method -> %v", err)
}
