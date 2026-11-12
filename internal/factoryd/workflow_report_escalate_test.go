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

// The human's answer to a report's gate stop, a Continue with a note from
// the console, reaches the report that runs again, tagged with the stop's
// question: the stop promised the comment as guidance.
func TestWorkflowReportRunsAgainWithTheAnswerToItsStop(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state.Report = true
	var answers [][]string
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		var taskID, model string
		var pin int64
		var got []string
		if info.ActivityType.Name == ActivityRunReport && args.Get(&taskID, &model, &pin, &got) == nil {
			answers = append(answers, got)
		}
	})
	h.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).After(15*time.Minute).Return(ReportResult{}, nil).Once()
	h.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).Return(ReportResult{}, nil)
	h.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the report session's pod waits for an image it cannot pull",
		Recommendation: "check the registry pull secret"})
	h.tripAfter(5*time.Minute, ActivityRunReport, 1)
	h.signalAfter(time.Hour, SignalCaseResolved, TaskSignal{Author: "sebastian", At: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
		Case: &CaseSignal{CaseID: 701, Rung: 3, By: "sebastian", Res: Resolution{Action: ActContinue, Note: "pull from the mirror instead"}}})
	h.run(t)
	want := `sebastian (2026-11-12), to "The gate supervisor stopped RunReport (attempt 1) past its bound of 10m0s: the report session's pod waits for an image it cannot pull": pull from the mirror instead`
	if len(h.finals) != 1 || h.finals[0][0] != "done" || len(answers) != 2 || len(answers[0]) != 0 || !slices.Equal(answers[1], []string{want}) {
		t.Fatalf("finals %v, the reports' answers %q; want done, the first report with none and the second with [%q]", h.finals, answers, want)
	}
}

// A report card's run restarted after the console answered its stop hands
// the answer Reconcile rebuilt to its first report.
func TestWorkflowReportRestartedAfterAnAnswerCarriesIt(t *testing.T) {
	h := newWorkflowHarness(t)
	answer := `sebastian (2026-11-12), to "The gate supervisor stopped RunReport (attempt 1) past its bound of 10m0s: the session made no call": size it from the handler alone`
	h.state.Report, h.state.Answers = true, []string{answer}
	var answers [][]string
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		var taskID, model string
		var pin int64
		var got []string
		if info.ActivityType.Name == ActivityRunReport && args.Get(&taskID, &model, &pin, &got) == nil {
			answers = append(answers, got)
		}
	})
	if final := h.run(t); final != "done" || len(answers) != 1 || !slices.Equal(answers[0], []string{answer}) {
		t.Fatalf("final %q, the reports' answers %q; want done after one report with [%q]", final, answers, answer)
	}
}

