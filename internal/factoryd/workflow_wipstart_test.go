package factoryd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	client "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/workflow"
)

// deadAfterPushing is a session that pushed work in progress to commit w2
// and ended on outcome.
func deadAfterPushing(outcome SessionOutcome) SessionResult {
	return SessionResult{Outcome: outcome, SessionID: "s1", Commit: "w2", Summary: "the pod was killed",
		Findings: []string{"previous session ended unfinished: the pod was killed"}}
}

// A session that pushed and then ended without a verdict hands the pushed
// head to the next session as its start, with itself as the parent. A run
// from before wip-start keeps the base (the empty start) and, as before, the
// refused session's parent.
func TestWorkflowNextSessionStartsFromThePushedHead(t *testing.T) {
	for _, tc := range []struct {
		outcome SessionOutcome
		gate    bool
	}{
		{OutcomeNoPush, true}, {OutcomeRedirect, true}, {OutcomeRetryStronger, true},
		{OutcomeProviderRefused, true}, {OutcomeDayCap, true},
		{OutcomeNoPush, false}, {OutcomeRedirect, false}, {OutcomeDayCap, false},
	} {
		name := string(tc.outcome) + " from before the gate keeps the base"
		if tc.gate {
			name = string(tc.outcome) + " hands its pushed head on"
		}
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			if !tc.gate {
				h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
			}
			dead := deadAfterPushing(tc.outcome)
			if tc.outcome == OutcomeProviderRefused {
				dead.Status = 429
			}
			h.onSessions(dead, pushed("s2", "c3"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, want done", final)
			}
			if len(h.sessions) < 2 {
				t.Fatalf("sessions = %d, want 2", len(h.sessions))
			}
			second := h.sessions[1]
			want := ""
			if tc.gate {
				want = "w2"
			}
			if second.StartRef != want {
				t.Fatalf("second start = %q, want %q", second.StartRef, want)
			}
			if tc.gate && second.ParentSessionID != "s1" {
				t.Fatalf("second parent = %q, want s1, the session whose head it starts from", second.ParentSessionID)
			}
		})
	}
}

// A healed run whose card has no pull request starts its first session from
// the branch head Reconcile read, whether the card was running or waiting on
// a human; a run from before wip-start starts from the base however the head
// reads.
func TestWorkflowHealedRunStartsFromTheBranchHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  RunState
		answer bool
	}{
		{"a running card", RunState{Status: "running", Head: "w2", Usage: TaskUsage{Attempts: 1}}, false},
		{"a card waiting on a human", RunState{Status: "waiting-human", Question: "which alias?", Head: "w2", Usage: TaskUsage{Attempts: 1}}, true},
	} {
		for _, gate := range []bool{true, false} {
			name, start := tc.name+" adopts the head at the gate", "w2"
			if !gate {
				name, start = tc.name+" keeps the base before the gate", ""
			}
			t.Run(name, func(t *testing.T) {
				h := newWorkflowHarness(t)
				if !gate {
					h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
				}
				h.state = tc.state
				h.onSessions(pushed("s2", "c3"))
				h.onChecks(ChecksResult{State: ChecksSuccess})
				if tc.answer {
					h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
				}
				h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

				if final := h.run(t); final != "done" {
					t.Fatalf("final = %q, want done", final)
				}
				if got := h.sessions[0].StartRef; got != start {
					t.Fatalf("first start = %q, want %q", got, start)
				}
			})
		}
	}
}

// A healed run whose open pull request holds an interim push as its head
// does not take that head through the gates as finished work: a session
// continues from it.
func TestWorkflowHealedRunDoesNotVerifyAnInterimHead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "running", PR: PullRequest{Number: 9}, Head: "w2", Unfinished: true, Usage: TaskUsage{Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if want := []string{"session 2 after 0 cards", "pr c3 after 0 cards", "approval c3"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v: no gate on w2", steps, want)
	}
	if h.sessions[0].StartRef != "w2" {
		t.Fatalf("first start = %q, want the interim head w2", h.sessions[0].StartRef)
	}
}

// startsSeen records the SessionInput of each RunSession a replay schedules.
type startsSeen struct {
	interceptor.WorkerInterceptorBase
	got *[]SessionInput
}

func (c startsSeen) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &startsInbound{got: c.got}
	i.Next = next
	return i
}

type startsInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	got *[]SessionInput
}

func (i *startsInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &startsOutbound{got: i.got}
	o.Next = out
	return i.Next.Init(o)
}

type startsOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	got *[]SessionInput
}

func (o *startsOutbound) ExecuteActivity(ctx workflow.Context, activityType string, args ...any) workflow.Future {
	if activityType == ActivityRunSession {
		*o.got = append(*o.got, args[0].(SessionInput))
	}
	return o.Next.ExecuteActivity(ctx, activityType, args...)
}

