package policy

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
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

// DefaultWorkInProgress is how many of a project's cards hold a work
// slot at once when its file sets no work_in_progress.
const DefaultWorkInProgress = 5

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

// ProjectCap is the name of repository's project and its month cap, its
// file's budget.month_eur: +Inf when the file sets none or no file names
// repository.
func (t Tree) ProjectCap(repository string) (string, float64) {
	name, p, _ := t.ProjectOf(repository)
	if p.Budget == nil {
		return name, math.Inf(1)
	}
	return name, p.Budget.MonthEUR
}

// WorkInProgressOf is how many of repository's cards hold a work slot at
// once: its project file's work_in_progress, else the default.
func (t Tree) WorkInProgressOf(repository string) int {
	if _, p, ok := t.ProjectOf(repository); ok && p.WorkInProgress > 0 {
		return p.WorkInProgress
	}
	return DefaultWorkInProgress
}

// ApproversOf are the approvers of repository's project: its file's,
// else the policy's.
func (t Tree) ApproversOf(repository string) []string {
	if _, p, _ := t.ProjectOf(repository); len(p.Approvers) > 0 {
		return p.Approvers
	}
	return t.Document.Approvers
}

// ForgejoLogin reports whether u is a Forgejo user name.
func ForgejoLogin(u string) bool { return forgejoLogin.MatchString(u) }

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
	// Process names the file processes/<name>.yaml the project's cards
	// pass; required with agents coder, not allowed with agents none.
	Process string `yaml:"process,omitempty" json:"process,omitempty"`
	// Template names the directory templates/<name>/ registration seeds
	// the empty repository's main with; a factory project's only.
	Template string `yaml:"template,omitempty" json:"template,omitempty"`
	// Audit runs the process's audit stage on the project's pull
	// requests (ruling 188); absent: it does not.
	Audit bool `yaml:"audit,omitempty" json:"audit,omitempty"`
	// Notify are the Forgejo users told about the project's cards; empty:
	// the project's approvers.
	Notify []string `yaml:"notify,omitempty" json:"notify"`
	// Approvers are the Forgejo users who may authorize the project's
	// spend in the console (approve its merges and resource requests,
	// accept and start its plans) and invite its members; empty: the
	// policy's approvers.
	Approvers []string `yaml:"approvers,omitempty" json:"approvers"`
	// Namespaces are the cluster namespaces the project deploys into.
	Namespaces []string `yaml:"namespaces,omitempty" json:"namespaces"`
	// Environments are the project's deploy directories, one Flux
	// Kustomization each.
	Environments []Environment `yaml:"environments,omitempty" json:"environments"`
	// Quota is each namespace's ResourceQuota; absent: project_default's.
	// Stricter wins: bounded by policy.yaml's project_default.quota, each
	// value at most the policy's; a looser file is rejected.
	Quota *Quota `yaml:"quota,omitempty" json:"quota,omitempty"`
	// Secrets are secret/key entries: each namespace gets the secret with
	// a random value for the key when it is missing.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets"`
	// Envelope bounds what the project's tasks commission in the agent
	// cloud project without a human; absent: envelope_default. Stricter
	// wins: bounded by policy.yaml's envelope_default, each number at most
	// the policy's, each server type one it lists; a looser file is
	// rejected.
	Envelope *Envelope `yaml:"envelope,omitempty" json:"envelope,omitempty"`
	// Standing bounds the project's standing resources; absent:
	// project_default's. Stricter wins: bounded by policy.yaml's
	// project_default.standing, each number at most the policy's, each
	// server type one it lists; a looser file is rejected.
	Standing *Standing `yaml:"standing,omitempty" json:"standing,omitempty"`
	// Egress are the host names outside the cluster the project's
	// namespaces may reach, on 443 only; absent: none. A factory project's
	// only: its network policy is registration's.
	Egress []string `yaml:"egress,omitempty" json:"egress"`
	// Tokens name the seeded secrets factoryd reaches a seed project's
	// repository with; a factory project's are registration's own.
	Tokens *SeedTokens `yaml:"tokens,omitempty" json:"tokens,omitempty"`
	// Time overrides limits.agent_time and limits.check_in (and the
	// class's or process's, if set) for the project's tasks: most
	// specific wins, a project's own file over the process, the process
	// over the class.
	Time *Time `yaml:"time,omitempty" json:"time,omitempty"`
	// Budget caps the spend of the project's tasks together; absent: the
	// project has no cap of its own. Stricter wins: bounded by policy.yaml's
	// budget.month_eur and budget.task_max_eur; a looser file is rejected.
	Budget *ProjectBudget `yaml:"budget,omitempty" json:"budget,omitempty"`
	// WorkInProgress is how many of the project's cards hold a work slot
	// at once (waiting on a dependency, working, in review or waiting on a
	// human); a queued card starts only under it. Absent: the default, 5.
	WorkInProgress int `yaml:"work_in_progress,omitempty" json:"work_in_progress,omitempty"`
	// Model runs a coding card's first session on Models.Stronger instead
	// of the ladder's first rung; the only accepted value is "stronger".
	// Absent: a coding card starts on the first rung. The planner and
	// report sessions keep their own rung.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Chat is the project agent's block for this project; its keys
	// replace the policy's.
	Chat *ChatConfig `yaml:"chat,omitempty" json:"chat,omitempty"`
}

