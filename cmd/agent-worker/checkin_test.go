//go:build unix

package main

// The check-in's three endpoints (the escalation ladder's plan D): the
// tools factoryd's check-in conversation runs in this workspace, the
// handover that starts the coding conversation, and the parent session's
// record delivered into the workspace.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func checkinHandler(t *testing.T, ck *checkinServe) http.Handler {
	t.Helper()
	return messageHandler(mustHash(t, testMessageHash), newPushGate(), func(context.Context, string, string) error { return nil }, ck)
}

func postTool(t *testing.T, h http.Handler, auth, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/tool", strings.NewReader(body))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func toolBody(name string, args string) string {
	return fmt.Sprintf(`{"name":%q,"args":{%s}}`, name, args)
}

type toolAnswer struct {
	Output string `json:"output"`
	Cut    bool   `json:"cut"`
}

func decodeTool(t *testing.T, body string) toolAnswer {
	t.Helper()
	var out toolAnswer
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("tool answer %q: %v", body, err)
	}
	return out
}

// The check-in's tools answer only the session's bearer, exactly as
// /message does.
func TestToolEndpointAuth(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("the note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := checkinHandler(t, newCheckinServe(ws))
	body := toolBody("read_file", `"path":"notes.md"`)
	for name, auth := range map[string]string{"no bearer": "", "wrong bearer": "Bearer session-secreT", "the hash itself": "Bearer " + testMessageHash} {
		if code, out := postTool(t, h, auth, body); code != http.StatusUnauthorized || out != "unauthorized\n" {
			t.Errorf("%s: status %d, body %q, want 401 unauthorized", name, code, out)
		}
	}
	code, out := postTool(t, h, "Bearer session-secret", body)
	if code != http.StatusOK {
		t.Fatalf("status %d, body %q, want 200", code, out)
	}
	if got := decodeTool(t, out); got.Output != "the note\n" || got.Cut {
		t.Fatalf("output = %+q cut %v", got.Output, got.Cut)
	}
}

// The allow-list, the workspace boundary and the two caps: only the four
// named tools run, a file outside the workspace is refused, output is cut
// at the cap with the cut said, and a run past its timeout names it.
func TestToolEndpointBounds(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "repo"), 0o750); err != nil {
		t.Fatal(err)
	}
	h := checkinHandler(t, newCheckinServe(ws))
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("rm", `"path":"/workspace/repo"`)); code != http.StatusBadRequest || !strings.Contains(out, "unknown tool") {
		t.Errorf("rm: status %d, body %q, want 400 unknown tool", code, out)
	}
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("read_file", `"path":"/etc/passwd"`)); code != http.StatusForbidden || !strings.Contains(out, "outside the workspace") {
		t.Errorf("read_file /etc/passwd: status %d, body %q, want 403 outside the workspace", code, out)
	}
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("read_file", `"path":"../../etc/passwd"`)); code != http.StatusForbidden || !strings.Contains(out, "outside the workspace") {
		t.Errorf("read_file ../..: status %d, body %q, want 403 outside the workspace", code, out)
	}
	code, out := postTool(t, h, "Bearer session-secret", toolBody("run", `"command":"yes | head -c 102400"`))
	if code != http.StatusOK {
		t.Fatalf("100 KiB run: status %d, body %q, want 200", code, out)
	}
	got := decodeTool(t, out)
	if len(got.Output) != checkinOutputCap || !got.Cut {
		t.Fatalf("output %d bytes cut %v, want exactly the cap %d with cut true", len(got.Output), got.Cut, checkinOutputCap)
	}
	ck := &checkinServe{ws: ws, timeout: 50 * time.Millisecond, handover: make(chan string, 1)}
	if code, out := postTool(t, checkinHandler(t, ck), "Bearer session-secret", toolBody("run", `"command":"sleep 600"`)); code != http.StatusBadRequest || !strings.Contains(out, "50ms") {
		t.Fatalf("sleep 600: status %d, body %q, want 400 naming the timeout", code, out)
	}
}

