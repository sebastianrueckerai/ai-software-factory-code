package factoryd

import (
	"testing"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// A worker the sandbox killed for its memory leaves the kubelet's report
// of its container (OOMKilled, exit 137) and the events its log showed.
// opencode emits a bash call as a running tool call and an EventCommand
// only when it completes; a call the sandbox killed the worker in never
// completes. The result names the open call's command, else the last
// completed one.
func TestSessionKilledForMemoryNamesTheKillAndTheCommandItDiedIn(t *testing.T) {
	completed := func(id, cmd string) string {
		return envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(),
			Detail: map[string]any{"call_id": id, "command": cmd, "status": "completed", "exit": 0}}) + "\n"
	}
	running := func(id, cmd string) string {
		return envLine(harness.Event{Kind: harness.EventToolCall, Time: time.Now(),
			Detail: map[string]any{"call_id": id, "tool": "bash", "status": "running", "args": map[string]any{"command": cmd}}}) + "\n"
	}
	for name, tc := range map[string]struct{ logs, want string }{
		"killed in a running call":         {completed("c1", "gofmt -l .") + running("c2", "go test -race ./..."), "go test -race ./..."},
		"the running call completed first": {running("c1", "gofmt -l .") + completed("c1", "gofmt -l ."), "gofmt -l ."},
		"no command at all":                {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			task := Task{ID: "t6k", Status: "running", Objective: "fix it", Repository: "sf/repo", Track: "direct"}
			kube := newFakeKube()
			kube.onCreatePod = func() {
				kube.podOrder[len(kube.podOrder)-1].containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"},
					{Name: "worker", State: "terminated", Reason: "OOMKilled", ExitCode: 137}}
			}
			runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
			res, err := runSession(t, nil, runner, SessionInput{TaskID: "t6k", Attempt: 1, Model: "m"}, nil, tc.logs)
			if err != nil {
				t.Fatal(err)
			}
			want := &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "worker", Command: tc.want}
			if res.Killed == nil || *res.Killed != *want {
				t.Fatalf("Killed = %+v, want %+v", res.Killed, want)
			}
		})
	}
}

// A container that failed for another reason, or none, is no kill, and a
// kill with no command before it names none.
func TestSessionKillIsOnlyTheMemoryKill(t *testing.T) {
	oom := ContainerState{Name: "worker", State: "terminated", Reason: "OOMKilled", ExitCode: 137}
	for name, tc := range map[string]struct {
		d    PodDiagnosis
		want *SessionKill
	}{
		"oom":           {PodDiagnosis{Containers: []ContainerState{oom}}, &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "worker"}},
		"plain error":   {PodDiagnosis{Containers: []ContainerState{{Name: "worker", State: "terminated", Reason: "Error", ExitCode: 1}}}, nil},
		"exit 137 only": {PodDiagnosis{Containers: []ContainerState{{Name: "worker", State: "terminated", Reason: "Error", ExitCode: 137}}}, nil},
		"evicted":       {PodDiagnosis{Reason: "Evicted"}, nil},
		"none":          {PodDiagnosis{}, nil},
	} {
		got := killOf(tc.d, "")
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Errorf("%s: killOf = %+v, want %+v", name, got, tc.want)
		}
	}
}

// The memory kill of a session rides the stop that follows it into the
// trip's input; a run from before the change id carries none, and a later
// session that was not killed clears it.
func TestWorkflowCarriesTheSessionKillToTheStop(t *testing.T) {
	kill := &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "worker", Command: "go test -race ./..."}
	stopped := checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, Reason: "the session died twice",
		Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}})
	stopped.Killed = kill
	for name, tc := range map[string]struct {
		pinned bool
		want   *SessionKill
	}{"gated": {false, kill}, "before the change": {true, nil}} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			h.taskConfig = askTaskConfig()
			if tc.pinned {
				h.env.OnGetVersion("session-kill", workflow.DefaultVersion, versionSessionKill).Return(workflow.DefaultVersion)
			}
			deadSessions(h, SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped", Killed: kill}, stopped, pushed("s3", "c3"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
			if final := h.run(t); final != "done" || len(h.trips) == 0 {
				t.Fatalf("final = %q, trips %d", final, len(h.trips))
			}
			if got := h.trips[0].Killed; (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("the stop's kill = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A later session that was not killed clears the kill: the stop after it
// carries none.
func TestWorkflowLaterSessionClearsTheSessionKill(t *testing.T) {
	kill := &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "worker", Command: "go test -race ./..."}
	stopped := checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, Reason: "the session died twice",
		Record: &CheckInRecord{Moment: CheckInDead, Session: "s2", ParentSession: "s1"}})
	h := newWorkflowHarness(t)
	h.taskConfig = askTaskConfig()
	deadSessions(h, SessionResult{Outcome: OutcomeNoPush, SessionID: "s1", Summary: "stopped", Killed: kill}, stopped, pushed("s3", "c3"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go on", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || len(h.trips) == 0 {
		t.Fatalf("final = %q, trips %d", final, len(h.trips))
	}
	if h.trips[0].Killed != nil {
		t.Fatalf("the stop after an unkilled session carries %+v", h.trips[0].Killed)
	}
}
