package main

import (
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/prompts"
)

// indexFixture answers the smallest row set the index renders from: two
// loop rows sharing the trunk, one direct row sharing a block with them,
// one config row.
func indexFixture() []indexRow {
	shared := "Write plainly. Short sentences."
	return []indexRow{
		{
			agent: Agent{ID: "coding", Family: "A", Job: "Implements the card", Runs: "agent-worker", Loop: true},
			model: "small", purpose: "session", task: "agentdoc-fixture", tools: 29, requestBytes: 49923,
			spans: []agentdoc.Span{
				{Name: "the sandbox facts", Symbol: "sandboxFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "Sandbox: gVisor pod"},
				{Name: "the guide", Symbol: "workGuide", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "Work the card."},
				{Name: "the writing guide", Symbol: "writingguide.Guide", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: shared},
			},
		},
		{
			agent: Agent{ID: "screening", Family: "A", Job: "Reads an issue", Runs: "agent-worker, report mode", Loop: true},
			model: "small", purpose: "report", task: "agentdoc-fixture", tools: 27, requestBytes: 49811,
			spans: []agentdoc.Span{
				{Name: "the sandbox facts", Symbol: "sandboxFacts", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "Sandbox: gVisor pod"},
				{Name: "the guide", Symbol: "screeningGuide", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "Judge the issue."},
			},
		},
		{
			agent: Agent{ID: "checkin", Family: "B", Job: "Decides continue", Runs: "factoryd"},
			model: "big", purpose: "checkin", task: "t-fixture", tools: 4, requestBytes: 5120,
			spans: []agentdoc.Span{
				{Name: "the writing guide", Symbol: "writingguide.Guide", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: shared},
			},
		},
	}
}

func TestIndexRendersInventoryMatrixSelfCheck(t *testing.T) {
	md, failures := index(indexFixture(), "deadbeef", "policyhash", "opencode 1.18.32")
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, want := range []string{
		"# The agent context map",
		"`deadbeef`",
		"`policyhash`",
		"`opencode 1.18.32`",
		"unlabelled spans: 0",
		"| agent | family | job | model | purpose | runs | tools | request bytes |",
		"[coding](coding.md)",
		"`small`",
		"`agentdoc-fixture`",
		"29",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("index lacks %q", want)
		}
	}
	// the duplication matrix names the block both agents carry
	if !strings.Contains(md, "writingguide.Guide") || !strings.Contains(md, "[coding](coding.md)") || !strings.Contains(md, "[checkin](checkin.md)") {
		t.Error("the matrix does not name the shared block and both its carriers")
	}
	// the trunk's bullets name the shared parts and drop the guide the
	// modes swap: same slot name, different bytes
	trunk := md[strings.Index(md, "## The one loop"):]
	if !strings.Contains(trunk, "- the sandbox facts — ") || strings.Contains(trunk, "- the guide — ") {
		t.Error("the trunk does not separate the shared parts from the swapped guide")
	}
	if !strings.Contains(md, "```mermaid") || !strings.Contains(md, "coding") {
		t.Error("the mermaid placeholder is missing")
	}
}

