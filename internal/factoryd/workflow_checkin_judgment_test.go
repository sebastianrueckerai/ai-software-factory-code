package factoryd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
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
	var seen judgmentFlags
	replaySplice(t, hist.Events, &seen)
	if len(seen.flags) != 1 || !seen.flags[0] {
		t.Fatalf("flags %v, want the one session with the flag", seen.flags)
	}
}

// A share stop recorded before the change replays on the old branch: the
// recording holds no checkin-judgment marker, and every session the replay
// buys goes out with the flag false. The history is the audit-beside run
// with a share stop spliced in; its answer grants nothing.
func TestWorkflowReplaysAShareStopFromBeforeCheckinJudgment(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "checkin-judgment") {
		t.Fatal("the recording holds the checkin-judgment marker: it is no run from before")
	}
	var seen judgmentFlags
	replayCheckinAnswer(t, CheckinShareSpent, true, &seen)
	if len(seen.flags) == 0 || slices.Contains(seen.flags, true) {
		t.Fatalf("flags %v, want sessions replayed, none with the flag", seen.flags)
	}
}

// Plan runs recorded before the change send their planner sessions no flag.
func TestWorkflowReplaysAPlanRunFromBeforeCheckinJudgment(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-plan-question-answer-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "checkin-judgment") {
		t.Fatal("the recording holds the checkin-judgment marker: it is no run from before")
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var seen judgmentFlags
	replaySplice(t, base.Events, &seen)
	if len(seen.flags) != 3 || slices.Contains(seen.flags, true) {
		t.Fatalf("flags %v, want the 3 planner sessions without the flag", seen.flags)
	}
}

// judgmentFlags records the flag every replayed RunSession, RunReport and
// RunPlan carries.
type judgmentFlags struct {
	interceptor.WorkerInterceptorBase
	flags []bool
}

func (j *judgmentFlags) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &judgmentInbound{seen: j}
	i.Next = next
	return i
}

type judgmentInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	seen *judgmentFlags
}

func (i *judgmentInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &judgmentOutbound{seen: i.seen}
	o.Next = out
	return i.Next.Init(o)
}

type judgmentOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	seen *judgmentFlags
}

func (o *judgmentOutbound) ExecuteActivity(ctx workflow.Context, activityType string, args ...any) workflow.Future {
	switch activityType {
	case ActivityRunSession:
		o.seen.flags = append(o.seen.flags, args[0].(SessionInput).CheckinJudgment)
	case ActivityRunReport, ActivityRunPlan:
		o.seen.flags = append(o.seen.flags, args[len(args)-1].(bool))
	}
	return o.Next.ExecuteActivity(ctx, activityType, args...)
}
