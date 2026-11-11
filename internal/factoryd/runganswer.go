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
// the re-ask on a refusal are the check-in path's business.

// caseKindNames are every case kind a case is opened as, in the order
// cases.go declares them.
var caseKindNames = []string{
	string(CaseSessionCap), string(CaseAgentTimeCap), string(CaseBudgetFloor), string(CaseDayCap),
	string(CaseMonthCap), string(CaseProjectCap), string(CaseCheckinHuman), string(CaseCheckinShare), string(CaseCheckinFailed),
	string(CaseGateStop), string(CaseFindingsRepeated), string(CaseDependencyClosed), string(CaseDependencyFailed),
	string(CaseWaitExpired), string(CasePodNeverStarted), string(CaseProviderRefused), string(CaseNoAnswer),
	string(CaseContentFilter), string(CaseImageWait), string(CaseScreeningSuspect), string(CaseAgentEscalate),
	string(CaseAgentRequestInfo), string(CasePlanQuestion), string(CasePlanConfirmation), string(CaseMergeApproval),
	string(CaseHumanCard), string(CaseOther), string(CaseMonitorFlag),
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

// AnswerSchema derives the answer tool's JSON schema from f and rung: the
// decision an enum of exactly the action names Offered lists for the
// rung, reason_kind the closed enum answerReasonKinds holds, the verdicts
// an object keyed by the finding ids on f, each one of the check-in's
// three verdicts with its evidence, a note, a recommendation with its
// grounds that picks from the same offered actions, and an optional draw
// that rides beside the decision. Every required list is an array of
// strings. The same facts and rung derive the same schema.
func AnswerSchema(f CaseFacts, rung int) map[string]any {
	offered := answerDecisions(f, rung)
	verdicts := map[string]any{"type": "object", "additionalProperties": false}
	if len(f.Findings) > 0 {
		props := make(map[string]any, len(f.Findings))
		for _, id := range f.Findings {
			props[id] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"verdict":  map[string]any{"type": "string", "enum": []string{CheckConfirmed, CheckDropped, CheckRemark}},
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
				"enum":        offered,
				"description": "the action this rung takes, one of the offered actions",
			},
			"reason_kind": map[string]any{
				"type":        "string",
				"enum":        slices.Clone(answerReasonKinds),
				"description": "the kind of reason the answer answers",
			},
			"verdicts": verdicts,
			"note":     map[string]any{"type": "string", "description": "the note the answer hands the card"},
			"recommendation": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":  map[string]any{"type": "string", "enum": slices.Clone(offered), "description": "the action the answer recommends"},
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
// decision from the rung's offered actions, its reason kind, a verdict on
// every finding keyed by the finding's id, a note, a recommendation with
// grounds, and the draw that rides beside the decision.
type RungAnswer struct {
	Decision       string                 `json:"decision"`
	ReasonKind     string                 `json:"reason_kind"`
	Verdicts       map[string]RungVerdict `json:"verdicts"`
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
// refuses: an answer that is not one JSON object, one that leaves a
// required field out, a decision or a recommended action outside the
// rung's offered actions, a reason_kind outside the closed enum, a
// verdict on an id the facts do not hold or a finding without one, a
// verdict outside the check-in's three or without its evidence, and a
// draw any part of which is below zero. The refusal names what the
// answer broke, for the one re-ask later cards build on it.
func ParseRungAnswer(f CaseFacts, rung int, text string) (RungAnswer, error) {
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return RungAnswer{}, fmt.Errorf("the answer is not JSON: %w", err)
	}
	if _, ok := raw.(map[string]any); !ok {
		return RungAnswer{}, errors.New("the answer is not one JSON object")
	}
	var j rungAnswerJSON
	if err := json.Unmarshal([]byte(text), &j); err != nil {
		return RungAnswer{}, fmt.Errorf("the answer is malformed: %w", err)
	}
	offered := answerDecisions(f, rung)
	switch {
	case j.Decision == nil:
		return RungAnswer{}, errors.New("the answer leaves decision out; the rung answers with " + offeredText(offered))
	case j.ReasonKind == nil:
		return RungAnswer{}, errors.New("the answer leaves reason_kind out")
	case j.Verdicts == nil:
		return RungAnswer{}, errors.New("the answer leaves verdicts out; with no findings it is an empty object")
	case j.Note == nil:
		return RungAnswer{}, errors.New("the answer leaves note out")
	case j.Recommendation == nil:
		return RungAnswer{}, errors.New("the answer leaves recommendation out")
	}
	if !slices.Contains(offered, *j.Decision) {
		return RungAnswer{}, fmt.Errorf("decision %q is not offered at this rung: the rung answers with %s", *j.Decision, offeredText(offered))
	}
	if !slices.Contains(answerReasonKinds, *j.ReasonKind) {
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
		case v.Verdict == nil:
			return RungAnswer{}, fmt.Errorf("the verdict on %s leaves verdict out", id)
		case v.Evidence == nil:
			return RungAnswer{}, fmt.Errorf("the verdict on %s leaves evidence out", id)
		case !slices.Contains([]string{CheckConfirmed, CheckDropped, CheckRemark}, *v.Verdict):
			return RungAnswer{}, fmt.Errorf("the verdict %q on %s is not one of %s", *v.Verdict, id, textutil.AndList([]string{CheckConfirmed, CheckDropped, CheckRemark}))
		}
	}
	if j.Recommendation.Action == nil {
		return RungAnswer{}, errors.New("the recommendation leaves action out")
	}
	if j.Recommendation.Grounds == nil {
		return RungAnswer{}, errors.New("the recommendation leaves grounds out")
	}
	if !slices.Contains(offered, *j.Recommendation.Action) {
		return RungAnswer{}, fmt.Errorf("the recommended action %q is not offered at this rung: the rung answers with %s", *j.Recommendation.Action, offeredText(offered))
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
	for _, id := range f.Findings {
		out.Verdicts[id] = RungVerdict{Verdict: *j.Verdicts[id].Verdict, Evidence: *j.Verdicts[id].Evidence}
	}
	return out, nil
}
