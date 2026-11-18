package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// refToken is what linkRefs takes whole from a text, by the patterns the
// card's door reads it with: a fenced block, a code span, or a markdown
// link with its words and its target.
var refToken = regexp.MustCompile("(?:" + factoryd.FencedBlock.String() + ")|(?:" + factoryd.CodeSpan.String() + ")|(?:" + factoryd.MDLink.String() + ")")

// linkRefs is text, a card's of repo, as HTML: each #N a link to its
// card's drawer, else to its pull request or issue in Forgejo, followed
// by the target's title and state in words; a link to a document of the
// project a link that opens it in the drawer at its heading, one to the
// web a link that opens a tab, any other link its words. Code stays as it
// is, and a number or a document that resolves to nothing stays text. A
// number the board of v holds costs no read, any other one read per
// render. A card's own text (title, why line, summary, body), which
// agents and humans write about the product, takes linkRefs alone;
// factoryText is the factory's.
func (c *Console) linkRefs(ctx context.Context, v *boardView, repo, text string) template.HTML {
	r := v.refsOf(ctx, c, repo)
	var b strings.Builder
	at := 0
	for _, m := range refToken.FindAllStringSubmatchIndex(text, -1) {
		b.WriteString(c.issueLinks(v, r.issues, text[at:m[0]]))
		at = m[1]
		switch tok := text[m[0]:m[1]]; {
		case strings.HasPrefix(tok, "```"):
			b.WriteString("<pre><code>" + template.HTMLEscapeString(strings.TrimSuffix(tok[3:], "```")) + "</code></pre>")
		case strings.HasPrefix(tok, "`"):
			b.WriteString("<code>" + template.HTMLEscapeString(tok[1:len(tok)-1]) + "</code>")
		default:
			b.WriteString(c.docLink(r.docs, text[m[2]:m[3]], text[m[4]:m[5]]))
		}
	}
	b.WriteString(c.issueLinks(v, r.issues, text[at:]))
	return template.HTML(b.String())
}

// linkMarkup is raw, markdown of repo, as markup renders it, with each #N
// of its prose linked and named as linkRefs links it: the live title and
// state of the target. Code and links stay as they are.
func (c *Console) linkMarkup(ctx context.Context, v *boardView, repo, raw string) template.HTML {
	r := v.refsOf(ctx, c, repo)
	var b strings.Builder
	keep := 0
	z := html.NewTokenizer(strings.NewReader(string(markup(raw))))
	for {
		tt := z.Next()
		name, _ := z.TagName()
		kept := slices.Contains([]string{"a", "code", "pre"}, string(name))
		switch {
		case tt == html.ErrorToken:
			return template.HTML(b.String()) // #nosec G203 -- markup's HTML, its prose escaped by issueLinks
		case tt == html.TextToken && keep == 0 && factoryd.RefPattern.Match(z.Raw()):
			b.WriteString(c.issueLinks(v, r.issues, string(z.Text())))
			continue
		case tt == html.StartTagToken && kept:
			keep++
		case tt == html.EndTagToken && kept && keep > 0:
			keep--
		}
		b.Write(z.Raw())
	}
}

// htmlText is the text h shows.
func htmlText(h template.HTML) string {
	var b strings.Builder
	z := html.NewTokenizer(strings.NewReader(string(h)))
	for tt := z.Next(); tt != html.ErrorToken; tt = z.Next() {
		if tt == html.TextToken {
			b.Write(z.Text())
		}
	}
	return b.String()
}

// factoryText is text the factory composes for a card of repo (an ask, a
// story line, a state, a waits clause, an effect line) as HTML: its
// references linked, and the first use of each factory term on the face
// marked into seen.
func (c *Console) factoryText(ctx context.Context, v *boardView, repo, text string, seen map[string]int) template.HTML {
	return explainTerms(c.linkRefs(ctx, v, repo, text), seen)
}

// renderRefs are a render's resolvers of one repository's references,
// read as its viewer: issues for the numbers, docs for the document
// links, apart so that a fault of the documents leaves the numbers
// resolving and a render treats documents it cannot reach as none.
type renderRefs struct{ issues, docs *cardRefs }

func (v *boardView) refsOf(ctx context.Context, c *Console, repo string) renderRefs {
	if r, ok := v.refs[repo]; ok {
		return r
	}
	s, _ := ctx.Value(ctxKey{}).(*session)
	one := func() *cardRefs {
		return &cardRefs{ctx: ctx, c: c, a: actor{s: s, repository: repo}, t: factoryd.Task{Repository: repo}}
	}
	r := renderRefs{issues: one(), docs: one()}
	if v.refs == nil {
		v.refs = map[string]renderRefs{}
	}
	v.refs[repo] = r
	return r
}

