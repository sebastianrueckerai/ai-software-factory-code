package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"maps"
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
	TransitionTask(ctx context.Context, id string, from []string, to string) error
}, forgejo *Forgejo, tl *Timeline, repo Repo, q HumanQuestion, by string, card Task, labels ...string) (Task, error) {
	return EscalateToHumanWith(ctx, store, forgejo, tl, repo, q, by, card, nil, labels...)
}

// EscalateToHumanWith is EscalateToHuman with facts, which the ask's
// question_asked row carries beside its own keys and which the ask's case
// holds as its facts.
func EscalateToHumanWith(ctx context.Context, store interface {
	CreateTask(ctx context.Context, t Task) error
	TransitionTask(ctx context.Context, id string, from []string, to string) error
}, forgejo *Forgejo, tl *Timeline, repo Repo, q HumanQuestion, by string, card Task, facts map[string]any, labels ...string) (Task, error) {
	via, asker := ViaChat, "agent"
	footer := fmt.Sprintf("The project agent asked in %s's chat. Answer in the chat's inbox, or close this issue once decided.", by)
	if card.Source != SourceChat {
		via, asker = ViaFactoryd, card.Source
		footer = fmt.Sprintf("factoryd asks (%s). Answer on the card in the console or in the chat's inbox, or close this issue once decided.", by)
	}
	title, body := askTitle(q), questionBody(q, footer)
	t := card
	t.ID, t.Kind, t.RiskClass, t.Objective, t.Author = cmp.Or(card.ID, NewTaskID()), "human", "standard", title+"\n\n"+body, forgejo.BotUser
	t, _, err := FileTask(ctx, store, forgejo, repo, title, body, append([]string{"human"}, labels...), t)
	if err != nil {
		return Task{}, err
	}
	if err := recordAsk(ctx, tl, t, q, by, asker, via, "", facts); err != nil {
		return Task{}, abandonUnasked(ctx, store, forgejo, t, err)
	}
	return t, nil
}

// abandonUnasked ends human card t, which waits on a human with no ask row,
// and closes its issue best effort, so no card waits without its ask's
// record. It answers cause.
func abandonUnasked(ctx context.Context, store interface {
	TransitionTask(ctx context.Context, id string, from []string, to string) error
}, forgejo *Forgejo, t Task, cause error) error {
	if err := store.TransitionTask(ctx, t.ID, []string{"waiting-human"}, "failed"); err != nil {
		log.Printf("task %s: end the card whose ask row failed: %v", t.ID, err)
	}
	if repo, err := SplitRepo(t.Repository); err == nil && forgejo != nil && t.IssueNumber != 0 {
		if err := forgejo.CloseIssue(ctx, repo.Owner, repo.Name, t.IssueNumber); err != nil {
			log.Printf("task %s: issue #%d stands without its ask: close it: %v", t.ID, t.IssueNumber, err)
		}
	}
	return cause
}

// MaxAsk bounds a card's title, the ask itself, in bytes.
const MaxAsk = 200

// askTitle is the ask on one line, cut at MaxAsk only as a guard.
func askTitle(q HumanQuestion) string {
	return askOf(q.Ask, "")
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

// recordAsk is the ask's record: the question_asked row the case, the board
// and the inbox's poke read. by is who asks, asker the source the wake reads,
// decider the poke's decider line ("" for none).
func recordAsk(ctx context.Context, tl *Timeline, t Task, q HumanQuestion, by, asker, via, decider string, facts map[string]any) error {
	asked := map[string]any{}
	maps.Copy(asked, facts)
	maps.Copy(asked, map[string]any{
		"question": textutil.Truncate(strings.Join(strings.Fields(q.Ask), " "), 300),
		"ask":      q.Ask,
		"why":      q.Why,
		"info":     q.Info,
		"source":   asker,
		"by":       by,
	})
	if q.Recommendation != "" {
		asked["recommendation"], asked["recommended_by"] = q.Recommendation, cmp.Or(q.Source, "the factory")
	}
	if len(q.Choices) > 0 {
		asked["choices"], asked["offers"] = q.Labels(), q.Choices
	}
	if decider != "" {
		asked["decider_line"] = decider
	}
	return tl.WriteRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, via, asked)
}
