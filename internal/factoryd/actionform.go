package factoryd

import (
	"encoding/json"
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
	ActRaiseCeiling:     "Raise the card's ceiling by the amount you enter.",
	ActSuccessor:        "End this card and start a successor on its branch.",
	ActRetry:            "File the failed dependency again as a new card.",
	ActReplan:           "Send the card, or a failed dependency's work, back to the planner.",
	ActClose:            "End the card and close its issue.",
	ActDeployNow:        "Release the held production now.",
	ActSettle:           "End the flag's case; the card goes on.",
	ActSalvage:          "End the stuck run, close the card and start its successor from main's tip with its findings.",
	ActWithdraw:         "Withdraw the case.",
	ActNote:             "Record your note and keep the case open.",
	ActApproveMerge:     "Approve the pull request; factoryd merges it.",
	ActDenyMerge:        "Return the pull request to its agent, or reject it, with your note as the reason.",
	ActSetDependency:    "Make the card wait for the card you name.",
	ActRemoveDependency: "Take the card's dependency on the card you name away.",
	ActFileCard:         "File the root cause outside this card as a card of its own.",
	ActRestartPlan:      "End the plan and file it anew, your note the shape to take.",
	ActAnswerCriteria:   "Record a verdict on one criterion of the plan.",
	ActGrantCriteria:    "Grant one more bounded card for the open criteria.",
	ActConfirmCriteria:  "Confirm the delivered cards meet every open criterion; your note is the evidence.",
	ActDenyCriteria:     "Deny the request, your note the reason; waive a criterion instead if it should not hold.",
}

// ActionArg is one input of an action's arguments: its key, its caption and
// what it takes, a select's options, a number or free text.
type ActionArg struct {
	Name, Label string
	Options     []string
	Number      bool
	Float       bool // a decimal amount
}

// ActionArgs are the arguments each action takes beside the fund and the
// note, as its form asks for them. A fund's and a ceiling raise's amount
// is the fund field's own, and extend's minutes are agent_time_s in
// minutes.
var ActionArgs = map[string][]ActionArg{
	ActRaiseCeiling:     {{Name: "eur", Label: "raise the ceiling by, EUR", Number: true, Float: true}},
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
type ActionRefusal struct {
	Action string `json:"action"`
	Why    string `json:"why"`
}

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

// AskOffer is one button a face stores: a table action by name, its label
// and its effect in a line. The ask's detail keeps them under offers, after the
// question's own choices, which carry no effect.
type AskOffer struct {
	Value  string `json:"value"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
}

// anyCase are the actions that apply on every case: a close, a note, an
// answer in words and a replan.
var anyCase = []string{ActClose, ActNote, ActContinue, ActReplan}

// OffersFor are the buttons the human's face stores for a case on its live
// facts f: Offered at the human's rung, each with its label and effect line;
// and, when only the actions every case takes remain, the refusals of the
// others. A face stores them when the case reaches the human and again when
// Act refuses a press on it, and a render reads them as stored.
func OffersFor(f CaseFacts) ([]AskOffer, []ActionRefusal) {
	actions := Offered(f, 3)
	var out []AskOffer
	for _, a := range actions {
		out = append(out, AskOffer{Value: a.Name, Label: ActionLabel(a.Name), Effect: ActionEffects[a.Name]})
	}
	var refusals []ActionRefusal
	if !slices.ContainsFunc(actions, func(a Action) bool { return !slices.Contains(anyCase, a.Name) }) {
		refusals = ActionRefusals(f, 3)
	}
	return out, refusals
}

// AnswersByOffers reports whether the face of case c on a card of kind cardKind stores table
// offers: not a human card's, whose question's choices answer it, and not a
// merge's or a plan's criteria, whose approval card is their face, nor a
// monitor's flag.
func AnswersByOffers(c Case, cardKind string) bool {
	k := c.Kind
	return !ChoiceCase(c, cardKind) && k != CaseMergeApproval && k != CasePlanCriteria && k != CaseMonitorFlag &&
		k != CasePlanQuestion && k != CasePlanConfirmation
}

// storeOffers are the offers and refusals of case c on facts f as the ask's
// detail keeps them; none for a case whose face is not the table's.
func storeOffers(c Case, t Task, f CaseFacts) (offers []AskOffer, refusals []ActionRefusal, ok bool) {
	if c.ID == 0 || !AnswersByOffers(c, t.Kind) {
		return nil, nil, false
	}
	offers, refusals = OffersFor(f)
	return offers, refusals, true
}

// withTableOffers are an ask's own choices (the offers in own that carry no
// effect line) followed by the table's offers.
func withTableOffers(own any, table []AskOffer) []AskOffer {
	var kept []AskOffer
	if b, err := json.Marshal(own); err == nil {
		_ = json.Unmarshal(b, &kept)
	}
	kept = slices.DeleteFunc(kept, func(o AskOffer) bool { return o.Effect != "" })
	return slices.Concat(kept, table)
}
