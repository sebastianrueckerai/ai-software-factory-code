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
	Prices  ToolPrices       `yaml:"prices" json:"prices"`
}

// Budget caps spend in EUR: per task (default and maximum at intake) and
// per UTC day over all tasks together; a day cap of 0 stops all spend.
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

// ClassCoder is the class intake gives agent tasks.
const ClassCoder = "coder"

// The gateway's paid tools, the names classes.<c>.tools caps.
// ToolEscalate is free and always allowed for the coder class.
const (
	ToolWebScrape = "web_scrape"
	ToolWebSearch = "web_search"
	ToolReplicate = "replicate_predict"
	ToolFal       = "fal_run"
	ToolEscalate  = "escalate_to_human"
)

// PaidTools are the tools a class may list.
var PaidTools = []string{ToolWebScrape, ToolWebSearch, ToolReplicate, ToolFal}

// ToolPrices are the tool prices no provider API reports.
type ToolPrices struct {
	Firecrawl FirecrawlPrices `yaml:"firecrawl" json:"firecrawl"`
	Replicate ReplicatePrices `yaml:"replicate" json:"replicate"`
	Fal       FalPrices       `yaml:"fal" json:"fal"`
}

// FirecrawlPrices: a call costs its creditsUsed at EURPerCredit; the
// credit figures are the worst case a call is held at.
type FirecrawlPrices struct {
	EURPerCredit  float64 `yaml:"eur_per_credit" json:"eur_per_credit"`
	ScrapeCredits int     `yaml:"scrape_credits" json:"scrape_credits"`
	SearchCredits int     `yaml:"search_credits" json:"search_credits"`
}

// ReplicatePrices: a prediction costs its predict_time at its hardware's
// rate. Replicate's API names no model's hardware, so Models maps each
// usable model (owner/name) to its hardware SKU; any other model is
// refused. A prediction is held at MaxSeconds of its hardware.
type ReplicatePrices struct {
	EURPerSecond map[string]float64 `yaml:"eur_per_second" json:"eur_per_second"`
	Models       map[string]string  `yaml:"models" json:"models"`
	MaxSeconds   float64            `yaml:"max_seconds" json:"max_seconds"`
}

// FalPrices: fal's pricing API prices each endpoint per unit; a call
// priced per megapixel is held at MaxMegapixels.
type FalPrices struct {
	MaxMegapixels float64 `yaml:"max_megapixels" json:"max_megapixels"`
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
	if b.TaskDefaultEUR <= 0 || b.TaskMaxEUR <= 0 {
		return errors.New("policy: budget.task_default_eur and task_max_eur must be positive")
	}
	if b.DayEUR < 0 {
		return errors.New("policy: budget.day_eur must not be negative (0 stops all spend)")
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
			if tool == ToolEscalate {
				return fmt.Errorf("policy: class %s lists %s, which is free and always allowed for coder", name, tool)
			}
			if !slices.Contains(PaidTools, tool) {
				return fmt.Errorf("policy: class %s lists unknown tool %q (known: %v)", name, tool, PaidTools)
			}
			if tc.MaxRequests <= 0 || tc.MaxEUR <= 0 {
				return fmt.Errorf("policy: class %s tool %q needs a positive max_requests and max_eur", name, tool)
			}
			if err := d.Prices.validate(tool); err != nil {
				return fmt.Errorf("policy: class %s lists %s: %w", name, tool, err)
			}
		}
	}
	return nil
}

func (p ToolPrices) validate(tool string) error {
	switch tool {
	case ToolWebScrape, ToolWebSearch:
		f := p.Firecrawl
		if f.EURPerCredit <= 0 || f.ScrapeCredits <= 0 || f.SearchCredits <= 0 {
			return errors.New("prices.firecrawl needs a positive eur_per_credit, scrape_credits and search_credits")
		}
	case ToolReplicate:
		r := p.Replicate
		if r.MaxSeconds <= 0 || len(r.Models) == 0 {
			return errors.New("prices.replicate needs a positive max_seconds and at least one model")
		}
		for model, sku := range r.Models {
			if rate := r.EURPerSecond[sku]; rate <= 0 {
				return fmt.Errorf("prices.replicate model %s runs on %q, which has no positive eur_per_second", model, sku)
			}
		}
	case ToolFal:
		if p.Fal.MaxMegapixels <= 0 {
			return errors.New("prices.fal needs a positive max_megapixels")
		}
	}
	return nil
}

// ToolCap is the most generous cap any of classes grants tool; false when
// none lists it.
func (d Document) ToolCap(classes []string, tool string) (ToolCap, bool) {
	var best ToolCap
	found := false
	for _, c := range classes {
		if tc, ok := d.Classes[c].Tools[tool]; ok {
			best.MaxRequests = max(best.MaxRequests, tc.MaxRequests)
			best.MaxEUR = max(best.MaxEUR, tc.MaxEUR)
			found = true
		}
	}
	return best, found
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

// Models are the aliases any of classes may call, sorted, once each.
func (d Document) Models(classes []string) []string {
	var out []string
	for _, c := range classes {
		out = append(out, d.Classes[c].Models...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ClampBudget is a new task's budget: the default when none (or a
// non-positive one) was requested, never more than the task maximum.
func (b Budget) ClampBudget(requested float64) float64 {
	if requested <= 0 {
		requested = b.TaskDefaultEUR
	}
	return min(requested, b.TaskMaxEUR)
}
