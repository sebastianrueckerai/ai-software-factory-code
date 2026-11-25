package follow

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	pins "github.com/sebastianrueckerai/ai-software-factory-code"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// dg is the digest sha256:<64 times c>.
func dg(c string) string { return "sha256:" + strings.Repeat(c, 64) }

const (
	fileSum = "33997073eb6d" + "0123456789abcdef0123456789abcdef0123456789abcdef0123" // 64 hex
	nodeSum = "4f2b175a6d53a5c617a80dba2676238a0688cef28c6da4eba35c7d844e66fcfb"
)

var copyBytes = []byte("new manifest\n")

func images() string {
	return strings.Join([]string{
		"# the list",
		"docker.io/library/golang:1.26.12-alpine@" + dg("a") + " patch",
		"docker.io/library/golang:1.26.12@" + dg("b") + " patch",
		"docker.io/library/golang:1.25-bookworm@" + dg("c") + " patch",
		"ghcr.io/berriai/litellm:1.108.17@" + dg("d") + " patch",
		"ghcr.io/berriai/litellm-helm:1.108.17@" + dg("e") + " with ghcr.io/berriai/litellm",
		"https://example.org/temporal-1.8.0.tgz@sha256:" + strings.Repeat("5", 64) + " helm",
		"https://example.org/k8s-infra-0.17.1.tgz@sha256:" + strings.Repeat("6", 64) + " helm",
		"https://example.org/dl/histogram-quantile_linux_amd64.tar.gz@sha256:" + fileSum + " onto docker.io/library/alpine:3.18.12",
		"quay.io/jetstack/cert-manager-controller:v1.21.3@" + dg("f") + " bootstrap",
		"code.forgejo.org/forgejo/forgejo:17.0.2-rootless@" + dg("7") + " bootstrap",
		"docker.io/library/registry:3@" + dg("8"),
		"docker.io/library/node:22-bookworm-slim@" + dg("1"),
		"docker.io/library/node:24-bookworm-slim@" + dg("2"),
		"",
	}, "\n")
}

func filesList() string {
	return strings.Join([]string{
		"node v24.22.3 node-v24.22.3-linux-x64.tar.xz https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz " + nodeSum,
		"cnpg v1.30.2 cnpg.yaml https://example.org/cnpg.yaml " + sumHex(copyBytes),
		"",
	}, "\n")
}

type fakeStore struct {
	digest string
	copy   []byte
	err    error
}

func (s fakeStore) FileImageDigest(archive, tag string) (string, error) {
	if archive != "histogram-quantile_linux_amd64.tar.gz" || tag != "33997073eb6d" {
		return "", errors.New("unexpected " + archive + ":" + tag)
	}
	return s.digest, s.err
}

func (s fakeStore) FileCopy(f pins.File) ([]byte, error) { return s.copy, s.err }

var store = fakeStore{digest: dg("9"), copy: copyBytes}

// tree is a tree with the two lists and the given files.
func tree(files map[string]string) map[string][]byte {
	out := map[string][]byte{ImagesList: []byte(images()), FilesList: []byte(filesList())}
	for p, c := range files {
		out[p] = []byte(c)
	}
	return out
}

var all = []policy.Part{{Name: "all", Lane: policy.LaneAlone, Paths: []string{"k8s", "Dockerfile"}}}

