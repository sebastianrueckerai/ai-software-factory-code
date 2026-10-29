package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
		"# The writing guide",
		"No em dashes",
		"the finding is real and must be fixed",
		"a file and line at a ref, or a command and its output",
		"say it, block nothing",
		"only a human can act — a goal conflict, spend above policy, a policy class, access the factory lacks — stop and say which, with why",
		"at most 2000 characters",
		"f1: TasksByPR is undefined (from review)",
		"f2: no test covers the handler (from review)",
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

// scriptedChat answers a check-in's conversations in order. Fails makes
// the first calls error before the answers apply; delay holds each call,
// honouring the caller's context, so a session's deadline cuts it; onCall
// runs at each call's start.
type scriptedChat struct {
	answers []CheckinResponse
	calls   int
	got     CheckinRequest
	gots    []CheckinRequest
	aliases []string
	fails   int
	delay   time.Duration
	onCall  func()
}

func (c *scriptedChat) Call(ctx context.Context, _, alias string, in CheckinRequest) (CheckinResponse, error) {
	c.got = in
	c.gots = append(c.gots, in)
	c.aliases = append(c.aliases, alias)
	c.calls++
	if c.onCall != nil {
		c.onCall()
	}
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return CheckinResponse{}, ctx.Err()
		}
	}
	if n := c.calls - c.fails; n < 1 || n > len(c.answers) {
		return CheckinResponse{}, errors.New("provider refused")
	}
	return c.answers[c.calls-c.fails-1], nil
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
		scriptedTools{out: map[string]string{"grep": grepOut}}, 12, 1.5, 1, 2000)
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
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
		if n := utf8.RuneCountInString(out.Handover); n != 2000 {
			t.Fatalf("handover holds %d runes, want the cap's 2000 runes", n)
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
				&scriptedChat{answers: []CheckinResponse{{Text: c.answer}}}, scriptedTools{}, 12, 1.5, 1, 2000)
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
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
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
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
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
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
		if out.Decision != CheckHandover || out.Verdicts["no test covers the handler"].State != CheckConfirmed {
			t.Fatalf("outcome = %+v, want the repaired answer to stand", out)
		}
		if chat.calls != 2 {
			t.Fatalf("calls = %d, want the repair round", chat.calls)
		}
	})
	t.Run("a mixed dropped_all is a handover", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"findings":[{"text":"TasksByPR is undefined","verdict":"dropped","evidence":"` + goodDrop + `"},{"text":"no test covers the handler","verdict":"confirmed"}],"decision":"dropped_all"}`}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
		if out.Decision != CheckHandover {
			t.Fatalf("decision = %q, want handover: a set with a finding left standing verifies nothing", out.Decision)
		}
	})
	t.Run("a paraphrased conflict verdict needs no repair round", func(t *testing.T) {
		conflict := conflictVerdict("the branch conflicts with its base").findings[0]
		ask := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: conflict, Source: "changes"}}}
		chat := &scriptedChat{answers: []CheckinResponse{{Text: `{"findings":[{"id":"f1","text":"the PR stopped merging into main because main moved under it (Forgejo: the branch conflicts with its base). Merge origin/main into the branch, resolve every conflict keeping both main's changes and the task's, run the tests of the touched packages, and end the session. No rebase; leave .forgejo/ as the merge brings it.","verdict":"confirmed"}],"decision":"handover","handover":"merge main into the branch first"}`}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
		if out.Decision != CheckHandover || chat.calls != 1 {
			t.Fatalf("outcome %q after %d calls, want the paraphrased verdict read on the first answer", out.Decision, chat.calls)
		}
		if v, ok := out.Verdicts[conflict]; !ok || v.State != CheckConfirmed {
			t.Fatalf("verdicts = %v, want the conflict finding confirmed under its own text", out.Verdicts)
		}
	})
}

func TestParseCheckinAnswerNamesTheFinding(t *testing.T) {
	conflict := conflictVerdict("the branch conflicts with its base").findings[0]
	ask := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: conflict, Source: "changes"}}}

	t.Run("a verdict that paraphrases the conflict finding by its id parses", func(t *testing.T) {
		a, err := parseCheckinAnswer(`{"findings":[{"id":"f1","text":"the pull request stopped merging: main moved under it; merge main in, resolve every conflict keeping both sides and end the session","verdict":"confirmed"}],"decision":"handover","handover":"merge main before anything else"}`, ask)
		if err != nil {
			t.Fatalf("the paraphrased verdict did not parse: %v", err)
		}
		if len(a.Findings) != 1 || a.Findings[0].Text != conflict {
			t.Fatalf("findings = %+v, want the verdict keyed by the ask's own text", a.Findings)
		}
	})
	t.Run("a verdict that re-punctuates the finding parses without its id", func(t *testing.T) {
		repunctuated := strings.ReplaceAll(conflict, ": ", " — ")
		repunctuated = strings.ReplaceAll(repunctuated, "`", "")
		repunctuated = strings.ReplaceAll(repunctuated, ". ", "  ")
		repunctuated = strings.ReplaceAll(repunctuated, ", ", " ; ")
		repunctuated = "The" + repunctuated[3:]
		a, err := parseCheckinAnswer(`{"findings":[{"text":"`+repunctuated+`","verdict":"confirmed"}],"decision":"handover","handover":"merge main"}`, ask)
		if err != nil {
			t.Fatalf("the re-punctuated verdict did not parse: %v", err)
		}
		if len(a.Findings) != 1 || a.Findings[0].Text != conflict {
			t.Fatalf("findings = %+v, want the verdict keyed by the ask's own text", a.Findings)
		}
	})
	t.Run("an answer naming a finding the ask never carried still fails", func(t *testing.T) {
		for _, answer := range []string{
			`{"findings":[{"id":"f2","verdict":"confirmed"}],"decision":"handover"}`,
			`{"findings":[{"id":"f2","text":"a finding never raised","verdict":"confirmed"}],"decision":"handover"}`,
		} {
			if _, err := parseCheckinAnswer(answer, ask); err == nil || !strings.Contains(err.Error(), "unknown finding") {
				t.Errorf("answer %s: err = %v, want the unknown finding", answer, err)
			}
		}
	})
}

