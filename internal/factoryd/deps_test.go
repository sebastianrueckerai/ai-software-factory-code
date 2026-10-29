package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
		{ID: "t-a", Status: "running", IssueNumber: 5, Track: "direct"},
		{ID: "t-b", Status: "done", IssueNumber: 3, Track: "direct"},
		{ID: "t-c", Status: "failed", IssueNumber: 4, Track: "direct"},
		{ID: "t-x", Status: "waiting-deps", IssueNumber: 7, Track: "direct"},
		{ID: "t-y", Status: "queued", IssueNumber: 8, Track: "direct"},
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
	all, err := s.Dependencies(ctx, "t-x")
	if want := []Dependency{want[0], want[1], {TaskID: "t-b", Issue: 3, Status: "done"}}; err != nil || !slices.Equal(all, want) {
		t.Fatalf("Dependencies = %+v, %v; want %+v, the done one last", all, err, want)
	}
	dependents, err := s.Dependents(ctx, "t-a")
	if want := []Dependency{{TaskID: "t-x", Issue: 7, Status: "waiting-deps"}, {TaskID: "t-y", Issue: 8, Status: "queued"}}; err != nil || !slices.Equal(dependents, want) {
		t.Fatalf("Dependents(t-a) = %+v, %v; want %+v", dependents, err, want)
	}
	if got, err := s.Dependents(ctx, "t-x"); err != nil || len(got) != 0 {
		t.Fatalf("Dependents(t-x) = %+v, %v; want none", got, err)
	}
	if got, err := s.WaitingDependents(ctx, "t-a"); err != nil || !slices.Equal(got, []string{"t-x"}) {
		t.Fatalf("WaitingDependents(t-a) = %v, %v; want [t-x], the queued card waits on nothing yet", got, err)
	}
	if got, _ := s.OpenDependencies(ctx, "t-b"); len(got) != 0 {
		t.Fatalf("a card without dependencies has open ones: %+v", got)
	}
	for range 2 {
		if err := s.RemoveTaskDeps(ctx, "t-y", []string{"t-a", "t-b"}); err != nil {
			t.Fatalf("RemoveTaskDeps: %v", err)
		}
	}
	if got, err := s.Dependencies(ctx, "t-y"); err != nil || len(got) != 0 {
		t.Fatalf("Dependencies(t-y) after the removal = %+v, %v; want none", got, err)
	}
	if got, err := s.Dependencies(ctx, "t-x"); err != nil || len(got) != 3 {
		t.Fatalf("Dependencies(t-x) after t-y's removal = %+v, %v; want its three", got, err)
	}
	if err := s.AddTaskDeps(ctx, "t-y", []string{"t-a"}); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.BoardTasks(ctx, "sf/r")
	if err != nil {
		t.Fatal(err)
	}
	waiting, blocked := map[string]string{}, map[string]string{}
	for _, b := range tasks {
		waiting[b.ID], blocked[b.ID] = b.WaitingOn, b.Dependencies
	}
	if waiting["t-x"] != "#4, #5" || waiting["t-y"] != "" {
		t.Fatalf("board waiting-on = %q, want #4, #5 on the waiting card only", waiting)
	}
	if blocked["t-x"] != "#4, #5" || blocked["t-y"] != "#5" || blocked["t-a"] != "" {
		t.Fatalf("board dependencies = %q, want the deps not done (#3 is) in any status: t-x #4, #5, t-y #5", blocked)
	}
}

