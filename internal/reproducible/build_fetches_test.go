package reproducible

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	// fetchURL picks the URL out of a fetch: https or http up to the
	// first space, quote or shell variable, so a $-interpolated path
	// keeps its host.
	fetchURL = regexp.MustCompile(`https?://[^\s"'` + "`" + `$]+`)
	// aptSourceURL is an apt sources entry's URL: deb, an optional
	// [key=value] block, then the archive. Roadmap 242's deliberate
	// exception: the builds' apt still reads Debian's dated snapshot and
	// PGDG's archive, which the store does not front.
	aptSourceURL   = regexp.MustCompile(`\bdeb\s+(?:\[[^\]]*\]\s*)?(https?://` + "[^\\s\"'`$]+" + `)`)
	aptArchiveHost = map[string]bool{"snapshot.debian.org": true, "apt-archive.postgresql.org": true}
	// ciMirrorValue reads the registry and cache mirrors ci.yml names: the
	// values of its env's CACHE and BUILDKIT.
	ciMirrorValue = regexp.MustCompile(`(?m)^\s+(CACHE|BUILDKIT): (\S+)`)
	// buildVMDenyList is the hosts the build VM's own rules refuse.
	buildVMDenyList = regexp.MustCompile(`(?s)buildVMUpstreamHosts\s*=\s*\[\]string\{([^}]*)\}`)
	quotedString    = regexp.MustCompile(`"([^"]+)"`)
)

