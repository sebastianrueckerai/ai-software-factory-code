package factoryd

import (
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A merge ends the workflow: Finish runs at the merge with the merge's
// own reason and decides the card's state — merged, its tokens revoked,
// no task_ended until its chain covers it.
func TestAMergeEndsTheWorkflowMerged(t *testing.T) {
	r, hub, store, objects := chainRunner(t, withStatus(mergedCard, "in-review"), policy.ChainModeRun)
	g := newGateHarness(t, autoGates)
	g.finish = r.Finish
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})

	if final := g.run(t); final != "done" || !slices.Equal(g.finals, [][2]string{{"done", "pull request merged by factory-merge on the merge rule auto"}}) {
		t.Fatalf("final = %q, finals = %v; want one Finish with the merge's reason", final, g.finals)
	}
	if g.steps[len(g.steps)-1] != "merge" || slices.Contains(g.states, "merged") {
		t.Fatalf("steps = %v, states = %v; want the merge the last step and no merged move of the workflow's own", g.steps, g.states)
	}
	if got, _, _ := store.Task(t.Context(), "t1"); got.Status != "merged" {
		t.Fatalf("status = %q, want merged", got.Status)
	}
	if !slices.Equal(hub.revoked, []int64{1, 2}) || len(taskEnds(t, objects, "t1")) != 0 {
		t.Fatalf("revoked = %v, task_ended = %q; want the card's tokens revoked and no end", hub.revoked, taskEnds(t, objects, "t1"))
	}
}
