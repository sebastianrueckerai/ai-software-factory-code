package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestPGStoreTaskDeps(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_deps"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-a", Status: "running", IssueNumber: 5},
		{ID: "t-b", Status: "done", IssueNumber: 3},
		{ID: "t-c", Status: "failed", IssueNumber: 4},
		{ID: "t-x", Status: "waiting-deps", IssueNumber: 7},
		{ID: "t-y", Status: "queued", IssueNumber: 8},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind, tk.Source = 1, "sf/r", "agent", SourcePlan
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET plan_key = 'links-api' WHERE id = 't-a'; UPDATE tasks SET plan_key = 'accounts' WHERE id = 't-c'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.AddTaskDeps(ctx, "t-x", []string{"t-a", "t-b", "t-c"}); err != nil {
			t.Fatalf("AddTaskDeps: %v", err)
		}
	}
	if err := s.AddTaskDeps(ctx, "t-y", []string{"t-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, "t-y", []string{"t-y"}); err == nil {
		t.Fatal("a card depending on itself was recorded")
	}
	open, err := s.OpenDependencies(ctx, "t-x")
	if err != nil {
		t.Fatal(err)
	}
	want := []Dependency{{TaskID: "t-c", Issue: 4, Key: "accounts", Status: "failed"}, {TaskID: "t-a", Issue: 5, Key: "links-api", Status: "running"}}
	if !slices.Equal(open, want) {
		t.Fatalf("OpenDependencies = %+v, want %+v", open, want)
	}
	if got, err := s.WaitingDependents(ctx, "t-a"); err != nil || !slices.Equal(got, []string{"t-x"}) {
		t.Fatalf("WaitingDependents(t-a) = %v, %v; want [t-x], the queued card waits on nothing yet", got, err)
	}
	if got, _ := s.OpenDependencies(ctx, "t-b"); len(got) != 0 {
		t.Fatalf("a card without dependencies has open ones: %+v", got)
	}
	tasks, err := s.BoardTasks(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	waiting := map[string]string{}
	for _, b := range tasks {
		waiting[b.ID] = b.WaitingOn
	}
	if waiting["t-x"] != "#4, #5" || waiting["t-y"] != "" {
		t.Fatalf("board waiting-on = %q, want #4, #5 on the waiting card only", waiting)
	}
}

// depsConfig is TaskConfig for a card of process spec, which has the
// dependencies block.
func depsConfig() *TaskConfig {
	return &TaskConfig{Process: "spec", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
		Stages: testStages(), Queue: policy.QueueProject, Dependencies: &policy.Dependencies{WaitUntil: "done", OnFailed: "human"}}
}

// The card waits on #3 and #5; #3's end leaves it waiting, #5's starts
// it: the wait costs no session, and the first session begins after the
// second dep_ended.
func TestWaitStartsWhenDepsDone(t *testing.T) {
	t.Run("workflow", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		start := h.env.Now()
		var asked []time.Duration
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
			at := h.env.Now().Sub(start)
			asked = append(asked, at)
			switch {
			case at < 3*time.Minute:
				return DepState{Waiting: []int64{3, 5}}, nil
			case at < 7*time.Minute:
				return DepState{Waiting: []int64{5}}, nil
			}
			return DepState{}, nil
		})
		var began []time.Duration
		h.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(func(ctx context.Context, in SessionInput) (SessionResult, error) {
			began = append(began, h.env.Now().Sub(start))
			h.sessions = append(h.sessions, in)
			return pushed("s1", "c1"), nil
		})
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(3*time.Minute, SignalDepEnded, TaskSignal{})
		h.signalAfter(7*time.Minute, SignalDepEnded, TaskSignal{})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if !slices.Equal(asked, []time.Duration{0, 3 * time.Minute, 7 * time.Minute}) {
			t.Fatalf("dependencies asked at %v, want at the start and at each dep_ended", asked)
		}
		if !slices.Equal(began, []time.Duration{7 * time.Minute}) || len(h.sessions[0].Findings) != 0 {
			t.Fatalf("sessions began at %v with %+v, want one at 7m with no finding", began, h.sessions)
		}
	})
	t.Run("timer", func(t *testing.T) {
		// a lost dep_ended costs at most the re-check's 10 minutes
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		start := h.env.Now()
		var asked []time.Duration
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
			at := h.env.Now().Sub(start)
			asked = append(asked, at)
			if at < 15*time.Minute {
				return DepState{Waiting: []int64{5}}, nil
			}
			return DepState{}, nil
		})
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		h.run(t)
		if !slices.Equal(asked, []time.Duration{0, 10 * time.Minute, 20 * time.Minute}) {
			t.Fatalf("dependencies asked at %v, want every 10 minutes", asked)
		}
	})
	t.Run("activity and trigger", func(t *testing.T) {
		ctx := context.Background()
		card := Task{ID: "t-c", Kind: "agent", Status: "queued", Lane: "ready", Process: "spec", Repository: "sf/repo", IssueNumber: 7,
			Parent: "t-plan", Source: SourcePlan}
		fj := &prForgejo{labels: map[int64][]string{7: {"agent", "state/queued"}}}
		r, store, objects := newActivityRunner(t, card, fj)
		wf := &fakeWorkflows{}
		r.cfg.Workflows = wf
		for id, issue := range map[string]int64{"t-a": 3, "t-b": 5} {
			store.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Lane: "ready", Process: "spec", Repository: "sf/repo",
				IssueNumber: issue, Parent: "t-plan", Source: SourcePlan})
		}
		if err := store.AddTaskDeps(ctx, "t-c", []string{"t-b", "t-a"}); err != nil {
			t.Fatal(err)
		}
		ask := func() DepState {
			t.Helper()
			v, err := execActivity(t, r, ActivityDependencies, "t-c")
			if err != nil {
				t.Fatal(err)
			}
			var d DepState
			if err := v.Get(&d); err != nil {
				t.Fatal(err)
			}
			return d
		}
		if d := ask(); !slices.Equal(d.Waiting, []int64{3, 5}) || d.Ended != nil {
			t.Fatalf("first ask = %+v, want waiting on 3 and 5", d)
		}
		got, _, _ := store.Task(ctx, "t-c")
		if got.Status != "waiting-deps" || got.Gate != GateDependencies || !slices.Contains(fj.labels[7], "state/waiting-deps") {
			t.Fatalf("card = %s in gate %q, labels %v; want waiting-deps in gate dependencies", got.Status, got.Gate, fj.labels[7])
		}
		if _, err := execActivity(t, r, ActivityFinish, "t-a", "done", "merged"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(wf.signals, []string{"t-c dep_ended  #0: "}) {
			t.Fatalf("signals after #3 = %q, want dep_ended to the waiting card", wf.signals)
		}
		if d := ask(); !slices.Equal(d.Waiting, []int64{5}) {
			t.Fatalf("second ask = %+v, want waiting on 5", d)
		}
		if _, err := execActivity(t, r, ActivityFinish, "t-b", "done", "merged"); err != nil {
			t.Fatal(err)
		}
		if len(wf.signals) != 2 {
			t.Fatalf("signals after #5 = %q", wf.signals)
		}
		if d := ask(); len(d.Waiting) != 0 || d.Ended != nil {
			t.Fatalf("third ask = %+v, want the card free to start", d)
		}
		if _, err := execActivity(t, r, ActivitySetState, "t-c", "running"); err != nil {
			t.Fatalf("waiting-deps to running: %v", err)
		}
		if steps := depSteps(t, objects, "t-c"); !slices.Equal(steps, []string{"dependencies_wait [3 5]", "dependencies_done started"}) {
			t.Fatalf("timeline = %q, want the wait once and its end with its start", steps)
		}
	})
}

