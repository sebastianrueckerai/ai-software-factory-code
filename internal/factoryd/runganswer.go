package factoryd

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The rung answer contract: the JSON schema of the answer tool a rung's
// model call is forced through, the typed answer its arguments parse
// into, and the validator that refuses an answer outside the schema's
// enums. All pure functions on CaseFacts; the conversation that asks and
// the re-ask on a refusal are runRung's business.

// caseKindNames are every case kind a case is opened as, in the order
// cases.go declares them.
var caseKindNames = []string{
	string(CaseSessionCap), string(CaseAgentTimeCap), string(CaseBudgetFloor), string(CaseDayCap),
	string(CaseMonthCap), string(CaseProjectCap), string(CaseCheckinHuman), string(CaseCheckinShare), string(CaseCheckinFailed),
	string(CaseGateStop), string(CaseFindingsRepeated), string(CaseDependencyClosed), string(CaseDependencyFailed),
	string(CaseWaitExpired), string(CasePodNeverStarted), string(CaseProviderRefused), string(CaseNoAnswer),
	string(CaseContentFilter), string(CaseImageWait), string(CaseScreeningSuspect), string(CaseAgentEscalate),
	string(CaseAgentRequestInfo), string(CasePlanQuestion), string(CasePlanConfirmation), string(CaseMergeApproval),
	string(CaseHumanCard), string(CaseOther), string(CaseMonitorFlag), string(CaseJudgmentBudget),
}

// answerReasonKinds is the closed enum of an answer's reason_kind: every
// case kind, then the check-in's reason constants, the shapes of a stop
// only a human can act on. A new kind joins caseKindNames and lands here
// with it; no free text does.
var answerReasonKinds = append(slices.Clone(caseKindNames), CheckGoalConflict, CheckSpend, CheckPolicyClass, CheckAccess)

// answerDecisions are the decision enum's values: the actions Offered
// lists for rung on f, in table order.
func answerDecisions(f CaseFacts, rung int) []string {
	names := make([]string, 0, len(Actions))
	for _, a := range Offered(f, rung) {
		names = append(names, a.Name)
	}
	return names
}

// RungHandUp is the decision of a rung that cannot resolve: it takes no
// action and recommends one to the rung above.
const RungHandUp = "hand_up"

// decisionEnum is the decision enum: the offered actions, then hand_up
// for a rung that has a rung above to hand up to. The human is the last
// rung and hands up to none.
func decisionEnum(f CaseFacts, rung int) []string {
	names := answerDecisions(f, rung)
	if rung < 3 {
		names = append(names, RungHandUp)
	}
	return names
}

// actionsFrom are the names Offered lists for any rung from the given one
// to the human's, in table order.
func actionsFrom(f CaseFacts, from int) []string {
	var offered [][]Action
	for r := from; r <= 3; r++ {
		offered = append(offered, Offered(f, r))
	}
	var names []string
	for _, a := range Actions {
		if slices.ContainsFunc(offered, func(o []Action) bool {
			return slices.ContainsFunc(o, func(x Action) bool { return x.Name == a.Name })
		}) {
			names = append(names, a.Name)
		}
	}
	return names
}

// recommendable is the recommendation's action enum for decision: the
// actions of the rungs above on a hand_up, the offered ones otherwise.
func recommendable(f CaseFacts, rung int, decision string) []string {
	if decision == RungHandUp {
		return actionsFrom(f, rung+1)
	}
	return answerDecisions(f, rung)
}

// AnswerSchema derives the answer tool's JSON schema from f and rung: the
// decision an enum of the action names Offered lists for the rung and
// hand_up while a rung is above, reason_kind the closed enum
// answerReasonKinds holds, the verdicts an object keyed by the finding
// ids on f, each one of the check-in's three verdicts with its evidence,
// a note, a recommendation with its grounds whose action is one the rung
// offers or a rung above can take, an optional model for a handover, and
// an optional draw that rides beside the decision. The same facts and
// rung derive the same schema.
func AnswerSchema(f CaseFacts, rung int) map[string]any {
	verdicts := map[string]any{"type": "object", "additionalProperties": false}
	if len(f.Findings) > 0 {
		props := make(map[string]any, len(f.Findings))
		for _, id := range f.Findings {
			props[id] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"verdict":  map[string]any{"type": "string", "enum": slices.Clone(checkVerdicts)},
					"evidence": map[string]any{"type": "string", "description": "what the verdict stands on: a file and line at a ref, or a command and its output"},
				},
				"required":             []string{"verdict", "evidence"},
				"additionalProperties": false,
			}
		}
		verdicts["properties"] = props
		verdicts["required"] = slices.Clone(f.Findings)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{
				"type":        "string",
				"enum":        decisionEnum(f, rung),
				"description": "the action this rung takes, one of the offered actions, or hand_up when the rung cannot resolve",
			},
			"reason_kind": map[string]any{
				"type":        "string",
				"enum":        slices.Clone(answerReasonKinds),
				"description": "the kind of reason the answer answers",
			},
			"model": map[string]any{
				"type":        "string",
				"enum":        slices.Clone(handoverModels),
				"description": "the model of the next session, required with the decision handover",
			},
			"verdicts": verdicts,
			"note":     map[string]any{"type": "string", "description": "the note the answer hands the card"},
			"recommendation": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":  map[string]any{"type": "string", "enum": actionsFrom(f, rung), "description": "the action the answer recommends: one of the offered actions, or on hand_up one the rungs above can take"},
					"grounds": map[string]any{"type": "string", "description": "why, in plain words"},
				},
				"required":             []string{"action", "grounds"},
				"additionalProperties": false,
			},
			"draw": map[string]any{
				"type":        "object",
				"description": "the draw that rides beside the decision, from the pool",
				"properties": map[string]any{
					"sessions":     map[string]any{"type": "integer", "minimum": 0},
					"agent_time_s": map[string]any{"type": "integer", "minimum": 0},
					"eur":          map[string]any{"type": "number", "minimum": 0},
				},
				"additionalProperties": false,
			},
		},
		"required":             []string{"decision", "reason_kind", "verdicts", "note", "recommendation"},
		"additionalProperties": false,
	}
}

