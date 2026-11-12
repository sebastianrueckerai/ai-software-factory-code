package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// agentDocLoopPolicyYAML is the loop's fixture policy: a tree the real
// loader accepts — the infra tree's shape, trimmed to what the captures
// read: the classes with their tools, the bounds the guides name, the
// limits the session runs under. The loader itself names what it lacks.
const agentDocLoopPolicyYAML = `# yaml-language-server: $schema=schema/policy.schema.json
version: 1
budget:
  task_default_eur: 5
  task_max_eur: 15
  day_eur: 100
  month_eur: 600
  session_floor_cheap_eur: 0.30
  session_floor_frontier_eur: 0.60
chat:
  monthly_eur: 30
  model: premium
limits:
  sessions: 3
  agent_time: 2h
  check_in: 45m
  supervisor_calls: 6
  supervisor_timeout: 6m
  stronger_after: 3
  steps: 1500
classes:
  coder:
    session: code
    models: [small, big]
    review_model: premium
    supervisor_model: big
    audit_model: big
    checkin_model: big
    vision_model: vision
    tools:
      escalate: {}
      request_info: {}
      delegate: {}
      request_tool: {}
      request_cut: {}
      manual: {}
  analyzer:
    session: report
    models: [small, big]
    supervisor_model: big
    vision_model: vision
    tools:
      escalate: {}
      request_info: {}
      manual: {}
  planner:
    session: plan
    models: [small, big, premium]
    session_model: premium
    supervisor_model: big
    review_model: big
    time: {agent_time: 30m, check_in: 0s}
    tools:
      escalate: {}
      file_plan: {}
      manual: {}
  prover:
    session: report
    models: [small, big]
    session_model: big
    supervisor_model: big
    time: {agent_time: 90m, check_in: 0s}
    tools:
      file_proofs: {}
      escalate: {}
      manual: {}
prices:
  max_output_tokens:
    small: 24576
    big: 24576
    premium: 24576
    vision: 4096
risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: auto}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: high, paths: ["k8s/**", "**/go.mod", "**/go.sum", "**/AGENTS.md"]}
  default: normal
  large_lines: 1000
envelope_default:
  monthly_eur: 10
  server_types: [cx23, cx33]
  max_count: 2
  max_lease_hours: 4
  volume_max_gb: 20
`

// agentDocLoopPolicy loads the fixture tree the way the tool loads the
// real one; a fixture the loader rejects fails the test where the loader
// says why.
func agentDocLoopPolicy() policy.Version {
	tree, err := CheckTree(map[string][]byte{"policy.yaml": []byte(agentDocLoopPolicyYAML)},
		ModelLadder{Start: "factory/" + policy.Small, Stronger: "factory/" + policy.Big}.Aliases())
	if err != nil {
		panic(err)
	}
	return policy.Version{Commit: "fixture", Tree: tree}
}

// The loop's modes: coding, screening, sizing, the planner, the prover and
// the message responder.
func TestAgentDocLoopIDs(t *testing.T) {
	if got, want := AgentDocLoopIDs(), []string{"coding", "screening", "sizing", "planner", "prover", "message-responder"}; !slices.Equal(got, want) {
		t.Fatalf("AgentDocLoopIDs() = %q, want %q", got, want)
	}
}

// Every loop mode's first user turn — the brief — is labelled by the
// builders that render it, and tiles with no unlabelled byte.
func TestAgentDocLoopBriefLabelsTheUserTurn(t *testing.T) {
	restore := randomTag
	randomTag = func() string { return agentDocLoopTag }
	defer func() { randomTag = restore }()
	v := agentDocLoopPolicy()
	for _, id := range AgentDocLoopIDs() {
		t.Run(id, func(t *testing.T) {
			b := agentDocLoopBrief(id, v)
			text := objectiveText(b)
			claims, err := agentdoc.Locate(text, agentDocLoopSpans(id, b, agentDocLoopTag))
			if err != nil {
				t.Fatalf("locate: %v", err)
			}
			for _, c := range claims {
				if c.Unlabelled {
					t.Errorf("%d unlabelled bytes: %q", c.End-c.Start, text[c.Start:c.End])
				}
			}
		})
	}
}

