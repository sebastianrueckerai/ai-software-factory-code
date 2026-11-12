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
	ask := CaseAsk{Detail: map[string]any{"recommendation": "handover: the review's two findings are real; a fresh session with the note fixes both", "recommended_by": "the factory agent"}}
	got := CaseFace(c, steps, CaseFacts{}, ask, nil)
	if got.Ask != c.Problem {
		t.Errorf("Ask = %q, want the problem", got.Ask)
	}
	if got.Money != "1.48 of 2.00 EUR spent; the next step needs 0.50 EUR for one more session" {
		t.Errorf("Money = %q", got.Money)
	}
	if !strings.HasSuffix(got.Why, "two findings stand: f1 at store.go:672 and f2. The second is older.") {
		t.Errorf("Why = %q, want the found cut after its second sentence", got.Why)
	}
	if got.Recommendation != ask.Detail["recommendation"] || got.Source != "the factory agent" {
		t.Errorf("Recommendation = %q by %q, want the ask's with its writer", got.Recommendation, got.Source)
	}
}

func TestCaseFaceBudgetFloorOnAGreenHeadAsksTheMoney(t *testing.T) {
	c := Case{Kind: CaseBudgetFloor, Problem: "The budget is short.", Money: &CaseMoney{BudgetEUR: 2, SpentEUR: 1.9, NeedEUR: 0.4, Buys: "a session"}}
	line := "1.90 of 2.00 EUR spent; the next step needs 0.40 EUR for a session"
	if got := CaseFace(c, nil, CaseFacts{Checks: "green"}, CaseAsk{}, nil); got.Ask != line || got.Money != "" {
		t.Errorf("green head: Ask = %q, Money = %q; want the money line as the ask", got.Ask, got.Money)
	}
	if got := CaseFace(c, nil, CaseFacts{Checks: "green", Findings: []string{"f1"}}, CaseAsk{}, nil); got.Ask != c.Problem || got.Money != line {
		t.Errorf("with a finding: Ask = %q, Money = %q; want the problem over the money line", got.Ask, got.Money)
	}
}

func TestCaseFaceWithoutAKindReasonSaysTheKind(t *testing.T) {
	got := CaseFace(Case{Kind: CaseOther, Problem: "p"}, nil, CaseFacts{}, CaseAsk{}, nil)
	if got.Recommendation != "" || got.Why != "The case is the open problem." {
		t.Errorf("Recommendation = %q, Why = %q", got.Recommendation, got.Why)
	}
}

func TestCaseFaceTakesTheAsksOffersInfoAndTheLastAnswer(t *testing.T) {
	at := time.Date(2026, 11, 1, 21, 40, 0, 0, time.UTC)
	ask := CaseAsk{Detail: map[string]any{"info": "What each answer does.",
		"offers":      []any{map[string]any{"value": "park", "label": "Park it"}},
		"preselected": map[string]any{"action": "handover"}}}
	got := CaseFace(Case{Kind: CaseOther, Problem: "p"}, nil, CaseFacts{}, ask, &at)
	if len(got.Choices) != 1 || got.Choices[0] != (QuestionChoice{Value: "park", Label: "Park it"}) || got.Preselected != "handover" {
		t.Errorf("Choices = %v, Preselected = %q", got.Choices, got.Preselected)
	}
	if got.Info != "What each answer does.\n\nLast answer processed: 2026-11-01 21:40 UTC" || got.LastAnswerAt != &at {
		t.Errorf("Info = %q", got.Info)
	}
}

func TestCaseFaceBudgetFloorOnARedHeadKeepsTheProblem(t *testing.T) {
	c := Case{Kind: CaseBudgetFloor, Problem: "The budget is short.", Money: &CaseMoney{BudgetEUR: 2, SpentEUR: 1.9}}
	if got := CaseFace(c, nil, CaseFacts{Checks: "red"}, CaseAsk{}, nil); got.Ask != c.Problem {
		t.Errorf("red head: Ask = %q, want the problem", got.Ask)
	}
	c.Kind = CaseSessionCap
	if got := CaseFace(c, nil, CaseFacts{Checks: "green"}, CaseAsk{}, nil); got.Ask != c.Problem || got.Money == "" {
		t.Errorf("green session cap with money: Ask = %q, Money = %q", got.Ask, got.Money)
	}
}

