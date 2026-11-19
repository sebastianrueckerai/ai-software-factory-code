package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// SourceChat is the source of a human-lane card the chat's agent filed
// as its escalation (roadmap 239).
const SourceChat = "chat"

// EscalateToHuman files an escalation on repo as the bot: a human-lane
// card whose question carries its five parts (the question format), and
// its question_asked row. card gives the card its source and commit. A
// SourceChat card is the chat agent's ask, by the human whose chat asked,
// in Needs you at once; any other source is factoryd's own, by the part
// of it that asks, and waits for its case's face (HumanCardStatus).
func EscalateToHuman(ctx context.Context, store humanCardStore, forgejo *Forgejo, repo Repo, q HumanQuestion, by string, card Task, labels ...string) (Task, error) {
	return EscalateToHumanWith(ctx, store, forgejo, repo, q, by, card, nil, labels...)
}

// EscalateToHumanWith is EscalateToHuman with facts, which the ask's
// question_asked row carries beside its own keys and which the ask's case
// holds as its facts.
func EscalateToHumanWith(ctx context.Context, store humanCardStore, forgejo *Forgejo, repo Repo, q HumanQuestion, by string, card Task, facts map[string]any, labels ...string) (Task, error) {
	via, asker := ViaChat, "agent"
	footer := fmt.Sprintf("The project agent asked in %s's chat. Answer in the chat's inbox, or close this issue once decided.", by)
	if card.Source != SourceChat {
		via, asker, footer = ViaFactoryd, card.Source, factorydFooter(by)
	}
	title, body := askTitle(q), questionBody(q, footer)
	t := card
	t.ID, t.RiskClass, t.Objective, t.Author = cmp.Or(card.ID, NewTaskID()), "standard", title+"\n\n"+body, forgejo.BotUser
	t, _, err := FileHumanCard(ctx, store, forgejo, repo, title, body, append([]string{"human"}, labels...), t, HumanAsk{Q: q, By: by, Asker: asker, Via: via, Facts: facts})
	return t, err
}

// factorydFooter closes the issue of a human card that who, a part of
// factoryd, asks with.
func factorydFooter(who string) string {
	return "factoryd asks (" + who + "). Answer on the card in the console or in the chat's inbox, or close this issue once decided."
}

// askAtOnce are the sources whose human card asks a human at its filing:
// the chat's escalate, whose agent worked the problem and chose to ask, and
// the month cap's and the judgment budget's cards, whose cap refuses the
// rungs' own calls.
var askAtOnce = []string{SourceChat, SourceMonthCap, SourceChatBudget}

// humanCardWaits are the statuses a human card waits in before it ends:
// for its case's face, and on a human.
var humanCardWaits = []string{"waiting-agent", "waiting-human"}

// HumanCardStatus is the status a human card of source is filed in:
// waiting-human for a source that asks at once, else waiting-agent, out of
// sight until its case's face files its issue and asks.
func HumanCardStatus(source string) string {
	if slices.Contains(askAtOnce, source) {
		return "waiting-human"
	}
	return "waiting-agent"
}

// humanCardStore is the store side of every human card's filing: the card's
// row and its ask row land in one transaction (CreateHumanCard).
type humanCardStore interface {
	CreateHumanCard(ctx context.Context, t Task, ask HumanAsk) (Task, bool, error)
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
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

// HumanAsk is the ask a human card is born with: the question_asked row the
// case, the board and the inbox's poke read. By is who asks, Asker the source
// the wake reads, Decider the poke's decider line ("" for none), Labels the
// issue's, which the face of a card filed with none puts on the issue it
// files.
type HumanAsk struct {
	Q                       HumanQuestion
	By, Asker, Via, Decider string
	Labels                  []string
	Facts                   map[string]any
}

// Event is the ask's question_asked row of card t: held while t waits for
// its case's face, so the case climbs and no one is poked or woken.
func (a HumanAsk) Event(t Task) Event {
	asked := map[string]any{}
	maps.Copy(asked, a.Facts)
	maps.Copy(asked, map[string]any{
		"question": textutil.Truncate(strings.Join(strings.Fields(a.Q.Ask), " "), 300),
		"ask":      a.Q.Ask,
		"why":      a.Q.Why,
		"info":     a.Q.Info,
		"source":   a.Asker,
		"by":       a.By,
	})
	if a.Q.Recommendation != "" {
		asked["recommendation"], asked["recommended_by"] = a.Q.Recommendation, cmp.Or(a.Q.Source, "the factory")
	}
	if len(a.Q.Choices) > 0 {
		asked["choices"], asked["offers"] = a.Q.Labels(), a.Q.Choices
	}
	if a.Decider != "" {
		asked["decider_line"] = a.Decider
	}
	if len(a.Labels) > 0 {
		asked["labels"] = a.Labels
	}
	if t.Status == "waiting-agent" {
		asked["held"] = true
	}
	return Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: a.Via, Detail: asked}
}
