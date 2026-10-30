package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Card kinds: the ways a card enters a process.
const (
	CardDirect = "direct" // filed on the direct track
	CardPlan   = "plan"   // filed on the plan track
	CardChild  = "child"  // filed by a plan with no open question
)

// CardKinds are the card kinds.
var CardKinds = []string{CardDirect, CardPlan, CardChild}

// NodeKind is what a node of a process graph is.
type NodeKind int

const (
	NodeStep NodeKind = iota
	NodeHuman
	NodeDone
	NodeFailed
	NodeClosed
)

// Node is one step a card can be at.
type Node struct {
	ID string
	// Name names the node in the check's refusals, such as track direct.
	Name string
	// Head is the node's line in config explain.
	Head string
	// Ref is how an edge into the node reads; empty reads "step n".
	Ref  string
	Kind NodeKind
	// Cap is what bounds the returns into the node, for the caps line.
	Cap string
	// Later is what may happen after the path ends, for the later line.
	Later string
}

// Edge is one way from a node to another.
type Edge struct {
	From, To string
	// Label is what takes the edge; empty on an unlabelled main path.
	Label string
	// Next marks the path config explain numbers.
	Next bool
	// Capped: the edge spends a session (limits.sessions) or passes a
	// human, so a cycle through it ends.
	Capped bool
	// Line is where config explain prints the edge: 0 on the node's own
	// line, n on its n-th indented line, capLine on the caps line.
	Line int
	// Note follows the edge's target.
	Note string
}

const capLine = -1

// Graph is the directed graph of a process: one path per card kind from
// its creation through its track's class program, stages, the merge queue and
// the dependency wait to done, failed, closed or a human card.
type Graph struct {
	Process string
	Nodes   []Node
	Edges   []Edge
	// Entries are the card kinds' first nodes; a kind the process cannot
	// receive has none.
	Entries map[string]string
}

// The shared ends of every path.
const (
	nodeHuman  = "human"
	nodeFailed = "failed"
	nodeClosed = "closed"
)

type builder struct {
	g *Graph
	p Process
	d Document
}

func (b *builder) node(n Node) string {
	b.g.Nodes = append(b.g.Nodes, n)
	return n.ID
}

func (b *builder) edge(e Edge) { b.g.Edges = append(b.g.Edges, e) }

// next links a chain of nodes by their unlabelled main path.
func (b *builder) next(ids ...string) {
	for i := 1; i < len(ids); i++ {
		b.edge(Edge{From: ids[i-1], To: ids[i], Next: true})
	}
}

// BuildGraph builds the graph of process name from its blocks' contracts
// (stage11-design.md sections 1 and 3); d gives the numbers the text reads.
func BuildGraph(name string, p Process, d Document) Graph {
	b := &builder{g: &Graph{Process: name, Entries: map[string]string{}}, p: p, d: d}
	b.node(Node{ID: nodeHuman, Name: "human card", Head: "human card", Ref: "human card", Kind: NodeHuman})
	b.node(Node{ID: nodeFailed, Name: "failed", Head: "end: failed", Ref: "end: failed", Kind: NodeFailed})
	b.node(Node{ID: nodeClosed, Name: "closed", Head: "end: closed", Ref: "end: closed", Kind: NodeClosed})
	direct := p.Tracks.Direct
	created := b.node(Node{ID: "direct/created", Name: "card created",
		Head: "card created on track direct: an issue with no track label, or labelled bug or agent, from anyone, or any card factoryd files"})
	b.next(created, b.code(CardDirect, direct, " once a work slot is free", "end: done"))
	b.g.Entries[CardDirect] = created
	if plan := p.Tracks.Plan; plan != nil {
		b.planPath(*plan)
		b.childPath(direct)
	}
	return *b.g
}

