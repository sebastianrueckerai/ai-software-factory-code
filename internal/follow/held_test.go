package follow

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Each move's key finds what the store must hold: an image by its digest, a
// chart by its archive, a file by its package path, a file-derived image by
// its tag.
func TestTargetsOfMoves(t *testing.T) {
	files := map[string][]byte{ImagesList: []byte(images()), FilesList: []byte(filesList())}
	moves := []Move{
		{Key: "ghcr.io/berriai/litellm", After: "1.108.17"},
		{Key: "docker.io/library/golang:1.26.*-alpine", After: "1.26.12-alpine"},
		{Key: "chart temporal", After: "1.8.0"},
		{Key: "file node", After: "v24.22.3"},
		{Key: "copy cnpg", After: "v1.30.2"},
		{Key: "image-of histogram-quantile_linux_amd64.tar.gz", Version: "33997073eb6d"},
	}
	got, err := Targets(files, moves)
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{
		{"ghcr.io/berriai/litellm", KindImage, "ghcr.io/berriai/litellm@" + dg("d"), dg("d")},
		{"docker.io/library/golang:1.26.*-alpine", KindImage, "docker.io/library/golang@" + dg("a"), dg("a")},
		{"chart temporal", KindChart, "helm/temporal-1.8.0.tgz", ""},
		{"file node", KindFile, "generic/node/v24.22.3/node-v24.22.3-linux-x64.tar.xz", ""},
		{"copy cnpg", KindFile, "generic/cnpg/v1.30.2/cnpg.yaml", ""},
		{"image-of histogram-quantile_linux_amd64.tar.gz", KindDerived, "files/histogram-quantile_linux_amd64.tar.gz:33997073eb6d", "33997073eb6d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %+v\nwant %+v", got, want)
	}
	if _, err := Targets(files, []Move{{Key: "no.such/image"}}); err == nil {
		t.Fatal("a key with no line gave a target")
	}
}

// The store answers a HEAD with the login: 200 holds, 404 is ErrMissing, any
// other answer is an error that is no verdict.
func TestStoreHolds(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		if u, p, _ := r.BasicAuth(); u != "bot" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/helm/") && r.Method != http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed) // Forgejo's Helm endpoint answers no HEAD
		case strings.Contains(r.URL.Path, "missing"):
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "broken"):
			w.WriteHeader(http.StatusBadGateway)
		case strings.Contains(r.URL.Path, "/files/") && r.Method == http.MethodHead:
			w.Header().Set("Docker-Content-Digest", dg("9"))
		}
	}))
	defer srv.Close()
	s := HTTPStore{Base: srv.URL, User: "bot", Password: "pw"}
	for _, tc := range []struct {
		t       Target
		path    string
		missing bool
		broken  bool
	}{
		{Target{Kind: KindImage, Path: "ghcr.io/a/b@" + dg("d"), Ref: dg("d")}, "/v2/upstream/ghcr.io/a/b/manifests/" + dg("d"), false, false},
		{Target{Kind: KindChart, Path: "helm/temporal-1.8.0.tgz"}, "/api/packages/upstream/helm/temporal-1.8.0.tgz", false, false},
		{Target{Kind: KindChart, Path: "helm/missing-1.0.0.tgz"}, "/api/packages/upstream/helm/missing-1.0.0.tgz", true, false},
		{Target{Kind: KindFile, Path: "generic/node/v1/missing.tgz"}, "/api/packages/upstream/generic/node/v1/missing.tgz", true, false},
		{Target{Kind: KindImage, Path: "x/broken@" + dg("1"), Ref: dg("1")}, "/v2/upstream/x/broken/manifests/" + dg("1"), false, true},
		{Target{Kind: KindDerived, Path: "files/a.tar.gz:123456789abc"}, "/v2/upstream/files/a.tar.gz/manifests/123456789abc", false, false},
	} {
		asked = nil
		err := s.Holds(tc.t)
		if got := errors.Is(err, ErrMissing); got != tc.missing || (err != nil) != (tc.missing || tc.broken) {
			t.Errorf("%s: err = %v, want missing %v broken %v", tc.t.Path, err, tc.missing, tc.broken)
		}
		verb := "HEAD "
		if tc.t.Kind == KindChart {
			verb = "GET "
		}
		if len(asked) != 1 || asked[0] != verb+tc.path {
			t.Errorf("%s: asked %q, want %s%s", tc.t.Path, asked, verb, tc.path)
		}
	}
}
