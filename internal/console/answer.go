package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// chatAnswer answers POST /projects/{name}/chat/answer: a choice of an
// escalation row of the member's thread, which decides its card as the
// board's buttons do, with no model turn (roadmap 239, C14's answer row):
// the member's row with the choice and the factory's row with what came
// of it land in the thread behind a turn in flight there (roadmap 263),
// then 204. A card that waits no more says so: the first answer wins.
func (c *Console) chatAnswer(w http.ResponseWriter, r *http.Request) {
	t, ok := c.chatTurn(w, r)
	if !ok {
		return
	}
	if _, err := t.thread(r, true); err != nil {
		c.fail(w, r, err)
		return
	}
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PostFormValue("message"), 10, 64)
	choice := r.PostFormValue("choice")
	rows, err := c.cfg.Chat.ChatMessages(ctx, t.p.Repository, t.a.s.user, t.conv, chatHistory)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		http.Error(w, "cannot read the conversation", http.StatusInternalServerError)
		return
	}
	i := slices.IndexFunc(rows, func(m factoryd.ChatMessage) bool {
		return m.ID == id && m.Kind == factoryd.MessageEscalation && m.TaskID != ""
	})
	switch {
	case i < 0:
		c.fail(w, r, refuse(http.StatusNotFound, "404 page not found"))
		return
	case !slices.Contains(rows[i].Choices, choice):
		c.fail(w, r, refuse(http.StatusBadRequest, "the choice is none of the escalation's"))
		return
	}
	a := button(r)
	a.repository = t.p.Repository
	note, err := c.resolveChoice(ctx, a, rows[i].TaskID, choice, rows[i])
	if err != nil {
		note = "The answer did not land: " + t.sentence(err)
	}
	queued, land := c.Behind(t.p.Repository, t.a.s.user, t.conv)
	var ids []int64
	defer func() { land(ids...) }()
	for _, m := range []factoryd.ChatMessage{{Role: "user", Content: choice}, {Role: "assistant", Kind: factoryd.MessagePoke, Content: note, TaskID: rows[i].TaskID}} {
		m.Conversation, m.Queued = t.conv, queued
		id, err := c.cfg.Chat.AddChatMessage(ctx, t.p.Repository, t.a.s.user, m)
		if err != nil {
			log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
			http.Error(w, "cannot store the answer", http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// answerKinds are the events that settle an escalation: an answer on any
// venue, or factoryd's withdrawal of the question. The first one after
// the escalation wins.
var answerKinds = []string{factoryd.EventQuestionAnswered, factoryd.EventSessionGranted, factoryd.EventMergeApproved, factoryd.EventMergeDenied,
	factoryd.EventPlanAccepted, factoryd.EventPlanChangesRequested, factoryd.EventPlanRejected, factoryd.EventCardClosed, factoryd.EventQuestionWithdrawn}

// pokeSettle is an ask row's derived state: open, or settled with the
// word, actor and time of the event that settled it (the
// notification-settling concept). Nothing is stored when a row settles.
type pokeSettle struct {
	Open bool
	Word string
	By   string
	At   time.Time
	// Event is the settling event, zero when no event names the row and
	// the wait guard settled it.
	Event factoryd.Event
}

// askFamily is poke row m's family in the settle rule: question or
// approval for an ask, "" for an inform, which the seen marker settles.
// The row's birth kind tells it, as the Poker wrote it; an escalation
// row with none is a question whatever the join found, since only a
// question's ask reads as one there.
func askFamily(m factoryd.ChatMessage) string {
	switch {
	case m.Kind == factoryd.MessageEscalation && m.BirthKind != factoryd.EventApprovalRequested:
		return "question"
	case m.BirthKind == factoryd.EventApprovalRequested:
		return "approval"
	case m.BirthKind == factoryd.EventQuestionAsked:
		return "question"
	}
	return ""
}

// settles reports whether event kind settles family's rows.
func settles(kind, family string) bool {
	return slices.Contains(answerKinds, kind) || kind == factoryd.EventTaskEnded ||
		family == "approval" && kind == factoryd.EventApprovalRequested
}

// settleWords are a settling event's reason word (the concept's table).
var settleWords = map[string]string{
	factoryd.EventQuestionAnswered: "answered", factoryd.EventSessionGranted: "answered", factoryd.EventCardClosed: "answered",
	factoryd.EventQuestionWithdrawn: "withdrawn",
	factoryd.EventMergeApproved:     "approved", factoryd.EventPlanAccepted: "approved",
	factoryd.EventMergeDenied: "rejected", factoryd.EventPlanRejected: "rejected", factoryd.EventPlanChangesRequested: "rejected",
	factoryd.EventTaskEnded: "moved on", factoryd.EventApprovalRequested: "superseded",
}

// settleOf derives ask row m's state from its card's events and the
// wait fact of its family: a question's card in Needs you, an approval's
// pending request. The first settling event at or after the row settles
// it with the event's word, actor and time; none, and the fact gone,
// settles it as moved on; the fact still there, the row is open. A
// row's repeats move its time, so a bumped row asks again after an
// answer that preceded it.
func settleOf(m factoryd.ChatMessage, events []factoryd.Event, cardWaits, approvalWaits bool) pokeSettle {
	family := askFamily(m)
	var first, last factoryd.Event
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
		return pokeSettle{Word: settleWords[first.Kind], By: first.Actor, At: first.At, Event: first}
	}
	if cardWaits && family == "question" || approvalWaits && family == "approval" {
		return pokeSettle{Open: true}
	}
	at := m.At
	if last.ID != 0 {
		at = last.At
	}
	return pokeSettle{Word: "moved on", At: at}
}

// reasonLine is a settled ask's line, the decider line's pattern: word,
// actor, time (the ruling of 2026-10-17).
func reasonLine(s pokeSettle) string {
	if s.By != "" {
		return s.Word + " by " + s.By + " · " + s.At.UTC().Format("15:04")
	}
	return s.Word + " · " + s.At.UTC().Format("15:04")
}

// resolveChoice decides card id with choice as its state asks: a pending
// merge approves on Approve, or runs a factory pull request's red checks
// again on Re-run the checks, and a pending plan accepts on Accept, each
// under the role its board button and chat tool need, a waiting
// question takes the choice as the member's answer, a human-lane
// escalation closes with it. A card settled since its escalation row m
// asked, by an answer, the question's withdrawal or the wait guard,
// says so: the first answer wins.
func (c *Console) resolveChoice(ctx context.Context, a actor, id, choice string, m factoryd.ChatMessage) (string, error) {
	t, ok, err := c.cfg.Tasks.Task(ctx, id)
	switch {
	case err != nil:
		return "", refuse(http.StatusInternalServerError, "cannot read the card")
	case !ok:
		return "", refuse(http.StatusNotFound, "There is no card "+id+" in this project.")
	}
	events, err := c.cfg.Events.TaskEvents(ctx, id, startEvents)
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the card's events")
	}
	pending, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the approval requests")
	}
	s := settleOf(m, events, t.Status == "waiting-human", approvalWaits(pending, id))
	switch {
	case s.Event.ID != 0 && s.Event.Kind == factoryd.EventQuestionWithdrawn:
		return fmt.Sprintf("#%d needs no answer: its question is withdrawn because %v.", t.IssueNumber, s.Event.Detail["reason"]), nil
	case s.Event.ID != 0 && s.Event.Kind != factoryd.EventApprovalRequested:
		return fmt.Sprintf("#%d is answered already: %s by %s.", t.IssueNumber, strings.ReplaceAll(s.Event.Kind, "_", " "), s.Event.Actor), nil
	case !s.Open:
		return fmt.Sprintf("#%d waits no more: it was answered or moved on elsewhere.", t.IssueNumber), nil
	}
	for _, q := range pending {
		switch {
		case q.TaskID != id:
		case q.Kind == factoryd.ApprovalMerge && choice == "Approve":
			if err := c.admit(ctx, accessTable["approve_merge"], a, t.Repository, "task "+id); err != nil {
				return "", err
			}
			return c.doApprove(ctx, a, q.ID)
		case q.Kind == factoryd.ApprovalMerge && choice == factoryd.ChoiceRerun:
			if err := c.admit(ctx, accessTable["rerun_checks"], a, t.Repository, "task "+id); err != nil {
				return "", err
			}
			return c.doRerun(ctx, a, q.ID)
		case q.Kind == factoryd.ApprovalPlan && choice == "Accept":
			if err := c.admit(ctx, accessTable["accept_plan"], a, t.Repository, "task "+id); err != nil {
				return "", err
			}
			var spec factoryd.PlanSpec
			if err := json.Unmarshal(q.Spec, &spec); err != nil {
				return "", refuse(http.StatusInternalServerError, "cannot read the plan's revision")
			}
			return c.doAcceptPlan(ctx, a, id, spec.Revision, spec.SHA256)
		}
	}
	switch {
	case t.Status != "waiting-human":
		return fmt.Sprintf("#%d waits no more: it was answered or moved on elsewhere.", t.IssueNumber), nil
	case t.Kind == "human":
		return c.doClose(ctx, a, id, "Answered in the chat: "+choice)
	}
	note, err := c.doAnswer(ctx, a, id, choice)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(note, ".") + fmt.Sprintf(" with “%s”; the card goes on.", choice), nil
}

