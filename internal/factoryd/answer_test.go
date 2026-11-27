package factoryd

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// screenAnswer is the test's answer type, the screening verdict's shape.
type screenAnswer struct {
	Verdict string   `json:"verdict" enum:"clear,suspect"`
	Reasons []string `json:"reasons"`
}

var screenTool = prompts.AnswerTool(reflect.TypeFor[screenAnswer]())

// typedCall runs the typed call on the plan namer's prompt, forcing the
// screen answer, against ll.
func typedCall(t *testing.T, ll *fakeLiteLLM) (screenAnswer, string, error) {
	t.Helper()
	return answerWith[screenAnswer](context.Background(), testSupervisorModel(ll.srv(t).URL), "t1", "small", time.Minute,
		testPrompt(t, "plan-namer", namerPrompt), screenTool, map[string]any{"title": "Count rows"})
}

// A call with an answer tool sends that tool and forces it: no JSON mode,
// and the user message is the input alone, without the JSON-mode lead.
func TestAnswerSendsAForcedTool(t *testing.T) {
	ll := &fakeLiteLLM{arguments: []string{`{"verdict":"clear","reasons":[]}`}}
	if _, _, err := typedCall(t, ll); err != nil {
		t.Fatal(err)
	}
	req := ll.chats[0]
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "answer" {
		t.Errorf("tools = %v, want the answer tool alone", req["tools"])
	}
	if want := map[string]any{"type": "function", "function": map[string]any{"name": "answer"}}; !reflect.DeepEqual(req["tool_choice"], want) {
		t.Errorf("tool_choice = %v, want %v", req["tool_choice"], want)
	}
	if _, ok := req["response_format"]; ok {
		t.Errorf("the call asks for JSON mode: %v", req["response_format"])
	}
	if user := req["messages"].([]any)[1].(map[string]any); user["content"] != `{"title":"Count rows"}` {
		t.Errorf("the user message = %q, want the input JSON alone", user["content"])
	}
}

// The tool call's arguments decode into the answer type, beside the
// deployment that answered.
func TestAnswerReturnsTheParsedStruct(t *testing.T) {
	ll := &fakeLiteLLM{arguments: []string{`{"verdict":"suspect","reasons":["links a lure"]}`}, deployment: "zai/glm-small"}
	got, deployment, err := typedCall(t, ll)
	if err != nil || !reflect.DeepEqual(got, screenAnswer{Verdict: "suspect", Reasons: []string{"links a lure"}}) || deployment != "zai/glm-small" {
		t.Fatalf("answer = %+v from %q, %v; want suspect for links a lure from zai/glm-small", got, deployment, err)
	}
}

func TestAnswerRefusesAnUnknownField(t *testing.T) {
	ll := &fakeLiteLLM{arguments: []string{`{"verdict":"clear","reasons":[],"extra":1}`}}
	if _, _, err := typedCall(t, ll); err == nil || !strings.Contains(err.Error(), "step plan-namer") || !strings.Contains(err.Error(), `"extra"`) {
		t.Fatalf("answer = %v, want an error naming the step and the field extra", err)
	}
}

func TestAnswerWithoutAToolCallIsAnError(t *testing.T) {
	ll := &fakeLiteLLM{content: "clear, nothing to flag"}
	_, _, err := typedCall(t, ll)
	if want := "step plan-namer answered in text, not through its answer tool: clear, nothing to flag"; err == nil || err.Error() != want {
		t.Fatalf("answer = %v, want %s", err, want)
	}
}

// An answer cut at its limit is asked once more at low thinking depth, and
// the second answer's arguments are the struct.
func TestAnswerCutAnswerRetriesAtLowDepth(t *testing.T) {
	ll := &fakeLiteLLM{finishes: []string{"length", "tool_calls"}, arguments: []string{`{"verd`, `{"verdict":"clear","reasons":["no link"]}`}}
	got, _, err := typedCall(t, ll)
	if err != nil || !reflect.DeepEqual(got, screenAnswer{Verdict: "clear", Reasons: []string{"no link"}}) || len(ll.chats) != 2 {
		t.Fatalf("answer = %+v, %v after %d calls; want the second answer", got, err, len(ll.chats))
	}
	if ll.chats[1]["reasoning_effort"] != "low" || ll.chats[0]["reasoning_effort"] != nil {
		t.Errorf("reasoning_effort = %v then %v, want none then low", ll.chats[0]["reasoning_effort"], ll.chats[1]["reasoning_effort"])
	}
}

// ChatAnswer calls only a step whose profile names an answer type.
func TestChatAnswerNeedsTheStepsAnswerType(t *testing.T) {
	ll := &fakeLiteLLM{}
	_, _, err := ChatAnswer[screenAnswer](context.Background(), testSupervisorModel(ll.srv(t).URL), "t1", "small", time.Minute,
		testPrompt(t, "plan-namer", namerPrompt), map[string]any{})
	if want := "step plan-namer: its profile names no answer type"; err == nil || err.Error() != want || len(ll.chats) != 0 {
		t.Fatalf("ChatAnswer = %v after %d calls, want %s and no call", err, len(ll.chats), want)
	}
}

// The moments' schema comes from their answer types: the decision closed
// to the ask's decisions and the only required field, the findings a
// gate's alone; a session's whole schema is the sessionAnswer's.
func TestMomentAnswerSchemaFromTheGenerator(t *testing.T) {
	for name, ask := range map[string]*CheckInAsk{
		"session cuttable":   {Moment: CheckInSession, Replan: true, Trip: &TripReport{Detector: "repetition"}},
		"session uncut":      {Moment: CheckInSession, Trip: &TripReport{Detector: "repetition"}},
		"gate with findings": {Moment: CheckInGate, Findings: []CheckFinding{{Text: "the handler grows a second cache layer without a test", Source: "review"}}},
		"dead":               {Moment: CheckInDead},
	} {
		s := momentAnswerSchema(ask)
		props := s["properties"].(map[string]any)
		if got := props["decision"].(map[string]any)["enum"].([]string); !slices.Equal(got, askDecisions(ask)) {
			t.Errorf("%s: decision enum = %v, want %v", name, got, askDecisions(ask))
		}
		if !reflect.DeepEqual(s["required"], []string{"decision"}) || s["additionalProperties"] != false {
			t.Errorf("%s: required %v, additionalProperties %v; want decision alone and false", name, s["required"], s["additionalProperties"])
		}
		if _, ok := props["findings"]; ok != (ask.Moment == CheckInGate) {
			t.Errorf("%s: findings present %v, want %v", name, ok, ask.Moment == CheckInGate)
		}
	}
	ask := &CheckInAsk{Moment: CheckInDead}
	want := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"decision"}, "properties": map[string]any{
		"decision":    map[string]any{"type": "string", "enum": []string{CheckHandover, RungHandUp}},
		"guidance":    map[string]any{"type": "string"},
		"reason_kind": map[string]any{"type": "string", "enum": []string{"", CheckGoalConflict, CheckSpend, CheckPolicyClass, CheckAccess}},
		"reason":      map[string]any{"type": "string"},
		"handover":    map[string]any{"type": "string"},
	}}
	if got := momentAnswerSchema(ask); !reflect.DeepEqual(got, want) {
		t.Errorf("the dead moment's schema =\n%#v\nwant\n%#v", got, want)
	}
}
