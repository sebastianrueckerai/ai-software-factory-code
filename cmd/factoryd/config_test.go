package main

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy/policytest"
)

// tree copies the policy package's fixture tree into a temporary
// directory, with the generated schemas under schema/.
func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files, err := policy.ReadDir("../../internal/policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"schema", "--out", filepath.Join(dir, "schema")}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("schema exit %d", code)
	}
	for name, b := range files {
		write(t, filepath.Join(dir, name), string(b))
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaCommandWritesTheGeneratedSchemas(t *testing.T) {
	out := filepath.Join(t.TempDir(), "schema")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"schema", "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want, err := policy.Schemas()
	if err != nil {
		t.Fatal(err)
	}
	got, err := policy.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("wrote %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	if code := run([]string{"schema"}, &stdout, &stderr); code != 2 {
		t.Fatalf("schema without --out: exit %d, want 2", code)
	}
}

func TestConfigCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := tree(t)
	if code := run([]string{"config", "check", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("valid tree: exit %d: %s", code, stderr.String())
	}
	if want := dir + ": ok, 4 projects, 2 processes, 1 template\nopen cards per track: checked by the running factoryd only\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}

	for name, tc := range map[string]struct {
		change func(dir string)
		want   []string
	}{
		"invalid enum and unknown key": {func(dir string) {
			write(t, filepath.Join(dir, "projects/ai-software-factory-infra.yaml"), "# yaml-language-server: $schema=../schema/project.schema.json\nrepository: sf/ai-software-factory-infra\nmerge_rule: human\nobjects: flux\nagents: none\n")
			b, _ := os.ReadFile(filepath.Join(dir, "processes/spec.yaml"))
			write(t, filepath.Join(dir, "processes/spec.yaml"), strings.Replace(string(b), "enabled: true}", "enabled: true, retries: 2}", 1))
		}, []string{`projects/ai-software-factory-infra.yaml: project ai-software-factory-infra: objects "flux"`, "processes/spec.yaml: yaml: unmarshal errors:"}},
		"ladder model left out": {func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "policy.yaml"))
			write(t, filepath.Join(dir, "policy.yaml"), strings.Replace(string(b), "models: [small, big]", "models: [small]", 1))
		}, []string{`policy.yaml: capability class "coder" does not allow the ladder model "big" (track direct of processes/maintenance.yaml starts it)`}},
		"a looser project file": {func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "projects/demo-app.yaml"))
			write(t, filepath.Join(dir, "projects/demo-app.yaml"), strings.Replace(string(b), "max_lease_hours: 2", "max_lease_hours: 48", 1))
		}, []string{"projects/demo-app.yaml: project demo-app: envelope.max_lease_hours 48 is looser than policy.yaml's envelope_default.max_lease_hours 4"}},
		"the old workflow file": {func(dir string) {
			write(t, filepath.Join(dir, "workflow.yaml"), "# yaml-language-server: $schema=schema/workflow.schema.json\nstages: []\n")
			write(t, filepath.Join(dir, "schema/workflow.schema.json"), "{}\n")
		}, []string{`workflow.yaml: unknown file "workflow.yaml": the tree holds policy.yaml, commands.yaml, processes/<name>.yaml, projects/<name>.yaml, templates/<name>/, routines/<name>.md and schema/`,
			"schema/workflow.schema.json is not a schema factoryd writes"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := tree(t)
			tc.change(dir)
			var stdout, stderr bytes.Buffer
			if code := run([]string{"config", "check", dir}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1; stderr %s", code, stderr.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
				}
			}
		})
	}
	for _, args := range [][]string{{"config", "check"}, {"config"}, {"config", "lint", dir}} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := run([]string{"config", "check", filepath.Join(dir, "absent")}, &stdout, &stderr); code != 1 {
		t.Errorf("absent directory: exit %d, want 1", code)
	}
}

// A stale or missing schema copy warns and exits 0: factoryd opens the
// pull request for it.
func TestConfigCheckWarnsOnStaleSchemas(t *testing.T) {
	root := t.TempDir()
	if err := os.Rename(tree(t), filepath.Join(root, "factory")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	write(t, "factory/schema/policy.schema.json", "{}\n")
	if err := os.Remove("factory/schema/process.schema.json"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "check", "factory"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, want 0; stderr %s", code, stderr.String())
	}
	want := "warning: factory/schema/policy.schema.json differs from this factoryd's schema (factoryd opens a pull request for it)\n" +
		"warning: factory/schema/process.schema.json differs from this factoryd's schema (factoryd opens a pull request for it)\n" +
		"factory: ok, 4 projects, 2 processes, 1 template\nopen cards per track: checked by the running factoryd only\n"
	if stdout.String() != want || stderr.String() != "" {
		t.Fatalf("stdout = %q, stderr = %q, want stdout %q", stdout.String(), stderr.String(), want)
	}
}

// runExplain runs config explain on the fixture tree.
func runExplain(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"config", "explain", "--tree", tree(t)}, args...), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestExplainSpecPlan(t *testing.T) {
	const want = `process spec (processes/spec.yaml), card kind plan
 1. card created on track plan: an issue labelled plan by a writer
 2. track plan: starts a task of class planner (session plan) once a work slot is free
 3. planner session on the stronger rung: answers a plan of 1 to 12 cards a round, one foundation first, one integration last; a bigger plan files its next round once every card of a round is done
      plan invalid -> back to the planner (step 3); an open question -> human card: the last answer, or a writer's comment -> back to the planner (step 3), the issue closed -> end: closed
 4. a plan with no open question: factoryd files one card per planned card on track direct, with their dependencies (card kind child)
      a cycle or any finding -> back to the planner (step 3)
 5. end: done
 caps: every planner session counts against limits.sessions (3); at the cap -> human card
`
	if got, stderr, code := runExplain(t, "spec", "plan"); code != 0 || got != want {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, got, want)
	}
}

