package factoryd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// PlanAnswerRefusal is the one rule for who answers a plan's questions and
// confirms its revision: a human always may; the agent only at full auto,
// and never confirms a revision that takes merged work off main. It answers
// the refusal in words that name the human's press, "" when decider may.
// confirming is the decision confirm; takesOffMain says the revision names
// work to take off main.
func PlanAnswerRefusal(byAgent bool, autonomy string, confirming, takesOffMain bool) string {
	switch {
	case !byAgent:
		return ""
	case autonomy != policy.AutonomyFullAuto && confirming:
		return "A plan confirmation below full auto is the human's, made with the plan's Confirm button."
	case autonomy != policy.AutonomyFullAuto:
		return "A plan's answer below full auto is the human's, made on the plan's card with its choices or its Request changes button."
	case confirming && takesOffMain:
		return "This revision takes merged work off main, so its confirmation is the human's, made with the plan's Confirm button."
	}
	return ""
}

// RevisionTakesOffMain says revision n of plan taskID names merged cards to
// take off main.
func RevisionTakesOffMain(ctx context.Context, s planRevisions, taskID string, n int) (bool, error) {
	rev, ok, err := s.PlanRevision(ctx, taskID, n)
	if err != nil || !ok {
		return false, err
	}
	var p Plan
	if err := json.Unmarshal(rev.Plan, &p); err != nil {
		return false, fmt.Errorf("revision %d of %s: %w", n, taskID, err)
	}
	return len(p.TakeOffMain) > 0, nil
}

// planOffer is the line of rung 2's statement on a plan case: what the
// agent may do with the answer tool, as PlanAnswerRefusal allows it.
func planOffer(c Case, autonomy string, takesOffMain bool) string {
	revision, _ := toInt(c.Facts["revision"])
	index, _ := toInt(c.Facts["index"])
	at := fmt.Sprintf("revision %d, question %d", revision, index)
	if c.Kind == CasePlanQuestion {
		if why := PlanAnswerRefusal(true, autonomy, false, false); why != "" {
			return why
		}
		return fmt.Sprintf("You may answer this question with the answer tool (%s).", at)
	}
	confirmWhy := PlanAnswerRefusal(true, autonomy, true, takesOffMain)
	reviseWhy := PlanAnswerRefusal(true, autonomy, false, false)
	switch {
	case confirmWhy == "":
		return fmt.Sprintf("You may confirm this plan with the answer tool (%s, decision confirm), or request changes (decision revise, with your words).", at)
	case reviseWhy == "":
		return confirmWhy + fmt.Sprintf(" You may request changes with the answer tool (%s, decision revise, with your words).", at)
	}
	return confirmWhy
}
