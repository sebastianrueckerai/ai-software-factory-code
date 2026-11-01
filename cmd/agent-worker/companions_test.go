//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// With COMPANIONS set the clone container also clones each companion,
// every file of it, into ws/companions/<name>; a companion that cannot be
// cloned leaves the task repository's clone intact and records why, for
// the brief.
func TestCloneModeClonesTheCompanions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	task, infra := filepath.Join(root, "task"), filepath.Join(root, "infra")
	for _, repo := range []string{task, infra} {
		run("init", "-q", "-b", "main", repo)
	}
	run("-C", task, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	files := map[string]string{"factory/policy.yaml": "companions: []", "main.tf": "tf", "shot.png": "png"}
	for name, body := range files {
		path := filepath.Join(infra, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("-C", infra, "add", ".")
	run("-C", infra, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "init")
	taskBare, infraBare := filepath.Join(root, "task.git"), filepath.Join(root, "infra.git")
	run("clone", "-q", "--bare", task, taskBare)
	run("clone", "-q", "--bare", infra, infraBare)
	head := run("-C", task, "rev-parse", "HEAD")

	for k, v := range map[string]string{"MODE": "clone", "REPO_URL": "file://" + taskBare, "PUSH_URL": "", "REPO_SHA": head,
		"BRANCH": "factory/t1", "REPO_TOKEN": "r3ad-tok"} {
		t.Setenv(k, v)
	}

	t.Run("cloned", func(t *testing.T) {
		t.Setenv("COMPANIONS", "file://"+infraBare)
		ws := t.TempDir()
		if code := runClone(nolog, ws); code != 0 {
			t.Fatalf("clone exit %d", code)
		}
		if err := fetched(ws); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			got, err := os.ReadFile(filepath.Join(ws, "companions", "infra", name))
			if err != nil || string(got) != body {
				t.Fatalf("companions/infra/%s = %q, %v, want %q: every file of a companion, no sparse filter", name, got, err, body)
			}
		}
		if lines := companionsState(ws, []string{"/workspace/companions/infra"}); len(lines) != 1 || lines[0] != "infra at /workspace/companions/infra" {
			t.Fatalf("companionsState = %v, want the companion's place", lines)
		}
	})

	t.Run("failed", func(t *testing.T) {
		t.Setenv("COMPANIONS", "file://"+filepath.Join(root, "missing.git"))
		ws := t.TempDir()
		if code := runClone(nolog, ws); code != 0 {
			t.Fatalf("clone exit %d, want 0", code)
		}
		if err := fetched(ws); err != nil {
			t.Fatalf("the task repository's fetch failed: %v", err)
		}
		lines := companionsState(ws, []string{"/workspace/companions/missing"})
		if len(lines) != 1 || !strings.Contains(lines[0], "could not be read") {
			t.Fatalf("companionsState = %v, want the clone's failure", lines)
		}
		if strings.Contains(lines[0], "r3ad-tok") {
			t.Fatalf("companionsState shows the token: %v", lines)
		}
	})

	t.Run("none", func(t *testing.T) {
		ws := t.TempDir()
		if code := runClone(nolog, ws); code != 0 {
			t.Fatalf("clone exit %d", code)
		}
		if lines := companionsState(ws, nil); lines != nil {
			t.Fatalf("companionsState = %v, want nothing for a project without companions", lines)
		}
	})
}
