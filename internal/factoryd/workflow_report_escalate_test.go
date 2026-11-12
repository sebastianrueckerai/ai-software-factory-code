package factoryd

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"
)

// A report run whose gate supervisor escalates asks the human at a case and
// runs the report again on the answer, once the answer's 30 s of quiet
// passed, at the start's model. A run from before the change fails with
// the escalation.
func TestWorkflowReportGateEscalationAsksAndRunsAgain(t *testing.T) {
	escalation := "The gate supervisor stopped RunReport (attempt 1) past its bound of 10m0s: the report session's pod waits for an image it cannot pull\n" +
		"Recommendation: check the registry pull secret"
	for _, before := range []bool{false, true} {
		h := newWorkflowHarness(t)
		h.state.Report = true
		var models []string
		var began []time.Time
		h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
			var taskID, model string
			if info.ActivityType.Name == ActivityRunReport && args.Get(&taskID, &model) == nil {
				models, began = append(models, model), append(began, h.env.Now())
			}
		})
		h.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).After(15*time.Minute).Return(ReportResult{}, nil).Once()
		h.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(ReportResult{}, nil)
		h.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the report session's pod waits for an image it cannot pull",
			Recommendation: "check the registry pull secret"})
		h.tripAfter(5*time.Minute, ActivityRunReport, 1)
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "fixed the secret", Comment: 700})
		start := h.env.Now()
		want, runs, states := [2]string{"done", "report kept on the task, no issue comment"}, 2, []string{"running", "running"}
		if before {
			h.env.OnGetVersion("report-escalate", workflow.DefaultVersion, versionReportEscalate).Return(workflow.DefaultVersion)
			want, runs, states = [2]string{"failed", escalation}, 1, []string{"running"}
		}
		h.run(t)
		if len(h.finals) != 1 || h.finals[0] != want || !slices.Equal(h.states, states) {
			t.Fatalf("before %v: finals %v, states %v; want %v, %v", before, h.finals, h.states, want, states)
		}
		if before {
			if len(h.escalated) != 0 || len(began) != runs {
				t.Fatalf("before the change: escalations %q, reports %d; want none and %d", h.escalated, len(began), runs)
			}
			continue
		}
		if !slices.Equal(h.escalated, []string{escalation}) || len(began) != runs || !began[1].Equal(start.Add(time.Hour+30*time.Second)) || models[1] != "factory/small" {
			t.Fatalf("escalations %q, reports at %v on %q; want the escalation, the report again at the answer on the start's model", h.escalated, began, models)
		}
	}
}

// A report run whose gate supervisor escalated, recorded on a dev server
// before the change: it failed with the escalation, and the replay
// schedules the activities it recorded with the inputs it recorded.
func TestWorkflowReplaysAReportGateEscalationFromBefore(t *testing.T) {
	hist := loadHistory(t, filepath.Join("testdata", "devserver-report-gate-escalate.json"))
	calls, inputs := scheduledActivities(hist)
	want := []string{ActivityTaskConfig, ActivityEnsureIssue, ActivityReconcile, ActivitySetState, ActivityRunReport, ActivitySuperviseGate, ActivityFinish}
	finish := `"t1"` + "\n" + `"failed"` + "\n" + `"The gate supervisor stopped RunReport (attempt 1) past its bound of 10m0s: the report session's pod waits for an image it cannot pull\nRecommendation: check the registry pull secret"`
	if !slices.Equal(calls, want) || inputs[len(inputs)-1] != finish {
		t.Fatalf("the recording schedules %q, its Finish %q; want %q and %q", calls, inputs[len(inputs)-1], want, finish)
	}
	if st := reconciled(t, hist); st["Report"] != true {
		t.Fatalf("the recorded Reconcile answer %v: want a report card", st)
	}
	var got, gotInputs []string
	replaySplice(t, hist.Events, &activitiesSeen{got: &got, inputs: &gotInputs})
	if !slices.Equal(got, calls) || !slices.Equal(gotInputs, inputs) {
		t.Fatalf("the replay scheduled %q with %q, the recording %q with %q", got, gotInputs, calls, inputs)
	}
}
