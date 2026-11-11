package factoryd

import (
	"context"
	"errors"
	"maps"
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

// The face of a failed check-in's case says a reply starts a new attempt
// with the comment as guidance. Drive the reply through the run: a card with
// agent time left starts the session with the comment among its answers; a
// card whose agent time is spent starts none from the reply alone, and asks
// again at its agent-time cap.
func TestFailedCheckinReplyStartsASession(t *testing.T) {
	for name, tc := range map[string]struct {
		took     time.Duration
		sessions int
	}{
		"time left":  {time.Minute, 2},
		"time spent": {20 * time.Minute, 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			h.onTimedSessions(
				timedSession{tc.took, checkinStopped("s1", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckTimeCap,
					Reason: "check-in time cap reached after 10m0s", Record: &CheckInRecord{Moment: CheckInGate, Session: "s1"}})},
				timedSession{time.Minute, pushed("s2", "c2")})
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(24*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "try the narrow test", Comment: 602})
			h.signalAfter(48*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			h.run(t)
			if len(h.sessions) != tc.sessions {
				t.Fatalf("sessions = %d, want %d; asks %q", len(h.sessions), tc.sessions, h.escalated)
			}
			if tc.sessions == 2 && !strings.Contains(strings.Join(h.sessions[1].Answers, "\n"), "try the narrow test") {
				t.Fatalf("second session's answers = %q, want the comment as guidance", h.sessions[1].Answers)
			}
			if tc.sessions == 1 && (len(h.escalated) != 2 || !strings.Contains(h.escalated[1], timeCapReached)) {
				t.Fatalf("asks = %q, want the agent-time cap asked after the reply", h.escalated)
			}
		})
	}
}

// The face names what starts the attempt by the card's room: a reply where
// a session fits, the cap's Extend or the Fund form where none does, for
// the stops whose reply buys a session. The facts are the run's rule: a
// 7200 s cap, 3 sessions, and 300 s a session needs to start; no money
// facts read as a budget that funds one.
func TestFailedStopFaceByAgentTime(t *testing.T) {
	facts := func(attempts int, used float64) map[string]any {
		return map[string]any{"attempts": attempts, "session_cap": 3, "agent_time_cap_s": 7200, "agent_time_s": used, "session_least_s": 300}
	}
	for _, kind := range []CaseKind{CaseCheckinFailed, CaseCheckinHuman, CaseCheckinShare} {
		room := tripNext(Case{Kind: kind, Facts: facts(1, 600)}, "check-in: x: y")
		if !strings.Contains(room, "Reply on this issue to start a new attempt") {
			t.Errorf("%s with time left: %q", kind, room)
		}
		// 7200s cap less 6900s leaves 300s, which is minSession: room; 6901s leaves 299s: none
		if strings.Contains(tripNext(Case{Kind: kind, Facts: facts(1, 6900)}, "x"), "A reply alone starts no attempt") {
			t.Errorf("%s with exactly a session left says none fits", kind)
		}
		spent := tripNext(Case{Kind: kind, Facts: facts(1, 6901)}, "x")
		if !strings.Contains(spent, "A reply alone starts no attempt") || strings.Contains(spent, "Reply on this issue to start") {
			t.Errorf("%s with time spent: %q", kind, spent)
		}
		// 6900.5s used leaves 299.5s, under 300s: none, though whole seconds read 300
		if half := tripNext(Case{Kind: kind, Facts: facts(1, 6900.5)}, "x"); !strings.Contains(half, "the agent-time cap next") {
			t.Errorf("%s with half a second short: %q", kind, half)
		}
		// the third of 3 sessions ran: none, with the time left
		if limit := tripNext(Case{Kind: kind, Facts: facts(3, 600)}, "x"); !strings.Contains(limit, "the session cap next") || strings.Contains(limit, "Reply on this issue to start") {
			t.Errorf("%s at the session limit: %q", kind, limit)
		}
		// 0.48 EUR left less the 0.40 reserve is 0.08, under the 0.30 an opening needs
		short := facts(1, 600)
		short["left_eur"], short["reserve_eur"], short["floor_eur"] = 0.48, 0.4, 0.3
		if got := tripNext(Case{Kind: kind, Facts: short}, "x"); !strings.Contains(got, "the budget floor next") || !strings.Contains(got, "the Fund form on the board") {
			t.Errorf("%s with the budget short: %q", kind, got)
		}
		// a plan's loop reads no opening floor: its reply starts the planner
		short["planner"] = true
		if got := tripNext(Case{Kind: kind, Facts: short}, "x"); !strings.Contains(got, "Reply on this issue to start a new attempt") {
			t.Errorf("%s of a plan with the budget short: %q", kind, got)
		}
		short["partial"] = []any{"floor"}
		if got := tripNext(Case{Kind: kind, Facts: short}, "x"); !strings.Contains(got, "Reply on this issue to start a new attempt") {
			t.Errorf("%s of a plan with its floor unread: %q", kind, got)
		}
		delete(short, "partial")
		short["planner"], short["floor_free"] = false, true
		if got := tripNext(Case{Kind: kind, Facts: short}, "x"); !strings.Contains(got, "Reply on this issue to start a new attempt") {
			t.Errorf("%s with no price source and the budget short: %q", kind, got)
		}
		floorUnread := facts(1, 600)
		floorUnread["partial"] = []any{"floor"}
		if got := tripNext(Case{Kind: kind, Facts: floorUnread}, "x"); strings.Contains(got, "Reply on this issue to start") || !strings.Contains(got, "the Fund form") {
			t.Errorf("%s with the floor unread: %q", kind, got)
		}
		unread := facts(1, 600)
		delete(unread, "agent_time_cap_s")
		if got := tripNext(Case{Kind: kind, Facts: unread}, "x"); strings.Contains(got, "Reply on this issue to start") || !strings.Contains(got, "Extend on the board") {
			t.Errorf("%s with the cap unread: %q", kind, got)
		}
	}
}

