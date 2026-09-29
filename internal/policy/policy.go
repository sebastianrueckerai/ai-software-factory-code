// Package policy is the factory's capability policy and cost ledger: the
// policy document (authored in Git, loaded by factoryd into immutable
// policy_versions rows, enforced by the gateway from Postgres) and the
// ledger both binaries book costs into.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// Document is policy.yaml, schema version 1: budgets, limits, capability
// classes, prices, risk rules, the default envelope, approvers, activity
// bounds and the supervisor's detectors.
type Document struct {
	// Version is the file's schema version.
	Version int `yaml:"version" json:"version"`
	// Budget caps spend in EUR per task and per UTC day.
	Budget Budget `yaml:"budget" json:"budget"`
	// Limits bound every task's work: sessions, agent time, supervisor
	// calls.
	Limits Limits `yaml:"limits" json:"limits"`
	// Classes are the capability classes a task can hold, by name.
	Classes map[string]Class `yaml:"classes" json:"classes"`
	// Prices are the tool prices no provider API reports.
	Prices ToolPrices `yaml:"prices,omitempty" json:"prices"`
	// Risk classifies a pull request by the paths it changes.
	Risk Risk `yaml:"risk" json:"risk"`
	// EnvelopeDefault is the resource envelope of a project whose file
	// sets none.
	EnvelopeDefault Envelope `yaml:"envelope_default" json:"envelope_default"`
	// Approvers are the Forgejo users who may authorize spend in the
	// console (ruling 128): approve a resource request or a merge, promote
	// a resource. Empty: nobody.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers"`
	// Bounds are how many seconds one run of an activity may take before
	// it raises a gate_bound alert, per activity name and a default for the
	// rest; absent keys take DefaultBounds.
	Bounds map[string]int `yaml:"bounds,omitempty" json:"bounds"`
	// Supervisor holds the session supervisor's progress detectors: how
	// many seconds without the progress each names before it trips;
	// absent keys take DefaultDetectors (ruling 161: no_write only).
	Supervisor map[string]int `yaml:"supervisor,omitempty" json:"supervisor"`
	// ProjectDefault is what a project file leaves out takes; absent:
	// DefaultProject.
	ProjectDefault *ProjectDefault `yaml:"project_default,omitempty" json:"project_default,omitempty"`
}

// Limits bound a task's work; a task keeps the limits of the version it
// started under.
type Limits struct {
	// Sessions is how many agent sessions a task gets before it goes to a
	// human with every finding so far (the round cap); a human may grant
	// more.
	Sessions int `yaml:"sessions" json:"sessions"`
	// AgentTime is the agent's working time across all sessions of a task,
	// such as 20m; waiting on checks or on a reviewer does not count.
	AgentTime time.Duration `yaml:"agent_time" json:"agent_time"`
	// SupervisorCalls is how many supervisor model calls one session may
	// make; the next trip escalates without one.
	SupervisorCalls int `yaml:"supervisor_calls" json:"supervisor_calls"`
	// SupervisorTimeout bounds one supervisor or review model call, such as
	// 6m.
	SupervisorTimeout time.Duration `yaml:"supervisor_timeout" json:"supervisor_timeout"`
	// StrongerAfter is how many returns from the same stage a task takes
	// on its model; the next return takes the stronger rung.
	StrongerAfter int `yaml:"stronger_after" json:"stronger_after"`
	// Steps caps one agent session's model turns (opencode's agent steps),
	// at least 1: at the cap the harness asks the model to summarise and
	// stop. Absent: DefaultSteps.
	Steps int `yaml:"steps,omitempty" json:"steps"`
}

// DefaultSteps is the step cap of a policy without limits.steps, and of a
// version pinned before the key existed: a turn takes 5 to 15 s, so 200
// steps outlast the 20m agent time at the slow end and cut only a fast
// loop that turns without end (opencode #45442: 364 calls in 50 minutes).
const DefaultSteps = 200

// StepCap is Steps, or DefaultSteps when the policy sets none.
func (l Limits) StepCap() int {
	if l.Steps == 0 {
		return DefaultSteps
	}
	return l.Steps
}

// limitsJSON is Limits with its durations as strings, as in the YAML.
type limitsJSON struct {
	Sessions          int    `json:"sessions"`
	AgentTime         string `json:"agent_time"`
	SupervisorCalls   int    `json:"supervisor_calls"`
	SupervisorTimeout string `json:"supervisor_timeout"`
	StrongerAfter     int    `json:"stronger_after"`
	Steps             int    `json:"steps"`
}