// rowOf reads a capture's row facts from the wire body itself.
func TestRowOfReadsTheWire(t *testing.T) {
	raw := []byte(`{"model":"small","tools":[{"type":"function","function":{"name":"bash"}}],"messages":[{"role":"user","content":"hi"}]}`)
	row, err := rowOf(Agent{ID: "coding"}, agentdoc.Recorded{Raw: raw, Task: "t-1", Purpose: "session"},
		[]agentdoc.Span{{Name: "the brief", Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if row.model != "small" || row.task != "t-1" || row.purpose != "session" || row.tools != 1 || row.requestBytes != len(raw) {
		t.Fatalf("row: %+v", row)
	}
}

// A span two agents share under different names still groups: the matrix
// keys the content, and each carrier names its own part.
func TestIndexMatrixGroupsByTextAcrossAgents(t *testing.T) {
	rows := indexFixture()
	md, failures := index(rows, "deadbeef", "policyhash", "opencode 1.18.32")
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	i := strings.Index(md, "## The duplication matrix")
	if i < 0 {
		t.Fatal("no matrix section")
	}
	section := md[i:]
	if !strings.Contains(section, "[coding](coding.md) — the writing guide") || !strings.Contains(section, "[checkin](checkin.md) — the writing guide") {
		t.Errorf("the matrix rows do not name agent and part:\n%s", section)
	}
}

// The matrix counts only cross-agent duplication: a block one agent
// carries twice, or two agents' distinct blocks, produce no rows.
func TestIndexMatrixSkipsSingleCarrierBlocks(t *testing.T) {
	rows := []indexRow{
		{agent: Agent{ID: "a", Family: "B"}, spans: []agentdoc.Span{
			{Name: "one", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "same"},
			{Name: "two", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "same"},
		}},
		{agent: Agent{ID: "b", Family: "B"}, spans: []agentdoc.Span{
			{Name: "other", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: "different"},
		}},
	}
	md, failures := index(rows, "deadbeef", "policyhash", "opencode 1.18.32")
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	i := strings.Index(md, "## The duplication matrix")
	rest := md[i:]
	if strings.Contains(rest, "`same`") || strings.Contains(rest, "`different`") {
		t.Errorf("single-carrier blocks entered the matrix:\n%s", rest)
	}
}

// The inventory prints each agent's profile and its words by part beside
// the words no profile made and the tools' words.
func TestIndexPrintsWordsByPart(t *testing.T) {
	rows := []indexRow{{
		agent: Agent{ID: "fake", Family: "B", Job: "fakes", Runs: "nowhere", Steps: []string{"x"}},
		model: "small", purpose: "review", task: "agentdoc-fixture", requestBytes: 100,
		parts: prompts.Parts{Guide: 10, Forms: 20, Chunks: 3, Paragraph: 40, Facts: 7}, harness: 100, toolWords: 50,
		spans: []agentdoc.Span{{Name: "the text", Symbol: "x", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: "text"}},
	}}
	md, failures := index(rows, "deadbeef", "policyhash", "opencode 1.19.6")
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, want := range []string{
		"| profile | harness words | guide words | forms words | chunks words | paragraph words | facts words | tool words |",
		"| x | 100 | 10 | 20 | 3 | 40 | 7 | 50 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("index lacks %q:\n%s", want, md)
		}
	}
}

// Every agent of the map names the steps its request is built from, or is
// the config page, or sends no text of ours; every step it names is
// registered.
func TestEveryAgentNamesItsSteps(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range prompts.Registered() {
		registered[r.Step] = true
	}
	for _, a := range registry {
		if len(a.Steps) == 0 && !a.NoPrompt {
			t.Errorf("%s names no step: list the profiles its request is built from, or mark it NoPrompt", a.ID)
		}
		for _, step := range a.Steps {
			if !registered[step] {
				t.Errorf("%s names step %s, which no code registers", a.ID, step)
			}
		}
	}
}

// A harness agent's words no profile made are its system text and its
// harness's own tools less its standing text; the tool words are the
// gateway's tools'. A direct agent's request is all its builders', and
// every tool on its wire counts as a tool. A tool's words are its
// description's and its parameters' descriptions'.
func TestWireWordsSplitTheHarnessFromTheTools(t *testing.T) {
	b, err := agentdoc.ParseBody([]byte(`{"messages":[{"role":"system","content":"one two three four five"},{"role":"user","content":"six seven"}],` +
		`"tools":[{"type":"function","function":{"name":"bash","description":"runs a command","parameters":{"properties":{"command":{"description":"the command to run"}}}}},` +
		`{"type":"function","function":{"name":"factory-tools_digest","description":"digests a file","parameters":{"properties":{"path":{"description":"the file"}}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if harness, tools := wireWords(Agent{Loop: true}, b, 2); harness != 10 || tools != 5 {
		t.Errorf("a harness agent's words = %d, %d; want 5 + 7 - 2 = 10 and 5", harness, tools)
	}
	if harness, tools := wireWords(Agent{}, b, 0); harness != 0 || tools != 12 {
		t.Errorf("a direct agent's words = %d, %d; want 0 and 7 + 5 = 12", harness, tools)
	}
}