// The conversation's last call carries no tools and asks for the answer,
// and every tool answer before it tells the judge the calls it has left.
func TestCheckinLastCallCarriesNoToolsAndAsksForTheAnswer(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-1", Trip: &TripReport{Detector: TripRepetition}}
	chat := &scriptedChat{answers: []CheckinResponse{
		{ToolName: "card_facts"},
		{Text: `{"decision":"continue","reason":"research"}`},
	}}
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat,
		scriptedTools{out: map[string]string{"card_facts": "budget 2 EUR"}}, 2, 1.5, 1, 2000)
	if len(chat.gots) != 2 {
		t.Fatalf("calls = %d, want the tool round and the answer", len(chat.gots))
	}
	if chat.gots[0].Tools == nil {
		t.Error("the first call carries no tools")
	}
	if chat.gots[1].Tools != nil {
		t.Errorf("the last call carries tools %v, want none", chat.gots[1].Tools)
	}
	if !strings.HasSuffix(chat.gots[1].Input, "\n\nThis is your last call: answer now with the JSON object, no tool.") {
		t.Errorf("the last call's input does not end on the ask for the answer:\n%s", chat.gots[1].Input)
	}
	if !strings.Contains(chat.gots[1].Input, "\n(calls left 1)") {
		t.Errorf("the tool answer does not name the calls left:\n%s", chat.gots[1].Input)
	}
	if out.Decision != "continue" {
		t.Fatalf("outcome = %+v, want the last call's continue", out)
	}
}

