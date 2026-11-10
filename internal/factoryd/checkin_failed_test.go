package factoryd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	leakyURL    = "https://user:pass@git.example/sf/r.git"
	leakyBearer = "Authorization: Bearer abcdef0123456789abcdef"
)

func masked(t *testing.T, name, text string) {
	t.Helper()
	for _, leak := range []string{"user:pass", "abcdef0123456789abcdef"} {
		if strings.Contains(text, leak) {
			t.Errorf("%s leaks %q: %s", name, leak, text)
		}
	}
}

// A check-in that fails is the factory's own failure, and its stop's kind
// names its cause: a cap on time is checkin_time_cap, a cap on calls
// checkin_call_cap and an answer it could not give readably
// checkin_unreadable. None is spend_above_policy, which only the budget's
// share is.
func TestCheckinFailureKindsNameTheirCause(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInGate, ParentSession: "s-1"}
	t.Run("time cap", func(t *testing.T) {
		chat := &scriptedChat{delay: time.Second, answers: []CheckinResponse{{ToolName: "grep"}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000, 20*time.Millisecond)
		if out.Decision != CheckHuman || out.ReasonKind != CheckTimeCap || out.Record.ReasonKind != CheckTimeCap {
			t.Fatalf("outcome = %+v, record kind %q; want the time cap's kind", out, out.Record.ReasonKind)
		}
	})
	t.Run("call cap", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "card_facts"}, {ToolName: "card_facts"}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{out: map[string]string{"card_facts": "x"}}, 2, 1.5, 1, 2000, time.Hour)
		if out.ReasonKind != CheckCallCap || out.Record.ReasonKind != CheckCallCap {
			t.Fatalf("outcome = %+v; want the call cap's kind", out)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{{Text: "take my word"}, {Text: "take my word again"}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000, time.Hour)
		if out.Decision != CheckHuman || out.ReasonKind != CheckUnreadable || out.Record.ReasonKind != CheckUnreadable {
			t.Fatalf("outcome = %+v; want the unreadable answer's kind", out)
		}
	})
}

// A model call that fails after the cap ran out is the time cap's stop,
// not a refusal: the call's error stays out of the stop, and a refused call
// inside the cap hands over with no note and masks the credentials its
// error carries.
func TestCheckinCallErrorCases(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInGate, ParentSession: "s-1"}
	t.Run("error past the cap", func(t *testing.T) {
		chat := &scriptedChat{fails: 1, delay: 50 * time.Millisecond}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000, 10*time.Millisecond)
		if out.Decision != CheckHuman || out.ReasonKind != CheckTimeCap || out.Reason != "check-in time cap reached after 10ms" {
			t.Fatalf("outcome = %+v, want the time cap's stop", out)
		}
	})
	t.Run("refused inside the cap", func(t *testing.T) {
		chat := &errChat{err: errors.New("post " + leakyURL + ": 401 " + leakyBearer)}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000, time.Hour)
		if out.Decision != CheckHandover || out.Handover != "" || len(out.Verdicts) != 0 {
			t.Fatalf("outcome = %+v, want a handover with no note: the round runs as if no check-in existed", out)
		}
		if !strings.HasPrefix(out.Record.Reason, "check-in calls refused: ") {
			t.Fatalf("record reason = %q", out.Record.Reason)
		}
		masked(t, "the record's reason", out.Record.Reason)
	})
}

type errChat struct{ err error }

func (c *errChat) Call(context.Context, string, string, CheckinRequest) (CheckinResponse, error) {
	return CheckinResponse{}, c.err
}

// The unreadable answer's reason quotes what the model wrote, and the tool
// error the model reads quotes the tool's own: both go through the scrubber.
func TestCheckinTextsFromErrorsAreScrubbed(t *testing.T) {
	ask := &CheckInAsk{Moment: CheckInGate, ParentSession: "s-1", Findings: []CheckFinding{{Text: "f"}}}
	secret := leakyURL + " " + leakyBearer
	t.Run("unreadable answer", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{
			{Text: `{"findings":[{"id":"f1","verdict":"confirmed"},{"text":"` + secret + `","verdict":"confirmed"}],"decision":"handover"}`},
			{Text: `{"findings":[{"id":"f1","verdict":"confirmed"},{"text":"` + secret + `","verdict":"confirmed"}],"decision":"handover"}`}}}
		out := new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, scriptedTools{}, 12, 1.5, 1, 2000, time.Hour)
		if out.Decision != CheckHuman || !strings.HasPrefix(out.Reason, "check-in answer unreadable: ") {
			t.Fatalf("outcome = %+v", out)
		}
		masked(t, "the stop's reason", out.Reason)
		masked(t, "the repair round's prompt", chat.gots[1].Input)
	})
	t.Run("tool error", func(t *testing.T) {
		chat := &scriptedChat{answers: []CheckinResponse{{ToolName: "grep"}, {Text: `{"decision":"handover","handover":"x"}`}}}
		new(Runner).runCheckin(context.Background(), "t-1", "checkin", ask, nil, chat, errTools{errors.New("read " + secret)}, 12, 1.5, 1, 2000, time.Hour)
		masked(t, "the tool answer", chat.gots[1].Input)
	})
}

type errTools struct{ err error }

func (e errTools) Tool(context.Context, string, map[string]any) (string, bool, error) {
	return "", false, e.err
}