// Every form of value, behind: the line below the marker becomes what the
// list says, and the move names before and after.
func TestRunRewritesEachShape(t *testing.T) {
	old := dg("0")
	for _, tc := range []struct {
		name, marker, line, want string
		move                     Move
	}{
		{"values tag with digest", "# pin ghcr.io/berriai/litellm",
			`      tag: "1.102.4@` + old + `"`, `      tag: "1.108.17@` + dg("d") + `"`,
			Move{"ghcr.io/berriai/litellm", "1.102.4", "1.108.17", "1.108.17"}},
		{"full reference with the store prefix", "# pin ghcr.io/berriai/litellm",
			`image: git.example/upstream/ghcr.io/berriai/litellm:1.102.4@` + old, `image: git.example/upstream/ghcr.io/berriai/litellm:1.108.17@` + dg("d"),
			Move{"ghcr.io/berriai/litellm", "1.102.4", "1.108.17", "1.108.17"}},
		{"runner label string", "# pin ghcr.io/berriai/litellm",
			`  - ubuntu-latest:docker://x/litellm:1.1.1@` + old, `  - ubuntu-latest:docker://x/litellm:1.108.17@` + dg("d"),
			Move{"ghcr.io/berriai/litellm", "1.1.1", "1.108.17", "1.108.17"}},
		{"a dockerfile FROM with an alias", "# pin docker.io/library/golang:1.25-bookworm",
			`FROM ${UPSTREAM}docker.io/library/golang:1.25-bookworm@` + old + ` AS go`, `FROM ${UPSTREAM}docker.io/library/golang:1.25-bookworm@` + dg("c") + ` AS go`,
			Move{"docker.io/library/golang:1.25-bookworm", "1.25-bookworm@000000000000", "1.25-bookworm@cccccccccccc", "1.25-bookworm"}},
		{"pattern with a digit run", "# pin docker.io/library/golang:1.26.*-alpine",
			`FROM golang:1.26.11-alpine@` + old, `FROM golang:1.26.12-alpine@` + dg("a"),
			Move{"docker.io/library/golang:1.26.*-alpine", "1.26.11-alpine", "1.26.12-alpine", "1.26.12-alpine"}},
		{"pattern of the plain tag", "# pin docker.io/library/golang:1.26.*",
			`FROM golang:1.26.11@` + old, `FROM golang:1.26.12@` + dg("b"),
			Move{"docker.io/library/golang:1.26.*", "1.26.11", "1.26.12", "1.26.12"}},
		{"floating tag, digest moves", "# pin docker.io/library/registry",
			`image: reg/registry:3@` + old, `image: reg/registry:3@` + dg("8"),
			Move{"docker.io/library/registry", "3@000000000000", "3@888888888888", "3"}},
		{"digest alone of an OCI chart", "# pin ghcr.io/berriai/litellm-helm",
			`  ref: {digest: "` + old + `"}`, `  ref: {digest: "` + dg("e") + `"}`,
			Move{"ghcr.io/berriai/litellm-helm", "sha256:000000000000", "sha256:eeeeeeeeeeee", "1.108.17"}},
		{"kustomize images digest", "# pin quay.io/jetstack/cert-manager-controller",
			`- {name: quay.io/jetstack/cert-manager-controller, digest: "` + old + `"}`, `- {name: quay.io/jetstack/cert-manager-controller, digest: "` + dg("f") + `"}`,
			Move{"quay.io/jetstack/cert-manager-controller", "sha256:000000000000", "sha256:ffffffffffff", "v1.21.3"}},
		{"tag and digest as two keys, the chart's own suffix left out", "# pin code.forgejo.org/forgejo/forgejo",
			`image: {tag: "17.0.1", digest: "` + old + `"}`, `image: {tag: "17.0.2", digest: "` + dg("7") + `"}`,
			Move{"code.forgejo.org/forgejo/forgejo", "17.0.1", "17.0.2", "17.0.2-rootless"}},
		{"chart version", "# pin chart temporal",
			`      version: 1.7.0`, `      version: 1.8.0`,
			Move{"chart temporal", "1.7.0", "1.8.0", "1.8.0"}},
		{"chart with a dash in its name", "# pin chart k8s-infra",
			`      version: "0.16.0"`, `      version: "0.17.1"`,
			Move{"chart k8s-infra", "0.16.0", "0.17.1", "0.17.1"}},
		{"file-derived image", "# pin image-of histogram-quantile_linux_amd64.tar.gz",
			`          image: {tag: "111111111111@` + old + `"}`, `          image: {tag: "33997073eb6d@` + dg("9") + `"}`,
			Move{"image-of histogram-quantile_linux_amd64.tar.gz", "111111111111", "33997073eb6d", "33997073eb6d"}},
		{"file version", "# pin file node",
			`ARG NODE_VERSION=v24.20.0`, `ARG NODE_VERSION=v24.22.3`,
			Move{"file node", "v24.20.0", "v24.22.3", "v24.22.3"}},
		{"file URL", "# pin file node",
			`url: https://nodejs.org/dist/v24.20.0/node-v24.20.0-linux-x64.tar.xz`, `url: https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz`,
			Move{"file node", "https://nodejs.org/dist/v24.20.0/node-v24.20.0-linux-x64.tar.xz", "https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz", "v24.22.3"}},
		{"file sha256", "# pin file node",
			`ARG NODE_SHA256=` + strings.Repeat("e", 64), `ARG NODE_SHA256=` + nodeSum,
			Move{"file node", "sha256:eeeeeeeeeeee", "sha256:4f2b175a6d53", "v24.22.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tree(map[string]string{"k8s/f.yaml": "a: 1\n  " + tc.marker + "\n" + tc.line + "\nb: 2\n"})
			res, err := Run(files, all, store)
			if err != nil {
				t.Fatal(err)
			}
			want := "a: 1\n  " + tc.marker + "\n" + tc.want + "\nb: 2\n"
			if got := string(res[0].Changes["k8s/f.yaml"]); got != want {
				t.Fatalf("file:\n%s\nwant:\n%s", got, want)
			}
			if !reflect.DeepEqual(res[0].Moves, []Move{tc.move}) || res[0].Behind() != 1 {
				t.Fatalf("moves %+v, want %+v", res[0].Moves, tc.move)
			}
			if m := res[0].Markers; len(m) != 1 || m[0].Have != tc.line || m[0].Want != tc.want || m[0].Line != 3 {
				t.Fatalf("markers %+v, want have %q and want %q on line 3", m, tc.line, tc.want)
			}
		})
	}
}

