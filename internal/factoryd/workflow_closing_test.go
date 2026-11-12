package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Runs restarted at closing, recorded on a dev server before the move to
// closing stored its end: the Reconcile answer holds no end, and the replay
// schedules the activities the recording did with the inputs it did. A
// plain card's Finish fails it for the interrupted completion; a report
// card runs its report, whose move to running the closing card refused.
func TestWorkflowReplaysRunsRestartedAtClosing(t *testing.T) {
	for _, tc := range []struct {
		file   string
		calls  []string
		finish string
	}{
		{"devserver-closing-restart.json", []string{ActivityTaskConfig, ActivityEnsureIssue, ActivityReconcile, ActivityFinish},
			`"t1"` + "\n" + `"failed"` + "\n" + `"completion resumed after an interrupted run"`},
		{"devserver-report-closing-restart.json", []string{ActivityTaskConfig, ActivityEnsureIssue, ActivityReconcile, ActivitySetState, ActivityFinish},
			`"t1"` + "\n" + `"failed"` + "\n" + `"set running: task t1 is closing, cannot move to running"`},
	} {
		t.Run(tc.file, func(t *testing.T) {
			hist := loadHistory(t, filepath.Join("testdata", tc.file))
			calls, inputs := scheduledActivities(hist)
			if !slices.Equal(calls, tc.calls) || inputs[len(inputs)-1] != tc.finish {
				t.Fatalf("the recording schedules %q, its Finish %q; want %q and %q", calls, inputs[len(inputs)-1], tc.calls, tc.finish)
			}
			st := reconciled(t, hist)
			if _, stored := st["ClosingFinal"]; st["Status"] != "closing" || stored {
				t.Fatalf("the recorded Reconcile answer %v: want a closing card with no stored end", st)
			}
			var got, gotInputs []string
			replaySplice(t, hist.Events, &activitiesSeen{got: &got, inputs: &gotInputs})
			if !slices.Equal(got, calls) || !slices.Equal(gotInputs, inputs) {
				t.Fatalf("the replay scheduled %q with %q, the recording %q with %q", got, gotInputs, calls, inputs)
			}
		})
	}
}

// A run restarted at closing finishes with the end the cut-off run stored
// with its move there, a report card's too, and runs nothing else.
func TestWorkflowRunRestartedAtClosingFinishesWithItsStoredEnd(t *testing.T) {
	for name, st := range map[string]RunState{
		"done":   {Status: "closing", ClosingFinal: "done", ClosingReason: "pull request merged by factory-merge after sebastian's approval"},
		"closed": {Status: "closing", ClosingFinal: "closed", ClosingReason: "issue closed without merge"},
		"report": {Status: "closing", Report: true, ClosingFinal: "done", ClosingReason: "report kept on the task, no issue comment"},
	} {
		h := newWorkflowHarness(t)
		h.state = st
		h.onSessions(pushed("s1", "c1"))
		if final := h.run(t); final != st.ClosingFinal || len(h.finals) != 1 || h.finals[0][1] != st.ClosingReason || len(h.sessions) != 0 || len(h.states) != 0 {
			t.Fatalf("%s: final = %q, finals %v, sessions %d, states %v; want %s for %q and nothing else", name, final, h.finals, len(h.sessions), h.states, st.ClosingFinal, st.ClosingReason)
		}
	}
}

// A report card restarted at closing in a run from before the change runs
// its report, the stored end unread.
func TestWorkflowReportRestartedAtClosingBeforeTheChangeRunsTheReport(t *testing.T) {
	h, began, _ := reportHarness(t, ReportResult{})
	h.state.Status, h.state.ClosingFinal, h.state.ClosingReason = "closing", "closed", "issue closed without merge"
	h.env.OnGetVersion("closing-intent", workflow.DefaultVersion, versionClosingIntent).Return(workflow.DefaultVersion)
	h.run(t)
	if want := [2]string{"done", "report kept on the task, no issue comment"}; len(h.finals) != 1 || h.finals[0] != want || len(*began) != 1 || !slices.Equal(h.states, []string{"running"}) {
		t.Fatalf("finals %v, reports %d, states %v; want %v after one report", h.finals, len(*began), h.states, want)
	}
}

