// Package doclinks resolves the relative markdown links of a card's text
// against the files of the project's document repository. The text's own
// location is unknown, so a link matches every file whose path ends in the
// link with its leading ../ and ./ removed.
package doclinks

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const maxLinks = 20

var (
	link   = regexp.MustCompile(`\]\(\s*([^\s)]+)[^)]*\)`)
	scheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// Links are the text's relative link targets in order, each once, without
// fragment or query, at most 20.
func Links(text string) []string {
	var out []string
	for _, m := range link.FindAllStringSubmatch(text, -1) {
		t := m[1]
		if scheme.MatchString(t) || strings.HasPrefix(t, "/") || strings.HasPrefix(t, "#") {
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

// Resolve is the sorted paths of tree that are the link, or end in it,
// without its leading ../ and ./.
func Resolve(link string, tree []string) []string {
	rel := path.Clean(link)
	for strings.HasPrefix(rel, "../") {
		rel = rel[len("../"):]
	}
	var out []string
	for _, p := range tree {
		if p == rel || strings.HasSuffix(p, "/"+rel) {
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
			out = append(out, l+" is not in the document repository")
		case 1:
			out = append(out, l+" is "+path.Join(root, m[0]))
		default:
			out = append(out, fmt.Sprintf("%s matches %d files: %s", l, len(m), strings.Join(m[:min(len(m), 3)], ", ")))
		}
	}
	return out
}
