package reproducible

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type dockerfileStage struct {
	name string
	args map[string]string
	envs map[string]string
	runs []string
	// lines holds the stage's instructions after FROM in order, for the
	// tests that need one to come before another.
	lines []string
}

func logicalLines(text string) []string {
	var out []string
	var buf strings.Builder
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(strings.TrimSpace(strings.TrimSuffix(line, "\\")))
		if !strings.HasSuffix(line, "\\") {
			out = append(out, buf.String())
			buf.Reset()
		}
	}
	return out
}

func parseDockerfile(t *testing.T, path string) []*dockerfileStage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var stages []*dockerfileStage
	var cur *dockerfileStage
	for _, line := range logicalLines(string(raw)) {
		keyword, rest, _ := strings.Cut(line, " ")
		if cur != nil {
			cur.lines = append(cur.lines, line)
		}
		switch strings.ToUpper(keyword) {
		case "FROM":
			cur = &dockerfileStage{args: map[string]string{}, envs: map[string]string{}}
			fields := strings.Fields(rest)
			for i, f := range fields {
				if strings.EqualFold(f, "AS") && i+1 < len(fields) {
					cur.name = fields[i+1]
				}
			}
			stages = append(stages, cur)
		case "ARG", "ENV":
			if cur == nil {
				if keyword == "ENV" {
					t.Fatalf("%s: ENV before any FROM", path)
				}
				continue // ARG in the global scope is not stage-scoped
			}
			for _, f := range strings.Fields(rest) {
				if k, v, ok := strings.Cut(f, "="); ok {
					if keyword == "ARG" {
						cur.args[k] = v
					} else {
						cur.envs[k] = v
					}
				} else if keyword == "ARG" {
					cur.args[f] = ""
				}
			}
		case "RUN":
			if cur == nil {
				t.Fatalf("%s: RUN before any FROM", path)
			}
			cur.runs = append(cur.runs, rest)
		}
	}
	for i, s := range stages {
		if s.name != "" {
			continue
		}
		if i == len(stages)-1 {
			s.name = "final"
		} else {
			s.name = fmt.Sprintf("unnamed-%d", i)
		}
	}
	return stages
}

func runsGoBuild(s *dockerfileStage) bool {
	for _, run := range s.runs {
		if strings.Contains(run, "go build") {
			return true
		}
	}
	return false
}

func TestDockerfilesPinSourceDateEpochInEveryBuildStage(t *testing.T) {
	// Hand-derived from the three Dockerfiles as they stand: these and only
	// these stages run `go build`.
	want := []struct {
		file   string
		stages []string
	}{
		{"Dockerfile", []string{"build"}},
		{"Dockerfile.sandbox", []string{"build"}},
		{"Dockerfile.runner", []string{"final"}},
	}
	for _, w := range want {
		t.Run(w.file, func(t *testing.T) {
			stages := parseDockerfile(t, filepath.Join("..", "..", w.file))
			var got []string
			for _, s := range stages {
				if runsGoBuild(s) {
					got = append(got, s.name)
				}
			}
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running go build = %v, want %v; a moved or added go build must update this list and carry the SOURCE_DATE_EPOCH pin", w.file, got, w.stages)
			}
			for _, s := range stages {
				if !runsGoBuild(s) {
					continue
				}
				if _, ok := s.args["SOURCE_DATE_EPOCH"]; !ok {
					t.Errorf("%s stage %s: no ARG SOURCE_DATE_EPOCH", w.file, s.name)
				}
				env, ok := s.envs["SOURCE_DATE_EPOCH"]
				if !ok || env != "$SOURCE_DATE_EPOCH" {
					t.Errorf(`%s stage %s: SOURCE_DATE_EPOCH not exported to the build environment, want ENV SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH, got %q`, w.file, s.name, env)
				}
				for _, run := range s.runs {
					if !strings.Contains(run, "go build") {
						continue
					}
					if !strings.Contains(run, "-trimpath") {
						t.Errorf("%s stage %s: go build without -trimpath: %s", w.file, s.name, run)
					}
					if !strings.Contains(run, "-buildvcs=false") {
						t.Errorf("%s stage %s: go build without -buildvcs=false: %s", w.file, s.name, run)
					}
				}
			}
		})
	}
}

// stagesWhere names the stages of path that match.
func stagesWhere(t *testing.T, path string, match func(*dockerfileStage) bool) ([]*dockerfileStage, []string) {
	t.Helper()
	var stages []*dockerfileStage
	var names []string
	for _, s := range parseDockerfile(t, path) {
		if match(s) {
			stages = append(stages, s)
			names = append(names, s.name)
		}
	}
	return stages, names
}

