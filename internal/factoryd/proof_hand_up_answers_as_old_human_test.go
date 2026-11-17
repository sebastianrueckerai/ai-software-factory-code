package factoryd

import (
	"slices"
	"testing"
)

// [hand_up_answers_as_old_human] Every moment's hand_up resolves as the old
// answer human did: the three moments' answers read hand_up, and a case
// step the store wrote before the change under the name human reads
// through the same reader as one written hand_up — the hand-up, with the
// moment's grounds as its found and no action of its own.
func TestProofHandUpAnswersAsOldHuman(t *testing.T) {
	grounds := "the objective asks for two stores"
	step := func(decision string) []CaseStep {
		return []CaseStep{{CaseID: 1, Rung: 1, Actor: CheckinDecider, Outcome: StepHandedUp, Decision: decision, Grounds: grounds}}
	}
	asOld := HandedUpBy(step("human"), 1, "")
	asNew := HandedUpBy(step(RungHandUp), 1, "")
	if asNew == nil || asNew.Found != grounds || asNew.Action != "" {
		t.Fatalf("a hand_up step reads as %+v, want the hand-up with the grounds as its found", asNew)
	}
	if asOld == nil || asOld.Found != asNew.Found || asOld.Action != asNew.Action {
		t.Fatalf("a stored human step reads as %+v, want the same hand-up as a hand_up step: %+v", asOld, asNew)
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