// RungAnswer is a rung's answer as the model's tool call parses it: the
// decision from the rung's offered actions or hand_up, its reason kind, a verdict on
// every finding keyed by the finding's id, a note, a recommendation with
// grounds, and the draw that rides beside the decision.
type RungAnswer struct {
	Decision       string                 `json:"decision"`
	ReasonKind     string                 `json:"reason_kind"`
	Verdicts       map[string]RungVerdict `json:"verdicts"`
	Model          string                 `json:"model,omitempty"`
	Note           string                 `json:"note"`
	Recommendation RungRecommendation     `json:"recommendation"`
	Draw           *RungDraw              `json:"draw,omitempty"`
}

// RungVerdict is the answer's verdict on one finding, keyed by the
// finding's id: one of the check-in's three verdicts and the evidence the
// verdict stands on.
type RungVerdict struct {
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

// RungRecommendation is the action the answer recommends with its
// grounds, for the rung above when the answer hands up.
type RungRecommendation struct {
	Action  string `json:"action"`
	Grounds string `json:"grounds"`
}

// RungDraw is the draw an answer rides beside its decision: sessions,
// agent time in seconds and money, the parts of a pool draw.
type RungDraw struct {
	Sessions   int     `json:"sessions"`
	AgentTimeS int64   `json:"agent_time_s"`
	EUR        float64 `json:"eur"`
}

// rungAnswerJSON is the answer with every required field a pointer, so a
// field the answer left out reads as absent, not as a zero value.
type rungAnswerJSON struct {
	Decision       *string                     `json:"decision"`
	ReasonKind     *string                     `json:"reason_kind"`
	Verdicts       map[string]*rungVerdictJSON `json:"verdicts"`
	Model          *string                     `json:"model"`
	Note           *string                     `json:"note"`
	Recommendation *rungRecommendationJSON     `json:"recommendation"`
	Draw           *RungDraw                   `json:"draw"`
}

type rungVerdictJSON struct {
	Verdict  *string `json:"verdict"`
	Evidence *string `json:"evidence"`
}

type rungRecommendationJSON struct {
	Action  *string `json:"action"`
	Grounds *string `json:"grounds"`
}

// handUpText closes a decision refusal with the way out of a rung that
// cannot resolve.
func handUpText(rung int) string {
	if rung >= 3 {
		return ""
	}
	return ", or " + RungHandUp + " when it cannot resolve"
}

// offeredText names the offered actions for a refusal, none when the rung
// may take no action on the facts.
func offeredText(names []string) string {
	if len(names) == 0 {
		return "no action"
	}
	return textutil.AndList(names)
}

// ParseRungAnswer reads the model's answer text against the schema
// AnswerSchema derives for rung on f, and refuses what that schema
// refuses: an answer that is not one JSON object, one that holds a field
// the schema does not have or leaves a required field out, a decision
// outside the rung's offered actions and hand_up, a recommended action
// outside the offered actions (outside the actions of the rungs above on
// a hand_up), a reason_kind outside the closed enum, a verdict on an id
// the facts do not hold, a finding without one or a null one, a verdict
// outside the check-in's three or without its evidence, a model outside
// handoverModels or left out of a handover, and a draw any part of which
// is below zero. The refusal names what the answer broke, for runRung's
// one re-ask.
func ParseRungAnswer(f CaseFacts, rung int, text string) (RungAnswer, error) {
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return RungAnswer{}, fmt.Errorf("the answer is not JSON: %w", err)
	}
	if _, ok := raw.(map[string]any); !ok {
		return RungAnswer{}, errors.New("the answer is not one JSON object")
	}
	var j rungAnswerJSON
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return RungAnswer{}, fmt.Errorf("the answer holds the field %s, which the schema does not have; the fields are decision, reason_kind, verdicts, model, note, recommendation and draw (a draw holds sessions, agent_time_s and eur; a recommendation action and grounds; a verdict verdict and evidence)", field)
		}
		return RungAnswer{}, fmt.Errorf("the answer is malformed: %w", err)
	}
	offered := answerDecisions(f, rung)
	switch {
	case j.Decision == nil:
		return RungAnswer{}, errors.New("the answer leaves decision out; the rung answers with " + offeredText(offered) + handUpText(rung))
	case j.ReasonKind == nil:
		return RungAnswer{}, errors.New("the answer leaves reason_kind out")
	case j.Verdicts == nil:
		return RungAnswer{}, errors.New("the answer leaves verdicts out; with no findings it is an empty object")
	case j.Note == nil:
		return RungAnswer{}, errors.New("the answer leaves note out")
	case j.Recommendation == nil:
		return RungAnswer{}, errors.New("the answer leaves recommendation out")
	}
	if enumCheck("decision", *j.Decision, decisionEnum(f, rung)) != nil {
		return RungAnswer{}, fmt.Errorf("decision %q is not offered at this rung: the rung answers with %s%s", *j.Decision, offeredText(offered), handUpText(rung))
	}
	if enumCheck("reason_kind", *j.ReasonKind, answerReasonKinds) != nil {
		return RungAnswer{}, fmt.Errorf("reason_kind %q is outside the closed enum of the case kinds and the check reasons", *j.ReasonKind)
	}
	ids := make(map[string]bool, len(f.Findings))
	for _, id := range f.Findings {
		ids[id] = true
	}
	var unknown []string
	for id := range j.Verdicts {
		if !ids[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		held := strings.Join(f.Findings, ", ")
		if held == "" {
			held = "none"
		}
		return RungAnswer{}, fmt.Errorf("the answer verdicts %s, which the facts do not hold; the findings are %s", textutil.AndList(unknown), held)
	}
	var missing []string
	for _, id := range f.Findings {
		if _, ok := j.Verdicts[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return RungAnswer{}, fmt.Errorf("the answer holds no verdict on %s; every finding needs one", textutil.AndList(missing))
	}
	for _, id := range f.Findings {
		v := j.Verdicts[id]
		switch {
		case v == nil:
			return RungAnswer{}, fmt.Errorf("the verdict on %s is null; give it as an object with verdict and evidence", id)
		case v.Verdict == nil:
			return RungAnswer{}, fmt.Errorf("the verdict on %s leaves verdict out", id)
		case v.Evidence == nil:
			return RungAnswer{}, fmt.Errorf("the verdict on %s leaves evidence out", id)
		case enumCheck("verdict", *v.Verdict, checkVerdicts) != nil:
			return RungAnswer{}, fmt.Errorf("the verdict %q on %s is not one of %s", *v.Verdict, id, textutil.AndList(checkVerdicts))
		}
	}
	if j.Recommendation.Action == nil {
		return RungAnswer{}, errors.New("the recommendation leaves action out")
	}
	if j.Recommendation.Grounds == nil {
		return RungAnswer{}, errors.New("the recommendation leaves grounds out")
	}
	if enumCheck("action", *j.Recommendation.Action, recommendable(f, rung, *j.Decision)) != nil {
		if *j.Decision == RungHandUp {
			return RungAnswer{}, fmt.Errorf("the recommended action %q is not an action for the rungs above: they answer with %s", *j.Recommendation.Action, offeredText(recommendable(f, rung, RungHandUp)))
		}
		return RungAnswer{}, fmt.Errorf("the recommended action %q is not offered at this rung: the rung answers with %s", *j.Recommendation.Action, offeredText(offered))
	}
	if j.Model != nil && enumCheck("model", *j.Model, handoverModels) != nil {
		return RungAnswer{}, fmt.Errorf("model %q is not one of %s", *j.Model, textutil.AndList(handoverModels))
	}
	if *j.Decision == ActHandover && j.Model == nil {
		return RungAnswer{}, errors.New("a handover leaves model out; it names the model of the next session: " + textutil.AndList(handoverModels))
	}
	if j.Draw != nil && (j.Draw.Sessions < 0 || j.Draw.AgentTimeS < 0 || j.Draw.EUR < 0) {
		return RungAnswer{}, fmt.Errorf("a draw does not go below zero: sessions %d, agent_time_s %d, eur %.2f", j.Draw.Sessions, j.Draw.AgentTimeS, j.Draw.EUR)
	}
	out := RungAnswer{
		Decision:       *j.Decision,
		ReasonKind:     *j.ReasonKind,
		Verdicts:       make(map[string]RungVerdict, len(f.Findings)),
		Note:           *j.Note,
		Recommendation: RungRecommendation{Action: *j.Recommendation.Action, Grounds: *j.Recommendation.Grounds},
		Draw:           j.Draw,
	}
	if j.Model != nil {
		out.Model = *j.Model
	}
	for _, id := range f.Findings {
		out.Verdicts[id] = RungVerdict{Verdict: *j.Verdicts[id].Verdict, Evidence: *j.Verdicts[id].Evidence}
	}
	return out, nil
}