func (b *builder) planPath(plan Track) {
	created := b.node(Node{ID: "plan/created", Name: "card created",
		Head: "card created on track plan: an issue labelled plan by a writer"})
	start := b.node(Node{ID: "plan/start", Name: "track plan",
		Head: fmt.Sprintf("track plan: starts a task of class %s (session %s) once a work slot is free", plan.Class, b.d.Classes[plan.Class].Session)})
	var bounds PlanShape
	if plan.Plan != nil {
		bounds = *plan.Plan
	}
	shape := fmt.Sprintf("answers a plan of %d to %d cards a round", bounds.Minimum(), bounds.MaxCards)
	if bounds.Foundation {
		shape += ", one foundation first"
	}
	if bounds.Integration {
		shape += ", one integration last"
	}
	shape += "; a bigger plan files its next round once every card of a round is done"
	session := b.node(Node{ID: "plan/session", Name: plan.Class + " session", Head: plan.Class + " session on the stronger rung: " + shape,
		Ref: "back to the " + plan.Class, Cap: fmt.Sprintf("every %s session counts against limits.sessions (%d)", plan.Class, b.d.Limits.Sessions)})
	children := b.node(Node{ID: "plan/children", Name: "children filed",
		Head: "a plan with no open question: factoryd files one card per planned card on track direct, with their dependencies (card kind child)"})
	questions := b.node(Node{ID: "plan/questions", Name: "plan questions", Head: "human card: the plan's open questions, for the writers to answer",
		Ref: "human card", Kind: NodeHuman})
	done := b.node(Node{ID: "plan/done", Name: "done", Head: "end: done", Kind: NodeDone})
	b.next(created, start, session, children, done)
	b.edge(Edge{From: session, To: session, Label: "plan invalid", Capped: true, Line: 1})
	b.edge(Edge{From: session, To: questions, Label: "an open question", Capped: true, Line: 1})
	b.edge(Edge{From: session, To: nodeHuman, Label: "at the cap", Capped: true, Line: capLine})
	b.edge(Edge{From: questions, To: session, Label: "the last answer, or a writer's comment", Capped: true})
	b.edge(Edge{From: questions, To: nodeClosed, Label: "the issue closed", Capped: true})
	b.edge(Edge{From: children, To: session, Label: "a cycle or any finding", Capped: true, Line: 1})
	b.g.Entries[CardPlan] = created
}

func (b *builder) childPath(direct Track) {
	created := b.node(Node{ID: "child/created", Name: "card created",
		Head: "card created: by factoryd from a plan with no open question, on track direct, with the plan's dependencies"})
	wait := b.node(Node{ID: "child/wait", Name: "dependency wait", Head: `track direct: waits until every card it depends on is done ("waiting on #n")`})
	stuck := b.node(Node{ID: "child/stuck", Name: "dependency human card", Ref: "human card", Kind: NodeHuman})
	start := b.code(CardChild, direct, "", "end: done; the cards that depend on this one may start")
	b.next(created, wait)
	b.edge(Edge{From: wait, To: stuck, Label: "a dependency ends failed or closed", Capped: true, Line: 1})
	b.edge(Edge{From: stuck, To: start, Label: "an answer", Capped: true})
	b.edge(Edge{From: stuck, To: nodeClosed, Label: "a close", Capped: true})
	b.next(wait, start)
	b.g.Entries[CardChild] = created
}

// code builds the coding program of kind's card from the direct track's
// start to its end and returns the start.
func (b *builder) code(kind string, direct Track, atOnce, end string) string {
	id := func(s string) string { return kind + "/" + s }
	start := b.node(Node{ID: id("start"), Name: "track direct",
		Head: fmt.Sprintf("track direct: starts a task of class %s (session %s)%s", direct.Class, b.d.Classes[direct.Class].Session, atOnce)})
	session := Node{ID: id("session"), Name: direct.Class + " session", Ref: "back to the " + direct.Class,
		Head: fmt.Sprintf("%s session (at most %d per task, limits.sessions)", direct.Class, b.d.Limits.Sessions),
		Cap:  fmt.Sprintf("every return to the %s counts against limits.sessions (%d)", direct.Class, b.d.Limits.Sessions)}
	chain := []string{start}
	stages := b.p.Stages
	if len(stages) > 0 && stages[0].Name == StageScreening {
		chain = append(chain, b.stage(id, stages[0], session.ID))
		stages = stages[1:]
	}
	chain = append(chain, b.node(session))
	b.edge(Edge{From: session.ID, To: nodeHuman, Label: "at the cap", Capped: true, Line: capLine})
	// the stages follow in the process's order: the local gates (policy,
	// migration, reproduction) first, then the checks with the review
	// beside their wait, audit, approval and merge (roadmap 222, item b);
	// the chain stays a chain whatever the order
	for _, s := range stages {
		if s.Name == StageApproval && b.p.Merge.Queue == QueueProject {
			q := b.node(Node{ID: id("queue"), Name: "merge queue",
				Head: "merge queue (project): one pull request at a time; main merged in and the checks run again when main moved"})
			b.edge(Edge{From: q, To: session.ID, Label: "a conflict or red checks", Capped: true, Line: 1})
			chain = append(chain, q)
		}
		chain = append(chain, b.stage(id, s, session.ID))
	}
	chain = append(chain,
		b.node(Node{ID: id("merged"), Name: "merged",
			Head: "merged: waits until the chain runs the change live (done names the run that covers it)"}),
		b.node(Node{ID: id("done"), Name: "done", Head: end, Kind: NodeDone}))
	b.next(chain...)
	return start
}

