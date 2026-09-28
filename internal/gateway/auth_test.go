package gateway

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testAudience = "factory-gateway"

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func publicJWK(kid string, pub *rsa.PublicKey) jwk {
	return jwk{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		Use: "sig",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

type tokenOpts struct {
	key   *rsa.PrivateKey
	kid   string
	iss   string
	aud   any // string or []string
	sub   string
	saUID string // "" = claim absent
	exp   time.Time
	noExp bool
	alg   jwt.SigningMethod
	hmac  []byte
}

// authFixture is one issuer: a TLS JWKS server, its signing key, a rogue key,
// the gateway's own SA token file and CA bundle, and a ready Auth.
type authFixture struct {
	auth     *Auth
	iss      string
	signKey  *rsa.PrivateKey
	wrongKey *rsa.PrivateKey
	mu       sync.Mutex
	bearers  []string
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	signKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &authFixture{signKey: signKey, wrongKey: wrongKey}

	ownToken := "gateway-own-projected-token"
	ownPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(ownPath, []byte(ownToken), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openid/v1/jwks" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.bearers = append(f.bearers, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []jwk{publicJWK("test-key-1", &signKey.PublicKey)},
		})
	}))
	t.Cleanup(srv.Close)
	f.iss = srv.URL

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	f.auth, err = NewAuth(AuthOptions{
		Audience:     testAudience,
		FetchBase:    "", // tests fetch the issuer as-is
		OwnTokenPath: ownPath,
		CACertPath:   caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *authFixture) mint(t *testing.T, mod func(*tokenOpts)) string {
	t.Helper()
	o := tokenOpts{
		key:   f.signKey,
		kid:   "test-key-1",
		iss:   f.iss,
		aud:   testAudience,
		sub:   "system:serviceaccount:factory:task-42",
		saUID: "sa-uid-1",
		exp:   time.Now().Add(time.Hour),
		alg:   jwt.SigningMethodRS256,
	}
	if mod != nil {
		mod(&o)
	}
	return mintToken(t, o)
}

func mintToken(t *testing.T, o tokenOpts) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": o.iss,
		"sub": o.sub,
		"iat": time.Now().Unix(),
	}
	if o.aud != nil {
		claims["aud"] = o.aud
	}
	if o.saUID != "" {
		claims["kubernetes.io"] = map[string]any{
			"namespace":      "factory",
			"serviceaccount": map[string]any{"name": "task-42", "uid": o.saUID},
		}
	}
	if !o.noExp {
		claims["exp"] = o.exp.Unix()
	}
	tok := jwt.NewWithClaims(o.alg, claims)
	tok.Header["kid"] = o.kid
	var signed string
	var err error
	if o.alg == jwt.SigningMethodHS256 {
		signed, err = tok.SignedString(o.hmac)
	} else {
		signed, err = tok.SignedString(o.key)
	}
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func (f *authFixture) bearersSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bearers...)
}

func bearerRequest(raw string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if raw != "" {
		r.Header.Set("Authorization", "Bearer "+raw)
	}
	return r
}

