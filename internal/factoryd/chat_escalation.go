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
// card in Needs you whose question carries its five parts (the question
// format), and the question_asked row the inbox's poke carries to the
// project's members. card gives the card its source and commit. A
// SourceChat card is the chat agent's ask, by the member whose chat
// asked; any other source is factoryd's own, by the part of it that
// asks, and the waker wakes the project agent on it, a month cap's ask
// excepted.
func EscalateToHuman(ctx context.Context, store humanCardStore, forgejo *Forgejo, repo Repo, q HumanQuestion, by string, card Task, labels ...string) (Task, error) {
	return EscalateToHumanWith(ctx, store, forgejo, repo, q, by, card, nil, labels...)
}

// EscalateToHumanWith is EscalateToHuman with facts, which the ask's
// question_asked row carries beside its own keys and which the ask's case
// holds as its facts.
func EscalateToHumanWith(ctx context.Context, store humanCardStore, forgejo *Forgejo, repo Repo, q HumanQuestion, by string, card Task, facts map[string]any, labels ...string) (Task, error) {
	via, asker := ViaChat, "agent"
	if card.Source != SourceChat {
		via, asker = ViaFactoryd, card.Source
	}
	b := HumanCardText(q, by, card.Source == SourceChat)
	t := card
	t.ID, t.RiskClass, t.Objective, t.Author = cmp.Or(card.ID, NewTaskID()), "standard", IssueObjective(b.Title, b.Body), forgejo.BotUser
	t, _, err := FileHumanCard(ctx, store, forgejo, repo, b, append([]string{"human"}, labels...), t, HumanAsk{Q: q, By: by, Asker: asker, Via: via, Facts: facts})
	return t, err
}

// humanCardStore is the store side of every human card's filing: the card's
// row and its ask row land in one transaction (CreateHumanCard).
type humanCardStore interface {
	CreateHumanCard(ctx context.Context, t Task, ask HumanAsk) (Task, bool, error)
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
}

// HumanCardText is the brief of the human card that asks q: the ask on
// one line as its title, the question's why as its why line, and as its
// text the question's other parts and the footer naming who asks: the
// project agent in by's chat when chat is set, else the factory.
func HumanCardText(q HumanQuestion, by string, chat bool) BriefText {
	footer := "The factory asks. Answer on the card in the console or in the chat's inbox, or close this issue once decided."
	if chat {
		footer = fmt.Sprintf("The project agent asked in %s's chat. Answer in the chat's inbox, or close this issue once decided.", by)
	}
	return BriefText{Title: askTitle(q), Why: q.Why, Body: questionBody(q, footer)}
}

// askTitle is the ask on one line.
func askTitle(q HumanQuestion) string {
	return oneLine(q.Ask)
}

// askQuestion is an ask as its row's question holds it: one line of at
// most 300 runes.
func askQuestion(ask string) string {
	return textutil.Truncate(strings.Join(strings.Fields(ask), " "), 300)
}

// Refaced is human card t once its live ask says q, with the facts that
// ask's case takes: the card's title and why line are q's ask and why, as
// HumanCardText files them, and its text stays as it was filed.
func Refaced(t Task, q HumanQuestion) (Task, map[string]any) {
	_, body, _ := strings.Cut(t.Objective, "\n\n")
	t.Objective, t.Why = IssueObjective(askTitle(q), body), q.Why
	return t, map[string]any{"ask": q.Ask, "question": askQuestion(q.Ask), "why": q.Why, "happened": q.Happened}
}

// questionBody is a human card's text: the question's parts in order but
// its why, which the issue's mirror leads with, then footer.
func questionBody(q HumanQuestion, footer string) string {
	var b strings.Builder
	b.WriteString(fenced(q.Ask))
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
	return b.String() + "\n" + footer + "\n\nFiled by the factory."
}

// HumanAsk is the ask a human card is born with: the question_asked row the
// case, the board and the inbox's poke read. By is who asks, Asker the source
// the wake reads, Decider the poke's decider line ("" for none).
type HumanAsk struct {
	Q                       HumanQuestion
	By, Asker, Via, Decider string
	Facts                   map[string]any
}

