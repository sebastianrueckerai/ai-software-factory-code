package factoryd

import (
	"maps"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestEvidenceShape(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"a file and line at main", "internal/factoryd/store.go:672 at main: func (s *PGStore) TasksByPR", true},
		{"a file and line at a commit", "store.go:672 at f92f5b6: line text", true},
		{"a command and its output", "$ go test ./internal/factoryd/ -run TestX\nok  PASS\n", true},
		{"empty", "", false},
		{"a bare assertion", "it exists on main", false},
		{"a vague place", "the method is defined somewhere", false},
		{"a path with neither shape", "internal/factoryd/store.go at main", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := evidenceOK(c.in); got != c.ok {
				t.Errorf("evidenceOK(%q) = %v, want %v", c.in, got, c.ok)
			}
		})
	}
}

func TestVerdictGuards(t *testing.T) {
	goodDrop := "internal/factoryd/store.go:672 at main: func (s *PGStore) TasksByPR"
	for _, c := range []struct {
		name     string
		findings []CheckFinding
		verdicts map[string]CheckVerdict
		sessions int
		human    string
		ok       bool
		want     map[string]CheckVerdict
	}{
		{
			name:     "a drop without evidence is no drop",
			findings: []CheckFinding{{Text: "TasksByPR is undefined"}},
			verdicts: map[string]CheckVerdict{"TasksByPR is undefined": {State: CheckDropped, Evidence: "it exists on main"}},
			sessions: 1,
			ok:       true,
			want:     map[string]CheckVerdict{"TasksByPR is undefined": {State: CheckConfirmed, Evidence: "drop without evidence: TasksByPR is undefined"}},
		},
		{
			name:     "a finding dropped again after a drop goes to the human",
			findings: []CheckFinding{{Text: "the handler leaks", PriorDropped: 1}},
			verdicts: map[string]CheckVerdict{"the handler leaks": {State: CheckDropped, Evidence: goodDrop}},
			sessions: 1,
			human:    "re-raised after a drop",
			want:     map[string]CheckVerdict{"the handler leaks": {State: CheckDropped, Evidence: goodDrop}},
		},
		{
			name:     "a finding confirmed as often as the sessions grant goes to the human",
			findings: []CheckFinding{{Text: "no test covers the handler", PriorConfirmed: 1}},
			verdicts: map[string]CheckVerdict{"no test covers the handler": {State: CheckConfirmed, Evidence: goodDrop}},
			sessions: 1,
			human:    "confirmed 1 times",
			want:     map[string]CheckVerdict{"no test covers the handler": {State: CheckConfirmed, Evidence: goodDrop}},
		},
		{
			name:     "a remark upgrades nothing",
			findings: []CheckFinding{{Text: "the merge rule reads oddly", PriorConfirmed: 3, PriorDropped: 2}},
			verdicts: map[string]CheckVerdict{"the merge rule reads oddly": {State: CheckRemark}},
			sessions: 1,
			ok:       true,
			want:     map[string]CheckVerdict{"the merge rule reads oddly": {State: CheckRemark}},
		},
		{
			name: "a clean set stands as answered",
			findings: []CheckFinding{
				{Text: "TasksByPR is undefined", PriorConfirmed: 1},
				{Text: "the handler leaks"},
			},
			verdicts: map[string]CheckVerdict{
				"TasksByPR is undefined": {State: CheckConfirmed, Evidence: goodDrop},
				"the handler leaks":      {State: CheckDropped, Evidence: goodDrop},
			},
			sessions: 3,
			ok:       true,
			want: map[string]CheckVerdict{
				"TasksByPR is undefined": {State: CheckConfirmed, Evidence: goodDrop},
				"the handler leaks":      {State: CheckDropped, Evidence: goodDrop},
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			human, ok := applyVerdictGuards(c.findings, c.verdicts, c.sessions)
			if human != c.human || ok != c.ok {
				t.Fatalf("applyVerdictGuards = (%q, %v), want (%q, %v)", human, ok, c.human, c.ok)
			}
			if !maps.Equal(c.verdicts, c.want) {
				t.Errorf("verdicts = %v, want %v", c.verdicts, c.want)
			}
		})
	}
}

