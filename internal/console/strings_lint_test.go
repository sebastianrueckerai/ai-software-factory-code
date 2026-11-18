package console

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/facefixtures"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"golang.org/x/net/html"
)

// shownText is one text a human reads and where it stood.
type shownText struct{ Where, Text string }

// inline are the elements whose text reads as part of the line around
// them.
var inline = map[string]bool{"a": true, "span": true, "b": true, "i": true, "em": true, "strong": true, "code": true, "abbr": true, "time": true, "br": true}

// writerWords says whether n holds a writer's words, which the guide's
// checks of the factory's text do not hold: a quote (a span or a quoted
// block of markup), a writer's own words,
// a card's body, an exact text (a log, a file, code) and a field a human
// types into.
func writerWords(n *html.Node) bool {
	return hasClass(n, "quote") || hasClass(n, "own-words") || hasClass(n, "o-body") || slices.Contains([]string{"pre", "code", "textarea", "blockquote"}, n.Data)
}

// lineOf is the text of block n's own line: its text and its inline
// elements', a space between, its blocks and a writer's words left out.
func lineOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		for c := range m.ChildNodes() {
			switch {
			case c.Type == html.TextNode:
				b.WriteString(c.Data)
			case c.Type == html.ElementNode && inline[c.Data] && !writerWords(c):
				b.WriteString(" ")
				walk(c)
				b.WriteString(" ")
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// shownTexts are the texts of page fragment body a human reads outside a
// writer's words: each block's own line and the labels a reader hears.
func shownTexts(where, body string) []shownText {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return []shownText{{where, "the fragment does not parse: " + err.Error()}}
	}
	var out []shownText
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode || n.Data == "script" || n.Data == "style" || writerWords(n) || within(n, nil, writerWords) {
			continue
		}
		for _, a := range []string{"aria-label", "title", "placeholder", "alt"} {
			if v := htmlAttr(n, a); v != "" {
				out = append(out, shownText{where, v})
			}
		}
		if s := lineOf(n); s != "" && !inline[n.Data] {
			out = append(out, shownText{where, s})
		}
	}
	return out
}

// writerTexts are the texts the scene's writers wrote, which the faces
// show as written: each card's title, why line, summary and body, and
// each string of its plan's revisions.
func writerTexts(r *rig) []string {
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, bt := range r.tasks.tasks {
		out = append(out, factoryd.CardTitle(bt.Task), bt.Why, bt.Summary)
		if _, body, ok := strings.Cut(bt.Objective, "\n"); ok {
			for _, l := range strings.Split(body, "\n") {
				out = append(out, strings.TrimSpace(strings.TrimLeft(l, "-* ")))
			}
		}
		for _, rev := range r.tasks.plans[bt.ID] {
			var plan any
			_ = json.Unmarshal(rev.Plan, &plan)
			walk(plan)
		}
	}
	out = slices.DeleteFunc(out, func(s string) bool { return len(s) < 12 })
	// the longest first, so a title inside a longer text goes with it
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// pressedRefusals are the refusals of every answer form on opened body
// that a press with an invalid input gets: a typed field left empty, an
// amount or a number below zero, a choice none of the options, and a
// press with no field on an ask that is no longer open.
func pressedRefusals(r *rig, ck *http.Cookie, csrf, where, opened string) []shownText {
	doc, err := html.Parse(strings.NewReader(opened))
	if err != nil {
		return nil
	}
	var out []shownText
	for n := range doc.Descendants() {
		if n.Type != html.ElementNode || n.Data != "form" || !hasClass(n, "answer") {
			continue
		}
		v, fields := url.Values{}, false
		for f := range n.Descendants() {
			if f.Type != html.ElementNode {
				continue
			}
			name := htmlAttr(f, "name")
			switch {
			case f.Data == "input" && htmlAttr(f, "type") == "hidden":
				v.Set(name, htmlAttr(f, "value"))
			case f.Data == "input" && htmlAttr(f, "type") == "number":
				v.Set(name, "-1")
				fields = true
			case f.Data == "input" && htmlAttr(f, "type") == "text", f.Data == "textarea":
				v.Set(name, "")
				fields = true
			case f.Data == "select":
				v.Set(name, "none of these")
				fields = true
			}
		}
		if !fields && v.Has("case_id") {
			v.Set("case_id", "999999")
		}
		v.Set("csrf", csrf)
		rec := httptest.NewRecorder()
		func() {
			// a door the rig wires no fake for answers nothing
			defer func() { _ = recover() }()
			rec = r.do(htmxPost(htmlAttr(n, "action"), v), ck)
		}()
		if rec.Code >= 400 && rec.Code != http.StatusNotFound {
			out = append(out, shownText{where + " refusal of " + htmlAttr(n, "data-answer"), strings.TrimSpace(rec.Body.String())})
		}
	}
	return out
}

// galleryTexts are the texts a human reads of gallery entry e: the small
// card, the opened card, its life fold and the refusals its answers get on
// an invalid input; a routine's card and drawer; the status strip shut and
// open; the inbox's rows. A writer's text in them stands as "…".
func galleryTexts(t *testing.T, e galleryEntry) []shownText {
	r := newRig(t)
	r.fg.setPullHead(labPull, labPullHead)
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, bool) error { return nil }
	ck, s := r.loggedIn(t)
	target := e.Seat(r)
	name := e.Group + "/" + e.Kind
	get := func(path string) string {
		return r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
	}
	page := get("/?project=" + labCodeProject.Name)
	var texts []shownText
	switch {
	case strings.HasPrefix(target, galleryInbox+":"):
		var answer chatGetAnswer
		if err := json.Unmarshal([]byte(get(chatPath+"?conversation="+strings.TrimPrefix(target, galleryInbox+":"))), &answer); err != nil {
			t.Fatal(err)
		}
		for _, m := range answer.Messages {
			own := []string{m.Line, m.Short, m.Reason, m.Decider}
			if m.Kind != "" {
				own = append(append(own, m.Content), m.Choices...)
			}
			for _, s := range own {
				if s != "" {
					texts = append(texts, shownText{name + " inbox", s})
				}
			}
		}
	case target == galleryStatus:
		shut, open, err := statusFaces(page)
		if err != nil {
			t.Fatal(err)
		}
		texts = shownTexts(name+" status", string(shut)+string(open))
	case strings.HasPrefix(target, galleryRoutine):
		routine := strings.TrimPrefix(target, galleryRoutine)
		small, err := boardItems(page, "data-ghost", routine)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(shownTexts(name+" small", string(small)), shownTexts(name+" opened", get("/routines/"+routine+"/drawer"))...)
	default:
		small, err := boardItems(page, "data-card", target)
		if err != nil {
			t.Fatal(err)
		}
		opened := get("/cards/" + target + "/drawer?tab=overview")
		texts = append(shownTexts(name+" small", string(small)), shownTexts(name+" opened", opened)...)
		texts = append(texts, shownTexts(name+" life", get("/tasks/"+target+"/digest"))...)
		texts = append(texts, pressedRefusals(r, ck, s.csrf, name, opened)...)
	}
	writers := writerTexts(r)
	for i, st := range texts {
		for _, w := range writers {
			st.Text = strings.ReplaceAll(st.Text, w, "…")
		}
		texts[i] = st
	}
	return texts
}

