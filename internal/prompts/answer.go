package prompts

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// AnswerToolName is the forced answer tool's name.
const AnswerToolName = "answer"

// answerDescription is every answer tool's description: what the step
// answers is its fields'.
const answerDescription = "Your answer: one JSON object as this tool's arguments."

var answers struct {
	limits map[string]int
	types  map[string]answerType
}

type answerType struct {
	want reflect.Type
	tool Tool
}

// RegisterLimit names a bound the parser also uses, so a schema and its
// parser share one constant. It panics on a duplicate name.
func RegisterLimit(name string, n int) {
	registry.Lock()
	defer registry.Unlock()
	if _, ok := answers.limits[name]; ok {
		panic("prompts: limit " + name + " is registered twice")
	}
	if answers.limits == nil {
		answers.limits = map[string]int{}
	}
	answers.limits[name] = n
}

// RegisterAnswer names a struct type as a step's answer, as a profile's
// answer field does. It panics on a duplicate name or a type AnswerTool
// refuses.
func RegisterAnswer[T any](name string) {
	tool := AnswerTool(reflect.TypeFor[T]())
	registry.Lock()
	defer registry.Unlock()
	if _, ok := answers.types[name]; ok {
		panic("prompts: answer " + name + " is registered twice")
	}
	if answers.types == nil {
		answers.types = map[string]answerType{}
	}
	answers.types[name] = answerType{reflect.TypeFor[T](), tool}
}

// answerOf is the registered answer type name names.
func answerOf(name string) (answerType, bool) {
	registry.Lock()
	defer registry.Unlock()
	a, ok := answers.types[name]
	return a, ok
}

// AnswerTool derives the forced answer tool of struct type t. It panics on a
// pointer, map, interface or untagged exported field. Fields are required unless omitempty.
//
//	json:"name,omitempty"  the property name, and optional
//	desc:"..."             the property's description
//	enum:"a,b"             a closed string set
//	max:"limit"            string: maxLength, slice: maxItems, the registered limit's value
//	item:"limit"           slice of string: maxLength of each element
func AnswerTool(t reflect.Type) Tool {
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("prompts: answer type %s is no struct", t))
	}
	return Tool{Name: AnswerToolName, Description: answerDescription, Parameters: objectSchema(t, t.String())}
}

// objectSchema is struct type t's object schema; where names t in a panic.
func objectSchema(t reflect.Type, where string) map[string]any {
	props := map[string]any{}
	var required []string
	var fields func(t reflect.Type)
	fields = func(t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag, tagged := f.Tag.Lookup("json")
			switch {
			case f.Anonymous && !tagged && f.Type.Kind() == reflect.Struct:
				fields(f.Type)
				continue
			case !f.IsExported() || tag == "-":
				continue
			case !tagged:
				panic(fmt.Sprintf("prompts: answer type %s field %s has no json tag", where, f.Name))
			}
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" {
				panic(fmt.Sprintf("prompts: answer type %s field %s names no property in its json tag", where, f.Name))
			}
			props[name] = fieldSchema(f, where+"."+f.Name)
			if !slices.Contains(strings.Split(opts, ","), "omitempty") {
				required = append(required, name)
			}
		}
	}
	fields(t)
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// fieldSchema is field f's schema under its tags; where names it in a panic.
func fieldSchema(f reflect.StructField, where string) map[string]any {
	s := valueSchema(f.Type, where)
	if d := f.Tag.Get("desc"); d != "" {
		s["description"] = d
	}
	target := s
	if f.Type.Kind() == reflect.Slice {
		target = s["items"].(map[string]any)
	}
	if e, ok := f.Tag.Lookup("enum"); ok {
		if target["type"] != "string" {
			panic(fmt.Sprintf("prompts: answer type %s: enum on a %s", where, f.Type))
		}
		target["enum"] = strings.Split(e, ",")
	}
	if m := f.Tag.Get("max"); m != "" {
		switch f.Type.Kind() {
		case reflect.String:
			s["maxLength"] = limit(m, where)
		case reflect.Slice:
			s["maxItems"] = limit(m, where)
		default:
			panic(fmt.Sprintf("prompts: answer type %s: max on a %s", where, f.Type))
		}
	}
	if m := f.Tag.Get("item"); m != "" {
		if f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.String {
			panic(fmt.Sprintf("prompts: answer type %s: item on a %s, not a slice of string", where, f.Type))
		}
		target["maxLength"] = limit(m, where)
	}
	return s
}

// valueSchema is the schema of a value of type t; where names it in a panic.
func valueSchema(t reflect.Type, where string) map[string]any {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": valueSchema(t.Elem(), where)}
	case reflect.Struct:
		return objectSchema(t, where)
	}
	panic(fmt.Sprintf("prompts: answer type %s is a %s, which a schema cannot close", where, t.Kind()))
}

// limit is the registered limit name names.
func limit(name, where string) int {
	registry.Lock()
	defer registry.Unlock()
	n, ok := answers.limits[name]
	if !ok {
		panic(fmt.Sprintf("prompts: answer type %s names limit %s, which no RegisterLimit named", where, name))
	}
	return n
}

// WithEnum returns a copy with the closed set of field replaced, for a set
// that depends on the call (the check-in's decisions). field is a property
// path, items' properties by a dot ("findings.verdict"). An empty set drops
// the field, which no value could fill. It panics on a field the schema
// lacks.
func (t Tool) WithEnum(field string, values []string) Tool {
	t.Parameters = clone(t.Parameters).(map[string]any)
	parent, s := t.Parameters, t.Parameters
	path := strings.Split(field, ".")
	for i, name := range path {
		if s["type"] == "array" {
			s = s["items"].(map[string]any)
		}
		props, _ := s["properties"].(map[string]any)
		next, ok := props[name].(map[string]any)
		if !ok {
			panic(fmt.Sprintf("prompts: answer tool has no field %s", field))
		}
		if i < len(path)-1 {
			s = next
			continue
		}
		parent, s = s, next
	}
	last := path[len(path)-1]
	if len(values) == 0 {
		delete(parent["properties"].(map[string]any), last)
		if required, ok := parent["required"].([]string); ok {
			parent["required"] = slices.DeleteFunc(required, func(n string) bool { return n == last })
		}
		return t
	}
	if s["type"] == "array" {
		s = s["items"].(map[string]any)
	}
	s["enum"] = slices.Clone(values)
	return t
}

// clone copies the maps and slices of a schema value, so a copy's edit
// leaves the original whole.
func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := maps.Clone(x)
		for k, e := range out {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := slices.Clone(x)
		for i, e := range out {
			out[i] = clone(e)
		}
		return out
	case []string:
		return slices.Clone(x)
	}
	return v
}
