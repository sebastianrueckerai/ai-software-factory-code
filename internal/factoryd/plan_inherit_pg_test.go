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

// The per-card draw bound restarts on a continuation: the old card's pool
// draws stay its own, and the inherited row, which is no pool draw, adds
// none to the new card's.
func TestAContinuationStartsItsDrawBoundOver(t *testing.T) {
	ctx := context.Background()
	s := cardCaseStore(t, "bound")
	for _, id := range []string{"old", "new"} {
		if err := s.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Objective: "x", Repository: "demo-app/demo-app", Track: policy.TrackDirect}); err != nil {
			t.Fatal(err)
		}
	}
	draw := CaseDraw{Rung: 1, Extend: policy.Draw{Sessions: 2}, CardBound: policy.Draw{Sessions: 5}, Pool: policy.Draw{Sessions: 30, AgentTime: 10 * time.Hour, EUR: 60}, PolicyCeiling: 7,
		Event: Event{Repository: "demo-app/demo-app", TaskID: "old", Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"pool": true, "rung": 1}}}
	if err := s.DrawCard(ctx, "old", draw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, Event{Repository: "demo-app/demo-app", TaskID: "new", Kind: EventCardExtended, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"inherited_from": "old", "pool": false, "sessions": 2, "amount_eur": 0.0}}); err != nil {
		t.Fatal(err)
	}
	old, _ := s.CardDrawn(ctx, "old")
	fresh, _ := s.CardDrawn(ctx, "new")
	if old.Sessions != 2 || !fresh.IsZero() {
		t.Fatalf("old card drew %+v, new card %+v; want 2 sessions and nothing", old, fresh)
	}
}
