package bump

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A source lists the tags a maker offers for one line: an image's tags,
// or the tags of a file's releases.
type source interface {
	tags(ctx context.Context, w *Web) ([]string, error)
}

// registryTags lists an image's tags through its registry's API, page by
// page as the Link header leads.
type registryTags struct{ name string }

func (s registryTags) tags(ctx context.Context, w *Web) ([]string, error) {
	host, repo := registryAPI(s.name)
	var all []string
	next := "/v2/" + repo + "/tags/list?n=1000"
	for next != "" {
		b, h, err := w.get(ctx, "https://"+host+next)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return nil, fmt.Errorf("the tags of %s: %w", s.name, err)
		}
		all = append(all, page.Tags...)
		next = ""
		if m := nextPage.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return all, nil
}

var nextPage = regexp.MustCompile(`<([^>]*)>;\s*rel="next"`)

// releases lists the tags of a repository's releases on GitHub or a
// Forgejo, drafts and pre-releases left out.
type releases struct {
	api string // the releases API's URL
}

func githubReleases(repo string) releases {
	return releases{"https://api.github.com/repos/" + repo + "/releases?per_page=100"}
}

func forgejoReleases(host, repo string) releases {
	return releases{"https://" + host + "/api/v1/repos/" + repo + "/releases?limit=50"}
}

func (s releases) tags(ctx context.Context, w *Web) ([]string, error) {
	b, _, err := w.get(ctx, s.api)
	if err != nil {
		return nil, err
	}
	var rs []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("%s: %w", s.api, err)
	}
	var tags []string
	for _, r := range rs {
		if !r.Draft && !r.Prerelease {
			tags = append(tags, r.Tag)
		}
	}
	return tags, nil
}

// nodeReleases lists Node's versions from nodejs.org's index.
type nodeReleases struct{}

func (nodeReleases) tags(ctx context.Context, w *Web) ([]string, error) {
	b, _, err := w.get(ctx, "https://nodejs.org/dist/index.json")
	if err != nil {
		return nil, err
	}
	var vs []struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &vs); err != nil {
		return nil, fmt.Errorf("nodejs.org's index: %w", err)
	}
	tags := make([]string, len(vs))
	for i, v := range vs {
		tags[i] = v.Version
	}
	return tags, nil
}

// goReleases lists Go's stable versions (go1.25.14) from go.dev's download
// index, every minor's, not the newest two only.
type goReleases struct{}

func (goReleases) tags(ctx context.Context, w *Web) ([]string, error) {
	b, _, err := w.get(ctx, "https://go.dev/dl/?mode=json&include=all")
	if err != nil {
		return nil, err
	}
	var rs []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("go.dev's index: %w", err)
	}
	var tags []string
	for _, r := range rs {
		if r.Stable {
			tags = append(tags, r.Version)
		}
	}
	return tags, nil
}

var goTarball = regexp.MustCompile(`^/dl/(go\d+(?:\.\d+)+)\.`)

// registryAPI is the host and repository of an image name's registry API.
func registryAPI(name string) (host, repo string) {
	host, repo, _ = strings.Cut(name, "/")
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return host, repo
}

// imageSource is where an image's tags are listed: its registry, or for a
// registry that lists none the releases that name its versions.
func imageSource(name string) source {
	if host, _ := registryAPI(name); host == "factory.talos.dev" {
		return githubReleases("siderolabs/talos")
	}
	return registryTags{name}
}

// fileSource is where the versions of a file's URL are listed, and the tag
// the URL holds: a GitHub or Forgejo release download, nodejs.org's dist,
// or a Go tarball of go.dev. A URL of none has no versions: its line keeps its URL.
func fileSource(raw string) (source, string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", false
	}
	parts := strings.Split(u.EscapedPath(), "/")
	switch {
	case u.Host == "go.dev" && goTarball.MatchString(u.Path):
		return goReleases{}, goTarball.FindStringSubmatch(u.Path)[1], true
	case u.Host == "nodejs.org" && len(parts) > 3 && parts[1] == "dist":
		return nodeReleases{}, parts[2], true
	case len(parts) > 5 && parts[3] == "releases" && parts[4] == "download":
		tag, err := url.PathUnescape(parts[5])
		if err != nil {
			return nil, "", false
		}
		repo := parts[1] + "/" + parts[2]
		if u.Host == "github.com" {
			return githubReleases(repo), tag, true
		}
		return forgejoReleases(u.Host, repo), tag, true
	}
	return nil, "", false
}

var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// amd64Digest is the digest of name:tag's linux/amd64 manifest, as
// `crane digest --platform linux/amd64` gives it: an index's entry for the
// platform, or a single manifest's own (an OCI chart's).
func amd64Digest(ctx context.Context, w *Web, name, tag string) (string, error) {
	host, repo := registryAPI(name)
	b, h, err := w.get(ctx, "https://"+host+"/v2/"+repo+"/manifests/"+tag, manifestTypes...)
	if err != nil {
		return "", err
	}
	var m struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("the manifest of %s:%s: %w", name, tag, err)
	}
	if m.Manifests == nil {
		if d := h.Get("Docker-Content-Digest"); d != "" {
			return d, nil
		}
		s := sha256.Sum256(b)
		return "sha256:" + hex.EncodeToString(s[:]), nil
	}
	for _, e := range m.Manifests {
		if e.Platform.OS == "linux" && e.Platform.Architecture == "amd64" {
			return e.Digest, nil
		}
	}
	return "", fmt.Errorf("%s:%s has no linux/amd64 image", name, tag)
}