// buildFetchHosts are the hosts a build-time fetch may name: the store's
// host and the registry and cache mirrors ci.yml itself names (roadmap
// 203, step 3). Nothing else answers a build.
func buildFetchHosts(t *testing.T) map[string]bool {
	t.Helper()
	storeHost, _, _ := strings.Cut(store, "/")
	hosts := map[string]bool{storeHost: true}
	ci, err := os.ReadFile(filepath.Join("..", "..", ".forgejo", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ciMirrorValue.FindAllStringSubmatch(string(ci), -1) {
		value := strings.TrimPrefix(strings.TrimPrefix(m[2], "https://"), "http://")
		host, _, _ := strings.Cut(value, "/")
		if host == "" {
			t.Fatalf("ci.yml: %s names no host", m[1])
		}
		hosts[host] = true
	}
	if len(hosts) < 2 {
		t.Fatal("ci.yml names no mirror beside the store")
	}
	return hosts
}

// buildFetchFiles holds where a build names its fetches: the three
// images' Dockerfiles, the workflow that builds them, and the build VM's
// cloud-init (internal/factoryd/buildvm.go).
func buildFetchFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range []string{
		"Dockerfile", "Dockerfile.runner", "Dockerfile.sandbox",
		filepath.Join(".forgejo", "workflows", "ci.yml"),
		filepath.Join("internal", "factoryd", "buildvm.go"),
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(raw)
	}
	return out
}

// outsideFetches names every build-time fetch in the file's lines that
// resolves beyond allowed: for a Dockerfile an ADD or RUN URL — the go
// module and npm restore paths among them — and a FROM that is neither
// scratch, a stage named above it, nor the store's ${UPSTREAM} copy,
// whose ARG default must be the store; for a workflow or Go file any URL
// at all. Each finding names the file and the URL.
func outsideFetches(name string, lines []string, allowed map[string]bool) []string {
	var out []string
	dockerfile := strings.HasPrefix(name, "Dockerfile")
	var args map[string]string
	if dockerfile {
		args = dockerfileARGs(lines)
	}
	stages := map[string]bool{"scratch": true}
	for _, line := range lines {
		if dockerfile {
			if m := fromLine.FindStringSubmatch(line); m != nil {
				if !stages[m[1]] && !strings.HasPrefix(m[1], "${UPSTREAM}") {
					out = append(out, fmt.Sprintf("%s: FROM %s is not the store's ${UPSTREAM} copy", name, m[1]))
				}
				if f := strings.Fields(line); len(f) == 4 && strings.EqualFold(f[2], "AS") {
					stages[f[3]] = true
				}
			}
			if !strings.HasPrefix(line, "ADD ") && !strings.HasPrefix(line, "RUN ") {
				continue
			}
		}
		aptArchive := map[string]bool{}
		for _, m := range aptSourceURL.FindAllStringSubmatch(line, -1) {
			aptArchive[m[1]] = true
		}
		for _, u := range fetchURL.FindAllString(line, -1) {
			parsed, err := url.Parse(u)
			host := ""
			if err == nil {
				host = parsed.Host
			}
			if aptArchive[u] && aptArchiveHost[host] {
				continue
			}
			if !allowed[host] {
				out = append(out, fmt.Sprintf("%s: fetch %s reaches %s, want the store or a mirror ci.yml names", name, u, host))
			}
		}
	}
	if dockerfile && args["UPSTREAM"] != store {
		out = append(out, fmt.Sprintf("%s: ARG UPSTREAM=%q, want the store's %q", name, args["UPSTREAM"], store))
	}
	return out
}

// Every build-time fetch the build files name resolves to the store's
// host or a registry or cache mirror or the factory's control plane
// ci.yml itself names (roadmap 203, step 3): every ADD, wget and curl
// URL, every FROM, the go module and npm restore paths — apt's dated
// snapshot and PGDG archive aside, roadmap 242's deliberate exception.
// Anything else names an upstream the store fronts — a build must not
// even name one, let alone reach it: the build VM's own rules deny those
// hosts (TestTheBuildVMDeniesEveryUpstreamFileHost).
func TestEveryBuildFetchResolvesToTheStoreOrAMirror(t *testing.T) {
	allowed := buildFetchHosts(t)
	for f, raw := range buildFetchFiles(t) {
		lines := strings.Split(raw, "\n")
		if strings.HasPrefix(f, "Dockerfile") {
			lines = logicalLines(raw)
		}
		for _, problem := range outsideFetches(f, lines, allowed) {
			t.Error(problem)
		}
	}
}

// The fetch guard names the file and the URL of everything outside the
// store and ci.yml's mirrors: an unpinned FROM, an ADD of github.com, a
// wget of nodejs.org, a workflow's curl of code.forgejo.org, a Go
// source's fetch of www.postgresql.org, and an ${UPSTREAM} that points
// anywhere but the store. Hand-derived fixtures; the store's own shapes
// stay silent.
func TestOutsideFetchesAreNamedWithTheirFileAndURL(t *testing.T) {
	allowed := map[string]bool{"git.aisoftwarefactory.world": true}
	storeARG := "ARG UPSTREAM=" + store
	cases := []struct {
		name   string
		lines  []string
		wantIn []string
	}{
		{
			name: "Dockerfile",
			lines: logicalLines(strings.Join([]string{
				storeARG,
				"FROM ghcr.io/example/gateway:1.2.3",
			}, "\n")),
			wantIn: []string{"Dockerfile", "ghcr.io/example/gateway:1.2.3"},
		},
		{
			// no stage above is named base: Docker Hub's library/base
			name: "Dockerfile",
			lines: logicalLines(strings.Join([]string{
				storeARG,
				"FROM base",
				"FROM ${UPSTREAM}docker.io/library/debian:bookworm-slim AS base",
			}, "\n")),
			wantIn: []string{"Dockerfile", "FROM base is"},
		},
		{
			name: "Dockerfile",
			lines: logicalLines(strings.Join([]string{
				"ARG UPSTREAM=ghcr.io/example/",
				"FROM ${UPSTREAM}gateway:1.2.3",
			}, "\n")),
			wantIn: []string{"Dockerfile", "ARG UPSTREAM="},
		},
		{
			name: "Dockerfile",
			lines: logicalLines(strings.Join([]string{
				storeARG,
				"ADD https://github.com/example/gateway/tarball /tmp/gateway.tar.gz",
			}, "\n")),
			wantIn: []string{"Dockerfile", "https://github.com/example/gateway/tarball"},
		},
		{
			name: "Dockerfile",
			lines: logicalLines(strings.Join([]string{
				storeARG,
				`RUN wget -O node.tar.xz "https://nodejs.org/dist/v24.1.0/node-v24.1.0-linux-x64.tar.xz"`,
			}, "\n")),
			wantIn: []string{"Dockerfile", "https://nodejs.org/dist/"},
		},
		{
			name: "Dockerfile",
			lines: logicalLines(strings.Join([]string{
				storeARG,
				`RUN echo "deb http://github.com/example/deb ./" > /etc/apt/sources.list`,
			}, "\n")),
			wantIn: []string{"Dockerfile", "http://github.com/example/deb"},
		},
		{
			name:   filepath.Join(".forgejo", "workflows", "ci.yml"),
			lines:  strings.Split("      run: curl -fsSL https://code.forgejo.org/forgejo/runner -o runner", "\n"),
			wantIn: []string{"ci.yml", "https://code.forgejo.org/"},
		},
		{
			name:   filepath.Join("internal", "factoryd", "buildvm.go"),
			lines:  strings.Split("curl -fsSL -o key.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc", "\n"),
			wantIn: []string{"buildvm.go", "https://www.postgresql.org/"},
		},
	}
	for _, c := range cases {
		problems := outsideFetches(c.name, c.lines, allowed)
		if len(problems) == 0 {
			t.Errorf("%s: the outside fetch raised no finding", c.name)
			continue
		}
		joined := strings.Join(problems, "; ")
		for _, want := range c.wantIn {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: findings %q lack %q", c.name, joined, want)
			}
		}
	}
	quiet := outsideFetches("Dockerfile", logicalLines(strings.Join([]string{
		storeARG,
		"FROM ${UPSTREAM}docker.io/library/debian:bookworm-slim@sha256:f3034a6ec3c1205360777c4aae76234998866ad18806ae62b63a3f84ccad782b AS base",
		"FROM base",
		`RUN wget -O /tmp/go-modules.tar.gz "https://git.aisoftwarefactory.world/api/packages/upstream/generic/go-modules/$sum/go-modules.tar.gz"`,
		`RUN echo "deb http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT} bookworm main" > /etc/apt/sources.list`,
		`RUN echo "deb [signed-by=/etc/apt/keyrings/pgdg.asc] https://apt-archive.postgresql.org/pub/repos/apt bookworm-pgdg-archive main" > /etc/apt/sources.list.d/pgdg.list`,
	}, "\n")), allowed)
	if len(quiet) != 0 {
		t.Errorf("the store's own fetches raised %v", quiet)
	}
}

