package factoryd

import (
	"bytes"
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
	"testing"
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
	usage := map[string]any{"prompt_tokens": 120, "completion_tokens": 45, "total_tokens": 165}
	finish, delta := "stop", map[string]any{"role": "assistant", "content": text}
	if tool != "" {
		args, _ := json.Marshal(map[string]string{"command": command, "description": "fixture step"})
		finish, delta = "tool_calls", map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": fmt.Sprintf("call_%d", len(f.loop)), "type": "function",
			"function": map[string]any{"name": tool, "arguments": string(args)}}}}
	}
	if !req.Stream {
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
	cfgJSON, err := buildOpencodeConfig(srv.URL, "factory", "factory-main", 10000, recordSteps)
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
	for i, req := range fake.loop {
		if slices.Contains(req.tools, "question") {
			t.Errorf("call %d offers the question tool: %v", i+1, req.tools)
		}
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