var goCall = regexp.MustCompile(`(^|[\s;&|/])go\s+[a-z]`)

const telemetryOff = `printf off > "$HOME/.config/go/telemetry/mode"`

func TestDockerfilesTurnGoTelemetryOffBeforeTheFirstGoCall(t *testing.T) {
	// Hand-derived from the three Dockerfiles: these and only these stages
	// run the go command (go mod download, go build, go version).
	want := []struct {
		file   string
		stages []string
	}{
		{"Dockerfile", []string{"build"}},
		{"Dockerfile.sandbox", []string{"build", "final"}},
		{"Dockerfile.runner", []string{"final"}},
	}
	for _, w := range want {
		t.Run(w.file, func(t *testing.T) {
			stages, got := stagesWhere(t, filepath.Join("..", "..", w.file), func(s *dockerfileStage) bool {
				return goCall.MatchString(strings.Join(s.runs, "\n"))
			})
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running go = %v, want %v; a stage that adds a go call must turn telemetry off first", w.file, got, w.stages)
			}
			for _, s := range stages {
				runs := strings.Join(s.runs, "\n")
				first := goCall.FindStringIndex(runs)[0]
				if off := strings.Index(runs, telemetryOff); off < 0 || off > first {
					t.Errorf("%s stage %s: the first go call runs before %s: its telemetry counters carry the build's date", w.file, s.name, telemetryOff)
				}
			}
		})
	}
}

func TestDockerfilesDropTheAptInstallClocks(t *testing.T) {
	// Hand-derived from the two-build proof's layer diff of 2026-10-14: the
	// only files of an apt-get install layer that differ between two builds.
	// The gateway's install of git and ca-certificates writes no snakeoil
	// pair (ssl-cert's, which postgresql-common pulls in) and runs no
	// update-alternatives.
	clocks := []string{
		"/var/cache/ldconfig/aux-cache",
		"/var/log/apt/history.log",
		"/var/log/apt/term.log",
		"/var/log/dpkg.log",
	}
	withSSLCert := []string{
		"/etc/ssl/certs/ssl-cert-snakeoil.pem",
		"/etc/ssl/private/ssl-cert-snakeoil.key",
		"/var/cache/ldconfig/aux-cache",
		"/var/log/alternatives.log",
		"/var/log/apt/history.log",
		"/var/log/apt/term.log",
		"/var/log/dpkg.log",
	}
	want := []struct {
		file   string
		stages []string
		clocks []string
	}{
		{"Dockerfile", []string{"final"}, clocks},
		{"Dockerfile.sandbox", []string{"base"}, withSSLCert},
		{"Dockerfile.runner", []string{"final"}, withSSLCert},
	}
	for _, w := range want {
		t.Run(w.file, func(t *testing.T) {
			stages, got := stagesWhere(t, filepath.Join("..", "..", w.file), func(s *dockerfileStage) bool {
				return strings.Contains(strings.Join(s.runs, "\n"), "apt-get install")
			})
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running apt-get install = %v, want %v", w.file, got, w.stages)
			}
			for _, s := range stages {
				for _, run := range s.runs {
					if !strings.Contains(run, "apt-get install") {
						continue
					}
					cmds := strings.Split(run, "&&")
					last := strings.Fields(cmds[len(cmds)-1])
					if len(last) == 0 || last[0] != "rm" || !reflect.DeepEqual(last[1:], w.clocks) {
						t.Errorf("%s stage %s: the apt-get install step ends with %q, want rm %v", w.file, s.name, strings.Join(last, " "), w.clocks)
					}
				}
			}
		})
	}
}

