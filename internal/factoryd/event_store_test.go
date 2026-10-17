package factoryd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func TestPGStoreEventsRecordAndList(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	id1, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-1", IssueNumber: 5, Kind: EventCardFiled, Actor: "alice", Via: ViaButton, Detail: map[string]any{"lane": "ready"}})
	if err != nil {
		t.Fatalf("Record 1: %v", err)
	}
	if id1 == 0 {
		t.Fatal("Record 1 returned id 0")
	}
	time.Sleep(time.Millisecond)
	id2, err := s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-2", Kind: EventCardMoved, Actor: "bob", Via: ViaButton})
	if err != nil {
		t.Fatalf("Record 2: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("id2 %d <= id1 %d", id2, id1)
	}
	id3, err := s.Record(ctx, Event{Repository: "other/repo", Kind: EventProjectFiled, Actor: "carol", Via: ViaChat})
	if err != nil {
		t.Fatalf("Record 3: %v", err)
	}
	events, err := s.ProjectEvents(ctx, "sf/r", 0, 50)
	if err != nil {
		t.Fatalf("ProjectEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ProjectEvents(sf/r) = %d events, want 2", len(events))
	}
	if events[0].ID != id2 || events[1].ID != id1 {
		t.Fatalf("order: got ids %d, %d; want %d, %d (newest first)", events[0].ID, events[1].ID, id2, id1)
	}
	if events[0].Kind != EventCardMoved || events[0].Actor != "bob" || events[0].Via != ViaButton {
		t.Fatalf("event 0: %+v", events[0])
	}
	if events[1].IssueNumber != 5 || events[1].Detail["lane"] != "ready" {
		t.Fatalf("event 1: %+v", events[1])
	}
	after, err := s.ProjectEvents(ctx, "sf/r", id1, 50)
	if err != nil {
		t.Fatalf("ProjectEvents after: %v", err)
	}
	if len(after) != 1 || after[0].ID != id2 {
		t.Fatalf("after id1: got %d events, want 1 with id %d", len(after), id2)
	}
	limit1, err := s.ProjectEvents(ctx, "sf/r", 0, 1)
	if err != nil {
		t.Fatalf("ProjectEvents limit: %v", err)
	}
	if len(limit1) != 1 || limit1[0].ID != id2 {
		t.Fatalf("limit 1: got %d events, want 1 with id %d", len(limit1), id2)
	}
	other, err := s.ProjectEvents(ctx, "other/repo", 0, 50)
	if err != nil {
		t.Fatalf("ProjectEvents other: %v", err)
	}
	if len(other) != 1 || other[0].ID != id3 {
		t.Fatalf("other/repo: got %d events, want 1 with id %d", len(other), id3)
	}
}

func TestPGStoreEventsNewestTaskEvents(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-1", Kind: EventCardFiled, Actor: "alice", Via: ViaButton})
	time.Sleep(time.Millisecond)
	s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-1", Kind: EventCardMoved, Actor: "bob", Via: ViaButton})
	s.Record(ctx, Event{Repository: "sf/r", TaskID: "t-2", Kind: EventPlanStarted, Actor: "carol", Via: ViaButton})
	newest, err := s.NewestTaskEvents(ctx, []string{"t-1", "t-2", "t-missing"})
	if err != nil {
		t.Fatalf("NewestTaskEvents: %v", err)
	}
	if len(newest) != 2 {
		t.Fatalf("NewestTaskEvents = %d entries, want 2", len(newest))
	}
	if newest["t-1"].Kind != EventCardMoved || newest["t-1"].Actor != "bob" {
		t.Fatalf("t-1 newest: %+v", newest["t-1"])
	}
	if newest["t-2"].Kind != EventPlanStarted || newest["t-2"].Actor != "carol" {
		t.Fatalf("t-2 newest: %+v", newest["t-2"])
	}
	if _, ok := newest["t-missing"]; ok {
		t.Fatal("t-missing should have no entry")
	}
	empty, err := s.NewestTaskEvents(ctx, nil)
	if err != nil {
		t.Fatalf("NewestTaskEvents empty: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("NewestTaskEvents(nil) = %d, want 0", len(empty))
	}
}

// A project's filed pull request is its last project_filed event's: a
// later filing's replaces an earlier one's, and neither a merge event nor
// another project's filing counts. No filing reads none.
func TestPGStoreFiledPR(t *testing.T) {
	s, ctx := chatStore(t), context.Background()
	for _, e := range []Event{
		{Repository: "linkshort/linkshort", Kind: EventProjectFiled, Detail: map[string]any{"name": "linkshort", "pr": 12}},
		{Repository: "linkshort/linkshort", Kind: EventProjectFiled, Detail: map[string]any{"name": "linkshort", "pr": 13, "source": "acme/app"}},
		{Repository: "linkshort/linkshort", Kind: EventProjectMerged, Detail: map[string]any{"name": "linkshort", "pr": 12}},
		{Repository: "other/other", Kind: EventProjectFiled, Detail: map[string]any{"name": "other", "pr": 20}},
	} {
		e.Actor, e.Via = "sebastian", ViaButton
		if _, err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]int64{"linkshort": 13, "other": 20, "none": 0} {
		if pr, ok, err := s.FiledPR(ctx, name); err != nil || pr != want || ok != (want > 0) {
			t.Errorf("FiledPR(%s) = %d, %v, %v; want %d", name, pr, ok, err, want)
		}
	}
}

func TestPGStoreRecordEventLeavesRow(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events, tasks CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := s.CreateTask(ctx, Task{ID: "t-ev", Repository: "sf/r", IssueNumber: 7, Kind: "agent", Status: "running"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	objects := &fakeObjectStore{}
	tl := &Timeline{Objects: objects, Events: s, Store: s}
	tl.recordEvent(ctx, "t-ev", EventCardEnteredLane, map[string]any{"lane": "ready", "by": "factoryd"})
	events, err := s.ProjectEvents(ctx, "sf/r", 0, 50)
	if err != nil {
		t.Fatalf("ProjectEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	e := events[0]
	if e.Kind != EventCardEnteredLane || e.Actor != FactoryDecider || e.Via != ViaFactoryd || e.TaskID != "t-ev" || e.IssueNumber != 7 {
		t.Fatalf("event: %+v", e)
	}
}

func TestPGStoreEventRequiresRepositoryAndKind(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.Record(ctx, Event{Kind: "x", Actor: "a", Via: "button"}); err == nil {
		t.Fatal("Record without repository should fail")
	}
	if _, err := s.Record(ctx, Event{Repository: "sf/r", Actor: "a", Via: "button"}); err == nil {
		t.Fatal("Record without kind should fail")
	}
}

// The pulse's kinds are historical — nothing writes them since the pulse
// band's removal — but their rows live on, and a project's events list
// leaves them out.
func TestPGStoreLeavesHistoricalPulseEventsOut(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, kind := range []string{EventCardMoved, EventPulseComputing, EventPulse} {
		if _, err := s.Record(ctx, Event{Repository: "sf/r", Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ProjectEvents(ctx, "sf/r", 0, 10)
	if err != nil || len(events) != 1 || events[0].Kind != EventCardMoved {
		t.Fatalf("ProjectEvents = %+v, %v: want the card_moved only", events, err)
	}
}

// Record publishes the row it stored: the id it answers and the time the
// table gave it when the caller set none. A second row naming the same
// answer comment is no row: Record answers 0 and publishes nothing, also
// for the webhook's answer of the comment a grant named.
func TestPGStoreRecordPublishesTheStoredRow(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	bus := NewEventBus()
	s.SetBus(bus)
	ch := make(chan Event, 4)
	defer bus.Subscribe("sf/r", ch)()
	answer := Event{Repository: "sf/r", TaskID: "t-1", Kind: EventQuestionAnswered, Actor: "alice", Via: ViaButton,
		Detail: map[string]any{"comment": int64(7)}}
	id, err := s.Record(ctx, answer)
	if err != nil || id == 0 {
		t.Fatalf("Record = %d, %v", id, err)
	}
	stored, err := s.ProjectEvents(ctx, "sf/r", 0, 50)
	if err != nil || len(stored) != 1 {
		t.Fatalf("ProjectEvents = %d rows, %v; want 1", len(stored), err)
	}
	got := receive(t, ch)
	if got.ID != id || !got.At.Equal(stored[0].At) || got.At.IsZero() {
		t.Fatalf("published id %d at %v; want id %d at the stored %v", got.ID, got.At, id, stored[0].At)
	}
	answer.Via = ViaWebhook
	if id, err := s.Record(ctx, answer); id != 0 || err != nil {
		t.Fatalf("the same answer again = %d, %v; want 0 and no error", id, err)
	}
	select {
	case e := <-ch:
		t.Fatalf("the repeated answer published event %d", e.ID)
	default:
	}
	grant := Event{Repository: "sf/r", TaskID: "t-1", Kind: EventSessionGranted, Actor: "alice", Via: ViaChat,
		Detail: map[string]any{"comment": int64(8)}}
	if id, err := s.Record(ctx, grant); id == 0 || err != nil {
		t.Fatalf("Record(grant) = %d, %v", id, err)
	}
	receive(t, ch)
	answer.Detail, answer.Via = grant.Detail, ViaWebhook
	if id, err := s.Record(ctx, answer); id != 0 || err != nil {
		t.Fatalf("the webhook's answer of the grant's comment = %d, %v; want 0 and no error", id, err)
	}
}

// Every status or gate change of the store leaves one task_status row by
// factoryd, with the task's repository and issue and its status and gate
// after the change, and publishes it; an update that changes neither (the
// same gate again, a start of a running task) or whose condition matches
// no row (a transition from a status the task is not in, a gate on a
// final task) leaves none.
func TestPGStoreStatusChangesLeaveTaskStatusRows(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events, tasks CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	bus := NewEventBus()
	s.SetBus(bus)
	published := make(chan Event, 16)
	defer bus.Subscribe("sf/r", published)()
	for _, tk := range []Task{
		{ID: "t-s", Repository: "sf/r", IssueNumber: 7, Kind: "agent", Status: "backlog", BudgetEUR: 1},
		{ID: "t-h", Repository: "sf/r", IssueNumber: 8, Kind: "agent", Status: "queued", BudgetEUR: 1},
	} {
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	changed := func(ok bool, err error) error {
		if err == nil && !ok {
			return ErrNotFound
		}
		return err
	}
	for _, step := range []struct {
		name   string
		change func() error
		task   string
		issue  int64
		detail map[string]any // nil: no row
	}{
		{"EnterLane", func() error { return changed(s.EnterLane(ctx, "t-s", "ready", "coder", "abc")) },
			"t-s", 7, map[string]any{"status": "queued", "gate": GateIntake}},
		{"MarkStarted", func() error { return s.MarkStarted(ctx, "t-s") },
			"t-s", 7, map[string]any{"status": "running", "gate": GateIntake}},
		{"SetGate", func() error { return s.SetGate(ctx, "t-s", GateChecks, time.Now()) },
			"t-s", 7, map[string]any{"status": "running", "gate": GateChecks}},
		{"SetGate to the gate the task is in", func() error { return s.SetGate(ctx, "t-s", GateChecks, time.Now()) }, "t-s", 7, nil},
		{"MarkStarted on a running task", func() error { return s.MarkStarted(ctx, "t-s") }, "t-s", 7, nil},
		{"TransitionTask from a status the task is not in", func() error {
			if err := s.TransitionTask(ctx, "t-s", []string{"queued"}, "in-review"); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("= %v, want ErrNotFound", err)
			}
			return nil
		}, "t-s", 7, nil},
		{"TransitionTask", func() error { return s.TransitionTask(ctx, "t-s", []string{"running"}, "in-review") },
			"t-s", 7, map[string]any{"status": "in-review", "gate": GateChecks}},
		{"TransitionTask to a final status", func() error { return s.TransitionTask(ctx, "t-s", []string{"in-review"}, "done") },
			"t-s", 7, map[string]any{"status": "done", "gate": ""}},
		{"SetGate on a final task", func() error { return s.SetGate(ctx, "t-s", GateReview, time.Now()) }, "t-s", 7, nil},
		{"ToHumanLane", func() error { return changed(s.ToHumanLane(ctx, "t-h")) },
			"t-h", 8, map[string]any{"status": "waiting-human", "gate": ""}},
	} {
		before, err := s.ProjectEvents(ctx, "sf/r", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		if err := step.change(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		after, err := s.ProjectEvents(ctx, "sf/r", 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		rows := after[:len(after)-len(before)]
		if step.detail == nil {
			if len(rows) != 0 {
				t.Errorf("%s left %d rows: %+v", step.name, len(rows), rows)
			}
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("%s left %d rows, want 1: %+v", step.name, len(rows), rows)
		}
		e := rows[0]
		if e.Kind != EventTaskStatus || e.Actor != FactoryDecider || e.Via != ViaFactoryd || e.TaskID != step.task ||
			e.Repository != "sf/r" || e.IssueNumber != step.issue || !reflect.DeepEqual(e.Detail, step.detail) {
			t.Errorf("%s: row %+v, want task_status by factoryd of %s #%d with %v", step.name, e, step.task, step.issue, step.detail)
		}
		if got := receive(t, published); got.ID != e.ID {
			t.Errorf("%s: published event %d, want the row %d", step.name, got.ID, e.ID)
		}
	}
}

// receive is the next event on ch; the test fails after 5 s without one.
func receive(t *testing.T, ch chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event published within 5 s")
		return Event{}
	}
}
