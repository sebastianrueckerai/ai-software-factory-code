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
// of it land in the thread, then 204. A card that waits no more says so:
// the first answer wins.
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
	note, err := c.resolveChoice(ctx, a, rows[i].TaskID, choice, rows[i].At)
	if err != nil {
		note = "The answer did not land: " + t.sentence(err)
	}
	for _, m := range []factoryd.ChatMessage{{Role: "user", Content: choice}, {Role: "assistant", Kind: factoryd.MessagePoke, Content: note, TaskID: rows[i].TaskID}} {
		m.Conversation = t.conv
		if err := c.cfg.Chat.AddChatMessage(ctx, t.p.Repository, t.a.s.user, m); err != nil {
			log.Printf("console: the chat of %s on %s: %v", t.a.s.user, t.p.Repository, err)
			http.Error(w, "cannot store the answer", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// answerKinds are the events that settle an escalation: an answer on any
// venue, or factoryd's withdrawal of the question. The first one after
// the escalation wins.
var answerKinds = []string{factoryd.EventQuestionAnswered, factoryd.EventSessionGranted, factoryd.EventMergeApproved, factoryd.EventMergeDenied,
	factoryd.EventPlanAccepted, factoryd.EventPlanChangesRequested, factoryd.EventPlanRejected, factoryd.EventCardClosed, factoryd.EventQuestionWithdrawn}

// resolveChoice decides card id with choice as its state asks: a pending
// merge approves on Approve and a pending plan accepts on Accept, each
// under the role its board button and chat tool need, a waiting
// question takes the choice as the member's answer, a human-lane
// escalation closes with it. A card answered since the escalation asked
// at asked, its question withdrawn, or waiting no more, says so: the
// first answer wins.
func (c *Console) resolveChoice(ctx context.Context, a actor, id, choice string, asked time.Time) (string, error) {
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
	if i := slices.IndexFunc(events, func(e factoryd.Event) bool { return slices.Contains(answerKinds, e.Kind) && !e.At.Before(asked) }); i >= 0 {
		if e := events[i]; e.Kind == factoryd.EventQuestionWithdrawn {
			return fmt.Sprintf("#%d needs no answer: its question is withdrawn because %v.", t.IssueNumber, e.Detail["reason"]), nil
		}
		return fmt.Sprintf("#%d is answered already: %s by %s.", t.IssueNumber, strings.ReplaceAll(events[i].Kind, "_", " "), events[i].Actor), nil
	}
	pending, err := c.cfg.Approvals.ApprovalRequests(ctx, "pending")
	if err != nil {
		return "", refuse(http.StatusInternalServerError, "cannot read the approval requests")
	}
	for _, q := range pending {
		switch {
		case q.TaskID != id:
		case q.Kind == factoryd.ApprovalMerge && choice == "Approve":
			if err := c.admit(ctx, accessTable["approve_merge"], a, t.Repository, "task "+id); err != nil {
				return "", err
			}
			return c.doApprove(ctx, a, q.ID)
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

// escalate is the escalate tool: the question and its choices filed as a
// human-lane card in Needs you, which the inbox's poke carries to the
// project's members.
func (t *turn) escalate(ask string, choices []string) (string, error) {
	ask = strings.TrimSpace(ask)
	for i := range choices {
		choices[i] = strings.TrimSpace(choices[i])
	}
	switch {
	case t.c.cfg.Escalate == nil:
		return "", refuse(http.StatusConflict, "this factory files no escalation from the chat")
	case ask == "" || len(ask) > 2000:
		return "", refuse(http.StatusBadRequest, "ask: the question of 1 to 2000 characters")
	case len(choices) == 1 || len(choices) > factoryd.MaxChoices ||
		slices.ContainsFunc(choices, func(c string) bool { return c == "" || utf8.RuneCountInString(c) > factoryd.MaxChoice }):
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("choices: 2 to %d answers of 1 to %d characters, or none", factoryd.MaxChoices, factoryd.MaxChoice))
	}
	n, err := t.c.cfg.Escalate(t.r.Context(), t.p.Repository, ask, choices, t.a.s.user)
	if err != nil {
		log.Printf("console: the escalation of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return "", refuse(http.StatusBadGateway, "The escalation could not be filed; see the factoryd log.")
	}
	return fmt.Sprintf("Filed the escalation as #%d in Needs you; the inbox carries it to the project's members with its choices.", n), nil
}
