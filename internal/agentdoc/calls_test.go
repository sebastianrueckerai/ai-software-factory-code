package agentdoc

import (
	"path/filepath"
	"strings"
	"testing"
)

// The walk reads none of its own package: its lists name model paths as
// data, which would make the walk a transport of its own. Every site comes
// with the transport the walk reached it from.
func TestModelCallsReadNoneOfTheWalk(t *testing.T) {
	root := filepath.Join("..", "..")
	g, err := readCallGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	for fn := range g.requests {
		if strings.HasPrefix(fn, "agentdoc.") {
			t.Errorf("the walk read %s of its own package", fn)
		}
	}
	sites, err := ModelCalls(root)
	if err != nil {
		t.Fatal(err)
	}
	for fn, want := range map[string]string{
		"factoryd.(*Runner).RunSession":     "factoryd.(*Runner).runSession",
		"factoryd.(*Runner).MessageSession": "factoryd.(*Runner).messageWorker",
		"factoryd.(*Namer).Name":            "factoryd.postChat",
	} {
		if got := sites[fn].Via; got != want {
			t.Errorf("%s comes via %q, want %q", fn, got, want)
		}
	}
}
