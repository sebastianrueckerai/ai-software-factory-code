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
	"math"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Document is schema version 1 of the policy file.
type Document struct {
	Version int              `yaml:"version" json:"version"`
	Budget  Budget           `yaml:"budget" json:"budget"`
	Classes map[string]Class `yaml:"classes" json:"classes"`
	Prices  ToolPrices       `yaml:"prices" json:"prices"`
	Risk    Risk             `yaml:"risk" json:"risk"`
	// EnvelopeDefault is the resource envelope a project's first
	// commissioning request copies into its envelopes row.
	EnvelopeDefault Envelope `yaml:"envelope_default" json:"envelope_default"`
	// Approvers are the Forgejo users who may authorize spend in the
	// console (ruling 128): approve a resource request or a merge, edit an
	// envelope, promote a resource. Empty: nobody.
	Approvers []string `yaml:"approvers" json:"approvers"`
	// Bounds are how many seconds one run of an activity may take before
	// it raises a gate_bound alert, per activity name and a default for the
	// rest; absent keys take DefaultBounds.
	Bounds map[string]int `yaml:"bounds" json:"bounds"`
}

// BoundDefault is the bounds key for every activity without its own.
const BoundDefault = "default"

// DefaultBounds are the bounds a policy without them gets (ruling 140).
var DefaultBounds = map[string]int{
	"RunSession": 1200, "WaitChecks": 600, "GateReproduction": 900, "ModelReview": 180, "Merge": 300, BoundDefault: 120,
}

// BoundActivities are factoryd's activity names, the keys bounds takes
// besides default.
var BoundActivities = []string{
	"EnsureIssue", "SetState", "RunSession", "OpenPullRequest", "WaitChecks", "PullRequestMerged", "RecordStep",
	"Finish", "Reconcile", "Escalate", "RecordUsage", "GatePolicy", "GateMigration", "GateReproduction",
	"ModelReview", "RequestApproval", "Merge", "ReportMergeFailure", "ReportMergeRetriesExhausted",
}

// Bound is how long one run of activity may take before it alerts: its own
// bound, else the policy's default, else DefaultBounds'.
func (d Document) Bound(activity string) time.Duration {
	s, ok := d.Bounds[activity]
	if !ok {
		s, ok = DefaultBounds[activity]
	}
	if !ok {
		s, ok = d.Bounds[BoundDefault]
	}
	if !ok {
		s = DefaultBounds[BoundDefault]
	}
	return time.Duration(s) * time.Second
}

// Envelope bounds what a project's tasks may commission in the agent
// cloud project without a human: EUR per calendar month (reservations and
// accrued cost together), the server types, how many servers at once, the
// longest lease and the largest volume.
type Envelope struct {
	MonthlyEUR    float64  `yaml:"monthly_eur" json:"monthly_eur"`
	ServerTypes   []string `yaml:"server_types" json:"server_types"`
	MaxCount      int      `yaml:"max_count" json:"max_count"`
	MaxLeaseHours int      `yaml:"max_lease_hours" json:"max_lease_hours"`
	VolumeMaxGB   int      `yaml:"volume_max_gb" json:"volume_max_gb"`
}

// MinVolumeGB is the smallest volume Hetzner creates.
const MinVolumeGB = 10

func (e Envelope) validate() error {
	if len(e.ServerTypes) == 0 || slices.Contains(e.ServerTypes, "") {
		return errors.New("envelope_default.server_types must list at least one server type, none empty")
	}
	if e.MonthlyEUR <= 0 {
		return errors.New("envelope_default.monthly_eur must be positive")
	}
	if e.MaxCount < 1 {
		return errors.New("envelope_default.max_count must be at least 1")
	}
	if e.MaxLeaseHours < 1 {
		return errors.New("envelope_default.max_lease_hours must be at least 1")
	}
	if e.VolumeMaxGB != 0 && e.VolumeMaxGB < MinVolumeGB {
		return fmt.Errorf("envelope_default.volume_max_gb must be 0 (no volumes) or at least %d", MinVolumeGB)
	}
	return nil
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

// The gateway's paid tools: a class allows one by listing it in
// classes.<c>.tools with its caps.
const (
	ToolWebScrape = "web_scrape"
	ToolWebSearch = "web_search"
	ToolReplicate = "replicate_predict"
	ToolFal       = "fal_run"
)

// PaidTools are the tools a class lists with caps.
var PaidTools = []string{ToolWebScrape, ToolWebSearch, ToolReplicate, ToolFal}

// The gateway's free tools: a class allows one by listing it in
// classes.<c>.tools without caps ({}). The commissioning tools spend
// through their own reservation and envelope, not a per-task tool cap.
const (
	ToolEscalate       = "escalate_to_human"
	ToolCloudPrices    = "cloud_prices"
	ToolCommission     = "commission_server"
	ToolListResources  = "list_resources"
	ToolRelease        = "release_resource"
	ToolSSHCertificate = "ssh_certificate"
	ToolRequestStatus  = "resource_request_status"
)

// FreeTools are the tools a class lists without caps.
var FreeTools = []string{ToolEscalate, ToolCloudPrices, ToolCommission, ToolListResources, ToolRelease, ToolSSHCertificate, ToolRequestStatus}

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
// refused. A prediction is held at MaxSeconds of its hardware, and
// Replicate cancels it ReplicateCancelMargin seconds before that
// (Cancel-After), so it can never bill more than its hold.
type ReplicatePrices struct {
	EURPerSecond map[string]float64 `yaml:"eur_per_second" json:"eur_per_second"`
	Models       map[string]string  `yaml:"models" json:"models"`
	MaxSeconds   float64            `yaml:"max_seconds" json:"max_seconds"`
}

// ReplicateCancelMargin (seconds) covers a cancelled prediction's wind-down;
// ReplicateMinSeconds leaves Replicate's Cancel-After minimum of 5 s.
const (
	ReplicateCancelMargin = 5
	ReplicateMinSeconds   = 10
)

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
	if err := d.Risk.validate(); err != nil {
		return err
	}
	if err := d.EnvelopeDefault.validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	for i, a := range d.Approvers {
		if a == "" || slices.Contains(d.Approvers[:i], a) {
			return fmt.Errorf("policy: approvers must be distinct, non-empty user names (%q)", a)
		}
	}
	for name, s := range d.Bounds {
		if name != BoundDefault && !slices.Contains(BoundActivities, name) {
			return fmt.Errorf("policy: bounds: unknown activity %q (known: %v and %s)", name, BoundActivities, BoundDefault)
		}
		if s <= 0 {
			return fmt.Errorf("policy: bounds.%s must be a positive number of seconds, is %d", name, s)
		}
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
			if slices.Contains(FreeTools, tool) {
				if tc != (ToolCap{}) {
					return fmt.Errorf("policy: class %s tool %s is free: list it as {}, without max_requests or max_eur", name, tool)
				}
				continue
			}
			if !slices.Contains(PaidTools, tool) {
				return fmt.Errorf("policy: class %s lists unknown tool %q (paid: %v, free: %v)", name, tool, PaidTools, FreeTools)
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
		if len(r.Models) == 0 {
			return errors.New("prices.replicate needs at least one model")
		}
		if r.MaxSeconds < ReplicateMinSeconds || r.MaxSeconds != math.Trunc(r.MaxSeconds) {
			return fmt.Errorf("prices.replicate.max_seconds must be whole seconds, at least %d", ReplicateMinSeconds)
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

// AllowsTool reports whether any of classes lists tool, paid or free.
func (d Document) AllowsTool(classes []string, tool string) bool {
	_, ok := d.ToolCap(classes, tool)
	return ok
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
