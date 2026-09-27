package factoryd

import (
	"bytes"
	"context"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
)

// A workflow that failed or was terminated may be started again by the
// heal pass; a completed one is refused, and the refusal is not an error.
func TestTemporalStartAllowsRestartOfFailedWorkflowsOnly(t *testing.T) {
	c := &mocks.Client{}
	var opts client.StartWorkflowOptions
	c.On("ExecuteWorkflow", mock.Anything, mock.Anything, "TaskWorkflow", "t1").
		Run(func(args mock.Arguments) { opts = args.Get(1).(client.StartWorkflowOptions) }).
		Return(nil, serviceerror.NewWorkflowExecutionAlreadyStarted("done already", "", ""))
	if err := (TemporalWorkflows{Client: c}).Start(context.Background(), "t1"); err != nil {
		t.Fatalf("Start = %v, want an already-started refusal to be success", err)
	}
	if opts.ID != "task-t1" || opts.TaskQueue != TaskQueue ||
		opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY {
		t.Fatalf("options = %+v", opts)
	}
}

// Every agent task not yet finished gets its workflow (re)started: a task
// whose workflow died in review or mid-completion is not stranded.
func TestHealPassCoversEveryUnfinishedAgentTask(t *testing.T) {
	store := newFakeStore()
	for id, st := range map[string]string{"tq": "queued", "tr": "running", "tv": "in-review", "tw": "waiting-human", "tc": "closing", "td": "done", "tf": "failed"} {
		store.CreateTask(context.Background(), Task{ID: id, Status: st, Kind: "agent", Repository: "sf/repo"})
	}
	store.CreateTask(context.Background(), Task{ID: "tbroken", Status: "running", Kind: "agent", Repository: "no-slash"})
	store.CreateTask(context.Background(), Task{ID: "th", Status: "queued", Kind: "human"})
	wf := &fakeWorkflows{}
	HealPass(context.Background(), store, wf)
	slices.Sort(wf.started)
	if want := []string{"tc", "tq", "tr", "tv", "tw"}; !slices.Equal(wf.started, want) {
		t.Fatalf("started = %v, want %v", wf.started, want)
	}
	if len(store.pruned) != 1 || store.pruned[0] != 30*24*time.Hour {
		t.Fatalf("pruned = %v, want one pass forgetting deliveries older than 30 days", store.pruned)
	}
	if got, _, _ := store.Task(context.Background(), "tbroken"); got.Status != "failed" {
		t.Fatalf("broken task = %q, want failed outside any workflow, so the pass skips it next time", got.Status)
	}
}

// A merge or issue close that lands while no run exists (between a
// failure and the next heal pass) must not be lost: the signal starts the
// workflow, which reconciles from Forgejo first.
func TestTemporalSignalStartsTheWorkflowWhenNoneRuns(t *testing.T) {
	c := &mocks.Client{}
	var opts client.StartWorkflowOptions
	var sig TaskSignal
	c.On("SignalWithStartWorkflow", mock.Anything, "task-t1", SignalPRMerged, mock.Anything, mock.Anything, "TaskWorkflow", "t1").
		Run(func(args mock.Arguments) {
			sig = args.Get(3).(TaskSignal)
			opts = args.Get(4).(client.StartWorkflowOptions)
		}).Return(nil, nil)
	if err := (TemporalWorkflows{Client: c}).Signal(context.Background(), "t1", SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9}); err != nil {
		t.Fatal(err)
	}
	if sig.PR != 9 || opts.ID != "task-t1" || opts.TaskQueue != TaskQueue ||
		opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY {
		t.Fatalf("signal %+v, options %+v", sig, opts)
	}

	done := &mocks.Client{}
	done.On("SignalWithStartWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, serviceerror.NewWorkflowExecutionAlreadyStarted("completed", "", ""))
	if err := (TemporalWorkflows{Client: done}).Signal(context.Background(), "t1", SignalPRMerged, TaskSignal{}); err != nil {
		t.Fatalf("signal to a completed workflow = %v, want no-op", err)
	}
}

// captureLog redirects the standard logger into a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// An operator reads a task's path in factoryd's log: a real start and a
// delivered signal each leave a line; a refused duplicate start does not.
func TestTemporalStartAndSignalAreLogged(t *testing.T) {
	buf := captureLog(t)
	c := &mocks.Client{}
	c.On("ExecuteWorkflow", mock.Anything, mock.Anything, "TaskWorkflow", "t1").Return(nil, nil)
	c.On("SignalWithStartWorkflow", mock.Anything, "task-t1", SignalPRMerged, mock.Anything, mock.Anything, "TaskWorkflow", "t1").Return(nil, nil)
	w := TemporalWorkflows{Client: c}
	if err := w.Start(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if err := w.Signal(context.Background(), "t1", SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task t1: workflow task-t1 started", "task t1: signal pr_merged from sebastian delivered"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log = %q, want a line containing %q", buf.String(), want)
		}
	}

	buf.Reset()
	dup := &mocks.Client{}
	dup.On("ExecuteWorkflow", mock.Anything, mock.Anything, "TaskWorkflow", "t1").
		Return(nil, serviceerror.NewWorkflowExecutionAlreadyStarted("running", "", ""))
	if err := (TemporalWorkflows{Client: dup}).Start(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("log = %q, want nothing for a refused duplicate start (the heal pass tries every minute)", buf.String())
	}
}

// The SDK swallows an already-started refusal into the existing run unless
// told otherwise; Start relies on seeing that error to stay quiet.
func TestStartOptionsSurfaceAnAlreadyStartedRun(t *testing.T) {
	if !startOptions("t1").WorkflowExecutionErrorWhenAlreadyStarted {
		t.Fatal("startOptions must set WorkflowExecutionErrorWhenAlreadyStarted")
	}
}
