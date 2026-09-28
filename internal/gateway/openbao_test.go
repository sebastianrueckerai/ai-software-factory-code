package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBao struct {
	mu        sync.Mutex
	calls     []string
	logins    []map[string]string
	signs     []map[string]string
	tokens    []string
	forbidOne bool
}

func newFakeBao(t *testing.T) (*fakeBao, *OpenBao) {
	t.Helper()
	f := &fakeBao{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/ssh-client-signer/public_key":
			io.WriteString(w, "ssh-rsa AAAACAKEY\n")
		case "POST /v1/auth/kubernetes/login":
			f.logins = append(f.logins, body)
			io.WriteString(w, `{"auth":{"client_token":"tok-`+string(rune('0'+len(f.logins)))+`","lease_duration":3600}}`)
		case "POST /v1/ssh-client-signer/sign/factory-agent":
			f.tokens = append(f.tokens, r.Header.Get("X-Vault-Token"))
			if f.forbidOne {
				f.forbidOne = false
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"errors":["permission denied"]}`)
				return
			}
			f.signs = append(f.signs, body)
			io.WriteString(w, `{"data":{"serial_number":"1","signed_key":"ssh-ed25519-cert-v01@openssh.com AAAACERT\n"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("sa-jwt\n"), 0o600)
	return f, &OpenBao{BaseURL: srv.URL, AuthRole: "factory-gateway", SSHRole: "factory-agent", TokenFile: tokenFile, HTTP: srv.Client()}
}

func TestOpenBaoSignsWithKubernetesLogin(t *testing.T) {
	f, bao := newFakeBao(t)
	ctx := context.Background()
	for range 2 {
		key, err := bao.CAPublicKey(ctx)
		if err != nil || key != "ssh-rsa AAAACAKEY" {
			t.Fatalf("ca key = %q, %v", key, err)
		}
	}
	cert, err := bao.Sign(ctx, "ssh-ed25519 AAAAPUB agent", "root", 30*time.Minute, "task=t1 resource=5")
	if err != nil || cert != "ssh-ed25519-cert-v01@openssh.com AAAACERT" {
		t.Fatalf("cert = %q, %v", cert, err)
	}
	if _, err := bao.Sign(ctx, "ssh-ed25519 AAAAPUB agent", "root", 10*time.Minute, "task=t1 resource=5"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logins) != 1 || f.logins[0]["role"] != "factory-gateway" || f.logins[0]["jwt"] != "sa-jwt" {
		t.Fatalf("logins = %v", f.logins)
	}
	want := map[string]string{"public_key": "ssh-ed25519 AAAAPUB agent", "valid_principals": "root", "cert_type": "user",
		"ttl": "1800s", "key_id": "task=t1 resource=5"}
	for k, v := range want {
		if f.signs[0][k] != v {
			t.Fatalf("sign body %s = %q, want %q", k, f.signs[0][k], v)
		}
	}
	if f.signs[1]["ttl"] != "600s" || strings.Join(f.tokens, ",") != "tok-1,tok-1" {
		t.Fatalf("second sign = %v, tokens %v", f.signs[1], f.tokens)
	}
	if n := strings.Count(strings.Join(f.calls, ","), "public_key"); n != 1 {
		t.Fatalf("ca key fetched %d times", n)
	}
}

func TestOpenBaoLogsInAgainAfterAForbiddenSign(t *testing.T) {
	f, bao := newFakeBao(t)
	ctx := context.Background()
	if _, err := bao.Sign(ctx, "ssh-ed25519 AAAAPUB", "root", time.Minute, "k"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.forbidOne = true
	f.mu.Unlock()
	if _, err := bao.Sign(ctx, "ssh-ed25519 AAAAPUB", "root", time.Minute, "k"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.tokens, ",") != "tok-1,tok-1,tok-2" {
		t.Fatalf("tokens = %v", f.tokens)
	}
}