// The responder's turn is the writer's message under the session's basics
// preamble: the preamble is the runner's, the writer's words are fixture.
func TestAgentDocLoopMessageLabelsTheRespondersTurn(t *testing.T) {
	text, spans := agentDocLoopMessage()
	claims, err := agentdoc.Locate(text, spans)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	for _, c := range claims {
		if c.Unlabelled {
			t.Errorf("%d unlabelled bytes: %q", c.End-c.Start, text[c.Start:c.End])
		}
	}
}

// The fixture policy's bounds reach the guides: the sizing names the
// lines it derives from the risk bound.
func TestAgentDocLoopSizingCarriesThePolicyBound(t *testing.T) {
	b := agentDocLoopBrief("sizing", agentDocLoopPolicy())
	if !strings.Contains(b.Report, "small is at most 300") || !strings.Contains(b.Report, "medium at most 600") {
		t.Fatalf("the sizing guide lacks the policy's bounds: %q", b.Report)
	}
}

// The six modes share the fixture's facts shape: a coding session names
// its attempt, a report session does not.
func TestAgentDocLoopFactsFollowTheMode(t *testing.T) {
	v := agentDocLoopPolicy()
	coding := agentDocLoopBrief("coding", v).Facts
	screening := agentDocLoopBrief("screening", v).Facts
	if !strings.Contains(coding, "this is session 2 of the 3") {
		t.Errorf("the coding facts name no attempt: %q", coding)
	}
	if strings.Contains(screening, "session 2") {
		t.Errorf("the screening facts name an attempt: %q", screening)
	}
}

// sessionMessage is the one composer of a writer's message into a running
// session: the first one carries the basics, later ones do not.
func TestSessionMessage(t *testing.T) {
	first := sessionMessage("t-1", "Ann", "carry on", true)
	again := sessionMessage("t-1", "Ann", "and again", false)
	if !strings.Contains(first, "You are the agent session of card t-1") ||
		!strings.Contains(first, "A message from Ann, a writer of this project: carry on") {
		t.Fatalf("the first message lacks its basics or its wrapper: %q", first)
	}
	if strings.Contains(again, "You are the agent session") ||
		!strings.Contains(again, "A message from Ann, a writer of this project: and again") {
		t.Fatalf("a later message carries the basics: %q", again)
	}
}

// The loop's wire parts — the harness's system prompt around the
// repository's AGENTS.md, the offered tools, the scripted turns of the
// responder's later call — are read out of the recorded body itself: the
// page shows the wire, not a reconstruction of it.
func TestAgentDocLoopWireSpansReadTheBody(t *testing.T) {
	sys := strconv.Quote("PREAMBLE" + agentDocLoopAgents + "TAIL")
	builtin := `{"type":"function","function":{"name":"bash","description":"runs it"}}`
	factory := `{"type":"function","function":{"name":"factory-tools_escalate","description":"asks"}}`
	call := `{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo fixture-answer\"}"}}`
	raw := []byte(`{"model":"small","stream":true,"messages":[{"role":"system","content":` + sys + `},` +
		`{"role":"user","content":"THE-BRIEF"},{"role":"assistant","content":"","tool_calls":[` + call + `]},` +
		`{"role":"tool","content":"TOOL-RESULT"},{"role":"user","content":"THE-MESSAGE"}],` +
		`"tools":[` + builtin + `,` + factory + `]}`)
	b, err := agentdoc.ParseBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	spans, err := agentDocLoopWireSpans(b, agentDocLoopAgents)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ place, name, symbol, kind, text string }
	var got []row
	for _, sp := range spans {
		got = append(got, row{sp.Place, sp.Name, sp.Symbol, sp.Kind, sp.Text})
	}
	want := []row{
		{"system", "the harness preamble", AgentDocOpencodeVersion, agentdoc.KindHarness, "PREAMBLE"},
		{"system", "AGENTS.md", "AGENTS.md", agentdoc.KindFile, agentDocLoopAgents},
		{"system", "the harness tail", AgentDocOpencodeVersion, agentdoc.KindHarness, "TAIL"},
		{"user", "the scripted tool call", "", agentdoc.KindFixture, call},
		{"user", "the scripted tool result", "", agentdoc.KindFixture, "TOOL-RESULT"},
		{"tools", "tool bash", AgentDocOpencodeVersion, agentdoc.KindHarness, builtin},
		{"tools", "tool factory-tools_escalate", "gateway.Server.toolServer", agentdoc.KindGateway, factory},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("wire spans:\n got %+v\nwant %+v", got, want)
	}
}