// A history recorded before wip-start carries no marker of it: its first
// session, spliced to end no_push with a pushed commit (the dead pod's last
// interim head), buys the recorded successor, which starts from the base as
// before, and the replay leaves no command unmatched.
func TestWorkflowReplaysAPushedDeadSessionFromBeforeWipStart(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "wip-start") {
		t.Fatal("the recording holds a wip-start marker: it is no run from before")
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ev := base.Events
	if len(ev) != 368 || ev[151].GetActivityTaskScheduledEventAttributes().GetActivityId() != "RunSession-1" ||
		ev[181].GetActivityTaskScheduledEventAttributes().GetActivityId() != "RunSession-2" {
		t.Fatalf("recorded history drifted: %d events", len(ev))
	}
	for _, e := range ev {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"burst-accept"` {
			id[0].Data = []byte(`"blind-proofs"`)
		}
	}
	done := ev[153].GetActivityTaskCompletedEventAttributes()
	var res map[string]any
	if err := json.Unmarshal(done.GetResult().GetPayloads()[0].GetData(), &res); err != nil {
		t.Fatal(err)
	}
	if res["Outcome"] != string(OutcomeNoPush) || res["Commit"] != "" {
		t.Fatalf("recorded first session = %v, want no_push with no commit", res)
	}
	res["Commit"] = "w1"
	rb, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	done.Result.Payloads[0].Data = rb

	var starts []SessionInput
	replaySplice(t, ev, &startsSeen{got: &starts})
	if len(starts) < 2 || starts[1].Attempt != 2 || starts[1].StartRef != "" {
		t.Fatalf("starts = %+v, want the bought successor to start from the base", starts)
	}
}

// An interim head is no finished work: where a path would take it through
// the gates, the gate's refusal sends the card on in a session that starts
// from that head, and the card never fails on it. A run from before the
// gate fails on the refusal, as before.
func TestWorkflowContinuesInASessionWhereAGateRefusesAnInterimHead(t *testing.T) {
	spendStop := SessionResult{SessionID: "s2", Outcome: OutcomeCheckinHuman, Commit: "w2", Trip: string(TripNoProgress), Reason: CheckinShareSpent,
		CheckIn: &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent, Record: &CheckInRecord{Moment: CheckInSession, Session: "s2"}}}
	for _, tc := range []struct {
		name     string
		sessions []SessionResult
		state    RunState
		answer   bool
		checks   []ChecksResult // none: green
		// the heads that open a pull request, in order, and the session that ran last
		prs     []string
		started string
	}{
		{"a dropped check-in on an interim head",
			[]SessionResult{deadAfterPushing(OutcomeNoPush), checkinStopped("s2", OutcomeCheckinDropped, &CheckInOutcome{Decision: CheckDroppedAll}), pushed("s3", "c3")},
			RunState{}, false, nil, []string{"w2", "c3"}, "w2"},
		{"the answer to a spend stop whose end push did not land",
			[]SessionResult{pushed("s1", "c1"), spendStop, pushed("s3", "c3")},
			RunState{}, true, []ChecksResult{gosecRed, {State: ChecksSuccess}}, []string{"c1", "w2", "c3"}, "w2"},
		{"a healed card waiting on a spend stop with an interim head",
			[]SessionResult{pushed("s2", "c3")},
			RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: the check-in's share of the budget is spent",
				PR: PullRequest{Number: 9}, Head: "w2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}, true, nil, []string{"w2", "c3"}, "w2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.beforeCases()
			h.taskConfig = askTaskConfig()
			h.state = tc.state
			h.wipHeads = []string{"w2"}
			var steps []string
			h.onVerify(&steps, tc.sessions, tc.checks...)
			if tc.answer {
				h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			}
			h.signalAfter(48*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q (%v), want done: no failure on an interim head", final, h.finals)
			}
			var prs []string
			for _, s := range steps {
				if head, ok := strings.CutPrefix(s, "pr "); ok {
					prs = append(prs, strings.Fields(head)[0])
				}
			}
			if !slices.Equal(prs, tc.prs) {
				t.Fatalf("pull requests opened on %v, want %v (steps %v, escalated %q)", prs, tc.prs, steps, h.escalated)
			}
			var starts []string
			for _, in := range h.sessions {
				starts = append(starts, in.StartRef)
			}
			if got := h.sessions[len(h.sessions)-1].StartRef; got != tc.started {
				t.Fatalf("session starts = %v, want the last from %s, the interim head", starts, tc.started)
			}
		})
	}
}

// A run from before the gate meets the refusal as before: it fails on it.
func TestWorkflowBeforeWipStartFailsWhereAGateRefusesAnInterimHead(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
	h.state = RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: the check-in's share of the budget is spent",
		PR: PullRequest{Number: 9}, Head: "w2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	h.beforeCases()
	h.wipHeads = []string{"w2"}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})

	if final := h.run(t); final != "failed" || !strings.Contains(h.finals[0][1], "interim push") {
		t.Fatalf("final = %q (%v), want failed on the refusal", final, h.finals)
	}
}
