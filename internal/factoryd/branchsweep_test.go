package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The sweep deletes a fork branch of a card that ended 30 days ago or more,
// and a plan's salvage branch once the plan closed and its card finished
// 30 days ago or more, and no other: not a younger end, a live card, a
// branch with no card row, a plan that did not close, nor a branch that is
// neither a card's nor a plan's salvage.
func TestSweepBranches(t *testing.T) {
	ctx := context.Background()
	day := func(s string) *time.Time {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return &at
	}
	fj := &prForgejo{branches: []string{"factory/t-old", "factory/t-edge", "factory/t-new", "factory/t-live", "factory/t-none", "main", "github/sync-1",
		"salvage/p-old", "salvage/p-young", "salvage/p-open", "salvage/p-none"}}
	r, store, _ := newActivityRunner(t, withStatus(prTask, "running"), fj)
	v := r.cfg.Policy()
	v.Tree.Projects = map[string]policy.ProjectConfig{"repo": {Repository: "sf/repo"}}
	r.cfg.Policy = func() policy.Version { return v }
	for id, card := range map[string]struct {
		status string
		ended  *time.Time
	}{
		"t-old":  {"failed", day("2026-10-30T00:00:00Z")},
		"t-edge": {"closed", day("2026-10-31T00:00:00Z")},
		"t-new":  {"done", day("2026-11-15T00:00:00Z")},
		"t-live": {"running", nil},
	} {
		task := prTask
		task.ID, task.Status, task.FinishedAt = id, card.status, card.ended
		if err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for id, plan := range map[string]struct {
		ended  *time.Time
		closed bool
	}{
		"p-old":   {day("2026-10-30T00:00:00Z"), true},
		"p-young": {day("2026-11-01T00:00:00Z"), true},
		"p-open":  {day("2026-09-30T00:00:00Z"), false},
	} {
		task := prTask
		task.ID, task.Track, task.Status, task.FinishedAt = id, policy.TrackPlan, "done", plan.ended
		if err := store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		store.closed[id] = plan.closed
	}
	if err := r.SweepBranches(ctx, *day("2026-11-30T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	want := []string{"factory-agent/repo factory/t-old", "factory-agent/repo factory/t-edge", "factory-agent/repo salvage/p-old"}
	if !slices.Equal(fj.deleted, want) {
		t.Fatalf("deleted = %q, want %q", fj.deleted, want)
	}
}
