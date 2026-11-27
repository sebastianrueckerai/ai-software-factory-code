package prompts

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type verdictAnswer struct {
	Verdict string   `json:"verdict" enum:"clear,suspect" desc:"suspect when an agent must not act on the issue"`
	Reasons []string `json:"reasons" max:"answerTestReasons" item:"answerTestReason" desc:"one line each"`
	Note    string   `json:"note,omitempty"`
}

type kindsBase struct {
	Step string `json:"step"`
}

type kindsItem struct {
	ID   string `json:"id"`
	Seen bool   `json:"seen,omitempty"`
}

type kindsAnswer struct {
	kindsBase
	N      int         `json:"n"`
	Share  float64     `json:"share,omitempty"`
	Items  []kindsItem `json:"items,omitempty"`
	Inner  kindsItem   `json:"inner"`
	hidden string
}

func init() {
	RegisterLimit("answerTestReasons", 5)
	RegisterLimit("answerTestReason", 200)
	RegisterAnswer[verdictAnswer]("answer-test")
}

// A tagged struct becomes a closed object schema: the fields without
// omitempty required, the enum, the description and the registered
// limits where the tags name them.
func TestAnswerToolSchema(t *testing.T) {
	got := AnswerTool(reflect.TypeFor[verdictAnswer]())
	want := Tool{Name: "answer", Description: "Your answer: one JSON object as this tool's arguments.", Parameters: map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"verdict", "reasons"},
		"properties": map[string]any{
			"verdict": map[string]any{"type": "string", "enum": []string{"clear", "suspect"}, "description": "suspect when an agent must not act on the issue"},
			"reasons": map[string]any{"type": "array", "items": map[string]any{"type": "string", "maxLength": 200}, "maxItems": 5, "description": "one line each"},
			"note":    map[string]any{"type": "string"},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AnswerTool =\n%#v\nwant\n%#v", got, want)
	}
}

// An embedded struct's fields join the object, a nested struct and a slice
// of structs are closed objects of their own, and an unexported field is
// none of the schema's.
func TestAnswerToolKinds(t *testing.T) {
	item := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id"},
		"properties": map[string]any{"id": map[string]any{"type": "string"}, "seen": map[string]any{"type": "boolean"}}}
	want := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"step", "n", "inner"},
		"properties": map[string]any{
			"step":  map[string]any{"type": "string"},
			"n":     map[string]any{"type": "integer"},
			"share": map[string]any{"type": "number"},
			"items": map[string]any{"type": "array", "items": item},
			"inner": item,
		}}
	if got := AnswerTool(reflect.TypeFor[kindsAnswer]()).Parameters; !reflect.DeepEqual(got, want) {
		t.Errorf("AnswerTool's parameters =\n%#v\nwant\n%#v", got, want)
	}
}

// panicOf is what f panics with, "" when it returns.
func panicOf(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	f()
	return ""
}

// A field a schema cannot close panics, naming the field.
func TestAnswerToolRefusesWhatItCannotExpress(t *testing.T) {
	type pointer struct {
		Ref *string `json:"ref"`
	}
	type mapped struct {
		Counts map[string]int `json:"counts"`
	}
	type untagged struct {
		Verdict string
	}
	for name, c := range map[string]struct {
		typ  reflect.Type
		want string
	}{
		"a pointer":         {reflect.TypeFor[pointer](), "prompts: answer type prompts.pointer.Ref is a ptr, which a schema cannot close"},
		"a map":             {reflect.TypeFor[mapped](), "prompts: answer type prompts.mapped.Counts is a map, which a schema cannot close"},
		"an untagged field": {reflect.TypeFor[untagged](), "prompts: answer type prompts.untagged field Verdict has no json tag"},
	} {
		if got := panicOf(func() { AnswerTool(c.typ) }); got != c.want {
			t.Errorf("%s: AnswerTool panics with %q, want %q", name, got, c.want)
		}
	}
}

// WithEnum sets one field's closed set on a copy, through a slice's items
// too, and an empty set drops the field; the original stays as it was.
func TestWithEnumReplacesOneSet(t *testing.T) {
	base := AnswerTool(reflect.TypeFor[kindsAnswer]())
	step := base.WithEnum("step", []string{"plan", "build"})
	if got := step.Parameters["properties"].(map[string]any)["step"]; !reflect.DeepEqual(got, map[string]any{"type": "string", "enum": []string{"plan", "build"}}) {
		t.Errorf("step = %v, want its enum", got)
	}
	ids := base.WithEnum("items.id", []string{"a", "b"})
	item := ids.Parameters["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	if got := item["properties"].(map[string]any)["id"]; !reflect.DeepEqual(got, map[string]any{"type": "string", "enum": []string{"a", "b"}}) {
		t.Errorf("items.id = %v, want its enum", got)
	}
	dropped := base.WithEnum("inner.id", nil)
	inner := dropped.Parameters["properties"].(map[string]any)["inner"].(map[string]any)
	if _, ok := inner["properties"].(map[string]any)["id"]; ok || !reflect.DeepEqual(inner["required"], []string{}) {
		t.Errorf("inner after an empty set = %v, want no id and nothing required", inner)
	}
	if !reflect.DeepEqual(base, AnswerTool(reflect.TypeFor[kindsAnswer]())) {
		t.Error("WithEnum changed the tool it copied")
	}
	if got := panicOf(func() { base.WithEnum("missing", []string{"x"}) }); got != "prompts: answer tool has no field missing" {
		t.Errorf("an unknown field panics with %q", got)
	}
}

func TestRegisterLimitDuplicatePanics(t *testing.T) {
	if got := panicOf(func() { RegisterLimit("answerTestReasons", 6) }); got != "prompts: limit answerTestReasons is registered twice" {
		t.Errorf("a second answerTestReasons panics with %q", got)
	}
	if got := panicOf(func() { RegisterAnswer[verdictAnswer]("answer-test") }); got != "prompts: answer answer-test is registered twice" {
		t.Errorf("a second answer-test panics with %q", got)
	}
}

// A profile's answer gives its Prompt the registered tool and type; a name
// no RegisterAnswer named fails the build.
func TestBuildCarriesTheAnswerTool(t *testing.T) {
	profile := strings.Replace(validProfile, `answer: ""`, "answer: answer-test", 1)
	c, err := load(profileFS(profile), testWriting)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.build("x", nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := p.Answer()
	if !ok || !reflect.DeepEqual(tool, AnswerTool(reflect.TypeFor[verdictAnswer]())) || p.AnswerType() != reflect.TypeFor[verdictAnswer]() {
		t.Errorf("Answer = %v, %v and AnswerType %v, want the verdict answer", tool, ok, p.AnswerType())
	}
	c, err = load(profileFS(strings.Replace(validProfile, `answer: ""`, "answer: nothing", 1)), testWriting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.build("x", nil); err == nil || err.Error() != "step x: answer nothing is not a registered answer type: call prompts.RegisterAnswer beside the step's call" {
		t.Errorf("build = %v, want the unregistered answer named", err)
	}
	if _, ok := (Prompt{}).Answer(); ok {
		t.Error("a prompt with no answer type has one")
	}
}
