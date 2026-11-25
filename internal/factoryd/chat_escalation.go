package factoryd

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// SourceChat is the source of a human-lane card the chat's agent filed
// as its escalation (roadmap 239).
const SourceChat = "chat"

// FactAbout is the fact of a chat escalation's ask row, and so of its
// case, that names the task id of the card or plan the ask is about.
const FactAbout = "about"

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
	if card.Source != SourceChat {
		via, asker = ViaFactoryd, card.Source
	}
	b := HumanCardText(q, by, card.Source == SourceChat)
	t := card
	t.ID, t.RiskClass, t.Objective, t.Author = cmp.Or(card.ID, NewTaskID()), "standard", IssueObjective(b.Title, b.Body), forgejo.BotUser
	t, _, err := FileHumanCard(ctx, store, forgejo, repo, b, append([]string{"human"}, labels...), t, HumanAsk{Q: q, By: by, Asker: asker, Via: via, Facts: facts})
	return t, err
}

// askAtOnce are the sources whose human card asks a human at its filing:
// the chat's escalate, whose agent worked the problem and chose to ask, the
// month cap's and the judgment budget's cards, whose cap refuses the rungs'
// own calls, the provider freeze's card, whose limit may hold them, and the
// platform deaths' card, whose fix needs the nodes' admin key.
var askAtOnce = []string{SourceChat, SourceMonthCap, SourceChatBudget, SourceFreezeCard, SourcePlatformDeaths}

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
// HumanCardText files them, and its text stays as it was filed, so the
// ask's moments are q's and the card's own, which that text names.
func Refaced(t Task, q HumanQuestion) (Task, map[string]any) {
	_, body, _ := strings.Cut(t.Objective, "\n\n")
	t.Objective, t.Why = IssueObjective(askTitle(q), body), q.Why
	said := slices.Clone(q.Moments)
	for _, m := range t.Moments {
		said.Say(m.At, m.Layout)
	}
	return t, map[string]any{"ask": q.Ask, "question": askQuestion(q.Ask), "why": q.Why, "happened": q.Happened, "moments": said}
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
	if q.Act != "" {
		b.WriteString("\n\nAct:\n\n" + fenced(q.Act))
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
		"question": askQuestion(a.Q.Ask),
		"ask":      a.Q.Ask,
		"why":      a.Q.Why,
		"info":     a.Q.Info,
		"source":   a.Asker,
		"by":       a.By,
	})
	for key, line := range map[string]string{"happened": a.Q.Happened, "waits": a.Q.Waits, "check": a.Q.Check, "act": a.Q.Act} {
		if line != "" {
			asked[key] = line
		}
	}
	if len(a.Q.Moments) > 0 {
		asked["moments"] = a.Q.Moments
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
		park, _ := ChoiceOf(KeyPark)
		q.Choices = append(slices.Clone(q.Choices), park)
	}
	if len(q.Choices) > 0 {
		asked["choices"], asked["offers"] = q.Labels(), q.Choices
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
	// Act is the exact command or press a human does outside the factory,
	// "" for none: the card shows it as its own block.
	Act string `json:"act,omitempty"`
	// Moments are the instants its texts name, which a screen shows in
	// the reader's zone.
	Moments Moments `json:"moments,omitempty"`
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

// AnswerRetryFollow is the failure card's retry of a follow move, whose work
// is RetryFollow.
const AnswerRetryFollow = "retry_follow"

// factWaitsOn is the fact of a card's case that waits on the human card
// whose task id it holds (waitOnAsk).
const factWaitsOn = "waits_on"

// askRecheck is how often rung 0 reads again the ask a held case waits on.
const askRecheck = time.Minute

// waitOnAsk is rung 0's hold of case c, a stop on which its card would ask
// a human: while a human card the chat filed about the card or its plan
// asks a human (openAskAbout), the card asks no second time. It waits on
// that card as on a dependency: in waiting-deps with its edge to it, on
// the issues too, holding no slot, and the case keeps a wake at rung 0.
// Once that card's ask ended, the edge goes and rung 0 resumes the card,
// the answer its note (askEnd), within the case's hold, so a resume Act
// refuses hands the case up with why. A card that does not run takes no
// wait.
func (r *Runner) waitOnAsk(ctx context.Context, c Case) (bool, error) {
	t, repo, err := r.task(ctx, c.TaskID)
	if err != nil {
		return false, err
	}
	hold := func() (bool, error) {
		until := r.Clock().Add(askRecheck)
		return true, r.store.SetCaseWake(ctx, c.ID, 0, c.WakeOn, &until)
	}
	if id, _ := c.Facts[factWaitsOn].(string); id != "" {
		h, _, err := r.task(ctx, id)
		if err != nil {
			return false, err
		}
		note, over, err := r.askEnd(ctx, h)
		switch {
		case err != nil:
			return false, err
		case !over:
			return hold()
		}
		if err := r.store.RemoveTaskDeps(ctx, t.ID, []string{h.ID}); err != nil {
			return false, err
		}
		if err := r.store.SetCaseFact(ctx, c.ID, factWaitsOn, ""); err != nil {
			return false, err
		}
		if err := r.forgejo.SetIssueDependency(ctx, repo, t.IssueNumber, h.IssueNumber, true); err != nil {
			log.Printf("task %s: issue #%d no longer on #%d: %s", t.ID, t.IssueNumber, h.IssueNumber, maskedErr(err))
		}
		r.record(ctx, t.ID, "dependencies_done", gateEnd(t, GateDependencies, map[string]any{"case": c.ID}))
		if _, err := r.Act(ctx, ActInput{TaskID: t.ID, CaseID: c.ID, By: FactoryDecider, Via: ViaFactoryd, Rung: 0,
			Res: Resolution{Action: ActResume, Note: note}}); err != nil {
			return false, fmt.Errorf("%s, and the factory's resume was refused: %w", note, err)
		}
		return true, nil
	}
	if t.Status != "running" {
		return false, nil
	}
	h, ok, err := r.openAskAbout(ctx, t)
	if err != nil || !ok {
		return false, err
	}
	if err := r.store.TransitionTask(ctx, t.ID, []string{"running"}, "waiting-deps"); err != nil {
		return false, err
	}
	if err := r.store.AddTaskDeps(ctx, t.ID, []string{h.ID}); err != nil {
		return false, err
	}
	if err := r.store.SetCaseFact(ctx, c.ID, factWaitsOn, h.ID); err != nil {
		return false, err
	}
	if err := r.store.SetGate(ctx, t.ID, GateDependencies, r.Clock()); err != nil {
		return false, err
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-deps")
	if err := r.forgejo.SetIssueDependency(ctx, repo, t.IssueNumber, h.IssueNumber, false); err != nil {
		log.Printf("task %s: issue #%d on #%d: %s", t.ID, t.IssueNumber, h.IssueNumber, maskedErr(err))
	}
	r.record(ctx, t.ID, "dependencies_wait", map[string]any{"on": []int64{h.IssueNumber}, "case": c.ID})
	pullQueued(ctx, r.store, r.cfg.Workflows, t.Repository)
	return hold()
}

// openAskAbout is the oldest human card the chat filed about card t, about
// its plan or about another card of its plan, whose ask waits on a human.
func (r *Runner) openAskAbout(ctx context.Context, t Task) (Task, bool, error) {
	plan := ""
	switch {
	case t.PlanCard():
		plan = t.Parent
	case t.Track == policy.TrackPlan:
		plan = t.ID
	}
	cases, err := r.store.OpenCasesIn(ctx, t.Repository)
	if err != nil {
		return Task{}, false, err
	}
	for _, c := range cases {
		about, _ := c.Facts[FactAbout].(string)
		if c.Kind != CaseHumanCard || c.Status != CaseAtHuman || about == "" {
			continue
		}
		same := about == t.ID || plan != "" && about == plan
		if !same && plan != "" {
			a, ok, err := r.store.Task(ctx, about)
			if err != nil {
				return Task{}, false, err
			}
			same = ok && a.PlanCard() && a.Parent == plan
		}
		if !same {
			continue
		}
		h, ok, err := r.store.Task(ctx, c.TaskID)
		if err != nil {
			return Task{}, false, err
		}
		if ok && h.Source == SourceChat && h.Status == "waiting-human" {
			return h, true, nil
		}
	}
	return Task{}, false, nil
}

// askEnd says whether the ask of human card h ended, and the note a card
// that waited on it goes on with: who answered it and their words, or that
// it ended with no answer.
func (r *Runner) askEnd(ctx context.Context, h Task) (note string, over bool, err error) {
	cases, err := r.store.CasesOf(ctx, h.ID)
	if err != nil {
		return "", false, err
	}
	var ask Case
	for _, c := range cases {
		if c.Kind == CaseHumanCard && c.ID > ask.ID {
			ask = c
		}
	}
	if (ask.Status == CaseOpen || ask.Status == CaseAtHuman) && !Final(h.Status) {
		return "", false, nil
	}
	name := fmt.Sprintf("#%d “%s”", h.IssueNumber, CardTitle(h))
	if res := ask.Resolution; res != nil {
		if words := cmp.Or(strings.TrimSpace(res.Reason), LabelOf(res.Action)); words != "" {
			return fmt.Sprintf("%s answered %s, which this card waited on: %s", DeciderWords(res.By), name, words), true, nil
		}
	}
	return name + ", which this card waited on, ended with no answer.", true, nil
}
