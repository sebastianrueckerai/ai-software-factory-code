package factoryd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The answers that are no action of the table: a human card's own choices,
// a plan's decisions, a resource's, and the tools of every card.
const (
	AnswerSteer       = "steer"
	AnswerEditBrief   = "edit_brief"
	AnswerAccent      = "accent"
	AnswerThread      = "thread"
	AnswerConfirmPlan = "confirm"
	AnswerRevisePlan  = "revise"
	AnswerPlanChoice  = "plan_answer"
	AnswerApprove     = "approve"
	AnswerDeny        = "deny"
)

// The variants of the vocabulary: the form of an action for one class of
// card.
const (
	variantPark     = "park"
	variantDone     = "done"
	variantNoted    = "noted"
	variantDismiss  = "dismiss"
	variantPlanWait = "plan_wait"
	variantRoll     = "roll"
	variantOverrule = "overrule"
	variantOwnWords = "own_words"
	variantHeld     = "held"
	variantPlan     = "plan"
)

// noteField is the field of an answer that takes the human's note.
const noteField = "note"

func optionalNote(label string) Field {
	return Field{Name: noteField, Label: label, Kind: FieldText, Default: none}
}

func requiredNote(label, hint string) Field {
	return Field{Name: noteField, Label: label, Hint: hint, Kind: FieldText, Required: true}
}

func none(Facts) string { return "" }

func fixed(v string) func(Facts) string { return func(Facts) string { return v } }

// Vocabulary is every answer a face may offer: one row per action a human
// may pick, plus the named variants of a card class. No label exists
// outside it, but the words an agent or the planner offered.
var Vocabulary = []Answer{
	{Action: ActResume, Label: "Run one more session", Effect: "Starts the next session on the card's model."},
	{Action: ActContinue, Label: "Go on with my note", Effect: "Starts a session that reads your note first.",
		Fields: []Field{requiredNote("Your note", "What the session should know or do first")}},
	{Action: ActContinue, Variant: variantOwnWords, Label: "Answer in my words", Effect: "Sends your words to the agent as its answer.",
		Fields: []Field{requiredNote("Your answer", "The answer the agent needs")}},
	{Action: ActHandover, Label: "Switch the model", Effect: "Starts the next session on the model you pick.",
		Fields: []Field{{Name: "model", Label: "Model", Kind: FieldChoice, Default: fixed("stronger"),
			Options: []Option{{"stronger", "A stronger model"}, {"same", "The same model"}, {"cheaper", "A cheaper model"}}}}},
	{Action: ActReverify, Label: "Check the work again", Effect: "Runs the review and the checks again on the same code."},
	{Action: ActReverify, Variant: variantOverrule, Label: "Overrule the review", Effect: "Drops the review's findings and checks the work again.",
		Fields: []Field{requiredNote("Your reason", "Why the review's findings do not hold")}},
	{Action: ActRerunChecks, Label: "Run the failed checks again", Effect: "Runs the red checks again on the same code."},
	{Action: ActWaitChecks, Label: "Wait for the checks", Effect: "Waits once more for the checks. No session runs."},
	{Action: ActConfirmAudit, Label: "Clear the audit's findings", Effect: "Marks the audit's findings as safe test data and checks the work again.",
		Fields: []Field{{Name: "findings", Label: "Findings to clear, one per line, or none for all", Kind: FieldText, Default: none},
			requiredNote("Your reason", "Why the findings are safe")}},
	{Action: ActExtend, Label: "Allow more sessions", Effect: "Raises what the card may use.",
		Fields: []Field{{Name: "sessions", Label: "More sessions", Kind: FieldNumber, Default: extendSessions},
			{Name: "minutes", Label: "More minutes of agent time", Kind: FieldNumber, Default: extendMinutes},
			{Name: "lines", Label: "More changed lines", Kind: FieldNumber, Default: fixed("0")}}},
	{Action: ActFund, Label: "Add money", Effect: "Adds the amount to the card's budget and goes on.",
		Fields: []Field{{Name: "eur", Label: "EUR to add", Kind: FieldMoney, Default: func(f Facts) string { return money(shortfall(f.CaseFacts)) }}}},
	{Action: ActRaiseCeiling, Label: "Raise the spend limit", Effect: "Raises the most this card may ever spend.",
		Fields: []Field{{Name: "eur", Label: "EUR to raise the limit by", Kind: FieldMoney, Default: func(f Facts) string { return money(ceilingPass(f.CaseFacts)) }}}},
	{Action: ActSuccessor, Label: "Start a fresh card on this work", Effect: "Ends this card. A new card goes on from its branch with a full budget."},
	{Action: ActRetry, Label: "Try the failed card again", Effect: "Files the failed card again as a new card."},
	{Action: ActRetry, Variant: variantRoll, Label: "Try the roll again", Effect: "Starts the image roll again."},
	{Action: ActReplan, Label: "Send back to the planner", Effect: "The planner cuts this work into cards again.",
		Fields: []Field{optionalNote("A note for the planner")}},
	{Action: ActReplan, Variant: variantHeld, Label: "Send back to the planner", Effect: "The planner revises the plan that holds production."},
	{Action: ActClose, Label: "End the card", Effect: "Ends the card and closes its issue. Nothing more runs.",
		Fields: []Field{optionalNote("A note on why")}},
	{Action: ActClose, Variant: variantDone, Label: "I did it", Effect: "Closes the card. The factory runs no check of the work."},
	{Action: ActClose, Variant: variantNoted, Label: "Noted", Effect: "Ends this notice."},
	{Action: ActClose, Variant: variantDismiss, Label: "End the card", Effect: "Ends the card and stops watching the chain until main moves."},
	{Action: ActClose, Variant: variantPlanWait, Label: "End the failed card", Effect: "Ends the failed card. The plan goes on without it."},
	{Action: ActApproveMerge, Label: "Merge it", Effect: "Merges the pull request."},
	{Action: ActDenyMerge, Label: "Send it back", Effect: "Returns the pull request to its agent with your note.",
		Fields: []Field{requiredNote("What to change", "What the agent should change")}},
	{Action: ActDenyMerge, Variant: "reject", Label: "Reject and close it", Effect: "Closes the pull request. The card ends.",
		Fields: []Field{requiredNote("Your reason", "Why the change should not land")}},
	{Action: ActSetDependency, Label: "Make it wait for a card", Effect: "The card starts after the card you name ends.",
		Fields: []Field{{Name: "depends_on", Label: "The card to wait for", Hint: "a card number", Kind: FieldText, Required: true}}},
	{Action: ActRemoveDependency, Label: "Stop waiting for a card", Effect: "The card no longer waits for the card you name.",
		Fields: []Field{{Name: "depends_on", Label: "The card to stop waiting for", Kind: FieldText, Default: openDependency}}},
	{Action: ActFileCard, Label: "File the cause as its own card", Effect: "Files a new card for the cause. This card waits for it.",
		Fields: []Field{{Name: "title", Label: "Title", Hint: "The work, verb first", Kind: FieldText, Required: true},
			{Name: "why", Label: "Why", Hint: "The reason in one sentence", Kind: FieldText, Default: none},
			{Name: "summary", Label: "Summary", Hint: "What gets built and how it is checked", Kind: FieldText, Default: none},
			{Name: "body", Label: "Details", Kind: FieldText, Default: none},
			{Name: "size", Label: "Size", Kind: FieldChoice, Default: fixed("small"), Options: sizeOptions()},
			{Name: AsWritten, Label: "File it as written if the check refuses it", Kind: FieldCheck, Default: none}}},
	{Action: ActRestartPlan, Label: "Start the plan over", Effect: "Ends the plan and files it again with your note.",
		Fields: []Field{requiredNote("The plan's new shape", "What the new plan should do differently")}},
	{Action: ActAnswerCriteria, Label: "It is met", Effect: "Records the promise as met.",
		Fields: []Field{criterionField()}},
	{Action: ActAnswerCriteria, Variant: "not_met", Label: "It is not met", Effect: "Records the promise as not met. The plan stays open.",
		Fields: []Field{criterionField(), requiredNote("What is missing", "What the work lacks")}},
	{Action: ActAnswerCriteria, Variant: "waive", Label: "Drop this promise", Effect: "Takes the promise off the plan.",
		Fields: []Field{criterionField(), requiredNote("Your reason", "Why the plan no longer needs it")}},
	{Action: ActGrantCriteria, Label: "Build the missing part", Effect: "Files one more card for the open promises."},
	{Action: ActConfirmCriteria, Label: "The work is enough", Effect: "Records every open promise as met by the delivered cards.",
		Fields: []Field{optionalNote("What shows it")}},
	{Action: ActDenyCriteria, Label: "Leave the promises open", Effect: "Refuses the extra card. The plan stays open.",
		Fields: []Field{requiredNote("Your reason", "Why no card should be built")}},
	{Action: ActDeployNow, Label: "Deploy now", Effect: "Releases the held production deploy."},
	{Action: ActSettle, Label: "It is fine, go on", Effect: "Ends this notice. The card goes on."},
	{Action: ActSalvage, Label: "Rescue the work into a new card", Effect: "Ends the stuck run and starts a new card from its branch."},
	{Action: ActNote, Label: "Not now", Effect: "The card stays as it is.", Fields: []Field{optionalNote("A note")}},
	{Action: ActNote, Variant: variantPark, Label: "Not now", Effect: "The card stays as it is."},
	{Action: AnswerGo, Label: "Have the factory do it", Effect: "Files the work as a card for an agent."},
	{Action: AnswerRerunAgain, Label: "Run the chain again", Effect: "Runs the stalled deploy chain again."},
	{Action: AnswerCheckAgain, Label: "Look again", Effect: "Reads the chain's state again."},
	{Action: AnswerConfirmPlan, Label: "Confirm the plan", Effect: "The plan's cards start."},
	{Action: AnswerRevisePlan, Label: "Ask for changes", Effect: "The planner revises the plan with your note.",
		Fields: []Field{requiredNote("What to change", "What the plan should do differently")}},
	{Action: AnswerPlanChoice, Variant: variantOwnWords, Label: "Answer in my words", Effect: "Sends your words to the planner as the answer.",
		Fields: []Field{requiredNote("Your answer", "The answer the planner needs")}},
	{Action: AnswerApprove, Label: "Approve it", Effect: "Rents the resource at its price."},
	{Action: AnswerDeny, Label: "Refuse it", Effect: "The card goes on without the resource.",
		Fields: []Field{requiredNote("Your reason", "Why the card should do without it")}},
	{Action: AnswerSteer, Label: "Tell the running session", Effect: "The running session reads your message at its next step.",
		Fields: []Field{requiredNote("Your message", "What the session should know")}},
	{Action: AnswerEditBrief, Label: "Edit the brief", Effect: "Changes the title, why line and summary for the agent and the board."},
	{Action: AnswerAccent, Label: "Colour", Effect: "Marks the card with a colour."},
	{Action: AnswerThread, Label: "Talk about this card", Effect: "Opens a chat thread on this card."},
}

