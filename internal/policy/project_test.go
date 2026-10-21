package policy

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// A project's month cap may only tighten the policy's (fixture month_eur
// 50); 0 stops the project's spend; absent, the project has no cap of its
// own, nor has a repository no file names.
func TestProjectBudgetBounded(t *testing.T) {
	with := func(budget string) map[string][]byte {
		files := fixtureTree(t)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], budget...)
		return files
	}
	for name, tc := range map[string]struct{ budget, want string }{
		"negative":            {"budget: {month_eur: -1}\n", "project demo-app: budget.month_eur must not be negative (0 stops the project's spend)"},
		"poolWithoutRaiseMax": {"budget: {month_eur: 10, discretionary_eur: 5}\n", "project demo-app: budget.discretionary_eur 5 needs budget.raise_max_eur above 0: every raise would refuse"},
		"looser":              {"budget: {month_eur: 60}\n", "project demo-app: budget.month_eur 60 is looser than policy.yaml's budget.month_eur 50"},
		"empty":               {"budget: {}\n", "projects/demo-app.yaml: schema:"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTree(with(tc.budget)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseTree error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	for budget, want := range map[string]float64{"budget: {month_eur: 10}\n": 10, "budget: {month_eur: 0}\n": 0, "budget: {month_eur: 50}\n": 50, "": math.Inf(1)} {
		tree, err := ParseTree(with(budget))
		if err != nil {
			t.Fatalf("%q: %v", budget, err)
		}
		if name, eur := tree.ProjectCap("demo-app/demo-app"); name != "demo-app" || eur != want {
			t.Fatalf("%q: ProjectCap = (%q, %g), want (demo-app, %g)", budget, name, eur, want)
		}
	}
	tree, err := ParseTree(with("budget: {month_eur: 10}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if name, eur := tree.ProjectCap("nobody/nothing"); name != "" || !math.IsInf(eur, 1) {
		t.Fatalf("ProjectCap of a repository no file names = (%q, %g), want no cap", name, eur)
	}
	// a tree read back from Postgres is held to the same bound
	p := tree.Projects["demo-app"]
	p.Budget = &ProjectBudget{MonthEUR: 60}
	tree.Projects["demo-app"] = p
	if err := tree.Validate(); err == nil || !strings.Contains(err.Error(), "budget.month_eur 60 is looser") {
		t.Fatalf("Validate = %v, want the looser cap refused", err)
	}
}

// A project's card maximum may only tighten the policy's task_max_eur
// (here 15); the project schema lists it under budget.
func TestProjectCardMaximumBounded(t *testing.T) {
	with := func(budget string) map[string][]byte {
		files := fixtureTree(t)
		files["policy.yaml"] = []byte(strings.Replace(string(files["policy.yaml"]), "task_max_eur: 5", "task_max_eur: 15", 1))
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], budget...)
		return files
	}
	if _, err := ParseTree(with("budget: {month_eur: 50, task_max_eur: 20}\n")); err == nil ||
		!strings.Contains(err.Error(), "project demo-app: budget.task_max_eur 20 is looser than policy.yaml's budget.task_max_eur 15") {
		t.Fatalf("ParseTree error = %v, want the looser card maximum refused", err)
	}
	if _, err := ParseTree(with("budget: {month_eur: 50, task_max_eur: -1}\n")); err == nil || !strings.Contains(err.Error(), "projects/demo-app.yaml: schema:") {
		t.Fatalf("ParseTree error = %v, want the negative card maximum refused", err)
	}
	tree, err := ParseTree(with("budget: {month_eur: 50, task_max_eur: 15}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Projects["demo-app"].Budget; got == nil || got.TaskMaxEUR != 15 {
		t.Fatalf("budget = %+v, want task_max_eur 15", got)
	}
	budget := resolvedSchema(t, "project.schema.json").Schema().Properties["budget"]
	if p := budget.Properties["task_max_eur"]; p == nil || p.Description == "" || slices.Contains(budget.Required, "task_max_eur") {
		t.Fatalf("project schema budget = %+v, want an optional, described task_max_eur", budget)
	}
}

// A project's model key is "stronger" or absent; any other value refused.
func TestProjectModelStronger(t *testing.T) {
	with := func(model string) map[string][]byte {
		files := fixtureTree(t)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], model...)
		return files
	}
	tree, err := ParseTree(with("model: stronger\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Projects["demo-app"].Model; got != ModelStronger {
		t.Fatalf("Model = %q, want %q", got, ModelStronger)
	}
	tree, err = ParseTree(with(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Projects["demo-app"].Model; got != "" {
		t.Fatalf("Model = %q, want empty (a project with no model key starts on the ladder's first rung)", got)
	}
	want := `project demo-app: model "fastest": want one of [stronger]`
	if _, err := ParseTree(with("model: fastest\n")); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("ParseTree error = %v, want it to contain %q", err, want)
	}
}

func TestProjectDefaultAbsentTakesBuiltIns(t *testing.T) {
	d, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectDefault{Quota: Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
		Standing: Standing{ServerTypes: []string{}}}
	if got := d.Project(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Project() = %+v, want %+v", got, want)
	}
	env := d.DefaultEnvelope()
	if env.MonthlyEUR != 10 || !reflect.DeepEqual(env.Standing, want.Standing) {
		t.Fatalf("DefaultEnvelope() = %+v", env)
	}
}

const withProject = valid + `project_default:
  quota: {cpu: 1500m, memory: 3Gi, storage: 20Gi}
  standing: {monthly_eur: 12.5, server_types: [cx23], max_count: 1}
`

func TestProjectDefaultParsed(t *testing.T) {
	d, err := Parse([]byte(withProject))
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectDefault{Quota: Quota{CPU: "1500m", Memory: "3Gi", Storage: "20Gi"},
		Standing: Standing{MonthlyEUR: 12.5, ServerTypes: []string{"cx23"}, MaxCount: 1}}
	if got := d.Project(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Project() = %+v, want %+v", got, want)
	}
	if got := d.DefaultEnvelope(); !reflect.DeepEqual(got.Standing, want.Standing) || got.MaxCount != 2 {
		t.Fatalf("DefaultEnvelope() = %+v", got)
	}
}

func TestProjectDefaultRejects(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"unknown key":       {valid + "project_default: {quota: {cpu: '1', memory: 1Gi, storage: 1Gi}, standing: {monthly_eur: 0, server_types: [], max_count: 0}, colour: red}\n", "colour"},
		"merge rule":        {strings.Replace(withProject, "project_default:\n", "project_default:\n  merge_rule: auto\n", 1), "merge_rule"},
		"quota missing":     {strings.Replace(withProject, "memory: 3Gi, ", "", 1), "quota.memory"},
		"quota not a size":  {strings.Replace(withProject, "storage: 20Gi", "storage: lots", 1), "quota.storage"},
		"standing negative": {strings.Replace(withProject, "monthly_eur: 12.5", "monthly_eur: -1", 1), "standing.monthly_eur"},
		"standing count":    {strings.Replace(withProject, "max_count: 1}", "max_count: -1}", 1), "standing.max_count"},
		"standing type":     {strings.Replace(withProject, "server_types: [cx23]", "server_types: ['']", 1), "standing.server_types"},
		"standing in task":  {strings.Replace(valid, "volume_max_gb: 20", "volume_max_gb: 20\n  standing: {monthly_eur: 1}", 1), "standing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestQuantity(t *testing.T) {
	for q, ok := range map[string]bool{"2": true, "1500m": true, "0.5": true, "4Gi": true, "10G": true, "": false, "4 Gi": false, "-1": false, "1e3": false, "Gi": false} {
		if got := Quantity(q); got != ok {
			t.Errorf("Quantity(%q) = %v, want %v", q, got, ok)
		}
	}
}

// A project's envelope and quota are its file's blocks, each absent one
// the policy's default.
func TestEnvelopeAndQuotaOf(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	d := tree.Document
	demo := Envelope{MonthlyEUR: 5, ServerTypes: []string{"cx23"}, MaxCount: 1, MaxLeaseHours: 2,
		Standing: Standing{MonthlyEUR: 3, ServerTypes: []string{"cx33"}, MaxCount: 1}}
	if got := tree.EnvelopeOf("demo-app/demo-app"); !reflect.DeepEqual(got, demo) {
		t.Fatalf("demo-app's envelope = %+v, want %+v", got, demo)
	}
	def := Envelope{MonthlyEUR: 10, ServerTypes: []string{"cx23"}, MaxCount: 2, MaxLeaseHours: 4, VolumeMaxGB: 20,
		Standing: Standing{MonthlyEUR: 5, ServerTypes: []string{"cx23", "cx33"}, MaxCount: 2}}
	for _, repo := range []string{"sf/ai-software-factory-code", "nobody/else"} {
		if got := tree.EnvelopeOf(repo); !reflect.DeepEqual(got, def) {
			t.Fatalf("%s's envelope = %+v, want the default %+v", repo, got, def)
		}
	}
	only := tree.Projects["demo-app"]
	only.Envelope = nil
	if got := d.EnvelopeOf(only); got.MonthlyEUR != 10 || !reflect.DeepEqual(got.Standing, demo.Standing) {
		t.Fatalf("a standing block alone = %+v", got)
	}
	if got := d.QuotaOf(tree.Projects["demo-app"]); got != (Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"}) {
		t.Fatalf("demo-app's quota = %+v", got)
	}
	if got := d.QuotaOf(tree.Projects["ai-software-factory-code"]); got != (Quota{CPU: "4", Memory: "8Gi", Storage: "20Gi"}) {
		t.Fatalf("code's quota = %+v", got)
	}
	if name, p, ok := tree.ProjectOf("sf/ai-software-factory-infra"); !ok || name != "ai-software-factory-infra" || p.Agents != AgentsNone {
		t.Fatalf("ProjectOf(sf/ai-software-factory-infra) = %s, %+v, %v", name, p, ok)
	}
}

// Registration's rules on a factory project's file (review-task65's among
// them), and what a seed project may not say.
func TestProjectConfigValidate(t *testing.T) {
	demo := func() ProjectConfig {
		return ProjectConfig{Repository: "demo-app/demo-app", MergeRule: MergeAuto, Objects: ObjectsFactory, Agents: AgentsCoder,
			Namespaces: []string{"demo-staging", "demo-prod"},
			Environments: []Environment{{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.example.org"},
				{Namespace: "demo-prod", Path: "deploy/prod", Hostname: "demo.example.org"}},
			Quota: &Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"}, Secrets: []string{"demo-write-token/token"}}
	}
	if err := demo().Validate("demo-app"); err != nil {
		t.Fatalf("demo-app: %v", err)
	}
	withEgress := demo()
	withEgress.Egress = []string{"api.example.com", "hooks.example.org"}
	if err := withEgress.Validate("demo-app"); err != nil {
		t.Fatalf("demo-app with egress: %v", err)
	}
	for name, tc := range map[string]struct {
		file   string
		change func(*ProjectConfig)
		want   string
	}{
		"name upper":    {"Demo", func(p *ProjectConfig) {}, "the file name"},
		"name factory":  {"factory-x", func(p *ProjectConfig) { p.Repository = "factory-x/factory-x" }, "not factory-*"},
		"name flux":     {"flux-system", func(p *ProjectConfig) { p.Repository = "flux-system/flux-system" }, "not factory-*, flux-system, sf or upstream"},
		"name sf":       {"sf", func(p *ProjectConfig) { p.Repository = "sf/sf" }, "not factory-*, flux-system, sf or upstream"},
		"name upstream": {"upstream", func(p *ProjectConfig) { p.Repository = "upstream/upstream" }, "not factory-*, flux-system, sf or upstream"},
		"too long":      {strings.Repeat("a", 38), func(p *ProjectConfig) {}, "the file name"},
		"repository":    {"demo-app", func(p *ProjectConfig) { p.Repository = "demo-app" }, "owner/name in lower case"},
		"in sf":         {"demo-app", func(p *ProjectConfig) { p.Repository = "sf/demo-app" }, "its own organisation"},
		// review-task65 I2: Forgejo folds case, so this would name the infra repo
		"folded":            {"demo-app", func(p *ProjectConfig) { p.Repository = "sf/AI-Software-Factory-Infra" }, "owner/name in lower case"},
		"kube namespace":    {"demo-app", func(p *ProjectConfig) { p.Namespaces = []string{"kube-system"}; p.Environments = nil }, `namespace "kube-system"`},
		"factory namespace": {"demo-app", func(p *ProjectConfig) { p.Namespaces = []string{"factory"}; p.Environments = nil }, `namespace "factory"`},
		"task namespace":    {"demo-app", func(p *ProjectConfig) { p.Namespaces = []string{"factory-agents-x"}; p.Environments = nil }, `namespace "factory-agents-x"`},
		"twice":             {"demo-app", func(p *ProjectConfig) { p.Namespaces = []string{"a", "a"}; p.Environments = nil }, `namespace "a": a distinct DNS label`},
		"env path abs":      {"demo-app", func(p *ProjectConfig) { p.Environments[0].Path = "/deploy" }, "path"},
		"host twice":        {"demo-app", func(p *ProjectConfig) { p.Environments[1].Hostname = p.Environments[0].Hostname }, "a distinct DNS name"},
		"env twice":         {"demo-app", func(p *ProjectConfig) { p.Environments[1].Namespace = "demo-staging" }, `environment "demo-staging": given twice`},
		"quota":             {"demo-app", func(p *ProjectConfig) { p.Quota.CPU = "two" }, "quota.cpu"},
		"egress wildcard":   {"demo-app", func(p *ProjectConfig) { p.Egress = []string{"*"} }, `egress "*": a distinct DNS name`},
		"egress address":    {"demo-app", func(p *ProjectConfig) { p.Egress = []string{"10.0.0.1"} }, `egress "10.0.0.1": a distinct DNS name`},
		"egress twice":      {"demo-app", func(p *ProjectConfig) { p.Egress = []string{"api.example.com", "api.example.com"} }, `egress "api.example.com": a distinct DNS name`},
		"seed egress": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Egress: []string{"api.example.com"}, Tokens: &SeedTokens{Read: SecretConfig}}
		}, "objects seed: egress"},
		"seed quota": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Quota: &Quota{"1", "1Gi", "1Gi"}, Tokens: &SeedTokens{Read: SecretConfig}}
		}, "objects seed: environments, quota and secrets"},
		"seed secrets": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Secrets: []string{"a/b"}, Tokens: &SeedTokens{Read: SecretConfig}}
		}, "objects seed: environments, quota and secrets"},
		"approver no login": {"demo-app", func(p *ProjectConfig) { p.Approvers = []string{"no spaces here"} }, `approvers "no spaces here": distinct Forgejo user names`},
		"approver twice":    {"demo-app", func(p *ProjectConfig) { p.Approvers = []string{"bob", "bob"} }, `approvers "bob": distinct Forgejo user names`},
		"approver dots":     {"demo-app", func(p *ProjectConfig) { p.Approvers = []string{"x."} }, `approvers "x.": distinct Forgejo user names`},
		"seed unknown merge": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsCoder, Tokens: &SeedTokens{Read: SecretConfig, Actions: SecretCIRegistrar, Merge: "root"}}
		}, `tokens.merge "root"`},
		"seed unknown gate": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeAuto, Objects: ObjectsSeed, Agents: AgentsCoder, Tokens: &SeedTokens{Read: SecretConfig, Actions: SecretCIRegistrar, Merge: SecretMerge, Gate: "root"}}
		}, `tokens.gate "root": want forgejo-gate`},
		// each token key takes its own secrets: the gate's approves, the
		// merge token merges, and neither stands in for the other
		"seed gate names the merge token": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeAuto, Objects: ObjectsSeed, Agents: AgentsCoder, Tokens: &SeedTokens{Read: SecretConfig, Actions: SecretCIRegistrar, Merge: SecretMerge, Gate: SecretMerge}}
		}, `tokens.gate "forgejo-merge": want forgejo-gate`},
		"seed merge names the gate token": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeAuto, Objects: ObjectsSeed, Agents: AgentsCoder, Tokens: &SeedTokens{Read: SecretConfig, Actions: SecretCIRegistrar, Merge: SecretGate}}
		}, `tokens.merge "forgejo-gate": want one of [forgejo-ci-registrar forgejo-merge forgejo-config]`},
	} {
		t.Run(name, func(t *testing.T) {
			p := demo()
			tc.change(&p)
			if err := p.Validate(tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
	seed := ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Namespaces: []string{"factory"},
		Tokens: &SeedTokens{Read: SecretConfig}}
	if err := seed.Validate("x"); err != nil {
		t.Fatalf("a seed project in a factory namespace: %v", err)
	}
}

// A project file's approvers are the project's; a file without them, or
// a repository no file names, has the policy's.
func TestApproversOf(t *testing.T) {
	files := fixtureTree(t)
	files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], "approvers: [bob]\n"...)
	tree, err := ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	for repo, want := range map[string][]string{"demo-app/demo-app": {"bob"}, "linkshort/linkshort": {"sebastian"}, "nobody/else": {"sebastian"}} {
		if got := tree.ApproversOf(repo); !reflect.DeepEqual(got, want) {
			t.Errorf("ApproversOf(%s) = %v, want %v", repo, got, want)
		}
	}
}

