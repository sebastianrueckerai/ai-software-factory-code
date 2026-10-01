package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	h := messageHandler(mustHash(t, testMessageHash), newPushGate(), func(ctx context.Context, text string) error {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > harness.MessageTimeout {
			t.Errorf("hand-over deadline %v (set %v), want within %s", time.Until(dl), ok, harness.MessageTimeout)
		}
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

// The push token reaches the worker only on POST /push with the session's
// bearer, and only once the worker asks for it: after killOthers returned
// and the commit is made. The endpoint takes one token; before the ask,
// while the agent is killed and after the token was taken it answers 409.
func TestPushTokenOnlyAfterTheAgentIsGone(t *testing.T) {
	dir, e, base, mainHead, fork := changedSession(t)
	gate := newPushGate()
	h := messageHandler(mustHash(t, testMessageHash), gate, func(context.Context, string) error { return nil })
	post := func(auth, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/push", strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const bearer, tokW = "Bearer session-secret", `{"token":"tok-w"}`
	codes := []int{post(bearer, tokW)}
	var request, got string
	commit, err := commitAndPush(dir, e, base, mainHead, func() error {
		codes = append(codes, post(bearer, tokW))
		return nil
	}, func(commit string) (string, error) {
		tok, err := gate.ask(commit, time.Second, writerFunc(func(p []byte) (int, error) {
			// factoryd reads the request and answers it
			request = string(p)
			codes = append(codes, post("Bearer session-secreT", tokW), post(bearer, `{"token":""}`), post(bearer, tokW), post(bearer, `{"token":"tok-x"}`))
			return len(p), nil
		}))
		got = tok
		return tok, err
	})
	codes = append(codes, post(bearer, tokW))
	if err != nil {
		t.Fatal(err)
	}
	// before, during the kill, wrong bearer, no token, the one token, a second token, after
	if want := []int{409, 409, 401, 400, 204, 409, 409}; !slices.Equal(codes, want) {
		t.Fatalf("POST /push answered %v, want %v", codes, want)
	}
	if got != "tok-w" {
		t.Fatalf("the push got token %q, want tok-w", got)
	}
	if want := `{"type":"push","push":{"commit":"` + commit + `"}}` + "\n"; request != want {
		t.Fatalf("push request = %q, want %q", request, want)
	}
	if out, _ := exec.Command("git", "-C", fork, "rev-parse", "refs/heads/factory/t1").Output(); strings.TrimSpace(string(out)) != commit {
		t.Fatalf("fork branch = %q, want the pushed commit %s", out, commit)
	}
}

// A push request nobody answers is written again inside the window, so one
// transient failure on factoryd's side costs an interval, not the push.
func TestPushTokenReaskedInsideTheWindow(t *testing.T) {
	previous := pushRetryEvery
	pushRetryEvery = 15 * time.Millisecond
	t.Cleanup(func() { pushRetryEvery = previous })
	gate := newPushGate()
	var lines atomic.Int32
	asked := make(chan string, 1)
	go func() {
		tok, err := gate.ask("c1", 2*time.Second, writerFunc(func(p []byte) (int, error) {
			lines.Add(1)
			return len(p), nil
		}))
		if err != nil {
			t.Error(err)
		}
		asked <- tok
	}()
	// the first ask went unanswered; only a re-ask may be answered
	for until := time.Now().Add(time.Second); lines.Load() < 2 && time.Now().Before(until); {
		time.Sleep(2 * time.Millisecond)
	}
	if n := lines.Load(); n < 2 {
		t.Fatalf("push request written %d times, want a re-ask", n)
	}
	if !gate.take("tok-r") {
		t.Fatal("no ask waited for the re-asked token")
	}
	if tok := <-asked; tok != "tok-r" {
		t.Fatalf("the push got token %q, want tok-r", tok)
	}
}
