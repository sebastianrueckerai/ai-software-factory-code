package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestZZReproCommitDropsScratchReproTests(t *testing.T) {
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
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, "internal", "x"), 0o755)
	os.WriteFile(filepath.Join(seed, "internal", "x", "fix.go"), []byte("package x\n"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "init")
	sha := run("-C", seed, "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)

	env := func(branch string) workerEnv {
		return workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch, repoURL: remote, pushURL: fork}
	}

	t.Run("scratch out, fix in", func(t *testing.T) {
		e := env("factory/t1")
		dir := filepath.Join(t.TempDir(), "repo")
		if err := cloneBase(e, dir); err != nil {
			t.Fatal(err)
		}
		base, err := prepareRepo(dir, e)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "internal", "x", "fix.go"), []byte("package x\n\nfunc F() {}\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "internal", "x", "zz_repro_fix_test.go"), []byte("package x\n"), 0o644)

		commit, err := commitAndPush(dir, e, base, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if commit == "" {
			t.Fatal("nothing pushed, want the fix")
		}
		tree := run("-C", fork, "ls-tree", "-r", "--name-only", commit)
		if !strings.Contains(tree, "internal/x/fix.go") {
			t.Fatalf("pushed tree lost the fix:\n%s", tree)
		}
		if strings.Contains(tree, "zz_repro_fix_test.go") {
			t.Fatalf("pushed tree carries the scratch repro test:\n%s", tree)
		}
	})

	t.Run("scratch only pushes nothing", func(t *testing.T) {
		e := env("factory/t2")
		dir := filepath.Join(t.TempDir(), "repo")
		if err := cloneBase(e, dir); err != nil {
			t.Fatal(err)
		}
		base, err := prepareRepo(dir, e)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(dir, "internal", "y"), 0o755)
		os.WriteFile(filepath.Join(dir, "internal", "y", "zz_repro_only_test.go"), []byte("package y\n"), 0o644)

		commit, err := commitAndPush(dir, e, base, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if commit != "" {
			t.Fatalf("commit = %q, want nothing pushed for a scratch-only tree", commit)
		}
	})
}
