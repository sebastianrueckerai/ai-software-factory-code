package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// SourceChatBudget is the source of the human-lane task filed when a
// project's chat budget is spent.
const SourceChatBudget = "chat_budget"

// ChatBudgetBook is what the chat budget's cards read and keep.
type ChatBudgetBook interface {
	CreateTask(ctx context.Context, t Task) error
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error
	ListTasksByStatus(ctx context.Context, status string) ([]Task, error)
	TransitionTask(ctx context.Context, id string, from []string, to string) error
	projectLookup
	taskReader
}

// ChatBudgetCards files the human card of a project's chat budget that is
// spent, once per month and budget, and closes it when a raise lifts the
// budget or the month ends. The card is on the project's repository, its
// Parent the chat task and its BudgetEUR the budget it was filed at.
type ChatBudgetCards struct {
	Store    ChatBudgetBook
	Forgejo  *Forgejo
	Timeline *Timeline
	Metrics  *telemetry.Metrics
}

// chatMonth is the UTC month of a chat task of project name: YYYY-MM.
func chatMonth(name, taskID string) string {
	return strings.TrimPrefix(taskID, "t-chat-"+name+"-")
}

func chatBudgetQuestion(budget float64, month string) HumanQuestion {
	return HumanQuestion{
		Ask:     fmt.Sprintf("The chat budget of %.2f EUR for %s is spent: raise it?", budget, month),
		Why:     "Rung 2 of the ladder answers no stopped card through the chat until the chat budget is raised or the UTC month turns, so every stop reaches Needs you.",
		Info:    "The action: raise the chat budget, with Fund and go on at the stop in the chat. The raise ends this card.",
		Choices: []QuestionChoice{{AnswerClose, "Decided, close"}},
	}
}

// File opens the card of project p's chat task chat at its budget, unless
// that card is open already.
func (c *ChatBudgetCards) File(ctx context.Context, p Project, chat Task) error {
	open, err := c.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return err
	}
	for _, t := range open {
		if t.Source == SourceChatBudget && t.Parent == chat.ID && t.BudgetEUR == chat.BudgetEUR {
			return nil
		}
	}
	t := Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard", Source: SourceChatBudget, Parent: chat.ID, BudgetEUR: chat.BudgetEUR, Author: c.Forgejo.BotUser}
	_, _, err = fileHumanTask(ctx, c.Store, c.Forgejo, c.Timeline, p.Repo(), chatBudgetQuestion(chat.BudgetEUR, chatMonth(p.Name, chat.ID)), nil, t, "")
	return err
}

// Check closes every open card whose chat task's budget was raised above
// the card's, or whose month ended before now. A card that fails to close
// does not stop the others; the failures are joined.
func (c *ChatBudgetCards) Check(ctx context.Context, now time.Time) error {
	open, err := c.Store.ListTasksByStatus(ctx, "waiting-human")
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range open {
		if t.Source != SourceChatBudget {
			continue
		}
		chat, ok, err := c.Store.Task(ctx, t.Parent)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		month := t.Parent[len(t.Parent)-len("2006-01"):]
		var msg string
		switch {
		case month < now.UTC().Format("2006-01"):
			msg = fmt.Sprintf("The UTC month %s ended: the card closes.", month)
		case ok && chat.BudgetEUR > t.BudgetEUR:
			msg = fmt.Sprintf("The chat budget is %.2f EUR for %s: the card closes.", chat.BudgetEUR, month)
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
