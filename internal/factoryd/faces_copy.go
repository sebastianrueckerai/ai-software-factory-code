package factoryd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
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
	AnswerFundChat    = "fund_chat"
	AnswerSkipRun     = "skip_run"
	AnswerNeedsMe     = "needs_me"
	AnswerRunNow      = "run_now"
	AnswerKeepHoldout = "keep_holdout"
	AnswerDropHoldout = "drop_holdout"
	AnswerDropAndEnd  = "drop_promises"
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
	variantOffMain  = "off_main"
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
			Options: []Option{{"stronger", "A stronger model"}, {"same", "The same model"}, {"cheaper", "The cheapest model"}}}}},
	{Action: ActReverify, Label: "Check the work again", Effect: "Runs the review and the checks again on the same code."},
	{Action: ActReverify, Variant: variantOverrule, Label: "Overrule the review", Effect: "Drops the review's findings and checks the work again.",
		Fields: []Field{requiredNote("Your reason", "Why the review's findings do not hold")}},
	{Action: ActRerunChecks, Label: "Run the failed checks again", Effect: "Runs the red checks again on the same code."},
	{Action: ActWaitChecks, Label: "Wait for the checks", Effect: "Waits once more for the checks. No session runs."},
	{Action: ActConfirmAudit, Label: "Clear the audit's findings", Effect: "Marks the findings you tick as safe test data and checks the work again.",
		Fields: []Field{{Name: "findings", Label: "The findings to clear", Kind: FieldCheck, Required: true, Hint: "Tick at least one finding to clear"},
			requiredNote("Your reason", "Why the findings are safe")}},
	{Action: ActExtend, Label: "Allow more sessions", Effect: "Gives the card more sessions or agent time, and it goes on.",
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
	{Action: ActReplan, Variant: variantHeld, Label: "Send back to the planner", Effect: "The planner plans the plan again on top of what is merged."},
	{Action: ActReplan, Variant: variantOffMain, Label: "Take the plan off main", Effect: "Reverts the plan's merged cards, then the planner plans it again."},
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
	{Action: ActSettle, Label: "It is fine, go on", Effect: "Ends this notice. Nothing else changes."},
	{Action: ActSettle, Variant: variantNoted, Label: "Noted", Effect: "Ends this notice. Nothing else changes."},
	{Action: ActSalvage, Label: "Rescue the work into a new card", Effect: "Ends the card's run and starts a new card from its branch."},
	{Action: ActNote, Label: "Not now", Effect: "The card keeps waiting for your answer.", Fields: []Field{optionalNote("A note")}},
	{Action: ActNote, Variant: variantPark, Label: "Not now", Effect: "The card keeps waiting for your answer."},
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
	{Action: AnswerFundChat, Label: "Add money to the chat", Effect: "Raises the chat's budget for this month. The card closes.",
		Fields: []Field{{Name: "eur", Label: "EUR to add", Kind: FieldMoney, Default: func(f Facts) string { return money(f.Card.BudgetEUR) }}}},
	{Action: AnswerSkipRun, Label: "Skip the next run", Effect: "The routine misses its next run. Undo the skip reverses it."},
	{Action: AnswerNeedsMe, Label: "Give it to me", Effect: "The routine files its card for you."},
	{Action: AnswerRunNow, Label: "Run it now", Effect: "Files the routine's card now."},
	{Action: AnswerKeepHoldout, Label: "Keep it", Effect: "The promise joins the plan."},
	{Action: AnswerDropHoldout, Label: "Drop it", Effect: "The promise leaves the plan."},
	{Action: AnswerDropAndEnd, Label: "Drop them and close", Effect: "Drops the open promises and closes the plan.",
		Fields: []Field{requiredNote("Your reason", "Why the plan no longer needs them")}},
}

