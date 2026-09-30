package policy

import "testing"

// G1: a node with no way out reaches neither done nor a human card.
func TestGraphRefusesDeadEnd(t *testing.T) {
	g := Graph{Process: "x", Nodes: []Node{{ID: "ready", Name: "lane ready"}, {ID: "done", Name: "done", Kind: NodeDone}}}
	if err := g.Check(); err == nil || err.Error() != "processes/x.yaml: graph: from lane ready no path reaches done or a human card" {
		t.Fatalf("Check = %v", err)
	}
	g.Edges = append(g.Edges, Edge{From: "ready", To: "done", Next: true})
	if err := g.Check(); err != nil {
		t.Fatalf("with a way out: %v", err)
	}
}

// G2: checks -> session -> checks with no capped edge loops for ever.
func TestGraphRefusesUncappedCycle(t *testing.T) {
	g := Graph{Process: "x",
		Nodes: []Node{{ID: "session", Name: "coder session"}, {ID: "checks", Name: "stage checks"}, {ID: "done", Name: "done", Kind: NodeDone}},
		Edges: []Edge{
			{From: "session", To: "checks", Next: true},
			{From: "checks", To: "session", Label: "findings"},
			{From: "checks", To: "done", Next: true},
		}}
	err := g.Check()
	if err == nil || err.Error() != "processes/x.yaml: graph: the cycle coder session -> stage checks -> coder session has no cap" {
		t.Fatalf("Check = %v", err)
	}
	g.Nodes[0], g.Nodes[1] = g.Nodes[1], g.Nodes[0]
	if err := g.Check(); err == nil || err.Error() != "processes/x.yaml: graph: the cycle stage checks -> coder session -> stage checks has no cap" {
		t.Fatalf("Check = %v", err)
	}
	g.Edges[1].Capped = true
	if err := g.Check(); err != nil {
		t.Fatalf("with the return capped: %v", err)
	}
}

// The shipped processes' graphs pass their own check.
func TestGraphOfTheFixtureProcessesPasses(t *testing.T) {
	tree, err := ParseTree(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range tree.Processes {
		g := BuildGraph(name, p, tree.Document)
		if err := g.Check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if _, ok := g.Entries[CardPlan]; ok != (name == "spec") {
			t.Errorf("%s: plan entry %v", name, ok)
		}
	}
}
