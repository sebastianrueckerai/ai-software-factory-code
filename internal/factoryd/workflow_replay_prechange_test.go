package factoryd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	client "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// A history recorded before the checkin-spend change answered a spent
// check-in's escalation by buying a session that carried the answer. It
// carries no checkin-spend marker, so against the gated code GetVersion
// answers DefaultVersion and the replay must keep buying the session.
func TestWorkflowReplaysPreChangeCheckinSpendAnswerBuysSession(t *testing.T) {
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
	res["CheckIn"] = map[string]any{"ReasonKind": string(CheckSpend), "Reason": "check-in call cap reached"}
	rb, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	done.Result.Payloads[0].Data = rb

	var taskID string
	if err := json.Unmarshal(ev[0].GetWorkflowExecutionStartedEventAttributes().GetInput().GetPayloads()[0].GetData(), &taskID); err != nil {
		t.Fatal(err)
	}

	// the answer buys a session: the stopped session's usage, its checkin
	// record, the escalation, the answer's signal with its recorded step,
	// the burst's quiet timer, the grant's usage, then the bought
	// attempt's funding read
	checkin := act(stepTpl, 266, nil, nil)
	for _, p := range checkin[0].GetActivityTaskScheduledEventAttributes().GetInput().GetPayloads() {
		var v any
		if json.Unmarshal(p.Data, &v) == nil {
			if s, ok := v.(string); ok && s == "stage_findings" {
				p.Data = []byte(`"checkin"`)
			}
		}
	}
	esc := act(stepTpl, 272, encode(taskID, "check-in: spend_above_policy: check-in call cap reached", nil), encode(int64(601)))
	esc[0].GetActivityTaskScheduledEventAttributes().ActivityType.Name = "Escalate"
	sig := copyEvent(ev[331])
	sig.EventId, sig.EventTime = 278, stamp()
	sa := sig.GetWorkflowExecutionSignaledEventAttributes()
	sa.SignalName = SignalHumanComment
	sa.Input = encode(TaskSignal{Author: "sebastian", Body: "the cap is spent; review the head", Comment: 602})
	sa.Identity = "sebastian"

	// the history ends with the funding read of the answer's bought
	// attempt: a partial replay, everything up to and including the grant
	events := append([]*historypb.HistoryEvent{}, ev[:256]...)
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
	events = append(events, act(ruTpl, 293, nil, nil)...)
	events = append(events, wft(296)...)
	events = append(events, act(fundTpl, 299, nil, nil)...)
	events = append(events, wft(302)...)
	for i, e := range events {
		if e.EventId != int64(i+1) {
			t.Fatalf("event %d has id %d", i+1, e.EventId)
		}
	}

	path := filepath.Join(t.TempDir(), "prechange-checkin-spend-answer-buys-session.json")
	out, err := protojson.Marshal(&historypb.History{Events: events})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(TaskWorkflow{Config: func() WorkflowConfig { return testWorkflowConfig }}.Run, workflow.RegisterOptions{Name: "TaskWorkflow"})
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), path); err != nil {
		t.Fatalf("pre-change answer-buys-session history must replay: %v", err)
	}
}

// Before the checkin-spend change a restarted run took a spent check-in's
// answer as the buy of a session that carried it — the behavior the
// pre-change history above replays; only from versionCheckinSpend on does
// the answer verify the open head (TestCheckinSpendResumeReVerifiesOpenPR).
func TestCheckinSpendResumeBeforeTheChangeBuysSession(t *testing.T) {
	h := newWorkflowHarness(t)
	h.env.OnGetVersion("checkin-spend", workflow.DefaultVersion, versionCheckinSpend).Return(workflow.DefaultVersion)
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
