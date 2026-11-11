package factoryd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	client "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Histories recorded before a change to a spent check-in's answer replay
// against the gated code: a history carries no marker of the change, so
// GetVersion answers DefaultVersion and the replay keeps the answer's old
// commands. Each splices a stop into the recorded audit-beside history at
// its third session, which had pushed, with its pull request open.
//
// Before the checkin-spend change the answer to the call cap's stop bought
// a session that carried the answer. Before the checkin-answer change the
// answer to a share stop verified the head the session started from and
// granted nothing: that history holds checkin-spend, in place of
// poster-cap, which this card never reads, and the answer's next command is
// the verification's OpenPullRequest, where the change records the grant
// first.
func TestWorkflowReplaysPreChangeCheckinAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		spend        bool // the history holds checkin-spend
	}{
		{"checkin-spend: the answer buys a session", checkinCallCapStop, false},
		{"checkin-answer: the answer verifies the start head", CheckinShareSpent, true},
	} {
		t.Run(tc.name, func(t *testing.T) { replayCheckinAnswer(t, tc.reason, tc.spend) })
	}
}

func replayCheckinAnswer(t *testing.T, reason string, spend bool, interceptors ...interceptor.WorkerInterceptor) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ev := base.Events
	if len(ev) != 368 ||
		ev[253].GetActivityTaskScheduledEventAttributes().GetActivityId() != "RunSession-3" ||
		ev[331].GetWorkflowExecutionSignaledEventAttributes().GetSignalName() != SignalApproved {
		t.Fatalf("recorded history drifted: %d events", len(ev))
	}

	dc := converter.GetDefaultDataConverter()
	encode := func(v ...any) *commonpb.Payloads {
		p, err := dc.ToPayloads(v...)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	copyEvent := func(e *historypb.HistoryEvent) *historypb.HistoryEvent {
		b, err := proto.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var c historypb.HistoryEvent
		if err := proto.Unmarshal(b, &c); err != nil {
			t.Fatal(err)
		}
		return &c
	}

	// templates, cloned before the splice mutates event 256's result
	stepTpl := []*historypb.HistoryEvent{copyEvent(ev[235]), copyEvent(ev[236]), copyEvent(ev[237])}
	ruTpl := []*historypb.HistoryEvent{copyEvent(ev[259]), copyEvent(ev[260]), copyEvent(ev[261])}
	fundTpl := []*historypb.HistoryEvent{copyEvent(ev[241]), copyEvent(ev[242]), copyEvent(ev[243])}
	sigStepTpl := []*historypb.HistoryEvent{copyEvent(ev[335]), copyEvent(ev[336]), copyEvent(ev[337])}
	setStateTpl := []*historypb.HistoryEvent{copyEvent(ev[247]), copyEvent(ev[248]), copyEvent(ev[249])}

	t0 := ev[255].GetEventTime().AsTime()
	mark := 0
	stamp := func() *timestamppb.Timestamp {
		mark++
		return timestamppb.New(t0.Add(time.Duration(mark) * time.Second))
	}
	act := func(tpl []*historypb.HistoryEvent, id int64, input, result *commonpb.Payloads) []*historypb.HistoryEvent {
		s, st, c := copyEvent(tpl[0]), copyEvent(tpl[1]), copyEvent(tpl[2])
		s.EventId, st.EventId, c.EventId = id, id+1, id+2
		s.EventTime, st.EventTime, c.EventTime = stamp(), stamp(), stamp()
		sa := s.GetActivityTaskScheduledEventAttributes()
		sa.ActivityId = strconv.FormatInt(id, 10)
		if input != nil {
			sa.Input = input
		}
		st.GetActivityTaskStartedEventAttributes().ScheduledEventId = id
		ca := c.GetActivityTaskCompletedEventAttributes()
		ca.ScheduledEventId, ca.StartedEventId = id, id+1
		if result != nil {
			ca.Result = result
		}
		return []*historypb.HistoryEvent{s, st, c}
	}
	wft := func(id int64) []*historypb.HistoryEvent {
		s, st, c := copyEvent(ev[250]), copyEvent(ev[251]), copyEvent(ev[252])
		s.EventId, st.EventId, c.EventId = id, id+1, id+2
		s.EventTime, st.EventTime, c.EventTime = stamp(), stamp(), stamp()
		st.GetWorkflowTaskStartedEventAttributes().ScheduledEventId = id
		a := c.GetWorkflowTaskCompletedEventAttributes()
		a.ScheduledEventId, a.StartedEventId = id, id+1
		return []*historypb.HistoryEvent{s, st, c}
	}

	// the quiet window that ends an answer's burst (answerQuiet); its id is
	// the command sequence the SDK assigns, which in this history matches
	// the event id it lands on
	timer := func(id int64) []*historypb.HistoryEvent {
		tid := strconv.FormatInt(id, 10)
		start := &historypb.HistoryEvent{EventId: id, EventTime: stamp(), EventType: enumspb.EVENT_TYPE_TIMER_STARTED,
			Attributes: &historypb.HistoryEvent_TimerStartedEventAttributes{TimerStartedEventAttributes: &historypb.TimerStartedEventAttributes{
				TimerId: tid, StartToFireTimeout: durationpb.New(answerQuiet), WorkflowTaskCompletedEventId: id - 1}}}
		fired := &historypb.HistoryEvent{EventId: id + 1, EventTime: stamp(), EventType: enumspb.EVENT_TYPE_TIMER_FIRED,
			Attributes: &historypb.HistoryEvent_TimerFiredEventAttributes{TimerFiredEventAttributes: &historypb.TimerFiredEventAttributes{
				TimerId: tid, StartedEventId: id}}}
		return []*historypb.HistoryEvent{start, fired}
	}

	// session 3 stops at its spent check-in instead of pushing
	done := ev[255].GetActivityTaskCompletedEventAttributes()
	var res map[string]any
	if err := json.Unmarshal(done.GetResult().GetPayloads()[0].GetData(), &res); err != nil {
		t.Fatal(err)
	}
	res["Outcome"] = string(OutcomeCheckinHuman)
	res["CheckIn"] = map[string]any{"ReasonKind": string(CheckSpend), "Reason": reason}
	rb, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	done.Result.Payloads[0].Data = rb

	var taskID string
	if err := json.Unmarshal(ev[0].GetWorkflowExecutionStartedEventAttributes().GetInput().GetPayloads()[0].GetData(), &taskID); err != nil {
		t.Fatal(err)
	}

	// the stopped session's usage, its checkin record, the escalation, the
	// answer's signal with its recorded step and the burst's quiet timer;
	// then the call cap's answer buys a session, the grant's usage and the
	// bought attempt's funding read first, and the share stop's verifies
	// the head
	checkin := act(stepTpl, 266, nil, nil)
	for _, p := range checkin[0].GetActivityTaskScheduledEventAttributes().GetInput().GetPayloads() {
		var v any
		if json.Unmarshal(p.Data, &v) == nil {
			if s, ok := v.(string); ok && s == "stage_findings" {
				p.Data = []byte(`"checkin"`)
			}
		}
	}
	esc := act(stepTpl, 272, encode(taskID, checkinSpendAsk+reason, nil), encode(int64(601)))
	esc[0].GetActivityTaskScheduledEventAttributes().ActivityType.Name = "Escalate"
	sig := copyEvent(ev[331])
	sig.EventId, sig.EventTime = 278, stamp()
	sa := sig.GetWorkflowExecutionSignaledEventAttributes()
	sa.SignalName = SignalHumanComment
	sa.Input = encode(TaskSignal{Author: "sebastian", Body: "the cap is spent; review the head", Comment: 602})
	sa.Identity = "sebastian"

	// the history ends with an activity in flight, the bought RunSession or
	// the verification's OpenPullRequest: a partial replay, scheduled and
	// started but never completed, where the workflow blocks on its result
	// and the replay ends with no command left over
	opened, openedRun := copyEvent(ev[265]), copyEvent(ev[266])
	events := append([]*historypb.HistoryEvent{}, ev[:256]...)
	// the spliced history holds the drained blind proofs marker, which a
	// recording from before the change lacks: burst-accept's marker is
	// rewritten to it, its value read by no code
	for _, e := range events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"burst-accept"` {
			id[0].Data = []byte(`"blind-proofs"`)
		}
	}
	if spend {
		for _, e := range events {
			if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"poster-cap"` {
				id[0].Data = []byte(`"checkin-spend"`)
			}
		}
	}
	events = append(events, wft(257)...)
	events = append(events, act(ruTpl, 260, nil, nil)...)
	events = append(events, wft(263)...)
	events = append(events, checkin...)
	events = append(events, wft(269)...)
	events = append(events, esc...)
	events = append(events, wft(275)...)
	events = append(events, sig)
	events = append(events, wft(279)...)
	events = append(events, act(sigStepTpl, 282, nil, nil)...)
	events = append(events, wft(285)...)
	events = append(events, timer(288)...)
	events = append(events, wft(290)...)
	if spend {
		opened.EventId, opened.EventTime = 293, stamp()
		opened.GetActivityTaskScheduledEventAttributes().ActivityId = "293"
		openedRun.EventId, openedRun.EventTime = 294, stamp()
		openedRun.GetActivityTaskStartedEventAttributes().ScheduledEventId = 293
		events = append(events, opened, openedRun)
		replaySplice(t, events, interceptors...)
		return
	}
	events = append(events, act(ruTpl, 293, nil, nil)...)
	events = append(events, wft(296)...)
	events = append(events, act(fundTpl, 299, nil, nil)...)
	events = append(events, wft(302)...)
	events = append(events, act(setStateTpl, 305, nil, nil)...)
	events = append(events, wft(308)...)
	bought, began := copyEvent(ev[253]), copyEvent(ev[254])
	bought.EventId, bought.EventTime = 311, stamp()
	bought.GetActivityTaskScheduledEventAttributes().ActivityId = "RunSession-4"
	began.EventId, began.EventTime = 312, stamp()
	began.GetActivityTaskStartedEventAttributes().ScheduledEventId = 311
	events = append(events, bought, began)
	replaySplice(t, events, interceptors...)
}

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