// A retry of Dependencies after the card's move to waiting-deps (its gate
// failed) finishes the entry: the gate, the label and the wait's record.
func TestDependenciesRetryFinishesEntry(t *testing.T) {
	ctx := context.Background()
	card := Task{ID: "t-c", Kind: "agent", Status: "waiting-deps", Lane: "ready", Process: "spec", Repository: "sf/repo", IssueNumber: 7}
	fj := &prForgejo{labels: map[int64][]string{7: {"agent", "state/queued"}}}
	r, store, objects := newActivityRunner(t, card, fj)
	store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "running", Repository: "sf/repo", IssueNumber: 3})
	store.AddTaskDeps(ctx, "t-c", []string{"t-a"})
	for range 2 {
		if _, err := execActivity(t, r, ActivityDependencies, "t-c"); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := store.Task(ctx, "t-c")
	if got.Gate != GateDependencies || !slices.Contains(fj.labels[7], "state/waiting-deps") {
		t.Fatalf("card in gate %q with labels %v, want gate dependencies and state/waiting-deps", got.Gate, fj.labels[7])
	}
	if steps := depSteps(t, objects, "t-c"); !slices.Equal(steps, []string{"dependencies_wait [3]"}) {
		t.Fatalf("timeline = %q, want the wait recorded once", steps)
	}
}