func criterionField() Field {
	return Field{Name: "criterion", Label: "The promise", Hint: "The promise's name", Kind: FieldText, Required: true}
}

func sizeOptions() []Option {
	out := []Option{}
	for _, s := range policy.CardSizes {
		out = append(out, Option{s, s})
	}
	return out
}

// extendSessions is the sessions an extend adds by default: one, none at
// an agent time cap, whose stop names time.
func extendSessions(f Facts) string {
	if f.Kind == CaseAgentTimeCap {
		return "0"
	}
	return "1"
}

// extendMinutes is the agent time an extend adds by default: one session's
// length at an agent time cap, none elsewhere.
func extendMinutes(f Facts) string {
	if f.Kind != CaseAgentTimeCap {
		return "0"
	}
	return fmt.Sprint(max(1, int(math.Ceil(f.SessionLeastS/60))))
}

// shortfall is what the card's next session needs past what a session may
// spend, in cents rounded up; 0 when it has the room.
func shortfall(f CaseFacts) float64 {
	if f.SessionLeft() >= f.FloorEUR {
		return 0
	}
	return fundFor(f.FloorEUR, f.SessionLeft())
}

// ceilingPass is what a fund of the shortfall takes the budget past the
// card's spend limit by, 0 when it stays under.
func ceilingPass(f CaseFacts) float64 {
	return max(0, float64(Cents(f.BudgetEUR)+Cents(shortfall(f))-Cents(f.CeilingEUR))/100)
}

func money(eur float64) string { return fmt.Sprintf("%.2f", eur) }

// openDependency is the one dependency the card still waits for, "" for none
// or several.
func openDependency(f Facts) string {
	var open []DepFact
	for _, d := range f.Dependencies {
		if !Final(d.Status) || d.Status == "failed" || d.Status == "closed" {
			open = append(open, d)
		}
	}
	if len(open) != 1 || open[0].Issue == 0 {
		return ""
	}
	return fmt.Sprintf("#%d", open[0].Issue)
}

// effectFills are the effect lines the facts write, by answer key.
var effectFills = map[string]func(Facts) string{
	ActNote: waitEffect, ActNote + "/" + variantPark: waitEffect,
	ActClose: func(f Facts) string {
		if f.Card.ID != "" && f.Card.IssueNumber == 0 {
			return "Ends the card. Nothing more runs."
		}
		return ""
	},
	ActClose + "/" + variantPlanWait: func(f Facts) string {
		if d := failedDep(f.CaseFacts); d != nil && d.Issue != 0 {
			return fmt.Sprintf("Ends #%d. The plan goes on without it.", d.Issue)
		}
		return ""
	},
	AnswerApprove: func(f Facts) string {
		r := resourceOf(f)
		if r.ServerType == "" {
			return ""
		}
		return fmt.Sprintf("Rents a %s server for %s at about %.2f EUR.", r.ServerType, DurationText(time.Duration(r.LeaseMinutes)*time.Minute), r.ReservationEUR)
	},
}

// waitEffect is "Not now"'s effect: at a cap, when the cap resets.
func waitEffect(f Facts) string {
	if when := capReset(f); when != "" {
		return "The card waits and goes on by itself when the limit resets on " + when + "."
	}
	return ""
}

// capReset is when the cap a card waits on resets, in words, "" for a card
// at no cap.
func capReset(f Facts) string {
	var decision string
	switch f.Kind {
	case CaseDayCap:
		decision = policy.RefusedDayCap
	case CaseMonthCap:
		decision = policy.RefusedMonthCap
	case CaseProjectCap:
		decision = policy.RefusedProjectCap
	default:
		return ""
	}
	return CapTurn(decision, time.Now()).UTC().Format("2 January at 15:04 UTC")
}

// CaseFaces are the faces of every case kind.
var CaseFaces = map[CaseKind]FaceSpec{}

// ApprovalFaces are the faces of every kind of approval request.
var ApprovalFaces = map[string]FaceSpec{}

// FlagFaces are the faces of every monitor flag.
var FlagFaces = map[string]FaceSpec{}

