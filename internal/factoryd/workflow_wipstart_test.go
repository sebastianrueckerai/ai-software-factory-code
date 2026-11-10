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

// interimStop is a session that died after its check-ins pushed work in
// progress: the second interim head w2 is its Commit.
func interimStop(outcome SessionOutcome) SessionResult {
	return SessionResult{Outcome: outcome, SessionID: "s1", Commit: "w2", Interim: true, Summary: "the pod was killed",
		Findings: []string{"previous session ended unfinished: the pod was killed"}}
}

// A session that pushed and then ended without a result hands the pushed
// head to the next session as its start, with the dead session as parent.
// A run from before wip-start keeps the base (the empty start).
func TestWorkflowNextSessionStartsFromThePushedHead(t *testing.T) {
	for _, tc := range []struct {
		outcome SessionOutcome
		version workflow.Version
		start   string
	}{
		{OutcomeNoPush, versionWipStart, "w2"},
		{OutcomeRedirect, versionWipStart, "w2"},
		{OutcomeRetryStronger, versionWipStart, "w2"},
		{OutcomeProviderRefused, versionWipStart, "w2"},
		{OutcomeNoPush, workflow.DefaultVersion, ""},
		{OutcomeRedirect, workflow.DefaultVersion, ""},
	} {
		t.Run(string(tc.outcome)+"/v"+string(rune('0'+tc.version+1)), func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(tc.version)
			dead := interimStop(tc.outcome)
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
			if second.StartRef != tc.start || second.StartInterim != (tc.start != "") {
				t.Fatalf("second = start %q interim %v, want start %q", second.StartRef, second.StartInterim, tc.start)
			}
			if tc.outcome != OutcomeProviderRefused && second.ParentSessionID != "s1" {
				t.Fatalf("parent = %q, want s1", second.ParentSessionID)
			}
		})
	}
}

// A later session's own push is no interim head: its start carries no mark.
func TestWorkflowPushedStartIsNoInterimStart(t *testing.T) {
	h := newWorkflowHarness(t)
	real := SessionResult{Outcome: OutcomeRedirect, SessionID: "s1", Commit: "c1", Summary: "supervision trip: loop"}
	h.onSessions(real, pushed("s2", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if s := h.sessions[1]; s.StartRef != "c1" || s.StartInterim {
		t.Fatalf("second = start %q interim %v, want c1 pushed", s.StartRef, s.StartInterim)
	}
}

// A healed run whose card has no pull request starts its first session
// from the branch head Reconcile read.
func TestWorkflowHealedRunStartsFromTheBranchHead(t *testing.T) {
	for _, interim := range []bool{false, true} {
		h := newWorkflowHarness(t)
		h.state = RunState{Status: "running", Head: "w2", Interim: interim, Usage: TaskUsage{Attempts: 1}}
		h.onSessions(pushed("s2", "c3"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q, want done", final)
		}
		if s := h.sessions[0]; s.StartRef != "w2" || s.StartInterim != interim {
			t.Fatalf("first = start %q interim %v, want w2 with interim %v", s.StartRef, s.StartInterim, interim)
		}
	}
}

func TestWorkflowHealBeforeWipStartStartsFromTheBase(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("wip-start", workflow.DefaultVersion, versionWipStart).Return(workflow.DefaultVersion)
	h.state = RunState{Status: "running", Head: "w2", Usage: TaskUsage{Attempts: 1}}
	h.onSessions(pushed("s2", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if s := h.sessions[0]; s.StartRef != "" {
		t.Fatalf("first start = %q, want the base", s.StartRef)
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
	if len(starts) < 2 || starts[1].Attempt != 2 || starts[1].StartRef != "" || starts[1].StartInterim {
		t.Fatalf("starts = %+v, want the bought successor to start from the base", starts)
	}
}
