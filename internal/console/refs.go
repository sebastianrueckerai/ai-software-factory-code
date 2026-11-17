package console

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"log"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// refToken is what linkRefs takes whole from a text: a fenced block, a
// code span, or a markdown link with its words and its target.
var refToken = regexp.MustCompile("(?s)```(.*?)(?:```|$)|`([^`]*)`|\\[([^\\]]*)\\]\\(\\s*([^\\s)]*)[^)]*\\)")

// linkRefs is text, a card's of repo, as HTML: each #N a link to its
// card's drawer, else to its pull request or issue in Forgejo, followed
// by the target's title and state in words; a link to a document of the
// project a link that opens it in the drawer at its heading, one to the
// web a link that opens a tab, any other link its words. Code stays as it
// is, and a number or a document that resolves to nothing stays text. A
// number the board of v holds costs no read, any other one read per
// render.
func (c *Console) linkRefs(ctx context.Context, v *boardView, repo, text string) template.HTML {
	r := v.refsOf(ctx, c, repo)
	failed := r.err
	var b strings.Builder
	at := 0
	for _, m := range refToken.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(c.issueLinks(v, r, text[at:m[0]]))
		at = m[1]
		switch {
		case m[2] >= 0:
			b.WriteString("<pre><code>" + template.HTMLEscapeString(text[m[2]:m[3]]) + "</code></pre>")
		case m[4] >= 0:
			b.WriteString("<code>" + template.HTMLEscapeString(text[m[4]:m[5]]) + "</code>")
		default:
			b.WriteString(docLink(r, text[m[6]:m[7]], text[m[8]:m[9]]))
		}
	}
	b.WriteString(c.issueLinks(v, r, text[at:]))
	if failed == nil && r.err != nil {
		log.Printf("console: the references of %s: %v", repo, r.err)
	}
	return template.HTML(b.String())
}

// refsOf is the render's resolver of repo's references, read as its
// viewer.
func (v *boardView) refsOf(ctx context.Context, c *Console, repo string) *cardRefs {
	if r := v.refs[repo]; r != nil {
		return r
	}
	s, _ := ctx.Value(ctxKey{}).(*session)
	r := &cardRefs{ctx: ctx, c: c, a: actor{s: s, repository: repo}, t: factoryd.Task{Repository: repo}}
	if v.refs == nil {
		v.refs = map[string]*cardRefs{}
	}
	v.refs[repo] = r
	return r
}

// issueLinks is text, which holds no code and no link, with each #N
// linked and named.
func (c *Console) issueLinks(v *boardView, r *cardRefs, text string) string {
	var b strings.Builder
	at := 0
	for _, m := range factoryd.RefPattern.FindAllStringSubmatchIndex(text, -1) {
		n, err := strconv.Atoi(text[m[4]:m[5]])
		if err != nil {
			continue
		}
		b.WriteString(template.HTMLEscapeString(text[at:m[2]]))
		b.WriteString(c.issueLink(v.issue(r, n), text[m[2]:m[3]], r.t.Repository, n))
		at = m[3]
	}
	b.WriteString(template.HTMLEscapeString(text[at:]))
	return b.String()
}

// issueLink is ref, #n of repo resolved as i: a card opens its drawer, a
// pull request or an issue its Forgejo page.
func (c *Console) issueLink(i resolvedIssue, ref, repo string, n int) string {
	if !i.ok {
		return template.HTMLEscapeString(ref)
	}
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + repo
	href, tab, state := "#card="+i.card.ID, "", i.state
	switch {
	case i.card.ID != "":
		state = stateWord(i.card)
	case i.pull:
		href, tab = fmt.Sprintf("%s/pulls/%d", base, n), ` target="_blank" rel="noopener"`
		if i.merged {
			state = "merged"
		} else if i.state == "closed" {
			state = "closed without a merge"
		}
	default:
		href, tab = fmt.Sprintf("%s/issues/%d", base, n), ` target="_blank" rel="noopener"`
	}
	return `<a class="ref-link" href="` + template.HTMLEscapeString(href) + `"` + tab + `>` + template.HTMLEscapeString(ref) + `</a> (` +
		template.HTMLEscapeString(i.title) + ", " + template.HTMLEscapeString(state) + ")"
}

// issue is #n of r's repository: a card the board holds first, the live
// one before a final one and the newest among either, as the store's
// TaskByIssue picks; else r's read. Once a read failed, a number the
// board lacks stays unresolved for the rest of the render.
func (v *boardView) issue(r *cardRefs, n int) resolvedIssue {
	var held []factoryd.Task
	for _, t := range v.byID {
		if t.Repository == r.t.Repository && t.IssueNumber == int64(n) {
			held = append(held, t)
		}
	}
	if len(held) > 0 {
		t := slices.MinFunc(held, func(a, b factoryd.Task) int {
			if fa, fb := factoryd.Final(a.Status), factoryd.Final(b.Status); fa != fb {
				if fa {
					return 1
				}
				return -1
			}
			return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID))
		})
		return resolvedIssue{title: factoryd.CardTitle(t), state: t.Status, ok: true, card: t}
	}
	if r.err != nil {
		return resolvedIssue{}
	}
	return r.resolve(n)
}

