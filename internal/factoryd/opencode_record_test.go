package factoryd

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
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// recordSteps is the step cap of the recorded run: the third model turn is
// the last with tools.
const recordSteps = 3

// maxStepsMarker opens the message opencode v1.18.32 appends at the step cap
// (packages/core/src/session/runner/max-steps.ts:1).
const maxStepsMarker = "CRITICAL - MAXIMUM STEPS REACHED"

// deniedMarker is v1.18.32's PermissionDeniedError text the model sees
// (packages/core/src/v1/permission.ts:26).
const deniedMarker = "The user has specified a rule which prevents you from using this specific tool call"

// fakeModelTurn is the scripted model's answer to the loop's request with n
// tool results so far: a denied call, a call, a call past the cap, text.
func fakeModelTurn(n int) (tool, command, text string) {
	switch n {
	case 0:
		return "bash", "git push origin HEAD", ""
	case 1:
		return "bash", "echo two > two.txt", ""
	case 2:
		return "bash", "echo past-the-cap", ""
	}
	return "", "", "Maximum steps reached. Wrote two.txt; the push was denied. Nothing remains."
}

type recordedRequest struct {
	tools        []string
	toolResults  int
	maxSteps     bool
	lastToolText string
}

// fakeOpenAI is an OpenAI-compatible chat completions server that plays
// fakeModelTurn to opencode's agent loop (the requests that offer tools) and
// answers every other request (the title) with plain text.
type fakeOpenAI struct {
	mu   sync.Mutex
	loop []recordedRequest
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text := "Recorded fixture"
	var tool, command string
	if len(req.Tools) > 0 {
		rec := recordedRequest{}
		for _, t := range req.Tools {
			rec.tools = append(rec.tools, t.Function.Name)
		}
		for _, m := range req.Messages {
			if strings.Contains(string(m.Content), maxStepsMarker) {
				rec.maxSteps = true
			}
			if m.Role == "tool" {
				rec.toolResults++
				rec.lastToolText = string(m.Content)
			}
		}
		f.mu.Lock()
		f.loop = append(f.loop, rec)
		f.mu.Unlock()
		tool, command, text = fakeModelTurn(rec.toolResults)
	}
	f.mu.Lock()
	n := len(f.loop)
	f.mu.Unlock()
	writeCompletion(w, req.Stream, n, tool, command, text)
}

