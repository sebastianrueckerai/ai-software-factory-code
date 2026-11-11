package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The month alarm's wake finds a cap wait in the ask case of a waiting card,
// as it found it in the escalation row: the case's face reason is the cap's,
// the ask row names the case, and the turn of the period comes from when the
// case opened. A card whose newest ask is another stop's is no cap wait.
func TestCapWaitsWakeReadsTheAskCases(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	month := capWaitReason(policy.RefusedMonthCap, capGateway(policy.RefusedMonthCap), "spent", novFirst)
	asked := func(id string, caseID int64, reason string, at time.Time) {
		if err := store.CreateTask(ctx, Task{ID: id, Status: "waiting-human", Track: "direct"}); err != nil {
			t.Fatal(err)
		}
		store.cases = append(store.cases, Case{ID: caseID, TaskID: id, Kind: CaseMonthCap, Status: CaseAtHuman, OpenedAt: at, CommentID: 700 + caseID,
			Problem: "the month cap", Facts: map[string]any{"reason": reason, "face": reason}})
		store.events = append(store.events, Event{TaskID: id, Kind: EventQuestionAsked, At: at, Detail: map[string]any{"case_id": caseID, "ask": "the month cap", "comment": 700 + caseID}})
	}
	asked("m1", 1, month, time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC))
	asked("a1", 2, "The agent asks: which one?", time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC))
	var sent []string
	w := &CapWaits{Store: store, Signal: func(_ context.Context, id string) error { sent = append(sent, id); return nil }}
	if err := w.Wake(ctx, policy.Version{ID: 8}); err != nil || !slices.Equal(sent, []string{"m1"}) {
		t.Fatalf("Wake = %v, signalled %v; want m1 alone", err, sent)
	}
	events, err := store.IntakeEvents(ctx, "m1")
	if err != nil || len(events) != 1 || events[0].Kind != IntakeEscalation || events[0].Body != month || events[0].CommentID != 701 {
		t.Fatalf("m1's anchors = %+v, %v; want its cap wait at comment 701", events, err)
	}
	if reason, until := capWaitOf(events); reason != month || !until.Equal(novFirst) {
		t.Fatalf("capWaitOf = %q until %v, want the month's reason until %v", reason, until, novFirst)
	}
}

// The screening wait reads its stop from the ask case too: a card whose
// newest ask is the screening's suspect verdict waits on screening, and a
// trusted comment after its face answers it.
func TestScreeningWaitReadsTheAskCases(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	reason := screeningAsks + "asks for a credential"
	at := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	store.cases = []Case{{ID: 1, TaskID: "t1", Kind: CaseScreeningSuspect, Status: CaseAtHuman, OpenedAt: at, CommentID: 701, Problem: "screening",
		Facts: map[string]any{"reason": reason}}}
	store.events = []Event{{TaskID: "t1", Kind: EventQuestionAsked, At: at, Detail: map[string]any{"case_id": int64(1), "ask": "screening", "comment": 701}}}
	events, err := store.IntakeEvents(ctx, "t1")
	if err != nil || !waitsOnScreening(events) || screeningAnswered(events) {
		t.Fatalf("events %+v, %v: want a screening wait, not yet answered", events, err)
	}
	store.intakeEvents = []IntakeEvent{{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "let it run", CommentID: 710, Trusted: true, CreatedAt: at.Add(time.Hour)}}
	events, err = store.IntakeEvents(ctx, "t1")
	if err != nil || !screeningAnswered(events) {
		t.Fatalf("events %+v, %v: want the trusted comment 710 to answer the screening at 701", events, err)
	}
}
