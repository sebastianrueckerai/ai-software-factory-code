package main

import (
	"strings"
	"testing"
)

// The subagents' page is config-level: no first call of ours exists to
// capture — their prompts are opencode's own — so the page carries the
// agent block verbatim, its one builder's provenance, and says so instead
// of claiming a fixture task.
func TestConfigPageRendersTheAgentBlock(t *testing.T) {
	_, syms := writeFixtureModule(t)
	a := Agent{
		ID: "opencode-subagents", Pkg: "internal/fixture", Family: "A",
		Job: "opencode-internal task and explore delegation", Runs: "inside opencode, under the session config's agent block",
	}
	frag := []byte(`{"build":{"steps":1500},"explore":{"model":"factory/small","steps":1500},"general":{"model":"factory/small","steps":1500}}`)
	md, failures := configPage(a, "deadbeef", "policyhash", syms, frag)
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, want := range []string{
		"# opencode-subagents",
		"opencode-internal task and explore delegation · runs:",
		"`deadbeef`",
		"`policyhash`",
		"opencode 1.18.32",
		"`buildOpencodeConfig`",
		"blob/deadbeef/internal/fixture/a.go#L",
		"\"factory/small\"",
		"raw/opencode-subagents.json",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(md, "Fixture task") {
		t.Error("a config page claims a fixture task; no task runs here")
	}
}
