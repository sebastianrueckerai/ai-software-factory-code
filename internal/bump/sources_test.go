package bump

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const hubChallenge = `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/golang:pull"`

// Docker Hub asks for a token, then pages its tags by the Link header.
func TestRegistryTagsTakeATokenAndFollowThePages(t *testing.T) {
	w, f := newWeb(map[string]answer{
		"GET https://auth.docker.io/token?scope=repository%3Alibrary%2Fgolang%3Apull&service=registry.docker.io": {body: `{"token":"t1","access_token":"t1"}`},
		"GET https://registry-1.docker.io/v2/library/golang/tags/list?n=1000": {token: "t1", challenge: hubChallenge,
			header: map[string]string{"Link": `</v2/library/golang/tags/list?last=1.26.10-alpine&n=1000>; rel="next"`},
			body:   `{"name":"library/golang","tags":["1.26.10","1.26.10-alpine"]}`},
		"GET https://registry-1.docker.io/v2/library/golang/tags/list?last=1.26.10-alpine&n=1000": {token: "t1", challenge: hubChallenge,
			body: `{"name":"library/golang","tags":["1.26.11","1.26.11-alpine","1.27.4"]}`},
	})
	got, err := registryTags{"docker.io/library/golang"}.tags(context.Background(), w)
	if want := []string{"1.26.10", "1.26.10-alpine", "1.26.11", "1.26.11-alpine", "1.27.4"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("tags = %v, %v; want %v", got, err, want)
	}
	if n := slices.Index(f.asked, "GET https://auth.docker.io/token?scope=repository%3Alibrary%2Fgolang%3Apull&service=registry.docker.io"); n != 1 || len(f.asked) != 4 {
		t.Errorf("asked %q: want one token, taken after the first 401, then both pages", f.asked)
	}
}

