package console

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
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
// then 204; a choice of a bound ask row runs its bound tool call in code
// as the member's session or settles the ask, streamed (chatAskPress). A
// card that waits no more says so: the first answer wins.
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
		return m.ID == id && ((m.Kind == factoryd.MessageEscalation && m.TaskID != "") || (m.Kind == factoryd.MessageAsk && m.Proposal != ""))
	})
	if i < 0 {
		c.fail(w, r, refuse(http.StatusNotFound, "404 page not found"))
		return
	}
	if !slices.Contains(rows[i].Choices, choice) {
		whose := "the escalation's"
		if rows[i].Kind == factoryd.MessageAsk {
			whose = "the ask's"
		}
		c.fail(w, r, refuse(http.StatusBadRequest, "the choice is none of "+whose))
		return
	}
	if rows[i].Kind == factoryd.MessageAsk {
		c.chatAskPress(w, r, t, rows[i], choice)
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

// chatAskPress answers chatAnswer's ask branch: a choice of the
// thread's bound ask row, the ask tool's question. Every press lands
// the human's row with its choice; the bound choice's press then runs
// the bound tool call in code as the human's session, streamed, the
// do- row landing as a Do it's does, any other press feeds the agent a
// turn, which reads the row as their answer. A bound set_pool choice
// runs the offer's captured change — the one press the confirmation:
// no second proposal, no model turn, no fresh read — guarded before
// the stream on the blob the offer read, so a file that moved since
// the offer answers 409, the attempt valid and its token spent, and
// nothing rebases the change onto the moved file. Taking the binding
// settles the ask: a second press hears it is gone, another human's
// that it is not theirs; a binding expired leaves the question an
// ordinary ask, every press of it settling.
func (c *Console) chatAskPress(w http.ResponseWriter, r *http.Request, t *turn, m factoryd.ChatMessage, choice string) {
	p, err := t.c.cfg.Chat.TakeProposal(t.r.Context(), "ask", m.Proposal, t.p.Repository, t.a.s.user, time.Now())
	gone := errors.Is(err, factoryd.ErrNoProposal)
	switch {
	case err == nil:
		if err := t.inThread(p.Conversation); err != nil {
			c.fail(w, r, err)
			return
		}
		if choice == p.Choice && p.Pool != nil {
			// the bound pool choice confirms the offer's capture, so its
			// guard runs before the stream, as a Do it's does: the write
			// never lands against a file the offer did not read
			if err := t.stalePool(p.Pool.Source); err != nil {
				c.fail(w, r, err)
				return
			}
		}
	case errors.Is(err, factoryd.ErrOthersProposal):
		http.Error(w, "It is another human's.", http.StatusForbidden)
		return
	case errors.Is(err, factoryd.ErrUsedProposal):
		// the press's own words, not fail's plain 404: the plan pins the
		// used binding's answer on the ask row's button
		http.Error(w, "There is no such proposal or undo; it may have expired.", http.StatusNotFound)
		return
	case !gone:
		c.fail(w, r, err)
		return
	}
	pos, wait := c.turns.enter(t.key())
	defer c.turns.leave(t.key())
	t.detach()
	t.open(w)
	t.takeTurn(pos, wait)
	if _, ok := t.save(factoryd.ChatMessage{Role: "user", Content: choice}); !ok {
		return
	}
	if err == nil && choice == p.Choice {
		t.apply(p, nil)
		return
	}
	release := t.c.turns.slot(context.Background(), t.p.Repository, t.parallelTurns())
	defer release()
	task, err := t.c.cfg.Chat.EnsureChatTask(t.r.Context(), t.p.Name, t.p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", t.p.Name, err)
		t.fail("The chat's task could not be read.")
		return
	}
	t.run(task.ID, "")
}

// resolveChoice decides card id with choice as its state asks: a pending
// merge approves on Approve, or runs a factory pull request's red checks
// again on Re-run the checks, each under the role its board button and
// chat tool need, a waiting
// question takes the choice as the member's answer (its park, close or
// fund acting as the board's button does), a choice card (the stall
// card, the failed roll card) acts on it, another human-lane
// escalation closes with it. A card settled since
// its escalation row m asked, by an answer, the question's withdrawal or
// the wait guard, says so: the first answer wins.
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
		}
	}
	switch {
	case t.Status != "waiting-human":
		return fmt.Sprintf("#%d waits no more: it was answered or moved on elsewhere.", t.IssueNumber), nil
	case t.Kind == "human":
		return c.doAnswer(ctx, a, id, choice)
	}
	note, err := c.doAnswer(ctx, a, id, choice)
	if answered := fmt.Sprintf("Answered #%d.", t.IssueNumber); err != nil || note != answered {
		return note, err
	}
	return fmt.Sprintf("Answered #%d with “%s”; the card goes on.", t.IssueNumber, choice), nil
}

// askMember is the ask tool: the question kept in this thread as an ask
// row, its choices offered as presses. The row rides the answer's own
// row (run merges it in), and the turn ends on it: the member's press or
// their next message is the answer the next turn reads. A question may
// bind one choice to a tool call (tool, args, choice): that choice's
// press runs the call in code as the member's own session, under the
// checks the propose path runs in call, until the binding expires.
func (t *turn) askMember(in toolInput) (string, error) {
	question := strings.TrimSpace(in.Ask)
	choices := slices.Clone(in.Choices)
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
	token, desc, err := t.askBinding(in.Tool, in.Args, in.Choice, choices)
	if err != nil {
		return "", err
	}
	t.ask = &factoryd.ChatMessage{Kind: factoryd.MessageAsk, Content: question, Choices: choices, Proposal: token}
	if token == "" {
		return "Asked the human; this turn ends. A press or their next message answers it.", nil
	}
	return fmt.Sprintf("Asked the human; this turn ends. Pressing “%s” runs %s; another press answers it.", in.Choice, strings.TrimSuffix(desc, ".")), nil
}

