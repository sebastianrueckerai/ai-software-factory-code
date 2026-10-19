package factoryd

import (
	"slices"
	"time"
)

// The settling rule of roadmap 292, in one place: the console derives its
// rows' state with it and the messenger sender drops or edits its outbox
// rows with it. Nothing is stored when a row settles.

// answerKinds are the events that settle an escalation: an answer on any
// venue, or factoryd's withdrawal of the question. The first one after
// the escalation wins.
var answerKinds = []string{EventQuestionAnswered, EventSessionGranted, EventMergeApproved, EventMergeDenied,
	EventPlanAccepted, EventPlanChangesRequested, EventPlanRejected, EventCardClosed, EventQuestionWithdrawn}

// PokeSettle is an ask row's derived state: open, or settled with the
// word, actor and time of the event that settled it (the
// notification-settling concept).
type PokeSettle struct {
	Open bool
	Word string
	By   string
	At   time.Time
	// Event is the settling event, zero when no event names the row and
	// the wait guard settled it.
	Event Event
}

// AskFamily is poke row m's family in the settle rule: question or
// approval for an ask, "" for an inform, which the seen marker settles.
// The row's birth kind tells it, as the Poker wrote it; an escalation
// row with none is a question whatever the join found, since only a
// question's ask reads as one there.
func AskFamily(m ChatMessage) string {
	switch {
	case m.Kind == MessageEscalation && m.BirthKind != EventApprovalRequested:
		return "question"
	case m.BirthKind == EventApprovalRequested:
		return "approval"
	case m.BirthKind == EventQuestionAsked:
		return "question"
	}
	return ""
}

// settles reports whether event kind settles family's rows.
func settles(kind, family string) bool {
	return slices.Contains(answerKinds, kind) || kind == EventTaskEnded ||
		family == "approval" && kind == EventApprovalRequested
}

// settleWords are a settling event's reason word (the concept's table).
var settleWords = map[string]string{
	EventQuestionAnswered: "answered", EventSessionGranted: "answered", EventCardClosed: "answered",
	EventQuestionWithdrawn: "withdrawn",
	EventMergeApproved:     "approved", EventPlanAccepted: "approved",
	EventMergeDenied: "rejected", EventPlanRejected: "rejected", EventPlanChangesRequested: "rejected",
	EventTaskEnded: "moved on", EventApprovalRequested: "superseded",
}

// SettleOf derives ask row m's state from its card's events and the wait
// fact of its family: a question's card in Needs you, an approval's
// pending request, a poke's card in Needs you. The first settling event
// at or after the row settles it with the event's word, actor and time;
// none, and the fact gone, settles it as moved on; the fact still there,
// the row is open. A row's repeats move its time, so a bumped row asks
// again after an answer that preceded it.
func SettleOf(m ChatMessage, events []Event, cardWaits, approvalWaits bool) PokeSettle {
	family := AskFamily(m)
	var first, last Event
	for _, e := range events {
		if e.At.Before(m.At) {
			continue
		}
		if last.ID == 0 || e.ID > last.ID {
			last = e
		}
		if !settles(e.Kind, family) {
			continue
		}
		if first.ID == 0 || e.At.Before(first.At) || e.At.Equal(first.At) && e.ID < first.ID {
			first = e
		}
	}
	if first.ID != 0 {
		return PokeSettle{Word: settleWords[first.Kind], By: first.Actor, At: first.At, Event: first}
	}
	if cardWaits && (family == "question" || family == "" && m.Kind == MessagePoke) || approvalWaits && family == "approval" {
		return PokeSettle{Open: true}
	}
	at := m.At
	if last.ID != 0 {
		at = last.At
	}
	return PokeSettle{Word: "moved on", At: at}
}

// ReasonLine is a settled ask's line, the decider line's pattern: word,
// actor, time (the ruling of 2026-10-17).
func ReasonLine(s PokeSettle) string {
	if s.By != "" {
		return s.Word + " by " + s.By + " · " + s.At.UTC().Format("15:04")
	}
	return s.Word + " · " + s.At.UTC().Format("15:04")
}

// ApprovalWaits reports whether a pending approval request names id.
func ApprovalWaits(pending []ApprovalRequest, id string) bool {
	return slices.ContainsFunc(pending, func(q ApprovalRequest) bool { return q.TaskID == id })
}
