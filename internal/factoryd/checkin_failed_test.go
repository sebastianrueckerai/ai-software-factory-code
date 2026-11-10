package factoryd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"

	"github.com/stretchr/testify/mock"
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
// opens is the failed check-in's, for each
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
		if strings.Contains(q.Why, "person") || !strings.Contains(q.Why, "The failure is the factory's own, not the card's work.") {
			t.Errorf("%s: why %q", name, q.Why)
		}
	}
}

// The face of a failed check-in's case says the factory's check-in failed
// and who answers first, not the bare reply line.
func TestCheckinFailedFace(t *testing.T) {
	got := tripNext(Case{Kind: CaseCheckinFailed}, "check-in: "+CheckTimeCap+": check-in time cap reached after 10m0s")
	if !strings.Contains(got, "check-in failed") || !strings.Contains(got, "not the card's work") || strings.Contains(got, "answers it first") {
		t.Fatalf("face = %q", got)
	}
}

// A check-in's time cap stop, whatever its label, still buys the session
// with the answer: the stop's kinds are one spend stop to the run, live and
// healed from a card asked before the labels changed.
func TestWorkflowCheckinCapStopsAnswerBuysTheSession(t *testing.T) {
	for name, tc := range map[string]struct{ kind, stop, checkinStop string }{
		"time cap":             {CheckTimeCap, "check-in time cap reached after 10m0s", CheckinStopTimeCap},
		"call cap":             {CheckCallCap, "check-in call cap reached after 20 calls", CheckinStopCallCap},
		"recorded under spend": {CheckSpend, "check-in time cap reached after 10m0s", CheckinStopTimeCap},
	} {
		reason, stop := tc.kind, tc.stop
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
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
			if h.trips[0].CheckinStop != tc.checkinStop {
				t.Fatalf("first trip's checkin_stop = %q, want %q", h.trips[0].CheckinStop, tc.checkinStop)
			}
		})
	}
}

// A run restarted on a card waiting at the repeat ask of a failed check-in
// (the restart that produced no new commit) with its pull request open
// verifies the open head, as it does at the same ask under the recorded
// spend label: no session is bought. Without the widened prefix the run
// reads the ask as unknown and buys a session.
func TestWorkflowHealedCheckinRepeatAskVerifiesTheOpenHead(t *testing.T) {
	for _, label := range []string{CheckSpend, CheckTimeCap, CheckCallCap} {
		t.Run(label, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.state = RunState{Status: "waiting-human", Question: "check-in: " + label + ": The restart produced no new commit - the head is still c2. What should the agent do?",
				PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
			var steps []string
			h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, finals = %v", final, h.finals)
			}
			if len(h.sessions) != 0 {
				t.Fatalf("sessions = %d (steps %v), want none: the answer verifies the open head c2", len(h.sessions), steps)
			}
		})
	}
}

// A second failed check-in at the same head keeps its own label and opens
// the failed check-in's case again: never spend_above_policy, and never the
// deadlock form, which says no reply can change a repeated failure of the
// factory's own check-in. A restart that pushed nothing after a cap stop
// is named as such under the cap's label.
func TestWorkflowRepeatedFailedCheckinKeepsItsLabel(t *testing.T) {
	for name, tc := range map[string]struct {
		kind, stop string
		restart    bool
	}{
		"time cap":   {CheckTimeCap, "check-in time cap reached after 10m0s", true},
		"call cap":   {CheckCallCap, "check-in call cap reached after 20 calls", true},
		"unreadable": {CheckUnreadable, "check-in answer unreadable: no JSON", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			stop := func(id, parent string) SessionResult {
				return checkinStopped(id, OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: tc.kind, Reason: tc.stop,
					Record: &CheckInRecord{Moment: CheckInGate, Session: id, ParentSession: parent}})
			}
			var steps []string
			h.onVerify(&steps, []SessionResult{pushed("s1", "c1"), stop("s2", "s1"), stop("s3", "s2")})
			h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
					return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
				})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			if final := h.run(t); final != "closed" || len(h.escalated) != 2 {
				t.Fatalf("final = %q, escalations = %q, want closed after the stop and its repeat", final, h.escalated)
			}
			second := h.escalated[1]
			if !strings.HasPrefix(second, "check-in: "+tc.kind+": ") || strings.Contains(second, CheckSpend) || strings.Contains(second, "No reply can change this") {
				t.Fatalf("the repeat ask = %q, want it under %s with no spend label and no deadlock form", second, tc.kind)
			}
			if tc.restart && !strings.Contains(second, "no new commit") {
				t.Fatalf("the repeat ask = %q, want the restart that pushed nothing named", second)
			}
			if kind, facts := caseKindOf(escalationSource(second), second); kind != CaseCheckinFailed || facts["reason_kind"] != tc.kind {
				t.Fatalf("the repeat's case = %s %v, want checkin_failed under %s", kind, facts, tc.kind)
			}
		})
	}
}

