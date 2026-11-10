package factoryd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func sha(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

// image is a one-layer image the fake upstream serves.
type fakeImg struct {
	manifest []byte
	digest   string
	blobs    map[string][]byte
}

func newFakeImage(layer string) fakeImg {
	cfg, lay := []byte("config of "+layer), []byte(layer)
	m := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":%q,"size":%d},"layers":[{"digest":%q,"size":%d}]}`,
		sha(cfg), len(cfg), sha(lay), len(lay))
	return fakeImg{manifest: m, digest: sha(m), blobs: map[string][]byte{sha(cfg): cfg, sha(lay): lay}}
}

type fakeUpstream struct {
	mu             sync.Mutex
	images         map[string]fakeImg // by digest
	manifestReads  int
	blobReads      int
	bearerRequired bool
	srv            *httptest.Server
}

func newFakeUpstream(t *testing.T, images ...fakeImg) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{images: map[string]fakeImg{}, bearerRequired: true}
	for _, im := range images {
		u.images[im.digest] = im
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.URL.Path == "/token" {
			if r.URL.Query().Get("service") != "fake-registry" || r.URL.Query().Get("scope") != "repository:library/golang:pull" {
				http.Error(w, "bad token request "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"token":"anon-tok"}`)
			return
		}
		if u.bearerRequired && r.Header.Get("Authorization") != "Bearer anon-tok" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake-registry",scope="repository:library/golang:pull"`, u.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/v2/library/golang/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch kind, ref, _ := strings.Cut(rest, "/"); kind {
		case "manifests":
			u.manifestReads++
			im, ok := u.images[ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Write(im.manifest)
		case "blobs":
			u.blobReads++
			for _, im := range u.images {
				if b, ok := im.blobs[ref]; ok {
					w.Write(b)
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// fakeImgStore is the factory's registry as the copier uses it: reads with a
// bearer from /v2/token, writes with the site admin's basic login.
type fakeImgStore struct {
	mu        sync.Mutex
	blobs     map[string][]byte // repository/digest
	manifests map[string][]byte // repository/reference
	uploads   int
	writes    int
	badAuth   []string
	srv       *httptest.Server
}

const (
	storeReadUser, storeReadPass = "reader", "read-pass-123"
	storeAdminUser, storeAdminPw = "site-admin", "admin-pass-123"
)

func newFakeImgStore(t *testing.T) *fakeImgStore {
	t.Helper()
	s := &fakeImgStore{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path == "/v2/token" {
			if u, p, _ := r.BasicAuth(); u != storeReadUser || p != storeReadPass {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"token":"read-tok"}`)
			return
		}
		u, p, basic := r.BasicAuth()
		if basic || r.Method != http.MethodGet {
			if u != storeAdminUser || p != storeAdminPw {
				s.badAuth = append(s.badAuth, r.Method+" "+r.URL.Path)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		} else if r.Header.Get("Authorization") != "Bearer read-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/v2/")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(rest, "/blobs/uploads/"):
			s.uploads++
			w.Header().Set("Location", "/v2/"+strings.TrimSuffix(rest, "uploads/")+"uploads/u1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(rest, "/blobs/uploads/"):
			repo, _, _ := strings.Cut(rest, "/blobs/")
			body, _ := io.ReadAll(r.Body)
			d := r.URL.Query().Get("digest")
			if sha(body) != d {
				http.Error(w, "digest mismatch", http.StatusBadRequest)
				return
			}
			s.writes++
			s.blobs[repo+"/"+d] = body
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(rest, "/blobs/"):
			repo, d, _ := strings.Cut(rest, "/blobs/")
			if _, ok := s.blobs[repo+"/"+d]; !ok {
				w.WriteHeader(http.StatusNotFound)
			}
		case strings.Contains(rest, "/manifests/"):
			repo, ref, _ := strings.Cut(rest, "/manifests/")
			switch r.Method {
			case http.MethodPut:
				body, _ := io.ReadAll(r.Body)
				s.writes++
				s.manifests[repo+"/"+ref] = body
				s.manifests[repo+"/"+sha(body)] = body
				w.WriteHeader(http.StatusCreated)
			case http.MethodGet:
				b, ok := s.manifests[repo+"/"+ref]
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				w.Write(b)
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeImgStore) copier(up *fakeUpstream) *UpstreamCopier {
	return &UpstreamCopier{
		Registry: &Registry{BaseURL: s.srv.URL, User: storeReadUser, Password: storeReadPass},
		OpenBao:  &fakeKV{data: map[string]string{"username": storeAdminUser, "password": storeAdminPw}},
		Endpoint: func(host string) string {
			if host != "docker.io" {
				panic("unexpected registry " + host)
			}
			return up.srv.URL
		},
	}
}

func goLine(digest string) string {
	return "docker.io/library/golang:1.26.11-alpine@" + digest
}

func TestAddedImageLinesCompareByRefAndFlagMalformed(t *testing.T) {
	d1, d2 := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	base := "# header\n\n" + goLine(d1) + " bootstrap\nhttps://example.com/c.tgz@sha256:" + strings.Repeat("c", 64) + " helm\n"
	head := base + goLine(d1) + "\n" + // the same ref without its kind word: not added
		goLine(d2) + " bootstrap\n" +
		"https://example.com/d.tgz@sha256:" + strings.Repeat("d", 64) + " helm\n" +
		"docker.io/library/node:22\n" +
		"docker.io/library/node:22@sha256:abc\n" +
		goLine(d2) + "\n" // duplicate within head
	lines, findings := addedImageLines(base, head)
	if want := []string{goLine(d2) + " bootstrap"}; fmt.Sprint(lines) != fmt.Sprint(want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	want := []string{
		"docker.io/library/node:22: want name:tag@sha256:<digest>",
		"docker.io/library/node:22@sha256:abc: want name:tag@sha256:<digest>",
	}
	if fmt.Sprint(findings) != fmt.Sprint(want) {
		t.Errorf("findings = %q, want %q", findings, want)
	}
}

func TestUpstreamCopyCopiesAddedImageLine(t *testing.T) { // added_image_line_copied
	im := newFakeImage("golang layer")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	c := store.copier(up)
	base := "# images\n"
	head := base + goLine(im.digest) + "\n"
	findings, err := c.Copy(t.Context(), base, head)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	repo := "upstream/docker.io/library/golang"
	for _, ref := range []string{"1.26.11-alpine-" + strings.TrimPrefix(im.digest, "sha256:")[:12], "1.26.11-alpine", im.digest} {
		if got := store.manifests[repo+"/"+ref]; string(got) != string(im.manifest) {
			t.Errorf("store %s = %q, want the manifest", ref, got)
		}
	}
	for d, b := range im.blobs {
		if string(store.blobs[repo+"/"+d]) != string(b) {
			t.Errorf("store blob %s missing", d)
		}
	}
	if len(store.badAuth) != 0 {
		t.Errorf("writes without the admin's login: %v", store.badAuth)
	}
	reads, writes := up.manifestReads, store.writes
	if findings, err := c.Copy(t.Context(), base, head); err != nil || len(findings) != 0 {
		t.Fatalf("second Copy = %q, %v", findings, err)
	}
	if up.manifestReads != reads || store.writes != writes {
		t.Errorf("second call: %d more upstream manifest reads, %d more store writes, want 0 and 0", up.manifestReads-reads, store.writes-writes)
	}
}

func TestUpstreamCopyDoesNotMovePlainTagOfAnotherDigest(t *testing.T) {
	im, other := newFakeImage("new"), newFakeImage("old")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	repo := "upstream/docker.io/library/golang"
	store.manifests[repo+"/1.26.11-alpine"] = other.manifest
	store.manifests[repo+"/"+other.digest] = other.manifest
	if _, err := store.copier(up).Copy(t.Context(), "", goLine(im.digest)+"\n"); err != nil {
		t.Fatal(err)
	}
	if string(store.manifests[repo+"/1.26.11-alpine"]) != string(other.manifest) {
		t.Error("the plain tag moved off the digest another line holds")
	}
	if _, ok := store.manifests[repo+"/1.26.11-alpine-"+strings.TrimPrefix(im.digest, "sha256:")[:12]]; !ok {
		t.Error("the digest's own tag is missing")
	}
}

func TestUpstreamCopyMissingDigestIsFindingNotError(t *testing.T) { // missing_digest_card_finding
	good, gone := newFakeImage("good"), newFakeImage("gone")
	up, store := newFakeUpstream(t, good), newFakeImgStore(t)
	head := goLine(gone.digest) + "\n" + "docker.io/library/golang:1.26.12@" + good.digest + "\n"
	findings, err := store.copier(up).Copy(t.Context(), "", head)
	if err != nil {
		t.Fatal(err)
	}
	if want := goLine(gone.digest) + ": the upstream does not serve this digest"; len(findings) != 1 || findings[0] != want {
		t.Errorf("findings = %q, want [%q]", findings, want)
	}
	if _, ok := store.manifests["upstream/docker.io/library/golang/1.26.12"]; !ok {
		t.Error("the other line was not copied")
	}
}

func TestUpstreamCopyMissingBlobIsFinding(t *testing.T) {
	im := newFakeImage("blobless")
	for d := range im.blobs {
		delete(im.blobs, d)
	}
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	findings, err := store.copier(up).Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err != nil || len(findings) != 1 || !strings.HasSuffix(findings[0], ": the upstream does not serve this digest") {
		t.Errorf("Copy = %q, %v", findings, err)
	}
}

func TestUpstreamCopyEqualListsTouchNothing(t *testing.T) { // no_copy_without_change
	im := newFakeImage("same")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	list := "# images\n" + goLine(im.digest) + "\n"
	findings, err := store.copier(up).Copy(t.Context(), list, list)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	if up.manifestReads != 0 || up.blobReads != 0 || store.writes != 0 || store.uploads != 0 {
		t.Errorf("reads %d/%d, writes %d/%d, want all 0", up.manifestReads, up.blobReads, store.writes, store.uploads)
	}
}

func TestUpstreamCopyServerErrorIsErrorWithoutCredentials(t *testing.T) {
	im := newFakeImage("x")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	up.srv.Close()
	c := store.copier(up)
	c.Endpoint = func(string) string { return "http://user:pass@" + strings.TrimPrefix(up.srv.URL, "http://") }
	_, err := c.Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err == nil {
		t.Fatal("no error for an unreachable upstream")
	}
	if strings.Contains(err.Error(), "user:pass") || strings.Contains(err.Error(), "pass@") {
		t.Errorf("error carries userinfo: %v", err)
	}
}

func TestUpstreamCopyErrorTextIsScrubbed(t *testing.T) {
	im := newFakeImage("x")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	c := store.copier(up)
	c.OpenBao = &fakeKV{err: fmt.Errorf("get https://user:pass@bao.example/v1/x: Authorization: Bearer abcdefgh12345678")}
	_, err := c.Copy(t.Context(), "", goLine(im.digest)+"\n")
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{"user:pass", "abcdefgh12345678"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q", err, leak)
		}
	}
}