// A reply during a cap's wait, the day's, the month's or the project's,
// starts the next session sooner when the card has room, and starts none
// when its agent time is spent, its last session ran or its own budget no
// longer funds a session: the run asks that stop after the reply. The
// wait's face says which. The capped session made calls, so it counts.
func TestCapWaitReplyByAgentTime(t *testing.T) {
	const project = "free headroom 0.000001 EUR (task 2.000000, day 12.000000, month 40.000000, project 0.000001); " +
		"the project cap budget.month_eur in projects/linkshort.yaml is spent, and only a human raises it, by a commit"
	for capName, c := range map[string]struct {
		refusal CapRefusal
		outcome SessionOutcome
		wait    string
	}{
		"day":     {dayRefusal, OutcomeDayCap, "the day cap is reached"},
		"month":   {monthRefusal, OutcomeEscalate, "the month cap is reached"},
		"project": {CapRefusal{Refusal: policy.Refusal{Decision: policy.RefusedProjectCap, Reason: project}, At: oct31(21, 10, 0)}, OutcomeEscalate, "the project's month cap is reached"},
	} {
		worked := c.refusal
		worked.Allowed = true
		capped := SessionResult{Outcome: c.outcome, SessionID: "s1", Reason: capRefused(worked.Refusal), Capped: &worked}
		for name, tc := range map[string]struct {
			took     time.Duration
			before   int // the attempts before the capped session
			sessions int
			cap      string
			asked    string
		}{
			"time left":     {10 * time.Minute, 0, 2, "", ""},
			"time spent":    {20 * time.Minute, 0, 1, "agent-time cap", timeCapReached},
			"session limit": {10 * time.Minute, testWorkflowConfig.MaxSessions - 1, 1, "session cap", capReached},
			"budget short":  {10 * time.Minute, 0, 1, "budget floor", budgetRefused},
		} {
			t.Run(capName+" cap with "+name, func(t *testing.T) {
				h, _ := capHarness(t, tc.took, capped, pushed("s2", "c1"))
				h.env.SetStartTime(worked.At)
				h.state = RunState{Usage: TaskUsage{Attempts: tc.before}}
				money := fundedMoney
				if tc.asked == budgetRefused {
					fundUntilAsked(h)
					money = shortMoney
				}
				h.onChecks(ChecksResult{State: ChecksSuccess})
				h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 700})
				h.signalAfter(48*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
				h.run(t)
				if len(h.sessions) != tc.sessions {
					t.Fatalf("sessions = %d, want %d; asks %q", len(h.sessions), tc.sessions, h.escalated)
				}
				if tc.sessions == 1 && !slices.ContainsFunc(h.escalated, func(s string) bool { return strings.Contains(s, tc.asked) }) {
					t.Fatalf("asks %q, want the %s asked after the reply", h.escalated, tc.cap)
				}
				face := stopFace(t, h, c.wait, money)
				if tc.sessions == 2 && !strings.Contains(face, "start a new attempt sooner") ||
					tc.sessions == 1 && (!strings.Contains(face, "A reply alone starts no attempt") || !strings.Contains(face, tc.cap) || strings.Contains(face, "sooner")) {
					t.Fatalf("face %q, want it to say what the reply does", face)
				}
			})
		}
	}
}

