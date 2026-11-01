package main

// The authored layer: the one-liners and the master diagram, drafted
// once, picked by Sebastian, stored as data files beside the registry.
// Regeneration reads them here and never calls an LLM.

import (
	"embed"
	"strings"
	"sync"
)

//go:embed blurbs.md master.mmd
var authoredFS embed.FS

var blurbs = sync.OnceValue(func() map[string]string {
	b, err := authoredFS.ReadFile("blurbs.md")
	if err != nil {
		panic(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		id, text, found := strings.Cut(line, ": ")
		if !found || !strings.HasPrefix(line, "- ") {
			continue
		}
		out[strings.TrimPrefix(id, "- ")] = text
	}
	return out
})

// blurbOf answers an agent's authored one-liner.
func blurbOf(id string) (string, bool) {
	b, ok := blurbs()[id]
	return b, ok
}

// masterDiagram answers the authored mermaid source.
func masterDiagram() string {
	b, err := authoredFS.ReadFile("master.mmd")
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(b))
}

// hasNode answers whether the diagram carries id as a node of its own,
// not merely inside another node's label.
func hasNode(diagram, id string) bool {
	for _, open := range []string{"[", "(", "{"} {
		if strings.Contains(diagram, id+open) {
			return true
		}
	}
	return false
}

// pageDiagram answers the master diagram with one agent's node
// emphasised, so each page shows where its agent sits.
func pageDiagram(id string) string {
	return masterDiagram() + "\n  style " + id + " stroke-width:3px"
}
