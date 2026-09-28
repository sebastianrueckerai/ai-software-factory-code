package factoryd

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func demoProject() Project {
	return Project{Name: "demo-app", Repository: "demo-app/demo-app", MergeRule: "auto", Managed: true,
		Namespaces: []string{"demo-staging", "demo-prod"},
		Environments: []Environment{
			{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"},
			{Namespace: "demo-prod", Path: "deploy/prod", Hostname: "demo.aisoftwarefactory.world"}},
		Quota:   policy.Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
		Secrets: []string{"demo-write-token/token"}}
}

func TestProjectValidate(t *testing.T) {
	if err := demoProject().Validate(); err != nil {
		t.Fatalf("demo project: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(*Project)
		want   string
	}{
		"name upper":   {func(p *Project) { p.Name = "Demo" }, "name"},
		"name factory": {func(p *Project) { p.Name = "factory-x" }, "name"},
		"name flux":    {func(p *Project) { p.Name = "flux-system" }, "name"},
		"repository":   {func(p *Project) { p.Repository = "demo-app" }, "repository"},
		"in sf":        {func(p *Project) { p.Repository = "sf/demo-app" }, "repository"},
		// review-task65 I2: Forgejo folds case, so this would name the infra repo
		"folded":            {func(p *Project) { p.Repository = "sf/AI-Software-Factory-Infra" }, "repository"},
		"other org":         {func(p *Project) { p.Repository = "Demo-App/demo-app" }, "repository"},
		"name sf":           {func(p *Project) { p.Name, p.Repository = "sf", "sf/sf" }, "name"},
		"name too long":     {func(p *Project) { p.Name = strings.Repeat("a", 38); p.Repository = p.Name + "/" + p.Name }, "name"},
		"merge rule":        {func(p *Project) { p.MergeRule = "maybe" }, "merge rule"},
		"notify":            {func(p *Project) { p.Notify = []string{"a b"} }, "notify"},
		"kube namespace":    {func(p *Project) { p.Namespaces = []string{"kube-system"} }, "namespace"},
		"factory namespace": {func(p *Project) { p.Namespaces = []string{"factory"} }, "namespace"},
		"task namespace":    {func(p *Project) { p.Namespaces = []string{"factory-agents-x"} }, "namespace"},
		"flux namespace":    {func(p *Project) { p.Namespaces = []string{"flux-system"} }, "namespace"},
		"twice":             {func(p *Project) { p.Namespaces = []string{"a", "a"} }, "namespace"},
		"env outside":       {func(p *Project) { p.Environments[0].Namespace = "other" }, "not one of the project's namespaces"},
		"env path up":       {func(p *Project) { p.Environments[0].Path = "../infra" }, "path"},
		"env path abs":      {func(p *Project) { p.Environments[0].Path = "/deploy" }, "path"},
		"host":              {func(p *Project) { p.Environments[0].Hostname = "demo_x.example" }, "hostname"},
		"host twice":        {func(p *Project) { p.Environments[1].Hostname = p.Environments[0].Hostname }, "hostname"},
		"quota":             {func(p *Project) { p.Quota.CPU = "two" }, "quota.cpu"},
		"secret":            {func(p *Project) { p.Secrets = []string{"no-key"} }, "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			p := demoProject()
			p.Environments = append([]Environment(nil), p.Environments...)
			tc.change(&p)
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestParseEnvironments(t *testing.T) {
	envs, err := ParseEnvironments([]string{"demo-staging=deploy/staging/", "demo-prod=deploy/prod"},
		[]string{"demo-staging=demo-staging.aisoftwarefactory.world", "demo-prod=demo.aisoftwarefactory.world"})
	if err != nil || !reflect.DeepEqual(envs, demoProject().Environments) {
		t.Fatalf("ParseEnvironments = %+v, %v", envs, err)
	}
	for _, tc := range [][2][]string{
		{{"demo-staging"}, nil},
		{{"a=deploy/a"}, {"b=b.example.com"}},
		{{"a=deploy/a"}, {"a=x.example.com", "a=y.example.com"}},
	} {
		if _, err := ParseEnvironments(tc[0], tc[1]); err == nil {
			t.Errorf("ParseEnvironments(%v, %v) accepted", tc[0], tc[1])
		}
	}
}

func TestReadTokenHash(t *testing.T) {
	token, hash := NewReadToken()
	if len(token) != 64 || hash == token || len(hash) != 64 {
		t.Fatalf("token %d chars, hash %q", len(token), hash)
	}
	// sha256("abc"), hand-checked against sha256sum
	if got := HashToken("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("HashToken(abc) = %s", got)
	}
	p := Project{ReadTokenHash: hash}
	if !p.ReadTokenMatches(token) || p.ReadTokenMatches(token+"x") || p.ReadTokenMatches("") || p.ReadTokenMatches(hash) {
		t.Fatal("ReadTokenMatches accepts the wrong token or refuses the right one")
	}
	if (Project{}).ReadTokenMatches("") {
		t.Fatal("an unset hash matches the empty token")
	}
}

func TestResourceQuotaHard(t *testing.T) {
	want := map[string]any{"requests.cpu": "1500m", "limits.cpu": "1500m", "requests.memory": "3Gi", "limits.memory": "3Gi",
		"requests.storage": "20Gi"}
	if got := ResourceQuotaHard(policy.Quota{CPU: "1500m", Memory: "3Gi", Storage: "20Gi"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ResourceQuotaHard = %v", got)
	}
}

func TestListenerYAML(t *testing.T) {
	got := ListenerYAML(demoProject())
	for _, want := range []string{
		"    - name: https-demo-staging\n      protocol: HTTPS\n      port: 443\n      hostname: demo-staging.aisoftwarefactory.world\n",
		"            name: demo-staging-aisoftwarefactory-world-tls\n",
		"            matchLabels: {kubernetes.io/metadata.name: demo-staging}\n",
		"    - name: https-demo\n", "      hostname: demo.aisoftwarefactory.world\n",
		"            matchLabels: {kubernetes.io/metadata.name: demo-prod}\n",
		"# add to the git-redirect route's hostnames: demo-staging.aisoftwarefactory.world, demo.aisoftwarefactory.world\n",
		"  name: demo-aisoftwarefactory-world\n  namespace: forgejo\nspec:\n  secretName: demo-aisoftwarefactory-world-tls\n  dnsNames: [demo.aisoftwarefactory.world]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("listener YAML lacks %q:\n%s", want, got)
		}
	}
	if ListenerYAML(Project{Environments: []Environment{{Namespace: "a", Path: "deploy"}}}) != "" {
		t.Fatal("no hostnames, yet YAML")
	}
}

func TestPGProjects(t *testing.T) {
	dsn := os.Getenv("FACTORYD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FACTORYD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.ExecContext(ctx, `DELETE FROM projects WHERE managed`)
	ps, err := s.Projects(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("seeded projects = %+v, %v", ps, err)
	}
	code := ps[0]
	if code.Name != "ai-software-factory-code" || code.Repository != "sf/ai-software-factory-code" || code.MergeRule != "human" ||
		code.Managed || !reflect.DeepEqual(code.Namespaces, []string{"factory"}) || len(code.Notify) != 0 || len(code.Environments) != 0 {
		t.Fatalf("code row = %+v", code)
	}
	if infra := ps[1]; infra.Name != "ai-software-factory-infra" || infra.Managed || len(infra.Namespaces) != 0 {
		t.Fatalf("infra row = %+v", infra)
	}
	demo := demoProject()
	if err := s.CreateProject(ctx, demo); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, demo); !errors.Is(err, ErrProjectExists) {
		t.Fatalf("second create = %v", err)
	}
	// review-task65 m6: a namespace is one project's
	other := Project{Name: "other", Repository: "other/other", MergeRule: "human", Managed: true, Namespaces: []string{"other-ns", "demo-prod"}}
	if err := s.CreateProject(ctx, other); !errors.Is(err, ErrProjectExists) {
		t.Fatalf("another project's namespace = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects (name, repository) VALUES ('upper', 'Upper/upper')`); err == nil {
		t.Fatal("the table admits a repository in capitals")
	}
	if err := s.SetProjectTokens(ctx, "demo-app", ProjectTokens{HookSecretHash: "k1", GitToken: "g1", MergeToken: "m1", HostKeyHash: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectTokens(ctx, "demo-app", ProjectTokens{PullToken: "p1"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimReadToken(ctx, "demo-app", "h1"); !ok || err != nil {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if ok, err := s.ClaimReadToken(ctx, "demo-app", "h2"); ok || err != nil {
		t.Fatalf("second claim = %v, %v; want refused", ok, err)
	}
	if err := s.ForgetReadToken(ctx, "demo-app", "h2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectSettings(ctx, "demo-app", "human", []string{"alice"}, policy.Quota{CPU: "1", Memory: "1Gi", Storage: "1Gi"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Project(ctx, "demo-app")
	want := demo
	want.MergeRule, want.Notify, want.Quota = "human", []string{"alice"}, policy.Quota{CPU: "1", Memory: "1Gi", Storage: "1Gi"}
	want.ReadTokenHash, want.HookSecretHash, want.PullToken, want.GitToken, want.MergeToken, want.HostKeyHash = "h1", "k1", "p1", "g1", "m1", "s1"
	want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("project = %+v, %v, %v\nwant %+v", got, ok, err, want)
	}
	if err := s.SetProjectSettings(ctx, "nope", "human", nil, policy.Quota{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("settings of an unknown project = %v", err)
	}
}
