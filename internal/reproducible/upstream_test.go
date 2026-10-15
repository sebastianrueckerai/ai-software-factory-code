package reproducible

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const store = "git.aisoftwarefactory.world/upstream/"

// upstreamLine is one image of upstream-images.txt.
type upstreamLine struct {
	ref       string // name:tag@sha256:<digest>
	bootstrap bool
}

// upstreamRef is a line's reference: a name Forgejo's registry takes
// (its imageNamePattern) under a registry host, a tag, a digest.
var upstreamRef = regexp.MustCompile(`^([a-z0-9]+([._-][a-z0-9]+)*\.[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)+):([A-Za-z0-9_][A-Za-z0-9._-]{0,114})@sha256:[0-9a-f]{64}$`)

func readUpstreamImages(t *testing.T) []upstreamLine {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "upstream-images.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []upstreamLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		l := upstreamLine{ref: fields[0]}
		switch {
		case len(fields) == 2 && fields[1] == "bootstrap":
			l.bootstrap = true
		case len(fields) != 1:
			t.Errorf("upstream-images.txt: %q: want a reference, then nothing or bootstrap", sc.Text())
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// Every line pins one image by its digest, under its registry's full name
// (docker.io/library/debian, not debian): the copy's name is that name in
// the store, and a short name would hide which upstream it is. The copy's
// tag adds 13 characters to the line's, within the 128 a tag may have.
func TestUpstreamImagesPinEachImageByDigest(t *testing.T) {
	lines := readUpstreamImages(t)
	if len(lines) == 0 {
		t.Fatal("upstream-images.txt lists no image")
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if !upstreamRef.MatchString(l.ref) {
			t.Errorf("upstream-images.txt: %q is not registry/path:tag@sha256:<64 hex>", l.ref)
		}
		image, _, _ := strings.Cut(l.ref, "@")
		if seen[image] {
			t.Errorf("upstream-images.txt: %s twice: one digest per tag", image)
		}
		seen[image] = true
	}
}

var fromLine = regexp.MustCompile(`^FROM\s+(\S+)`)

// Every image a Dockerfile builds on and main CI's buildkitd come from the
// store by a line of upstream-images.txt (roadmap 203): no build reaches
// an upstream registry for an image, a pull request's neither: pr.yml
// hands buildctl the job token as the store's login and leaves UPSTREAM
// alone. Hand-derived from the three Dockerfiles and ci.yml as they stand.
func TestOutsideImagesComeFromTheStore(t *testing.T) {
	listed := map[string]bool{}
	for _, l := range readUpstreamImages(t) {
		if !l.bootstrap {
			listed[l.ref] = true
		}
	}
	want := map[string][]string{
		"Dockerfile": {
			"docker.io/library/golang:1.26.9-alpine",
			"docker.io/library/debian:bookworm-slim",
		},
		"Dockerfile.sandbox": {
			"docker.io/library/golang:1.26.9-alpine",
			"docker.io/library/node:22-bookworm-slim",
			"docker.io/library/debian:bookworm-slim",
		},
		"Dockerfile.runner": {
			"docker.io/library/golang:1.26.9",
			"docker.io/library/debian:bookworm-slim",
		},
	}
	for file, images := range want {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", file))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			stages := map[string]bool{"scratch": true}
			upstreamArg := ""
			for _, line := range logicalLines(string(raw)) {
				if v, ok := strings.CutPrefix(line, "ARG UPSTREAM="); ok && len(got) == 0 {
					upstreamArg = v
				}
				m := fromLine.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				if fields := strings.Fields(line); len(fields) == 4 && strings.EqualFold(fields[2], "AS") {
					stages[fields[3]] = true
				}
				if stages[m[1]] {
					continue
				}
				ref, ok := strings.CutPrefix(m[1], "${UPSTREAM}")
				if !ok || !listed[ref] {
					t.Errorf("%s: FROM %s: want ${UPSTREAM} and a line of upstream-images.txt not marked bootstrap", file, m[1])
					continue
				}
				image, _, _ := strings.Cut(ref, "@")
				got = append(got, image)
			}
			if upstreamArg != store {
				t.Errorf("%s: ARG UPSTREAM=%q before the first FROM, want %q", file, upstreamArg, store)
			}
			if !slices.Equal(got, images) {
				t.Errorf("%s: images from the store %v, want %v", file, got, images)
			}
		})
	}
	ci, err := os.ReadFile(filepath.Join("..", "..", ".forgejo", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Needs any `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ci, &workflow); err != nil {
		t.Fatal(err)
	}
	if ref, ok := strings.CutPrefix(workflow.Env["BUILDKIT"], store); !ok || !listed[ref] {
		t.Errorf("ci.yml: BUILDKIT=%s, want the store's copy of a line of upstream-images.txt", workflow.Env["BUILDKIT"])
	}
	if _, ok := workflow.Jobs["upstream"]; !ok {
		t.Error("ci.yml: no upstream job copies the list into the store")
	}
	if needs := workflow.Jobs["test"].Needs; needs != "upstream" {
		t.Errorf("ci.yml: the test job needs %v, want upstream: it builds the runner image from the store, so the copy comes first", needs)
	}
	pr, err := os.ReadFile(filepath.Join("..", "..", ".forgejo", "workflows", "pr.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pr), "UPSTREAM=") {
		t.Error("pr.yml overrides UPSTREAM: a pull request's build takes the outside images from their upstreams")
	}
	if !regexp.MustCompile(`"auths":\{"git\.aisoftwarefactory\.world":\{"auth":"%s"\}\}\}' "\$\(printf 'job:%s' "\$FORGEJO_TOKEN" \| base64 -w0\)"`).Match(pr) {
		t.Error("pr.yml: no store login from the job token for buildctl")
	}
}