// A value that already is the list's moves nothing, and the file is not a change.
func TestRunCurrentValueIsNotBehind(t *testing.T) {
	files := tree(map[string]string{"k8s/f.yaml": "# pin ghcr.io/berriai/litellm\ntag: \"1.108.17@" + dg("d") + "\"\n"})
	res, err := Run(files, all, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Changes) != 0 || res[0].Behind() != 0 || len(res[0].Markers) != 1 {
		t.Fatalf("result %+v, want one marker, nothing behind", res[0])
	}
}

// The parts come back in the order given; the same key under two markers
// counts twice, each with its own ID; a hand part holds no marker and its
// digests need none, whatever other hand parts overlap it.
func TestRunPartsAndLanes(t *testing.T) {
	parts := []policy.Part{
		{Name: "b", Lane: policy.LaneHuman, Paths: []string{"k8s/b"}},
		{Name: "a", Lane: policy.LaneAlone, Paths: []string{"k8s/a.yaml"}},
		{Name: "h", Lane: policy.LaneHand, Paths: []string{"k8s/h", "docs"}},
		{Name: "g", Lane: policy.LaneHand, Paths: []string{"k8s/h/deep"}},
		{Name: "n", Lane: policy.LaneHand},
	}
	files := tree(map[string]string{
		"k8s/a.yaml":      "# pin ghcr.io/berriai/litellm\ntag: \"1.1.1@" + dg("0") + "\"\n# pin ghcr.io/berriai/litellm\ntag: \"1.1.2@" + dg("0") + "\"\n",
		"k8s/b/c.yaml":    "# pin chart temporal\nversion: 1.8.0\n",
		"k8s/h/d.yaml":    "image: x@" + dg("1") + "\n",
		"k8s/h/deep/e.md": "cilium install --set image.digest=" + dg("2") + "\n",
	})
	res, err := Run(files, parts, store)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range res {
		names = append(names, r.Part.Name)
	}
	if !reflect.DeepEqual(names, []string{"b", "a", "h", "g", "n"}) {
		t.Fatalf("order %v", names)
	}
	if res[0].Behind() != 0 || len(res[0].Markers) != 1 {
		t.Errorf("b: %+v, want one marker, none behind", res[0])
	}
	if res[1].Behind() != 2 || res[1].Markers[0].ID == res[1].Markers[1].ID {
		t.Errorf("a: %+v, want two moves with distinct IDs", res[1])
	}
	for _, r := range res[2:] {
		if len(r.Markers) != 0 || len(r.Moves) != 0 || len(r.Changes) != 0 {
			t.Errorf("hand part %s: %+v, want nothing", r.Part.Name, r)
		}
	}
}

