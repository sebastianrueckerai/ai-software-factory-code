package factoryd

import (
	"slices"
	"testing"
	"time"
)

// TestChangelessRoundKeepsPr: a card whose branch carries no change to
// merge keeps its open PR as the plan's criteria-gate trigger, and its
// round is not closed PR-less. A re-cut card in review holding an open
// green PR whose changeless round finds its proofs met at main still
// reaches its merge press: the PR stays open with its empty diff, the card
// stays in review, and it closes only at the merge press.
func TestChangelessRoundKeepsPr(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state.Source = SourcePlan
	changeless := SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "the branch carries no change to merge",
		CheckIn: &CheckInOutcome{Decision: CheckHandover, Handover: "nothing to merge on the branch",
			Record: &CheckInRecord{Moment: CheckInSession}}}
	h.onSessions(changeless, pushed("s2", "c2"))
	// main already holds the card's work: the proofs it cites pass there,
	// which is the changeless fact — the PR's diff is empty
	calls := h.onProofsAtMain(ProofsAtMain{Head: "29b0d33aa", Met: true, Reason: "proofs met at 29b0d33: c1, c3"}, nil)
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v, want the changeless round to keep the card for its merge press, not close it PR-less", h.finals)
	}
	if len(h.sessions) != 2 {
		t.Fatalf("sessions %d, want the round to run on instead of ending PR-less", len(h.sessions))
	}
	if len(*calls) != 1 || (*calls)[0] != "t1" {
		t.Fatalf("proofs at main calls %q, want one on the card", *calls)
	}
	if !slices.Contains(h.states, "in-review") {
		t.Fatalf("states %q, want the card held in review with its PR", h.states)
	}
}
