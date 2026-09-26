package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func fixtureEvents(t *testing.T, name string) []harness.Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var events []harness.Event
	p := &opencodeParser{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.line([]byte(line), func(ev harness.Event) { events = append(events, ev) })
	}
	return events
}

func kinds(events []harness.Event) []harness.EventKind {
	var out []harness.EventKind
	for _, ev := range events {
		out = append(out, ev.Kind)
	}
	return out
}

func TestOpencodeMapsRecordedEditRun(t *testing.T) {
	events := fixtureEvents(t, "opencode-run-edit.jsonl")
	want := []harness.EventKind{
		harness.EventCheckpoint,
		harness.EventCommand,
		harness.EventModelCall,
		harness.EventCheckpoint,
		harness.EventFileChange,
		harness.EventModelCall,
		harness.EventCheckpoint,
		harness.EventModelCall,
	}
	if len(events) != len(want) {
		t.Fatalf("events = %v", kinds(events))
	}
	for i, k := range want {
		if events[i].Kind != k {
			t.Fatalf("event %d = %s, want %s (all: %v)", i, events[i].Kind, k, kinds(events))
		}
	}
	cmd := events[1].Detail
	if cmd["command"] != "echo recorded-fixture > hello.txt" || cmd["exit"] != 0 {
		t.Fatalf("command detail = %v", cmd)
	}
	fc := events[4].Detail
	if fc["path"] != "hello.txt" {
		t.Fatalf("file change detail = %v", fc)
	}
	last := events[7].Detail
	if last["reason"] != "stop" || last["cost"] != 0.00021 {
		t.Fatalf("final model call = %v", last)
	}
	tokens, ok := last["tokens"].(map[string]any)
	if !ok || tokens["total"] != float64(165) || tokens["reasoning"] != float64(0) {
		t.Fatalf("tokens = %v", last["tokens"])
	}
	// The assistant's closing text folds into the step_finish that ends it.
	if last["text"] != "I created hello.txt and notes.md." {
		t.Fatalf("folded text = %v", last["text"])
	}
	if events[7].Time.UnixMilli() != 1790437358403 {
		t.Fatalf("time = %v, want the fixture timestamp of the final step_finish", events[7].Time)
	}
}

func TestOpencodeMapsToolErrorAsToolCall(t *testing.T) {
	events := fixtureEvents(t, "opencode-run-tools.jsonl")
	var toolErr harness.Event
	for _, ev := range events {
		if ev.Kind == harness.EventFileChange {
			t.Fatalf("a failed write is not a file change: %+v", ev)
		}
		if ev.Kind == harness.EventToolCall {
			toolErr = ev
		}
	}
	if toolErr.Detail["tool"] != "write" || toolErr.Detail["status"] != "error" {
		t.Fatalf("tool error event = %+v", toolErr)
	}
	if !strings.Contains(fmt.Sprint(toolErr.Detail["error"]), "filePath") {
		t.Fatalf("tool error detail lost the message: %v", toolErr.Detail["error"])
	}
}

func TestOpencodeParserFlagsAPIError(t *testing.T) {
	p := &opencodeParser{}
	p.line([]byte(`{"type":"error","timestamp":1790437441579,"sessionID":"ses_x","error":{"name":"APIError","data":{"message":"stub boom","statusCode":500}}}`), func(ev harness.Event) {})
	if p.errMessage() != "APIError: stub boom (status 500)" {
		t.Fatalf("errMessage = %q", p.errMessage())
	}
	if p.stopped {
		t.Fatal("an errored run never reached a stop")
	}
}

func TestOpencodeParserRejectsGarbage(t *testing.T) {
	p := &opencodeParser{}
	p.line([]byte(`not json`), func(ev harness.Event) {})
	if p.errMessage() == "" {
		t.Fatal("garbage line must fail loudly, not silently vanish")
	}
}

func TestTokenBudgetNeedsHeadroom(t *testing.T) {
	if _, err := (TokenBudget{AnswerTokens: 1000, HeadroomTokens: 200}).OutputLimit(); err != nil {
		t.Fatalf("valid budget: %v", err)
	}
	if _, err := (TokenBudget{}).OutputLimit(); err == nil {
		t.Fatal("zero answer accepted")
	}
	if _, err := (TokenBudget{AnswerTokens: 1000, HeadroomTokens: -1}).OutputLimit(); err == nil {
		t.Fatal("negative headroom accepted")
	}
	if _, err := (TokenBudget{AnswerTokens: 1000, HeadroomTokens: 0}).OutputLimit(); err == nil {
		t.Fatal("zero headroom accepted: GLM is reasoning-always-on, output would truncate")
	}
}

