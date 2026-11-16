package factoryd

import (
	"slices"
	"testing"
)

// [moments_answer_from_offered] Each moment's answer schema derives its
// decision enum from the rungs' one action list plus hand_up: the session
// moment offers continue, handover and replan while the ask offers the cut,
// the gate a handover and the reverify, the dead a handover alone, and none
// of the moments offers its own old names — on the answer surface and in
// the store's case steps alike.
func TestProofMomentsAnswerFromOffered(t *testing.T) {
	requireStoredDecisionsOnTheRungsNames(t)
	for _, c := range []struct {
		name string
		ask  *CheckInAsk
		want []string
	}{
		{"session cuttable", &CheckInAsk{Moment: CheckInSession, Replan: true, Trip: &TripReport{Detector: "repetition"}},
			[]string{ActContinue, CheckHandover, CheckReplan, RungHandUp}},
		{"session uncut", &CheckInAsk{Moment: CheckInSession, Trip: &TripReport{Detector: "repetition"}},
			[]string{ActContinue, CheckHandover, RungHandUp}},
		{"gate with findings", &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "the handler grows a second cache layer without a test", Source: "review"}}},
			[]string{CheckHandover, ActReverify, RungHandUp}},
		{"dead", &CheckInAsk{Moment: CheckInDead}, []string{CheckHandover, RungHandUp}},
	} {
		t.Run(c.name, func(t *testing.T) {
			props, _ := momentAnswerSchema(c.ask)["properties"].(map[string]any)
			decision, _ := props["decision"].(map[string]any)
			got, _ := decision["enum"].([]string)
			if !slices.Equal(got, c.want) {
				t.Fatalf("decision enum = %v, want %v", got, c.want)
			}
			for _, name := range got {
				if name == RungHandUp {
					continue
				}
				if _, ok := ActionByName(name); !ok {
					t.Errorf("decision %q is no name of the one action list", name)
				}
			}
			for _, old := range []string{"message", "human", "dropped_all"} {
				if slices.Contains(got, old) {
					t.Errorf("decision enum offers the moments' own name %q", old)
				}
			}
		})
	}
}
