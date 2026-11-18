package factoryd

import (
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"go/ast"
	"go/parser"
	"go/token"
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
	if got := tripNext(c, "r"); strings.Contains(got, "396") || !strings.HasSuffix(got, " on the board.") {
		t.Fatalf("tripNext = %q", got)
	}
	c.Facts = map[string]any{"flag": "wait_over_bound", "reason": "r"}
	face := FaceOf(c, Facts{Card: Task{ID: "t-1", IssueNumber: 1410}})
	if s := face.Ask + face.Why + face.Story.Planned + face.Story.Happened + face.Story.Left; strings.Contains(s, "396") {
		t.Fatalf("the monitor flag's face = %+v", face)
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

// A monitor flag's reason is agent-derived: the case its trip opens asks in
// the register's words, so neither the ask nor the face carries the
// reason's secret, token, mention or credential.
func TestMonitorFaceMasksASecretAndNeutralisesAMention(t *testing.T) {
	r, _, _, _, _ := newRetryRig(t)
	r.Scrub = redact.New("hunter2-configured-secret")
	reason := "Monitor flag: hunter2-configured-secret Authorization: Bearer abcdefghijklmnop1234567890 @everyone https://user:pass@host.example/x"
	card := Task{ID: "t-1", Repository: "linkshort/linkshort", IssueNumber: 7}
	c, err := TripCase(card, CaseFacts{}, TripInput{TaskID: card.ID, Reason: reason, Kind: CaseMonitorFlag, StateKey: "k",
		Extra: map[string]any{"flag": "wait_over_bound"}}, r.Scrub.String)
	if err != nil {
		t.Fatal(err)
	}
	face := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: card})
	shown := strings.Join([]string{c.Problem, face.Ask, face.Why, face.Story.Planned, face.Story.Happened, face.Story.Left}, "\n")
	for _, leak := range []string{"hunter2-configured-secret", "abcdefghijklmnop1234567890", "@everyone", "user:pass"} {
		if strings.Contains(shown, leak) {
			t.Errorf("the face holds %q:\n%s", leak, shown)
		}
	}
	if c.Problem != face.Ask || face.Ask != FlagFaces["wait_over_bound"].Ask(Facts{CaseFacts: FactsOf(c), Card: card, Case: c}) {
		t.Errorf("the problem %q and the ask %q are not the flag's own", c.Problem, face.Ask)
	}
}

// No line the issue gets names a control by hand: the words of the issue's
// lines (ladder.go) and of the stop reasons it posts (activities.go) hold
// no form's name; the face's answers name what to press (boardAnswers).
func TestNoIssueLineNamesAControlByHand(t *testing.T) {
	for _, file := range []string{"ladder.go", "activities.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				for _, old := range []string{"Fund form", "Fund on the board", "Extend"} {
					if strings.Contains(lit.Value, old) {
						t.Errorf("%s holds %q in %s", file, old, lit.Value)
					}
				}
			}
			return true
		})
	}
}