func TestOpencodeConfigTargetsLocalProxy(t *testing.T) {
	cfgJSON, err := buildOpencodeConfig("http://127.0.0.1:45678/v1", "factory", "factory-main", 10000)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatalf("config is not JSON: %v (%s)", err, cfgJSON)
	}
	provider := cfg["provider"].(map[string]any)["factory"].(map[string]any)
	if provider["npm"] != "@ai-sdk/openai-compatible" {
		t.Fatalf("npm = %v", provider["npm"])
	}
	options := provider["options"].(map[string]any)
	if options["baseURL"] != "http://127.0.0.1:45678/v1" {
		t.Fatalf("baseURL = %v (must be the local identity proxy)", options["baseURL"])
	}
	model := provider["models"].(map[string]any)["factory-main"].(map[string]any)
	if model["reasoning"] != true {
		t.Fatalf("reasoning = %v", model["reasoning"])
	}
	if model["interleaved"].(map[string]any)["field"] != "reasoning_content" {
		t.Fatalf("interleaved = %v", model["interleaved"])
	}
	limit := model["limit"].(map[string]any)
	if limit["output"] != float64(10000) {
		t.Fatalf("limit.output = %v (answer + reasoning headroom)", limit["output"])
	}
}

func writeStubOpencode(t *testing.T, fixture string) string {
	t.Helper()
	dir := t.TempDir()
	fixturePath, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "opencode-stub")
	script := `#!/bin/sh
case "$OPENCODE_CONFIG_CONTENT" in
	*'"output":10000'*) ;;
	*) echo "config missing budget" >&2; exit 9;;
esac
printf '%s\n' "$*" > stub-args.txt
cat ` + fixturePath + `
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

func TestOpencodePrepareWritesBriefAndRunDrivesStub(t *testing.T) {
	stub := writeStubOpencode(t, "opencode-run-edit.jsonl")
	dir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)

	oc := &Opencode{
		Binary:     stub,
		WorkDir:    dir,
		GatewayURL: "http://127.0.0.1:1",
		TokenFile:  tokenFile,
		Model:      "factory/factory-main",
		Budget:     TokenBudget{AnswerTokens: 8000, HeadroomTokens: 2000},
	}
	brief := harness.TaskBrief{
		Objective:  "Add the store",
		Repository: "sf/ai-software-factory-code",
		SHA:        "abc123",
		Findings:   []string{"repo cloned at abc123"},
	}
	ctx := context.Background()
	if err := oc.Prepare(ctx, harness.SandboxEnv{Namespace: "tasks", ServiceAccount: "task-9", Image: "factory-sandbox"}, brief); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	briefBytes, err := os.ReadFile(filepath.Join(dir, "brief.json"))
	if err != nil {
		t.Fatalf("brief.json: %v", err)
	}
	var gotBrief harness.TaskBrief
	if err := json.Unmarshal(briefBytes, &gotBrief); err != nil || !reflect.DeepEqual(gotBrief, brief) {
		t.Fatalf("brief.json = %s (%v)", briefBytes, err)
	}

	var events []harness.Event
	res := oc.Run(ctx, brief, func(ev harness.Event) { events = append(events, ev) })
	if !res.Success {
		t.Fatalf("Run result = %+v", res)
	}
	last := events[len(events)-1]
	if last.Kind != harness.EventFinished || last.Detail["success"] != true {
		t.Fatalf("last event = %+v, want synthesized finished", last)
	}
	// The stub's argv proves the objective carried the findings forward.
	args, _ := os.ReadFile(filepath.Join(dir, "stub-args.txt"))
	if !strings.Contains(string(args), "Add the store") || !strings.Contains(string(args), "repo cloned at abc123") {
		t.Fatalf("objective passed to opencode = %q", args)
	}
}

func TestOpencodeRunFailsOnStubError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "opencode-stub")
	os.WriteFile(stub, []byte("#!/bin/sh\necho 'opencode exploded' >&2\nexit 3\n"), 0o755)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)

	oc := &Opencode{
		Binary: stub, WorkDir: dir, GatewayURL: "http://127.0.0.1:1", TokenFile: tokenFile,
		Model: "factory/factory-main", Budget: TokenBudget{AnswerTokens: 8000, HeadroomTokens: 2000},
	}
	var events []harness.Event
	res := oc.Run(context.Background(), harness.TaskBrief{Objective: "x"}, func(ev harness.Event) { events = append(events, ev) })
	if res.Success {
		t.Fatalf("result = %+v, want failure", res)
	}
	if !strings.Contains(res.Summary, "opencode exploded") {
		t.Fatalf("summary = %q, want stderr surfaced", res.Summary)
	}
	last := events[len(events)-1]
	if last.Kind != harness.EventFinished || last.Detail["success"] != false {
		t.Fatalf("last event = %+v", last)
	}
}

func TestOpencodeRunParsesUnterminatedFinalLine(t *testing.T) {
	// Command substitution strips the fixture's trailing newline, so the
	// stub's last step_finish reaches the adapter without a delimiter.
	stub := writeStubOpencode(t, "opencode-run-edit.jsonl")
	fixturePath, _ := filepath.Abs(filepath.Join("testdata", "opencode-run-edit.jsonl"))
	dir := t.TempDir()
	os.WriteFile(stub, []byte(`#!/bin/sh
