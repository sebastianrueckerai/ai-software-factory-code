package scripts

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A module of four packages: b imports a, c imports a only in its tests,
// d stands alone. A package without tests has nothing to run, so b has one.
var module = map[string]string{
	"go.mod":      "module example.com/m\n\ngo 1.22\n",
	"a/a.go":      "package a\n",
	"b/b.go":      "package b\n\nimport _ \"example.com/m/a\"\n",
	"b/b_test.go": "package b\n",
	"c/c.go":      "package c\n",
	"c/c_test.go": "package c\n\nimport _ \"example.com/m/a\"\n",
	"d/d.go":      "package d\n",
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo is the module committed in a fresh repository, and its commit.
func repo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	write(t, dir, module)
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

func affected(t *testing.T, dir, base string) string {
	t.Helper()
	script, err := filepath.Abs("affected-packages.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, base)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("affected-packages.sh: %v", err)
	}
	return string(out)
}

func TestAffectedPackages(t *testing.T) {
	for name, tt := range map[string]struct {
		change func(t *testing.T, dir string)
		want   string
	}{
		"a package, its importer and its test importer": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"a/a.go": "package a\n\nconst X = 1\n"})
		}, "example.com/m/a example.com/m/b example.com/m/c\n"},
		"a committed change": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"d/d.go": "package d\n\nconst X = 1\n"})
			git(t, dir, "commit", "-q", "-am", "d")
		}, "example.com/m/d\n"},
		"an untracked package": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"e/e.go": "package e\n"})
		}, "example.com/m/e\n"},
		"a diff confined to k8s/": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"k8s/probe.yaml": "kind: ConfigMap\n"})
		}, "./internal/reproducible/...\n"},
		"k8s/ and a Go change": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"k8s/probe.yaml": "kind: ConfigMap\n", "a/a.go": "package a\n\nconst X = 1\n"})
		}, "./...\n"},
		"a non-Go file outside k8s/": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"docs/notes.md": "n\n"})
		}, "./...\n"},
		"a non-Go file": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"a/a.go": "package a\n\nconst X = 1\n", "README.md": "m\n"})
		}, "./...\n"},
		"a directory left without Go files": {func(t *testing.T, dir string) {
			git(t, dir, "rm", "-q", "d/d.go")
		}, "./...\n"},
		"no change": {func(t *testing.T, dir string) {}, "./...\n"},
		"a subdirectory": {func(t *testing.T, dir string) {
			write(t, dir, map[string]string{"b/b.go": "package b\n\nimport _ \"example.com/m/a\"\n\nconst X = 1\n"})
		}, "example.com/m/b\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, base := repo(t)
			tt.change(t, dir)
			run := dir
			if name == "a subdirectory" {
				run = filepath.Join(dir, "a")
			}
			if got := affected(t, run, base); got != tt.want {
				t.Errorf("selection = %q, want %q", got, tt.want)
			}
		})
	}
}

// pr.yml selects by the base's copy of the script, so a pull request's
// edit of the rule does not pick its own tests.
func TestPullRequestJobSelectsByTheBasesScript(t *testing.T) {
	raw, err := os.ReadFile("../.forgejo/workflows/pr.yml")
	if err != nil {
		t.Fatal(err)
	}
	pr := string(raw)
	if !strings.Contains(pr, `git show "$base:scripts/affected-packages.sh"`) {
		t.Error("pr.yml does not run the base's scripts/affected-packages.sh")
	}
	if strings.Contains(pr, "go list") {
		t.Error("pr.yml walks the packages itself")
	}
}

// pr.yml's test run bounds go test at 30 minutes, its format check names
// the files it would change, and its image build prints a failed build's log.
func TestPullRequestJobsKeepTheirTimeoutFormatNamesAndBuildLog(t *testing.T) {
	raw, err := os.ReadFile("../.forgejo/workflows/pr.yml")
	if err != nil {
		t.Fatal(err)
	}
	pr := string(raw)
	for _, line := range []string{
		`scripts/pg-test.sh go test -timeout 30m `,
		`printf 'not gofmt-formatted:\n%s\n' "$files"`,
		"echo \"---- $df build log ----\"\n            cat \"$logdir/$df.log\"",
	} {
		if !strings.Contains(pr, line) {
			t.Errorf("pr.yml lacks %q", line)
		}
	}
}

