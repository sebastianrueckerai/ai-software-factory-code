package factoryd

import (
	"slices"
	"testing"
)

// [hand_up_answers_as_old_human] Every moment's hand_up resolves as the old
// answer human did: the three moments' enums hold hand_up and the parser
// accepts it, and a check-in case step with decision hand_up — what the
// landed migration rewrites a stored human row to — reads through handedUpBy
// as the hand-up: the moment's grounds as its found and no action of its own.
func TestProofHandUpAnswersAsOldHuman(t *testing.T) {
	grounds := "the objective asks for two stores"
	step := []CaseStep{{CaseID: 1, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp, Decision: RungHandUp, Grounds: grounds}}
	up := handedUpBy(step, 1, "")
	if up == nil || up.Found != grounds || up.Action != "" {
		t.Fatalf("a hand_up step reads as %+v, want the hand-up with the grounds as its found and no action of its own", up)
	}
	for _, moment := range []string{CheckInSession, CheckInGate, CheckInDead} {
		ask := &CheckInAsk{Moment: moment}
		if !slices.Contains(momentDecisions(moment, true), RungHandUp) {
			t.Errorf("the %s moment offers no hand_up", moment)
		}
		answer := `{"decision":"hand_up","reason_kind":"goal_conflict","reason":"` + grounds + `"}`
		a, err := parseCheckinAnswer(answer, ask)
		if err != nil {
			t.Fatalf("the %s moment refuses its hand_up: %v", moment, err)
		}
		if a.Decision != RungHandUp {
			t.Errorf("the %s moment answered %q, want hand_up", moment, a.Decision)
		}
	}
}
