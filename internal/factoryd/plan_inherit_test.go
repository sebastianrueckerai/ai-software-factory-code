package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
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
	for _, tc := range []struct {
		name    string
		ceiling float64
		branch  bool
		dirs    []string
		inherit bool
	}{
		{"raised ceiling", 9, true, nil, true},
		{"policy ceiling", 5, true, nil, true},
		{"the branch is gone", 9, false, nil, true},
		{"a card for a human", 9, true, []string{".forgejo/workflows/pr.yml"}, false},
	} {
		ceiling := tc.ceiling
		t.Run(tc.name, func(t *testing.T) {
			r, store, _, fj := newPlanRunner(t, testPlan())
			r.cfg.Policy = func() policy.Version { return pinned }
			fj.branches = map[string]string{"factory/t-ui": "c7"}
			if !tc.branch {
				fj.branches = map[string]string{}
			}
			ui2 := ui2
			ui2.Directories = tc.dirs
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
			wantCeiling, wantSessions, wantTime, wantLines := 0.0, 0, time.Duration(0), 0
			if tc.inherit {
				wantSessions, wantTime, wantLines = 1, 30*time.Minute, 200
				if ceiling > 5 {
					wantCeiling = ceiling
				}
			}
			if got.Usage.RaisedSessions != wantSessions || got.Usage.RaisedTime != wantTime || got.Usage.ExtraLines != wantLines || got.CeilingEUR != wantCeiling {
				t.Errorf("new card %+v, ceiling %.2f; want %d sessions, %v, %d lines, ceiling %.2f", got.Usage, got.CeilingEUR, wantSessions, wantTime, wantLines, wantCeiling)
			}
			var rows []Event
			for _, e := range store.events {
				if e.Kind == EventCardExtended && e.TaskID == got.ID {
					rows = append(rows, e)
				}
			}
			if !tc.inherit {
				if len(rows) != 0 {
					t.Errorf("card_extended rows %+v, want none for a card for a human", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Detail["inherited_from"] != "t-ui" || rows[0].Detail["pool"] != false || rows[0].Detail["inherited_issue"] != int64(31) {
				t.Errorf("card_extended rows %+v, want one naming t-ui (#31) with pool false", rows)
			}
		})
	}
}

// The digest reads an inherited raise as such: whose it is and the ceiling,
// not a fresh extension of 0.00 EUR.
func TestDigestNamesAnInheritedRaise(t *testing.T) {
	e := Event{Kind: EventCardExtended, Actor: FactoryDecider, Detail: map[string]any{"inherited_from": "t-ui", "inherited_issue": int64(31), "pool": false,
		"sessions": 1, "agent_time_s": int64(1800), "lines": 200, "amount_eur": 0.0, "ceiling_eur": 9.0}}
	steps := eventSteps(redact.New(), Task{}, []Event{e}, nil, nil)
	want := "inherited from #31: 1 session, 30 minutes of agent time, 200 lines, ceiling 9.00 EUR"
	if len(steps) != 1 || steps[0].Kind != "extended" || steps[0].Outcome != want {
		t.Fatalf("steps %+v, want one extended step %q", steps, want)
	}
}
