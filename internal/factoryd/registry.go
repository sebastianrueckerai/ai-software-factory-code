package factoryd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Registry reads image manifests from Forgejo's container registry. It
// asks BaseURL, the in-cluster Forgejo, for the bearer token itself with
// User and Password (a read:package token) and never follows a realm the
// registry names.
type Registry struct {
	BaseURL        string
	HTTP           *http.Client // nil = a 15 s timeout
	User, Password string
}

// ImageManifest is what an image reference resolves to: Digest, the
// manifest or index the reference names, and Platform, the linux/amd64
// image manifest inside an index (the index itself otherwise). Main CI's
// indexes carry a provenance attestation with build timestamps, so two
// builds of the same inputs push two index digests around one Platform.
type ImageManifest struct {
	Digest   string
	Platform string
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

// Resolve reads repository's manifest at reference, a tag or a digest,
// with a bearer from Token; a body that does not hash to a digest
// reference is refused.
func (r *Registry) Resolve(ctx context.Context, token, repository, reference string) (ImageManifest, error) {
	body, err := r.get(ctx, "/v2/"+repository+"/manifests/"+reference, bearer(token))
	if err != nil {
		return ImageManifest{}, err
	}
	sum := sha256.Sum256(body)
	m := ImageManifest{Digest: "sha256:" + hex.EncodeToString(sum[:])}
	if strings.HasPrefix(reference, "sha256:") && reference != m.Digest {
		return ImageManifest{}, fmt.Errorf("registry %s@%s: the manifest's digest %s does not match", repository, reference, m.Digest)
	}
	var doc struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ImageManifest{}, fmt.Errorf("registry %s:%s: %w", repository, reference, err)
	}
	if doc.Manifests == nil {
		m.Platform = m.Digest
		return m, nil
	}
	for _, d := range doc.Manifests {
		if d.Platform.OS == "linux" && d.Platform.Architecture == "amd64" {
			m.Platform = d.Digest
			return m, nil
		}
	}
	return ImageManifest{}, fmt.Errorf("registry %s:%s: the index has no linux/amd64 image", repository, reference)
}

// bearer authorizes a read of a manifest or blob with token.
func bearer(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", manifestAccept)
	}
}

// The signatures main CI's cosign 3 attaches to an image it signs by
// digest (roadmap 46): Forgejo's registry has no referrers API, so cosign
// keeps them under the tag sha256-<hex> of the image's digest, an index
// whose entries of artifactType bundleType are manifests with a sigstore
// bundle as layer. The bundle is a DSSE envelope over an in-toto statement
// of predicate cosignSign whose subject is the image's digest; no
// transparency log entry comes with it.
const (
	bundleType      = "application/vnd.dev.sigstore.bundle.v0.3+json"
	inTotoPayload   = "application/vnd.in-toto+json"
	inTotoStatement = "https://in-toto.io/Statement/v1"
	cosignSign      = "https://sigstore.dev/cosign/sign/v1"
)

// signatureRef is the one form of digest a signature index or manifest
// may name: it goes into a request's path.
var signatureRef = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ErrUnsigned refuses an image no signature by the factory's key covers;
// its message names the image and why.
type ErrUnsigned string

func (e ErrUnsigned) Error() string { return string(e) }

// ParseSigningKey reads the public key of the factory's cosign key pair,
// PEM as cosign generate-key-pair writes cosign.pub: ECDSA on P-256.
func ParseSigningKey(raw []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("signing key: no PEM PUBLIC KEY block")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	if ec, ok := k.(*ecdsa.PublicKey); ok && ec.Curve == elliptic.P256() {
		return ec, nil
	}
	return nil, fmt.Errorf("signing key: a %T, want ECDSA on P-256 as cosign generate-key-pair writes", k)
}

