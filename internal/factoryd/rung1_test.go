package factoryd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type failingPlanStore struct{ *fakeStore }

func (failingPlanStore) PlanCards(context.Context, string) ([]Task, error) {
	return nil, errors.New("store down")
}

// plan #1344 session-metrics is running with cards #1410 (the card), #1411
// done and #1412 failed, and #1500 stands in no plan.
func siblingRig() digestRig {
	g := newDigestRig(func(c *Task) { c.Source, c.Parent, c.PlanKey = SourcePlan, "t1344", "metrics-a" })
	g.store.tasks["t1344"] = Task{ID: "t1344", Kind: "agent", Repository: "sf/app", IssueNumber: 1344, Track: policy.TrackPlan, PlanName: "session-metrics", Status: "running"}
	g.store.tasks["tb"] = Task{ID: "tb", Kind: "agent", Repository: "sf/app", IssueNumber: 1411, Source: SourcePlan, Parent: "t1344", PlanKey: "metrics-b", Status: "done"}
	g.store.tasks["ta"] = Task{ID: "ta", Kind: "agent", Repository: "sf/app", IssueNumber: 1412, Source: SourcePlan, Parent: "t1344", PlanKey: "metrics-c", Status: "failed"}
	g.store.tasks["t1500"] = Task{ID: "t1500", Kind: "agent", Repository: "sf/app", IssueNumber: 1500, Source: "webhook", Status: "running"}
	if _, err := g.store.AddPlanRevision(context.Background(), "t1344", mustJSON(Plan{}), "r1"); err != nil {
		panic(err)
	}
	return g
}

func TestSiblingsFactsLeaveTheCardOut(t *testing.T) {
	g := siblingRig()
	f, err := g.r.tripFacts(context.Background(), g.store.tasks["t1410"], "")
	if err != nil {
		t.Fatal(err)
	}
	want := []SiblingFact{{Issue: 1411, Key: "metrics-b", Status: "done"}, {Issue: 1412, Key: "metrics-c", Status: "failed"}}
	if f.PlanIssue != 1344 || f.PlanName != "session-metrics" || f.PlanStatus != "running" || !slices.Equal(f.Siblings, want) {
		t.Errorf("plan facts = %d %q %q %+v, want 1344 session-metrics running %+v", f.PlanIssue, f.PlanName, f.PlanStatus, f.Siblings, want)
	}
	d, err := g.r.tripFacts(context.Background(), g.store.tasks["t1500"], "")
	if err != nil || d.PlanIssue != 0 || d.PlanName != "" || d.PlanStatus != "" || d.Siblings != nil {
		t.Errorf("direct card plan facts = %+v, %v, want none", d, err)
	}
	g.r.store = failingPlanStore{g.store}
	f, err = g.r.tripFacts(context.Background(), g.store.tasks["t1410"], "")
	if err != nil || !slices.Contains(f.Partial, "siblings") || f.Siblings != nil {
		t.Errorf("failed read: partial %v siblings %v err %v, want siblings named", f.Partial, f.Siblings, err)
	}
}

func TestRung1StatementReadsCard(t *testing.T) {
	g := siblingRig()
	ctx := context.Background()
	for _, c := range []struct {
		id   string
		want []string
	}{
		{"t1410", []string{"#1410 ·", "Plan #1344 session-metrics, running", "#1411 metrics-b: done", "#1412 metrics-c: failed"}},
		{"t1500", []string{"#1500 ·", "This card is in no plan."}},
	} {
		task := g.store.tasks[c.id]
		f, err := g.r.tripFacts(ctx, task, "")
		if err != nil {
			t.Fatal(err)
		}
		d, err := g.r.CardDigest(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		got := g.r.rungStatement(Case{Kind: CaseGateStop, Problem: "the gate stopped"}, f, d.Render(), nil, 1)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: statement lacks %q:\n%s", c.id, w, got)
			}
		}
		if c.id == "t1500" && strings.Contains(got, "Plan #") {
			t.Errorf("direct card statement names a plan:\n%s", got)
		}
	}
}

func TestRung1StatementOrderMoneyAndRefusals(t *testing.T) {
	f := CaseFacts{Kind: CaseGateStop, BudgetEUR: 5, SpentEUR: 4, LeftEUR: 1, CeilingEUR: 8,
		PoolLeft: policy.Draw{Sessions: 3, EUR: 6}, CardDrawLeft: policy.Draw{Sessions: 1, EUR: 2},
		Head: "h1", PR: 9, PROpen: true, Checks: "green"}
	got := (&Runner{}).rungStatement(Case{Problem: "PROBLEM"}, f, "DIGEST", []CaseStep{{Rung: 0, Actor: "code", Outcome: "handed_up", Decision: "hand_up", Grounds: "G", Found: "F"}}, 1)
	for _, w := range []string{"budget 5.00", "spent 4.00", "left 1.00", "ceiling 8.00", drawText(f.PoolLeft), drawText(f.CardDrawLeft),
		"This card is in no plan.", "- route_approval: not offered: the newest review of the head did not approve it", "- resume"} {
		if !strings.Contains(got, w) {
			t.Errorf("statement lacks %q:\n%s", w, got)
		}
	}
	at := func(s string) int { return strings.Index(got, s) }
	if !(at("PROBLEM") < at("budget 5.00") && at("budget 5.00") < at("DIGEST") && at("DIGEST") < at("This card is in no plan.") &&
		at("This card is in no plan.") < at("handed_up") && at("handed_up") < at("- route_approval")) {
		t.Errorf("statement parts out of order:\n%s", got)
	}
	for _, rung2 := range []string{"- close", "- raise_ceiling"} {
		if strings.Contains(got, rung2) {
			t.Errorf("rung 1 statement lists %q:\n%s", rung2, got)
		}
	}
}