// stage builds one stage's node and its ways out besides the next stage.
func (b *builder) stage(id func(string) string, s Stage, session string) string {
	n := Node{ID: id("stage-" + s.Name), Name: "stage " + s.Name, Head: "stage " + s.Name}
	if !s.Enabled {
		n.Head += ": switched off"
		return b.node(n)
	}
	var detail []string
	if s.When == WhenOutsider {
		detail = append(detail, "an outside account's card only")
	}
	if s.Name == StageAudit {
		detail = append(detail, "projects with audit: true only")
	}
	switch s.Name {
	case StageApproval:
		detail = append(detail, "human card, or merge rule auto without one")
		b.edge(Edge{From: n.ID, To: session, Label: "request changes", Capped: true, Line: 1})
		b.edge(Edge{From: n.ID, To: nodeFailed, Label: "reject", Capped: true, Line: 1})
	case StageMerge:
		b.edge(Edge{From: n.ID, To: nodeHuman, Label: "a refusal", Capped: true})
		b.edge(Edge{From: n.ID, To: session, Label: "a conflict", Capped: true})
	default:
		e := Edge{From: n.ID, Label: "findings", Capped: true}
		switch s.OnFindings {
		case FindingsAgent:
			e.To = session
			if s.MaxReturns != nil {
				e.Note = fmt.Sprintf(", at most %d returns, then human card", *s.MaxReturns)
			}
		case FindingsHuman:
			e.To = nodeHuman
		default:
			e.To = nodeFailed
		}
		b.edge(e)
		if s.Name == StageReview {
			b.edge(Edge{From: n.ID, To: nodeHuman, Label: "no verdict", Capped: true})
		}
	}
	if len(detail) > 0 {
		n.Head += ": " + strings.Join(detail, "; ")
	}
	return b.node(n)
}

func (g Graph) file() string { return ProcessFile(g.Process) }

func (g Graph) out(id string) []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

func (g Graph) nodeOf(id string) (Node, bool) {
	i := slices.IndexFunc(g.Nodes, func(n Node) bool { return n.ID == id })
	if i < 0 {
		return Node{}, false
	}
	return g.Nodes[i], true
}

// node is the node id of a graph whose edges Check found sound.
func (g Graph) node(id string) Node {
	n, _ := g.nodeOf(id)
	return n
}

