package factoryd

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// seededProject is demo-app naming a small template that uses every field
// of the data and one Actions expression, which passes through.
func seededProject() Project {
	p := demoProject()
	p.Template = "ts-go"
	p.TemplateFiles = map[string]string{
		".forgejo/workflows/ci.yml":         "env:\n  IMAGE: [[.Image]]\n  GIT_SSH_REPO: [[.Repository]]\n  TOKEN: ${{ secrets.X }}\n",
		"deploy/staging/kustomization.yaml": "namespace: [[index .Namespaces \"deploy/staging\"]]\n",
		"deploy/prod/kustomization.yaml":    "namespace: [[index .Namespaces \"deploy/prod\"]]\n",
		"AGENTS.md":                         "# [[.Name]]\n",
	}
	return p
}

// An empty repository whose project names a template gets the rendered
// files as one commit on main by factory-projects, every file created.
func TestSeedRendersTemplate(t *testing.T) {
	rig := newRegisterRig(t)
	rig.store.m["demo-app"] = seededProject()
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if len(rig.fj.seeds) != 1 {
		t.Fatalf("contents calls = %d", len(rig.fj.seeds))
	}
	c := rig.fj.seeds[0]
	want := map[string]string{
		".forgejo/workflows/ci.yml":         "env:\n  IMAGE: git.aisoftwarefactory.world/demo-app/demo-app\n  GIT_SSH_REPO: demo-app/demo-app\n  TOKEN: ${{ secrets.X }}\n",
		"deploy/staging/kustomization.yaml": "namespace: demo-staging\n",
		"deploy/prod/kustomization.yaml":    "namespace: demo-prod\n",
		"AGENTS.md":                         "# demo-app\n",
	}
	if !reflect.DeepEqual(c.files, want) {
		t.Errorf("seeded files = %q\nwant %q", c.files, want)
	}
	if c.branch != "main" || c.message != "Seed from template ts-go (registration)\n\nFactory-Role: registration\n" || c.by != "token projects-tok" {
		t.Errorf("seed commit on %q, %q, by %q", c.branch, c.message, c.by)
	}
	if ops := slices.Compact(slices.Sorted(maps.Values(c.ops))); !slices.Equal(ops, []string{"create"}) {
		t.Errorf("operations = %v", c.ops)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: seed: created 5eed from template ts-go\n") {
		t.Errorf("log lacks the seed line:\n%s", rig.logs)
	}
}

// A repository with main is never seeded, whatever its project names.
func TestSeedSkipsExistingMain(t *testing.T) {
	rig := newRegisterRig(t)
	rig.store.m["demo-app"] = seededProject()
	rig.fj.branches["main"] = "c0ffee"
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if len(rig.fj.seeds) != 0 {
		t.Fatalf("a repository with main was seeded: %+v", rig.fj.seeds)
	}
	if !strings.Contains(rig.logs.String(), "project demo-app: seed: no change\n") {
		t.Errorf("log lacks the seed line:\n%s", rig.logs)
	}
	rig = newRegisterRig(t)
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatal(err)
	}
	if len(rig.fj.seeds) != 0 || !strings.Contains(rig.logs.String(), "project demo-app: seed: waiting: no template; main's first push starts the project\n") {
		t.Errorf("an empty repository without a template: seeds %v\n%s", rig.fj.seeds, rig.logs)
	}
}

// A registered empty repository whose file gains a template after main's
// protection exists is not seeded, and the steps after the seed still run.
func TestSeedSkipsProtectedMain(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	rig.store.m["demo-app"] = seededProject()
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"]); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	if len(rig.fj.seeds) != 0 {
		t.Fatalf("seeded under main's protection: %+v", rig.fj.seeds)
	}
	for _, want := range []string{"seed: waiting: main's protection exists, which refuses the seed; push main's first commit", "protection main: no change", "labels: no change"} {
		if !strings.Contains(rig.logs.String(), "project demo-app: "+want+"\n") {
			t.Errorf("log lacks %q:\n%s", want, rig.logs)
		}
	}
}

