package console

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chat's budget stop. The gateway alone judges whether the chat can
// pay: its refused_budget on a chat call ends the turn on a stop row and
// is recorded once as the chat_budget_spent event of the month's chat
// task, and every other place reads that fact: the fund control under
// the row, the board's status panel, the wake's refusal. One fund lifts
// the stop and every stopped turn goes on.

const (
	// chatFundEvents bounds a stop's read of the chat task's funds.
	chatFundEvents = 50
	// chatFundNote is the note of a fund pressed in the chat.
	chatFundNote = "funded in the chat"
)

// chatStop is a project's chat task of the month and its stop: the
// task's newest chat_budget_spent event, ID 0 for none, and its
// chat_funded events, newest first. stopped: the event exists and the
// task's budget is not above the budget it recorded, so neither a fund
// nor a raise of the policy came since.
type chatStop struct {
	task    factoryd.Task
	event   factoryd.Event
	funds   []factoryd.Event
	stopped bool
}

// chatStopOf reads chat task task's stop.
func (c *Console) chatStopOf(ctx context.Context, task factoryd.Task) (chatStop, error) {
	s := chatStop{task: task}
	stops, err := c.cfg.Events.TaskEventsOfKind(ctx, task.ID, factoryd.EventChatBudgetSpent, 1)
	if err != nil {
		return s, err
	}
	if len(stops) > 0 {
		s.event = stops[0]
	}
	if s.funds, err = c.cfg.Events.TaskEventsOfKind(ctx, task.ID, factoryd.EventChatFunded, chatFundEvents); err != nil {
		return s, err
	}
	if s.event.ID != 0 {
		at, _ := s.event.Detail["budget_eur"].(float64)
		s.stopped = task.BudgetEUR <= at
	}
	return s, nil
}

// chatStopNow is project p's chat stop this UTC month; a month whose
// chat task is not filed yet is not stopped.
func (c *Console) chatStopNow(ctx context.Context, p factoryd.Project) (chatStop, error) {
	task, ok, err := c.cfg.Tasks.Task(ctx, factoryd.ChatTaskID(p.Name, time.Now()))
	if err != nil || !ok || task.Kind != factoryd.KindChat || task.Status != "running" {
		return chatStop{task: task}, err
	}
	return c.chatStopOf(ctx, task)
}

// chatBudget is a stopped chat's fund state, as GET /chat and the stop's
// stream event carry it: the budget, the spend, the fund the stop
// suggests, and whether the reader may fund the chat, an approver of the
// project as /chat/fund's rule has it.
type chatBudget struct {
	BudgetEUR  float64 `json:"budget_eur"`
	SpentEUR   float64 `json:"spent_eur"`
	SuggestEUR float64 `json:"suggest_eur"`
	CanFund    bool    `json:"can_fund"`
}

// fundState is stop s's fund state for login on repository; nil while
// the chat is not stopped.
func (c *Console) fundState(ctx context.Context, s chatStop, login, repository string) *chatBudget {
	if !s.stopped {
		return nil
	}
	suggest, _ := s.event.Detail["suggest_eur"].(float64)
	return &chatBudget{BudgetEUR: s.task.BudgetEUR, SpentEUR: s.task.EnforcedSpent(), SuggestEUR: suggest,
		CanFund: c.isApprover(ctx, login, repository)}
}

// askFund records the chat's stop once, the chat_budget_spent event the
// Poker turns into the inbox's needs-you row, with the fund it suggests,
// and files the stop's human card: only while the chat is not stopped
// already, so every refusal after the first asks nothing. The stop is
// recorded whatever the filing does; a card that fails to file is filed by
// factoryd's minute loop (factoryd.ChatBudgetCards.Check). It answers the
// stop as it then stands.
func (c *Console) askFund(ctx context.Context, p factoryd.Project, task factoryd.Task) chatStop {
	s, err := c.chatStopOf(ctx, task)
	if err != nil {
		log.Printf("console: the chat stop of %s: %v", p.Repository, err)
	}
	if s.stopped {
		return s
	}
	now := time.Now()
	spent7, err := c.cfg.Chat.TaskSpendSince(ctx, task.ID, now.Add(-7*24*time.Hour))
	if err != nil {
		log.Printf("console: the chat spend of %s: %v", p.Repository, err)
	}
	if c.cfg.FileChatBudgetCard != nil {
		if err := c.cfg.FileChatBudgetCard(ctx, p, task); err != nil {
			log.Printf("console: the chat budget card of %s: %v", p.Repository, err)
		}
	}
	e := factoryd.Event{Repository: p.Repository, TaskID: task.ID, Kind: factoryd.EventChatBudgetSpent,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"budget_eur": task.BudgetEUR,
			"spent_eur": task.EnforcedSpent(), "suggest_eur": factoryd.SuggestFund(spent7, now)}}
	if e.ID, err = c.cfg.Events.Record(ctx, e); err != nil {
		log.Printf("console: the chat ask of %s: %v", p.Repository, err)
	}
	s.event, s.stopped = e, true
	return s
}

