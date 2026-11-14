package factoryd

import (
	"slices"
	"strings"
	"testing"
)

// [replan_refused_past_cut] Replan is offered only while the ask offers the
// cut: a session moment whose cut has lapsed refuses the answer replan at
// its closed enum — the one refusal, no decision of its own recorded — and
// the moment stands answered only with one of the names it offers.
func TestProofReplanRefusedPastCut(t *testing.T) {
	requireStoredDecisionsOnTheRungsNames(t)
	ask := &CheckInAsk{Moment: CheckInSession, Trip: &TripReport{Detector: "repetition"}}
	if slices.Contains(momentDecisions(ask.Moment, ask.Replan), CheckReplan) {
		t.Fatal("a session moment whose ask offers no cut still offers replan")
	}
	refusals := 0
	if _, err := parseCheckinAnswer(`{"decision":"replan","reason":"cut the card's rest"}`, ask); err == nil {
		t.Fatal("a replan past the cut was accepted")
	} else {
		refusals++
		if !strings.Contains(err.Error(), CheckReplan) || !strings.Contains(err.Error(), ActContinue) {
			t.Fatalf("the refusal = %q, want it to name replan and the offered names", err.Error())
		}
	}
	if refusals != 1 {
		t.Fatalf("refusals = %d, want 1", refusals)
	}
	a, err := parseCheckinAnswer(`{"decision":"continue","guidance":"keep to the planned files"}`, ask)
	if err != nil || a.Decision != ActContinue {
		t.Fatalf("an answer among the offered names = (%+v, %v)", a, err)
	}
	for _, decision := range []string{"continue", "handover", RungHandUp} {
		if !slices.Contains(momentDecisions(ask.Moment, ask.Replan), decision) {
			t.Errorf("the waiting moment no longer offers %q", decision)
		}
	}
}
