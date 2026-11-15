package factoryd

import (
	"context"
	"strings"
	"testing"
)

// suitelessPlan is the suiteless hold-out's plan: a kind test criterion
// that names a test's name but no suite.
func suitelessPlan() Plan {
	return Plan{Summary: "s", Criteria: []PlanCriterion{{ID: "door_open", Kind: CriterionTest, Claim: "a door opens for its key",
		Inputs: "POST /doors/1/open with key 7", Expected: "200", Name: "TestDoorOpen"}}}
}

// filingProofPlan is a valid plan of testShape whose one kind test
// criterion a card of its directories carries: the suiteless hold-out
// blanks its suite and reads the filing's shape finding.
func filingProofPlan() Plan {
	return Plan{Spec: &Spec{Outcome: "A door opens for its key.", Glossary: []string{"door: a resource its key opens"}},
		Summary: "The foundation, then the door, then the acceptance tests.", Criteria: []PlanCriterion{
			{ID: "door_open", Kind: CriterionTest, Claim: "a door opens for its key", Inputs: "POST /doors/1/open with key 7", Expected: "200", Suite: "internal/doors", Name: "TestDoorOpen"},
		}, Cards: []PlanCard{
			{Key: "foundation", Role: RoleFoundation, Title: "Foundation: layout, contract, schema", Brief: "Lay out cmd/server, the store and one end-to-end test.",
				Directories: []string{"cmd/server", "internal/"}, Acceptance: []PlanItem{{Text: "go test ./... passes with TestEndToEndHealth"}}, Size: "medium"},
			{Key: "doors", Role: RoleFeature, Title: "Doors API", Brief: "Open a door for its key.",
				Directories: []string{"internal/doors"}, Acceptance: []PlanItem{{Criterion: "door_open", Text: "POST /doors/1/open answers 200"}}, Size: "small"},
			{Key: "integration", Role: RoleIntegration, Title: "Integration: acceptance tests", Brief: "Turn the criteria into acceptance/ tests.",
				Directories: []string{"acceptance/"}, Acceptance: []PlanItem{{Criterion: "door_open", Text: "TestDoorOpen in internal/doors: POST /doors/1/open with key 7: 200"}}, Size: "medium"},
		}}
}

// A kind test criterion that names no suite still counts as a proof due
// with no proof row, leaving proofsPending true, and no finding names a
// missing test for it.
func TestProofSuitelessTestStillOwesProver(t *testing.T) {
	ctx := context.Background()
	p := suitelessPlan()
	r, _, store := newProverRunner(t, planTask(), newFakeKube(), &fakeObjectStore{})
	seedProver(t, store, p)
	rows, err := store.PlanProofs(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	if due := proofsDue(p, rows); len(due) != 1 || due[0] != (proofDue{id: "door_open", kind: CriterionTest}) {
		t.Fatalf("proofs due = %+v, want door_open alone", due)
	}
	if pending, err := r.proofsPending(ctx, proverTask(), rows, p); err != nil || !pending {
		t.Fatalf("proofsPending = %v, %v; want the prover's wait for the suiteless criterion", pending, err)
	}

	// filing the plan says the criterion's shape is incomplete, never that
	// its test is missing or uncarried
	filed := filingProofPlan()
	filed.Criteria[0].Suite = ""
	if got := validatePlan(fencePlan(filed), testShape, testLimits, nil); len(got) != 1 ||
		!strings.Contains(got[0], "kind test names its claim, its inputs, its expected numbers, its suite and its test's name") {
		t.Fatalf("a test criterion that names no suite = %q, want the one shape finding", got)
	}
}
