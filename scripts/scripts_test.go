package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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
	dir, bin, logFile := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "log")
	write(t, dir, module)
	write(t, dir, map[string]string{
		"check":                        string(check),
		"scripts/affected-packages.sh": string(script),
		"scripts/pg-test.sh":           "echo \"pg-test.sh $*\" >>\"$LOG\"\n",
		"Dockerfile.runner":            "ARG GOSEC_VERSION=0\n",
		"Dockerfile.sandbox":           "ARG GOTESTSUM_VERSION=1.13.0\n",
	})
	write(t, bin, map[string]string{
		"go":   "[ \"$1\" = list ] && exec " + realGo + " \"$@\"\necho \"go $1\" >>\"$LOG\"\n",
		"make": "echo \"make $*\" >>\"$LOG\"\n",
	})
	for _, f := range []string{"scripts/affected-packages.sh", "scripts/pg-test.sh"} {
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
// "pg-test.sh gotestsum --junitfile r.xml -- -count=1 -run ^TestA$/^sub$ ./a\n";
// with no gotestsum on PATH (t.Skip when exec.LookPath("gotestsum") finds
// the host's), checkRun(t, "test", "./a") logs
// "pg-test.sh go run gotest.tools/gotestsum@v1.13.0 --junitfile r.xml -- -count=1 ./a\n".
func TestCheckTestRunsTheImagesGotestsum(t *testing.T) {
	log, err := checkRun(t, "with-gotestsum", "test", "./a", "TestA/sub")
	if err != nil {
		t.Fatal(err)
	}
	if want := "pg-test.sh gotestsum --junitfile r.xml -- -count=1 -run ^TestA$/^sub$ ./a\n"; log != want {
		t.Errorf("bash check test with gotestsum ran %q, want %q", log, want)
	}
	if _, err := exec.LookPath("gotestsum"); err == nil {
		t.Skip("the host's PATH carries gotestsum, so check cannot fall back to go run")
	}
	log, err = checkRun(t, "test", "./a")
	if err != nil {
		t.Fatal(err)
	}
	if want := "pg-test.sh go run gotest.tools/gotestsum@v1.13.0 --junitfile r.xml -- -count=1 ./a\n"; log != want {
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

// ci-volume-use.sh reads each CI volume's disk use in MiB: a directory that
// does not exist reads 0, and one holding a 5 MiB file reads 5 or 6 (du
// rounds the directory's own blocks up).
func TestCIVolumeUseReadsEachVolume(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "workspace", "blob"), make([]byte, 5<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "ci-volume-use.sh")
	cmd.Env = append(os.Environ(), "CI_VOLUME_ROOT="+root)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^ci-volume-use workspace=([0-9]+) home=0 tmp=0 modcache=0 cache=0\n$`).FindStringSubmatch(string(out))
	if m == nil || (m[1] != "5" && m[1] != "6") {
		t.Fatalf("ci-volume-use.sh printed %q, want workspace=5 or 6 and every absent volume 0", out)
	}
}
