package factoryd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const planAgentStep = " Record your analysis and recommended option with attach_analysis."

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
		return fmt.Sprintf("Rung %d decides no plan answer: it is rung 2's at full auto, or the human's, made on the plan's card.", rung)
	case autonomy == "":
		return "The project's autonomy level could not be read, so a plan's answer stays the human's, made on the plan's card with its choices, Confirm or Request changes." + planAgentStep
	case autonomy != policy.AutonomyFullAuto && confirming:
		return "A plan confirmation below full auto is the human's, made with the plan's Confirm button." + planAgentStep
	case autonomy != policy.AutonomyFullAuto:
		return "A plan's answer below full auto is the human's, made on the plan's card with its choices or its Request changes button." + planAgentStep
	case confirming && takesOffMain:
		return "This revision takes merged work off main, or its reverts could not be read, so its confirmation is the human's, made with the plan's Confirm button." + planAgentStep
	}
	return ""
}

// OffMainReplanRefusal is the rule for a replan that takes the plan's merged
// work off main: only the human takes it, at every level, as a confirmation
// of a revision that takes work off main is only the human's. "" for rung 3.
func OffMainReplanRefusal(rung int) string {
	return offMainRefusal(rung, "A replan that takes merged work off main is the human's, made with the Replan button's off-main box.")
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
// agent may do with the answer tool, as PlanAnswerRefusal allows it.
func planOffer(c Case, autonomy string, takesOffMain, holdoutsOpen bool) string {
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
	if confirmWhy == "" && holdoutsOpen {
		confirmWhy = "Hold-out candidates of this revision are undecided, and only the human okays or drops them, so its confirmation waits for them."
	}
	switch {
	case confirmWhy == "":
		return fmt.Sprintf("You may confirm this plan with the answer tool (%s, decision confirm), or request changes (decision revise, with your words).", at)
	case reviseWhy == "":
		return confirmWhy + fmt.Sprintf(" You may request changes with the answer tool (%s, decision revise, with your words).", at)
	}
	return confirmWhy
}
