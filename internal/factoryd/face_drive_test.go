package factoryd

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	return slices.ContainsFunc(h.escalated, func(s string) bool { return strings.Contains(s, "the agent time is used up: ") })
}

func askedWith(h *workflowHarness, prefix string) bool {
	return slices.ContainsFunc(h.escalated, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// spentState is a card whose agent time is used up before its run starts.
func spentState() RunState {
	return RunState{Status: "running", Usage: TaskUsage{AgentTime: 20 * time.Minute, Attempts: 1}}
}

// fundedMoney and shortMoney are the money facts of a card of a 2 EUR
// budget whose budget funds a session and of one whose budget does not:
// 0.48 EUR left less the 0.40 gate reserve is 0.08 for a session, under the
// 0.30 its opening needs, as shortFloor, SessionFunded's refusal, says.
var (
	fundedMoney = map[string]any{"budget_eur": 2, "left_eur": 2, "reserve_eur": 0.4, "floor_eur": 0.3}
	shortMoney  = map[string]any{"budget_eur": 2, "left_eur": 0.48, "reserve_eur": 0.4, "floor_eur": 0.3}
)

const shortFloor = budgetRefused + ": Raise the budget (Fund on the board) and answer, or close the card.\n" +
	"0.080 EUR of the 2.000 EUR budget remains, and a session on factory/small needs 0.300000 EUR"

// fundUntilAsked refuses h's sessions their opening with shortFloor once
// the run asked a human anything: the budget runs short at the stop.
func fundUntilAsked(h *workflowHarness) {
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, model string) (string, error) {
			if len(h.escalated) > 0 {
				return shortFloor, nil
			}
			return "", nil
		})
}