func (l Limits) MarshalJSON() ([]byte, error) {
	return json.Marshal(limitsJSON{l.Sessions, l.AgentTime.String(), l.SupervisorCalls, l.SupervisorTimeout.String(), l.StrongerAfter, l.Steps})
}

func (l *Limits) UnmarshalJSON(b []byte) error {
	var j limitsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	out := Limits{Sessions: j.Sessions, SupervisorCalls: j.SupervisorCalls, StrongerAfter: j.StrongerAfter, Steps: j.Steps}
	for _, d := range []struct {
		s  string
		to *time.Duration
	}{{j.AgentTime, &out.AgentTime}, {j.SupervisorTimeout, &out.SupervisorTimeout}} {
		if d.s == "" {
			continue
		}
		v, err := time.ParseDuration(d.s)
		if err != nil {
			return fmt.Errorf("limits: %w", err)
		}
		*d.to = v
	}
	*l = out
	return nil
}

// MaxSupervisorTimeout is the longest supervisor_timeout a policy may set.
const MaxSupervisorTimeout = 15 * time.Minute

func (l Limits) validate() error {
	switch {
	case l.Sessions <= 0:
		return fmt.Errorf("policy: limits.sessions must be positive, is %d", l.Sessions)
	case l.AgentTime < time.Minute:
		return fmt.Errorf("policy: limits.agent_time must be at least 1m, is %s", l.AgentTime)
	case l.SupervisorCalls <= 0:
		return fmt.Errorf("policy: limits.supervisor_calls must be positive, is %d", l.SupervisorCalls)
	case l.SupervisorTimeout < 30*time.Second || l.SupervisorTimeout > MaxSupervisorTimeout:
		return fmt.Errorf("policy: limits.supervisor_timeout must lie between 30s and %s, is %s", MaxSupervisorTimeout, l.SupervisorTimeout)
	case l.StrongerAfter < 0:
		return fmt.Errorf("policy: limits.stronger_after must not be negative, is %d", l.StrongerAfter)
	case l.Steps < 0:
		return fmt.Errorf("policy: limits.steps must not be negative, is %d", l.Steps)
	}
	return nil
}

// DetectorNoWrite is the progress detector (rulings 141, 161): agent
// events keep coming but no file write or edit tool call.
const DetectorNoWrite = "no_write"

// DefaultDetectors are the seconds a policy without them gets.
var DefaultDetectors = map[string]int{DetectorNoWrite: 300}

// Detector is how long the named progress detector waits before it trips.
func (d Document) Detector(name string) time.Duration {
	s, ok := d.Supervisor[name]
	if !ok {
		s = DefaultDetectors[name]
	}
	return time.Duration(s) * time.Second
}

// BoundDefault is the bounds key for every activity without its own.
const BoundDefault = "default"

// DefaultBounds are the bounds a policy without them gets (rulings 140,
// 154): a merge attempt is one API call under a 2-minute timeout, its
// retries on workflow timers (ruling 163). RunSession, RunReport, Screen and SuperviseGate
// follow the limits (Bound).
var DefaultBounds = map[string]int{
	"WaitChecks": 600, "GateReproduction": 900, "ModelReview": 180, "GateAudit": 360, "Merge": 90, BoundDefault: 120,
}

// BoundActivities are factoryd's activity names, the keys bounds takes
// besides default.
var BoundActivities = []string{
	"EnsureIssue", "SetState", "RunSession", "OpenPullRequest", "WaitChecks", "PullRequestMerged", "RecordStep",
	"Finish", "Reconcile", "Escalate", "RecordUsage", "GatePolicy", "GateMigration", "GateReproduction",
	"ModelReview", "RequestApproval", "Merge", "ReportMergeFailure", "ReportMergeRetriesExhausted", "SuperviseGate",
	"AutoMergeNote", "RequestInfo", "CloseNotReproduced", "Screen", "JudgeScreening", "RunReport", "RollCommit", "GateAudit",
}