// The brief's background is the face's info, over the ask's; a rung's
// recommended action keeps its name and writer while the grounds are the
// brief's words; with no rung's recommendation part 4 stays empty, whatever
// the writer answered, and the factory's own template recommendation stays.
func TestCaseFaceTakesTheBriefsBackgroundAndGrounds(t *testing.T) {
	handedUp := []CaseStep{{Rung: 2, Outcome: StepHandedUp, Decision: "handover", Grounds: "terse grounds"}}
	c := Case{Kind: CaseOther, Problem: "p", Brief: "The card set out to add a retry.", BriefDetail: "A fresh session fixes both findings."}
	ask := CaseAsk{Detail: map[string]any{"info": "the digest's render", "recommendation": "handover: terse grounds", "recommended_by": "the factory agent"}}
	got := CaseFace(c, handedUp, CaseFacts{}, ask, nil)
	if got.Info != "The card set out to add a retry." || got.Recommendation != "handover: A fresh session fixes both findings." || got.Source != "the factory agent" {
		t.Errorf("Info = %q, Recommendation = %q by %q", got.Info, got.Recommendation, got.Source)
	}
	if got := CaseFace(c, nil, CaseFacts{}, CaseAsk{Detail: map[string]any{"info": "x"}}, nil); got.Recommendation != "" || got.Source != "" {
		t.Errorf("with no rung recommendation: Recommendation = %q by %q, want none", got.Recommendation, got.Source)
	}
	template := CaseAsk{Detail: map[string]any{"recommendation": "Go on: the last review approved the head", "recommended_by": "the factory's"}}
	if got := CaseFace(c, nil, CaseFacts{}, template, nil); got.Recommendation != "Go on: the last review approved the head" {
		t.Errorf("the template's Recommendation = %q, want it kept", got.Recommendation)
	}
	c.Brief = ""
	if got := CaseFace(c, handedUp, CaseFacts{}, ask, nil); got.Info != "the digest's render" {
		t.Errorf("without a brief Info = %q, want the ask's", got.Info)
	}
}

// A newer step that handed up with no decision, or with the human's, does
// not name the action: the newest decision of a rung does.
func TestCaseFaceNamesTheNewestRungDecision(t *testing.T) {
	steps := []CaseStep{
		{Rung: 2, Outcome: StepHandedUp, Decision: "handover", Grounds: "g"},
		{Rung: 1, Outcome: StepHandedUp, Decision: CheckHuman},
		{Rung: 1, Outcome: StepHandedUp},
	}
	c := Case{Kind: CaseOther, Problem: "p", BriefDetail: "plain grounds"}
	ask := CaseAsk{Detail: map[string]any{"recommendation": "handover: g", "recommended_by": "the factory agent"}}
	if got := CaseFace(c, steps, CaseFacts{}, ask, nil); got.Recommendation != "handover: plain grounds" {
		t.Errorf("Recommendation = %q, want the handover's action with the brief's grounds", got.Recommendation)
	}
}

// A merge approval's why names the risk class the merge gate gave the
// change, from the case's facts; one with no class says the kind.
func TestCaseFaceOfAMergeApprovalNamesTheRiskClass(t *testing.T) {
	c := Case{Kind: CaseMergeApproval, Problem: "Pull request #9 waits for a merge approval", Facts: map[string]any{"class": "migration"}}
	if got, want := CaseFace(c, nil, CaseFacts{}, CaseAsk{}, nil).Why, "The merge gate classes the change migration: a human decides its merge."; got != want {
		t.Errorf("Why = %q, want %q", got, want)
	}
	c.Facts = nil
	if got, want := CaseFace(c, nil, CaseFacts{}, CaseAsk{}, nil).Why, "The case is the merge approval."; got != want {
		t.Errorf("Why with no class = %q, want %q", got, want)
	}
}

func TestCaseFaceOfAMergeApprovalNamesTheReviewThatDidNotRun(t *testing.T) {
	line := "The model review did not run: the month cap is spent."
	c := Case{Kind: CaseMergeApproval, Problem: "Pull request #9 waits for a merge approval", Facts: map[string]any{"class": "normal", "review_not_run": line}}
	if got, want := CaseFace(c, nil, CaseFacts{}, CaseAsk{}, nil).Why, "The merge gate classes the change normal: a human decides its merge. "+line; got != want {
		t.Errorf("Why = %q, want %q", got, want)
	}
}

func TestCaseFaceOfADependencyWaitNamesThePlansOwnWait(t *testing.T) {
	since := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	c := Case{Kind: CaseMonitorFlag, Problem: "p", Facts: map[string]any{"wait": "dependency"}}
	f := CaseFacts{Dependencies: []DepFact{{Issue: 31, Status: "waiting-human", Plan: true, Ask: "Confirm the plan?", Since: &since}}}
	want := `The card waits on plan #31, which waits on a human to answer "Confirm the plan?" since 2026-11-01 08:00 UTC.`
	if got := CaseFace(c, nil, f, CaseAsk{}, nil).Why; !strings.HasSuffix(got, want) {
		t.Errorf("Why = %q, want it to end with %q", got, want)
	}
	f.Dependencies = []DepFact{{Issue: 5, Status: "running", Since: &since}}
	want = "The card waits on card #5, which is running since 2026-11-01 08:00 UTC."
	if got := CaseFace(c, nil, f, CaseAsk{}, nil).Why; !strings.HasSuffix(got, want) {
		t.Errorf("Why = %q, want it to end with %q", got, want)
	}
	c.Facts["wait"] = "gate"
	if got := CaseFace(c, nil, f, CaseAsk{}, nil).Why; strings.Contains(got, "waits on card") {
		t.Errorf("Why = %q, want no dependency sentence for a gate wait", got)
	}
}
