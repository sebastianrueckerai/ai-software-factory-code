package console

// The document renderer (roadmap 277, the 2026-10-15 plan, item 1):
// goldmark renders CommonMark with raw HTML escaped and headings with ids,
// and the round's engine rules the links (design/rounds/05-documents,
// docs.js): an http(s) target opens a new tab; a relative target is
// resolved against the document's directory, and one that lies inside the
// roots opens in the drawer as a data-doc link, everything else renders
// as quiet text with a title.

import (
	"bytes"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// docExternal matches an http or https link target.
var docExternal = regexp.MustCompile(`^https?://`)

// docMissingTitle titles a link that is no document of the roots, the
// engine's "not in this prototype" as the console says it.
const docMissingTitle = "no document at that path"

// docResolve is target as a path against the directory of from, the
// engine's resolve: "" stays on the same file, "/" starts at the
// repository's root, "." and ".." walk as paths do; frag is the target's
// #fragment.
func docResolve(from, target string) (resolved, frag string) {
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target, frag = target[:i], target[i+1:]
	}
	switch {
	case target == "":
		return from, frag
	case strings.HasPrefix(target, "/"):
		return strings.TrimPrefix(path.Clean(target), "/"), frag
	default:
		return path.Join(path.Dir(from), target), frag
	}
}

// docRender is md, the document at path, as HTML.
func docRender(md, at string) (string, error) {
	var b bytes.Buffer
	err := goldmark.New(
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(&docLinks{doc: at}, 0))),
	).Convert([]byte(md), &b)
	return b.String(), err
}

// docHref is the drawer's href of the document path with its fragment:
// the path's segments escaped, the slashes left readable.
func docHref(docPath, frag string) string {
	segs := strings.Split(docPath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	href := "?doc=" + strings.Join(segs, "/")
	if frag != "" {
		href += "#" + frag
	}
	return href
}

// docLinks renders the links of the document at doc by the engine's
// rules, everything else as goldmark's safe renderer does.
type docLinks struct{ doc string }

func (l *docLinks) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindLink, l.anchor)
	reg.Register(ast.KindAutoLink, l.anchor)
}

// anchor writes one link's opening tag and its close: an external target
// with target="_blank", a relative one inside the roots as the drawer's
// data-doc link, and anything else as a quiet anchor with a title.
func (l *docLinks) anchor(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		_, _ = w.WriteString("</a>")
		return ast.WalkContinue, nil
	}
	var target string
	switch a := n.(type) {
	case *ast.Link:
		target = string(a.Destination)
	case *ast.AutoLink:
		target = string(a.URL(source))
	}
	switch {
	case docExternal.MatchString(target):
		_, _ = w.WriteString(`<a class="doc-link doc-ext" href="`)
		_, _ = w.Write(util.EscapeHTML([]byte(target)))
		_, _ = w.WriteString(`" target="_blank" rel="noopener">`)
	default:
		resolved, frag := docResolve(l.doc, target)
		if !docRootOK(resolved) {
			_, _ = w.WriteString(`<a class="doc-link doc-missing" title="` + docMissingTitle + `">`)
			return ast.WalkContinue, nil
		}
		_, _ = w.WriteString(`<a class="doc-link" data-doc="`)
		_, _ = w.Write(util.EscapeHTML([]byte(resolved)))
		_, _ = w.WriteString(`" href="`)
		_, _ = w.Write(util.EscapeHTML([]byte(docHref(resolved, frag))))
		_, _ = w.WriteString(`">`)
	}
	return ast.WalkContinue, nil
}
