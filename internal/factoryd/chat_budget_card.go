package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// SourceChatBudget is the source of the human-lane task filed when a
// project's chat budget is spent.
const SourceChatBudget = "chat_budget"

// ChatBudgetBook is what the chat budget's cards read and keep.
type ChatBudgetBook interface {
	CreateTask(ctx context.Context, t Task) error
	CreateHumanCard(ctx context.Context, t Task, ask HumanAsk) (Task, bool, error)
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	TasksByParent(ctx context.Context, id string) ([]Task, error)
	TaskEventsOfKind(ctx context.Context, id, kind string, limit int) ([]Event, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	projectLookup
	taskReader
}

// ChatBudgetCards files the human card of a project's chat budget that is
// spent, once per month and budget, and closes it when a raise lifts the
// budget or the month ends. The card is on the project's repository, its
// Parent the chat task and its BudgetEUR the budget it was filed at.
type ChatBudgetCards struct {
	mu       sync.Mutex
	Store    ChatBudgetBook
	Forgejo  *Forgejo
	Timeline *Timeline
	Metrics  *telemetry.Metrics
}

// SuggestFund is the fund a chat stop suggests, in whole euros and at least 1:
// spent7, the chat task's spend over the last 7 days, as a daily rate over
// those days, or over the month's days so far when fewer (a day at
// least), carried to the next UTC month.
func SuggestFund(spent7 float64, now time.Time) float64 {
	now = now.UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	days := min(7, max(1, now.Sub(month).Hours()/24))
	left := month.AddDate(0, 1, 0).Sub(now).Hours() / 24
	return max(1, math.Ceil(spent7/days*left))
}

// chatMonth is the UTC month of chat task id: YYYY-MM, the id's tail.
func chatMonth(id string) string {
	return id[len(id)-len("2006-01"):]
}

func chatBudgetQuestion(chat Task) HumanQuestion {
	month := chatMonth(chat.ID)
	return HumanQuestion{
		Ask: fmt.Sprintf("The judgment budget of %.2f EUR for %s is spent: raise it?", chat.BudgetEUR, month),
		Why: fmt.Sprintf("The project spent %.2f EUR of its judgment budget of %.2f EUR in %s. The judgment budget pays the chat, rung 1, rung 2 and the check-in's judgment. Until it is raised or the UTC month turns, no rung judges a stopped card, so every stop reaches Needs you unjudged.",
			chat.EnforcedSpent(), chat.BudgetEUR, month),
		Info: "The action: raise the judgment budget on the project screen, or press Fund and go on in the chat thread that hit the stop. " +
			"Only a human who approves the project can do either. The raise ends this card.",
		Choices: []QuestionChoice{{AnswerClose, "Decided, close"}},
	}
}

// File opens the card of project p's chat task chat at its budget, unless
// a card of that chat task and budget exists in any status: a card a human
// closed by hand is not filed again.
func (c *ChatBudgetCards) File(ctx context.Context, p Project, chat Task) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	read := chat
	chat, ok, err := c.Store.Task(ctx, read.ID)
	if err != nil || !ok || chat.Status != "running" || chat.BudgetEUR != read.BudgetEUR {
		return err
	}
	cards, err := c.Store.TasksByParent(ctx, chat.ID)
	if err != nil {
		return err
	}
	for _, t := range cards {
		if t.Source == SourceChatBudget && t.BudgetEUR == chat.BudgetEUR {
			return nil
		}
	}
	t := Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard", Source: SourceChatBudget, Parent: chat.ID, BudgetEUR: chat.BudgetEUR, Author: c.Forgejo.BotUser}
	_, _, err = fileHumanTask(ctx, c.Store, c.Forgejo, c.Timeline, p.Repo(), chatBudgetQuestion(chat), nil, t, "")
	return err
}

// stoppedWithoutCard files the card of every running chat task whose
// recorded stop stands at its budget: the filing of a stop that met a
// failure is tried again here.
func (c *ChatBudgetCards) stoppedWithoutCard(ctx context.Context, now time.Time) error {
	running, err := c.Store.ListTasksByStatus(ctx, "running")
	if err != nil {
		return err
	}
	var errs []error
	for _, chat := range running {
		if chat.Kind != KindChat || chatMonth(chat.ID) < now.UTC().Format("2006-01") {
			continue
		}
		stops, err := c.Store.TaskEventsOfKind(ctx, chat.ID, EventChatBudgetSpent, 1)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(stops) == 0 {
			continue
		}
		if at, _ := stops[0].Detail["budget_eur"].(float64); chat.BudgetEUR > at {
			continue
		}
		p, ok, err := c.Store.ProjectByRepository(ctx, chat.Repository)
		if err != nil || !ok {
			errs = append(errs, err)
			continue
		}
		if err := c.File(ctx, p, chat); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Check files the cards of the stops that have none, then closes every
// open card whose chat task's budget was raised above the card's, or whose
// month ended before now. A card that fails does not stop the others; the
// failures are joined.
func (c *ChatBudgetCards) Check(ctx context.Context, now time.Time) error {
	open, err := c.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return err
	}
	errs := []error{c.stoppedWithoutCard(ctx, now)}
	for _, t := range open {
		if t.Source != SourceChatBudget {
			continue
		}
		chat, ok, err := c.Store.Task(ctx, t.Parent)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		month := chatMonth(t.Parent)
		var msg string
		switch {
		case month < now.UTC().Format("2006-01"):
			msg = fmt.Sprintf("The UTC month %s ended: the card closes.", month)
		case ok && chat.BudgetEUR > t.BudgetEUR:
			msg = fmt.Sprintf("The judgment budget is %.2f EUR for %s: the card closes.", chat.BudgetEUR, month)
		default:
			continue
		}
		if err := closeIssueAsFactory(ctx, c.Store, c.Forgejo, t, msg); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := c.Store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "done"); err != nil {
			errs = append(errs, err)
			continue
		}
		countOutcome(ctx, c.Metrics, c.Store, t, "done")
		log.Printf("chat budget: %s closed issue #%d of task %s", msg, t.IssueNumber, t.ID)
	}
	return errors.Join(errs...)
}

// Loop checks every interval until ctx ends.
func (c *ChatBudgetCards) Loop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := c.Check(ctx, time.Now()); err != nil {
				log.Printf("chat budget: %v", err)
			}
		}
	}
}
