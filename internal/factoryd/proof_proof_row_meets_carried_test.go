package factoryd

import (
	"testing"
)

// proofRowPlan is the proof-row hold-out's fixture plan: one carried test
// criterion whose named test the head's tree does not hold.
func proofRowPlan() Plan {
	return Plan{Criteria: []PlanCriterion{{ID: "door_open", Kind: CriterionTest, Claim: "a door opens for its key",
		Inputs: "POST /doors/1/open with key 7", Expected: "200", Suite: "internal/doors", Name: "TestDoorOpen"}}}
}

// A proof row that holds a carried test criterion meets it without the
// gate running the named test, even when the test is absent from the
// head's tree: the row's own test runs blind from its file.
func TestProofProofRowMeetsCarriedTest(t *testing.T) {
	p := proofRowPlan()
	row := PlanProof{Idx: 1, Origin: ProofOriginCriterion, Criterion: "door_open", ID: "door_open", State: ProofStored,
		Suite: "proofs/door", Name: "TestDoorHeld", Path: "proofs/door/door_test.go", Code: "package door\n"}
	runs := gateRuns(p, []PlanProof{row})
	if len(runs) != 1 || runs[0].held != "" {
		t.Fatalf("runs = %+v, want the one blind run of the row, never held", runs)
	}
	run := runs[0]
	if run.suite != "./proofs/door" || run.name != "TestDoorHeld" || run.file == nil || run.file.Path != "proofs/door/door_test.go" {
		t.Fatalf("run = %+v, want the row's own test run blind with its file", run)
	}
	if run.suite == "./internal/doors" || run.name == "TestDoorOpen" {
		t.Fatalf("run = %+v, want no run recorded for the named test TestDoorOpen in ./internal/doors", run)
	}
	rep := criteriaReport(p, "c0ffee0a1b", runs, map[string]gateResult{"door_open": {
		outcome: proofOutcome{Outcome: proofPassed, Message: "1 passed"}, output: "PASS", ran: true}})
	if rep.met != 1 || len(rep.findings) != 0 || len(rep.card) != 0 {
		t.Fatalf("the proof row's green run: %d met, findings %q, card %q; want 1 met, no finding, no card line", rep.met, rep.findings, rep.card)
	}
	rows := gateRows(rep.proofs)
	if len(rows) != 1 || rows[0].Verdict != "met" {
		t.Fatalf("rows = %+v, want door_open met", rows)
	}
}
