package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// replanRig is plan p (issue 10, one session used) in status with
// revisions stored, its child c (issue 12, running) and the sibling s
// (issue 13) waiting on c, all on sf/repo.
func replanRig(t *testing.T, status string, revisions int) (*Runner, *fakeStore, *prForgejo) {
	t.Helper()
	fj := &prForgejo{labels: map[int64][]string{10: {LabelPlan, "state/" + status}}}
	r, store, _ := newActivityRunner(t, Task{ID: "p", Kind: "agent", Track: policy.TrackPlan, Source: "webhook", Repository: "sf/repo",
		IssueNumber: 10, Status: status, BudgetEUR: 5, Usage: TaskUsage{Attempts: 1}}, fj)
	ctx := context.Background()
	for _, c := range []Task{
		{ID: "c", PlanKey: "api", IssueNumber: 12, Status: "running"},
		{ID: "s", PlanKey: "ui", IssueNumber: 13, Status: "waiting-deps"},
	} {
		c.Kind, c.Track, c.Source, c.Parent, c.Repository = "agent", policy.TrackDirect, SourcePlan, "p", "sf/repo"
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddTaskDeps(ctx, "s", []string{"c"}); err != nil {
		t.Fatal(err)
	}
	for i := range revisions {
		if _, err := store.AddPlanRevision(ctx, "p", json.RawMessage(`{}`), fmt.Sprint("sha-", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	return r, store, fj
}

func replanChild(t *testing.T, r *Runner, child, work string) ReplanChildResult {
	t.Helper()
	val, err := execActivity(t, r, ActivityReplanChild, child, work)
	return getInto[ReplanChildResult](t, val, err)
}

// stepsNamed are task id's timeline lines of step, oldest first.
func stepsNamed(t *testing.T, r *Runner, id, step string) []json.RawMessage {
	t.Helper()
	b, err := r.timeline.Objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []json.RawMessage
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var m struct{ Step string }
		if json.Unmarshal([]byte(line), &m) == nil && m.Step == step {
			out = append(out, json.RawMessage(line))
		}
	}
	return out
}

func replanSteps(t *testing.T, r *Runner, id string) []ReplanStep {
	t.Helper()
	var out []ReplanStep
	for _, line := range stepsNamed(t, r, id, stepReplan) {
		var s ReplanStep
		if err := json.Unmarshal(line, &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func reopens(r *Runner) []reopenCall { return r.cfg.Workflows.(*fakeWorkflows).reopens }

// A child's cut sends its plan back to the planner: the plan records the
// replan with the child's work, takes the child's dependents, and reopens
// queued with its counters at zero. A second cut finds the replan
// running and joins it.
func TestReplanChildCutsItsPlan(t *testing.T) {
	r, store, _ := replanRig(t, "done", 2)
	if got := replanChild(t, r, "c", "w"); got != (ReplanChildResult{Reason: "cut: plan #10 replans its work"}) {
		t.Fatalf("answer %+v, want the cut", got)
	}
	if deps := store.deps["s"]; !slices.Equal(deps, []string{"p"}) {
		t.Fatalf("s depends on %v, want the plan", deps)
	}
	want := []ReplanStep{{Origin: ReplanCut, Card: "c", Work: "w", Stops: []string{"c"}, Revision: 2}}
	if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan's replan steps %+v, want %+v", got, want)
	}
	if p := store.tasks["p"]; p.Status != "queued" || p.Usage != (TaskUsage{}) {
		t.Fatalf("plan %s with usage %+v, want queued with none", p.Status, p.Usage)
	}
	if got := reopens(r); !reflect.DeepEqual(got, []reopenCall{{taskID: "p"}}) {
		t.Fatalf("reopens %+v, want p's without a signal", got)
	}
	if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 1 {
		t.Fatalf("the child's replan_stop steps: %d, want 1", n)
	}

	if got := replanChild(t, r, "c", "w"); got != (ReplanChildResult{Reason: "stopped for the replan of plan #10"}) {
		t.Fatalf("the second answer %+v, want the join", got)
	}
	if got := replanSteps(t, r, "p"); len(got) != 1 {
		t.Fatalf("the plan's replan steps after the second call: %+v, want the first alone", got)
	}
	if got := reopens(r); len(got) != 1 {
		t.Fatalf("reopens after the second call: %+v, want one", got)
	}
}

// A plan whose replan runs takes the cut child's dependents and records
// nothing of its own.
func TestReplanChildJoinsARunningReplan(t *testing.T) {
	r, store, _ := replanRig(t, "queued", 2)
	if got := replanChild(t, r, "c", "w"); got != (ReplanChildResult{Reason: "stopped for the replan of plan #10"}) {
		t.Fatalf("answer %+v, want the join", got)
	}
	if got := replanSteps(t, r, "p"); len(got) != 0 {
		t.Fatalf("the plan's replan steps %+v, want none", got)
	}
	if deps := store.deps["s"]; !slices.Equal(deps, []string{"p"}) {
		t.Fatalf("s depends on %v, want the plan", deps)
	}
	if got := reopens(r); len(got) != 0 {
		t.Fatalf("reopens %+v, want none", got)
	}
}

// A closed plan, and a failed one that never stored a revision, refuse
// the cut: the child records nothing and keeps its dependents.
func TestReplanChildRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		status    string
		revisions int
		closed    bool
		want      string
	}{
		"closed": {"done", 2, true, "Plan #10 is closed, so this card's work cannot go back to its planner: answer to go on with the card, or close it."},
		"failed": {"failed", 0, false, "Plan #10 is failed, so this card's work cannot go back to its planner: answer to go on with the card, or close it."},
	} {
		t.Run(name, func(t *testing.T) {
			r, store, _ := replanRig(t, tc.status, tc.revisions)
			store.closed["p"] = tc.closed
			if got := replanChild(t, r, "c", "w"); got != (ReplanChildResult{Refused: tc.want}) {
				t.Fatalf("answer %+v, want the refusal %q", got, tc.want)
			}
			if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 0 {
				t.Fatalf("the child's replan_stop steps: %d, want none", n)
			}
			if deps := store.deps["s"]; !slices.Equal(deps, []string{"c"}) {
				t.Fatalf("s depends on %v, want c still", deps)
			}
			if p := store.tasks["p"]; p.Status != tc.status || len(replanSteps(t, r, "p")) != 0 || len(reopens(r)) != 0 {
				t.Fatalf("plan %s, steps %+v, reopens %+v; want it untouched", p.Status, replanSteps(t, r, "p"), reopens(r))
			}
		})
	}
}

// Finding 20: a failed plan that stored a revision, its replan's planner
// failed, takes a cut as a done one does.
func TestReplanChildReopensAFailedPlan(t *testing.T) {
	r, store, _ := replanRig(t, "failed", 2)
	if got := replanChild(t, r, "c", "w"); got != (ReplanChildResult{Reason: "cut: plan #10 replans its work"}) {
		t.Fatalf("answer %+v, want the cut", got)
	}
	want := []ReplanStep{{Origin: ReplanCut, Card: "c", Work: "w", Stops: []string{"c"}, Revision: 2}}
	if got := replanSteps(t, r, "p"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the plan's replan steps %+v, want %+v", got, want)
	}
	if p := store.tasks["p"]; p.Status != "queued" {
		t.Fatalf("plan %s, want queued", p.Status)
	}
}

// fundings are task id's card_funded events.
func fundings(store *fakeStore, id string) []Event {
	var out []Event
	for _, e := range store.events {
		if e.TaskID == id && e.Kind == EventCardFunded {
			out = append(out, e)
		}
	}
	return out
}

// The planner's session gets the default card budget: a plan left with
// less is funded up to it, once.
func TestFundPlannerLeavesTheDefaultBudget(t *testing.T) {
	ctx := context.Background()
	r, store, _ := replanRig(t, "done", 1)
	v := r.cfg.Policy()
	v.Document.Budget.TaskDefaultEUR = 5
	r.cfg.Policy = func() policy.Version { return v }

	p := store.tasks["p"]
	p.BudgetEUR, p.SpentEUR = 10, 8
	store.tasks["p"] = p
	for range 2 {
		if err := r.fundPlanner(ctx, store.tasks["p"]); err != nil {
			t.Fatal(err)
		}
	}
	got := fundings(store, "p")
	if len(got) != 1 || got[0].Detail["amount_eur"] != 3.0 || got[0].Detail["reason"] != "the planner's session" ||
		got[0].Actor != FactoryDecider || got[0].Via != ViaFactoryd || store.tasks["p"].BudgetEUR != 13 {
		t.Fatalf("fundings %+v, budget %v; want one of 3.00 EUR by factoryd, the budget 13", got, store.tasks["p"].BudgetEUR)
	}

	p = store.tasks["p"]
	p.BudgetEUR, p.SpentEUR = 10, 2
	store.tasks["p"] = p
	if err := r.fundPlanner(ctx, store.tasks["p"]); err != nil {
		t.Fatal(err)
	}
	if got := fundings(store, "p"); len(got) != 1 {
		t.Fatalf("fundings %+v, want no new one with 8 EUR left", got)
	}
}

// A reopened plan's issue opens again when it was closed, and only then.
func TestReopenPlanReopensTheIssue(t *testing.T) {
	ctx := context.Background()
	for _, closed := range []bool{true, false} {
		r, store, fj := replanRig(t, "done", 1)
		fj.issueOff = closed
		if err := r.reopenPlan(ctx, store.tasks["p"]); err != nil {
			t.Fatal(err)
		}
		patches := 0
		for _, c := range fj.calls {
			if c == "PATCH /api/v1/repos/sf/repo/issues/10" {
				patches++
			}
		}
		if want := map[bool]int{true: 1, false: 0}[closed]; patches != want || fj.issueOff || !slices.Equal(fj.reopened, slices.Repeat([]int64{10}, want)) {
			t.Fatalf("issue closed %t: %d PATCHes, reopened %v, closed after %t; want %d and open", closed, patches, fj.reopened, fj.issueOff, want)
		}
		if got := fj.labels[10]; !slices.Contains(got, "state/queued") {
			t.Fatalf("issue closed %t: labels %v, want state/queued", closed, got)
		}
	}
}

// A card with a live session is asked to stop for the replan; one with
// none hands its dependents to the plan and ends closed at once.
func TestStopForReplan(t *testing.T) {
	ctx := context.Background()
	why := "stopped for the replan of plan #10"

	r, store, _ := replanRig(t, "done", 1)
	asks, end := r.liveSession("c", sessionWorker{})
	defer end()
	got := make(chan AgentAsk, 1)
	go func() { got <- <-asks }()
	if err := r.stopForReplan(ctx, store.tasks["c"], store.tasks["p"], why); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-got:
		if !reflect.DeepEqual(a, AgentAsk{Kind: AskReplan, Reason: why}) {
			t.Fatalf("ask %+v, want the replan with the reason", a)
		}
	case <-time.After(time.Second):
		t.Fatal("the session got no ask")
	}
	if c := store.tasks["c"]; c.Status != "running" || !slices.Equal(store.deps["s"], []string{"c"}) {
		t.Fatalf("c %s, s depends on %v; want the session to end it, its dependents kept", c.Status, store.deps["s"])
	}

	r, store, _ = replanRig(t, "done", 1)
	if err := r.stopForReplan(ctx, store.tasks["c"], store.tasks["p"], why); err != nil {
		t.Fatal(err)
	}
	if c := store.tasks["c"]; c.Status != "closed" || !slices.Equal(store.deps["s"], []string{"p"}) {
		t.Fatalf("c %s, s depends on %v; want closed, the dependents on the plan", c.Status, store.deps["s"])
	}
	finals := stepsNamed(t, r, "c", "final")
	var final struct{ State, Reason string }
	if len(finals) != 1 || json.Unmarshal(finals[0], &final) != nil || final.State != "closed" || final.Reason != why {
		t.Fatalf("final steps %s, want closed with %q", finals, why)
	}
}

// A replan is pending until the planner files its revision (replanned)
// or the switch is refused (to_plan_refused); a later replan is pending
// with its own fields.
func TestPendingReplanEndsAtARefusedSwitch(t *testing.T) {
	ctx := context.Background()
	for _, closer := range []string{"to_plan_refused", "replanned"} {
		r, store, _ := replanRig(t, "done", 1)
		for _, step := range []string{stepReplan, closer} {
			if err := r.timeline.Append(ctx, "p", step, map[string]any{"origin": ReplanCut, "card": "c"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, pending, err := r.pendingReplan(ctx, store.tasks["p"]); err != nil || pending {
			t.Fatalf("after %s: pending %t, %v; want none", closer, pending, err)
		}
		if closer != "replanned" {
			continue
		}
		if err := r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "too wide", "stops": []string{"s"}, "revision": 1}); err != nil {
			t.Fatal(err)
		}
		step, pending, err := r.pendingReplan(ctx, store.tasks["p"])
		if want := (ReplanStep{Origin: ReplanPerson, By: "ana", Comment: "too wide", Stops: []string{"s"}, Revision: 1}); err != nil || !pending || !reflect.DeepEqual(step, want) {
			t.Fatalf("the second replan: %+v pending %t, %v; want %+v", step, pending, err, want)
		}
	}
}

// The cards a pending replan stops hold the planner back until each is
// final; a person's replan carries their name and note.
func TestReplanStoppedWaitsForTheStops(t *testing.T) {
	ctx := context.Background()
	r, store, _ := replanRig(t, "queued", 1)
	if got, err := r.ReplanStopped(ctx, "p"); err != nil || !reflect.DeepEqual(got, ReplanWait{Stopped: true}) {
		t.Fatalf("no replan pending: %+v, %v; want stopped", got, err)
	}
	if err := r.timeline.Append(ctx, "p", stepReplan, map[string]any{"origin": ReplanPerson, "by": "ana", "comment": "too wide", "stops": []string{"c", "s"}}); err != nil {
		t.Fatal(err)
	}
	s := store.tasks["s"]
	s.Status = "closed"
	store.tasks["s"] = s
	want := ReplanWait{Open: []string{"c"}, By: "ana", Comment: "too wide"}
	if got, err := r.ReplanStopped(ctx, "p"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("c running: %+v, %v; want %+v", got, err, want)
	}
	c := store.tasks["c"]
	c.Status = "closed"
	store.tasks["c"] = c
	if got, err := r.ReplanStopped(ctx, "p"); err != nil || !reflect.DeepEqual(got, ReplanWait{Stopped: true, By: "ana", Comment: "too wide"}) {
		t.Fatalf("every stop final: %+v, %v; want stopped with the note", got, err)
	}
}
