package factoryd

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// answerFacts are seeded facts of a card at its budget floor with two
// findings: rung 3 answers reverify, extend, raise_ceiling, successor,
// replan and close on them.
var answerFacts = CaseFacts{Head: "h1", PR: 9, PROpen: true, Checks: "green", ReviewApproved: true, Attempts: 3, SessionCap: 3,
	AgentTimeS: 900, AgentTimeCapS: 1800, BudgetEUR: 5, SpentEUR: 5, LeftEUR: 0, FloorEUR: 0.3, CeilingEUR: 5,
	Findings: []string{idA, idB}}

// wantReasonKinds is the closed enum of reason_kind: every case kind in
// the order cases.go declares them, then the check-in's reason constants.
var wantReasonKinds = []string{
	"session_cap", "agent_time_cap", "budget_floor", "day_cap",
	"month_cap", "project_cap", "checkin_human", "checkin_share",
	"gate_stop", "findings_repeated", "dependency_closed", "dependency_failed",
	"wait_expired", "pod_never_started", "provider_refused", "no_answer",
	"content_filter", "image_wait", "screening_suspect", "agent_escalate",
	"agent_request_info", "plan_question", "plan_confirmation", "merge_approval",
	"human_card", "other", "monitor_flag",
	"goal_conflict", "spend_above_policy", "policy_class", "access_factory_lacks",
}

// answerPart is schema's part named part, failing the test when the
// schema holds no such part.
func answerPart(t *testing.T, schema map[string]any, part string) map[string]any {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the schema holds no properties: %v", schema["properties"])
	}
	got, ok := props[part].(map[string]any)
	if !ok {
		t.Fatalf("the schema holds no %s: %v", part, props[part])
	}
	return got
}

// answerEnum is part's enum as a list of strings, failing the test when
// part holds none.
func answerEnum(t *testing.T, schema map[string]any, part string) []string {
	t.Helper()
	got, ok := answerPart(t, schema, part)["enum"].([]string)
	if !ok {
		t.Fatalf("the schema's %s holds no enum of strings: %v", part, answerPart(t, schema, part)["enum"])
	}
	return got
}

// answerRequired is schema's required list as an array of strings,
// failing the test when it is anything else.
func answerRequired(t *testing.T, schema map[string]any) []string {
	t.Helper()
	got, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("required is %T, want an array of strings: %v", schema["required"], schema["required"])
	}
	return got
}

// The schema's decision enum is exactly the offered action names of the
// rung, in table order, the recommendation picks from the same enum,
// reason_kind is the closed enum of the case kinds and the check
// reasons, and every required list is an array of strings.
func TestAnswerSchemaDerivesFromOffered(t *testing.T) {
	noFindings := answerFacts
	noFindings.Findings = nil
	for _, c := range []struct {
		name string
		f    CaseFacts
	}{
		{"two findings", answerFacts},
		{"no findings", noFindings},
	} {
		for rung := 0; rung <= 3; rung++ {
			schema := AnswerSchema(c.f, rung)
			offered := offeredNames(c.f, rung)
			if got := answerEnum(t, schema, "decision"); !slices.Equal(got, offered) {
				t.Errorf("%s, rung %d: the decision enum is %v, want the offered actions %v", c.name, rung, got, offered)
			}
			if got := answerEnum(t, answerPart(t, schema, "recommendation"), "action"); !slices.Equal(got, offered) {
				t.Errorf("%s, rung %d: the recommendation's action enum is %v, want the offered actions %v", c.name, rung, got, offered)
			}
			if got := answerEnum(t, schema, "reason_kind"); !slices.Equal(got, wantReasonKinds) {
				t.Errorf("%s, rung %d: the reason_kind enum is %v, want %v", c.name, rung, got, wantReasonKinds)
			}
			if got, want := answerRequired(t, schema), []string{"decision", "reason_kind", "verdicts", "note", "recommendation"}; !slices.Equal(got, want) {
				t.Errorf("%s, rung %d: required is %v, want %v", c.name, rung, got, want)
			}
			if got, want := answerRequired(t, answerPart(t, schema, "recommendation")), []string{"action", "grounds"}; !slices.Equal(got, want) {
				t.Errorf("%s, rung %d: the recommendation's required is %v, want %v", c.name, rung, got, want)
			}
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Errorf("%s, rung %d: the schema is %v, want a closed object", c.name, rung, schema)
			}
		}
	}
	// the decision enum is the offered actions of that rung, not of every
	// rung: rung 3 answers the caps, rung 0 routes the approval and
	// withdraws instead of closing
	schema := AnswerSchema(answerFacts, 3)
	if got, want := answerEnum(t, schema, "decision"), []string{ActReverify, ActExtend, ActRaiseCeiling, ActSuccessor, ActReplan, ActClose}; !slices.Equal(got, want) {
		t.Errorf("rung 3's decision enum is %v, want %v", got, want)
	}
	if got, want := answerEnum(t, AnswerSchema(answerFacts, 0), "decision"), []string{ActRouteApproval, ActReverify, ActSuccessor, ActReplan, ActWithdraw}; !slices.Equal(got, want) {
		t.Errorf("rung 0's decision enum is %v, want %v", got, want)
	}
}

