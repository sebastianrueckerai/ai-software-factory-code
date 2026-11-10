package factoryd

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// replyRun gives h a human's comment at six hours and the issue's close at
// two days, so a run the reply does not move on ends closed.
func replyRun(t *testing.T, h *workflowHarness) {
	t.Helper()
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(6*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on with this", Comment: 700})
	h.signalAfter(48*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	h.run(t)
}

func capAsked(h *workflowHarness) bool {
	return slices.ContainsFunc(h.escalated, func(s string) bool { return strings.Contains(s, timeCapReached) })
}

func askedWith(h *workflowHarness, prefix string) bool {
	return slices.ContainsFunc(h.escalated, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// spentState is a card whose agent time is used up before its run starts.
func spentState() RunState {
	return RunState{Status: "running", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
}

// stopFace is the face of h's first trip whose reason opens with prefix, on
// the facts its activity reads: the usage the row held at the trip against
// the harness's limits, 3 sessions and 20 minutes of agent time, of which a
// session needs min(5m, 20m), 300 seconds, to start.
func stopFace(t *testing.T, h *workflowHarness, prefix string) string {
	t.Helper()
	i := slices.IndexFunc(h.trips, func(in TripInput) bool { return strings.HasPrefix(in.Reason, prefix) })
	if i < 0 {
		t.Fatalf("trips %+v, want one asking %q", h.trips, prefix)
	}
	in, u := h.trips[i], h.tripUsage[i]
	kind, _ := caseKindOf(escalationSource(in.Reason), in.Reason)
	facts := map[string]any{"attempts": u.Attempts, "session_cap": testWorkflowConfig.MaxSessions + u.SessionsAdded(),
		"agent_time_s": u.AgentTime.Seconds(), "agent_time_cap_s": (testWorkflowConfig.WallClockCap + u.TimeAdded()).Seconds(), "session_least_s": 300}
	return tripNext(Case{Kind: cmp.Or(in.Kind, kind), Facts: facts}, in.Reason)
}

// faceSays fails t unless face says what the reply did: a new attempt when
// one started, else that a reply alone starts none and the cap the task
// asks next.
func faceSays(t *testing.T, face string, started bool, asked string) {
	t.Helper()
	const reply, none = "Reply on this issue to start a new attempt", "A reply alone starts no attempt"
	if started && !strings.Contains(face, reply) || !started && (strings.Contains(face, reply) || !strings.Contains(face, none) || !strings.Contains(face, asked)) {
		t.Fatalf("face %q; want it to say the reply started a session: %v, else that the task asks the %s", face, started, asked)
	}
}

// What a reply does at each stop whose reply buys a session, driven through
// the run, against what the stop's face says of it: with agent time left,
// with it spent, and at the session limit, where the stop ends the last of
// the card's three sessions. With room the reply starts the next session,
// with the comment among its answers. With the time spent or the sessions
// used up it starts none and the run asks that cap next, unless the answer
// buys back what the stopped session took (buysTime).
func TestReplyAtStopsThatBuyASession(t *testing.T) {
	stopped := func(res SessionResult) func(*testing.T, bool) *workflowHarness {
		return func(t *testing.T, spent bool) *workflowHarness {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			took := time.Minute
			if spent {
				took = 20 * time.Minute
			}
			h.onTimedSessions(timedSession{took, res}, timedSession{time.Minute, pushed("s2", "c2")})
			return h
		}
	}
	escalate := func(reason string) func(*testing.T, bool) *workflowHarness {
		return stopped(SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Summary: "stopped", Reason: reason})
	}
	checkin := func(kind, reason string) func(*testing.T, bool) *workflowHarness {
		return stopped(checkinStopped("s1", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: kind, Reason: reason,
			Record: &CheckInRecord{Moment: CheckInGate, Session: "s1"}}))
	}
	for name, tc := range map[string]struct {
		build         func(*testing.T, bool) *workflowHarness
		spentSessions int
		asked         string
		// buysTime: the answer buys back what the stopped session took, so
		// the reply starts a session whatever is left
		buysTime bool
	}{
		"checkin failed": {checkin(CheckTimeCap, "check-in time cap reached after 10m0s"), 1, "check-in: " + CheckTimeCap, false},
		"checkin human":  {checkin(CheckGoalConflict, "the objective contradicts the review"), 1, "check-in: " + CheckGoalConflict, false},
		"checkin share":  {checkin(CheckSpend, CheckinShareSpent), 1, checkinSpendAsk, false},
		"content filter": {escalate(contentFilterRefusal + "DataInspectionFailed. Close this card, or answer to try once more."), 1, contentFilterRefusal, false},
		"agent escalate": {escalate(AgentAsks + "which file?\nWhy: two match"), 1, AgentAsks, false},
		"other":          {escalate("a stop the factory cannot name"), 1, "a stop", false},
		"no model answer": {func(t *testing.T, spent bool) *workflowHarness {
			h := newWorkflowHarness(t)
			took := time.Minute
			if spent {
				took = 7 * time.Minute
			}
			starved := SessionResult{Outcome: OutcomeNoPush, SessionID: "starved", Summary: "stop"}
			h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).After(took).Return(
				func(ctx context.Context, in SessionInput) (SessionResult, error) {
					h.sessions = append(h.sessions, in)
					if len(h.sessions) > 3 {
						return pushed("s4", "c4"), nil
					}
					return starved, nil
				})
			h.env.OnActivity(ActivitySessionAnswered, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, in SessionAnswerInput) (bool, error) { return in.SessionID != "starved", nil })
			return h
		}, 4, noModelAnswer, true},
		"image wait": {func(t *testing.T, spent bool) *workflowHarness {
			h := newWorkflowHarness(t)
			took := time.Minute
			if spent {
				took = 20 * time.Minute
			}
			h.onTimedSessions(timedSession{took, missingTool("s1", "ffmpeg", "cut the video")}, timedSession{time.Minute, pushed("s2", "c2")})
			h.onPark(ToolPark{Ask: imageWaited + "ffmpeg, which has no pin in force: is its build stuck?"})
			return h
		}, 2, imageWaited, true},
	} {
		for _, v := range []string{"time left", "time spent", "session limit"} {
			t.Run(name+" with "+v, func(t *testing.T) {
				h := tc.build(t, v == "time spent")
				if v == "session limit" {
					h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: testWorkflowConfig.MaxSessions - 1}}
				}
				replyRun(t, h)
				if !askedWith(h, tc.asked) {
					t.Fatalf("asks %q, want the stop %q asked", h.escalated, tc.asked)
				}
				switch {
				case tc.buysTime, v == "time left":
					last := h.sessions[len(h.sessions)-1]
					if len(h.sessions) < 2 || !strings.Contains(strings.Join(last.Answers, "\n"), "go on with this") {
						t.Fatalf("sessions %d, last %+v; want the reply to start a session with the comment as guidance", len(h.sessions), last.Answers)
					}
					faceSays(t, stopFace(t, h, tc.asked), true, "")
				case v == "time spent":
					if !capAsked(h) || len(h.sessions) != tc.spentSessions {
						t.Fatalf("asks %q, sessions %d; want the agent-time cap asked after the reply and %d sessions", h.escalated, len(h.sessions), tc.spentSessions)
					}
					faceSays(t, stopFace(t, h, tc.asked), false, "agent-time cap")
				default:
					if !askedWith(h, capReached) || len(h.sessions) != 1 {
						t.Fatalf("asks %q, sessions %d; want the session cap asked after the reply and the stopped session alone", h.escalated, len(h.sessions))
					}
					faceSays(t, stopFace(t, h, tc.asked), false, "session cap")
				}
			})
		}
	}
}