func seedCheckinRepo(t *testing.T) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	ws := t.TempDir()
	seed := t.TempDir()
	repo := filepath.Join(ws, "repo")
	run("init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "main.go"), []byte("package main\n\nvar SessionInput struct{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("-C", seed, "add", "main.go")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "first subject")
	if err := os.WriteFile(filepath.Join(seed, "notes.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("-C", seed, "add", "notes.md")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "second subject")
	run("clone", "-q", seed, repo)
	return ws
}

// The four tools read this workspace: grep finds the line at the head,
// git_log takes its few allowed arguments, and run answers a command's
// combined output.
func TestToolExecutes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	h := checkinHandler(t, newCheckinServe(seedCheckinRepo(t)))
	code, out := postTool(t, h, "Bearer session-secret", toolBody("grep", `"pattern":"SessionInput"`))
	if code != http.StatusOK {
		t.Fatalf("grep: status %d, body %q", code, out)
	}
	if got := decodeTool(t, out); !strings.Contains(got.Output, "main.go:3:") || !strings.Contains(got.Output, "SessionInput") || got.Cut {
		t.Fatalf("grep output = %q cut %v, want main.go's line 3", got.Output, got.Cut)
	}
	code, out = postTool(t, h, "Bearer session-secret", toolBody("git_log", `"args":"-n 1"`))
	if code != http.StatusOK {
		t.Fatalf("git_log: status %d, body %q", code, out)
	}
	if got := decodeTool(t, out); !strings.Contains(got.Output, "second subject") || strings.Contains(got.Output, "first subject") {
		t.Fatalf("git_log -n 1 = %q, want only the last commit", got.Output)
	}
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("git_log", `"args":"--since=yesterday --output=/tmp/x"`)); code != http.StatusBadRequest || !strings.Contains(out, "--since=yesterday") {
		t.Fatalf("git_log --since: status %d, body %q, want 400 naming the refused argument", code, out)
	}
	code, out = postTool(t, h, "Bearer session-secret", toolBody("run", `"command":"go version"`))
	if code != http.StatusOK {
		t.Fatalf("run go version: status %d, body %q", code, out)
	}
	if got := decodeTool(t, out); !strings.Contains(got.Output, "go version") {
		t.Fatalf("run go version = %q", got.Output)
	}
}

