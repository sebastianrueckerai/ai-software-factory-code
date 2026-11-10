package factoryd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
// the branch head Reconcile read; a run from before wip-start starts from the
// base however the head reads.
func TestWorkflowHealedRunStartsFromTheBranchHead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gate  bool
		start string
	}{{"the gate adopts the head", true, "w2"}, {"before the gate keeps the base", false, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			if !tc.gate {
				h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
			}
			h.state = RunState{Status: "running", Head: "w2", Usage: TaskUsage{Attempts: 1}}
			h.onSessions(pushed("s2", "c3"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q, want done", final)
			}
			if got := h.sessions[0].StartRef; got != tc.start {
				t.Fatalf("first start = %q, want %q", got, tc.start)
			}
		})
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
	res["Commit"], res["Interim"] = "w1", true
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
