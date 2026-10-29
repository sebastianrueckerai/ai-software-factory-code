package factoryd

import (
	"reflect"
	"testing"
	"time"
)

// The settle rule's table (the notification-settling concept): the first
// settling event at or after an ask row settles it with its word, actor and
// time, the wait guard settles it as moved on, a newer request supersedes an
// approval but not a question, and a repeat's bumped clock sets a re-asked
// row open again.
func TestSettleOf(t *testing.T) {
	at := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	ask := ChatMessage{ID: 10, At: at, Kind: MessageEscalation, TaskID: "t-e", Content: "#13 needs you: Use a replica?"}
	approval := ChatMessage{ID: 10, At: at, Kind: MessageEscalation, TaskID: "t-m", BirthKind: EventApprovalRequested}
	ev := func(id int64, min int, kind, actor string) Event {
		return Event{ID: id, At: at.Add(time.Duration(min) * time.Minute), Kind: kind, Actor: actor}
	}
	open := PokeSettle{Open: true}
	for _, tt := range []struct {
		name                 string
		m                    ChatMessage
		events               []Event
		cardWaits, apprWaits bool
		want                 PokeSettle
	}{
		{"an open question", ask, nil, true, false, open},
		{"a newer request leaves the question open", ask, []Event{ev(12, 1, EventApprovalRequested, FactoryDecider)}, true, true, open},
		{"an answered question", ask, []Event{ev(12, 1, EventQuestionAnswered, "sebastian")}, true, false,
			PokeSettle{Word: "answered", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, EventQuestionAnswered, "sebastian")}},
		{"an answer before the row settles nothing", ask, []Event{ev(5, -1, EventQuestionAnswered, "bob")}, true, false, open},
		{"an answer on the row's own minute settles", ask, []Event{ev(7, 0, EventQuestionAnswered, "bob")}, false, false,
			PokeSettle{Word: "answered", By: "bob", At: at, Event: ev(7, 0, EventQuestionAnswered, "bob")}},
		{"a withdrawn question", ask, []Event{ev(12, 1, EventQuestionWithdrawn, FactoryDecider)}, false, false,
			PokeSettle{Word: "withdrawn", By: FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, EventQuestionWithdrawn, FactoryDecider)}},
		{"an answer on another venue", ask, []Event{ev(12, 1, EventCardClosed, "sebastian")}, false, false,
			PokeSettle{Word: "answered", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, EventCardClosed, "sebastian")}},
		{"a granted session", ask, []Event{ev(12, 1, EventSessionGranted, FactoryDecider)}, false, false,
			PokeSettle{Word: "answered", By: FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, EventSessionGranted, FactoryDecider)}},
		{"a task ended", ask, []Event{ev(12, 1, EventTaskEnded, FactoryDecider)}, false, false,
			PokeSettle{Word: "moved on", By: FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, EventTaskEnded, FactoryDecider)}},
		{"the wait guard moves a row on", ask, []Event{ev(11, 1, EventCardRetried, "the agent")}, false, false,
			PokeSettle{Word: "moved on", At: at.Add(time.Minute)}},
		{"the wait guard with no event at all", ask, nil, false, false, PokeSettle{Word: "moved on", At: at}},
		{"the first settling event wins", ask, []Event{ev(14, 2, EventMergeApproved, "sebastian"), ev(12, 1, EventQuestionAnswered, "bob")}, false, false,
			PokeSettle{Word: "answered", By: "bob", At: at.Add(time.Minute), Event: ev(12, 1, EventQuestionAnswered, "bob")}},
		{"a tie reads the smaller id first", ask, []Event{ev(13, 1, EventQuestionAnswered, "bob"), ev(12, 1, EventMergeDenied, "sebastian")}, false, false,
			PokeSettle{Word: "rejected", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, EventMergeDenied, "sebastian")}},
		{"a bumped row asks again", ChatMessage{ID: 10, At: at.Add(3 * time.Minute), Kind: MessageEscalation, TaskID: "t-e", Repeats: 1},
			[]Event{ev(12, 1, EventQuestionAnswered, "bob")}, true, false, open},
		{"an open approval on its pending request", approval, nil, false, true, open},
		{"an approval whose request is gone moved on", approval, nil, false, false, PokeSettle{Word: "moved on", At: at}},
		{"a poke on a card that waits", ChatMessage{ID: 11, At: at, Kind: MessagePoke, TaskID: "t-w", BirthKind: EventRoutineRan}, nil, true, false, open},
		{"a poke on a card that moved on", ChatMessage{ID: 11, At: at, Kind: MessagePoke, TaskID: "t-w", BirthKind: EventRoutineRan}, nil, false, false, PokeSettle{Word: "moved on", At: at}},
		{"an approved merge", approval, []Event{ev(12, 1, EventMergeApproved, "sebastian")}, false, true,
			PokeSettle{Word: "approved", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, EventMergeApproved, "sebastian")}},
		{"a denied merge", approval, []Event{ev(12, 1, EventMergeDenied, "sebastian")}, false, true,
			PokeSettle{Word: "rejected", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, EventMergeDenied, "sebastian")}},
		{"plan changes requested", approval, []Event{ev(12, 1, EventPlanChangesRequested, "sebastian")}, false, true,
			PokeSettle{Word: "rejected", By: "sebastian", At: at.Add(time.Minute), Event: ev(12, 1, EventPlanChangesRequested, "sebastian")}},
		{"a superseding request", approval, []Event{ev(12, 1, EventApprovalRequested, FactoryDecider)}, false, true,
			PokeSettle{Word: "superseded", By: FactoryDecider, At: at.Add(time.Minute), Event: ev(12, 1, EventApprovalRequested, FactoryDecider)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := SettleOf(tt.m, tt.events, tt.cardWaits, tt.apprWaits)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("settleOf =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
	family := func(name string, m ChatMessage, want string) {
		t.Helper()
		if got := AskFamily(m); got != want {
			t.Fatalf("%s: AskFamily = %q, want %q", name, got, want)
		}
	}
	family("an escalation's family is a question whatever the join found", ask, "question")
	family("an approval row's family", approval, "approval")
	family("a poke-kind row takes its family from the event",
		ChatMessage{Kind: MessagePoke, BirthKind: EventQuestionAsked}, "question")
	family("an inform's family", ChatMessage{Kind: MessagePoke, BirthKind: EventTaskEnded}, "")
	family("a row with no birth kind is an inform", ChatMessage{Kind: MessagePoke}, "")
}
