package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A plan card stored with raises keeps them, and one stored with none keeps
// a null ceiling, which reads as the policy's.
func TestCreatePlanCardStoresInheritedRaises(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "inherit")
	base := Task{Kind: "agent", Status: "queued", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackDirect, Source: SourcePlan, Parent: "plan"}
	if err := s.CreateTask(ctx, Task{ID: "plan", Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackPlan}); err != nil {
		t.Fatal(err)
	}
	raised, plain := base, base
	raised.ID, raised.PlanKey, raised.CeilingEUR = "c1", "a", 9
	raised.Usage = TaskUsage{RaisedSessions: 1, RaisedTime: 30 * time.Minute, ExtraLines: 200}
	plain.ID, plain.PlanKey = "c2", "b"
	for _, c := range []Task{raised, plain} {
		if _, err := s.CreatePlanCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := s.Task(ctx, "c1")
	if got.Usage.RaisedSessions != 1 || got.Usage.RaisedTime != 30*time.Minute || got.Usage.ExtraLines != 200 || got.CeilingEUR != 9 {
		t.Errorf("c1 %+v ceiling %.2f", got.Usage, got.CeilingEUR)
	}
	got, _, _ = s.Task(ctx, "c2")
	if got.Usage.RaisedSessions != 0 || got.CeilingEUR != 0 {
		t.Errorf("c2 %+v ceiling %.2f; want none", got.Usage, got.CeilingEUR)
	}
}
