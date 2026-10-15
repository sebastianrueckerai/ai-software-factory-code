package factoryd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// The recorded serve session maps onto the events `opencode run` gave:
// three model steps, two commands (the second after a permission ask the
// driver answers), the final text, then idle.
func TestOpencodeMapsRecordedServeSession(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "opencode-serve.sse"))
	if err != nil {
		t.Fatal(err)
	}
	const session = "ses_f111daf97ffeEpTXXz2p2WARoR"
	m := &serveMapper{session: session, sessions: map[string]bool{session: true}}
	p := &opencodeParser{workDir: t.TempDir()}
	var events []harness.Event
	var asks []string
	for _, line := range strings.Split(string(b), "\n") {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok || m.idle {
			continue
		}
		out := m.event([]byte(data), time.UnixMilli(1))
		if out.line != nil {
			p.line(out.line, func(ev harness.Event) { events = append(events, ev) })
		}
		if out.permission != "" {
			asks = append(asks, out.permission)
		}
	}
	want := []harness.EventKind{
		harness.EventCheckpoint, harness.EventCommand, harness.EventModelCall,
		harness.EventCheckpoint, harness.EventCommand, harness.EventModelCall,
		harness.EventCheckpoint, harness.EventModelCall,
	}
	if got := kinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i, cmd := range map[int]string{1: "echo one > one.txt", 4: "echo two > two.txt"} {
		if events[i].Detail["command"] != cmd || events[i].Detail["exit"] != 0 {
			t.Errorf("event %d = %v, want %q exit 0", i, events[i].Detail, cmd)
		}
	}
	for i, reason := range map[int]string{2: "tool-calls", 5: "tool-calls", 7: "stop"} {
		if events[i].Detail["reason"] != reason {
			t.Errorf("model call %d reason = %v, want %s", i, events[i].Detail["reason"], reason)
		}
	}
	if !reflect.DeepEqual(asks, []string{"per_0eee25394001C7STG5t65eIZKB"}) {
		t.Errorf("permission asks = %v", asks)
	}
	if !m.idle || !p.stopped || p.errMessage() != "" || p.summary() != "Wrote one.txt and two.txt after the nudge." {
		t.Errorf("idle %v, stopped %v, error %q, summary %q", m.idle, p.stopped, p.errMessage(), p.summary())
	}
}

func TestServeMapperFollowsSubagentAsksAndErrors(t *testing.T) {
	m := &serveMapper{session: "ses_a", sessions: map[string]bool{"ses_a": true}}
	ev := func(s string) served { return m.event([]byte(s), time.UnixMilli(5)) }
	if out := ev(`{"type":"permission.asked","properties":{"id":"per_x","sessionID":"ses_b"}}`); out.permission != "" {
		t.Fatalf("an unrelated session's ask answered: %+v", out)
	}
	ev(`{"type":"session.created","properties":{"info":{"id":"ses_b","parentID":"ses_a"}}}`)
	if out := ev(`{"type":"permission.asked","properties":{"id":"per_y","sessionID":"ses_b"}}`); out.permission != "per_y" {
		t.Fatalf("a subagent's ask = %+v, want per_y", out)
	}
	if out := ev(`{"type":"message.part.updated","properties":{"part":{"sessionID":"ses_b","type":"step-start"}}}`); out.line != nil {
		t.Fatalf("a subagent's part became a line: %s", out.line)
	}
	if out := ev(`{"type":"message.part.updated","properties":{"part":{"sessionID":"ses_a","type":"text","text":"half"}}}`); out.line != nil {
		t.Fatalf("an unfinished text part became a line: %s", out.line)
	}
	out := ev(`{"type":"session.error","properties":{"sessionID":"ses_a","error":{"name":"APIError","data":{"message":"boom","statusCode":500}}}}`)
	p := &opencodeParser{}
	p.line(out.line, func(harness.Event) {})
	if p.errMessage() != "APIError: boom (status 500)" {
		t.Fatalf("error line %s parsed to %q", out.line, p.errMessage())
	}
	if ev(`{"type":"session.status","properties":{"sessionID":"ses_b","status":{"type":"idle"}}}`); m.idle {
		t.Fatal("a subagent's idle ended the session")
	}
	if ev(`{"type":"session.status","properties":{"sessionID":"ses_a","status":{"type":"idle"}}}`); !m.idle {
		t.Fatal("the session's idle not seen")
	}
}

// The serve stream's line past the reader's 1 MiB cap: a text part whose
// text alone is 2 MiB, its type and part id in its head. The fake server
// streams its filler in chunks and the test names its size as a constant,
// so neither process buffers the whole 2 MiB line at once.
const (
	oversizeHead = "data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"id\":\"prt_2mib\",\"sessionID\":\"ses_f111daf97ffeEpTXXz2p2WARoR\",\"type\":\"text\",\"text\":\""
	oversizeTail = "\"}}"
	oversizeFill = 2 << 20
	oversizeLen  = len(oversizeHead) + oversizeFill + len(oversizeTail)
)

func writeOversizeLine(w io.Writer) {
	fmt.Fprint(w, oversizeHead)
	chunk := strings.Repeat("x", 64<<10)
	for sent := 0; sent < oversizeFill; sent += len(chunk) {
		fmt.Fprint(w, chunk)
	}
	fmt.Fprint(w, oversizeTail)
}

