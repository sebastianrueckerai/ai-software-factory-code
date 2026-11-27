package main

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/console"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The registry is hand-maintained; this keeps its shape honest: every
// entry has exactly one driver, every capture the drivers know has its
// page, and no entry ships without its descriptive lines.
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
	for _, id := range slices.Concat(factoryd.AgentDocDirectIDs(), console.AgentDocConsoleIDs()) {
		if a := agentByID(id); a == nil || a.Capture == nil {
			t.Errorf("the direct capture %s has no page", id)
		}
	}
	if agentByID("opencode-subagents") == nil {
		t.Error("the opencode-internal subagents have no page")
	}
}

// noPage are the model calls the map gives no page of their own, each
// with why.
var noPage = map[string]string{
	"factoryd.(*Runner).message":     "the supervisor's guidance rides the running session as its next turn, the turn the message-responder page shows",
	"factoryd.(*Runner).nudge":       "the nudge rides the running session as its next turn, the turn the message-responder page shows",
	"factoryd.(*Runner).watchLimits": "the note on a limit rides the running session as its next turn, the turn the message-responder page shows",
	"gateway.(*Server).replicate":    "the replicate_predict tool runs a hosted model on the session's own arguments, which hold no word of the factory's",
	"gateway.(*Server).fal":          "the fal_run tool runs a hosted model on the session's own arguments, which hold no word of the factory's",
}

// Every function that makes a model call, as the walk over the module
// finds it, is a call some page's capture drives, or one noPage gives a
// reason for. A capture of the map's own is no step of the factory. No
// entry names a function that makes no model call.
func TestRegistryHoldsEveryModelCall(t *testing.T) {
	sites, err := agentdoc.ModelCalls(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, a := range registry {
		if !a.Config && len(a.Calls) == 0 {
			t.Errorf("%s names no model call its capture drives", a.ID)
		}
		for _, c := range a.Calls {
			held[c] = true
			if _, ok := sites[c]; !ok {
				t.Errorf("%s names %s, which makes no model call", a.ID, c)
			}
		}
	}
	for c, why := range noPage {
		if _, ok := sites[c]; !ok {
			t.Errorf("noPage holds %s, which makes no model call (its reason: %s)", c, why)
		}
		if held[c] {
			t.Errorf("%s has a page and a reason for none", c)
		}
		held[c] = true
	}
	for _, c := range slices.Sorted(maps.Keys(sites)) {
		if !held[c] && !strings.HasPrefix(c[strings.LastIndex(c, ".")+1:], "agentDoc") {
			t.Errorf("%s makes a model call at %s that no page captures: add a registry entry whose capture drives it, or its reason to noPage", c, sites[c])
		}
	}
}
