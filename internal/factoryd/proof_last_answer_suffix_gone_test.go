package factoryd

import (
	"strings"
	"testing"
	"time"
)

// last_answer_suffix_gone_proven: a case answered at a known time shows
// the ask's info alone as its face's Info, the case's brief where it has
// one; the answer's time rides LastAnswerAt, and no part of the face
// carries the processed-at line.
func TestProofLastAnswerSuffixGone(t *testing.T) {
	at := time.Date(2026, 11, 1, 21, 40, 0, 0, time.UTC)
	ask := CaseAsk{At: at.Add(-time.Hour), Detail: map[string]any{"info": "What each answer does.",
		"recommendation": "handover: a fresh session fixes it", "recommended_by": "the factory agent"}}
	for _, tc := range []struct {
		name, info string
		c          Case
	}{
		{"the ask's info", "What each answer does.", Case{Kind: CaseSessionCap, Problem: "#7 used its 3 sessions."}},
		{"the case's brief", "Postgres is shared.", Case{Kind: CaseAgentEscalate, Problem: "Use a replica?", Brief: "Postgres is shared."}},
	} {
		answered := at
		face := CaseFace(tc.c, nil, CaseFacts{}, ask, &answered)
		if face.Info != tc.info {
			t.Errorf("%s: Info = %q, want %q alone", tc.name, face.Info, tc.info)
		}
		if face.LastAnswerAt == nil || !face.LastAnswerAt.Equal(at) {
			t.Errorf("%s: LastAnswerAt = %v, want %v", tc.name, face.LastAnswerAt, at)
		}
		for _, part := range []string{face.Ask, face.Money, face.Why, face.Info, face.Recommendation, face.Source} {
			if strings.Contains(part, "Last answer processed") {
				t.Errorf("%s: the face carries the processed-at line: %q", tc.name, part)
			}
		}
	}
}
