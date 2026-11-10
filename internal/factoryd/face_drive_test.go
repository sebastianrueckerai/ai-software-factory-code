package factoryd

import (
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

// What a reply does at each stop whose reply buys a session, driven through
// the run, with agent time left and with it spent. With time left the reply
// starts the next session, with the comment among its answers. Where the
// stop can be reached with the agent time spent, the reply starts none and
// the run asks the agent-time cap next. Where it cannot, the cap is asked
// before the stop ever is.
func TestReplyAtStopsThatBuyASession(t *testing.T) {
	escalate := func(reason string) func(*testing.T, bool) *workflowHarness {
		return func(t *testing.T, spent bool) *workflowHarness {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			took := time.Minute
			if spent {
				took = 20 * time.Minute
			}
			h.onTimedSessions(timedSession{took, SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Summary: "stopped", Reason: reason}},
				timedSession{time.Minute, pushed("s2", "c2")})
			return h
		}
	}
	for name, tc := range map[string]struct {
		build         func(*testing.T, bool) *workflowHarness
		spentSessions int
		asked         string
		// buysTime: the answer buys the session its agent time too
		buysTime bool
	}{
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
		for _, spent := range []bool{false, true} {
			label := name + " with time left"
			if spent {
				label = name + " with time spent"
			}
			t.Run(label, func(t *testing.T) {
				h := tc.build(t, spent)
				replyRun(t, h)
				if !askedWith(h, tc.asked) {
					t.Fatalf("asks %q, want the stop %q asked", h.escalated, tc.asked)
				}
				if !spent || tc.buysTime {
					last := h.sessions[len(h.sessions)-1]
					if len(h.sessions) < 2 || !strings.Contains(strings.Join(last.Answers, "\n"), "go on with this") {
						t.Fatalf("sessions %d, last %+v; want the reply to start a session with the comment as guidance", len(h.sessions), last.Answers)
					}
					return
				}
				if !capAsked(h) {
					t.Fatalf("asks %q, want the agent-time cap asked after the reply", h.escalated)
				}
				if len(h.sessions) != tc.spentSessions {
					t.Fatalf("sessions = %d, want %d: the reply starts none with the time spent", len(h.sessions), tc.spentSessions)
				}
			})
		}
	}
}

// A provider that refuses, and pods that never start, ask only when the
// card has agent time: they spend none, so a card whose time is spent asks
// its cap before either stop. With time left the reply starts the next
// session.
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
		})
		t.Run(name+" with time spent", func(t *testing.T) {
			h, asked := build(t, true)
			h.state = spentState()
			replyRun(t, h)
			if askedWith(h, asked) || !capAsked(h) || len(h.sessions) != 0 {
				t.Fatalf("asks %q, sessions %d; want the agent-time cap asked and the stop never reached", h.escalated, len(h.sessions))
			}
		})
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