// depSteps are task id's dependency steps: the wait with its issues, the
// end with whether it carries the wait's start.
func depSteps(t *testing.T, objects *fakeObjectStore, id string) []string {
	t.Helper()
	b, err := objects.Get(context.Background(), "sessions/"+id+"/timeline.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		switch e["step"] {
		case "dependencies_wait":
			out = append(out, fmt.Sprintf("dependencies_wait %v", e["on"]))
		case "dependencies_done":
			if _, ok := e["started"]; ok {
				out = append(out, "dependencies_done started")
			} else {
				out = append(out, "dependencies_done")
			}
		}
	}
	return out
}

// A dependency that ended failed or closed opens the card with the
// reason: an answer starts the card with the reason as a finding, a close
// ends it closed.
func TestFailedDependencyOpensCard(t *testing.T) {
	const reason = "dependency #3 (links-api) ended failed: close this card, or answer to start it anyway"
	failed := func(h *workflowHarness) {
		h.taskConfig = depsConfig()
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(
			DepState{Waiting: []int64{5}, Ended: &Dependency{TaskID: "t-a", Issue: 3, Key: "links-api", Status: "failed"}}, nil)
	}
	t.Run("answer starts", func(t *testing.T) {
		h := newWorkflowHarness(t)
		failed(h)
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go ahead", Comment: 700})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		if final := h.run(t); final != "done" {
			t.Fatalf("final = %q", final)
		}
		if !slices.Equal(h.escalated, []string{reason}) {
			t.Fatalf("escalated = %q", h.escalated)
		}
		if len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Findings, []string{reason}) ||
			len(h.sessions[0].Answers) != 1 || !strings.HasSuffix(h.sessions[0].Answers[0], ": go ahead") {
			t.Fatalf("sessions = %+v, want one with the reason as its finding and the answer", h.sessions)
		}
		started := 0
		for _, s := range h.recorded {
			if s.step == stepDepsStarted && s.detail["reason"] == reason {
				started++
			}
		}
		if started != 1 {
			t.Fatalf("recorded = %+v, want one dependencies_started with the reason", h.recorded)
		}
	})
	for _, status := range []string{"running", "waiting-human"} {
		t.Run("started anyway, a later run in "+status, func(t *testing.T) {
			// factoryd restarted in the first session, or before it: the
			// timeline's answer starts the card, no second question
			h := newWorkflowHarness(t)
			failed(h)
			h.state = RunState{Status: status, DepStarted: reason, Question: reason}
			h.onSessions(pushed("s1", "c1"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q", final)
			}
			if len(h.escalated) != 0 || len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Findings, []string{reason}) {
				t.Fatalf("escalated %q, sessions %+v; want no question and the reason as the finding", h.escalated, h.sessions)
			}
		})
	}
	t.Run("close ends", func(t *testing.T) {
		h := newWorkflowHarness(t)
		failed(h)
		h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final != "closed" {
			t.Fatalf("final = %q, want closed", final)
		}
		if len(h.sessions) != 0 {
			t.Fatalf("sessions = %+v, want none", h.sessions)
		}
	})
	t.Run("closed while factoryd was down", func(t *testing.T) {
		h := newWorkflowHarness(t)
		failed(h)
		h.state = RunState{Status: "waiting-deps", IssueClosed: true}
		if final := h.run(t); final != "closed" || len(h.sessions) != 0 || len(h.escalated) != 0 {
			t.Fatalf("final %q, sessions %d, escalated %q; want closed, nothing run", final, len(h.sessions), h.escalated)
		}
	})
	t.Run("closed during the wait", func(t *testing.T) {
		h := newWorkflowHarness(t)
		h.taskConfig = depsConfig()
		start := h.env.Now()
		asked := 0
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
			asked++
			return DepState{Waiting: []int64{5}}, nil
		})
		h.signalAfter(25*time.Minute, SignalIssueClosed, TaskSignal{Author: "sebastian"})
		if final := h.run(t); final != "closed" || len(h.sessions) != 0 || len(h.escalated) != 0 {
			t.Fatalf("final %q, sessions %d, escalated %q; want closed, nothing run", final, len(h.sessions), h.escalated)
		}
		if asked != 3 || h.finishAt.Sub(start) != 25*time.Minute {
			t.Fatalf("asked %d times, finished at %s; want 3 asks (0, 10m, 20m) and the end at the close, 25m", asked, h.finishAt.Sub(start))
		}
	})
	t.Run("healed asks again", func(t *testing.T) {
		h := newWorkflowHarness(t)
		failed(h)
		h.state = RunState{Status: "waiting-human", Question: reason}
		h.onSessions(pushed("s1", "c1"))
		h.onChecks(ChecksResult{State: ChecksSuccess})
		h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "go ahead", Comment: 700})
		h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{PR: 9})
		h.run(t)
		if !slices.Equal(h.escalated, []string{healedReason}) || len(h.sessions) != 1 || !slices.Equal(h.sessions[0].Findings, []string{reason}) {
			t.Fatalf("escalated %q, sessions %+v", h.escalated, h.sessions)
		}
	})
	t.Run("activity", func(t *testing.T) {
		ctx := context.Background()
		card := Task{ID: "t-c", Kind: "agent", Status: "waiting-deps", Lane: "ready", Process: "spec", Repository: "sf/repo", IssueNumber: 7}
		r, store, _ := newActivityRunner(t, card, &prForgejo{})
		store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "closed", Repository: "sf/repo", IssueNumber: 3, PlanKey: "links-api"})
		store.CreateTask(ctx, Task{ID: "t-b", Kind: "agent", Status: "running", Repository: "sf/repo", IssueNumber: 5})
		store.AddTaskDeps(ctx, "t-c", []string{"t-a", "t-b"})
		v, err := execActivity(t, r, ActivityDependencies, "t-c")
		if err != nil {
			t.Fatal(err)
		}
		var d DepState
		v.Get(&d)
		if d.Ended == nil || *d.Ended != (Dependency{TaskID: "t-a", Issue: 3, Key: "links-api", Status: "closed"}) {
			t.Fatalf("Ended = %+v, want #3 closed", d.Ended)
		}
		if _, err := execActivity(t, r, ActivityEscalate, "t-c", "why"); err != nil {
			t.Fatalf("waiting-deps to waiting-human: %v", err)
		}
	})
}

