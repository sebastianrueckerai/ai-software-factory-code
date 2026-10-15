package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
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
	// SAUID is the service account's UID. A requeued task runs under a
	// recreated account, so the UID tells its runs apart.
	SAUID string
	// Session is the session whose worker pod the token is bound to, the
	// pod's name past SessionPod; "" for a token of any other pod.
	Session string
}

// SessionPod prefixes the name of a session's worker pod, which factoryd
// names SessionPod+<session id>: the API server signs the pod a projected
// token is bound to into it, so no agent names its session.
const SessionPod = "worker-"

type saClaims struct {
	jwt.RegisteredClaims
	K8s struct {
		ServiceAccount struct {
			UID string `json:"uid"`
		} `json:"serviceaccount"`
		Pod struct {
			Name string `json:"name"`
		} `json:"pod"`
	} `json:"kubernetes.io"`
}

const (
	// defaultAudience is what sandbox token requests ask for; override with
	// GATEWAY_AUDIENCE.
	defaultAudience = "factory-gateway"

	// InClusterTokenPath is the pod's own ServiceAccount token: the JWKS
	// fetch presents it.
	InClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- projected token file path, not a credential value

	// InClusterCACertPath is the pod's mounted CA bundle; the JWKS fetch to
	// InClusterFetchBase() presents a certificate signed by it, so production
	// wiring must pass this as CACertPath (a missing bundle fails boot).
	InClusterCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	// keyfuncCacheCap bounds the JWKS client cache below.
	keyfuncCacheCap = 8
)

// InClusterFetchBase is the JWKS fetch override in production. The token's
// issuer is the cluster's public endpoint URL (Talos default), which pods
// cannot reach; the in-cluster API Service serves the same keys. Its
// address comes from the pod environment, so no DNS name is needed: the
// gateway's egress policy admits the API server, not its names.
func InClusterFetchBase() string {
	return "https://" + net.JoinHostPort(os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"))
}

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
	opts   AuthOptions
	client *http.Client
	mu     sync.Mutex
	// keyfuncBy is keyed by the DERIVED JWKS URL, never the raw iss claim,
	// which is unverified input. Each value's cancel reaps the entry's
	// refresh goroutine on eviction.
	keyfuncBy map[string]cachedKeyfunc
}

type cachedKeyfunc struct {
	kf     keyfunc.Keyfunc
	cancel context.CancelFunc
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
		opts.OwnTokenPath = InClusterTokenPath
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
	claims := &saClaims{}
	if _, err := jwt.ParseWithClaims(raw, claims, k.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(iss),
		jwt.WithAudience(a.opts.Audience),
		jwt.WithExpirationRequired(),
	); err != nil {
		return Identity{}, fmt.Errorf("verify token: %w", err)
	}
	id, err := taskIdentity(claims.Subject)
	if err != nil {
		return Identity{}, err
	}
	if claims.K8s.ServiceAccount.UID == "" {
		return Identity{}, errors.New("token has no service account uid")
	}
	id.SAUID = claims.K8s.ServiceAccount.UID
	if session, ok := strings.CutPrefix(claims.K8s.Pod.Name, SessionPod); ok {
		id.Session = session
	}
	return id, nil
}

// keyfunc returns a cached JWKS client for the URL derived from the token's
// issuer. At the cap one arbitrary entry is evicted and its context
// cancelled, ending its refresh goroutine: legitimate traffic holds exactly
// one entry (the fetch-base override collapses every issuer onto one URL),
// so eviction can only reclaim attacker noise or cost one legit token a
// single JWKS re-fetch — arbitrary victim selection is correct here.
func (a *Auth) keyfunc(iss string) (keyfunc.Keyfunc, error) {
	u, err := jwksURL(iss, a.opts.FetchBase)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.keyfuncBy[u]; ok {
		return c.kf, nil
	}
	if len(a.keyfuncBy) >= keyfuncCacheCap {
		for k, v := range a.keyfuncBy {
			v.cancel()
			delete(a.keyfuncBy, k)
			break
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	k, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{u}, keyfunc.Override{
		Client:          a.client,
		RefreshInterval: time.Hour,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("jwks %s: %w", u, err)
	}
	if a.keyfuncBy == nil {
		a.keyfuncBy = map[string]cachedKeyfunc{}
	}
	a.keyfuncBy[u] = cachedKeyfunc{kf: k, cancel: cancel}
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
