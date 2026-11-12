package factoryd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The month alarm's wake finds a cap wait in the ask case of a waiting card,
// as it found it in the escalation row it replaced: the case's face reason is the cap's,
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
		store.face(id, caseID, CaseMonthCap, 700+caseID, reason, at)
	}
	asked("m1", 1, month, time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC))
	asked("a1", 2, "The agent asks: which one?", time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC))
	var sent []string
	w := &CapWaits{Store: store, Signal: func(_ context.Context, id string) error { sent = append(sent, id); return nil }}
	if err := w.Wake(ctx, policy.Version{ID: 8}); err != nil || !slices.Equal(sent, []string{"m1"}) {
		t.Fatalf("Wake = %v, signalled %v; want m1 alone", err, sent)
	}
	asks, err := store.AskAnchors(ctx, "m1")
	if err != nil || len(asks) != 1 || asks[0].Body != month || asks[0].Comment != 701 {
		t.Fatalf("m1's anchors = %+v, %v; want its cap wait at comment 701", asks, err)
	}
	if reason, until := capWaitOf(asks); reason != month || !until.Equal(novFirst) {
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
	store.face("t1", 1, CaseScreeningSuspect, 701, reason, at)
	asks, err := store.AskAnchors(ctx, "t1")
	if err != nil || !waitsOnScreening(asks) || screeningAnswered(nil, asks) {
		t.Fatalf("asks %+v, %v: want a screening wait, not yet answered", asks, err)
	}
	store.intakeEvents = []IntakeEvent{{TaskID: "t1", Kind: "issue_comment", Author: "sebastian", Body: "let it run", CommentID: 710, Trusted: true, CreatedAt: at.Add(time.Hour)}}
	events, err := store.IntakeEvents(ctx, "t1")
	if err != nil || !screeningAnswered(events, asks) {
		t.Fatalf("events %+v, %v: want the trusted comment 710 to answer the screening at 701", events, err)
	}
}
