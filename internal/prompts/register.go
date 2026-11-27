package prompts

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

type renderer struct {
	want   reflect.Type
	render func(any) (string, error)
}

// Registration is a step's model-call sites and the example facts its
// prompt is built with.
type Registration struct {
	Step    string
	Calls   []string
	Fixture any
}

var registry struct {
	sync.Mutex
	renderers map[string]renderer
	steps     map[string]Registration
}

// text renders a plain string: the facts of a step whose prompt is one
// fixed text or one pushed message.
func init() { RegisterFacts("text", func(s string) (string, error) { return s, nil }) }

// RegisterFacts names a typed renderer. A renderer prints values and
// conditions. It is called at init and panics on a duplicate name.
func RegisterFacts[T any](name string, render func(T) (string, error)) {
	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.renderers[name]; ok {
		panic("prompts: facts renderer " + name + " is registered twice")
	}
	if registry.renderers == nil {
		registry.renderers = map[string]renderer{}
	}
	registry.renderers[name] = renderer{reflect.TypeFor[T](), func(facts any) (string, error) {
		return render(facts.(T))
	}}
}

// RegisterStep names a step's model-call sites, as the walk names them,
// and its fixture: the example facts the agent map and the budget test
// build with. It panics on a step registered twice.
func RegisterStep(step string, calls []string, fixture any) {
	registry.Lock()
	defer registry.Unlock()
	if _, ok := registry.steps[step]; ok {
		panic("prompts: step " + step + " is registered twice")
	}
	if registry.steps == nil {
		registry.steps = map[string]Registration{}
	}
	registry.steps[step] = Registration{step, slices.Clone(calls), fixture}
}

// Registered returns every registered step, sorted by step.
func Registered() []Registration {
	registry.Lock()
	defer registry.Unlock()
	var out []Registration
	for _, r := range registry.steps {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Registration) int { return strings.Compare(a.Step, b.Step) })
	return out
}

// render is facts through the renderer p names; nil facts render nothing.
func render(p Profile, facts any) (string, error) {
	if facts == nil {
		return "", nil
	}
	registry.Lock()
	r, ok := registry.renderers[p.Facts]
	registry.Unlock()
	if !ok {
		return "", fmt.Errorf("step %s: facts renderer %s is not registered", p.Step, p.Facts)
	}
	if got := reflect.TypeOf(facts); got != r.want {
		return "", fmt.Errorf("step %s: facts of type %s, want %s", p.Step, got, r.want)
	}
	return r.render(facts)
}
