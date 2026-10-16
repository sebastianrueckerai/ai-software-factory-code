//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// The repro mode probes the sandbox's egress before the repository's code
// runs: a sandbox more open than its policy, a gateway that does not
// answer (transient), or an env without the probe's targets reports a
// start failure instead of a repro, and the command never runs; a closed
// sandbox runs it. The probe holds the sandbox to the env's targets.
func TestReproProbesTheEgressBeforeTheCommand(t *testing.T) {
	closed := closedSandbox(t)
	open := closed
	open.directIP = listening(t)
	silent := closed
	silent.gateway = restarting(t)
	failed := func(summary, step string, transient bool) harness.WorkerEnvelope {
		return harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: summary, Step: step, Transient: transient}}
	}
	for name, tc := range map[string]struct {
		probe egressProbe
		unset string
		want  harness.WorkerEnvelope
	}{
		"closed": {probe: closed, want: harness.WorkerEnvelope{Type: "repro", Repro: &harness.Repro{Exit: 0}}},
		"open":   {probe: open, want: failed("start failed: egress probe: direct IP "+open.directIP+" answered: connected to "+open.directIP, "egress probe", false)},
		"gateway restarting": {probe: silent,
			want: failed("start failed: egress probe: gateway: "+silent.gateway+"/healthz answered 503 Service Unavailable", "egress probe", true)},
		"no canary": {probe: closed, unset: "EGRESS_CANARY", want: failed("start failed: environment: EGRESS_CANARY is required", "environment", false)},
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.Mkdir(filepath.Join(ws, "repo"), 0o700); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"REPRO_COMMAND": "touch ran", "REPRO_TIMEOUT": "60",
				"GATEWAY_URL": "http://gw", "EGRESS_CANARY": "example.com:443", "EGRESS_DIRECT_IP": "192.0.2.7:443"} {
				t.Setenv(k, v)
			}
			if tc.unset != "" {
				t.Setenv(tc.unset, "")
			}
			got := reproEnvelope(context.Background(), ws, nil, func(e workerEnv) egressProbe {
				if e.gatewayURL != "http://gw" || e.egressCanary != "example.com:443" || e.egressDirectIP != "192.0.2.7:443" {
					t.Errorf("the probe's env = %+v", e)
				}
				return tc.probe
			})
			if !reflect.DeepEqual(got, tc.want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(tc.want)
				t.Fatalf("envelope = %s\nwant %s", g, w)
			}
			_, err := os.Stat(filepath.Join(ws, "repo", "ran"))
			if ran := err == nil; ran != (tc.want.Repro != nil) {
				t.Fatalf("the command ran: %v, want %v", ran, tc.want.Repro != nil)
			}
		})
	}
}

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