func TestDockerfilesDateUseraddByTheEpoch(t *testing.T) {
	// Hand-derived: useradd writes the day of the user's creation into
	// /etc/shadow, from SOURCE_DATE_EPOCH when the step sees it. These
	// stages add a user.
	want := []struct {
		file   string
		stages []string
	}{
		{"Dockerfile", nil},
		{"Dockerfile.sandbox", []string{"base"}},
		{"Dockerfile.runner", []string{"final"}},
	}
	for _, w := range want {
		t.Run(w.file, func(t *testing.T) {
			stages, got := stagesWhere(t, filepath.Join("..", "..", w.file), func(s *dockerfileStage) bool {
				return strings.Contains(strings.Join(s.runs, "\n"), "useradd ")
			})
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running useradd = %v, want %v", w.file, got, w.stages)
			}
			for _, s := range stages {
				arg, add := -1, -1
				for i, line := range s.lines {
					if arg < 0 && (line == "ARG SOURCE_DATE_EPOCH" || strings.HasPrefix(line, "ARG SOURCE_DATE_EPOCH=")) {
						arg = i
					}
					if add < 0 && strings.HasPrefix(line, "RUN ") && strings.Contains(line, "useradd ") {
						add = i
					}
				}
				if arg < 0 || arg > add {
					t.Errorf("%s stage %s: useradd runs without ARG SOURCE_DATE_EPOCH before it, so /etc/shadow carries the build's day", w.file, s.name)
				}
			}
		})
	}
}

// warmStep is the final stage of Dockerfile.runner and its one RUN that
// runs go test: the step that fills the build cache the image ships.
func warmStep(t *testing.T) (*dockerfileStage, string) {
	t.Helper()
	stages := parseDockerfile(t, filepath.Join("..", "..", "Dockerfile.runner"))
	final := stages[len(stages)-1]
	var warm []string
	for _, run := range final.runs {
		if strings.Contains(run, "go test") {
			warm = append(warm, run)
		}
	}
	if len(warm) != 1 {
		t.Fatalf("Dockerfile.runner: %d RUNs run go test, want the one warm step", len(warm))
	}
	return final, warm[0]
}