// writeCompletion answers a chat completion with a bash tool call (tool
// set) or text, streamed when stream; n numbers the call id.
func writeCompletion(w http.ResponseWriter, stream bool, n int, tool, command, text string) {
	usage := map[string]any{"prompt_tokens": 120, "completion_tokens": 45, "total_tokens": 165}
	finish, delta := "stop", map[string]any{"role": "assistant", "content": text}
	if tool != "" {
		args, _ := json.Marshal(map[string]string{"command": command, "description": "fixture step"})
		finish, delta = "tool_calls", map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprintf("call_%d", n), "type": "function",
			"function": map[string]any{"name": tool, "arguments": string(args)}}}}
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		msg := map[string]any{"role": "assistant", "content": text}
		json.NewEncoder(w).Encode(map[string]any{"id": "cmpl", "object": "chat.completion", "created": 1, "model": "factory-main",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, chunk := range []map[string]any{
		{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage},
	} {
		chunk["id"], chunk["object"], chunk["created"], chunk["model"] = "cmpl", "chat.completion.chunk", 1, "factory-main"
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestRecordOpencodeFixture records testdata/opencode-run-stepcap-deny.jsonl
// from the pinned opencode binary (OPENCODE_RECORD_BIN) against fakeOpenAI,
// with the worker's config at a step cap of 3 and one recording-only deny
// rule, and checks what the model was shown. Re-run on every version bump.
func TestRecordOpencodeFixture(t *testing.T) {
	bin := os.Getenv("OPENCODE_RECORD_BIN")
	if bin == "" {
		t.Skip("OPENCODE_RECORD_BIN not set")
	}
	fake := &fakeOpenAI{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cfgJSON, err := buildOpencodeConfig(srv.URL, "factory", "factory-main", 10000, recordSteps, "")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(cfgJSON, &cfg)
	cfg["permission"].(map[string]any)["bash"] = map[string]any{"*": "allow", "git push*": "deny"}
	cfgJSON, _ = json.Marshal(cfg)

	home, work := t.TempDir(), t.TempDir()
	cmd := exec.Command(bin, "run", "--format", "json", "--auto", "--model", "factory/factory-main", "Write two.txt and push it.")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "PWD="+work, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local/share"), "XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local/state"), "OPENCODE_CONFIG_CONTENT="+string(cfgJSON))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("opencode: %v\nstderr: %s", err, stderr.String())
	}
	if err := os.WriteFile(filepath.Join("testdata", "opencode-run-stepcap-deny.jsonl"), stdout.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.loop) != 4 {
		t.Fatalf("agent loop made %d model calls, want 4 (denied, call, call past the cap, text)", len(fake.loop))
	}
	// No question check here: run denies question for its own session
	// (run.ts:430-448), so the config's rule shows only in the golden file.
	for i, req := range fake.loop {
		if want := i+1 >= recordSteps; req.maxSteps != want {
			t.Errorf("call %d: step-cap message present = %v, want %v", i+1, req.maxSteps, want)
		}
	}
	if !strings.Contains(fake.loop[1].lastToolText, deniedMarker) {
		t.Errorf("the call after the denial shows the model %q, want the denial", fake.loop[1].lastToolText)
	}
	if _, err := os.Stat(filepath.Join(work, "two.txt")); err != nil {
		t.Errorf("the loop did not continue past the denial: %v", err)
	}
	t.Logf("tools offered: %v", fake.loop[0].tools)
}

// serveNudge is the message the serve recording sends into the running
// session while the model answers its first call.
const serveNudge = "Recorded nudge: change approach, then continue."

// fakeServeModel plays three loop calls to `opencode serve`: a command,
// a command the recording's config makes opencode ask for, and text. On
// the first it sends serveNudge through the adapter's Message.
type fakeServeModel struct {
	oc       *Opencode
	mu       sync.Mutex
	loop     [][]string // per loop call: each message as role: content
	nudgeErr error
}

func (f *fakeServeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Tools) == 0 {
		writeCompletion(w, req.Stream, 0, "", "", "Recorded serve fixture")
		return
	}
	var msgs []string
	for _, m := range req.Messages {
		msgs = append(msgs, m.Role+": "+string(m.Content))
	}
	f.mu.Lock()
	f.loop = append(f.loop, msgs)
	n := len(f.loop)
	f.mu.Unlock()
	switch n {
	case 1:
		err := f.oc.Message(r.Context(), serveNudge)
		f.mu.Lock()
		f.nudgeErr = err
		f.mu.Unlock()
		writeCompletion(w, req.Stream, n, "bash", "echo one > one.txt", "")
	case 2:
		writeCompletion(w, req.Stream, n, "bash", "echo two > two.txt", "")
	default:
		writeCompletion(w, req.Stream, n, "", "", "Wrote one.txt and two.txt after the nudge.")
	}
}

// TestRecordOpencodeServeFixture records testdata/opencode-serve.sse, the
// raw event stream of one session under the pinned `opencode serve`
// (OPENCODE_RECORD_BIN), driven by Run against fakeServeModel: a nudge
// sent mid-turn reaches the next model call after the tool result, and a
// permission ask (a recording-only bash rule) is answered without a human.
func TestRecordOpencodeServeFixture(t *testing.T) {
	bin := os.Getenv("OPENCODE_RECORD_BIN")
	if bin == "" {
		t.Skip("OPENCODE_RECORD_BIN not set")
	}
	home, work := t.TempDir(), t.TempDir()
	for k, v := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local/share"),
		"XDG_CACHE_HOME": filepath.Join(home, ".cache"), "XDG_STATE_HOME": filepath.Join(home, ".local/state")} {
		t.Setenv(k, v)
	}
	var tap bytes.Buffer
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)
	oc := &Opencode{Binary: bin, WorkDir: work, TokenFile: tokenFile, Model: "factory/factory-main",
		Budget: TokenBudget{AnswerTokens: 8000, HeadroomTokens: 2000}, Steps: 20, eventTap: &tap}
	fake := &fakeServeModel{oc: oc}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	oc.GatewayURL = srv.URL
	// the project config merges under the worker's, which sets no bash rule
	if err := os.WriteFile(filepath.Join(work, "opencode.json"), []byte(`{"permission":{"bash":{"*":"allow","echo two*":"ask"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var events []harness.Event
	res := oc.Run(context.Background(), harness.TaskBrief{Objective: "Write one.txt and two.txt."}, func(ev harness.Event) { events = append(events, ev) })
	if !res.Success {
		t.Fatalf("Run = %+v", res)
	}
	if err := os.WriteFile(filepath.Join("testdata", "opencode-serve.sse"), tap.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.nudgeErr != nil {
		t.Fatalf("Message during the first call: %v", fake.nudgeErr)
	}
	if len(fake.loop) != 3 {
		t.Fatalf("agent loop made %d model calls, want 3", len(fake.loop))
	}
	second := fake.loop[1]
	last := second[len(second)-1]
	if !strings.HasPrefix(last, "user: ") || !strings.Contains(last, serveNudge) {
		t.Fatalf("the second call ends with %q, want the nudge as a user message; all: %q", last, second)
	}
	if !strings.HasPrefix(second[len(second)-2], "tool: ") {
		t.Errorf("the nudge follows %q, want the first call's tool result", second[len(second)-2])
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !bytes.Contains(tap.Bytes(), []byte(`"type":"permission.asked"`)) {
		t.Error("the recording holds no permission ask")
	}
	if oc.Message(context.Background(), "late") != harness.ErrNotRunning {
		t.Error("Message after the session accepted")
	}
	t.Logf("%d events, summary %q", len(events), res.Summary)
}
