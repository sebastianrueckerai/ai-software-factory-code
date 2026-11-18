package pg

import (
	"context"
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A failed
// card outside a plan holds no key and is replaced once by its
// replacement's id: the replacement takes its dependents; a second
// replacement with that id and the replacement of a card that did not
// fail are refused and change nothing.
func TestPGStoreReplacesACardOutsideAPlan(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_retry_direct"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-d", Status: "failed", IssueNumber: 5},
		{ID: "t-w", Status: "waiting-deps", IssueNumber: 6},
		{ID: "t-r", Status: "running", IssueNumber: 7},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind, tk.Source, tk.Track = 1, "sf/r", "agent", "webhook", policy.TrackDirect
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, "t-w", []string{"t-d"}); err != nil {
		t.Fatal(err)
	}
	card := func(id string) Task {
		return Task{ID: id, Status: "queued", Track: policy.TrackDirect, BudgetEUR: 1, Repository: "sf/r", Kind: "agent", Source: "webhook"}
	}
	if ok, err := s.ReplaceCard(ctx, "t-d", "failed", card("t-d2")); !ok || err != nil {
		t.Fatalf("first retry = %v, %v; want done", ok, err)
	}
	if err := s.SetTaskIssue(ctx, "t-d2", 9); err != nil {
		t.Fatal(err)
	}
	for _, again := range []struct{ of, id string }{{"t-d", "t-d2"}, {"t-r", "t-r2"}} {
		if ok, err := s.ReplaceCard(ctx, again.of, "failed", card(again.id)); ok || err != nil {
			t.Fatalf("retry of %s as %s = %v, %v; want refused", again.of, again.id, ok, err)
		}
	}
	if _, found, _ := s.Task(ctx, "t-r2"); found {
		t.Fatal("the refused retry of t-r inserted t-r2")
	}
	if got, _, _ := s.Task(ctx, "t-d2"); got.IssueNumber != 9 || got.Status != "queued" || got.Source != "webhook" || got.Parent != "" || got.PlanKey != "" {
		t.Fatalf("replacement = %s on #%d, source %q, parent %q, key %q; want queued on #9, webhook, no parent, no key",
			got.Status, got.IssueNumber, got.Source, got.Parent, got.PlanKey)
	}
	if d, _, _ := s.Task(ctx, "t-d"); d.Status != "failed" || d.PlanKey != "" {
		t.Fatalf("t-d = %s with key %q, want failed without a key", d.Status, d.PlanKey)
	}
	open, err := s.OpenDependencies(ctx, "t-w")
	if err != nil || !slices.Equal(open, []Dependency{{TaskID: "t-d2", Issue: 9, Status: "queued"}}) {
		t.Fatalf("t-w waits on %+v, %v; want the replacement #9", open, err)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A plan's
// card waiting on a human is handed to an agent card in one transaction:
// the human card lets go of its key, which the agent card holds under the
// key's unique index, with the human card's dependency and dependent; a
// card that does not wait on a human is not replaced as one.
func TestPGStoreHandsAPlansCardOver(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_hand_over"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-plan", Status: "running", IssueNumber: 1, Kind: "agent", Track: policy.TrackPlan, Source: "webhook"},
		{ID: "t-f", Status: "done", IssueNumber: 4, Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "t-plan", PlanKey: "foundation"},
		{ID: "t-h", Status: "waiting-human", IssueNumber: 5, Kind: "human", Source: SourcePlan, Parent: "t-plan", PlanKey: "links-api"},
		{ID: "t-b", Status: "waiting-deps", IssueNumber: 6, Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "t-plan", PlanKey: "redirects"},
	} {
		tk.BudgetEUR, tk.Repository = 1, "sf/r"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	for id, deps := range map[string][]string{"t-h": {"t-f"}, "t-b": {"t-h"}} {
		if err := s.AddTaskDeps(ctx, id, deps); err != nil {
			t.Fatal(err)
		}
	}
	agent := Task{ID: "t-a", Status: "queued", Kind: "agent", Track: policy.TrackDirect, BudgetEUR: 1, Repository: "sf/r", IssueNumber: 9,
		Source: SourcePlan, Parent: "t-plan", PlanKey: "links-api"}
	if ok, err := s.ReplaceCard(ctx, "t-h", "failed", agent); ok || err != nil {
		t.Fatalf("a retry's replace of a card waiting on a human = %v, %v; want refused", ok, err)
	}
	if ok, err := s.ReplaceCard(ctx, "t-h", "waiting-human", agent); !ok || err != nil {
		t.Fatalf("the hand-over = %v, %v; want done", ok, err)
	}
	if h, _, _ := s.Task(ctx, "t-h"); h.Status != "waiting-human" || h.PlanKey != "" {
		t.Fatalf("t-h = %s with key %q, want waiting-human without a key", h.Status, h.PlanKey)
	}
	cards, err := s.PlanCards(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range cards {
		keys = append(keys, c.ID+":"+c.PlanKey)
	}
	if !slices.Equal(keys, []string{"t-a:links-api", "t-b:redirects", "t-f:foundation"}) {
		t.Fatalf("the plan's cards = %q, want the agent card in t-h's place", keys)
	}
	if deps, err := s.Dependencies(ctx, "t-a"); err != nil || len(deps) != 1 || deps[0].TaskID != "t-f" {
		t.Fatalf("t-a depends on %+v, %v; want t-f", deps, err)
	}
	if open, err := s.OpenDependencies(ctx, "t-b"); err != nil || len(open) != 1 || open[0].TaskID != "t-a" {
		t.Fatalf("t-b waits on %+v, %v; want t-a", open, err)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A plan
// child's successor lands with the old card's key, revision and edges in
// one transaction: it depends on the old card's dependency, and the
// sibling that waited on the old card waits on the successor; the old card
// keeps its own row and key.
func TestPGStoreSuccessorTakesOverTheEdges(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_successor_edges"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-plan", Status: "done", IssueNumber: 1, Source: "webhook", Track: policy.TrackPlan},
		{ID: "t-f", Status: "done", IssueNumber: 4, PlanKey: "foundation"},
		{ID: "t-a", Status: "running", IssueNumber: 5, PlanKey: "links"},
		{ID: "t-b", Status: "waiting-deps", IssueNumber: 6, PlanKey: "redirects"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if tk.ID != "t-plan" {
			tk.Source, tk.Parent, tk.Track = SourcePlan, "t-plan", policy.TrackDirect
		}
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, "t-a", []string{"t-f"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, "t-b", []string{"t-a"}); err != nil {
		t.Fatal(err)
	}
	succ := Task{ID: SuccessorID("t-a"), Status: "queued", Track: policy.TrackDirect, BudgetEUR: 1, Repository: "sf/r", Kind: "agent",
		Source: SourceSuccessor, Parent: "t-plan", PlanKey: "links", PlanRevision: 2}
	if err := s.CreateSuccessor(ctx, "t-a", succ); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.Task(ctx, succ.ID); err != nil || got.PlanKey != "links" || got.PlanRevision != 2 {
		t.Fatalf("the successor's row %+v, %v; want key links at revision 2", got, err)
	}
	if err := s.SetTaskIssue(ctx, succ.ID, 9); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Dependencies(ctx, succ.ID); err != nil || !slices.Equal(got, []Dependency{{TaskID: "t-f", Issue: 4, Key: "foundation", Status: "done"}}) {
		t.Fatalf("the successor depends on %+v, %v; want t-f", got, err)
	}
	if got, err := s.OpenDependencies(ctx, "t-b"); err != nil || !slices.Equal(got, []Dependency{{TaskID: succ.ID, Issue: 9, Key: "links", Status: "queued"}}) {
		t.Fatalf("t-b waits on %+v, %v; want the successor #9", got, err)
	}
	if got, err := s.Dependents(ctx, "t-a"); err != nil || len(got) != 0 {
		t.Fatalf("t-a keeps the dependents %+v, %v; want none", got, err)
	}
	if a, _, _ := s.Task(ctx, "t-a"); a.Status != "running" || a.PlanKey != "links" {
		t.Fatalf("t-a = %s with key %q, want running with its key", a.Status, a.PlanKey)
	}
	if err := s.CreateSuccessor(ctx, "t-a", succ); err == nil {
		t.Fatal("a second insert of the successor's id passed")
	}
	if got, _ := s.OpenDependencies(ctx, "t-b"); len(got) != 1 {
		t.Fatalf("t-b after the refused second insert waits on %+v, want the successor alone", got)
	}
}
