package main

import (
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
)

// pageFixture is one captured request, hand-built: a system message a
// builder made, a user message of fixture data, one offered tool.
func pageFixture() (agentdoc.Recorded, []agentdoc.Span) {
	raw := `{"model":"big","stream":true,"max_tokens":16,"messages":[` +
		`{"role":"system","content":"SYS-A"},{"role":"user","content":"USER-B"}],` +
		`"tools":[{"type":"function","function":{"name":"hammer","description":"it hits","parameters":{"type":"object"}}}]}`
	tool := `{"type":"function","function":{"name":"hammer","description":"it hits","parameters":{"type":"object"}}}`
	return agentdoc.Recorded{Raw: []byte(raw), Task: "t-fixture", Purpose: "operator"},
		[]agentdoc.Span{
			{Name: "the rules", Symbol: "(*R).M", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceSystem, Text: "SYS-A"},
			{Name: "the ask", Symbol: "askConst", Kind: agentdoc.KindFixture, Place: agentdoc.PlaceUser, Text: "USER-B"},
			{Name: "tool hammer", Symbol: "(*R).offered", Kind: agentdoc.KindBuilder, Place: agentdoc.PlaceTools, Text: tool},
		}
}

func pageAgent() Agent {
	return Agent{ID: "wired", Pkg: "internal/fixture", Family: "test", Job: "a job line", Runs: "somewhere in the factory"}
}

func TestPageRendersProvenancePerPart(t *testing.T) {
	_, syms := writeFixtureModule(t)
	rec, spans := pageFixture()
	md, failures := page(pageAgent(), "deadbeef", "policyhash", syms, rec, spans)
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, want := range []string{
		"**Fixture task, not live data.**",
		"`(*R).M`",
		"blob/deadbeef/internal/fixture/a.go#L10",
		"```text\nSYS-A\n```",
		"`askConst`",
		"USER-B",
		"### hammer",
		"it hits",
		"raw/wired.json",
		"`t-fixture`",
		"`operator`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestPageCollapsesLongParts(t *testing.T) {
	_, syms := writeFixtureModule(t)
	rec, spans := pageFixture()
	long := strings.Repeat("x", 2048)
	spans[0].Text = long
	rec.Raw = []byte(strings.Replace(string(rec.Raw), "SYS-A", long, 1))
	md, _ := page(pageAgent(), "deadbeef", "policyhash", syms, rec, spans)
	if !strings.Contains(md, "<details>") {
		t.Fatal("a 2 KiB part is not collapsed")
	}
}

func TestPageFailsOnUnlabelledBytes(t *testing.T) {
	_, syms := writeFixtureModule(t)
	rec, spans := pageFixture()
	spans[1].Text = "USER" // the trailing -B stays unclaimed
	md, failures := page(pageAgent(), "deadbeef", "policyhash", syms, rec, spans)
	if len(failures) == 0 {
		t.Fatal("unlabelled bytes produced no failure")
	}
	if !strings.Contains(strings.Join(failures, "\n"), "unlabelled") {
		t.Fatalf("failures do not name the unlabelled bytes: %v", failures)
	}
	if !strings.Contains(md, "unlabelled") {
		t.Fatal("the page does not mark the unlabelled bytes")
	}
}

func TestPageFailsOnARottedSpan(t *testing.T) {
	_, syms := writeFixtureModule(t)
	rec, spans := pageFixture()
	spans[0].Text = "SYS-A-ROTTED" // no builder output matches any more
	_, failures := page(pageAgent(), "deadbeef", "policyhash", syms, rec, spans)
	if len(failures) == 0 {
		t.Fatal("a span that no longer matches produced no failure")
	}
}

func TestPageFailsOnAnUnresolvableSymbol(t *testing.T) {
	_, syms := writeFixtureModule(t)
	rec, spans := pageFixture()
	spans[0].Symbol = "(*Gone).Moved"
	_, failures := page(pageAgent(), "deadbeef", "policyhash", syms, rec, spans)
	if len(failures) == 0 {
		t.Fatal("an unresolvable symbol produced no failure")
	}
}