// forgeRef is a CI run, a commit or a file of the factory's configuration
// named in words, as the factory's texts name them: "CI run 3384", "commit
// 3c9a1e0", "the factory's file policy.yaml", "the project's file
// projects/demo.yaml". A hash holds a digit, which keeps a word of the
// letters a to f a word: "commit defaced".
var forgeRef = regexp.MustCompile(`\bCI run (\d+)\b|\b[Cc]ommit ([0-9a-f]{7,40})\b|\bthe (?:factory|project)'s file (policy\.yaml|` + factoryd.ProjectFilePattern + `)`)

// issueLinks is text, which holds no code and no link, with each #N
// linked and named, each CI run and commit named in words linked to its
// page of r's repository on the forge, and each file of the factory's
// configuration to its page there, where the console knows the address.
func (c *Console) issueLinks(v *boardView, r *cardRefs, text string) string {
	base := ""
	if c.cfg.ForgejoPublicURL != "" && r.t.Repository != "" {
		base = strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + r.t.Repository
	}
	var b strings.Builder
	at := 0
	for _, m := range forgeRef.FindAllStringSubmatchIndex(text, -1) {
		from, href := m[0], ""
		switch {
		case m[6] >= 0:
			if from = m[6]; c.cfg.ConfigRepo != "" {
				href = strings.TrimSuffix(c.cfg.ConfigRepo, "/") + "/src/branch/main/" + c.cfg.ConfigDir + "/" + text[m[6]:m[7]]
			}
		case base == "":
		case m[2] >= 0:
			href = base + "/actions/runs/" + text[m[2]:m[3]]
		case strings.ContainsAny(text[m[4]:m[5]], "0123456789"):
			href = base + "/commit/" + v.fullHash(text[m[4]:m[5]])
		}
		if href == "" {
			continue
		}
		b.WriteString(c.numberLinks(v, r, text[at:from]))
		b.WriteString(`<a class="ref-link" href="` + template.HTMLEscapeString(href) + `" target="_blank" rel="noopener">` + template.HTMLEscapeString(text[from:m[1]]) + `</a>`)
		at = m[1]
	}
	b.WriteString(c.numberLinks(v, r, text[at:]))
	return b.String()
}

// fullHash is the whole hash of the commit a text names by short, where a
// card, a case or a pending request of the view holds it; else short.
func (v *boardView) fullHash(short string) string {
	whole := func(sha string) bool { return len(sha) == 40 && strings.HasPrefix(sha, short) }
	for _, t := range v.byID {
		for _, sha := range []string{t.SHA, t.MergeCommit} {
			if whole(sha) {
				return sha
			}
		}
	}
	for _, cases := range v.cases.byCard {
		for _, c := range cases {
			for _, fact := range c.Facts {
				if sha, _ := fact.(string); whole(sha) {
					return sha
				}
			}
		}
	}
	for _, a := range v.pending {
		var spec struct {
			Head string `json:"head_sha"`
		}
		if json.Unmarshal(a.Spec, &spec) == nil && whole(spec.Head) {
			return spec.Head
		}
	}
	return short
}

// numberLinks is text with each #N linked and named.
func (c *Console) numberLinks(v *boardView, r *cardRefs, text string) string {
	var b strings.Builder
	at := 0
	for _, m := range factoryd.RefPattern.FindAllStringSubmatchIndex(text, -1) {
		n, err := strconv.Atoi(text[m[4]:m[5]])
		if err != nil {
			continue
		}
		b.WriteString(template.HTMLEscapeString(text[at:m[2]]))
		b.WriteString(c.issueLink(v, r, c.issue(v, r, n), text[m[2]:m[3]], n))
		at = m[3]
	}
	b.WriteString(template.HTMLEscapeString(text[at:]))
	return b.String()
}