// Every case kind's face against what a reply does, which the test named
// in each row drives through the run (no test drives a row no trip
// reaches: unreached says why). A kind whose reply buys a session names the
// reply with room, the cap's Extend at the agent-time cap or the session
// limit, and the Fund form with the budget short of a session, after the
// wait on a day, month or project cap that binds first; a share
// stop's with the pull request open takes its head through the gates
// again; a plan's with its automatic session to come starts it; the gate's
// reply runs the gate again; a dependency's starts the card anyway; the
// deadlock's changes nothing while a session fits. The facts are the run's
// rule: a 7200 s cap, 3 sessions, 300 s a session needs to start, and 0.30
// EUR its opening.
func TestEveryFaceMatchesWhatAReplyDoes(t *testing.T) {
	const reply = "Reply on this issue to start a new attempt"
	const none = "A reply alone starts no attempt"
	facts := func(attempts, used int, left float64) map[string]any {
		return map[string]any{"attempts": attempts, "session_cap": 3, "agent_time_cap_s": 7200, "agent_time_s": used, "session_least_s": 300,
			"left_eur": left, "reserve_eur": 0.4, "floor_eur": 0.3}
	}
	// short: 0.48 EUR left less the 0.40 reserve is 0.08, under the 0.30 an opening needs
	room, spent, limit, short := facts(1, 600, 2), facts(1, 7000, 2), facts(3, 600, 2), facts(1, 600, 0.48)
	with := func(f map[string]any, more map[string]any) map[string]any {
		f = maps.Clone(f)
		maps.Copy(f, more)
		return f
	}
	open := map[string]any{"pr_open": true, "head": "c1"}
	dayFirst, _, _ := strings.Cut(capWaitReason(policy.RefusedDayCap, capFloor(policy.RefusedDayCap), "x", time.Now().Add(time.Hour)), "\n")
	capped := map[string]any{"cap_wait": dayFirst}
	// a plan's automatic second session tops its spent time up
	planRetry, planSpent := map[string]any{"planner": true, "planner_retry": true}, map[string]any{"planner": true}
	covered := map[CaseKind]bool{}
	defer func() {
		for _, name := range caseKindNames {
			if !covered[CaseKind(name)] {
				t.Errorf("case kind %s has no row in the face table", name)
			}
		}
	}()
	dayWait := capWaitReason(policy.RefusedDayCap, capFloor(policy.RefusedDayCap), "x", time.Now().Add(time.Hour))
	monthWait := capWaitReason(policy.RefusedMonthCap, capFloor(policy.RefusedMonthCap), "x", time.Now().Add(time.Hour))
	projectWait := capWaitReason(policy.RefusedProjectCap, capFloor(policy.RefusedProjectCap), "x", time.Now().Add(time.Hour))
	gate := gateStopped + ActivityModelReview + " after 3 tries"
	for _, tc := range []struct {
		kind      CaseKind
		reason    string
		facts     map[string]any
		want      string
		notWant   string
		drive     func(*testing.T)
		unreached string
	}{
		{CaseCheckinFailed, "x", room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinFailed, "x", spent, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinFailed, "x", limit, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinFailed, "x", short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinHuman, "x", room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinHuman, "x", spent, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinHuman, "x", limit, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinHuman, "x", short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinShare, "x", room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinShare, "x", spent, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinShare, "x", limit, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinShare, "x", short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseCheckinShare, "x", with(room, open), "through the gates again", "new attempt", TestReplyAtAShareStopWithThePullRequestOpen, ""},
		{CaseCheckinShare, "x", with(limit, open), "through the gates again", "session cap", TestReplyAtAShareStopWithThePullRequestOpen, ""},
		{CasePodNeverStarted, podNeverStarted, room, reply, none, TestReplyAtProviderAndPodStops, ""},
		{CasePodNeverStarted, podNeverStarted, spent, none, reply, TestReplyAtProviderAndPodStops, ""},
		{CasePodNeverStarted, podNeverStarted, limit, none, reply, TestReplyAtProviderAndPodStops, ""},
		{CasePodNeverStarted, podNeverStarted, short, none, reply, TestReplyAtProviderAndPodStops, ""},
		{CaseProviderRefused, providerRefused, room, reply, none, TestReplyAtProviderAndPodStops, ""},
		{CaseProviderRefused, providerRefused, spent, none, reply, TestReplyAtProviderAndPodStops, ""},
		{CaseProviderRefused, providerRefused, limit, none, reply, TestReplyAtProviderAndPodStops, ""},
		{CaseProviderRefused, providerRefused, short, none, reply, TestReplyAtProviderAndPodStops, ""},
		{CaseNoAnswer, noModelAnswer, room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseNoAnswer, noModelAnswer, spent, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseNoAnswer, noModelAnswer, short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseImageWait, imageWaited, room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseImageWait, imageWaited, spent, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseImageWait, imageWaited, limit, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseImageWait, imageWaited, short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseContentFilter, contentFilterRefusal, room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseContentFilter, contentFilterRefusal, spent, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseContentFilter, contentFilterRefusal, limit, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseContentFilter, contentFilterRefusal, short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseContentFilter, contentFilterRefusal, with(short, capped), "The day cap is reached", reply, TestReplyWhileADayCapBindsBeforeTheBudget, ""},
		{CaseDayCap, dayWait, with(short, capped), "the budget floor next", "The day cap is reached", TestReplyWhileADayCapBindsBeforeTheBudget, ""},
		{CaseAgentEscalate, AgentAsks, room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseAgentEscalate, AgentAsks, spent, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseAgentEscalate, AgentAsks, limit, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseAgentEscalate, AgentAsks, short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseAgentEscalate, AgentAsks, with(spent, planRetry), reply, none, TestReplyAtAPlanStopByItsAutomaticSession, ""},
		{CaseAgentEscalate, AgentAsks, with(spent, planSpent), none, reply, TestReplyAtAPlanStopByItsAutomaticSession, ""},
		{CaseOther, "x", room, reply, none, TestReplyAtStopsThatBuyASession, ""},
		{CaseOther, "x", spent, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseOther, "x", limit, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseOther, "x", short, none, reply, TestReplyAtStopsThatBuyASession, ""},
		{CaseScreeningSuspect, screeningAsks + "x", room, "let the agent work on it", "new attempt", TestReplyAtASuspectScreening, ""},
		{CaseHumanCard, "x", room, "No attempt starts", reply, TestHumanCardRunsNoSession, ""},
		{CaseWaitExpired, staleQuiet, room, "on its own form", "Reply on this issue", nil, "nothing writes a stale ask"},
		{CaseMergeApproval, "x", room, "on its own form", "Reply on this issue", nil, "answered on the approval, never by a trip"},
		{CaseAgentRequestInfo, AgentAsks, room, "on its own form", "Reply on this issue", nil, "the poster's wait, never a trip"},
		{CasePlanQuestion, "x", room, "on its own form", "Reply on this issue", nil, "answered on the plan's questions, never by a trip"},
		{CasePlanConfirmation, "x", room, "on its own form", "Reply on this issue", nil, "answered on the plan's questions, never by a trip"},
		{CaseDayCap, dayWait, room, "start a new attempt sooner", none, TestCapWaitReplyByAgentTime, ""},
		{CaseDayCap, dayWait, spent, none, "start a new attempt sooner", TestCapWaitReplyByAgentTime, ""},
		{CaseDayCap, dayWait, limit, none, "start a new attempt sooner", TestCapWaitReplyByAgentTime, ""},
		{CaseDayCap, dayWait, short, none, "start a new attempt sooner", TestCapWaitReplyByAgentTime, ""},
		{CaseMonthCap, monthWait, room, "start a new attempt sooner", none, TestCapWaitReplyByAgentTime, ""},
		{CaseMonthCap, monthWait, limit, none, "start a new attempt sooner", TestCapWaitReplyByAgentTime, ""},
		{CaseProjectCap, projectWait, spent, none, "start a new attempt sooner", TestCapWaitReplyByAgentTime, ""},
		{CaseGateStop, gate, room, "run the gate again", "new attempt", TestWorkflowGateEscalationAnswerRunsTheGateAgain, ""},
		{CaseGateStop, gate, spent, "run the gate again", "new attempt", TestWorkflowGateEscalationAnswerRunsTheGateAgain, ""},
		{CaseDependencyClosed, "dependency #1 (a) ended closed", room, "start the card anyway", "new attempt", TestReplyAtADependencyStop, ""},
		{CaseDependencyFailed, depFailed + "#1", spent, "start the card anyway", "new attempt", TestReplyAtADependencyStop, ""},
		{CaseSessionCap, capReached, limit, "A reply alone starts none", reply, TestReplyAtACapStartsNone, ""},
		{CaseAgentTimeCap, timeCapReached, spent, "A reply alone starts none", reply, TestReplyAtACapStartsNone, ""},
		{CaseBudgetFloor, budgetRefused, room, "A reply alone starts none", reply, TestReplyAtACapStartsNone, ""},
		{CaseMonitorFlag, "x", room, "The card goes on meanwhile", reply, TestMonitorFacesAQuietRunBesideIt, ""},
		{CaseFindingsRepeated, deadlockAskMark, room, "A reply alone changes nothing", reply, TestReplyAtTheDeadlock, ""},
		{CaseFindingsRepeated, deadlockAskMark, limit, none, "changes nothing", TestReplyAtTheDeadlock, ""},
	} {
		covered[tc.kind] = true
		if (tc.drive == nil) == (tc.unreached == "") {
			t.Errorf("%s: a row names a drive or why no trip reaches it, not both", tc.kind)
		}
		got := tripNext(Case{Kind: tc.kind, Facts: tc.facts}, tc.reason)
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.notWant) {
			t.Errorf("%s (%v): face %q, want %q and not %q", tc.kind, tc.facts, got, tc.want, tc.notWant)
		}
	}
}
