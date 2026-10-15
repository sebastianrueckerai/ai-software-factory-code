package factoryd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
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

// What cosign 3.1.3 attached to busybox 1.36's manifest on Forgejo
// 15.0.9 when it signed it by digest with a test key pair (cosign sign
// --key, a signing config of no services), as Forgejo serves it: the
// index under the referrers tag, its signature manifest and the bundle;
// and the pair's public half.
const (
	cosignImage    = "sha256:5b0745afdfec8efe7225bbe20cd87dc9139381e676400707ea979ec5406de3a8"
	cosignIndex    = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","size":887,"digest":"sha256:4b5596daa39bbb6297be885fdf98db43bc522da211db846589ca25de04a2f15a","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"}]}`
	cosignManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","size":2,"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"},"layers":[{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","size":659,"digest":"sha256:93da3b2a29e4ca00266738ba81a3e86db77445d29b74fb5a96a04723aaed19f7"}],"annotations":{"dev.sigstore.bundle.content":"dsse-envelope","dev.sigstore.bundle.predicateType":"https://sigstore.dev/cosign/sign/v1","org.opencontainers.image.created":"2026-10-15T07:34:12Z"},"subject":{"mediaType":"application/vnd.docker.distribution.manifest.v2+json","size":527,"digest":"sha256:5b0745afdfec8efe7225bbe20cd87dc9139381e676400707ea979ec5406de3a8"},"artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`
	cosignBundle   = `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"publicKey":{"hint":"pqjCYvUzo36pN8kn+i183TcAPg57HOIMeMsfTd0O9+U="}},"dsseEnvelope":{"payload":"eyJfdHlwZSI6Imh0dHBzOi8vaW4tdG90by5pby9TdGF0ZW1lbnQvdjEiLCJzdWJqZWN0IjpbeyJkaWdlc3QiOnsic2hhMjU2IjoiNWIwNzQ1YWZkZmVjOGVmZTcyMjViYmUyMGNkODdkYzkxMzkzODFlNjc2NDAwNzA3ZWE5NzllYzU0MDZkZTNhOCJ9LCJhbm5vdGF0aW9ucyI6e319XSwicHJlZGljYXRlVHlwZSI6Imh0dHBzOi8vc2lnc3RvcmUuZGV2L2Nvc2lnbi9zaWduL3YxIiwicHJlZGljYXRlIjp7fX0=","payloadType":"application/vnd.in-toto+json","signatures":[{"sig":"MEUCIQDAqRuAumvsBa4lN0SqRyIPRg1uFQRS6o2N76nytXkg7AIgYHanS2qkfjwuuzKzjcr1Z51uMKKo22Cx0TRU6/jVhP4="}]}}`
	cosignPub      = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEf/5qkPDMKabdDlNqjPLNLtG0KwSp