// The session's start holds for the check-in's handover: without a push
// it ends at its deadline with an empty handover and one log line, and a
// push inside the wait becomes the brief the conversation runs on.
func TestBriefWait(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	// start() binds the worker's one fixed message port; inside a live
	// worker pod that port is the pod's own, held for the session
	if ln, err := net.Listen("tcp", ":"+strconv.Itoa(harness.MessagePort)); err != nil {
		t.Skip("the worker's message port is held: " + err.Error())
	} else {
		ln.Close()
	}
	ready := func(t *testing.T) {
		t.Helper()
		setRequiredEnv(t)
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		// the seeded repo is its own base; an ambient REPO_SHA from the
		// harness names a commit it does not carry
		t.Setenv("REPO_SHA", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := checkinWait
	t.Cleanup(func() { checkinWait = old })

	t.Run("no push", func(t *testing.T) {
		ready(t)
		checkinWait = 50 * time.Millisecond
		var logs []string
		s, err := start(context.Background(), func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
			seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
		if err != nil {
			t.Fatal(err)
		}
		defer s.closeTelemetry()
		s.stopMessages()
		if s.brief.Handover != "" {
			t.Fatalf("brief handover = %q, want empty after the deadline", s.brief.Handover)
		}
		var expiry []string
		for _, l := range logs {
			if strings.Contains(l, "handover") {
				expiry = append(expiry, l)
			}
		}
		if len(expiry) != 1 || !strings.Contains(expiry[0], "50ms") {
			t.Fatalf("expiry logs = %q, want the one wait's expiry", expiry)
		}
	})

	t.Run("push inside the wait", func(t *testing.T) {
		ready(t)
		checkinWait = 10 * time.Second
		const note = "the finding is false; build and test as usual"
		answered := make(chan int, 1)
		go func() {
			// start() binds port 8091 somewhere in its clone and checkpoint
			// phases; the push retries until the listener answers
			body, _ := json.Marshal(map[string]string{"handover": note})
			var code int
			for deadline := time.Now().Add(5 * time.Second); ; {
				req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8091/brief", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer session-secret")
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					resp.Body.Close()
					code = resp.StatusCode
					break
				}
				if time.Now().After(deadline) {
					code = http.StatusInternalServerError
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			answered <- code
		}()
		s, err := start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
		if err != nil {
			t.Fatal(err)
		}
		defer s.closeTelemetry()
		s.stopMessages()
		if code := <-answered; code != http.StatusNoContent {
			t.Fatalf("POST /brief answered %d, want 204", code)
		}
		if s.brief.Handover != note {
			t.Fatalf("brief handover = %q, want %q", s.brief.Handover, note)
		}
	})
}

// The parent session's record lands in the workspace where the design
// says the coding agent greps it, and a session id that could leave the
// sessions directory is refused.
func TestRecordsEndpoint(t *testing.T) {
	ws := t.TempDir()
	h := checkinHandler(t, newCheckinServe(ws))
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/records", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer session-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const record = "{\"time\":\"2026-10-16T10:00:00Z\",\"kind\":\"model_call\"}\n"
	body, err := json.Marshal(struct {
		Session string `json:"session"`
		Record  string `json:"record"`
	}{"s-7", record})
	if err != nil {
		t.Fatal(err)
	}
	if code := post(string(body)); code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", code)
	}
	b, err := os.ReadFile(filepath.Join(ws, ".factory", "sessions", "s-7.jsonl"))
	if err != nil || string(b) != record {
		t.Fatalf("record file = (%q, %v), want the pushed bytes", b, err)
	}
	for _, session := range []string{"a/b", "..", "a/../b"} {
		if code := post(fmt.Sprintf(`{"session":%q,"record":"x"}`, session)); code != http.StatusBadRequest {
			t.Errorf("session %q: status %d, want 400", session, code)
		}
	}
	if code := post(`{"session":"","record":"x"}`); code != http.StatusBadRequest {
		t.Errorf("empty session: status %d, want 400", code)
	}
	if code := post(""); code != http.StatusBadRequest {
		t.Errorf("empty body: status %d, want 400", code)
	}
	if _, err := os.Stat(filepath.Join(ws, ".factory", "sessions", "a.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a refused session wrote a file: %v", err)
	}
}

func postImage(t *testing.T, h http.Handler, auth, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/image", strings.NewReader(fmt.Sprintf(`{"path":%q}`, path)))
	req.Header.Set("Authorization", auth)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
}

// POST /image answers a workspace image's bytes, refuses a path outside
// the workspace (a symlink out included), and names what is wrong with a
// file that is not a small image.
func TestImageEndpoint(t *testing.T) {
	ws := t.TempDir()
	repo := filepath.Join(ws, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "shots"), 0o750); err != nil {
		t.Fatal(err)
	}
	png := "\x89PNG\r\n\x1a\n" + "pixels"
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, "shots", "page.png"), png)
	write(filepath.Join(ws, "scratch.png"), png)
	write(filepath.Join(repo, "notes.txt"), "just text\n")
	write(filepath.Join(repo, "huge.png"), "\x89PNG\r\n\x1a\n"+strings.Repeat("x", policy.MaxLookBytes+1-8))
	secret := filepath.Join(t.TempDir(), "secret.png")
	write(secret, png)
	if err := os.Symlink(secret, filepath.Join(repo, "link.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("shots", "page.png"), filepath.Join(repo, "inside.png")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(repo, "fifo.png"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := checkinHandler(t, newCheckinServe(ws))
	const auth = "Bearer session-secret"

	for name, tc := range map[string]struct {
		path   string
		status int
		ctype  string
		body   string
	}{
		"relative to the repo":  {"shots/page.png", 200, "image/png", png},
		"absolute in workspace": {filepath.Join(ws, "scratch.png"), 200, "image/png", png},
		"absolute outside":      {"/etc/hostname", 403, "", "/etc/hostname is outside the workspace\n"},
		"climbing out":          {"../../etc/hostname", 403, "", "../../etc/hostname is outside the workspace\n"},
		"symlink out":           {"link.png", 403, "", "link.png goes through a symlink that is absolute or leaves the workspace: name the file by its own path\n"},
		"symlink inside":        {"inside.png", 200, "image/png", png},
		"a FIFO, never opened for a read that blocks": {"fifo.png", 400, "", "fifo.png is not a regular file\n"},
		"missing":             {"shots/none.png", 404, "", "no file shots/none.png in the workspace\n"},
		"directory":           {"shots", 400, "", "shots is a directory, not an image file\n"},
		"text":                {"notes.txt", 415, "", "notes.txt is text/plain; charset=utf-8, not a PNG, JPEG, WebP or GIF image\n"},
		"over the size limit": {"huge.png", 413, "", "huge.png is larger than 8 MiB\n"},
	} {
		code, ctype, body := postImage(t, h, auth, tc.path)
		if code != tc.status || body != tc.body || (tc.ctype != "" && ctype != tc.ctype) {
			t.Errorf("%s: status %d, type %q, body %.80q; want %d, %q, %.80q", name, code, ctype, body, tc.status, tc.ctype, tc.body)
		}
	}
	if code, _, body := postImage(t, h, "", "shots/page.png"); code != http.StatusUnauthorized || body != "unauthorized\n" {
		t.Errorf("no bearer: status %d, body %q, want 401 unauthorized", code, body)
	}
}
