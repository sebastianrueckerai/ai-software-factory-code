package factoryd

import (
	"context"
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