printf '%s' "$(cat `+fixturePath+`)"
`), 0o755)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)

	oc := &Opencode{
		Binary: stub, WorkDir: dir, GatewayURL: "http://127.0.0.1:1", TokenFile: tokenFile,
		Model: "factory/factory-main", Budget: TokenBudget{AnswerTokens: 8000, HeadroomTokens: 2000},
	}
	var events []harness.Event
	res := oc.Run(context.Background(), harness.TaskBrief{Objective: "x"}, func(ev harness.Event) { events = append(events, ev) })
	if !res.Success {
		t.Fatalf("result = %+v: the unterminated final step_finish was not parsed", res)
	}
	var lastStep harness.Event
	for _, ev := range events {
		if ev.Kind == harness.EventModelCall {
			lastStep = ev
		}
	}
	if lastStep.Detail == nil || lastStep.Detail["reason"] != "stop" {
		t.Fatalf("last model call = %+v, want the fixture's final stop step", lastStep)
	}
}

func TestOpencodePrepareRejectsHeadroomlessBudget(t *testing.T) {
	oc := &Opencode{WorkDir: t.TempDir(), Budget: TokenBudget{AnswerTokens: 1000}}
	if err := oc.Prepare(context.Background(), harness.SandboxEnv{}, harness.TaskBrief{Objective: "x"}); err == nil {
		t.Fatal("budget without headroom accepted at task start")
	}
}

// running reports a live, non-zombie process: an orphan's reaper may be a
// container init that never reaps.
func running(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// the state field follows the parenthesized command name
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// Whatever the agent leaves running must die with the session: one
// leftover still holds opencode's stdout, one has let go of it.
func TestOpencodeRunKillsLeftoverProcesses(t *testing.T) {
	fixturePath, _ := filepath.Abs(filepath.Join("testdata", "opencode-run-edit.jsonl"))
	dir := t.TempDir()
	stub := filepath.Join(dir, "opencode-stub")
	os.WriteFile(stub, []byte(`#!/bin/sh
cat `+fixturePath+`
sleep 300 &
echo $! > `+dir+`/holder.pid
sleep 300 >/dev/null 2>&1 </dev/null &
echo $! > `+dir+`/detached.pid
`), 0o755)
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("jwt"), 0o600)

	oc := &Opencode{
		Binary: stub, WorkDir: dir, GatewayURL: "http://127.0.0.1:1", TokenFile: tokenFile,
		Model: "factory/factory-main", Budget: TokenBudget{AnswerTokens: 8000, HeadroomTokens: 2000},
	}
	start := time.Now()
	res := oc.Run(context.Background(), harness.TaskBrief{Objective: "x"}, func(harness.Event) {})
	if !res.Success {
		t.Fatalf("result = %+v: a leftover process must not fail the run", res)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("Run took %s: it waited on the stdout holder", d)
	}
	for _, name := range []string{"holder.pid", "detached.pid"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		deadline := time.Now().Add(5 * time.Second)
		for running(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("%s (pid %d) still alive after Run", name, pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
