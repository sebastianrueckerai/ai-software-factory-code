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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func checkinHandler(t *testing.T, ck *checkinServe) http.Handler {
	t.Helper()
	return messageHandler(mustHash(t, testMessageHash), newPushGate(), func(context.Context, string, string) error { return nil }, ck, nil, nil)
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
	if err := os.MkdirAll(filepath.Join(ws, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "repo", "notes.md"), []byte("the note\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := checkinHandler(t, newCheckinServe(ws, nolog))
	// the path as grep prints it, relative to the repo
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
	h := checkinHandler(t, newCheckinServe(ws, nolog))
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("rm", `"path":"/workspace/repo"`)); code != http.StatusBadRequest || !strings.Contains(out, "unknown tool") {
		t.Errorf("rm: status %d, body %q, want 400 unknown tool", code, out)
	}
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("read_file", `"path":"/etc/passwd"`)); code != http.StatusForbidden || !strings.Contains(out, "outside the workspace") {
		t.Errorf("read_file /etc/passwd: status %d, body %q, want 403 outside the workspace", code, out)
	}
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("read_file", `"path":"../../etc/passwd"`)); code != http.StatusForbidden || !strings.Contains(out, "outside the workspace") {
		t.Errorf("read_file ../..: status %d, body %q, want 403 outside the workspace", code, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "repo", "big"), bytes.Repeat([]byte("y\n"), 51200), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := postTool(t, h, "Bearer session-secret", toolBody("run", `"command":"id"`)); code != http.StatusBadRequest || !strings.Contains(out, "unknown tool") {
		t.Errorf("run: status %d, body %q, want 400 unknown tool", code, out)
	}
	code, out := postTool(t, h, "Bearer session-secret", toolBody("read_file", `"path":"big"`))
	if code != http.StatusOK {
		t.Fatalf("100 KiB file: status %d, body %q, want 200", code, out)
	}
	got := decodeTool(t, out)
	if len(got.Output) != checkinOutputCap || !got.Cut {
		t.Fatalf("output %d bytes cut %v, want exactly the cap %d with cut true", len(got.Output), got.Cut, checkinOutputCap)
	}
	ck := &checkinServe{ws: ws, timeout: time.Nanosecond, handover: make(chan harness.BriefPush, 1)}
	if code, out := postTool(t, checkinHandler(t, ck), "Bearer session-secret", toolBody("git_log", `"args":"-n 1"`)); code != http.StatusBadRequest || !strings.Contains(out, "git took longer than 1ns: narrow it, with -n <count> and a path after -- for git_log, or a path for grep") {
		t.Fatalf("git_log past its timeout: status %d, body %q, want 400 naming the timeout", code, out)
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
	h := checkinHandler(t, newCheckinServe(seedCheckinRepo(t), nolog))
	code, out := postTool(t, h, "Bearer session-secret", toolBody("grep", `"pattern":"SessionInput"`))
	if code != http.StatusOK {
		t.Fatalf("grep: status %d, body %q", code, out)
	}
	if got := decodeTool(t, out); !strings.Contains(got.Output, "main.go:3:") || !strings.Contains(got.Output, "SessionInput") || got.Cut {
		t.Fatalf("grep output = %q cut %v, want main.go's line 3", got.Output, got.Cut)
	}
	// a regex finds nothing in a fixed-string grep, and the miss is said
	code, out = postTool(t, h, "Bearer session-secret", toolBody("grep", `"pattern":"Session.*"`))
	if got := decodeTool(t, out); code != http.StatusOK || got.Output != `no line matches the fixed string "Session.*" as a whole word at HEAD` {
		t.Fatalf("grep miss: status %d, output %q, want the miss said", code, got.Output)
	}
	// read_file takes the path grep printed
	code, out = postTool(t, h, "Bearer session-secret", toolBody("read_file", `"path":"main.go"`))
	if got := decodeTool(t, out); code != http.StatusOK || !strings.Contains(got.Output, "var SessionInput struct{}") {
		t.Fatalf("read_file main.go: status %d, output %q, want the file grep named", code, got.Output)
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
}

// The session's start holds for the check-in's handover: without a push
// it ends at its deadline with an empty handover and one log line, and a
// push inside the wait becomes the brief the conversation runs on.
func TestBriefWait(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	old := checkinWait
	t.Cleanup(func() { checkinWait = old })

	t.Run("no push", func(t *testing.T) {
		loopbackPort(t)
		readyToStart(t)
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
		addr := loopbackPort(t)
		readyToStart(t)
		checkinWait = 10 * time.Second
		const note = "the finding is false; build and test as usual"
		body, _ := json.Marshal(map[string]string{"handover": note})
		answered := pushWhileStarting(addr, body)
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

// readyToStart sets the environment start() reads for a session on a
// seeded repo, its gateway one that answers the card's brief and stores
// the session's text; it returns that gateway.
func readyToStart(t *testing.T) *sessionGateway {
	t.Helper()
	setRequiredEnv(t)
	gw := briefGateway(t, http.StatusOK, `{"text":"Do it\n\nWhy: It is due."}`)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	// the seeded repo is its own base; an ambient REPO_SHA from the
	// harness names a commit it does not carry
	t.Setenv("REPO_SHA", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	return gw
}

// sessionGateway is the texts a session stored at its gateway, and the
// status the gateway answers the next store.
type sessionGateway struct {
	mu     sync.Mutex
	texts  []string
	status int
}

func (g *sessionGateway) stored() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.texts)
}

// briefGateway is the session's gateway (GATEWAY_URL): GET /worker/brief
// answers status and body, and POST /worker/session-brief stores the
// session's text, to the bearer of the session's token file (TOKEN_FILE),
// 401 to any other.
func briefGateway(t *testing.T, status int, body string) *sessionGateway {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gw := &sessionGateway{status: http.StatusNoContent}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer sa-jwt":
			http.Error(w, "not the session's token", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/worker/brief":
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		case r.Method == http.MethodPost && r.URL.Path == "/worker/session-brief":
			text, _ := io.ReadAll(r.Body)
			gw.mu.Lock()
			defer gw.mu.Unlock()
			gw.texts = append(gw.texts, string(text))
			if gw.status != http.StatusNoContent {
				http.Error(w, "the session's brief of t1: factoryd answered 503", gw.status)
				return
			}
			w.WriteHeader(gw.status)
		default:
			http.Error(w, "no such route", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("TOKEN_FILE", token)
	return gw
}

// The session stores the text it gives its first model call at the
// gateway, on its own token, before its run starts: the card's text, the
// repository map and the sandbox facts stand in it. A text the gateway
// cannot store fails the start at the brief step, as a service that may
// answer later does, and so does a gateway that holds the store past
// briefWait: no session runs on a text nobody kept.
func TestStartStoresTheSessionsText(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	loopbackPort(t)
	old := checkinWait
	t.Cleanup(func() { checkinWait = old })
	checkinWait = 10 * time.Millisecond

	gw := readyToStart(t)
	t.Setenv("DIRECTORIES", `["docs"]`)
	t.Setenv("FACTS", "FACTS: session 1 of 3")
	s, err := start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
	if err != nil {
		t.Fatal(err)
	}
	s.closeTelemetry()
	s.stopMessages()
	if got := gw.stored(); len(got) != 1 || got[0] != s.text {
		t.Fatalf("stored %q, want the one text the run prompts, %q", got, s.text)
	}
	for _, part := range []string{"Do it\n\nWhy: It is due.", "docs/: not in the repository", "FACTS: session 1 of 3", factoryd.SandboxFacts(nil, nil)} {
		if !strings.Contains(s.text, part) {
			t.Errorf("the session's text lacks %q", part)
		}
	}

	gw = readyToStart(t)
	gw.status = http.StatusBadGateway
	s, err = start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
	s.closeTelemetry()
	s.stopMessages()
	if res := startFailed(err); res.Step != "brief" || !res.Transient || !strings.Contains(res.Summary, "status 502: the session's brief of t1: factoryd answered 503") {
		t.Fatalf("start = %+v, want a transient failure at the brief step", res)
	}

	oldWait := briefWait
	t.Cleanup(func() { briefWait = oldWait })
	briefWait = 50 * time.Millisecond
	readyToStart(t)
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"text":"Do it"}`)
			return
		}
		// the server sees the client leave once the body is read
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(hung.Close)
	t.Setenv("GATEWAY_URL", hung.URL)
	s, err = start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
	s.closeTelemetry()
	s.stopMessages()
	if res := startFailed(err); res.Step != "brief" || !res.Transient || !strings.Contains(res.Summary, "context deadline exceeded") {
		t.Fatalf("start = %+v, want a transient failure at the brief step past its bound", res)
	}
}

// The session reads its card's brief from the gateway on its own token:
// the brief's text stands between the session's own parts, and its title
// is the commit's subject. A brief the gateway cannot give fails the
// start at the brief step, as a service that may answer later does.
func TestStartReadsTheCardsBriefFromTheGateway(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	loopbackPort(t)
	old := checkinWait
	t.Cleanup(func() { checkinWait = old })
	checkinWait = 10 * time.Millisecond

	readyToStart(t)
	s, err := start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
	if err != nil {
		t.Fatal(err)
	}
	s.closeTelemetry()
	s.stopMessages()
	if s.brief.Objective != "Do it\n\nWhy: It is due." || s.env.title != "Do it" {
		t.Fatalf("brief %q, title %q, want the gateway's text and its first line", s.brief.Objective, s.env.title)
	}

	readyToStart(t)
	briefGateway(t, http.StatusBadGateway, "the card's brief of t1: factoryd answered 503")
	s, err = start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
	s.closeTelemetry()
	s.stopMessages()
	if res := startFailed(err); res.Step != "brief" || !res.Transient || !strings.Contains(res.Summary, "status 502: the card's brief of t1: factoryd answered 503") {
		t.Fatalf("start = %+v, want a transient failure at the brief step", res)
	}
}

// A gateway that holds the brief's read past briefWait fails the start at
// the brief step, as a service that may answer later does: the session
// does not hang on it.
func TestStartBoundsTheBriefsRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	loopbackPort(t)
	old := briefWait
	t.Cleanup(func() { briefWait = old })
	briefWait = 50 * time.Millisecond
	readyToStart(t)
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hung.Close)
	t.Setenv("GATEWAY_URL", hung.URL)
	s, err := start(context.Background(), nolog, seedCheckinRepo(t), nil, func(workerEnv) egressProbe { return closedSandbox(t) })
	s.closeTelemetry()
	s.stopMessages()
	if res := startFailed(err); res.Step != "brief" || !res.Transient || !strings.Contains(res.Summary, "context deadline exceeded") {
		t.Fatalf("start = %+v, want a transient failure at the brief step past its bound", res)
	}
}

// A card's brief past the bound is cut on a character, and the brief says
// what the bound left out among factoryd's notes.
func TestFitCardCutsTheBriefPastItsBound(t *testing.T) {
	if card, left := fitCard("short", []string{"a note"}); card != "short" || !slices.Equal(left, []string{"a note"}) {
		t.Fatalf("fitCard(short) = %q, %q", card, left)
	}
	long := "a" + strings.Repeat("ä", maxCard/2)
	card, left := fitCard(long, nil)
	if want := "a" + strings.Repeat("ä", maxCard/2-2) + "…"; card != want {
		t.Fatalf("fitCard cut %d bytes, want %d", len(card), len(want))
	}
	if want := []string{fmt.Sprintf("the card's brief past its first %d of %d bytes", maxCard-3, maxCard+1)}; !slices.Equal(left, want) {
		t.Fatalf("left out %q, want %q", left, want)
	}
}

// pushWhileStarting posts body to POST /brief on the message port at addr
// until the listener answers: start() binds it somewhere in its clone and
// checkpoint phases. The channel gets the answer's status.
func pushWhileStarting(addr string, body []byte) <-chan int {
	answered := make(chan int, 1)
	go func() {
		var code int
		for deadline := time.Now().Add(5 * time.Second); ; {
			req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/brief", bytes.NewReader(body))
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
	return answered
}

// The parent session's record lands in the workspace where the design
// says the coding agent greps it, and a session id that could leave the
// sessions directory is refused.
func TestRecordsEndpoint(t *testing.T) {
	ws := t.TempDir()
	h := checkinHandler(t, newCheckinServe(ws, nolog))
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
	return postRead(t, h, "/image", auth, path)
}

func postRead(t *testing.T, h http.Handler, route, auth, path string) (int, string, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
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
	h := checkinHandler(t, newCheckinServe(ws, nolog))
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

// POST /file answers a workspace text file's bytes for a digest, through
// the same workspace read as POST /image: a path outside refuses, a
// directory, a missing file, an empty one and one that is no UTF-8 text
// refuse by name, and the byte cap holds.
func TestFileEndpoint(t *testing.T) {
	ws := t.TempDir()
	repo := filepath.Join(ws, "repo")
	for _, dir := range []string{filepath.Join(repo, "docs"), filepath.Join(ws, "docs")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := "# Plan\n\nThe cards: één, two.\n"
	full := strings.Repeat("x", policy.MaxDigestBytes)
	write(filepath.Join(repo, "docs", "plan.md"), plan)
	write(filepath.Join(ws, "docs", "spec.md"), plan)
	write(filepath.Join(repo, "cap.md"), full)
	write(filepath.Join(repo, "huge.md"), full+"x")
	write(filepath.Join(repo, "empty.md"), "")
	write(filepath.Join(repo, "shot.png"), "\x89PNG\r\n\x1a\npixels")
	write(filepath.Join(repo, "nul.txt"), "a\x00b")
	write(filepath.Join(repo, "latin1.txt"), "caf\xe9")
	secret := filepath.Join(t.TempDir(), "secret.md")
	write(secret, plan)
	if err := os.Symlink(secret, filepath.Join(repo, "link.md")); err != nil {
		t.Fatal(err)
	}
	h := checkinHandler(t, newCheckinServe(ws, nolog))
	const auth = "Bearer session-secret"
	// the gateway admits a path of 4096 bytes: in short names, in one name
	// over the file system's 255, and in bytes JSON escapes to six each
	longPath := strings.Repeat("a/", 2047) + "aa"
	longName := strings.Repeat("n", 300)
	quoted := strings.Repeat("\x01", 4096)

	for name, tc := range map[string]struct {
		path   string
		status int
		body   string
	}{
		"relative to the repo":  {"docs/plan.md", 200, plan},
		"absolute in workspace": {filepath.Join(ws, "docs", "spec.md"), 200, plan},
		"at the byte cap":       {"cap.md", 200, full},
		"absolute outside":      {"/etc/hostname", 403, "/etc/hostname is outside the workspace\n"},
		"symlink out":           {"link.md", 403, "link.md goes through a symlink that is absolute or leaves the workspace: name the file by its own path\n"},
		"missing":               {"docs/none.md", 404, "no file docs/none.md in the workspace\n"},
		"directory":             {"docs", 400, "docs is a directory: digest one file in it\n"},
		"over the byte cap":     {"huge.md", 413, "huge.md is larger than 128 KiB, more than one digest reads: read the part you need with the read tool's offset and limit\n"},
		"empty":                 {"empty.md", 400, "empty.md is empty: there is nothing to digest\n"},
		"an image":              {"shot.png", 415, "shot.png is not UTF-8 text, which a digest reads\n"},
		"a NUL byte":            {"nul.txt", 415, "nul.txt is not UTF-8 text, which a digest reads\n"},
		"not UTF-8":             {"latin1.txt", 415, "latin1.txt is not UTF-8 text, which a digest reads\n"},
		"under a file":          {"docs/plan.md/x", 404, "no file docs/plan.md/x in the workspace\n"},
		"a name too long":       {longName, 400, longName + " is not a path a file can have: name a file of the workspace\n"},
		"a NUL in the path":     {"docs/plan\x00.md", 400, "docs/plan\x00.md is not a path a file can have: name a file of the workspace\n"},
		"the longest path":      {longPath, 404, "no file " + longPath + " in the workspace\n"},
		"escaped to the limit":  {quoted, 400, quoted + " is not a path a file can have: name a file of the workspace\n"},
	} {
		code, _, body := postRead(t, h, "/file", auth, tc.path)
		if code != tc.status || body != tc.body {
			t.Errorf("%s: status %d, body %.120q; want %d, %.120q", name, code, body, tc.status, tc.body)
		}
	}
	if code, _, body := postRead(t, h, "/file", "", "docs/plan.md"); code != http.StatusUnauthorized || body != "unauthorized\n" {
		t.Errorf("no bearer: status %d, body %q, want 401 unauthorized", code, body)
	}
}

// The brief's files land under .factory/prior outside the repository
// before the conversation's brief is released, and a name that could
// leave that directory writes nothing.
func TestBriefEndpointLandsTheReportsOutsideTheRepo(t *testing.T) {
	post := func(h http.Handler, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/brief", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer session-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	t.Run("lands", func(t *testing.T) {
		ws := t.TempDir()
		ck := newCheckinServe(ws, nolog)
		code := post(checkinHandler(t, ck), `{"handover":"note","prior":{"session":"s-1","heading":"h","body":"b"},"files":[{"Name":"report-1.md","Text":"full"}]}`)
		if code != http.StatusNoContent {
			t.Fatalf("status %d, want 204", code)
		}
		dir := filepath.Join(ws, ".factory", "prior", "s-1")
		if b, err := os.ReadFile(filepath.Join(dir, "report-1.md")); err != nil || string(b) != "full" {
			t.Fatalf("report = %q, %v, want full", b, err)
		}
		push := ck.waitBrief(t.Logf)
		if push.Handover != "note" || push.Prior == nil || push.Prior.Dir != dir || !slices.Equal(push.Prior.Reports, []string{"report-1.md"}) {
			t.Fatalf("push = %+v, prior %+v", push, push.Prior)
		}
	})
	for name, body := range map[string]string{
		"session with a slash": `{"prior":{"session":"a/b"},"files":[{"Name":"r.md","Text":"x"}]}`,
		"session dotdot":       `{"prior":{"session":".."},"files":[{"Name":"r.md","Text":"x"}]}`,
		"session empty":        `{"prior":{"session":""},"files":[{"Name":"r.md","Text":"x"}]}`,
		"name dotdot":          `{"prior":{"session":"s-1"},"files":[{"Name":"../x","Text":"x"}]}`,
		"name with a slash":    `{"prior":{"session":"s-1"},"files":[{"Name":"a/b","Text":"x"}]}`,
		"name dot":             `{"prior":{"session":"s-1"},"files":[{"Name":"r.md","Text":"x"},{"Name":".","Text":"x"}]}`,
		"session dot":          `{"prior":{"session":"."},"files":[{"Name":"r.md","Text":"x"}]}`,
		"name empty":           `{"prior":{"session":"s-1"},"files":[{"Name":"","Text":"x"}]}`,
		"files without prior":  `{"files":[{"Name":"r.md","Text":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			if code := post(checkinHandler(t, newCheckinServe(ws, nolog)), body); code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", code)
			}
			if _, err := os.Stat(filepath.Join(ws, ".factory")); !os.IsNotExist(err) {
				t.Fatalf("a refused push wrote under .factory: %v", err)
			}
		})
	}
}

// Each refusal of an unfit name says every rule the name broke against:
// not empty, not ".", no "/" and no "..".
func TestUnfitNameRefusalsNameEveryRule(t *testing.T) {
	const rule = `must be a bare name: not empty, not "." and without "/" or ".."`
	for _, tc := range []struct{ path, body, want string }{
		{"/brief", `{"prior":{"session":"s-1"},"files":[{"Name":".","Text":"x"}]}`, `file name "." ` + rule + "\n"},
		{"/brief", `{"prior":{"session":"."},"files":[{"Name":"r.md","Text":"x"}]}`, `prior session "." ` + rule + "\n"},
		{"/records", `{"session":".","record":"x"}`, `session "." ` + rule + "\n"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer session-secret")
		rec := httptest.NewRecorder()
		checkinHandler(t, newCheckinServe(t.TempDir(), nolog)).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != tc.want {
			t.Errorf("%s: status %d, body %q; want 400, %q", tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

// The brief endpoint takes a push up to harness.MaxBriefPush, the largest
// factoryd builds among them, and refuses a larger one.
func TestBriefEndpointTakesPushesUpToTheCap(t *testing.T) {
	for _, tc := range []struct {
		handover int
		status   int
	}{
		{(harness.MaxBriefPush - 100) / 6, http.StatusNoContent},
		{harness.MaxBriefPush / 6, http.StatusBadRequest},
	} {
		body, err := json.Marshal(harness.BriefPush{Handover: strings.Repeat("<", tc.handover)})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/brief", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer session-secret")
		rec := httptest.NewRecorder()
		checkinHandler(t, newCheckinServe(t.TempDir(), nolog)).ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("a body of %d bytes: status %d, want %d", len(body), rec.Code, tc.status)
		}
	}
}

// The brief's opened proofs land in the clone before the conversation's
// brief is released, each where its path holds no file, and the brief
// names every one the clone holds, written now or there already. A path
// out of the tree refuses the push, which writes none of its files, and
// a path the clone cannot hold is left out with a log line.
func TestBriefPushWritesTheOpenedProofsIntoTheClone(t *testing.T) {
	const push = `{"proofs":[{"path":"proofs/size_bound/size_bound_test.go","content":"package size_bound"},` +
		`{"path":"acceptance/group_open_test.go","content":"v1"}]}`
	held := []string{"proofs/size_bound/size_bound_test.go", "acceptance/group_open_test.go"}
	keep := func(t *testing.T, repo string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, "acceptance"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "acceptance", "group_open_test.go"), []byte("v2"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files := func(t *testing.T, repo string) {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(repo, "proofs", "size_bound", "size_bound_test.go")); err != nil || string(b) != "package size_bound" {
			t.Fatalf("size_bound_test.go = %q, %v, want the proof's code", b, err)
		}
		if b, err := os.ReadFile(filepath.Join(repo, "acceptance", "group_open_test.go")); err != nil || string(b) != "v2" {
			t.Fatalf("group_open_test.go = %q, %v, want the clone's own v2", b, err)
		}
	}
	post := func(h http.Handler, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/brief", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer session-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	t.Run("lands", func(t *testing.T) {
		ws := t.TempDir()
		repo := filepath.Join(ws, "repo")
		keep(t, repo)
		ck := newCheckinServe(ws, nolog)
		if rec := post(checkinHandler(t, ck), push); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d %q, want 204", rec.Code, rec.Body.String())
		}
		files(t, repo)
		if got := ck.waitBrief(t.Logf); !slices.Equal(got.InClone, held) {
			t.Fatalf("in clone = %q, want %q", got.InClone, held)
		}
	})
	t.Run("the run's brief", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("no git binary")
		}
		addr := loopbackPort(t)
		readyToStart(t)
		old := checkinWait
		t.Cleanup(func() { checkinWait = old })
		checkinWait = 10 * time.Second
		ws := seedCheckinRepo(t)
		repo := filepath.Join(ws, "repo")
		keep(t, repo)
		answered := pushWhileStarting(addr, []byte(push))
		s, err := start(context.Background(), nolog, ws, nil, func(workerEnv) egressProbe { return closedSandbox(t) })
		if err != nil {
			t.Fatal(err)
		}
		defer s.closeTelemetry()
		s.stopMessages()
		if code := <-answered; code != http.StatusNoContent {
			t.Fatalf("POST /brief answered %d, want 204", code)
		}
		files(t, repo)
		if !slices.Equal(s.brief.Proofs, held) {
			t.Fatalf("brief proofs = %q, want %q", s.brief.Proofs, held)
		}
	})
	t.Run("a path the clone cannot hold", func(t *testing.T) {
		ws := t.TempDir()
		repo := filepath.Join(ws, "repo")
		keep(t, repo)
		// a file where the proof's directory would be: its Lstat answers ENOTDIR
		if err := os.WriteFile(filepath.Join(repo, "proofs"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		var logs []string
		ck := newCheckinServe(ws, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
		if rec := post(checkinHandler(t, ck), push); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d %q, want 204", rec.Code, rec.Body.String())
		}
		if got := ck.waitBrief(t.Logf); !slices.Equal(got.InClone, []string{"acceptance/group_open_test.go"}) {
			t.Fatalf("in clone = %q, want the held group_open alone", got.InClone)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], "proofs/size_bound/size_bound_test.go") || !strings.Contains(logs[0], "not a directory") {
			t.Fatalf("logs = %q, want one line naming the proof and why", logs)
		}
	})
	t.Run("a push after the wait", func(t *testing.T) {
		ws := t.TempDir()
		repo := filepath.Join(ws, "repo")
		if err := os.MkdirAll(repo, 0o750); err != nil {
			t.Fatal(err)
		}
		ck := newCheckinServe(ws, nolog)
		ck.wait = time.Millisecond
		if got := ck.waitBrief(t.Logf); got.Handover != "" || got.InClone != nil {
			t.Fatalf("push = %+v, want the zero push at the deadline", got)
		}
		if rec := post(checkinHandler(t, ck), push); rec.Code != http.StatusConflict {
			t.Fatalf("status %d %q, want 409", rec.Code, rec.Body.String())
		}
		if _, err := os.Lstat(filepath.Join(repo, "proofs")); !os.IsNotExist(err) {
			t.Fatalf("a push after the wait wrote into the clone: %v", err)
		}
	})
	for _, out := range []string{"../x_test.go", "proofs/../../x_test.go", "/x_test.go"} {
		t.Run("a path out of the tree: "+out, func(t *testing.T) {
			ws := t.TempDir()
			repo := filepath.Join(ws, "repo")
			if err := os.MkdirAll(repo, 0o750); err != nil {
				t.Fatal(err)
			}
			rec := post(checkinHandler(t, newCheckinServe(ws, nolog)), `{"prior":{"session":"s-1"},"files":[{"Name":"r.md","Text":"x"}],`+
				`"proofs":[{"path":"proofs/size_bound/size_bound_test.go","content":"package size_bound"},{"path":"`+out+`","content":"x"}]}`)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), out) {
				t.Fatalf("status %d %q, want 400 naming the path", rec.Code, rec.Body.String())
			}
			for _, p := range []string{filepath.Join(repo, "proofs"), filepath.Join(ws, "x_test.go"), filepath.Join(ws, ".factory")} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Fatalf("a refused push wrote %s: %v", p, err)
				}
			}
		})
	}
}