// escalate is the escalate tool: the question and its five parts (the
// question format) filed as a human-lane card in Needs you, which the
// inbox's poke carries to the project's members. The writer's own ask is
// never forced to a template, but it must carry its why and its info.
func (t *turn) escalate(ask, why, info, recommendation string, choices []string) (string, error) {
	ask, why, info = strings.TrimSpace(ask), strings.TrimSpace(why), strings.TrimSpace(info)
	recommendation = strings.TrimSpace(recommendation)
	for i := range choices {
		choices[i] = strings.TrimSpace(choices[i])
	}
	switch {
	case t.c.cfg.Escalate == nil:
		return "", refuse(http.StatusConflict, "this factory files no escalation from the chat")
	case ask == "" || len(ask) > 2000:
		return "", refuse(http.StatusBadRequest, "ask: the question of 1 to 2000 characters")
	case why == "" || len(why) > 2000:
		return "", refuse(http.StatusBadRequest, "why: why the card cannot go on without a person, 1 to 2000 characters")
	case info == "" || len(info) > 2000:
		return "", refuse(http.StatusBadRequest, "info: what a person should know to answer well, 1 to 2000 characters")
	case len(recommendation) > 2000:
		return "", refuse(http.StatusBadRequest, "recommendation: 0 to 2000 characters, empty to offer none")
	case len(choices) == 1 || len(choices) > factoryd.MaxChoices ||
		slices.ContainsFunc(choices, func(c string) bool { return c == "" || utf8.RuneCountInString(c) > factoryd.MaxChoice }):
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("choices: 2 to %d answers of 1 to %d characters, or none", factoryd.MaxChoices, factoryd.MaxChoice))
	}
	q := factoryd.HumanQuestion{Ask: ask, Why: why, Info: info, Recommendation: recommendation}
	for _, c := range choices {
		q.Choices = append(q.Choices, factoryd.QuestionChoice{Value: c, Label: c})
	}
	n, err := t.c.cfg.Escalate(t.r.Context(), t.p.Repository, q, t.a.s.user)
	if err != nil {
		log.Printf("console: the escalation of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return "", refuse(http.StatusBadGateway, "The escalation could not be filed; see the factoryd log.")
	}
	return fmt.Sprintf("Filed the escalation as #%d in Needs you; the inbox carries it to the project's members with its choices.", n), nil
}