// budgetEvent is the stop's stream event: the stop row, and the fund
// state for the member when the chat's own budget stopped it.
type budgetEvent struct {
	Message factoryd.ChatMessage `json:"message"`
	Budget  *chatBudget          `json:"budget,omitempty"`
}

// stop ends the turn on the gateway's refusal of its call for money,
// false for a refusal of another kind: a row of kind budget in the
// thread, its first line plain and the gateway's reason the next. The
// chat task's own budget records the ask (askFund) and offers the fund;
// a factory cap names the cap and what raises it. The open view gets the
// row at once with its fund control, then done.
func (t *turn) stop(refusal *policy.Refusal) bool {
	row := factoryd.ChatMessage{Role: "assistant", Kind: factoryd.MessageBudget}
	switch refusal.Decision {
	case policy.RefusedBudget:
		row.BirthKind = factoryd.EventChatBudgetSpent
	case policy.RefusedDayCap:
		row.Content = "The factory's day cap is used up: the chat goes on when the UTC day turns, or once a commit raises budget.day_eur in policy.yaml."
	case policy.RefusedMonthCap:
		row.Content = "The factory's month cap is used up: the chat goes on when the UTC month turns, or once a commit raises budget.month_eur in policy.yaml."
	case policy.RefusedProjectCap:
		row.Content = "The project's month cap is used up: the chat goes on when the UTC month turns, or once a commit raises budget.month_eur in projects/" +
			t.p.Name + ".yaml."
	default:
		return false
	}
	var fund *chatBudget
	if row.BirthKind != "" {
		ctx := context.WithoutCancel(t.r.Context())
		task, ok, err := t.c.cfg.Tasks.Task(ctx, t.task)
		if err != nil || !ok {
			log.Printf("console: chat task %s: found %v, %v", t.task, ok, err)
			t.fail("The chat's task could not be read.")
			return true
		}
		row.Content = fmt.Sprintf("The judgment budget for %s is used up: €%.2f of €%.2f spent, and the next call does not fit what is left.",
			time.Now().UTC().Format("January"), task.EnforcedSpent(), task.BudgetEUR)
		fund = t.c.fundState(ctx, t.c.askFund(ctx, t.p, task), t.a.s.user, t.p.Repository)
	}
	row.Content += "\n" + refusal.Reason
	id, ok := t.save(row)
	if !ok {
		return true
	}
	row.ID, row.At, row.Conversation = id, time.Now(), t.conv
	t.send("budget", budgetEvent{Message: row, Budget: fund})
	t.send("done", struct{}{})
	return true
}

// isStopRow says m is the chat budget's stop row, the end of a turn a
// fund resumes.
func isStopRow(m factoryd.ChatMessage) bool {
	return m.Kind == factoryd.MessageBudget && m.BirthKind == factoryd.EventChatBudgetSpent
}

// fundRow says m asks for the chat's fund: a stop row, or the inbox's
// poke of the stop.
func fundRow(m factoryd.ChatMessage) bool {
	return m.BirthKind == factoryd.EventChatBudgetSpent && (m.Kind == factoryd.MessageBudget || m.Kind == factoryd.MessagePoke)
}

// fundSettle is fund row m's derived state (the notification-settling
// concept): settled by the first fund at or after it, "funded by <actor>
// · 15:04", else once the chat stands stopped no more, "moved on".
func fundSettle(m factoryd.ChatMessage, s chatStop) (bool, string) {
	for i := len(s.funds) - 1; i >= 0; i-- {
		if f := s.funds[i]; !f.At.Before(m.At) {
			return true, factoryd.ReasonLine(factoryd.PokeSettle{Word: "funded", By: f.Actor, At: f.At})
		}
	}
	if !s.stopped {
		return true, "moved on"
	}
	return false, ""
}

