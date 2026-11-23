package factoryd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// The answers that are no action of the table: a human card's own choices,
// a plan's decisions, a resource's, and the tools of every card.
const (
	AnswerSteer       = "steer"
	AnswerEditBrief   = "edit_brief"
	AnswerAccent      = "accent"
	AnswerThread      = "thread"
	AnswerHold        = "hold_card"
	AnswerRelease     = "release_hold"
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
	AnswerClosePlan   = "close_plan"
)

// KeyPark is the key of the Not now with no note field.
const KeyPark = ActNote + "/" + variantPark

// The variants of the vocabulary: the form of an action for one class of
// card.
const (
	variantPark     = "park"
	variantDone     = "done"
	variantNoted    = "noted"
	variantDismiss  = "dismiss"
	variantPlanWait = "plan_wait"
	variantRoll     = "roll"
	variantThaw     = "thaw"
	variantOverrule = "overrule"
	variantOwnWords = "own_words"
	variantHeld     = "held"
	variantOffMain  = "off_main"
	variantPlan     = "plan"
	variantKeepOpen = "keep_open"
	variantIssue    = "issue"
	variantUndo     = "undo"
	variantAlone    = "alone"
	variantFile     = "file"
	variantOpen     = "open"
	variantAgain    = "again"
)

// The keys of a routine's answers in their other states: the undo of a
// skip, and the two other sides of who its card is for.
const (
	KeyUndoSkip   = AnswerSkipRun + "/" + variantUndo
	KeyRunAlone   = AnswerNeedsMe + "/" + variantAlone
	KeyFollowFile = AnswerNeedsMe + "/" + variantFile
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
	{Action: ActExtend, Label: "Give more room", Effect: "Gives the card more sessions' worth of time and money, and it goes on.",
		Fields: []Field{{Name: "sessions", Label: "Sessions' worth", Kind: FieldNumber, Default: roomSessions, Min: 1, Max: MaxRoomSessions},
			{Name: "money", Label: "Money", Kind: FieldCheck, Default: roomMoney}}},
	{Action: ActSuccessor, Label: "Start a fresh card on this work", Effect: "Ends this card. A new card goes on from its branch with a full budget."},
	{Action: ActRetry, Label: "Try the failed card again", Effect: "Files the failed card again as a new card."},
	{Action: ActRetry, Variant: variantRoll, Label: "Try the roll again", Effect: "Starts the image roll again."},
	{Action: ActRetry, Variant: variantThaw, Label: "Retry now", Effect: "Ends the freeze now. The waiting cards call the provider again, and a new limit makes them wait until its reset."},
	{Action: ActReplan, Label: "Send back to the planner", Effect: "The planner cuts this work into cards again.",
		Fields: []Field{optionalNote("A note for the planner")}},
	{Action: ActReplan, Variant: variantHeld, Label: "Send back to the planner", Effect: "The planner plans the plan again on top of what is merged."},
	{Action: ActReplan, Variant: variantOffMain, Label: "Take the plan off main", Effect: "Reverts the plan's merged cards, then the planner plans it again."},
	{Action: ActClose, Label: "End the card", Effect: "Ends the card and closes its issue. Nothing more runs.",
		Fields: []Field{optionalNote("A note on why")}},
	{Action: ActClose, Variant: variantDone, Label: "I did it", Effect: "The factory does not check this. The card closes."},
	{Action: ActHandBack, Label: "I cannot do this", Effect: "This card ends, and the planner plans the whole plan again with your note. Its started cards stop.",
		Fields: []Field{requiredNote("What stops you", "One line for the planner: what keeps you from doing it")}},
	{Action: ActClose, Variant: variantNoted, Label: "Noted", Effect: "Ends this notice."},
	{Action: ActClose, Variant: variantIssue, Label: "Close the issue", Effect: "Closes the card's issue."},
	{Action: ActClose, Variant: SourceScreening, Label: "End the screening", Effect: "Ends the screening. You then decide whether an agent may work on this card."},
	{Action: ActClose, Variant: SourceSizing, Label: "End the sizing", Effect: "Ends the sizing. This card goes on at the size it was filed with."},
	{Action: ActClose, Variant: SourceProver, Label: "End the proof writing", Effect: "Ends the proof writing. The plan goes on without its tests."},
	{Action: ActClose, Variant: variantDismiss, Label: "End the card", Effect: "Ends the card and stops watching the chain until main moves."},
	{Action: ActClose, Variant: variantPlanWait, Label: "End the failed card", Effect: "Ends the failed card. The plan goes on without it.",
		Fields: []Field{optionalNote("A note on why")}},
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
	{Action: ActConfirmCriteria, Label: "The work is enough", Effect: "Records every open promise as met by the delivered cards. Drop first the ones you cannot confirm.",
		Fields: []Field{optionalNote("What shows it")}},
	{Action: ActRunCriteria, Label: "Run the tests now", Effect: "Runs the plan's tests on main and records each result."},
	{Action: ActDeployNow, Label: "Deploy now", Effect: "Releases the held production deploy."},
	{Action: ActSettle, Label: "It is fine, go on", Effect: "Ends this notice. Nothing else changes."},
	{Action: ActSettle, Variant: variantNoted, Label: "Noted", Effect: "Ends this notice. Nothing else changes."},
	{Action: ActSettle, Variant: variantOpen, Label: "Leave the issue open", Effect: "Ends this notice. The issue stays open."},
	{Action: ActSalvage, Label: "Rescue the work into a new card", Effect: "Ends the card's run and starts a new card from its branch."},
	{Action: ActNote, Label: "Not now", Effect: "The card keeps waiting for your answer.", Fields: []Field{optionalNote("A note")}},
	{Action: ActNote, Variant: variantPark, Label: "Not now", Effect: "The card keeps waiting for your answer."},
	{Action: ActNote, Variant: variantOwnWords, Label: "Answer in my words", Effect: "Posts your answer on the issue as you. The card waits until you end it.",
		Fields: []Field{requiredNote("Your answer", "The answer, posted on the issue as you")}},
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
	{Action: AnswerHold, Label: "Put on hold", Effect: "The card does not start until you release it, or until the card you name is merged or done.",
		Fields: []Field{requiredNote("Why it waits", "The reason, which the board shows on the card"),
			{Name: "release", Label: "The card that releases it", Hint: "An issue number, or empty to release it yourself", Kind: FieldText, Default: none}}},
	{Action: AnswerHold, Variant: variantAgain, Label: "Hold again", Effect: "The hold takes your new reason and the card you name in place of the ones it has."},
	{Action: AnswerRelease, Label: "Release the hold", Effect: "The card takes its turn to start again."},
	{Action: AnswerEditBrief, Label: "Edit the brief", Effect: "Changes the title, why line and summary for the agent and the board.",
		Fields: []Field{{Name: "title", Label: "Title", Kind: FieldText, Default: func(f Facts) string { return CardTitle(f.Card) }},
			{Name: "why", Label: "Why", Kind: FieldText, Default: func(f Facts) string { return f.Card.Why }},
			{Name: "summary", Label: "Summary", Kind: FieldText, Default: func(f Facts) string { return f.Card.Summary }}}},
	{Action: AnswerAccent, Label: "Colour", Effect: "Marks the card with a colour.",
		Fields: []Field{{Name: "accent", Label: "Colour", Kind: FieldChoice, Default: func(f Facts) string { return f.Card.Accent }, Options: accentOptions()}}},
	{Action: AnswerThread, Label: "Talk about this card", Effect: "Opens a chat thread on this card."},
	{Action: AnswerFundChat, Label: "Add money to the chat", Effect: "Raises the chat's budget for this month. The card closes.",
		Fields: []Field{{Name: "eur", Label: "EUR to add", Kind: FieldMoney, Default: func(f Facts) string { return textutil.EUR(f.Card.BudgetEUR) }}}},
	{Action: AnswerSkipRun, Label: "Skip the next run", Effect: "The routine misses its next run. “Undo the skip” reverses it."},
	{Action: AnswerSkipRun, Variant: variantUndo, Label: "Undo the skip", Effect: "The routine runs at its next time again."},
	{Action: AnswerNeedsMe, Label: "Give it to me", Effect: "The routine files its card for you."},
	{Action: AnswerNeedsMe, Variant: variantAlone, Label: "Let it run alone", Effect: "The routine files its card for an agent."},
	{Action: AnswerNeedsMe, Variant: variantFile, Label: "Follow the file", Effect: "The routine files its card as its file says."},
	{Action: AnswerRunNow, Label: "Run it now", Effect: "Files the routine's card now."},
	{Action: AnswerKeepHoldout, Label: "Keep it", Effect: "The promise joins the plan."},
	{Action: AnswerDropHoldout, Label: "Drop it", Effect: "The promise leaves the plan."},
	{Action: ActSettle, Variant: variantKeepOpen, Label: "Keep the plan open", Effect: "The plan stays open and holds production deploys until an approver closes it."},
	{Action: AnswerDropAndEnd, Label: "Drop them and close", Effect: "Drops the open promises with your reason and closes the plan, so production deploys go on.",
		Fields: []Field{requiredNote("Your reason", "Why the plan no longer needs them")}},
	{Action: AnswerClosePlan, Label: "Close the plan now", Effect: "Closes the plan now, so production deploys go on."},
}

func criterionField() Field {
	return Field{Name: "criterion", Label: "The promise", Hint: "The promise's name", Kind: FieldText, Required: true}
}

// accentOptions are the colours a card may wear, and none.
func accentOptions() []Option {
	out := []Option{{"", "none"}}
	for _, a := range Accents {
		out = append(out, Option{a, a})
	}
	return out
}

func sizeOptions() []Option {
	out := []Option{}
	for _, s := range policy.CardSizes {
		out = append(out, Option{s, s})
	}
	return out
}

// proposedRoom is the room a proposal on f asks for, false for none.
func proposedRoom(f Facts) (RoomAsk, bool) {
	if f.Recommend.Action != ActExtend {
		return RoomAsk{}, false
	}
	ask, _, why := Resolution{Action: ActExtend, Args: f.Recommend.Args}.roomAsk()
	return ask, why == ""
}

// RoomProposed says a rung proposes the room f opens at, with its own
// tick, which the readout keeps as a human's.
func RoomProposed(f Facts) bool {
	_, ok := proposedRoom(f)
	return ok
}

// roomCount is the count a room opens at: the proposal's, else 1.
func roomCount(f Facts) int {
	if ask, ok := proposedRoom(f); ok {
		return ask.Sessions
	}
	return 1
}

func roomSessions(f Facts) string { return strconv.Itoa(roomCount(f)) }

