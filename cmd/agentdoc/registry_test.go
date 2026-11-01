package main

import (
	"slices"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The registry is hand-maintained; this keeps its shape honest: every
// entry has exactly one driver, the six loop modes are all present in the
// map, and no entry ships without its descriptive lines.
func TestRegistryShape(t *testing.T) {
	ids := make([]string, 0, len(registry))
	for _, a := range registry {
		drivers := 0
		for _, on := range []bool{a.Capture != nil, a.Loop, a.Config} {
			if on {
				drivers++
			}
		}
		if drivers != 1 {
			t.Errorf("%s: %d drivers, want exactly one", a.ID, drivers)
		}
		if a.ID == "" || a.Job == "" || a.Runs == "" || a.Family == "" || a.Pkg == "" {
			t.Errorf("%s: an inventory line is empty", a.ID)
		}
		if a.Capture == nil && a.Answer != "" {
			t.Errorf("%s: an answer without a direct capture", a.ID)
		}
		if !slices.Contains(ids, a.ID) {
			ids = append(ids, a.ID)
		} else {
			t.Errorf("%s: listed twice", a.ID)
		}
	}
	for _, id := range factoryd.AgentDocLoopIDs() {
		a := agentByID(id)
		if a == nil {
			t.Errorf("loop mode %s has no page", id)
			continue
		}
		if !a.Loop {
			t.Errorf("loop mode %s is not marked Loop", id)
		}
	}
	if agentByID("opencode-subagents") == nil {
		t.Error("the opencode-internal subagents have no page")
	}
}
