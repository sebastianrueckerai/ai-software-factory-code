package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/agentdoc"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The naming call's page, end to end: the real capture crosses the fake
// gateway, its spans locate, its symbols resolve against this repo, and
// the page carries the parts the plan promises.
func TestTitleAgentPageEndToEnd(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	syms, err := loadSymbols(root)
	if err != nil {
		t.Fatal(err)
	}
	a := agentByID("title")
	if a == nil {
		t.Fatal("no agent titled title in the registry")
	}
	g := agentdoc.NewGateway(`{"decision":"continue"}`)
	defer g.Close()
	v := policy.Version{Commit: "fixture", Tree: policy.Tree{Document: policy.Document{Version: 1}}}
	spans, err := factoryd.AgentDocDirect(context.Background(), "title", g.URL, v, nil)
	if err != nil {
		t.Fatal(err)
	}
	recs := g.Bodies()
	if len(recs) != 1 {
		t.Fatalf("one call crosses the wire, got %d", len(recs))
	}
	md, failures := page(*a, "localsha", "policyhash", syms, recs[0], spans)
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	for _, want := range []string{
		"factoryd.titlePrompt",
		"(*OperatorModel).Title",
		"the thread's first message",
		"Set the staging budget to 40 euros.",
		"raw/title.json",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
