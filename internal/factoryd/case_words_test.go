package factoryd

import (
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"strings"
	"testing"
	"time"
)

func TestCaseWordsNameTheCardAndTheProblemNotTheNumber(t *testing.T) {
	task := Task{ID: "t-1", IssueNumber: 1410}
	c := Case{ID: 8731, Kind: CaseSessionCap, Problem: "#1410 is one green check from merge but " + strings.Repeat("x", 100)}
	res := Resolution{Action: ActContinue}
	want := `the session cap on #1410 ("#1410 is one green check from merge but ` + strings.Repeat("x", 80-len("#1410 is one green check from merge but ")) + `…")`
	if got := caseWords("#1410", c); got != want {
		t.Fatalf("caseWords = %q, want %q", got, want)
	}
	if got, want := caseWords("#1410", Case{Kind: CaseMonitorFlag}), "the monitor flag on #1410"; got != want {
		t.Fatalf("caseWords with no problem = %q, want %q", got, want)
	}

	console := actText(ActInput{Via: ViaConsole}, task, c, res, nil)
	if strings.Contains(console, "8731") || !strings.Contains(console, "#1410") || !strings.HasPrefix(console, "The session cap on #1410") {
		t.Fatalf("actText via the console = %q, want the words, #1410 and no case id", console)
	}
	chat := actText(ActInput{Via: ViaChat}, task, c, res, nil)
	if !strings.Contains(chat, "8731") || !strings.Contains(chat, "#1410") {
		t.Fatalf("actText via the chat = %q, want the words and the id", chat)
	}
}

func TestFacesOfAMonitorFlagNameNoCaseId(t *testing.T) {
	c := Case{ID: 396, Kind: CaseMonitorFlag}
	if got := tripNext(c, "r"); strings.Contains(got, "396") || !strings.Contains(got, "project chat") {
		t.Fatalf("tripNext = %q", got)
	}
	q := monitorQuestion("r")
	if s := q.Ask + q.Why + q.Info; strings.Contains(s, "396") {
		t.Fatalf("monitorQuestion = %+v", q)
	}
}

func TestErrCaseEndedNamesTheCardNotTheNumber(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	base := Case{ID: 412, Kind: CaseMonitorFlag, Status: CaseResolved}
	for name, c := range map[string]Case{
		"open":      base,
		"resolved":  {ID: 412, Kind: CaseMonitorFlag, Status: CaseResolved, ResolvedAt: &at, Resolution: &CaseResolution{By: "sebastian", Action: "note"}},
		"withdrawn": {ID: 412, Kind: CaseMonitorFlag, Status: CaseWithdrawn, ResolvedAt: &at, Resolution: &CaseResolution{Reason: "gone"}},
	} {
		got := ErrCaseEnded{Case: c, Card: "#1659"}.Error()
		if strings.Contains(got, "412") || !strings.Contains(got, "the monitor flag on #1659") {
			t.Errorf("%s: Error = %q", name, got)
		}
	}
	got := ErrCaseEnded{Case: base}.Error()
	if got != "the monitor flag is resolved" {
		t.Errorf("with no card Error = %q", got)
	}
}

func TestMonitorFaceMasksASecretAndNeutralisesAMention(t *testing.T) {
	r, _, _, _, _ := newRetryRig(t)
	r.Scrub = redact.New("hunter2-configured-secret")
	reason := "Monitor flag: hunter2-configured-secret Authorization: Bearer abcdefghijklmnop1234567890 @everyone https://user:pass@host.example/x"
	q := r.monitorFace(Task{Repository: "linkshort/linkshort"}, reason)
	for _, leak := range []string{"hunter2-configured-secret", "abcdefghijklmnop1234567890", "@everyone", "user:pass"} {
		if strings.Contains(q.Ask, leak) {
			t.Errorf("Ask = %q holds %q", q.Ask, leak)
		}
	}
	if !strings.Contains(q.Ask, "The monitor flagged this card (") {
		t.Errorf("Ask = %q", q.Ask)
	}
}