// The verdicts part is keyed by the facts' finding ids: each id holds
// one of the check-in's three verdicts with its evidence, and the
// findings themselves are the required list. Facts without findings
// take an empty verdicts object that names no id. The draw rides beside
// the decision with three parts, each floored at zero, and no part of
// it is required.
func TestAnswerSchemaVerdictsAndDraw(t *testing.T) {
	verdicts := answerPart(t, AnswerSchema(answerFacts, 3), "verdicts")
	props, ok := verdicts["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the verdicts hold no properties: %v", verdicts["properties"])
	}
	if len(props) != 2 || props[idA] == nil || props[idB] == nil {
		t.Fatalf("the verdicts are keyed %v, want exactly %s and %s", props, idA, idB)
	}
	for _, id := range []string{idA, idB} {
		one, ok := props[id].(map[string]any)
		if !ok {
			t.Fatalf("the verdict on %s is %v, want an object", id, props[id])
		}
		if got, want := answerEnum(t, one, "verdict"), []string{CheckConfirmed, CheckDropped, CheckRemark}; !slices.Equal(got, want) {
			t.Errorf("the verdict on %s picks from %v, want %v", id, got, want)
		}
		if _, ok := answerPart(t, one, "evidence")["type"]; !ok {
			t.Errorf("the verdict on %s holds no evidence", id)
		}
		if got, want := answerRequired(t, one), []string{"verdict", "evidence"}; !slices.Equal(got, want) {
			t.Errorf("the verdict on %s requires %v, want %v", id, got, want)
		}
	}
	if got, want := answerRequired(t, verdicts), []string{idA, idB}; !slices.Equal(got, want) {
		t.Errorf("the verdicts require %v, want the findings %v", got, want)
	}
	if verdicts["additionalProperties"] != false {
		t.Errorf("the verdicts take unknown ids: %v", verdicts)
	}
	noFindings := answerFacts
	noFindings.Findings = nil
	empty := answerPart(t, AnswerSchema(noFindings, 3), "verdicts")
	if _, has := empty["properties"]; has {
		t.Errorf("facts without findings still key verdicts: %v", empty["properties"])
	}
	if _, has := empty["required"]; has {
		t.Errorf("facts without findings still require verdicts: %v", empty["required"])
	}
	draw := answerPart(t, AnswerSchema(answerFacts, 3), "draw")
	parts, ok := draw["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the draw holds no properties: %v", draw["properties"])
	}
	for _, part := range []string{"sessions", "agent_time_s", "eur"} {
		one, ok := parts[part].(map[string]any)
		if !ok {
			t.Fatalf("the draw holds no %s: %v", part, parts[part])
		}
		if one["minimum"] != 0 {
			t.Errorf("the draw's %s is floored at %v, want 0", part, one["minimum"])
		}
	}
	if parts["sessions"].(map[string]any)["type"] != "integer" || parts["agent_time_s"].(map[string]any)["type"] != "integer" || parts["eur"].(map[string]any)["type"] != "number" {
		t.Errorf("the draw's parts are mistyped: %v", parts)
	}
	if slices.Contains(answerRequired(t, AnswerSchema(answerFacts, 3)), "draw") {
		t.Error("the draw is required, want it beside the decision")
	}
}

