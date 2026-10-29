// Package doclinks resolves the relative markdown links of a card's text
// against the files of the project's document repository. The text's own
// location is unknown, so a link with its leading ../ and ./ removed names
// the file at that path, else every file whose path ends in it.
package doclinks

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	maxLinks  = 20
	maxTarget = 300
)

var (
	link   = regexp.MustCompile(`\]\(\s*([^\s)]+)[^)]*\)`)
	scheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// Links are the text's relative link targets in order, each once, without
// fragment or query, at most 20; a target over 300 bytes is skipped.
func Links(text string) []string {
	var out []string
	for _, m := range link.FindAllStringSubmatch(text, -1) {
		t := m[1]
		if len(t) > maxTarget || scheme.MatchString(t) || strings.HasPrefix(t, "/") || strings.HasPrefix(t, "#") {
			continue
		}
		if i := strings.IndexAny(t, "#?"); i >= 0 {
			t = t[:i]
		}
		if t == "" || slices.Contains(out, t) {
			continue
		}
		if out = append(out, t); len(out) == maxLinks {
			break
		}
	}
	return out
}

// Resolve is the path of tree that is the link without its leading ../
// and ./, else the sorted paths that end in it.
func Resolve(link string, tree []string) []string {
	rel := path.Clean(link)
	for strings.HasPrefix(rel, "../") {
		rel = rel[len("../"):]
	}
	if slices.Contains(tree, rel) {
		return []string{rel}
	}
	var out []string
	for _, p := range tree {
		if strings.HasSuffix(p, "/"+rel) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// Lines says for each of the text's links where it is under root: one
// file, none, or how many with the first three named.
func Lines(text string, tree []string, root string) []string {
	var out []string
	for _, l := range Links(text) {
		switch m := Resolve(l, tree); len(m) {
		case 0:
			out = append(out, l+" is not among the text files at "+root)
		case 1:
			out = append(out, l+" is "+path.Join(root, m[0]))
		default:
			out = append(out, fmt.Sprintf("%s matches %d files: %s", l, len(m), strings.Join(m[:min(len(m), 3)], ", ")))
		}
	}
	return out
}
