package reproducible

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
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
	clocks := []string{
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
	}{
		{"Dockerfile", nil},
		{"Dockerfile.sandbox", []string{"final"}},
		{"Dockerfile.runner", []string{"final"}},
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
					if len(last) == 0 || last[0] != "rm" || !reflect.DeepEqual(last[1:], clocks) {
						t.Errorf("%s stage %s: the apt-get install step ends with %q, want rm %v", w.file, s.name, strings.Join(last, " "), clocks)
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
		{"Dockerfile.sandbox", []string{"final"}},
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
	if !strings.HasPrefix(warm, "--mount=type=bind,target=/tmp/warm ") {
		t.Errorf("Dockerfile.runner warm step: the source is not bind-mounted at /tmp/warm, so a layer would hold it: %s", warm)
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

// One Go builds what ships and what CI tests with: the golang base of every
// Dockerfile names the same exact version, each pinned by digest. Hand-
// derived from the three files: go1.25.14, alpine for the gateway's and
// the sandbox's build stages, Debian for the runner's toolchain stage.
func TestDockerfilesBuildWithOneGo(t *testing.T) {
	from := regexp.MustCompile(`(?m)^FROM golang:([0-9.]+)(-alpine)?@sha256:[0-9a-f]{64}\b`)
	want := map[string]string{"Dockerfile": "1.25.14", "Dockerfile.sandbox": "1.25.14", "Dockerfile.runner": "1.25.14"}
	for file, version := range want {
		b, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatal(err)
		}
		m := from.FindAllStringSubmatch(string(b), -1)
		if len(m) != 1 || m[0][1] != version {
			t.Errorf("%s: golang bases %v, want one at %s pinned by digest", file, m, version)
		}
	}
}
