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
	MergeRule string   `yaml:"merge_rule" json:"merge_rule"`
	Quota     Quota    `yaml:"quota" json:"quota"`
	Standing  Standing `yaml:"standing" json:"standing"`
}

// Quota is one namespace's ResourceQuota in Kubernetes quantities: CPU and
// memory bound requests and limits alike, storage the claims' requests.
type Quota struct {
	CPU     string `yaml:"cpu" json:"cpu"`
	Memory  string `yaml:"memory" json:"memory"`
	Storage string `yaml:"storage" json:"storage"`
}

// Standing bounds a project's standing resources: no lease, created by
// registration or a human, never by an agent session, a fixed monthly line.
type Standing struct {
	MonthlyEUR  float64  `yaml:"monthly_eur" json:"monthly_eur"`
	ServerTypes []string `yaml:"server_types" json:"server_types"`
	MaxCount    int      `yaml:"max_count" json:"max_count"`
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