// stopFace is the face of h's first trip whose reason opens with prefix, on
// the facts its activity reads: the usage the row held at the trip against
// the harness's limits, 3 sessions and 20 minutes of agent time, of which a
// session needs min(5m, 20m), 300 seconds, to start, the trip's head, and
// more, the facts the harness keeps no row of (money, an open pull request,
// a plan's).
func stopFace(t *testing.T, h *workflowHarness, prefix string, more map[string]any) string {
	t.Helper()
	i := slices.IndexFunc(h.trips, func(in TripInput) bool { return strings.HasPrefix(in.Reason, prefix) })
	if i < 0 {
		t.Fatalf("trips %+v, want one asking %q", h.trips, prefix)
	}
	in, u := h.trips[i], h.tripUsage[i]
	kind, _ := caseKindOf(escalationSource(in.Reason), in.Reason)
	facts := map[string]any{"attempts": u.Attempts, "session_cap": testWorkflowConfig.MaxSessions + u.SessionsAdded(),
		"agent_time_s": u.AgentTime.Seconds(), "agent_time_cap_s": (testWorkflowConfig.WallClockCap + u.TimeAdded()).Seconds(), "session_least_s": 300}
	if in.Head != "" {
		facts["head"] = in.Head
	}
	maps.Copy(facts, more)
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
// with it spent, at the session limit, where the stop ends the last of the
// card's three sessions, and with the budget short of a session from the
// stop on. With room the reply starts the next session, with the comment
// among its answers. With the time spent or the sessions used up it starts
// none and the run asks that cap next, unless the answer buys back what the
// stopped session took (buysTime). With the budget short it starts none
// and the run asks the budget floor, whatever the answer buys.
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
		// stopSessions: the sessions run when the stop asks
		stopSessions int
		asked        string
		// buysTime: the answer buys back what the stopped session took, so
		// the reply starts a session whatever is left
		buysTime bool
	}{
		"checkin failed": {checkin(CheckTimeCap, "check-in time cap reached after 10m0s"), 1, 1, "check-in: " + CheckTimeCap, false},
		"checkin human":  {checkin(CheckGoalConflict, "the objective contradicts the review"), 1, 1, "check-in: " + CheckGoalConflict, false},
		"checkin share":  {checkin(CheckSpend, spentStop), 1, 1, checkinSpendAsk, false},
		"content filter": {escalate(contentFilterRefusal + "DataInspectionFailed. Close this card, or answer to try once more."), 1, 1, contentFilterRefusal, false},
		"agent escalate": {escalate(AgentAsks + "which file?\nWhy: two match"), 1, 1, AgentAsks, false},
		"other":          {escalate("a stop the factory cannot name"), 1, 1, "a stop", false},
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
		}, 4, 3, noModelAnswer, true},
		"image wait": {func(t *testing.T, spent bool) *workflowHarness {
			h := newWorkflowHarness(t)
			took := time.Minute
			if spent {
				took = 20 * time.Minute
			}
			h.onTimedSessions(timedSession{took, missingTool("s1", "ffmpeg", "cut the video")}, timedSession{time.Minute, pushed("s2", "c2")})
			h.onPark(ToolPark{Ask: imageWaited + "ffmpeg, which has no pin in force: is its build stuck?"})
			return h
		}, 2, 1, imageWaited, true},
	} {
		for _, v := range []string{"time left", "time spent", "session limit", "budget short"} {
			t.Run(name+" with "+v, func(t *testing.T) {
				h := tc.build(t, v == "time spent")
				switch v {
				case "session limit":
					h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: testWorkflowConfig.MaxSessions - 1}}
				case "budget short":
					fundUntilAsked(h)
				}
				replyRun(t, h)
				if !askedWith(h, tc.asked) {
					t.Fatalf("asks %q, want the stop %q asked", h.escalated, tc.asked)
				}
				switch {
				case v == "budget short":
					if !askedWith(h, budgetRefused) || len(h.sessions) != tc.stopSessions {
						t.Fatalf("asks %q, sessions %d; want the budget floor asked after the reply and %d sessions", h.escalated, len(h.sessions), tc.stopSessions)
					}
					faceSays(t, stopFace(t, h, tc.asked, shortMoney), false, "budget floor")
				case tc.buysTime, v == "time left":
					last := h.sessions[len(h.sessions)-1]
					if len(h.sessions) < 2 || !strings.Contains(strings.Join(last.Answers, "\n"), "go on with this") {
						t.Fatalf("sessions %d, last %+v; want the reply to start a session with the comment as guidance", len(h.sessions), last.Answers)
					}
					faceSays(t, stopFace(t, h, tc.asked, fundedMoney), true, "")
				case v == "time spent":
					if !capAsked(h) || len(h.sessions) != tc.spentSessions {
						t.Fatalf("asks %q, sessions %d; want the agent-time cap asked after the reply and %d sessions", h.escalated, len(h.sessions), tc.spentSessions)
					}
					faceSays(t, stopFace(t, h, tc.asked, fundedMoney), false, "agent-time cap")
				default:
					if !askedWith(h, capReachedLadder) || len(h.sessions) != 1 {
						t.Fatalf("asks %q, sessions %d; want the session cap asked after the reply and the stopped session alone", h.escalated, len(h.sessions))
					}
					faceSays(t, stopFace(t, h, tc.asked, fundedMoney), false, "session cap")
				}
			})
		}
	}
}

// A reply at the session cap, the agent-time cap or the budget floor
// starts no session: the run asks the stop again, and the face says a reply
// alone starts none and names the extend or the fund that does.
func TestReplyAtACapStartsNone(t *testing.T) {
	for name, tc := range map[string]struct {
		usage    TaskUsage
		refusal  string // SessionFunded's, "" to fund the session
		asked    string
		buysOnly string
	}{
		"session cap":    {TaskUsage{Attempts: testWorkflowConfig.MaxSessions, AgentTime: time.Minute}, "", capReachedLadder, "Extend on the board"},
		"agent-time cap": {TaskUsage{Attempts: 1, AgentTime: testWorkflowConfig.WallClockCap}, "", timeCapReachedLadder, "Extend on the board"},
		"budget floor":   {TaskUsage{Attempts: 1, AgentTime: time.Minute}, shortFloor, budgetRefused, "Fund form on the board"},
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
			if face := stopFace(t, h, tc.asked, nil); !strings.Contains(face, "A reply alone starts none") || !strings.Contains(face, tc.buysOnly) {
				t.Fatalf("face %q, want it to say a reply alone starts none and name %q", face, tc.buysOnly)
			}
		})
	}
}