// The seed commit lands before main's protection is made, and the same
// reconcile starts the deploy branch from it.
func TestSeedBeforeProtection(t *testing.T) {
	rig := newRegisterRig(t)
	rig.store.m["demo-app"] = seededProject()
	if _, err := rig.reg.Register(context.Background(), "demo-app", false); err != nil {
		t.Fatalf("%v\n%s", err, rig.logs)
	}
	seed := slices.Index(rig.fj.writes, "POST /api/v1/repos/demo-app/demo-app/contents by token projects-tok")
	protect := slices.Index(rig.fj.writes, "POST /api/v1/repos/demo-app/demo-app/branch_protections by token projects-tok")
	if seed < 0 || protect < 0 || seed > protect {
		t.Fatalf("seed at %d, first protection at %d: %v", seed, protect, rig.fj.writes)
	}
	if main, ok := rig.fj.protection["main"]; !ok || main["rule_name"] != "main" {
		t.Fatalf("main's protection = %v", main)
	}
	if rig.fj.branches["deploy"] != "5eed" {
		t.Fatalf("deploy = %q, want the seed commit", rig.fj.branches["deploy"])
	}
	for _, want := range []string{"branch deploy: created", "protection deploy: created"} {
		if !strings.Contains(rig.logs.String(), "project demo-app: "+want+"\n") {
			t.Errorf("log lacks %q:\n%s", want, rig.logs)
		}
	}
}

// ready_at is set by the first reconcile whose every line is created,
// updated, no change, ensured or ready, and never cleared.
func TestReadyAtAfterCleanReconcile(t *testing.T) {
	rig := newRegisterRig(t)
	ctx := context.Background()
	if _, err := rig.reg.Register(ctx, "demo-app", false); err != nil {
		t.Fatal(err)
	}
	if got := rig.store.m["demo-app"]; !got.ReadyAt.IsZero() {
		t.Fatalf("ready after a reconcile that waits for main:\n%s", rig.logs)
	}
	rig.fj.branches["main"] = "c0ffee"
	if err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"]); err != nil {
		t.Fatal(err)
	}
	ready := rig.store.m["demo-app"].ReadyAt
	if ready.IsZero() {
		t.Fatalf("not ready after a clean reconcile:\n%s", rig.logs)
	}
	delete(rig.fj.branches, "main")
	delete(rig.fj.branches, "deploy")
	delete(rig.fj.protection, "deploy")
	rig.logs.Reset()
	if err := rig.reg.Reconcile(ctx, rig.store.m["demo-app"]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rig.logs.String(), string(NoMain)) || !rig.store.m["demo-app"].ReadyAt.Equal(ready) {
		t.Fatalf("a later waiting reconcile changed ready_at %v -> %v:\n%s", ready, rig.store.m["demo-app"].ReadyAt, rig.logs)
	}

	for _, tc := range []struct {
		line  Change
		clean bool
	}{{Created, true}, {Updated, true}, {Unchanged, true}, {"ensured", true}, {"ready", true}, {"created bug, agent", true},
		{"created 5eed from template ts-go", true}, {NoMain, false}, {Waiting, false}, {"removed alice", false},
		{"missing: its protection refuses the create; the next roll starts it", false}, {"set by another reconcile", false}} {
		if got := cleanLine(tc.line); got != tc.clean {
			t.Errorf("cleanLine(%q) = %v", tc.line, got)
		}
	}
	rig = newRegisterRig(t)
	rig.fj.branches["main"] = "c0ffee"
	rig.kube.fail = "/namespaces/demo-prod/ciliumnetworkpolicies/"
	if _, err := rig.reg.Register(ctx, "demo-app", false); err == nil {
		t.Fatal("the failing apply passed")
	}
	if !rig.store.m["demo-app"].ReadyAt.IsZero() {
		t.Fatal("ready after a failed step")
	}
}

// The column is set once and read back with the project.
func TestPGProjectReady(t *testing.T) {
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
	if _, _, err := s.RecordPolicy(ctx, "project-ready", projectTree(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureProjectRows(ctx, []string{"demo-app"}); err != nil {
		t.Fatal(err)
	}
	if p, _, err := s.Project(ctx, "demo-app"); err != nil || !p.ReadyAt.IsZero() {
		t.Fatalf("a new row is ready: %v, %v", p.ReadyAt, err)
	}
	if err := s.SetProjectReady(ctx, "demo-app"); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.Project(ctx, "demo-app")
	if err != nil || first.ReadyAt.IsZero() {
		t.Fatalf("ready_at after SetProjectReady = %v, %v", first.ReadyAt, err)
	}
	if err := s.SetProjectReady(ctx, "demo-app"); err != nil {
		t.Fatal(err)
	}
	if second, _, _ := s.Project(ctx, "demo-app"); !second.ReadyAt.Equal(first.ReadyAt) {
		t.Fatalf("a second SetProjectReady moved ready_at %v -> %v", first.ReadyAt, second.ReadyAt)
	}
}