func TestCutHandover(t *testing.T) {
	note := strings.Repeat("ü", 1000) + strings.Repeat("n", 2000) // 3,000 runes, 4,000 bytes
	got := cutHandover(note, 2000)
	if want := strings.Repeat("ü", 1000) + strings.Repeat("n", 1000); got != want {
		t.Fatalf("cut handover is not the note's first 2,000 runes")
	}
	if n := utf8.RuneCountInString(got); n != 2000 {
		t.Errorf("cut handover holds %d runes, want 2000", n)
	}
	if !utf8.ValidString(got) {
		t.Error("cut handover cuts mid-UTF-8")
	}
	if got := cutHandover("stamp the test rows, do not drop the check", 2000); got != "stamp the test rows, do not drop the check" {
		t.Errorf("a note under the cap = %q, want it unchanged", got)
	}
}

func TestCheckinStatement(t *testing.T) {
	ask := &CheckInAsk{
		Moment: CheckInGate,
		Findings: []CheckFinding{
			{Text: "TasksByPR is undefined", Source: "review"},
			{Text: "no test covers the handler", Source: "review", PriorConfirmed: 1},
		},
		Review: &Review{Verdict: "concerns", Summary: "the handler grows a second cache layer without a test"},
		Gates: &GateResults{
			Policy:       PolicyGate{Class: policy.ClassNormal, Outcome: policy.OutcomeAuto, MergeRule: policy.MergeAuto},
			Reproduction: ReproResult{Ran: true, Command: "go test ./...", Exit: 0},
			ChecksPassed: true,
		},
	}
	tail := strings.Repeat("t", 100)
	s := checkinStatement(ask, []byte(tail), 7, 1.5, 2000)
	for _, want := range []string{
		"at the gate moment",
		"before the next session is bought",
		"the finding is real and must be fixed",
		"a file and line at a ref, or a command and its output",
		"say it, block nothing",
		"only a human can act — a goal conflict, spend above policy, a policy class, access the factory lacks — stop and say which, with why",
		"at most 2000 characters",
		"TasksByPR is undefined",
		"no test covers the handler",
		"confirmed once before",
		"the handler grows a second cache layer without a test",
		"concerns",
		"risk class normal is auto",
		`reproduction "go test ./..." exited 0`,
		"checks green",
		"transcript_search",
		"transcript_read",
		"card_facts",
		"gates",
		"read_file",
		"grep",
		"git_log",
		"run",
		"/workspace/.factory/sessions/<parent>.jsonl",
		"<<<SESSION-TAIL>>>\n" + tail + "\nSESSION-TAIL>>>",
		"calls left 7, 1.50 EUR left for the check-in on this card",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("statement lacks %q:\n%s", want, s)
		}
	}
	order := []string{
		"at the gate moment",
		"only a human can act",
		"TasksByPR is undefined",
		"the handler grows a second cache layer without a test",
		"risk class normal is auto",
		"<<<SESSION-TAIL>>>",
		"calls left 7, 1.50 EUR left",
	}
	for i := 1; i < len(order); i++ {
		earlier, later := strings.Index(s, order[i-1]), strings.Index(s, order[i])
		if earlier < 0 || later < 0 || earlier >= later {
			t.Errorf("section %q (%d) does not come before %q (%d)", order[i-1], earlier, order[i], later)
		}
	}

	t.Run("a finding dropped before keeps its history line", func(t *testing.T) {
		ask := &CheckInAsk{
			Moment: CheckInGate,
			Findings: []CheckFinding{
				{Text: "the handler leaks", Source: "review", PriorDropped: 1},
				{Text: "no test covers the handler", Source: "review", PriorConfirmed: 2},
			},
		}
		s := checkinStatement(ask, nil, 7, 1.5, 2000)
		for _, want := range []string{"dropped before: keep it dropped only against new evidence", "confirmed 2 times before"} {
			if !strings.Contains(s, want) {
				t.Errorf("statement lacks %q:\n%s", want, s)
			}
		}
	})

	t.Run("no parent record", func(t *testing.T) {
		s := checkinStatement(ask, nil, 7, 1.5, 2000)
		if strings.Contains(s, "<<<SESSION-TAIL>>>") {
			t.Error("statement holds the tail markers with no tail")
		}
		if !strings.Contains(s, "record is absent") {
			t.Errorf("statement does not say the record is absent:\n%s", s)
		}
	})
}
