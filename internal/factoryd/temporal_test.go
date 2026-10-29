package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
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
// waiting on Forgejo (in review, on a human) is re-read there too; a
// running one is not, its own run reads Forgejo. A merged card's workflow
// ended at its merge: the chain poller ends it, nothing restarts it.
func TestHealPassCoversEveryUnfinishedAgentTask(t *testing.T) {
	store := newFakeStore()
	for id, st := range map[string]string{"tq": "queued", "tr": "running", "tv": "in-review", "tw": "waiting-human", "ti": "waiting-image", "tc": "closing", "tm": "merged", "td": "done", "tf": "failed"} {
		store.CreateTask(context.Background(), withStatus(Task{ID: id, Kind: "agent", Track: "direct", Repository: "sf/ai-software-factory-code", IssueNumber: 30}, st))
	}
	store.CreateTask(context.Background(), Task{ID: "tbroken", Status: "running", Kind: "agent", Track: "direct", Repository: "no-slash"})
	store.CreateTask(context.Background(), Task{ID: "th", Status: "waiting-human", Kind: "human", Repository: "sf/ai-software-factory-code", IssueNumber: 30})
	wf := &fakeWorkflows{}
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	m, reader := testMetrics(t)
	completion := newCompletion(store, fjURL, llURL)
	completion.Metrics = m
	objects := &fakeObjectStore{}
	HealPass(context.Background(), store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent", Metrics: m,
		Timeline: &Timeline{Objects: objects}}, completion, noneCompleted)
	if got := events(t, objects, "tbroken"); !slices.Equal(got, []string{"task_ended failed"}) {
		t.Fatalf("the failed task's events = %v", got)
	}
	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=0,merge_rule=human,model_alias=none,process=none,project=no-slash,state=failed,track=direct":                1,
		"attempts=0,merge_rule=human,model_alias=none,process=none,project=sf-ai-software-factory-code,state=done,track=none": 1,
	})
	// the queued tq starts twice, idempotently: once in the slot the closing
	// tc frees as it ends (taskEnded's pull), once in the pass's own pull
	slices.Sort(wf.started)
	if want := []string{"tc", "ti", "tq", "tq", "tr", "tv", "tw"}; !slices.Equal(wf.started, want) {
		t.Fatalf("started = %v, want %v", wf.started, want)
	}
	slices.Sort(wf.signals)
	want := []string{"ti human_comment sebastian #0 c80: answer: do X", "ti issue_closed sebastian #0: ",
		"tv human_comment sebastian #0 c80: answer: do X", "tv issue_closed sebastian #0: ",
		"tw human_comment sebastian #0 c80: answer: do X", "tw issue_closed sebastian #0: "}
	if !slices.Equal(wf.signals, want) {
		t.Fatalf("signals = %q, want the closed issue and the owner's comment re-read for the waiting and in-review tasks only", wf.signals)
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

// noneCompleted is the heal pass's run check where no run completed.
func noneCompleted(context.Context, string) (bool, error) { return false, nil }

// An open card whose last run completed, a reopen that moved its row but
// started no run, gets a new run; Start would be refused. A card whose
// run is open or failed starts as before, and one its run just finished
// (final as the pass looks again) is left alone.
func TestHealPassReopensACardWhoseRunCompleted(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	for id, st := range map[string]string{"tq": "queued", "tr": "running", "tx": "running"} {
		store.CreateTask(ctx, Task{ID: id, Status: st, Kind: "agent", Track: "direct", Repository: "sf/ai-software-factory-code", IssueNumber: 30})
	}
	completed := func(_ context.Context, id string) (bool, error) {
		switch id {
		case "tq":
			return true, nil
		case "tx":
			x := store.tasks["tx"]
			x.Status = "done"
			store.tasks["tx"] = x
			return true, nil
		}
		return false, nil
	}
	wf := &fakeWorkflows{}
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	HealPass(ctx, store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent",
		Timeline: &Timeline{Objects: &fakeObjectStore{}}}, newCompletion(store, fjURL, llURL), completed)
	if !reflect.DeepEqual(wf.reopens, []reopenCall{{taskID: "tq"}}) || !slices.Equal(wf.started, []string{"tr"}) {
		t.Fatalf("reopens %+v, starts %v; want tq reopened, tr started, tx left", wf.reopens, wf.started)
	}
}