// askBinding validates and stores the ask's bound action, the tool a
// press of choice runs with args, under the checks the propose path runs
// in call: the tool exists and writes, the arguments normalise and
// decode, admitTool passes for the member's roles, describe writes the
// human-facing line. A set_pool choice binds the offer's own capture —
// describe read the project file, its blob and its pool — in the row,
// so the press runs the change the member read, never one the press
// re-reads. It answers the binding's token and its description; both ""
// when the ask binds nothing.
func (t *turn) askBinding(name string, args json.RawMessage, choice string, choices []string) (string, string, error) {
	if name == "" && choice == "" && len(args) == 0 {
		return "", "", nil
	}
	if name == "" || choice == "" {
		return "", "", refuse(http.StatusBadRequest, "tool: the write tool the bound choice runs, with its choice")
	}
	if !slices.Contains(choices, choice) {
		return "", "", refuse(http.StatusBadRequest, "choice: one of the ask's choices")
	}
	tool, ok := t.c.chatTool(name)
	if !ok {
		return "", "", refuse(http.StatusBadRequest, "tool: there is no tool "+name)
	}
	if !tool.write {
		return "", "", refuse(http.StatusBadRequest, "tool: "+name+" reads; a bound tool writes")
	}
	var in toolInput
	raw := cmp.Or(string(args), "{}")
	raw = tool.normalized(raw)
	if json.Unmarshal([]byte(raw), &in) != nil {
		return "", "", refuse(http.StatusBadRequest, "args: no JSON object of "+name+"'s parameters")
	}
	asked := in
	if err := t.byTaskID(&in); err != nil {
		return "", "", err
	}
	if err := t.admitTool(name, in); err != nil {
		return "", "", err
	}
	desc, err := t.describe(tool, &in)
	if err != nil {
		return "", "", err
	}
	if !reflect.DeepEqual(in, asked) {
		if b, err := json.Marshal(in); err == nil {
			raw = string(b)
		}
	}
	token := t.store(factoryd.ChatProposal{Token: randomToken(), Kind: "ask", Choice: choice, Repository: t.p.Repository, Login: t.owner,
		Tool: name, Args: json.RawMessage(raw), Description: desc, Conversation: t.conv, Pool: t.pool})
	t.pool = nil
	if token == "" {
		return "", "", refuse(http.StatusInternalServerError, "The binding could not be stored; nothing runs.")
	}
	// the park covers the tool at once: a later call of this same answer
	// meets the refusal, not only the next turn's
	if t.parked != nil {
		t.parked[name] = true
	}
	return token, desc, nil
}

// escalate is the escalate tool: the question and its five parts (the
// question format) filed as a human-lane card in Needs you, which the
// inbox's poke carries to the project's members. The ask is the card's
// title, so it is one short sentence. Each choice sits in the "say:" value
// namespace, so no label the agent writes equals a factory control value.
func (t *turn) escalate(ask, why, info, recommendation string, choices []string) (string, error) {
	ask, why, info = strings.TrimSpace(ask), strings.TrimSpace(why), strings.TrimSpace(info)
	recommendation = strings.TrimSpace(recommendation)
	for i := range choices {
		choices[i] = strings.TrimSpace(choices[i])
	}
	switch {
	case t.c.cfg.Escalate == nil:
		return "", refuse(http.StatusConflict, "this factory files no escalation from the chat")
	case ask == "" || len(ask) > factoryd.MaxAsk || strings.ContainsAny(ask, "\r\n"):
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("ask: one sentence of 1 to %d bytes, without a line break; this one has %d", factoryd.MaxAsk, len(ask)))
	case why == "" || len(why) > 2000:
		return "", refuse(http.StatusBadRequest, "why: why the card cannot go on without a person, 1 to 2000 characters")
	case info == "" || len(info) > 2000:
		return "", refuse(http.StatusBadRequest, "info: what a person should know to answer well, 1 to 2000 characters")
	case recommendation == "" || len(recommendation) > 2000:
		return "", refuse(http.StatusBadRequest, "recommendation: the answer you would pick and where you read it, 1 to 2000 characters")
	case len(choices) < 2 || len(choices) > factoryd.MaxChoices ||
		slices.ContainsFunc(choices, func(c string) bool { return c == "" || utf8.RuneCountInString(c) > factoryd.MaxChoice }):
		return "", refuse(http.StatusBadRequest, fmt.Sprintf("choices: 2 to %d answers of 1 to %d characters", factoryd.MaxChoices, factoryd.MaxChoice))
	case len(slices.Compact(slices.Sorted(slices.Values(choices)))) != len(choices):
		return "", refuse(http.StatusBadRequest, "choices: each answer once")
	}
	q := factoryd.HumanQuestion{Ask: ask, Why: why, Info: info, Recommendation: recommendation, Source: "the agent's"}
	for _, c := range choices {
		q.Choices = append(q.Choices, factoryd.QuestionChoice{Value: "say:" + c, Label: c})
	}
	n, err := t.c.cfg.Escalate(t.r.Context(), t.p.Repository, q, t.a.s.user)
	if err != nil {
		log.Printf("console: the escalation of %s on %s: %v", t.a.s.user, t.p.Repository, err)
		return "", refuse(http.StatusBadGateway, "The escalation could not be filed; see the factoryd log.")
	}
	return fmt.Sprintf("Filed the escalation as #%d in Needs you; the inbox carries it to the project's humans with its choices.", n), nil
}