// sourceFiles are the files a face's text comes from: the console's
// templates and the Go files of the console, factoryd and the policy.
func sourceFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, glob := range []string{"templates/*.html", "*.go", "../factoryd/*.go", "../policy/*.go"} {
		names, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if strings.HasSuffix(n, "_test.go") {
				continue
			}
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			out[n] = string(b)
		}
	}
	return out
}

// sourceOf is the file that holds the longest run of s's words, "" when
// none holds three in a row.
func sourceOf(files map[string]string, s string) string {
	words := strings.Fields(s)
	best, at := 2, ""
	names := slices.Sorted(maps.Keys(files))
	for i := range words {
		for j := i + best + 1; j <= len(words); j++ {
			run := strings.Join(words[i:j], " ")
			found := ""
			for _, name := range names {
				if strings.Contains(files[name], run) {
					found = name
					break
				}
			}
			if found == "" {
				break
			}
			best, at = j-i, found
		}
	}
	return at
}

// Every text a human reads on the gallery's faces passes the guide's
// checks and the face's (facefixtures.TextFaults): the small card, the
// opened card, the card's life fold, the refusal each answer gets on an
// invalid input, a routine's card and drawer, the status strip and the
// inbox's rows. A writer's words pass as written. A failing text prints
// with the file that holds it and the faces it stands on.
func TestRenderedTextPassesLint(t *testing.T) {
	type fault struct {
		where []string
		lines []string
	}
	faults := map[string]*fault{}
	for _, e := range galleryEntries() {
		for _, st := range galleryTexts(t, e) {
			if f, ok := faults[st.Text]; ok {
				f.where = append(f.where, st.Where)
			} else if lines := facefixtures.TextFaults(st.Text); len(lines) > 0 {
				faults[st.Text] = &fault{[]string{st.Where}, lines}
			}
		}
	}
	files := sourceFiles(t)
	for _, text := range slices.Sorted(maps.Keys(faults)) {
		f := faults[text]
		where := f.where[0]
		if len(f.where) > 1 {
			where += fmt.Sprintf(" and %d more", len(f.where)-1)
		}
		t.Errorf("%q\n\tin %s, on %s\n\t%s", text, sourceOf(files, text), where, strings.Join(f.lines, " "))
	}
}

// doorCalls are the calls whose text a human or an agent reads, by the
// package they stand in, with the index of that text among their
// arguments: the refusals of the console's presses and chat tools, of Act
// and of the gateway's tools, the comments the factory posts on issues and
// pull requests, and the bodies of the console's errors.
var doorCalls = map[string]map[string]int{
	"console":  {"refuse": 1, "refuseAgent": 1, "Error": 1},
	"factoryd": {"ErrActRefused": 0, "agentRefused": 0, "CreateIssueComment": 4, "CommentOnce": 4, "comment": 4, "commentOnce": 4, "closeIssueAsFactory": 4, "closeAsk": 2},
	"gateway":  {"refuse": 5, "refuseTool": 3, "toolError": 0, "refusedArgs": 0, "refuseArgs": 0},
}

