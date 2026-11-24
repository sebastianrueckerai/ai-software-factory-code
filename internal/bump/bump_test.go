package bump

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// readTree reads Files from dir, each that dir holds.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, p := range Files {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		files[p] = string(b)
	}
	return files
}

// index is a registry's answer for an image whose linux/amd64 manifest is
// digest.
func index(digest string) answer {
	return answer{body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"digest":"` + digest + `","platform":{"architecture":"amd64","os":"linux"}},` +
		`{"digest":"sha256:f2f78569955abcecbebdd30ffd9349ac2821580d3d6e3af83b50e5d7cff13591","platform":{"architecture":"unknown","os":"unknown"}}]}`}
}

func tagList(tags ...string) answer {
	return answer{body: `{"tags":["` + strings.Join(tags, `","`) + `"]}`}
}

func releaseList(tags ...string) answer {
	var b strings.Builder
	b.WriteString("[")
	for i, tag := range tags {
		if i > 0 {
			b.WriteString(",")
		}
		pre := strings.Contains(tag, "-rc") || strings.Contains(tag, "-beta")
		b.WriteString(`{"tag_name":"` + tag + `","draft":false,"prerelease":` + map[bool]string{true: "true", false: "false"}[pre] + `}`)
	}
	return answer{body: b.String() + "]"}
}

const (
	hub  = "GET https://registry-1.docker.io/v2/"
	quay = "GET https://quay.io/v2/"
	gh   = "GET https://api.github.com/repos/"
)

// The answers the makers gave for the bump of 2026-11-24 (pull request
// #2309), cut to the lines of testdata/pass: the digests are the ones
// #2309 pinned, a download's body stands for its file, and the PGDG
// archive serves libpq5 19.0 as it did that evening.
func answersOf2309(t *testing.T) map[string]answer {
	return map[string]answer{
		hub + "library/golang/tags/list?n=1000":                                                tagList("1.25-bookworm", "1.26.10", "1.26.10-alpine", "1.26.11", "1.26.11-alpine", "1.27.4", "1.27.4-alpine", "1.27rc2"),
		hub + "library/golang/manifests/1.26.11-alpine":                                        index("sha256:f496f8c537ed506626ee1a6913afb32c45068a48697163c088f604cc775a0c38"),
		hub + "library/golang/manifests/1.26.11":                                               index("sha256:6327cb290853a5d13091c436b88259b4f35ff70e49b1f8d9e5d1dd1757e9c6ba"),
		hub + "library/node/tags/list?n=1000":                                                  tagList("22-bookworm-slim", "22.22.0-bookworm-slim", "24-bookworm-slim"),
		hub + "library/node/manifests/22-bookworm-slim":                                        index("sha256:d20a7b8b4184bf74fe77499b5460a3d69e582bc733d50287e40c3fe791764851"),
		hub + "library/debian/tags/list?n=1000":                                                tagList("bookworm", "bookworm-20261104-slim", "bookworm-20261123", "bookworm-20261123-slim", "bookworm-slim"),
		hub + "library/debian/manifests/bookworm-slim":                                         index("sha256:311f7a192fd9a92b165a86b32fa3aae9005b11849bfb1db2d4fd784559ee464f"),
		"HEAD https://registry-1.docker.io/v2/library/debian/manifests/bookworm-slim":          {header: map[string]string{"Docker-Content-Digest": "sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e"}},
		"HEAD https://registry-1.docker.io/v2/library/debian/manifests/bookworm-20261123-slim": {header: map[string]string{"Docker-Content-Digest": "sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e"}},
		hub + "moby/buildkit/tags/list?n=1000":                                                 tagList("buildx-stable-1", "v0.33.0", "v0.34.2", "v0.35.1", "v0.35.1-rootless", "v0.36.0-rc1"),
		hub + "moby/buildkit/manifests/v0.35.1":                                                index("sha256:7583a0667093d941826bcd1d2878d6a360dfd08a82c93e6d6a6f6cce78801e12"),
		hub + "temporalio/server/tags/list?n=1000":                                             tagList("1.31.4", "1.32.1", "1.32.2", "latest"),
		hub + "temporalio/server/manifests/1.32.2":                                             index("sha256:3e11544607762c3187c12a63e289e03beddf46c21ebb52b47ccb77db179d6313"),
		hub + "temporalio/admin-tools/manifests/1.32.2":                                        index("sha256:d6655e288364110b61204fbae5f103232b2c5c78e647a27bc11603d274145809"),
		quay + "cilium/cilium/tags/list?n=1000":                                                tagList("v1.19.9", "v1.20.4", "v1.20.5", "v1.21.0-pre.1"),
		quay + "cilium/cilium/manifests/v1.20.5":                                               index("sha256:a54ced05354670afb7fecd075e91130dff5cca16518da7ff3d1ef093c5c2d0ae"),
		quay + "cilium/operator-generic/manifests/v1.20.5":                                     index("sha256:3362404f43b6a01cf6e370fb637dac427b7b0259ff2629faa9e5b665121a3a76"),
		gh + "SigNoz/charts/releases?per_page=100":                                             releaseList("signoz-mcp-server-1.0.0", "signoz-0.151.0", "signoz-0.150.0", "k8s-infra-0.17.1"),
		gh + "moby/buildkit/releases?per_page=100":                                             {body: recorded(t, "github-moby-buildkit.json")},
		gh + "google/go-containerregistry/releases?per_page=100":                               releaseList("v0.22.1", "v0.22.0"),
		gh + "anomalyco/opencode/releases?per_page=100":                                        releaseList("v2.4.1", "v2.4.0", "v1.19.6", "v1.19.5"),
		"GET https://code.forgejo.org/api/v1/repos/forgejo/runner/releases?limit=50":           {body: recorded(t, "forgejo-forgejo-runner.json")},
		"GET https://nodejs.org/dist/index.json":                                               {body: recorded(t, "nodejs-index.json")},
		"GET https://vuln.go.dev/index/db.json.gz":                                             {body: gzipped(t, `{"modified":"2026-11-24T15:56:00Z"}`)},
		"GET https://apt.postgresql.org/pub/repos/apt/dists/bookworm-pgdg/main/binary-amd64/Packages.gz": {body: gzipped(t,
			"Package: libpq5\nVersion: 18.7-1.pgdg12+2\n\nPackage: libpq5\nVersion: 19.0-2.pgdg12+1\n\nPackage: libpq5\nVersion: 19.0-1.pgdg12+1\n\nPackage: libpq5\nVersion: 18.6-1.pgdg12+2\n\n"+
				"Package: postgresql-17\nVersion: 17.11-1.pgdg12+2\n\nPackage: postgresql-17\nVersion: 17.12-1.pgdg12+1\n\n"+
				"Package: postgresql-18\nVersion: 18.7-1.pgdg12+2\n\nPackage: postgresql-common\nVersion: 297.pgdg12+1\n")},
		// the downloads: each body's sha256 stands in testdata/pass/after
		"GET https://github.com/SigNoz/charts/releases/download/signoz-0.151.0/signoz-0.151.0.tgz":                {body: "signoz-0.151.0.tgz\n"},
		"GET https://github.com/moby/buildkit/releases/download/v0.35.1/buildkit-v0.35.1.linux-amd64.tar.gz":      {body: "buildkit v0.35.1\n"},
		"GET https://code.forgejo.org/forgejo/runner/releases/download/v13.2.2/forgejo-runner-13.2.2-linux-amd64": {body: "forgejo-runner v13.2.2\n"},
		"GET https://github.com/anomalyco/opencode/releases/download/v1.19.6/opencode-linux-x64.tar.gz":           {body: "opencode v1.19.6\n"},
		"GET https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz":                                     {body: "node v24.22.3\n"},
		"GET https://vuln.go.dev/vulndb.zip":                 {body: "vulndb 20261124T155600Z\n"},
		"GET https://go.dev/dl/go1.26.11.linux-amd64.tar.gz": {body: "go1.26.11\n"},
	}
}

// Last week's files of testdata/pass/before become this week's of
// testdata/pass/after, which are #2309's but where the rule differs:
// DEBIAN_SNAPSHOT is the date of the Debian image (bookworm-20261123-slim)
// where #2309 wrote 20261122T000000Z by hand, cilium-envoy holds where
// #2309 moved it with Cilium's chart, and a download's sha256 is its
// recorded body's.
func TestABumpTurnsLastWeeksPinsIntoThisWeeks(t *testing.T) {
	w, f := newWeb(answersOf2309(t))
	before, after := readTree(t, filepath.Join("testdata", "pass", "before")), readTree(t, filepath.Join("testdata", "pass", "after"))
	changed, report := Bump(context.Background(), w, before)
	for _, p := range Files {
		got, ok := changed[p]
		if !ok {
			got = before[p]
		}
		if got != after[p] {
			t.Errorf("%s:\n%s\nwant\n%s", p, got, after[p])
		}
	}
	want := Report{
		Moved: []string{
			"docker.io/library/golang 1.26.10-alpine → 1.26.11-alpine",
			"docker.io/library/golang 1.26.10 → 1.26.11",
			"docker.io/library/node 22-bookworm-slim: a new digest",
			"docker.io/library/debian bookworm-slim: a new digest",
			"docker.io/moby/buildkit v0.33.0 → v0.35.1",
			"signoz-0.149.0 → signoz-0.151.0",
			"quay.io/cilium/cilium v1.20.4 → v1.20.5",
			"quay.io/cilium/operator-generic v1.20.4 → v1.20.5",
			"buildkit v0.34.2 → v0.35.1",
			"forgejo-runner v13.2.1 → v13.2.2",
			"govulndb 20261105T234957Z → 20261124T155600Z",
			"opencode v1.19.5 → v1.19.6",
			"node v24.22.2 → v24.22.3",
			"DEBIAN_SNAPSHOT 20261108T000000Z → 20261123T000000Z",
			"postgresql-17 17.11-1.pgdg12+2 → 17.12-1.pgdg12+1",
			"libpq5 18.6-1.pgdg12+2 → 18.7-1.pgdg12+2",
			"postgresql-common 293.pgdg12+1 → 297.pgdg12+1",
		},
		Held: []string{
			"docker.io/library/alpine 3.18.12",
			"quay.io/cilium/cilium-envoy v1.37.7-1791291218-293ab4c1e2b1fec84de8fc5a2e3017889aaeac4d: quay.io/cilium/cilium moved to v1.20.5; quay.io/cilium/cilium-envoy is held and moves by hand with it",
		},
		Newer: []string{"docker.io/library/golang 1.27.4-alpine", "docker.io/library/golang 1.27.4", "opencode v2.4.1", "node v26.13.1", "libpq5 19.0-2.pgdg12+1"},
	}
	if !reflect.DeepEqual(report, want) {
		t.Errorf("report\n%#v\nwant\n%#v", report, want)
	}
	for _, a := range f.asked {
		if strings.Contains(a, "alpine/manifests") || strings.Contains(a, "alpine/tags") || strings.Contains(a, "cilium-envoy") || strings.Contains(a, "admin-tools/tags") || strings.Contains(a, "operator-generic/tags") {
			t.Errorf("asked %s: a held line asks nothing, a with line no tags", a)
		}
	}
}

// One pin the bump cannot resolve stays as it is and stands in the report
// with why; the other pins move.
func TestAPinThatCannotBeResolvedStaysAndIsNamed(t *testing.T) {
	answers := answersOf2309(t)
	delete(answers, gh+"anomalyco/opencode/releases?per_page=100")
	w, _ := newWeb(answers)
	before := readTree(t, filepath.Join("testdata", "pass", "before"))
	changed, report := Bump(context.Background(), w, before)
	want := []string{"opencode v1.19.5: GET https://api.github.com/repos/anomalyco/opencode/releases?per_page=100: 404 Not Found no recorded answer"}
	if !reflect.DeepEqual(report.Failed, want) {
		t.Errorf("failed %q, want %q", report.Failed, want)
	}
	for _, p := range []string{"Dockerfile.sandbox", "upstream-files.txt"} {
		if !strings.Contains(changed[p], "v1.19.5") || strings.Contains(changed[p], "v1.19.6") {
			t.Errorf("%s moved opencode or nothing:\n%s", p, changed[p])
		}
	}
	if _, ok := changed["opencode-plugin/package.json"]; ok {
		t.Error("the plugin moved without its opencode line")
	}
	if !strings.Contains(changed["upstream-files.txt"], "buildkit v0.35.1") {
		t.Error("the other lines did not move")
	}
	if !strings.Contains(report.Text(), "Not resolved, kept as they were:\n- opencode v1.19.5: GET") {
		t.Errorf("the text names no failure:\n%s", report.Text())
	}
}

// A with line whose lead is held, absent or named twice is no lead: the
// line stays and says why.
func TestAWithLineNeedsOneLeadThatMoves(t *testing.T) {
	w, _ := newWeb(map[string]answer{})
	list := "docker.io/library/golang:1.26.10@sha256:aa hold\n" +
		"docker.io/library/golang:1.26.10-alpine@sha256:bb with docker.io/library/golang\n"
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-images.txt": list})
	want := []string{"docker.io/library/golang:1.26.10-alpine@sha256:bb: with docker.io/library/golang: no line of that name leads it (one, not held, in the list)"}
	if len(changed) != 0 || !reflect.DeepEqual(report.Failed, want) {
		t.Errorf("changed %v, failed %q; want nothing changed and %q", changed, report.Failed, want)
	}
}

// The text lists what moved, then each held line with its tag, a held line
// whose lead moved saying so, then the newer lines and what failed.
func TestTheReportReadsInItsOrder(t *testing.T) {
	r := Report{Moved: []string{"quay.io/cilium/cilium v1.20.5 → v1.20.6"},
		Held:   []string{"quay.io/cilium/cilium-envoy v1.39.6-1: quay.io/cilium/cilium moved to v1.20.6; quay.io/cilium/cilium-envoy is held and moves by hand with it"},
		Newer:  []string{"opencode v2.4.1"},
		Failed: []string{"node v24.22.3: GET https://nodejs.org/dist/index.json: 503 Service Unavailable"}}
	want := "The factory's lookup moved the pins below to the newest release of their lines: upstream-bump, run in the bump pod.\n" +
		"\nMoved:\n- quay.io/cilium/cilium v1.20.5 → v1.20.6\n" +
		"\nHeld by rule (hold in upstream-images.txt), not moved:\n- quay.io/cilium/cilium-envoy v1.39.6-1: quay.io/cilium/cilium moved to v1.20.6; quay.io/cilium/cilium-envoy is held and moves by hand with it\n" +
		"\nNewer lines exist, which the rules of upstream-images.txt or a pin's major leave:\n- opencode v2.4.1\n" +
		"\nNot resolved, kept as they were:\n- node v24.22.3: GET https://nodejs.org/dist/index.json: 503 Service Unavailable\n"
	if got := r.Text(); got != want {
		t.Errorf("Text =\n%s\nwant\n%s", got, want)
	}
}
