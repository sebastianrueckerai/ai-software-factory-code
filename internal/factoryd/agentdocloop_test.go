package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http/httptest"
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
      file_screening: {}
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
			p, err := objectiveText(b)
			if err != nil {
				t.Fatal(err)
			}
			text := p.Facts()
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

// The responder's turn is the writer's message under the guide and the
// chat form, as the session reads them the first time: the speaker is
// the runner's, the writer's words are fixture.
func TestAgentDocLoopMessageLabelsTheRespondersTurn(t *testing.T) {
	p, spans, err := agentDocLoopMessage()
	if err != nil {
		t.Fatal(err)
	}
	text := p.Text()
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

// The coding page shows what a session reads after one the platform ended:
// its findings hold the platform's line, which GateLines claims, and no
// report mode's brief holds a finding.
func TestAgentDocLoopCodingShowsThePlatformsLine(t *testing.T) {
	v := agentDocLoopPolicy()
	want := "Your previous session did not fail on its own. The node's memory guard killed its pod on sf-2 at 14:02 UTC. " +
		"Work after the last push, commit abc1234, is lost. That session does not count against the card."
	b := agentDocLoopBrief("coding", v)
	if !slices.Equal(b.Findings, []string{want}) {
		t.Fatalf("the coding brief's findings = %q, want the platform's line", b.Findings)
	}
	if !slices.ContainsFunc(agentDocLoopSpans("coding", b, agentDocLoopTag), func(s agentdoc.Span) bool { return s.Symbol == "GateLines" && s.Text == want }) {
		t.Fatal("no span of GateLines claims the platform's line")
	}
	if f := agentDocLoopBrief("screening", v).Findings; len(f) != 0 {
		t.Fatalf("the screening brief's findings = %q, want none", f)
	}
}

// sessionMessage is the one composer of a writer's message into a running
// session: it names the speaker, and the project agent's names the project
// agent, never a writer.
func TestSessionMessage(t *testing.T) {
	if got := sessionMessage("Ann", "carry on"); got != "A message from Ann, a writer of this project: carry on" {
		t.Fatalf("a writer's message = %q, want it named as the writer's", got)
	}
	if got := sessionMessage(ChatUser, "rebase first"); got != "A message from the project agent: rebase first" {
		t.Fatalf("the project agent's message = %q, want it named as the project agent's", got)
	}
}

// The loop's wire parts — the harness's system prompt around the
// repository's AGENTS.md, the offered tools, the scripted turns of the
// responder's later call — are read out of the recorded body itself: the
// page shows the wire, not a reconstruction of it.
func TestAgentDocLoopWireSpansReadTheBody(t *testing.T) {
	sys := strconv.Quote("LEADSTANDINGENV" + agentDocLoopAgents + "TAIL")
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
	spans, err := agentDocLoopWireSpans(b, "STANDING", true)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ place, name, symbol, kind, text string }
	var got []row
	for _, sp := range spans {
		got = append(got, row{sp.Place, sp.Name, sp.Symbol, sp.Kind, sp.Text})
	}
	want := []row{
		{"system", "the harness's lead", AgentDocOpencodeVersion, agentdoc.KindHarness, "LEAD"},
		{"system", "the profile's texts", "prompts.Build", agentdoc.KindBuilder, "STANDING"},
		{"system", "the harness's environment", AgentDocOpencodeVersion, agentdoc.KindHarness, "ENV"},
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

// The system prompt without the repository's AGENTS.md is a broken
// capture, not a page with a hole.
func TestAgentDocLoopWireSpansWantTheAgentsFile(t *testing.T) {
	b, err := agentdoc.ParseBody([]byte(`{"messages":[{"role":"system","content":"only the harness"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentDocLoopWireSpans(b, "only the harness", true); err == nil {
		t.Fatal("a system prompt without AGENTS.md derived spans")
	}
}

// A step whose profile loads no AGENTS.md has none on the wire: the page
// holds the harness around the standing text, and a body that holds the
// file is a broken capture.
func TestAgentDocLoopWireSpansForAStepWithoutAgents(t *testing.T) {
	read := func(system string) ([]agentdoc.Span, error) {
		b, err := agentdoc.ParseBody([]byte(`{"messages":[{"role":"system","content":` + strconv.Quote(system) + `}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return agentDocLoopWireSpans(b, "STANDING", false)
	}
	spans, err := read("LEADSTANDINGENV")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sp := range spans {
		names = append(names, sp.Name)
	}
	if want := []string{"the harness's lead", "the profile's texts", "the harness's environment"}; !slices.Equal(names, want) {
		t.Errorf("spans = %q, want %q", names, want)
	}
	if _, err := read("LEADSTANDING" + agentDocLoopAgents); err == nil {
		t.Error("a system prompt holding AGENTS.md derived spans for a step that loads none")
	}
}

// TestAgentDocLoopRecord drives every mode's capture against the pinned
// opencode binary (OPENCODE_RECORD_BIN): the real session assembly and
// identity proxy, a scripted model, the gateway's real MCP tool list. It
// proves the captures the pages regenerate from: the chosen request tiles
// with no unlabelled byte, the mode's gateway tool is on the wire, and
// every built-in tool carries its text from internal/prompts/tools.
func TestAgentDocLoopRecord(t *testing.T) {
	bin := os.Getenv("OPENCODE_RECORD_BIN")
	if bin == "" {
		t.Skip("OPENCODE_RECORD_BIN not set")
	}
	v := agentDocLoopPolicy()
	for _, id := range AgentDocLoopIDs() {
		t.Run(id, func(t *testing.T) {
			spans, rec, err := AgentDocLoop(context.Background(), id, bin, repoBake(t), v)
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
			case "screening":
				want = "factory-tools_file_screening"
			case "sizing":
				want = "factory-tools_manual"
			}
			if !slices.Contains(names, want) {
				t.Errorf("%s: the wire offers no %s: %v", id, want, names)
			}
			for _, tl := range b.Tools {
				if strings.HasPrefix(tl.Name, "factory-tools_") {
					continue
				}
				text, err := os.ReadFile(filepath.Join("..", "prompts", "tools", tl.Name+".md"))
				if err != nil {
					t.Errorf("%s: built-in %s has no text of ours: %v", id, tl.Name, err)
					continue
				}
				var tool struct {
					Function struct{ Description string } `json:"function"`
				}
				if err := json.Unmarshal(tl.Raw, &tool); err != nil || tool.Function.Description != strings.TrimSpace(string(text)) {
					t.Errorf("%s: built-in %s's description = %q, want internal/prompts/tools/%s.md", id, tl.Name, tool.Function.Description, tl.Name)
				}
			}
			if id == "message-responder" && !strings.Contains(userText, "A message from fixture-writer, a writer of this project") {
				t.Error("the responder's captured call holds no writer's message")
			}
		})
	}
}

// repoBake is the opencode config directory the sandbox image bakes, as
// the repository's files make it.
func repoBake(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := AgentDocBake(filepath.Join("..", ".."), dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The baked plugin gives a built-in tool the text of its file under
// tool-text/ and leaves a tool without one its harness text: the coding
// session's first request carries bash.md as bash's description, and
// edit keeps opencode's own. OPENCODE_BAKE names a built bake, such as
// the sandbox image's /opt/opencode-home/.config/opencode, in place of
// the repository's plugins.
func TestPluginReplacesABuiltinText(t *testing.T) {
	bin := os.Getenv("OPENCODE_RECORD_BIN")
	if bin == "" {
		t.Skip("OPENCODE_RECORD_BIN not set")
	}
	var bake string
	if from := os.Getenv("OPENCODE_BAKE"); from != "" {
		bake = filepath.Join(t.TempDir(), "opencode")
		if err := os.CopyFS(bake, os.DirFS(from)); err != nil {
			t.Fatal(err)
		}
	} else {
		bake = repoBake(t)
	}
	if err := os.RemoveAll(filepath.Join(bake, "tool-text")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bake, "tool-text"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bake, "tool-text", "bash.md"), []byte("Run one shell command.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, rec, err := AgentDocLoop(context.Background(), "coding", bin, bake, agentDocLoopPolicy())
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	var body struct {
		Tools []struct {
			Function struct{ Name, Description string } `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rec.Raw, &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tl := range body.Tools {
		got[tl.Function.Name] = tl.Function.Description
	}
	if got["bash"] != "Run one shell command." {
		t.Errorf("bash's description = %q, want the text of tool-text/bash.md", got["bash"])
	}
	if want := "Performs exact string replacements in files."; !strings.HasPrefix(got["edit"], want) {
		t.Errorf("edit's description = %q, want opencode's own, which opens %q", got["edit"], want)
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
	spans, rec, err := AgentDocLoop(context.Background(), id, stub, repoBake(t), v)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	return spans, rec
}

// Two captures of one page, each in its own random directory and on its
// own day, are the same bytes: the harness writes both into the prompt of
// the session's call, and neither belongs on the page.
func TestAgentDocLoopCaptureIsByteStable(t *testing.T) {
	v := agentDocLoopPolicy()
	for _, id := range []string{"coding"} {
		t.Run(id, func(t *testing.T) {
			spansA, recA := fakeCapture(t, id, v, "Wed Nov 04 2026")
			spansB, recB := fakeCapture(t, id, v, "Thu Nov 12 2026")
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
		})
	}
}

// The capture's gateway answers the window ask a session makes of every
// declared model before it starts, as the real gateway does, so the
// session runs under its models' windows and the auto compaction they
// turn on, never on the trust cap a refused ask leaves.
func TestAgentDocLoopGatewayServesTheWindow(t *testing.T) {
	srv := httptest.NewServer(agentDocLoopMux(&agentDocLoopModel{}, agentDocLoopPolicy(), "coding"))
	defer srv.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("agentdoc-dummy-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	oc := &Opencode{GatewayURL: srv.URL, TokenFile: token}
	for _, alias := range []string{policy.Small, policy.Big, policy.Premium} {
		if w, err := oc.servedWindow(context.Background(), alias); err != nil || w != 128000 {
			t.Errorf("the window of %s: %d, %v, want 128000", alias, w, err)
		}
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
		"planner": "small", "prover": "big", "screening": "small", "sizing": "small", "coding": "small", "message-responder": "small"})
}

// Without any session_model the base choice shows: the planner on big, the
// prover and the rest on small.
func TestAgentDocLoopNamesTheRunnersBaseModel(t *testing.T) {
	yaml := regexp.MustCompile(`(?m)^    session_model: .*\n`).ReplaceAllString(agentDocLoopPolicyYAML, "")
	if strings.Contains(yaml, "session_model") {
		t.Fatal("a session_model line is left in the fixture policy")
	}
	assertLoopModels(t, agentDocLoopPolicyFrom(t, yaml), map[string]string{
		"planner": "big", "prover": "small", "screening": "small", "sizing": "small", "coding": "small", "message-responder": "small"})
}

// session_model is for plan and report classes only; a code session runs on
// factoryd's ladder, as the tree's check says. A coder class that carries
// one anyway leaves the coding and responder pages on the cheap rung.
func TestAgentDocLoopCodeSessionsIgnoreSessionModel(t *testing.T) {
	v := agentDocLoopPolicy()
	coder := v.Document.Classes["coder"]
	coder.SessionModel = policy.Big
	classes := maps.Clone(v.Document.Classes)
	classes["coder"] = coder
	v.Document.Classes = classes
	assertLoopModels(t, v, map[string]string{"coding": "small", "message-responder": "small"})
}