// A provider that refuses, and pods that never start, ask only when the
// card has room: they spend no agent time, take no number and no money, so
// a card whose time is spent, whose sessions are used up or whose budget
// funds no session asks that stop before either. With room the reply
// starts the next session.
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
			faceSays(t, stopFace(t, h, asked, fundedMoney), true, "")
		})
		for v, tc := range map[string]struct {
			state RunState
			cap   string
		}{
			"time spent":    {spentState(), timeCapReachedLadder},
			"session limit": {RunState{Status: "running", Usage: TaskUsage{Attempts: testWorkflowConfig.MaxSessions}}, capReachedLadder},
			"budget short":  {RunState{Status: "running", Usage: TaskUsage{Attempts: 1, AgentTime: time.Minute}}, budgetRefused},
		} {
			t.Run(name+" with "+v, func(t *testing.T) {
				h, asked := build(t, true)
				h.state = tc.state
				if tc.cap == budgetRefused {
					h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(shortFloor, nil)
				}
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
	if face := stopFace(t, g.workflowHarness, screeningAsks, nil); !strings.Contains(face, "let the agent work on it") {
		t.Fatalf("face %q, want it to say the reply lets the agent work", face)
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

// A trip carries the model the run's next session opens on, the one whose
// floor the stop's facts weigh: the start rung's after a plain stop, the
// stronger rung's after a retry_stronger, which the next session runs on.
// A run from before the change carries none.
func TestTripCarriesTheNextSessionsModel(t *testing.T) {
	climb := SessionResult{Outcome: OutcomeRetryStronger, SessionID: "s1", Summary: "too hard", Findings: []string{"needs the stronger rung"}}
	stop := SessionResult{Outcome: OutcomeEscalate, SessionID: "s2", Summary: "stopped", Reason: "a stop the factory cannot name"}
	for name, tc := range map[string]struct {
		sessions []SessionResult
		before   bool
		want     string
	}{
		"a plain stop":           {[]SessionResult{stop}, false, testWorkflowConfig.Models.Start},
		"after a retry_stronger": {[]SessionResult{climb, stop}, false, testWorkflowConfig.Models.Stronger},
		"a run from before":      {[]SessionResult{climb, stop}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			if tc.before {
				h.env.OnGetVersion("trip-model", workflow.DefaultVersion, versionTripModel).Return(workflow.DefaultVersion)
			}
			h.onSessions(tc.sessions...)
			h.signalAfter(48*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			h.run(t)
			i := slices.IndexFunc(h.trips, func(in TripInput) bool { return strings.HasPrefix(in.Reason, "a stop") })
			if i < 0 || h.trips[i].Model != tc.want {
				t.Fatalf("trips %+v; want the stop to carry %q", h.trips, tc.want)
			}
			if last := h.sessions[len(h.sessions)-1]; tc.want != "" && last.Model != tc.want {
				t.Fatalf("the stopped session ran on %q, want %q", last.Model, tc.want)
			}
		})
	}
}

// A check-in share stop at the gate, which follows the gates' findings on
// the open pull request's head c1: the reply takes c1 through the gates
// again, with no session, at the session limit too, and the face says so.
// The review's concerns on the first round send the card back; on the
// second it approves, and the card merges.
func TestReplyAtAShareStopWithThePullRequestOpen(t *testing.T) {
	for name, before := range map[string]int{"time left": 0, "session limit": testWorkflowConfig.MaxSessions - 2} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			h.state = RunState{Status: "running", Usage: TaskUsage{Attempts: before}}
			var steps []string
			h.onVerify(&steps, []SessionResult{
				pushed("s1", "c1"),
				checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: spentStop,
					Record: &CheckInRecord{Moment: CheckInGate, Session: "s2", ParentSession: "s1"}})})
			reviews := 0
			h.env.OnActivity(ActivityModelReview, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, taskID string, pr PullRequest, commit string, g GateResults, earlier []Review, round int) (Review, error) {
					if reviews++; reviews == 1 {
						return Review{Verdict: "concerns", Summary: "one fault", Findings: []string{"rename the flag"}}, nil
					}
					return Review{}, nil
				})
			h.signalAfter(2*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on with this", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" || len(h.sessions) != 2 || askedWith(h, capReachedLadder) || !slices.Contains(steps, "pr c1 after 1 cards") {
				t.Fatalf("final %q, sessions %d, steps %q, asks %q; want c1 through the gates again after the reply, no session", final, len(h.sessions), steps, h.escalated)
			}
			face := stopFace(t, h, checkinSpendAsk, map[string]any{"pr_open": true})
			if !strings.Contains(face, "through the gates again") || strings.Contains(face, "new attempt") || strings.Contains(face, "session cap") {
				t.Fatalf("face %q, want it to say the reply takes the head through the gates again", face)
			}
		})
	}
}

