package factoryd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func TestDigestCollectsCommandsFilesErrorsAndDiff(t *testing.T) {
	t0 := time.Unix(1000, 0)
	d := newDigestBuilder(t0)
	d.add(cmdEvent("go test ./...", 1, "--- FAIL: TestA (0.01s)\n    a_test.go:9: want 1"), t0.Add(30*time.Second))
	d.add(fileEvent("/workspace/repo/a.go", "h1"), t0.Add(40*time.Second))
	d.add(fileEvent("/workspace/repo/a.go", "h2"), t0.Add(50*time.Second))
	d.add(fileEvent("/workspace/repo/b.go", "h3"), t0.Add(55*time.Second))
	d.add(toolEvent("read", map[string]any{"filePath": "a.go"}), t0.Add(56*time.Second))
	d.add(cmdEvent("go test ./...", 1, "--- FAIL: TestA (0.02s)\n    a_test.go:11: want 1"), t0.Add(90*time.Second))
	d.add(cmdEvent("go vet ./...", 0, ""), t0.Add(95*time.Second))
	d.add(harness.Event{Kind: harness.EventCheckpoint, Detail: map[string]any{
		"diff": map[string]any{"files": float64(2), "insertions": float64(7), "deletions": float64(1)}, "patch": "diff --git a/a.go b/a.go\n+x"}}, t0.Add(100*time.Second))
	d.add(harness.Event{Kind: harness.EventCheckpoint}, t0.Add(101*time.Second)) // adapter step, no diff

	g := d.digest()
	if len(g.Commands) != 3 {
		t.Fatalf("commands = %+v", g.Commands)
	}
	c0 := g.Commands[0]
	if c0.At != "30s" || c0.Command != "go test ./..." || c0.Exit == nil || *c0.Exit != 1 || c0.Signature == "" {
		t.Fatalf("first command = %+v", c0)
	}
	if g.Commands[1].Signature != c0.Signature {
		t.Fatal("the same failure got two signatures")
	}
	if g.Commands[2].Signature != "" || *g.Commands[2].Exit != 0 {
		t.Fatalf("a passing command = %+v, want no signature", g.Commands[2])
	}
	if g.Files["/workspace/repo/a.go"] != 2 || g.Files["/workspace/repo/b.go"] != 1 || g.ToolCalls != 1 {
		t.Fatalf("files = %v, tool calls = %d", g.Files, g.ToolCalls)
	}
	if len(g.Errors) != 1 || g.Errors[0].Count != 2 || !strings.Contains(g.Errors[0].Output, "want 1") {
		t.Fatalf("errors = %+v, want one signature seen twice with its output", g.Errors)
	}
	if g.Diff.Files != 2 || g.Diff.Insertions != 7 || g.Diff.Deletions != 1 || !strings.Contains(g.Diff.Patch, "+x") {
		t.Fatalf("diff = %+v", g.Diff)
	}
	want := "3 commands (2 failed), 2 files changed 3 times, 1 other tool calls, diff 2 files +7/-1; last error: `go test ./...` ×2"
	if s := g.Summary(); s != want {
		t.Fatalf("summary = %q\nwant      %q", s, want)
	}
}

func TestDigestKeepsTheLastCommandsAndErrors(t *testing.T) {
	t0 := time.Unix(0, 0)
	d := newDigestBuilder(t0)
	for i := 0; i < digestCommands+10; i++ {
		d.add(cmdEvent("cmd", 1, strings.Repeat("e", i+1)), t0)
	}
	g := d.digest()
	if len(g.Commands) != digestCommands || len(g.Errors) != digestErrors {
		t.Fatalf("commands = %d, errors = %d, want %d and %d", len(g.Commands), len(g.Errors), digestCommands, digestErrors)
	}
	if last := g.Errors[len(g.Errors)-1]; last.Output != strings.Repeat("e", digestCommands+10) {
		t.Fatalf("last error = %q, want the newest", last.Output)
	}
}