func TestRunCopy(t *testing.T) {
	files := tree(map[string]string{
		"k8s/cnpg/kustomization.yaml": "resources:\n# pin copy cnpg\n- cnpg.yaml\n",
		"k8s/cnpg/cnpg.yaml":          "old manifest sha256:" + strings.Repeat("0", 64) + "\n",
	})
	res, err := Run(files, all, store)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if string(r.Changes["k8s/cnpg/cnpg.yaml"]) != string(copyBytes) || len(r.Changes) != 1 {
		t.Fatalf("changes %v, want the copy replaced by the store's bytes", r.Changes)
	}
	wantMove := Move{"copy cnpg", "sha256:" + sumHex(files["k8s/cnpg/cnpg.yaml"])[:12], "v1.30.2", "v1.30.2"}
	if !reflect.DeepEqual(r.Moves, []Move{wantMove}) {
		t.Fatalf("moves %+v, want %+v", r.Moves, wantMove)
	}
	if m := r.Markers[0]; m.Have != sumHex(files["k8s/cnpg/cnpg.yaml"]) || m.Want != sumHex(copyBytes) {
		t.Fatalf("marker %+v", m)
	}

	// offline: behind is known from the sha256, no bytes come
	res, err = Run(files, all, Offline{})
	if err != nil || res[0].Behind() != 1 || len(res[0].Changes) != 0 {
		t.Fatalf("offline: %+v, %v; want one move, no change", res[0], err)
	}

	// the copy as the list pins it is not behind
	files["k8s/cnpg/cnpg.yaml"] = copyBytes
	res, err = Run(files, all, store)
	if err != nil || res[0].Behind() != 0 || len(res[0].Changes) != 0 {
		t.Fatalf("current copy: %+v, %v", res[0], err)
	}
}