// A plan card's planner asks a human with its agent time short. While its
// automatic second session is still to come, the reply tops the time up to
// half the cap and the planner runs again; once that session is spent, the
// reply asks the planner's agent-time cap. The face, on the plan's facts,
// says which: the class's 20 minutes halved is 10, at least the 5 a
// session needs, so the top-up is due while RetryTime is 0.
func TestReplyAtAPlanStopByItsAutomaticSession(t *testing.T) {
	for name, tc := range map[string]struct {
		usage   TaskUsage
		planned int
		asked   string
	}{
		// 13m used, the planner's 5m make 18m: 2m left
		"second session to come": {TaskUsage{AgentTime: 13 * time.Minute}, 2, ""},
		// 20m + 8m topped up = 28m, 21m used, the planner's 5m make 26m: 2m left
		"second session spent": {TaskUsage{AgentTime: 21 * time.Minute, RetryTime: 8 * time.Minute}, 1, PlannerOutOfTime},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPlanHarness(t, PlanResult{Escalate: AgentAsks + "which plan?\nWhy: two fit"}, PlanResult{Revision: 1, SHA256: "sha-1"})
			h.state = RunState{Status: "running", Usage: tc.usage}
			h.file = map[int]bool{1: true}
			replyRun(t, h.workflowHarness)
			if len(h.plans) != tc.planned || tc.asked != "" && !askedWith(h.workflowHarness, tc.asked) {
				t.Fatalf("planner sessions %d, asks %q; want %d and %q", len(h.plans), h.escalated, tc.planned, tc.asked)
			}
			u := h.tripUsage[0]
			face := stopFace(t, h.workflowHarness, AgentAsks, map[string]any{"planner": true, "planner_retry": u.RetryTime == 0})
			faceSays(t, face, tc.asked == "", "agent-time cap")
		})
	}
}

// A stop on a dependency that failed or ended closed: the reply starts the
// card anyway, with the stop's reason as the session's finding, and the
// face says so.
func TestReplyAtADependencyStop(t *testing.T) {
	failed := Dependency{TaskID: "t-a", Issue: 5, Key: "links", Status: "failed"}
	closed := Dependency{TaskID: "t-a", Issue: 5, Key: "links", Status: "closed"}
	for name, st := range map[string]DepState{
		"failed": {Waiting: []int64{5}, Failed: &failed},
		"closed": {Waiting: []int64{5}, Ended: &closed},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = depsConfig()
			h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(st, nil)
			h.onSessions(pushed("s1", "c1"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the links are in already", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
			if final := h.run(t); final != "done" || len(h.escalated) != 1 || len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Findings, h.escalated) {
				t.Fatalf("final %q, asks %q, sessions %+v; want the card started anyway with the stop as its finding", final, h.escalated, h.sessions)
			}
			if face := stopFace(t, h, h.escalated[0], nil); !strings.Contains(face, "start the card anyway") {
				t.Fatalf("face %q, want it to say the reply starts the card anyway", face)
			}
		})
	}
}

