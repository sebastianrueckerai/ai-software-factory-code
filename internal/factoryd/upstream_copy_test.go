package factoryd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sha(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

// image is a one-layer image the fake upstream serves.
type fakeImg struct {
	manifest []byte
	digest   string
	blobs    map[string][]byte
	ctype    string // the manifest's Content-Type; an image manifest's when ""
}

// fakeIndex is an index of children, each a manifest the upstream serves.
func fakeIndex(children ...fakeImg) fakeImg {
	var parts []string
	blobs := map[string][]byte{}
	for _, c := range children {
		parts = append(parts, fmt.Sprintf(`{"digest":%q,"size":%d}`, c.digest, len(c.manifest)))
		for d, b := range c.blobs {
			blobs[d] = b
		}
	}
	return rawIndex("["+strings.Join(parts, ",")+"]", blobs)
}

func rawIndex(manifests string, blobs map[string][]byte) fakeImg {
	m := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":` + manifests + `}`)
	return fakeImg{manifest: m, digest: sha(m), blobs: blobs, ctype: "application/vnd.oci.image.index.v1+json"}
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
	headerDelay    time.Duration // a manifest's headers wait this long
	active         atomic.Int32
	maxActive      atomic.Int32
	srv            *httptest.Server
}

func newFakeUpstream(t *testing.T, images ...fakeImg) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{images: map[string]fakeImg{}, bearerRequired: true}
	for _, im := range images {
		u.images[im.digest] = im
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := u.active.Add(1)
		defer u.active.Add(-1)
		for m := u.maxActive.Load(); n > m && !u.maxActive.CompareAndSwap(m, n); m = u.maxActive.Load() {
		}
		time.Sleep(5 * time.Millisecond)
		if strings.Contains(r.URL.Path, "/manifests/") {
			time.Sleep(u.headerDelay)
		}
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
			w.Header().Set("Content-Type", cmp.Or(im.ctype, "application/vnd.oci.image.manifest.v1+json"))
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
// bearer from /v2/token, which the site admin's basic login asks for, writes
// with that login.
type fakeImgStore struct {
	mu        sync.Mutex
	blobs     map[string][]byte // repository/digest
	manifests map[string][]byte // repository/reference
	uploads   int
	putDelay  time.Duration // a blob PUT's answer waits this long
	writes    int
	badAuth   []string
	srv       *httptest.Server
}

const storeToken = "store-token-123456"

func newFakeImgStore(t *testing.T) *fakeImgStore {
	t.Helper()
	s := &fakeImgStore{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path == "/v2/token" {
			if u, p, _ := r.BasicAuth(); u != UpstreamUser || p != storeToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"token":"read-tok"}`)
			return
		}
		u, p, basic := r.BasicAuth()
		if basic || r.Method != http.MethodGet {
			if u != UpstreamUser || p != storeToken {
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
			time.Sleep(s.putDelay)
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
				var doc manifestDoc
				json.Unmarshal(body, &doc)
				for _, ch := range doc.Manifests {
					if _, ok := s.manifests[repo+"/"+ch.Digest]; !ok {
						http.Error(w, "manifest blob unknown", http.StatusBadRequest)
						return
					}
				}
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
		Registry: &Registry{BaseURL: s.srv.URL, User: UpstreamUser, Password: storeToken},
		Endpoint: func(host string) string {
			if host != "docker.io" {
				panic("unexpected registry " + host)
			}
			return up.srv.URL
		},
	}
}

// copyLists is what the copy pod does with the two lists: the lines head adds
// to base, copied, and the findings of both steps.
func copyLists(ctx context.Context, c *UpstreamCopier, base, head string) ([]string, error) {
	lines, findings := addedImageLines(base, head)
	got, err := c.Copy(ctx, lines)
	return append(findings, got...), err
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
	findings, err := copyLists(t.Context(), c, base, head)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	repo := "upstream/docker.io/library/golang"
	for _, ref := range []string{"1.26.11-alpine-" + strings.TrimPrefix(im.digest, "sha256:")[:12], im.digest} {
		if got := store.manifests[repo+"/"+ref]; string(got) != string(im.manifest) {
			t.Errorf("store %s = %q, want the manifest", ref, got)
		}
	}
	for d, b := range im.blobs {
		if string(store.blobs[repo+"/"+d]) != string(b) {
			t.Errorf("store blob %s missing", d)
		}
	}
	if _, ok := store.manifests[repo+"/1.26.11-alpine"]; ok {
		t.Error("the plain tag was written: only main's upstream job names it")
	}
	if len(store.badAuth) != 0 {
		t.Errorf("writes without the admin's login: %v", store.badAuth)
	}
	reads, writes := up.manifestReads, store.writes
	if findings, err := copyLists(t.Context(), c, base, head); err != nil || len(findings) != 0 {
		t.Fatalf("second Copy = %q, %v", findings, err)
	}
	if up.manifestReads != reads || store.writes != writes {
		t.Errorf("second call: %d more upstream manifest reads, %d more store writes, want 0 and 0", up.manifestReads-reads, store.writes-writes)
	}
}

func TestUpstreamCopyMissingDigestIsFindingNotError(t *testing.T) { // missing_digest_card_finding
	good, gone := newFakeImage("good"), newFakeImage("gone")
	up, store := newFakeUpstream(t, good), newFakeImgStore(t)
	head := goLine(gone.digest) + "\n" + "docker.io/library/golang:1.26.12@" + good.digest + "\n"
	findings, err := copyLists(t.Context(), store.copier(up), "", head)
	if err != nil {
		t.Fatal(err)
	}
	if want := goLine(gone.digest) + ": the upstream does not serve this digest"; len(findings) != 1 || findings[0] != want {
		t.Errorf("findings = %q, want [%q]", findings, want)
	}
	if _, ok := store.manifests["upstream/docker.io/library/golang/1.26.12-"+strings.TrimPrefix(good.digest, "sha256:")[:12]]; !ok {
		t.Error("the other line was not copied")
	}
}

func TestUpstreamCopyMissingBlobIsFinding(t *testing.T) {
	im := newFakeImage("blobless")
	for d := range im.blobs {
		delete(im.blobs, d)
	}
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(im.digest)+"\n")
	if err != nil || len(findings) != 1 || !strings.HasSuffix(findings[0], ": the upstream does not serve this digest") {
		t.Errorf("Copy = %q, %v", findings, err)
	}
}

func TestUpstreamCopyEqualListsTouchNothing(t *testing.T) { // no_copy_without_change
	im := newFakeImage("same")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	list := "# images\n" + goLine(im.digest) + "\n"
	findings, err := copyLists(t.Context(), store.copier(up), list, list)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	if up.manifestReads != 0 || up.blobReads != 0 || store.writes != 0 || store.uploads != 0 {
		t.Errorf("reads %d/%d, writes %d/%d, want all 0", up.manifestReads, up.blobReads, store.writes, store.uploads)
	}
}

func copyRunner(t *testing.T, task Task, fj *prForgejo, u *UpstreamPod) *Runner {
	t.Helper()
	r, _, _ := newActivityRunner(t, task, fj)
	r.Upstream = u
	return r
}

func TestCopyUpstreamImagesActivityReadsHeadOnTheForkAndBaseOnTheRepository(t *testing.T) { // added_image_line_copied, missing_digest_card_finding
	good, gone := newFakeImage("good"), newFakeImage("gone")
	up, store := newFakeUpstream(t, good), newFakeImgStore(t)
	base := "# images\n"
	fj := &prForgejo{raw: map[string]string{
		"sf/repo@base":          base,
		"factory-agent/repo@c1": base + goLine(good.digest) + "\n" + goLine(gone.digest) + "\n",
	}}
	r := copyRunner(t, prTask, fj, store.pod(t, up))
	val, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	if err := val.Get(&findings); err != nil {
		t.Fatal(err)
	}
	if want := goLine(gone.digest) + ": the upstream does not serve this digest"; len(findings) != 1 || findings[0] != want {
		t.Errorf("findings = %q, want [%q]", findings, want)
	}
	if _, ok := store.manifests["upstream/docker.io/library/golang/1.26.11-alpine-"+strings.TrimPrefix(good.digest, "sha256:")[:12]]; !ok {
		t.Error("the added line is not in the store")
	}
}

func TestCopyUpstreamImagesActivityTakesMainsHeadForAnEmptyBase(t *testing.T) {
	im := newFakeImage("x")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	task := prTask
	task.SHA = ""
	fj := &prForgejo{head: "m9", raw: map[string]string{"sf/repo@m9": goLine(im.digest) + "\n", "factory-agent/repo@c1": goLine(im.digest) + "\n"}}
	r := copyRunner(t, task, fj, store.pod(t, up))
	if _, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1"); err != nil {
		t.Fatal(err)
	}
	if up.manifestReads != 0 || store.writes != 0 {
		t.Errorf("main's own line was copied: %d reads, %d writes", up.manifestReads, store.writes)
	}
}

func TestCopyUpstreamImagesActivityUnchangedOrAbsentListTouchesNoRegistry(t *testing.T) { // no_copy_without_change
	im := newFakeImage("x")
	for name, raw := range map[string]map[string]string{
		"equal":   {"sf/repo@base": goLine(im.digest) + "\n", "factory-agent/repo@c1": goLine(im.digest) + "\n"},
		"no file": {},
		"removed": {"sf/repo@base": goLine(im.digest) + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			up, store := newFakeUpstream(t, im), newFakeImgStore(t)
			pod := store.pod(t, up)
			pod.OpenBao = &fakeKV{err: fmt.Errorf("no login wanted")}
			r := copyRunner(t, prTask, &prForgejo{raw: raw}, pod)
			val, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1")
			var findings []string
			if err != nil || val.Get(&findings) != nil || len(findings) != 0 {
				t.Fatalf("findings %q, err %v", findings, err)
			}
			if up.manifestReads+store.writes != 0 {
				t.Errorf("registry calls: %d upstream reads, %d store writes", up.manifestReads, store.writes)
			}
			if got := pod.Kube.(*copyPodKube).created; len(got) != 0 {
				t.Errorf("a pod for no added line: %v", got)
			}
		})
	}
}

func TestCopyUpstreamImagesActivityWithoutCopierDoesNothing(t *testing.T) {
	r := copyRunner(t, prTask, &prForgejo{}, nil)
	val, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1")
	var findings []string
	if err != nil || val.Get(&findings) != nil || len(findings) != 0 {
		t.Fatalf("findings %q, err %v", findings, err)
	}
}

func TestCopyUpstreamImagesActivityErrorIsScrubbed(t *testing.T) {
	im := newFakeImage("x")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	pod := store.pod(t, up)
	pod.OpenBao = &fakeKV{err: fmt.Errorf("get https://user:pass@bao.example/v1/x: Authorization: Bearer abcdefgh12345678")}
	fj := &prForgejo{raw: map[string]string{"sf/repo@base": "", "factory-agent/repo@c1": goLine(im.digest) + "\n"}}
	r := copyRunner(t, prTask, fj, pod)
	_, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1")
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{"user:pass", "abcdefgh12345678"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries %q", err, leak)
		}
	}
}

func TestAddedImageLinesHoldNamesToTheOCIGrammar(t *testing.T) {
	d := "@sha256:" + strings.Repeat("a", 64)
	for line, ok := range map[string]bool{
		"docker.io/library/golang:1.26-alpine" + d:          true,
		"ghcr.io/berriai/litellm:1.2" + d:                   true,
		"factory.talos.dev/installer/ab_c.d--e:v1.14.5" + d: true,
		"registry.k8s.io/coredns/coredns:v1.14.7" + d:       true,
		"docker.io/Library/golang:1" + d:                    false,
		"docker.io/library/../golang:1" + d:                 false,
		"docker.io/library/gol?ang:1" + d:                   false,
		"docker.io/library/gol#ang:1" + d:                   false,
		"docker.io/library/gol%2Fang:1" + d:                 false,
		"docker.io//golang:1" + d:                           false,
		"docker.io/library/-golang:1" + d:                   false,
		"docker.io/library/golang_:1" + d:                   false,
		"docker.io:1" + d:                                   false,
		"golang:1" + d:                                      false,
		"docker.io/library/golang:" + d:                     false,
	} {
		lines, findings := addedImageLines("", line+"\n")
		if ok != (len(lines) == 1 && len(findings) == 0) || !ok && (len(lines) != 0 || len(findings) != 1 || findings[0] != line+": want name:tag@sha256:<digest>") {
			t.Errorf("%q: lines %q, findings %q, want ok = %v", line, lines, findings, ok)
		}
	}
}

func TestAddedImageLinesTooLongALineIsAFinding(t *testing.T) {
	head := "docker.io/library/a:1@sha256:" + strings.Repeat("a", 64) + "\n" + strings.Repeat("x", 70000) + "\n"
	_, findings := addedImageLines("", head)
	if len(findings) != 1 || !strings.HasPrefix(findings[0], "upstream-images.txt at the head: a line is too long to read") {
		t.Errorf("findings = %q", findings)
	}
}

func TestUpstreamCopyCopiesAnIndexAfterItsChildren(t *testing.T) {
	a, b := newFakeImage("amd64"), newFakeImage("arm64")
	idx := fakeIndex(a, b)
	up, store := newFakeUpstream(t, a, b, idx), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(idx.digest)+"\n")
	if err != nil || len(findings) != 0 {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	repo := "upstream/docker.io/library/golang/"
	tag := "1.26.11-alpine-" + strings.TrimPrefix(idx.digest, "sha256:")[:12]
	if string(store.manifests[repo+tag]) != string(idx.manifest) {
		t.Error("the index is not in the store")
	}
	for _, c := range []fakeImg{a, b} {
		if string(store.manifests[repo+c.digest]) != string(c.manifest) {
			t.Errorf("child %s is not in the store", c.digest)
		}
		for d := range c.blobs {
			if _, ok := store.blobs[strings.TrimSuffix(repo, "/")+"/"+d]; !ok {
				t.Errorf("blob %s of a child is not in the store", d)
			}
		}
	}
}

func TestUpstreamCopyRefusesAnIndexChildThatIsNotADigest(t *testing.T) {
	good := newFakeImage("good")
	idx := rawIndex(fmt.Sprintf(`[{"digest":%q,"size":1},{"digest":"latest","size":1}]`, good.digest), good.blobs)
	up, store := newFakeUpstream(t, good, idx), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(idx.digest)+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := goLine(idx.digest) + `: the upstream's manifest names "latest", which is not a sha256 digest`; len(findings) != 1 || findings[0] != want {
		t.Errorf("findings = %q, want [%q]", findings, want)
	}
	for k := range store.manifests {
		if strings.HasSuffix(k, "/latest") || strings.Contains(k, "1.26.11-alpine-") {
			t.Errorf("store holds %s: nothing of the refused index is to be named", k)
		}
	}
}

func TestUpstreamCopyRefusesABlobOverTheBound(t *testing.T) {
	im := newFakeImage("big")
	m := fmt.Appendf(nil, `{"schemaVersion":2,"config":{"digest":%q,"size":1},"layers":[{"digest":%q,"size":%d}]}`, sha([]byte("c")), sha([]byte("l")), maxBlobBytes+1)
	big := fakeImg{manifest: m, digest: sha(m), blobs: im.blobs}
	up, store := newFakeUpstream(t, big), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(big.digest)+"\n")
	if err != nil || len(findings) != 1 || !strings.HasPrefix(findings[0], goLine(big.digest)+": a blob of 536870913 bytes is too large to copy before the merge") {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	if up.blobReads != 0 || store.writes != 0 {
		t.Errorf("%d blob reads, %d store writes, want none", up.blobReads, store.writes)
	}
}

func TestUpstreamCopyStopsABlobLongerThanItsDescriptor(t *testing.T) {
	lay := []byte(strings.Repeat("x", 100))
	m := fmt.Appendf(nil, `{"schemaVersion":2,"config":{"digest":%q,"size":1},"layers":[{"digest":%q,"size":10}]}`, sha([]byte("c")), sha(lay))
	im := fakeImg{manifest: m, digest: sha(m), blobs: map[string][]byte{sha([]byte("c")): []byte("c"), sha(lay): lay}}
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(im.digest)+"\n")
	want := goLine(im.digest) + ": a blob of " + sha(lay) + " is longer than its descriptor's 10 bytes"
	if err != nil || len(findings) != 1 || findings[0] != want {
		t.Fatalf("Copy = %q, %v; want the finding %q", findings, err, want)
	}
}

func TestUpstreamCopyBlobThatFailsItsHashIsAFinding(t *testing.T) {
	lay := []byte("layer")
	m := fmt.Appendf(nil, `{"schemaVersion":2,"config":{"digest":%q,"size":1},"layers":[{"digest":%q,"size":5}]}`, sha([]byte("c")), sha(lay))
	im := fakeImg{manifest: m, digest: sha(m), blobs: map[string][]byte{sha([]byte("c")): []byte("c"), sha(lay): []byte("LAYER")}}
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(im.digest)+"\n")
	if err != nil || len(findings) != 1 || !strings.HasPrefix(findings[0], goLine(im.digest)+": a blob of "+sha(lay)+" hashes to ") {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
}

func TestUpstreamCopyShortBlobIsAFindingStatingTheRealCount(t *testing.T) {
	lay := []byte("layer")
	m := fmt.Appendf(nil, `{"schemaVersion":2,"config":{"digest":%q,"size":1},"layers":[{"digest":%q,"size":50}]}`, sha([]byte("c")), sha(lay))
	im := fakeImg{manifest: m, digest: sha(m), blobs: map[string][]byte{sha([]byte("c")): []byte("c"), sha(lay): lay}}
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(im.digest)+"\n")
	want := goLine(im.digest) + ": a blob of " + sha(lay) + " is 5 bytes, its descriptor says 50"
	if err != nil || len(findings) != 1 || findings[0] != want {
		t.Fatalf("Copy = %q, %v; want the finding %q", findings, err, want)
	}
}

