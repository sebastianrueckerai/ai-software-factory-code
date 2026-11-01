package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
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
		{
			agent:   Agent{ID: "opencode-subagents", Family: "A", Job: "delegation", Runs: "inside opencode"},
			purpose: "config",
			spans: []agentdoc.Span{
				{Name: "the agent block", Symbol: "buildOpencodeConfig", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceUser, Text: `{"build":{}}`},
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
		"[opencode-subagents](opencode-subagents.md)",
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

// rowOf reads a capture's row facts from the wire body itself, and
// configRow answers the config page's row: one span, no request.
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
	cfg := configRow(Agent{ID: "opencode-subagents"}, json.RawMessage(`{"build":{}}`))
	if cfg.purpose != "config" || cfg.model != "" || cfg.task != "" || cfg.tools != 0 ||
		len(cfg.spans) != 1 || cfg.spans[0].Text != `{"build":{}}` {
		t.Fatalf("config row: %+v", cfg)
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

// The config row has no request: its bytes cell says so, like the model
// cell, instead of reading as a request of zero bytes.
func TestIndexMarksTheConfigRowAsNoRequest(t *testing.T) {
	md, failures := index(indexFixture(), "deadbeef", "policyhash", "opencode 1.18.32")
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "|") && strings.Contains(line, "[opencode-subagents](opencode-subagents.md) |") {
			if !strings.HasSuffix(line, "| 0 | — |") {
				t.Fatalf("config row reads %q, want the request-bytes cell to say —", line)
			}
		}
	}
}