// debianSnapshot is the snapshot.debian.org time a Debian image name:tag
// (bookworm-slim) was made from: the date of its dated tag
// (bookworm-20261123-slim) that names the same index, at midnight.
func debianSnapshot(ctx context.Context, w *Web, name, tag string) (string, error) {
	host, repo := registryAPI(name)
	manifest := func(t string) (string, error) {
		h, err := w.head(ctx, "https://"+host+"/v2/"+repo+"/manifests/"+t, manifestTypes...)
		if err != nil {
			return "", err
		}
		return h.Get("Docker-Content-Digest"), nil
	}
	want, err := manifest(tag)
	if err != nil {
		return "", err
	}
	codename, rest, _ := strings.Cut(tag, "-")
	if rest != "" {
		rest = "-" + regexp.QuoteMeta(rest)
	}
	dated := regexp.MustCompile("^" + regexp.QuoteMeta(codename) + `-(\d{8})` + rest + "$")
	tags, err := registryTags{name}.tags(ctx, w)
	if err != nil {
		return "", err
	}
	var dates []string
	for _, t := range tags {
		if m := dated.FindStringSubmatch(t); m != nil {
			dates = append(dates, m[1])
		}
	}
	slices.Sort(dates)
	// the newest first: the tag names the newest dated image
	for _, d := range slices.Backward(dates) {
		got, err := manifest(strings.Replace(tag, codename, codename+"-"+d, 1))
		if err != nil {
			return "", err
		}
		if got == want {
			return d + "T000000Z", nil
		}
	}
	return "", fmt.Errorf("no dated tag of %s:%s names its image %s", name, tag, want)
}

// pgdgVersions are the versions of each package the PGDG archive's dist
// serves now (bookworm-pgdg), oldest first.
func pgdgVersions(ctx context.Context, w *Web, dist string) (map[string][]string, error) {
	u := "https://apt.postgresql.org/pub/repos/apt/dists/" + dist + "/main/binary-amd64/Packages.gz"
	b, _, err := w.get(ctx, u)
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	served := map[string][]string{}
	sc := bufio.NewScanner(io.LimitReader(z, maxAnswer))
	sc.Buffer(nil, 1<<20)
	pkg := ""
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "Package: "); ok {
			pkg = p
		} else if v, ok := strings.CutPrefix(sc.Text(), "Version: "); ok {
			served[pkg] = append(served[pkg], v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	for _, vs := range served {
		slices.SortFunc(vs, func(a, b string) int {
			if debianLess(a, b) {
				return -1
			}
			return 1
		})
	}
	return served, nil
}

// vulnDB is the Go vulnerability database's version and the sha256 of its
// zip, from the pinned ones. vuln.go.dev rebuilds the zip under one
// modified time of its index with its entries in another order, other
// bytes, and the store keeps a version's first upload: a modified time no
// newer than the pin's keeps the pin, with no download, and a new version
// is the modified time inside the zip the program hashed, written as
// 20261124T155600Z, and the first 12 hex digits of that hash, so it names
// those bytes alone.
func vulnDB(ctx context.Context, w *Web, pinned, pinSum string) (version, sum string, err error) {
	at, _, _ := strings.Cut(pinned, "-")
	b, _, err := w.get(ctx, "https://vuln.go.dev/index/db.json.gz")
	if err != nil {
		return "", "", err
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return "", "", fmt.Errorf("vuln.go.dev's index: %w", err)
	}
	if version, err = vulnModified(z); err != nil || version <= at {
		return pinned, pinSum, err
	}
	b, _, err = w.get(ctx, "https://vuln.go.dev/vulndb.zip")
	if err != nil {
		return "", "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", "", fmt.Errorf("vuln.go.dev's zip: %w", err)
	}
	f, err := zr.Open("index/db.json")
	if err != nil {
		return "", "", fmt.Errorf("vuln.go.dev's zip: %w", err)
	}
	defer f.Close()
	if version, err = vulnModified(f); err != nil || version <= at {
		return pinned, pinSum, err
	}
	h := sha256.Sum256(b)
	sum = hex.EncodeToString(h[:])
	return version + "-" + sum[:12], sum, nil
}

// vulnModified is the modified time of the database's index r, written as
// 20261124T155600Z: the fixed width orders the versions as strings.
func vulnModified(r io.Reader) (string, error) {
	var db struct {
		Modified time.Time `json:"modified"`
	}
	if err := json.NewDecoder(r).Decode(&db); err != nil {
		return "", fmt.Errorf("vuln.go.dev's index: %w", err)
	}
	return db.Modified.UTC().Format("20060102T150405Z"), nil
}
