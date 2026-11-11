package factoryd

import (
	"strings"
	"testing"
	"time"
)

func TestCaseFaceReadsTheLadder(t *testing.T) {
	c := Case{Kind: CaseSessionCap, Problem: "#1 used its 3 sessions, and 2 findings are open.",
		Money: &CaseMoney{Kind: "session_cap", BudgetEUR: 2, SpentEUR: 1.48, NeedEUR: 0.5, Buys: "one more session"}}
	steps := []CaseStep{
		{Rung: 1, Outcome: StepHandedUp, Found: "two findings stand: f1 at store.go:672 and f2. The second is older. A third sentence.", Decision: "continue", Grounds: "rung one grounds"},
		{Rung: 2, Outcome: StepHandedUp, Decision: "handover", Grounds: "the review's two findings are real; a fresh session with the note fixes both"},
	}
	got := CaseFace(c, steps, CaseFacts{}, CardDigest{})
	if got.Ask != c.Problem {
		t.Errorf("Ask = %q, want the problem", got.Ask)
	}
	if got.Money != "1.48 of 2.00 EUR spent; the next step needs 0.50 EUR for one more session" {
		t.Errorf("Money = %q", got.Money)
	}
	if !strings.HasSuffix(got.Why, "two findings stand: f1 at store.go:672 and f2. The second is older.") {
		t.Errorf("Why = %q, want the found cut after its second sentence", got.Why)
	}
	if got.Recommendation != "handover: the review's two findings are real; a fresh session with the note fixes both" || got.Source != "the factory agent" {
		t.Errorf("Recommendation = %q from %q", got.Recommendation, got.Source)
	}
}

func TestCaseFaceBudgetFloorOnAGreenHeadAsksTheMoney(t *testing.T) {
	c := Case{Kind: CaseBudgetFloor, Problem: "The budget is short.", Money: &CaseMoney{BudgetEUR: 2, SpentEUR: 1.9, NeedEUR: 0.4, Buys: "a session"}}
	line := "1.90 of 2.00 EUR spent; the next step needs 0.40 EUR for a session"
	if got := CaseFace(c, nil, CaseFacts{Checks: "green"}, CardDigest{}); got.Ask != line || got.Money != "" {
		t.Errorf("green head: Ask = %q, Money = %q; want the money line as the ask", got.Ask, got.Money)
	}
	if got := CaseFace(c, nil, CaseFacts{Checks: "green", Findings: []string{"f1"}}, CardDigest{}); got.Ask != c.Problem || got.Money != line {
		t.Errorf("with a finding: Ask = %q, Money = %q; want the problem over the money line", got.Ask, got.Money)
	}
}

func TestCaseFaceWithoutADecisionRecommendsNothing(t *testing.T) {
	steps := []CaseStep{{Rung: 2, Outcome: StepHandedUp, Decision: CheckHuman, Grounds: "no grounds"}, {Rung: 1, Outcome: StepHandedUp}}
	got := CaseFace(Case{Kind: CaseOther, Problem: "p"}, steps, CaseFacts{}, CardDigest{})
	if got.Recommendation != "" || got.Source != "" {
		t.Errorf("Recommendation = %q from %q, want none", got.Recommendation, got.Source)
	}
	if got.Why != "The case is the open problem." {
		t.Errorf("Why = %q", got.Why)
	}
}

func TestCaseFaceTakesTheRecordedOffersAndLastAnswer(t *testing.T) {
	at := time.Date(2026, 11, 1, 21, 40, 0, 0, time.UTC)
	c := Case{Kind: CaseOther, Problem: "p", Facts: map[string]any{
		"offers":      []any{map[string]any{"value": "park", "label": "Park it"}},
		"preselected": map[string]any{"action": "handover"},
	}}
	d := CardDigest{Head: DigestHead{Issue: 7}, cases: []Case{{Status: CaseResolved, ResolvedAt: &at}}}
	got := CaseFace(c, nil, CaseFacts{}, d)
	if len(got.Choices) != 1 || got.Choices[0] != (QuestionChoice{Value: "park", Label: "Park it"}) || got.Preselected != "handover" {
		t.Errorf("Choices = %v, Preselected = %q", got.Choices, got.Preselected)
	}
	if !strings.HasSuffix(got.Info, "Last answer processed: 2026-11-01 21:40 UTC") || got.LastAnswerAt == nil {
		t.Errorf("Info = %q", got.Info)
	}
}
