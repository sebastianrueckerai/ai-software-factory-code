package factoryd

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The settling rule of roadmap 292, in one place: the console derives its
// rows' state with it and the messenger sender drops or edits its outbox
// rows with it. Nothing is stored when a row settles.

// answerKinds are the events that settle an escalation: an answer on any
// venue, or factoryd's withdrawal of the question. The first one after
// the escalation wins.
var answerKinds = []string{EventQuestionAnswered, EventSessionGranted, EventMergeApproved, EventMergeDenied,
	EventPlanChangesRequested, EventCardClosed, EventQuestionWithdrawn, EventPlanClosed}

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
	EventQuestionAnswered: "answered", EventSessionGranted: "answered", EventCardClosed: "answered", EventPlanClosed: "answered",
	EventPlanQuestionAnswered: "answered", EventQuestionWithdrawn: "withdrawn",
	EventMergeApproved: "approved",
	EventMergeDenied:   "rejected", EventPlanChangesRequested: "rejected",
	EventTaskEnded: "moved on", EventApprovalRequested: "superseded",
}

// SettleWord is event kind's word in a settled row's reason line: the
// table's, else "moved on".
func SettleWord(kind string) string { return cmp.Or(settleWords[kind], "moved on") }

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
// that, until the issue closes. A plan's ask beside it on its failed card
// (PlanWaitCase) and a closed plan's ask to close it (CasePlanHeld) wait
// on their case alone too, which the plan's close answers, and a plan's
// criteria case's face and a merge case's face as an approval row does,
// while the request pends. A row's repeats move its time, so a bumped row
// asks again after an answer that preceded it.
// PlanWaitAsk says ask row m on card t is a plan's wait on its failed card
// (PlanWaitCase): an open ask beside the ended plan, answered on the failed
// card.
func PlanWaitAsk(m ChatMessage, events []Event, t Task) bool {
	if !Final(t.Status) || t.Track != policy.TrackPlan {
		return false
	}
	return birthOf(m, events).Detail["case_kind"] == string(CaseDependencyFailed)
}

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
	if monitor && (!Final(status) || birth.Detail["flag"] == "issue_open_after_end") ||
		(birth.Detail["case_kind"] == string(CaseDependencyFailed) || birth.Detail["case_kind"] == string(CasePlanHeld)) && Final(status) ||
		(birth.Detail["case_kind"] == string(CasePlanCriteria) || birth.Detail["case_kind"] == string(CaseMergeApproval)) && approvalWaits {
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
// writes after its event, whose question or why the row's words hold when
// they hold one, so two asks of a card written in one pass each find
// their own.
func birthOf(m ChatMessage, events []Event) Event {
	var newest, said Event
	if m.BirthKind != EventQuestionAsked {
		return newest
	}
	for _, e := range events {
		if e.Kind != EventQuestionAsked || e.At.After(m.At) {
			continue
		}
		if e.ID > newest.ID {
			newest = e
		}
		if e.ID > said.ID && saysAsk(m.Content, e) {
			said = e
		}
	}
	if said.ID != 0 || m.Content == "" {
		return said
	}
	return newest
}

// saysAsk reports whether a row's content holds ask row e's words: its
// question, or its why as the message says it (askMessage).
func saysAsk(content string, e Event) bool {
	q, _ := e.Detail["question"].(string)
	why, _ := e.Detail["why"].(string)
	why = strings.TrimRight(capitalize(why), ".")
	return content == "" || q != "" && strings.Contains(content, strings.TrimSuffix(q, "…")) || why != "" && strings.Contains(content, why)
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

// ReasonLead is a settled ask's word and actor, the decider line's pattern
// before its time: "answered by sebastian", "withdrawn". A withdrawal the
// factory recorded with a reason reads "withdrawn: <reason>".
func ReasonLead(s PokeSettle) string {
	word, by := s.Word, DeciderWords(s.By)
	if s.By == FactoryDecider {
		switch {
		case s.Event.Kind == EventCardClosed || s.Event.Kind == EventPlanClosed:
			// the factory's close of a card or a plan withdraws its ask
			word, by = "withdrawn", ""
		case word != "answered":
			by = ""
		}
		if reason, _ := s.Event.Detail["reason"].(string); s.Event.Kind == EventQuestionWithdrawn && reason != "" {
			return word + ": " + reason
		}
	}
	if by != "" {
		return word + " by " + by
	}
	return word
}

// ReasonLine is a settled ask's line, the decider line's pattern: word,
// actor, time (the ruling of 2026-10-17).
func ReasonLine(s PokeSettle) string { return SettleLine(ReasonLead(s), s.At) }

// SettleLine is a settled row's reason with the time that settled it, as
// every reader but a screen reads it: in UTC with no zone named,
// "answered by sebastian · 15:04"; a reason with no time stands alone.
func SettleLine(reason string, at time.Time) string {
	if at.IsZero() {
		return reason
	}
	return reason + " · " + at.UTC().Format("15:04")
}

// deciderLead is DeciderWords at the start of a sentence: a decider's
// words capitalised, a human's login as it is.
func deciderLead(actor string) string {
	if who := DeciderWords(actor); who != actor {
		return capitalize(who)
	}
	return actor
}

// DeciderWords is who decided as a human reads it: the factory, the
// check-in, the factory agent or the planner by name, a human by their
// login, "" for none.
func DeciderWords(actor string) string {
	switch actor {
	case FactoryDecider:
		return "the factory"
	case CheckinDecider:
		return "the check-in"
	case ChatUser:
		return "the factory agent"
	case FinderPlanner:
		return "the planner"
	}
	return actor
}

// ApprovalWaits reports whether a pending approval request names id.
func ApprovalWaits(pending []ApprovalRequest, id string) bool {
	return slices.ContainsFunc(pending, func(q ApprovalRequest) bool { return q.TaskID == id })
}