func TestExplainSpecChild(t *testing.T) {
	const want = `process spec (processes/spec.yaml), card kind child
 1. card created: by factoryd from a plan with no open question, on track direct, with the plan's dependencies
 2. track direct: waits until every card it depends on is done ("waiting on #n")
      a dependency ends failed or closed -> human card: an answer -> step 3, a close -> end: closed
 3. track direct: starts a task of class coder (session code)
 4. stage screening: an outside account's card only; findings -> human card
 5. coder session (at most 3 per task, limits.sessions)
 6. stage policy: findings -> end: failed
 7. stage migration: findings -> human card
 8. stage reproduction: findings -> back to the coder (step 5)
 9. stage checks: findings -> back to the coder (step 5)
10. stage review: findings -> back to the coder (step 5); no verdict -> human card
11. stage audit: projects with audit: true only; findings -> human card
12. merge queue (project): one pull request at a time; main merged in and the checks run again when main moved
      a conflict or red checks -> back to the coder (step 5)
13. stage approval: human card, or merge rule auto without one
      request changes -> back to the coder (step 5); reject -> end: failed
14. stage merge: a refusal -> human card; a conflict -> back to the coder (step 5)
15. merged: waits until the chain runs the change live (done names the run that covers it)
16. end: done; the cards that depend on this one may start
 caps: every return to the coder counts against limits.sessions (3); at the cap -> human card
`
	if got, stderr, code := runExplain(t, "spec", "child"); code != 0 || got != want {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, got, want)
	}
}

func TestExplainMaintenanceDirect(t *testing.T) {
	const want = `process maintenance (processes/maintenance.yaml), card kind direct
 1. card created on track direct: an issue with no track label, or labelled bug or agent, from anyone, or any card factoryd files
 2. track direct: starts a task of class coder (session code) once a work slot is free
 3. stage screening: an outside account's card only; findings -> human card
 4. coder session (at most 3 per task, limits.sessions)
 5. stage checks: findings -> back to the coder (step 4)
 6. stage policy: findings -> end: failed
 7. stage migration: findings -> human card
 8. stage reproduction: findings -> back to the coder (step 4)
 9. stage review: findings -> back to the coder (step 4); no verdict -> human card
10. stage audit: projects with audit: true only; findings -> human card
11. stage approval: human card, or merge rule auto without one
      request changes -> back to the coder (step 4); reject -> end: failed
12. stage merge: a refusal -> human card; a conflict -> back to the coder (step 4)
13. merged: waits until the chain runs the change live (done names the run that covers it)
14. end: done
 caps: every return to the coder counts against limits.sessions (3); at the cap -> human card
`
	if got, stderr, code := runExplain(t, "maintenance", "direct"); code != 0 || got != want {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, stderr, got, want)
	}
}

