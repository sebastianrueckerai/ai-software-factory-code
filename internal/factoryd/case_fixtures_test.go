package factoryd

import (
	"context"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

const caseTask = "t1410"

// cardCaseStore is caseEventStore on database name.
func cardCaseStore(t *testing.T, name string) *PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, name))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE cases, events, intake_events, tasks, approval_requests, plan_revisions, plan_questions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: caseTask, Kind: "agent", Status: "waiting-human", Objective: "x", Repository: "sf/r",
		IssueNumber: 1410, Track: "direct", BudgetEUR: 5}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return s
}

func caseByID(t *testing.T, s *PGStore, id int64) Case {
	t.Helper()
	c, err := s.CaseByID(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseByID %d: %v", id, err)
	}
	return c
}

func caseStepsOf(t *testing.T, s *PGStore, id int64) []CaseStep {
	t.Helper()
	steps, err := s.CaseSteps(context.Background(), id)
	if err != nil {
		t.Fatalf("CaseSteps %d: %v", id, err)
	}
	return steps
}