// A tool asked for on the last call is never run: the conversation ends
// at the call cap's human ask under spend.
func TestCheckinToolAnswerOnTheLastCallIsTheCapStop(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-1", Trip: &TripReport{Detector: TripRepetition}}
	chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "card_facts"}, {ToolName: "card_facts"}}}
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat,
		scriptedTools{out: map[string]string{"card_facts": "budget 2 EUR"}}, 2, 1.5, 1, 2000)
	if out.Decision != CheckHuman || out.ReasonKind != CheckSpend || out.Reason != "check-in call cap reached" {
		t.Fatalf("outcome = %+v, want the cap's human ask under spend", out)
	}
	if chat.calls != 2 {
		t.Fatalf("calls = %d, want both calls made", chat.calls)
	}
	if out.Record.ToolCalls != 1 {
		t.Errorf("tool calls = %d, want 1: the last call's tool never ran", out.Record.ToolCalls)
	}
}

// An unreadable answer on the last call leaves no room for the repair
// round: the outcome is the unreadable answer's human ask, not the cap's.
func TestCheckinUnreadableLastAnswerSaysSo(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-1", Trip: &TripReport{Detector: TripRepetition}}
	chat := &scriptedChat{answers: []CheckinResponse{{Text: "the session looks fine"}}}
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 1, 1.5, 1, 2000)
	if out.Decision != CheckHuman || !strings.HasPrefix(out.Reason, "check-in answer unreadable: ") || out.Reason == "check-in call cap reached" {
		t.Fatalf("outcome = %+v, want the unreadable answer's human ask", out)
	}
	if out.ReasonKind != "" || out.Record.Reason != out.Reason {
		t.Fatalf("outcome kind %q, record reason %q; want no spend kind and the reason recorded", out.ReasonKind, out.Record.Reason)
	}
	if chat.calls != 1 {
		t.Fatalf("calls = %d, want the one call", chat.calls)
	}
}

// At the session moment a handover with no note is the shape of a refused
// call: the repair round asks again, and the handover with its note stands.
func TestCheckinSessionHandoverNeedsItsNote(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-1", Trip: &TripReport{Detector: TripChurn}}
	chat := &scriptedChat{answers: []CheckinResponse{
		{Text: `{"decision":"handover","reason":"the agent circles"}`},
		{Text: `{"decision":"handover","reason":"the agent circles","handover":"start from the failing test"}`},
	}}
	out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000)
	if out.Decision != CheckHandover || out.Handover != "start from the failing test" {
		t.Fatalf("outcome = %+v, want the second answer's handover", out)
	}
	if chat.calls != 2 {
		t.Fatalf("calls = %d, want one repair round", chat.calls)
	}
	if !strings.Contains(chat.gots[1].Input, "Your last answer was unreadable: a handover needs the note to the next session") {
		t.Errorf("the repair round does not name the missing note:\n%s", chat.gots[1].Input)
	}
}

