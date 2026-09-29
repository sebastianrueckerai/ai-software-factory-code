package policy

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// ProjectDefault is what a project file leaves out takes (ruling 146):
// the in-cluster quota of each of its namespaces and its envelope's
// standing block (ruling 147).
type ProjectDefault struct {
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
var DefaultProject = ProjectDefault{Quota: Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
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

// DefaultEnvelope is the envelope of a project whose file sets none: the
// task block from envelope_default, the standing block from
// project_default.
func (d Document) DefaultEnvelope() Envelope {
	env := d.EnvelopeDefault
	env.ServerTypes = slices.Clone(env.ServerTypes)
	env.Standing = d.Project().Standing
	return env
}

// EnvelopeOf is p's envelope: its file's task and standing blocks, each
// absent one the default's.
func (d Document) EnvelopeOf(p ProjectConfig) Envelope {
	env := d.DefaultEnvelope()
	if p.Envelope != nil {
		env = *p.Envelope
		env.ServerTypes = slices.Clone(env.ServerTypes)
		env.Standing = d.Project().Standing
	}
	if p.Standing != nil {
		env.Standing = *p.Standing
		env.Standing.ServerTypes = slices.Clone(p.Standing.ServerTypes)
	}
	return env
}

// QuotaOf is p's namespaces' quota: its file's, else project_default's.
func (d Document) QuotaOf(p ProjectConfig) Quota {
	if p.Quota != nil {
		return *p.Quota
	}
	return d.Project().Quota
}

// ProjectOf is the project whose file names repository.
func (t Tree) ProjectOf(repository string) (string, ProjectConfig, bool) {
	for name, p := range t.Projects {
		if p.Repository == repository {
			return name, p, true
		}
	}
	return "", ProjectConfig{}, false
}

// EnvelopeOf is the envelope of repository's project, the default one for
// a repository no file names.
func (t Tree) EnvelopeOf(repository string) Envelope {
	_, p, _ := t.ProjectOf(repository)
	return t.Document.EnvelopeOf(p)
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
	// Audit runs the workflow's audit stage on the project's pull
	// requests (ruling 188); absent: it does not.
	Audit bool `yaml:"audit,omitempty" json:"audit,omitempty"`
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
	// Envelope bounds what the project's tasks commission in the agent
	// cloud project without a human; absent: envelope_default.
	Envelope *Envelope `yaml:"envelope,omitempty" json:"envelope,omitempty"`
	// Standing bounds the project's standing resources; absent:
	// project_default's.
	Standing *Standing `yaml:"standing,omitempty" json:"standing,omitempty"`
	// Tokens name the seeded secrets factoryd reaches a seed project's
	// repository with; a factory project's are registration's own.
	Tokens *SeedTokens `yaml:"tokens,omitempty" json:"tokens,omitempty"`
}

// SeedTokens name, for a seed project, which of factoryd's seeded Forgejo
// secrets does what on its repository (ruling 179). factory-projects' is
// none of them: it holds nothing in sf (ruling 164).
type SeedTokens struct {
	// Read reads the collaborators and assignees (the trust gate) and the
	// webhooks where its user administers the repository.
	Read string `yaml:"read" json:"read"`
	// Actions lists the Actions jobs and registers the runners; agents
	// coder only.
	Actions string `yaml:"actions,omitempty" json:"actions,omitempty"`
	// Merge merges an approved pull request; agents coder only.
	Merge string `yaml:"merge,omitempty" json:"merge,omitempty"`
}

// The seeded secrets a seed project may name: Kubernetes secret names, no
// credentials.
const (
	SecretCIRegistrar = "forgejo-ci-registrar" // #nosec G101 -- a secret's name
	SecretMerge       = "forgejo-merge"        // #nosec G101 -- a secret's name
	SecretConfig      = "forgejo-config"       // #nosec G101 -- a secret's name
)

// SeedSecrets are the tokens values.
var SeedSecrets = []string{SecretCIRegistrar, SecretMerge, SecretConfig}

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
	dnsLabel        = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	deployPath      = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	hostname        = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	forgejoLogin    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)
	secretEntry     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/[A-Za-z0-9_.-]{1,64}$`)
)

// reservedNamespace: namespaces registration never creates, the
// cluster's and the factory's own.
func reservedNamespace(ns string) bool {
	switch ns {
	case "default", "factory", "flux-system", "forgejo", "platform", "cnpg-system", "local-path-storage", "cilium-secrets":
		return true
	}
	return strings.HasPrefix(ns, "kube-") || strings.HasPrefix(ns, "factory-")
}

// Validate checks the file's shape; name is its file name without .yaml.
// A factory project is its own organisation <name>/<name> (ruling 164)
// whose objects registration creates; a seed project's objects are the
// infra repository's and the seed's, so its file names the seeded tokens
// factoryd reads it with and nothing registration would create.
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
	for _, u := range p.Notify {
		if !forgejoLogin.MatchString(u) {
			bad("notify %q: not a Forgejo user name", u)
		}
	}
	for i, ns := range p.Namespaces {
		if !dnsLabel.MatchString(ns) || slices.Contains(p.Namespaces[:i], ns) {
			bad("namespace %q: a distinct DNS label", ns)
		}
	}
	var hosts []string
	for i, e := range p.Environments {
		if !slices.Contains(p.Namespaces, e.Namespace) {
			bad("environment %q: not one of the project's namespaces", e.Namespace)
		}
		if slices.ContainsFunc(p.Environments[:i], func(o Environment) bool { return o.Namespace == e.Namespace }) {
			bad("environment %q: given twice", e.Namespace)
		}
		if !deployPath.MatchString(e.Path) {
			bad("environment %s: path %q: a relative directory such as deploy/staging", e.Namespace, e.Path)
		}
		if e.Hostname != "" {
			if !hostname.MatchString(e.Hostname) || slices.Contains(hosts, e.Hostname) {
				bad("environment %s: hostname %q: a distinct DNS name", e.Namespace, e.Hostname)
			}
			hosts = append(hosts, e.Hostname)
		}
	}
	if p.Quota != nil {
		if err := p.Quota.Validate(); err != nil {
			bad("%v", err)
		}
	}
	for i, s := range p.Secrets {
		if !secretEntry.MatchString(s) || slices.Contains(p.Secrets[:i], s) {
			bad("secret %q: want secret-name/key, each once", s)
		}
	}
	if p.Envelope != nil {
		if err := p.Envelope.validate("envelope"); err != nil {
			bad("%v", err)
		}
	}
	if p.Standing != nil {
		if err := p.Standing.Validate(); err != nil {
			bad("%v", err)
		}
	}
	switch p.Objects {
	case ObjectsFactory:
		if strings.HasPrefix(name, "factory-") || name == "flux-system" || name == "sf" {
			bad("objects factory: the name is its Forgejo organisation's, not factory-*, flux-system or sf")
		}
		if p.Repository != name+"/"+name {
			bad("objects factory: repository %q: a project is its own organisation, its repository %s/%s", p.Repository, name, name)
		}
		for _, ns := range p.Namespaces {
			if reservedNamespace(ns) {
				bad("objects factory: namespace %q is the cluster's or the factory's", ns)
			}
		}
		if p.Tokens != nil {
			bad("objects factory: tokens are registration's, the file names none")
		}
	case ObjectsSeed:
		if len(p.Environments) > 0 || p.Quota != nil || len(p.Secrets) > 0 {
			bad("objects seed: environments, quota and secrets are registration's objects, which a seed project's infra repository owns")
		}
		switch {
		case p.Tokens == nil || p.Tokens.Read == "":
			bad("objects seed: tokens.read names the secret factoryd reads the repository with")
		case p.Agents == AgentsCoder && (p.Tokens.Actions == "" || p.Tokens.Merge == ""):
			bad("objects seed with agents coder: tokens.actions and tokens.merge name the secrets its CI and its merges use")
		}
		if p.Tokens != nil {
			for _, t := range []struct{ key, v string }{{"read", p.Tokens.Read}, {"actions", p.Tokens.Actions}, {"merge", p.Tokens.Merge}} {
				if t.v != "" && !slices.Contains(SeedSecrets, t.v) {
					bad("tokens.%s %q: want one of %v", t.key, t.v, SeedSecrets)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// checkProjects: a repository and a namespace belong to one project each.
func checkProjects(projects map[string]ProjectConfig) error {
	var errs []error
	repos, namespaces := map[string]string{}, map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(projects)) {
		p := projects[name]
		if other, ok := repos[p.Repository]; ok {
			errs = append(errs, fmt.Errorf("project %s: repository %s is project %s's", name, p.Repository, other))
		}
		repos[p.Repository] = name
		for _, ns := range p.Namespaces {
			if other, ok := namespaces[ns]; ok {
				errs = append(errs, fmt.Errorf("project %s: namespace %s is project %s's", name, ns, other))
			}
			namespaces[ns] = name
		}
	}
	return errors.Join(errs...)
}