// ProjectBudget caps a project's spend in EUR.
type ProjectBudget struct {
	// MonthEUR caps the project's tasks together per UTC calendar month;
	// 0 stops the project's spend.
	MonthEUR float64 `yaml:"month_eur" json:"month_eur"`
	// TaskMaxEUR is the project's own maximum for one card, up to the
	// policy's task_max_eur; absent: a large card's budget.
	TaskMaxEUR float64 `yaml:"task_max_eur,omitempty" json:"task_max_eur,omitempty"`
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

// ModelStronger is ProjectConfig.Model's only accepted value.
const ModelStronger = "stronger"

// ModelValues are the model values.
var ModelValues = []string{ModelStronger}

var (
	projectFileName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,35}[a-z0-9])?$`)
	repositoryName  = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)
	dnsLabel        = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	deployPath      = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	hostname        = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	forgejoLogin    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,38}[A-Za-z0-9])?$`)
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
	if p.WorkInProgress < 0 {
		bad("work_in_progress %d: a count of cards", p.WorkInProgress)
	}
	for _, u := range p.Notify {
		if !forgejoLogin.MatchString(u) {
			bad("notify %q: not a Forgejo user name", u)
		}
	}
	for i, u := range p.Approvers {
		if !forgejoLogin.MatchString(u) || slices.Contains(p.Approvers[:i], u) {
			bad("approvers %q: distinct Forgejo user names", u)
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
	for i, h := range p.Egress {
		if !hostname.MatchString(h) || slices.Contains(p.Egress[:i], h) {
			bad("egress %q: a distinct DNS name", h)
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
	if p.Time != nil {
		if err := p.Time.validate(); err != nil {
			bad("%v", err)
		}
	}
	if p.Budget != nil && p.Budget.MonthEUR < 0 {
		bad("budget.month_eur must not be negative (0 stops the project's spend)")
	}
	if p.Model != "" && !slices.Contains(ModelValues, p.Model) {
		bad("model %q: want one of %v", p.Model, ModelValues)
	}
	if err := p.Chat.validate(); err != nil {
		bad("%v", err)
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
		if len(p.Egress) > 0 {
			bad("objects seed: egress is the network policy registration writes, which a seed project's infra repository owns")
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

// checkBounds: a project file may only tighten what policy.yaml bounds
// (stricter wins): each quota, envelope, standing and budget value at most
// the policy's, each server type one the policy lists.
func (d Document) checkBounds(name string, p ProjectConfig) error {
	var errs []error
	looser := func(field string, v, bound any, policyField string) {
		errs = append(errs, fmt.Errorf("project %s: %s %v is looser than policy.yaml's %s %v", name, field, v, policyField, bound))
	}
	number := func(field string, v, bound float64, policyField string) {
		if v > bound {
			looser(field, v, bound, policyField)
		}
	}
	types := func(field string, v, bound []string, policyField string) {
		for _, t := range v {
			if !slices.Contains(bound, t) {
				errs = append(errs, fmt.Errorf("project %s: %s lists %s, which policy.yaml's %s does not", name, field, t, policyField))
			}
		}
	}
	if q := p.Quota; q != nil {
		b := d.Project().Quota
		for _, f := range []struct{ key, v, bound string }{{"cpu", q.CPU, b.CPU}, {"memory", q.Memory, b.Memory}, {"storage", q.Storage, b.Storage}} {
			if quantityValue(f.v) > quantityValue(f.bound) {
				looser("quota."+f.key, strconv.Quote(f.v), strconv.Quote(f.bound), "project_default.quota."+f.key)
			}
		}
	}
	if e := p.Envelope; e != nil {
		b := d.EnvelopeDefault
		number("envelope.monthly_eur", e.MonthlyEUR, b.MonthlyEUR, "envelope_default.monthly_eur")
		types("envelope.server_types", e.ServerTypes, b.ServerTypes, "envelope_default.server_types")
		number("envelope.max_count", float64(e.MaxCount), float64(b.MaxCount), "envelope_default.max_count")
		number("envelope.max_lease_hours", float64(e.MaxLeaseHours), float64(b.MaxLeaseHours), "envelope_default.max_lease_hours")
		number("envelope.volume_max_gb", float64(e.VolumeMaxGB), float64(b.VolumeMaxGB), "envelope_default.volume_max_gb")
	}
	if s := p.Standing; s != nil {
		b := d.Project().Standing
		number("standing.monthly_eur", s.MonthlyEUR, b.MonthlyEUR, "project_default.standing.monthly_eur")
		types("standing.server_types", s.ServerTypes, b.ServerTypes, "project_default.standing.server_types")
		number("standing.max_count", float64(s.MaxCount), float64(b.MaxCount), "project_default.standing.max_count")
	}
	if b := p.Budget; b != nil {
		number("budget.month_eur", b.MonthEUR, d.Budget.MonthEUR, "budget.month_eur")
		number("budget.task_max_eur", b.TaskMaxEUR, d.Budget.TaskMaxEUR, "budget.task_max_eur")
	}
	if p.Chat != nil {
		number("chat.monthly_eur", p.Chat.MonthlyEUR, d.ChatOf(ProjectConfig{}).MonthlyEUR, "chat.monthly_eur")
	}
	return errors.Join(errs...)
}

var quantityUnits = map[string]float64{"": 1, "m": 1e-3, "k": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
	"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}

// quantityValue is a Quantity's value in base units; 0 for none.
func quantityValue(q string) float64 {
	m := quantity.FindStringSubmatch(q)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseFloat(strings.TrimSuffix(q, m[2]), 64)
	return n * quantityUnits[m[2]]
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