// Before the checkin-spend change a restarted run took a spent check-in's
// answer as the buy of a session that carried it — the behavior the
// pre-change history of the call cap's stop replays; only from versionCheckinSpend on does
// the answer verify the open head (TestCheckinSpendResumeReVerifiesOpenPR).
func TestCheckinSpendResumeBeforeTheChangeBuysSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("checkin-spend", workflow.DefaultVersion, versionCheckinSpend).Return(workflow.DefaultVersion)
	h.beforeCases()
	h.state = RunState{Status: "waiting-human", Question: "check-in: spend_above_policy: check-in call cap reached",
		PR: PullRequest{Number: 9}, Head: "c2", Usage: TaskUsage{AgentTime: 5 * time.Minute, Attempts: 1}}
	var steps []string
	h.onVerify(&steps, []SessionResult{pushed("s2", "c3")})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "the cap is spent; review the head", Comment: 602})
	h.signalAfter(3*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, finals = %v", final, h.finals)
	}
	if want := []string{"session 2 after 1 cards", "pr c3 after 1 cards", "approval c3"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	if len(h.sessions) != 1 || len(h.usage) != 2 || h.usage[1].Attempts != 2 || len(h.recorded) == 0 || h.recorded[0].step != "signal" {
		t.Fatalf("sessions = %d, usage = %+v, recorded = %+v; want one bought session on the answer's signal step", len(h.sessions), h.usage, h.recorded)
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

// [prechange_run_replays] A run recorded before the checks-wait change
// carries no marker for it, so GetVersion answers DefaultVersion: the
// replay matches every command, and the code schedules each recorded
// WaitChecks with the options of before, 45m + 1m = 46m, not the 52m of a
// current run. The recording is a whole run of the audit-beside card, with
// burst-accept's marker rewritten to the drained blind proofs' one that a
// recording from before its deploy lacks.
func TestWorkflowReplaysARunRecordedBeforeTheChecksWait(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 {
			switch string(id[0].GetData()) {
			case `"checks-wait"`:
				t.Fatal("the recording holds the checks-wait marker: it is no run from before")
			case `"burst-accept"`:
				id[0].Data = []byte(`"blind-proofs"`)
			}
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a.GetActivityType().GetName() == ActivityWaitChecks {
			recorded++
		}
	}
	var got []time.Duration
	replaySplice(t, base.Events, &checksOptionsSeen{got: &got})
	if len(got) != recorded || recorded == 0 {
		t.Fatalf("the replay scheduled %d checks waits, the recording holds %d", len(got), recorded)
	}
	for _, d := range got {
		if d != 46*time.Minute {
			t.Fatalf("replayed WaitChecks start-to-close = %s, want 46m0s", d)
		}
	}
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

// [prechange_run_replays] A run recorded before the surge-resume change
// carries no marker for it, so GetVersion answers DefaultVersion: the
// replay matches every command, and each session start sets the state to
// running as recorded, with no AdmitResume before it, which the recording
// lacks.
func TestWorkflowReplaysARunRecordedBeforeTheSurgeResume(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "history-t-c35a843b9e57-audit-beside-merged.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 {
			switch string(id[0].GetData()) {
			case `"surge-resume"`:
				t.Fatal("the recording holds the surge-resume marker: it is no run from before")
			case `"burst-accept"`:
				id[0].Data = []byte(`"blind-proofs"`)
			}
		}
		if e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName() == ActivitySetState {
			recorded++
		}
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	states := 0
	for _, a := range got {
		switch a {
		case ActivityAdmitResume:
			t.Fatal("the replay of a run from before scheduled AdmitResume")
		case ActivitySetState:
			states++
		}
	}
	if states != recorded || recorded == 0 {
		t.Fatalf("the replay scheduled %d SetState, the recording holds %d", states, recorded)
	}
}

// [prechange_run_replays] A plan card's run recorded on a dev server before
// the surge-resume change (a question, its answers and three planner
// sessions, under stub activities) replays at today's code: it carries no
// marker, the plan loop's session starts add no command, and the recorded
// SetState commands match with no AdmitResume among them.
func TestWorkflowReplaysAPlanRunRecordedBeforeTheSurgeResume(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-plan-question-answer-sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recordedStates, recordedPlans := 0, 0
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"surge-resume"` {
			t.Fatal("the recording holds the surge-resume marker: it is no run from before")
		}
		switch e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName() {
		case ActivitySetState:
			recordedStates++
		case ActivityRunPlan:
			recordedPlans++
		}
	}
	if recordedPlans != 3 {
		t.Fatalf("the recording holds %d planner sessions, want 3", recordedPlans)
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	states := 0
	for _, a := range got {
		switch a {
		case ActivityAdmitResume:
			t.Fatal("the replay of a plan run from before scheduled AdmitResume")
		case ActivitySetState:
			states++
		}
	}
	if states != recordedStates || recordedStates == 0 {
		t.Fatalf("the replay scheduled %d SetState, the recording holds %d", states, recordedStates)
	}
}

// [prechange_run_replays] A small card's run recorded on a dev server before
// the sized-least change, under stub activities: its class gives 8 minutes,
// its size 4, and the least read the 8 minutes unsized, 5 minutes, so the
// run asked the agent-time cap before any session and the issue's close
// ended it. It carries no marker, so where the two leasts differ GetVersion
// answers DefaultVersion and the replay asks the cap again, with no
// RunSession.
func TestWorkflowReplaysASmallCardRecordedBeforeTheSizedLeast(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-small-card-agent-time-cap.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []string
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"sized-least"` {
			t.Fatal("the recording holds the sized-least marker: it is no run from before")
		}
		if name := e.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName(); name != "" {
			recorded = append(recorded, name)
		}
	}
	if slices.Contains(recorded, ActivityRunSession) || !slices.Contains(recorded, ActivityOpenTrip) {
		t.Fatalf("the recording schedules %q, want the cap's trip and no session", recorded)
	}
	var got []string
	replaySplice(t, base.Events, &activitiesSeen{got: &got})
	if !slices.Equal(got, recorded) {
		t.Fatalf("the replay scheduled %q, the recording %q", got, recorded)
	}
}

// [prechange_run_replays] The small card's run recorded before the
// trip-model change carries no marker for it: the replayed trip carries
// the input recorded, with no model, so the facts of a run from before
// weigh the card's last session's model.
func TestWorkflowReplaysATripRecordedBeforeTheTripModel(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "devserver-small-card-agent-time-cap.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.HistoryFromJSON(bytes.NewReader(data), client.HistoryJSONOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []TripInput
	for _, e := range base.Events {
		if id := e.GetMarkerRecordedEventAttributes().GetDetails()["change-id"].GetPayloads(); len(id) == 1 && string(id[0].GetData()) == `"trip-model"` {
			t.Fatal("the recording holds the trip-model marker: it is no run from before")
		}
		if a := e.GetActivityTaskScheduledEventAttributes(); a.GetActivityType().GetName() == ActivityOpenTrip {
			var in TripInput
			if err := converter.GetDefaultDataConverter().FromPayloads(a.GetInput(), &in); err != nil {
				t.Fatal(err)
			}
			recorded = append(recorded, in)
		}
	}
	var got []TripInput
	replaySplice(t, base.Events, &tripsSeen{got: &got})
	if len(recorded) != 1 || !reflect.DeepEqual(got, recorded) {
		t.Fatalf("the replay's trips %+v, the recording's %+v", got, recorded)
	}
}

type tripsSeen struct {
	interceptor.WorkerInterceptorBase
	got *[]TripInput
}

func (s tripsSeen) InterceptWorkflow(_ workflow.Context, next interceptor.WorkflowInboundInterceptor) interceptor.WorkflowInboundInterceptor {
	i := &tripsInbound{got: s.got}
	i.Next = next
	return i
}

type tripsInbound struct {
	interceptor.WorkflowInboundInterceptorBase
	got *[]TripInput
}

func (i *tripsInbound) Init(out interceptor.WorkflowOutboundInterceptor) error {
	o := &tripsOutbound{got: i.got}
	o.Next = out
	return i.Next.Init(o)
}

type tripsOutbound struct {
	interceptor.WorkflowOutboundInterceptorBase
	got *[]TripInput
}

func (o *tripsOutbound) ExecuteActivity(ctx workflow.Context, activityType string, args ...any) workflow.Future {
	if in, ok := args[0].(TripInput); ok && activityType == ActivityOpenTrip {
		*o.got = append(*o.got, in)
	}
	return o.Next.ExecuteActivity(ctx, activityType, args...)
}