// An index's linux/amd64 entry is the digest; a single manifest's own is
// its Docker-Content-Digest.
func TestAmd64DigestReadsAnIndexOrASingleManifest(t *testing.T) {
	w, _ := newWeb(map[string]answer{
		"GET https://registry-1.docker.io/v2/library/golang/manifests/1.26.11": {body: recorded(t, "dockerhub-golang-1.26.11-index.json")},
		"GET https://quay.io/v2/jetstack/charts/cert-manager/manifests/v1.21.3": {
			header: map[string]string{"Docker-Content-Digest": "sha256:e482675aec32862d323a9218293ff7de81192337998615e4378c56498473f240"},
			body:   `{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json"}}`},
	})
	for name, want := range map[string]string{
		"docker.io/library/golang:1.26.11":             "sha256:e38b9d87c900de416a12351135e18daa9e477a79faa60fcb108f9b8279b85f8c",
		"quay.io/jetstack/charts/cert-manager:v1.21.3": "sha256:e482675aec32862d323a9218293ff7de81192337998615e4378c56498473f240",
	} {
		n, tag, _ := strings.Cut(name, ":")
		if got, err := amd64Digest(context.Background(), w, n, tag); err != nil || got != want {
			t.Errorf("amd64Digest(%s) = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestReleasesLeaveOutDraftsAndPreReleases(t *testing.T) {
	w, _ := newWeb(map[string]answer{
		"GET https://api.github.com/repos/moby/buildkit/releases?per_page=100":       {body: recorded(t, "github-moby-buildkit.json")},
		"GET https://code.forgejo.org/api/v1/repos/forgejo/runner/releases?limit=50": {body: recorded(t, "forgejo-forgejo-runner.json")},
		"GET https://api.github.com/repos/siderolabs/talos/releases?per_page=100":    {body: recorded(t, "github-siderolabs-talos.json")},
		"GET https://nodejs.org/dist/index.json":                                     {body: recorded(t, "nodejs-index.json")},
	})
	ctx := context.Background()
	for _, c := range []struct {
		src  source
		want []string
	}{
		{githubReleases("moby/buildkit"), []string{"v0.35.1", "v0.35.0", "dockerfile/1.29.0-labs", "dockerfile/1.29.0", "v0.34.2", "v0.34.1", "v0.34.0", "dockerfile/1.28.0-labs"}},
		{forgejoReleases("code.forgejo.org", "forgejo/runner"), []string{"v13.2.2", "v13.2.1", "v13.2.0", "v13.1.0", "v13.0.0", "v12.13.2", "v12.13.1", "v12.13.0"}},
		{imageSource("factory.talos.dev/installer/d9ff"), []string{"v1.14.7", "v1.14.6", "v1.14.5", "v1.13.12", "v1.14.4", "v1.14.3", "v1.13.11"}},
		{nodeReleases{}, []string{"v26.13.1", "v26.13.0", "v25.9.0", "v24.22.3", "v24.22.2", "v24.22.1", "v24.22.0", "v24.21.0"}},
	} {
		if got, err := c.src.tags(ctx, w); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: tags %v, %v; want %v", c.src, got, err, c.want)
		}
	}
}

func TestFileSourcesComeFromTheirURL(t *testing.T) {
	for raw, want := range map[string]struct {
		src source
		tag string
	}{
		"https://github.com/moby/buildkit/releases/download/v0.35.1/buildkit-v0.35.1.linux-amd64.tar.gz":                       {githubReleases("moby/buildkit"), "v0.35.1"},
		"https://github.com/SigNoz/signoz/releases/download/histogram-quantile%2Fv0.0.1/histogram-quantile_linux_amd64.tar.gz": {githubReleases("SigNoz/signoz"), "histogram-quantile/v0.0.1"},
		"https://code.forgejo.org/forgejo/runner/releases/download/v13.2.2/forgejo-runner-13.2.2-linux-amd64":                  {forgejoReleases("code.forgejo.org", "forgejo/runner"), "v13.2.2"},
		"https://nodejs.org/dist/v24.22.3/node-v24.22.3-linux-x64.tar.xz":                                                      {nodeReleases{}, "v24.22.3"},
	} {
		src, tag, ok := fileSource(raw)
		if !ok || !reflect.DeepEqual(src, want.src) || tag != want.tag {
			t.Errorf("fileSource(%s) = %+v %q %v; want %+v %q", raw, src, tag, ok, want.src, want.tag)
		}
	}
	for _, raw := range []string{"https://www.postgresql.org/media/keys/ACCC4CF8.asc", "https://vuln.go.dev/vulndb.zip"} {
		if _, _, ok := fileSource(raw); ok {
			t.Errorf("fileSource(%s) found versions", raw)
		}
	}
}

// The snapshot is the date of the dated tag that names the same index as
// the floating one, read by HEAD, which Docker Hub's pull limit leaves out.
func TestDebianSnapshotIsTheDateOfTheSameImage(t *testing.T) {
	head := func(d string) answer { return answer{header: map[string]string{"Docker-Content-Digest": d}} }
	w, f := newWeb(map[string]answer{
		"HEAD https://registry-1.docker.io/v2/library/debian/manifests/bookworm-slim":          head("sha256:e67e"),
		"GET https://registry-1.docker.io/v2/library/debian/tags/list?n=1000":                  {body: `{"tags":["bookworm","bookworm-20261104","bookworm-20261104-slim","bookworm-20261123-slim","bookworm-slim","trixie-20261123-slim"]}`},
		"HEAD https://registry-1.docker.io/v2/library/debian/manifests/bookworm-20261123-slim": head("sha256:e67e"),
		"HEAD https://registry-1.docker.io/v2/library/debian/manifests/bookworm-20261104-slim": head("sha256:734a"),
	})
	got, err := debianSnapshot(context.Background(), w, "docker.io/library/debian", "bookworm-slim")
	if err != nil || got != "20261123T000000Z" {
		t.Errorf("debianSnapshot = %q, %v; want 20261123T000000Z", got, err)
	}
	for _, a := range f.asked {
		if strings.HasPrefix(a, "GET ") && strings.Contains(a, "/manifests/") {
			t.Errorf("asked %s: a manifest by GET counts against the pull limit", a)
		}
	}
}

func TestPGDGServesEachPackagesVersionsInOrder(t *testing.T) {
	w, _ := newWeb(map[string]answer{
		"GET https://apt.postgresql.org/pub/repos/apt/dists/bookworm-pgdg/main/binary-amd64/Packages.gz": {body: gzipped(t, recorded(t, "pgdg-Packages"))},
	})
	got, err := pgdgVersions(context.Background(), w, "bookworm-pgdg")
	want := map[string][]string{
		"libpq5":            {"18.7-1.pgdg12+2", "19.0-1.pgdg12+1", "19.0-2.pgdg12+1"},
		"postgresql-17":     {"17.10-1.pgdg12+1", "17.11-1.pgdg12+2", "17.12-1.pgdg12+1"},
		"postgresql-common": {"294.pgdg12+2", "295.pgdg12+1", "297.pgdg12+1"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("pgdgVersions = %v, %v; want %v", got, err, want)
	}
}

// vuln.go.dev rebuilds the zip under one modified time of its index with
// its entries in another order, other bytes: a modified time no newer than
// the pin's keeps the pin and the zip is not fetched. A new version is the
// modified time inside the zip the program hashed, even when the zip lags
// the index, and the first 12 hex digits of its sha256, so it names those
// bytes alone.
func TestVulnDBNamesTheZipItHashes(t *testing.T) {
	const stored = "217647abd48cf32d8df308fe5fa58cfac42aad05190c0dc36ee3f7639ba0e4df"
	zip1556 := vulnZip(t, "2026-11-24T15:56:00Z", "index/db.json", "index/modules.json", "index/vulns.json")
	sum1556 := sha256.Sum256([]byte(zip1556))
	for name, c := range map[string]struct {
		index, pinned, pinSum string
		version, sum          string
		fetched               bool
	}{
		"the index names the pinned version": {index: recorded(t, "vuln-index-db.json.gz"), pinned: "20261124T155600Z", pinSum: stored,
			version: "20261124T155600Z", sum: stored},
		"the index is older than the pin": {index: gzipped(t, `{"modified":"2026-11-05T23:49:57Z"}`), pinned: "20261124T155600Z", pinSum: stored,
			version: "20261124T155600Z", sum: stored},
		"a newer version is the zip's own": {index: gzipped(t, `{"modified":"2026-11-24T17:17:00Z"}`), pinned: "20261105T234957Z", pinSum: "0a54b7eb56bf031dc214fc4f29ca6db71073ab324595c3de9485f97a334e9e22",
			version: "20261124T155600Z-" + hex.EncodeToString(sum1556[:])[:12], sum: hex.EncodeToString(sum1556[:]), fetched: true},
		"a pin of the same modified time and its bytes": {index: recorded(t, "vuln-index-db.json.gz"), pinned: "20261124T155600Z-a71565a40a0b", pinSum: "a71565a40a0bd2bf5b1b46bcc333df3c4645ea7543c559d87350889c35702674",
			version: "20261124T155600Z-a71565a40a0b", sum: "a71565a40a0bd2bf5b1b46bcc333df3c4645ea7543c559d87350889c35702674"},
		"a zip that lags to the pin keeps it": {index: gzipped(t, `{"modified":"2026-11-24T17:17:00Z"}`), pinned: "20261124T155600Z", pinSum: stored,
			version: "20261124T155600Z", sum: stored, fetched: true},
	} {
		t.Run(name, func(t *testing.T) {
			w, f := newWeb(map[string]answer{
				"GET https://vuln.go.dev/index/db.json.gz": {body: c.index},
				"GET https://vuln.go.dev/vulndb.zip":       {body: zip1556},
			})
			version, sum, err := vulnDB(context.Background(), w, c.pinned, c.pinSum)
			if err != nil || version != c.version || sum != c.sum {
				t.Errorf("vulnDB = %q %q %v; want %q %q", version, sum, err, c.version, c.sum)
			}
			if fetched := slices.Contains(f.asked, "GET https://vuln.go.dev/vulndb.zip"); fetched != c.fetched {
				t.Errorf("zip fetched %v, want %v", fetched, c.fetched)
			}
		})
	}
}

// A burst of tag pages meets ghcr.io's 429: the page is asked again after
// a pause, and a token taken after the pause still counts.
func TestATooManyRequestsAnswerIsAskedAgain(t *testing.T) {
	const tags = "GET https://ghcr.io/v2/cloudnative-pg/postgresql/tags/list?n=1000"
	challenge := `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:cloudnative-pg/postgresql:pull"`
	w, f := newWeb(map[string]answer{
		"GET https://ghcr.io/token?scope=repository%3Acloudnative-pg%2Fpostgresql%3Apull&service=ghcr.io": {body: `{"token":"g1"}`},
	})
	f.seq = map[string][]answer{tags: {
		{status: 429, body: `{"errors":[{"code":"TOOMANYREQUESTS"}]}`},
		{token: "g1", challenge: challenge},
		{token: "g1", challenge: challenge, body: `{"tags":["18.7-system-trixie"]}`},
	}}
	var pauses []int
	w.Pause = func(n int) time.Duration { pauses = append(pauses, n); return 0 }
	got, err := registryTags{"ghcr.io/cloudnative-pg/postgresql"}.tags(context.Background(), w)
	if err != nil || !reflect.DeepEqual(got, []string{"18.7-system-trixie"}) || !reflect.DeepEqual(pauses, []int{0}) {
		t.Errorf("tags %v, %v, pauses %v", got, err, pauses)
	}
}