// runnerTools answers the transcript and card questions itself and runs
// the workspace tools through the worker's /tool endpoint: the local
// answers, the posted ones with their bearer, a non-200 the model sees,
// the output cut at the cap, and a run that outlives its timeout named.
func TestRunnerTools(t *testing.T) {
	kube := newFakeKube()
	kube.pods["ns1/p1"] = &fakePod{ns: "ns1", name: "p1"}
	r := &Runner{kube: kube}
	worker := &fakeWorkerEndpoint{status: http.StatusNoContent}
	worker.start(t, r)
	tools := &runnerTools{r: r, ns: "ns1", pod: "p1", token: "bearer-7",
		parent: []byte("alpha\nbeta\ngamma\n"), facts: `{"budget_eur":0.3,"spent_eur":0.05,"attempt":1,"sessions":3}`,
		ask: &CheckInAsk{Gates: &GateResults{Pin: 7}}}
	setTool := func(reply func(name string, args map[string]any) (int, string)) {
		worker.mu.Lock()
		worker.onTool = reply
		worker.mu.Unlock()
	}

	if out, cut, err := tools.Tool(t.Context(), "transcript_search", map[string]any{"query": "beta"}); out != "beta" || cut || err != nil {
		t.Fatalf("search = (%q, %v, %v), want the matching line", out, cut, err)
	}
	if out, _, _ := tools.Tool(t.Context(), "transcript_search", map[string]any{"query": "zzz"}); out != "" {
		t.Fatalf("search without a match = %q, want the empty answer git grep gives", out)
	}
	if out, cut, err := tools.Tool(t.Context(), "transcript_read", map[string]any{"from": 2.0, "to": 3.0}); out != "beta\ngamma" || cut || err != nil {
		t.Fatalf("read = (%q, %v, %v), want the span's lines", out, cut, err)
	}
	if _, _, err := tools.Tool(t.Context(), "transcript_read", map[string]any{"from": 9.0, "to": 12.0}); err == nil ||
		!strings.Contains(err.Error(), "the parent record holds 3 lines") {
		t.Fatalf("read past the end = %v, want the record's size named", err)
	}
	if out, _, err := tools.Tool(t.Context(), "card_facts", nil); out != tools.facts || err != nil {
		t.Fatalf("card_facts = (%q, %v), want the card's facts", out, err)
	}
	if out, _, err := tools.Tool(t.Context(), "gates", nil); err != nil || !strings.Contains(out, `"Pin":7`) {
		t.Fatalf("gates = (%q, %v), want the ask's gates serialized", out, err)
	}

	setTool(func(name string, args map[string]any) (int, string) {
		b, _ := json.Marshal(map[string]any{"output": name + " " + args["path"].(string), "cut": false})
		return http.StatusOK, string(b)
	})
	if out, cut, err := tools.Tool(t.Context(), "read_file", map[string]any{"path": "x.go"}); out != "read_file x.go" || cut || err != nil {
		t.Fatalf("posted tool = (%q, %v, %v), want the worker's answer", out, cut, err)
	}
	worker.mu.Lock()
	asks, auth := slices.Clone(worker.tools), slices.Clone(worker.auth)
	worker.mu.Unlock()
	if !slices.Equal(asks, []string{`read_file {"path":"x.go"}`}) || !slices.Equal(auth, []string{"Bearer bearer-7"}) {
		t.Fatalf("worker got %q with %q, want the tool's ask under the session's bearer", asks, auth)
	}

	setTool(func(string, map[string]any) (int, string) { return http.StatusBadRequest, "unknown tool rm" })
	if _, _, err := tools.Tool(t.Context(), "rm", nil); err == nil ||
		!strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unknown tool rm") {
		t.Fatalf("a refused tool = %v, want the status and the text the model sees", err)
	}

	setTool(func(string, map[string]any) (int, string) {
		b, _ := json.Marshal(map[string]any{"output": strings.Repeat("k", 40<<10), "cut": false})
		return http.StatusOK, string(b)
	})
	if out, cut, _ := tools.Tool(t.Context(), "run", map[string]any{"command": "cat big"}); len(out) != 32<<10 || !cut {
		t.Fatalf("posted tool = %d bytes cut %v, want the cap's cut", len(out), cut)
	}

	old := checkinToolTimeout
	checkinToolTimeout = 50 * time.Millisecond
	t.Cleanup(func() { checkinToolTimeout = old })
	setTool(func(string, map[string]any) (int, string) { time.Sleep(time.Second); return http.StatusOK, "{}" })
	if _, _, err := tools.Tool(t.Context(), "run", map[string]any{"command": "sleep"}); err == nil ||
		!strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("a hanging tool = %v, want the deadline named", err)
	}
}

