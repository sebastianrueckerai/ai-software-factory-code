package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const planAgentStep = " Record your analysis and the option you recommend (`attach_analysis`)."

// PlanAnswerRefusal is the one rule for who answers a plan's questions and
// confirms its revision. rung is who decides: 3 the human, who always
// passes; 2 the agent, which passes a plan answer (a question or Request
// changes) only at full auto and a confirmation only at full auto on a
// revision that takes nothing off main; any other rung never decides one.
// It answers the refusal in words that name the state, the human's press
// and the agent's next step, "" when the decider may. autonomy "" is a
// level that could not be read. confirming is the decision confirm.
func PlanAnswerRefusal(rung int, autonomy string, confirming, takesOffMain bool) string {
	switch {
	case rung == 3:
		return ""
	case rung != 2:
		return capitalize(who(rung)) + " decides no plan answer: it is the factory agent's at full auto, or the human's, made on the plan's card."
	case autonomy == "":
		return "The project's autonomy level could not be read, so a plan's answer stays the human's, made on the plan's card with its choices, Confirm the plan or Ask for changes." + planAgentStep
	case autonomy != policy.AutonomyFullAuto && confirming:
		return "A plan confirmation below full auto is the human's, made with Confirm the plan on the plan's card." + planAgentStep
	case autonomy != policy.AutonomyFullAuto:
		return "A plan's answer below full auto is the human's, made on the plan's card with its choices or its Ask for changes." + planAgentStep
	case confirming && takesOffMain:
		return "This revision takes merged work off main, or its reverts could not be read, so its confirmation is the human's, made with Confirm the plan on the plan's card." + planAgentStep
	}
	return ""
}

// HoldoutRefusal is PlanAnswerRefusal's rule for keeping or dropping one
// extra promise of a plan's round: the human always, the factory agent at
// full auto, any other rung never. "" when the decider may.
func HoldoutRefusal(rung int, autonomy string) string {
	if PlanAnswerRefusal(rung, autonomy, false, false) == "" {
		return ""
	}
	const press = "kept or dropped with Keep it or Drop it on its page of the plan's card."
	switch {
	case rung != 2:
		return capitalize(who(rung)) + " decides no extra promise: it is the factory agent's at full auto, or the human's, " + press
	case autonomy == "":
		return "The project's autonomy level could not be read, so an extra promise stays the human's, " + press + planAgentStep
	}
	return "The project runs at " + autonomy + ", and below full auto an extra promise is the human's, " + press + planAgentStep
}

// OffMainReplanRefusal is the rule for a replan that takes the plan's merged
// work off main: only the human takes it, at every level, as a confirmation
// of a revision that takes work off main is only the human's. "" for rung 3.
func OffMainReplanRefusal(rung int) string {
	return offMainRefusal(rung, "A replan that takes merged work off main is the human's, made with the off-main box of the card's Send back to the planner.")
}

// OffMainChoiceRefusal is OffMainReplanRefusal for the held-production ask's
// choice that takes plan #issue off main, which the human makes with that
// choice on the card.
func OffMainChoiceRefusal(rung int, issue int64) string {
	return offMainRefusal(rung, fmt.Sprintf("Taking plan #%d off main is the human's, made with this card's choice \"Take plan #%d off main and replan\".", issue, issue))
}

func offMainRefusal(rung int, why string) string {
	switch rung {
	case 3:
		return ""
	case 2:
		return why + planAgentStep
	}
	return why
}

// RevisionTakesOffMain says revision n of plan taskID names merged cards to
// take off main. A revision that cannot be read counts as one that does.
func RevisionTakesOffMain(ctx context.Context, s planRevisions, taskID string, n int) (bool, error) {
	rev, ok, err := s.PlanRevision(ctx, taskID, n)
	if err != nil || !ok {
		return true, err
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return true, fmt.Errorf("revision %d of %s: %w", n, taskID, err)
	}
	return len(p.TakeOffMain) > 0, nil
}

// planOffer is the line of rung 2's statement on a plan case: what the
// agent may do with the answer tool, as PlanAnswerRefusal allows it, and
// on a confirmation whose extra promises open wait, each of them with its
// claim, its inputs and what it expects where HoldoutRefusal lets the agent
// keep or drop it.
func planOffer(c Case, autonomy string, takesOffMain bool, open []PlanProof) string {
	revision, _ := toInt(c.Facts["revision"])
	index, _ := toInt(c.Facts["index"])
	at := fmt.Sprintf("revision %d, question %d", revision, index)
	if c.Kind == CasePlanQuestion {
		if why := PlanAnswerRefusal(2, autonomy, false, false); why != "" {
			return why
		}
		return fmt.Sprintf("You may answer this question with the answer tool (%s).", at)
	}
	confirmWhy := PlanAnswerRefusal(2, autonomy, true, takesOffMain)
	reviseWhy := PlanAnswerRefusal(2, autonomy, false, false)
	var settle string
	if len(open) > 0 && HoldoutRefusal(2, autonomy) == "" {
		settle = holdoutOffer(revision, open)
	}
	switch {
	case confirmWhy == "" && settle != "":
		return settle + fmt.Sprintf("Once each is kept or dropped, you may confirm this plan with the answer tool (%s, decision confirm), or request changes (decision revise, with your words).", at)
	case confirmWhy == "":
		return fmt.Sprintf("You may confirm this plan with the answer tool (%s, decision confirm), or request changes (decision revise, with your words).", at)
	case reviseWhy == "":
		return settle + confirmWhy + fmt.Sprintf(" You may request changes with the answer tool (%s, decision revise, with your words).", at)
	}
	return confirmWhy
}

// holdoutOffer is the part of rung 2's statement that lists the extra
// promises open of revision, each by its number with its claim, its inputs
// and what it expects, and how the agent keeps or drops one.
func holdoutOffer(revision int, open []PlanProof) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This revision's extra promises wait to be kept or dropped, and its confirmation waits for them: each is a claim the plan's criteria missed, which a test the building agents never see checks once it is kept. "+
		"Keep or drop each with the answer tool (revision %d, holdout its number, decision %s or %s, your reason as text).\n", revision, DecisionKeep, DecisionDrop)
	for _, p := range open {
		fmt.Fprintf(&b, "- holdout %d: %s\n  inputs: %s\n  expected: %s\n", p.Idx, p.Claim, p.Inputs, p.Expected)
	}
	return b.String()
}
