package factoryd

import (
	"reflect"
	"slices"
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
			status := "running"
			if tt.cardWaits {
				status = "waiting-human"
			}
			got := SettleOf(tt.m, tt.events, status, tt.apprWaits)
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
// one that names no case on any row but a monitor case's. Its next ask
// moves it on, as askAnswer reads it: a plan question's a later
// revision's, any other a newer ask of the card's own. A monitor's ask
// settles on a row naming its case alone and stays open while the case
// lives, until its card ends, except the one that asks the ended card's
// issue closed. Two asks written in one pass each find their own birth by
// their words.
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
	const waiting, running = "waiting-human", "running"
	newer := ev(36, 150, EventQuestionAsked, FactoryDecider, map[string]any{"question": "Which port?"})
	// a plan revision's two questions and its confirmation, asked in one
	// pass, and a later revision's question
	plan := func(id int64, sec int, question string, revision, index int) Event {
		return ev(id, sec, EventQuestionAsked, FactoryDecider, map[string]any{"question": question, "revision": float64(revision), "index": float64(index)})
	}
	confirm := plan(42, 400, "Confirm the plan?", 2, 3)
	confirm.Detail["kind"] = QuestionConfirmation
	revision := []Event{confirm, plan(41, 400, "Which cache?", 2, 2), plan(40, 400, "Which DB?", 2, 1)}
	later := plan(43, 500, "Which DB now?", 3, 1)
	planRow := func(question string) ChatMessage {
		return ChatMessage{ID: 41, At: at.Add(405 * time.Second), Kind: MessageEscalation, BirthKind: EventQuestionAsked, TaskID: "t-e",
			Content: "#13 needs you: " + question}
	}
	// the monitor's ask on an ended card whose issue is open
	issueAsk := ev(44, 600, EventQuestionAsked, FactoryDecider, map[string]any{"question": "The monitor flagged this card: what should happen? Answer case 14 in the project chat.",
		"case_id": float64(14), "case_kind": string(CaseMonitorFlag), "flag": "issue_open_after_end"})
	issueRow := ChatMessage{At: issueAsk.At, Kind: MessageEscalation, BirthKind: EventQuestionAsked}
	issueClosed := ev(45, 700, EventQuestionWithdrawn, FactoryDecider, map[string]any{"case_id": float64(14), "reason": "the issue is closed"})
	// the console's answer to the revision's first question
	planAnswer := ev(46, 450, EventPlanQuestionAnswered, "sebastian", map[string]any{"revision": float64(2), "index": float64(1), "answer": "postgres"})
	open := PokeSettle{Open: true}
	by := func(e Event, word string) PokeSettle { return PokeSettle{Word: word, By: e.Actor, At: e.At, Event: e} }
	for _, tt := range []struct {
		name   string
		m      ChatMessage
		events []Event
		status string
		want   PokeSettle
	}{
		{"the monitor's withdrawal settles its ask", mon, []Event{withdrawn, monAsk, ownAsk}, waiting, by(withdrawn, "withdrawn")},
		{"and leaves the card's own open", own, []Event{withdrawn, monAsk, ownAsk}, waiting, open},
		{"an answer naming the card's own case settles its ask", own, []Event{answered, monAsk, ownAsk}, waiting, by(answered, "answered")},
		{"and leaves the monitor's open on a card that runs", mon, []Event{answered, monAsk, ownAsk}, running, open},
		{"an answer naming the monitor's case settles its ask", mon, []Event{monAnswer, answered, monAsk, ownAsk}, running, by(monAnswer, "answered")},
		{"an answer between the ask and its row settles it", mon, []Event{early, monAsk, ownAsk}, running, by(early, "answered")},
		{"an answer naming no case leaves the monitor's ask open", mon, []Event{webhook, closed, monAsk, ownAsk}, running, open},
		{"an own ask naming its case settles on an answer naming none", own, []Event{webhook, withdrawn, monAsk, ownAsk5}, waiting, by(webhook, "answered")},
		{"and not on one naming another case", own, []Event{another, monAnswer, monAsk, ownAsk5}, waiting, open},
		{"a row with no words finds its birth by its time", ChatMessage{At: monAsk.At, Kind: MessageEscalation, BirthKind: EventQuestionAsked},
			[]Event{withdrawn, monAsk, ownAsk}, running, by(withdrawn, "withdrawn")},
		{"a card with one ask and no case ids settles as before", own, []Event{webhook, ownAsk}, waiting, by(webhook, "answered")},
		{"and moves on with its card", own, []Event{ownAsk}, running, PokeSettle{Word: "moved on", At: own.At}},
		{"a newer ask of the card's own moves its row on", own, []Event{newer, monAsk, ownAsk5}, waiting, PokeSettle{Word: "moved on", At: newer.At}},
		{"an answer before the newer ask settles it first", own, []Event{newer, answered, monAsk, ownAsk}, waiting, by(answered, "answered")},
		{"a newer ask of the card's own leaves the monitor's open", mon, []Event{newer, monAsk, ownAsk}, waiting, open},
		{"a monitor's ask moves on when its card ended", mon, []Event{monAsk, ownAsk}, "done", PokeSettle{Word: "moved on", At: mon.At}},
		{"a plan's first question stays open past its revision's other asks", planRow("Which DB?"), revision, waiting, open},
		{"and its second", planRow("Which cache?"), revision, waiting, open},
		{"and its confirmation", planRow("Confirm the plan?"), revision, waiting, open},
		{"a later revision moves its first question on", planRow("Which DB?"), append([]Event{later}, revision...), waiting,
			PokeSettle{Word: "moved on", At: later.At}},
		{"and its confirmation", planRow("Confirm the plan?"), append([]Event{later}, revision...), waiting, PokeSettle{Word: "moved on", At: later.At}},
		{"the question's answer settles its row", planRow("Which DB?"), append([]Event{planAnswer}, revision...), waiting, by(planAnswer, "answered")},
		{"and not its sibling's", planRow("Which cache?"), append([]Event{planAnswer}, revision...), waiting, open},
		{"nor its confirmation's", planRow("Confirm the plan?"), append([]Event{planAnswer}, revision...), waiting, open},
		{"a plan answer leaves the card's own ask open", own, append(append(slices.Clone(revision), ownAsk), planAnswer), waiting, open},
		{"a plan question leaves the card's own ask open", own, append(slices.Clone(revision), ownAsk), waiting, open},
		{"the ask of an ended card's open issue stays open", issueRow, []Event{issueAsk}, "done", open},
		{"until a row names its case", issueRow, []Event{issueClosed, issueAsk}, "done", by(issueClosed, "withdrawn")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SettleOf(tt.m, tt.events, tt.status, false); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("SettleOf =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// A plan's ask on its failed card sits beside the plan at rest: the plan
// card is done, and the ask stays open until a row names its case.
func TestAPlansWaitAskStaysOpenUntilItsCaseEnds(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	const question = "A dependency failed: #5 (links-api), card t-a, ended failed with no replacement"
	birth := Event{ID: 1, Kind: EventQuestionAsked, TaskID: "t-plan", At: at, Actor: FactoryDecider,
		Detail: map[string]any{"question": question, "case_id": float64(7), "case_kind": string(CaseDependencyFailed)}}
	m := ChatMessage{Kind: MessageEscalation, BirthKind: EventQuestionAsked, TaskID: "t-plan", At: at.Add(time.Second), Content: "#1 needs you: " + question}
	if s := SettleOf(m, []Event{birth}, "done", false); !s.Open {
		t.Fatalf("the ask on the done plan = %+v, want it open", s)
	}
	withdrawn := Event{ID: 2, Kind: EventQuestionWithdrawn, TaskID: "t-plan", At: at.Add(time.Hour), Actor: FactoryDecider,
		Detail: map[string]any{"case_id": float64(7), "case_kind": string(CaseDependencyFailed), "reason": "the plan no longer waits on that card"}}
	if s := SettleOf(m, []Event{birth, withdrawn}, "done", false); s.Open || s.Word != "withdrawn" {
		t.Fatalf("the ask after its case ended = %+v, want it withdrawn", s)
	}
	// a direct card's own dependency ask holds no case of the plan's: the
	// card that moved on says so, and one that ended has its task_ended row
	direct := Event{ID: 1, Kind: EventQuestionAsked, TaskID: "t-c", At: at, Actor: FactoryDecider,
		Detail: map[string]any{"question": question, "case_id": float64(8), "case_kind": string(CaseDependencyFailed)}}
	dm := ChatMessage{Kind: MessageEscalation, BirthKind: EventQuestionAsked, TaskID: "t-c", At: at.Add(time.Second), Content: "#9 needs you: " + question}
	if s := SettleOf(dm, []Event{direct}, "running", false); s.Open || s.Word != "moved on" {
		t.Fatalf("the direct card's ask while it runs = %+v, want it moved on", s)
	}
	if s := SettleOf(dm, []Event{direct}, "waiting-deps", false); s.Open || s.Word != "moved on" {
		t.Fatalf("the direct card's ask on its wait = %+v, want it moved on", s)
	}
}
