package factoryd

import (
	"context"
	"encoding/json"
	"errors"
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
	// a delete takes every row of one criterion but the approver's: c3's
	// review and judgement rows go, its waiver stands, c1 is untouched
	if err := s.PutCriteriaVerdicts(ctx, "t1", "judgement", FactoryDecider, []CriterionVerdict{{Criterion: "c3", Verdict: "met", Kind: CriterionInvariant}}); err != nil {
		t.Fatalf("PutCriteriaVerdicts judgement: %v", err)
	}
	if err := s.DeleteVerdictsButApprover(ctx, "t1", "c3"); err != nil {
		t.Fatalf("DeleteVerdictsButApprover: %v", err)
	}
	if err := s.DeleteVerdictsButApprover(ctx, "t1", "c9"); err != nil {
		t.Fatalf("DeleteVerdictsButApprover of no row: %v", err)
	}
	if rows, err := s.CriteriaVerdicts(ctx, "t1"); err != nil || len(rows) != 2 || rows[0].Criterion != "c1" || rows[1].Criterion != "c3" || rows[1].Source != "approver" {
		t.Fatalf("rows after the delete = %+v, %v; want c1's and c3's waiver", rows, err)
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

// The waivers and the plan_closed row land together or not at all: a close
// row insertEvent refuses (no repository) leaves no waiver, and a good one
// leaves both. The plan must be done, and closes once: a plan that is not
// done, or whose row stands, is refused. Two plan_closed rows of one plan
// collapse to one.
func TestPGStoreClosePlanWithWaivers(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, criteria_verdicts, events, approval_requests CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, id := range []string{"t1", "t2", "t3", "t5"} {
		if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "done", Objective: "x", Repository: "sf/r", IssueNumber: 1, Track: "plan"}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'failed' WHERE id = 't5'`); err != nil {
		t.Fatal(err)
	}
	// a plan whose only merge is a pull_request_merged row holds production,
	// as the open groups read it; one with neither commit nor row does not
	if err := s.CreateTask(ctx, Task{ID: "t3-card", Kind: "agent", Status: "done", Objective: "x", Repository: "sf/r", IssueNumber: 3, Track: "direct", Source: SourcePlan, Parent: "t3", PlanKey: "a"}); err != nil {
		t.Fatal(err)
	}
	if merged, err := s.PlanMerged(ctx, "t3"); err != nil || merged {
		t.Fatalf("PlanMerged with no commit and no row = %t, %v; want false", merged, err)
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t3-card", Kind: "pull_request_merged"}); err != nil {
		t.Fatal(err)
	}
	if merged, err := s.PlanMerged(ctx, "t3"); err != nil || !merged {
		t.Fatalf("PlanMerged with a pull_request_merged row and no merge commit = %t, %v; want true", merged, err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t4", Kind: "agent", Status: "running", Objective: "x", Repository: "sf/r", IssueNumber: 2, Track: "plan"}); err != nil {
		t.Fatal(err)
	}
	waivers := []CriterionVerdict{{Criterion: "c1", Verdict: "waived", Evidence: "why", Kind: CriterionTest}}
	prepared := func(e *Event) func(context.Context) (PlanClose, error) {
		return func(context.Context) (PlanClose, error) {
			return PlanClose{Home: "t1", Waive: waivers, Closed: e, Words: "closed by sebastian"}, nil
		}
	}
	if err := s.ClosePlanWithWaivers(ctx, "t1", "sebastian", prepared(&Event{TaskID: "t1", Kind: EventPlanClosed})); err == nil {
		t.Fatal("a close row without its repository was accepted")
	}
	if rows, _ := s.CriteriaVerdicts(ctx, "t1"); len(rows) != 0 {
		t.Fatalf("the failed close left rows %+v", rows)
	}
	refused := errors.New("not this time")
	if err := s.ClosePlanWithWaivers(ctx, "t1", "sebastian", func(context.Context) (PlanClose, error) { return PlanClose{}, refused }); !errors.Is(err, refused) {
		t.Fatalf("a refusing prepare: %v, want its error", err)
	}
	if err := s.ClosePlanWithWaivers(ctx, "t4", "sebastian", prepared(nil)); !errors.Is(err, ErrNotDonePlan) {
		t.Fatalf("a running plan: %v, want ErrNotDonePlan", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO approval_requests (project, task_id, kind, status, spec) VALUES ('sf/r', 't1', 'criteria', 'pending', '{}'), ('sf/r', 't1', 'merge', 'pending', '{}')`); err != nil {
		t.Fatal(err)
	}
	e := &Event{Repository: "sf/r", TaskID: "t1", Kind: EventPlanClosed, Actor: "sebastian", Via: ViaConsole, Detail: map[string]any{"criteria": 1, "words": "closed by sebastian"}}
	if err := s.ClosePlanWithWaivers(ctx, "t1", "sebastian", prepared(e)); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.CriteriaVerdicts(ctx, "t1")
	if _, closed, err := s.PlanClosed(ctx, "t1"); err != nil || !closed || len(rows) != 1 || rows[0].Verdict != "waived" || rows[0].DecidedBy != "sebastian" {
		t.Fatalf("closed %t (%v), rows %+v; want both written", closed, err, rows)
	}
	var pending []string
	rs, err := s.db.QueryContext(ctx, `SELECT kind FROM approval_requests WHERE task_id = 't1' AND status = 'pending'`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var k string
		rs.Scan(&k)
		pending = append(pending, k)
	}
	rs.Close()
	if !slices.Equal(pending, []string{"merge"}) {
		t.Errorf("pending asks %v, want the merge ask alone: the criteria ask ends with the close", pending)
	}
	if err := s.ClosePlanWithWaivers(ctx, "t1", "sebastian", prepared(e)); !errors.Is(err, ErrPlanClosed) {
		t.Fatalf("a second close: %v, want ErrPlanClosed", err)
	}
	both := Event{Repository: "sf/r", TaskID: "t2", Kind: EventPlanClosed, Actor: FactoryDecider, Via: ViaFactoryd}
	for range 2 {
		if _, err := s.Record(ctx, both); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE task_id = 't2' AND kind = 'plan_closed'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("plan_closed rows of t2: %d (%v), want 1", n, err)
	}
	if got, err := s.PlanClosesToFinish(ctx); err != nil || len(got) != 0 {
		t.Fatalf("PlanClosesToFinish = %v, %v; want none: the close is under a minute old", got, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET at = now() - interval '2 minutes' WHERE task_id = 't1'`); err != nil {
		t.Fatal(err)
	}
	// a failed plan factoryd closed itself (a row without words) is to finish
	// too; a done one with such a row is not
	for _, id := range []string{"t2", "t5"} {
		if _, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: id, Kind: EventPlanClosed, Actor: FactoryDecider, Via: ViaFactoryd}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE events SET at = now() - interval '2 minutes' WHERE kind = 'plan_closed'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PlanClosesToFinish(ctx); err != nil || len(got) != 2 || got["t1"] != "closed by sebastian" || got["t5"] != "" {
		t.Fatalf("PlanClosesToFinish = %v, %v; want t1's words and the failed t5 without any", got, err)
	}
}