// A project's chain block parses its mode, requires and stall; a file
// without one leaves all three at their zero values, and Resolved fills
// the defaults in, leaving what the file set as given.
func TestChainConfigDefaults(t *testing.T) {
	with := func(chain string) map[string][]byte {
		files := fixtureTree(t)
		files["projects/demo-app.yaml"] = append(files["projects/demo-app.yaml"], chain...)
		return files
	}
	tree, err := ParseTree(with("chain: {mode: run, requires: [factory/roll], stall: 45m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := ChainConfig{Mode: "run", Requires: []string{"factory/roll"}, Stall: 45 * time.Minute}
	if got := tree.Projects["demo-app"].Chain; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %+v, want %+v", got, want)
	}
	empty, err := ParseTree(with(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.Projects["demo-app"].Chain; got.Mode != "" || got.Requires != nil || got.Stall != 0 {
		t.Fatalf("a file without a chain block = %+v, want the zero values", got)
	}
	full := ChainConfig{Mode: "run", Requires: []string{"factory/roll"}, Stall: 45 * time.Minute}
	if got := full.Resolved(); !reflect.DeepEqual(got, full) {
		t.Fatalf("Resolved of a full chain = %+v, want it kept as %+v", got, full)
	}
	if got := (ChainConfig{}).Resolved(); !reflect.DeepEqual(got, ChainConfig{Mode: "run", Stall: 30 * time.Minute}) {
		t.Fatalf("Resolved of an empty chain = %+v, want mode run, no requires, stall 30m", got)
	}
}