// The three moments share the tail markers and the limits line; the gate
// statement keeps its opening byte for byte, and the session and dead
// statements open on their own sentences, never on another moment's.
func TestCheckinStatementMoments(t *testing.T) {
	const gateOpening = "You are the factory's check-in at the gate moment: a round came back with findings, and you judge each one before the next session is bought."
	const sessionOpening = "You are the factory's check-in inside session s-live-3: the detector churn tripped (4 writes to the same file in 6 minutes), and you decide what happens before the session continues. You never edit anything; you gather evidence and answer."
	const deadOpening = "You are the factory's check-in before a fresh session: the session s-dead-2 ended without a result: wall clock with the work uncommitted. You never edit anything; you gather evidence and answer."

	gate := checkinStatement(&CheckInAsk{
		Moment: CheckInGate, ParentSession: "s-parent-9",
		Findings: []CheckFinding{{Text: "TasksByPR is undefined", Source: "review"}},
	}, []byte("the tail"), 7, 1.5, 2000)
	if !strings.Contains(gate, gateOpening) {
		t.Fatalf("gate statement lost its opening:\n%s", gate)
	}

	session := checkinStatement(&CheckInAsk{
		Moment: CheckInSession, ParentSession: "s-live-3",
		Trip:     &TripReport{Detector: TripChurn, Detail: "4 writes to the same file in 6 minutes"},
		Messages: []string{"stop rewriting the table; write the test first"},
	}, []byte("the tail"), 7, 1.5, 2000)
	if !strings.HasPrefix(session, sessionOpening+"\n\n") {
		t.Fatalf("session statement does not open on the in-session sentence:\n%s", session)
	}
	for _, want := range []string{
		"Guidance delivered to the agent this session:\n- stop rewriting the table; write the test first",
		"at most 2000 characters",
		"The agent's report per finding is the last message in its record: what it fixed, disputed or left open. Verify it; never take it on trust.",
		"# The writing guide",
	} {
		if !strings.Contains(session, want) {
			t.Errorf("session statement lacks %q:\n%s", want, session)
		}
	}
	if strings.Contains(session, "\nFindings:\n") {
		t.Errorf("session statement renders the findings block:\n%s", session)
	}
	if strings.Contains(session, "/workspace/.factory/sessions/") {
		t.Errorf("session statement claims a pushed record copy the live pod does not hold:\n%s", session)
	}

	dead := checkinStatement(&CheckInAsk{
		Moment: CheckInDead, ParentSession: "s-dead-2",
		Stop: "wall clock with the work uncommitted",
	}, []byte("the tail"), 7, 1.5, 2000)
	if !strings.HasPrefix(dead, deadOpening+"\n\n") {
		t.Fatalf("dead statement does not open on the dead sentence:\n%s", dead)
	}
	if want := "Your handover note to the next session is at most 2000 characters. It names: what the earlier session did, the state of the branch, each finding with its verdict and the reason, what to do first, and what not to repeat."; !strings.Contains(dead, want) {
		t.Errorf("dead statement lacks the note's contents:\n%s", dead)
	}
	if strings.Contains(dead, "\nFindings:\n") {
		t.Errorf("dead statement renders the findings block:\n%s", dead)
	}
	if !strings.Contains(dead, "/workspace/.factory/sessions/s-dead-2.jsonl") {
		t.Errorf("dead statement does not name the pushed parent record:\n%s", dead)
	}
	// the dead moment writes its note for the next session, not for a
	// person, so it carries no writing guide
	if strings.Contains(dead, "The writing guide") {
		t.Errorf("dead statement carries the writing guide:\n%s", dead)
	}

	for _, s := range []struct{ name, text string }{{"gate", gate}, {"session", session}, {"dead", dead}} {
		if !strings.HasSuffix(s.text, "<<<SESSION-TAIL>>>\nthe tail\nSESSION-TAIL>>>\n\ncalls left 7, 1.50 EUR left for the check-in on this card") {
			t.Errorf("%s statement does not end in the shared tail and limits:\n%s", s.name, s.text)
		}
	}
	for _, c := range []struct{ text, other, name string }{
		{session, gateOpening, "session statement carries the gate opening"},
		{dead, gateOpening, "dead statement carries the gate opening"},
		{gate, sessionOpening, "gate statement carries the session opening"},
		{dead, sessionOpening, "dead statement carries the session opening"},
		{gate, deadOpening, "gate statement carries the dead opening"},
		{session, deadOpening, "session statement carries the dead opening"},
	} {
		if strings.Contains(c.text, c.other) {
			t.Error(c.name)
		}
	}
}

