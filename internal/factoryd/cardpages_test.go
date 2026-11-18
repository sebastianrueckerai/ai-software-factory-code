package factoryd

import (
	"slices"
	"testing"
)

// A case names its request by the request's id however its facts decode
// the number: a resource request's case as its merge and criteria
// siblings do, so an id of a million matches as it reads.
func TestACaseNamesItsRequestByItsID(t *testing.T) {
	q := ApprovalRequest{ID: 1000000, Kind: ApprovalResource}
	for _, id := range []any{int64(1000000), float64(1000000)} {
		if !RequestAnswers(q, Case{Kind: CaseKind(ApprovalResource), Facts: map[string]any{"approval": id}}) {
			t.Errorf("a resource case of approval %v does not name request 1000000", id)
		}
	}
	if RequestAnswers(q, Case{Kind: CaseKind(ApprovalResource), Facts: map[string]any{"approval": float64(7)}}) {
		t.Error("a resource case of approval 7 names request 1000000")
	}
}

// A plan case whose facts name no revision sorts by its state key's
// revision: the newest revision's facts-less question reads first, its
// confirmation after it, and the older revision's question last.
func TestAFactslessPlanCaseSortsInItsKeysRevision(t *testing.T) {
	list := []Case{
		{ID: 9, Kind: CasePlanQuestion, StateKey: "c3@r1", Facts: map[string]any{"revision": 1.0, "index": 1.0}},
		{ID: 8, Kind: CasePlanQuestion, StateKey: "a1@r2"},
		{ID: 7, Kind: CasePlanConfirmation, StateKey: "b2@r2", Facts: map[string]any{"revision": 2.0, "index": 3.0}},
	}
	orderPlanCases(list)
	var got []int64
	for _, k := range list {
		got = append(got, k.ID)
	}
	if want := []int64{8, 7, 9}; !slices.Equal(got, want) {
		t.Errorf("the plan's cases read %v, want %v", got, want)
	}
}

// A plan case whose facts name no revision and no index takes both from
// its state key and its ask's place among its revision's questions, with
// that question's choices; one whose ask matches no question, or whose
// revision is not the request's, stays as it is.
func TestAFactslessPlanCaseTakesItsQuestionsSlot(t *testing.T) {
	r := &Request{ApprovalRequest: ApprovalRequest{Kind: ApprovalPlan}, PlanSpec: PlanSpec{Revision: 2},
		PlanQuestions: []PlanQuestion{{Text: "Which store?", Choices: []string{"Postgres", "SQLite"}, Recommended: "Postgres"}, {Text: "Who may read  the feed?"}}}
	got := withPlanSlot(Case{Kind: CasePlanQuestion, StateKey: "a1@r2", Problem: "Who may read the feed?"}, r)
	if FactInt(got, "revision") != 2 || FactInt(got, "index") != 2 || got.Facts["choices"] != nil {
		t.Errorf("the second question's case holds %v, want revision 2, index 2 and no choices", got.Facts)
	}
	got = withPlanSlot(Case{Kind: CasePlanQuestion, StateKey: "b2@r2", Problem: "Which store?"}, r)
	if choices, _ := got.Facts["choices"].([]any); FactInt(got, "index") != 1 || len(choices) != 2 || choices[0] != "Postgres" || got.Facts["recommended"] != "Postgres" {
		t.Errorf("the first question's case holds %v, want index 1, its two choices and Postgres recommended", got.Facts)
	}
	for name, c := range map[string]Case{
		"an ask of no question": {Kind: CasePlanQuestion, StateKey: "c3@r2", Problem: "Which region?"},
		"another revision":      {Kind: CasePlanQuestion, StateKey: "d4@r1", Problem: "Which store?"},
	} {
		if got := withPlanSlot(c, r); got.Facts != nil {
			t.Errorf("%s: facts = %v, want none", name, got.Facts)
		}
	}
}
