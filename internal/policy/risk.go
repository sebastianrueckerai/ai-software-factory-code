package policy

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Risk classifies a pull request by the paths it changes (architecture 7):
// each changed path takes the class of the first rule with a matching glob,
// else Default, and the pull request takes the most severe class among its
// paths. Classes are listed most severe first. Only auto, on class normal,
// merges without a human, and only where the project's merge rule is auto
// and the reproduction and the model review say so (ruling 149).
type Risk struct {
	Classes []RiskClass `yaml:"classes" json:"classes"`
	Rules   []RiskRule  `yaml:"rules" json:"rules"`
	Default string      `yaml:"default" json:"default"`
}

type RiskClass struct {
	Name    string `yaml:"name" json:"name"`
	Outcome string `yaml:"outcome" json:"outcome"`
	// Migration marks paths whose change needs the human's schema review.
	Migration bool `yaml:"migration" json:"migration"`
}

// RiskRule: Paths are globs anchored at the repository root, "/"
// separated; "**" matches any number of whole segments, other segments
// match as path.Match.
type RiskRule struct {
	Paths []string `yaml:"paths" json:"paths"`
	Class string   `yaml:"class" json:"class"`
}

// Risk outcomes.
const (
	OutcomeForbidden        = "forbidden"
	OutcomeRequiresApproval = "requires_approval"
	OutcomeAuto             = "auto"
)

// ClassNormal is the one class outcome auto is allowed on.
const ClassNormal = "normal"

func (r Risk) validate() error {
	if len(r.Classes) == 0 {
		return errors.New("policy: risk.classes is empty")
	}
	var names []string
	for _, c := range r.Classes {
		if c.Name == "" {
			return errors.New("policy: a risk class has an empty name")
		}
		if slices.Contains(names, c.Name) {
			return fmt.Errorf("policy: risk class %s is listed twice", c.Name)
		}
		if c.Outcome != OutcomeForbidden && c.Outcome != OutcomeRequiresApproval && c.Outcome != OutcomeAuto {
			return fmt.Errorf("policy: risk class %s has outcome %q, want %s, %s or %s", c.Name, c.Outcome, OutcomeForbidden, OutcomeRequiresApproval, OutcomeAuto)
		}
		if c.Outcome == OutcomeAuto && (c.Name != ClassNormal || c.Migration) {
			return fmt.Errorf("policy: risk class %s: outcome auto is allowed on class %s only", c.Name, ClassNormal)
		}
		names = append(names, c.Name)
	}
	if !slices.Contains(names, r.Default) {
		return fmt.Errorf("policy: risk.default %q is not a risk class", r.Default)
	}
	for i, rule := range r.Rules {
		if !slices.Contains(names, rule.Class) {
			return fmt.Errorf("policy: risk rule %d names unknown class %q", i+1, rule.Class)
		}
		if len(rule.Paths) == 0 {
			return fmt.Errorf("policy: risk rule %d has no paths", i+1)
		}
		for _, p := range rule.Paths {
			if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
				return fmt.Errorf("policy: risk rule %d path %q: want a relative glob like dir/**", i+1, p)
			}
			for _, seg := range strings.Split(p, "/") {
				if _, err := path.Match(seg, ""); err != nil || seg == "" {
					return fmt.Errorf("policy: risk rule %d path %q is not a valid glob", i+1, p)
				}
			}
		}
	}
	return nil
}

// Classification is a pull request's risk: its class, the paths that put
// it there, and the changed paths of a migration class.
type Classification struct {
	Class      RiskClass `json:"class"`
	Paths      []string  `json:"paths"`
	Migrations []string  `json:"migrations"`
}

// Classify classifies the changed paths (a rename lists both names).
func (r Risk) Classify(changed []string) Classification {
	rank := func(name string) int {
		return slices.IndexFunc(r.Classes, func(c RiskClass) bool { return c.Name == name })
	}
	best := rank(r.Default)
	byClass := map[int][]string{}
	var migrations []string
	for _, p := range changed {
		i := rank(r.classOf(p))
		byClass[i] = append(byClass[i], p)
		best = min(best, i)
		if r.Classes[i].Migration {
			migrations = append(migrations, p)
		}
	}
	slices.Sort(migrations)
	paths := byClass[best]
	slices.Sort(paths)
	return Classification{Class: r.Classes[best], Paths: slices.Compact(paths), Migrations: slices.Compact(migrations)}
}

func (r Risk) classOf(p string) string {
	name := strings.Split(p, "/")
	for _, rule := range r.Rules {
		for _, g := range rule.Paths {
			if globMatch(strings.Split(g, "/"), name) {
				return rule.Class
			}
		}
	}
	return r.Default
}

func globMatch(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if globMatch(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