// Check refuses a node from which no path reaches done or a human card
// (G1) and a cycle without a capped edge (G2).
func (g Graph) Check() error {
	var errs []error
	for _, e := range g.Edges {
		for _, id := range []string{e.From, e.To} {
			if _, ok := g.nodeOf(id); !ok {
				errs = append(errs, fmt.Errorf("%s: graph: an edge names %q, which is no node", g.file(), id))
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	for _, n := range g.Nodes {
		if n.Kind != NodeStep {
			continue
		}
		seen := map[string]bool{n.ID: true}
		queue, ok := []string{n.ID}, false
		for len(queue) > 0 && !ok {
			for _, e := range g.out(queue[0]) {
				if k := g.node(e.To).Kind; k == NodeDone || k == NodeHuman {
					ok = true
				}
				if !seen[e.To] {
					seen[e.To] = true
					queue = append(queue, e.To)
				}
			}
			queue = queue[1:]
		}
		if !ok {
			errs = append(errs, fmt.Errorf("%s: graph: from %s no path reaches done or a human card", g.file(), n.Name))
		}
	}
	if cycle := g.uncappedCycle(); cycle != nil {
		names := make([]string, len(cycle))
		for i, id := range cycle {
			names[i] = g.node(id).Name
		}
		errs = append(errs, fmt.Errorf("%s: graph: the cycle %s has no cap", g.file(), strings.Join(names, " -> ")))
	}
	return errors.Join(errs...)
}

// uncappedCycle is a cycle of uncapped edges, from its first node in the
// graph's order back to it, or nil.
func (g Graph) uncappedCycle() []string {
	state := map[string]int{} // 1 on the path, 2 done
	var path []string
	var found []string
	var visit func(id string) bool
	visit = func(id string) bool {
		state[id] = 1
		path = append(path, id)
		for _, e := range g.out(id) {
			if e.Capped {
				continue
			}
			switch state[e.To] {
			case 1:
				found = slices.Clone(path[slices.Index(path, e.To):])
				return true
			case 0:
				if visit(e.To) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		state[id] = 2
		return false
	}
	for _, n := range g.Nodes {
		if state[n.ID] == 0 && visit(n.ID) {
			break
		}
	}
	if found == nil {
		return nil
	}
	first := 0
	for i, id := range found {
		if slices.IndexFunc(g.Nodes, func(n Node) bool { return n.ID == id }) < slices.IndexFunc(g.Nodes, func(n Node) bool { return n.ID == found[first] }) {
			first = i
		}
	}
	found = append(found[first:], found[:first]...)
	return append(found, found[0])
}

// Explain is config explain's text: the path of a card of kind through the
// process, numbered, with every other way out of each step.
func (g Graph) Explain(kind string) (string, error) {
	if !slices.Contains(CardKinds, kind) {
		return "", fmt.Errorf("card kind %q: want one of %v", kind, CardKinds)
	}
	entry, ok := g.Entries[kind]
	if !ok {
		return "", fmt.Errorf("process %s has no plan track: a %s card cannot enter it", g.Process, kind)
	}
	var path []string
	step := map[string]int{}
	for id := entry; id != ""; {
		path = append(path, id)
		step[id] = len(path)
		next := ""
		for _, e := range g.out(id) {
			if e.Next {
				next = e.To
			}
		}
		id = next
	}
	var target func(id string) string
	edge := func(e Edge) string { return e.Label + " -> " + target(e.To) + e.Note }
	target = func(id string) string {
		n := g.node(id)
		if s, ok := step[id]; ok {
			if n.Ref == "" {
				return fmt.Sprintf("step %d", s)
			}
			return fmt.Sprintf("%s (step %d)", n.Ref, s)
		}
		var outs []string
		for _, e := range g.out(id) {
			outs = append(outs, edge(e))
		}
		if len(outs) == 0 {
			return n.Ref
		}
		return n.Ref + ": " + strings.Join(outs, ", ")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "process %s (%s), card kind %s\n", g.Process, g.file(), kind)
	var later, caps []string
	for _, id := range path {
		n := g.node(id)
		lines := map[int][]string{}
		maxLine := 0
		for _, e := range g.out(id) {
			if e.Label == "" {
				continue
			}
			lines[e.Line] = append(lines[e.Line], edge(e))
			maxLine = max(maxLine, e.Line)
		}
		head := n.Head
		if inline := lines[0]; len(inline) > 0 {
			sep := ": "
			if strings.Contains(head, ": ") {
				sep = "; "
			}
			head += sep + strings.Join(inline, "; ")
		}
		fmt.Fprintf(&sb, "%2d. %s\n", step[id], head)
		for l := 1; l <= maxLine; l++ {
			fmt.Fprintf(&sb, "      %s\n", strings.Join(lines[l], "; "))
		}
		if n.Later != "" {
			later = append(later, n.Later)
		}
		if n.Cap != "" {
			caps = append(caps, n.Cap+"; "+strings.Join(lines[capLine], "; "))
		}
	}
	for _, l := range later {
		fmt.Fprintf(&sb, " later: %s\n", l)
	}
	for _, c := range caps {
		fmt.Fprintf(&sb, " caps: %s\n", c)
	}
	return sb.String(), nil
}
