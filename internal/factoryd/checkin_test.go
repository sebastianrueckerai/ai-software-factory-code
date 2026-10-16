package factoryd

import (
	"context"
	"errors"
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
		Moment:        CheckInGate,
		ParentSession: "s-parent-9",
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
		"/workspace/.factory/sessions/s-parent-9.jsonl",
		"<<<SESSION-TAIL>>>\n" + tail + "\nSESSION-TAIL>>>",
		"calls left 7, 1.50 EUR left for the check-in on this card",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("statement lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "<parent>") {
		t.Errorf("statement names the placeholder, not the concrete record:\n%s", s)
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

type scriptedChat struct {
	answers []CheckinResponse
	calls   int
	got     CheckinRequest
}

func (c *scriptedChat) Call(_ context.Context, _, _ string, in CheckinRequest) (CheckinResponse, error) {
	c.got = in
	c.calls++
	if c.calls > len(c.answers) {
		return CheckinResponse{}, errors.New("provider refused")
	}
	return c.answers[c.calls-1], nil
}

type scriptedTools struct{ out map[string]string }

func (t scriptedTools) Tool(_ context.Context, name string, _ map[string]any) (string, bool, error) {
	o, _ := t.out[name]
	return o, false, nil
}

func TestCheckinRunnerDropsWithEvidence(t *testing.T) {
	goodDrop := "internal/factoryd/store.go:672 at main: func (s *PGStore) TasksByPR"
	grepOut := "internal/factoryd/store.go:672:func (s *PGStore) TasksByPR {"
	ask := &CheckInAsk{Moment: CheckInGate, ParentSession: "s-parent-9",
		Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}
	chat := &scriptedChat{answers: []CheckinResponse{
		{ToolName: "grep", ToolArgs: map[string]any{"pattern": "TasksByPR"}, UsageEUR: 0.25},
		{Text: `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + goodDrop + `"}],"decision":"handover","handover":"the finding is false; build and test as usual"}`, UsageEUR: 0.5},
	}}
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, []byte("the tail"), chat,
		scriptedTools{out: map[string]string{"grep": grepOut}}, 12, 1.5, 1)
	if out.Decision != CheckHandover {
		t.Fatalf("decision = %q, want handover", out.Decision)
	}
	v, ok := out.Verdicts["TasksByPR is undefined"]
	if !ok || v.State != CheckDropped || v.Evidence != goodDrop {
		t.Fatalf("verdict = %+v, want dropped with its evidence kept", v)
	}
	if out.Handover != "the finding is false; build and test as usual" {
		t.Fatalf("handover = %q", out.Handover)
	}
	rec := out.Record
	if rec == nil {
		t.Fatal("the conversation left no record")
	}
	if rec.Model != "checkin" || rec.ParentSession != "s-parent-9" || rec.Moment != CheckInGate {
		t.Fatalf("record head = %q %q %q", rec.Model, rec.ParentSession, rec.Moment)
	}
	if rec.StatementBytes == 0 || rec.TailBytes != len("the tail") {
		t.Errorf("statement bytes %d tail bytes %d, want a statement and the tail's size", rec.StatementBytes, rec.TailBytes)
	}
	if len(rec.Calls) != 3 {
		t.Fatalf("record holds %d calls, want the two model calls and the tool's execution", len(rec.Calls))
	}
	for i, c := range rec.Calls {
		if c.Duration <= 0 {
			t.Errorf("call %d (%s) has no duration", i, c.Name)
		}
	}
	if rec.Calls[0].Name != "checkin" || rec.Calls[2].Name != "checkin" {
		t.Fatalf("the model calls are not recorded under the alias: %+v", rec.Calls)
	}
	tc := rec.Calls[1]
	if tc.Name != "grep" || tc.ResultBytes != len(grepOut) || tc.Args == "" {
		t.Fatalf("tool call = %+v, want grep with its args and result size", tc)
	}
	if rec.ToolCalls != 1 || rec.CostEUR != 0.75 {
		t.Errorf("tool calls %d cost %v, want 1 and 0.75", rec.ToolCalls, rec.CostEUR)
	}
	if rec.Decision != CheckHandover || rec.Duration <= 0 {
		t.Errorf("record decision %q duration %v, want handover and a duration", rec.Decision, rec.Duration)
	}
	if !strings.Contains(chat.got.Input, grepOut) {
		t.Error("the tool's output never reached the conversation")
	}

	t.Run("the handover is cut at the cap", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + goodDrop + `"}],"decision":"handover","handover":"` + strings.Repeat("k", 2500) + `"}`}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1)
		if n := utf8.RuneCountInString(out.Handover); n != 2000 {
			t.Fatalf("handover holds %d runes, want the 2000 of the default cap", n)
		}
	})
}

