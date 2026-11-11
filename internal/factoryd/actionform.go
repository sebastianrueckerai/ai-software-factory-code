package factoryd

import (
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// ActionEffects say in one line what each action does, as the human's
// button states it.
var ActionEffects = map[string]string{
	ActResume:           "Start the next session on the card's own model.",
	ActContinue:         "Start a new attempt with your note as its guidance.",
	ActHandover:         "Hand the work to a session on the model you pick.",
	ActRouteApproval:    "Send the head to its merge approval as it stands.",
	ActReverify:         "Run the gates again on the head and judge the findings anew.",
	ActRerunChecks:      "Run the red checks again on the same head.",
	ActWaitChecks:       "Wait on the stopped checks once more, with no session.",
	ActExtend:           "Raise the card's sessions, agent time or lines.",
	ActFund:             "Raise the card's budget by the amount in the fund field.",
	ActRaiseCeiling:     "Raise the card's ceiling by the amount in the fund field.",
	ActSuccessor:        "End this card and start a successor on its branch.",
	ActRetry:            "File the failed dependency again as a new card.",
	ActReplan:           "Send the card, or a failed dependency's work, back to the planner.",
	ActClose:            "End the card and close its issue.",
	ActDeployNow:        "Release the held production now.",
	ActSettle:           "End the flag's case; the card goes on.",
	ActWithdraw:         "Withdraw the case.",
	ActNote:             "Record your note and keep the case open.",
	ActApproveMerge:     "Approve the pull request; factoryd merges it.",
	ActDenyMerge:        "Return the pull request to its agent, or reject it, with your note as the reason.",
	ActSetDependency:    "Make the card wait for the card you name.",
	ActRemoveDependency: "Take the card's dependency on the card you name away.",
	ActFileCard:         "File the root cause outside this card as a card of its own.",
	ActRestartPlan:      "End the plan and file it anew, your note the shape to take.",
	ActAnswerCriteria:   "Record a verdict on one criterion of the plan.",
}

// ActionArg is one input of an action's arguments: its key, its caption and
// what it takes, a select's options, a number or free text.
type ActionArg struct {
	Name, Label string
	Options     []string
	Number      bool
}

// ActionArgs are the arguments each action takes beside the fund and the
// note, as its form asks for them. A fund's and a ceiling raise's amount
// is the fund field's own, and extend's minutes are agent_time_s in
// minutes.
var ActionArgs = map[string][]ActionArg{
	ActHandover:         {{Name: "model", Label: "model", Options: handoverModels}},
	ActExtend:           {{Name: "sessions", Label: "sessions more", Number: true}, {Name: "minutes", Label: "minutes of agent time more", Number: true}, {Name: "lines", Label: "lines more", Number: true}},
	ActSetDependency:    {{Name: "depends_on", Label: "waits for (card)"}},
	ActRemoveDependency: {{Name: "depends_on", Label: "no longer waits for (card)"}},
	ActFileCard:         {{Name: "title", Label: "title"}, {Name: "body", Label: "description"}, {Name: "size", Label: "size", Options: append([]string{""}, policy.CardSizes...)}},
	ActAnswerCriteria:   {{Name: "criterion", Label: "criterion"}, {Name: "verdict", Label: "verdict", Options: []string{"met", "not_met", "waive"}}},
	ActDenyMerge:        {{Name: "action", Label: "denial", Options: []string{"request_changes", "reject"}}},
}

// ActionLabel is action name as its button reads: "raise_ceiling" is
// "Raise ceiling".
func ActionLabel(name string) string {
	s := strings.ReplaceAll(name, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// ActionRefusal is why an action the rung may pick does not apply on a case.
type ActionRefusal struct{ Action, Why string }

// ActionRefusals are the actions rung may pick that Offered leaves out on facts f,
// each with the table's reason, in table order.
func ActionRefusals(f CaseFacts, rung int) []ActionRefusal {
	offered := Offered(f, rung)
	var out []ActionRefusal
	for _, a := range orderedActions(f) {
		if !a.Rungs[rung] || slices.ContainsFunc(offered, func(o Action) bool { return o.Name == a.Name }) {
			continue
		}
		p := probe(a, f)
		if why := a.refusal(f, rung, Resolution{Action: a.Name, Args: p.Args, Note: p.Note}); why != "" {
			out = append(out, ActionRefusal{a.Name, why})
		}
	}
	return out
}