// The config the subagents run under is the same tree the session reads:
// what buildOpencodeConfig tells opencode about general and explore, in
// the policy's own steps and the ladder's cheap rung.
func TestAgentDocAgentConfigAnswersTheAgentBlock(t *testing.T) {
	frag, err := AgentDocAgentConfig(agentDocLoopPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var agent struct {
		Build   map[string]any `json:"build"`
		General map[string]any `json:"general"`
		Explore map[string]any `json:"explore"`
	}
	if err := json.Unmarshal(frag, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.General["model"] != "factory/"+policy.Small ||
		agent.Explore["model"] != "factory/"+policy.Small {
		t.Fatalf("the subagents are not on the cheap rung: %s", frag)
	}
	for _, sub := range []map[string]any{agent.General, agent.Explore} {
		if sub["steps"] != float64(1500) {
			t.Fatalf("the subagents' step cap is not the policy's steps: %s", frag)
		}
	}
	if _, ok := agent.Build["model"]; ok {
		t.Fatalf("the build agent carries a model the session does not set: %s", frag)
	}
	if string(mustJSON(agent.General)) != string(mustJSON(agent.Explore)) {
		t.Fatalf("general and explore differ though the config sets them alike: %s", frag)
	}
}

// The system prompt without the repository's AGENTS.md is a broken
// capture, not a page with a hole.
func TestAgentDocLoopWireSpansWantTheAgentsFile(t *testing.T) {
	b, err := agentdoc.ParseBody([]byte(`{"messages":[{"role":"system","content":"only the harness"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentDocLoopWireSpans(b, agentDocLoopAgents); err == nil {
		t.Fatal("a system prompt without AGENTS.md derived spans")
	}
}

// TestAgentDocLoopRecord drives every mode's capture against the pinned
// opencode binary (OPENCODE_RECORD_BIN): the real session assembly and
// identity proxy, a scripted model, the gateway's real MCP tool list. It
// proves the captures the pages regenerate from: the chosen request tiles
// with no unlabelled byte, and the delegation's tools are on the wire.
func TestAgentDocLoopRecord(t *testing.T) {
	bin := os.Getenv("OPENCODE_RECORD_BIN")
	if bin == "" {
		t.Skip("OPENCODE_RECORD_BIN not set")
	}
	v := agentDocLoopPolicy()
	for _, id := range AgentDocLoopIDs() {
		t.Run(id, func(t *testing.T) {
			spans, rec, err := AgentDocLoop(context.Background(), id, bin, v)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			b, err := agentdoc.ParseBody(rec.Raw)
			if err != nil {
				t.Fatal(err)
			}
			textOf := func(m agentdoc.Message) string {
				if m.Parts == nil {
					return m.Content + m.ToolCallText()
				}
				var sb strings.Builder
				for _, p := range m.Parts {
					sb.WriteString(p.Text)
				}
				return sb.String() + m.ToolCallText()
			}
			var userText string
			for _, place := range []string{agentdoc.PlaceSystem, agentdoc.PlaceUser} {
				var texts []string
				for _, m := range b.Messages {
					if (place == agentdoc.PlaceSystem) != (m.Role == "system") {
						continue
					}
					texts = append(texts, textOf(m))
				}
				if len(texts) == 0 {
					continue
				}
				joined := strings.Join(texts, "\x00")
				if place == agentdoc.PlaceUser {
					userText = joined
				}
				var placeSpans []agentdoc.Span
				for _, sp := range spans {
					if sp.Place == place {
						placeSpans = append(placeSpans, sp)
					}
				}
				claims, err := agentdoc.Locate(joined, placeSpans)
				if err != nil {
					t.Fatalf("%s place: %v", place, err)
				}
				for _, c := range claims {
					// the \x00 between messages is the join's own byte, not a
					// wire one: the renderer skips it the same way
					if strings.Trim(joined[c.Start:c.End], "\x00") == "" {
						continue
					}
					if c.Unlabelled {
						t.Errorf("%s place: %d unlabelled bytes: %q", place, c.End-c.Start, joined[c.Start:c.End])
					}
				}
			}
			var names []string
			for _, tl := range b.Tools {
				names = append(names, tl.Name)
			}
			want := "factory-tools_escalate"
			switch id {
			case "planner":
				want = "factory-tools_file_plan"
			case "prover":
				want = "factory-tools_file_proofs"
			}
			if !slices.Contains(names, want) {
				t.Errorf("%s: the wire offers no %s: %v", id, want, names)
			}
			if id == "message-responder" && !strings.Contains(userText, "A message from fixture-writer, a writer of this project") {
				t.Error("the responder's captured call holds no writer's message")
			}
		})
	}
}

// fakeCapture runs AgentDocLoop against the fake opencode in capture mode,
// which writes day into the prompt's environment block.
func fakeCapture(t *testing.T, id string, v policy.Version, day string) ([]agentdoc.Span, agentdoc.Recorded) {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "opencode-serve.sse"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeServeEnv, fixture)
	t.Setenv("FAKE_SERVE_MODE", "capture")
	t.Setenv("FAKE_SERVE_DATE", day)
	stub := filepath.Join(t.TempDir(), "opencode-stub")
	self, _ := os.Executable()
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec "+self+" -test.run '^TestFakeOpencodeServe$'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spans, rec, err := AgentDocLoop(context.Background(), id, stub, v)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return spans, rec
}

// Two captures of one mode, each in its own random directory and on its
// own day, are the same bytes: the harness writes both into the prompt,
// and neither belongs on the page.
func TestAgentDocLoopCaptureIsByteStable(t *testing.T) {
	v := agentDocLoopPolicy()
	spansA, recA := fakeCapture(t, "coding", v, "Wed Nov 04 2026")
	spansB, recB := fakeCapture(t, "coding", v, "Thu Nov 12 2026")
	if !bytes.Equal(recA.Raw, recB.Raw) {
		t.Fatalf("the recorded bodies differ:\n%s\n%s", recA.Raw, recB.Raw)
	}
	if !reflect.DeepEqual(spansA, spansB) {
		t.Fatal("the spans differ between two captures")
	}
	for _, leak := range []string{"agentdoc-loop-", "Nov 04", "Nov 12"} {
		if bytes.Contains(recA.Raw, []byte(leak)) {
			t.Errorf("the body still carries %q", leak)
		}
	}
	if !bytes.Contains(recA.Raw, []byte("Working directory: /workspace\\n")) || !bytes.Contains(recA.Raw, []byte("Today's date: the day of the run\\n")) {
		t.Errorf("the masks are missing from %s", recA.Raw)
	}
}

func agentDocLoopPolicyFrom(t *testing.T, yaml string) policy.Version {
	t.Helper()
	tree, err := CheckTree(map[string][]byte{"policy.yaml": []byte(yaml)},
		ModelLadder{Start: "factory/" + policy.Small, Stronger: "factory/" + policy.Big}.Aliases())
	if err != nil {
		t.Fatal(err)
	}
	return policy.Version{Commit: "fixture", Tree: tree}
}

func assertLoopModels(t *testing.T, v policy.Version, want map[string]string) {
	t.Helper()
	for id, model := range want {
		_, rec := fakeCapture(t, id, v, "Wed Nov 04 2026")
		b, err := agentdoc.ParseBody(rec.Raw)
		if err != nil {
			t.Fatal(err)
		}
		if b.Model != model {
			t.Errorf("%s: the page names model %q, want %q", id, b.Model, model)
		}
	}
}

// Each mode's session runs on the model the runner picks: the plan session
// starts on the stronger rung (big), a report or code session on the cheap
// one (small), and a plan or report class's session_model replaces that.
// With the planner's session_model set to small the planner's page names
// small; the prover's keeps it on big.
func TestAgentDocLoopNamesTheModelThePolicyGives(t *testing.T) {
	yaml := strings.Replace(agentDocLoopPolicyYAML, "session_model: premium", "session_model: small", 1)
	if yaml == agentDocLoopPolicyYAML {
		t.Fatal("the fixture policy has no premium session_model to replace")
	}
	assertLoopModels(t, agentDocLoopPolicyFrom(t, yaml), map[string]string{
		"planner": "small", "prover": "big", "coding": "small", "message-responder": "small"})
}

// Without any session_model the base choice shows: the planner on big, the
// prover and the rest on small.
func TestAgentDocLoopNamesTheRunnersBaseModel(t *testing.T) {
	yaml := regexp.MustCompile(`(?m)^    session_model: .*\n`).ReplaceAllString(agentDocLoopPolicyYAML, "")
	if strings.Contains(yaml, "session_model") {
		t.Fatal("a session_model line is left in the fixture policy")
	}
	assertLoopModels(t, agentDocLoopPolicyFrom(t, yaml), map[string]string{
		"planner": "big", "prover": "small", "coding": "small", "message-responder": "small"})
}
