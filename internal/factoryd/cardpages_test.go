package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
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

// A plan request is answered by the plan cases of its own revision, read
// from the case's facts or its state key: a case of another revision and a
// case that names none answer no request.
func TestAPlanRequestIsAnsweredByItsRevisionsCases(t *testing.T) {
	q := ApprovalRequest{ID: 4, Kind: ApprovalPlan, Spec: []byte(`{"revision":2}`)}
	for _, tc := range []struct {
		name string
		c    Case
		want bool
	}{
		{"a question whose facts name the revision", Case{Kind: CasePlanQuestion, Facts: map[string]any{"revision": 2.0}}, true},
		{"a confirmation whose state key names the revision", Case{Kind: CasePlanConfirmation, StateKey: "b2@r2"}, true},
		{"a question of the revision before", Case{Kind: CasePlanQuestion, Facts: map[string]any{"revision": 1.0}}, false},
		{"a question that names no revision", Case{Kind: CasePlanQuestion, StateKey: "a1"}, false},
	} {
		if got := RequestAnswers(q, tc.c); got != tc.want {
			t.Errorf("%s: RequestAnswers = %v, want %v", tc.name, got, tc.want)
		}
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
// that question's choices; a question's case whose ask matches none asks
// the revision's first open question, and one whose revision is not the
// request's stays as it is.
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
	// a question's case whose ask matches none asks the first open question
	got = withPlanSlot(Case{Kind: CasePlanQuestion, StateKey: "c3@r2", Problem: "Which region?"}, r)
	if choices, _ := got.Facts["choices"].([]any); got.Problem != "Which store?" || FactInt(got, "revision") != 2 || FactInt(got, "index") != 1 || len(choices) != 2 {
		t.Errorf("an ask of no question: the case asks %q with %v, want the first open question, index 1 with its two choices", got.Problem, got.Facts)
	}
	for name, c := range map[string]Case{
		"a confirmation of no question": {Kind: CasePlanConfirmation, StateKey: "c3@r2", Problem: "Does the plan match?"},
		"another revision":              {Kind: CasePlanQuestion, StateKey: "d4@r1", Problem: "Which store?"},
	} {
		if got := withPlanSlot(c, r); got.Facts != nil || got.Problem != c.Problem {
			t.Errorf("%s: the case asks %q with %v, want it as it is", name, got.Problem, got.Facts)
		}
	}
}

// The merge request of the successor that carries the integration card's
// key shows the plan's promises with their standing rows on the
// plan-filed integration card, as the card's own request does, and its
// human promise with no verdict holds the merge.
func TestMergeRowsOfASuccessorOfTheIntegrationCard(t *testing.T) {
	plan, _ := json.Marshal(Plan{Criteria: []PlanCriterion{
		{ID: "c1", Kind: "test", Claim: "a link is stored", Inputs: "POST /api/links", Expected: "201"},
		{ID: "c2", Kind: "human", Claim: "the feature reads well"},
	}, Cards: []PlanCard{{Key: "integration", Role: RoleIntegration}}})
	store := newFakeStore()
	store.plans = map[string][]PlanRevision{"t-plan": {{TaskID: "t-plan", Revision: 1, Plan: plan}}}
	store.tasks["t-int"] = Task{ID: "t-int", Parent: "t-plan", Source: SourcePlan, PlanKey: "integration", Status: "failed"}
	store.criteriaVerdicts = map[string][]CriterionVerdict{"t-int": {{Criterion: "c1", Kind: "test", Verdict: "met", Evidence: "TestLink passed"}}}
	succ := Task{ID: "t-succ", Parent: "t-plan", Source: SourceSuccessor, PlanKey: "integration"}
	rows, unsettled, integration, err := MergeRows(context.Background(), store, succ)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %s %q %q", r.ID, r.Kind, r.Verdict, r.Evidence))
	}
	want := []string{`c1 test "met" "TestLink passed"`, `c2 human "" ""`}
	if err != nil || !slices.Equal(got, want) || !slices.Equal(unsettled, []string{"c2"}) || integration != "t-int" {
		t.Fatalf("the successor's merge rows = %q, unsettled %q on %q (%v); want %q, c2 unsettled on t-int", got, unsettled, integration, err, want)
	}
}
