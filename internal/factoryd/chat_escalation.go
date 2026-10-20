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
// card in Needs you whose question carries its five parts (the question
// format), and the question_asked row the inbox's poke carries to the
// project's members. card gives the card its source and commit. A
// SourceChat card is the chat agent's ask, by the member whose chat
// asked; any other source is factoryd's own, by the part of it that
// asks, and the waker wakes the project agent on it, a month cap's ask
// excepted.
func EscalateToHuman(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
}, forgejo *Forgejo, tl *Timeline, repo Repo, q HumanQuestion, by string, card Task) (Task, error) {
	via, asker := ViaChat, "agent"
	footer := fmt.Sprintf("The project agent asked in %s's chat. Answer in the chat's inbox, or close this issue once decided.", by)
	if card.Source != SourceChat {
		via, asker = ViaFactoryd, card.Source
		footer = fmt.Sprintf("factoryd asks (%s). Answer on the card in the console or in the chat's inbox, or close this issue once decided.", by)
	}
	title, body := askTitle(q), questionBody(q, footer)
	t := card
	t.ID, t.Kind, t.RiskClass, t.Objective, t.Author = NewTaskID(), "human", "standard", title+"\n\n"+body, forgejo.BotUser
	t, _, err := FileTask(ctx, store, forgejo, repo, title, body, []string{"human"}, t)
	if err != nil {
		return Task{}, err
	}
	if err := recordAsk(ctx, store, tl, t, q, by, asker, via); err != nil {
		return Task{}, err
	}
	return t, nil
}

// MaxAsk bounds a card's title, the ask itself, in bytes.
const MaxAsk = 200

// askTitle is the ask on one line, cut at MaxAsk only as a guard.
func askTitle(q HumanQuestion) string {
	return textutil.Truncate(strings.Join(strings.Fields(q.Ask), " "), MaxAsk-len("…"))
}

// questionBody is a human card's issue body: the question's five parts in
// order, then footer.
func questionBody(q HumanQuestion, footer string) string {
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
	return b.String() + "\n" + footer + "\n\nfiled by factoryd"
}

// recordAsk is the ask's durable anchor: the intake row the board and
// drawer read, and the question_asked row the inbox's poke carries. by is
// who asks, asker the source the wake reads.
func recordAsk(ctx context.Context, store interface {
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
}, tl *Timeline, t Task, q HumanQuestion, by, asker, via string) error {
	if _, err := store.InsertIntakeEvent(ctx, IntakeEvent{TaskID: t.ID, Kind: IntakeEscalation, Author: "factoryd",
		Body: q.Ask, Question: &q, Choices: q.Labels()}); err != nil {
		return fmt.Errorf("task %s: record the ask: %w", t.ID, err)
	}
	asked := map[string]any{
		"question": textutil.Truncate(strings.Join(strings.Fields(q.Ask), " "), 300),
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
	return nil
}