// A report card's run restarted while its stop's case is open waits on that
// case: at a gate stop it asks again (healedReason) and runs no report
// until the human's answer resolves the case, then runs it with the
// answer. At the day cap it asks the cap's reason again and runs the report
// at the cap's own end, the UTC day's turn at 00:00, before the human's
// answer at 30 h. Its restored wait checks the day cap at once at 20:43 on
// the report's model, then every five minutes from 20:48 to 23:58, then at
// the turn: 41 checks. A Close at the gate stop ends the card closed with no
// report: the card has no issue, so Act signals issue_closed before it
// resolves the case. A run from before runs the report at once.
func TestWorkflowReportRestartedAtAnOpenStopWaits(t *testing.T) {
	gate := "The gate supervisor stopped RunReport (attempt 1) past its bound of 10m0s: the report session's pod waits for an image it cannot pull"
	atGate := RunState{Report: true, Status: "waiting-human", Question: gate}
	continued := TaskSignal{Author: "sebastian", At: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
		Case: &CaseSignal{CaseID: 701, Rung: 3, By: "sebastian", Res: Resolution{Action: ActContinue, Note: "pull from the mirror instead"}}}
	for _, tc := range []struct {
		name    string
		start   time.Time
		state   RunState
		before  bool
		answer  time.Duration // when the human's answer comes
		close   bool          // the answer is a Close, else a Continue with a note
		final   string
		asked   []string
		reports []time.Time
		answers [][]string  // each report's answers
		checks  []time.Time // when CapResume ran
		check   CapCheck    // the first CapResume's input
	}{
		{name: "a gate stop", start: oct(14, 9, 0), state: atGate, answer: time.Hour, final: "done", asked: []string{healedReason},
			reports: []time.Time{oct(14, 10, 0)}, answers: [][]string{{`sebastian (2026-11-12), to "` + gate + `": pull from the mirror instead`}}},
		{name: "a gate stop, from before", start: oct(14, 9, 0), state: atGate, before: true, answer: time.Hour, final: "done",
			reports: []time.Time{oct(14, 9, 0)}, answers: [][]string{nil}},
		{name: "the day cap", start: oct(14, 20, 43), answer: 30 * time.Hour, final: "done",
			state: RunState{Report: true, Status: "waiting-human", Question: dayQuestion, Cap: dayReason, CapUntil: oct(15, 0, 0)},
			asked: []string{dayReason}, reports: []time.Time{oct(15, 0, 0)}, answers: [][]string{nil},
			checks: slices.Concat([]time.Time{oct(14, 20, 43)}, every(oct(14, 20, 48), oct(14, 23, 58), dayCheck), []time.Time{oct(15, 0, 0)}),
			check:  CapCheck{TaskID: "t1", Model: "factory/small", Decision: "refused_day_cap"}},
		{name: "a close at a gate stop", start: oct(14, 9, 0), state: atGate, answer: time.Hour, close: true, final: "closed", asked: []string{healedReason}},
	} {
		h := newWorkflowHarness(t)
		h.env.SetStartTime(tc.start)
		h.state = tc.state
		var began, checked []time.Time
		var answers [][]string
		var checks []CapCheck
		h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
			var taskID, model string
			var pin int64
			var got []string
			var in CapCheck
			switch {
			case info.ActivityType.Name == ActivityRunReport && args.Get(&taskID, &model, &pin, &got) == nil:
				began, answers = append(began, h.env.Now()), append(answers, got)
			case info.ActivityType.Name == ActivityCapResume && args.Get(&in) == nil:
				checked, checks = append(checked, h.env.Now()), append(checks, in)
			}
		})
		if tc.close {
			h.signalAfter(tc.answer, SignalIssueClosed, TaskSignal{Author: "sebastian"})
			h.signalAfter(tc.answer, SignalCaseResolved, TaskSignal{Author: "sebastian",
				Case: &CaseSignal{CaseID: 701, Rung: 3, By: "sebastian", Res: Resolution{Action: ActClose}}})
		} else {
			h.signalAfter(tc.answer, SignalCaseResolved, continued)
		}
		if tc.before {
			h.env.OnGetVersion("report-open-stop", workflow.DefaultVersion, versionReportOpenStop).Return(workflow.DefaultVersion)
		}
		final := h.run(t)
		if final != tc.final || !slices.Equal(h.escalated, tc.asked) || !sameTimes(began, tc.reports) || !slices.EqualFunc(answers, tc.answers, slices.Equal) {
			t.Fatalf("%s: final %q, escalations %q, reports at %v with %q; want %s, %q, reports at %v with %q",
				tc.name, final, h.escalated, began, answers, tc.final, tc.asked, tc.reports, tc.answers)
		}
		if !sameTimes(checked, tc.checks) || len(checks) > 0 && checks[0] != tc.check {
			t.Fatalf("%s: %d cap checks at %v, the first %+v; want %d at %v, the first %+v", tc.name, len(checked), checked, checks[:min(1, len(checks))], len(tc.checks), tc.checks, tc.check)
		}
	}
}

// A report card's run restarted at an open stop, recorded on a dev server
// before the change: it ran the report at once, and the replay schedules
// the activities it recorded with the inputs it recorded.
func TestWorkflowReplaysAReportRestartedAtAnOpenStopFromBefore(t *testing.T) {
	hist := loadHistory(t, filepath.Join("testdata", "devserver-report-open-stop.json"))
	calls, inputs := scheduledActivities(hist)
	want := []string{ActivityTaskConfig, ActivityEnsureIssue, ActivityReconcile, ActivitySetState, ActivityRunReport, ActivityFinish}
	if !slices.Equal(calls, want) {
		t.Fatalf("the recording schedules %q, want %q", calls, want)
	}
	if st := reconciled(t, hist); st["Report"] != true || st["Status"] != "waiting-human" || st["Question"] == "" {
		t.Fatalf("the recorded Reconcile answer %v: want a report card waiting at its stop", st)
	}
	var got, gotInputs []string
	replaySplice(t, hist.Events, &activitiesSeen{got: &got, inputs: &gotInputs})
	if !slices.Equal(got, calls) || !slices.Equal(gotInputs, inputs) {
		t.Fatalf("the replay scheduled %q with %q, the recording %q with %q", got, gotInputs, calls, inputs)
	}
}

// A Close at a report's stop ends the card closed and runs no report again:
// a report card has no issue, so Act signals the run's issue_closed before
// it resolves the case, and the wait reads the close first.
func TestWorkflowReportStopClosesOnClose(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state.Report = true
	reports := 0
	h.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == ActivityRunReport {
			reports++
		}
	})
	h.env.OnActivity(ActivityRunReport, mock.Anything, "t1", mock.Anything, mock.Anything, mock.Anything).After(15*time.Minute).Return(ReportResult{}, nil)
	h.onGateSupervisor(GateDecision{Action: ActionEscalate, Diagnosis: "the report session's pod waits for an image it cannot pull",
		Recommendation: "check the registry pull secret"})
	h.tripAfter(5*time.Minute, ActivityRunReport, 1)
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	h.signalAfter(time.Hour, SignalCaseResolved, TaskSignal{Author: "sebastian",
		Case: &CaseSignal{CaseID: 701, Rung: 3, By: "sebastian", Res: Resolution{Action: ActClose}}})
	if final := h.run(t); final != "closed" || reports != 1 || len(h.escalated) != 1 {
		t.Fatalf("final %q after %d reports and escalations %q; want closed after one report and its stop", final, reports, h.escalated)
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