func TestAuthenticate(t *testing.T) {
	f := newAuthFixture(t)

	tests := []struct {
		name    string
		req     *http.Request
		wantID  Identity
		wantErr string // "" with fails=false means success; "" with fails=true means any error
		fails   bool
	}{
		{
			name: "happy path",
			req:  bearerRequest(f.mint(t, nil)),
			wantID: Identity{
				Subject:   "system:serviceaccount:factory:task-42",
				Namespace: "factory",
				TaskID:    "42",
				SAUID:     "sa-uid-1",
			},
		},
		{
			name: "audience list containing ours",
			req: bearerRequest(f.mint(t, func(o *tokenOpts) {
				o.aud = []string{"other-svc", testAudience}
			})),
			wantID: Identity{
				Subject:   "system:serviceaccount:factory:task-42",
				Namespace: "factory",
				TaskID:    "42",
				SAUID:     "sa-uid-1",
			},
		},
		{
			name:    "service account uid missing",
			req:     bearerRequest(f.mint(t, func(o *tokenOpts) { o.saUID = "" })),
			wantErr: "uid",
			fails:   true,
		},
		{
			name:    "expired",
			req:     bearerRequest(f.mint(t, func(o *tokenOpts) { o.exp = time.Now().Add(-time.Minute) })),
			wantErr: "expired",
			fails:   true,
		},
		{
			name:    "expiration missing",
			req:     bearerRequest(f.mint(t, func(o *tokenOpts) { o.noExp = true })),
			wantErr: "exp",
			fails:   true,
		},
		{
			name:    "foreign audience",
			req:     bearerRequest(f.mint(t, func(o *tokenOpts) { o.aud = "other-svc" })),
			wantErr: "aud",
			fails:   true,
		},
		{
			name:    "wrong subject shape",
			req:     bearerRequest(f.mint(t, func(o *tokenOpts) { o.sub = "system:serviceaccount:factory:worker-1" })),
			wantErr: "sub",
			fails:   true,
		},
		{
			name:    "not a service account",
			req:     bearerRequest(f.mint(t, func(o *tokenOpts) { o.sub = "system:admin" })),
			wantErr: "sub",
			fails:   true,
		},
		{
			name: "unknown signing key kid",
			req: bearerRequest(f.mint(t, func(o *tokenOpts) {
				o.key, o.kid = f.wrongKey, "rogue-kid"
			})),
			fails: true,
		},
		{
			name: "signed by rogue key under known kid",
			req: bearerRequest(f.mint(t, func(o *tokenOpts) {
				o.key = f.wrongKey
			})),
			fails: true,
		},
		{
			name: "hs256 confusion attack",
			req: bearerRequest(f.mint(t, func(o *tokenOpts) {
				o.alg = jwt.SigningMethodHS256
				o.hmac = x509.MarshalPKCS1PublicKey(&f.signKey.PublicKey)
			})),
			fails: true,
		},
		{
			name:    "no bearer header",
			req:     bearerRequest(""),
			wantErr: "bearer",
			fails:   true,
		},
		{
			name:  "not a jwt",
			req:   bearerRequest("garbage.token"),
			fails: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := f.auth.Authenticate(tt.req)
			if tt.fails {
				if err == nil {
					t.Fatalf("Authenticate succeeded with %+v, want error", id)
				}
				if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Authenticate error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id != tt.wantID {
				t.Fatalf("identity = %+v, want %+v", id, tt.wantID)
			}
		})
	}

	bearers := f.bearersSeen()
	if len(bearers) == 0 {
		t.Fatal("JWKS server never fetched")
	}
	for _, b := range bearers {
		if b != "Bearer gateway-own-projected-token" {
			t.Fatalf("JWKS fetch Authorization = %q, want the gateway's own mounted SA token", b)
		}
	}
}

func TestJWKSURL(t *testing.T) {
	tests := []struct {
		name, iss, base, want string
		wantErr               bool
	}{
		{
			name: "in-cluster override replaces public endpoint",
			iss:  "https://cluster.aisoftwarefactory.world",
			base: "https://kubernetes.default.svc",
			want: "https://kubernetes.default.svc/openid/v1/jwks",
		},
		{
			name: "no override uses issuer as-is",
			iss:  "https://cluster.example.com",
			base: "",
			want: "https://cluster.example.com/openid/v1/jwks",
		},
		{
			name: "trailing slash does not double",
			iss:  "https://cluster.example.com/",
			base: "https://kubernetes.default.svc",
			want: "https://kubernetes.default.svc/openid/v1/jwks",
		},
		{
			name: "issuer keeps its path",
			iss:  "https://cluster.example.com/issuer",
			base: "https://kubernetes.default.svc",
			want: "https://kubernetes.default.svc/issuer/openid/v1/jwks",
		},
		{
			name:    "not a url",
			iss:     "not-a-url",
			base:    "https://kubernetes.default.svc",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jwksURL(tt.iss, tt.base)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("jwksURL(%q, %q) = %q, want error", tt.iss, tt.base, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("jwksURL(%q, %q): %v", tt.iss, tt.base, err)
			}
			if got != tt.want {
				t.Fatalf("jwksURL(%q, %q) = %q, want %q", tt.iss, tt.base, got, tt.want)
			}
		})
	}
}