// stateWord is card t's state as the board says it: "needs you" while a
// human holds it, its stage while it runs, else its state.
func stateWord(t factoryd.Task) string {
	if fiveColumn(t) == colNeedsYou {
		return "needs you"
	}
	return cmp.Or(runKind(t.Status, t.Track), t.Status)
}

// docLink is the markdown link [words](target) of a card's text: a link
// to a document of the project's document repository opens it in the
// drawer at its heading, a link to the web opens a tab, and any other
// keeps its words.
func docLink(r *cardRefs, words, target string) string {
	words = template.HTMLEscapeString(words)
	if docExternal.MatchString(target) {
		return `<a href="` + template.HTMLEscapeString(target) + `" target="_blank" rel="noopener">` + words + `</a>`
	}
	p, frag, _ := strings.Cut(target, "#")
	p, _, _ = strings.Cut(p, "?")
	if !strings.HasSuffix(p, ".md") || r.err != nil {
		return words
	}
	files := r.Files(strings.TrimPrefix(p, "/"))
	if r.err != nil || r.docs == (factoryd.Repo{}) || len(files) != 1 {
		return words
	}
	return `<a class="doc-link" data-doc="` + template.HTMLEscapeString(files[0]) + `" href="` + template.HTMLEscapeString(docHref(files[0], frag)) + `">` + words + `</a>`
}

// termPlurals are the terms whose plural takes no s.
var termPlurals = map[string]string{"criterion": "criteria"}

// termPattern matches the terms of factoryd.Terms as whole words, in any
// case and in the plural, the longest first; termAt is the term a match
// of it found.
var termPattern, termAt = func() (*regexp.Regexp, func(m []int) string) {
	terms := slices.SortedFunc(maps.Keys(factoryd.Terms), func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	alts := make([]string, len(terms))
	for i, t := range terms {
		word := strings.ReplaceAll(regexp.QuoteMeta(t), " ", `\s+`)
		if p, ok := termPlurals[t]; ok {
			alts[i] = "(" + word + "|" + regexp.QuoteMeta(p) + ")"
		} else {
			alts[i] = "(" + word + "s?)"
		}
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`), func(m []int) string {
		for i := range terms {
			if m[2+2*i] >= 0 {
				return terms[i]
			}
		}
		return ""
	}
}()

// termSkip are the elements whose text explainTerms leaves as it is: a
// link, code, and its own marks and sentences.
var termSkip = map[string]bool{"a": true, "code": true, "pre": true, "dfn": true, "small": true}

// explainTerms is h with the first use of each factory term marked as a
// definition and its sentence after the text: a note a reader sees with
// no script and on a phone. seen holds the terms the face's earlier texts
// explained, and takes h's, so a face passes one map through its texts;
// nil explains h alone. A link, code and an earlier mark stay as they are,
// so explainTerms runs over linkRefs's output and over its own.
func explainTerms(h template.HTML, seen map[string]bool) template.HTML {
	if seen == nil {
		seen = map[string]bool{}
	}
	var b, notes strings.Builder
	skip, marked := 0, 0
	z := html.NewTokenizer(strings.NewReader(string(h)))
	for {
		switch tt := z.Next(); tt {
		case html.ErrorToken:
			return template.HTML(b.String() + notes.String())
		case html.TextToken:
			text := string(z.Text())
			if skip == 0 {
				b.WriteString(markTerms(text, seen, &notes))
				continue
			}
			if marked > 0 {
				for _, m := range termPattern.FindAllStringSubmatchIndex(text, -1) {
					seen[termAt(m)] = true
				}
			}
			b.WriteString(template.HTMLEscapeString(text))
		case html.StartTagToken, html.EndTagToken:
			b.Write(z.Raw())
			name, _ := z.TagName()
			if termSkip[string(name)] {
				d := 1
				if tt == html.EndTagToken {
					d = -1
				}
				skip = max(skip+d, 0)
				if string(name) == "dfn" {
					marked = max(marked+d, 0)
				}
			}
		default:
			b.Write(z.Raw())
		}
	}
}

// markTerms is text escaped, the first use of each term seen lacks marked
// and its sentence added to notes.
func markTerms(text string, seen map[string]bool, notes *strings.Builder) string {
	var b strings.Builder
	at := 0
	for _, m := range termPattern.FindAllStringSubmatchIndex(text, -1) {
		term := termAt(m)
		if seen[term] {
			continue
		}
		seen[term] = true
		b.WriteString(template.HTMLEscapeString(text[at:m[0]]) + `<dfn class="term">` + template.HTMLEscapeString(text[m[0]:m[1]]) + `</dfn>`)
		notes.WriteString(` <small class="gloss">` + template.HTMLEscapeString(factoryd.Terms[term]) + `</small>`)
		at = m[1]
	}
	b.WriteString(template.HTMLEscapeString(text[at:]))
	return b.String()
}