// issueLink is ref, #n of r's repository resolved as i: a card opens its
// drawer, a pull request or an issue its Forgejo page. The target's title
// and state stand in a span explainTerms passes over, so a product's title
// never reads as a factory term.
func (c *Console) issueLink(v *boardView, r *cardRefs, i resolvedIssue, ref string, n int) string {
	if !i.ok {
		return template.HTMLEscapeString(ref)
	}
	base := strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/") + "/" + r.t.Repository
	href, tab, state := "#card="+i.card.ID, "", i.state
	switch {
	case i.card.ID != "":
		state = c.stateWord(r.ctx, v, i.card)
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
	link := `<a class="ref-link" href="` + template.HTMLEscapeString(href) + `"` + tab + `>` + template.HTMLEscapeString(ref) + `</a>`
	of := strings.Join(slices.DeleteFunc([]string{i.title, state}, func(s string) bool { return s == "" }), ", ")
	if of == "" {
		return link
	}
	return link + ` <span class="ref-of">(` + template.HTMLEscapeString(of) + ")</span>"
}

// issue is #n of r's repository: a card the board of v holds first, as
// the store's TaskByIssue picks among them, else r's read. Once a read
// failed, a number the board lacks stays unresolved for the rest of the
// render, and the fault is logged.
func (c *Console) issue(v *boardView, r *cardRefs, n int) resolvedIssue {
	var held []factoryd.Task
	for _, t := range v.byID {
		if t.Repository == r.t.Repository && t.IssueNumber == int64(n) {
			held = append(held, t)
		}
	}
	if len(held) > 0 {
		t := slices.MinFunc(held, factoryd.IssueCardOrder)
		return resolvedIssue{title: factoryd.CardTitle(t), state: t.Status, ok: true, card: t}
	}
	if r.err != nil {
		return resolvedIssue{}
	}
	i := r.resolve(n)
	if r.err != nil {
		c.logFault("references "+r.t.Repository, "console: the references of %s: %v", r.t.Repository, r.err)
	}
	return i
}

// stateWord is card t's state as the board says it: "needs you" while a
// human holds it, its stage while it runs, a plan that is done planning
// how many of its cards are done while some are not, else its state.
func (c *Console) stateWord(ctx context.Context, v *boardView, t factoryd.Task) string {
	if fiveColumn(t) == colNeedsYou {
		return "needs you"
	}
	if words, _ := c.planState(ctx, v, t); words != "" {
		return words
	}
	return cmp.Or(runKind(t.Status, t.Track), t.Status)
}

// planState is plan t's cards as its state reads them, read once a
// render: "3 of 8 cards done" while not all of them are delivered (done
// or merged, as the plan's close counts them), and open while one of
// them has not ended; "its state could not be read" and open when the
// plan or its cards could not be read; none for a plan that is not done
// planning.
func (c *Console) planState(ctx context.Context, v *boardView, t factoryd.Task) (words string, open bool) {
	if t.Track != policy.TrackPlan || t.Status != "done" {
		return "", false
	}
	if r, ok := v.rollups[t.ID]; ok {
		return r.words, r.open
	}
	var r rollup
	p, ok, err := v.planOf(ctx, c, t.ID)
	if ok {
		var cards []factoryd.CardRef
		if cards, err = factoryd.PlanCardRefs(ctx, c.cfg.Tasks, t, p); err != nil {
			log.Printf("console: the cards of plan %s: %v", t.ID, err)
			v.problems = append(v.problems, "the cards of plan "+t.ID+" could not be read")
		}
		done := 0
		for _, k := range cards {
			switch {
			case k.State == "done" || k.State == "merged":
				done++
			case !factoryd.Final(k.State):
				r.open = true
			}
		}
		if n := len(cards); done < n {
			r.words = fmt.Sprintf("%d of %d cards done", done, n)
		}
	}
	if err != nil {
		r = rollup{words: "its state could not be read", open: true}
	}
	if v.rollups == nil {
		v.rollups = map[string]rollup{}
	}
	v.rollups[t.ID] = r
	return r.words, r.open
}

// rollup is a done plan's state: its words and whether a card is open.
type rollup struct {
	words string
	open  bool
}

// docLink is the markdown link [words](target) of a card's text, r the
// render's resolver of its documents: a link to a document of the
// project's document repository opens it in the drawer at its heading, a
// link to the web opens a tab, and any other keeps its words, as does
// every document link once the documents could not be read.
func (c *Console) docLink(r *cardRefs, words, target string) string {
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
	if r.err != nil {
		c.logFault("documents "+r.t.Repository, "console: the documents of %s: %v", r.t.Repository, r.err)
		return words
	}
	if r.docs == (factoryd.Repo{}) || len(files) != 1 {
		return words
	}
	return `<a class="doc-link" data-doc="` + template.HTMLEscapeString(files[0]) + `" href="` + template.HTMLEscapeString(docHref(files[0], frag)) + `">` + words + `</a>`
}

// logFault logs a fault of key at most once a minute: the board renders
// on every event, and a lasting fault would fill the log.
func (c *Console) logFault(key, format string, args ...any) {
	c.mu.Lock()
	now := time.Now()
	logs := now.Sub(c.faults[key]) >= time.Minute
	if logs {
		if c.faults == nil {
			c.faults = map[string]time.Time{}
		}
		c.faults[key] = now
	}
	c.mu.Unlock()
	if logs {
		log.Printf(format, args...)
	}
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

// The elements explainTerms reads: one whose text it marks, one it leaves
// as it is (a link, code, a reference's title and state), and its own
// mark of an earlier pass.
const (
	termRead = iota
	termSkip
	termMark
)

func termElement(name, class string) int {
	switch {
	case name == "span" && class == "term":
		return termMark
	case name == "a" || name == "code" || name == "pre" || name == "span" && class == "ref-of":
		return termSkip
	}
	return termRead
}

// voidTags are the elements without an end tag.
var voidTags = map[string]bool{"br": true, "hr": true, "img": true, "wbr": true}

// explainTerms is h, text the factory composed (an ask, a story line, a
// state, a waits clause, an effect line), with the first use of each
// factory term on the face marked as a quiet span. seen holds the terms
// the face's earlier texts used, each with its place in the order of
// first use, and takes h's: a face passes one map through its texts, and
// glosses(seen) gives their sentences for the opened card's one fold. The
// small card runs none. It never runs over a card's title, why line,
// summary or body: those speak of the product, where "session" or
// "gateway" means the product's own thing, and whole words cannot tell
// the two apart. A link, code, a mark and a reference's title and state
// stay as they are, so it runs over linkRefs's output and over its own.
func explainTerms(h template.HTML, seen map[string]int) template.HTML {
	if seen == nil {
		seen = map[string]int{}
	}
	var b strings.Builder
	var open []int
	z := html.NewTokenizer(strings.NewReader(string(h)))
	for {
		switch tt := z.Next(); tt {
		case html.ErrorToken:
			return template.HTML(b.String())
		case html.TextToken:
			text := string(z.Text())
			switch {
			case !slices.ContainsFunc(open, func(e int) bool { return e != termRead }):
				b.WriteString(markTerms(text, seen))
				continue
			case open[len(open)-1] == termMark:
				for _, m := range termPattern.FindAllStringSubmatchIndex(text, -1) {
					use(seen, termAt(m))
				}
			}
			b.WriteString(template.HTMLEscapeString(text))
		case html.StartTagToken:
			b.Write(z.Raw())
			name, more := z.TagName()
			if voidTags[string(name)] {
				continue
			}
			var class string
			for more {
				var k, v []byte
				k, v, more = z.TagAttr()
				if string(k) == "class" {
					class = string(v)
				}
			}
			open = append(open, termElement(string(name), class))
		case html.EndTagToken:
			b.Write(z.Raw())
			if len(open) > 0 {
				open = open[:len(open)-1]
			}
		default:
			b.Write(z.Raw())
		}
	}
}

// use records term in seen at its place in the order of first use.
func use(seen map[string]int, term string) bool {
	if seen[term] > 0 {
		return false
	}
	seen[term] = len(seen) + 1
	return true
}

// markTerms is text escaped, the first use of each term seen lacks
// marked and recorded.
func markTerms(text string, seen map[string]int) string {
	var b strings.Builder
	at := 0
	for _, m := range termPattern.FindAllStringSubmatchIndex(text, -1) {
		if !use(seen, termAt(m)) {
			continue
		}
		b.WriteString(template.HTMLEscapeString(text[at:m[0]]) + `<span class="term">` + template.HTMLEscapeString(text[m[0]:m[1]]) + `</span>`)
		at = m[1]
	}
	b.WriteString(template.HTMLEscapeString(text[at:]))
	return b.String()
}

// gloss is one factory term a face used and its sentence.
type gloss struct{ Term, Sentence string }

// glosses are the terms of seen with their sentences, in the order of
// first use: the opened card's fold "What these words mean".
func glosses(seen map[string]int) []gloss {
	terms := slices.SortedFunc(maps.Keys(seen), func(a, b string) int { return seen[a] - seen[b] })
	out := make([]gloss, len(terms))
	for i, t := range terms {
		out[i] = gloss{t, factoryd.Terms[t]}
	}
	return out
}