// The heal pass signals dep_ended to a waiting card whose wait would end
// (every dependency done, or one ended failed), not to one still waiting,
// and starts the workflow of every waiting card.
func TestHealSignalsWaitingCards(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	for id, st := range map[string]string{"t-done": "done", "t-failed": "failed", "t-open": "running"} {
		store.CreateTask(ctx, Task{ID: id, Status: st, Kind: "agent", Repository: "sf/ai-software-factory-code"})
	}
	for id, deps := range map[string][]string{"tw-free": {"t-done"}, "tw-failed": {"t-done", "t-failed", "t-open"}, "tw-waits": {"t-done", "t-open"}} {
		store.CreateTask(ctx, Task{ID: id, Status: "waiting-deps", Kind: "agent", Repository: "sf/ai-software-factory-code"})
		store.AddTaskDeps(ctx, id, deps)
	}
	wf := &fakeWorkflows{}
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	HealPass(ctx, store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent"}, nil)
	var dep []string
	for _, s := range wf.signals {
		if strings.Contains(s, " "+SignalDepEnded+" ") {
			dep = append(dep, strings.Fields(s)[0])
		}
	}
	slices.Sort(dep)
	if !slices.Equal(dep, []string{"tw-failed", "tw-free"}) {
		t.Fatalf("dep_ended to %v, want tw-failed and tw-free", dep)
	}
	for _, id := range []string{"tw-free", "tw-failed", "tw-waits"} {
		if !slices.Contains(wf.started, id) {
			t.Fatalf("started = %v, want every waiting card's workflow", wf.started)
		}
	}
}

// A process without the dependencies block waits on nothing: no
// Dependencies call at all, and neither does a run from before dep-wait.
func TestNoDepsNoWait(t *testing.T) {
	for name, setup := range map[string]func(h *workflowHarness){
		"maintenance": func(h *workflowHarness) {},
		"before dep-wait": func(h *workflowHarness) {
			h.taskConfig = depsConfig()
			h.env.OnGetVersion("dep-wait", workflow.DefaultVersion, versionDepWait).Return(workflow.DefaultVersion)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newWorkflowHarness(t)
			setup(h)
			h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
				t.Error("Dependencies called")
				return DepState{}, nil
			})
			h.onSessions(pushed("s1", "c1"))
			h.onChecks(ChecksResult{State: ChecksSuccess})
			h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{PR: 9})
			if final := h.run(t); final != "done" {
				t.Fatalf("final = %q", final)
			}
		})
	}
}
