package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Identity is the verified caller: a sandbox pod's service account.
type Identity struct {
	Subject   string // system:serviceaccount:<namespace>:task-<id>
	Namespace string
	TaskID    string
}

const (
	// defaultAudience is what sandbox token requests ask for; override with
	// GATEWAY_AUDIENCE.
	defaultAudience = "factory-gateway"

	// InClusterFetchBase is the JWKS fetch override in production. The token's
	// issuer is the cluster's public endpoint URL (Talos default), which pods
	// cannot reach; the in-cluster API Service serves the same keys.
	InClusterFetchBase = "https://kubernetes.default.svc"

	defaultOwnTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultCACertPath   = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

type AuthOptions struct {
	Audience     string
	FetchBase    string // "" = fetch the issuer as-is
	OwnTokenPath string // gateway pod's own mounted SA token
	CACertPath   string // pod CA bundle, "" = system roots
}

// Auth verifies sandbox ServiceAccount JWTs against the cluster's OIDC
// issuer. The issuer is read from each token (Talos derives it from the
// cluster endpoint URL, so it must never be hardcoded); the JWKS fetch is
// redirected through FetchBase independently of claim validation.
type Auth struct {
	opts      AuthOptions
	client    *http.Client
	mu        sync.Mutex
	keyfuncBy map[string]keyfunc.Keyfunc
}

// bearerTransport injects the gateway pod's own mounted SA token on every
// JWKS fetch: the apiserver's discovery endpoints are RBAC-guarded, and the
// built-in system:serviceaccounts binding lets every SA read them. The token
// file is re-read per request because the kubelet rotates it.
type bearerTransport struct {
	tokenPath string
	next      http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := os.ReadFile(t.tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read own SA token: %w", err)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	return t.next.RoundTrip(r)
}

func NewAuth(opts AuthOptions) (*Auth, error) {
	if opts.Audience == "" {
		opts.Audience = defaultAudience
	}
	if opts.OwnTokenPath == "" {
		opts.OwnTokenPath = defaultOwnTokenPath
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opts.CACertPath != "" {
		pemBytes, err := os.ReadFile(opts.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no certificates in %s", opts.CACertPath)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &Auth{
		opts: opts,
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: bearerTransport{tokenPath: opts.OwnTokenPath, next: transport},
		},
	}, nil
}

func (a *Auth) Authenticate(r *http.Request) (Identity, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(raw) == "" {
		return Identity{}, errors.New("missing bearer token")
	}
	return a.Verify(r.Context(), strings.TrimSpace(raw))
}

// Verify checks signature and claims of a raw JWT and maps its subject to a
// task identity. RS256 only; expiration required; audience must be the
// configured one; the subject must be a task service account.
func (a *Auth) Verify(ctx context.Context, raw string) (Identity, error) {
	var unverified jwt.RegisteredClaims
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &unverified); err != nil {
		return Identity{}, fmt.Errorf("parse token: %w", err)
	}
	iss := unverified.Issuer
	if iss == "" {
		return Identity{}, errors.New("token has no issuer")
	}
	k, err := a.keyfunc(iss)
	if err != nil {
		return Identity{}, err
	}
	claims := &jwt.RegisteredClaims{}
	if _, err := jwt.ParseWithClaims(raw, claims, k.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(iss),
		jwt.WithAudience(a.opts.Audience),
		jwt.WithExpirationRequired(),
	); err != nil {
		return Identity{}, fmt.Errorf("verify token: %w", err)
	}
	return taskIdentity(claims.Subject)
}

// keyfunc returns a cached JWKS client for one issuer. keyfunc launches a
// refresh goroutine per instance, so exactly one is kept per issuer; the
// background context keeps it alive for the process lifetime.
func (a *Auth) keyfunc(iss string) (keyfunc.Keyfunc, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if k, ok := a.keyfuncBy[iss]; ok {
		return k, nil
	}
	u, err := jwksURL(iss, a.opts.FetchBase)
	if err != nil {
		return nil, err
	}
	k, err := keyfunc.NewDefaultOverrideCtx(context.Background(), []string{u}, keyfunc.Override{
		Client:          a.client,
		RefreshInterval: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("jwks %s: %w", u, err)
	}
	if a.keyfuncBy == nil {
		a.keyfuncBy = map[string]keyfunc.Keyfunc{}
	}
	a.keyfuncBy[iss] = k
	return k, nil
}

// jwksURL derives the JWKS URL from the token's issuer, optionally replacing
// scheme and host with an in-cluster fetch base. Claim validation and the
// fetch URL are independent.
func jwksURL(iss, fetchBase string) (string, error) {
	u, err := url.Parse(iss)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("issuer %q is not a URL", iss)
	}
	if fetchBase != "" {
		b, err := url.Parse(fetchBase)
		if err != nil || b.Scheme == "" || b.Host == "" {
			return "", fmt.Errorf("fetch base %q is not a URL", fetchBase)
		}
		u.Scheme, u.Host = b.Scheme, b.Host
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/openid/v1/jwks"
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

// taskIdentity maps a verified subject to its task. Only sandbox identities
// are served: system:serviceaccount:<namespace>:task-<id>.
func taskIdentity(sub string) (Identity, error) {
	const prefix = "task-"
	parts := strings.Split(sub, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" ||
		parts[2] == "" || !strings.HasPrefix(parts[3], prefix) || len(parts[3]) == len(prefix) {
		return Identity{}, fmt.Errorf("subject %q is not a task service account", sub)
	}
	return Identity{Subject: sub, Namespace: parts[2], TaskID: parts[3][len(prefix):]}, nil
}