// The store fronts the hosts upstream-files.txt downloads from; the
// build VM's own user-data rules deny exactly them (roadmap 203, step
// 3), so a build that names one anyway cannot reach it. The two lists
// travel together: a new upstream line without its deny rule fails
// here, and a deny rule for a host the store does not front fails too.
func TestTheBuildVMDeniesEveryUpstreamFileHost(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "factoryd", "buildvm.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := buildVMDenyList.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("buildvm.go: no buildVMUpstreamHosts deny list in the VM's user data")
	}
	denied := map[string]bool{}
	for _, q := range quotedString.FindAllStringSubmatch(m[1], -1) {
		denied[q[1]] = true
	}
	if len(denied) == 0 {
		t.Fatal("buildvm.go: the deny list names no host")
	}
	upstream := map[string]bool{}
	for _, l := range readUpstreamFiles(t) {
		u, err := url.Parse(l.url)
		if err != nil {
			t.Fatalf("upstream-files.txt: %s: %v", l.url, err)
		}
		upstream[u.Host] = true
	}
	for host := range upstream {
		if !denied[host] {
			t.Errorf("buildvm.go: the VM's rules do not deny %s, an upstream upstream-files.txt fronts", host)
		}
	}
	for host := range denied {
		if !upstream[host] {
			t.Errorf("buildvm.go: the VM denies %s, which upstream-files.txt does not name", host)
		}
	}
}

// The image-build job's own budget is 60 minutes (roadmap 203, step 3):
// run 2376's cold compile of the three images was cancelled by the job's
// 45-minute activeDeadlineSeconds at 44m26s, while each buildctl still
// had budget of its own (pr.yml gives it 2700 s). The pod deadline and
// the build VM's lease both hang off this constant, so the slowest
// serial path — runner start, checkout, the crane cache pulls, three
// parallel 45-minute builds and the push — must fit it.
func TestTheImageBuildJobGetsAnHour(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "cmd", "factoryd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`ciJobTimeout\s*=\s*(\d+)\s*\*\s*time\.Minute`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("cmd/factoryd/main.go: no ciJobTimeout constant")
	}
	mins, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if mins < 60 {
		t.Errorf("ciJobTimeout = %dm, want at least 60: a cold three-image compile must fit the job's own deadline", mins)
	}
}