// A stuck trip's statement names the stuck rule that held after the
// delivered guidance, in the factory's words only, and what a handover
// does on it: the work moves to the stronger model off the top rung, and
// on it the next session starts from the note. The check-in decides with
// both.
func TestCheckinStatementNamesTheStuckRule(t *testing.T) {
	const detail = `tool read called 3 times with the same arguments {"filePath":"/w/a.go"} after 2 nudges`
	const stuck = "stuck after the supervisor's message: same_call: " + detail
	const climb = "- on this trip a handover moves the work to the stronger model, with your note as its next instruction.\n"
	const top = "- on this trip a handover ends the session, and the next session starts from your note.\n"
	statement := func(trip TripReport, messages ...string) string {
		return checkinStatement(&CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-4", Trip: &trip, Messages: messages},
			[]byte("the tail"), 7, 1.5, 2000)
	}
	got := statement(TripReport{Detector: TripSameCall, Detail: detail, Stuck: stuck, Climbs: true}, "stop reading")
	if want := "Guidance delivered to the agent this session:\n- stop reading\n" +
		"Stuck rule: after the message, the same_call detector tripped with no file written since.\n" + climb; !strings.Contains(got, want) {
		t.Fatalf("stuck statement lacks %q:\n%s", want, got)
	}
	if n := strings.Count(got, detail); n != 1 {
		t.Fatalf("the trip's detail appears %d times, want once, in the opening line:\n%s", n, got)
	}
	got = statement(TripReport{Detector: TripSameCall, Detail: detail, Stuck: stuck}, "stop reading")
	if want := "Stuck rule: after the message, the same_call detector tripped with no file written since.\n" + top; !strings.Contains(got, want) || strings.Contains(got, climb) {
		t.Fatalf("stuck statement on the top rung lacks %q or names the climb:\n%s", want, got)
	}
	got = statement(TripReport{Detector: TripChurn, Detail: "4 writes to the same file in 6 minutes", Stuck: "stuck after the supervisor's message: churn: 4 writes to the same file in 6 minutes", Climbs: true}, "stop reading")
	if want := "- stop reading\nStuck rule: after a message, the churn detector tripped.\n" + climb; !strings.Contains(got, want) {
		t.Fatalf("churn statement lacks %q:\n%s", want, got)
	}
	if got := statement(TripReport{Detector: TripRepetition, Detail: "60 tool calls and commands without a file change", Climbs: true}, "stop reading"); strings.Contains(got, "Stuck rule:") ||
		strings.Contains(got, climb) || strings.Contains(got, top) {
		t.Fatalf("statement without the stuck fact names it:\n%s", got)
	}
	if got := statement(TripReport{Detector: TripSameCall, Detail: detail, Stuck: stuck, Climbs: true}); strings.Contains(got, "Guidance delivered") ||
		!strings.Contains(got, "Stuck rule: after the message, the same_call detector tripped with no file written since.\n"+climb) {
		t.Fatalf("stuck statement without messages lists guidance or lacks the stuck rule:\n%s", got)
	}
}

// The gate persona is today's constant; the session and dead personas name
// their own answer shapes and none of the gate's verdict vocabulary.
func TestCheckinSystemPerMoment(t *testing.T) {
	if checkinSystemFor(CheckInGate) != checkinSystem {
		t.Fatal("the gate persona is not today's constant")
	}
	session := checkinSystemFor(CheckInSession)
	if !strings.Contains(session, `{"decision":"continue|message|handover|human","guidance":"..."`) {
		t.Fatalf("session persona lacks the answer's shape:\n%s", session)
	}
	if !strings.Contains(session, `"message" needs guidance`) || !strings.Contains(session, `"handover" stops the session`) {
		t.Fatalf("session persona does not carry the message and handover rules:\n%s", session)
	}
	dead := checkinSystemFor(CheckInDead)
	if !strings.Contains(dead, `{"decision":"handover|human","reason_kind":"..."`) {
		t.Fatalf("dead persona lacks the answer's shape:\n%s", dead)
	}
	for name, persona := range map[string]string{"session": session, "dead": dead} {
		if strings.Contains(persona, "verdict") || strings.Contains(persona, "dropped_all") {
			t.Errorf("the %s persona names the gate's verdict vocabulary:\n%s", name, persona)
		}
	}
}