func TestUpstreamCopyQuotesAChildDigestCut(t *testing.T) {
	long := "a" + strings.Repeat("é", 100)
	idx := rawIndex(fmt.Sprintf(`[{"digest":%q,"size":1}]`, long), nil)
	up, store := newFakeUpstream(t, idx), newFakeImgStore(t)
	findings, err := copyLists(t.Context(), store.copier(up), "", goLine(idx.digest)+"\n")
	if err != nil || len(findings) != 1 || strings.Contains(findings[0], long) || !strings.Contains(findings[0], `"a`+strings.Repeat("é", 39)+`..."`) {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
}

// A manifest and a blob the upstream redirects to another host are read
// there with no bearer, as registry.k8s.io's are (checked by hand against
// the real one): the copy follows both redirects.
func TestUpstreamCopyFollowsRedirectsToAnotherHost(t *testing.T) {
	im := newFakeImage("redirected")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	up.bearerRequired = false
	var auth []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		http.Redirect(w, r, up.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(front.Close)
	c := store.copier(up)
	c.Endpoint = func(string) string { return front.URL }
	findings, err := copyLists(t.Context(), c, "", goLine(im.digest)+"\n")
	if err != nil || len(findings) != 0 {
		t.Fatalf("Copy = %q, %v", findings, err)
	}
	if up.manifestReads != 1 || up.blobReads != 2 || len(auth) != 3 {
		t.Errorf("manifest reads %d, blob reads %d, front requests %d; want 1, 2, 3", up.manifestReads, up.blobReads, len(auth))
	}
}

func TestCopyUpstreamImagesActivityReadsAnOpenPullRequestsHeadRepository(t *testing.T) {
	im := newFakeImage("x")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	fj := &prForgejo{
		pulls: map[int64]string{12: `{"number":12,"state":"open","html_url":"https://git.example/sf/repo/pulls/12","head":{"sha":"c1","ref":"factory/schema-abc","repo":{"full_name":"someone/fork"}},"base":{"ref_name":"main"}}`},
		raw:   map[string]string{"sf/repo@base": "", "someone/fork@c1": goLine(im.digest) + "\n"},
	}
	task := prTask
	task.PRNumber = 12
	r := copyRunner(t, task, fj, store.pod(t, up))
	if _, err := execActivity(t, r, ActivityCopyUpstreamImages, "t1", "c1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.manifests["upstream/docker.io/library/golang/"+im.digest]; !ok {
		t.Error("the line of the pull request's head repository was not copied")
	}
}

func TestUpstreamCopyBadLayerDescriptorsAreFindings(t *testing.T) {
	long := "x" + strings.Repeat("é", 100)
	for name, tc := range map[string]struct{ layer, want string }{
		"not a digest":  {fmt.Sprintf(`{"digest":%q,"size":1}`, long), `: the upstream's manifest names a blob "x` + strings.Repeat("é", 39) + `...", which is not a sha256 digest`},
		"negative size": {fmt.Sprintf(`{"digest":%q,"size":-5}`, sha([]byte("l"))), ": a blob of " + sha([]byte("l")) + " has the size -5 in its descriptor"},
	} {
		t.Run(name, func(t *testing.T) {
			m := fmt.Appendf(nil, `{"schemaVersion":2,"config":{"digest":%q,"size":1},"layers":[%s]}`, sha([]byte("c")), tc.layer)
			im := fakeImg{manifest: m, digest: sha(m), blobs: map[string][]byte{sha([]byte("c")): []byte("c")}}
			up, store := newFakeUpstream(t, im), newFakeImgStore(t)
			findings, err := copyLists(t.Context(), store.copier(up), "", goLine(im.digest)+"\n")
			if err != nil || len(findings) != 1 || findings[0] != goLine(im.digest)+tc.want {
				t.Fatalf("Copy = %q, %v; want %q", findings, err, tc.want)
			}
		})
	}
}

// The header timeout bounds the upstream registries alone: Forgejo answers
// a blob's PUT after it wrote the blob to its storage, which takes longer.
func TestUpstreamCopyHeaderTimeoutBoundsTheUpstreamNotTheStore(t *testing.T) {
	const timeout = 100 * time.Millisecond
	im := newFakeImage("slow")
	up, store := newFakeUpstream(t, im), newFakeImgStore(t)
	store.putDelay = 3 * timeout
	c := store.copier(up)
	c.HeaderTimeout = timeout
	if findings, err := copyLists(t.Context(), c, "", goLine(im.digest)+"\n"); err != nil || len(findings) != 0 {
		t.Fatalf("a store slower than the header timeout: Copy = %q, %v", findings, err)
	}

	slow, store2 := newFakeImage("stalled"), newFakeImgStore(t)
	up2 := newFakeUpstream(t, slow)
	up2.headerDelay = 3 * timeout
	c2 := store2.copier(up2)
	c2.HeaderTimeout = timeout
	if _, err := copyLists(t.Context(), c2, "", goLine(slow.digest)+"\n"); err == nil {
		t.Fatal("an upstream that delays its headers past the timeout was waited for")
	}
}