// Roadmap 201: files_edited keeps the digestFiles paths edited last, as
// the digest keeps the last commands: of 50 paths f0 to f49 the last 40,
// f10 to f49; f10 edited again is the newest, so g pushes out f11.
func TestDigestKeepsTheFilesEditedLast(t *testing.T) {
	t0 := time.Unix(0, 0)
	d := newDigestBuilder(t0)
	for i := range digestFiles + 10 {
		d.add(fileEvent(fmt.Sprint("f", i), "h"), t0)
	}
	want := map[string]int{}
	for i := 10; i < digestFiles+10; i++ {
		want[fmt.Sprint("f", i)] = 1
	}
	if g := d.digest(); !reflect.DeepEqual(g.Files, want) {
		t.Fatalf("files = %v, want f10 to f49 once each", g.Files)
	}
	d.add(fileEvent("f10", "h2"), t0)
	d.add(fileEvent("g", "h"), t0)
	delete(want, "f11")
	want["f10"], want["g"] = 2, 1
	if g := d.digest(); !reflect.DeepEqual(g.Files, want) {
		t.Fatalf("files = %v, want f11 gone, f10 twice and g", g.Files)
	}
}

// Roadmap 201: where a model reads a bash command (a finding, the
// digest, the supervisor's record) CommandLine cuts it like a tool's
// string argument: its first 512 bytes, "…", its size and the first 8
// bytes of its SHA-256. A short one stands whole. Two long failing
// commands alike in their first 512 bytes, with one output, read as two
// lines with two signatures.
func TestCommandLineIsCutWhereAModelReadsIt(t *testing.T) {
	a := strings.Repeat("a", 512) + strings.Repeat("b", 488)
	b := strings.Repeat("a", 512) + strings.Repeat("c", 488)
	line := func(cmd string) string {
		sum := sha256.Sum256([]byte(cmd))
		return strings.Repeat("a", 512) + "… (1000 bytes, sha256 " + hex.EncodeToString(sum[:8]) + ")"
	}
	if CommandLine("go test ./...") != "go test ./..." || CommandLine(a) != line(a) || line(a) == line(b) {
		t.Fatalf("CommandLine: short %q, long %q", CommandLine("go test ./..."), CommandLine(a))
	}
	t0 := time.Unix(0, 0)
	d := newDigestBuilder(t0)
	d.add(cmdEvent(a, 1, "FAIL"), t0)
	d.add(cmdEvent(b, 1, "FAIL"), t0)
	g := d.digest()
	if g.Commands[0].Command != line(a) || g.Commands[1].Command != line(b) || g.Commands[0].Signature == g.Commands[1].Signature || len(g.Errors) != 2 {
		t.Fatalf("digest = %+v, want two cut lines with two signatures", g)
	}
	sup, err := NewSupervisor(SupervisorConfig{NoProgressAfter: time.Minute, WallClockCap: time.Hour, RepetitionEvents: 24}, t0, t0)
	if err != nil {
		t.Fatal(err)
	}
	sup.Event(cmdEvent(a, 1, "FAIL"), t0)
	if f := sup.findings; len(f) != 1 || f[0] != "ran `"+line(a)+"` (exit 1)" {
		t.Fatalf("findings = %q", f)
	}
}

// fakeLiteLLM stands in for LiteLLM's key API (/key/list) and the
// gateway's model route, which factoryd calls with the gateway-factoryd
// token, the task and the purpose.
type fakeLiteLLM struct {
	chats    []map[string]any
	auth     []string
	tasks    []string
	purposes []string
	status   int
	decision string // X-Gateway-Decision of a refusal
	content  string
	contents []string // answers in order; past the last, content
	delay    time.Duration
	keys     string // /key/list body; empty = no keys
	// deployment is the gateway's deployment header on an answer
	deployment string
}

func (f *fakeLiteLLM) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		switch r.URL.Path {
		case "/key/list":
			if f.keys == "" {
				io.WriteString(w, `{"keys":[],"total_pages":1}`)
				return
			}
			io.WriteString(w, f.keys)
		case "/v1/chat/completions":
			f.chats = append(f.chats, body)
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			f.tasks = append(f.tasks, r.Header.Get(gateway.HeaderFactoryTask))
			f.purposes = append(f.purposes, r.Header.Get(gateway.HeaderFactoryPurpose))
			if f.status != 0 {
				if f.decision != "" {
					w.Header().Set(gateway.HeaderDecision, f.decision)
				}
				w.WriteHeader(f.status)
				io.WriteString(w, `{"error":{"message":"task headroom 0.000000 EUR","type":"budget_exceeded"}}`)
				return
			}
			if f.deployment != "" {
				w.Header().Set(gateway.HeaderDeployment, f.deployment)
			}
			content := f.content
			if n := len(f.chats) - 1; n < len(f.contents) {
				content = f.contents[n]
			}
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
			w.Write(out)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testSupervisorModel(url string) *SupervisorModel {
	return &SupervisorModel{
		GatewayURL: url,
		Token:      "gw-token",
		Budget:     TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000},
	}
}
