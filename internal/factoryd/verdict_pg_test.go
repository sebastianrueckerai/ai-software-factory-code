package factoryd

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The criteria verdict rows and the plan catches round-trip through
// Postgres: the upsert keeps a source's other rows, the approver's row
// stands beside the review's, and the catches dedupe on their key.
func TestPGStoreCriteriaVerdictsAndCatches(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, criteria_verdicts, plan_catches CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t1", Kind: "agent", Status: "queued", Objective: "x", Repository: "sf/r", IssueNumber: 1, Track: "direct"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	review := []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "go test ./... exits 0", Fix: "card", Kind: CriterionTest},
		{Criterion: "c3", Verdict: "unmet", Evidence: "the endpoint times out", Fix: "planner", Kind: CriterionInvariant},
	}
	if err := s.PutCriteriaVerdicts(ctx, "t1", "review", FactoryDecider, review); err != nil {
		t.Fatalf("PutCriteriaVerdicts review: %v", err)
	}
	// the same source again: c1 updates in place, c3 stands
	if err := s.PutCriteriaVerdicts(ctx, "t1", "review", FactoryDecider, []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "go test ./... exits 0, 214 tests", Fix: "card", Kind: CriterionTest},
	}); err != nil {
		t.Fatalf("PutCriteriaVerdicts review again: %v", err)
	}
	// the approver's waiver of c3 lands beside the review's row
	if err := s.PutCriteriaVerdicts(ctx, "t1", "approver", "sebastian", []CriterionVerdict{
		{Criterion: "c3", Verdict: "waived", Evidence: "the timeout is the provider's SLA", Kind: CriterionInvariant},
	}); err != nil {
		t.Fatalf("PutCriteriaVerdicts approver: %v", err)
	}
	rows, err := s.CriteriaVerdicts(ctx, "t1")
	if err != nil {
		t.Fatalf("CriteriaVerdicts: %v", err)
	}
	for i := range rows {
		rows[i].CreatedAt = rows[i].CreatedAt.UTC()
	}
	want := []CriterionVerdict{
		{Criterion: "c1", Verdict: "met", Evidence: "go test ./... exits 0, 214 tests", Fix: "card", Kind: CriterionTest, Source: "review", DecidedBy: FactoryDecider},
		{Criterion: "c3", Verdict: "waived", Evidence: "the timeout is the provider's SLA", Kind: CriterionInvariant, Source: "approver", DecidedBy: "sebastian"},
		{Criterion: "c3", Verdict: "unmet", Evidence: "the endpoint times out", Fix: "planner", Kind: CriterionInvariant, Source: "review", DecidedBy: FactoryDecider},
	}
	for i := range rows {
		if rows[i].CreatedAt.IsZero() {
			t.Errorf("row %d carries no time", i)
			rows[i].CreatedAt = want[i].CreatedAt
		}
	}
	if !slices.EqualFunc(rows, want, func(a, b CriterionVerdict) bool {
		return a.Criterion == b.Criterion && a.Verdict == b.Verdict && a.Evidence == b.Evidence &&
			a.Fix == b.Fix && a.Kind == b.Kind && a.Source == b.Source && a.DecidedBy == b.DecidedBy
	}) {
		t.Fatalf("rows =\n%+v\nwant\n%+v", rows, want)
	}
	if ids, err := s.CriteriaVerdictTasks(ctx); err != nil || !slices.Equal(ids, []string{"t1"}) {
		t.Fatalf("CriteriaVerdictTasks = %v, %v; want [t1]", ids, err)
	}

	// the catches: filed under a plan revision, deduped on their key
	plan, _ := json.Marshal(Plan{Summary: "s"})
	rev, err := s.AddPlanRevision(ctx, "t1", plan, "sha-1")
	if err != nil {
		t.Fatalf("AddPlanRevision: %v", err)
	}
	if _, err := s.PutPlanCatches(ctx, "t1", rev, CatchDelta, FinderReview, []string{"c3 at integration round 1", "c1 at integration round 1"}); err != nil {
		t.Fatalf("PutPlanCatches: %v", err)
	}
	if again, err := s.PutPlanCatches(ctx, "t1", rev, CatchDelta, FinderReview, []string{"c3 at integration round 1"}); err != nil || again {
		t.Fatalf("the same catch again = %v, %v; want false, nil (deduped)", again, err)
	}
	if _, err := s.PutPlanCatches(ctx, "t1", rev, CatchDeltaFiled, FinderReview, []string{"c3 at integration round 1"}); err != nil {
		t.Fatalf("PutPlanCatches filed: %v", err)
	}
	// the same finder under another revision is another catch
	if next, err := s.AddPlanRevision(ctx, "t1", plan, "sha-2"); err != nil {
		t.Fatalf("AddPlanRevision 2: %v", err)
	} else if _, err := s.PutPlanCatches(ctx, "t1", next, CatchDelta, FinderReview, []string{"c3 at integration round 1"}); err != nil {
		t.Fatalf("PutPlanCatches round 2: %v", err)
	}
	dues, err := s.PlanCatches(ctx, "t1")
	if err != nil {
		t.Fatalf("PlanCatches: %v", err)
	}
	if len(dues) != 4 {
		t.Fatalf("catches =\n%+v\nwant 4 rows (two deltas of round 1, its filed marker, round 2's delta)", dues)
	}
	first := dues[0]
	if first.Revision != rev || first.Kind != CatchDelta || first.Finder != FinderReview || first.Detail != "c3 at integration round 1" {
		t.Errorf("first catch = %+v, want round %d's first delta c3", first, rev)
	}
}