func init() {
	capFace := func(ask string) FaceSpec {
		return FaceSpec{
			Ask: func(f Facts) string {
				if f.Card.Kind == "human" {
					return humanFace.Ask(f)
				}
				return ask
			},
			Why:   capWhy,
			Story: capStory,
			Answers: func(f Facts) []string {
				if f.Card.Kind == "human" {
					return humanFace.Answers(f)
				}
				return []string{ActNote, ActClose}
			},
		}
	}
	work := func(s FaceSpec) FaceSpec {
		if s.Story == nil {
			s.Story = workStory
		}
		return s
	}
	limit := work(FaceSpec{Ask: limitAsk, Why: limitWhy, Answers: keys(ActExtend, ActSuccessor, ActClose, ActNote)})
	modelStop := func(ask, why string) FaceSpec {
		return work(FaceSpec{Ask: fixed(ask), Why: fixed(why), Answers: keys(ActResume, ActHandover, ActClose, ActNote)})
	}
	for kind, s := range map[CaseKind]FaceSpec{
		CaseSessionCap:   limit,
		CaseAgentTimeCap: limit,
		CaseBudgetFloor:  work(FaceSpec{Ask: budgetFaceAsk, Why: budgetWhy, Answers: keys(ActFund, ActSuccessor, ActClose, ActNote)}),
		CaseDayCap:       capFace("The factory's spending limit for today is reached. Wait for it to reset, or end the card?"),
		CaseMonthCap:     capFace("The factory's spending limit for this month is reached. Wait for it to reset, or end the card?"),
		CaseProjectCap:   capFace("This project's spending limit for the month is reached. Wait for it to reset, or end the card?"),
		CaseCheckinHuman: work(FaceSpec{Ask: fixed("A check during the session stopped the card for your decision. How should it go on?"),
			Why: reasonWhy("A check during the session found a question that only a human can settle."), Story: writerStory,
			Answers: keys(ActContinue, ActReplan, ActClose, ActNote)}),
		CasePolicyClass: work(FaceSpec{Ask: fixed("The change touches files the policy keeps with a human. Let the card go on with it?"),
			Why: reasonWhy("The policy keeps changes to these files with a human, so the card waits for your word."), Story: writerStory,
			Answers: keys(ActContinue, ActReplan, ActClose, ActNote)}),
		CaseCheckinShare: work(FaceSpec{Ask: fixed("The session spent more than its share of the card's money. Allow more, or end the card?"),
			Why: reasonWhy("A session may spend only a share of the card's money before a human looks."), Answers: keys(ActExtend, ActClose, ActNote)}),
		CaseCheckinFailed: work(FaceSpec{Ask: fixed("The factory's own check of the session failed, not the work. Run one more session?"),
			Why: fixed("The check that judges each session gave no answer, so the card stopped to be safe."), Answers: keys(ActResume, ActClose, ActNote)}),
		CaseGateStop: work(FaceSpec{Ask: gateAsk, Why: gateWhy, Story: gateStory, Answers: gateAnswers}),
		CaseFindingsRepeated: work(FaceSpec{Ask: fixed("The review keeps raising the same findings. Overrule it, or allow more sessions?"),
			Why: fixed("Another session alone will not change the review's verdict, so the card stopped."), Story: writerStory,
			Answers: keys(ActExtend, ActReverify+"/"+variantOverrule, ActSuccessor, ActClose)}),
		CaseDependencyFailed: work(FaceSpec{Ask: depFailedAsk, Why: depWhy, Answers: depFailedAnswers}),
		CaseDependencyClosed: work(FaceSpec{Ask: fixed("A card this one waits for was closed. Stop waiting for it, or send this card back?"),
			Why: depWhy, Answers: func(f Facts) []string { return append(holding(f, ActRemoveDependency), ActReplan, ActClose) }}),
		CaseWaitExpired: work(FaceSpec{Ask: fixed("The card has waited a long time for its author to answer. Keep waiting?"),
			Why: fixed("Nobody answered the card's question, so the card went quiet."), Answers: keys(ActNote, ActResume, ActClose)}),
		CasePodNeverStarted: modelStop("No machine could start the card's session. Try again, or end the card?",
			"Several sessions in a row waited for a machine with room, and no agent ran."),
		CaseProviderRefused: modelStop("The model provider refused the card's sessions. Try again, or switch the model?",
			"The provider turned down the first call of several sessions in a row."),
		CaseNoAnswer: modelStop("The model gave no answer in the last sessions. Try again, or switch the model?",
			"Several sessions in a row ended without an answer from the model."),
		CaseContentFilter: modelStop("The provider's content filter refused the session. Try again, or switch the model?",
			"The provider's filter blocked the session's request, so no work was done."),
		CaseImageWait: work(FaceSpec{Ask: fixed("The card waits for a tool its image does not have yet. Keep waiting?"),
			Why: fixed("The card needs a tool that another card is still adding to its image."), Answers: keys(ActNote, ActClose)}),
		CaseScreeningSuspect: work(FaceSpec{Ask: fixed("The screening found this issue unsafe for an agent. Let the agent work on it?"),
			Why: reasonWhy("A check of new issues found something an agent should not do without a human."), Story: writerStory,
			Answers: keys(ActContinue, ActClose)}),
		CaseAgentEscalate: work(FaceSpec{Ask: agentAsk("The agent asked a question it cannot answer alone. How should it go on?"),
			Why: func(f Facts) string {
				return cmp.Or(whyOf(agentLine(f, "Why")), "The agent stopped because it needs a decision only a human can make.")
			},
			Story:   agentStory,
			Answers: func(f Facts) []string { return append(sayKeys(f), ActNote, ActClose) },
			Suggest: func(f Facts) (string, string, string) {
				rec := agentLine(f, "Recommendation")
				if key := namedAnswer(rec, f, sayKeys(f)); key != "" {
					return key, "the agent", rec
				}
				return "", "", ""
			}}),
		CaseAgentRequestInfo: work(FaceSpec{Ask: agentAsk("The agent needs an answer to go on. What should it know?"),
			Why: fixed("The agent cannot go on without this answer."), Story: agentStory,
			Answers: keys(ActContinue+"/"+variantOwnWords, ActNote, ActClose)}),
		CasePlanQuestion:     planQuestionFace,
		CasePlanConfirmation: planConfirmFace,
		CaseMergeApproval:    mergeFace,
		CasePlanCriteria:     criteriaFace(false),
		CaseHumanCard:        humanFace,
		CaseJudgmentBudget:   humanFace,
		CaseRedMain:          humanFace,
		CaseRollHeld:         humanFace,
		CaseConfigRefused:    humanFace,
		CaseOther: work(FaceSpec{Ask: fixed("The card stopped on a problem it cannot fix by itself. Run one more session, or send it back?"),
			Why: reasonWhy("The card stopped on a problem that no session could fix."), Story: writerStory,
			Answers: keys(ActResume, ActReplan, ActClose, ActNote)}),
		CaseMonitorFlag: {Ask: fixed("The factory flagged something odd on this card. Is that fine?"),
			Why: fixed("The factory watches every card for signs that its work is stuck."), Answers: keys(ActSettle, ActNote)},
	} {
		CaseFaces[kind] = s
	}
	ApprovalFaces = map[string]FaceSpec{
		ApprovalMerge:         mergeFace,
		ApprovalResource:      resourceFace,
		ApprovalPlan:          planConfirmFace,
		ApprovalCriteria:      criteriaFace(false),
		ApprovalCriteriaRerun: criteriaFace(true),
	}
	FlagFaces = flagFaces()
}

func keys(k ...string) func(Facts) []string { return func(Facts) []string { return k } }

// holding are those of keys whose answers can happen on f: the answers a
// kind offers only in some of its states.
func holding(f Facts, keys ...string) []string {
	return slices.DeleteFunc(keys, func(key string) bool {
		a, ok := answerFor(key, f)
		return !ok || !answerHolds(a, f)
	})
}

// title is the card's title as its story names it.
func title(f Facts) string {
	if t := CardTitle(f.Card); t != "" {
		return endSentence(t)
	}
	return ""
}

// endSentence is s ending in a full stop, a question or an exclamation.
func endSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s[len(s)-1:], ".?!") {
		return s
	}
	return s + "."
}

// line is s as one story line: "" when it runs past the bound.
func line(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > storyMax {
		return ""
	}
	return s
}

// workStory is the story of a stop on a work card: what it set out to do,
// what its sessions spent, and what is left on its head.
func workStory(f Facts) Story {
	return Story{Planned: line(title(f)), Happened: line(spentLine(f)), Left: line(leftLine(f))}
}