// A reply at the session cap, the agent-time cap or the budget floor
// starts no session: the run asks the stop again, and the face says a reply
// alone starts none and names the extend or the fund that does.
func TestReplyAtACapStartsNone(t *testing.T) {
	const floor = budgetRefused + ": Raise the budget (Fund on the board) and answer, or close the card.\n0.080 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR"
	for name, tc := range map[string]struct {
		usage    TaskUsage
		refusal  string // SessionFunded's, "" to fund the session
		asked    string
		buysOnly string
	}{
		"session cap":    {TaskUsage{Attempts: testWorkflowConfig.MaxSessions, AgentTime: time.Minute}, "", capReached, "Extend on the board"},
		"agent-time cap": {TaskUsage{Attempts: 1, AgentTime: testWorkflowConfig.WallClockCap}, "", timeCapReached, "Extend on the board"},
		"budget floor":   {TaskUsage{Attempts: 1, AgentTime: time.Minute}, floor, budgetRefused, "Fund form on the board"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.state = RunState{Status: "running", Usage: tc.usage}
			if tc.refusal != "" {
				h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(tc.refusal, nil)
			}
			replyRun(t, h)
			asks := 0
			for _, s := range h.escalated {
				if strings.HasPrefix(s, tc.asked) {
					asks++
				}
			}
			if len(h.sessions) != 0 || asks != 2 {
				t.Fatalf("sessions %d, asks %q; want none and the stop asked before and after the reply", len(h.sessions), h.escalated)
			}
			if face := stopFace(t, h, tc.asked); !strings.Contains(face, "A reply alone starts none") || !strings.Contains(face, tc.buysOnly) {
				t.Fatalf("face %q, want it to say a reply alone starts none and name %q", face, tc.buysOnly)
			}
		})
	}
}