// fakeServeEnv makes the test binary a fake `opencode serve`
// (TestFakeOpencodeServe): it replays the named fixture's events after the
// first prompt and records every request it takes in the working
// directory's requests.jsonl.
const fakeServeEnv = "FAKE_OPENCODE_SERVE"

// TestFakeOpencodeServe is the fake server's body, a no-op in a plain run.
func TestFakeOpencodeServe(t *testing.T) {
	fixture := os.Getenv(fakeServeEnv)
	if fixture == "" {
		t.Skip("the fake server runs only when re-executed")
	}
	os.Exit(fakeServe(fixture))
}

func fakeServe(fixture string) int {
	mode := os.Getenv("FAKE_SERVE_MODE")
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "opencode exploded")
		return 3
	}
	events, err := os.ReadFile(fixture)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	record, _ := os.OpenFile("requests.jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	pwd, _ := os.Getwd()
	fmt.Fprintf(record, "{\"cwd\":%q,\"pwd\":%q,\"config\":%q}\n", pwd, os.Getenv("PWD"), os.Getenv("OPENCODE_CONFIG_CONTENT"))
	prompted := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-prompted:
		case <-r.Context().Done():
			return
		}
		switch mode {
		case "oversize":
			// a normal event, one line past the reader's cap, then the
			// session's own events: the reader must survive the middle one
			fmt.Fprint(w, "data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"id\":\"prt_step\",\"sessionID\":\"ses_f111daf97ffeEpTXXz2p2WARoR\",\"type\":\"step-start\"}}}\n\n")
			writeOversizeLine(w)
			fmt.Fprint(w, "\n\n")
		case "hold":
			// a leftover the agent started, one holding the output pipe
			exec.Command("sh", "-c", "sleep 300 & echo $! > holder.pid; sleep 300 >/dev/null 2>&1 </dev/null & echo $! > detached.pid").Run()
		case "wait":
			// the session runs until the test's message arrives
			w.(http.Flusher).Flush()
			<-release
		}
		w.Write(events)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		line, _ := json.Marshal(map[string]string{"path": r.URL.Path, "body": string(body)})
		record.Write(append(line, '\n'))
		switch {
		case r.URL.Path == "/session":
			fmt.Fprint(w, `{"id":"ses_f111daf97ffeEpTXXz2p2WARoR"}`)
		case strings.HasSuffix(r.URL.Path, "/prompt_async"):
			w.WriteHeader(http.StatusNoContent)
			select {
			case <-prompted:
				close(release)
			default:
				close(prompted)
			}
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "true")
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 2
	}
	fmt.Println("Warning: OPENCODE_SERVER_PASSWORD is not set; server is unsecured.")
	fmt.Println(serveListening + "http://" + ln.Addr().String())
	http.Serve(ln, mux)
	return 0
}

// fakeServeOpencode is an adapter whose binary is the fake server; mode
// picks its behaviour ("" replays the fixture).
func fakeServeOpencode(t *testing.T, mode string) (*Opencode, string) {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "opencode-serve.sse"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeServeEnv, fixture)
	t.Setenv("FAKE_SERVE_MODE", mode)
	dir := t.TempDir()
	stub := filepath.Join(t.TempDir(), "opencode-stub")
	self, _ := os.Executable()
	os.WriteFile(stub, []byte("#!/bin/sh\nexec "+self+" -test.run '^TestFakeOpencodeServe$'\n"), 0o755)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)
	return &Opencode{
		Binary: stub, WorkDir: dir, GatewayURL: "http://127.0.0.1:1", TokenFile: tokenFile,
		Model: "factory/factory-main", Budget: TokenBudget{AnswerTokens: 8000, HeadroomTokens: 10000 - 8000}, Steps: 200,
	}, dir
}

type servedRequest struct {
	Path, Body       string
	Cwd, Pwd, Config string
}

func servedRequests(t *testing.T, dir string) []servedRequest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []servedRequest
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var r servedRequest
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// A subagent's finished tool call becomes the session's tool_use line, so
// its writes keep the supervisor's write clock; its other parts stay out.
func TestServeMapperCountsASubagentsToolCalls(t *testing.T) {
	m := &serveMapper{session: "ses_a", sessions: map[string]bool{"ses_a": true}}
	ev := func(s string) served { return m.event([]byte(s), time.UnixMilli(5)) }
	ev(`{"type":"session.created","properties":{"info":{"id":"ses_b","parentID":"ses_a"}}}`)
	out := ev(`{"type":"message.part.updated","properties":{"part":{"sessionID":"ses_b","type":"tool","tool":"edit","state":{"status":"completed"}}}}`)
	if out.line == nil || !strings.Contains(string(out.line), `"type":"tool_use"`) || !strings.Contains(string(out.line), `"sessionID":"ses_a"`) {
		t.Fatalf("a subagent's tool call = %s, want the session's tool_use line", out.line)
	}
	if out := ev(`{"type":"message.part.updated","properties":{"part":{"sessionID":"ses_b","type":"text","time":{"end":5}}}}`); out.line != nil {
		t.Fatalf("a subagent's text became a line: %s", out.line)
	}
	if out := ev(`{"type":"message.part.updated","properties":{"part":{"sessionID":"ses_c","type":"tool","state":{"status":"completed"}}}}`); out.line != nil {
		t.Fatalf("an unrelated session's tool call became a line: %s", out.line)
	}
}
