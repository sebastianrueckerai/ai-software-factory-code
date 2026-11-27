package factoryd

import (
	"strings"
	"testing"
)

// Each bound is one constant. textutil.Truncate keeps n bytes and marks the
// cut with "…", so a text cut at n is n characters of text and the mark.
func cutAt(c byte, n int) string { return strings.Repeat(string(c), n) + "…" }

func TestReviewSummaryIsCutAt500(t *testing.T) {
	rv := parseReview(`{"verdict":"concerns","summary":"` + strings.Repeat("s", 600) + `","findings":["f.go: a fault"]}`)
	if rv.Summary != cutAt('s', 500) {
		t.Fatalf("summary of 600 characters parsed to %d runes (Note %q), want 500 and the mark", len([]rune(rv.Summary)), rv.Note)
	}
}

func TestAuditWhyIsCutAt300AndFileKeeps500(t *testing.T) {
	answer := `{"verdict":"findings","findings":[{"item":1,"file":"` + strings.Repeat("f", 400) + `","line":3,"why":"` + strings.Repeat("w", 400) + `"}]}`
	fs, why := parseAudit(answer)
	if len(fs) != 1 {
		t.Fatalf("parseAudit = %v, %q, want one finding", fs, why)
	}
	if fs[0].Why != cutAt('w', 300) {
		t.Errorf("why of 400 characters parsed to %d runes, want 300 and the mark", len([]rune(fs[0].Why)))
	}
	if fs[0].File != strings.Repeat("f", 400) {
		t.Errorf("file of 400 characters parsed to %d, want 400 whole", len(fs[0].File))
	}
}

func TestGateDecisionTextIsCutAt1000(t *testing.T) {
	long := strings.Repeat("d", 1500)
	d := parseGateDecision(`{"action":"escalate","diagnosis":"`+long+`","recommendation":"`+long+`"}`, []SupervisorAction{ActionEscalate}, nil)
	if d.Diagnosis != cutAt('d', 1000) || d.Recommendation != cutAt('d', 1000) {
		t.Errorf("1500 characters parsed to %d and %d runes, want 1000 and the mark each", len([]rune(d.Diagnosis)), len([]rune(d.Recommendation)))
	}
}

func TestCheckinGuidanceIsNotCutAtTheGateBound(t *testing.T) {
	long := strings.Repeat("g", 1500)
	a, err := parseCheckinAnswer(`{"decision":"continue","guidance":"`+long+`"}`, &CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-1"})
	if err != nil || a.Guidance != long {
		t.Fatalf("guidance of 1500 characters parsed to %d (err %v), want 1500 whole", len(a.Guidance), err)
	}
}