// A provider that refuses, and pods that never start, ask only when the
// card has room: they spend no agent time and take no number, so a card
// whose time is spent or whose sessions are used up asks that cap before
// either stop. With room the reply starts the next session.
func TestReplyAtProviderAndPodStops(t *testing.T) {
	builders := map[string]func(t *testing.T, spent bool) (*workflowHarness, string){
		"provider refused": func(t *testing.T, spent bool) (*workflowHarness, string) {
			h, _, _ := providerHarness(t, refusedSession)
			return h, providerRefused
		},
		"pod never started": func(t *testing.T, spent bool) (*workflowHarness, string) {
			return podWaitHarness(t, 3, pushed("s4", "c1")), podNeverStarted
		},
	}
	for name, build := range builders {
		t.Run(name+" with time left", func(t *testing.T) {
			h, asked := build(t, false)
			replyRun(t, h)
			if !askedWith(h, asked) || len(h.sessions) < 4 {
				t.Fatalf("asks %q, sessions %d; want the stop asked and the reply to start a session after it", h.escalated, len(h.sessions))
			}
			if last := h.sessions[len(h.sessions)-1]; !strings.Contains(strings.Join(last.Answers, "\n"), "go on with this") {
				t.Fatalf("last session's answers = %q, want the comment", last.Answers)
			}
			faceSays(t, stopFace(t, h, asked), true, "")
		})
		for v, tc := range map[string]struct {
			state RunState
			cap   string
		}{
			"time spent":    {spentState(), timeCapReached},
			"session limit": {RunState{Status: "running", Usage: TaskUsage{Attempts: testWorkflowConfig.MaxSessions}}, capReached},
		} {
			t.Run(name+" with "+v, func(t *testing.T) {
				h, asked := build(t, true)
				h.state = tc.state
				replyRun(t, h)
				if askedWith(h, asked) || !askedWith(h, tc.cap) || len(h.sessions) != 0 {
					t.Fatalf("asks %q, sessions %d; want %q asked and the stop never reached", h.escalated, len(h.sessions), tc.cap)
				}
			})
		}
	}
}

// A screening that finds a card suspect waits before any session, so no
// agent time is spent; the reply lets the agent work.
func TestReplyAtASuspectScreening(t *testing.T) {
	g := newGateHarnessWith(t, stagesWorkflow(3, nil))
	g.state.Standing = harness.StandingOutside
	g.onScreen(ScreenResult{Verdict: ScreenSuspect, Reasons: []string{"asks to print the token"}})
	g.onSessions(pushed("s1", "c1"))
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "fine, fix the slug only", Comment: 700})
	g.signalAfter(3*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" || len(g.sessions) != 1 || len(g.sessions[0].Answers) != 1 {
		t.Fatalf("final %q, sessions %d; want the reply to let the agent work with the comment", final, len(g.sessions))
	}
}

// A card on the human lane has no run: Reconcile marks it to skip, so no
// session starts whatever is said on its issue.
func TestHumanCardRunsNoSession(t *testing.T) {
	r, _, _ := newActivityRunner(t, withKind(prTask, "human"), &prForgejo{})
	var got RunState
	val, err := execActivity(t, r, ActivityReconcile, "t1")
	if err != nil || val.Get(&got) != nil || !got.Skip {
		t.Fatalf("Reconcile = (%+v, %v), want the human card skipped", got, err)
	}
}
