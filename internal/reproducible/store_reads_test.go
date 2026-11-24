package reproducible

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
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

// .forgejo/store-holds.sh asks the store, without a login, for each input
// the Dockerfiles it is given read, and for no other: a line of
// upstream-images.txt whose digest a Dockerfile names, a line of
// upstream-files.txt whose sha256 it names, and the go-modules and
// npm-cache tarballs of go.sum and the plugin lockfile when it fetches
// them. While one is missing it waits for the copy before the checks, and
// past its bound it names each one still missing. A stub curl stands in
// for the store and a stub sleep for the wait, whose first call puts what
// the store lacked.
func TestStoreHoldsNamesWhatTheChangedDockerfilesReadAndTheStoreLacks(t *testing.T) {
	golang, quay, helm := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("e", 64)
	node, gosec := strings.Repeat("c", 64), strings.Repeat("d", 64)
	gosum := fmt.Sprintf("%x", sha256.Sum256([]byte("go.sum bytes\n")))
	files := map[string]string{
		"Dockerfile.x": "FROM ${UPSTREAM}docker.io/library/golang:1.26.11@sha256:" + golang + " AS build\n" +
			"ADD --checksum=sha256:" + node + " https://git.aisoftwarefactory.world/api/packages/upstream/generic/node/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz /node.tar.xz\n" +
			"RUN sum=$(sha256sum go.sum | awk '{print $1}') && wget -O /tmp/go-modules.tar.gz \"https://git.aisoftwarefactory.world/api/packages/upstream/generic/go-modules/$sum/go-modules.tar.gz\"\n",
		"upstream-images.txt": "# images\ndocker.io/library/golang:1.26.11@sha256:" + golang + "\n" +
			"quay.io/x/y:1@sha256:" + quay + " bootstrap\nhttps://github.com/a/b-1.tgz@sha256:" + helm + " helm\n",
		"upstream-files.txt": "# files\nnode v24.22.3 node-v24.22.3-linux-x64.tar.xz https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz " + node + "\n" +
			"gosec v2.30.0 gosec.tar.gz https://github.com/securego/gosec/releases/download/v2.30.0/gosec.tar.gz " + gosec + "\n",
		"go.sum":                            "go.sum bytes\n",
		"opencode-plugin/package-lock.json": "lock bytes\n",
	}
	paths := map[string]string{
		"golang": "v2/upstream/docker.io/library/golang/manifests/sha256:" + golang,
		"node":   "api/packages/upstream/generic/node/v24.22.3/node-v24.22.3-linux-x64.tar.xz",
		"gomods": "api/packages/upstream/generic/go-modules/" + gosum + "/go-modules.tar.gz",
	}
	state := strings.NewReplacer("/", "_", ":", "_")
	script, err := filepath.Abs(filepath.Join("..", "..", ".forgejo", "store-holds.sh"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, held []string, wait string) (out, calls string, err error) {
		t.Helper()
		dir := t.TempDir()
		for name, body := range files {
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		bin, store := t.TempDir(), t.TempDir()
		for _, h := range held {
			if err := os.WriteFile(filepath.Join(store, state.Replace(paths[h])), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		stubs := map[string]string{
			"curl": "#!/bin/sh\necho \"curl $*\" >> " + bin + "/calls\nfor a; do url=$a; done\n" +
				"case \"$url\" in\n*/v2/token\\?*) echo '{\"token\":\"tok\"}' ;;\n" +
				"*) [ -f \"" + store + "/$(printf %s \"${url#*://*/}\" | tr '/:' '__')\" ] || exit 22 ;;\nesac\n",
			"sleep": "#!/bin/sh\necho \"sleep $*\" >> " + bin + "/calls\ntouch " + filepath.Join(store, state.Replace(paths["node"])) + "\n",
		}
		for name, body := range stubs {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("bash", script, "Dockerfile.x")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "STORE=http://store.test/", "STORE_WAIT="+wait)
		raw, err := cmd.CombinedOutput()
		c, _ := os.ReadFile(filepath.Join(bin, "calls"))
		return string(raw), string(c), err
	}
	t.Run("held", func(t *testing.T) {
		out, calls, err := run(t, []string{"golang", "node", "gomods"}, "0")
		if err != nil {
			t.Fatalf("store-holds: %v: %s", err, out)
		}
		for _, want := range []string{"http://store.test/" + paths["golang"], "-H Authorization: Bearer tok",
			"http://store.test/v2/token?scope=repository:upstream/docker.io/library/golang:pull",
			"http://store.test/" + paths["node"], "http://store.test/" + paths["gomods"]} {
			if !strings.Contains(calls, want) {
				t.Errorf("the store was not asked %s:\n%s", want, calls)
			}
		}
		for _, unread := range []string{quay, helm, "gosec", "npm-cache", "sleep", "store.test//"} {
			if strings.Contains(calls, unread) {
				t.Errorf("the check asked for %s, which no Dockerfile reads:\n%s", unread, calls)
			}
		}
	})
	t.Run("lacking past the wait", func(t *testing.T) {
		out, calls, err := run(t, nil, "0")
		if err == nil {
			t.Fatalf("store-holds passed on an empty store: %s", out)
		}
		for _, want := range []string{
			"the store lacks docker.io/library/golang 1.26.11 sha256:" + golang + ": the copy before the checks did not make it\n",
			"the store lacks node v24.22.3 node-v24.22.3-linux-x64.tar.xz: the copy before the checks did not make it\n",
			"the store lacks go-modules " + gosum + " go-modules.tar.gz: the copy before the checks did not make it\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(calls, "sleep") {
			t.Errorf("a wait of 0 slept:\n%s", calls)
		}
	})
	t.Run("copied while it waits", func(t *testing.T) {
		out, calls, err := run(t, []string{"golang", "gomods"}, "60")
		if err != nil || strings.Count(calls, "sleep ") != 1 || !strings.Contains(out, "waiting for the copy before the checks") {
			t.Errorf("err %v, calls:\n%s\noutput:\n%s\nwant one wait, then a pass", err, calls, out)
		}
	})
}