// A guard that spans the roll: a cap stop recorded under spend and the next
// one under its own kind are one stop to the repeat guard.
func TestStopFamilySpansTheLabelsOfTheRoll(t *testing.T) {
	for _, k := range []string{CheckSpend, CheckTimeCap, CheckCallCap} {
		if stopFamily(k) != stopFamily(CheckSpend) {
			t.Errorf("stopFamily(%s) = %q, want %q", k, stopFamily(k), stopFamily(CheckSpend))
		}
	}
	if stopFamily(CheckUnreadable) != "" || stopFamily(CheckPolicyClass) != CheckPolicyClass {
		t.Errorf("unreadable %q, policy_class %q", stopFamily(CheckUnreadable), stopFamily(CheckPolicyClass))
	}
}

// An unreadable stop recorded before the labels opens the failed
// check-in's case too, with the same fact.
func TestCaseKindOfReadsAnUnreadableStopRecordedBeforeTheRoll(t *testing.T) {
	reason := "check-in: gate: check-in answer unreadable: not one JSON object"
	kind, facts := caseKindOf(escalationSource(reason), reason)
	if kind != CaseCheckinFailed || facts["reason_kind"] != CheckUnreadable {
		t.Fatalf("case = %s %v, want checkin_failed unreadable", kind, facts)
	}
}

// A failed check-in's case is no agent-time stop: an extend of it is
// refused, the session resumes or the card hands over, and the card keeps
// the budget and spend facts its cap case carried.
func TestCheckinFailedCaseBuysNoAgentTime(t *testing.T) {
	f := CaseFacts{Kind: CaseCheckinFailed, PoolLeft: policy.Draw{Sessions: 5, AgentTime: 5 * time.Hour, EUR: 20},
		CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 4}, BudgetEUR: 5, LeftEUR: 3.5, CeilingEUR: 10, Attempts: 1, SessionCap: 3, AgentTimeCapS: 7200, AgentTimeS: 600}
	for name, args := range map[string]map[string]any{"agent time": {"agent_time_s": 600}, "a session": {"sessions": 1}} {
		if got := applies(t, ActExtend, f, Resolution{Args: args}); got != "this card asked a question, not for a session: answer it, or extend at its cap" {
			t.Errorf("extend of %s applies = %q, want the refusal", name, got)
		}
	}
	for _, name := range []string{ActResume, ActHandover} {
		if !slices.Contains(offeredNames(f, 2), name) {
			t.Errorf("rung 2 is not offered %s at a failed check-in", name)
		}
	}
	task := Task{ID: "t1", Repository: "o/r", BudgetEUR: 5, SpentEUR: 1.5}
	c, err := caseFromAsk(task, Event{Kind: EventQuestionAsked, Detail: map[string]any{"source": "supervisor", "question": "gave no answer",
		"reason": "check-in: " + CheckTimeCap + ": check-in time cap reached after 10m0s"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != CaseCheckinFailed || c.Money == nil || c.Money.BudgetEUR != 5 || c.Money.SpentEUR != 1.5 {
		t.Fatalf("case %s with money %+v, want the failed check-in's with the budget 5 and spend 1.5", c.Kind, c.Money)
	}
}

// The kinds only a human can act on keep the spend label at a repeat, as
// before: only the three failure kinds keep their own.
func TestRepeatAskKeepsTheSpendLabelForTheOtherKinds(t *testing.T) {
	for _, kind := range []string{CheckGoalConflict, CheckPolicyClass, CheckAccess, CheckSpend, ""} {
		prev := checkinStopMark{Kind: kind, Head: "c1", Tip: "c1", Granted: true}
		got := repeatAsk(prev, checkinStopMark{Kind: kind, Head: "c1", Tip: "c1"}, false)
		if !strings.HasPrefix(got, checkinSpendAsk) {
			t.Errorf("kind %q: repeat ask %q, want it under %q", kind, got, checkinSpendAsk)
		}
	}
}