// auditFindings are the texts of the head's newest audit findings, one per
// line: the boxes of the findings to clear.
func auditFindings(f Facts) string {
	var out []string
	for _, id := range f.AuditFindings {
		if text := f.FindingTexts[id]; text != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, "\n")
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

// raiseFund is what a raise of the spend limit adds to the budget beside
// it: a card out of money gets the shortfall, so it goes on; 0 elsewhere.
func raiseFund(f CaseFacts) float64 {
	if f.Kind != CaseBudgetFloor {
		return 0
	}
	return shortfall(f)
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
	ActFund: func(f Facts) string {
		if slices.Contains(questionKinds, f.Case.Kind) {
			return "Adds the amount to the card's budget, and " + onward(f, "") + "."
		}
		return ""
	},
	ActRaiseCeiling: func(f Facts) string {
		if need := raiseFund(f.CaseFacts); need > 0 {
			return fmt.Sprintf("Raises the spend limit by %s EUR and adds %s EUR, and %s.", money(ceilingPass(f.CaseFacts)), money(need), onward(f, "the card goes on"))
		}
		return ""
	},
	ActNote: waitEffect, ActNote + "/" + variantPark: waitEffect,
	ActClose: func(f Facts) string {
		switch {
		case f.Kind == CaseMonitorFlag && f.Final != "":
			return "Closes the card's issue."
		case f.Card.ID != "" && f.Card.IssueNumber == 0:
			return "Ends the card. Nothing more runs."
		}
		return ""
	},
	ActClose + "/" + variantPlanWait: func(f Facts) string {
		d := failedDep(f.CaseFacts)
		switch {
		case d == nil:
			return ""
		case d.Title != "":
			return fmt.Sprintf("Ends %q. The plan goes on without it.", d.Title)
		case d.Issue != 0:
			return fmt.Sprintf("Ends #%d. The plan goes on without it.", d.Issue)
		}
		return ""
	},
	ActExtend: func(f Facts) string {
		if m := extendMinutes(f); m != "0" {
			return "Gives the card " + m + " more minutes of agent time, and " + onward(f, "it goes on") + "."
		}
		return "Gives the card one more session, and " + onward(f, "it goes on") + "."
	},
	ActReplan: func(f Facts) string {
		switch {
		case f.Planner:
			return "The planner plans this plan again."
		case f.Plan != "":
			return "The planner plans the whole plan again. Its started cards stop."
		}
		return ""
	},
	AnswerApprove: func(f Facts) string {
		r := resourceOf(f)
		if r.ServerType == "" {
			return ""
		}
		return fmt.Sprintf("Rents a %s server for %s at about %.2f EUR.", r.ServerType, spanWords(time.Duration(r.LeaseMinutes)*time.Minute), r.ReservationEUR)
	},
}

// waitEffect is "Not now"'s effect: at a cap, when the cap resets; on a
// notice, that it stays.
func waitEffect(f Facts) string {
	if when := capReset(f); when != "" && slices.Contains(questionKinds, f.Case.Kind) {
		return "The card waits, and the agent's question comes back to you when the limit resets on " + when + "."
	} else if when != "" {
		return "The card waits and goes on by itself when the limit resets on " + when + "."
	}
	if f.Kind == CaseMonitorFlag {
		return "The notice stays on the card. Nothing else changes."
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
	at := cmp.Or(f.Case.OpenedAt, time.Now())
	if slices.Contains(questionKinds, f.Case.Kind) {
		// the question opened before its cap bound: the cap is today's
		at = time.Now()
	}
	return CapTurn(decision, at).UTC().Format("2 January at 15:04 UTC")
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
		CaseBudgetFloor: work(FaceSpec{Ask: budgetFaceAsk, Why: budgetWhy,
			Answers: func(f Facts) []string {
				return append(holding(f, ActFund, ActRaiseCeiling), ActSuccessor, ActClose, ActNote)
			}}),
		CaseDayCap:     capFace("The factory's spending limit for today is reached. Wait for it to reset, or end the card?"),
		CaseMonthCap:   capFace("The factory's spending limit for this month is reached. Wait for it to reset, or end the card?"),
		CaseProjectCap: capFace("This project's spending limit for the month is reached. Wait for it to reset, or end the card?"),
		CaseCheckinHuman: work(FaceSpec{Ask: fixed("The session's check found a question only you can settle. Answer it, or send the card back to the planner?"),
			Why: reasonWhy("A check during the session found a question that only a human can settle."), Story: writerStory,
			Answers: keys(ActContinue, ActReplan, ActClose, ActNote)}),
		CasePolicyClass: work(FaceSpec{Ask: fixed("The change touches files only a human may approve. Let the card go on with it?"),
			Why: reasonWhy("The factory's rules let only a human approve a change to these files."), Story: writerStory,
			Answers: keys(ActContinue, ActReplan, ActClose, ActNote)}),
		CaseCheckinShare: work(FaceSpec{Ask: fixed("The last session spent more than its share of the card's money. Allow one more session, or end the card?"),
			Why: reasonWhy("A session may spend only a share of the card's money before a human looks."), Answers: keys(ActExtend, ActClose, ActNote)}),
		CaseCheckinFailed: work(FaceSpec{Ask: fixed("The factory's own check of the session failed, not the work. Run one more session?"),
			Why: fixed("The check that judges each session gave no answer, so the card stopped to be safe."), Answers: keys(ActResume, ActClose, ActNote)}),
		CaseGateStop: work(FaceSpec{Ask: gateAsk, Why: gateWhy, Story: gateStory, Answers: gateAnswers}),
		CaseFindingsRepeated: work(FaceSpec{Ask: fixed("The review keeps raising the same findings. Overrule it, or allow more sessions?"),
			Why: fixed("The review raised the same findings after each of the last sessions, so the card stopped."), Story: writerStory,
			Answers: keys(ActExtend, ActReverify+"/"+variantOverrule, ActSuccessor, ActClose)}),
		CaseDependencyFailed: work(FaceSpec{Ask: depFailedAsk, Why: depWhy, Answers: depFailedAnswers}),
		CaseDependencyClosed: work(FaceSpec{Ask: depClosedAsk,
			Why: depWhy, Answers: func(f Facts) []string { return append(holding(f, ActRemoveDependency), ActReplan, ActClose) }}),
		CaseWaitExpired: work(FaceSpec{Ask: fixed("The card's author has not answered its question. Go on without the answer, or end the card?"),
			Why: fixed("The card's question waited past its time limit without an answer."), Answers: keys(ActNote, ActResume, ActClose)}),
		CasePodNeverStarted: modelStop("No machine could start the card's session. Try again, or end the card?",
			"Several sessions in a row waited for a machine with room, and no agent ran."),
		CaseProviderRefused: modelStop("The model provider refused the card's sessions. Try again, or switch the model?",
			"The provider turned down the first call of several sessions in a row."),
		CaseNoAnswer: modelStop("The model gave no answer in the last sessions. Try again, or switch the model?",
			"Several sessions in a row ended without an answer from the model."),
		CaseContentFilter: modelStop("The provider's content filter refused the session. Try again, or switch the model?",
			"The provider's filter blocked the session's request, so no work was done."),
		CaseImageWait: work(FaceSpec{Ask: fixed("The card waits for another card to add a tool it needs. Let it wait, or end it?"),
			Why: fixed("The card needs a tool that another card is still installing for it."), Answers: keys(ActNote, ActClose)}),
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
				return agentPick(f), "the agent", agentLine(f, "Recommendation")
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
		CaseOther: work(FaceSpec{Ask: fixed("The card stopped on a problem it cannot fix by itself. Run one more session, or send it back to the planner?"),
			Why: reasonWhy("The card stopped for a reason the factory has no rule for."), Story: writerStory,
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
func writerStory(f Facts) Story { return writerLines(workStory(f), f.Case) }

// writerLines is story s with the lines case c's writer wrote over its own.
func writerLines(s Story, c Case) Story {
	if h := line(c.Brief); h != "" {
		s.Happened = h
	}
	if l := line(c.BriefDetail); l != "" {
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
	n, need := openFindings(f.CaseFacts), money(shortfall(f.CaseFacts))
	if ceilingPass(f.CaseFacts) > 0 {
		if n > 0 {
			return fmt.Sprintf("Raise the spend limit and add %s EUR to fix the %s, or end the card?", need, plural(n, "open review finding"))
		}
		return fmt.Sprintf("The card is out of money and at its spend limit. Raise the limit and add %s EUR, or end the card?", need)
	}
	if n > 0 {
		return fmt.Sprintf("Add %s EUR to fix the %s, or end the card?", need, plural(n, "open review finding"))
	}
	return fmt.Sprintf("The card is out of money. Add %s EUR, or end the card?", need)
}

// budgetWhy says what the card has left, what the review and the audit
// hold back of it, and what the next session needs: the sums of the ask.
func budgetWhy(f Facts) string {
	if f.ReserveEUR > 0 {
		return fmt.Sprintf("The card has %s EUR left, the review and the audit hold back %s EUR, and the next session needs %s EUR.",
			money(f.LeftEUR), money(f.ReserveEUR), money(f.FloorEUR))
	}
	return fmt.Sprintf("The card has %s EUR left, and the next session needs %s EUR.", money(f.LeftEUR), money(f.FloorEUR))
}

// capWhy names the limit and the file that sets it.
func capWhy(f Facts) string {
	if f.Card.Kind == "human" {
		return humanFace.Why(f)
	}
	limit, file := "The daily limit", "policy.yaml"
	switch f.Kind {
	case CaseMonthCap:
		limit = "The monthly limit"
	case CaseProjectCap:
		limit, file = "The project's monthly limit", cmp.Or(projectFile.FindString(f.CapWait+" "+factString(f.Case, "reason")), "the project's file")
	}
	return fmt.Sprintf("%s is set in %s, and %s when it resets.", limit, file, onward(f, "the card goes on by itself"))
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
	step := "A step of the work"
	if m := gateShape.FindStringSubmatch(factString(f.Case, "reason")); m != nil {
		step = cmp.Or(gateWords[m[1]], step)
	}
	return step + " ran past its time limit. Check the work again, or tell the card how to go on?"
}

// gateWords name each long gate's step in plain words.
var gateWords = map[string]string{ActivityRunSession: "The session", ActivityModelReview: "The review", ActivityGateAudit: "The audit",
	ActivityGateReproduction: "The reproduction check", ActivityGateCriteria: "The test of the plan's promises", ActivityMerge: "The merge"}

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
	switch retry, free := len(holding(f, ActRetry)) > 0, len(holding(f, ActRemoveDependency)) > 0; {
	case retry && free:
		return "A card this one waits for failed. Try it again, or stop waiting for it?"
	case retry:
		return "A card this one waits for failed. Try it again, or send this card back to the planner?"
	case free:
		return "A card this one waits for failed for good. Stop waiting for it, or send this card back to the planner?"
	}
	return "A card this one waits for failed for good. Send this card back to the planner, or end it?"
}

func depClosedAsk(f Facts) string {
	if len(holding(f, ActRemoveDependency)) > 0 {
		return "A card this one waits for was closed. Stop waiting for it, or send this card back to the planner?"
	}
	return "A card this one waits for was closed. Send this card back to the planner, or end it?"
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

// agentAsk is the agent's question when it reads as an ask, else its last
// sentence when that asks, else fallback.
func agentAsk(fallback string) func(Facts) string {
	return func(f Facts) string {
		_, q := agentParts(f)
		return cmp.Or(q, fallback)
	}
}

// agentParts are the agent's question as the face shows it: what it said
// before the sentence that asks, and that sentence, "" for a question no
// ask can show.
func agentParts(f Facts) (before, ask string) {
	q := capitalize(agentQuestion(f))
	if q != "" && readsAsAsk(q) {
		return "", q
	}
	if b := sentenceBreaks(q); len(b) > 0 {
		last := q[b[len(b)-1][3]:]
		if strings.HasSuffix(last, "?") && readsAsAsk(last) {
			return strings.TrimSpace(q[:b[len(b)-1][3]]), last
		}
	}
	return q, ""
}

// agentStory tells what the agent said before its question, or the
// question itself, whole, where the ask could not show it: the words are
// the agent's, and the face wraps them.
func agentStory(f Facts) Story {
	s := workStory(f)
	before, ask := agentParts(f)
	switch {
	case before == "":
	case ask != "":
		if h := line(firstSentence(before)); h != "" {
			s.Happened = endSentence(h)
		}
	default:
		s.Happened = AgentAsks + agentWhole(f)
	}
	return s
}

// agentWhole is the agent's question whole, on one line: the words are the
// agent's, and the face wraps them.
func agentWhole(f Facts) string {
	return endSentence(capitalize(strings.Join(strings.Fields(agentQuestion(f)), " ")))
}

// onward is what follows a draw on f: goes, or on the agent's question,
// which the draw leaves open, the question's return to the human.
func onward(f Facts, goes string) string {
	if slices.Contains(questionKinds, f.Case.Kind) {
		return "the agent's question comes back to you"
	}
	return goes
}

// agentPick is the key of the answer the agent picked through its
// escalate's choice, which the reason's Choice line numbers from 1; "" for
// none, or for a choice the face does not offer.
func agentPick(f Facts) string {
	n, err := strconv.Atoi(strings.Fields(agentLine(f, "Choice") + " 0")[0])
	if choices := factStrings(f.Case, "choices"); err == nil && n >= 1 && n <= len(choices) {
		if key := sayPrefix + strings.TrimSpace(choices[n-1]); slices.Contains(sayKeys(f), key) {
			return key
		}
	}
	return ""
}

// sayKeys are the keys of the answers the agent offered, at most three:
// words that read as an answer of the vocabulary are the vocabulary's, so
// the agent's "End the card" never stands for the card's own end, and words
// the guide faults, filed before their door checked them, show on no face.
func sayKeys(f Facts) []string {
	var out []string
	for _, c := range factStrings(f.Case, "choices") {
		c = strings.TrimSpace(c)
		if !offerable(c) || len(out) == 3 || slices.ContainsFunc(Vocabulary, func(a Answer) bool { return ChoiceKey(a.Label) == ChoiceKey(c) }) {
			continue
		}
		out = append(out, sayPrefix+c)
	}
	return out
}

// offerable says words a writer offered as an answer pass the guide's
// check of a button.
func offerable(words string) bool { return len(writingguide.ChoiceFaults(words)) == 0 }

// agentLine is the line of the agent's stop that "key: " opens, "" for none.
func agentLine(f Facts, key string) string {
	for l := range strings.Lines(cmp.Or(factString(f.Case, "face"), factString(f.Case, "reason"))) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+": "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
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
			if len(out) < 3 && offerable(c) {
				out = append(out, planPrefix+c)
			}
		}
		return append(out, AnswerPlanChoice+"/"+variantOwnWords)
	},
	Suggest: func(f Facts) (string, string, string) {
		if r := factString(f.Case, "recommended"); offerable(r) {
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
		if words, ok := classWords[factString(f.Case, "class")]; ok {
			return words + ", and the factory's rules let only a human merge it."
		}
		return "The factory's rules let only a human merge this change."
	},
	Story: workStory,
	Answers: func(f Facts) []string {
		if f.Checks == "red" {
			return []string{ActRerunChecks, ActDenyMerge, ActDenyMerge + "/reject"}
		}
		return []string{ActApproveMerge, ActDenyMerge, ActDenyMerge + "/reject"}
	},
}

// classWords say what a change of each risk class is, in plain words.
var classWords = map[string]string{policy.ClassNormal: "The change is of normal risk", policy.ClassHigh: "The change is high risk",
	"migration": "The change alters the database", policy.ClassRunner: "The change touches files the factory's own runner reads",
	policy.ClassLarge: "The change is large"}

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
			return fmt.Sprintf("This plan finished, but %s %s not met. Build the missing part?", plural(n, "promise"), isAre(n))
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
		return fmt.Sprintf("Rent a %s server for %s at about %.2f EUR?", r.ServerType, spanWords(time.Duration(r.LeaseMinutes)*time.Minute), r.ReservationEUR)
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
		if t := CardTitle(f.Card); t != "" {
			if q := "Is \u201c" + t + "\u201d done?"; readsAsAsk(q) {
				return q
			}
		}
		return "Is the work of this card done?"
	},
	Why: func(f Facts) string {
		return cmp.Or(whyOf(factString(f.Case, "why")), whyOf(f.Card.Why), "Only a human can do this work.")
	},
	Story: func(f Facts) Story {
		return Story{Planned: line(title(f)), Happened: whyOf(factString(f.Case, "why"))}
	},
	Answers: humanAnswers,
	Suggest: func(f Facts) (string, string, string) {
		return "", factString(f.Case, "recommended_by"), factString(f.Case, "recommendation")
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
	case strings.HasPrefix(v, AnswerReplanOnTop+":"):
		key = ActReplan + "/" + variantHeld
	case strings.HasPrefix(v, AnswerTakeOffMain+":"):
		key = ActReplan + "/" + variantOffMain
	case v == AnswerClose && slices.Contains(informingSources, f.Card.Source):
		key = ActClose + "/" + variantNoted
	case v == AnswerClose && f.Card.Source != SourceRollFailed:
		key = ActClose + "/" + variantDone
	case v == AnswerClose, v == AnswerGo, v == AnswerRerunAgain, v == AnswerCheckAgain, v == ActDeployNow:
		key = v
	case !offerable(c.Label):
		return Answer{}, false
	default:
		return Answer{Action: v, Label: c.Label, Effect: "Posts these words as the answer and closes the card.", Value: v}, true
	}
	a, ok := answerFor(key, f)
	a.Value = value
	if plan := planRef.FindString(c.Label); plan != "" && a.Action == ActReplan {
		a.Effect = strings.Replace(a.Effect, "the plan", "plan "+plan, 1)
	}
	return a, ok
}

// planRef finds the plan a held production's replan names in its label.
var planRef = regexp.MustCompile(`#\d+`)

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

// flagFace is one monitor flag's face: the fact it found on the card, why
// that matters, and the answer that fits it with the question it answers.
// A notice states its fact and asks nothing: it is noted, or kept for later.
type flagFace struct {
	fact         func(Facts) string
	why, fitting string
	question     string
	notice       bool
}

func flagFaces() map[string]FaceSpec {
	unjudged := "A check judges each session, and a session no check judged may have gone wrong unseen."
	out := map[string]FaceSpec{}
	for flag, ff := range map[string]flagFace{
		"answer_changed_nothing": {notice: true, fact: fixed("Your last answer on this card changed nothing."),
			why: "The card stopped again where it stopped before, and its other question asks how to go on."},
		"budget_moved_no_event": {fact: budgetMoved, why: "Every change of a card's budget should leave a record."},
		"checkin_cut":           {notice: true, fact: fixed("This card's last session went unjudged because its check ended early."), why: unjudged},
		"checkin_refused":       {notice: true, fact: fixed("This card's last session went unjudged because its check was refused."), why: unjudged},
		"criterion_unproven":    {fact: fixed("A promise of the plan has no test result yet."), why: "A plan closes only once each promise has a test result."},
		"dependency_failed": {fact: fixed("A card this one waits for failed."), why: "This card cannot start while a card it waits for stays failed.",
			fitting: ActRetry, question: "Try it again?"},
		"finding_confirmed_twice": {fact: fixed("The review found the same fault twice on this card."),
			why: "A fault the review finds twice may need a fresh start, not another session.", fitting: ActSalvage, question: "Rescue the work into a new card?"},
		"grant_bought_nothing": {notice: true, fact: fixed("This card's last session, which you allowed, changed nothing."),
			why: "The session pushed no change and moved no check, so another one may not help."},
		"issue_open_after_end": {fact: fixed("The card ended and its issue is still open."), why: "An ended card should not leave its issue open.",
			fitting: ActClose, question: "Close it?"},
		"opened_proof_changed": {fact: fixed("A test of the plan changed after the plan was confirmed."),
			why: "A plan's test should not change once the plan is confirmed.", fitting: ActReplan, question: "Plan this part again?"},
		"opened_proof_red_twice": {fact: fixed("A test of the plan failed on two versions of this card's work."),
			why: "A test that fails twice may test the wrong thing.", fitting: ActReplan, question: "Plan this part again?"},
		"proofs_missing": {fact: fixed("The plan has no tests for its promises."), why: "A plan's promises need tests before its cards finish.",
			fitting: ActReplan, question: "Plan this part again?"},
		"prover_refused_thrice": {fact: fixed("The plan has no tests for its promises."), why: "The writer of the plan's tests refused three times.",
			fitting: ActReplan, question: "Plan this part again?"},
		"queued_over_bound": {fact: waitedFact, why: "A card that waits this long in the queue may never get its turn."},
		"review_refused_not_run": {notice: true, fact: fixed("This card cannot merge yet because the review did not run on its newest work."),
			why: "The review must run on a card's newest work before the card merges."},
		"session_no_push": {notice: true, fact: fixed("This card's last sessions changed no code."),
			why: "A session that changes no code spends money and moves the work no closer to done."},
		"wait_over_bound": {fact: waitedFact, why: "A card that waits past its limit may be stuck.", fitting: ActSalvage, question: "Rescue the work into a new card?"},
	} {
		fits := func(f Facts) bool {
			a, ok := answerFor(ff.fitting, f)
			return ff.fitting != "" && ok && answerHolds(a, f)
		}
		out[flag] = FaceSpec{
			Ask: func(f Facts) string {
				switch {
				case ff.notice:
					return ff.fact(f)
				case fits(f):
					return ff.fact(f) + " " + ff.question
				}
				return ff.fact(f) + " Is that fine?"
			},
			Why:   fixed(ff.why),
			Story: flagStory,
			Answers: func(f Facts) []string {
				if ff.notice {
					return []string{ActSettle + "/" + variantNoted, ActNote}
				}
				return append(holding(f, ff.fitting), ActSettle, ActNote)
			},
		}
	}
	return out
}

// budgetMoved is the budget flag's fact: what the card's budget reads and
// what its records add up to.
func budgetMoved(f Facts) string {
	reads, records, ok := strings.Cut(factString(f.Case, "detail"), " not ")
	if !ok {
		return "This card's budget changed, and no record says why."
	}
	return fmt.Sprintf("This card's budget reads %s, and its records add up to %s.", reads, records)
}

// waitedFact is a wait flag's fact: what the card waits for, past its
// limit.
func waitedFact(f Facts) string {
	wait := factString(f.Case, "wait")
	if wait == "run_quiet" {
		return "This card's run has neither moved nor answered past its limit" + boundWords(f) + "."
	}
	what, ok := waitWords[wait]
	if !ok {
		what = "its turn"
	}
	return "This card has waited for " + what + " past its limit" + boundWords(f) + "."
}

// waitWords say what a card waits for in each kind of wait.
var waitWords = map[string]string{"queued": "a free place to run", "dependency": "the cards it depends on", "gate": "a check of its work",
	"merge_retry": "its merge to be tried again", "provider_backoff": "the model provider", "provider_recheck": "the model provider",
	"provider_limit": "the model provider", "rung_answer": "the factory's own answer", "checks": "its checks"}

// boundWords is " of <the wait's bound>" in words, "" when the case names
// none.
func boundWords(f Facts) string {
	d, err := time.ParseDuration(factString(f.Case, "bound"))
	if err != nil || d <= 0 {
		return ""
	}
	return " of " + spanWords(d)
}

// spanWords is d in hours and minutes, as words.
func spanWords(d time.Duration) string {
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return minutesWord(d)
	case m == 0:
		return plural(h, "hour")
	}
	return plural(h, "hour") + " and " + plural(m, "minute")
}
