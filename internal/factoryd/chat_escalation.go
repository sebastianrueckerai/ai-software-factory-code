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

// EscalateToHuman files the chat agent's escalation on repo as the bot:
// a human-lane card in Needs you with the question and its choices, and
// the question_asked row the inbox's poke carries to the project's
// members. by is the member whose chat asked.
func EscalateToHuman(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
}, forgejo *Forgejo, tl *Timeline, repo Repo, question string, choices []string, by string) (Task, error) {
	question = strings.TrimSpace(question)
	title := "needs you: " + textutil.Truncate(strings.Join(strings.Fields(question), " "), 80)
	var b strings.Builder
	b.WriteString(fenced(question))
	if len(choices) > 0 {
		b.WriteString("\n\nChoices:\n")
		for _, c := range choices {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	fmt.Fprintf(&b, "\nThe project agent asked in %s's chat. Answer in the chat's inbox, or close this issue once decided.\n\nfiled by factoryd", by)
	t := Task{ID: NewTaskID(), Kind: "human", RiskClass: "standard", Objective: title + "\n\n" + b.String(), Source: SourceChat, Author: forgejo.BotUser}
	t, _, err := FileTask(ctx, store, forgejo, repo, title, b.String(), []string{"human"}, t)
	if err != nil {
		return Task{}, err
	}
	asked := map[string]any{"question": textutil.Truncate(question, 300), "source": "agent", "by": by}
	if len(choices) > 0 {
		asked["choices"] = choices
	}
	tl.RecordRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, ViaChat, asked)
	return t, nil
}