// Bound is how long one run of activity may take before it alerts: the
// policy's bound for it; else for a session the agent time plus a minute
// (a session at the cap is stopped by the cap already) and for the gate
// supervisor its call's timeout plus two; else DefaultBounds' for it, else
// the policy's default, else DefaultBounds' default.
func (d Document) Bound(activity string) time.Duration {
	s, ok := d.Bounds[activity]
	switch {
	case ok:
	case activity == "RunSession" || activity == "RunReport":
		return d.Limits.AgentTime + time.Minute
	case activity == "Screen":
		// the screening's report session, and its pod start
		return d.Limits.AgentTime + 2*time.Minute
	case activity == "SuperviseGate":
		return d.Limits.SupervisorTimeout + 2*time.Minute
	}
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
	// MonthlyEUR caps a calendar month (UTC), reservations and accrued
	// cost together, in net EUR.
	MonthlyEUR float64 `yaml:"monthly_eur" json:"monthly_eur"`
	// ServerTypes are the Hetzner server types a task may commission.
	ServerTypes []string `yaml:"server_types" json:"server_types"`
	// MaxCount is how many servers may run at once.
	MaxCount int `yaml:"max_count" json:"max_count"`
	// MaxLeaseHours is the longest lease.
	MaxLeaseHours int `yaml:"max_lease_hours" json:"max_lease_hours"`
	// VolumeMaxGB is the largest volume per server: 0 for none, else at
	// least 10.
	VolumeMaxGB int `yaml:"volume_max_gb" json:"volume_max_gb"`
	// Standing is the second block, for standing resources; the policy
	// sets its default in project_default, not in envelope_default.
	Standing Standing `yaml:"-" json:"-"`
}

// MinVolumeGB is the smallest volume Hetzner creates.
const MinVolumeGB = 10

func (e Envelope) validate(key string) error {
	if len(e.ServerTypes) == 0 || slices.Contains(e.ServerTypes, "") {
		return fmt.Errorf("%s.server_types must list at least one server type, none empty", key)
	}
	if e.MonthlyEUR <= 0 {
		return fmt.Errorf("%s.monthly_eur must be positive", key)
	}
	if e.MaxCount < 1 {
		return fmt.Errorf("%s.max_count must be at least 1", key)
	}
	if e.MaxLeaseHours < 1 {
		return fmt.Errorf("%s.max_lease_hours must be at least 1", key)
	}
	if e.VolumeMaxGB != 0 && e.VolumeMaxGB < MinVolumeGB {
		return fmt.Errorf("%s.volume_max_gb must be 0 (no volumes) or at least %d", key, MinVolumeGB)
	}
	return nil
}

// Budget caps spend in EUR: per task (default and maximum at intake) and
// per UTC day over all tasks together; a day cap of 0 stops all spend.
type Budget struct {
	// TaskDefaultEUR is a task's budget when its intake asks for none.
	TaskDefaultEUR float64 `yaml:"task_default_eur" json:"task_default_eur"`
	// TaskMaxEUR is the most a task may be given.
	TaskMaxEUR float64 `yaml:"task_max_eur" json:"task_max_eur"`
	// DayEUR caps all tasks together per UTC day; 0 stops all spend.
	DayEUR float64 `yaml:"day_eur" json:"day_eur"`
}

// Class is a capability class a task can hold: the model aliases it may
// call through the gateway and its tools with per-task caps, and the
// aliases factoryd calls on the task's account, which the agent may not.
type Class struct {
	// Models are the aliases the agent may call through the gateway.
	Models []string `yaml:"models" json:"models"`
	// Tools are the gateway tools the class may call: a paid tool with its
	// per-task caps, a free tool as {}.
	Tools map[string]ToolCap `yaml:"tools,omitempty" json:"tools"`
	// ReviewModel is the alias factoryd's model review calls on the task's
	// account.
	ReviewModel string `yaml:"review_model,omitempty" json:"review_model,omitempty"`
	// SupervisorModel is the alias the session and gate supervisors call
	// on the task's account.
	SupervisorModel string `yaml:"supervisor_model,omitempty" json:"supervisor_model,omitempty"`
	// AuditModel is the alias factoryd's security audit calls on the
	// task's account (ruling 188).
	AuditModel string `yaml:"audit_model,omitempty" json:"audit_model,omitempty"`
}

// The purposes factoryd calls a model for on a task's account.
const (
	PurposeReview         = "review"
	PurposeSupervisor     = "supervisor"
	PurposeGateSupervisor = "gate-supervisor"
	PurposeAudit          = "audit"
)

// Purposes are the factoryd purposes the gateway admits.
var Purposes = []string{PurposeReview, PurposeSupervisor, PurposeGateSupervisor, PurposeAudit}

// ToolCap caps a paid tool per task; a free tool carries none.
type ToolCap struct {
	// MaxRequests is how many calls of the tool a task may make.
	MaxRequests int `yaml:"max_requests,omitempty" json:"max_requests"`
	// MaxEUR is how much a task may spend on the tool.
	MaxEUR float64 `yaml:"max_eur,omitempty" json:"max_eur"`
}

// ClassCoder is the class intake gives agent tasks.
const ClassCoder = "coder"