// Finding-1 regression: main wires CACertPath: InClusterCACertPath, so the
// JWKS fetch to https://kubernetes.default.svc must verify against the pod's
// CA bundle, and an absent bundle must fail construction (fail-fast boot).
func TestInClusterCACertWiring(t *testing.T) {
	if InClusterCACertPath != "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" {
		t.Fatalf("InClusterCACertPath = %q, want the mounted serviceaccount CA path", InClusterCACertPath)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if got := InClusterFetchBase(); got != "https://10.96.0.1:443" {
		t.Fatalf("InClusterFetchBase = %q, want the API Service address", got)
	}

	absent := AuthOptions{Audience: testAudience, FetchBase: InClusterFetchBase(), CACertPath: filepath.Join(t.TempDir(), "absent.crt")}
	if _, err := NewAuth(absent); err == nil {
		t.Fatal("absent CA bundle must fail NewAuth, want fail-fast boot")
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	auth, err := NewAuth(AuthOptions{Audience: testAudience, FetchBase: InClusterFetchBase(), CACertPath: caPath})
	if err != nil {
		t.Fatal(err)
	}
	bt, ok := auth.client.Transport.(bearerTransport)
	if !ok {
		t.Fatalf("JWKS transport = %T, want bearerTransport", auth.client.Transport)
	}
	tlsCfg := bt.next.(*http.Transport).TLSClientConfig
	if tlsCfg == nil || tlsCfg.RootCAs == nil {
		t.Fatal("CA pool never reached the JWKS client's TLS config")
	}
}

// Finding-2 regression: the JWKS client cache is keyed by the derived URL
// (distinct issuers collapse onto one entry under the fetch-base override)
// and stays bounded against distinct unverified issuer paths.
func TestKeyfuncCacheBoundedAndKeyedByURL(t *testing.T) {
	signKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []jwk{publicJWK("test-key-1", &signKey.PublicKey)},
		})
	}))
	defer srv.Close()
	ownPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(ownPath, []byte("gateway-own-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	auth, err := NewAuth(AuthOptions{Audience: testAudience, FetchBase: srv.URL, OwnTokenPath: ownPath})
	if err != nil {
		t.Fatal(err)
	}
	token := func(iss string) string {
		return mintToken(t, tokenOpts{
			key: signKey, kid: "test-key-1", iss: iss, aud: testAudience,
			sub: "system:serviceaccount:factory:task-42", saUID: "sa-uid-1",
			exp: time.Now().Add(time.Hour), alg: jwt.SigningMethodRS256,
		})
	}

	for _, iss := range []string{"https://cluster-a.example", "https://cluster-b.example"} {
		id, err := auth.Authenticate(bearerRequest(token(iss)))
		if err != nil {
			t.Fatalf("issuer %s: %v", iss, err)
		}
		if id.TaskID != "42" {
			t.Fatalf("identity = %+v", id)
		}
	}
	if n := len(auth.keyfuncBy); n != 1 {
		t.Fatalf("keyfunc cache has %d entries for issuers collapsed onto one URL, want 1", n)
	}

	for i := 0; i < keyfuncCacheCap*3; i++ {
		iss := fmt.Sprintf("https://cluster.example/attack-%d", i)
		if _, err := auth.Authenticate(bearerRequest(token(iss))); err != nil {
			t.Fatalf("issuer %s: %v", iss, err)
		}
	}
	if n := len(auth.keyfuncBy); n > keyfuncCacheCap {
		t.Fatalf("keyfunc cache grew to %d entries, want <= %d", n, keyfuncCacheCap)
	}
}