func TestCheckinRunnerGuardsUpgradeToHuman(t *testing.T) {
	goodDrop := "internal/factoryd/store.go:672 at main: func (s *PGStore) TasksByPR"
	for _, c := range []struct {
		name    string
		finding CheckFinding
		answer  string
		reason  string
	}{
		{
			name:    "confirmed as often as the sessions grant",
			finding: CheckFinding{Text: "no test covers the handler", PriorConfirmed: 1},
			answer:  `{"findings":[{"text":"no test covers the handler","verdict":"confirmed"}],"decision":"handover","handover":"fix it"}`,
			reason:  "confirmed 1 times",
		},
		{
			name:    "dropped again after a drop",
			finding: CheckFinding{Text: "the handler leaks", PriorDropped: 1},
			answer:  `{"findings":[{"text":"the handler leaks","verdict":"dropped","evidence":"` + goodDrop + `"}],"decision":"handover","handover":"still false"}`,
			reason:  "re-raised after a drop",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ask := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{c.finding}}
			out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil,
				&scriptedChat{answers: []CheckinResponse{{Text: c.answer}}}, scriptedTools{}, 12, 1.5, 1)
			if out.Decision != CheckHuman {
				t.Fatalf("decision = %q, want human", out.Decision)
			}
			if out.ReasonKind != "" || out.Reason != c.reason {
				t.Fatalf("reason = (%q, %q), want (%q, %q)", out.ReasonKind, out.Reason, "", c.reason)
			}
		})
	}
}

func TestCheckinRunnerRefusedCallsEndTheConversation(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}
	chat := &scriptedChat{} // the provider refuses every call
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1)
	if out.Decision != CheckHandover || out.Handover != "" || len(out.Verdicts) != 0 {
		t.Fatalf("outcome = %+v, want a handover with no note and no verdicts: the round runs as if no check-in existed", out)
	}
	if out.Record == nil || out.Record.Reason != "check-in calls refused: provider refused" {
		t.Fatalf("record = %+v, want the refusal named", out.Record)
	}
	if chat.calls != 1 {
		t.Fatalf("calls = %d, want the conversation ended at the first refusal", chat.calls)
	}
}

func TestCheckinRunnerFinalAnswerShape(t *testing.T) {
	goodDrop := "internal/factoryd/store.go:672 at main: func (s *PGStore) TasksByPR"
	ask := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{
		{Text: "TasksByPR is undefined", Source: "review"},
		{Text: "no test covers the handler", Source: "review"},
	}}
	unreadable := func(t *testing.T, text, want string) {
		t.Helper()
		chat := &scriptedChat{answers: []CheckinResponse{{Text: text}, {Text: text}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1)
		if out.Decision != CheckHuman || !strings.HasPrefix(out.Reason, "check-in answer unreadable: ") {
			t.Fatalf("outcome = %q %q, want a human over the unreadable answer", out.Decision, out.Reason)
		}
		if !strings.Contains(out.Reason, want) {
			t.Errorf("reason %q lacks %q", out.Reason, want)
		}
		if chat.calls != 2 {
			t.Fatalf("calls = %d, want the one repair round before the human", chat.calls)
		}
	}
	t.Run("not JSON", func(t *testing.T) {
		unreadable(t, "the finding is false, take my word", "not one JSON object")
	})
	t.Run("an unknown finding", func(t *testing.T) {
		unreadable(t, `{"findings":[{"text":"a finding never raised","verdict":"confirmed"}],"decision":"handover"}`, `unknown finding "a finding never raised"`)
	})
	t.Run("an unknown verdict", func(t *testing.T) {
		unreadable(t, `{"findings":[{"text":"TasksByPR is undefined","verdict":"maybe"}],"decision":"handover"}`, `unknown verdict "maybe"`)
	})
	t.Run("a verdict missing for a finding the ask named", func(t *testing.T) {
		unreadable(t, `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"`+goodDrop+`"}],"decision":"handover"}`, `no verdict for "no test covers the handler"`)
	})
	t.Run("one repair round", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{
			{Text: "not json"},
			{Text: `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + goodDrop + `"},{"text":"no test covers the handler","verdict":"confirmed"}],"decision":"handover","handover":"drop one, fix the other"}`},
		}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1)
		if out.Decision != CheckHandover || out.Verdicts["no test covers the handler"].State != CheckConfirmed {
			t.Fatalf("outcome = %+v, want the repaired answer to stand", out)
		}
		if chat.calls != 2 {
			t.Fatalf("calls = %d, want the repair round", chat.calls)
		}
	})
	t.Run("a mixed dropped_all is a handover", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + goodDrop + `"},{"text":"no test covers the handler","verdict":"confirmed"}],"decision":"dropped_all"}`}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1)
		if out.Decision != CheckHandover {
			t.Fatalf("decision = %q, want handover: a set with a finding left standing verifies nothing", out.Decision)
		}
	})
}

func TestCheckinRunnerLimits(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}}}
	chat := &scriptedChat{answers: []CheckinResponse{
		{ToolName: "grep", ToolArgs: map[string]any{"pattern": "a"}},
		{ToolName: "grep", ToolArgs: map[string]any{"pattern": "b"}},
		{ToolName: "grep", ToolArgs: map[string]any{"pattern": "c"}},
	}}
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat,
		scriptedTools{out: map[string]string{"grep": "one line"}}, 2, 1.5, 1)
	if out.Decision != CheckHuman || out.ReasonKind != CheckSpend || out.Reason != "check-in call cap reached" {
		t.Fatalf("outcome = %+v, want the cap's human ask under spend", out)
	}
	if chat.calls != 2 {
		t.Fatalf("calls = %d, want the third call refused by the runner", chat.calls)
	}
	if out.Record.ToolCalls != 2 {
		t.Errorf("tool calls = %d, want 2", out.Record.ToolCalls)
	}
}