Grpxt2RYZltAF2CfsgdO7N89OqaQuE68foau1BHJDBWJzDYE0+wSj3sxNQ==
-----END PUBLIC KEY-----
`
)

// testCosignKey signs the images the roller's tests publish, as main CI's
// key signs them.
var testCosignKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

// cosignSigned is what cosign attaches to repository@digest when it signs
// it with key, as registry entries: the index under the referrers tag,
// the signature manifest, and the bundle, whose DSSE envelope signs the
// in-toto statement of cosign's sign predicate on digest.
func cosignSigned(key *ecdsa.PrivateKey, repository, digest string) map[string]string {
	statement := `{"_type":"https://in-toto.io/Statement/v1","subject":[{"digest":{"sha256":"` + strings.TrimPrefix(digest, "sha256:") +
		`"},"annotations":{}}],"predicateType":"https://sigstore.dev/cosign/sign/v1","predicate":{}}`
	sum := sha256.Sum256(fmt.Appendf(nil, "DSSEv1 28 application/vnd.in-toto+json %d %s", len(statement), statement))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, sum[:])
	bundle := `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"publicKey":{"hint":"test"}},` +
		`"dsseEnvelope":{"payload":"` + base64.StdEncoding.EncodeToString([]byte(statement)) + `","payloadType":"application/vnd.in-toto+json",` +
		`"signatures":[{"sig":"` + base64.StdEncoding.EncodeToString(sig) + `"}]}}`
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json",` +
		`"layers":[{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","digest":"` + sha256Of(bundle) + `"}],"subject":{"digest":"` + digest + `"}}`
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"` + sha256Of(manifest) + `","artifactType":"application/vnd.dev.sigstore.bundle.v0.3+json"}]}`
	return map[string]string{
		repository + "/manifests/sha256-" + strings.TrimPrefix(digest, "sha256:"): index,
		repository + "/manifests/" + sha256Of(manifest):                           manifest,
		repository + "/blobs/" + sha256Of(bundle):                                 bundle,
	}
}

// The roller rolls an image only with a signature by the factory's key
// over its digest (roadmap 46): cosign's own signature verifies; an image
// with none, one by another key, one whose statement names another image
// (a signature copied under this image's tag), or one whose statement was
// altered after signing is refused by name; a registry that cannot be
// read is an error, no refusal.
func TestRegistryVerifiesCosignSignatures(t *testing.T) {
	key, err := ParseSigningKey([]byte(cosignPub))
	if err != nil {
		t.Fatal(err)
	}
	other := "sha256:" + strings.Repeat("e", 64)
	tampered := strings.Replace(cosignBundle, `"payload":"eyJf`, `"payload":"eyJg`, 1)
	entries := map[string]string{
		"sf/img/manifests/sha256-" + strings.TrimPrefix(cosignImage, "sha256:"):      cosignIndex,
		"sf/img/manifests/" + sha256Of(cosignManifest):                               cosignManifest,
		"sf/img/blobs/" + sha256Of(cosignBundle):                                     cosignBundle,
		"sf/copied/manifests/sha256-" + strings.Repeat("e", 64):                      cosignIndex,
		"sf/copied/manifests/" + sha256Of(cosignManifest):                            cosignManifest,
		"sf/copied/blobs/" + sha256Of(cosignBundle):                                  cosignBundle,
		"sf/tampered/manifests/sha256-" + strings.TrimPrefix(cosignImage, "sha256:"): cosignIndex,
		"sf/tampered/manifests/" + sha256Of(cosignManifest):                          cosignManifest,
		"sf/tampered/blobs/" + sha256Of(cosignBundle):                                tampered,
	}
	reg, _ := testRegistry(t, entries)
	ctx := context.Background()
	tok, err := reg.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Verify(ctx, tok, "sf/img", cosignImage, key); err != nil {
		t.Fatalf("cosign's signature: %v", err)
	}
	stranger, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, repository, digest string
		key                      *ecdsa.PublicKey
		want                     string
	}{
		{"another key", "sf/img", cosignImage, &stranger.PublicKey,
			"sf/img@" + cosignImage + " carries no signature by the factory's key over this digest (1 by another key or over another digest)"},
		{"no signature", "sf/img", other, key, "sf/img@" + other + " carries no signature"},
		{"copied", "sf/copied", other, key,
			"sf/copied@" + other + " carries no signature by the factory's key over this digest (1 by another key or over another digest)"},
		{"tampered", "sf/tampered", cosignImage, key,
			"sf/tampered@" + cosignImage + " carries no signature by the factory's key over this digest (1 by another key or over another digest)"},
	} {
		var refusal ErrUnsigned
		if err := reg.Verify(ctx, tok, c.repository, c.digest, c.key); !errors.As(err, &refusal) || err.Error() != c.want {
			t.Errorf("%s: %v, want the refusal %q", c.name, err, c.want)
		}
	}
	// the generated signatures of the roller's tests verify the same way
	maps.Copy(entries, cosignSigned(testCosignKey, "sf/made", other))
	if err := reg.Verify(ctx, tok, "sf/made", other, &testCosignKey.PublicKey); err != nil {
		t.Fatalf("a test signature: %v", err)
	}
	down := &Registry{BaseURL: "http://127.0.0.1:1"}
	var refusal ErrUnsigned
	if err := down.Verify(ctx, tok, "sf/img", cosignImage, key); err == nil || errors.As(err, &refusal) {
		t.Fatalf("an unreachable registry: %v, want an error and no refusal", err)
	}
}

// The key is cosign.pub as cosign generate-key-pair writes it: a PEM
// PUBLIC KEY of ECDSA on P-256, and nothing else.
func TestParseSigningKeyTakesCosignsPublicKeyOnly(t *testing.T) {
	if _, err := ParseSigningKey([]byte(cosignPub)); err != nil {
		t.Fatalf("cosign.pub: %v", err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"empty":   "",
		"private": strings.Replace(cosignPub, "PUBLIC KEY", "ENCRYPTED SIGSTORE PRIVATE KEY", 2),
		"P-384":   string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	} {
		if _, err := ParseSigningKey([]byte(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
