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
	rows, err := c.cfg.Chat.ChatMessages(ctx, t.p.Repository, t.owner, t.conv, chatHistory)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.owner, t.p.Repository, err)
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
	queued, land := c.Behind(t.p.Repository, t.owner, t.conv)
	var ids []int64
	defer func() { land(ids...) }()
	for _, m := range []factoryd.ChatMessage{{Role: "user", Content: choice}, {Role: "assistant", Kind: factoryd.MessagePoke, Content: note, TaskID: rows[i].TaskID}} {
		m.Conversation, m.Queued = t.conv, queued
		id, err := c.cfg.Chat.AddChatMessage(ctx, t.p.Repository, t.owner, m)
		if err != nil {
			log.Printf("console: the chat of %s on %s: %v", t.owner, t.p.Repository, err)
			http.Error(w, "cannot store the answer", http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
	}
	w.WriteHeader(http.StatusNoContent)
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
	s := factoryd.SettleOf(m, events, t.Status == "waiting-human", factoryd.ApprovalWaits(pending, id))
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

// askMember is the ask tool: the question kept in this thread as an ask
// row, its choices offered as presses. The row rides the answer's own
// row (run merges it in), and the turn ends on it: the member's press or
// their next message is the answer the next turn reads.
func (t *turn) askMember(question string, choices []string) (string, error) {
	question = strings.TrimSpace(question)
	for i := range choices {
		choices[i] = strings.TrimSpace(choices[i])
	}
	switch {
	case question == "" || len(question) > 2000:
		return "", refuse(http.StatusBadRequest, "question: the question of 1 to 2000 characters")
	case len(choices) < 2 || len(choices) > factoryd.MaxChoices ||
		slices.ContainsFunc(choices, func(c string) bool { return c == "" || utf8.RuneCountInString(c) > factoryd.MaxChoice }):
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("choices: 2 to %d answers of 1 to %d characters", factoryd.MaxChoices, factoryd.MaxChoice))
	}
	t.ask = &factoryd.ChatMessage{Kind: factoryd.MessageAsk, Content: question, Choices: choices}
	return "Asked the member; this turn ends. A press or their next message answers it.", nil
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
