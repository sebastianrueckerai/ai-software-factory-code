package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Registry reads image manifests from Forgejo's container registry. It
// asks BaseURL, the in-cluster Forgejo, for the bearer token itself and
// never follows a realm the registry names.
type Registry struct {
	BaseURL    string
	HTTP       *http.Client // nil = a 15 s timeout
	Credential func(ctx context.Context) (user, password string, err error)
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
	body, err := r.get(ctx, "/v2/"+repository+"/manifests/"+reference, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", manifestAccept)
	})
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

// Token reads a bearer for the registry with the credential.
func (r *Registry) Token(ctx context.Context) (string, error) {
	user, password, err := r.Credential(ctx)
	if err != nil {
		return "", fmt.Errorf("registry credential: %w", err)
	}
	body, err := r.get(ctx, "/v2/token", func(req *http.Request) { req.SetBasicAuth(user, password) })
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
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry GET %s: status %d", path, res.StatusCode)
	}
	return body, nil
}

// PullSecretCredential reads host's credential from a
// kubernetes.io/dockerconfigjson secret at each call, so a rotated secret
// is picked up without a restart.
func PullSecretCredential(kube interface {
	GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error)
}, namespace, name, host string) func(ctx context.Context) (string, string, error) {
	return func(ctx context.Context) (string, string, error) {
		data, err := kube.GetSecretData(ctx, namespace, name)
		if err != nil {
			return "", "", err
		}
		var cfg struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		if err := json.Unmarshal([]byte(data[".dockerconfigjson"]), &cfg); err != nil {
			return "", "", fmt.Errorf("pull secret %s/%s: %w", namespace, name, err)
		}
		entry, ok := cfg.Auths[host]
		if !ok {
			return "", "", fmt.Errorf("pull secret %s/%s has no entry for %s", namespace, name, host)
		}
		raw, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return "", "", fmt.Errorf("pull secret %s/%s: %w", namespace, name, err)
		}
		user, password, ok := strings.Cut(string(raw), ":")
		if !ok {
			return "", "", fmt.Errorf("pull secret %s/%s: the %s entry is no user:password", namespace, name, host)
		}
		return user, password, nil
	}
}