func TestRunnerWarmCacheHoldsNoTestResultsAndNoSource(t *testing.T) {
	final, warm := warmStep(t)
	// -count=1 caches no test result, so no cache entry's time is ever read
	// (its one reader beyond display is the go clean -testcache expiry);
	// -run '^$' runs no test.
	test := regexp.MustCompile(`go test [^&]*`).FindString(warm)
	for _, flag := range []string{"-count=1", "-run '^$'"} {
		if !strings.Contains(test, " "+flag+" ") {
			t.Errorf("Dockerfile.runner warm step: %q runs without %s", test, flag)
		}
	}
	// the module and every top-level tree holding Go files are bind-mounted
	// under /tmp/src, and nothing else: a change outside them (k8s/, docs)
	// keeps the layer; ci.yml's RUNNER_INPUTS, the runner image's epoch,
	// name the same paths
	mounts := regexp.MustCompile(`--mount=type=bind,source=([^,\s]+),target=/tmp/src/([^,\s]+) `).FindAllStringSubmatch(warm, -1)
	if n := strings.Count(warm, "--mount="); n != len(mounts) {
		t.Errorf("Dockerfile.runner warm step: %d mounts, %d of them binds of a source path at /tmp/src/<the same path>: %s", n, len(mounts), warm)
	}
	var mounted []string
	for _, m := range mounts {
		if m[1] != m[2] {
			t.Errorf("Dockerfile.runner warm step: %s is mounted at /tmp/src/%s, want /tmp/src/%s", m[1], m[2], m[1])
		}
		mounted = append(mounted, m[1])
	}
	want := append([]string{"go.mod", "go.sum"}, goTrees(t)...)
	sort.Strings(mounted)
	sort.Strings(want)
	if !reflect.DeepEqual(mounted, want) {
		t.Errorf("Dockerfile.runner warm step mounts %v, want the module and every tree holding Go files: %v", mounted, want)
	}
	ci, err := os.ReadFile(filepath.Join("..", "..", ".forgejo", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^\s*RUNNER_INPUTS: (.*)$`).FindSubmatch(ci)
	if line == nil {
		t.Fatal("ci.yml: no RUNNER_INPUTS")
	}
	inputs := strings.Fields(string(line[1]))
	sort.Strings(inputs)
	wantInputs := append([]string{".dockerignore", "Dockerfile.runner"}, want...)
	sort.Strings(wantInputs)
	if !reflect.DeepEqual(inputs, wantInputs) {
		t.Errorf("ci.yml RUNNER_INPUTS = %v, want Dockerfile.runner, .dockerignore and the warm step's mounts: %v", inputs, wantInputs)
	}
	for _, line := range final.lines {
		fields := strings.Fields(line)
		if strings.ToUpper(fields[0]) != "COPY" {
			continue
		}
		for _, f := range fields[1 : len(fields)-1] {
			if f == "." || f == "./" {
				t.Errorf("Dockerfile.runner: %q copies the source into a layer", line)
			}
		}
	}
}

func TestRunnerWarmCacheTakesTheCommitsClock(t *testing.T) {
	_, warm := warmStep(t)
	const set = "cache=$(go env GOCACHE)"
	i := strings.Index(warm, set)
	if i < 0 {
		t.Fatalf("Dockerfile.runner warm step: no %s to rewrite the cache under: %s", set, warm)
	}
	if goCall.MatchString(warm[i+len(set):]) {
		t.Fatalf("Dockerfile.runner warm step: a go call after the rewrite writes new clocks: %s", warm[i:])
	}
	// The step's rewrite, run on a cache of two action entries, an output
	// and trim.txt as go build left them at 1792006198 (2026-10-14T19:29:58Z),
	// with the commit's time 1792007035.
	dir := t.TempDir()
	const (
		id   = "00030457eb1872ca6ac1b6b90f53fa7b5519a12ac3983ff0a266486bc870b245"
		out  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		size = "                1234"
	)
	files := map[string][2]string{
		"00/" + id + "-a":  {"v1 " + id + " " + out + " " + size + "  1792006198080880757\n", "v1 " + id + " " + out + " " + size + "  1792007035000000000\n"},
		"ff/" + out + "-a": {"v1 " + out + " " + id + " " + size + "      998244353000001\n", "v1 " + out + " " + id + " " + size + "  1792007035000000000\n"},
		"e3/" + out + "-d": {"output bytes, not an entry: 12345678901234567890\n", "output bytes, not an entry: 12345678901234567890\n"},
		"trim.txt":         {"1792006210", "1792007035"},
	}
	for name, f := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(f[0]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", "cache='"+dir+"'"+warm[i+len(set):])
	cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=1792007035")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the warm step's rewrite: %v: %s", err, b)
	}
	for name, f := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != f[1] {
			t.Errorf("%s after the rewrite = %q, want %q", name, got, f[1])
		}
	}
}

// Go's module index keys each package of the module by its files' names,
// sizes and mtimes, and a checkout sets the mtimes to its own clock: main's
// runs 1229 and 1231 built one source into warm layers that differed in
// the 22 index entries of the module's packages, and in nothing else.
func TestRunnerWarmBuildReadsTheSourceAtTheCommitsClock(t *testing.T) {
	_, warm := warmStep(t)
	const cp, into = "cp -r /tmp/src /tmp/warm ", " && cd /tmp/warm && "
	start, end := strings.Index(warm, cp), strings.Index(warm, into)
	if start < 0 || end < start {
		t.Fatalf("Dockerfile.runner warm step: no %q before %q: %s", cp, into, warm)
	}
	gos := goCall.FindAllStringIndex(warm, -1)
	if gos[0][0] < end {
		t.Fatalf("Dockerfile.runner warm step: a go call before the build enters its copy: %s", warm)
	}
	set := strings.Index(warm, "cache=$(go env GOCACHE)")
	if last := gos[len(gos)-1][1]; set < last || !strings.Contains(warm[last:set], " && cd / && rm -r /tmp/warm && ") {
		t.Errorf("Dockerfile.runner warm step: the copy is not removed between the last go call and the cache rewrite, so a layer holds the source: %s", warm)
	}
	// The step's copy, run on a checkout dated 1792006198
	// (2026-10-14T19:29:58Z) with the commit's time 1792007035.
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "warm")
	files := map[string]string{"go.mod": "module example.com/m\n", "internal/p/p.go": "package p\n"}
	checkout := time.Unix(1792006198, 0)
	for name, body := range files {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, checkout, checkout); err != nil {
			t.Fatal(err)
		}
	}
	prep := strings.NewReplacer("/tmp/src", src, "/tmp/warm", dst).Replace(warm[start:end])
	cmd := exec.Command("sh", "-c", prep)
	cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=1792007035")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the warm step's copy: %v: %s", err, b)
	}
	for name, body := range files {
		path := filepath.Join(dst, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != body || info.ModTime().Unix() != 1792007035 {
			t.Errorf("%s in the copy = %q dated %d, want %q dated 1792007035", name, got, info.ModTime().Unix(), body)
		}
	}
}

// One Go builds what ships and what CI and a local run test with: the
// golang base of every Dockerfile names the same exact version, each pinned
// by digest, and go.mod's toolchain line names it too. Hand-derived from
// the four files: go1.26.9, alpine for the gateway's and the sandbox's
// build stages (the sandbox's twice: its npm-cache fetch stage shares the
// store's copy), Debian for the runner's toolchain stage.
func TestDockerfilesBuildWithOneGo(t *testing.T) {
	mod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^toolchain go(\S+)$`).FindAllStringSubmatch(string(mod), -1); len(m) != 1 || m[0][1] != "1.26.9" {
		t.Errorf("go.mod: toolchain lines %v, want one at go1.26.9", m)
	}
	from := regexp.MustCompile(`(?m)^FROM \$\{UPSTREAM\}docker\.io/library/golang:([0-9.]+)(-alpine)?@sha256:[0-9a-f]{64}\b`)
	want := map[string][]string{"Dockerfile": {"1.26.9"}, "Dockerfile.sandbox": {"1.26.9", "1.26.9"}, "Dockerfile.runner": {"1.26.9"}}
	for file, versions := range want {
		b, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatal(err)
		}
		m := from.FindAllStringSubmatch(string(b), -1)
		got := make([]string, len(m))
		for i, g := range m {
			got[i] = g[1]
		}
		if !reflect.DeepEqual(got, versions) {
			t.Errorf("%s: golang bases %v, want %v pinned by digest", file, got, versions)
		}
	}
}

