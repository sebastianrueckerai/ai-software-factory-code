//go:build unix

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

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

// An argv runs with no shell: its metacharacters are plain arguments.
func TestReproduceRunsAnArgvWithoutAShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "injected")
	got := reproduce(reproEnv{argv: []string{"sh", "-c", `test "$1" = "x; touch ` + marker + `" && exit 7`, "sh", "x; touch " + marker}, timeout: 10 * time.Second}, dir)
	if got.Exit != 7 {
		t.Fatalf("repro = %+v, want the argv's exit 7", got)
	}
	got = reproduce(reproEnv{argv: []string{"echo", "$(touch " + marker + ")"}, timeout: 10 * time.Second}, dir)
	if _, err := os.Stat(marker); err == nil || got.Exit != 0 || strings.TrimSpace(got.Output) != "$(touch "+marker+")" {
		t.Fatalf("repro = %+v, marker written: %v", got, err == nil)
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
	if err != nil || e.command != "make" || e.argv != nil || e.timeout != 10*time.Minute {
		t.Fatalf("env = %+v, %v", e, err)
	}
	t.Setenv("REPRO_ARGV", `["go","test","-run","^TestY$","./x"]`)
	if e, err = loadReproEnv(); err != nil || e.command != "" || !slices.Equal(e.argv, []string{"go", "test", "-run", "^TestY$", "./x"}) {
		t.Fatalf("argv env = %+v, %v", e, err)
	}
	for _, bad := range []string{"go test", "[]", `["a",1]`} {
		t.Setenv("REPRO_ARGV", bad)
		if _, err := loadReproEnv(); err == nil {
			t.Fatalf("REPRO_ARGV %q loaded", bad)
		}
	}
	t.Setenv("REPRO_ARGV", "")
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

// With OVERLAY_PKG the base's tree gets the head's test files and testdata
// of that package alone (regular files only, the base's own removed): a
// test the head extends runs its new body on the base's code, a test the
// head adds whole runs, another package's tests stay the base's, a head
// without the package directory fails the fetch.
func TestReproOverlaysTheHeadsTestsAtTheBase(t *testing.T) {
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
	write := func(name, content string) {
		path := filepath.Join(seed, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		run("-C", seed, "add", "-A")
		run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "c")
		return run("-C", seed, "rev-parse", "HEAD")
	}
	write("go.mod", "module m\n\ngo 1.21\n")
	write("a/a.go", "package a\n\nfunc One() int { return 1 }\n")
	write("a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n}\n")
	write("b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n")
	write("a/main_test.go", "package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) { m.Run(); os.Exit(3) }\n")
	write("r.go", "package m\n\nconst R = 1\n")
	write("a/testdata/old.txt", "base fixture\n")
	base := commit()
	run("-C", seed, "rm", "-q", "a/main_test.go")
	write("r.go", "package m\n\nconst R = 2\n")
	write(":!zz_test.go", "package m\n")
	write("a/dir_test.go/x.txt", "a directory, not a test file\n")
	write("a/a.go", "package a\n\nfunc One() int { return 2 }\n")
	write("a/testdata/want.txt", "head fixture\n")
	write("a/sub/x_test.go", "package sub\n")
	if err := os.Symlink("../r.go", filepath.Join(seed, "a", "link_test.go")); err != nil {
		t.Fatal(err)
	}
	run("-C", seed, "rm", "-q", "a/testdata/old.txt")
	write("a/a_test.go", "package a\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 2 {\n\t\tt.Fatal(\"head asserts two\")\n\t}\n}\n")
	write("a/new_test.go", "package a\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {}\n")
	write("b/b_test.go", "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { t.Fatal(\"head b\") }\n")
	head := commit()
	run("clone", "-q", "--bare", seed, remote)
	run("-C", remote, "update-ref", "refs/pull/9/head", head)
	reproEnvs(t, remote, head, "true")
	t.Setenv("CHECKOUT_SHA", base)
	// ./a and a are one package; the overlay is the head's test files, the
	// base's TestMain (deleted at the head) gone, the base's a.go kept
	t.Setenv("OVERLAY_PKG", "./a")
	ws := t.TempDir()
	if code := runReproFetch(nolog, ws); code != 0 {
		t.Fatalf("fetch exit %d", code)
	}
	if err := fetched(ws); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(ws, "repo")
	goTest := func(name, pkg string) harness.Repro {
		return reproduce(reproEnv{argv: []string{"go", "test", "-count=1", "-run", "^" + name + "$", pkg}, timeout: time.Minute}, repo)
	}
	if _, err := os.Stat(filepath.Join(repo, "a", "main_test.go")); err == nil {
		t.Fatal("the base's own test file stays beside the head's")
	}
	if b, err := os.ReadFile(filepath.Join(repo, "a", "testdata", "want.txt")); err != nil || string(b) != "head fixture\n" {
		t.Fatalf("the head's testdata fixture = %q, %v", b, err)
	}
	for _, gone := range []string{"testdata/old.txt", "link_test.go", "sub/x_test.go"} {
		if _, err := os.Lstat(filepath.Join(repo, "a", gone)); err == nil {
			t.Fatalf("a/%s is in the overlaid tree", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "a", "dir_test.go")); err == nil {
		t.Fatal("a directory named like a test file was copied")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a", "a.go")); !strings.Contains(string(b), "return 1") {
		t.Fatalf("a.go = %q, want the base's", b)
	}
	if got := goTest("TestOne", "./a"); got.Exit != 1 || !strings.Contains(got.Output, "head asserts two") {
		t.Fatalf("TestOne at base with the head's tests = %+v, want its new body failing", got)
	}
	if got := goTest("TestNew", "./a"); got.Exit != 0 || strings.Contains(got.Output, "no tests to run") {
		t.Fatalf("TestNew at base = %+v, want the head's added test run", got)
	}
	if got := goTest("TestB", "./b"); got.Exit != 0 {
		t.Fatalf("TestB = %+v, want the base's test, the package not being overlaid", got)
	}
	// a package the head lacks fails the fetch: the run is unsettled
	t.Setenv("OVERLAY_PKG", "nosuchdir")
	ws = t.TempDir()
	runReproFetch(nolog, ws)
	if err := fetched(ws); err == nil || !strings.Contains(err.Error(), "no package directory nosuchdir") {
		t.Fatalf("a package the head lacks: %v", err)
	}
	if got := repro(ws); got.Exit != -1 {
		t.Fatalf("repro = %+v, want exit -1", got)
	}
	// a package that is a file at the head is no package directory
	t.Setenv("OVERLAY_PKG", "a/a.go")
	ws = t.TempDir()
	runReproFetch(nolog, ws)
	if err := fetched(ws); err == nil || !strings.Contains(err.Error(), "no package directory a/a.go") {
		t.Fatalf("a file as the package: %v", err)
	}
	// the root package: a head file named like a pathspec is no pathspec
	t.Setenv("OVERLAY_PKG", ".")
	ws = t.TempDir()
	runReproFetch(nolog, ws)
	if err := fetched(ws); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "repo", "r.go")); !strings.Contains(string(b), "R = 1") {
		t.Fatalf("r.go = %q, want the base's", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "repo", ":!zz_test.go")); err != nil {
		t.Fatalf("the head's test file was not copied: %v", err)
	}
	t.Setenv("OVERLAY_PKG", "../x")
	ws = t.TempDir()
	runReproFetch(nolog, ws)
	if err := fetched(ws); err == nil || !strings.Contains(err.Error(), "not inside the repository") {
		t.Fatalf("an overlay outside the repository: %v", err)
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

// proofsCheck is the check script of the proofs tests: it prints its
// arguments, the paths it got, whether it sees the bearer's hash and
// whether the message port answers, then writes a report naming the test
// (the suite's when none) and an artifact; suite ./none exits 2 first.
const proofsCheck = `printf 'argc=%s %s\n' "$#" "$*"
printf 'report=%s\nartifacts=%s\n' "$CHECK_REPORT" "$CHECK_ARTIFACTS"
printf 'hash=%s\n' "${MESSAGE_TOKEN_SHA256-unset}"
if (exec 3<>/dev/tcp/127.0.0.1/8091) 2>/dev/null; then echo port=open; else echo port=closed; fi
[ -d "$CHECK_ARTIFACTS" ] || exit 9
[ "$2" = ./none ] && exit 2
printf '<testsuites><testcase name="%s"></testcase></testsuites>' "${3:-suite}" >"$CHECK_REPORT"
printf 'seen %s' "$2" >"$CHECK_ARTIFACTS/page.txt"
`

// proofsWorkspace is a workspace whose repo holds check with script.
func proofsWorkspace(t *testing.T, script string) (ws, repo string) {
	t.Helper()
	ws = t.TempDir()
	repo = filepath.Join(ws, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "check"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws, repo
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// emitted gathers the envelopes its emit gets.
type emitted []harness.WorkerEnvelope

func (e *emitted) emit(env harness.WorkerEnvelope) { *e = append(*e, env) }

// runs is the proof runs of e's envelopes, each of Type "proof".
func (e emitted) runs(t *testing.T) []harness.ProofRun {
	t.Helper()
	var runs []harness.ProofRun
	for _, env := range e {
		if env.Type != "proof" || env.Proof == nil {
			t.Fatalf("envelope %+v is no proof", env)
		}
		runs = append(runs, *env.Proof)
	}
	return runs
}

// proofRuns runs proofs in ws, a minute each within ten, and returns
// their runs.
func proofRuns(t *testing.T, ws string, proofs ...harness.Proof) []harness.ProofRun {
	t.Helper()
	var got emitted
	runProofList(proofsEnv{proofs: proofs, timeout: time.Minute, budget: 10 * time.Minute}, ws, time.Now(), got.emit)
	if len(got) != len(proofs) {
		t.Fatalf("%d envelopes for %d proofs", len(got), len(proofs))
	}
	return got.runs(t)
}

// proofsEnvs sets the proofs mode's environment: a minute an entry
// within ten.
func proofsEnvs(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{"REPRO_TIMEOUT": "60", "PROOFS_BUDGET": "600"} {
		t.Setenv(k, v)
	}
}

// listed is a proofs mode's list as a test hands it over, in place of the
// message port.
func listed(proofs ...harness.Proof) func() ([]harness.Proof, error) {
	return func() ([]harness.Proof, error) { return proofs, nil }
}

// The proofs mode runs bash check test with the entry's suite, and its
// name only when given, each with a report path and an artifacts
// directory of its own outside the tree, and returns what the check left,
// each run started whatever its exit.
func TestProofsRunCheckTestPerEntry(t *testing.T) {
	ws, repo := proofsWorkspace(t, proofsCheck)
	proofsEnvs(t)
	var got emitted
	closing := proofs(ws, []harness.Proof{{ID: "a", Suite: "./a", Name: "TestA"}, {ID: "all", Suite: "./..."}, {ID: "none", Suite: "./none"}}, time.Now(), got.emit)
	if want := (harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{}}); !reflect.DeepEqual(closing, want) {
		t.Fatalf("closing envelope = %+v", closing)
	}
	runs := got.runs(t)
	if len(runs) != 3 {
		t.Fatalf("runs = %+v", runs)
	}
	for i, want := range []struct {
		id, first, report string
		exit              int
		artifacts         map[string][]byte
	}{
		{"a", "argc=3 test ./a TestA", `<testsuites><testcase name="TestA"></testcase></testsuites>`, 0, map[string][]byte{"page.txt": []byte("seen ./a")}},
		{"all", "argc=2 test ./...", `<testsuites><testcase name="suite"></testcase></testsuites>`, 0, map[string][]byte{"page.txt": []byte("seen ./...")}},
		{"none", "argc=2 test ./none", "", 2, nil},
	} {
		r := runs[i]
		if r.ID != want.id || r.Exit != want.exit || !r.Started || strings.SplitN(r.Output, "\n", 2)[0] != want.first {
			t.Errorf("run %d = %+v, want id %s, exit %d, first line %q", i, r, want.id, want.exit, want.first)
		}
		if want.report == "" && r.Report != nil {
			t.Errorf("%s: report %q, want none", r.ID, gunzip(t, r.Report))
		}
		if want.report != "" && (r.Report == nil || gunzip(t, r.Report) != want.report) {
			t.Errorf("%s: report %q, want %q", r.ID, r.Report, want.report)
		}
		if !reflect.DeepEqual(r.Artifacts, want.artifacts) {
			t.Errorf("%s: artifacts %q, want %q", r.ID, r.Artifacts, want.artifacts)
		}
	}
	paths := map[string]bool{}
	for _, r := range runs {
		for _, line := range strings.Split(r.Output, "\n") {
			for _, key := range []string{"report=", "artifacts="} {
				if p, ok := strings.CutPrefix(line, key); ok {
					if rel, err := filepath.Rel(repo, p); err != nil || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						t.Errorf("%s: %s%s lies in the tree", r.ID, key, p)
					}
					paths[p] = true
				}
			}
		}
	}
	if len(paths) != 6 {
		t.Errorf("paths = %v, want six distinct", paths)
	}
}

// An entry's file is in the tree while its check runs, and only then: the
// tree's own file at its path is back after, so a later entry, a suite
// above all, runs on the tree as pushed.
func TestProofsWriteTheEntrysFileForItsRunAlone(t *testing.T) {
	ws, repo := proofsWorkspace(t, "cat p/*_test.go\n")
	if err := os.Mkdir(filepath.Join(repo, "p"), 0o700); err != nil {
		t.Fatal(err)
	}
	const own = "package p // the tree's own\n"
	if err := os.WriteFile(filepath.Join(repo, "p", "own_test.go"), []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}
	runs := proofRuns(t, ws,
		harness.Proof{ID: "new", Suite: "./p", Name: "TestNew", File: &harness.ProofFile{Path: "p/new_test.go", Content: "package p // new\n"}},
		harness.Proof{ID: "over", Suite: "./p", Name: "TestOver", File: &harness.ProofFile{Path: "p/own_test.go", Content: "package p // over\n"}},
		harness.Proof{ID: "all", Suite: "./..."})
	for i, want := range []string{"package p // new\n" + own, "package p // over\n", own} {
		if runs[i].Exit != 0 || runs[i].Output != want {
			t.Errorf("run %s = %+v, want the check to read %q", runs[i].ID, runs[i], want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(repo, "p", "own_test.go")); err != nil || string(b) != own {
		t.Fatalf("the tree's own file holds %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "p", "new_test.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the entry's file stayed: %v", err)
	}
}

// A file the tree cannot hold, by a parent step, an absolute path or a
// link out of it, is not written, and its entry does not start.
func TestProofsRefuseAFileOutsideTheTree(t *testing.T) {
	ws, repo := proofsWorkspace(t, "touch ran")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(t.TempDir(), "abs_test.go")
	var proofs []harness.Proof
	for _, path := range []string{"../escape_test.go", abs, "link/x_test.go"} {
		proofs = append(proofs, harness.Proof{ID: path, Suite: "./x", File: &harness.ProofFile{Path: path, Content: "package x\n"}})
	}
	for _, r := range proofRuns(t, ws, proofs...) {
		if r.Exit != -1 || r.Started || !strings.HasPrefix(r.Output, "proofs mode: write ") {
			t.Errorf("run = %+v", r)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check ran: %v", err)
	}
	for _, p := range []string{filepath.Join(ws, "escape_test.go"), abs, filepath.Join(outside, "x_test.go")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was written: %v", p, err)
		}
	}
}

// The artifacts are the regular files of the directory, each cut at
// 256 KiB: no link, no directory.
func TestProofsCapAnArtifactAndReadOnlyRegularFiles(t *testing.T) {
	ws, _ := proofsWorkspace(t, `head -c 300000 /dev/zero >"$CHECK_ARTIFACTS/big.bin"
ln -s /etc/hostname "$CHECK_ARTIFACTS/host"
mkdir "$CHECK_ARTIFACTS/sub"
echo x >"$CHECK_ARTIFACTS/sub/x"
`)
	r := proofRuns(t, ws, harness.Proof{ID: "a", Suite: "./a"})[0]
	if len(r.Artifacts) != 1 || len(r.Artifacts["big.bin"]) != 262144 {
		t.Fatalf("artifacts: %d keys, big.bin %d bytes", len(r.Artifacts), len(r.Artifacts["big.bin"]))
	}
}

// A text cut at 256 KiB through a character keeps no part of it: the
// artifact stays valid text. One that ends at the cap is kept whole.
func TestProofsCutAnArtifactOnACharacterBoundary(t *testing.T) {
	ws, _ := proofsWorkspace(t, `head -c 262143 /dev/zero | tr '\0' a >"$CHECK_ARTIFACTS/cut.txt"
printf '\303\251b' >>"$CHECK_ARTIFACTS/cut.txt"
head -c 262142 /dev/zero | tr '\0' a >"$CHECK_ARTIFACTS/whole.txt"
printf '\303\251' >>"$CHECK_ARTIFACTS/whole.txt"
`)
	r := proofRuns(t, ws, harness.Proof{ID: "a", Suite: "./a"})[0]
	if cut := r.Artifacts["cut.txt"]; len(cut) != 262143 || !utf8.Valid(cut) {
		t.Fatalf("cut.txt: %d bytes, valid %v; want the 262143 bytes before the cut character", len(cut), utf8.Valid(cut))
	}
	if whole := r.Artifacts["whole.txt"]; len(whole) != 262144 || !utf8.Valid(whole) {
		t.Fatalf("whole.txt: %d bytes, valid %v; want all 262144", len(whole), utf8.Valid(whole))
	}
}

// Three artifacts at their cap would pass the 1 MiB that factoryd reads
// of one log line, which would lose the report with them: those past the
// line's room are left out and named in Dropped.
func TestProofsLeaveOutArtifactsPastTheLine(t *testing.T) {
	ws, _ := proofsWorkspace(t, `for f in a b c; do head -c 300000 /dev/zero >"$CHECK_ARTIFACTS/$f.bin"; done
printf '<testsuites></testsuites>' >"$CHECK_REPORT"
`)
	var got emitted
	runProofList(proofsEnv{proofs: []harness.Proof{{ID: "a", Suite: "./a"}}, timeout: time.Minute, budget: time.Minute}, ws, time.Now(), got.emit)
	r := got.runs(t)[0]
	if len(r.Artifacts) != 2 || len(r.Artifacts["a.bin"]) != 262144 || len(r.Artifacts["b.bin"]) != 262144 {
		t.Fatalf("artifacts: %d keys, a.bin %d bytes, b.bin %d bytes", len(r.Artifacts), len(r.Artifacts["a.bin"]), len(r.Artifacts["b.bin"]))
	}
	if r.Exit != 0 || r.Report == nil || !slices.Equal(r.Dropped, []string{"c.bin"}) {
		t.Fatalf("exit %d, a report: %v, dropped %q", r.Exit, r.Report != nil, r.Dropped)
	}
	if line, err := json.Marshal(got[0]); err != nil || len(line) >= 1<<20 {
		t.Fatalf("line of %d bytes, %v", len(line), err)
	}
}

// A report past 64 MiB is not read; one that does not fit one log line
// gzipped is left out. Either entry's line still holds its exit and why.
func TestProofsLeaveOutAReportPastItsBounds(t *testing.T) {
	ws, _ := proofsWorkspace(t, `case $2 in
./huge) head -c $(((64 << 20) + 1)) /dev/zero >"$CHECK_REPORT" ;;
./dense) head -c 1100000 /dev/urandom >"$CHECK_REPORT" ;;
esac
`)
	var got emitted
	runProofList(proofsEnv{proofs: []harness.Proof{{ID: "huge", Suite: "./huge"}, {ID: "dense", Suite: "./dense"}},
		timeout: time.Minute, budget: time.Minute}, ws, time.Now(), got.emit)
	runs := got.runs(t)
	for i, why := range []string{"\nproofs mode: the report passed 64 MiB", " bytes gzipped, does not fit one log line"} {
		r := runs[i]
		if r.Exit != 0 || r.Report != nil || !strings.Contains(r.Output, why) {
			t.Errorf("%s: exit %d, a report: %v, output %q", r.ID, r.Exit, r.Report != nil, r.Output)
		}
		if line, err := json.Marshal(got[i]); err != nil || len(line) >= 1<<20 {
			t.Errorf("%s: line of %d bytes, %v", r.ID, len(line), err)
		}
	}
}

// A FIFO at an entry's file or at its report blocks no write and no read:
// the entry ends at once.
func TestProofsNeverBlockOnAFIFO(t *testing.T) {
	ws, repo := proofsWorkspace(t, "[ \"$2\" = ./pipe ] && mkfifo \"$CHECK_REPORT\"\nexit 0\n")
	if err := os.Mkdir(filepath.Join(repo, ".holdout1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repo, ".holdout1", "holdout_test.go"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan []harness.ProofRun)
	go func() {
		var got emitted
		runProofList(proofsEnv{proofs: []harness.Proof{
			{ID: "held", Suite: "./.holdout1", Name: "TestDoor", File: &harness.ProofFile{Path: ".holdout1/holdout_test.go", Content: "package holdout\n"}},
			{ID: "pipe", Suite: "./pipe"}}, timeout: time.Minute, budget: time.Minute}, ws, time.Now(), got.emit)
		var runs []harness.ProofRun
		for _, env := range got {
			runs = append(runs, *env.Proof)
		}
		done <- runs
	}()
	var runs []harness.ProofRun
	select {
	case runs = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a FIFO blocked the run")
	}
	if held := runs[0]; held.Exit != -1 || !strings.HasPrefix(held.Output, "proofs mode: write .holdout1/holdout_test.go: ") {
		t.Errorf("held = %+v", held)
	}
	if pipe := runs[1]; pipe.Exit != 0 || pipe.Report != nil || !strings.Contains(pipe.Output, "proofs mode: report.xml is not a regular file") {
		t.Errorf("pipe = %+v", pipe)
	}
}

// An entry's own directory is new: one a process left there, with a
// report planted in it, fails the entry before the check starts.
func TestProofsRefuseAnEntryDirectoryThatExists(t *testing.T) {
	ws, repo := proofsWorkspace(t, "touch ran\n")
	planted := filepath.Join(ws, "proofs", "0")
	if err := os.MkdirAll(planted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "report.xml"), []byte(`<testsuites><testcase name="TestA"></testcase></testsuites>`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := proofRuns(t, ws, harness.Proof{ID: "a", Suite: "./a", Name: "TestA"})[0]
	if r.Exit != -1 || r.Started || r.Report != nil || !strings.HasPrefix(r.Output, "proofs mode: mkdir ") {
		t.Fatalf("run = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(repo, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check ran: %v", err)
	}
}

// Each entry's envelope goes out before the next entry starts: the
// proofs mode stops the processes an entry left there.
func TestProofsEmitEachEntryBeforeTheNextStarts(t *testing.T) {
	ws, repo := proofsWorkspace(t, "touch \"ran-$2\"\n")
	var ids []string
	runProofList(proofsEnv{proofs: []harness.Proof{{ID: "a", Suite: "a"}, {ID: "b", Suite: "b"}}, timeout: time.Minute, budget: time.Minute}, ws, time.Now(),
		func(env harness.WorkerEnvelope) {
			ids = append(ids, env.Proof.ID)
			_, errA := os.Stat(filepath.Join(repo, "ran-a"))
			_, errB := os.Stat(filepath.Join(repo, "ran-b"))
			if env.Proof.ID == "a" && (errA != nil || !errors.Is(errB, os.ErrNotExist)) {
				t.Errorf("at a's envelope: ran-a %v, ran-b %v", errA, errB)
			}
		})
	if !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("envelopes %v", ids)
	}
}

// A report of a large suite rides one log line gzipped, under the 1 MiB
// that factoryd reads of one.
func TestProofsFitALargeReportInOneLogLine(t *testing.T) {
	ws, _ := proofsWorkspace(t, `{
printf '<testsuites>\n'
printf '<testcase classname="github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd" name="Test%d"></testcase>\n' {1..20000}
printf '</testsuites>\n'
} >"$CHECK_REPORT"
`)
	var got emitted
	runProofList(proofsEnv{proofs: []harness.Proof{{ID: "all", Suite: "./..."}}, timeout: time.Minute, budget: time.Minute}, ws, time.Now(), got.emit)
	r := got.runs(t)[0]
	if n := len(gunzip(t, r.Report)); n <= 2<<20 {
		t.Fatalf("report of %d bytes", n)
	}
	line, err := json.Marshal(got[0])
	if err != nil || len(line) >= 1<<20 {
		t.Fatalf("line of %d bytes, %v", len(line), err)
	}
}

// Each entry has its timeout, and all share the run's budget: an entry
// its own timeout ends had its try; one the budget cuts short says so and
// had none, as one it does not reach, which does not start. An entry the
// budget shortens that ends before the deadline had its try.
func TestProofsTimeOutPerEntryAndWithinTheBudget(t *testing.T) {
	list := []harness.Proof{{ID: "slow", Suite: "./slow"}, {ID: "fast", Suite: "./a", Name: "TestA"}}
	for _, tc := range []struct {
		timeout, budget time.Duration
		slow            string
		slowStarted     bool
		fast            harness.ProofRun
	}{
		{300 * time.Millisecond, time.Minute, "\n(timed out after 300ms)", true, harness.ProofRun{ID: "fast", Exit: 0, Started: true}},
		{time.Minute, 300 * time.Millisecond, "\n(the run's 300ms ran out)", false, harness.ProofRun{ID: "fast", Exit: -1, Output: "proofs mode: the run's 300ms ran out before this entry"}},
	} {
		ws, _ := proofsWorkspace(t, "if [ \"$2\" = ./slow ]; then sleep 30; fi\n")
		began := time.Now()
		var got emitted
		runProofList(proofsEnv{proofs: list, timeout: tc.timeout, budget: tc.budget}, ws, time.Now(), got.emit)
		runs := got.runs(t)
		if len(runs) != 2 || runs[0].ID != "slow" || runs[0].Exit != -1 || runs[0].Started != tc.slowStarted || !strings.HasSuffix(runs[0].Output, tc.slow) {
			t.Fatalf("timeout %s, budget %s: runs = %+v", tc.timeout, tc.budget, runs)
		}
		if !reflect.DeepEqual(runs[1], tc.fast) {
			t.Errorf("timeout %s, budget %s: fast = %+v, want %+v", tc.timeout, tc.budget, runs[1], tc.fast)
		}
		if took := time.Since(began); took > 10*time.Second {
			t.Errorf("timeout %s, budget %s: took %s", tc.timeout, tc.budget, took)
		}
	}
	ws, _ := proofsWorkspace(t, "exit 1\n")
	var got emitted
	runProofList(proofsEnv{proofs: []harness.Proof{{ID: "short", Suite: "./a"}}, timeout: time.Minute, budget: 30 * time.Second}, ws, time.Now(), got.emit)
	if r := got.runs(t)[0]; r.Exit != 1 || !r.Started {
		t.Errorf("an entry the budget shortens, ended before the deadline = %+v, want exit 1 and started", r)
	}
}

// The check runs under the image's /bin/bash: the PATH may lead with a
// directory an agent wrote, where a planted bash would fake the result.
func TestProofsRunTheImagesBash(t *testing.T) {
	planted := t.TempDir()
	marker := filepath.Join(t.TempDir(), "planted-ran")
	if err := os.WriteFile(filepath.Join(planted, "bash"), []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", planted+":"+os.Getenv("PATH"))
	ws, _ := proofsWorkspace(t, "exit 3\n")
	r := proofRuns(t, ws, harness.Proof{ID: "a", Suite: "./a"})[0]
	if _, err := os.Stat(marker); err == nil || r.Exit != 3 {
		t.Fatalf("run = %+v, planted bash ran: %v", r, err == nil)
	}
}

// The check is the tree's own: with none in the tree, a check on the PATH
// does not run in its place.
func TestProofsRunTheTreesCheck(t *testing.T) {
	planted := t.TempDir()
	marker := filepath.Join(t.TempDir(), "planted-ran")
	if err := os.WriteFile(filepath.Join(planted, "check"), []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", planted+":"+os.Getenv("PATH"))
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := proofRuns(t, ws, harness.Proof{ID: "a", Suite: "./a"})[0]
	if _, err := os.Stat(marker); err == nil || r.Exit != 127 {
		t.Fatalf("run = %+v, planted check ran: %v", r, err == nil)
	}
}

// A fetch that failed or an environment the mode cannot read ends the
// run before any entry, as does a sandbox more open than its policy.
func TestProofsReportAFetchFailureOrABadEnvironment(t *testing.T) {
	moved := "refs/pull/9/head is at c2, not the gated head c1: the pull request moved"
	notRan := func(repo string) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(repo, "ran")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the check ran: %v", err)
		}
	}
	list := []harness.Proof{{ID: "a", Suite: "./a"}}
	ws, repo := proofsWorkspace(t, "touch ran")
	proofsEnvs(t)
	if err := os.WriteFile(fetchFailed(ws), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	var got emitted
	if closing := proofs(ws, list, time.Now(), got.emit); len(got) != 0 || closing.Type != "proofs" || closing.Proofs == nil || closing.Proofs.Error != "proofs mode: "+moved {
		t.Fatalf("emitted %d, closing envelope = %+v", len(got), closing)
	}
	notRan(repo)

	ws, repo = proofsWorkspace(t, "touch ran")
	t.Setenv("PROOFS_BUDGET", "0")
	if closing := proofs(ws, list, time.Now(), got.emit); len(got) != 0 || closing.Proofs == nil || closing.Proofs.Error != "proofs mode: PROOFS_BUDGET: want positive seconds" {
		t.Fatalf("emitted %d, closing envelope = %+v", len(got), closing)
	}
	notRan(repo)

	proofsEnvs(t)
	sandboxEnvs(t)
	open := closedSandbox(t)
	open.directIP = listening(t)
	closing := proofsReport(context.Background(), ws, nil, func(workerEnv) egressProbe { return open }, listed(list...), got.emit)
	want := harness.WorkerEnvelope{Type: "result", Result: &harness.Result{
		Summary: "start failed: egress probe: direct IP " + open.directIP + " answered: connected to " + open.directIP, Step: "egress probe"}}
	if len(got) != 0 || !reflect.DeepEqual(closing, want) {
		g, _ := json.Marshal(closing)
		w, _ := json.Marshal(want)
		t.Fatalf("emitted %d, closing envelope = %s\nwant %s", len(got), g, w)
	}
	notRan(repo)
}

func TestLoadProofsEnv(t *testing.T) {
	t.Setenv("REPRO_TIMEOUT", "600")
	t.Setenv("PROOFS_BUDGET", "2100")
	e, err := loadProofsEnv()
	if want := (proofsEnv{timeout: 10 * time.Minute, budget: 35 * time.Minute}); err != nil || !reflect.DeepEqual(e, want) {
		t.Fatalf("env = %+v, %v", e, err)
	}
	t.Setenv("PROOFS_BUDGET", "0")
	if _, err := loadProofsEnv(); err == nil {
		t.Fatal("a zero budget loaded")
	}
}

// messagePortFree skips a test that binds the worker's one fixed message
// port while something holds it, as inside a live worker pod.
func messagePortFree(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(harness.MessagePort))
	if err != nil {
		t.Skip("the worker's message port is held: " + err.Error())
	}
	ln.Close()
}

// postProofs posts body to the proofs mode's port with auth, again until
// the port answers within five seconds: its status, 0 for none.
func postProofs(auth, body string) int {
	for deadline := time.Now().Add(5 * time.Second); ; {
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(harness.MessagePort)+"/proofs", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			return resp.StatusCode
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sandboxEnvs sets what the egress probe of the proofs mode reads.
func sandboxEnvs(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{"GATEWAY_URL": "http://gw", "EGRESS_CANARY": "example.com:443", "EGRESS_DIRECT_IP": "192.0.2.7:443"} {
		t.Setenv(k, v)
	}
}

// The proofs mode takes its list over the message port before anything
// else: a post without the session's bearer, or with no list, is refused
// and the wait goes on; the list posted runs. The port is closed by the
// time a check runs, and no check sees the bearer's hash.
func TestProofsTakeTheListOverTheMessagePort(t *testing.T) {
	messagePortFree(t)
	ws, _ := proofsWorkspace(t, proofsCheck)
	proofsEnvs(t)
	sandboxEnvs(t)
	t.Setenv("MESSAGE_TOKEN_SHA256", testMessageHash)
	old := proofsListWait
	t.Cleanup(func() { proofsListWait = old })
	proofsListWait = 10 * time.Second
	answers := make(chan []int, 1)
	go func() {
		answers <- []int{
			postProofs("", `[{"id":"a","suite":"./a","name":"TestA"}]`),
			postProofs("Bearer session-secret", "not json"),
			postProofs("Bearer session-secret", `[{"id":"a","suite":"./a","name":"TestA"}]`),
		}
	}()
	var got emitted
	closing := proofsReport(context.Background(), ws, nil, func(workerEnv) egressProbe { return closedSandbox(t) },
		func() ([]harness.Proof, error) { return awaitProofs(nolog) }, got.emit)
	if codes := <-answers; !slices.Equal(codes, []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusNoContent}) {
		t.Fatalf("the port answered %v, want 401, 400, 204", codes)
	}
	if want := (harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{}}); !reflect.DeepEqual(closing, want) {
		t.Fatalf("closing envelope = %+v", closing)
	}
	runs := got.runs(t)
	if len(runs) != 1 || runs[0].ID != "a" || runs[0].Exit != 0 || !strings.HasPrefix(runs[0].Output, "argc=3 test ./a TestA\n") {
		t.Fatalf("runs = %+v, want a's", runs)
	}
	for _, line := range []string{"\nhash=unset\n", "\nport=closed\n"} {
		if !strings.Contains(runs[0].Output, line) {
			t.Errorf("the check's output %q lacks %q", runs[0].Output, line)
		}
	}
}

// A pod nobody posts a list to ends at the wait with the closing
// envelope's error, before the egress probe and any check, and frees the
// port; a bearer's hash it cannot read ends it at once.
func TestProofsEndWhenNoListArrives(t *testing.T) {
	messagePortFree(t)
	ws, repo := proofsWorkspace(t, "touch ran")
	proofsEnvs(t)
	old := proofsListWait
	t.Cleanup(func() { proofsListWait = old })
	proofsListWait = 50 * time.Millisecond
	probed := false
	probe := func(workerEnv) egressProbe { probed = true; return closedSandbox(t) }
	port := func() ([]harness.Proof, error) { return awaitProofs(nolog) }
	for hash, want := range map[string]string{
		testMessageHash: "proofs mode: no proofs list from factoryd within 50ms",
		"":              "proofs mode: MESSAGE_TOKEN_SHA256 must be 64 hex digits",
	} {
		t.Setenv("MESSAGE_TOKEN_SHA256", hash)
		var got emitted
		closing := proofsReport(context.Background(), ws, nil, probe, port, got.emit)
		if len(got) != 0 || closing.Type != "proofs" || closing.Proofs == nil || closing.Proofs.Error != want {
			t.Fatalf("emitted %d, closing envelope = %+v, want the error %q", len(got), closing, want)
		}
	}
	if probed {
		t.Error("the egress probe ran before the list")
	}
	if _, err := os.Stat(filepath.Join(repo, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check ran: %v", err)
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(harness.MessagePort))
	if err != nil {
		t.Fatalf("the port is still held: %v", err)
	}
	ln.Close()
}

// The wait for the list counts against the run's budget, so the pod's
// time past its start stays bounded however late the list comes.
func TestProofsCountTheWaitAgainstTheBudget(t *testing.T) {
	ws, repo := proofsWorkspace(t, "touch ran")
	t.Setenv("REPRO_TIMEOUT", "60")
	t.Setenv("PROOFS_BUDGET", "1")
	sandboxEnvs(t)
	late := func() ([]harness.Proof, error) {
		time.Sleep(1100 * time.Millisecond)
		return []harness.Proof{{ID: "a", Suite: "./a"}}, nil
	}
	var got emitted
	proofsReport(context.Background(), ws, nil, func(workerEnv) egressProbe { return closedSandbox(t) }, late, got.emit)
	if runs := got.runs(t); !reflect.DeepEqual(runs, []harness.ProofRun{{ID: "a", Exit: -1, Output: "proofs mode: the run's 1s ran out before this entry"}}) {
		t.Fatalf("runs = %+v, want a out of budget", runs)
	}
	if _, err := os.Stat(filepath.Join(repo, "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check ran: %v", err)
	}
}

// A failing listing of the head fails the overlay, never an empty one.
func TestOverlayFailsWhenTheListingFails(t *testing.T) {
	err := overlayTests(workerEnv{}, filepath.Join(t.TempDir(), "no-repository"), "abc", ".", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "list the head's") {
		t.Fatalf("overlay = %v, want the listing's failure", err)
	}
}

// The base's package directory may be a symlink out of the workspace: the
// removal of its test files cannot follow it.
func TestOverlayRemovalStaysInTheWorkspace(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim_test.go")
	if err := os.WriteFile(victim, []byte("package v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	run("init", "-q", "-b", "main", seed)
	commit := func() string {
		run("-C", seed, "add", "-A")
		run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "c")
		return run("-C", seed, "rev-parse", "HEAD")
	}
	if err := os.Symlink(outside, filepath.Join(seed, "a")); err != nil {
		t.Fatal(err)
	}
	base := commit()
	run("-C", seed, "rm", "-q", "a")
	if err := os.MkdirAll(filepath.Join(seed, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "a", "a_test.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := commit()
	run("clone", "-q", "--bare", seed, remote)
	run("-C", remote, "update-ref", "refs/pull/9/head", head)
	reproEnvs(t, remote, head, "true")
	t.Setenv("CHECKOUT_SHA", base)
	t.Setenv("OVERLAY_PKG", "a")
	ws := t.TempDir()
	runReproFetch(nolog, ws)
	if err := fetched(ws); err == nil {
		t.Fatal("the overlay followed the base's symlinked package directory")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("the file outside the workspace is gone: %v", err)
	}
}
