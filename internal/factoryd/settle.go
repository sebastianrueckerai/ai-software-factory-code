package factoryd

import (
	"slices"
	"strings"
	"time"
)

// The settling rule of roadmap 292, in one place: the console derives its
// rows' state with it and the messenger sender drops or edits its outbox
// rows with it. Nothing is stored when a row settles.

// answerKinds are the events that settle an escalation: an answer on any
// venue, or factoryd's withdrawal of the question. The first one after
// the escalation wins.
var answerKinds = []string{EventQuestionAnswered, EventSessionGranted, EventMergeApproved, EventMergeDenied,
	EventPlanChangesRequested, EventCardClosed, EventQuestionWithdrawn}

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
// A chat ask row is a question outright.
// The row's birth kind tells it, as the Poker wrote it; an escalation
// row with none is a question whatever the join found, since only a
// question's ask reads as one there.
func AskFamily(m ChatMessage) string {
	switch {
	case m.Kind == MessageAsk:
		return "question"
	case m.Kind == MessageEscalation && m.BirthKind != EventApprovalRequested:
		return "question"
	case m.BirthKind == EventApprovalRequested:
		return "approval"
	case m.BirthKind == EventQuestionAsked:
		return "question"
	}
	return ""
}

// settles reports whether event kind settles family's rows: a question's
// or a poke's any answer on any venue, an approval's its own family's
// events alone — a merge or plan decided, the task's end, or a newer
// request superseding it. An answer on another venue is no word on a
// request, so it settles no approval.
func settles(kind, family string) bool {
	if family == "approval" {
		return kind == EventApprovalRequested || kind == EventTaskEnded ||
			kind == EventMergeApproved || kind == EventMergeDenied || kind == EventPlanChangesRequested
	}
	return slices.Contains(answerKinds, kind) || kind == EventTaskEnded
}

// settleWords are a settling event's reason word (the concept's table).
var settleWords = map[string]string{
	EventQuestionAnswered: "answered", EventSessionGranted: "answered", EventCardClosed: "answered",
	EventPlanQuestionAnswered: "answered", EventQuestionWithdrawn: "withdrawn",
	EventMergeApproved: "approved",
	EventMergeDenied:   "rejected", EventPlanChangesRequested: "rejected",
	EventTaskEnded: "moved on", EventApprovalRequested: "superseded",
}

// SettleOf derives ask row m's state from its card's events and its
// card's status, with the wait fact of its family: a question's card in
// Needs you, an approval's pending request, a poke's card in Needs you.
// The first settling event at or after the row's birth, the ask that
// wrote it, settles it with the event's word, actor and time, when it
// ends the birth's case (caseEnds); the birth's next ask before any
// moves the row on (movesOn, as askAnswer stops); none, and the fact
// gone, settles it as moved on; the fact still there, the row is open. A
// monitor's ask waits on its case alone, which only a row naming it ends,
// until its card ends, and the ask of an ended card's open issue past
// that, until the issue closes. A row's repeats move its time, so a
// bumped row asks again after an answer that preceded it.
func SettleOf(m ChatMessage, events []Event, status string, approvalWaits bool) PokeSettle {
	family := AskFamily(m)
	cardWaits := status == "waiting-human"
	birth := birthOf(m, events)
	ends := caseEnds(birth, events)
	monitor := birth.Detail["case_kind"] == string(CaseMonitorFlag)
	from := m.At
	if birth.ID != 0 {
		from = birth.At
	}
	var first, last Event
	for _, e := range events {
		if e.At.Before(from) || e.ID == birth.ID {
			continue
		}
		if last.ID == 0 || e.ID > last.ID {
			last = e
		}
		next := birth.ID != 0 && e.ID > birth.ID && movesOn(birth, e)
		settling := settles(e.Kind, family)
		if e.Kind == EventPlanQuestionAnswered {
			settling = family == "question" && answersPlan(birth, e)
		}
		if !next && (!settling || !ends(e)) {
			continue
		}
		if first.ID == 0 || e.At.Before(first.At) || e.At.Equal(first.At) && e.ID < first.ID {
			first = e
		}
	}
	if first.Kind == EventQuestionAsked {
		return PokeSettle{Word: "moved on", At: first.At}
	}
	if first.ID != 0 {
		return PokeSettle{Word: settleWords[first.Kind], By: first.Actor, At: first.At, Event: first}
	}
	if monitor && (!Final(status) || birth.Detail["flag"] == "issue_open_after_end") {
		return PokeSettle{Open: true}
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

// birthOf is the question_asked event that wrote ask row m, zero when none
// is among events: the newest at or before the row, which the Poker
// writes after its event, whose question the row's words hold when it has
// words, so two asks of a card written in one pass each find their own.
func birthOf(m ChatMessage, events []Event) Event {
	var birth Event
	if m.BirthKind != EventQuestionAsked {
		return birth
	}
	for _, e := range events {
		q, _ := e.Detail["question"].(string)
		if e.Kind == EventQuestionAsked && !e.At.After(m.At) && e.ID > birth.ID &&
			(m.Content == "" || strings.Contains(m.Content, strings.TrimSuffix(q, "…"))) {
			birth = e
		}
	}
	return birth
}

// movesOn reports whether question_asked e, newer than ask, is ask's next
// ask, which moves it on: for a plan question a later revision's
// question, for a monitor's ask one on its case, for any other ask of the
// card's own a newer one that is neither a plan question nor a monitor's.
// askAnswer and SettleOf both stop there.
func movesOn(ask, e Event) bool {
	if e.Kind != EventQuestionAsked {
		return false
	}
	revision, _ := toInt(ask.Detail["revision"])
	rev, _ := toInt(e.Detail["revision"])
	own, _ := toInt(ask.Detail["case_id"])
	named, _ := toInt(e.Detail["case_id"])
	beside := func(x Event) bool { return x.Detail["case_kind"] == string(CaseMonitorFlag) }
	switch {
	case revision > 0:
		return rev > revision
	case beside(ask):
		return named == own
	}
	return rev == 0 && !beside(e)
}

// answersPlan reports whether answer e answers plan question ask: the
// same revision and the same index. One question's answer settles its
// row alone, not its siblings'.
func answersPlan(ask, e Event) bool {
	revision, _ := toInt(ask.Detail["revision"])
	index, _ := toInt(ask.Detail["index"])
	rev, _ := toInt(e.Detail["revision"])
	idx, _ := toInt(e.Detail["index"])
	return revision > 0 && rev == revision && idx == index
}

// caseEnds reports which of events end ask's case, as the events writer
// routes them: a row naming that case; a row naming none, unless ask is a
// monitor's, which is answered by its number alone; and for an ask that
// names no case, a row naming any case but a monitor's.
func caseEnds(ask Event, events []Event) func(Event) bool {
	monitor := map[int]bool{}
	for _, e := range append([]Event{ask}, events...) {
		if id, _ := toInt(e.Detail["case_id"]); e.Kind == EventQuestionAsked && e.Detail["case_kind"] == string(CaseMonitorFlag) {
			monitor[id] = true
		}
	}
	own, _ := toInt(ask.Detail["case_id"])
	return func(e Event) bool {
		named, _ := toInt(e.Detail["case_id"])
		return named == own || named == 0 && !monitor[own] || own == 0 && !monitor[named]
	}
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