// A check-in stop repeated on an unchanged head and unchanged limits is the
// deadlock. With sessions left a reply starts one, which the check-in stops
// again, so the deadlock repeats; with the last session run the reply asks
// the session cap. The face says which: 3 sessions ran (s1 and the two
// stopped ones) of 6, or of 3. The first stop's face, a goal conflict with
// the pull request open, says a reply starts a new attempt, as it did.
func TestReplyAtTheDeadlock(t *testing.T) {
	const deadlock = checkinSpendAsk + deadlockAskMark
	for name, sessions := range map[string]int{"sessions left": 6, "session limit": 3} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			tc := askTaskConfig()
			tc.Sessions = sessions
			h.taskConfig = tc
			stop := func(id string) SessionResult {
				return checkinStopped(id, OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
					Reason: "the objective contradicts the review", Record: &CheckInRecord{Moment: CheckInGate, Session: id}})
			}
			var steps []string
			h.onVerify(&steps, []SessionResult{pushed("s1", "c1"), stop("s2"), stop("s3"), stop("s4")})
			h.onReviews("c1")
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(3*time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on again", Comment: 603})
			h.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			h.run(t)
			if len(h.escalated) != 3 || !strings.HasPrefix(h.escalated[1], deadlock) {
				t.Fatalf("asks %q; want the stop, the deadlock and what the reply on it asks", h.escalated)
			}
			face := stopFace(t, h, deadlock, map[string]any{"session_cap": sessions})
			if sessions == 6 {
				// the first stop, a goal conflict on the open pull request's
				// head, is no spend stop: its reply started session 3
				first := stopFace(t, h, "check-in: "+CheckGoalConflict, map[string]any{"session_cap": sessions, "pr_open": true})
				faceSays(t, first, true, "")
				if !strings.HasPrefix(h.escalated[2], deadlock) || !strings.Contains(face, "A reply alone changes nothing") {
					t.Fatalf("asks %q, face %q; want the deadlock again and the face to say a reply changes nothing", h.escalated, face)
				}
				return
			}
			if !strings.HasPrefix(h.escalated[2], capReachedLadder) {
				t.Fatalf("asks %q; want the session cap after the reply", h.escalated)
			}
			faceSays(t, face, false, "session cap")
		})
	}
}

// A stop whose reply meets a day cap that binds before the card's short
// budget: the reply starts no session, the run waits on the day cap until
// the UTC day turns, then asks the budget floor. The face, on facts that
// hold the day cap's wait (its question line, lifting at the turn after the
// stop) and the short money, names the day cap first and the floor after
// it; the cap wait's own face, which waits on the cap, names the floor.
func TestReplyWhileADayCapBindsBeforeTheBudget(t *testing.T) {
	const state = "0.010000 EUR of the day's 20.00 EUR (budget.day_eur) is free, and a session on factory/small needs 0.300000 EUR"
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	// the reply at six hours lands before the UTC day turns
	h.env.SetStartTime(oct(14, 9, 0))
	h.onTimedSessions(timedSession{time.Minute, SessionResult{Outcome: OutcomeEscalate, SessionID: "s1", Summary: "stopped",
		Reason: contentFilterRefusal + "DataInspectionFailed. Close this card, or answer to try once more."}}, timedSession{time.Minute, pushed("s2", "c2")})
	h.env.OnActivity(ActivitySessionFunded, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, model string) (string, error) {
			switch {
			case len(h.escalated) == 0:
				return "", nil
			case h.env.Now().Before(dayTurn(h.escalatedAt[0])):
				return "the day cap cannot fund a call: Answer, and the attempt waits on its own for the UTC day's headroom, or raise budget.day_eur in policy.yaml by a commit.\n" + state, nil
			}
			return shortFloor, nil
		})
	replyRun(t, h)
	if len(h.sessions) != 1 || len(h.escalated) != 3 || !strings.HasPrefix(h.escalated[1], dayCapResumes) || !strings.HasPrefix(h.escalated[2], budgetRefused) ||
		h.escalatedAt[2].Before(dayTurn(h.escalatedAt[0])) {
		t.Fatalf("sessions %d, asks %q at %v; want the day cap's wait, then the budget floor at the day's turn, no session", len(h.sessions), h.escalated, h.escalatedAt)
	}
	capWait, _, _ := strings.Cut(capWaitReason(policy.RefusedDayCap, capFloor(policy.RefusedDayCap), state, dayTurn(h.escalatedAt[0])), "\n")
	more := maps.Clone(shortMoney)
	more["cap_wait"] = capWait
	face := stopFace(t, h, contentFilterRefusal, more)
	if !strings.HasPrefix(face, "The day cap is reached: the card resumes on its own") || !strings.Contains(face, stamp(dayTurn(h.escalatedAt[0]))) ||
		!strings.Contains(face, "Once it lifts, the task asks the budget floor") {
		t.Fatalf("face %q, want the day cap's wait first, then the budget floor", face)
	}
	if face := stopFace(t, h, dayCapResumes, more); !strings.Contains(face, "the task asks the budget floor next") || strings.Contains(face, "The day cap is reached") {
		t.Fatalf("the cap wait's face %q, want the budget floor after the wait", face)
	}
}
