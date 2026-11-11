package factoryd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"
)

// A run under checkin-judgment sends the flag with every session's input,
// the gate moment's included; a run from before sends none.
func TestWorkflowCheckinJudgmentMarksEverySessionInput(t *testing.T) {
	for name, before := range map[string]bool{"under the change": false, "from before it": true} {
		t.Run(name, func(t *testing.T) {
			finding := "a.go: x is missing"
			g := newGateHarness(t, func(g *gateHarness) {
				g.taskConfig = askTaskConfig()
				g.reviews = []Review{{Verdict: "concerns", Summary: "the file lacks the check", Findings: []string{finding}}}
				if before {
					g.env.OnGetVersion("checkin-judgment", workflow.DefaultVersion, versionCheckinJudgment).Return(workflow.DefaultVersion)
				}
			})
			g.onSessions(pushed("s1", "c1"),
				checkinStopped("s2", OutcomeCheckinHuman, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckGoalConflict,
					Reason: "the objective conflicts with the guardrails"}))
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(24*time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			if final := g.run(t); final != "closed" {
				t.Fatalf("final = %q, want closed", final)
			}
			if len(g.sessions) != 2 || g.sessions[1].CheckInAsk == nil {
				t.Fatalf("sessions = %d, want the push and the gate moment's pod", len(g.sessions))
			}
			for i, in := range g.sessions {
				if in.CheckinJudgment == before {
					t.Errorf("session %d: CheckinJudgment = %v", i+1, in.CheckinJudgment)
				}
			}
		})
	}
}

// A run recorded on a dev server after the change carries its marker and
// sends the flag with its session's input; it replays on today's code.
func TestWorkflowReplaysAHistoryRecordedUnderCheckinJudgment(t *testing.T) {
	f := filepath.Join("testdata", "devserver-checkin-judgment-session.json")
	hist := loadHistory(t, f)
	var marked, flagged bool
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-judgment"` {
			marked = true
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a.GetActivityType().GetName() == ActivityRunSession {
			var in SessionInput
			if err := converter.GetDefaultDataConverter().FromPayloads(a.GetInput(), &in); err != nil {
				t.Fatal(err)
			}
			flagged = in.CheckinJudgment
		}
	}
	if !marked || !flagged {
		t.Fatalf("marker %v, flag %v: the recording is no run under the change", marked, flagged)
	}
	replayHistory(t, f)
}

// A share stop recorded before the change replays on the old branch: the
// recording holds no checkin-judgment marker, and its answer still grants
// the share the stop asked for.
func TestWorkflowReplaysAShareStopFromBeforeCheckinJudgment(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "checkin-judgment") {
		t.Fatal("the recording holds the checkin-judgment marker: it is no run from before")
	}
	replayCheckinAnswer(t, CheckinShareSpent, true)
}
