package factoryd

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	client "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"
)

// replaySplice replays the spliced history events against TaskWorkflow.
func replaySplice(t *testing.T, events []*historypb.HistoryEvent, interceptors ...interceptor.WorkerInterceptor) {
	t.Helper()
	for i, e := range events {
		if e.EventId != int64(i+1) {
			t.Fatalf("event %d has id %d", i+1, e.EventId)
		}
	}
	path := filepath.Join(t.TempDir(), "prechange-checkin-answer.json")
	out, err := protojson.Marshal(&historypb.History{Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	replayer, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{Interceptors: interceptors})
	if err != nil {
		t.Fatal(err)
	}
	replayer.RegisterWorkflowWithOptions(TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), path); err != nil {
		t.Fatalf("the pre-change history must replay: %v", err)
	}
}

// checksOptionsSeen records the start-to-close the workflow code gives each
// WaitChecks it schedules, which a replay recomputes whatever the history
// recorded.
type checksOptionsSeen struct {
	interceptor.WorkerInterceptorBase
	got *[]time.Duration
}

func (c checksOptionsSeen) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &checksOptionsInbound{got: c.got}
	i.Next = next
	return i
}

type checksOptionsInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	got *[]time.Duration
}

func (i *checksOptionsInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &checksOptionsOutbound{got: i.got}
	o.Next = out
	return i.Next.Init(o)
}

type checksOptionsOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	got *[]time.Duration
}

func (o *checksOptionsOutbound) ExecuteActivity(ctx workflow.Context, activityType string, args ...any) workflow.Future {
	if activityType == ActivityWaitChecks {
		*o.got = append(*o.got, workflow.GetActivityOptions(ctx).StartToCloseTimeout)
	}
	return o.Next.ExecuteActivity(ctx, activityType, args...)
}

type activitiesSeen struct {
	interceptor.WorkerInterceptorBase
	got *[]string
}

func (a activitiesSeen) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &activitiesInbound{got: a.got}
	i.Next = next
	return i
}

type activitiesInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	got *[]string
}

func (i *activitiesInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &activitiesOutbound{got: i.got}
	o.Next = out
	return i.Next.Init(o)
}

type activitiesOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	got *[]string
}

func (o *activitiesOutbound) ExecuteActivity(ctx workflow.Context, activityType string, args ...any) workflow.Future {
	*o.got = append(*o.got, activityType)
	return o.Next.ExecuteActivity(ctx, activityType, args...)
}

// [prechange_run_replays] A card at its last round whose review raised a
// finding, recorded on a dev server before the last-round-findings change,
// put the finding on the approval card as capped and asked for the merge. It
// carries no marker, so GetVersion answers DefaultVersion there and the
// replay requests the approval again, with no second session.
func TestWorkflowReplaysALastRoundCardRecordedBeforeLastRoundFindings(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-last-round-findings-approval.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"last-round-findings"` {
			t.Fatal("the recording holds the last-round-findings marker: it is no run from before")
		}
		if name := e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName(); name != "" {
			recorded = append(recorded, name)
		}
	}
	count := func(name string) int {
		n := 0
		for _, r := range recorded {
			if r == name {
				n++
			}
		}
		return n
	}
	if count(ActivityRequestApproval) != 1 || count(ActivityRunSession) != 1 || slices.Contains(recorded, ActivityOpenTrip) {
		t.Fatalf("the recording schedules %q, want one session, the approval and no trip", recorded)
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	if !slices.Equal(got, recorded) {
		t.Fatalf("the replay scheduled %q, the recording %q", got, recorded)
	}
}
