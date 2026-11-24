package bump

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
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

func TestPGDGServesTheNewestOfEachPackage(t *testing.T) {
	w, _ := newWeb(map[string]answer{
		"GET https://apt.postgresql.org/pub/repos/apt/dists/bookworm-pgdg/main/binary-amd64/Packages.gz": {body: gzipped(t, recorded(t, "pgdg-Packages"))},
	})
	got, err := pgdgNewest(context.Background(), w, "bookworm-pgdg")
	want := map[string]string{"libpq5": "19.0-2.pgdg12+1", "postgresql-17": "17.12-1.pgdg12+1", "postgresql-common": "297.pgdg12+1"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("pgdgNewest = %v, %v; want %v", got, err, want)
	}
}

// The database's version is its modified time; its sum the sha256 of the
// zip, downloaded again when the time moved during the download.
func TestVulnDBPinsTheZipOfOneModifiedTime(t *testing.T) {
	ctx := context.Background()
	w, _ := newWeb(map[string]answer{
		"GET https://vuln.go.dev/index/db.json.gz": {body: gzipped(t, `{"modified":"2026-11-24T15:56:00Z"}`)},
		"GET https://vuln.go.dev/vulndb.zip":       {body: "the zip\n"},
	})
	version, sum, err := vulnDB(ctx, w)
	// printf 'the zip\n' | sha256sum
	if err != nil || version != "20261124T155600Z" || sum != "1580a4aaf1d72f6c3ef898c9e776a1a5f767eb493ed8f5821b9f88a8e766c3bd" {
		t.Errorf("vulnDB = %q %q %v", version, sum, err)
	}
	w, f := newWeb(map[string]answer{"GET https://vuln.go.dev/vulndb.zip": {body: "the zip\n"}})
	f.seq = map[string][]answer{"GET https://vuln.go.dev/index/db.json.gz": {
		{body: gzipped(t, `{"modified":"2026-11-24T15:56:00Z"}`)},
		{body: gzipped(t, `{"modified":"2026-11-24T17:17:00Z"}`)},
		{body: gzipped(t, `{"modified":"2026-11-24T17:17:00Z"}`)},
		{body: gzipped(t, `{"modified":"2026-11-24T17:17:00Z"}`)},
	}}
	if version, _, err := vulnDB(ctx, w); err != nil || version != "20261124T171700Z" {
		t.Errorf("a database that moved during the download: vulnDB = %q %v, want the second time", version, err)
	}
}
