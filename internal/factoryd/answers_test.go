package factoryd

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// property is the schema of the field at path of tool's parameters, items
// of an array or an object entered by their properties.
func property(t *testing.T, tool prompts.Tool, path ...string) map[string]any {
	t.Helper()
	s := tool.Parameters
	for _, name := range path {
		if items, ok := s["items"].(map[string]any); ok {
			s = items
		}
		next, ok := s["properties"].(map[string]any)[name].(map[string]any)
		if !ok {
			t.Fatalf("the schema has no field %v: %v", path, tool.Parameters)
		}
		s = next
	}
	return s
}

// Each bound the parser keeps is the schema's too: one constant, hand
// derived from the prompts' numbers before the move (summary 500, why 300,
// diagnosis 1000, happened 160, left 87).
func TestAnswerToolsCarryTheLimits(t *testing.T) {
	review := prompts.AnswerTool(reflect.TypeFor[ReviewAnswer]())
	audit := prompts.AnswerTool(reflect.TypeFor[AuditAnswer]())
	gate := prompts.AnswerTool(reflect.TypeFor[GateDecisionAnswer]())
	caseBrief := prompts.AnswerTool(reflect.TypeFor[CaseBriefAnswer]())
	for name, tc := range map[string]struct {
		got  map[string]any
		key  string
		want int
	}{
		"review summary":      {property(t, review, "summary"), "maxLength", 500},
		"review findings":     {property(t, review, "findings"), "maxItems", 10},
		"audit why":           {property(t, audit, "findings", "why"), "maxLength", 300},
		"audit file":          {property(t, audit, "findings", "file"), "maxLength", 500},
		"gate diagnosis":      {property(t, gate, "diagnosis"), "maxLength", 1000},
		"gate recommendation": {property(t, gate, "recommendation"), "maxLength", 1000},
		"case happened":       {property(t, caseBrief, "happened"), "maxLength", 160},
		"case left":           {property(t, caseBrief, "left"), "maxLength", 87},
	} {
		if tc.got[tc.key] != tc.want {
			t.Errorf("%s: %s = %v, want %d", name, tc.key, tc.got[tc.key], tc.want)
		}
	}
}

// A step answers through its type: the fake gateway returns a tool call
// with the name, and the namer takes it.
func TestStepsAnswerThroughTheirType(t *testing.T) {
	ll := &fakeLiteLLM{arguments: []string{`{"name":"Fix counter"}`}}
	n, _ := newNamer(t, ll, nil)
	if got := n.Name(context.Background(), nameTask(), "Fix the counter", "the counter counts", "it skips rows"); got != "Fix counter" {
		t.Fatalf("name %q, want Fix counter", got)
	}
	if tools, _ := ll.chats[0]["tools"].([]any); len(tools) != 1 || ll.chats[0]["response_format"] != nil {
		t.Errorf("the request carries tools %v and response_format %v, want the answer tool alone", ll.chats[0]["tools"], ll.chats[0]["response_format"])
	}
}

// No direct call asks for JSON mode: every one forces its answer tool.
func TestNoJSONMode(t *testing.T) {
	p := testPrompt(t, "plan-namer", "")
	tool, _ := p.Answer()
	body, err := chatPayload("small", p.Text(), []byte(`{}`), 100, false, tool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "response_format") || strings.Contains(string(body), "The input, as JSON") {
		t.Errorf("the payload asks for JSON mode: %s", body)
	}
}

// A direct step's text never spells its answer's shape: the answer type
// does.
func TestNoPromptSpellsJSON(t *testing.T) {
	for _, step := range []string{"model-review", "model-review-part", "security-audit", "security-audit-part", "gate-supervisor", "delta-planner",
		"holdout-proposer", "judgement", "answer-screener", "brief-writer", "case-brief-writer", "plan-namer", "follow-liveness"} {
		var p prompts.Prompt
		for _, r := range prompts.Registered() {
			if r.Step == step {
				var err error
				if p, err = prompts.Build(step, r.Fixture); err != nil {
					t.Fatal(err)
				}
			}
		}
		if p.Step() == "" {
			t.Fatalf("step %s is not registered", step)
		}
		if strings.Contains(p.Text(), "Answer with one JSON object") || strings.Contains(p.Text(), "JSON object") {
			t.Errorf("the %s text spells its JSON:\n%s", step, p.Text())
		}
		if _, ok := p.Answer(); !ok {
			t.Errorf("the %s profile names no answer type", step)
		}
	}
}
