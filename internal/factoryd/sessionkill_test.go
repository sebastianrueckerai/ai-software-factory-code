package factoryd

import (
	"testing"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// A worker the sandbox killed for its memory leaves the kubelet's report
// of its container (OOMKilled, exit 137) and the commands its log showed.
// The session's result names the kill and the last command the session
// ran, which was the one the sandbox killed it in.
func TestSessionKilledForMemoryNamesTheKillAndTheLastCommand(t *testing.T) {
	task := Task{ID: "t6k", Status: "running", Objective: "fix it", Repository: "sf/repo", Track: "direct"}
	kube := newFakeKube()
	kube.onCreatePod = func() {
		kube.podOrder[len(kube.podOrder)-1].containers = []ContainerState{{Name: "clone", State: "terminated", Reason: "Completed"},
			{Name: "worker", State: "terminated", Reason: "OOMKilled", ExitCode: 137}}
	}
	runner, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	logs := envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "gofmt -l .", "exit": 0}}) + "\n" +
		envLine(harness.Event{Kind: harness.EventCommand, Time: time.Now(), Detail: map[string]any{"command": "go test -race ./..."}}) + "\n"
	res, err := runSession(t, nil, runner, SessionInput{TaskID: "t6k", Attempt: 1, Model: "m"}, nil, logs)
	if err != nil {
		t.Fatal(err)
	}
	want := &SessionKill{Reason: "OOMKilled", ExitCode: 137, Container: "worker", Command: "go test -race ./..."}
	if res.Killed == nil || *res.Killed != *want {
		t.Fatalf("Killed = %+v, want %+v", res.Killed, want)
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
