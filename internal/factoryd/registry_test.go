package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// An index as main CI's dind pushes it: the linux/amd64 image and its
// provenance attestation.
const (
	testImageManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:c0"},"layers":[]}`
	testIndexTemplate = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:ATT","platform":{"architecture":"unknown","os":"unknown"},` +
		`"annotations":{"vnd.docker.reference.type":"attestation-manifest"}},` +
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"IMG","platform":{"architecture":"amd64","os":"linux"}}]}`
)

func testRegistry(t *testing.T, manifests map[string]string) (*Registry, *int) {
	t.Helper()
	tokens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/token":
			tokens++
			if u, p, ok := r.BasicAuth(); !ok || u != "puller" || p != "pull-pw" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"token":"bearer-tok"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/sf/"):
			if r.Header.Get("Authorization") != "Bearer bearer-tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://elsewhere.example/v2/token"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				t.Errorf("accept %q", r.Header.Get("Accept"))
			}
			body, ok := manifests[strings.TrimPrefix(r.URL.Path, "/v2/")]
			if !ok {
				http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
				return
			}
			w.Write([]byte(body))
		default:
			t.Errorf("unexpected call %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Registry{BaseURL: srv.URL, User: "puller", Password: "pull-pw"}, &tokens
}

// A tag resolves to the index main CI pushed; its platform digest is the
// linux/amd64 manifest inside, which a rebuild with no input change
// repeats while the index digest does not. A plain manifest is its own
// platform digest.
func TestRegistryResolvesTheIndexAndItsPlatformManifest(t *testing.T) {
	index := strings.Replace(testIndexTemplate, "IMG", sha256Of(testImageManifest), 1)
	reg, tokens := testRegistry(t, map[string]string{
		"sf/factory-gateway/manifests/abc1234":            index,
		"sf/ci-runner/manifests/abc1234":                  testImageManifest,
		"sf/factory-gateway/manifests/" + sha256Of(index): index,
		"sf/opencode-worker/manifests/" + digestWorkerOld: testImageManifest,
	})
	ctx := context.Background()
	tok, err := reg.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reg.Resolve(ctx, tok, "sf/factory-gateway", "abc1234")
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImageManifest{Digest: sha256Of(index), Platform: sha256Of(testImageManifest)}); got != want {
		t.Fatalf("index %+v, want %+v", got, want)
	}
	if got, err := reg.Resolve(ctx, tok, "sf/factory-gateway", sha256Of(index)); err != nil || got.Platform != sha256Of(testImageManifest) {
		t.Fatalf("by digest %+v %v", got, err)
	}
	if got, err := reg.Resolve(ctx, tok, "sf/ci-runner", "abc1234"); err != nil || got != (ImageManifest{Digest: sha256Of(testImageManifest), Platform: sha256Of(testImageManifest)}) {
		t.Fatalf("plain manifest %+v %v", got, err)
	}
	// a body that does not hash to the digest asked for is refused
	if _, err := reg.Resolve(ctx, tok, "sf/opencode-worker", digestWorkerOld); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered manifest: %v", err)
	}
	if _, err := reg.Resolve(ctx, tok, "sf/factory-gateway", "fff0000"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing tag: %v", err)
	}
	if *tokens != 1 {
		t.Fatalf("%d token reads for five manifests, want 1", *tokens)
	}
	reg.Password = "wrong"
	if _, err := reg.Token(ctx); err == nil {
		t.Fatal("a refused token read gave a token")
	}
}
