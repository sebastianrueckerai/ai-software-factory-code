package bump

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		hub + "library/alpine/tags/list?n=1000":                                                tagList("3.18.12", "3.18.11"),
		quay + "cilium/cilium-envoy/tags/list?n=1000":                                          tagList("v1.37.7-1791291218-293ab4c1e2b1fec84de8fc5a2e3017889aaeac4d"),
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
		"GET https://vuln.go.dev/index/db.json.gz":                                             {body: recorded(t, "vuln-index-db.json.gz")},
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
		"GET https://vuln.go.dev/vulndb.zip":                 {body: vulnZip(t, "2026-11-24T15:56:00Z", "index/db.json", "index/modules.json", "index/vulns.json")},
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
			"govulndb 20261105T234957Z → 20261124T155600Z-d2ea60770940",
			"opencode v1.19.5 → v1.19.6",
			"node v24.22.2 → v24.22.3",
			"DEBIAN_SNAPSHOT 20261108T000000Z → 20261123T000000Z",
			"postgresql-17 17.11-1.pgdg12+2 → 17.12-1.pgdg12+1",
			"libpq5 18.6-1.pgdg12+2 → 18.7-1.pgdg12+2",
			"postgresql-common 293.pgdg12+1 → 297.pgdg12+1",
		},
		Held: []string{
			"quay.io/cilium/cilium-envoy v1.37.7-1791291218-293ab4c1e2b1fec84de8fc5a2e3017889aaeac4d: quay.io/cilium/cilium moved to v1.20.5; quay.io/cilium/cilium-envoy is held and moves by hand with it",
		},
		HeldKept: 1,
		Newer:    []string{"docker.io/library/golang 1.27.4-alpine", "docker.io/library/golang 1.27.4", "opencode v2.4.1", "node v26.13.1", "libpq5 19.0-2.pgdg12+1"},
	}
	if !reflect.DeepEqual(report, want) {
		t.Errorf("report\n%#v\nwant\n%#v", report, want)
	}
	for _, a := range f.asked {
		if strings.Contains(a, "alpine/manifests") || strings.Contains(a, "cilium-envoy/manifests") || strings.Contains(a, "admin-tools/tags") || strings.Contains(a, "operator-generic/tags") {
			t.Errorf("asked %s: a held line asks for its tags only, a with line none", a)
		}
	}
}

// Run 8 of the bump (pull request #2326): vuln.go.dev's index still named
// 2026-11-24T15:56:00Z, the version main pinned with the sha256 of the zip
// the store keeps under it, but vuln.go.dev had rebuilt the zip with the
// same entries in another order (index/vulns.json first in the store's
// copy, index/db.json first in the new one, both 3680074 bytes): other
// bytes under one version. The pin stays, and the zip is not fetched.
func TestAVulnDBVersionTheIndexStillNamesKeepsItsPin(t *testing.T) {
	const sum = "217647abd48cf32d8df308fe5fa58cfac42aad05190c0dc36ee3f7639ba0e4df"
	files := map[string]string{
		"upstream-files.txt": "govulndb 20261124T155600Z vulndb.zip https://vuln.go.dev/vulndb.zip " + sum + "\n",
		"Dockerfile.runner":  "ARG GOVULNDB_VERSION=20261124T155600Z\nADD --checksum=sha256:" + sum + " https://git.aisoftwarefactory.world/api/packages/upstream/generic/govulndb/${GOVULNDB_VERSION}/vulndb.zip /vulndb.zip\n",
	}
	w, f := newWeb(map[string]answer{
		"GET https://vuln.go.dev/index/db.json.gz": {body: recorded(t, "vuln-index-db.json.gz")},
		"GET https://vuln.go.dev/vulndb.zip":       {body: vulnZip(t, "2026-11-24T15:56:00Z", "index/db.json", "index/modules.json", "index/vulns.json")},
	})
	changed, report := Bump(context.Background(), w, files)
	if len(changed) != 0 || len(report.Moved) != 0 || len(report.Failed) != 0 {
		t.Errorf("changed %v, moved %q, failed %q: want the pin as it is", changed, report.Moved, report.Failed)
	}
	if slices.Contains(f.asked, "GET https://vuln.go.dev/vulndb.zip") {
		t.Error("the zip of the version the pin names was fetched")
	}
}

