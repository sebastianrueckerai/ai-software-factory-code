package policy

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// ProjectDefault is what a newly registered project starts with (ruling
// 146): its merge rule, the in-cluster quota of each of its namespaces,
// and its envelope's standing block (ruling 147).
type ProjectDefault struct {
	// MergeRule: auto merges a clean agent pull request without a human,
	// human waits for an approver.
	MergeRule string `yaml:"merge_rule" json:"merge_rule"`
	// Quota is each of the project's namespaces' ResourceQuota.
	Quota Quota `yaml:"quota" json:"quota"`
	// Standing bounds the project's standing resources.
	Standing Standing `yaml:"standing" json:"standing"`
}

// Quota is one namespace's ResourceQuota in Kubernetes quantities: CPU and
// memory bound requests and limits alike, storage the claims' requests.
type Quota struct {
	// CPU bounds CPU requests and limits, such as 2 or 500m.
	CPU string `yaml:"cpu" json:"cpu"`
	// Memory bounds memory requests and limits, such as 4Gi.
	Memory string `yaml:"memory" json:"memory"`
	// Storage bounds the persistent volume claims' requests, such as 10Gi.
	Storage string `yaml:"storage" json:"storage"`
}

// Standing bounds a project's standing resources: no lease, created by
// registration or a human, never by an agent session, a fixed monthly line.
type Standing struct {
	// MonthlyEUR is the fixed monthly line for standing resources.
	MonthlyEUR float64 `yaml:"monthly_eur" json:"monthly_eur"`
	// ServerTypes are the server types a standing resource may have.
	ServerTypes []string `yaml:"server_types" json:"server_types"`
	// MaxCount is how many standing servers may exist at once.
	MaxCount int `yaml:"max_count" json:"max_count"`
}

// Merge rules: auto merges a clean agent pull request without a human
// (ruling 149), human waits for an approver.
const (
	MergeAuto  = "auto"
	MergeHuman = "human"
)

// DefaultProject is what a policy without project_default gives.
var DefaultProject = ProjectDefault{MergeRule: MergeHuman, Quota: Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
	Standing: Standing{ServerTypes: []string{}}}

// Project is the policy's project_default, else DefaultProject.
func (d Document) Project() ProjectDefault {
	if d.ProjectDefault == nil {
		p := DefaultProject
		p.Standing.ServerTypes = slices.Clone(p.Standing.ServerTypes)
		return p
	}
	return *d.ProjectDefault
}

// DefaultEnvelope is the envelope a project's first row copies: the task
// block from envelope_default, the standing block from project_default.
func (d Document) DefaultEnvelope() Envelope {
	env := d.EnvelopeDefault
	env.ServerTypes = slices.Clone(env.ServerTypes)
	env.Standing = d.Project().Standing
	return env
}

var quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|Ki|Mi|Gi|Ti)?$`)

// Quantity reports whether q is a plain Kubernetes quantity (no exponent,
// no sign).
func Quantity(q string) bool { return quantity.MatchString(q) }

// Validate checks a quota; each field must be a quantity.
func (q Quota) Validate() error {
	for _, f := range []struct{ name, v string }{{"cpu", q.CPU}, {"memory", q.Memory}, {"storage", q.Storage}} {
		if !Quantity(f.v) {
			return fmt.Errorf("quota.%s %q is not a Kubernetes quantity such as 2, 500m or 4Gi", f.name, f.v)
		}
	}
	return nil
}

// Validate checks a standing block: zero values close it to new standing
// resources without a human raising it.
func (s Standing) Validate() error {
	if s.MonthlyEUR < 0 {
		return errors.New("standing.monthly_eur must not be negative")
	}
	if s.MaxCount < 0 {
		return errors.New("standing.max_count must not be negative")
	}
	if slices.Contains(s.ServerTypes, "") {
		return errors.New("standing.server_types must not list an empty type")
	}
	return nil
}

func (p ProjectDefault) validate() error {
	if p.MergeRule != MergeAuto && p.MergeRule != MergeHuman {
		return fmt.Errorf("project_default.merge_rule %q: want auto or human", p.MergeRule)
	}
	if err := p.Quota.Validate(); err != nil {
		return fmt.Errorf("project_default.%w", err)
	}
	if err := p.Standing.Validate(); err != nil {
		return fmt.Errorf("project_default.%w", err)
	}
	return nil
}

// ProjectConfig is projects/<name>.yaml: one project, the file name its
// name (rulings 146, 179).
type ProjectConfig struct {
	// Repository is the project's Forgejo repository, owner/name.
	Repository string `yaml:"repository" json:"repository"`
	// MergeRule: auto merges a clean agent pull request without a human,
	// human waits for an approver.
	MergeRule string `yaml:"merge_rule" json:"merge_rule"`
	// Objects names who creates and reconciles the project's repository
	// objects: factory (registration) or seed (the infra repository and
	// the seed; factoryd only reads).
	Objects string `yaml:"objects" json:"objects"`
	// Agents: coder lets agent tasks run, none sends every issue to the
	// human lane and refuses tasks.
	Agents string `yaml:"agents" json:"agents"`
	// Notify are the Forgejo users told about the project's cards; empty:
	// the policy's approvers.
	Notify []string `yaml:"notify,omitempty" json:"notify"`
	// Namespaces are the cluster namespaces the project deploys into.
	Namespaces []string `yaml:"namespaces,omitempty" json:"namespaces"`
	// Environments are the project's deploy directories, one Flux
	// Kustomization each.
	Environments []Environment `yaml:"environments,omitempty" json:"environments"`
	// Quota is each namespace's ResourceQuota; absent: project_default's.
	Quota *Quota `yaml:"quota,omitempty" json:"quota,omitempty"`
	// Secrets are secret/key entries: each namespace gets the secret with
	// a random value for the key when it is missing.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets"`
}

// Environment is one Flux Kustomization: path of the repository applied
// into namespace (ruling 157), and the public hostname, if any (ruling 155).
type Environment struct {
	// Namespace is one of the project's namespaces.
	Namespace string `yaml:"namespace" json:"namespace"`
	// Path is the repository directory applied into it, such as
	// deploy/staging.
	Path string `yaml:"path" json:"path"`
	// Hostname is the environment's public host name, if any.
	Hostname string `yaml:"hostname,omitempty" json:"hostname,omitempty"`
}

// Who creates a project's repository objects.
const (
	ObjectsFactory = "factory"
	ObjectsSeed    = "seed"
)

// ObjectOwners are the objects values.
var ObjectOwners = []string{ObjectsFactory, ObjectsSeed}

// Whether agents work on a project.
const (
	AgentsCoder = "coder"
	AgentsNone  = "none"
)

// AgentModes are the agents values.
var AgentModes = []string{AgentsCoder, AgentsNone}

// MergeRules are the merge_rule values.
var MergeRules = []string{MergeAuto, MergeHuman}

var (
	projectFileName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,35}[a-z0-9])?$`)
	repositoryName  = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)
)

// Validate checks the file's shape; name is its file name without .yaml.
func (p ProjectConfig) Validate(name string) error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("project %s: "+format, append([]any{name}, a...)...))
	}
	if !projectFileName.MatchString(name) {
		bad("the file name: lowercase letters, digits and dashes, at most 37")
	}
	if !repositoryName.MatchString(p.Repository) {
		bad("repository %q: owner/name in lower case", p.Repository)
	}
	if !slices.Contains(MergeRules, p.MergeRule) {
		bad("merge_rule %q: want one of %v", p.MergeRule, MergeRules)
	}
	if !slices.Contains(ObjectOwners, p.Objects) {
		bad("objects %q: want one of %v", p.Objects, ObjectOwners)
	}
	if !slices.Contains(AgentModes, p.Agents) {
		bad("agents %q: want one of %v", p.Agents, AgentModes)
	}
	for _, e := range p.Environments {
		if !slices.Contains(p.Namespaces, e.Namespace) {
			bad("environment %q: not one of the project's namespaces", e.Namespace)
		}
	}
	if p.Quota != nil {
		if err := p.Quota.Validate(); err != nil {
			bad("%v", err)
		}
	}
	return errors.Join(errs...)
}
