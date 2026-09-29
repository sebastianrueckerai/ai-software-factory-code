package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// testMessageHash is sha256("session-secret"), hex.
const testMessageHash = "135fa7d67146ca540f17e51e101d45de5b1edde8ea8a13f9c7b93b71aa21f8a4"

func TestLoadEnvMessageTokenHash(t *testing.T) {
	setRequiredEnv(t)
	e, err := loadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := mustHash(t, testMessageHash); e.messageHash != want {
		t.Fatalf("messageHash = %x", e.messageHash)
	}
	if _, set := os.LookupEnv("MESSAGE_TOKEN_SHA256"); set {
		t.Fatal("MESSAGE_TOKEN_SHA256 left in the environment opencode inherits")
	}
	for _, v := range []string{"", "abc", strings.Repeat("z", 64)} {
		setRequiredEnv(t)
		t.Setenv("MESSAGE_TOKEN_SHA256", v)
		if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "MESSAGE_TOKEN_SHA256") {
			t.Errorf("MESSAGE_TOKEN_SHA256=%q: err = %v", v, err)
		}
	}
}

func mustHash(t *testing.T, hexed string) [32]byte {
	t.Helper()
	h, err := parseHash(hexed)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestMessageEndpoint(t *testing.T) {
	var sent []string
	var answer error
	h := messageHandler(mustHash(t, testMessageHash), func(ctx context.Context, text string) error {
		sent = append(sent, text)
		return answer
	})
	post := func(auth, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const body = `{"text":"change approach"}`
	for name, auth := range map[string]string{"no bearer": "", "wrong bearer": "Bearer session-secreT", "basic": "Basic session-secret", "the hash itself": "Bearer " + testMessageHash} {
		if code := post(auth, body); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
	if len(sent) != 0 {
		t.Fatalf("refused requests reached the harness: %q", sent)
	}
	if code := post("Bearer session-secret", `{"text":"  "}`); code != http.StatusBadRequest {
		t.Errorf("empty text: status %d, want 400", code)
	}
	if code := post("Bearer session-secret", body); code != http.StatusNoContent || len(sent) != 1 || sent[0] != "change approach" {
		t.Fatalf("status %d, sent %q", code, sent)
	}
	for err, want := range map[error]int{harness.ErrNotSupported: http.StatusNotImplemented, harness.ErrNotRunning: http.StatusConflict, errors.New("boom"): http.StatusBadGateway} {
		answer = err
		if code := post("Bearer session-secret", body); code != want {
			t.Errorf("%v: status %d, want %d", err, code, want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/message", nil)
	req.Header.Set("Authorization", "Bearer session-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
}
