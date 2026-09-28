package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// reproRemote is a bare repository whose refs/pull/9/head holds a commit
// with a script; it returns the remote and that commit.
func reproRemote(t *testing.T) (string, string) {
	t.Helper()
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	run("init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "check.sh"), []byte("echo checking\necho oops >&2\nexit 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("-C", seed, "add", "check.sh")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "head")
	run("clone", "-q", "--bare", seed, remote)
	head := run("-C", seed, "rev-parse", "HEAD")
	run("-C", remote, "update-ref", "refs/pull/9/head", head)
	return remote, head
}

func TestReproduceRunsTheCommandAtTheHead(t *testing.T) {
	remote, head := reproRemote(t)
	e := reproEnv{repoURL: remote, repoToken: "tok", ref: "refs/pull/9/head", sha: head, command: "sh check.sh", timeout: time.Minute}
	got := reproduce(e, filepath.Join(t.TempDir(), "repo"))
	if got != (harness.Repro{Exit: 3, Output: "checking\noops\n"}) {
		t.Fatalf("repro = %+v", got)
	}
	e.command = "true"
	if got := reproduce(e, filepath.Join(t.TempDir(), "repo")); got != (harness.Repro{Exit: 0}) {
		t.Fatalf("repro = %+v", got)
	}
}

// A head that is not the gated commit is not run.
func TestReproduceRefusesAMovedHead(t *testing.T) {
	remote, _ := reproRemote(t)
	e := reproEnv{repoURL: remote, repoToken: "tok", ref: "refs/pull/9/head", sha: "0000", command: "touch ran", timeout: time.Minute}
	dir := filepath.Join(t.TempDir(), "repo")
	got := reproduce(e, dir)
	if got.Exit != -1 || !strings.Contains(got.Output, "the pull request moved") {
		t.Fatalf("repro = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Fatal("the command ran on a moved head")
	}
}

// A command past its time is killed with its process group.
func TestReproduceTimesOut(t *testing.T) {
	remote, head := reproRemote(t)
	e := reproEnv{repoURL: remote, repoToken: "tok", ref: "refs/pull/9/head", sha: head, command: "echo start; sleep 30 & sleep 30", timeout: 300 * time.Millisecond}
	began := time.Now()
	got := reproduce(e, filepath.Join(t.TempDir(), "repo"))
	if got.Exit != -1 || !strings.Contains(got.Output, "start\n") || !strings.Contains(got.Output, "timed out after 300ms") {
		t.Fatalf("repro = %+v", got)
	}
	if time.Since(began) > 10*time.Second {
		t.Fatalf("took %s", time.Since(began))
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{max: 4}
	b.Write([]byte("abc"))
	b.Write([]byte("defg"))
	if b.String() != "defg" {
		t.Fatalf("tail = %q", b.String())
	}
}

func TestLoadReproEnvDropsTheToken(t *testing.T) {
	for k, v := range map[string]string{"REPO_URL": "u", "REPO_TOKEN": "secret", "PR_REF": "refs/pull/9/head", "REPO_SHA": "c1", "REPRO_COMMAND": "make", "REPRO_TIMEOUT": "600"} {
		t.Setenv(k, v)
	}
	e, err := loadReproEnv()
	if err != nil || e.repoToken != "secret" || e.timeout != 10*time.Minute {
		t.Fatalf("env = %+v, %v", e, err)
	}
	if _, ok := os.LookupEnv("REPO_TOKEN"); ok {
		t.Fatal("REPO_TOKEN is still in the environment the command inherits")
	}
	t.Setenv("REPRO_TIMEOUT", "0")
	if _, err := loadReproEnv(); err == nil {
		t.Fatal("a zero timeout loaded")
	}
}