// Event is the ask's question_asked row of card t.
func (a HumanAsk) Event(t Task) Event {
	asked := map[string]any{}
	maps.Copy(asked, a.Facts)
	maps.Copy(asked, map[string]any{
		"question": askQuestion(a.Q.Ask),
		"ask":      a.Q.Ask,
		"why":      a.Q.Why,
		"info":     a.Q.Info,
		"source":   a.Asker,
		"by":       a.By,
	})
	for key, line := range map[string]string{"happened": a.Q.Happened, "waits": a.Q.Waits, "check": a.Q.Check} {
		if line != "" {
			asked[key] = line
		}
	}
	if a.Q.Recommendation != "" {
		asked["recommendation"], asked["recommended_by"] = a.Q.Recommendation, cmp.Or(a.Q.Source, "the factory")
	}
	if slices.ContainsFunc(a.Q.Choices, func(c QuestionChoice) bool { return c.Value == a.Q.Recommended && c.Value != "" }) {
		asked["preselected"] = map[string]any{"action": choicePrefix + a.Q.Recommended}
	}
	q := a.Q
	if len(q.Choices) == 1 && q.Choices[0].Value != AnswerPark {
		// no card has one button: the card's park joins a lone choice
		park, _ := ChoiceOf(ActNote + "/" + variantPark)
		q.Choices = append(slices.Clone(q.Choices), park)
	}
	if len(q.Choices) > 0 {
		asked["choices"], asked["offers"] = q.Labels(), q.Choices
	}
	if a.Decider != "" {
		asked["decider_line"] = a.Decider
	}
	return Event{Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: a.Via, Detail: asked}
}

// Question choice values a question's buttons post: close ends the card
// with the human's note, done says the human did the card's work, noted
// that the human read the notice, go resumes the card, and park and keep
// waiting record the answer and leave the card waiting on its human, no
// comment posted, because a posted comment would start the attempt the
// human declined to start.
const (
	AnswerPark        = "park"
	AnswerClose       = "close"
	AnswerDone        = "done"
	AnswerNoted       = "noted"
	AnswerKeepWaiting = "keep_waiting"
	AnswerGo          = "go"
)

// The marks a stop's reason carries where it names an owner decision:
// the deadlock headline (the repeat guard, workflow.go) and a refused
// review's line in the work state (workStateLines). A stop that carries
// one offers the decision as the exits.
const (
	DeadlockAskMark    = "No reply can change this"
	reviewConcernsMark = "review: concerns"
)

// HumanQuestion is the question a human card asks. Source names what
// backs the recommendation: the check-in's judgment, the agent's, or a
// factoryd rule; empty without one.
type HumanQuestion struct {
	Ask            string `json:"ask"`
	Why            string `json:"why"`
	Info           string `json:"info"`
	Recommendation string `json:"recommendation,omitempty"`
	// Recommended is the value of the choice Recommendation picks, ""
	// for none: the card preselects it.
	Recommended string           `json:"recommended,omitempty"`
	Source      string           `json:"source,omitempty"`
	Choices     []QuestionChoice `json:"choices,omitempty"`
	// Happened is what the factory saw, one line with its numbers; Waits
	// what waits on the card, one sentence; Check what the factory checks
	// once a human says the work is done, "" where it checks nothing.
	Happened string `json:"happened,omitempty"`
	Waits    string `json:"waits,omitempty"`
	Check    string `json:"check,omitempty"`
}

// The caps an escalation names.
const (
	CapSession = "session"
	CapTime    = "time"
)

// QuestionCap is the cap escalation names, "" for none.
func QuestionCap(escalation string) string {
	switch {
	case AtTimeStop(escalation):
		return CapTime
	case AtSessionCap(escalation):
		return CapSession
	}
	return ""
}

// QuestionChoice is one premade answer: the value the record carries and
// the label the person reads.
type QuestionChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ChoiceValue returns the value of the choice whose label words equal
// words up to case, surrounding space and end punctuation, "" when the
// question has no such choice.
func (q *HumanQuestion) ChoiceValue(words string) string {
	words = ChoiceKey(words)
	for _, c := range q.Choices {
		if ChoiceKey(c.Label) == words {
			return c.Value
		}
	}
	return ""
}

// ChoiceKey is the words of a choice label as ChoiceValue compares them:
// lower case, without surrounding space and end punctuation.
func ChoiceKey(s string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), ".!?,;: \t"))
}

// ChoiceLabel returns the label of the choice valued value, "" when the
// question has no such choice.
func (q *HumanQuestion) ChoiceLabel(value string) string {
	for _, c := range q.Choices {
		if c.Value == value {
			return c.Label
		}
	}
	return ""
}

// Offers reports whether the question has a choice valued value.
func (q *HumanQuestion) Offers(value string) bool {
	return q.ChoiceLabel(value) != ""
}

// Labels returns the choice labels, the words a click posts as the
// answer.
func (q *HumanQuestion) Labels() []string {
	out := make([]string, 0, len(q.Choices))
	for _, c := range q.Choices {
		out = append(out, c.Label)
	}
	return out
}

// The stall card's answers: re-run again re-runs the head's stuck runs
// and keeps the card, check again closes it and lets the watch run on,
// dismiss resolves the stall alert and closes it until the head changes.
const (
	AnswerRerunAgain = "rerun_again"
	AnswerCheckAgain = "check_again"
	AnswerDismiss    = "dismiss"
)

// AnswerRetryRoll is the failed-roll card's retry, whose work is RetryRoll.
const AnswerRetryRoll = "retry"
