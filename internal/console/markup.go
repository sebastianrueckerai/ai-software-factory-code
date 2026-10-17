package console

// The question renderer (needs-you round): the drawer renders the
// writer's markup — headings as headings, lists as lists, code as
// monospace blocks — with goldmark's defaults: raw HTML is dropped, and
// the html writer keeps javascript:, vbscript:, file: and data: targets
// off links. A card's face never renders markup; it shows one ask line
// (askline) and the buttons.

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// md is the question's CommonMark engine at its defaults.
var md = goldmark.New()

// markup is the writer's text as HTML. On an error it returns the text
// escaped, so a broken document never loses its words.
func markup(text string) template.HTML {
	src := []byte(text)
	doc := md.Parser().Parse(text.NewReader(src))
	// a question carries no links but a reader's: keep a target only
	// when it names a page the member could actually visit
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := n.(*ast.Link); ok && entering && !markupSafeTarget(string(link.Destination)) {
			link.Destination = nil
		}
		return ast.WalkContinue, nil
	})
	var b bytes.Buffer
	if err := md.Renderer().Render(&b, src, doc); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(b.String())
}

// markupSafeTarget is a link target a question may carry: none, a
// fragment, a path on the console or the forge, http or https, mailto.
func markupSafeTarget(target string) bool {
	switch {
	case target == "", target[0] == '#', target[0] == '/',
		strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"),
		strings.HasPrefix(target, "mailto:"):
		return true
	}
	return !strings.Contains(target, ":") // a bare relative path
}

// askline is a text's first line: what a card's face shows of its
// question, so the board stays small.
func askline(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}
