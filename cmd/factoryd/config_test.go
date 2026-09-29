package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// tree copies the policy package's fixture tree into a temporary
// directory, with the generated schemas under schema/.
func tree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files, err := policy.ReadDir("../../internal/policy/testdata/factory")
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
	if !strings.Contains(stdout.String(), "3 projects, 9 stages") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	for name, tc := range map[string]struct {
		change func(dir string)
		want   []string
	}{
		"invalid enum and unknown key": {func(dir string) {
			write(t, filepath.Join(dir, "projects/infra.yaml"), "# yaml-language-server: $schema=../schema/project.schema.json\nrepository: sf/infra\nmerge_rule: human\nobjects: flux\nagents: none\n")
			b, _ := os.ReadFile(filepath.Join(dir, "workflow.yaml"))
			write(t, filepath.Join(dir, "workflow.yaml"), strings.Replace(string(b), "enabled: true}", "enabled: true, retries: 2}", 1))
		}, []string{`projects/infra.yaml: project infra: objects "flux"`, "workflow.yaml: yaml: unmarshal errors:"}},
		"ladder model left out": {func(dir string) {
			b, _ := os.ReadFile(filepath.Join(dir, "policy.yaml"))
			write(t, filepath.Join(dir, "policy.yaml"), strings.Replace(string(b), "models: [coding-cheap, coding-frontier]", "models: [coding-cheap]", 1))
		}, []string{`policy.yaml: capability classes ["coder"] do not allow the ladder model "coding-frontier"`}},
		"schema drift": {func(dir string) {
			write(t, filepath.Join(dir, "schema/policy.schema.json"), "{}\n")
		}, []string{"schema/policy.schema.json differs from factoryd schema"}},
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
