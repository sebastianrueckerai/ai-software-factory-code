package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// The plan staleness column round-trips through Postgres (piece 10): the
// migration's own backfill statement derives each card's revision from
// the newest plan revision created before its first session, the stamp
// lands only while the card never started, and the brief update lands.
func TestPGStorePlanStaleness(t *testing.T) {
	dsn := schematest.DSN(t, "pg")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.DB().Close()
	if _, err := s.DB().ExecContext(ctx, `TRUNCATE tasks, plan_revisions, sessions CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seed := Task{Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Track: "plan"}
	parent := seed
	parent.ID, parent.Status, parent.IssueNumber = "t-plan", "done", 20
	if err := s.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	mid, last, fresh := seed, seed, seed
	mid.ID, mid.Status, mid.IssueNumber, mid.Source, mid.Parent, mid.PlanKey = "t-mid", "queued", 21, SourcePlan, "t-plan", "foundation"
	last.ID, last.Status, last.IssueNumber, last.Source, last.Parent, last.PlanKey = "t-last", "queued", 22, SourcePlan, "t-plan", "links-api"
	fresh.ID, fresh.Status, fresh.IssueNumber, fresh.Source, fresh.Parent, fresh.PlanKey = "t-fresh", "queued", 23, SourcePlan, "t-plan", "redirects"
	mid.Track, last.Track, fresh.Track = "direct", "direct", "direct"
	for _, task := range []Task{mid, last, fresh} {
		if err := s.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"one"}`), "s1"); err != nil {
		t.Fatal(err)
	}
	// t-mid's first session runs against revision 1, before the second
	// revision lands; t-last's runs after it
	if err := s.InsertSession(ctx, Session{ID: "s-mid", TaskID: "t-mid", Model: "factory/big"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "s2"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSession(ctx, Session{ID: "s-last", TaskID: "t-last", Model: "factory/big"}); err != nil {
		t.Fatal(err)
	}

	// the backfill, run again on this data, is the same derivation the
	// migration itself made
	file, err := os.ReadFile("../../schema/migrations/factoryd/20261020100000_plan_staleness.sql")
	if err != nil {
		t.Fatal(err)
	}
	from := bytes.Index(file, []byte("UPDATE tasks"))
	to := from + bytes.Index(file[from:], []byte(";\n")) + 1
	if _, err := s.DB().ExecContext(ctx, string(file[from:to])); err != nil {
		t.Fatalf("the backfill: %v", err)
	}
	if got, _, _ := s.Task(ctx, "t-mid"); got.PlanRevision != 1 {
		t.Fatalf("the card that ran between the revisions = %d, want 1", got.PlanRevision)
	}
	if got, _, _ := s.Task(ctx, "t-last"); got.PlanRevision != 2 {
		t.Fatalf("the card that ran after revision 2 = %d, want 2", got.PlanRevision)
	}
	// a card with no session stays unstamped: its first session stamps it
	if got, _, _ := s.Task(ctx, "t-fresh"); got.PlanRevision != 0 {
		t.Fatalf("a card that never ran = %d, want 0", got.PlanRevision)
	}

	// the stamp lands only while the card never started
	if err := s.SetTaskPlanRevision(ctx, "t-fresh", 3); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-fresh"); got.PlanRevision != 3 {
		t.Fatalf("the stamp: %d, want 3", got.PlanRevision)
	}
	if err := s.SetTaskPlanRevision(ctx, "t-fresh", 4); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-fresh"); got.PlanRevision != 3 {
		t.Fatalf("a second stamp moved the card to %d, want it pinned at 3", got.PlanRevision)
	}
	if err := s.SetTaskPlanRevision(ctx, "t-mid", 4); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-mid"); got.PlanRevision != 1 {
		t.Fatalf("the backfill's stamp moved: %d, want it pinned at 1", got.PlanRevision)
	}
	if err := s.UpdatePlanCardBrief(ctx, "t-fresh", Task{Objective: "Links API\n\nThe amended brief.", Image: "browser", Rung: RungFrontier, Size: "large"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Task(ctx, "t-fresh"); got.Objective != "Links API\n\nThe amended brief." || got.Image != "browser" || got.Rung != RungFrontier || got.Size != "large" {
		t.Fatalf("the brief = %q, image %q, rung %q, size %q", got.Objective, got.Image, got.Rung, got.Size)
	}
}

// Requires a disposable Postgres. A plan's human card, filed as
// fileChildren files a workflow card, holds its dependents until its Done,
// wakes them then, counts in the plan's cards and ends the plan's delivery
// check only when done.
func TestPGStorePlanHumanChild(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plan_human_child"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := Task{Kind: "agent", Repository: "sf/r", BudgetEUR: 1}
	plan := base
	plan.ID, plan.Status, plan.Track, plan.IssueNumber = "t-plan", "running", "plan", 1
	if err := s.CreateTask(ctx, plan); err != nil {
		t.Fatal(err)
	}
	human := Task{ID: "t-ci", Kind: "human", Status: "waiting-human", Repository: "sf/r", BudgetEUR: 1, Source: SourcePlan, Parent: "t-plan", PlanKey: "ci", IssueNumber: 2}
	app := base
	app.ID, app.Status, app.Track, app.Source, app.Parent, app.PlanKey, app.IssueNumber = "t-app", "waiting-deps", "direct", SourcePlan, "t-plan", "app", 3
	for _, c := range []Task{human, app} {
		if _, err := s.CreatePlanCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, "t-app", []string{"t-ci"}); err != nil {
		t.Fatal(err)
	}
	if cards, err := s.PlanCards(ctx, "t-plan"); err != nil || len(cards) != 2 {
		t.Fatalf("PlanCards = %d, %v; want both cards", len(cards), err)
	}
	if open, _ := s.OpenDependencies(ctx, "t-app"); len(open) != 1 || open[0].Status != "waiting-human" {
		t.Fatalf("t-app waits on %+v, want the human's card waiting", open)
	}
	if err := s.TransitionTask(ctx, "t-app", []string{"waiting-deps"}, "done"); err != nil {
		t.Fatal(err)
	}
	if ok, open, err := PlanDelivered(ctx, s, "t-plan"); err != nil || ok || open.ID != "t-ci" {
		t.Fatalf("PlanDelivered before Done = %v, %q, %v; want false on t-ci", ok, open.ID, err)
	}
	if err := s.TransitionTask(ctx, "t-ci", []string{"waiting-human"}, "done"); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenDependencies(ctx, "t-app"); len(open) != 0 {
		t.Fatalf("t-app waits on %+v after Done", open)
	}
	if ok, _, err := PlanDelivered(ctx, s, "t-plan"); err != nil || !ok {
		t.Fatalf("PlanDelivered after Done = %v, %v; want true", ok, err)
	}
}

// Requires a disposable Postgres. A plan's cards done, merged and closed
// deliver it, the closed one with no successor row; a failed card does
// not, nor one still running, and each is named; a closed card cut into a
// running successor stands as the successor.
func TestPGStorePlanDeliveredTakesAClosedCard(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plan_delivered_closed"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "done", Repository: "sf/r", BudgetEUR: 1, Track: "plan", IssueNumber: 1}
	if err := s.CreateTask(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct{ id, status string }{{"t-done", "done"}, {"t-merged", "merged"}, {"t-closed", "closed"}} {
		card := Task{ID: c.id, Kind: "agent", Status: c.status, Repository: "sf/r", BudgetEUR: 1, Track: "direct", Source: SourcePlan,
			Parent: "t-plan", PlanKey: c.id, IssueNumber: int64(i + 2)}
		if _, err := s.CreatePlanCard(ctx, card); err != nil {
			t.Fatal(err)
		}
	}
	if ok, open, err := PlanDelivered(ctx, s, "t-plan"); err != nil || !ok {
		t.Fatalf("PlanDelivered over done, merged and closed = %v, %q, %v; want true", ok, open.ID, err)
	}
	last := Task{ID: "t-last", Kind: "agent", Status: "failed", Repository: "sf/r", BudgetEUR: 1, Track: "direct", Source: SourcePlan,
		Parent: "t-plan", PlanKey: "last", IssueNumber: 5}
	if _, err := s.CreatePlanCard(ctx, last); err != nil {
		t.Fatal(err)
	}
	if ok, open, err := PlanDelivered(ctx, s, "t-plan"); err != nil || ok || open.ID != "t-last" || open.Status != "failed" {
		t.Fatalf("PlanDelivered with a failed card = %v, %q %q, %v; want false on t-last failed", ok, open.ID, open.Status, err)
	}
	if err := s.TransitionTask(ctx, "t-last", []string{"failed"}, "running"); err != nil {
		t.Fatal(err)
	}
	if ok, open, err := PlanDelivered(ctx, s, "t-plan"); err != nil || ok || open.ID != "t-last" {
		t.Fatalf("PlanDelivered with a running card = %v, %q, %v; want false on t-last", ok, open.ID, err)
	}
	if err := s.TransitionTask(ctx, "t-last", []string{"running"}, "closed"); err != nil {
		t.Fatal(err)
	}
	succ := Task{ID: SuccessorID("t-last"), Kind: "agent", Status: "running", Repository: "sf/r", BudgetEUR: 1, Track: "direct", Source: SourceSuccessor,
		Parent: "t-plan", PlanKey: "last", IssueNumber: 6}
	if err := s.CreateTask(ctx, succ); err != nil {
		t.Fatal(err)
	}
	if ok, open, err := PlanDelivered(ctx, s, "t-plan"); err != nil || ok || open.ID != succ.ID {
		t.Fatalf("PlanDelivered with a closed card cut into a running successor = %v, %q, %v; want false on %s", ok, open.ID, err, succ.ID)
	}
}