// depsConfig is TaskConfig for a card of process spec, which has the
// dependencies block.
func depsConfig() *TaskConfig {
	return &TaskConfig{Process: "spec", Track: policy.TrackDirect, Class: policy.ClassCoder, Session: policy.SessionCode,
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
		card := Task{ID: "t-c", Kind: "agent", Status: "queued", Track: "direct", Process: "spec", Repository: "sf/repo", IssueNumber: 7,
			Parent: "t-plan", Source: SourcePlan}
		fj := &prForgejo{labels: map[int64][]string{7: {"agent", "state/queued"}}}
		r, store, objects := newActivityRunner(t, card, fj)
		wf := &fakeWorkflows{}
		r.cfg.Workflows = wf
		for id, issue := range map[string]int64{"t-a": 3, "t-b": 5} {
			store.CreateTask(ctx, Task{ID: id, Kind: "agent", Status: "running", Track: "direct", Process: "spec", Repository: "sf/repo",
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
	card := Task{ID: "t-c", Kind: "agent", Status: "waiting-deps", Track: "direct", Process: "spec", Repository: "sf/repo", IssueNumber: 7}
	fj := &prForgejo{labels: map[int64][]string{7: {"agent", "state/queued"}}}
	r, store, objects := newActivityRunner(t, card, fj)
	store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "running", Repository: "sf/repo", IssueNumber: 3, Track: "direct"})
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

// A dependency that ended closed opens the card with the reason: an
// answer starts the card with the reason as a finding, a close ends it
// closed.
func TestClosedDependencyOpensCard(t *testing.T) {
	const reason = "dependency #3 (links-api) ended closed: close this card, or answer to start it anyway"
	failed := func(h *workflowHarness) {
		h.taskConfig = depsConfig()
		h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(
			DepState{Waiting: []int64{5}, Ended: &Dependency{TaskID: "t-a", Issue: 3, Key: "links-api", Status: "closed"}}, nil)
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
		card := Task{ID: "t-c", Kind: "agent", Status: "waiting-deps", Track: "direct", Process: "spec", Repository: "sf/repo", IssueNumber: 7}
		r, store, _ := newActivityRunner(t, card, &prForgejo{})
		store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "closed", Repository: "sf/repo", IssueNumber: 3, PlanKey: "links-api", Track: "direct"})
		store.CreateTask(ctx, Task{ID: "t-b", Kind: "agent", Status: "running", Repository: "sf/repo", IssueNumber: 5, Track: "direct"})
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
// (every dependency done, or one ended closed), not to one still waiting,
// a failed dependency's included, and starts the workflow of every
// waiting card.
func TestHealSignalsWaitingCards(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	for id, st := range map[string]string{"t-done": "done", "t-failed": "failed", "t-closed": "closed", "t-open": "running"} {
		store.CreateTask(ctx, Task{ID: id, Status: st, Kind: "agent", Repository: "sf/ai-software-factory-code", Track: "direct"})
	}
	for id, deps := range map[string][]string{"tw-free": {"t-done"}, "tw-failed": {"t-done", "t-failed"}, "tw-closed": {"t-done", "t-closed", "t-open"},
		"tw-waits": {"t-done", "t-open"}} {
		store.CreateTask(ctx, Task{ID: id, Status: "waiting-deps", Kind: "agent", Repository: "sf/ai-software-factory-code", Track: "direct"})
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
	if !slices.Equal(dep, []string{"tw-closed", "tw-free"}) {
		t.Fatalf("dep_ended to %v, want tw-closed and tw-free", dep)
	}
	for _, id := range []string{"tw-free", "tw-failed", "tw-closed", "tw-waits"} {
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

// Item 119: a dependency that failed keeps the card waiting, with no human
// card, until the failed card is retried; one closed ends the wait. A card
// a human was asked about a failed dependency before (a healed run in
// waiting-human) goes back to the wait.
func TestFailedDependencyKeepsTheCardWaiting(t *testing.T) {
	for _, status := range []string{"queued", "waiting-human"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			card := Task{ID: "t-c", Kind: "agent", Status: status, Track: "direct", Process: "spec", Repository: "sf/repo", IssueNumber: 7}
			r, store, _ := newActivityRunner(t, card, &prForgejo{labels: map[int64][]string{7: {"agent"}}})
			store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "failed", Repository: "sf/repo", IssueNumber: 5, PlanKey: "links", Track: "direct"})
			store.AddTaskDeps(ctx, "t-c", []string{"t-a"})
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
			if d := ask(); !slices.Equal(d.Waiting, []int64{5}) || d.Ended != nil {
				t.Fatalf("failed dependency: %+v, want waiting on 5 and none ended", d)
			}
			if got, _, _ := store.Task(ctx, "t-c"); got.Status != "waiting-deps" || got.Gate != GateDependencies {
				t.Fatalf("card %s in gate %q, want waiting-deps in gate dependencies", got.Status, got.Gate)
			}
			a := store.tasks["t-a"]
			a.Status = "closed"
			store.tasks["t-a"] = a
			if d := ask(); d.Ended == nil || *d.Ended != (Dependency{TaskID: "t-a", Issue: 5, Key: "links", Status: "closed"}) {
				t.Fatalf("closed dependency: Ended = %+v, want #5 closed", d.Ended)
			}
		})
	}
}

// retryCard is linkshort's card links-api of plan t-plan (#1), as
// fileChildren files it.
const retryCard = "Links API\n\nCard links-api of plan #1 (2 of 4), filed for sebastian. Written by the planner.\n\n" +
	"POST /api/links creates a link.\n\nDirectories (guidance, not a boundary): none\nDepends on: #4 (foundation)\nAcceptance:\n" +
	"- POST /api/links answers 201\nSize: small"

// newRetryRig is plan t-plan (#1) on linkshort with its cards: the
// foundation t-f (#4) done, A t-a (#5, links-api) failed with the reason
// "wall-clock cap reached" and its branch at c7, depending on the
// foundation, and B t-b (#6, redirects) waiting on A; issues up to #8
// exist.
func newRetryRig(t *testing.T) (*Runner, *fakeStore, *fakeObjectStore, *planForgejo, *fakeWorkflows) {
	t.Helper()
	ctx := context.Background()
	r, store, objects, fj := newPlanRunner(t, testPlan())
	for n := int64(2); n <= 8; n++ {
		fj.issues[n] = &planIssue{title: fmt.Sprintf("issue %d", n), poster: "factory-agent", state: "open"}
	}
	fj.next = 8
	fj.branches = map[string]string{"main": "m1", "factory/t-a": "c7"}
	card := func(id string, n int64, status, key, objective string) Task {
		return Task{ID: id, Kind: "agent", Status: status, Track: policy.TrackDirect, Process: "spec", Repository: "linkshort/linkshort",
			IssueNumber: n, Source: SourcePlan, Parent: "t-plan", PlanKey: key, Priority: 2, RiskClass: "standard", BudgetEUR: 2,
			Author: "sebastian", AuthorWriter: true, SHA: "b0", Objective: objective}
	}
	for _, c := range []Task{card("t-f", 4, "done", "foundation", "Foundation"), card("t-a", 5, "failed", "links-api", retryCard),
		card("t-b", 6, "waiting-deps", "redirects", "Redirects")} {
		if err := store.CreateTask(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	store.AddTaskDeps(ctx, "t-a", []string{"t-f"})
	store.AddTaskDeps(ctx, "t-b", []string{"t-a"})
	objects.Put(ctx, "sessions/t-a/timeline.jsonl", []byte(`{"step":"final","state":"failed","reason":"wall-clock cap reached"}`+"\n"))
	wf := &fakeWorkflows{}
	r.cfg.Workflows = wf
	return r, store, objects, fj, wf
}

// Item 119: the retry of failed card A files its replacement A' (the same
// plan, key, process, priority, author and base, queued on the direct
// track with the project's direct class, the objective with the line
// naming A and its reason) on a new issue labelled as fileChildren labels a
// card, takes A's key, its dependencies and its dependents, copies its
// branch, starts it, wakes the dependents, and closes A's issue as the
// bot, recorded.
func TestRetryPlanCardFilesAReplacement(t *testing.T) {
	const line = "Replaces #5, which failed: wall-clock cap reached"
	for name, branch := range map[string]bool{"branch": true, "no branch": false} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, store, objects, fj, wf := newRetryRig(t)
			if !branch {
				delete(fj.branches, "factory/t-a")
			}
			fj.calls = nil
			issue, err := r.RetryPlanCard(ctx, "t-a", "alice")
			if err != nil || issue != 9 {
				t.Fatalf("RetryPlanCard = %d, %v; want #9", issue, err)
			}
			cards := cardsByKey(t, store)
			n := cards["links-api"]
			if len(cards) != 3 || n.ID != "t-33198a8c2c69" || cards["redirects"].ID != "t-b" {
				t.Fatalf("plan cards %v, want the foundation, the replacement of A and B", slices.Sorted(maps.Keys(cards)))
			}
			if n.Status != "queued" || n.Track != policy.TrackDirect || !slices.Equal(n.RequiredCapabilities, []string{policy.ClassCoder}) ||
				n.Process != "spec" || n.Source != SourcePlan || n.Parent != "t-plan" ||
				n.Kind != "agent" || n.Priority != 2 || n.RiskClass != "standard" || n.BudgetEUR != 2 || n.Author != "sebastian" || !n.AuthorWriter ||
				n.SHA != "b0" || n.IssueNumber != 9 || n.Repository != "linkshort/linkshort" || n.Objective != retryCard+"\n\n"+line {
				t.Fatalf("replacement = %+v", n)
			}
			if a := store.tasks["t-a"]; a.PlanKey != "" || a.Status != "failed" {
				t.Fatalf("A = %s with key %q, want failed without a key", a.Status, a.PlanKey)
			}
			title, body, _ := strings.Cut(retryCard, "\n\n")
			if is := fj.issues[9]; is.title != title || is.body != body+"\n\n"+line || is.poster != "factory-agent" ||
				!slices.Equal(is.labels, []string{"agent", "state/queued"}) {
				t.Fatalf("issue #9 = %+v", is)
			}
			if !slices.Equal(store.deps[n.ID], []string{"t-f"}) || !slices.Equal(store.deps["t-b"], []string{n.ID}) {
				t.Fatalf("task_deps: replacement %v, B %v; want [t-f] and [%s]", store.deps[n.ID], store.deps["t-b"], n.ID)
			}
			if head, ok := fj.branches["factory/"+n.ID]; ok != branch || branch && head != "c7" {
				t.Fatalf("branch factory/%s = %q (%v), want c7 only when A's branch exists", n.ID, head, ok)
			}
			var onA []string
			for _, c := range fj.comments {
				if c.issue == 5 {
					onA = append(onA, c.body)
				}
			}
			if !slices.Equal(onA, []string{"factoryd: Retried as #9."}) || fj.issues[5].state != "closed" {
				t.Fatalf("#5: comments %q, state %q; want the retry's comment and closed", onA, fj.issues[5].state)
			}
			if !slices.ContainsFunc(store.intakeEvents, func(e IntakeEvent) bool {
				return e.TaskID == "t-a" && e.Kind == IntakeFactoryClosed && e.CommentID == 5
			}) {
				t.Fatalf("intake events %+v, want factoryd's record of closing #5", store.intakeEvents)
			}
			if !slices.Equal(wf.started, []string{n.ID}) || !slices.Equal(wf.signals, []string{"t-b dep_ended  #0: "}) {
				t.Fatalf("started %q, signals %q; want the replacement started and dep_ended to B", wf.started, wf.signals)
			}
			want := []string{"POST issues", "PUT issues/9/labels", "GET fork branch factory/" + n.ID, "POST fork branches factory/" + n.ID,
				"POST issues/5/comments", "PATCH issues/5"}
			if !slices.Equal(fj.calls, want) {
				t.Fatalf("forgejo calls\n%q\nwant\n%q", fj.calls, want)
			}
			if got := events(t, objects, n.ID); !slices.Equal(got, []string{"card_created direct alice"}) {
				t.Fatalf("replacement events %v", got)
			}
			tl, _ := objects.Get(ctx, "sessions/t-a/timeline.jsonl")
			if step := lastStep(tl, "retried"); step == nil || step["as"] != n.ID ||
				step["issue"] != float64(9) || step["by"] != "alice" {
				t.Fatalf("A's retried step = %v", step)
			}
		})
	}
}

// The replacement of a failed card runs on the image the card named.
func TestRetryPlanCardKeepsTheImage(t *testing.T) {
	r, store, _, _, _ := newRetryRig(t)
	a := store.tasks["t-a"]
	a.Image = "browser"
	store.tasks["t-a"] = a
	if _, err := r.RetryPlanCard(context.Background(), "t-a", "alice"); err != nil {
		t.Fatal(err)
	}
	if got := cardsByKey(t, store)["links-api"].Image; got != "browser" {
		t.Fatalf("the replacement's image %q, want browser", got)
	}
}

// A card retried once, a card that did not fail and a task that is no
// card of a plan are refused, and nothing is filed.
func TestRetryRefusedTwice(t *testing.T) {
	ctx := context.Background()
	r, store, _, fj, _ := newRetryRig(t)
	if _, err := r.RetryPlanCard(ctx, "t-a", "alice"); err != nil {
		t.Fatal(err)
	}
	fj.calls = nil
	tasks := len(store.tasks)
	for id, want := range map[string]string{
		"t-a":    "Card #5 was already retried.",
		"t-b":    "Card #6 did not fail: it is waiting-deps.",
		"t-plan": "Task t-plan is no card of a plan.",
	} {
		_, err := r.RetryPlanCard(ctx, id, "alice")
		var no ErrNoRetry
		if !errors.As(err, &no) || err.Error() != want {
			t.Errorf("retry of %s: %v, want the refusal %q", id, err, want)
		}
	}
	if len(fj.calls) != 0 || len(store.tasks) != tasks {
		t.Fatalf("a refused retry acted: forgejo %q, %d tasks (were %d)", fj.calls, len(store.tasks), tasks)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). The
// retry's rows, in one transaction, once: the failed card lets go of its
// key, the replacement takes it, the failed card's dependencies and its
// dependents; the plan's cards are those that hold a key.
func TestPGStoreRetryCard(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_retry"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-plan", Status: "done", IssueNumber: 1, Source: "webhook", Track: policy.TrackPlan},
		{ID: "t-f", Status: "done", IssueNumber: 4, PlanKey: "foundation", Track: "direct"},
		{ID: "t-a", Status: "failed", IssueNumber: 5, PlanKey: "links", Track: "direct"},
		{ID: "t-b", Status: "waiting-deps", IssueNumber: 6, PlanKey: "redirects", Track: "direct"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind = 1, "sf/r", "agent"
		if tk.ID != "t-plan" {
			tk.Source, tk.Parent, tk.Track = SourcePlan, "t-plan", policy.TrackDirect
		}
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, "t-a", []string{"t-f"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, "t-b", []string{"t-a"}); err != nil {
		t.Fatal(err)
	}
	card := func(id, key string) Task {
		return Task{ID: id, Status: "queued", Track: policy.TrackDirect, BudgetEUR: 1, Repository: "sf/r", Kind: "agent",
			Source: SourcePlan, Parent: "t-plan", PlanKey: key}
	}
	if ok, err := s.ReplaceFailedCard(ctx, "t-a", card("t-a2", "links")); !ok || err != nil {
		t.Fatalf("first retry = %v, %v; want done", ok, err)
	}
	if a, _, _ := s.Task(ctx, "t-a"); a.PlanKey != "" || a.Status != "failed" {
		t.Fatalf("A = %s with key %q, want failed without a key", a.Status, a.PlanKey)
	}
	if got, _, _ := s.Task(ctx, "t-a2"); got.PlanKey != "links" || got.Status != "queued" || got.Track != policy.TrackDirect {
		t.Fatalf("replacement = %s with key %q", got.Status, got.PlanKey)
	}
	for _, again := range []struct{ of, id, key string }{{"t-a", "t-a3", "links"}, {"t-f", "t-f2", "foundation"}} {
		if ok, err := s.ReplaceFailedCard(ctx, again.of, card(again.id, again.key)); ok || err != nil {
			t.Fatalf("retry of %s = %v, %v; want refused", again.of, ok, err)
		}
		if _, found, _ := s.Task(ctx, again.id); found {
			t.Fatalf("a refused retry of %s inserted %s", again.of, again.id)
		}
	}
	if err := s.SetTaskIssue(ctx, "t-a2", 9); err != nil {
		t.Fatal(err)
	}
	var on []string
	rows, err := s.db.QueryContext(ctx, `SELECT depends_on FROM task_deps WHERE task_id = 't-a2'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d string
		rows.Scan(&d)
		on = append(on, d)
	}
	rows.Close()
	if !slices.Equal(on, []string{"t-f"}) {
		t.Fatalf("the replacement depends on %v, want [t-f]", on)
	}
	open, err := s.OpenDependencies(ctx, "t-b")
	if err != nil || !slices.Equal(open, []Dependency{{TaskID: "t-a2", Issue: 9, Key: "links", Status: "queued"}}) {
		t.Fatalf("B waits on %+v, %v; want the replacement #9", open, err)
	}
	if got, _ := s.WaitingDependents(ctx, "t-a"); len(got) != 0 {
		t.Fatalf("A still has dependents %v", got)
	}
	cards, err := s.PlanCards(ctx, "t-plan")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cards {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"t-a2", "t-b", "t-f"}) {
		t.Fatalf("plan cards %v, want those holding a key", ids)
	}
}

// cancelOnReplace cancels the request's context once the retry's
// transaction committed, as a browser that leaves mid-way does.
type cancelOnReplace struct {
	*fakeStore
	cancel context.CancelFunc
}

func (s cancelOnReplace) ReplaceFailedCard(ctx context.Context, id string, t Task) (bool, error) {
	defer s.cancel()
	return s.fakeStore.ReplaceFailedCard(ctx, id, t)
}

// A retry whose request is cancelled after its transaction still files the
// issue, moves the dependencies and closes the failed card's issue.
func TestRetryFinishesAfterTheClientLeaves(t *testing.T) {
	r, store, _, fj, _ := newRetryRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.store = cancelOnReplace{store, cancel}
	if issue, err := r.RetryPlanCard(ctx, "t-a", "alice"); err != nil || issue != 9 {
		t.Fatalf("RetryPlanCard = %d, %v; want #9", issue, err)
	}
	if fj.issues[5].state != "closed" || !slices.Equal(store.deps["t-b"], []string{cardsByKey(t, store)["links-api"].ID}) {
		t.Fatalf("#5 %s, B waits on %v; want #5 closed and B on the replacement", fj.issues[5].state, store.deps["t-b"])
	}
}

// A retry that stops at a failed Forgejo call after its transaction is
// finished by the next Retry, which finds the replacement by its id: one
// issue, B on the replacement, the branch copied once (a branch already
// there is kept), #5 commented once and closed; a third Retry is refused.
func TestRetryResumesAfterAFailedStep(t *testing.T) {
	const replacement = "t-33198a8c2c69"
	for name, fail := range map[string]func(*planForgejo){
		"labels":  func(fj *planForgejo) { fj.failLabel = 1 },
		"comment": func(fj *planForgejo) { fj.failCall = "POST issues/5/comments" },
		"close":   func(fj *planForgejo) { fj.failCall = "PATCH issues/5" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, store, _, fj, _ := newRetryRig(t)
			fail(fj)
			if _, err := r.RetryPlanCard(ctx, "t-a", "alice"); err == nil || errors.As(err, new(ErrNoRetry)) {
				t.Fatalf("first retry: %v, want it stopped by Forgejo", err)
			}
			if got := store.tasks[replacement]; got.IssueNumber != 9 || !slices.Equal(store.deps["t-b"], []string{replacement}) ||
				fj.issues[5].state == "closed" {
				t.Fatalf("after the failed step: replacement on #%d, B waits on %v, #5 %s; want #9, the replacement, #5 open",
					got.IssueNumber, store.deps["t-b"], fj.issues[5].state)
			}
			wantHead := "c7"
			if _, copied := fj.branches["factory/"+replacement]; copied {
				wantHead = "c8"
				fj.branches["factory/"+replacement] = wantHead
			}
			if issue, err := r.RetryPlanCard(ctx, "t-a", "alice"); err != nil || issue != 9 {
				t.Fatalf("second retry = %d, %v; want #9", issue, err)
			}
			var onA []string
			for _, c := range fj.comments {
				if c.issue == 5 {
					onA = append(onA, c.body)
				}
			}
			if fj.next != 9 || !slices.Equal(fj.issues[9].labels, []string{"agent", "state/queued"}) ||
				fj.branches["factory/"+replacement] != wantHead || fj.issues[5].state != "closed" || !slices.Equal(onA, []string{"factoryd: Retried as #9."}) {
				t.Fatalf("after the second retry: %d issues, #9 labels %v, branch %q, #5 %s with %q",
					fj.next, fj.issues[9].labels, fj.branches["factory/"+replacement], fj.issues[5].state, onA)
			}
			var no ErrNoRetry
			if _, err := r.RetryPlanCard(ctx, "t-a", "alice"); !errors.As(err, &no) || err.Error() != "Card #5 was already retried." {
				t.Fatalf("third retry: %v, want already retried", err)
			}
		})
	}
}

// A healed card that goes back to the wait asks about a dependency that
// ends closed later with that dependency's question, not the healed one.
func TestHealedCardBackInTheWaitAsksAnew(t *testing.T) {
	const reason = "dependency #6 (redirects) ended closed: close this card, or answer to start it anyway"
	h := newWorkflowHarness(t)
	h.taskConfig = depsConfig()
	h.state = RunState{Status: "waiting-human", Question: "dependency #5 (links-api) ended failed: close this card, or answer to start it anyway"}
	start := h.env.Now()
	h.env.OnActivity(ActivityDependencies, mock.Anything, "t1").Return(func(ctx context.Context, taskID string) (DepState, error) {
		if h.env.Now().Sub(start) < 3*time.Minute {
			return DepState{Waiting: []int64{5, 6}}, nil
		}
		return DepState{Waiting: []int64{5}, Ended: &Dependency{TaskID: "t-r", Issue: 6, Key: "redirects", Status: "closed"}}, nil
	})
	h.signalAfter(3*time.Minute, SignalDepEnded, TaskSignal{})
	h.signalAfter(time.Hour, SignalIssueClosed, TaskSignal{Author: "sebastian"})
	if final := h.run(t); final != "closed" {
		t.Fatalf("final = %q", final)
	}
	if !slices.Equal(h.escalated, []string{reason}) {
		t.Fatalf("escalated %q, want the closed dependency's question", h.escalated)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A failed
// plan card that a waiting card depends on stays on the board past the
// week the board shows finished tasks; one nothing waits on does not.
func TestPGStoreBoardKeepsABlockingFailedCard(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_retry_board"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tk := range []Task{
		{ID: "t-plan", Status: "done", IssueNumber: 1, Track: "direct"},
		{ID: "t-a", Status: "failed", IssueNumber: 5, PlanKey: "links", Track: "direct"},
		{ID: "t-b", Status: "waiting-deps", IssueNumber: 6, PlanKey: "redirects", Track: "direct"},
		{ID: "t-c", Status: "failed", IssueNumber: 7, PlanKey: "search", Track: "direct"},
		{ID: "t-d", Status: "done", IssueNumber: 8, PlanKey: "tags", Track: "direct"},
	} {
		tk.BudgetEUR, tk.Repository, tk.Kind, tk.Source = 1, "sf/r", "agent", "webhook"
		if tk.ID != "t-plan" {
			tk.Source, tk.Parent = SourcePlan, "t-plan"
		}
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTaskDeps(ctx, "t-b", []string{"t-a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskDeps(ctx, "t-d", []string{"t-c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET finished_at = now() - interval '8 days' WHERE id IN ('t-plan', 't-a', 't-c', 't-d')`); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.BoardTasks(ctx, "sf/r")
	if err != nil {
		t.Fatal(err)
	}
	done, _, err := s.FinishedTasks(ctx, FinishedQuery{Repository: "sf/r", Statuses: FinalStatuses, Since: time.Now().Add(-7 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	tasks = append(tasks, done...)
	var ids []string
	for _, b := range tasks {
		ids = append(ids, b.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"t-a", "t-b"}) {
		t.Fatalf("board = %v, want the waiting card and the failed card it waits on", ids)
	}
}

func TestContinueBranch(t *testing.T) {
	ctx := context.Background()
	from := Task{ID: "t-a", SHA: "b0"}
	to := Task{ID: "t-n"}
	posts := func(fj *planForgejo) (n int) {
		for _, c := range fj.calls {
			if strings.HasPrefix(c, "POST fork branches") {
				n++
			}
		}
		return n
	}
	t.Run("puts the new branch at the old head", func(t *testing.T) {
		r, _, _, fj, _ := newRetryRig(t)
		head, err := r.ContinueBranch(ctx, from, to)
		if err != nil || head != "c7" || fj.branches["factory/t-n"] != "c7" {
			t.Fatalf("ContinueBranch = %q, %v; branch %q; want c7 on both", head, err, fj.branches["factory/t-n"])
		}
	})
	t.Run("twice creates once", func(t *testing.T) {
		r, _, _, fj, _ := newRetryRig(t)
		r.ContinueBranch(ctx, from, to)
		head, err := r.ContinueBranch(ctx, from, to)
		if err != nil || head != "c7" || posts(fj) != 1 {
			t.Fatalf("second ContinueBranch = %q, %v with %d POST fork branches; want c7, one", head, err, posts(fj))
		}
	})
	t.Run("keeps an existing branch", func(t *testing.T) {
		r, _, _, fj, _ := newRetryRig(t)
		fj.branches["factory/t-n"] = "c9"
		head, err := r.ContinueBranch(ctx, from, to)
		if err != nil || head != "c9" || fj.branches["factory/t-n"] != "c9" {
			t.Fatalf("ContinueBranch = %q, %v; branch %q; want c9 kept", head, err, fj.branches["factory/t-n"])
		}
	})
	t.Run("a missing old branch", func(t *testing.T) {
		r, _, _, fj, _ := newRetryRig(t)
		delete(fj.branches, "factory/t-a")
		_, err := r.ContinueBranch(ctx, from, to)
		if _, made := fj.branches["factory/t-n"]; !errors.Is(err, ErrNoBranch) || made {
			t.Fatalf("ContinueBranch err = %v, branch made %v; want ErrNoBranch, none", err, made)
		}
	})
}

func TestContinuedCardResumesOnTheBranch(t *testing.T) {
	ctx := context.Background()
	r, store, _, _, _ := newRetryRig(t)
	to := Task{ID: "t-n", Kind: "agent", Status: "queued", Track: policy.TrackDirect, Process: "spec", Repository: "linkshort/linkshort", SHA: "b0", Source: SourcePlan, Parent: "t-plan", PlanKey: "n"}
	if err := store.CreateTask(ctx, to); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ContinueBranch(ctx, Task{ID: "t-a", SHA: "b0"}, to); err != nil {
		t.Fatal(err)
	}
	_, repo, err := r.task(ctx, to.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := r.startRef(ctx, &to, repo, ""); err != nil || ref != "c7" || to.SHA != "b0" {
		t.Fatalf("startRef = %q, %v with SHA %q; want c7 and b0", ref, err, to.SHA)
	}
	if head, err := r.BranchHead(ctx, to.ID); err != nil || head != "c7" {
		t.Fatalf("BranchHead = %q, %v; want c7", head, err)
	}
}