// Offline the digest of a file-derived image stays as the tree has it, the tag moves.
func TestRunOfflineKeepsTheDerivedDigest(t *testing.T) {
	files := tree(map[string]string{"k8s/f.yaml": "# pin image-of histogram-quantile_linux_amd64.tar.gz\ntag: \"111111111111@" + dg("0") + "\"\n"})
	res, err := Run(files, all, Offline{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(res[0].Changes["k8s/f.yaml"]), "# pin image-of histogram-quantile_linux_amd64.tar.gz\ntag: \"33997073eb6d@"+dg("0")+"\"\n"; got != want {
		t.Fatalf("file %q, want %q", got, want)
	}
}

func TestRunRefuses(t *testing.T) {
	old := dg("0")
	for _, tc := range []struct {
		name  string
		files map[string]string
		parts []policy.Part
		want  []string
	}{
		{"a key with no line", map[string]string{"k8s/f.yaml": "# pin ghcr.io/none/none\nimage: x:1@" + old + "\n"}, all,
			[]string{"k8s/f.yaml:1: # pin ghcr.io/none/none: no line of upstream-images.txt has this name: fix the key"}},
		{"a pattern with no line", map[string]string{"k8s/f.yaml": "# pin docker.io/library/golang:1.99.*\nimage: x:1@" + old + "\n"}, all,
			[]string{"k8s/f.yaml:1: # pin docker.io/library/golang:1.99.*: no line of upstream-images.txt has this name and tag"}},
		{"a key of several lines", map[string]string{"k8s/f.yaml": "# pin docker.io/library/golang\nimage: x:1@" + old + "\n"}, all,
			[]string{"k8s/f.yaml:1: # pin docker.io/library/golang: the key fits the lines of upstream-images.txt: 1.26.12-alpine, 1.26.12, 1.25-bookworm: write # pin docker.io/library/golang:<tag pattern> that fits one"}},
		{"a pattern of several lines", map[string]string{"k8s/f.yaml": "# pin docker.io/library/golang:1.26.*-alpine\nimage: x:1@" + old + "\n# pin docker.io/library/node:*-bookworm-slim\nimage: x:1@" + old + "\n"}, all,
			[]string{"k8s/f.yaml:3: # pin docker.io/library/node:*-bookworm-slim: the key fits the lines of upstream-images.txt: 22-bookworm-slim, 24-bookworm-slim"}},
		{"a value of no shape", map[string]string{"k8s/f.yaml": "# pin ghcr.io/berriai/litellm\nimage: latest\n"}, all,
			[]string{"k8s/f.yaml:1: # pin ghcr.io/berriai/litellm: the value has no known shape"}},
		{"a marker with no value", map[string]string{"k8s/f.yaml": "x: 1\n# pin ghcr.io/berriai/litellm\n"}, all,
			[]string{"k8s/f.yaml:2: # pin ghcr.io/berriai/litellm has no value line below it"}},
		{"a marker that is none", map[string]string{"k8s/f.yaml": "# pin the base image\n"}, all,
			[]string{"k8s/f.yaml:1: \"# pin the base image\" is no marker"}},
		{"a marker outside every part", map[string]string{"other/f.yaml": "# pin chart temporal\nversion: 1.7.0\n"}, all,
			[]string{"other/f.yaml:1: # pin chart temporal lies in no part: add other/f.yaml to the paths of a part"}},
		{"a marker in a hand part", map[string]string{"hand/f.yaml": "# pin chart temporal\nversion: 1.7.0\n"},
			[]policy.Part{{Name: "h", Lane: policy.LaneHand, Paths: []string{"hand"}}, {Name: "x", Lane: policy.LaneAlone, Paths: []string{"hand/f.yaml"}}},
			[]string{"hand/f.yaml:1: # pin chart temporal lies in the hand part h, which carries no marker"}},
		{"a marker in two parts", map[string]string{"k8s/f.yaml": "# pin chart temporal\nversion: 1.7.0\n"},
			[]policy.Part{{Name: "one", Lane: policy.LaneAlone, Paths: []string{"k8s"}}, {Name: "two", Lane: policy.LaneHuman, Paths: []string{"k8s/f.yaml"}}},
			[]string{"k8s/f.yaml:1: # pin chart temporal lies in the parts one and two"}},
		{"an unmarked digest", map[string]string{"k8s/f.yaml": "a: 1\nimage: x:1@" + dg("1") + "\n"}, all,
			[]string{"k8s/f.yaml:2: a sha256 digest with no marker above it, in no part of lane hand"}},
		{"a downgrade", map[string]string{"k8s/f.yaml": "# pin chart temporal\nversion: 1.9.0\n"}, all,
			[]string{"k8s/f.yaml:1: # pin chart temporal: the tree holds 1.9.0, newer than the list's 1.8.0: a human moved the tree by hand and not the list"}},
		{"a downgrade of a tag", map[string]string{"k8s/f.yaml": "# pin ghcr.io/berriai/litellm\ntag: \"1.200.0@" + old + "\"\n"}, all,
			[]string{"the tree holds 1.200.0, newer than the list's 1.108.17"}},
		{"a chart marker over a digest", map[string]string{"k8s/f.yaml": "# pin chart temporal\nref: " + old + "\n"}, all,
			[]string{"a chart marker names a version"}},
		{"a chart with no line", map[string]string{"k8s/f.yaml": "# pin chart nothing\nversion: 1.0.0\n"}, all,
			[]string{"no helm line of upstream-images.txt has the archive nothing-<version>.tgz"}},
		{"an image-of with no line", map[string]string{"k8s/f.yaml": "# pin image-of x.tgz\ntag: \"1\"\n"}, all,
			[]string{"no onto line of upstream-images.txt has a URL ending in /x.tgz"}},
		{"a file package with no line", map[string]string{"k8s/f.yaml": "# pin file nothing\nv: 1.0.0\n"}, all,
			[]string{"no line of upstream-files.txt has the package nothing"}},
		{"a file marker over another shape", map[string]string{"k8s/f.yaml": "# pin file node\nimage: a/b\n"}, all,
			[]string{"is no version, URL or 64 hex"}},
		{"a copy that is not in the tree", map[string]string{"k8s/f.yaml": "# pin copy cnpg\n- cnpg.yaml\n"}, all,
			[]string{"names k8s/cnpg.yaml, which the tree does not hold"}},
		{"a copy that leaves the tree", map[string]string{"k8s/f.yaml": "# pin copy cnpg\n- ../../x.yaml\n"}, all,
			[]string{"../../x.yaml leaves the tree"}},
		{"a lane that is none", map[string]string{}, []policy.Part{{Name: "p", Lane: "now"}},
			[]string{"part p: lane \"now\": use alone, human or hand"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(tree(tc.files), tc.parts, store)
			if err == nil {
				t.Fatal("no refusal")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestRunRefusesAWrongStoreCopy(t *testing.T) {
	files := tree(map[string]string{"k8s/f.yaml": "# pin copy cnpg\n- cnpg.yaml\n", "k8s/cnpg.yaml": "old\n"})
	_, err := Run(files, all, fakeStore{copy: []byte("other\n")})
	if err == nil || !strings.Contains(err.Error(), "the store holds a wrong file") {
		t.Fatalf("refusal %v", err)
	}
	_, err = Run(files, all, fakeStore{err: errors.New("boom")})
	if err == nil || !strings.Contains(err.Error(), "the store's copy of cnpg/v1.30.2/cnpg.yaml: boom") {
		t.Fatalf("refusal %v", err)
	}
}

func TestRunNeedsTheLists(t *testing.T) {
	_, err := Run(map[string][]byte{}, all, store)
	if err == nil || !strings.Contains(err.Error(), "the schema copy writes them at the root") {
		t.Fatalf("refusal %v", err)
	}
	files := tree(nil)
	files[ImagesList] = []byte("docker.io/x:1@" + dg("a") + " bogus\n")
	if _, err := Run(files, all, store); err == nil || !strings.Contains(err.Error(), "upstream-images.txt") {
		t.Fatalf("refusal %v", err)
	}
}

// Lines of a hand part, the lists, the copies, the factory's own registry
// path, an all-zero placeholder and binary files are no unmarked digest.
func TestRunUnmarkedDigestExemptions(t *testing.T) {
	d := "image: x@" + dg("1") + "\n"
	files := tree(map[string]string{
		"hand/talos.yaml":         d,
		"k8s/cnpg.yaml":           "sha256:" + strings.Repeat("2", 64) + "\n",
		"k8s/f.yaml":              "# pin copy cnpg\n- cnpg.yaml\n",
		"k8s/blob.bin":            "\x00" + d,
		"k8s/pinned.yaml":         "# pin chart temporal\nversion: 1.8.0\n",
		"k8s/marked-it.txt":       "# pin ghcr.io/berriai/litellm\nimage: l:1.108.17@" + dg("d") + "\n",
		"k8s/own.yaml":            "image: git.aisoftwarefactory.world/sf/factory-gateway:main@" + dg("3") + "\n",
		"k8s/zero.yaml":           "digest: \"" + dg("0") + "\"\n",
		"factory/projects/p.yaml": "  chromium:\n    digest: " + dg("6") + "\n",
	})
	_, err := Run(files, append([]policy.Part{{Name: "hand", Lane: policy.LaneHand, Paths: []string{"hand"}}}, all...), Offline{})
	if err != nil {
		t.Fatal(err)
	}
	files["k8s/zero.yaml"] = []byte("digest: \"" + dg("0") + "\" other: " + dg("4") + "\n")
	if _, err := Run(files, all, Offline{}); err == nil || !strings.Contains(err.Error(), "k8s/zero.yaml:1: a sha256 digest with no marker") {
		t.Fatalf("a zero digest beside a real one: %v", err)
	}
}

func TestValues(t *testing.T) {
	files := tree(map[string]string{
		"k8s/f.yaml": "# pin ghcr.io/berriai/litellm\ntag: 1\n# pin ghcr.io/berriai/litellm\ntag: 2\n# pin copy cnpg\n- c.yaml\n",
		"k8s/c.yaml": "x",
		"README.md":  "# pin chart nothing\nversion: 1\n",
	})
	got := Values(files)
	want := map[string]string{
		"k8s/f.yaml#ghcr.io/berriai/litellm#1": "tag: 1",
		"k8s/f.yaml#ghcr.io/berriai/litellm#2": "tag: 2",
		"k8s/f.yaml#copy cnpg#1":               sumHex([]byte("x")),
		"README.md#chart nothing#1":            "version: 1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("values %v, want %v", got, want)
	}
}