// A switched-off stage and a stage's own return cap read in its row.
func TestExplainSwitchedOffAndMaxReturns(t *testing.T) {
	dir := tree(t)
	b, _ := os.ReadFile(filepath.Join(dir, "processes/maintenance.yaml"))
	s := strings.Replace(string(b), "{name: reproduction, on_findings: agent, enabled: true}", "{name: reproduction, on_findings: agent, enabled: false}", 1)
	s = strings.Replace(s, "{name: checks, on_findings: agent, enabled: true}", "{name: checks, on_findings: agent, enabled: true, max_returns: 2}", 1)
	write(t, filepath.Join(dir, "processes/maintenance.yaml"), s)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "explain", "--tree", dir, "maintenance", "direct"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{" 5. stage checks: findings -> back to the coder (step 4), at most 2 returns, then human card\n", " 8. stage reproduction: switched off\n"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestExplainRefusesKind(t *testing.T) {
	got, stderr, code := runExplain(t, "maintenance", "plan")
	if code != 1 || got != "" || stderr != "process maintenance has no plan track: a plan card cannot enter it\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, got, stderr)
	}
	for _, args := range [][]string{{"maintenance", "epic"}, {"maintenance", "bug"}, {"nightly", "direct"}} {
		if _, stderr, code := runExplain(t, args...); code != 1 || stderr == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	var stdout, errOut bytes.Buffer
	for _, args := range [][]string{{"config", "explain"}, {"config", "explain", "spec"}, {"config", "explain", "--tree"}} {
		if code := run(args, &stdout, &errOut); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// treeSource is a ConfigSource at one commit.
type treeSource map[string][]byte

func (s treeSource) Head(context.Context) (string, error) { return "c1", nil }
func (s treeSource) Files(context.Context, string) (map[string][]byte, error) {
	return maps.Clone(s), nil
}

// emptyStore is a PolicyStore that holds no version yet.
type emptyStore struct{ recorded []string }

func (s *emptyStore) RecordPolicy(_ context.Context, commit string, tree policy.Tree) (policy.Version, bool, error) {
	s.recorded = append(s.recorded, commit)
	return policy.Version{ID: 1, Commit: commit, Tree: tree}, true, nil
}
func (s *emptyStore) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	return policy.Version{}, false, nil
}
func (s *emptyStore) PolicyVersion(context.Context, int64) (policy.Version, bool, error) {
	return policy.Version{}, false, nil
}
func (s *emptyStore) Costs(context.Context, time.Time, time.Time) (policy.Costs, error) {
	return policy.Costs{}, nil
}
func (s *emptyStore) SweepHolds(context.Context) (int, error) { return 0, nil }
func (s *emptyStore) OpenCardsByTrack(context.Context) ([]factoryd.OpenCard, error) {
	return nil, nil
}

// config check and the loader judge one way (stage11-design.md section 3):
// every refusal fixture of TestProcessRefusals, and the valid tree, give
// the same verdict and the same lines through both.
func TestConfigCheckAndLoaderAgree(t *testing.T) {
	fixture, err := policy.ReadDir("../../internal/policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string][]byte){"valid": func(map[string][]byte) {}}
	for name, r := range policytest.Refusals {
		cases[name] = r.Change
	}
	for name, r := range policytest.TemplateRefusals {
		cases[name] = r.Change
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			files := maps.Clone(fixture)
			change(files)
			dir := t.TempDir()
			if code := run([]string{"schema", "--out", filepath.Join(dir, "schema")}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
				t.Fatalf("schema exit %d", code)
			}
			for f, b := range files {
				write(t, filepath.Join(dir, f), string(b))
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"config", "check", dir}, &stdout, &stderr)

			store := &emptyStore{}
			loader := &factoryd.PolicyLoader{Source: treeSource(files), Store: store, LadderModels: ladder().Aliases()}
			loadErr := loader.Load(context.Background())

			if name == "valid" {
				if code != 0 || loadErr != nil || !slices.Equal(store.recorded, []string{"c1"}) {
					t.Fatalf("check exit %d (%s), load %v, recorded %v: want both to accept", code, stderr.String(), loadErr, store.recorded)
				}
				return
			}
			if code != 1 || loadErr == nil || len(store.recorded) != 0 {
				t.Fatalf("check exit %d, load %v, recorded %v: want both to refuse", code, loadErr, store.recorded)
			}
			if got, want := stderr.String(), dir+": rejected:\n"+loadErr.Error()+"\n"; got != want {
				t.Fatalf("config check says\n%s\nthe loader\n%s", got, want)
			}
		})
	}
}

// config check never consults a database (D1c): a head that drops the
// plan track, which the running factoryd refuses while cards sit on it,
// passes, and the check says who judges the open cards.
func TestConfigCheckSkipsOpenCards(t *testing.T) {
	dir := tree(t)
	spec := filepath.Join(dir, "processes", "spec.yaml")
	b, err := os.ReadFile(spec) // #nosec G304 -- the test's own temporary tree
	if err != nil {
		t.Fatal(err)
	}
	noPlan := strings.Replace(string(b), "  plan:\n    class: planner\n    plan: {max_cards: 12, min_cards: 1, foundation: true, integration: true}\n", "", 1)
	noPlan = strings.Replace(noPlan, "dependencies: {wait_until: done, on_failed: human}\n", "", 1)
	if strings.Contains(noPlan, "  plan:") || strings.Contains(noPlan, "dependencies:") {
		t.Fatalf("spec.yaml still holds the plan track:\n%s", noPlan)
	}
	write(t, spec, noPlan)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "check", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if want := dir + ": ok, 4 projects, 2 processes, 1 template\nopen cards per track: checked by the running factoryd only\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// config check refuses a process file that still carries a lanes block,
// by name: the schema removed it, so it is no key the decoder ignores.
func TestConfigCheckRefusesALanesBlock(t *testing.T) {
	dir := tree(t)
	spec := filepath.Join(dir, "processes", "spec.yaml")
	b, err := os.ReadFile(spec) // #nosec G304 -- the test's own temporary tree
	if err != nil {
		t.Fatal(err)
	}
	write(t, spec, strings.Replace(string(b), "tracks:\n", "lanes:\n  - {name: backlog}\n  - {name: ready, class: coder, moved_by: writers}\ntracks:\n", 1))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"config", "check", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1; stdout %q", code, stdout.String())
	}
	want := dir + ": rejected:\nprocesses/spec.yaml: lanes: removed: whoever files a card sets its track, plan or direct, and the classes and the plan bounds sit under tracks\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}
