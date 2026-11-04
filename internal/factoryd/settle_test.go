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
		{"a question answered leaves an approval open", approval, []Event{ev(12, 1, EventQuestionAnswered, "sebastian")}, false, true, open},
		{"a granted session leaves an approval open", approval, []Event{ev(12, 1, EventSessionGranted, FactoryDecider)}, false, true, open},
		{"a card closed on another venue leaves an approval open", approval, []Event{ev(12, 1, EventCardClosed, "sebastian")}, false, true, open},
		{"a withdrawn question leaves an approval open", approval, []Event{ev(12, 1, EventQuestionWithdrawn, FactoryDecider)}, false, true, open},
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

// An ask row settles on what ends its case, as the events writer routes
// it: its birth, the question_asked row the Poker wrote it from, names the
// case. The card's own ask settles on a row naming its case or none, and
// one that names no case on any row but a monitor case's. A monitor's ask
// settles on a row naming its case alone and stays open while the case
// lives, whatever the card's status. Two asks written in one pass each
// find their own birth by their words.
func TestSettleOfByCase(t *testing.T) {
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	ev := func(id int64, sec int, kind, actor string, detail map[string]any) Event {
		return Event{ID: id, At: at.Add(time.Duration(sec) * time.Second), Kind: kind, Actor: actor, TaskID: "t-e", Detail: detail}
	}
	const flagged = "The monitor flagged this card: what should happen? Answer case 12 in the project chat."
	ownAsk := ev(20, 0, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Use a replica?"})
	ownAsk5 := ev(20, 0, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Use a replica?", "case_id": float64(5)})
	monAsk := ev(21, 1, EventQuestionAsked, FactoryDecider, map[string]any{"question": flagged, "case_id": float64(12), "case_kind": string(CaseMonitorFlag)})
	row := func(question string) ChatMessage {
		return ChatMessage{ID: 40, At: at.Add(3 * time.Second), Kind: MessageEscalation, BirthKind: EventQuestionAsked, TaskID: "t-e",
			Content: "#13 needs you: " + question}
	}
	own, mon := row("Use a replica?"), row(flagged)
	early := ev(29, 2, EventQuestionAnswered, "sebastian", map[string]any{"case_id": float64(12), "action": ActNote})
	withdrawn := ev(30, 60, EventQuestionWithdrawn, FactoryDecider, map[string]any{"case_id": float64(12), "reason": "the wait ended"})
	answered := ev(31, 120, EventQuestionAnswered, "sebastian", map[string]any{"case_id": float64(5), "action": ActContinue})
	webhook := ev(32, 180, EventQuestionAnswered, "anna", map[string]any{"comment": float64(77)})
	monAnswer := ev(33, 240, EventQuestionAnswered, "sebastian", map[string]any{"case_id": float64(12), "action": ActNote})
	another := ev(34, 300, EventQuestionAnswered, "sebastian", map[string]any{"case_id": float64(6), "action": ActContinue})
	closed := ev(35, 360, EventCardClosed, "sebastian", nil)
	open := PokeSettle{Open: true}
	by := func(e Event, word string) PokeSettle { return PokeSettle{Word: word, By: e.Actor, At: e.At, Event: e} }
	for _, tt := range []struct {
		name      string
		m         ChatMessage
		events    []Event
		cardWaits bool
		want      PokeSettle
	}{
		{"the monitor's withdrawal settles its ask", mon, []Event{withdrawn, monAsk, ownAsk}, true, by(withdrawn, "withdrawn")},
		{"and leaves the card's own open", own, []Event{withdrawn, monAsk, ownAsk}, true, open},
		{"an answer naming the card's own case settles its ask", own, []Event{answered, monAsk, ownAsk}, true, by(answered, "answered")},
		{"and leaves the monitor's open on a card that runs", mon, []Event{answered, monAsk, ownAsk}, false, open},
		{"an answer naming the monitor's case settles its ask", mon, []Event{monAnswer, answered, monAsk, ownAsk}, false, by(monAnswer, "answered")},
		{"an answer between the ask and its row settles it", mon, []Event{early, monAsk, ownAsk}, false, by(early, "answered")},
		{"an answer naming no case leaves the monitor's ask open", mon, []Event{webhook, closed, monAsk, ownAsk}, false, open},
		{"an own ask naming its case settles on an answer naming none", own, []Event{webhook, withdrawn, monAsk, ownAsk5}, true, by(webhook, "answered")},
		{"and not on one naming another case", own, []Event{another, monAnswer, monAsk, ownAsk5}, true, open},
		{"a row with no words finds its birth by its time", ChatMessage{At: monAsk.At, Kind: MessageEscalation, BirthKind: EventQuestionAsked},
			[]Event{withdrawn, monAsk, ownAsk}, false, by(withdrawn, "withdrawn")},
		{"a card with one ask and no case ids settles as before", own, []Event{webhook, ownAsk}, true, by(webhook, "answered")},
		{"and moves on with its card", own, []Event{ownAsk}, false, PokeSettle{Word: "moved on", At: own.At}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SettleOf(tt.m, tt.events, tt.cardWaits, false); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SettleOf =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}
