package factoryd

import (
	"strings"
	"testing"
	"time"
)

// last_answer_suffix_gone_proven: no part of a case's face carries the
// processed-at line.
func TestProofLastAnswerSuffixGone(t *testing.T) {
	at := time.Date(2026, 11, 1, 21, 40, 0, 0, time.UTC)
	ask := CaseAsk{At: at.Add(-time.Hour), Detail: map[string]any{"info": "What each answer does.",
		"recommendation": "handover: a fresh session fixes it", "recommended_by": "the factory agent"}}
	for _, tc := range []struct {
		name string
		c    Case
	}{
		{"the ask's info", Case{Kind: CaseSessionCap, Problem: "#7 used its 3 sessions."}},
		{"the case's brief", Case{Kind: CaseAgentEscalate, Problem: "Use a replica?", Brief: "Postgres is shared."}},
	} {
		face := CaseFace(tc.c, Task{}, ask)
		for _, part := range []string{face.Ask, face.Money, face.Why, face.Info, face.Recommendation, face.Source} {
			if strings.Contains(part, "Last answer processed") {
				t.Errorf("%s: the face carries the processed-at line: %q", tc.name, part)
			}
		}
	}
}
