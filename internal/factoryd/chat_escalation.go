package factoryd

import (
	"context"
	"fmt"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// SourceChat is the source of a human-lane card the chat's agent filed
// as its escalation (roadmap 239).
const SourceChat = "chat"

// EscalateToHuman files an escalation on repo as the bot: a human-lane
// card of source in Needs you whose question carries its five parts (the
// question format), and the question_asked row the inbox's poke carries
// to the project's members. A SourceChat card is the chat agent's ask, by
// the member whose chat asked; any other source is factoryd's own, by
// the part of it that asks.
func EscalateToHuman(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
}, forgejo *Forgejo, tl *Timeline, repo Repo, q HumanQuestion, by, source string) (Task, error) {
	ask := strings.Join(strings.Fields(q.Ask), " ")
	title := "needs you: " + textutil.Truncate(ask, 80)
	var b strings.Builder
	b.WriteString(fenced(q.Ask))
	b.WriteString("\n\nWhy: " + q.Why)
	b.WriteString("\n\nInfo: " + q.Info)
	if q.Recommendation != "" {
		b.WriteString("\n\nRecommendation: " + q.Recommendation)
	}
	if len(q.Choices) > 0 {
		b.WriteString("\n\nChoices:\n")
		for _, c := range q.Choices {
			fmt.Fprintf(&b, "- %s\n", c.Label)
		}
	}
	via, asker := ViaChat, "agent"
	if source == SourceChat {
		fmt.Fprintf(&b, "\nThe project agent asked in %s's chat. Answer in the chat's inbox, or close this issue once decided.\n\nfiled by factoryd", by)
	} else {
		via, asker = ViaFactoryd, source
		fmt.Fprintf(&b, "\nAsked by %s. Answer on the card in the console or in the chat's inbox, or close this issue once decided.\n\nfiled by factoryd", by)
	}
	t := Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard", Objective: title + "\n\n" + b.String(), Source: source, Author: forgejo.BotUser}
	t, _, err := FileTask(ctx, store, forgejo, repo, title, b.String(), []string{"human"}, t)
	if err != nil {
		return Task{}, err
	}
	// the ask's durable anchor, the same row the runner's escalations
	// write: without it the card waits with its question on the chat alone
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeEscalation, Author: "factoryd",
		Body: q.Ask, Question: &q}); err != nil {
		return Task{}, fmt.Errorf("task %s: record the chat escalation: %w", t.ID, err)
	}
	asked := map[string]any{
		"question": textutil.Truncate(ask, 300),
		"ask":      q.Ask,
		"why":      q.Why,
		"info":     q.Info,
		"source":   asker,
		"by":       by,
	}
	if q.Recommendation != "" {
		asked["recommendation"] = q.Recommendation
	}
	if len(q.Choices) > 0 {
		asked["choices"] = q.Labels()
	}
	tl.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, via, asked)
	return t, nil
}
