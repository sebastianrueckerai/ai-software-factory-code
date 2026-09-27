// Package policy is the factory's capability policy and cost ledger: the
// policy document (authored in Git, loaded by factoryd into immutable
// policy_versions rows, enforced by the gateway from Postgres) and the
// ledger both binaries book costs into.
package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"gopkg.in/yaml.v3"
)

// Document is schema version 1 of the policy file.
type Document struct {
	Version int              `yaml:"version" json:"version"`
	Budget  Budget           `yaml:"budget" json:"budget"`
	Classes map[string]Class `yaml:"classes" json:"classes"`
}

// Budget caps spend in EUR: per task (default and maximum at intake) and
// per UTC day over all tasks together.
type Budget struct {
	TaskDefaultEUR float64 `yaml:"task_default_eur" json:"task_default_eur"`
	TaskMaxEUR     float64 `yaml:"task_max_eur" json:"task_max_eur"`
	DayEUR         float64 `yaml:"day_eur" json:"day_eur"`
}

// Class is a capability class a task can hold: the model aliases it may
// call through the gateway and its tools with per-task caps.
type Class struct {
	Models []string           `yaml:"models" json:"models"`
	Tools  map[string]ToolCap `yaml:"tools" json:"tools"`
}

type ToolCap struct {
	MaxRequests int     `yaml:"max_requests" json:"max_requests"`
	MaxEUR      float64 `yaml:"max_eur" json:"max_eur"`
}

// Parse decodes and validates a policy file. Unknown keys are an error, so
// a typo never silently drops a rule.
func Parse(b []byte) (Document, error) {
	var d Document
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		if errors.Is(err, io.EOF) {
			return Document{}, errors.New("policy: empty document")
		}
		return Document{}, fmt.Errorf("policy: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Document{}, errors.New("policy: more than one YAML document")
	}
	return d, d.Validate()
}

func (d Document) Validate() error {
	if d.Version != 1 {
		return fmt.Errorf("policy: version %d, want 1", d.Version)
	}
	b := d.Budget
	if b.TaskDefaultEUR <= 0 || b.TaskMaxEUR <= 0 || b.DayEUR <= 0 {
		return errors.New("policy: budget.task_default_eur, task_max_eur and day_eur must all be positive")
	}
	if b.TaskDefaultEUR > b.TaskMaxEUR {
		return fmt.Errorf("policy: budget.task_default_eur %g exceeds task_max_eur %g", b.TaskDefaultEUR, b.TaskMaxEUR)
	}
	if len(d.Classes) == 0 {
		return errors.New("policy: no capability classes")
	}
	for name, c := range d.Classes {
		if name == "" {
			return errors.New("policy: a capability class has an empty name")
		}
		if len(c.Models) == 0 {
			return fmt.Errorf("policy: class %s allows no models", name)
		}
		if slices.Contains(c.Models, "") {
			return fmt.Errorf("policy: class %s lists an empty model", name)
		}
		for tool, tc := range c.Tools {
			if tool == "" || tc.MaxRequests <= 0 || tc.MaxEUR <= 0 {
				return fmt.Errorf("policy: class %s tool %q needs a name, a positive max_requests and max_eur", name, tool)
			}
		}
	}
	return nil
}

// AllowsModel reports whether any of classes may call model.
func (d Document) AllowsModel(classes []string, model string) bool {
	for _, c := range classes {
		if cl, ok := d.Classes[c]; ok && slices.Contains(cl.Models, model) {
			return true
		}
	}
	return false
}

// ClampBudget is a new task's budget: the default when none (or a
// non-positive one) was requested, never more than the task maximum.
func (b Budget) ClampBudget(requested float64) float64 {
	if requested <= 0 {
		requested = b.TaskDefaultEUR
	}
	return min(requested, b.TaskMaxEUR)
}