// goTrees are the repository's top-level directories that hold a .go
// file, hidden ones aside.
func goTrees(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var trees []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found := false
		filepath.WalkDir(filepath.Join(root, e.Name()), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") {
				found = true
				return filepath.SkipAll
			}
			return err
		})
		if found {
			trees = append(trees, e.Name())
		}
	}
	return trees
}

// Every file a Dockerfile downloads with ADD carries its checksum: the
// bytes behind a URL can change, and the image with them.
func TestDockerfileDownloadsCarryAChecksum(t *testing.T) {
	for _, f := range []string{"Dockerfile", "Dockerfile.sandbox", "Dockerfile.runner"} {
		for _, s := range parseDockerfile(t, filepath.Join("..", "..", f)) {
			for _, line := range s.lines {
				fields := strings.Fields(line)
				if len(fields) < 2 || strings.ToUpper(fields[0]) != "ADD" || !strings.Contains(line, "://") {
					continue
				}
				pinned := false
				for _, flag := range fields[1:] {
					if !strings.HasPrefix(flag, "--") {
						break
					}
					pinned = pinned || strings.HasPrefix(flag, "--checksum=sha256:")
				}
				if !pinned {
					t.Errorf("%s: %q downloads without --checksum", f, line)
				}
			}
		}
	}
}

// The warm step dates its source copy and rewrites the cache's times with
// SOURCE_DATE_EPOCH, and pr.yml's image check and the nightly build the
// runner without passing it: its ARG carries a number as default, or
// "touch -d @" fails the build (main's nightly, run 1241).
func TestRunnerEpochHasADefault(t *testing.T) {
	final, _ := warmStep(t)
	v, ok := final.args["SOURCE_DATE_EPOCH"]
	if !ok {
		t.Fatal("Dockerfile.runner's final stage: no ARG SOURCE_DATE_EPOCH")
	}
	if _, err := strconv.ParseInt(v, 10, 64); err != nil {
		t.Errorf("Dockerfile.runner: ARG SOURCE_DATE_EPOCH defaults to %q, want a number a build without the arg can use", v)
	}
}

// The ARGs that date or pin every apt source: one Debian snapshot for
// every image, and one version of each PGDG package for the two that
// install PostgreSQL, as one Go builds them all.
var aptPins = []string{"DEBIAN_SNAPSHOT", "POSTGRESQL_VERSION", "LIBPQ_VERSION", "POSTGRESQL_COMMON_VERSION"}

const pgdgArchive = "https://apt-archive.postgresql.org/pub/repos/apt"

var (
	snapshotDate = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
	debSource    = regexp.MustCompile(`"deb (?:\[[^\]]*\] )?(\S+) (\S+)`)
	snapshotURL  = regexp.MustCompile(`^http://snapshot\.debian\.org/archive/(debian|debian-security)/\$\{DEBIAN_SNAPSHOT\}$`)
	installArgs  = regexp.MustCompile(`apt-get install -y --no-install-recommends ((?:[^&]|&[^&])*)`)
	versionArg   = regexp.MustCompile(`^\$\{[A-Z_]+_VERSION\}$`)
)

