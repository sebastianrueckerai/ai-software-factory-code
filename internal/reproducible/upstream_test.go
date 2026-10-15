package reproducible

import (
	"bufio"
	"crypto/sha256"
	"fmt"
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

// upstreamLine is one line of upstream-images.txt: an image or OCI chart
// (ref name:tag@sha256:<digest>, kind "" or bootstrap), or a file (ref
// https URL@sha256:<sha256>, kind helm, or onto and the base image's
// name:tag).
type upstreamLine struct {
	ref       string
	bootstrap bool
	kind      string
	base      string
}

// upstreamRef is a line's reference: a name Forgejo's registry takes
// (its imageNamePattern) under a registry host, a tag, a digest.
var fileRef = regexp.MustCompile(`^https://[^\s@]+/[^/\s@]+@sha256:[0-9a-f]{64}$`)

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
		if len(fields) > 1 {
			l.kind = fields[1]
		}
		switch {
		case len(fields) == 2 && l.kind == "bootstrap":
			l.bootstrap = true
		case len(fields) == 2 && l.kind == "helm":
		case len(fields) == 3 && l.kind == "onto":
			l.base = fields[2]
		case len(fields) != 1:
			t.Errorf("upstream-images.txt: %q: want a reference, then nothing, bootstrap, helm or onto <name>:<tag>", sc.Text())
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
	for i, l := range lines {
		if l.kind == "onto" && !slices.ContainsFunc(lines[:i], func(b upstreamLine) bool { return strings.HasPrefix(b.ref, l.base+"@") && !b.bootstrap }) {
			t.Errorf("upstream-images.txt: %s goes onto %s, which no image line before it names", l.ref, l.base)
		}
		if strings.HasPrefix(l.ref, "https://") {
			if !fileRef.MatchString(l.ref) || (l.kind != "helm" && l.kind != "onto") {
				t.Errorf("upstream-images.txt: %q %s is not an https URL@sha256:<64 hex> of a file and what the store makes of it", l.ref, l.kind)
			}
		} else if !upstreamRef.MatchString(l.ref) || (l.kind != "" && !l.bootstrap) {
			t.Errorf("upstream-images.txt: %q %s is not registry/path:tag@sha256:<64 hex>", l.ref, l.kind)
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
// costs manifest reads there and nothing upstream, so a main push while an
// upstream is down still passes; a missing one is copied by its digest
// under a tag no other digest takes. The upstream tag follows the pinned
// digest. A stub crane records the calls: the store holds the first line's
// digest under both tags, the second line's tag names an older digest, the
// third is missing.
func TestUpstreamCopyCopiesWhatTheStoreLacks(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "list")
	const (
		a   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		b   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		c   = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		old = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		s   = "git.aisoftwarefactory.world/upstream/"
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
		"case \"$*\" in\n" +
		"\"manifest " + s + "docker.io/library/debian@" + a + "\") exit 0 ;;\n" +
		"\"digest " + s + "docker.io/library/debian:bookworm-slim\") echo " + a + " ;;\n" +
		"\"digest " + s + "ghcr.io/fluxcd/source-controller:v1.9.5\") echo " + old + " ;;\n" +
		"manifest*|digest*) exit 1 ;;\n" +
		"esac\n"
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
	want := "manifest " + s + "docker.io/library/debian@" + a + "\n" +
		"digest " + s + "docker.io/library/debian:bookworm-slim\n" +
		"manifest " + s + "ghcr.io/fluxcd/source-controller@" + b + "\n" +
		"copy ghcr.io/fluxcd/source-controller@" + b + " " + s + "ghcr.io/fluxcd/source-controller:v1.9.5-bbbbbbbbbbbb\n" +
		"digest " + s + "ghcr.io/fluxcd/source-controller:v1.9.5\n" +
		"tag " + s + "ghcr.io/fluxcd/source-controller@" + b + " v1.9.5\n" +
		"manifest " + s + "code.forgejo.org/forgejo/runner@" + c + "\n" +
		"copy code.forgejo.org/forgejo/runner@" + c + " " + s + "code.forgejo.org/forgejo/runner:12.7.3-cccccccccccc\n" +
		"digest " + s + "code.forgejo.org/forgejo/runner:12.7.3\n" +
		"tag " + s + "code.forgejo.org/forgejo/runner@" + c + " 12.7.3\n"
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

// A chart archive is fetched, checked against its sha256 and uploaded to
// the store's Helm repository, unless that holds it: a stub curl serves
// the archive and the repository, which has the first chart only.
func TestUpstreamCopyUploadsChartArchives(t *testing.T) {
	dir := t.TempDir()
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("chart bytes")))
	body := "https://example.org/a-1.0.0.tgz@sha256:" + sum + " helm\n" +
		"https://example.org/b-2.0.0.tgz@sha256:" + sum + " helm\n" +
		"https://example.org/c-3.0.0.tgz@sha256:" + strings.Repeat("0", 64) + " helm\n"
	list := filepath.Join(dir, "list")
	if err := os.WriteFile(list, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls")
	curl := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"last=; out=; prev=\nfor a; do [ \"$prev\" = -o ] && out=$a; prev=$a; last=$a; done\n" +
		"case \"$last\" in\n" +
		"https://git.aisoftwarefactory.world/api/packages/upstream/helm/a-1.0.0.tgz) exit 0 ;;\n" +
		"https://git.aisoftwarefactory.world/api/packages/upstream/helm/api/charts) exit 0 ;;\n" +
		"https://example.org/*) printf 'chart bytes' > \"$out\" ;;\n" +
		"*) exit 22 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join("..", "..", "scripts", "upstream-copy"), list)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "REGISTRY_USER=u", "REGISTRY_PASSWORD=p")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "c-3.0.0.tgz@sha256:0000") {
		t.Fatalf("upstream-copy with a wrong sum: %v: %s, want c's failure", err, out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	const helm = "https://git.aisoftwarefactory.world/api/packages/upstream/helm"
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		f := strings.Fields(l)
		got = append(got, f[len(f)-1])
	}
	want := []string{
		helm + "/a-1.0.0.tgz",
		helm + "/b-2.0.0.tgz", "https://example.org/b-2.0.0.tgz", helm + "/api/charts",
		helm + "/c-3.0.0.tgz", "https://example.org/c-3.0.0.tgz",
	}
	if !slices.Equal(got, want) {
		t.Errorf("curl calls end in\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(string(calls), "-u u:p -X POST --upload-file ") {
		t.Errorf("the upload is not a POST of the file with the login: %s", calls)
	}
}

// A file a pod needs becomes the one added layer of an image on the
// store's copy of its base line, under upstream/files/<its name>, unless
// the store holds that image: a stub curl serves the file, a stub crane
// the store, which has the first file's image only.
func TestUpstreamCopyPutsFilesOntoTheirBase(t *testing.T) {
	dir := t.TempDir()
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("udf bytes")))
	const base = "docker.io/library/alpine:3.18.2@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	body := base + "\n" +
		"https://example.org/x%2Fv1/held.tar.gz@sha256:" + sum + " onto docker.io/library/alpine:3.18.2\n" +
		"https://example.org/x%2Fv1/udf.tar.gz@sha256:" + sum + " onto docker.io/library/alpine:3.18.2\n"
	list := filepath.Join(dir, "list")
	if err := os.WriteFile(list, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls")
	const s = "git.aisoftwarefactory.world/upstream/"
	crane := "#!/bin/sh\necho \"crane $*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"\"digest " + s + "files/held.tar.gz:" + sum[:12] + "\") echo sha256:held ;;\n" +
		"manifest*|digest*) exit 1 ;;\n" +
		"esac\n"
	curl := "#!/bin/sh\necho \"curl $*\" >> " + log + "\nprev=\nfor a; do [ \"$prev\" = -o ] && printf 'udf bytes' > \"$a\"; prev=$a; done\n"
	for name, script := range map[string]string{"crane": crane, "curl": curl} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join("..", "..", "scripts", "upstream-copy"), list)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("upstream-copy: %v: %s", err, out)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var appends []string
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(l, "crane append ") {
			appends = append(appends, strings.Join(strings.Fields(l)[2:4], " ")+" "+strings.Join(strings.Fields(l)[6:], " "))
		}
	}
	want := []string{"-b " + s + "docker.io/library/alpine@sha256:2222222222222222222222222222222222222222222222222222222222222222 -t " + s + "files/udf.tar.gz:" + sum[:12]}
	if !slices.Equal(appends, want) {
		t.Errorf("crane appends %q, want %q", appends, want)
	}
	if !strings.Contains(string(calls), "curl -fsSL --retry 5 -o ") || !strings.Contains(string(calls), " https://example.org/x%2Fv1/udf.tar.gz") {
		t.Errorf("the file was not fetched: %s", calls)
	}
}
