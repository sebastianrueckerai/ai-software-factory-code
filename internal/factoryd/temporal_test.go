package factoryd

import (
	"context"
	"slices"
	"testing"

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
func TestStartOpenWorkflowsCoversEveryUnfinishedAgentTask(t *testing.T) {
	store := newFakeStore()
	for id, st := range map[string]string{"tq": "queued", "tr": "running", "tv": "in-review", "tc": "closing", "td": "done", "tf": "failed"} {
		store.CreateTask(context.Background(), Task{ID: id, Status: st, Kind: "agent"})
	}
	store.CreateTask(context.Background(), Task{ID: "th", Status: "queued", Kind: "human"})
	wf := &fakeWorkflows{}
	StartOpenWorkflows(context.Background(), store, wf)
	slices.Sort(wf.started)
	if want := []string{"tc", "tq", "tr", "tv"}; !slices.Equal(wf.started, want) {
		t.Fatalf("started = %v, want %v", wf.started, want)
	}
}