// writerStory is workStory with the case writer's lines where it wrote
// them.
func writerStory(f Facts) Story {
	s := workStory(f)
	if h := line(f.Case.Brief); h != "" {
		s.Happened = h
	}
	if l := line(f.Case.BriefDetail); l != "" {
		s.Left = l
	}
	return s
}

func spentLine(f Facts) string {
	if f.Attempts == 0 {
		return fmt.Sprintf("No session ran yet. The card has %s EUR of budget.", money(f.BudgetEUR))
	}
	return fmt.Sprintf("It ran %s and spent %s of %s EUR.", plural(f.Attempts, "session"), money(f.SpentEUR), money(f.BudgetEUR))
}

func leftLine(f Facts) string {
	var parts []string
	if n := openFindings(f.CaseFacts); n > 0 {
		parts = append(parts, fmt.Sprintf("%s %s open", plural(n, "review finding"), isAre(n)))
	}
	switch f.Checks {
	case "red":
		parts = append(parts, "the checks are red")
	case "green":
		if len(parts) == 0 {
			parts = append(parts, "the checks are green")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return capitalize(strings.Join(parts, ", and ")) + "."
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// capitalize is s with its first letter up.
func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// openFindings is how many review and audit findings stand on the head.
func openFindings(f CaseFacts) int {
	n := 0
	for _, id := range f.Findings {
		if slices.Contains(f.ReviewFindings, id) || slices.Contains(f.AuditFindings, id) {
			n++
		}
	}
	return n
}

func limitAsk(f Facts) string {
	what := fmt.Sprintf("all %s", plural(f.SessionCap, "session"))
	if f.Kind == CaseAgentTimeCap {
		what = "its agent time of " + minutesWord(time.Duration(f.AgentTimeCapS)*time.Second)
	}
	if n := openFindings(f.CaseFacts); n > 0 {
		return fmt.Sprintf("The card used %s with %s open. Allow more, or end the card?", what, plural(n, "review finding"))
	}
	return fmt.Sprintf("The card used %s. Allow more, or end the card?", what)
}

func limitWhy(f Facts) string {
	if f.Kind == CaseAgentTimeCap {
		return "A card runs only for the agent time it was given, and this one has used it."
	}
	return fmt.Sprintf("A card runs only the sessions it was given, and this one has used all %d.", f.SessionCap)
}

func budgetFaceAsk(f Facts) string {
	need := money(shortfall(f.CaseFacts))
	if n := openFindings(f.CaseFacts); n > 0 {
		return fmt.Sprintf("Add %s EUR to fix the %s, or end the card?", need, plural(n, "open review finding"))
	}
	return fmt.Sprintf("The card is out of money. Add %s EUR, or end the card?", need)
}

func budgetWhy(f Facts) string {
	return fmt.Sprintf("The card spent %s of its %s EUR, and its next session needs %s EUR.", money(f.SpentEUR), money(f.BudgetEUR), money(f.FloorEUR))
}

// capWhy names the file and the key that set the limit.
func capWhy(f Facts) string {
	if f.Card.Kind == "human" {
		return humanFace.Why(f)
	}
	key, file := "budget.day_eur", "policy.yaml"
	switch f.Kind {
	case CaseMonthCap:
		key = "budget.month_eur"
	case CaseProjectCap:
		key, file = "budget.month_eur", cmp.Or(projectFile.FindString(f.CapWait+" "+factString(f.Case, "reason")), "the project's file")
	}
	return fmt.Sprintf("The limit is %s in %s, and the card goes on by itself when it resets.", key, file)
}

func capStory(f Facts) Story {
	if f.Card.Kind == "human" {
		return humanFace.Story(f)
	}
	s := workStory(f)
	s.Happened = "The limit stopped the card before its next session."
	if when := capReset(f); when != "" {
		s.Left = "The limit resets on " + when + "."
	}
	return s
}

// factString is fact key of case c as a string.
func factString(c Case, key string) string {
	s, _ := c.Facts[key].(string)
	return s
}

// reasonText is the stop's own words past its machine prefixes.
func reasonText(f Facts) string {
	r, _, _ := strings.Cut(cmp.Or(factString(f.Case, "reason"), f.Case.Problem), "\n")
	if m := gateDiagnosis.FindStringSubmatch(r); m != nil {
		r = m[1]
	}
	for _, p := range []string{"check-in: ", AgentAsks, screeningAsks} {
		r = strings.TrimPrefix(r, p)
	}
	if kind, rest, ok := strings.Cut(r, ": "); ok && !strings.Contains(kind, " ") {
		r = rest
	}
	return strings.TrimSpace(r)
}

// whyOf is s as a why line: one statement of at most 160 runes, the first
// letter up; "" when s is no such sentence or asks a question.
func whyOf(s string) string {
	s = endSentence(strings.Join(strings.Fields(s), " "))
	if s == "" || strings.HasSuffix(s, "?") {
		return ""
	}
	s = capitalize(s)
	if utf8.RuneCountInString(s) > whyMax || len(sentenceBreaks(s)) > 0 || strings.ContainsAny(s, ";—") {
		return ""
	}
	return s
}

// firstSentence is s up to the end of its first sentence.
func firstSentence(s string) string {
	if b := sentenceBreaks(s); len(b) > 0 {
		return s[:b[0][2]+1]
	}
	return s
}

// reasonWhy is a why line from the stop's own words, else fallback.
func reasonWhy(fallback string) func(Facts) string {
	return func(f Facts) string { return cmp.Or(whyOf(reasonText(f)), fallback) }
}

// gateDiagnosis reads what a gate stop found past its bound.
var gateDiagnosis = regexp.MustCompile(`past its bound of [^:]+: (.+)`)

// gateShape reads the gate a stop names and its try.
var gateShape = regexp.MustCompile(`stopped (\S+) \(attempt (\d+)\)`)

func gateAsk(f Facts) string {
	switch {
	case f.Gate == ActivityWaitChecks:
		return "The checks ran past their time limit. Wait for them once more, or end the card?"
	case reviewStop(f):
		return "The review keeps raising findings on this card. Overrule it, or allow more sessions?"
	}
	step := "a check of the work"
	if m := gateShape.FindStringSubmatch(factString(f.Case, "reason")); m != nil {
		step = "the " + strings.ReplaceAll(m[1], "_", " ")
	}
	return fmt.Sprintf("The work stopped at %s. Check it again, or tell the card how to go on?", step)
}

func gateWhy(f Facts) string {
	if f.Gate == ActivityWaitChecks {
		return "The checks did not finish in the time they were given, so the card stopped."
	}
	return reasonWhy("A step of the work did not finish in the time it was given.")(f)
}

// gateStory is a gate stop's story: the writer's, and at a checks wait the
// checks' state the stop found as what is left.
func gateStory(f Facts) Story {
	if f.Gate != ActivityWaitChecks {
		return writerStory(f)
	}
	s := workStory(f)
	if m := gateDiagnosis.FindStringSubmatch(cmp.Or(factString(f.Case, "face"), factString(f.Case, "reason"))); m != nil {
		if l := line(endSentence(capitalize(m[1]))); l != "" {
			s.Left = l
		}
	}
	return s
}

func reviewStop(f Facts) bool {
	return strings.Contains(factString(f.Case, "face")+factString(f.Case, "reason"), reviewConcernsMark)
}

func gateAnswers(f Facts) []string {
	switch {
	case f.Gate == ActivityWaitChecks:
		return slices.Concat([]string{ActWaitChecks}, holding(f, ActRerunChecks), []string{ActClose})
	case reviewStop(f):
		return []string{ActExtend, ActReverify + "/" + variantOverrule, ActSuccessor, ActClose}
	}
	return []string{ActReverify, ActContinue, ActReplan, ActClose}
}

// failedDep is the failed dependency with no replacement the card waits
// for, nil for none.
func failedDep(f CaseFacts) *DepFact {
	return tripDep(CaseDependencyFailed, f)
}

// planWait says the case is a plan's wait on its failed card.
func planWait(f Facts) bool { return PlanWaitCase(f.Case, f.Card) }

func depFailedAsk(f Facts) string {
	if planWait(f) {
		return "A card of this plan failed. Try it again, or end it so the plan goes on?"
	}
	return "A card this one waits for failed. Try it again, or stop waiting for it?"
}

func depFailedAnswers(f Facts) []string {
	if planWait(f) {
		return []string{ActRetry, ActClose + "/" + variantPlanWait}
	}
	return slices.Concat([]string{ActRetry}, holding(f, ActRemoveDependency), []string{ActReplan, ActClose})
}

func depWhy(f Facts) string {
	kind := CaseDependencyClosed
	if f.Kind == CaseDependencyFailed {
		kind = CaseDependencyFailed
	}
	d := tripDep(kind, f.CaseFacts)
	switch {
	case d == nil || d.Issue == 0:
		return "The card waits for another card that did not finish."
	case planWait(f):
		return fmt.Sprintf("The plan cannot close while #%d stays %s.", d.Issue, d.Status)
	}
	return fmt.Sprintf("This card waits for #%d, which ended %s.", d.Issue, d.Status)
}

// agentQuestion is the agent's question: the stop's first line.
func agentQuestion(f Facts) string {
	return strings.TrimSpace(EscalationQuestion(cmp.Or(factString(f.Case, "face"), factString(f.Case, "reason"), f.Case.Problem)))
}

// agentAsk is the agent's question when it reads as an ask, else fallback.
func agentAsk(fallback string) func(Facts) string {
	return func(f Facts) string {
		q := capitalize(agentQuestion(f))
		if q != "" && readsAsAsk(q) {
			return q
		}
		return fallback
	}
}

// agentStory names the agent's question where the ask could not.
func agentStory(f Facts) Story {
	s := workStory(f)
	q := capitalize(agentQuestion(f))
	if readsAsAsk(q) {
		return s
	}
	if h := line(endSentence(q)); h != "" {
		s.Happened = "The agent asked: " + h
		if utf8.RuneCountInString(s.Happened) > storyMax {
			s.Happened = h
		}
	}
	return s
}

// sayKeys are the keys of the answers the agent offered, at most three:
// words that read as an answer of the vocabulary are the vocabulary's, so
// the agent's "End the card" never stands for the card's own end.
func sayKeys(f Facts) []string {
	var out []string
	for _, c := range factStrings(f.Case, "choices") {
		c = strings.TrimSpace(c)
		if c == "" || len(out) == 3 || slices.ContainsFunc(Vocabulary, func(a Answer) bool { return ChoiceKey(a.Label) == ChoiceKey(c) }) {
			continue
		}
		out = append(out, sayPrefix+c)
	}
	return out
}

// agentLine is the line of the agent's stop that "key: " opens, "" for none.
func agentLine(f Facts, key string) string {
	for l := range strings.Lines(cmp.Or(factString(f.Case, "face"), factString(f.Case, "reason"))) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// namedAnswer is the key among keys of the answer whose label rec opens
// with as words of their own, the longest such label; "" for none.
func namedAnswer(rec string, f Facts, keys []string) string {
	rec = strings.ToLower(strings.TrimSpace(rec))
	best, most := "", 0
	for _, key := range keys {
		a, ok := answerFor(key, f)
		label := strings.ToLower(strings.TrimSpace(a.Label))
		if !ok || label == "" || len(label) <= most || !strings.HasPrefix(rec, label) {
			continue
		}
		if next, _ := utf8.DecodeRuneInString(rec[len(label):]); unicode.IsLetter(next) || unicode.IsDigit(next) {
			continue
		}
		best, most = key, len(label)
	}
	return best
}

func factStrings(c Case, key string) []string {
	var out []string
	switch raw := c.Facts[key].(type) {
	case []string:
		out = raw
	case []any:
		for _, x := range raw {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// readsAsAsk says words written for another reader pass as a face's ask.
func readsAsAsk(q string) bool {
	return len(CheckEdit(BriefText{Title: q}, BriefText{Title: q}, BriefAsk, noRefs{})) == 0
}

// noRefs resolves no reference: an ask names none.
type noRefs struct{}

func (noRefs) Issue(int) (string, string, bool) { return "", "", false }
func (noRefs) Files(string) []string            { return nil }
func (noRefs) Heading(string, string) bool      { return false }
func (noRefs) Err() error                       { return nil }

// planQuestionFace is a plan question's face: the planner's question, its
// choices and an answer in the human's words.
var planQuestionFace = FaceSpec{
	Ask: func(f Facts) string {
		q := capitalize(strings.TrimSpace(f.Case.Problem))
		if q != "" && readsAsAsk(q) {
			return q
		}
		return "The planner has a question about this plan. How should it go?"
	},
	Why: fixed("The planner needs your answer before it can finish the plan."),
	Answers: func(f Facts) []string {
		var out []string
		for _, c := range factStrings(f.Case, "choices") {
			if len(out) < 3 {
				out = append(out, planPrefix+c)
			}
		}
		return append(out, AnswerPlanChoice+"/"+variantOwnWords)
	},
	Suggest: func(f Facts) (string, string, string) {
		if r := factString(f.Case, "recommended"); r != "" {
			return planPrefix + r, "the planner", factString(f.Case, "grounds")
		}
		return "", "", ""
	},
}

var planConfirmFace = FaceSpec{
	Ask: func(f Facts) string {
		if f.Card.PlanName != "" && utf8.RuneCountInString(f.Card.PlanName) < 60 {
			return fmt.Sprintf("Confirm the plan %q so its cards can start?", f.Card.PlanName)
		}
		return "Confirm this plan so its cards can start?"
	},
	Why:     fixed("The plan's cards and promises are fixed only once a human confirms them."),
	Answers: keys(AnswerConfirmPlan, AnswerRevisePlan),
}

var mergeFace = FaceSpec{
	Ask: func(f Facts) string {
		if f.Checks == "red" {
			return "The checks of this card's pull request are red. Run them again, or send it back?"
		}
		return "Merge this card's pull request?"
	},
	Why: func(f Facts) string {
		if w := whyOf(firstSentence(factString(f.Case, "review_not_run"))); w != "" {
			return w
		}
		if class := factString(f.Case, "class"); class != "" {
			return fmt.Sprintf("The policy keeps changes of the %s risk class with a human, so the merge waits for you.", class)
		}
		return "The policy keeps this merge with a human."
	},
	Story: workStory,
	Answers: func(f Facts) []string {
		if f.Checks == "red" {
			return []string{ActRerunChecks, ActDenyMerge, ActDenyMerge + "/reject"}
		}
		return []string{ActApproveMerge, ActDenyMerge, ActDenyMerge + "/reject"}
	},
}

// criteriaNames are the promises a criteria request names.
func criteriaNames(f Facts) []string {
	_, list, ok := strings.Cut(f.Case.Problem, ": ")
	if !ok {
		return nil
	}
	return strings.Split(list, ", ")
}

func criteriaFace(rerun bool) FaceSpec {
	return FaceSpec{
		Ask: func(f Facts) string {
			n := len(criteriaNames(f))
			if f.ApprovalKind == ApprovalCriteriaRerun || rerun {
				return fmt.Sprintf("This plan finished, but no test checked its %s. Settle each one.", plural(n, "promise"))
			}
			return fmt.Sprintf("This plan finished, but %s not met. Build the missing part?", plural(n, "promise")+map[bool]string{true: " is", false: " are"}[n == 1])
		},
		Why: fixed("Until each promise is met or dropped, the plan stays open and holds production deploys."),
		Answers: func(f Facts) []string {
			if f.ApprovalKind == ApprovalCriteriaRerun || rerun {
				return []string{ActAnswerCriteria + "/waive", ActConfirmCriteria}
			}
			return []string{ActGrantCriteria, ActAnswerCriteria + "/waive", ActConfirmCriteria}
		},
	}
}

// resourceOf is the resource a request on the case asks for.
func resourceOf(f Facts) resourceFacts {
	var r resourceFacts
	if raw, err := json.Marshal(f.Case.Facts); err == nil {
		_ = json.Unmarshal(raw, &r)
	}
	return r
}

// resourceFacts are what a resource request's case holds of it.
type resourceFacts struct {
	ServerType     string  `json:"server_type"`
	Purpose        string  `json:"purpose"`
	LeaseMinutes   int     `json:"lease_minutes"`
	ReservationEUR float64 `json:"reservation_eur"`
}

var resourceFace = FaceSpec{
	Ask: func(f Facts) string {
		r := resourceOf(f)
		if r.ServerType == "" {
			return "Rent the server this card asks for?"
		}
		return fmt.Sprintf("Rent a %s server for %s?", r.ServerType, DurationText(time.Duration(r.LeaseMinutes)*time.Minute))
	},
	Why: func(f Facts) string {
		if p := resourceOf(f).Purpose; p != "" {
			return cmp.Or(whyOf("The card needs it for the "+p), "The card needs a server it may not rent without a human.")
		}
		return "The card needs a server it may not rent without a human."
	},
	Answers: keys(AnswerApprove, AnswerDeny),
}

// humanFace is the face of a card a human does or decides: its question as
// filed, and the choices it stores, each in the vocabulary's words.
var humanFace = FaceSpec{
	Ask: func(f Facts) string {
		q := strings.TrimSpace(cmp.Or(factString(f.Case, "ask"), f.Case.Problem))
		if q != "" && readsAsAsk(q) {
			return q
		}
		return "This work needs a human. Is it done?"
	},
	Why: func(f Facts) string {
		return cmp.Or(whyOf(factString(f.Case, "why")), whyOf(f.Card.Why), "Only a human can do this work.")
	},
	Story: func(f Facts) Story {
		return Story{Planned: line(title(f)), Happened: whyOf(factString(f.Case, "why"))}
	},
	Answers: humanAnswers,
	Suggest: func(f Facts) (string, string, string) {
		rec := factString(f.Case, "recommendation")
		if key := namedAnswer(rec, f, humanAnswers(f)); key != "" {
			return key, cmp.Or(factString(f.Case, "recommended_by"), "the factory"), rec
		}
		if keys := humanAnswers(f); len(keys) > 0 {
			return keys[0], "the factory", ""
		}
		return "", "", ""
	},
}

// humanAnswers are the keys of the choices a human card's ask stores.
func humanAnswers(f Facts) []string {
	var out []string
	for _, c := range storedChoices(f.Case) {
		out = append(out, choicePrefix+c.Value)
	}
	return out
}

// storedChoices are the choices the card's ask stores.
func storedChoices(c Case) []QuestionChoice {
	var out []QuestionChoice
	if raw, err := json.Marshal(c.Facts["offers"]); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return slices.DeleteFunc(out, func(q QuestionChoice) bool { return q.Value == "" })
}

// choiceAnswer is the answer of a human card's stored choice valued value:
// the vocabulary's row its label names, else the row its value routes to,
// else the card's own words.
func choiceAnswer(value string, f Facts) (Answer, bool) {
	i := slices.IndexFunc(storedChoices(f.Case), func(c QuestionChoice) bool { return c.Value == value })
	if i < 0 {
		return Answer{}, false
	}
	c := storedChoices(f.Case)[i]
	key := ""
	for _, a := range Vocabulary {
		if a.Label == c.Label && choiceValue(a) == c.Value {
			key = a.Key()
		}
	}
	switch v := c.Value; {
	case key != "":
	case v == AnswerPark, v == AnswerKeepWaiting, v == AnswerHoldWait:
		key = ActNote + "/" + variantPark
	case v == AnswerDismiss:
		key = ActClose + "/" + variantDismiss
	case v == AnswerRetryRoll && f.Card.Source == SourceRollFailed:
		key = ActRetry + "/" + variantRoll
	case strings.HasPrefix(v, AnswerReplanOnTop+":"), strings.HasPrefix(v, AnswerTakeOffMain+":"):
		key = ActReplan + "/" + variantHeld
	case v == AnswerClose && slices.Contains(informingSources, f.Card.Source):
		key = ActClose + "/" + variantNoted
	case v == AnswerClose && f.Card.Source != SourceRollFailed:
		key = ActClose + "/" + variantDone
	case v == AnswerClose, v == AnswerGo, v == AnswerRerunAgain, v == AnswerCheckAgain, v == ActDeployNow:
		key = v
	default:
		return Answer{Action: v, Label: c.Label, Effect: "Answers with these words. The card closes.", Value: v}, true
	}
	a, ok := answerFor(key, f)
	a.Value = value
	return a, ok
}

// informingSources are the human cards that tell and ask nothing to do.
var informingSources = []string{SourceAlert, SourceChatBudget, SourceMonthCap}

// choiceValue is the value a human card's choice of answer a posts.
func choiceValue(a Answer) string {
	switch a.Key() {
	case ActNote + "/" + variantPark:
		return AnswerPark
	case ActClose + "/" + variantDismiss:
		return AnswerDismiss
	case ActRetry + "/" + variantRoll:
		return AnswerRetryRoll
	}
	return a.Action
}

// ChoiceOf is the stored choice of the answer key names, as a human card's
// question offers it; ok is false for a key the vocabulary lacks.
func ChoiceOf(key string) (QuestionChoice, bool) {
	a, ok := AnswerOf(key)
	if !ok {
		return QuestionChoice{}, false
	}
	return QuestionChoice{Value: choiceValue(a), Label: a.Label}, true
}

// flagStory is a flag's story: a wait on dependencies names what each
// dependency waits on as what is left.
func flagStory(f Facts) Story {
	s := workStory(f)
	if factString(f.Case, "wait") != "dependency" {
		return s
	}
	var waits []string
	for _, d := range f.Dependencies {
		waits = append(waits, d.waitSentence())
	}
	if l := line(strings.Join(waits, " ")); l != "" {
		s.Left = l
	}
	return s
}

// waitSentence is what the card waits on in dependency d, which waits on a
// human's answer or holds its status since a time.
func (d DepFact) waitSentence() string {
	what := "card"
	if d.Plan {
		what = "plan"
	}
	s := fmt.Sprintf("The card waits on %s #%d, which is %s", what, d.Issue, d.Status)
	if d.Ask != "" {
		s = fmt.Sprintf("The card waits on %s #%d, which waits on a human to answer %q", what, d.Issue, d.Ask)
	}
	if d.Since != nil {
		s += " since " + d.Since.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return s + "."
}

// flagFace is one monitor flag's face: the ask with its fitting answer on
// the face, and without it where the answer cannot happen.
type flagFace struct {
	ask, askWithout, why, fitting string
}

func flagFaces() map[string]FaceSpec {
	const fine = "Is that fine?"
	out := map[string]FaceSpec{}
	for flag, ff := range map[string]flagFace{
		"answer_changed_nothing": {why: "An answer that changes nothing leaves the card where it was.",
			askWithout: "Your last answer changed nothing on this card. " + fine},
		"budget_moved_no_event": {why: "Every change of a budget should leave a record.",
			askWithout: "This card's budget changed and no record says why. " + fine},
		"checkin_cut": {why: "The check that judges each session ended before it gave a verdict.",
			askWithout: "The check of this card's session did not finish. " + fine},
		"checkin_refused": {why: "The check that judges each session was refused before it gave a verdict.",
			askWithout: "The check of this card's session did not run. " + fine},
		"criterion_unproven": {why: "A plan closes only once each promise has a test result.",
			askWithout: "A promise of the plan has no test result yet. " + fine},
		"dependency_failed": {why: "This card cannot start while a card it waits for stays failed.", fitting: ActRetry,
			ask: "A card this one waits for failed. Try it again?", askWithout: "A card this one waits for failed. " + fine},
		"finding_confirmed_twice": {why: "A finding the review raises twice may need a fresh start, not another session.",
			askWithout: "The review found the same fault twice on this card. " + fine},
		"grant_bought_nothing": {why: "The session you allowed pushed no change and moved no check.",
			askWithout: "The extra session you allowed changed nothing. " + fine},
		"issue_open_after_end": {why: "An ended card should not leave its issue open.", fitting: ActClose,
			ask: "The card ended and its issue is still open. Close it?", askWithout: "The card ended and its issue is still open. " + fine},
		"money_ask_hides_findings": {why: "Money spent over open findings may build on faulty work.",
			askWithout: "This card asks for money and has open review findings. " + fine},
		"opened_proof_changed": {why: "A plan's test should not change once the plan is confirmed.", fitting: ActReplan,
			ask: "A test of the plan changed or failed twice. Plan this part again?", askWithout: "A test of the plan changed or failed twice. " + fine},
		"opened_proof_red_twice": {why: "A test that fails twice may test the wrong thing.", fitting: ActReplan,
			ask: "A test of the plan changed or failed twice. Plan this part again?", askWithout: "A test of the plan changed or failed twice. " + fine},
		"proofs_missing": {why: "A plan's promises need tests before its cards finish.", fitting: ActReplan,
			ask: "The plan has no tests for its promises. Plan this part again?", askWithout: "The plan has no tests for its promises. " + fine},
		"prover_refused_thrice": {why: "The writer of the plan's tests refused three times.", fitting: ActReplan,
			ask: "The plan has no tests for its promises. Plan this part again?", askWithout: "The plan has no tests for its promises. " + fine},
		"queued_over_bound": {why: "A card should not wait in the queue this long.",
			askWithout: "This card has waited in the queue longer than it should. " + fine},
		"review_refused_not_run": {why: "A card should not go on without its review.",
			askWithout: "The review did not run on this card. " + fine},
		"session_no_push": {why: "A session that changes no code spends money for nothing.",
			askWithout: "The last session changed no code. " + fine},
		"wait_over_bound": {why: "The card has waited longer than its limit allows.", fitting: ActSalvage,
			ask: "This card's run went quiet. Rescue the work into a new card?", askWithout: "This card has waited longer than its limit allows. " + fine},
	} {
		out[flag] = FaceSpec{
			Ask: func(f Facts) string {
				if a, ok := answerFor(ff.fitting, f); ff.fitting != "" && ok && answerHolds(a, f) {
					return ff.ask
				}
				return ff.askWithout
			},
			Why:     fixed(ff.why),
			Story:   flagStory,
			Answers: func(f Facts) []string { return append(holding(f, ff.fitting), ActSettle, ActNote) },
			Suggest: func(f Facts) (string, string, string) {
				if a, ok := answerFor(ff.fitting, f); ff.fitting != "" && ok && answerHolds(a, f) {
					return ff.fitting, "the factory", ""
				}
				return ActSettle, "the factory", ""
			},
		}
	}
	return out
}

// StallQuestion is the stall card's question: what of repository's head
// at sha has been pending since when, the cards the stall holds, and the
// three answers.
func StallQuestion(repository, sha string, since time.Time, pending []string, cards []int64) HumanQuestion {
	held := "No merged card waits on the chain."
	if len(cards) > 0 {
		held = "The stall holds " + issueRefs(cards) + ": merged, they wait for a green chain to cover them."
	}
	b := SourceBriefs[SourceChainStall](BriefFacts{Name: repository})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The chain of %s has been pending at %s since %s UTC, past the stall bound: %s.",
			repository, sha[:min(7, len(sha))], since.UTC().Format("2006-01-02 15:04"), strings.Join(pending, "; ")) +
			"\n\n" + held + "\n\nRun the chain again runs the pending run and keeps this card. Look again closes it and lets the watch run on: " +
			"a head still pending a stall bound later asks again. End the card resolves the stall alert and asks nothing more until the head changes. " +
			"A head that concludes, green or red, closes this card by itself.",
		Choices: choices(AnswerRerunAgain, AnswerCheckAgain, ActClose+"/"+variantDismiss),
	}
}

// RepairQuestion is the human repair card of repository's red head at
// sha, on a project without agents: findings name each red run and red
// required context, runURL is the failing run's page ("" without one),
// cards the issues of the cards whose change the head carried.
func RepairQuestion(repository, sha string, findings []string, runURL string, cards []int64) HumanQuestion {
	short := sha[:min(7, len(sha))]
	carried := "No card's change rides this head; the chain alone is the work."
	if len(cards) > 0 {
		carried = "The head carries " + issueRefs(cards) + ": merged, they wait for a green chain to cover them; this card is their repair."
	}
	if runURL != "" {
		carried = "The run: " + runURL + "\n\n" + carried
	}
	b := SourceBriefs[SourceChainRepair](BriefFacts{Name: repository})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The chain of %s is red at %s: %s. The project runs no agents, so a human repairs it.", repository, sha, strings.Join(findings, "; ")) +
			"\n\n" + carried + "\n\nRead the run's log or the red status, find the cause, fix it from main's head, and push: the fix rides the chain again, and a green chain ends every wait. " +
			"A green head closes this card by itself, and so does a head that moves on. Closing it yourself dismisses the red at " + short + ": no new card asks until the head changes.",
		Choices: humanDoneChoices,
	}
}

// FactoryPRQuestion is the human card of the pull request at url, of
// brief b, which factoryd opened on a project without agents: no card's
// gates carry it, so a person merges it in Forgejo or closes it there.
func FactoryPRQuestion(b BriefText, url string) HumanQuestion {
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: "factoryd opened the pull request, and the project runs no agents: no card's gates check it, so a human merges it. " +
			"The pull request: " + url + "\n\nRead its change and merge it in Forgejo, or close it there if it should not land: " +
			"either ends this card.",
		Choices: humanDoneChoices,
	}
}

// AlertQuestion is the human card of alert a on project in namespace ns:
// why says why no agent takes it, body is the alert's write-up. An event's
// card (eventLabel) offers only the close, since no resolve clears it, and
// names the failed Job its labels carry.
func AlertQuestion(a SigNozAlert, project, ns, why, body string) HumanQuestion {
	b := alertBrief(a, ns)
	if a.Labels[eventLabel] == "true" {
		info := "The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card stays open until a human closes it."
		if job := a.Labels["k8s.object.name"]; dnsLabel.MatchString(job) {
			if cron := cronJobOf(job); cron != "" {
				info = fmt.Sprintf("The alert is an event, not a state: SigNoz's resolve only means the rule's window moved on, so this card closes when the CronJob `%s` completes a run, or when a human closes it.", cron)
			}
			info = fmt.Sprintf("The Job `%s` in namespace `%s` failed: read its pods' logs (`kubectl -n %s logs job/%s --all-containers`). ", job, ns, ns, job) + info
		}
		return HumanQuestion{
			Ask:     b.Title,
			Why:     b.Why,
			Info:    why + "\n\n" + info + "\n\n" + body,
			Choices: choices(ActClose+"/"+variantNoted, ActNote+"/"+variantPark),
		}
	}
	return HumanQuestion{
		Ask:     b.Title,
		Why:     b.Why,
		Info:    why + "\n\n" + body,
		Choices: choices(ActClose+"/"+variantNoted, ActNote+"/"+variantPark),
	}
}

// RollRefusedQuestion is the card of a green main CI run whose images the
// roller will not roll: lines name each unsigned image and why.
func RollRefusedQuestion(short string, names, lines []string, runURL string) HumanQuestion {
	b := SourceBriefs[SourceRollRefused](BriefFacts{Name: strings.Join(names, ", ")})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: "Main CI's run is green at " + short + ", and factoryd rolls none of its images: an image it rolls must carry a cosign signature by the factory's key (k8s/cosign.pub), and these do not." +
			"\n\nThe run: " + runURL + "\n\n- " + strings.Join(lines, "\n- ") + "\n\n" +
			"Main CI's image jobs sign each image they push. No signature means a job did not sign it or the tag was pushed outside main CI; " +
			"a signature by another key means the code repository's Actions secret COSIGN_PRIVATE_KEY is not the key k8s/cosign.pub holds. " +
			"Find which, fix it, and close this issue. The next green push run of main is judged again; an empty commit on main starts one.",
		Choices: humanDoneChoices,
	}
}

// RerunQuestion is the card of a red main whose push run index of commit
// sha failed only in jobs that lost their runner: failed names the jobs,
// lost holds their records, why says why factoryd's own re-run did not
// end it.
func RerunQuestion(index int64, sha, failed string, lost []string, why string) HumanQuestion {
	b := SourceBriefs[SourceRerun](BriefFacts{Name: failed})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("Main CI's push run %d of %s failed, but none of its steps did: each failed job lost its runner while it ran "+
			"(the runner killed or shut down, or its docker daemon restarted). No change to the code fixes that, so no agent works on this card. %s", index, sha, why) +
			"\n\n" + strings.Join(lost, "\n\n") + "\n\n" +
			"Re-run each job from its page. The project's merge queue is paused until a push run of main is green: the re-run's green " +
			"result ends the pause, and factoryd closes this issue.",
		Choices: humanDoneChoices,
	}
}

// StillRedQuestion is the human card of a red main whose remedy merged
// while main's push run index at sha, at link, is red still.
func StillRedQuestion(index int64, sha, remedy, link string) HumanQuestion {
	b := SourceBriefs[SourceStillRed](BriefFacts{})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("Main CI's push run %d of %s failed after %s merged. factoryd files one automatic remedy for a red main, "+
			"the fix-forward card or the revert after it, and main stayed red after it, so a human decides.", index, sha, remedy) +
			"\n\nThe run: " + link + "\n\nRead its log, then fix main or revert by hand. The project's merge queue is paused until a push run of " +
			"main is green: that run ends the pause, and factoryd closes this issue.",
		Choices: humanDoneChoices,
	}
}

// ConfigRejectedQuestion is the card of a configuration tree dir factoryd
// refused at commit short: stays says which version holds, link is the
// tree, reasons the loader's refusal.
func ConfigRejectedQuestion(dir, short, link, stays, reasons string) HumanQuestion {
	b := SourceBriefs[SourceConfig](BriefFacts{Name: dir + "/"})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The commit %s of %s/ was rejected. ", short, dir) + stays + "\n\nReasons (`factoryd config check " + dir + "` shows the same):\n\n" + fenced(reasons) + "\n\n" +
			"Fix the files and push; the next valid commit on main becomes current within 30 s and closes this issue. The tree: " + link,
		Choices: humanDoneChoices,
	}
}