// check runs in a clone of the module with go, make and pg-test.sh
// replaced by stubs that write what they were asked to run.
func checkRun(t *testing.T, args ...string) (log string, err error) {
	t.Helper()
	realGo, lerr := exec.LookPath("go")
	if lerr != nil {
		t.Fatal(lerr)
	}
	check, rerr := os.ReadFile("../check")
	if rerr != nil {
		t.Fatal(rerr)
	}
	script, rerr := os.ReadFile("affected-packages.sh")
	if rerr != nil {
		t.Fatal(rerr)
	}
	gosec, rerr := os.ReadFile("../.forgejo/gosec.sh")
	if rerr != nil {
		t.Fatal(rerr)
	}
	dir, bin, logFile := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "log")
	write(t, dir, module)
	write(t, dir, map[string]string{
		"check":                        string(check),
		"scripts/affected-packages.sh": string(script),
		".forgejo/gosec.sh":            string(gosec),
		"scripts/pg-test.sh":           "echo \"pg-test.sh $*\" >>\"$LOG\"\n",
		"Dockerfile.runner":            "ARG GOSEC_VERSION=0\n",
		"Dockerfile.sandbox":           "ARG GOTESTSUM_VERSION=1.13.0\n",
	})
	write(t, bin, map[string]string{
		"go":   "[ \"$1\" = list ] && exec " + realGo + " \"$@\"\necho \"go $1\" >>\"$LOG\"\n",
		"make": "echo \"make $*\" >>\"$LOG\"\n",
	})
	for _, f := range []string{"scripts/affected-packages.sh", ".forgejo/gosec.sh", "scripts/pg-test.sh"} {
		os.Chmod(filepath.Join(dir, f), 0o755)
	}
	os.Chmod(filepath.Join(bin, "go"), 0o755)
	os.Chmod(filepath.Join(bin, "make"), 0o755)
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	if len(args) == 0 || args[0] != "no-origin" {
		git(t, dir, "update-ref", "refs/remotes/origin/HEAD", "HEAD")
	} else {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "with-gotestsum" {
		write(t, bin, map[string]string{"gotestsum": "echo \"gotestsum $*\" >>\"$LOG\"\n"})
		if err := os.Chmod(filepath.Join(bin, "gotestsum"), 0o755); err != nil {
			t.Fatal(err)
		}
		args = args[1:]
	}
	write(t, dir, map[string]string{"a/a.go": "package a\n\nconst X = 1\n"})
	cmd := exec.Command("bash", append([]string{"check"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "LOG="+logFile, "TMPDIR="+t.TempDir(), "CHECK_REPORT=r.xml")
	out, err := cmd.CombinedOutput()
	raw, _ := os.ReadFile(logFile)
	if err != nil {
		t.Logf("bash check %v: %v\n%s", args, err, out)
	}
	return string(raw), err
}

func TestCheckModes(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{nil, "go vet\ngo run\nmake test\n"},
		{[]string{"static"}, "go vet\ngo run\n"},
		{[]string{"changed"}, "go vet\ngo run\npg-test.sh go test -p 2 -timeout 30m example.com/m/a example.com/m/b example.com/m/c\n"},
	} {
		log, err := checkRun(t, tt.args...)
		if err != nil {
			t.Fatalf("bash check %v: %v", tt.args, err)
		}
		if log != tt.want {
			t.Errorf("bash check %v ran %q, want %q", tt.args, log, tt.want)
		}
	}
}

// checkRun(t, "with-gotestsum", "test", "./a", "TestA/sub") logs
// "pg-test.sh gotestsum --junitfile r.xml -- -count=1 -timeout 30m -run ^TestA$/^sub$ ./a\n";
// with no gotestsum on PATH (t.Skip when exec.LookPath("gotestsum") finds
// the host's), checkRun(t, "test", "./a") logs
// "pg-test.sh go run gotest.tools/gotestsum@v1.13.0 --junitfile r.xml -- -count=1 -timeout 30m ./a\n".
func TestCheckTestRunsTheImagesGotestsum(t *testing.T) {
	log, err := checkRun(t, "with-gotestsum", "test", "./a", "TestA/sub")
	if err != nil {
		t.Fatal(err)
	}
	if want := "pg-test.sh gotestsum --junitfile r.xml -- -count=1 -timeout 30m -run ^TestA$/^sub$ ./a\n"; log != want {
		t.Errorf("bash check test with gotestsum ran %q, want %q", log, want)
	}
	if _, err := exec.LookPath("gotestsum"); err == nil {
		t.Skip("the host's PATH carries gotestsum, so check cannot fall back to go run")
	}
	log, err = checkRun(t, "test", "./a")
	if err != nil {
		t.Fatal(err)
	}
	if want := "pg-test.sh go run gotest.tools/gotestsum@v1.13.0 --junitfile r.xml -- -count=1 -timeout 30m ./a\n"; log != want {
		t.Errorf("bash check test without gotestsum ran %q, want %q", log, want)
	}
}

func TestCheckChangedNeedsMain(t *testing.T) {
	log, err := checkRun(t, "no-origin", "changed")
	if err == nil {
		t.Fatal("bash check changed passed without origin/HEAD")
	}
	if strings.Contains(log, "pg-test.sh") {
		t.Errorf("bash check changed tested without a base: %q", log)
	}
}

// gosecRun runs .forgejo/gosec.sh in a fresh copy of the module, with
// extra files beside it, and a gosec stub that logs where it ran and its
// arguments, prints a line per checked file and one more to stderr, and
// exits with code. It answers the log, the script's stderr and its error.
func gosecRun(t *testing.T, code int, extra map[string]string) (log, stderr string, err error) {
	t.Helper()
	script, aerr := filepath.Abs("../.forgejo/gosec.sh")
	if aerr != nil {
		t.Fatal(aerr)
	}
	dir, bin, logFile := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "log")
	write(t, dir, module)
	write(t, dir, extra)
	write(t, bin, map[string]string{"gosec": fmt.Sprintf("echo \"$PWD $*\" >>\"$LOG\"\n"+
		"echo '[gosec] 2026/11/24 05:00:00 Checking file: '\"$PWD\"'/a/a.go' >&2\n"+
		"echo '[gosec] 2026/11/24 05:00:00 Import directory: '\"$PWD\"'/a' >&2\nexit %d\n", code)})
	if err := os.Chmod(filepath.Join(bin, "gosec"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, filepath.Join(bin, "gosec"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LOG="+logFile)
	var errOut strings.Builder
	cmd.Stderr = &errOut
	err = cmd.Run()
	raw, _ := os.ReadFile(logFile)
	return string(raw), errOut.String(), err
}

// gosec runs from a short link to the checkout, inside a directory made for
// the run and removed when it ends, so go/packages never splits a
// package's file list into parts. It checks the packages go list names,
// each by its directory through the link, and no directory go list leaves
// out, as a worktree under .worktrees. Its line per checked file is left
// out of the output, its other lines and its exit code are kept.
func TestGosecRunsFromAShortLink(t *testing.T) {
	log, stderr, err := gosecRun(t, 0, map[string]string{".worktrees/w/w.go": "package w\n"})
	if err != nil {
		t.Fatalf("gosec.sh: %v\n%s", err, stderr)
	}
	m := regexp.MustCompile(`^(/tmp/gosec\.[A-Za-z0-9]{6})/s (.*)\n$`).FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("gosec ran as %q, want from /tmp/gosec.XXXXXX/s", log)
	}
	tmp, link := m[1], m[1]+"/s"
	if want := link + "/a " + link + "/b " + link + "/c " + link + "/d"; m[2] != want {
		t.Errorf("gosec checked %q, want the packages' directories %q", m[2], want)
	}
	if _, lerr := os.Lstat(tmp); !errors.Is(lerr, os.ErrNotExist) {
		t.Errorf("%s left behind: %v", tmp, lerr)
	}
	if strings.Contains(stderr, "Checking file") || !strings.Contains(stderr, "Import directory: "+link+"/a") {
		t.Errorf("stderr = %q, want the import line of the link and no line per file", stderr)
	}
	if _, _, err := gosecRun(t, 1, nil); err == nil {
		t.Error("gosec.sh passed where gosec failed")
	}
}

// A package whose file list from the link passes go/packages' 16383
// characters is refused before gosec runs: 70 files of 230-character
// names list more than that. The refusal names the package, its size and
// the action.
func TestGosecRefusesAPackagePastTheLimit(t *testing.T) {
	long := map[string]string{}
	for i := range 70 {
		long[fmt.Sprintf("p/%03d%s.go", i, strings.Repeat("x", 227))] = "package p\n"
	}
	log, stderr, err := gosecRun(t, 0, long)
	if err == nil {
		t.Fatal("gosec.sh passed with a package past the limit")
	}
	if log != "" {
		t.Errorf("gosec ran: %q", log)
	}
	if !regexp.MustCompile(`^package example\.com/m/p lists 1[0-9]{4} characters of file paths from /tmp/gosec\.[A-Za-z0-9]{6}/s, past go/packages' 16383: ` +
		`gosec would type-check it in parts\. Split the package\.\n$`).MatchString(stderr) {
		t.Errorf("stderr = %q, want the refusal of package example.com/m/p alone, with its size and the action", stderr)
	}
}

// Every gosec run goes through .forgejo/gosec.sh, which no agent branch may
// change, from the checkout: bash check, pr.yml's security job and main's
// CI.
func TestEveryGosecRunGoesThroughTheScript(t *testing.T) {
	for _, f := range []string{"../check", "../.forgejo/workflows/pr.yml", "../.forgejo/workflows/ci.yml"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if body := string(raw); !strings.Contains(body, ".forgejo/gosec.sh ") || strings.Contains(body, "scripts/gosec.sh") ||
			regexp.MustCompile(`gosec(@v\S+"?)? +(-\S+ +)*\./\.\.\.`).MatchString(body) {
			t.Errorf("%s runs gosec past .forgejo/gosec.sh", f)
		}
	}
	if _, err := os.Stat("gosec.sh"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scripts/gosec.sh exists: %v", err)
	}
}

// ci-volume-use.sh reads each CI volume's disk use in MiB, each from its own
// path: a volume holding an n MiB file reads n or n+1 (du rounds the
// directory's own blocks up), and an absent one reads 0.
func TestCIVolumeUseReadsEachVolume(t *testing.T) {
	root := t.TempDir()
	sizes := map[string]int{"workspace": 3, "home/runner": 5, "modcache": 7, "cache": 9}
	for dir, mib := range sizes {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "blob"), make([]byte, mib<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "ci-volume-use.sh")
	cmd.Env = append(os.Environ(), "CI_VOLUME_ROOT="+root)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^ci-volume-use workspace=([0-9]+) home=([0-9]+) tmp=0 modcache=([0-9]+) cache=([0-9]+)\n$`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("ci-volume-use.sh printed %q, want every volume read and tmp, which is absent, 0", out)
	}
	for i, dir := range []string{"workspace", "home/runner", "modcache", "cache"} {
		if want := sizes[dir]; m[i+1] != strconv.Itoa(want) && m[i+1] != strconv.Itoa(want+1) {
			t.Errorf("%s read %s MiB, want %d or %d", dir, m[i+1], want, want+1)
		}
	}
}

// pgRun starts pg-test.sh on the shell command under base as TMPDIR, in a
// process group of its own, once the command has printed its first line.
func pgRun(t *testing.T, base, command string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command("bash", "pg-test.sh", "sh", "-c", command)
	cmd.Env = append(os.Environ(), "TMPDIR="+base)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatalf("pg-test.sh %q printed no line: %v", command, err)
	}
	return cmd, stdin
}

// pgDirs are the directories pg-test.sh left under base.
func pgDirs(t *testing.T, base string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(base, "pg-test.*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// alive reports whether process pid runs: it exists and is no zombie.
func alive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// A run killed outright, as a tool's timeout kills its process group,
// skips its trap: its server runs on and its directory stays. The next run
// ends both, and leaves a run still going alone.
func TestPGTestEndsTheServersOfKilledRuns(t *testing.T) {
	if _, err := os.Stat("/usr/lib/postgresql/17/bin/initdb"); err != nil {
		t.Skip("PostgreSQL 17 is not installed")
	}
	base, err := os.MkdirTemp("", "pgt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if os.Geteuid() == 0 {
		// run as root, the script runs the server as nobody
		if err := os.Chmod(base, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	killed, _ := pgRun(t, base, "echo up; exec sleep 600")
	if err := syscall.Kill(-killed.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed.Wait()
	left := pgDirs(t, base)
	if len(left) != 1 || !strings.HasPrefix(filepath.Base(left[0]), fmt.Sprintf("pg-test.%d.", killed.Process.Pid)) {
		t.Fatalf("directories after the kill = %v, want one named pg-test.%d.*", left, killed.Process.Pid)
	}
	pidFile, err := os.ReadFile(filepath.Join(left[0], "data", "postmaster.pid"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := strconv.Atoi(strings.SplitN(string(pidFile), "\n", 2)[0])
	if err != nil || !alive(server) {
		t.Fatalf("the killed run's server %q is not running: %v", pidFile, err)
	}

	live, stdin := pgRun(t, base, "echo up; read line || true")
	next := exec.Command("bash", "pg-test.sh", "true")
	next.Env = append(os.Environ(), "TMPDIR="+base)
	if out, err := next.CombinedOutput(); err != nil {
		t.Fatalf("the next run: %v\n%s", err, out)
	}
	if alive(server) {
		t.Errorf("the killed run's server %d still runs after the next run", server)
	}
	if dirs := pgDirs(t, base); len(dirs) != 1 || !strings.HasPrefix(filepath.Base(dirs[0]), fmt.Sprintf("pg-test.%d.", live.Process.Pid)) {
		t.Errorf("directories after the next run = %v, want the live run's pg-test.%d.* alone", dirs, live.Process.Pid)
	}

	stdin.Close()
	if err := live.Wait(); err != nil {
		t.Fatalf("the live run: %v", err)
	}
	if dirs := pgDirs(t, base); len(dirs) != 0 {
		t.Errorf("directories after every run ended = %v, want none", dirs)
	}
}
