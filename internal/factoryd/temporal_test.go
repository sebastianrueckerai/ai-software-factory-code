package factoryd

import (
	"bytes"
	"context"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
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
// whose workflow died in review or mid-completion is not stranded. A task
// waiting on Forgejo (in review, on a human) is re-read there too (I1); a
// running one is not, its own run reads Forgejo.
func TestHealPassCoversEveryUnfinishedAgentTask(t *testing.T) {
	store := newFakeStore()
	for id, st := range map[string]string{"tq": "queued", "tr": "running", "tv": "in-review", "tw": "waiting-human", "tc": "closing", "td": "done", "tf": "failed"} {
		store.CreateTask(context.Background(), Task{ID: id, Status: st, Kind: "agent", Repository: "sf/ai-software-factory-code", IssueNumber: 30})
	}
	store.CreateTask(context.Background(), Task{ID: "tbroken", Status: "running", Kind: "agent", Repository: "no-slash"})
	store.CreateTask(context.Background(), Task{ID: "th", Status: "waiting-human", Kind: "human", Repository: "sf/ai-software-factory-code", IssueNumber: 30})
	wf := &fakeWorkflows{}
	forgejo, _ := resyncForgejoStub(t)
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	HealPass(context.Background(), store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent"}, newCompletion(store, fjURL, llURL))
	slices.Sort(wf.started)
	if want := []string{"tc", "tq", "tr", "tv", "tw"}; !slices.Equal(wf.started, want) {
		t.Fatalf("started = %v, want %v", wf.started, want)
	}
	slices.Sort(wf.signals)
	want := []string{"tv human_comment sebastian #0 c80: answer: do X", "tv issue_closed factoryd heal pass #0: ",
		"tw human_comment sebastian #0 c80: answer: do X", "tw issue_closed factoryd heal pass #0: "}
	if !slices.Equal(wf.signals, want) {
		t.Fatalf("signals = %q, want the closed issue and the owner's comment re-read for the in-review and waiting-human tasks only", wf.signals)
	}
	if len(store.pruned) != 1 || store.pruned[0] != 30*24*time.Hour {
		t.Fatalf("pruned = %v, want one pass forgetting deliveries older than 30 days", store.pruned)
	}
	if !slices.Equal(back.litellmCalls, []string{"GET /key/list"}) || !slices.ContainsFunc(back.forgejoCalls, func(c string) bool { return strings.HasSuffix(c, "/tokens") }) {
		t.Fatalf("litellm calls = %v, forgejo calls = %v: want the broken task completed like any failure (tokens listed for revocation, keys listed for blocking)", back.litellmCalls, back.forgejoCalls)
	}
	if got, _, _ := store.Task(context.Background(), "tbroken"); got.Status != "failed" {
		t.Fatalf("broken task = %q, want failed outside any workflow, so the pass skips it next time", got.Status)
	}
	if got, _, _ := store.Task(context.Background(), "th"); got.Status != "done" {
		t.Fatalf("human task = %q, want done: its issue closed while the webhook was missed", got.Status)
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

// runningService is a Temporal frontend whose every start meets a running
// execution.
type runningService struct {
	workflowservice.UnimplementedWorkflowServiceServer
}

func (runningService) GetSystemInfo(context.Context, *workflowservice.GetSystemInfoRequest) (*workflowservice.GetSystemInfoResponse, error) {
	return &workflowservice.GetSystemInfoResponse{Capabilities: &workflowservice.GetSystemInfoResponse_Capabilities{}}, nil
}

func (runningService) StartWorkflowExecution(context.Context, *workflowservice.StartWorkflowExecutionRequest) (*workflowservice.StartWorkflowExecutionResponse, error) {
	return nil, serviceerror.ToStatus(serviceerror.NewWorkflowExecutionAlreadyStarted("running", "start-1", "run-1")).Err()
}

// The SDK swallows the frontend's already-started refusal into the existing
// run unless told otherwise: through the real client, a start against a
// running workflow must still reach Start as that refusal and log nothing.
func TestTemporalStartIsQuietAgainstARunningWorkflow(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	workflowservice.RegisterWorkflowServiceServer(srv, runningService{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	c, err := client.Dial(client.Options{HostPort: "passthrough:///bufconn", ConnectionOptions: client.ConnectionOptions{DialOptions: []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := captureLog(t)
	if err := (TemporalWorkflows{Client: c}).Start(context.Background(), "t1"); err != nil {
		t.Fatalf("Start = %v, want a quiet no-op", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("log = %q, want nothing for a start against a running workflow", buf.String())
	}
}