// The copy reads the store before the upstream: a line the store holds
// costs one manifest read there and nothing upstream, so a main push
// while an upstream is down still passes; a missing one is copied by its
// digest under a tag no other digest takes. A stub crane records the
// calls; the store holds the first line's digest.
func TestUpstreamCopyCopiesWhatTheStoreLacks(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "list")
	const (
		a = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		b = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		c = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	body := "# a comment\n\n" +
		"docker.io/library/debian:bookworm-slim@" + a + "\n" +
		"ghcr.io/fluxcd/source-controller:v1.9.5@" + b + " bootstrap\n" +
		"code.forgejo.org/forgejo/runner:12.7.3@" + c + "\n"
	if err := os.WriteFile(list, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls")
	crane := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"[ \"$1\" = manifest ] && [ \"$2\" = \"git.aisoftwarefactory.world/upstream/docker.io/library/debian@" + a + "\" ] && exit 0\n" +
		"[ \"$1\" = manifest ] && exit 1\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "crane"), []byte(crane), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join("..", "..", "scripts", "upstream-copy"), list)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upstream-copy: %v: %s", err, out)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "manifest git.aisoftwarefactory.world/upstream/docker.io/library/debian@" + a + "\n" +
		"manifest git.aisoftwarefactory.world/upstream/ghcr.io/fluxcd/source-controller@" + b + "\n" +
		"copy ghcr.io/fluxcd/source-controller@" + b + " git.aisoftwarefactory.world/upstream/ghcr.io/fluxcd/source-controller:v1.9.5-bbbbbbbbbbbb\n" +
		"manifest git.aisoftwarefactory.world/upstream/code.forgejo.org/forgejo/runner@" + c + "\n" +
		"copy code.forgejo.org/forgejo/runner@" + c + " git.aisoftwarefactory.world/upstream/code.forgejo.org/forgejo/runner:12.7.3-cccccccccccc\n"
	if string(got) != want {
		t.Errorf("crane calls:\n%s\nwant:\n%s", got, want)
	}
	if err := os.WriteFile(list, []byte("docker.io/library/debian:bookworm-slim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(filepath.Join("..", "..", "scripts", "upstream-copy"), list).CombinedOutput(); err == nil || !strings.Contains(string(out), "want name:tag@sha256:<digest>") {
		t.Errorf("upstream-copy on a line without a digest: %v: %s, want a refusal", err, out)
	}
}
