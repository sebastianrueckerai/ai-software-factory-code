package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// A revision's card that continues an old card goes on with its raises. By
// hand: the old card raised one session, 30 minutes of agent time, 200
// lines and its ceiling to 9.00, above the policy's 5.00; the new row holds
// the same four, at the size's own budget, and one card_extended row names
// the old card with pool false. An old card whose ceiling stays at the
// policy's passes no ceiling on.
func TestFileChildrenCarriesTheRaisesOfTheCardItContinues(t *testing.T) {
	ctx := context.Background()
	pinned := fixtureVersion(t)
	ui2 := PlanCard{Key: "ui2", Role: RoleFeature, Title: "UI again", Brief: "Finish the UI.", Size: "small", From: "ui",
		Acceptance: []PlanItem{{Text: "the page renders"}}}
	for name, ceiling := range map[string]float64{"raised ceiling": 9, "policy ceiling": 5} {
		t.Run(name, func(t *testing.T) {
			r, store, _, fj := newPlanRunner(t, testPlan())
			r.cfg.Policy = func() policy.Version { return pinned }
			fj.branches = map[string]string{"factory/t-ui": "c7"}
			if err := store.CreateTask(ctx, Task{ID: "t-ui", Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "t-plan", PlanKey: "ui",
				Repository: "linkshort/linkshort", IssueNumber: 31, SHA: "b0", Status: "closed", PlanRevision: 1, CeilingEUR: ceiling,
				Usage: TaskUsage{RaisedSessions: 1, RaisedTime: 30 * time.Minute, ExtraLines: 200}}); err != nil {
				t.Fatal(err)
			}
			plan := store.tasks["t-plan"]
			plan.IssueNumber, plan.SHA = 40, "p0"
			store.tasks["t-plan"] = plan
			if err := r.timeline.Append(ctx, "t-plan", stepReplan, map[string]any{"origin": ReplanCut, "card": "t-plan", "work": "the half"}); err != nil {
				t.Fatal(err)
			}
			_, repo, err := r.task(ctx, "t-plan")
			if err != nil {
				t.Fatal(err)
			}
			old, err := r.oldCards(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			cards, err := r.fileChildren(ctx, plan, repo, pinned, "spec", "plan #40", Plan{Summary: "s", Cards: []PlanCard{ui2}}, FactoryDecider, true, 2, ReplanPerson, old)
			if err != nil || len(cards) != 1 {
				t.Fatalf("fileChildren = %+v, %v", cards, err)
			}
			got := store.tasks[cards[0].ID]
			wantCeiling := 0.0
			if ceiling > 5 {
				wantCeiling = ceiling
			}
			if got.Usage.RaisedSessions != 1 || got.Usage.RaisedTime != 30*time.Minute || got.Usage.ExtraLines != 200 || got.CeilingEUR != wantCeiling {
				t.Errorf("new card %+v, ceiling %.2f; want 1 session, 30m, 200 lines, ceiling %.2f", got.Usage, got.CeilingEUR, wantCeiling)
			}
			var rows []Event
			for _, e := range store.events {
				if e.Kind == EventCardExtended && e.TaskID == got.ID {
					rows = append(rows, e)
				}
			}
			if len(rows) != 1 || rows[0].Detail["inherited_from"] != "t-ui" || rows[0].Detail["pool"] != false {
				t.Errorf("card_extended rows %+v, want one naming t-ui with pool false", rows)
			}
		})
	}
}
