package console

import (
	"context"
	"fmt"
	"log"
	"math"
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
	// chatStopEvents bounds a stop's read of the chat task's events, which
	// are its stops and its funds alone.
	chatStopEvents = 50
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
	events, err := c.cfg.Events.TaskEvents(ctx, task.ID, chatStopEvents)
	if err != nil {
		return s, err
	}
	for _, e := range events {
		switch {
		case e.Kind == factoryd.EventChatBudgetSpent && s.event.ID == 0:
			s.event = e
		case e.Kind == factoryd.EventChatFunded:
			s.funds = append(s.funds, e)
		}
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
	return &chatBudget{BudgetEUR: s.task.BudgetEUR, SpentEUR: s.task.SpentEUR, SuggestEUR: suggest,
		CanFund: c.isApprover(ctx, login, repository)}
}

// suggestFund is the fund a stop suggests, in whole euros and at least 1:
// spent7, the chat task's spend over the last 7 days, as a daily rate over
// those days, or over the month's days so far when fewer (a day at
// least), carried to the next UTC month.
func suggestFund(spent7 float64, now time.Time) float64 {
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	days := min(7, max(1, now.Sub(month).Hours()/24))
	left := month.AddDate(0, 1, 0).Sub(now).Hours() / 24
	return max(1, math.Ceil(spent7/days*left))
}

// askFund records the chat's stop once, the chat_budget_spent event the
// Poker turns into the inbox's needs-you row, with the fund it suggests:
// only while the chat is not stopped already, so every refusal after the
// first asks nothing. It answers the stop as it then stands.
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
	e := factoryd.Event{Repository: p.Repository, TaskID: task.ID, Kind: factoryd.EventChatBudgetSpent,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, Detail: map[string]any{"budget_eur": task.BudgetEUR,
			"spent_eur": task.SpentEUR, "suggest_eur": suggestFund(spent7, now)}}
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
		row.Content = fmt.Sprintf("The chat's budget for %s is used up: €%.2f of €%.2f spent, and the next call does not fit what is left.",
			time.Now().UTC().Format("January"), task.SpentEUR, task.BudgetEUR)
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
	t.send("applied", map[string]string{"tool": "fund the chat", "result": fmt.Sprintf("Raised the chat's budget from €%.2f to €%.2f.", from, to)})
	t.takeTurn(pos, wait)
	if !t.stopRow() {
		t.send("done", struct{}{})
		return
	}
	release := c.turns.slot(t.p.Repository, t.parallelTurns())
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
	t.stream(func(string) error { return nil }, nil)
	t.takeTurn(pos, wait)
	if !t.stopRow() {
		return
	}
	release := c.turns.slot(p.Repository, t.parallelTurns())
	defer release()
	task, err := c.cfg.Chat.EnsureChatTask(ctx, p.Name, p.Repository, t.cfg.MonthlyEUR, time.Now())
	if err != nil {
		log.Printf("console: the chat task of %s: %v", p.Name, err)
		return
	}
	t.run(task.ID, "")
}
