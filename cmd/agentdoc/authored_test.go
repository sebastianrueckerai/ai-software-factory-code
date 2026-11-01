package main

import (
	"strings"
	"testing"
)

// The authored layer is data the tool embeds, not code: every registry
// agent has exactly one blurb and a node in the master diagram. A
// registry entry that outgrows the authored layer fails here, at test
// time, not at regeneration.
func TestAuthoredLayerCoversTheRegistry(t *testing.T) {
	for _, a := range registry {
		if b, ok := blurbOf(a.ID); !ok || strings.TrimSpace(b) == "" {
			t.Errorf("%s: no blurb", a.ID)
		}
		if !strings.Contains(masterDiagram(), a.ID) {
			t.Errorf("%s: no node in the master diagram", a.ID)
		}
	}
}

// A page carries its authored blurb under the job line and the master
// diagram with its own node emphasised.
func TestPageCarriesTheAuthoredLayer(t *testing.T) {
	_, syms := writeFixtureModule(t)
	rec, spans := pageFixture()
	a := pageAgent()
	a.ID = "checkin"
	md, failures := page(a, "deadbeef", "policyhash", syms, rec, spans)
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	blurb, _ := blurbOf("checkin")
	for _, want := range []string{
		blurb,
		"## Where this agent sits",
		"signals --> screening",
		"style checkin stroke-width:3px",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// The config page carries the same authored layer as the captured pages.
func TestConfigPageCarriesTheAuthoredLayer(t *testing.T) {
	_, syms := writeFixtureModule(t)
	a := Agent{
		ID: "opencode-subagents", Pkg: "internal/fixture", Family: "A",
		Job: "opencode-internal task and explore delegation", Runs: "inside opencode, under the session config's agent block",
	}
	frag := []byte(`{"build":{"steps":1500}}`)
	md, failures := configPage(a, "deadbeef", "policyhash", syms, frag)
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	blurb, _ := blurbOf("opencode-subagents")
	for _, want := range []string{
		blurb,
		"## Where this agent sits",
		"style opencode-subagents stroke-width:3px",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("config page lacks %q", want)
		}
	}
}

// The index carries every agent's one-liner and the master diagram; the
// generated skeleton is gone.
func TestIndexCarriesTheAuthoredLayer(t *testing.T) {
	md, failures := index(indexFixture(), "deadbeef", "policyhash", "opencode 1.18.32")
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, id := range []string{"coding", "screening", "checkin", "opencode-subagents"} {
		blurb, ok := blurbOf(id)
		if !ok {
			t.Fatalf("%s: no blurb", id)
		}
		if !strings.Contains(md, "- ["+id+"]("+id+".md): "+blurb) {
			t.Errorf("index lacks %s's one-liner", id)
		}
	}
	if !strings.Contains(md, "signals --> screening") {
		t.Error("index lacks the master diagram")
	}
	if strings.Contains(md, "The authored master diagram lands here") || strings.Contains(md, "family A") {
		t.Error("the generated skeleton still stands in")
	}
}