var (
	// writtenCalls are the calls of doorCalls whose text a human reads as
	// the code writes it: a comment on Forgejo and an error's body
	writtenCalls = []string{"CreateIssueComment", "CommentOnce", "comment", "commentOnce", "closeIssueAsFactory", "closeAsk", "Error"}
	// agentDoors are the calls of doorCalls an agent alone meets
	agentDoors = []string{"refuseAgent", "agentRefused"}
	// agentTypes are the types of the chat's tools: code that takes one
	// answers the chat's agent
	agentTypes = []string{"turn", "toolInput"}
)

// doorFaults are the faults of text by who reads it: a text a human reads
// as written (facefixtures.HumanFaults), a refusal an agent alone reads,
// with the keys it types (facefixtures.TextFaults), and any other refusal
// as the human's door renders it (factoryd.ForHuman), where a value in a
// code span is an answer's key, which reads as its label. The gateway's tools,
// the chat's tools and agentDoors answer an agent alone.
func doorFaults(pkg, fn string, agent bool, text string) []string {
	switch {
	case slices.Contains(writtenCalls, fn):
		return facefixtures.HumanFaults(text)
	case pkg == "gateway" || agent || slices.Contains(agentDoors, fn):
		return facefixtures.TextFaults(text)
	}
	return facefixtures.HumanFaults(factoryd.ForHuman(strings.ReplaceAll(text, "`X`", "“X”")))
}

// takesAgentType says whether function type ft, with receiver recv, takes
// one of agentTypes.
func takesAgentType(recv *ast.FieldList, ft *ast.FuncType) bool {
	for _, fl := range []*ast.FieldList{recv, ft.Params} {
		if fl == nil {
			continue
		}
		for _, f := range fl.List {
			typ := f.Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}
			if id, ok := typ.(*ast.Ident); ok && slices.Contains(agentTypes, id.Name) {
				return true
			}
		}
	}
	return false
}

// formatVerb is a verb of a format string, which a value fills.
var formatVerb = regexp.MustCompile(`%[-+# 0-9.*]*[a-zA-Z]`)

// literalText is the words expression e writes that the code holds: a
// string literal, the literals of a concatenation and a format's text, a
// value standing as "X"; false when e holds no literal.
func literalText(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if s, err := strconv.Unquote(x.Value); err == nil && x.Kind == token.STRING {
			return s, true
		}
	case *ast.ParenExpr:
		return literalText(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			a, okA := literalText(x.X)
			b, okB := literalText(x.Y)
			if !okA {
				a = "X"
			}
			if !okB {
				// a value glued to a word carries its own space
				b = "X"
				if a != "" && unicode.IsLetter(rune(a[len(a)-1])) {
					b = " X"
				}
			}
			return a + b, okA || okB
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && slices.Contains([]string{"Sprintf", "Errorf"}, sel.Sel.Name) && len(x.Args) > 0 {
			s, ok := literalText(x.Args[0])
			return formatVerb.ReplaceAllString(s, "X"), ok
		}
	}
	return "", false
}

// Every refusal a door gives and every comment the factory posts passes
// the guide's checks and the face's in the words the code holds, as its
// reader reads it (doorFaults): each call of doorCalls, its literal text
// with each value as "X".
func TestEveryRefusalAndCommentPassesTheGuide(t *testing.T) {
	for pkg, calls := range doorCalls {
		names, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			fs := token.NewFileSet()
			file, err := parser.ParseFile(fs, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			// agent counts the functions around the node that answer an agent
			var stack []ast.Node
			agent := 0
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil {
					top := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if fd, ok := top.(*ast.FuncDecl); ok && takesAgentType(fd.Recv, fd.Type) {
						agent--
					} else if fl, ok := top.(*ast.FuncLit); ok && takesAgentType(nil, fl.Type) {
						agent--
					}
					return true
				}
				stack = append(stack, n)
				if fd, ok := n.(*ast.FuncDecl); ok && takesAgentType(fd.Recv, fd.Type) {
					agent++
				} else if fl, ok := n.(*ast.FuncLit); ok && takesAgentType(nil, fl.Type) {
					agent++
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn := ""
				switch f := call.Fun.(type) {
				case *ast.Ident:
					fn = f.Name
				case *ast.SelectorExpr:
					fn = f.Sel.Name
				}
				i, ok := calls[fn]
				if !ok || i >= len(call.Args) {
					return true
				}
				if text, ok := literalText(call.Args[i]); ok {
					if faults := doorFaults(pkg, fn, agent > 0, text); len(faults) > 0 {
						t.Errorf("%s: %q\n\t%s", fs.Position(call.Pos()), text, strings.Join(faults, " "))
					}
				}
				return true
			})
		}
	}
}
