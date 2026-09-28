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

// reproEnvs sets the repro-fetch and repro modes' environment.
func reproEnvs(t *testing.T, remote, sha, command string) {
	for k, v := range map[string]string{"REPO_URL": remote, "REPO_TOKEN": "tok", "PR_REF": "refs/pull/9/head", "REPO_SHA": sha,
		"REPRO_COMMAND": command, "REPRO_TIMEOUT": "60"} {
		t.Setenv(k, v)
	}
}

func nolog(string, ...any) {}

// The fetch leaves the head in the workspace, the repro mode runs the
// command there.
func TestReproRunsTheCommandAtTheFetchedHead(t *testing.T) {
	remote, head := reproRemote(t)
	reproEnvs(t, remote, head, "sh check.sh")
	ws := t.TempDir()
	if code := runReproFetch(nolog, ws); code != 0 {
		t.Fatalf("fetch exit %d", code)
	}
	if got := repro(ws); got != (harness.Repro{Exit: 3, Output: "checking\noops\n"}) {
		t.Fatalf("repro = %+v", got)
	}
	t.Setenv("REPRO_COMMAND", "true")
	if got := repro(ws); got != (harness.Repro{Exit: 0}) {
		t.Fatalf("repro = %+v", got)
	}
}

// A head that is not the gated commit is not run: the fetch records why,
// the repro mode reports it.
func TestReproRefusesAMovedHead(t *testing.T) {
	remote, _ := reproRemote(t)
	reproEnvs(t, remote, "0000", "touch ran")
	ws := t.TempDir()
	if code := runReproFetch(nolog, ws); code != 0 {
		t.Fatalf("fetch exit %d", code)
	}
	got := repro(ws)
	if got.Exit != -1 || !strings.Contains(got.Output, "the pull request moved") {
		t.Fatalf("repro = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", "ran")); err == nil {
		t.Fatal("the command ran on a moved head")
	}
}

// The repro mode needs no token: its environment has none.
func TestReproModeReadsNoToken(t *testing.T) {
	remote, head := reproRemote(t)
	reproEnvs(t, remote, head, "printf %s \"$REPO_TOKEN\"")
	ws := t.TempDir()
	if code := runReproFetch(nolog, ws); code != 0 {
		t.Fatalf("fetch exit %d", code)
	}
	os.Unsetenv("REPO_TOKEN")
	for _, k := range []string{"REPO_URL", "PR_REF", "REPO_SHA"} {
		os.Unsetenv(k)
	}
	if got := repro(ws); got != (harness.Repro{Exit: 0}) {
		t.Fatalf("repro = %+v", got)
	}
}

// A command past its time is killed with its process group.
func TestReproduceTimesOut(t *testing.T) {
	remote, head := reproRemote(t)
	reproEnvs(t, remote, head, "")
	ws := t.TempDir()
	if code := runReproFetch(nolog, ws); code != 0 {
		t.Fatalf("fetch exit %d", code)
	}
	e := reproEnv{command: "echo start; sleep 30 & sleep 30", timeout: 300 * time.Millisecond}
	began := time.Now()
	got := reproduce(e, filepath.Join(ws, "repo"))
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

func TestLoadReproEnv(t *testing.T) {
	t.Setenv("REPRO_COMMAND", "make")
	t.Setenv("REPRO_TIMEOUT", "600")
	e, err := loadReproEnv()
	if err != nil || e != (reproEnv{command: "make", timeout: 10 * time.Minute}) {
		t.Fatalf("env = %+v, %v", e, err)
	}
	t.Setenv("REPRO_TIMEOUT", "0")
	if _, err := loadReproEnv(); err == nil {
		t.Fatal("a zero timeout loaded")
	}
}

// A fetch that fails reports git's error without the read token: the
// failure file reaches the repro report, the PR comment and the timeline.
func TestReproFetchFailureHidesTheToken(t *testing.T) {
	reproEnvs(t, "https://x-access-token:s3cret-tok@127.0.0.1:1/repo.git", "0000", "true")
	t.Setenv("REPO_TOKEN", "s3cret-tok")
	ws := t.TempDir()
	if code := runReproFetch(nolog, ws); code != 0 {
		t.Fatalf("fetch exit %d", code)
	}
	got := repro(ws)
	if got.Exit != -1 || !strings.Contains(got.Output, "fetch refs/pull/9/head") || strings.Contains(got.Output, "s3cret-tok") || !strings.Contains(got.Output, "***") {
		t.Fatalf("repro = %+v", got)
	}
}
