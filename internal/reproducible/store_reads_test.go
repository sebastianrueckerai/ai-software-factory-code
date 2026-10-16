package reproducible

import (
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Builds read the store for every input the store holds (roadmap 203,
// step 2): the Go module cache as the go-modules tarball of go.sum, the
// npm cache as the npm-cache tarball of the opencode plugin lockfile,
// both versioned by scripts/upstream-files under the lockfile's sha256.
// A missing or wrong store copy fails the build — no RUN here falls
// back to an upstream.

const goModulesURL = "https://git.aisoftwarefactory.world/api/packages/upstream/generic/go-modules/"

// buildingGo is a RUN's go calls that compile or resolve modules.
var buildingGo = regexp.MustCompile(`go (build|vet|test|mod download)\b`)

// installingNpm is a RUN's npm calls that install packages.
var installingNpm = regexp.MustCompile(`npm (ci|install)\b`)

// offline reports whether the RUN's go calls see GOPROXY=off and
// GOFLAGS=-mod=mod, from the stage's ENV or the RUN's own environment.
func offline(s *dockerfileStage, run string) bool {
	if s.envs["GOPROXY"] == "off" && s.envs["GOFLAGS"] == "-mod=mod" {
		return true
	}
	return strings.Contains(run, "GOPROXY=off") && strings.Contains(run, "GOFLAGS=-mod=mod")
}

// Every Go build of the three images restores the store's go-modules
// package for its go.sum and runs offline (roadmap 203, step 2): the
// download cache is fetched under go.sum's sha256 and untarred into
// GOMODCACHE's download tree before go mod download, and every go call
// that builds, vets, tests or resolves runs with GOPROXY=off, so no
// upstream answers a miss. Hand-derived from the three Dockerfiles:
// these and only these stages run those calls.
func TestGoBuildsRestoreTheStoresModuleCache(t *testing.T) {
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
			stages, got := stagesWhere(t, filepath.Join("..", "..", w.file), func(s *dockerfileStage) bool {
				return buildingGo.MatchString(strings.Join(s.runs, "\n"))
			})
			if !reflect.DeepEqual(got, w.stages) {
				t.Fatalf("%s: stages running a building go call = %v, want %v", w.file, got, w.stages)
			}
			for _, s := range stages {
				var download string
				for _, run := range s.runs {
					if strings.Contains(run, "go mod download") {
						download = run
					}
					if buildingGo.MatchString(run) && !offline(s, run) {
						t.Errorf("%s stage %s: a building go call runs online: %s", w.file, s.name, run)
					}
				}
				if download == "" {
					t.Fatalf("%s stage %s: no go mod download to warm the cache from the store", w.file, s.name)
				}
				// the store's tarball is keyed by go.sum's own sha256
				for _, part := range []string{"sha256sum go.sum", goModulesURL, "$sum/go-modules.tar.gz", "cache/download", "-xzf", "go mod download"} {
					if !strings.Contains(download, part) {
						t.Errorf("%s stage %s: the module cache restore lacks %q: %s", w.file, s.name, part, download)
					}
				}
				if strings.Index(download, "go mod download") < strings.LastIndex(download, goModulesURL) {
					t.Errorf("%s stage %s: go mod download runs before the store's cache is restored: %s", w.file, s.name, download)
				}
			}
		})
	}
}

// The sandbox image's npm ci runs offline on the store's npm-cache
// package of the opencode plugin lockfile (roadmap 203, step 2): a stage
// fetches the tarball under the lockfile's sha256, the plugin stage
// installs from that cache alone, and no npm call anywhere in the image
// reaches registry.npmjs.org.
func TestSandboxNpmInstallsOfflineFromTheStoresCache(t *testing.T) {
	const npmCacheURL = "https://git.aisoftwarefactory.world/api/packages/upstream/generic/npm-cache/"
	stages := parseDockerfile(t, filepath.Join("..", "..", "Dockerfile.sandbox"))
	var fetcher string
	for _, s := range stages {
		for _, run := range s.runs {
			if !strings.Contains(run, npmCacheURL) {
				continue
			}
			if fetcher != "" {
				t.Fatalf("Dockerfile.sandbox: stages %s and %s both fetch the npm cache", fetcher, s.name)
			}
			fetcher = s.name
			for _, part := range []string{"sha256sum", "package-lock.json", "$sum/npm-cache.tar.gz", "tar"} {
				if !strings.Contains(run, part) {
					t.Errorf("Dockerfile.sandbox stage %s: the npm cache fetch lacks %q: %s", s.name, part, run)
				}
			}
		}
	}
	if fetcher == "" {
		t.Fatal("Dockerfile.sandbox: no stage fetches the store's npm cache")
	}
	var npm []string
	for _, s := range stages {
		for _, run := range s.runs {
			if !installingNpm.MatchString(run) {
				continue
			}
			npm = append(npm, s.name+": "+run)
			if !strings.Contains(run, "--offline") || !strings.Contains(run, "--cache") {
				t.Errorf("Dockerfile.sandbox stage %s: an npm install runs online: %s", s.name, run)
			}
		}
	}
	if len(npm) != 1 {
		t.Fatalf("Dockerfile.sandbox: %d npm installs, want the one ci: %v", len(npm), npm)
	}
	var copied bool
	for _, s := range stages {
		for _, line := range s.lines {
			if strings.HasPrefix(line, "COPY --from="+fetcher+" ") {
				copied = true
			}
		}
	}
	if !copied {
		t.Errorf("Dockerfile.sandbox: no COPY --from=%s installs on the store's cache", fetcher)
	}
}
