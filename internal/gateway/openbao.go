package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenBao signs sandbox SSH keys with the SSH CA (secrets engine
// ssh-client-signer). The gateway logs in with its ServiceAccount token
// through Kubernetes auth; its policy may only sign with SSHRole.
type OpenBao struct {
	BaseURL   string // http://openbao.platform.svc.cluster.local:8200
	AuthRole  string // the Kubernetes auth role bound to the gateway's ServiceAccount
	SSHRole   string // the ssh-client-signer role for commissioned servers
	TokenFile string // the pod's ServiceAccount token
	HTTP      *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	caKey   string
}

const sshMount = "ssh-client-signer"

// CAPublicKey is the CA's public key, which commissioned servers trust
// (unauthenticated in OpenBao; cached: it changes only on a CA rotation).
func (o *OpenBao) CAPublicKey(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.caKey != "" {
		return o.caKey, nil
	}
	b, err := call(ctx, o.HTTP, http.MethodGet, o.BaseURL+"/v1/"+sshMount+"/public_key", http.Header{}, nil)
	if err != nil {
		return "", fmt.Errorf("openbao ca public key: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if !strings.HasPrefix(key, "ssh-") && !strings.HasPrefix(key, "ecdsa-") {
		return "", fmt.Errorf("openbao ca public key: unexpected answer %q", truncate(key, 80))
	}
	o.caKey = key
	return key, nil
}

// Sign returns an SSH user certificate for publicKey, valid for principal
// for ttl, with keyID as its key id (it shows in the server's auth log).
func (o *OpenBao) Sign(ctx context.Context, publicKey, principal string, ttl time.Duration, keyID string) (string, error) {
	body := map[string]string{
		"public_key": publicKey, "valid_principals": principal, "cert_type": "user",
		"ttl": strconv.Itoa(int(ttl.Seconds())) + "s", "key_id": keyID,
	}
	for attempt := 0; ; attempt++ {
		token, err := o.login(ctx, attempt > 0)
		if err != nil {
			return "", err
		}
		b, err := call(ctx, o.HTTP, http.MethodPost, o.BaseURL+"/v1/"+sshMount+"/sign/"+o.SSHRole,
			http.Header{"X-Vault-Token": {token}}, body)
		var pe *providerError
		if errors.As(err, &pe) && pe.status == http.StatusForbidden && attempt == 0 {
			continue // a revoked or expired token: log in again once
		}
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
}

// login returns a client token, logging in when there is none, it is
// about to expire, or fresh is set.
func (o *OpenBao) login(ctx context.Context, fresh bool) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !fresh && o.token != "" && time.Until(o.expires) > time.Minute {
		return o.token, nil
	}
	jwt, err := os.ReadFile(o.TokenFile)
	if err != nil {
		return "", fmt.Errorf("openbao login: %w", err)
	}
	b, err := call(ctx, o.HTTP, http.MethodPost, o.BaseURL+"/v1/auth/kubernetes/login", http.Header{},
		map[string]string{"role": o.AuthRole, "jwt": strings.TrimSpace(string(jwt))})
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
	o.token, o.expires = out.Auth.ClientToken, time.Now().Add(time.Duration(out.Auth.LeaseDuration)*time.Second)
	return o.token, nil
}