// The ask a failed check-in leaves opens with its label, and the case it
// opens is the failed check-in's, which the project agent answers, for each
// stop the check-in's template serves. Only the budget's share is spend. A
// card from before the caps had kinds of their own carries a cap under
// spend, and it is a failed check-in's case too.
func TestCheckinStopLabelsAndCases(t *testing.T) {
	for name, tc := range map[string]struct {
		reason string
		kind   CaseKind
		facts  map[string]any
	}{
		"share":              {"check-in: " + CheckSpend + ": " + CheckinShareSpent, CaseCheckinShare, nil},
		"call cap":           {"check-in: " + CheckCallCap + ": check-in call cap reached after 20 calls", CaseCheckinFailed, map[string]any{"reason_kind": CheckCallCap}},
		"time cap":           {"check-in: " + CheckTimeCap + ": check-in time cap reached after 10m0s", CaseCheckinFailed, map[string]any{"reason_kind": CheckTimeCap}},
		"unreadable":         {"check-in: " + CheckUnreadable + ": check-in answer unreadable: no JSON", CaseCheckinFailed, map[string]any{"reason_kind": CheckUnreadable}},
		"time cap, recorded": {"check-in: " + CheckSpend + ": check-in time cap reached after 10m0s", CaseCheckinFailed, map[string]any{"reason_kind": CheckTimeCap}},
		"call cap, recorded": {"check-in: " + CheckSpend + ": check-in call cap reached after 3 calls", CaseCheckinFailed, map[string]any{"reason_kind": CheckCallCap}},
		"human only":         {"check-in: " + CheckPolicyClass + ": the change touches a migration", CaseCheckinHuman, map[string]any{"reason_kind": CheckPolicyClass}},
	} {
		kind, facts := caseKindOf(escalationSource(tc.reason), tc.reason)
		if kind != tc.kind || len(facts) != len(tc.facts) || facts["reason_kind"] != tc.facts["reason_kind"] {
			t.Errorf("%s: caseKindOf = %s %v, want %s %v", name, kind, facts, tc.kind, tc.facts)
		}
	}
}

// Each failed check-in's stop gets the cap template's question, not the
// plain one, with no budget word in its why, and the share's stop keeps its
// own.
func TestCheckinFailureQuestions(t *testing.T) {
	for name, tc := range map[string]struct {
		reason, ask string
	}{
		"time cap": {"check-in: " + CheckTimeCap + ": check-in time cap reached after 10m0s", "The check-in gave no answer within its 10 minutes. May the card go on?"},
		"call cap": {"check-in: " + CheckCallCap + ": check-in call cap reached after 20 calls", "The check-in gave no answer within its 20 calls. May the card go on?"},
	} {
		q := ShapedQuestion(tc.reason, QuestionFacts{}, nil)
		if q.Ask != tc.ask {
			t.Errorf("%s: ask %q, want %q", name, q.Ask, tc.ask)
		}
		if strings.Contains(q.Why, "person") || !strings.Contains(q.Why, "the project agent answers it first") {
			t.Errorf("%s: why %q", name, q.Why)
		}
	}
}

// The face of a failed check-in's case says the factory's check-in failed
// and who answers first, not the bare reply line.
func TestCheckinFailedFace(t *testing.T) {
	got := tripNext(Case{Kind: CaseCheckinFailed}, "check-in: "+CheckTimeCap+": check-in time cap reached after 10m0s")
	if !strings.Contains(got, "check-in failed") || !strings.Contains(got, "project agent answers it first") {
		t.Fatalf("face = %q", got)
	}
}

// A check-in's time cap stop, whatever its label, still buys the session
// with the answer: the stop's kinds are one spend stop to the run, live and
// healed from a card asked before the labels changed.
func TestWorkflowCheckinCapStopsAnswerBuysTheSession(t *testing.T) {
	for name, reason := range map[string]string{
		"time cap": CheckTimeCap, "call cap": CheckCallCap, "recorded under spend": CheckSpend,
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			stop := "check-in time cap reached after 10m0s"
			h.onSessions(checkinStopped("s1", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: reason, Reason: stop,
				Record: &CheckInRecord{Moment: CheckInGate, Session: "s1"}}), pushed("s2", "c2"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" || len(h.sessions) != 2 {
				t.Fatalf("final = %q after %d sessions, want done after the stopped one and the one the answer bought", final, len(h.sessions))
			}
			if want := "check-in: " + reason + ": " + stop; len(h.escalated) == 0 || !strings.HasPrefix(h.escalated[0], want) {
				t.Fatalf("asked %q, want it to open %q", h.escalated, want)
			}
			if h.trips[0].CheckinStop != CheckinStopTimeCap {
				t.Fatalf("first trip's checkin_stop = %q, want %q", h.trips[0].CheckinStop, CheckinStopTimeCap)
			}
		})
	}
}

// A run restarted on a card waiting at a cap stop with a label of its own
// buys the session, as one with the recorded spend label does.
func TestWorkflowHealedCheckinCapStopBuysTheSession(t *testing.T) {
	for _, label := range []string{CheckTimeCap, CheckCallCap} {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "waiting-human", Question: "check-in: " + label + ": check-in call cap reached",
			PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
		var steps []string
		h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "one more check-in round", Comment: 602})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("%s: final = %q, finals = %v", label, final, h.finals)
		}
		if want := []string{"session 2 after 1 cards", "pr c3 after 1 cards", "approval c3"}; !slices.Equal(steps, want) {
			t.Fatalf("%s: steps = %v, want %v: the answer buys the session, not a verify of c2", label, steps, want)
		}
	}
}