// Verify reads the signatures of repository@digest with a bearer from
// Token: nil when one is key's over digest, an ErrUnsigned when none is,
// any other error when a read fails. A signature the registry lists but
// cannot serve, or that does not parse, is no signature by key.
func (r *Registry) Verify(ctx context.Context, token, repository, digest string, key *ecdsa.PublicKey) error {
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return fmt.Errorf("registry %s@%s: not a sha256 digest", repository, digest)
	}
	body, err := r.get(ctx, "/v2/"+repository+"/manifests/sha256-"+hexDigest, bearer(token))
	if errors.Is(err, ErrNotFound) {
		return ErrUnsigned(fmt.Sprintf("%s@%s carries no signature", repository, digest))
	}
	if err != nil {
		return err
	}
	var index struct {
		Manifests []struct {
			Digest       string `json:"digest"`
			ArtifactType string `json:"artifactType"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return ErrUnsigned(fmt.Sprintf("%s@%s: its signature index does not parse: %v", repository, digest, err))
	}
	n := 0
	for _, m := range index.Manifests {
		if m.ArtifactType != bundleType {
			continue
		}
		n++
		if !signatureRef.MatchString(m.Digest) {
			continue
		}
		if ok, err := r.signedBy(ctx, token, repository, m.Digest, hexDigest, key); err != nil || ok {
			return err
		}
	}
	if n == 0 {
		return ErrUnsigned(fmt.Sprintf("%s@%s carries no signature", repository, digest))
	}
	return ErrUnsigned(fmt.Sprintf("%s@%s carries no signature by the factory's key over this digest (%d by another key or over another digest)", repository, digest, n))
}

// signedBy reports whether the signature manifest of repository carries a
// bundle by key over the image of hexDigest.
func (r *Registry) signedBy(ctx context.Context, token, repository, manifest, hexDigest string, key *ecdsa.PublicKey) (bool, error) {
	body, err := r.get(ctx, "/v2/"+repository+"/manifests/"+manifest, bearer(token))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var m struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if json.Unmarshal(body, &m) != nil {
		return false, nil
	}
	for _, l := range m.Layers {
		if l.MediaType != bundleType || !signatureRef.MatchString(l.Digest) {
			continue
		}
		bundle, err := r.get(ctx, "/v2/"+repository+"/blobs/"+l.Digest, bearer(token))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if bundleSignedBy(bundle, hexDigest, key) {
			return true, nil
		}
	}
	return false, nil
}

// bundleSignedBy reports whether bundle's DSSE envelope carries a
// signature by key over its payload, and the payload is cosign's
// statement on the image of hexDigest. The signature covers the envelope's
// pre-authentication encoding of payload type and payload.
func bundleSignedBy(bundle []byte, hexDigest string, key *ecdsa.PublicKey) bool {
	var b struct {
		Envelope struct {
			Payload     []byte `json:"payload"`
			PayloadType string `json:"payloadType"`
			Signatures  []struct {
				Sig []byte `json:"sig"`
			} `json:"signatures"`
		} `json:"dsseEnvelope"`
	}
	if json.Unmarshal(bundle, &b) != nil || b.Envelope.PayloadType != inTotoPayload {
		return false
	}
	e := b.Envelope
	sum := sha256.Sum256(fmt.Appendf(nil, "DSSEv1 %d %s %d %s", len(e.PayloadType), e.PayloadType, len(e.Payload), e.Payload))
	signed := false
	for _, s := range e.Signatures {
		signed = signed || ecdsa.VerifyASN1(key, sum[:], s.Sig)
	}
	if !signed {
		return false
	}
	var st struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Subject       []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
	}
	if json.Unmarshal(e.Payload, &st) != nil || st.Type != inTotoStatement || st.PredicateType != cosignSign {
		return false
	}
	for _, s := range st.Subject {
		if s.Digest["sha256"] == hexDigest {
			return true
		}
	}
	return false
}

// Token reads a bearer for the registry with the credential.
func (r *Registry) Token(ctx context.Context) (string, error) {
	body, err := r.get(ctx, "/v2/token", func(req *http.Request) { req.SetBasicAuth(r.User, r.Password) })
	if err != nil {
		return "", err
	}
	var res struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &res); err != nil || res.Token == "" {
		return "", fmt.Errorf("registry token: no token in the answer")
	}
	return res.Token, nil
}

func (r *Registry) get(ctx context.Context, path string, auth func(*http.Request)) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	auth(req)
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry GET %s: %w", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxForgejoAnswer+1))
	if err != nil {
		return nil, fmt.Errorf("registry GET %s: %w", path, err)
	}
	if len(body) > maxForgejoAnswer {
		return nil, fmt.Errorf("registry GET %s: the answer is longer than 1 MiB", path)
	}
	if res.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("registry GET %s: status 404: %w", path, ErrNotFound)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry GET %s: status %d", path, res.StatusCode)
	}
	return body, nil
}
