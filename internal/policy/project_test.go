package policy

import (
	"reflect"
	"strings"
	"testing"
)

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
		Standing: Standing{ServerTypes: []string{}}}
	for _, repo := range []string{"sf/code", "nobody/else"} {
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
	if got := d.QuotaOf(tree.Projects["code"]); got != DefaultProject.Quota {
		t.Fatalf("code's quota = %+v", got)
	}
	if name, p, ok := tree.ProjectOf("sf/infra"); !ok || name != "infra" || p.Agents != AgentsNone {
		t.Fatalf("ProjectOf(sf/infra) = %s, %+v, %v", name, p, ok)
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
	for name, tc := range map[string]struct {
		file   string
		change func(*ProjectConfig)
		want   string
	}{
		"name upper":   {"Demo", func(p *ProjectConfig) {}, "the file name"},
		"name factory": {"factory-x", func(p *ProjectConfig) { p.Repository = "factory-x/factory-x" }, "not factory-*"},
		"name flux":    {"flux-system", func(p *ProjectConfig) { p.Repository = "flux-system/flux-system" }, "not factory-*, flux-system or sf"},
		"name sf":      {"sf", func(p *ProjectConfig) { p.Repository = "sf/sf" }, "not factory-*, flux-system or sf"},
		"too long":     {strings.Repeat("a", 38), func(p *ProjectConfig) {}, "the file name"},
		"repository":   {"demo-app", func(p *ProjectConfig) { p.Repository = "demo-app" }, "owner/name in lower case"},
		"in sf":        {"demo-app", func(p *ProjectConfig) { p.Repository = "sf/demo-app" }, "its own organisation"},
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
		"seed quota": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Quota: &Quota{"1", "1Gi", "1Gi"}, Tokens: &SeedTokens{Read: SecretConfig}}
		}, "objects seed: environments, quota and secrets"},
		"seed secrets": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsNone, Secrets: []string{"a/b"}, Tokens: &SeedTokens{Read: SecretConfig}}
		}, "objects seed: environments, quota and secrets"},
		"seed unknown merge": {"x", func(p *ProjectConfig) {
			*p = ProjectConfig{Repository: "sf/x", MergeRule: MergeHuman, Objects: ObjectsSeed, Agents: AgentsCoder, Tokens: &SeedTokens{Read: SecretConfig, Actions: SecretCIRegistrar, Merge: "root"}}
		}, `tokens.merge "root"`},
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