// The answer's shape follows the moment: the session decides the session's
// next step and needs guidance for a message; the dead one only writes the
// handover or asks; the gate keeps today's decisions.
func TestParseCheckinAnswerMoments(t *testing.T) {
	session := &CheckInAsk{Moment: CheckInSession, ParentSession: "s-live-3"}
	a, err := parseCheckinAnswer(`{"decision":"continue","reason":"the write landed twice, not four times"}`, session)
	if err != nil || a.Decision != "continue" {
		t.Fatalf("continue = (%+v, %v), want it to parse", a, err)
	}
	if _, err := parseCheckinAnswer(`{"decision":"message","reason":"redo the test"}`, session); err == nil {
		t.Fatal("a message without guidance parsed")
	}
	a, err = parseCheckinAnswer(`{"decision":"message","guidance":"write the test first","reason":"redo the test"}`, session)
	if err != nil || a.Guidance != "write the test first" {
		t.Fatalf("message = (%+v, %v), want it to parse with its guidance", a, err)
	}
	a, err = parseCheckinAnswer(`{"decision":"handover","handover":"the state is on the branch"}`, session)
	if err != nil || len(a.Findings) != 0 {
		t.Fatalf("handover = (%+v, %v), want it to parse with no findings", a, err)
	}
	for _, text := range []string{`{"decision":"handover","reason":"stuck"}`, `{"decision":"handover","handover":" "}`} {
		if _, err := parseCheckinAnswer(text, session); err == nil || err.Error() != "a handover needs the note to the next session" {
			t.Errorf("the session ask's %s = %v, want the missing note named", text, err)
		}
	}
	for _, text := range []string{
		`{"decision":"dropped_all"}`,
		`{"decision":"continue","findings":[{"text":"TasksByPR is undefined","verdict":"confirmed"}]}`,
	} {
		if _, err := parseCheckinAnswer(text, session); err == nil {
			t.Errorf("the session ask parsed %s", text)
		}
	}
	_, err = parseCheckinAnswer(`{"decision":"continue","findings":[{"text":"TasksByPR is undefined","verdict":"confirmed"}]}`, session)
	if err == nil || !strings.Contains(err.Error(), `unknown finding "TasksByPR is undefined"`) {
		t.Fatalf("a session finding = %v, want unknown finding", err)
	}

	dead := &CheckInAsk{Moment: CheckInDead, ParentSession: "s-dead-2"}
	for _, text := range []string{
		`{"decision":"handover","handover":"n"}`,
		`{"decision":"human","reason_kind":"goal_conflict","reason":"r"}`,
	} {
		if _, err := parseCheckinAnswer(text, dead); err != nil {
			t.Errorf("the dead ask refused %s: %v", text, err)
		}
	}
	for _, text := range []string{
		`{"decision":"dropped_all"}`,
		`{"decision":"handover","findings":[{"text":"f","verdict":"confirmed"}]}`,
	} {
		if _, err := parseCheckinAnswer(text, dead); err == nil {
			t.Errorf("the dead ask parsed %s", text)
		}
	}

	gate := &CheckInAsk{Moment: CheckInGate, Findings: []CheckFinding{{Text: "f", Source: "review"}}}
	if _, err := parseCheckinAnswer(`{"decision":"continue","reason":"r"}`, gate); err == nil || !strings.Contains(err.Error(), `unknown decision "continue"`) {
		t.Fatalf("a gate continue = %v, want unknown decision", err)
	}
}

// The gate and dead check-ins may run a command in the workspace; the
// session's check-in may not, and gets the same list without it.
func TestCheckinToolsForMoments(t *testing.T) {
	for _, moment := range []string{CheckInGate, CheckInDead} {
		tools := checkinToolsFor(moment)
		if len(tools) != len(checkinTools) || tools[len(tools)-1].Name != "run" {
			t.Fatalf("%s tools = %d names, want the eight with run last", moment, len(tools))
		}
	}
	session := checkinToolsFor(CheckInSession)
	if len(session) != len(checkinTools)-1 {
		t.Fatalf("session tools = %d names, want the seven without run", len(session))
	}
	if !reflect.DeepEqual(session, checkinTools[:len(checkinTools)-1]) {
		t.Fatal("the session's tools are not the gate's list without run")
	}
}