// Completed answers whether the task's last run completed: a missing,
// running, failed or terminated run did not, and a Temporal outage is an
// error.
func TestTemporalCompletedIsACompletedRun(t *testing.T) {
	for name, tc := range map[string]struct {
		res  *workflowservice.DescribeWorkflowExecutionResponse
		err  error
		want bool
	}{
		"completed":  {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}}, nil, true},
		"running":    {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}}, nil, false},
		"failed":     {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_FAILED}}, nil, false},
		"terminated": {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED}}, nil, false},
		"not found":  {nil, serviceerror.NewNotFound("no such workflow"), false},
	} {
		t.Run(name, func(t *testing.T) {
			c := &mocks.Client{}
			c.On("DescribeWorkflowExecution", mock.Anything, "task-t1", "").Return(tc.res, tc.err)
			got, err := TemporalWorkflows{Client: c}.Completed(context.Background(), "t1")
			if err != nil || got != tc.want {
				t.Fatalf("Completed = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	c := &mocks.Client{}
	c.On("DescribeWorkflowExecution", mock.Anything, "task-t1", "").Return(nil, errors.New("frontier down"))
	if _, err := (TemporalWorkflows{Client: c}).Completed(context.Background(), "t1"); err == nil {
		t.Fatal("a Temporal outage must be an error")
	}
}

// A project's chat task runs no workflow: the heal pass starts none for
// it, beside the running card it does start.
func TestHealPassLeavesTheChatTaskBe(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	store.CreateTask(ctx, Task{ID: "tr", Status: "running", Kind: "agent", Repository: "sf/ai-software-factory-code", IssueNumber: 30, Track: "direct"})
	store.CreateTask(ctx, Task{ID: "t-chat-code-2026-10", Status: "running", Kind: KindChat, Repository: "sf/ai-software-factory-code"})
	wf := &fakeWorkflows{}
	forgejo, _ := resyncForgejoStub(t, "sebastian")
	back := &recordingBackend{forgejoStatus: 204, litellmStatus: 200}
	fjURL, llURL := back.srv(t)
	HealPass(ctx, store, wf, &Intake{Store: store, Forgejo: forgejo, Workflows: wf, BotUser: "factory-agent",
		Timeline: &Timeline{Objects: &fakeObjectStore{}}}, newCompletion(store, fjURL, llURL), noneCompleted)
	if !slices.Equal(wf.started, []string{"tr"}) {
		t.Fatalf("started = %v, want only the running card tr", wf.started)
	}
	if got, _, _ := store.Task(ctx, "t-chat-code-2026-10"); got.Status != "running" {
		t.Fatalf("chat task = %q, want it left running", got.Status)
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

// A reopen starts a new run of a completed workflow, with its signal when
// it has one; a run that runs already is no error.
func TestTemporalReopenStartsANewRun(t *testing.T) {
	c := &mocks.Client{}
	var opts []client.StartWorkflowOptions
	c.On("ExecuteWorkflow", mock.Anything, mock.Anything, "TaskWorkflow", "t1").
		Run(func(args mock.Arguments) { opts = append(opts, args.Get(1).(client.StartWorkflowOptions)) }).Return(nil, nil)
	var sig TaskSignal
	c.On("SignalWithStartWorkflow", mock.Anything, "task-t1", SignalToPlan, mock.Anything, mock.Anything, "TaskWorkflow", "t1").
		Run(func(args mock.Arguments) {
			sig = args.Get(3).(TaskSignal)
			opts = append(opts, args.Get(4).(client.StartWorkflowOptions))
		}).Return(nil, nil)
	w := TemporalWorkflows{Client: c}
	if err := w.Reopen(context.Background(), "t1", "", TaskSignal{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Reopen(context.Background(), "t1", SignalToPlan, TaskSignal{Author: "ana", Body: "too big"}); err != nil {
		t.Fatal(err)
	}
	if len(opts) != 2 || sig != (TaskSignal{Author: "ana", Body: "too big"}) {
		t.Fatalf("starts %+v, signal %+v; want one plain start and one with the signal", opts, sig)
	}
	for _, o := range opts {
		if o.ID != "task-t1" || o.TaskQueue != TaskQueue || o.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE {
			t.Fatalf("options %+v, want a new run of task-t1 allowed after any end", o)
		}
	}

	running := &mocks.Client{}
	running.On("ExecuteWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, serviceerror.NewWorkflowExecutionAlreadyStarted("running", "", ""))
	if err := (TemporalWorkflows{Client: running}).Reopen(context.Background(), "t1", "", TaskSignal{}); err != nil {
		t.Fatalf("a reopen of a running run = %v, want nil", err)
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

// The heal pass takes in an agent issue whose webhook was lost: its task
// exists and its workflow runs after one pass.
func TestHealPassTakesInMissedIssues(t *testing.T) {
	in, store, _ := newTestIntake(t)
	in.Projects = codeRepoOnly{}
	HealPass(context.Background(), store, in.Workflows, in, &Completion{Store: store}, noneCompleted)
	var agent string
	for _, tk := range store.tasks {
		if tk.IssueNumber == 7 {
			agent = tk.ID
		}
	}
	if agent == "" || !slices.Contains(workflowsOf(in).started, agent) {
		t.Fatalf("tasks = %+v, started = %q: want #7's task with its workflow started", store.tasks, workflowsOf(in).started)
	}
}

// A gate trip goes to the running workflow only: a completed one has
// nothing left to supervise, so no signal starts a new run.
func TestTemporalSignalGateSignalsTheRunningWorkflow(t *testing.T) {
	c := &mocks.Client{}
	trip := GateTrip{Activity: ActivityWaitChecks, ActivityID: "7", Attempt: 1, AlertID: 3}
	c.On("SignalWorkflow", mock.Anything, "task-t1", "", SignalGateBound, trip).Return(nil)
	if err := (TemporalWorkflows{Client: c}).SignalGate(context.Background(), "task-t1", trip); err != nil {
		t.Fatal(err)
	}
	c.AssertExpectations(t)
}

// The last failure is the pending activity run's, by its id.
func TestTemporalLastFailureIsThePendingRunsOwn(t *testing.T) {
	c := &mocks.Client{}
	c.On("DescribeWorkflowExecution", mock.Anything, "task-t1", "").Return(&workflowservice.DescribeWorkflowExecutionResponse{
		PendingActivities: []*workflowpb.PendingActivityInfo{
			{ActivityId: "5", LastFailure: &failurepb.Failure{Message: "other"}},
			{ActivityId: "7", LastFailure: &failurepb.Failure{Message: "poll: connection reset"}},
			{ActivityId: "9"},
		}}, nil)
	w := TemporalWorkflows{Client: c}
	for id, want := range map[string]string{"7": "poll: connection reset", "9": "", "11": ""} {
		if got, err := w.LastFailure(context.Background(), "task-t1", id); err != nil || got != want {
			t.Errorf("LastFailure(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
}

// Alive answers whether the task's workflow runs now: a missing, completed,
// failed or terminated workflow is not alive, and a Temporal outage is an
// error, not a dead workflow.
func TestTemporalAliveIsARunningWorkflow(t *testing.T) {
	for name, tc := range map[string]struct {
		res  *workflowservice.DescribeWorkflowExecutionResponse
		err  error
		want bool
	}{
		"running":          {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}}, nil, true},
		"completed":        {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED}}, nil, false},
		"failed":           {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_FAILED}}, nil, false},
		"canceled":         {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED}}, nil, false},
		"continued as new": {&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW}}, nil, false},
		"not found":        {nil, serviceerror.NewNotFound("no such workflow"), false},
	} {
		t.Run(name, func(t *testing.T) {
			c := &mocks.Client{}
			c.On("DescribeWorkflowExecution", mock.Anything, "task-t1", "").Return(tc.res, tc.err)
			got, err := TemporalWorkflows{Client: c}.Alive(context.Background(), "t1")
			if err != nil || got != tc.want {
				t.Fatalf("Alive = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	c := &mocks.Client{}
	c.On("DescribeWorkflowExecution", mock.Anything, "task-t1", "").Return(nil, errors.New("frontier down"))
	if _, err := (TemporalWorkflows{Client: c}).Alive(context.Background(), "t1"); err == nil {
		t.Fatal("a Temporal outage must be an error, not a dead workflow")
	}
}

// staleStore is escalateStale's store double: the calls it saw, and an
// injectable failure of the intake insert.
type staleStore struct {
	calls      []string
	status     string
	failInsert error
}

func (s *staleStore) ListTasksByStatus(ctx context.Context, status string) ([]Task, error) {
	return nil, nil
}
func (s *staleStore) NewestTaskEvents(ctx context.Context, tasks []string) (map[string]Event, error) {
	return nil, nil
}
func (s *staleStore) TransitionTask(ctx context.Context, id string, from []string, to string) error {
	s.calls = append(s.calls, "TransitionTask")
	s.status = to
	return nil
}
func (s *staleStore) InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error) {
	s.calls = append(s.calls, "InsertIntakeEvent")
	if s.failInsert != nil {
		return 0, s.failInsert
	}
	return 1, nil
}
func (s *staleStore) InsertAlert(ctx context.Context, a Alert) (int64, error) {
	s.calls = append(s.calls, "InsertAlert")
	return 1, nil
}

// A stale escalation that cannot write its intake row fails before the
// wait: no transition, no alert, the sweep retries. Without an Intake
// (no Forgejo, no timeline) the row is still the gate.
func TestEscalateStaleFailsBeforeTheWaitWhenItsRowCannotBeWritten(t *testing.T) {
	s := &staleStore{status: "running", failInsert: errors.New("the store is gone")}
	err := escalateStale(context.Background(), s, nil, Task{ID: "t1", Status: "running", Repository: "sf/repo"}, time.Hour)
	if err == nil {
		t.Fatal("escalateStale with a failing intake insert = nil error, want the insert's error")
	}
	if s.status != "running" {
		t.Fatalf("task status = %q, want running: a card waits only behind its row", s.status)
	}
	if want := []string{"InsertIntakeEvent"}; !slices.Equal(s.calls, want) {
		t.Fatalf("calls = %q, want %q: the transition and the alert never ran", s.calls, want)
	}
}

// The heal finishes a close factoryd recorded and did not confirm: the
// recorded comment posted where the issue lacks it, the issue closed, the
// record confirmed. It leaves a confirmed record, a young one and a task's
// issue a person closed alone, and settles a close Forgejo refuses.
func TestHealFactoryCloses(t *testing.T) {
	ctx := context.Background()
	const body = "factoryd: closing this issue."
	type want struct {
		calls     []string
		comments  []string
		state     string
		confirmed bool
	}
	cases := []struct {
		name      string
		age       time.Duration
		confirmed bool
		state     string
		comments  []string
		failCall  string
		failState int
		noTask    bool
		repo      string // the task's repository, linkshort/linkshort when empty
		want      want
		wantErr   bool
	}{
		{name: "a confirmed close of a reopened issue", age: 3 * time.Minute, confirmed: true, state: "open",
			want: want{state: "open", confirmed: true}},
		{name: "the comment exists", age: 3 * time.Minute, state: "open", comments: []string{body},
			want: want{calls: []string{"GET issues/5", "GET issues/5/comments", "PATCH issues/5"}, comments: []string{body}, state: "closed", confirmed: true}},
		{name: "the comment is missing", age: 3 * time.Minute, state: "open",
			want: want{calls: []string{"GET issues/5", "GET issues/5/comments", "POST issues/5/comments", "PATCH issues/5"}, comments: []string{body}, state: "closed", confirmed: true}},
		{name: "a person closed the issue", age: 3 * time.Minute, state: "closed",
			want: want{calls: []string{"GET issues/5"}, state: "closed", confirmed: true}},
		{name: "a young record", age: 30 * time.Second, state: "open",
			want: want{state: "open"}},
		{name: "a refused close", age: 3 * time.Minute, state: "open", comments: []string{body}, failCall: "PATCH issues/5", failState: http.StatusUnprocessableEntity,
			want: want{calls: []string{"GET issues/5", "GET issues/5/comments", "PATCH issues/5"}, comments: []string{body}, state: "open", confirmed: true}},
		{name: "a close that fails", age: 3 * time.Minute, state: "open", comments: []string{body}, failCall: "PATCH issues/5", wantErr: true,
			want: want{calls: []string{"GET issues/5", "GET issues/5/comments", "PATCH issues/5"}, comments: []string{body}, state: "open"}},
		{name: "a repository that does not split", age: 3 * time.Minute, state: "open", repo: "not-a-repo",
			want: want{state: "open", confirmed: true}},
		{name: "a deleted issue", age: 3 * time.Minute, state: "open", failCall: "GET issues/5", failState: http.StatusNotFound,
			want: want{calls: []string{"GET issues/5"}, state: "open", confirmed: true}},
		{name: "an issue read that fails", age: 3 * time.Minute, state: "open", failCall: "GET issues/5", wantErr: true,
			want: want{calls: []string{"GET issues/5"}, state: "open"}},
		{name: "a task that is gone", age: 3 * time.Minute, state: "open", noTask: true,
			want: want{state: "open", confirmed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			if !tc.noTask {
				store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Repository: cmp.Or(tc.repo, "linkshort/linkshort"), IssueNumber: 5, Track: "direct"})
			}
			store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-a", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 5, Body: body, CreatedAt: time.Now().Add(-tc.age)})
			if tc.confirmed {
				store.ConfirmFactoryClose(ctx, "t-a", 5)
			}
			fj := &planForgejo{issues: map[int64]*planIssue{5: {state: tc.state}}, failCall: tc.failCall, failStatus: tc.failState}
			for _, c := range tc.comments {
				fj.comments = append(fj.comments, &stubComment{id: 1, issue: 5, body: c, author: "factory-agent"})
			}
			in := &Intake{Store: store, Forgejo: &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
			err := in.HealFactoryCloses(ctx)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "#5") {
				t.Fatalf("error %q does not name issue #5", err)
			}
			var comments []string
			for _, c := range fj.comments {
				comments = append(comments, c.body)
			}
			got := want{fj.calls, comments, fj.issues[5].state, store.confirmedCloses[[2]any{"t-a", int64(5)}]}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// HealPass finishes an unconfirmed factory close.
func TestHealPassFinishesAFactoryClose(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	if err := store.CreateTask(ctx, Task{ID: "t-a", Kind: "agent", Status: "done", Repository: "linkshort/linkshort", IssueNumber: 5, Track: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: "t-a", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 5, CreatedAt: time.Now().Add(-3 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	fj := &planForgejo{issues: map[int64]*planIssue{5: {state: "open"}}}
	in := &Intake{Store: store, Forgejo: &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}}
	HealPass(ctx, store, &fakeWorkflows{}, in, nil, noneCompleted)
	if fj.issues[5].state != "closed" || !store.confirmedCloses[[2]any{"t-a", int64(5)}] {
		t.Fatalf("#5 is %q, confirmed %v; want closed and confirmed", fj.issues[5].state, store.confirmedCloses)
	}
}
