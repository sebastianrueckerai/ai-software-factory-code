package factoryd

import (
	"strings"
	"testing"
)

// redAtHeadPlan is the red-at-head hold-out's fixture plan: one carried
// test criterion and no plan_proofs rows.
func redAtHeadPlan() Plan {
	return Plan{Criteria: []PlanCriterion{{ID: "door_open", Kind: CriterionTest, Claim: "a door opens for its key",
		Inputs: "POST /doors/1/open with key 7", Expected: "200", Suite: "internal/doors", Name: "TestDoorOpen"}}}
}

// A carried test that is present in the head's tree but failing leaves its
// criterion unmet with a finding naming the test by name and suite and the
// failing run as its evidence - no missing-test wording, no card line, no
// escalation.
func TestProofCarriedTestRedAtHead(t *testing.T) {
	p := redAtHeadPlan()
	runs := gateRuns(p, nil)
	if len(runs) != 1 || runs[0].held != "" {
		t.Fatalf("runs = %+v, want the one tree run, never held", runs)
	}
	const failure = "Failed\n=== RUN   TestDoorOpen\n    doors_test.go:9: POST /doors/1/open answers 500, want 200\n--- FAIL: TestDoorOpen (0.00s)"
	rep := criteriaReport(p, "deadbeef0a", runs, map[string]gateResult{"door_open": {
		outcome: proofOutcome{Outcome: proofFailed, Message: failure}, output: failure, ran: true}})
	if rep.met != 0 || len(rep.findings) != 1 || len(rep.card) != 0 {
		t.Fatalf("the named test present but failing: %d met, findings %q, card %q; want 0 met, one finding, no card line", rep.met, rep.findings, rep.card)
	}
	finding := rep.findings[0]
	for _, want := range []string{"TestDoorOpen in ./internal/doors", "failed", "500, want 200"} {
		if !strings.Contains(finding, want) {
			t.Errorf("the finding carries no %q: %q", want, finding)
		}
	}
	if strings.Contains(finding, "no test TestDoorOpen") {
		t.Errorf("the finding reads a missing test where the test ran and failed: %q", finding)
	}
	rows := gateRows(rep.proofs)
	if len(rows) != 1 || rows[0].Verdict != "unmet" || !strings.Contains(rows[0].Evidence, "TestDoorOpen in ./internal/doors failed") {
		t.Fatalf("rows = %+v, want door_open unmet with the failing run as its evidence", rows)
	}
}
