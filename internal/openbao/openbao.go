// Package openbao is the OpenBao client of factoryd and the gateway: a
// Kubernetes auth login with the pod's projected ServiceAccount token of
// audience openbao, the SSH CA (secrets engine ssh-client-signer) and
// kv-v2 reads. Each caller's role grants only what it uses.
package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// Client logs in as AuthRole and keeps its client token until shortly
// before it expires.
type Client struct {
	BaseURL   string // http://openbao.platform.svc.cluster.local:8200
	AuthRole  string // the Kubernetes auth role bound to the caller's ServiceAccount
	SSHRole   string // the ssh-client-signer role for commissioned servers
	TokenFile string // the projected token of audience openbao
	HTTP      *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	caKey   string
	caAt    time.Time
}

const sshMount = "ssh-client-signer"

// TokenPath is the pod's projected ServiceAccount token with audience
// "openbao", the only audience OpenBao's Kubernetes auth roles accept
// (k8s/).
const TokenPath = "/var/run/secrets/openbao/token" // #nosec G101 -- a token file path, not a credential value

// caTTL bounds how long a fetched CA public key is used: a CA rotation
// reaches new servers within it.
const caTTL = time.Hour

// maxAnswer bounds an answer read from OpenBao.
const maxAnswer = 1 << 20

type statusError struct {
	status int
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("openbao answered %d: %s", e.status, textutil.Truncate(e.body, 500))
}

func (c *Client) call(ctx context.Context, method, target string, header http.Header, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+target, rd)
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req) // #nosec G704 -- BaseURL is OpenBao's fixed in-cluster address
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, &statusError{status: res.StatusCode, body: string(b)}
	}
	return b, nil
}

// authed calls OpenBao with the client token, logging in again once on a
// 403 (a revoked or expired token).
func (c *Client) authed(ctx context.Context, method, target string, body any) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.login(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		b, err := c.call(ctx, method, target, http.Header{"X-Vault-Token": {token}}, body)
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusForbidden && attempt == 0 {
			continue
		}
		return b, err
	}
}

// CAPublicKey is the CA's public key, which commissioned servers trust
// (unauthenticated in OpenBao), fetched at most hourly.
func (c *Client) CAPublicKey(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.caKey != "" && time.Since(c.caAt) < caTTL {
		return c.caKey, nil
	}
	b, err := c.call(ctx, http.MethodGet, "/v1/"+sshMount+"/public_key", http.Header{}, nil)
	if err != nil {
		return "", fmt.Errorf("openbao ca public key: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if !strings.HasPrefix(key, "ssh-") && !strings.HasPrefix(key, "ecdsa-") {
		return "", fmt.Errorf("openbao ca public key: unexpected answer %q", textutil.Truncate(key, 80))
	}
	c.caKey, c.caAt = key, time.Now()
	return key, nil
}

// Sign returns an SSH user certificate for publicKey, valid for principal
// for ttl, with keyID as its key id (it shows in the server's auth log).
func (c *Client) Sign(ctx context.Context, publicKey, principal string, ttl time.Duration, keyID string) (string, error) {
	b, err := c.authed(ctx, http.MethodPost, "/v1/"+sshMount+"/sign/"+c.SSHRole, map[string]string{
		"public_key": publicKey, "valid_principals": principal, "cert_type": "user",
		"ttl": strconv.Itoa(int(ttl.Seconds())) + "s", "key_id": keyID,
	})
	if err != nil {
		return "", fmt.Errorf("openbao sign: %w", err)
	}
	var out struct {
		Data struct {
			SignedKey string `json:"signed_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Data.SignedKey == "" {
		return "", fmt.Errorf("openbao sign: no signed_key in the answer")
	}
	return strings.TrimSpace(out.Data.SignedKey), nil
}

// KV reads the kv-v2 secret at path, "<mount>/<name>", as its fields. An
// error never quotes a successful answer: it holds the secret.
func (c *Client) KV(ctx context.Context, path string) (map[string]string, error) {
	mount, name, ok := strings.Cut(path, "/")
	if !ok || mount == "" || name == "" {
		return nil, fmt.Errorf("openbao kv %q: want <mount>/<name>", path)
	}
	b, err := c.authed(ctx, http.MethodGet, "/v1/"+mount+"/data/"+name, nil)
	if err != nil {
		return nil, fmt.Errorf("openbao kv %s: %w", path, err)
	}
	var out struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Data.Data == nil {
		return nil, fmt.Errorf("openbao kv %s: the answer holds no map of string fields", path)
	}
	return out.Data.Data, nil
}

// login returns a client token, logging in when there is none, it is
// about to expire, or fresh is set.
func (c *Client) login(ctx context.Context, fresh bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !fresh && c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	jwt, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("openbao login: %w", err)
	}
	b, err := c.call(ctx, http.MethodPost, "/v1/auth/kubernetes/login", http.Header{},
		map[string]string{"role": c.AuthRole, "jwt": strings.TrimSpace(string(jwt))})
	if err != nil {
		return "", fmt.Errorf("openbao login: %w", err)
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Auth.ClientToken == "" {
		return "", errors.New("openbao login: no client token in the answer")
	}
	c.token, c.expires = out.Auth.ClientToken, time.Now().Add(time.Duration(out.Auth.LeaseDuration)*time.Second)
	return c.token, nil
}