func TestRung1StatementCutsAndScrubs(t *testing.T) {
	found := "clone failed https://user:pass@host/x and Authorization: Bearer abc123 " + strings.Repeat("x", 400)
	got := (&Runner{}).rungStatement(Case{}, CaseFacts{}, "D", []CaseStep{{Rung: 1, Actor: "checkin", Found: found}}, 1)
	for _, bad := range []string{"user:pass", "abc123"} {
		if strings.Contains(got, bad) {
			t.Errorf("statement leaks %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "https://[redacted]@host") || !strings.Contains(got, "Bearer [redacted]") {
		t.Errorf("statement does not show both masked:\n%s", got)
	}
	if strings.Contains(got, strings.Repeat("x", 301)) {
		t.Errorf("found text not cut at 300")
	}
}

func TestHumanOnly(t *testing.T) {
	ci := func(kind string) Case {
		return Case{Kind: CaseCheckinHuman, Facts: map[string]any{"reason_kind": kind}}
	}
	need := func(eur float64) Case { return Case{Kind: CaseBudgetFloor, Money: &CaseMoney{NeedEUR: eur}} }
	for _, c := range []struct {
		name string
		c    Case
		pool float64
		want bool
	}{
		{"plan_question", Case{Kind: CasePlanQuestion}, 0, true},
		{"plan_confirmation", Case{Kind: CasePlanConfirmation}, 0, true},
		{"goal_conflict", ci(CheckGoalConflict), 0, true},
		{"policy_class", ci(CheckPolicyClass), 0, true},
		{"access", ci(CheckAccess), 0, true},
		{"spend above policy", ci(CheckSpend), 0, false},
		{"month_cap", Case{Kind: CaseMonthCap}, 0, true},
		{"project_cap", Case{Kind: CaseProjectCap}, 0, true},
		{"day_cap", Case{Kind: CaseDayCap}, 0, true},
		{"merge_approval", Case{Kind: CaseMergeApproval}, 0, false},
		{"gate_stop", Case{Kind: CaseGateStop}, 0, false},
		{"floor beyond pool", need(5), 4, true},
		{"floor within pool", need(5), 60, false},
		{"floor equal to pool", need(5), 5, false},
	} {
		got, why := humanOnly(c.c, CaseFacts{PoolLeft: policy.Draw{EUR: c.pool}})
		if got != c.want || got == (why == "") {
			t.Errorf("%s: humanOnly = %v %q, want %v with a reason iff true", c.name, got, why, c.want)
		}
	}
}

func TestRung1StatementPlanReadFailed(t *testing.T) {
	g := siblingRig()
	g.r.store = failingPlanStore{g.store}
	f, _ := g.r.tripFacts(context.Background(), g.store.tasks["t1410"], "")
	got := g.r.rungStatement(Case{Kind: CaseGateStop}, f, "D", nil, 1)
	if strings.Contains(got, "no plan") || !strings.Contains(got, "plan and its other cards could not be read") {
		t.Errorf("statement of an unread plan:\n%s", got)
	}
}

func TestRung1StatementWeighsTheCaseKind(t *testing.T) {
	got := (&Runner{}).rungStatement(Case{Kind: CaseMonitorFlag}, CaseFacts{}, "D", nil, 1)
	if !strings.Contains(got, "\n- settle\n") || strings.Contains(got, "\n- reverify\n") {
		t.Errorf("monitor flag statement:\n%s", got)
	}
}

func TestRung1StatementScrubsDigestAndProblem(t *testing.T) {
	secret := "https://user:pass@host and Authorization: Bearer abc123"
	got := (&Runner{}).rungStatement(Case{Problem: "P " + secret}, CaseFacts{}, "D "+secret, nil, 1)
	if strings.Contains(got, "user:pass") || strings.Contains(got, "abc123") {
		t.Errorf("statement leaks:\n%s", got)
	}
}

func TestRung1StatementScrubsBeforeItCuts(t *testing.T) {
	found := strings.Repeat("x", 280) + " https://user:pass@host/x"
	got := (&Runner{}).rungStatement(Case{}, CaseFacts{}, "D", []CaseStep{{Rung: 1, Found: found}}, 1)
	if strings.Contains(got, "user") || strings.Contains(got, "pass") {
		t.Errorf("cut straddles a secret:\n%s", got)
	}
}

func TestHumanOnlyPoolUnread(t *testing.T) {
	got, why := humanOnly(Case{Kind: CaseBudgetFloor, Money: &CaseMoney{NeedEUR: 1}}, CaseFacts{Partial: []string{"pool"}})
	if !got || !strings.Contains(why, "pool could not be read") {
		t.Errorf("humanOnly = %v %q", got, why)
	}
}

func TestRungZeroIgnoresAnUnreadSiblings(t *testing.T) {
	r := &Runner{}
	_, found, _ := r.rungZero(context.Background(), Task{}, Case{}, CaseFacts{Partial: []string{"siblings", "timeline"}}, nil)
	if found != "facts read partly: timeline, so rung 0 took nothing" {
		t.Errorf("found = %q", found)
	}
}

func TestDigestTextScrubsBeforeItCuts(t *testing.T) {
	got := digestText(strings.Repeat("x", 100) + " https://user:pass@host/x")
	if strings.Contains(got, "user") || strings.Contains(got, "pass") {
		t.Errorf("digest text leaks across the cut: %q", got)
	}
}
