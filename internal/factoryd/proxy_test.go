package factoryd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityProxyRereadsRotatingTokenFile(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("jwt-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &IdentityProxy{GatewayURL: upstream.URL, TokenFile: tokenFile}

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer sandbox-dummy")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Body.String() != `{"ok":true}` {
			t.Fatalf("upstream body not proxied: %q", rec.Body.String())
		}
		return rec.Code
	}

	// The kubelet rotates the projected JWT: same proxy, new file contents.
	for _, tok := range []string{"jwt-two", "jwt-three"} {
		if err := os.WriteFile(tokenFile, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := do(); code != 200 {
			t.Fatalf("status = %d", code)
		}
	}
	want := []string{"Bearer jwt-two", "Bearer jwt-three"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("upstream authorizations = %v, want %v (fresh read per request)", seen, want)
	}
}

func TestIdentityProxyFailsLoudlyWithoutToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached without a task identity")
	}))
	defer upstream.Close()
	p := &IdentityProxy{GatewayURL: upstream.URL, TokenFile: filepath.Join(t.TempDir(), "absent")}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestIdentityProxyPassesMethodPathQueryAndStatus(t *testing.T) {
	var gotPath, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.RequestURI(), r.Method
		w.WriteHeader(429)
		io.WriteString(w, "slow down")
	}))
	defer upstream.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)
	p := &IdentityProxy{GatewayURL: upstream.URL, TokenFile: tokenFile}

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=true", strings.NewReader(`{}`)))
	if rec.Code != 429 || rec.Body.String() != "slow down" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/chat/completions?stream=true" {
		t.Fatalf("upstream saw %s %s", gotMethod, gotPath)
	}
}
