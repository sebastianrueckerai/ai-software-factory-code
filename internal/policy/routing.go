package policy

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Routing steers a model alias to its twin when the alias runs slow.
type Routing struct {
	// SlowSeconds is the bound in seconds per 1000 output tokens: an alias
	// whose rate over the last hour's calls (the sum of their durations over
	// the sum of their output tokens) is above it serves from its twin, and
	// returns once the rate is at or under 80 percent of the bound. Absent or
	// 0: 30.
	SlowSeconds float64 `yaml:"slow_seconds_per_1000_tokens,omitempty" json:"slow_seconds_per_1000_tokens,omitempty"`
	// Twins is the twin of each alias, by alias; an alias without one never
	// switches. Absent: DefaultTwins.
	Twins map[string]string `yaml:"twins,omitempty" json:"twins,omitempty"`
}

// DefaultSlowSeconds is the bound an absent slow_seconds_per_1000_tokens takes.
const DefaultSlowSeconds = 30

// DefaultTwins are the twins an absent twins takes: the Alibaba twin of
// each LiteLLM alias that has one.
var DefaultTwins = map[string]string{Big: Big + "-alibaba", Small: Small + "-alibaba"}

// Bound is the slow bound in seconds per 1000 output tokens.
func (r Routing) Bound() float64 {
	if r.SlowSeconds == 0 {
		return DefaultSlowSeconds
	}
	return r.SlowSeconds
}

// Twin is alias's twin, "" when it has none.
func (r Routing) Twin(alias string) string { return r.twins()[alias] }

func (r Routing) twins() map[string]string {
	if r.Twins == nil {
		return DefaultTwins
	}
	return r.Twins
}

// primary is the alias twin is the twin of, "" when it is none's.
func (r Routing) primary(twin string) string {
	for _, a := range slices.Sorted(maps.Keys(r.twins())) {
		if r.Twin(a) == twin {
			return a
		}
	}
	return ""
}

// WithTwins is aliases with each one's twin beside it when served says
// LiteLLM serves the twin, sorted, once each: the models a task's key allows.
func (r Routing) WithTwins(aliases []string, served func(twin string) bool) []string {
	out := slices.Clone(aliases)
	for _, a := range aliases {
		if t := r.Twin(a); t != "" && served(t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (r Routing) validate() error {
	if r.SlowSeconds < 0 {
		return fmt.Errorf("routing.slow_seconds_per_1000_tokens must not be negative, is %g", r.SlowSeconds)
	}
	var errs []error
	for alias, twin := range r.Twins {
		if twin == "" || twin == alias {
			errs = append(errs, fmt.Errorf("routing.twins of %s must name another alias, is %q", alias, twin))
		} else if slices.Contains(TextClasses, twin) {
			errs = append(errs, fmt.Errorf("routing.twins of %s is %q, a model class with its own caps; a twin is an alias outside the classes", alias, twin))
		}
	}
	return errors.Join(errs...)
}
