package factoryd

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	historypb "go.temporal.io/api/history/v1"
	client "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
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

// activitiesSeen records the type of every activity the replayed run
// schedules into got, and with inputs set its encoded input too, as
// payloadsText writes a recorded one.
type activitiesSeen struct {
	interceptor.WorkerInterceptorBase
	got, inputs *[]string
}

func (a activitiesSeen) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &activitiesInbound{seen: a}
	i.Next = next
	return i
}

type activitiesInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	seen activitiesSeen
}

func (i *activitiesInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &activitiesOutbound{seen: i.seen}
	o.Next = out
	return i.Next.Init(o)
}

type activitiesOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	seen activitiesSeen
}

func (o *activitiesOutbound) ExecuteActivity(ctx workflow.Context, activityType string, args ...any) workflow.Future {
	*o.seen.got = append(*o.seen.got, activityType)
	if o.seen.inputs != nil {
		p, err := converter.GetDefaultDataConverter().ToPayloads(args...)
		if err != nil {
			panic(err)
		}
		*o.seen.inputs = append(*o.seen.inputs, payloadsText(p))
	}
	return o.Next.ExecuteActivity(ctx, activityType, args...)
}

// payloadsText is the data of p's payloads, one per line.
func payloadsText(p *commonpb.Payloads) string {
	var lines []string
	for _, one := range p.GetPayloads() {
		lines = append(lines, string(one.GetData()))
	}
	return strings.Join(lines, "\n")
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

// [prechange_run_replays] A run whose session's check-in handed the work
// to a human, recorded on a dev server before the checkin-decision change,
// recorded the moment's own decision name. It carries no marker, so
// GetVersion answers DefaultVersion there and the replay reads the step's
// decision through the change's one place, folding the old name onto the
// rungs' hand_up, and schedules the same activities the recording holds.
func TestWorkflowReplaysACheckinDecisionRecordedBeforeTheRungsNames(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-checkin-priors-repeats.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	recordedDecision := ""
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-decision"` {
			t.Fatal("the recording holds the checkin-decision marker: it is no run from before")
		}
		a := e.GetActivityTaskScheduledEventAttributes()
		if a == nil {
			continue
		}
		recorded = append(recorded, a.GetActivityType().GetName())
		if a.GetActivityType().GetName() == ActivityRecordStep {
			payloads := a.GetInput().GetPayloads()
			if len(payloads) == 3 && string(payloads[1].GetData()) == `"checkin"` &&
				strings.Contains(string(payloads[2].GetData()), `"decision":"human"`) {
				recordedDecision = "human"
			}
		}
	}
	if recordedDecision == "" {
		t.Fatal("the recording holds no check-in step under the moments' own decision name")
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	if !slices.Equal(got, recorded) {
		t.Fatalf("the replay scheduled %q, the recording %q", got, recorded)
	}
}

// [prechange_history_replays] A history recorded on a dev server before
// the checkin-decision change, whose check-in answered with the moment's
// own decision name, replays unchanged: the replay schedules the
// activities the recording holds in its order, and every step records
// the input the recording holds but the check-in's, whose decision the
// run reads through the change's one place — the old name folded onto
// the rungs' hand_up, its reason, reason kind and round as recorded.
func TestWorkflowReplaysAHistoryRecordedBeforeTheCheckinDecisionUnchanged(t *testing.T) {
	hist := loadHistory(t, filepath.Join("testdata", "devserver-checkin-priors-repeats.json"))
	for _, e := range hist.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"checkin-decision"` {
			t.Fatal("the recording holds the checkin-decision marker: it is no run from before")
		}
	}
	calls, inputs := scheduledActivities(hist)
	steps := 0
	for i, a := range calls {
		if a == ActivityRecordStep && strings.Contains(inputs[i], `"decision":"human"`) {
			steps++
		}
	}
	if steps != 1 {
		t.Fatalf("the recording holds %d check-in steps under the moment's own decision name, want the one", steps)
	}
	var got, gotInputs []string
	replaySplice(t, hist.Events, &activitiesSeen{got: &got, inputs: &gotInputs})
	if !slices.Equal(got, calls) {
		t.Fatalf("the replay scheduled %q, the recording %q", got, calls)
	}
	const folded = "\"t1\"\n\"checkin\"\n" +
		`{"decision":"hand_up","reason":"confirmed 2 times: a.go: x is missing","reason_kind":"","round":2}`
	for i, a := range got {
		if a == ActivityRecordStep && strings.Contains(inputs[i], `"decision":"human"`) {
			if gotInputs[i] != folded {
				t.Fatalf("the replayed check-in step =\n%s\nwant the recorded step with its decision folded onto the rungs' hand_up:\n%s", gotInputs[i], folded)
			}
			continue
		}
		if gotInputs[i] != inputs[i] {
			t.Fatalf("activity %d (%s): the replay recorded\n%s\nthe recording\n%s", i+1, a, gotInputs[i], inputs[i])
		}
	}
}