// MonthCapCardQuestion is the board card of the month cap reached at eur
// of limit in month; refusal is the gateway's newest refusal, if any. When
// that refusal is LiteLLM's outer guard and eur is below limit, the guard
// fired under the cap and link is the file that sets it.
func MonthCapCardQuestion(limit float64, month string, eur float64, link, refusal string) HumanQuestion {
	if outerGuardRefused(refusal, eur, limit) {
		b := SourceBriefs[SourceMonthCap](BriefFacts{Name: fmt.Sprintf("%.2f EUR", limit), Of: fmt.Sprintf("%.2f EUR", eur)})
		return HumanQuestion{
			Ask: b.Title,
			Why: b.Why,
			Info: fmt.Sprintf("The factory's variable spend this month (%s, UTC) is %.2f EUR, below the cap of %.2f EUR (`budget.month_eur`), "+
				"yet LiteLLM's proxy-wide `max_budget`, the outer guard behind the cap, refused a call. Either `max_budget` sits under 1.25 times the cap, "+
				"or LiteLLM's USD counter ran ahead of the ledger in EUR. scripts/budget-backstop-check tells them apart: it fails when `max_budget` is under 1.25 times `budget.month_eur`; "+
				"if it passes, the counter ran ahead and `max_budget` needs the raise all the same.", month, eur, limit) +
				"\n\nRaise `max_budget` in k8s/platform/litellm/helmrelease.yaml, the LiteLLM helmrelease, which stays at 1.25 times the cap or more: " + link +
				"\n\nThe policy cap needs no change. Close this issue once decided.\n\nThe newest refusal:\n\n" + fenced(refusal),
			Choices: choices(ActNote+"/"+variantPark, ActClose+"/"+variantNoted),
		}
	}
	info := "The gateway refuses every further call with `refused_month_cap` until the month ends or the cap is raised; " +
		"a task it refused waits on its card and resumes on its own once the cap is raised or the month turns. Raising the cap is a commit to the `month_eur` line, " +
		"like any setting: " + link + "\n\nClose this issue once decided."
	if refusal != "" {
		info += "\n\nThe newest refusal:\n\n" + fenced(refusal)
	}
	b := SourceBriefs[SourceMonthCap](BriefFacts{Name: fmt.Sprintf("%.2f EUR", limit)})
	return HumanQuestion{
		Ask: b.Title,
		Why: b.Why,
		Info: fmt.Sprintf("The factory's variable spend in %s (UTC, over the cost ledger: model calls, paid tools, commissioned servers) is %.2f EUR of the cap of %.2f EUR (`budget.month_eur`). ", month, eur, limit) +
			info,
		Choices: choices(ActNote+"/"+variantPark, ActClose+"/"+variantNoted),
	}
}