// Every apt source an image installs from is dated or version-pinned
// (roadmap 242): a cache miss on a later day installs the same packages.
// Debian's come from snapshot.debian.org at DEBIAN_SNAPSHOT, the base
// image's undated deb.debian.org source removed before the first update;
// PGDG's from its archive, pinned at priority 1 so that apt takes from it
// only a package named with its version, and every PostgreSQL package is
// named so. apt retries a slow snapshot. Hand-derived from the three
// Dockerfiles: these stages run apt-get install.
func TestDockerfilesTakeAptFromDatedOrVersionPinnedSources(t *testing.T) {
	want := []struct {
		file   string
		stages []string
	}{
		{"Dockerfile", []string{"final"}},
		{"Dockerfile.sandbox", []string{"base"}},
		{"Dockerfile.runner", []string{"final"}},
	}
	pins := map[string]string{}
	for _, w := range want {
		t.Run(w.file, func(t *testing.T) {
			stages, got := stagesWhere(t, filepath.Join("..", "..", w.file), func(s *dockerfileStage) bool {
				return strings.Contains(strings.Join(s.runs, "\n"), "apt-get install")
			})
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running apt-get install = %v, want %v", w.file, got, w.stages)
			}
			for _, s := range stages {
				if !snapshotDate.MatchString(s.args["DEBIAN_SNAPSHOT"]) {
					t.Errorf("%s stage %s: ARG DEBIAN_SNAPSHOT=%q, want a snapshot.debian.org time such as 20261015T000000Z", w.file, s.name, s.args["DEBIAN_SNAPSHOT"])
				}
				for _, arg := range aptPins {
					v := s.args[arg]
					if arg != "DEBIAN_SNAPSHOT" && !strings.Contains(strings.Join(s.runs, "\n"), pgdgArchive) {
						continue
					}
					if v == "" {
						t.Errorf("%s stage %s: no ARG %s with a value, so its apt step reads it empty", w.file, s.name, arg)
						continue
					}
					if prev, ok := pins[arg]; ok && prev != v {
						t.Errorf("%s: ARG %s=%s, another image pins %s: one snapshot and one PostgreSQL for every image", w.file, arg, v, prev)
					}
					pins[arg] = v
				}
				for _, run := range s.runs {
					if !strings.Contains(run, "apt-get install") {
						continue
					}
					checkAptRun(t, w.file+" stage "+s.name, run)
				}
			}
		})
	}
}

func checkAptRun(t *testing.T, where, run string) {
	t.Helper()
	update := strings.Index(run, "apt-get update")
	if rm := strings.Index(run, "rm /etc/apt/sources.list.d/debian.sources "); rm < 0 || rm > update {
		t.Errorf("%s: the base image's undated debian.sources is not removed before the first apt-get update", where)
	}
	for _, conf := range []string{"Acquire::Retries", "Acquire::http::Timeout", "Acquire::https::Timeout", "Acquire::Check-Valid-Until \"false\""} {
		if i := strings.Index(run, "'"+conf); i < 0 || i > update {
			t.Errorf("%s: no %s in apt's configuration before the first apt-get update", where, conf)
		}
	}
	sources := debSource.FindAllStringSubmatch(run, -1)
	if len(sources) == 0 {
		t.Fatalf("%s: no deb source found in the apt step", where)
	}
	archive := false
	for _, m := range sources {
		switch {
		case snapshotURL.MatchString(m[1]):
		case m[1] == pgdgArchive && m[2] == "bookworm-pgdg-archive":
			archive = true
		default:
			t.Errorf("%s: apt source %s %s is neither snapshot.debian.org at ${DEBIAN_SNAPSHOT} nor PGDG's archive", where, m[1], m[2])
		}
	}
	if archive && !strings.Contains(run, `'Package: *' 'Pin: origin apt-archive.postgresql.org' 'Pin-Priority: 1' > /etc/apt/preferences.d/`) {
		t.Errorf("%s: PGDG's archive is not pinned at priority 1, so apt would take its newest version of a package not named with one", where)
	}
	installs := installArgs.FindAllStringSubmatch(run, -1)
	if len(installs) == 0 {
		t.Fatalf("%s: no apt-get install -y --no-install-recommends", where)
	}
	for _, m := range installs {
		for _, pkg := range strings.Fields(m[1]) {
			if !strings.HasPrefix(pkg, "postgresql") && !strings.HasPrefix(pkg, "libpq") {
				continue
			}
			name, version, _ := strings.Cut(pkg, "=")
			if !versionArg.MatchString(version) {
				t.Errorf("%s: %s installs without a pinned version ARG", where, name)
			}
		}
	}
}