// A line whose download moves while its version label stays would put
// other bytes under a version the store already holds, which it never
// serves: the line stays and says why.
func TestAFileLineWhoseBytesMoveUnderItsVersionStays(t *testing.T) {
	const line = "cosign stable cosign-linux-amd64 https://github.com/sigstore/cosign/releases/download/v3.1.4/cosign-linux-amd64 1580a4aaf1d72f6c3ef898c9e776a1a5f767eb493ed8f5821b9f88a8e766c3bd\n"
	w, _ := newWeb(map[string]answer{
		gh + "sigstore/cosign/releases?per_page=100":                                         releaseList("v3.1.5", "v3.1.4"),
		"GET https://github.com/sigstore/cosign/releases/download/v3.1.5/cosign-linux-amd64": {body: "cosign v3.1.5\n"},
	})
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-files.txt": line})
	want := []string{"cosign stable: https://github.com/sigstore/cosign/releases/download/v3.1.5/cosign-linux-amd64 is other bytes under the same version, which the store never serves; the line stays"}
	if len(changed) != 0 || len(report.Moved) != 0 || !reflect.DeepEqual(report.Failed, want) {
		t.Errorf("changed %v, moved %q, failed %q; want the line as it is and %q", changed, report.Moved, report.Failed, want)
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

// The text lists what moved, then the held lines that have news and how
// many have none, then the newer lines and what failed.
func TestTheReportReadsInItsOrder(t *testing.T) {
	r := Report{Moved: []string{"quay.io/cilium/cilium v1.20.5 → v1.20.6"},
		Held:     []string{"quay.io/cilium/cilium-envoy v1.39.6-1: quay.io/cilium/cilium moved to v1.20.6; quay.io/cilium/cilium-envoy is held and moves by hand with it"},
		HeldKept: 3,
		Newer:    []string{"opencode v2.4.1"},
		Failed:   []string{"node v24.22.3: GET https://nodejs.org/dist/index.json: 503 Service Unavailable"}}
	want := "The factory's lookup moved the pins below to the newest release of their lines: upstream-bump, run in the bump pod.\n" +
		"\nMoved:\n- quay.io/cilium/cilium v1.20.5 → v1.20.6\n" +
		"\nHeld by rule (hold in the lists), not moved:\n- quay.io/cilium/cilium-envoy v1.39.6-1: quay.io/cilium/cilium moved to v1.20.6; quay.io/cilium/cilium-envoy is held and moves by hand with it\n- held lines with nothing newer: 3\n" +
		"\nNewer lines exist, which the rules of upstream-images.txt or a pin's major leave:\n- opencode v2.4.1\n" +
		"\nNot resolved, kept as they were:\n- node v24.22.3: GET https://nodejs.org/dist/index.json: 503 Service Unavailable\n"
	if got := r.Text(); got != want {
		t.Errorf("Text =\n%s\nwant\n%s", got, want)
	}
}

func sumOf(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// A held line is looked up and never moved. The text names it when its
// maker has a newer version (held at, newest), when the lookup fails, or
// when its lead moved, and only counts the held lines with nothing newer.
func TestAHeldLineIsLookedUpAndNeverMoved(t *testing.T) {
	list := "docker.io/a/one:1.2.3@sha256:aa hold\n" +
		"docker.io/a/two:2.0.0@sha256:bb hold\n" +
		"docker.io/a/three:4.5.6@sha256:cc hold\n" +
		"docker.io/a/four:5.0.0@sha256:dd hold\n" +
		"docker.io/a/lead:1.0.0@sha256:ee\n" +
		"docker.io/a/follower:1.0.0@sha256:ff hold with docker.io/a/lead\n"
	w, _ := newWeb(map[string]answer{
		hub + "a/one/tags/list?n=1000":      tagList("1.2.3", "1.2.4", "1.3.0"),
		hub + "a/two/tags/list?n=1000":      tagList("2.0.0", "3.0.0"),
		hub + "a/three/tags/list?n=1000":    tagList("4.5.6", "4.5.5"),
		hub + "a/lead/tags/list?n=1000":     tagList("1.0.0", "1.0.1"),
		hub + "a/lead/manifests/1.0.1":      index("sha256:" + strings.Repeat("1", 64)),
		hub + "a/follower/tags/list?n=1000": tagList("1.0.0"),
	})
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-images.txt": list})
	if want := strings.Replace(list, "a/lead:1.0.0@sha256:ee", "a/lead:1.0.1@sha256:"+strings.Repeat("1", 64), 1); changed["upstream-images.txt"] != want {
		t.Errorf("list\n%s\nwant\n%s", changed["upstream-images.txt"], want)
	}
	wantHeld := []string{
		"docker.io/a/one held at 1.2.3, newest 1.3.0",
		"docker.io/a/two held at 2.0.0, newest 3.0.0",
		"docker.io/a/four 5.0.0: not looked up: GET https://registry-1.docker.io/v2/a/four/tags/list?n=1000: 404 Not Found no recorded answer",
		"docker.io/a/follower 1.0.0: docker.io/a/lead moved to 1.0.1; docker.io/a/follower is held and moves by hand with it",
	}
	if !reflect.DeepEqual(report.Held, wantHeld) || report.HeldKept != 1 || len(report.Failed) != 0 {
		t.Errorf("held %q kept %d failed %q; want %q, 1 and none", report.Held, report.HeldKept, report.Failed, wantHeld)
	}
}

// A held line of upstream-files.txt is looked up the same way, and its
// download is not fetched.
func TestAHeldFileLineNamesItsNewestRelease(t *testing.T) {
	const line = "cosign v3.1.4 cosign-linux-amd64 https://github.com/sigstore/cosign/releases/download/v3.1.4/cosign-linux-amd64 1580a4aaf1d72f6c3ef898c9e776a1a5f767eb493ed8f5821b9f88a8e766c3bd hold\n"
	w, f := newWeb(map[string]answer{gh + "sigstore/cosign/releases?per_page=100": releaseList("v3.1.5", "v3.1.4")})
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-files.txt": line})
	if want := []string{"cosign held at v3.1.4, newest v3.1.5"}; len(changed) != 0 || !reflect.DeepEqual(report.Held, want) || len(report.Moved) != 0 {
		t.Errorf("changed %v, held %q, moved %q; want the line as it is and %q", changed, report.Held, report.Moved, want)
	}
	for _, a := range f.asked {
		if strings.Contains(a, "/download/") {
			t.Errorf("asked %s: a held line fetches no file", a)
		}
	}
}

// A file line that follows an image line takes that line's version and no
// lookup of its own: the manifest of a maker's release moves with the
// image the release ships, and the file's name and URL carry the version
// as the lead's tag spells it.
func TestAFileLineFollowsItsImageLine(t *testing.T) {
	const (
		cnpg   = "ghcr.io/cloudnative-pg/cloudnative-pg"
		barman = "ghcr.io/cloudnative-pg/plugin-barman-cloud"
		oldSum = "1580a4aaf1d72f6c3ef898c9e776a1a5f767eb493ed8f5821b9f88a8e766c3bd"
	)
	images := cnpg + ":1.30.1@sha256:" + strings.Repeat("a", 64) + " bootstrap\n" +
		barman + ":v0.15.0@sha256:" + strings.Repeat("b", 64) + " bootstrap\n"
	files := "cnpg-operator 1.30.1 cnpg-1.30.1.yaml https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml " + oldSum + " with " + cnpg + "\n" +
		"barman-cloud-plugin v0.15.0 manifest.yaml https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.15.0/manifest.yaml " + oldSum + " with " + barman + "\n"
	const cnpgBody, barmanBody = "cnpg 1.30.2\n", "barman v0.16.0\n"
	w, f := newWeb(map[string]answer{
		"GET https://ghcr.io/v2/cloudnative-pg/cloudnative-pg/tags/list?n=1000":                             tagList("1.30.1", "1.30.2"),
		"GET https://ghcr.io/v2/cloudnative-pg/cloudnative-pg/manifests/1.30.2":                             index("sha256:" + strings.Repeat("c", 64)),
		"GET https://ghcr.io/v2/cloudnative-pg/plugin-barman-cloud/tags/list?n=1000":                        tagList("v0.15.0", "v0.16.0"),
		"GET https://ghcr.io/v2/cloudnative-pg/plugin-barman-cloud/manifests/v0.16.0":                       index("sha256:" + strings.Repeat("d", 64)),
		"GET https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.2/cnpg-1.30.2.yaml":   {body: cnpgBody},
		"GET https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.16.0/manifest.yaml": {body: barmanBody},
	})
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-images.txt": images, "upstream-files.txt": files})
	want := "cnpg-operator 1.30.2 cnpg-1.30.2.yaml https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.2/cnpg-1.30.2.yaml " + sumOf(cnpgBody) + " with " + cnpg + "\n" +
		"barman-cloud-plugin v0.16.0 manifest.yaml https://github.com/cloudnative-pg/plugin-barman-cloud/releases/download/v0.16.0/manifest.yaml " + sumOf(barmanBody) + " with " + barman + "\n"
	if changed["upstream-files.txt"] != want {
		t.Errorf("files\n%s\nwant\n%s", changed["upstream-files.txt"], want)
	}
	if len(report.Failed) != 0 {
		t.Errorf("failed %q", report.Failed)
	}
	for _, a := range f.asked {
		if strings.Contains(a, "api.github.com") {
			t.Errorf("asked %s: a file line that follows an image line looks up no release", a)
		}
	}
}

// A file line whose lead no line of the list gives stays and says why.
func TestAFileLineWithoutItsLeadStays(t *testing.T) {
	const line = "cnpg-operator 1.30.1 cnpg-1.30.1.yaml https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml 1580a4aaf1d72f6c3ef898c9e776a1a5f767eb493ed8f5821b9f88a8e766c3bd with ghcr.io/cloudnative-pg/cloudnative-pg\n"
	w, _ := newWeb(map[string]answer{})
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-files.txt": line})
	want := []string{"cnpg-operator 1.30.1: with ghcr.io/cloudnative-pg/cloudnative-pg: no line of that name leads it (one, not held, in the list)"}
	if len(changed) != 0 || !reflect.DeepEqual(report.Failed, want) {
		t.Errorf("changed %v, failed %q; want nothing changed and %q", changed, report.Failed, want)
	}
}

// patch keeps a file line in its minor, as it does an image line: the go
// tool of the ts-go template moves to the newest 1.25 and names the 1.26.
func TestAPatchFileLineStaysInItsMinor(t *testing.T) {
	const old = "1580a4aaf1d72f6c3ef898c9e776a1a5f767eb493ed8f5821b9f88a8e766c3bd"
	const body = "go1.25.14\n"
	w, _ := newWeb(map[string]answer{
		"GET https://go.dev/dl/?mode=json&include=all":       {body: `[{"version":"go1.26.2","stable":true},{"version":"go1.25.14","stable":true},{"version":"go1.25.13","stable":true},{"version":"go1.26rc1","stable":false}]`},
		"GET https://go.dev/dl/go1.25.14.linux-amd64.tar.gz": {body: body},
	})
	list := "go 1.25.13 go1.25.13.linux-amd64.tar.gz https://go.dev/dl/go1.25.13.linux-amd64.tar.gz " + old + " patch\n"
	changed, report := Bump(context.Background(), w, map[string]string{"upstream-files.txt": list})
	want := "go 1.25.14 go1.25.14.linux-amd64.tar.gz https://go.dev/dl/go1.25.14.linux-amd64.tar.gz " + sumOf(body) + " patch\n"
	if changed["upstream-files.txt"] != want || !reflect.DeepEqual(report.Newer, []string{"go go1.26.2"}) {
		t.Errorf("files\n%s\nnewer %q; want\n%s\nand go go1.26.2", changed["upstream-files.txt"], report.Newer, want)
	}
}