// stopRow says the turn's thread's newest row is the chat budget's stop.
func (t *turn) stopRow() bool {
	rows, err := t.c.cfg.Chat.ChatMessages(t.r.Context(), t.p.Repository, t.owner, t.conv, 1)
	if err != nil {
		log.Printf("console: the chat of %s on %s: %v", t.owner, t.p.Repository, err)
		return false
	}
	return len(rows) == 1 && isStopRow(rows[0])
}

// chatFundAndGo answers POST /projects/{name}/chat/budget, an approver's
// Fund and go on at a stopped chat's row: the chat task's budget rises by
// eur (doChatFund, which resumes every other stopped thread), the stream
// says from what to what, and the thread's own turn goes on when its
// newest row is the stop, streamed as a turn is; else the stream ends.
func (c *Console) chatFundAndGo(w http.ResponseWriter, r *http.Request) {
	t, ok := c.chatTurn(w, r)
	if !ok {
		return
	}
	eur, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("eur")), 64)
	if err != nil {
		c.fail(w, r, refuse(http.StatusBadRequest, "an amount in EUR above zero is required"))
		return
	}
	if _, err := t.thread(r, true); err != nil {
		c.fail(w, r, err)
		return
	}
	t.a.decider = "member"
	from, to, err := c.doChatFund(r.Context(), t.a, t.p.Name, eur, chatFundNote, t.key())
	if err != nil {
		c.fail(w, r, err)
		return
	}
	pos, wait := c.turns.enter(t.key())
	defer c.turns.leave(t.key())
	t.detach()
	t.open(w)
	t.send("applied", map[string]string{"tool": "fund the chat", "result": fmt.Sprintf("Raised the judgment budget from €%.2f to €%.2f.", from, to)})
	t.takeTurn(pos, wait)
	if !t.stopRow() {
		t.send("done", struct{}{})
		return
	}
	release := c.turns.slot(context.Background(), t.p.Repository, t.parallelTurns())
	defer release()
	task, err := c.cfg.Chat.EnsureChatTask(t.r.Context(), t.p.Name, t.p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", t.p.Name, err)
		t.fail("The chat's task could not be read.")
		return
	}
	t.run(task.ID, "")
}

// resumeStopped goes on with every turn of project p the chat's budget
// stopped, thread skip's aside (a thread key, "" for none): each thread
// whose newest row is the stop resumes detached (resume).
func (c *Console) resumeStopped(ctx context.Context, p factoryd.Project, skip string) {
	threads, err := c.cfg.Chat.BudgetStoppedThreads(ctx, p.Repository)
	if err != nil {
		log.Printf("console: the stopped threads of %s: %v", p.Repository, err)
		return
	}
	for _, th := range threads {
		if threadKey(p.Repository, th.Login, th.Number) != skip {
			go c.resume(ctx, p, th.Login, th.Number)
		}
	}
}

// resume goes on with login's thread n of project p as the owner's wake
// on p's chat token (wakeTurn), behind a turn in flight there, when its
// place comes and its newest row is still the stop.
func (c *Console) resume(ctx context.Context, p factoryd.Project, login string, n int64) {
	t, err := c.wakeTurn(ctx, p, login, n)
	if err != nil {
		log.Printf("console: resume thread %d of %s on %s: %v", n, login, p.Repository, err)
		return
	}
	pos, wait := c.turns.enter(t.key())
	defer c.turns.leave(t.key())
	t.quiet()
	t.takeTurn(pos, wait)
	if !t.stopRow() {
		return
	}
	release := c.turns.slot(ctx, p.Repository, t.parallelTurns())
	defer release()
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", p.Name, err)
		return
	}
	t.run(task.ID, "")
}

// retryTurn answers login's thread n of project p once, when the turn
// that read its rows failed and put them back to the queue: a quiet wake
// turn (wakeTurn), behind any turn in flight there, whose own failure
// strands the rows for the next member turn instead of chaining retries.
func (c *Console) retryTurn(ctx context.Context, p factoryd.Project, login string, n int64) {
	t, err := c.wakeTurn(ctx, p, login, n)
	if err != nil {
		log.Printf("console: retry thread %d of %s on %s: %v", n, login, p.Repository, err)
		return
	}
	t.retry = true
	pos, wait := c.turns.enter(t.key())
	defer c.turns.leave(t.key())
	t.quiet()
	t.takeTurn(pos, wait)
	if !t.queuedRows() {
		return // another turn of the thread took the rows back
	}
	release := c.turns.slot(ctx, p.Repository, t.parallelTurns())
	defer release()
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", p.Name, err)
		return
	}
	t.run(task.ID, "")
}