// The move to closing stores the end the run decided; a completion that
// fails after it leaves the card at closing with that end, which the next
// completion's move keeps.
func TestCompletionStoresTheEndWithTheMoveToClosing(t *testing.T) {
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 500}
	fjURL, llURL := back.srv(t)
	store := newFakeStore()
	store.CreateTask(context.Background(), Task{ID: "t1", BudgetEUR: 5, Status: "in-review", Track: "direct"})

	if err := newCompletion(store, fjURL, llURL).Complete(context.Background(), "t1", "merged", "done", "pull request merged"); err == nil {
		t.Fatal("Complete succeeded against a refusing LiteLLM")
	}
	task, _, _ := store.Task(context.Background(), "t1")
	if task.Status != "closing" || task.ClosingFinal != "done" || task.ClosingReason != "pull request merged" {
		t.Fatalf("task = %s, end %q %q; want closing, done, pull request merged", task.Status, task.ClosingFinal, task.ClosingReason)
	}
}

// CloseTask moves a live card to closing with its end, and a card at
// closing again; a final card stays as it is.
func TestPGStoreCloseTaskStoresTheEnd(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events, tasks, cases CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []Task{
		{ID: "t-live", Repository: "sf/r", IssueNumber: 7, Kind: "agent", Status: "in-review", BudgetEUR: 1, Track: "direct"},
		{ID: "t-done", Repository: "sf/r", IssueNumber: 8, Kind: "agent", Status: "done", BudgetEUR: 1, Track: "direct"},
	} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	from := []string{"in-review", "closing"}
	for range 2 {
		if err := s.CloseTask(ctx, "t-live", from, "closed", "issue closed without merge"); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.Task(ctx, "t-live")
		if err != nil || got.Status != "closing" || got.ClosingFinal != "closed" || got.ClosingReason != "issue closed without merge" {
			t.Fatalf("t-live = %s, end %q %q (%v); want closing, closed, issue closed without merge", got.Status, got.ClosingFinal, got.ClosingReason, err)
		}
	}
	if err := s.CloseTask(ctx, "t-done", from, "failed", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CloseTask of a done card = %v, want ErrNotFound", err)
	}
	if got, _, _ := s.Task(ctx, "t-done"); got.Status != "done" || got.ClosingFinal != "" {
		t.Fatalf("t-done = %s, end %q; want done with none", got.Status, got.ClosingFinal)
	}
}

// scheduledActivities lists the activities hist scheduled and the
// encoded input of each.
func scheduledActivities(hist *historypb.History) (calls, inputs []string) {
	for _, e := range hist.Events {
		if a := e.GetActivityTaskScheduledEventAttributes(); a != nil {
			calls, inputs = append(calls, a.GetActivityType().GetName()), append(inputs, payloadsText(a.GetInput()))
		}
	}
	return calls, inputs
}

// reconciled is the Reconcile answer hist recorded, as JSON fields.
func reconciled(t *testing.T, hist *historypb.History) map[string]any {
	t.Helper()
	var reconcile int64
	for _, e := range hist.Events {
		if a := e.GetActivityTaskScheduledEventAttributes(); a.GetActivityType().GetName() == ActivityReconcile {
			reconcile = e.EventId
		}
		if c := e.GetActivityTaskCompletedEventAttributes(); c != nil && c.ScheduledEventId == reconcile {
			var st map[string]any
			if err := json.Unmarshal(c.GetResult().GetPayloads()[0].GetData(), &st); err != nil {
				t.Fatal(err)
			}
			return st
		}
	}
	t.Fatal("the recording holds no Reconcile answer")
	return nil
}