// RoutineQuestion is the card a routine that files to a person puts: its
// title the ask, its brief and provenance the info.
func RoutineQuestion(rt policy.Routine, brief, provenance string, agents bool) HumanQuestion {
	b := SourceBriefs[SourceRoutine](BriefFacts{Name: oneLine(rt.Title), Of: rt.Name})
	return HumanQuestion{
		Ask:     b.Title,
		Why:     b.Why,
		Info:    brief + "\n\n" + provenance,
		Choices: humanWorkChoices(agents),
	}
}

// MovedQuestion is the card of an issue filed for an agent on a project
// that runs no agents now: the issue's title is the ask.
func MovedQuestion(title, body, poster string) HumanQuestion {
	q := IssueQuestion(title, body, poster, false)
	q.Why = "filed by " + poster + " for an agent, and the project runs no agents now"
	return q
}

// IssueQuestion is the card of an issue a person filed into the human
// lane: the issue's title is the ask.
func IssueQuestion(title, body, poster string, agents bool) HumanQuestion {
	return HumanQuestion{
		Ask:     oneLine(title),
		Why:     "filed by " + poster + " into the human lane",
		Info:    body,
		Choices: humanWorkChoices(agents),
	}
}

// humanDoneChoices are the answers of a card whose work a human does where
// no agent may: "I did it" and "Not now".
var humanDoneChoices = choices(ActClose+"/"+variantDone, ActNote+"/"+variantPark)

// humanWorkChoices are the answers of a card a human does: "I did it", the
// factory's hand-over only where the project runs agents, and "Not now".
func humanWorkChoices(agents bool) []QuestionChoice {
	keys := []string{ActClose + "/" + variantDone}
	if agents {
		keys = append(keys, AnswerGo)
	}
	return choices(append(keys, ActNote+"/"+variantPark)...)
}

// choices are the stored choices of the answers keys name.
func choices(keys ...string) []QuestionChoice {
	out := make([]QuestionChoice, len(keys))
	for i, k := range keys {
		c, ok := ChoiceOf(k)
		if !ok {
			panic("factoryd: the vocabulary has no answer " + k)
		}
		out[i] = c
	}
	return out
}