// answerBody is a valid answer on answerFacts at rung 3, changed by
// edit before it is marshalled.
func answerBody(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	body := map[string]any{
		"decision":    ActExtend,
		"reason_kind": string(CaseSessionCap),
		"verdicts": map[string]any{
			idA: map[string]any{"verdict": CheckConfirmed, "evidence": "actions.go:38 misses e.repository = $1"},
			idB: map[string]any{"verdict": CheckRemark, "evidence": "carddigest.go:49 the same shape as idA"},
		},
		"note":           "one red test is left, extend by two sessions",
		"recommendation": map[string]any{"action": ActReverify, "grounds": "the head is green again on a reverify"},
		"draw":           map[string]any{"sessions": 2, "agent_time_s": 3600, "eur": 0.5},
	}
	if edit != nil {
		edit(body)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	return string(b)
}

// A valid answer parses into the typed struct, with its verdicts keyed
// by finding id and its draw beside the decision, and the validator
// refuses every answer the schema refuses.
func TestParseRungAnswer(t *testing.T) {
	got, err := ParseRungAnswer(answerFacts, 3, answerBody(t, nil))
	if err != nil {
		t.Fatalf("a valid answer is refused: %v", err)
	}
	if got.Decision != ActExtend || got.ReasonKind != string(CaseSessionCap) || got.Note != "one red test is left, extend by two sessions" {
		t.Errorf("the answer parsed into %+v", got)
	}
	if want := (RungVerdict{Verdict: CheckConfirmed, Evidence: "actions.go:38 misses e.repository = $1"}); got.Verdicts[idA] != want {
		t.Errorf("the verdict on %s is %+v, want %+v", idA, got.Verdicts[idA], want)
	}
	if len(got.Verdicts) != 2 {
		t.Errorf("the answer holds %d verdicts, want one per finding", len(got.Verdicts))
	}
	if want := (RungRecommendation{Action: ActReverify, Grounds: "the head is green again on a reverify"}); got.Recommendation != want {
		t.Errorf("the recommendation is %+v, want %+v", got.Recommendation, want)
	}
	if got.Draw == nil || got.Draw.Sessions != 2 || got.Draw.AgentTimeS != 3600 || got.Draw.EUR != 0.5 {
		t.Errorf("the draw is %+v, want 2 sessions, 3600 s and 0.50 EUR", got.Draw)
	}
	got, err = ParseRungAnswer(answerFacts, 3, answerBody(t, func(m map[string]any) { delete(m, "draw") }))
	if err != nil {
		t.Fatalf("a valid answer without a draw is refused: %v", err)
	}
	if got.Draw != nil {
		t.Errorf("the answer without a draw parsed a draw: %+v", got.Draw)
	}
	noFindings := answerFacts
	noFindings.Findings = nil
	if got, err = ParseRungAnswer(noFindings, 3, answerBody(t, func(m map[string]any) { m["verdicts"] = map[string]any{} })); err != nil || len(got.Verdicts) != 0 {
		t.Errorf("an empty verdicts object on facts without findings: %v, %v", got.Verdicts, err)
	}
	for _, c := range []struct {
		name    string
		f       CaseFacts
		rung    int
		text    string
		wantErr string
	}{
		{"not JSON", answerFacts, 3, "not one object", "the answer is not JSON"},
		{"a JSON string", answerFacts, 3, `"extend"`, "not one JSON object"},
		{"a JSON array", answerFacts, 3, `[1]`, "not one JSON object"},
		{"no decision", answerFacts, 3, answerBody(t, func(m map[string]any) { delete(m, "decision") }), "leaves decision out"},
		{"no reason_kind", answerFacts, 3, answerBody(t, func(m map[string]any) { delete(m, "reason_kind") }), "leaves reason_kind out"},
		{"no verdicts", answerFacts, 3, answerBody(t, func(m map[string]any) { delete(m, "verdicts") }), "leaves verdicts out"},
		{"no note", answerFacts, 3, answerBody(t, func(m map[string]any) { delete(m, "note") }), "leaves note out"},
		{"no recommendation", answerFacts, 3, answerBody(t, func(m map[string]any) { delete(m, "recommendation") }), "leaves recommendation out"},
		{"a decision outside the enum", answerFacts, 3, answerBody(t, func(m map[string]any) { m["decision"] = "teleport" }), "is not offered at this rung"},
		{"a decision of another rung", answerFacts, 2, answerBody(t, nil), "is not offered at this rung"},
		{"a reason_kind of free text", answerFacts, 3, answerBody(t, func(m map[string]any) { m["reason_kind"] = "policy class" }), "outside the closed enum"},
		{"a recommended action outside the enum", answerFacts, 3, answerBody(t, func(m map[string]any) {
			m["recommendation"] = map[string]any{"action": "teleport", "grounds": "why"}
		}), "is not offered at this rung"},
		{"a verdict on an id the facts do not hold", answerFacts, 3, answerBody(t, func(m map[string]any) {
			m["verdicts"].(map[string]any)["000000000000"] = map[string]any{"verdict": CheckRemark, "evidence": "nothing"}
		}), "which the facts do not hold"},
		{"a verdict on facts without findings", noFindings, 3, answerBody(t, func(m map[string]any) {
			m["verdicts"].(map[string]any)["000000000000"] = map[string]any{"verdict": CheckRemark, "evidence": "nothing"}
		}), "the findings are none"},
		{"a finding without its verdict", answerFacts, 3, answerBody(t, func(m map[string]any) {
			delete(m["verdicts"].(map[string]any), idB)
		}), "holds no verdict on"},
		{"a verdict outside the three", answerFacts, 3, answerBody(t, func(m map[string]any) {
			m["verdicts"].(map[string]any)[idA].(map[string]any)["verdict"] = "maybe"
		}), "is not one of"},
		{"a verdict without its evidence", answerFacts, 3, answerBody(t, func(m map[string]any) {
			delete(m["verdicts"].(map[string]any)[idA].(map[string]any), "evidence")
		}), "leaves evidence out"},
		{"a draw below zero", answerFacts, 3, answerBody(t, func(m map[string]any) {
			m["draw"].(map[string]any)["eur"] = -0.01
		}), "below zero"},
	} {
		_, err := ParseRungAnswer(c.f, c.rung, c.text)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: refusal is %v, want it to name %q", c.name, err, c.wantErr)
		}
	}
}
