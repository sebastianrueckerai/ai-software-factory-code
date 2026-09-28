package factoryd

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

func TestParseDecision(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     Decision
	}{
		{"continue", `{"action":"continue","reason":"tests are converging"}`, Decision{Action: ActionContinue, Reason: "tests are converging"}},
		{"redirect", ` {"action":"redirect","guidance":"stop editing the script; read the test"} `, Decision{Action: ActionRedirect, Guidance: "stop editing the script; read the test"}},
		{"retry", `{"action":"retry_stronger"}`, Decision{Action: ActionRetryStronger}},
		{"escalate", `{"action":"escalate","reason":"the check can never pass"}`, Decision{Action: ActionEscalate, Reason: "the check can never pass"}},
		{"fenced", "```json\n{\"action\":\"continue\"}\n```", Decision{Action: ActionContinue}},
	} {
		if got := parseDecision(tc.in); got != tc.want {
			t.Fatalf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	for name, in := range map[string]string{
		"not json":           `I think we should continue`,
		"unknown action":     `{"action":"raise_budget"}`,
		"unknown field":      `{"action":"continue","budget_eur":10}`,
		"two objects":        `{"action":"continue"}{"action":"escalate"}`,
		"trailing text":      `{"action":"continue"} because`,
		"redirect no advice": `{"action":"redirect"}`,
		"escalate no reason": `{"action":"escalate"}`,
		"empty":              ``,
		"array":              `[{"action":"continue"}]`,
	} {
		got := parseDecision(in)
		if got.Action != ActionEscalate || !strings.Contains(got.Reason, "invalid") {
			t.Fatalf("%s: %+v, want escalate with an invalid-answer reason", name, got)
		}
	}
	long := parseDecision(`{"action":"redirect","guidance":"` + strings.Repeat("g", 5000) + `"}`)
	if long.Action != ActionRedirect || len(long.Guidance) > maxDecisionText+len("…") {
		t.Fatalf("long guidance = %d bytes, want capped at %d", len(long.Guidance), maxDecisionText)
	}
}

type fakeLiteLLM struct {
	mints   []map[string]any
	chats   []map[string]any
	auth    []string
	status  int
	content string
	delay   time.Duration
	keys    string // /key/list body; empty = no keys
	cost    string // x-litellm-response-cost of a completion; "" = none
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
		case "/key/generate":
			if r.Header.Get("Authorization") != "Bearer master" {
				t.Errorf("mint auth = %q", r.Header.Get("Authorization"))
			}
			f.mints = append(f.mints, body)
			io.WriteString(w, `{"key":"sk-super-1","token":"hash1"}`)
		case "/v1/chat/completions":
			f.chats = append(f.chats, body)
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
			f.auth = append(f.auth, r.Header.Get("Authorization"))
			if f.cost != "" {
				w.Header().Set("x-litellm-response-cost", f.cost)
			}
			if f.status != 0 {
				w.WriteHeader(f.status)
				io.WriteString(w, `{"error":"boom"}`)
				return
			}
			out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": f.content}}}})
			w.Write(out)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var supervisorPrice = policy.Price{In: 1e-6, Out: 4e-6}

type onePrice policy.Price

func (p onePrice) Price(context.Context, string) (policy.Price, error) { return policy.Price(p), nil }

// testSupervisorModel meters against 20 EUR of free task and day headroom.
func testSupervisorModel(url string) *SupervisorModel {
	return &SupervisorModel{
		BaseURL: url,
		Admin:   &LiteLLMAdmin{BaseURL: url, MasterKey: "master"},
		Alias:   "supervisor",
		Budget:  TokenBudget{AnswerTokens: 1000, HeadroomTokens: 8000},
		Prices:  onePrice(supervisorPrice),
		Meter:   &memPolicy{taskFree: 20, dayFree: 20},
		DayEUR:  func() float64 { return 20 },
	}
}

func testInput() SupervisorInput {
	return SupervisorInput{
		Brief:   harness.TaskBrief{Objective: "make loopcheck pass", Repository: "sf/r", SHA: "abc"},
		Trip:    TripReport{Detector: TripSameFailure, Detail: "`sh loop.sh` failed 3 times"},
		Digest:  ProgressDigest{Files: map[string]int{}},
		Record:  `{"kind":"command"}`,
		Model:   "factory/coding-cheap",
		Calls:   1,
		MaxCall: 3,
	}
}

func TestSupervisorModelMintsATaskKeyAndCallsTheAlias(t *testing.T) {
	f := &fakeLiteLLM{content: `{"action":"redirect","guidance":"read the script first"}`, cost: "0.02"}
	m := testSupervisorModel(f.srv(t).URL)
	meter := m.Meter.(*memPolicy)
	d := m.Decide(context.Background(), "t1", testInput())
	if d != (Decision{Action: ActionRedirect, Guidance: "read the script first"}) {
		t.Fatalf("decision = %+v", d)
	}
	input, _ := json.MarshalIndent(testInput(), "", " ")
	payload, _ := chatPayload("supervisor", supervisorPrompt, input, 9000)
	if len(meter.holds) != 1 || meter.holds[0] != (policy.HoldRequest{TaskID: "t1", Kind: policy.KindModel, Ref: "supervisor",
		InTokens: len(payload), Price: supervisorPrice, WantOut: 9000, DayEUR: 20}) {
		t.Fatalf("holds = %+v, want one for the payload's bytes and 9000 output tokens", meter.holds)
	}
	holdEUR := float64(len(payload))*1e-6 + 9000*4e-6
	if len(f.mints) != 1 {
		t.Fatalf("mints = %d", len(f.mints))
	}
	mint := f.mints[0]
	meta, _ := mint["metadata"].(map[string]any)
	models, _ := mint["models"].([]any)
	if math.Abs(mint["max_budget"].(float64)-holdEUR) > 1e-12 || meta["task_id"] != "t1" || meta["purpose"] != "supervisor" || len(models) != 1 || models[0] != "supervisor" {
		t.Fatalf("mint = %v, want max_budget the hold %v, its task_id, the supervisor alias only", mint, holdEUR)
	}
	if len(f.chats) != 1 || f.auth[0] != "Bearer sk-super-1" {
		t.Fatalf("chats = %d, auth = %v, want one call with the minted key", len(f.chats), f.auth)
	}
	chat := f.chats[0]
	if chat["model"] != "supervisor" || chat["max_tokens"] != float64(9000) {
		t.Fatalf("chat = model %v, max_tokens %v; want the alias and answer+headroom", chat["model"], chat["max_tokens"])
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	user, _ := msgs[1].(map[string]any)["content"].(string)
	for _, want := range []string{"make loopcheck pass", "same_failure", "failed 3 times", `"record"`} {
		if !strings.Contains(user, want) {
			t.Fatalf("user message lacks %q: %s", want, user)
		}
	}
	// booked at call time with the call's own cost
	if len(meter.released) != 1 || meter.released[0].cost == nil || *meter.released[0].cost != 0.02 || meter.released[0].hold.Ref != "supervisor" {
		t.Fatalf("released = %+v, want the hold booked at 0.02", meter.released)
	}
}

// Hand-derived: the free headroom pays for the payload's bytes at 1e-6 and
// 2000 output tokens at 4e-6, so the call's 9000 are clamped to 2000.
func TestSupervisorOutputClampedToTheHeadroom(t *testing.T) {
	f := &fakeLiteLLM{content: `{"action":"continue"}`, cost: "0.001"}
	m := testSupervisorModel(f.srv(t).URL)
	input, _ := json.MarshalIndent(testInput(), "", " ")
	payload, _ := chatPayload("supervisor", supervisorPrompt, input, 9000)
	meter := &memPolicy{taskFree: float64(len(payload))*1e-6 + 2000*4e-6, dayFree: 20}
	m.Meter = meter
	if d := m.Decide(context.Background(), "t1", testInput()); d.Action != ActionContinue {
		t.Fatalf("decision = %+v", d)
	}
	if got := f.chats[0]["max_tokens"]; got != float64(2000) {
		t.Fatalf("max_tokens = %v, want 2000", got)
	}

	// below the minimum call: no key, no call, nothing to release
	f2 := &fakeLiteLLM{content: `{"action":"continue"}`}
	m2 := testSupervisorModel(f2.srv(t).URL)
	m2.Meter = &memPolicy{taskFree: 20, dayFree: float64(len(payload))*1e-6 + 1000*4e-6}
	d := m2.Decide(context.Background(), "t1", testInput())
	if d.Action != ActionEscalate || !strings.Contains(d.Reason, "no headroom") || len(f2.mints)+len(f2.chats) != 0 || len(m2.Meter.(*memPolicy).released) != 0 {
		t.Fatalf("no headroom: %+v, mints %d, chats %d; want escalate without a call", d, len(f2.mints), len(f2.chats))
	}
}

func TestSupervisorModelFailureEscalates(t *testing.T) {
	f := &fakeLiteLLM{status: http.StatusInternalServerError}
	m := testSupervisorModel(f.srv(t).URL)
	d := m.Decide(context.Background(), "t1", testInput())
	if d.Action != ActionEscalate || !strings.Contains(d.Reason, "500") {
		t.Fatalf("decision = %+v, want escalate naming the failure", d)
	}
	if r := m.Meter.(*memPolicy).released; len(r) != 1 || r[0].cost == nil || *r[0].cost != 0 {
		t.Fatalf("released = %+v, want the failed call booked at 0", r)
	}
	// an answer without a cost books the hold
	f2 := &fakeLiteLLM{content: "sure, continue"}
	m2 := testSupervisorModel(f2.srv(t).URL)
	if d := m2.Decide(context.Background(), "t1", testInput()); d.Action != ActionEscalate {
		t.Fatalf("prose answer: %+v, want escalate", d)
	}
	if r := m2.Meter.(*memPolicy).released; len(r) != 1 || r[0].cost != nil {
		t.Fatalf("released = %+v, want the hold booked in full (cost unknown)", r)
	}
}

func TestSessionSupervisionCapsCallsPerSession(t *testing.T) {
	calls := 0
	s := &sessionSupervision{max: 3, decide: func(context.Context, *Trip, int) Decision {
		calls++
		return Decision{Action: ActionContinue}
	}}
	trip := &Trip{Kind: TripSameCall, Detail: "read ×4"}
	for i := 1; i <= 3; i++ {
		if d, called := s.onTrip(context.Background(), trip); !called || d.Action != ActionContinue {
			t.Fatalf("trip %d: %+v called=%v, want a call answering continue", i, d, called)
		}
	}
	d, called := s.onTrip(context.Background(), trip)
	if called || calls != 3 || d.Action != ActionEscalate || !strings.Contains(d.Reason, "same_call") {
		t.Fatalf("4th trip: %+v called=%v calls=%d, want escalate without a call", d, called, calls)
	}
}