// The command runs under the image's /bin/sh: the PATH may lead with a
// directory an agent wrote, where a planted sh would fake the result.
func TestReproduceRunsTheImagesShell(t *testing.T) {
	planted := t.TempDir()
	marker := filepath.Join(t.TempDir(), "planted-ran")
	if err := os.WriteFile(filepath.Join(planted, "sh"), []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", planted+":"+os.Getenv("PATH"))
	got := reproduce(reproEnv{command: "exit 3", timeout: 10 * time.Second}, t.TempDir())
	if _, err := os.Stat(marker); err == nil || got.Exit != 3 {
		t.Fatalf("repro = %+v, planted sh ran: %v", got, err == nil)
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
	if got.Exit != -1 || !strings.Contains(got.Output, "fetch refs/pull/9/head") || strings.Contains(got.Output, "s3cret-tok") || !strings.Contains(got.Output, "[redacted]") {
		t.Fatalf("repro = %+v", got)
	}
}

// With CHECKOUT_SHA the fetch checks out that commit of the head's
// history (the task's base): the command runs before the fix.
func TestReproRunsAtTheBase(t *testing.T) {
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
	commit := func(script string) string {
		if err := os.WriteFile(filepath.Join(seed, "check.sh"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		run("-C", seed, "add", "check.sh")
		run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "c")
		return run("-C", seed, "rev-parse", "HEAD")
	}
	base, head := commit("exit 4\n"), commit("exit 0\n")
	run("clone", "-q", "--bare", seed, remote)
	run("-C", remote, "update-ref", "refs/pull/9/head", head)
	reproEnvs(t, remote, head, "sh check.sh")
	for at, want := range map[string]int{base: 4, "": 0, "1234567": -1} {
		t.Setenv("CHECKOUT_SHA", at)
		ws := t.TempDir()
		if code := runReproFetch(nolog, ws); code != 0 {
			t.Fatalf("fetch exit %d", code)
		}
		if got := repro(ws); got.Exit != want {
			t.Fatalf("at %q: repro = %+v, want exit %d", at, got, want)
		}
	}
}

// The command sees the commit's files and no git metadata (re-review R1):
// a git command cannot tell the base from the head, so it exits the same
// at both and proves no fix; the files still differ.
func TestReproHasNoGitMetadata(t *testing.T) {
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
	commit := func(content string) string {
		if err := os.WriteFile(filepath.Join(seed, "f"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		run("-C", seed, "add", "f")
		run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "c")
		return run("-C", seed, "rev-parse", "HEAD")
	}
	base, head := commit("broken\n"), commit("fixed\n")
	run("clone", "-q", "--bare", seed, remote)
	run("-C", remote, "update-ref", "refs/pull/9/head", head)
	exits := map[string][]int{}
	for _, at := range []string{base, ""} {
		for _, command := range []string{`test "$(git rev-parse HEAD)" = "$(git rev-parse FETCH_HEAD)"`, "git status", "test ! -e .git", "grep -q fixed f"} {
			reproEnvs(t, remote, head, command)
			t.Setenv("CHECKOUT_SHA", at)
			ws := t.TempDir()
			if code := runReproFetch(nolog, ws); code != 0 {
				t.Fatalf("fetch exit %d", code)
			}
			if _, err := os.Stat(filepath.Join(ws, "repo.fetch")); err == nil {
				t.Fatal("the scratch repository is left in the workspace")
			}
			exits[command] = append(exits[command], repro(ws).Exit)
		}
	}
	for command, got := range exits {
		same := got[0] == got[1]
		if command == "grep -q fixed f" {
			if got[0] != 1 || got[1] != 0 {
				t.Errorf("%s: exits %v at base and head, want 1 then 0: the files differ", command, got)
			}
		} else if !same || (command == "test ! -e .git" && got[0] != 0) {
			t.Errorf("%s: exits %v at base and head, want the same (no .git)", command, got)
		}
	}
}

// A repro worker that is not its container's init reports why as a start
// failure at step init, the egress probe's path, and neither probes nor
// runs the command: factoryd reads the start escalation, not a pod that
// exited without a report.
func TestReproModeReportsAWorkerThatIsNoInit(t *testing.T) {
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	reproEnvs(t, "unused", "unused", "touch ran")
	got := reproEnvelope(context.Background(), ws, errors.New("must run as the container's init (pid 1), runs as pid 7"), func(workerEnv) egressProbe {
		t.Error("probed the egress of a worker that is no init")
		return closedSandbox(t)
	})
	want := harness.WorkerEnvelope{Type: "result", Result: &harness.Result{
		Summary: "start failed: init: must run as the container's init (pid 1), runs as pid 7", Step: "init"}}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("envelope = %s\nwant %s", g, w)
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the command ran: %v", err)
	}
}

// A repro-fetch that failed on the way to Forgejo is a transient start
// failure, which the reproduction activity's retry answers; one that found
// the pull request moved is the repro's report, as before.
func TestReproFetchOnTheWayToForgejoIsTransient(t *testing.T) {
	down := "fetch refs/pull/9/head: git fetch -q --no-tags https://forgejo/sf/r.git refs/pull/9/head: exit status 128: " +
		"fatal: unable to access 'https://forgejo/sf/r.git/': The requested URL returned error: 503"
	moved := "refs/pull/9/head is at c2, not the gated head c1: the pull request moved"
	for name, tc := range map[string]struct {
		failed string
		want   harness.WorkerEnvelope
	}{
		"Forgejo down": {down, harness.WorkerEnvelope{Type: "result", Result: &harness.Result{Summary: "start failed: fetch: " + down, Step: "fetch", Transient: true}}},
		"moved":        {moved, harness.WorkerEnvelope{Type: "repro", Repro: &harness.Repro{Exit: -1, Output: "repro mode: " + moved}}},
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(fetchFailed(ws), []byte(tc.failed), 0o600); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"REPRO_COMMAND": "true", "REPRO_TIMEOUT": "60",
				"GATEWAY_URL": "http://gw", "EGRESS_CANARY": "example.com:443", "EGRESS_DIRECT_IP": "192.0.2.7:443"} {
				t.Setenv(k, v)
			}
			closed := closedSandbox(t)
			got := reproEnvelope(context.Background(), ws, nil, func(workerEnv) egressProbe { return closed })
			if !reflect.DeepEqual(got, tc.want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(tc.want)
				t.Fatalf("envelope = %s\nwant %s", g, w)
			}
		})
	}
}
