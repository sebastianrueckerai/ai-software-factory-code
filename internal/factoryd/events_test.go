package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// TestRecordEventLeavesOneFactorydRow asserts that Timeline.recordEvent
// with fakeEventRecorder and a fake store leaves exactly one row with via
// factoryd, actor factoryd, the task's repository and issue.
func TestRecordEventLeavesOneFactorydRow(t *testing.T) {
	store := &fakeStore{tasks: map[string]Task{
		"t1": {ID: "t1", Repository: "sf/code", IssueNumber: 42},
	}}
	rec := &fakeEventRecorder{}
	objects := &fakeObjectStore{}
	tl := &Timeline{Objects: objects, Events: rec, Store: store}
	tl.recordEvent(context.Background(), "t1", EventCardEnteredLane, map[string]any{"lane": "ready"})
	if len(rec.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rec.rows))
	}
	e := rec.rows[0]
	if e.Via != ViaFactoryd || e.Actor != FactoryDecider || e.Repository != "sf/code" || e.IssueNumber != 42 {
		t.Errorf("row = %+v", e)
	}
}

// Each of the five events is one timeline line of step event on the task
// it concerns, recorded where it happens.
func TestEventsRecorded(t *testing.T) {
	ctx := context.Background()

	t.Run("card_created", func(t *testing.T) {
		in, store, _ := laneIntake(t, "maintenance", maintenanceLanes)
		objects := &fakeObjectStore{}
		in.Timeline = &Timeline{Objects: objects}
		post(t, in, "issues", issueOpenedBody(5, "sebastian", []string{"agent"}, "Fix it", "body"))
		post(t, in, "issues", issueOpenedBody(6, "sebastian", []string{"human"}, "Decide it", "body"))
		if got := events(t, objects, card(store, 5).ID); !slices.Equal(got, []string{"card_created sebastian", "card_entered_lane ready sebastian"}) {
			t.Fatalf("agent card: %v", got)
		}
		if got := events(t, objects, card(store, 6).ID); !slices.Equal(got, []string{"card_created sebastian"}) {
			t.Fatalf("human-lane card: %v", got)
		}
		f, _ := fileStub(t)
		filed, _, err := FileAgentTask(ctx, newFakeStore(), f, &fakeWorkflows{}, in.Timeline, seedProject("sf/r"), "Fix it", "body", []string{"agent"}, Task{ID: "t-api", Source: "api", Author: "factory-agent"})
		if err != nil {
			t.Fatal(err)
		}
		if got := events(t, objects, filed.ID); !slices.Equal(got, []string{"card_created factory-agent", "card_entered_lane ready factory-agent"}) {
			t.Fatalf("filed card: %v", got)
		}
	})

	t.Run("card_entered_lane", func(t *testing.T) {
		in, store, _ := laneIntake(t, "maintenance", maintenanceLanes)
		objects := &fakeObjectStore{}
		in.Timeline = &Timeline{Objects: objects}
		post(t, in, "issues", issueOpenedBody(7, "outsider", []string{"improvement"}, "A wish", "body"))
		c := card(store, 7)
		writeUp, _, _ := store.ReportOf(ctx, c.ID, SourcePresort)
		if got := events(t, objects, c.ID); !slices.Equal(got, []string{"card_created outsider", "card_entered_lane backlog outsider"}) || writeUp.ID == "" ||
			!slices.Equal(workflowsOf(in).started, []string{writeUp.ID}) {
			t.Fatalf("backlog entry: %v, write-up %q, started %v", got, writeUp.ID, workflowsOf(in).started)
		}
		labels := []string{"improvement", "agent"}
		setLabels(t, in, 7, labels)
		post(t, in, "issue_label", labelEvent(7, "outsider", labels, "sebastian"))
		want := []string{"card_created outsider", "card_entered_lane backlog outsider", "card_entered_lane ready sebastian"}
		if got := events(t, objects, c.ID); !slices.Equal(got, want) || !slices.Equal(workflowsOf(in).started, []string{writeUp.ID, c.ID}) {
			t.Fatalf("move: %v, started %v", got, workflowsOf(in).started)
		}
	})

	t.Run("task_ended", func(t *testing.T) {
		r, _, objects := newActivityRunner(t, prTask, &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}})
		for range 2 {
			if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "3 attempts exhausted"); err != nil {
				t.Fatal(err)
			}
		}
		if got := events(t, objects, "t1"); !slices.Equal(got, []string{"task_ended failed"}) {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("pull_request_merged", func(t *testing.T) {
		r, store, objects := newGateRunner(t, gateTask, &gateForgejo{mergeStatus: []int{http.StatusOK}})
		store.OpenApproval(ctx, ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
		if _, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian"); err != nil {
			t.Fatal(err)
		}
		if got := events(t, objects, "t1"); !slices.Equal(got, []string{"pull_request_merged factory-merge 9"}) {
			t.Fatalf("Merge: %v", got)
		}
		in, istore, _ := newTestIntake(t)
		iobjects := &fakeObjectStore{}
		in.Timeline = &Timeline{Objects: iobjects}
		merged := func(id string, pr int64, sender string) {
			t.Helper()
			istore.tasks[id] = Task{ID: id, Repository: "sf/ai-software-factory-code", Status: "in-review", Kind: "agent", PRNumber: pr}
			b, _ := json.Marshal(map[string]any{"action": "closed", "pull_request": map[string]any{"number": pr, "merged": true,
				"head": map[string]any{"ref": "factory/" + id, "repo": map[string]any{"full_name": "factory-agent/ai-software-factory-code"}}},
				"repository": map[string]any{"full_name": "sf/ai-software-factory-code"}, "sender": map[string]string{"login": sender}})
			post(t, in, "pull_request", b)
		}
		merged("t-bot", 20, MergeUser)
		merged("t-human", 21, "sebastian")
		merged("t-human", 21, "sebastian")
		if got := events(t, iobjects, "t-bot"); len(got) != 0 {
			t.Fatalf("factory-merge's merge through intake: %v", got)
		}
		if got := events(t, iobjects, "t-human"); !slices.Equal(got, []string{"pull_request_merged sebastian 21"}) {
			t.Fatalf("a human's merge: %v", got)
		}
	})

	t.Run("all_children_done", func(t *testing.T) {
		plan := Task{ID: "t-plan", Kind: "agent", Status: "done", Lane: "plan", Process: "spec", Repository: "sf/repo", IssueNumber: 1}
		fj := &prForgejo{labels: map[int64][]string{1: {"plan", "state/done"}, 2: {"agent"}, 3: {"agent"}}}
		r, store, objects := newActivityRunner(t, plan, fj)
		for id, issue := range map[string]int64{"t-c2": 2, "t-c3": 3} {
			store.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Lane: "ready", Process: "spec", Repository: "sf/repo",
				IssueNumber: issue, Parent: "t-plan", Source: SourcePlan, PlanKey: id})
		}
		// a card that failed and was retried lets go of its key: the plan's cards are the others
		store.CreateTask(ctx, Task{ID: "t-c1", Kind: "agent", Status: "failed", Lane: "ready", Process: "spec", Repository: "sf/repo",
			IssueNumber: 4, Parent: "t-plan", Source: SourcePlan})
		if _, err := execActivity(t, r, ActivityFinish, "t-c2", "done", "merged"); err != nil {
			t.Fatal(err)
		}
		if got := events(t, objects, "t-plan"); len(got) != 0 || len(fj.comments) != 0 {
			t.Fatalf("one card open: plan events %v, comments %q", got, fj.comments)
		}
		if _, err := execActivity(t, r, ActivityFinish, "t-c3", "done", "merged"); err != nil {
			t.Fatal(err)
		}
		if got := events(t, objects, "t-plan"); !slices.Equal(got, []string{"all_children_done 2"}) {
			t.Fatalf("plan events = %v", got)
		}
		// the last two cards ending side by side both find every card done
		last, _, _ := store.Task(ctx, "t-c2")
		taskEnded(ctx, r.timeline, r.store, r.forgejo, nil, last, "done")
		if got := events(t, objects, "t-plan"); !slices.Equal(got, []string{"all_children_done 2"}) {
			t.Fatalf("plan events = %v", got)
		}
		if !slices.Equal(fj.comments, []string{"factoryd: all 2 cards of this plan are done."}) {
			t.Fatalf("comments = %q", fj.comments)
		}
	})
}