// roomMoney ticks a room's money as the proposal asks, else when the card
// lacks money for a room of 1.
func roomMoney(f Facts) string {
	ask, ok := proposedRoom(f)
	if !ok {
		ask.Money = RoomFor(f.CaseFacts, 1, true).EUR > 0
	}
	if ask.Money {
		return "yes"
	}
	return ""
}

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
	ActNote: waitEffect, KeyPark: waitEffect,
	ActClose + "/" + variantDone: func(f Facts) string { return factString(f.Case, "check") },
	ActApproveMerge: func(f Facts) string {
		if ChoiceKind(f.Case.Kind) {
			return HumanMergeEffect("")
		}
		return ""
	},
	ActClose: func(f Facts) string {
		switch {
		case f.Kind == CaseMonitorFlag && f.Final != "":
			return "Closes the card's issue."
		case ChoiceKind(f.Case.Kind) && f.Waiters > 0:
			// the card ends closed (humanEnd), so its dependents ask first
			return "Ends the card and closes its issue. The cards that wait for it ask you first."
		case f.Card.ID != "" && f.Card.IssueNumber == 0:
			return "Ends the card. Nothing more runs."
		}
		return ""
	},
	AnswerConfirmPlan: func(f Facts) string {
		switch p, _ := planOn(f); {
		case p.Asks > 0:
			return "The answers go to the next planner round."
		case len(p.OffMain) > 0:
			return "Reverts " + textutil.AndList(p.OffMain) + " from main, then the plan's cards start."
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
		return "Gives the card more sessions' worth, and " + onward(f, "it goes on") + "."
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
	ActGrantCriteria: func(Facts) string {
		// the delta planner sizes the card it files
		b := policy.Budget{}
		return fmt.Sprintf("Files one more card for the open promises, which spends up to %s, %s or %s EUR by the size its planner gives it.",
			textutil.EUR(b.SizeBudget("small")), textutil.EUR(b.SizeBudget("medium")), textutil.EUR(b.SizeBudget("large")))
	},
	ActRunCriteria: func(f Facts) string {
		if f.Kind == CaseMonitorFlag {
			return "Runs the plan's tests on this card's pull request and records each result."
		}
		return ""
	},
	AnswerApprove: func(f Facts) string {
		r := resourceOf(f)
		if r.ServerType == "" {
			return ""
		}
		return fmt.Sprintf("Rents a %s server for %s at about %s EUR.", r.ServerType, spanWords(time.Duration(r.LeaseMinutes)*time.Minute), textutil.EUR(r.ReservationEUR))
	},
}

// waitEffect is "Not now"'s effect: on a human's card, that it stays and
// what waits on it; at a cap, when the cap resets; on a notice, that it
// stays.
func waitEffect(f Facts) string {
	if f.Card.Kind == "human" {
		return strings.TrimSpace("The card stays in Needs you. " + factString(f.Case, "waits"))
	}
	if when := capReset(f); when != "" && slices.Contains(questionKinds, f.Case.Kind) {
		return "The card waits, and the agent's question comes back to you when the limit resets on " + when + "."
	} else if when != "" {
		return "The card waits and goes on by itself when the limit resets on " + when + "."
	}
	if f.Kind == CaseMonitorFlag {
		return "The notice stays on the card. Nothing else changes."
	}
	if p, _ := planOn(f); f.Case.Kind == CasePlanConfirmation && p.HoldoutsOpen > 0 {
		return fmt.Sprintf("The plan's cards wait until its %s %s kept or dropped.", plural(p.HoldoutsOpen, "extra promise"), isAre(p.HoldoutsOpen))
	}
	if p, ok := promiseOn(f); ok {
		if state, _ := promiseState(f, p); state == stateRunning {
			return "The tests go on, and each result lands on its promise."
		}
	}
	switch {
	case f.Settled && f.ClosesItself:
		return "The plan closes by itself within minutes."
	case f.Settled:
		return "The plan stays open until someone closes it."
	}
	if asked, err := time.Parse(time.RFC3339, factString(f.Case, "asked_at")); err == nil && f.Case.Kind == CaseAgentRequestInfo {
		return "The card keeps waiting for the author's answer until " + timeWords(asked.Add(infoWait)) +
			", then its issue closes as not reproducible."
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
	return timeWords(CapTurn(decision, at))
}

// CaseFaces are the faces of every case kind.
var CaseFaces = map[CaseKind]FaceSpec{}

// ApprovalFaces are the faces of every kind of approval request.
var ApprovalFaces = map[string]FaceSpec{}

// FlagFaces are the faces of every monitor flag.
var FlagFaces = map[string]FaceSpec{}

func init() {
	capFace := func(ask string) FaceSpec {
		return FaceSpec{Ask: fixed(ask), Why: capWhy, Story: workStory, Answers: keys(ActNote, ActClose)}
	}
	work := func(s FaceSpec) FaceSpec {
		if s.Story == nil {
			s.Story = workStory
		}
		return s
	}
	limit := work(FaceSpec{Ask: limitAsk, Why: limitWhy, Answers: keys(ActExtend, ActSuccessor, ActClose, ActNote)})
	modelStop := func(stop, why string) FaceSpec {
		ask := func(f Facts) string {
			if len(holding(f, ActHandover)) > 0 {
				return stop + " How should the card go on?"
			}
			return stop + " Try again, or end the card?"
		}
		return work(FaceSpec{Ask: ask, Why: fixed(why), Answers: func(f Facts) []string {
			// a helper runs its report on its own model
			return slices.Concat([]string{ActResume}, holding(f, ActHandover), []string{ActClose, ActNote})
		}})
	}
	for kind, s := range map[CaseKind]FaceSpec{
		CaseSessionCap:   limit,
		CaseAgentTimeCap: limit,
		CaseBudgetFloor:  work(FaceSpec{Ask: budgetFaceAsk, Why: budgetWhy, Answers: keys(ActExtend, ActSuccessor, ActClose, ActNote)}),
		CaseDayCap:       capFace("The factory's spending limit for today is reached. Wait for it to reset, or end the card?"),
		CaseMonthCap:     capFace("The factory's spending limit for this month is reached. Wait for it to reset, or end the card?"),
		CaseProjectCap:   capFace("This project's spending limit for the month is reached. Wait for it to reset, or end the card?"),
		CaseCheckinHuman: work(FaceSpec{Ask: fixed("The session's check found a question only you can settle. How should the card go on?"),
			Why:     reasonWhy("A check during the session found a question that only a human can settle."),
			Answers: keys(ActContinue, ActReplan, ActClose, ActNote)}),
		CasePolicyClass: work(FaceSpec{Ask: fixed("The change touches files only a human may approve. How should the card go on?"),
			Why:     reasonWhy("The factory's rules let only a human approve a change to these files."),
			Answers: keys(ActContinue, ActReplan, ActClose, ActNote)}),
		CaseCheckinShare: work(FaceSpec{Ask: fixed("The last session spent more than its share of the card's money. Give the card more room, or end it?"),
			Why: fixed("A session may spend only a share of the card's money before a human looks."), Answers: keys(ActExtend, ActClose, ActNote)}),
		CaseCheckinFailed: work(FaceSpec{Ask: fixed("The factory's own check of the session failed, not the work. Run one more session?"),
			Why: fixed("The check that judges each session gave no answer, so the card stopped to be safe."), Answers: keys(ActResume, ActClose, ActNote)}),
		CaseGateStop: work(FaceSpec{Ask: gateAsk, Why: gateWhy, Story: gateStory, Answers: gateAnswers, Suggest: gateSuggest}),
		CaseFindingsRepeated: work(FaceSpec{Ask: fixed("The review keeps raising the same findings. How should the card go on?"),
			Why:     fixed("The review raised the same findings after each of the last sessions, so the card stopped."),
			Answers: keys(ActExtend, ActReverify+"/"+variantOverrule, ActSuccessor, ActClose)}),
		CaseDependencyFailed: work(FaceSpec{Ask: depFailedAsk, Why: depWhy, Answers: depFailedAnswers}),
		CaseDependencyClosed: work(FaceSpec{Ask: depClosedAsk,
			Why: depWhy, Answers: func(f Facts) []string { return append(holding(f, ActRemoveDependency), ActReplan, ActClose) }}),
		CaseWaitExpired: work(FaceSpec{Ask: fixed("The card's author has not answered its question. Go on without the answer, or end the card?"),
			Why: fixed("The card's question waited past its time limit without an answer."), Answers: keys(ActNote, ActResume, ActClose)}),
		CasePodNeverStarted: modelStop("No machine could start the card's session.",
			"Several starts in a row waited for a machine with room, and no agent ran."),
		CaseProviderRefused: modelStop("The model provider refused the card's last starts.",
			"The provider turned down the first call of several starts in a row."),
		CasePlatformFault: modelStop("The platform ended the card's last sessions.",
			"The machines the card ran on ended several sessions in a row, not the card's own work."),
		CaseNoAnswer: modelStop("The model gave no answer to the card's last starts.",
			"Several starts in a row ended without an answer from the model."),
		CaseContentFilter: modelStop("The provider's content filter refused the session.",
			"The factory does not send a refused request again by itself."),
		CaseImageWait: work(FaceSpec{Ask: fixed("The card waits for another card to add a tool it needs. Let it wait, or end it?"),
			Why: fixed("The card needs a tool that another card is still installing for it."), Answers: keys(ActNote, ActClose)}),
		CaseScreeningSuspect: work(FaceSpec{Ask: fixed("The screening found this issue unsafe for an agent. Let the agent work on it?"),
			Why:     reasonWhy("A check of new issues found something an agent should not do without a human."),
			Answers: keys(ActContinue, ActClose)}),
		CaseAgentEscalate: work(FaceSpec{Ask: agentAsk("The agent asked a question it cannot answer alone. How should it go on?"),
			Why: func(f Facts) string {
				return cmp.Or(whyOf(agentLine(f, "Why")), "The agent stopped because it needs a decision only a human can make.")
			},
			Story: agentStory,
			Answers: func(f Facts) []string {
				say := sayKeys(f)
				if len(say) == 0 {
					// a question with no answers offered takes the human's words
					say = []string{ActContinue + "/" + variantOwnWords}
				}
				return append(say, ActNote, ActClose)
			},
			Suggest: func(f Facts) (string, string, string) {
				return agentPick(f), "the agent", agentLine(f, "Recommendation")
			}}),
		CaseAgentRequestInfo: work(FaceSpec{Ask: agentAsk("The agent needs an answer to go on. What should it know?"),
			Why: fixed("The agent cannot go on without this answer."), Story: infoStory,
			Answers: keys(ActContinue+"/"+variantOwnWords, ActNote, ActClose)}),
		CasePlanQuestion:     planQuestionFace,
		CasePlanConfirmation: planConfirmFace,
		CaseMergeApproval:    mergeFace,
		CasePlanCriteria:     criteriaFace,
		CasePlanHeld:         heldPlanFace,
		CaseHumanCard:        humanFace,
		CaseFactoryMonthCap:  humanFace,
		CaseProviderFreeze:   humanFace,
		CasePlatformDeaths:   humanFace,
		CaseJudgmentBudget:   humanFace,
		CaseRedMain:          humanFace,
		CaseRollHeld:         humanFace,
		CaseConfigRefused:    humanFace,
		CaseOther: work(FaceSpec{Ask: unasked("This card waits for a human, but none of its questions is open. How should it go on?",
			fixed("The card stopped on a problem it cannot fix by itself. How should it go on?")),
			Why:     unasked("The factory found the card waiting with nothing asked.", reasonWhy("The card stopped for a reason the factory has no rule for.")),
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
		ApprovalCriteria:      criteriaFace,
		ApprovalCriteriaRerun: criteriaFace,
	}
	FlagFaces = flagFaces()
}

func keys(k ...string) func(Facts) []string { return func(Facts) []string { return k } }

// holding are those of keys whose answers can happen on f: the answers a
// kind offers only in some of its states.
func holding(f Facts, keys ...string) []string {
	return slices.DeleteFunc(keys, func(key string) bool {
		a, ok := AnswerFor(key, f)
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
	if utf8.RuneCountInString(s) > StoryMax {
		return ""
	}
	return s
}

// workStory is the story of a stop on a work card, from its facts: the
// work it carries, what its sessions spent and what stopped it, and what
// is left of the work with what one more session costs.
func workStory(f Facts) Story {
	return Story{Planned: plannedLine(f), Happened: both(spentLine(f), stopSentence(f)), Left: line(leftLine(f))}
}

// plannedLine names the work card f carries by its title.
func plannedLine(f Facts) string {
	t := strings.TrimRight(CardTitle(f.Card), ".?! ")
	if t == "" {
		return ""
	}
	if f.Report {
		// a helper's title says what its step does to the card
		return line(t + ".")
	}
	verb := "builds"
	if f.Planner {
		verb = "plans"
	}
	return line(fmt.Sprintf("This card %s \u201c%s\u201d.", verb, t))
}

// TitleLine says story line l only names card t's work by its title
// (plannedLine), which a face whose head shows the title leaves out.
func TitleLine(l string, t Task) bool {
	for _, f := range []Facts{{Card: t}, {Card: t, CaseFacts: CaseFacts{Planner: true}}, {Card: t, CaseFacts: CaseFacts{Report: true}}} {
		if l != "" && l == plannedLine(f) {
			return true
		}
	}
	return false
}

// both is first and second as one story line, or the one of them that
// fits, second first.
func both(first, second string) string {
	if l := line(strings.TrimSpace(first + " " + second)); l != "" {
		return l
	}
	return cmp.Or(line(second), line(first))
}

// stopSentence is the sentence that names the stop of f's kind with its
// numbers, else what the last session did to the findings.
func stopSentence(f Facts) string {
	if s, ok := stopSentences[f.Kind]; ok {
		if l := s(f); l != "" {
			return l
		}
	}
	return progressLine(f)
}

// stopSentences are the stop sentences of the kinds whose stop is not the
// progress of the work.
var stopSentences = map[CaseKind]func(Facts) string{
	CaseAgentTimeCap:     timeCapLine,
	CaseDayCap:           capLine("The factory's spending reached its limit%s for the day."),
	CaseMonthCap:         capLine("The factory's spending reached its limit%s for the month."),
	CaseProjectCap:       capLine("This project's spending reached its limit%s for the month."),
	CaseCheckinHuman:     fixed("The check of the last session handed the card to a human."),
	CasePolicyClass:      fixed("The check of the last session found a change only a human may approve."),
	CaseOther:            unasked("The factory found no question of the card to ask again.", fixed("Its last session stopped on a problem that no rule of the factory covers.")),
	CaseCheckinShare:     shareLine,
	CaseCheckinFailed:    checkFailedLine,
	CaseGateStop:         gateLine,
	CaseDependencyClosed: depLine,
	CaseDependencyFailed: depLine,
	CaseWaitExpired:      waitedLine,
	CasePodNeverStarted:  inARow(`(\d+) sessions in a row waited`),
	CaseProviderRefused:  inARow(`first call of (\d+) sessions in a row`),
	CasePlatformFault:    platformLine,
	CaseNoAnswer:         inARow(`(\d+) sessions in a row ended without one`),
	CaseContentFilter:    fixed("The provider's content filter refused the request of the last session."),
	CaseImageWait:        imageWaitLine,
	CaseScreeningSuspect: fixed("The screening stopped the new issue before an agent read it."),
	CaseAgentRequestInfo: func(f Facts) string {
		return agentWords("The agent asked the issue's author after trying this: ", agentLine(f, "Tried"))
	},
}

// timeCapLine names the agent time the card's sessions used of what it was
// given.
func timeCapLine(f Facts) string {
	switch limit, used := seconds(f.AgentTimeCapS), seconds(f.AgentTimeS); {
	case limit <= 0:
		return ""
	case used >= limit:
		return "Its sessions used all " + minutesWord(limit) + " of agent time it was given."
	default:
		return fmt.Sprintf("Its sessions used %s of the %s of agent time it was given, too little left to start one more.", minutesWord(used), minutesWord(limit))
	}
}

// capLine names the spending limit that stopped the card, with its amount
// where the policy sets one.
func capLine(words string) func(Facts) string {
	return func(f Facts) string {
		amount := map[CaseKind]float64{CaseDayCap: f.DayCapEUR, CaseMonthCap: f.MonthCapEUR, CaseProjectCap: f.ProjectCapEUR}[f.Kind]
		if amount <= 0 {
			return fmt.Sprintf(words, "")
		}
		return fmt.Sprintf(words, " of "+textutil.EUR(amount)+" EUR")
	}
}

// shareLine names what the last session spent past its share of what the
// card had left: its amount only where the money line counts more sessions.
func shareLine(f Facts) string {
	if f.SpendShare <= 0 {
		return ""
	}
	share := fmt.Sprintf("%.0f percent", 100*f.SpendShare)
	if s := f.LastSession; s != nil && s.CostEUR >= 0.005 && f.Attempts > 1 {
		return fmt.Sprintf("The last session spent %s EUR, more than %s of what the card had left, and pushed nothing.", textutil.EUR(s.CostEUR), share)
	}
	return "The last session spent more than " + share + " of what the card had left, and pushed nothing."
}

// progressLine says what the card's last session left behind: no change
// pushed, or how many findings were open on the head it started from,
// counting a gate's findings only once that gate judged the new head; ""
// where the facts do not say. A finding has no identity a rewording
// keeps, so the line credits the session with no fix: the left line gives
// the count open now.
func progressLine(f Facts) string {
	s := f.LastSession
	switch {
	case s == nil || f.Planner:
		return ""
	case !s.Pushed:
		return "The last session pushed no change."
	}
	before, word := findingsBefore(f)
	if len(before) == 0 {
		return ""
	}
	return fmt.Sprintf("Before the last session, %s %s open.", plural(len(before), word), wasWere(len(before)))
}

// findingsBefore are the findings on the head the card's last session
// started from that a gate which judged the new head raised, and their
// word: the review's, the audit's, or findings.
func findingsBefore(f Facts) ([]string, string) {
	s := f.LastSession
	if s == nil || !s.Pushed {
		return nil, ""
	}
	judged := func(gate string, ids []string) []string {
		if slices.Contains(s.Judged, gate) {
			return ids
		}
		return nil
	}
	review, audit := judged("review", s.Review), judged("audit", s.Audit)
	before := slices.Compact(slices.Sorted(slices.Values(slices.Concat(review, audit))))
	return before, findingWord(before, review, audit)
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// uncheckedGates names the gates that have not judged the card's newest
// change, as a clause: the review, and the audit where it raised findings
// before; "" where both judged it.
func uncheckedGates(f Facts) string {
	s := f.LastSession
	if s == nil || f.Head == "" {
		return ""
	}
	review, audit := !slices.Contains(s.Judged, "review"), len(s.Audit) > 0 && !slices.Contains(s.Judged, "audit")
	switch {
	case review && audit:
		return "the review and the audit have not checked the newest change yet"
	case review:
		return "the review has not checked the newest change yet"
	case audit:
		return "the audit has not checked the newest change yet"
	}
	return ""
}

// agentWords are the agent's words after lead as one sentence, "" for none.
func agentWords(lead, words string) string {
	if words = strings.Join(strings.Fields(words), " "); words == "" {
		return ""
	}
	return endSentence(lead + words)
}

// checkFailedLine names how the check of the last session failed.
func checkFailedLine(f Facts) string {
	switch factString(f.Case, "reason_kind") {
	case CheckTimeCap:
		if d, err := time.ParseDuration(checkinCapOf(factString(f.Case, "reason"), checkinTimeCapStop)); err == nil {
			return "The check of the last session stopped at its time limit of " + minutesWord(d) + "."
		}
		return "The check of the last session stopped at its time limit."
	case CheckCallCap:
		return "The check of the last session stopped at its limit of model calls."
	case CheckUnreadable:
		return "The check of the last session answered in a form the factory could not read."
	}
	return ""
}

// gateBound reads the bound a gate stop ran past.
var gateBound = regexp.MustCompile(`past its bound of ([^:]+):`)

// gateLine names the step a gate stop stopped, its bound and its try; on
// the review's concerns, what the last session did to them.
func gateLine(f Facts) string {
	if reviewStop(f) {
		return cmp.Or(progressLine(f), "The review still raises findings on the newest change.")
	}
	reason := factString(f.Case, "reason")
	m, b := gateShape.FindStringSubmatch(reason), gateBound.FindStringSubmatch(reason)
	if m == nil || b == nil {
		return ""
	}
	d, err := time.ParseDuration(b[1])
	if err != nil {
		return ""
	}
	if f.Gate == ActivityWaitChecks {
		return "The checks ran longer than their " + spanWords(d) + "."
	}
	try := "on try " + m[2]
	if n, _ := strconv.Atoi(m[2]); n >= 1 && n <= len(ordinals) {
		try = "on its " + ordinals[n-1] + " try"
	}
	return fmt.Sprintf("%s ran longer than its %s %s.", cmp.Or(gateWords[m[1]], "A step of the work"), spanWords(d), try)
}

var ordinals = []string{"first", "second", "third", "fourth", "fifth"}

// depLine names how the card the card waits for ended; the reference
// names its work.
func depLine(f Facts) string {
	kind := CaseDependencyClosed
	if f.Kind == CaseDependencyFailed {
		kind = CaseDependencyFailed
	}
	d := tripDep(kind, f.CaseFacts)
	if d == nil || d.Issue == 0 {
		return ""
	}
	return fmt.Sprintf("#%d, which it waits for, %s.", d.Issue, endedWords(d.Status))
}

// endedWords says how a card ended: failed, or was closed.
func endedWords(status string) string {
	if status == "closed" {
		return "was closed"
	}
	return status
}

// waitedFor reads how long a stale wait's question waited.
var waitedFor = regexp.MustCompile(`quiet for (\d+) ?(d|h|min|m)\b`)

// waitedLine names how long the card's question waited for its author.
func waitedLine(f Facts) string {
	m := waitedFor.FindStringSubmatch(factString(f.Case, "reason"))
	if m == nil {
		return ""
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]string{"d": "day", "h": "hour", "min": "minute", "m": "minute"}[m[2]]
	return "Its question waited " + plural(n, unit) + " for the author."
}

// inARow is the stop sentence of a stop whose reason counts the sessions
// in a row that never reached a model answer: they are starts, not the
// sessions the money line counts. shape reads the count, the one fact the
// why line lacks.
func inARow(shape string) func(Facts) string {
	re := regexp.MustCompile(shape)
	return func(f Facts) string {
		m := re.FindStringSubmatch(factString(f.Case, "reason"))
		if m == nil {
			return ""
		}
		n, _ := strconv.Atoi(m[1])
		return plural(n, "start") + " in a row failed."
	}
}

// platformRow reads the count of PlatformEndedReason.
var platformRow = regexp.MustCompile(`^` + regexp.QuoteMeta(platformEnded) + `(\d+) in a row\.`)

// platformLine names the sessions in a row the platform ended and how it
// ended the last, the death the case's facts carry.
func platformLine(f Facts) string {
	m := platformRow.FindStringSubmatch(factString(f.Case, "reason"))
	if m == nil {
		return ""
	}
	n, _ := strconv.Atoi(m[1])
	if f.Death == nil {
		return fmt.Sprintf("The platform ended %s in a row.", plural(n, "session"))
	}
	return fmt.Sprintf("The platform ended %s in a row; in the last one, %s.", plural(n, "session"), f.Death.Short())
}

// imageTool reads the tool card an image wait waits for and its tool.
var imageTool = regexp.MustCompile(`which tool card #(\d+) adds (.+?) to:`)

// imageWaitLine names the card and the tool an image wait waits for.
func imageWaitLine(f Facts) string {
	if m := imageTool.FindStringSubmatch(factString(f.Case, "reason")); m != nil {
		return fmt.Sprintf("It waited 24 hours for #%s to add %s to its image, then stopped.", m[1], m[2])
	}
	return "It waited 24 hours for its image, then stopped."
}

// writerLines is story s with the lines case c's writer wrote in words
// where it wrote them, whole: what stopped the card after the money line,
// and what is left of the work before the findings and the price. CaseFace
// alone adds them, to the story the register composed.
func writerLines(s Story, c Case) Story {
	// a line the story holds already, as a row stored it, stands once
	if h := line(c.Brief); h != "" && !strings.Contains(s.Happened, h) {
		s.Happened = both(firstSentence(s.Happened), h)
	}
	if l := line(c.BriefDetail); l != "" && !strings.Contains(s.Left, l) {
		// the writer's door bounds l to fit (writerLeftMax)
		s.Left = strings.TrimSpace(l + " " + s.Left)
	}
	return s
}

func spentLine(f Facts) string {
	switch {
	case f.Attempts == 0 && f.BudgetEUR == 0:
		return "No session has run on this card."
	case f.Attempts == 0:
		return fmt.Sprintf("No session has run on its %s EUR budget yet.", textutil.EUR(f.BudgetEUR))
	case f.Kind == CaseSessionCap:
		return fmt.Sprintf("It spent %s of %s EUR in all %s it was given.", textutil.EUR(f.SpentEUR), textutil.EUR(f.BudgetEUR), plural(f.Attempts, "session"))
	}
	return fmt.Sprintf("It spent %s of %s EUR in %s.", textutil.EUR(f.SpentEUR), textutil.EUR(f.BudgetEUR), plural(f.Attempts, "session"))
}

// leftLine is what is left of the card's work: the open findings and the
// failed checks, and what one more session costs, as the last one did; of
// a plan's, its cards still to merge.
func leftLine(f Facts) string {
	if f.Planner {
		merged := 0
		for _, s := range f.Siblings {
			if s.Status == "merged" || s.Status == "done" {
				merged++
			}
		}
		if len(f.Siblings) == 0 {
			return ""
		}
		return fmt.Sprintf("%d of its %s %s merged.", merged, plural(len(f.Siblings), "card"), isAre(merged))
	}
	var parts []string
	before, _ := findingsBefore(f)
	switch n := openFindings(f.CaseFacts); {
	case n > 0:
		parts = append(parts, fmt.Sprintf("%s %s open", plural(n, "finding"), isAre(n)))
	case len(before) > 0:
		// the story counted the findings before: it says none is left
		parts = append(parts, "no finding is open")
		if f.Checks == "green" {
			parts = append(parts, "the checks are green")
		}
	}
	if f.Checks == "red" {
		parts = append(parts, "the checks failed")
	}
	unchecked := uncheckedGates(f)
	var out string
	switch {
	case len(parts) > 0 && unchecked != "":
		out = capitalize(strings.Join(parts, ", and ")) + ". " + capitalize(unchecked) + "."
	case len(parts) > 0:
		out = capitalize(strings.Join(parts, ", and ")) + "."
	case f.Report:
		out = "Its report is not written yet."
	case f.Head == "" && f.Attempts == 0:
		return "All of its work is still to do."
	case f.Head == "":
		out = "No change is pushed yet."
	case unchecked != "" && f.Checks == "green":
		out = "The checks are green, and " + unchecked + "."
	case unchecked != "":
		out = capitalize(unchecked) + "."
	case f.Checks == "green":
		out = "No finding is open, and the checks are green."
	}
	if f.Kind == CaseBudgetFloor && f.BranchRead && f.BranchHead == "" && f.Attempts > 0 {
		out = lostWorkLine
	}
	if f.MergeConflict != "" {
		out = strings.TrimSpace(out + " " + conflictLine)
	}
	// at the budget floor the why's amount is worked out from a session's
	// worth, so the story names where that comes from: the card's own
	// sessions, when they cost more than an opening
	switch est, s := SessionEstimate(f.CaseFacts), f.LastSession; {
	case s == nil || s.CostEUR < 0.005:
	case f.Kind != CaseBudgetFloor:
		out = strings.TrimSpace(out + fmt.Sprintf(" One more session costs about %s EUR.", textutil.EUR(s.CostEUR)))
	case est.FromSessions > 0 && est.EUR > f.FloorEUR:
		out = strings.TrimSpace(out + fmt.Sprintf(" A session's worth costs about %s EUR, from its %s so far.", textutil.EUR(est.EUR), plural(est.FromSessions, "session")))
	}
	return out
}

// conflictLine is what is left on a head that no longer merges into main:
// no run of the gates clears it, only a session that merges main.
const conflictLine = "The pull request no longer merges into main. Only a session that merges main into its branch fixes that."

// lostWorkLine is what is left at a budget floor after a session whose
// branch, once read, holds no work main lacks.
const lostWorkLine = "The last session's work was not pushed and is lost. The next session starts again from main."

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
func openFindings(f CaseFacts) int { return len(openIDs(f)) }

// openIDs are the review and audit findings that stand on the head.
func openIDs(f CaseFacts) []string {
	return slices.DeleteFunc(slices.Clone(f.Findings), func(id string) bool {
		return !slices.Contains(f.ReviewFindings, id) && !slices.Contains(f.AuditFindings, id)
	})
}

// findingWord names findings ids as the review's or the audit's when that
// gate raised them all, else as findings.
func findingWord(ids, review, audit []string) string {
	all := func(of []string) bool {
		return !slices.ContainsFunc(ids, func(id string) bool { return !slices.Contains(of, id) })
	}
	switch {
	case all(review):
		return "review finding"
	case all(audit):
		return "audit finding"
	}
	return "finding"
}

// openWord names the findings open on f's head.
func openWord(f CaseFacts) string { return findingWord(openIDs(f), f.ReviewFindings, f.AuditFindings) }

func limitAsk(f Facts) string {
	what := fmt.Sprintf("all %s", plural(f.SessionCap, "session"))
	if f.Kind == CaseAgentTimeCap {
		what = "its agent time of " + minutesWord(time.Duration(f.AgentTimeCapS)*time.Second)
	}
	if n := openFindings(f.CaseFacts); n > 0 {
		return fmt.Sprintf("The card used %s with %s open. How should it go on?", what, plural(n, openWord(f.CaseFacts)))
	}
	return fmt.Sprintf("The card used %s. How should it go on?", what)
}

func limitWhy(f Facts) string {
	if f.Kind == CaseAgentTimeCap {
		return "A card runs only for the agent time it was given, and this one has used it."
	}
	return fmt.Sprintf("A card runs only the sessions it was given, and this one has used all %d.", f.SessionCap)
}

// budgetFaceAsk names the stop and no answer to it: the money left does
// not pay the next session, at the spend limit where a room of 1 would
// pass it, with the findings open on the head.
func budgetFaceAsk(f Facts) string {
	ask := "The card's money left does not pay its next session"
	n, limit := openFindings(f.CaseFacts), RoomFor(f.CaseFacts, 1, true).Limit > 0
	open := fmt.Sprintf("%s %s open", plural(n, openWord(f.CaseFacts)), isAre(n))
	switch {
	case limit && n > 0:
		ask += ", the card is at its spend limit, and " + open
	case limit:
		ask += ", and the card is at its spend limit"
	case n > 0:
		ask += ", and " + open
	}
	return ask + ". How should it go on?"
}

// budgetWhy says what the card has left, what the review and the audit
// need of it, and what the card needs more for its next session: the
// money a room of 1 adds.
func budgetWhy(f Facts) string {
	left, need := textutil.EUR(f.LeftEUR), textutil.EUR(RoomFor(f.CaseFacts, 1, true).EUR)
	if f.ReserveEUR > 0 {
		return fmt.Sprintf("The card has %s EUR left, the review and the audit need %s EUR for themselves, and the card needs %s EUR more to pay its next session.",
			left, textutil.EUR(f.ReserveEUR), need)
	}
	return fmt.Sprintf("The card has %s EUR left and needs %s EUR more to pay its next session.", left, need)
}

// capWhy names the count a human resets in the status panel to resume the
// card sooner, then the turn of the cap's UTC period; the ask and the
// wait's reason name the commit that raises the cap. At an agent's
// question, the question comes back.
func capWhy(f Facts) string {
	count, turn := "today's count", "at 00:00 UTC"
	switch f.Kind {
	case CaseMonthCap:
		count, turn = "the month's count", "when the month turns"
	case CaseProjectCap:
		count, turn = "the project's month count", "when the month turns"
	}
	if slices.Contains(questionKinds, f.Case.Kind) {
		return fmt.Sprintf("Reset %s in the status panel to have the agent's question back sooner, or it comes back %s.", count, turn)
	}
	return fmt.Sprintf("Reset %s in the status panel to resume it sooner, or it resumes %s.", count, turn)
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
	for _, p := range []string{"check-in: ", AgentAsks, agentAsksAuthor, screeningAsks} {
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

// unasked is words on a case whose stop is the factory's own sentence that
// it asks again (askedAgain), which a card with no question to ask again
// stops on, else what other says.
func unasked(words string, other func(Facts) string) func(Facts) string {
	return func(f Facts) string {
		if askedAgain(factString(f.Case, "reason")) {
			return words
		}
		return other(f)
	}
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
		return "The checks ran past their time limit. How should the card go on?"
	case reviewStop(f):
		return "The review keeps raising findings on this card. How should it go on?"
	case strings.HasPrefix(factString(f.Case, "reason"), leftQueue):
		return "The pull request left the merge queue with red checks. How should the card go on?"
	}
	step := "A step of the work"
	if m := gateShape.FindStringSubmatch(factString(f.Case, "reason")); m != nil {
		step = cmp.Or(gateWords[m[1]], step)
	}
	return step + " ran past its time limit. How should the card go on?"
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

// gateStory is a gate stop's story, and at a checks wait the checks'
// state the stop found as what is left.
func gateStory(f Facts) Story {
	s := workStory(f)
	if f.Gate != ActivityWaitChecks {
		return s
	}
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
	keys := gateFaceAnswers(f.Gate, reviewStop(f))
	if len(holding(f, ActRerunChecks)) == 0 {
		keys = slices.DeleteFunc(keys, func(key string) bool { return key == ActRerunChecks })
	}
	return keys
}

// gateFaceAnswers are the answers the face of a gate stop of activity can
// hold; review says a refused review marks the stop.
func gateFaceAnswers(activity string, review bool) []string {
	switch {
	case activity == ActivityWaitChecks:
		return []string{ActWaitChecks, ActRerunChecks, ActClose}
	case review:
		return []string{ActExtend, ActReverify + "/" + variantOverrule, ActSuccessor, ActClose}
	}
	return []string{ActReverify, ActContinue, ActReplan, ActClose}
}

// GateStopAnswers are the answers the face of a gate supervisor's stop of
// activity can hold, with a refused review and without: the values of the
// answer its decision recommends.
func GateStopAnswers(activity string) []string {
	var out []string
	for _, key := range slices.Concat(gateFaceAnswers(activity, false), gateFaceAnswers(activity, true)) {
		out = appendNew(out, key)
	}
	return out
}

// gateSuggest is the gate supervisor's recommendation: the answer its
// decision names, where the face holds it, with the recommendation's words
// as grounds; else its words alone, which lead no answer.
func gateSuggest(f Facts) (key, by, grounds string) {
	if key = agentLine(f, "Answer"); !slices.Contains(gateAnswers(f), key) {
		key = ""
	}
	return key, "the gate supervisor", agentLine(f, "Recommendation")
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
	case retry:
		return "A card this one waits for failed. How should this card go on?"
	case free:
		return "A card this one waits for failed for good. How should this card go on?"
	}
	return "A card this one waits for failed for good. Send this card back to the planner, or end it?"
}

func depClosedAsk(f Facts) string {
	if len(holding(f, ActRemoveDependency)) > 0 {
		return "A card this one waits for was closed. How should this card go on?"
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
	return fmt.Sprintf("This card needs the work of #%d and cannot go on without it.", d.Issue)
}

// agentQuestion is the agent's question: the stop's first line, past the
// words that open an escalation or a request for information.
func agentQuestion(f Facts) string {
	q := EscalationQuestion(cmp.Or(factString(f.Case, "face"), factString(f.Case, "reason"), f.Case.Problem))
	return strings.TrimSpace(strings.TrimPrefix(q, agentAsksAuthor))
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
// ask can show. The last sentence is the ask only when a story line holds
// the first of what stands before it, else the story would drop it.
func agentParts(f Facts) (before, ask string) {
	q := capitalize(agentQuestion(f))
	if q != "" && readsAsAsk(q) {
		return "", q
	}
	if b := sentenceBreaks(q); len(b) > 0 {
		last := q[b[len(b)-1][3]:]
		head := strings.TrimSpace(q[:b[len(b)-1][3]])
		if strings.HasSuffix(last, "?") && readsAsAsk(last) && line(endSentence(firstSentence(head))) != "" {
			return head, last
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
		s.Happened = both(spentLine(f), endSentence(firstSentence(before)))
	default:
		s.Happened = AgentAsks + agentWhole(f)
	}
	return s
}

// infoStory is a request for information's story: what the agent tried,
// and what it still needs, in its own words.
func infoStory(f Facts) Story {
	s := workStory(f)
	if l := line(agentWords("What it still needs: ", agentLine(f, "Needs"))); l != "" {
		s.Left = l
	}
	return s
}

// asks opens the story line that quotes the agent's question of kind.
func asks(kind CaseKind) string {
	if kind == CaseAgentRequestInfo {
		return agentAsksAuthor
	}
	return AgentAsks
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
// a button cannot carry, filed before their door checked them, show on no face.
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

// offerable says words a writer offered as an answer pass the check of a
// button.
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
	Story: func(f Facts) Story {
		if p, ok := planOn(f); ok {
			return Story{Planned: line(firstSentence(p.Outcome))}
		}
		return Story{}
	},
	Answers: func(f Facts) []string {
		var out []string
		for _, c := range factStrings(f.Case, "choices") {
			if len(out) < 3 && offerable(c) {
				out = append(out, planPrefix+c)
			}
		}
		return append(out, AnswerPlanChoice+"/"+variantOwnWords, AnswerRevisePlan)
	},
	Suggest: func(f Facts) (string, string, string) {
		if r := factString(f.Case, "recommended"); offerable(r) {
			return planPrefix + r, "the planner", factString(f.Case, "grounds")
		}
		return "", "", ""
	},
}

// planConfirmFace is a plan confirmation's face: the confirmation itself,
// or a page of one extra promise its round proposed.
var planConfirmFace = FaceSpec{
	Ask: func(f Facts) string {
		if _, ok := holdoutOn(f); ok {
			return "Keep this extra promise for " + askedPlan(f) + "?"
		}
		p, ok := planOn(f)
		if p.HoldoutsOpen > 0 {
			// a confirm waits until they are settled
			n := plural(p.HoldoutsOpen, "extra promise")
			return firstAsk(fmt.Sprintf("Does %s need changes before its %s %s settled?", askedPlan(f), n, isAre(p.HoldoutsOpen)),
				fmt.Sprintf("Does this plan need changes before its %s %s settled?", n, isAre(p.HoldoutsOpen)))
		}
		if !ok || len(p.Cards) == 0 {
			return firstAsk("Confirm "+askedPlan(f)+" so its cards can start?", "Confirm this plan so its cards can start?")
		}
		what := plural(len(p.Cards), "card")
		if len(p.Promises) > 0 {
			what += " and " + plural(len(p.Promises), "promise")
		}
		ask := fmt.Sprintf("Confirm %s with %s", askedPlan(f), what)
		if len(p.OffMain) > 0 {
			ask += ", and take " + textutil.AndList(p.OffMain) + " off main"
		}
		return firstAsk(ask+"?", "Confirm this plan with "+what+"?")
	},
	Why: func(f Facts) string {
		if _, ok := holdoutOn(f); ok {
			return "The plan's cards start only once each extra promise is kept or dropped."
		}
		if p, _ := planOn(f); p.HoldoutsOpen > 0 {
			return fmt.Sprintf("The plan's cards start only once its %s %s kept or dropped.", plural(p.HoldoutsOpen, "extra promise"), isAre(p.HoldoutsOpen))
		}
		return planConfirmWhy
	},
	Story: func(f Facts) Story {
		if h, ok := holdoutOn(f); ok {
			return Story{Planned: "An extra promise is one the plan's own list missed, and a test the agent never sees checks it.",
				Happened: line("The extra promise: " + endSentence(h.Claim)), Left: "Keeping it adds one more test, which the plan's last card must pass."}
		}
		p, _ := planOn(f)
		s := Story{Planned: line(firstSentence(strings.TrimSpace(p.Outcome)))}
		if len(p.Cards) > 0 {
			s.Happened = cmp.Or(line("Its cards: "+textutil.AndList(p.Cards)+"."), fmt.Sprintf("It files %s, listed in the full brief.", plural(len(p.Cards), "card")))
		}
		if len(p.Promises) > 0 {
			s.Left = cmp.Or(line("It promises: "+strings.Join(p.Promises, " ")), fmt.Sprintf("It makes %s, listed in the full brief.", plural(len(p.Promises), "promise")))
		}
		if len(p.OffMainWhys) > 0 {
			s.View = cmp.Or(line("The planner's view: "+strings.Join(p.OffMainWhys, " ")), "The planner's view: each card it takes off main has its reason in the full brief.")
		}
		return s
	},
	Answers: func(f Facts) []string {
		if _, ok := holdoutOn(f); ok {
			return []string{AnswerKeepHoldout, AnswerDropHoldout}
		}
		if p, _ := planOn(f); p.HoldoutsOpen > 0 {
			return []string{AnswerRevisePlan, ActNote}
		}
		return []string{AnswerConfirmPlan, AnswerRevisePlan}
	},
}

// planConfirmWhy is why a plan's confirmation asks: on its row, which the
// inbox's message says, and on its face while no extra promise waits.
const planConfirmWhy = "The plan's cards and promises are fixed only once a human confirms them."

// firstAsk is the first of asks that reads as an ask.
func firstAsk(asks ...string) string {
	for _, q := range asks {
		if readsAsAsk(q) {
			return q
		}
	}
	return asks[len(asks)-1]
}

// askedPlan is the plan of card f as an ask names it: "plan “<name>”",
// else "this plan".
func askedPlan(f Facts) string {
	name := planName(f.Card)
	if f.Card.Track != policy.TrackPlan {
		name = f.PlanName
	}
	if name == "" {
		return "this plan"
	}
	return "plan \u201c" + name + "\u201d"
}

// planName is plan p's name: its short name, else its title.
func planName(p Task) string {
	return strings.TrimSpace(cmp.Or(p.PlanName, strings.TrimRight(CardTitle(p), ".?! ")))
}

// PlanWords names the plan of issue in a card that is not its own: plan
// “<name>” (#N), plan #N for a plan without a name. The name is text an
// agent wrote, and these words reach Forgejo in a commit status, an issue
// and a comment: it goes out cleaned, with no link kept.
func PlanWords(name string, issue int64) string {
	name = sanitize.Outbound(name, nil)
	if name == "" {
		return fmt.Sprintf("plan #%d", issue)
	}
	return fmt.Sprintf("plan \u201c%s\u201d (#%d)", name, issue)
}

// titled is card f's title in quotes, "" for a card without one.
func titled(f Facts) string {
	if t := strings.TrimRight(CardTitle(f.Card), ".?! "); t != "" {
		return "\u201c" + t + "\u201d"
	}
	return ""
}

var mergeFace = FaceSpec{
	Ask: func(f Facts) string {
		name := cmp.Or(titled(f), "this card's pull request")
		if _, ok := promiseOn(f); ok && titled(f) != "" {
			return firstAsk("Is this promise met by "+titled(f)+"?", "Is this promise of the plan met?")
		} else if ok {
			return "Is this promise of the plan met?"
		}
		switch {
		case f.Checks == "red" && f.Factory:
			return firstAsk("The checks of "+name+" are red. Run them again, or close it?", "The checks of this card's pull request are red. Run them again, or close it?")
		case f.Checks == "red":
			return firstAsk("The checks of "+name+" are red. Send it back to its agent?", "The checks of this card's pull request are red. Send it back to its agent?")
		}
		return firstAsk("Merge "+name+"?", "Merge this card's pull request?")
	},
	Why: func(f Facts) string {
		if _, ok := promiseOn(f); ok {
			if n := len(promisesOpen(f)); n > 1 {
				return fmt.Sprintf("The pull request merges only once each of the %d promises a human judges has a verdict.", n)
			}
			return "The pull request merges only once the promise a human judges has a verdict."
		}
		switch {
		case f.Checks == "red" && f.Factory:
			return "It cannot merge until its checks pass, and no agent works on a pull request the factory opened."
		case f.Checks == "red":
			return "It cannot merge until its checks pass."
		}
		if w := whyOf(firstSentence(factString(f.Case, "review_not_run"))); w != "" {
			return w
		}
		if words, ok := classWords[factString(f.Case, "class")]; ok {
			return words + ", and the factory's rules let only a human merge it."
		}
		return "The factory's rules let only a human merge this change."
	},
	Story: func(f Facts) Story {
		if p, ok := promiseOn(f); ok {
			s := Story{Planned: promisedLine(p)}
			if p.Evidence != "" {
				s.Happened = line("What to look at: " + endSentence(p.Evidence))
			}
			return s
		}
		// what the card built: its summary's first sentence, else its title
		s := Story{Planned: cmp.Or(line(firstSentence(strings.TrimSpace(f.Card.Summary))), plannedLine(f)), Left: checksLine(f)}
		if !mergeSuggested(f) {
			s.Happened = reviewLine(f)
		}
		return s
	},
	Answers: func(f Facts) []string {
		if _, ok := promiseOn(f); ok {
			return []string{ActAnswerCriteria, ActAnswerCriteria + "/not_met", ActAnswerCriteria + "/waive"}
		}
		deny := []string{ActDenyMerge, ActDenyMerge + "/reject"}
		if !f.Factory {
			if f.Checks == "red" {
				return deny
			}
			return append([]string{ActApproveMerge}, deny...)
		}
		// no agent works on a pull request factoryd opened: a deny closes
		// it, and its red checks run again
		if f.Checks == "red" {
			return []string{ActRerunChecks, ActDenyMerge + "/reject"}
		}
		return []string{ActApproveMerge, ActDenyMerge + "/reject"}
	},
	Suggest: func(f Facts) (string, string, string) {
		if !mergeSuggested(f) {
			return "", "", ""
		}
		return ActApproveMerge, "the model review", "It approved the change" + findingsClause(f) + "."
	},
}

// mergeSuggested: the model review approved the merge's head, and its
// checks are green, so the review suggests the merge.
func mergeSuggested(f Facts) bool {
	_, page := promiseOn(f)
	return !page && f.Checks != "red" && f.ReviewApproved
}

// reviewLine is the model review's verdict on a merge in one line, from
// its facts: its verdict and how many findings it and the security audit
// raised; "" when no review judged the head, which the why says.
func reviewLine(f Facts) string {
	switch factString(f.Case, "review_verdict") {
	case "approve":
		return "The model review approved the change" + findingsClause(f) + "."
	case "concerns":
		return fmt.Sprintf("The model review raised concerns in %s%s.", plural(caseInt(f.Case, "review_findings"), "finding"), auditClause(f))
	}
	return ""
}

// findingsClause names the review's and the audit's findings beside an
// approval.
func findingsClause(f Facts) string {
	n := caseInt(f.Case, "review_findings")
	clause := " with no finding"
	if n > 0 {
		clause = " with " + plural(n, "finding")
	}
	return clause + auditClause(f)
}

func auditClause(f Facts) string {
	if n := caseInt(f.Case, "audit_findings"); n > 0 {
		return ", and the security audit raised " + plural(n, "finding")
	}
	return ""
}

// checksLine says how a merge's checks stand.
func checksLine(f Facts) string {
	switch f.Checks {
	case "green":
		return "The checks are green."
	case "red":
		if failed := factStrings(f.Case, "failed_checks"); len(failed) > 0 {
			return cmp.Or(line("The checks are red: "+textutil.AndList(failed)+" failed."), "The checks are red.")
		}
		return "The checks are red."
	}
	return ""
}

// caseInt is case c's fact key as a whole number, 0 for none.
func caseInt(c Case, key string) int {
	switch n := c.Facts[key].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// classWords say what a change of each risk class is, in plain words.
var classWords = map[string]string{policy.ClassNormal: "The change is of normal risk", policy.ClassHigh: "The change is high risk",
	"migration": "The change alters the database", policy.ClassRunner: "The change touches files the factory's own runner reads",
	policy.ClassLarge: "The change is large"}

// PromiseFact is the promise one page of a plan's request asks about: its
// id, its words (PromiseOf), its kind, and its standing verdict with its
// evidence, or for a human promise what to look at.
type PromiseFact struct {
	ID, Promise, Kind, Verdict, Evidence string
}

// OnPromise is case c as the page of promise p: a case of a card whose
// plan holds criteria.
func OnPromise(c Case, p PromiseFact) Case {
	c.Facts = withFacts(c.Facts, map[string]any{"page_promise": p, "criteria": true})
	return c
}

// OnOpen is case c of a plan's request with the promises it asks about:
// open those still open, in the plan's order, unchecked those of them
// nothing judged yet, of how many promises the plan makes (0: not told).
// Its request holds them as they stood when it was asked, its card's rows
// as they stand now.
func OnOpen(c Case, open, unchecked []string, of int) Case {
	c.Facts = withFacts(c.Facts, map[string]any{"open": open, "unchecked": unchecked, "of": of})
	return c
}

// promisesOpen are the promises request f asks about still open (OnOpen).
func promisesOpen(f Facts) []string { return factStrings(f.Case, "open") }

// RunFact is the newest run of a plan's tests a human asked for: its
// outcome (asked, started, ran, cut, no_room), who asked, when it was
// written, and why a cut run stopped.
type RunFact struct {
	Outcome, By, Why string
	At               time.Time
}

// RunOf is the run a criteria gate run row e records, false for the
// factory's own gate.
func RunOf(e Event) (RunFact, bool) {
	by, _ := e.Detail["by"].(string)
	outcome, _ := e.Detail["outcome"].(string)
	why, _ := e.Detail["why"].(string)
	return RunFact{Outcome: outcome, By: by, Why: why, At: e.At}, by != ""
}

// OnRun is case c of a criteria request with the newest run of its tests
// a human asked for.
func OnRun(c Case, run RunFact) Case {
	c.Facts = withFacts(c.Facts, map[string]any{"page_run": run})
	return c
}

func runOn(f Facts) (RunFact, bool) {
	run, ok := f.Case.Facts["page_run"].(RunFact)
	return run, ok
}

// HoldoutFact is the extra promise one page of a plan's confirmation asks
// about: its index among the round's and its claim.
type HoldoutFact struct {
	Idx   int
	Claim string
}

// OnHoldout is case c as the page of extra promise h.
func OnHoldout(c Case, h HoldoutFact) Case {
	c.Facts = withFacts(c.Facts, map[string]any{"page_holdout": h})
	return c
}

// PlanFacts are what a plan's question and confirmation name of the plan:
// its outcome, its cards' titles, its promises, the cards it takes off
// main with the planner's reasons, how many extra promises wait, and how
// many questions the planner asked beside the confirmation, whose answers
// a confirm sends to the next planner round.
type PlanFacts struct {
	Outcome              string
	Cards, Promises      []string
	OffMain, OffMainWhys []string
	HoldoutsOpen, Asks   int
}

// OnPlan is case c of a plan's question or confirmation with plan p.
func OnPlan(c Case, p PlanFacts) Case {
	c.Facts = withFacts(c.Facts, map[string]any{"page_plan": p})
	return c
}

func withFacts(facts, more map[string]any) map[string]any {
	out := maps.Clone(facts)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, more)
	return out
}

func promiseOn(f Facts) (PromiseFact, bool) { return PromiseOn(f.Case) }

func holdoutOn(f Facts) (HoldoutFact, bool) { return HoldoutOn(f.Case) }

// PromiseOn is the promise page c asks about, false for a case no page made.
func PromiseOn(c Case) (PromiseFact, bool) {
	p, ok := c.Facts["page_promise"].(PromiseFact)
	return p, ok
}

// HoldoutOn is the extra promise page c asks about, false for a case no
// page made.
func HoldoutOn(c Case) (HoldoutFact, bool) {
	h, ok := c.Facts["page_holdout"].(HoldoutFact)
	return h, ok
}

func planOn(f Facts) (PlanFacts, bool) {
	p, ok := f.Case.Facts["page_plan"].(PlanFacts)
	return p, ok
}

// promisedLine is the story's line of what the plan promised; a promise in
// a plan's own words too long for it stands whole in a fold of its own on
// the page (PromiseFits).
func promisedLine(p PromiseFact) string {
	if !PromiseFits(p.Promise) {
		return "The plan's promise is below, in its own words."
	}
	return line("The plan promised: " + endSentence(p.Promise))
}

// PromiseFits says promise fits the story's line; one that does not stands
// whole in the page's fold "The promise in the plan's words".
func PromiseFits(promise string) bool {
	return line("The plan promised: "+endSentence(promise)) != ""
}

// gateKind: a test checks a promise of kind.
func gateKind(kind string) bool { return kind == CriterionTest || kind == CriterionInvariant }

// verdictLine is what judged an unmet promise said, from its row's
// evidence (criterionLine, judgeArtifact): a test's message without the
// test's name and path, a model's reason without its model and prompt.
func verdictLine(p PromiseFact) string {
	_, detail := GateEvidence(p.ID, p.Evidence)
	head := map[string]string{CriterionJudgement: "The model that judges it found it not met", CriterionHuman: "A human judged it not met"}[p.Kind]
	switch {
	case !gateKind(p.Kind):
		if i := strings.LastIndex(detail, " (model "); i > 0 {
			detail = detail[:i]
		}
	case strings.HasPrefix(detail, "no test ") || strings.Contains(detail, " ran no tests"):
		return "Its test was not found, so nothing checked it."
	default:
		head = "Its test ran and failed"
		if strings.Contains(detail, " skipped") {
			head = "Its test was skipped"
		}
		_, detail, _ = strings.Cut(detail, ": ")
	}
	if detail = strings.TrimSpace(detail); detail != "" {
		if l := line(head + ": " + endSentence(detail)); l != "" {
			return l
		}
	}
	return head + "."
}

// untilSettled is a criteria request's why: what stays blocked until its
// n open promises are settled.
func untilSettled(n int) string {
	switch n {
	case 0, 1:
		return "Until this promise is settled, the plan stays open and holds production deploys."
	case 2:
		return "Until both are settled, the plan stays open and holds production deploys."
	}
	return fmt.Sprintf("Until all %d are settled, the plan stays open and holds production deploys.", n)
}

// criteriaFace is the face of a plan's criteria request, a page per open
// promise: a gap, whose promises are not met, or criteria never judged,
// whose promises no test checked. Each page tells what is true of its
// promise and offers what can happen to it: a verdict's line, the newest
// run of the tests a human asked for, or that no test exists for it.
var criteriaFace = FaceSpec{
	Ask: func(f Facts) string {
		open, unchecked := len(promisesOpen(f)), len(factStrings(f.Case, "unchecked"))
		of, _ := toInt(f.Case.Facts["of"])
		if f.ApprovalKind != ApprovalCriteriaRerun {
			// a gap's promises with no result are not met either
			unchecked = 0
		}
		if f.Settled {
			return firstAsk(fmt.Sprintf("Every promise of %s is met or dropped. Close it now?", askedPlan(f)), "Every promise of this plan is met or dropped. Close it now?")
		}
		plan := capitalize(askedPlan(f))
		either := func(format string, args ...any) string {
			return firstAsk(fmt.Sprintf(format, append([]any{plan}, args...)...), fmt.Sprintf(format, append([]any{"This plan"}, args...)...))
		}
		switch unmet := open - unchecked; {
		case unchecked > 0 && unmet == 0:
			return either("%s finished, but no test checked its %s. Settle each one.", plural(open, "promise"))
		case unchecked > 0:
			return either("%s finished, but no test checked %s, and %d %s not met. Settle each one.", plural(unchecked, "promise"), unmet, isAre(unmet))
		case of == 0:
			return either("%s finished, but %s %s not met. Settle each one.", plural(open, "promise"), isAre(open))
		}
		return either("%s finished, but %d of %d promises %s not met. Settle each one.", open, of, isAre(open))
	},
	Why: func(f Facts) string {
		if f.Settled {
			return "Until it closes, the plan holds production deploys."
		}
		return untilSettled(len(promisesOpen(f)))
	},
	Story: func(f Facts) Story {
		p, ok := promiseOn(f)
		if !ok {
			return Story{Happened: line(factString(f.Case, "not_run"))}
		}
		s := Story{Planned: promisedLine(p)}
		switch state, run := promiseState(f, p); state {
		case stateJudged:
			s.Happened = verdictLine(p)
		case stateHuman:
			s.Left = "No human judged it yet."
		case stateRunning:
			s.Happened, s.Left = fmt.Sprintf("The tests run now, as %s asked at %s.", askedBy(run.By), runStamp(run.At)), "Each result lands on its promise."
		case stateNoRoom:
			s.Happened = fmt.Sprintf("The last card had no room under its spend limit for the tests %s asked for, so they did not run.", askedBy(run.By))
			s.Left = "A new card brings a budget of its own."
		case stateCut:
			s.Happened = line(fmt.Sprintf("The tests %s asked for at %s stopped before they finished: %s.", askedBy(run.By), runStamp(run.At), cmp.Or(run.Why, "no reason was recorded")))
			s.Left = "No test checked it."
		case stateRanPast:
			s.Happened, s.Left = fmt.Sprintf("The tests ran at %s, but left no result for it.", runStamp(run.At)), "No test checked it."
		case stateRunnable:
			s.Happened, s.Left = line(factString(f.Case, "not_run")), "No test checked it."
		case stateNoTest:
			s.Happened, s.Left = line(factString(f.Case, "not_run")), "No test exists for it yet."
		case stateGapOpen:
			s.Happened = "No one judged it yet."
			if gateKind(p.Kind) {
				s.Happened = "No test checked it yet."
			}
		}
		return s
	},
	Answers: func(f Facts) []string {
		p, ok := promiseOn(f)
		waive, human := ActAnswerCriteria+"/waive", []string{ActAnswerCriteria, ActAnswerCriteria + "/not_met"}
		switch {
		case !ok && f.Settled:
			return []string{AnswerClosePlan, ActNote}
		case !ok && f.ApprovalKind == ApprovalCriteriaRerun && len(f.Runnable) > 0:
			return []string{ActRunCriteria, waive}
		case !ok:
			return []string{ActGrantCriteria, waive}
		}
		switch state, _ := promiseState(f, p); state {
		case stateHuman:
			return append(human, waive)
		case stateRunning:
			return []string{waive, ActNote}
		case stateRunnable, stateCut, stateRanPast:
			return []string{ActRunCriteria, waive}
		case stateJudged, stateGapOpen:
			if p.Kind == CriterionHuman {
				return []string{ActGrantCriteria, waive, ActAnswerCriteria}
			}
		}
		return []string{ActGrantCriteria, waive}
	},
}

// heldPlanFace is the ask of a plan a human closed, by its card or by its
// issue, while its promises were open: close it and drop them, or keep it
// open, holding production deploys. A close that drops none, as the tests on main met
// them all, takes no reason.
var heldPlanFace = FaceSpec{
	Ask: func(f Facts) string {
		if dropsNone(f) {
			return firstAsk(fmt.Sprintf("Close %s now?", askedPlan(f)), "Close this plan now?")
		}
		n := plural(len(factStrings(f.Case, "open")), "open promise")
		return firstAsk(fmt.Sprintf("Close %s and drop its %s?", askedPlan(f), n), fmt.Sprintf("Close this plan and drop its %s?", n))
	},
	Why: fixed("Until it closes, the plan holds production deploys."),
	Story: func(f Facts) Story {
		promises := slices.Clone(factStrings(f.Case, "promises"))
		for i, p := range promises {
			promises[i] = endSentence(p)
		}
		s := Story{Planned: cmp.Or(line("The plan promised: "+strings.Join(promises, " ")), "The plan's open promises stand in its full brief."),
			Happened: "A human closed the plan while these promises were open."}
		switch met, open := len(factStrings(f.Case, "met_on_main")), len(factStrings(f.Case, "open")); {
		case met > 0 && met == open:
			s.Left = "The last run of the plan's tests on main met all of them, so closing drops none."
		case met > 0:
			s.Left = fmt.Sprintf("The last run of the plan's tests on main met %d of them, so closing drops only the other %d.", met, open-met)
		}
		return s
	},
	Answers: func(f Facts) []string {
		if dropsNone(f) {
			return []string{AnswerClosePlan, ActSettle + "/" + variantKeepOpen}
		}
		return []string{AnswerDropAndEnd, ActSettle + "/" + variantKeepOpen}
	},
}

// dropsNone says the close of the held plan on f drops no promise: the
// last run of its tests on main met every open one.
func dropsNone(f Facts) bool {
	met := len(factStrings(f.Case, "met_on_main"))
	return met > 0 && met == len(factStrings(f.Case, "open"))
}

// HeldPlanComment is the words on the issue of plan t, held with its open
// promises (heldPlanFace): the ask, its why, what happened with each open
// promise on a line of its own, as the issue holds no full brief, what a
// close drops where the tests on main met some, and the answers an
// approver gives on the board.
func HeldPlanComment(c Case, t Task) string {
	face := FaceOf(c, Facts{CaseFacts: FactsOf(c), Card: t, Case: c})
	happened := strings.TrimSuffix(face.Story.Happened, ".") + ":"
	for _, p := range factStrings(c, "promises") {
		happened += "\n- " + endSentence(p)
	}
	answers := "The answers on the board:"
	for _, a := range face.Answers {
		answers += "\n- " + a.Label + ": " + a.Effect
	}
	parts := []string{face.Ask + " " + face.Why, happened}
	if face.Story.Left != "" {
		parts = append(parts, face.Story.Left)
	}
	return strings.Join(append(parts, answers), "\n\n")
}

// The states of a promise on its page (promiseState).
const (
	stateJudged   = iota // a verdict stands: not met, by a test, a model or a human
	stateHuman           // a human's promise with no verdict yet
	stateNoTest          // no result, and no test exists for it, whatever a run did
	stateRunning         // a run of the tests a human asked for is on its way
	stateNoRoom          // that run found no room under the card's spend limit
	stateCut             // that run stopped before it finished
	stateRanPast         // that run ran and left it no result
	stateRunnable        // no result, and a run of the factory can check it
	stateGapOpen         // no result on a gap's page: the gate left none
)

// promiseState is what is true of promise p on its page of request f, with
// the newest run of the plan's tests a human asked for when it decides: a
// run speaks only of a promise a run can check.
func promiseState(f Facts, p PromiseFact) (int, RunFact) {
	run, asked := runOn(f)
	switch {
	case p.Verdict != "":
		return stateJudged, run
	case f.ApprovalKind != ApprovalCriteriaRerun:
		return stateGapOpen, run
	case p.Kind == CriterionHuman:
		return stateHuman, run
	case !slices.Contains(f.Runnable, p.ID):
		return stateNoTest, run
	case asked && (run.Outcome == gateAsked || run.Outcome == gateStarted):
		return stateRunning, run
	case asked && run.Outcome == gateNoRoom:
		return stateNoRoom, run
	case asked && run.Outcome == gateCut:
		return stateCut, run
	case asked && run.Outcome == gateDone:
		return stateRanPast, run
	}
	return stateRunnable, run
}

// askedBy names who asked for a run: a human by their name, the project
// agent as itself.
func askedBy(by string) string {
	if by == ChatUser {
		return "the project agent"
	}
	return by
}

// runStamp is a run's time as a face names it.
func runStamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") + " UTC" }

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
		return fmt.Sprintf("Rent a %s server for %s at about %s EUR?", r.ServerType, spanWords(time.Duration(r.LeaseMinutes)*time.Minute), textutil.EUR(r.ReservationEUR))
	},
	Why: func(f Facts) string {
		if p := resourceOf(f).Purpose; p != "" {
			return cmp.Or(whyOf("The card says what the server is for: "+p), "The card needs a server it may not rent without a human.")
		}
		return "The card needs a server it may not rent without a human."
	},
	Answers: keys(AnswerApprove, AnswerDeny),
}

// HumanMergeEffect is the effect of a human card's Merge it at head, the
// pull request's head the human reads beside it, "" before it is read.
func HumanMergeEffect(head string) string {
	if head == "" {
		return "Merges the pull request as you. The card ends."
	}
	return "Merges commit " + ShortSHA(head) + " as you. The card ends."
}

// WorkDoneAsk is a human card's ask where neither its stored ask nor its
// title reads as one.
const WorkDoneAsk = "Is the work of this card done?"

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
		return WorkDoneAsk
	},
	Why: func(f Facts) string {
		return cmp.Or(depWait(f), whyOf(factString(f.Case, "why")), whyOf(f.Card.Why), "Only a human can do this work.")
	},
	Story: func(f Facts) Story {
		return Story{Planned: line(title(f)), Happened: line(factString(f.Case, "happened"))}
	},
	Answers: humanAnswers,
	Suggest: func(f Facts) (string, string, string) {
		if factString(f.Case, "resolved_at") != "" {
			// an alert that resolved leaves nothing on the card to wait for
			return choicePrefix + AnswerNoted, "the factory", "The alert no longer fires."
		}
		return "", factString(f.Case, "recommended_by"), factString(f.Case, "recommendation")
	},
}

// humanAnswers are the keys of the choices a human card's ask stores; a
// question that stores none takes the human's words.
func humanAnswers(f Facts) []string {
	out := []string{}
	for _, c := range storedChoices(f.Case) {
		out = append(out, choicePrefix+c.Value)
	}
	if len(out) == 0 && cmp.Or(factString(f.Case, "ask"), f.Case.Problem) != "" {
		out = append(out, ActNote+"/"+variantOwnWords)
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
		key = KeyPark
	case v == AnswerDismiss:
		key = ActClose + "/" + variantDismiss
	case v == AnswerRetryRoll && f.Card.Source == SourceRollFailed:
		key = ActRetry + "/" + variantRoll
	case strings.HasPrefix(v, AnswerReplanOnTop+":"):
		key = ActReplan + "/" + variantHeld
	case strings.HasPrefix(v, AnswerTakeOffMain+":"):
		key = ActReplan + "/" + variantOffMain
	case v == AnswerClose, v == AnswerGo, v == AnswerRerunAgain, v == AnswerCheckAgain, v == ActDeployNow:
		key = v
	case !offerable(c.Label):
		return Answer{}, false
	default:
		return Answer{Action: v, Label: c.Label, Effect: "Posts these words as the answer and closes the card.", Value: v}, true
	}
	a, ok := AnswerFor(key, f)
	a.Value = value
	if plan := planRef.FindString(c.Label); plan != "" && a.Action == ActReplan {
		words := heldPlanWords(f.Case, plan)
		if owned := "the plan's merged cards"; strings.Contains(a.Effect, owned) {
			a.Effect = strings.Replace(a.Effect, owned, "the merged cards of "+words, 1)
		} else {
			a.Effect = strings.Replace(a.Effect, "the plan", words, 1)
		}
	}
	return a, ok
}

// heldPlanWords names the plan ref, "#N", of a held production's case c,
// by the name the case's facts hold for it.
func heldPlanWords(c Case, ref string) string {
	var plans []HoldPlan
	if raw, err := json.Marshal(c.Facts["plans"]); err == nil {
		_ = json.Unmarshal(raw, &plans)
	}
	for _, p := range plans {
		if fmt.Sprintf("#%d", p.Issue) == ref {
			return PlanWords(p.Name, p.Issue)
		}
	}
	return "plan " + ref
}

// planRef finds the plan a held production's replan names in its label.
var planRef = regexp.MustCompile(`#\d+`)

// choiceValue is the value a human card's choice of answer a posts.
func choiceValue(a Answer) string {
	switch a.Key() {
	case KeyPark:
		return AnswerPark
	case ActClose + "/" + variantDismiss:
		return AnswerDismiss
	case ActClose + "/" + variantDone:
		return AnswerDone
	case ActClose + "/" + variantNoted:
		return AnswerNoted
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

// endedStory is the story of a flag on an ended card: what its sessions
// spent, and no session left to run.
func endedStory(f Facts) Story {
	return Story{Happened: line(spentLine(f))}
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
// settle is the variant of the answer that leaves things as they are, and
// story the flag's story, flagStory where it names none.
type flagFace struct {
	fact         func(Facts) string
	why, fitting string
	question     string
	notice       bool
	settle       string
	story        func(Facts) Story
}

func flagFaces() map[string]FaceSpec {
	const replanQuestion = "Send the plan back to the planner?"
	unjudged := "A check judges each session, and a session no check judged may have gone wrong unseen."
	out := map[string]FaceSpec{}
	for flag, ff := range map[string]flagFace{
		"answer_changed_nothing": {notice: true, fact: fixed("This card's last answer changed nothing."),
			why: "The card stopped again where it stopped before, and its other question asks how to go on."},
		"budget_moved_no_event": {fact: budgetMoved, why: "Every change of a card's budget should leave a record."},
		"checkin_cut":           {notice: true, fact: fixed("This card's last session went unjudged because its check ended early."), why: unjudged},
		"checkin_refused":       {notice: true, fact: fixed("This card's last session went unjudged because its check was refused."), why: unjudged},
		"criterion_unproven": {fact: fixed("A promise of the plan has no test result yet."), why: "A plan closes only once each promise has a test result.",
			fitting: ActRunCriteria, question: "Run the plan's tests now?"},
		"dependency_failed": {fact: fixed("A card this one waits for failed."), why: "This card cannot start while a card it waits for stays failed.",
			fitting: ActRetry, question: "Try it again?"},
		"faced_unjudged": {notice: true, fact: fixed("This card's question reached a human before the factory's agents judged it."),
			why: "The factory's agents judge each question first, and only a change to the factory's code repairs one that skipped them."},
		"finding_confirmed_twice": {fact: fixed("The review found the same fault twice on this card."),
			why: "A fault the review finds twice may need a fresh start, not another session.", fitting: ActSalvage, question: "Rescue the work into a new card?"},
		"grant_bought_nothing": {notice: true, fact: fixed("This card's last session, which a human or the project agent allowed, changed nothing."),
			why: "The session pushed no change and moved no check, so another one may not help."},
		"issue_open_after_end": {fact: fixed("This card ended, and its issue is still open."),
			why:     "An open issue says the work is still to do, though no card works on it any more.",
			fitting: ActClose + "/" + variantIssue, question: "Close the issue?", settle: ActSettle + "/" + variantOpen, story: endedStory},
		"opened_proof_changed": {fact: fixed("A test of the plan changed after the plan was confirmed."),
			why: "A plan's test should not change once the plan is confirmed.", fitting: ActReplan, question: replanQuestion},
		"opened_proof_red_twice": {fact: fixed("A test of the plan failed on two versions of this card's work."),
			why: "A test that fails twice may test the wrong thing.", fitting: ActReplan, question: replanQuestion},
		"proofs_missing": {fact: fixed("The plan's promises have no tests yet."), why: "A plan's promises need tests before its cards finish.",
			fitting: ActReplan, question: replanQuestion},
		"prover_refused_thrice": {fact: fixed("No test could be written for one of the plan's promises."),
			why: "The agent that writes the plan's tests refused this promise three times.", fitting: ActReplan, question: replanQuestion},
		"queued_over_bound": {fact: waitedFact, why: "A card that waits this long in the queue may never get its turn."},
		"review_refused_not_run": {notice: true, fact: fixed("This card cannot merge yet because the review did not run on its newest work."),
			why: "The review must run on a card's newest work before the card merges."},
		"session_no_push": {notice: true, fact: fixed("This card's last sessions changed no code."),
			why: "A session that changes no code spends money and moves the work no closer to done."},
		"wait_over_bound": {fact: waitedFact, why: "A card that waits this long may be stuck.", fitting: ActSalvage, question: "Rescue the work into a new card?"},
	} {
		fits := func(f Facts) bool {
			a, ok := AnswerFor(ff.fitting, f)
			return ff.fitting != "" && ok && answerHolds(a, f)
		}
		if ff.story == nil {
			ff.story = flagStory
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
			Story: ff.story,
			Answers: func(f Facts) []string {
				if ff.notice {
					return []string{ActSettle + "/" + variantNoted, ActNote}
				}
				return append(holding(f, ff.fitting), cmp.Or(ff.settle, ActSettle), ActNote)
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
	return fmt.Sprintf("This card's budget reads %s, but its records add up to %s.", reads, records)
}

// waitedFact is a wait flag's fact: what the card waits for, and that the
// wait lasted more than its limit, which stays true while it lasts.
func waitedFact(f Facts) string {
	wait, over := factString(f.Case, "wait"), cmp.Or(boundWords(f), "too long")
	if wait == "run_quiet" {
		return "This card's run has not moved or answered for " + over + "."
	}
	what, ok := waitWords[wait]
	if !ok {
		what = "its turn"
	}
	return "This card has waited " + over + " for " + what + "."
}

// waitWords say what a card waits for in each kind of wait.
var waitWords = map[string]string{"queued": "a free place to run", "dependency": "the cards it depends on", "gate": "a check of its work",
	"merge_retry": "its merge to be tried again", "provider_backoff": "the model provider", "provider_recheck": "the model provider",
	"provider_limit": "the model provider", "rung_answer": "the factory to decide how it goes on", "checks": "its checks"}

// boundWords is "more than <the wait's bound>" in words, "" when the case
// names none.
func boundWords(f Facts) string {
	d, err := time.ParseDuration(factString(f.Case, "bound"))
	if err != nil || d <= 0 {
		return ""
	}
	return "more than " + spanWords(d)
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