// ClassAnalyzer is the class of a report session (ruling 185): it reads
// and writes a comment, it changes no code and commissions nothing.
const ClassAnalyzer = "analyzer"

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
	ToolRequestInfo    = "request_info"
)

// FreeTools are the tools a class lists without caps.
var FreeTools = []string{ToolEscalate, ToolRequestInfo, ToolCloudPrices, ToolCommission, ToolListResources, ToolRelease, ToolSSHCertificate, ToolRequestStatus}

// ToolPrices are the tool prices no provider API reports.
type ToolPrices struct {
	// Firecrawl prices web_scrape and web_search.
	Firecrawl FirecrawlPrices `yaml:"firecrawl,omitempty" json:"firecrawl"`
	// Replicate prices replicate_predict.
	Replicate ReplicatePrices `yaml:"replicate,omitempty" json:"replicate"`
	// Fal bounds fal_run.
	Fal FalPrices `yaml:"fal,omitempty" json:"fal"`
}

// FirecrawlPrices: a call costs its creditsUsed at EURPerCredit; the
// credit figures are the worst case a call is held at.
type FirecrawlPrices struct {
	// EURPerCredit is what one Firecrawl credit costs.
	EURPerCredit float64 `yaml:"eur_per_credit" json:"eur_per_credit"`
	// ScrapeCredits is the most credits one scrape is held at.
	ScrapeCredits int `yaml:"scrape_credits" json:"scrape_credits"`
	// SearchCredits is the most credits one search is held at.
	SearchCredits int `yaml:"search_credits" json:"search_credits"`
}

// ReplicatePrices: a prediction costs its predict_time at its hardware's
// rate. Replicate's API names no model's hardware, so Models maps each
// usable model (owner/name) to its hardware SKU; any other model is
// refused. A prediction is held at MaxSeconds of its hardware, and
// Replicate cancels it ReplicateCancelMargin seconds before that
// (Cancel-After), so it can never bill more than its hold.
type ReplicatePrices struct {
	// EURPerSecond is the price of a second of predict_time by hardware
	// SKU.
	EURPerSecond map[string]float64 `yaml:"eur_per_second" json:"eur_per_second"`
	// Models maps each usable model (owner/name) to its hardware SKU.
	Models map[string]string `yaml:"models" json:"models"`
	// MaxSeconds is how many seconds of its hardware a prediction is held
	// at, whole seconds, at least 10.
	MaxSeconds float64 `yaml:"max_seconds" json:"max_seconds"`
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
	// MaxMegapixels is how many megapixels a call priced per megapixel is
	// held at.
	MaxMegapixels float64 `yaml:"max_megapixels" json:"max_megapixels"`
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
	if err := d.Limits.validate(); err != nil {
		return err
	}
	if len(d.Classes) == 0 {
		return errors.New("policy: no capability classes")
	}
	if err := d.Risk.validate(); err != nil {
		return err
	}
	if err := d.EnvelopeDefault.validate("envelope_default"); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if d.ProjectDefault != nil {
		if err := d.ProjectDefault.validate(); err != nil {
			return fmt.Errorf("policy: %w", err)
		}
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
	for name, s := range d.Supervisor {
		if _, ok := DefaultDetectors[name]; !ok {
			return fmt.Errorf("policy: supervisor: unknown detector %q (known: %s)", name, DetectorNoWrite)
		}
		if s <= 0 {
			return fmt.Errorf("policy: supervisor.%s must be a positive number of seconds, is %d", name, s)
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

// Tools are the tools any of classes may call, sorted: what the gateway
// lists to the task and the brief names.
func (d Document) Tools(classes []string) []string {
	var out []string
	for _, tool := range append(slices.Clone(PaidTools), FreeTools...) {
		if d.AllowsTool(classes, tool) {
			out = append(out, tool)
		}
	}
	slices.Sort(out)
	return out
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

// FactoryModel is the alias factoryd calls for purpose on a task of
// classes: the first of classes that names one; "" when none does.
func (d Document) FactoryModel(classes []string, purpose string) string {
	for _, c := range classes {
		cl := d.Classes[c]
		var alias string
		switch purpose {
		case PurposeReview:
			alias = cl.ReviewModel
		case PurposeSupervisor, PurposeGateSupervisor:
			alias = cl.SupervisorModel
		case PurposeAudit:
			alias = cl.AuditModel
		}
		if alias != "" {
			return alias
		}
	}
	return ""
}

// FactoryModels are the aliases factoryd may call on a task of classes,
// sorted, once each.
func (d Document) FactoryModels(classes []string) []string {
	var out []string
	for _, p := range Purposes {
		if m := d.FactoryModel(classes, p); m != "" {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
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
