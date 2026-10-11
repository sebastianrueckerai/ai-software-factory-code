package factoryd

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func demoProject() Project {
	return Project{Name: "demo-app", Repository: "demo-app/demo-app", MergeRule: "auto", Objects: "factory", Agents: "coder", File: true,
		Namespaces: []string{"demo-staging", "demo-prod"},
		Environments: []Environment{
			{Namespace: "demo-staging", Path: "deploy/staging", Hostname: "demo-staging.aisoftwarefactory.world"},
			{Namespace: "demo-prod", Path: "deploy/prod", Hostname: "demo.aisoftwarefactory.world"}},
		Quota:   policy.Quota{CPU: "2", Memory: "4Gi", Storage: "10Gi"},
		Secrets: []string{"demo-write-token/token"},
		Process: "maintenance", Lanes: []policy.Lane{{Name: policy.LaneBacklog}, {Name: policy.LaneReady, Class: policy.ClassCoder, MovedBy: policy.MovedByWriters}}}
}

// demoFile is demoProject's projects/demo-app.yaml.
const demoFile = `repository: demo-app/demo-app
merge_rule: auto
objects: factory
agents: coder
process: maintenance
namespaces: [demo-staging, demo-prod]
environments:
  - {namespace: demo-staging, path: deploy/staging, hostname: demo-staging.aisoftwarefactory.world}
  - {namespace: demo-prod, path: deploy/prod, hostname: demo.aisoftwarefactory.world}
quota: {cpu: "2", memory: 4Gi, storage: 10Gi}
secrets: [demo-write-token/token]
envelope: {monthly_eur: 5, server_types: [cx23], max_count: 1, max_lease_hours: 2, volume_max_gb: 0}
egress: [api.example.com]
`

// codeFile is the factory's own code as a seed project agents work on.
const codeFile = `repository: sf/ai-software-factory-code
merge_rule: human
objects: seed
agents: coder
process: maintenance
namespaces: [factory]
tokens: {read: forgejo-ci-registrar, actions: forgejo-ci-registrar, merge: forgejo-merge}
`

// projectTree is testTree with the code and demo-app files beside infra's.
func projectTree(t *testing.T) policy.Tree {
	t.Helper()
	files := testFiles(testPolicyYAML)
	for name, body := range map[string]string{"ai-software-factory-code": codeFile, "demo-app": demoFile} {
		files["projects/"+name+".yaml"] = []byte(policy.Header("projects/"+name+".yaml") + "\n" + body)
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// A project is its file's settings over its row's tokens; a file without
// a row has no tokens yet, a row without a file is marked.
func TestProjectsOf(t *testing.T) {
	tree := projectTree(t)
	v := policy.Version{ID: 4, Tree: tree}
	rows := []Project{{Name: "demo-app", GitToken: "g", MergeToken: "m"}, {Name: "gone", PullToken: "p"},
		{Name: "ai-software-factory-code", MergeRule: "auto", Namespaces: []string{"stale"}}}
	got := projectsOf(rows, v)
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"ai-software-factory-code", "ai-software-factory-infra", "demo-app", "gone"}) {
		t.Fatalf("projects = %v", names)
	}
	demo := demoProject()
	demo.GitToken, demo.MergeToken = "g", "m"
	demo.Envelope = policy.Envelope{MonthlyEUR: 5, ServerTypes: []string{"cx23"}, MaxCount: 1, MaxLeaseHours: 2, Standing: policy.Standing{ServerTypes: []string{}}}
	demo.Egress = []string{"api.example.com"}
	if !reflect.DeepEqual(got[2], demo) {
		t.Fatalf("demo-app = %+v\nwant %+v", got[2], demo)
	}
	code := got[0]
	if code.MergeRule != "human" || !slices.Equal(code.Namespaces, []string{"factory"}) || code.Objects != "seed" || !code.File ||
		code.Tokens != (policy.SeedTokens{Read: "forgejo-ci-registrar", Actions: "forgejo-ci-registrar", Merge: "forgejo-merge"}) ||
		code.Quota != policy.DefaultProject.Quota || code.Envelope.MonthlyEUR != 10 {
		t.Fatalf("code = %+v", code)
	}
	if infra := got[1]; !infra.File || infra.Agents != "none" || infra.Tokens.Read != "forgejo-config" {
		t.Fatalf("infra = %+v", infra)
	}
	if gone := got[3]; gone.File || gone.Repository != "" || gone.PullToken != "p" || gone.Active() {
		t.Fatalf("a row without a file = %+v", gone)
	}
	if !code.Coder() || got[1].Coder() || !got[1].Active() || !got[2].Coder() {
		t.Fatal("Coder: the code and demo-app, Active: infra too")
	}
	unminted := got[2]
	unminted.GitToken = ""
	if unminted.Active() {
		t.Fatal("a factory project before register-project is active")
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
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE name NOT IN ('ai-software-factory-code', 'ai-software-factory-infra')`); err != nil {
		t.Fatal(err)
	}
	var cols []string
	rows, err := s.db.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'projects' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if want := []string{"created_at", "git_token", "hook_secret_hash", "host_key_hash", "merge_token", "name", "pull_token", "read_token_hash", "ready_at", "staging_hash", "updated_at"}; !slices.Equal(cols, want) {
		t.Fatalf("projects columns = %v, want %v", cols, want)
	}
	var envelopes bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass('envelopes') IS NOT NULL`).Scan(&envelopes); err != nil || envelopes {
		t.Fatalf("the envelopes table is still there: %v, %v", envelopes, err)
	}
	if _, _, err := s.RecordPolicy(ctx, "projects-from-files", projectTree(t)); err != nil {
		t.Fatal(err)
	}
	created, err := s.EnsureProjectRows(ctx, []string{"ai-software-factory-code", "ai-software-factory-infra", "demo-app"})
	if err != nil || !slices.Equal(created, []string{"demo-app"}) {
		t.Fatalf("rows created = %v, %v", created, err)
	}
	if created, err := s.EnsureProjectRows(ctx, []string{"demo-app"}); err != nil || len(created) != 0 {
		t.Fatalf("second EnsureProjectRows = %v, %v", created, err)
	}
	if err := s.SetProjectTokens(ctx, "demo-app", ProjectTokens{HookSecretHash: "k1", GitToken: "g1", MergeToken: "m1", HostKeyHash: "s1", StagingHash: "t1"}); err != nil {
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
	got, ok, err := s.Project(ctx, "demo-app")
	want := demoProject()
	want.Envelope = policy.Envelope{MonthlyEUR: 5, ServerTypes: []string{"cx23"}, MaxCount: 1, MaxLeaseHours: 2, Standing: policy.Standing{ServerTypes: []string{}}}
	want.Egress = []string{"api.example.com"}
	want.ReadTokenHash, want.HookSecretHash, want.PullToken, want.GitToken, want.MergeToken, want.HostKeyHash, want.StagingHash = "h1", "k1", "p1", "g1", "m1", "s1", "t1"
	want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("project = %+v, %v, %v\nwant %+v", got, ok, err, want)
	}
	if p, ok, err := s.ProjectByRepository(ctx, "sf/ai-software-factory-infra"); err != nil || !ok || p.Name != "ai-software-factory-infra" || p.Agents != "none" {
		t.Fatalf("by repository = %+v, %v, %v", p, ok, err)
	}
	if _, ok, err := s.ProjectByRepository(ctx, "someone/else"); err != nil || ok {
		t.Fatalf("an unknown repository = %v, %v", ok, err)
	}
	if err := s.SetProjectTokens(ctx, "nope", ProjectTokens{GitToken: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tokens of an unknown project = %v", err)
	}
}
