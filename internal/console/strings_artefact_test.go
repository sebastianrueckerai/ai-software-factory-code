package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
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

// writerWords says whether n holds a writer's words, which the checks of
// the factory's text do not hold: a quote (a span or a quoted
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
	r.c.cfg.CheckClosePlan = func(context.Context, string, string, string, bool) error { return nil }
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

// No text a human reads on the gallery's faces shows a machine artefact
// (facefixtures.TextFaults): the small card, the
// opened card, the card's life fold, the refusal each answer gets on an
// invalid input, a routine's card and drawer, the status strip and the
// inbox's rows. A writer's words pass as written. A failing text prints
// with the file that holds it and the faces it stands on.
func TestRenderedTextShowsNoMachineArtefact(t *testing.T) {
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
	// factorydPath is the import path of package factoryd
	factorydPath = reflect.TypeFor[factoryd.Task]().PkgPath()
	// exactCalls are the functions that mark their text as exact or as a
	// writer's words, in a code fence or as a quote of markup, which the
	// checks pass as written: each by its package's path and its
	// name, so a function of the same name elsewhere marks nothing
	exactCalls = []string{factorydPath + ".fenced", factorydPath + ".Quoted"}
	// agentTypes are the types of the chat's tools: code that takes one
	// answers the chat's agent
	agentTypes = []string{"turn", "toolInput"}
)

// doorFaults are the faults of text by who reads it: a text a human reads
// as written (facefixtures.HumanFaults), a refusal an agent alone reads,
// with the keys it types (facefixtures.TextFaults), and any other refusal
// as the human's door renders it (factoryd.ForHuman), where a value in a
// code span is an answer's key, which reads as its label. A value in a code
// span of a written text is exact text, a command or a name, and no key
// the code holds. The gateway's tools,
// the chat's tools and agentDoors answer an agent alone.
func doorFaults(pkg, fn string, agent bool, text string) []string {
	switch {
	case slices.Contains(writtenCalls, fn):
		return facefixtures.HumanFaults(strings.ReplaceAll(text, "`X`", "“X”"))
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

// filling are the values that fill the words literalText reads of e: the
// parts of a concatenation that are no literal and the values of a format.
func filling(e ast.Expr) []ast.Expr {
	switch x := e.(type) {
	case *ast.BasicLit:
		return nil
	case *ast.ParenExpr:
		return filling(x.X)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return append(filling(x.X), filling(x.Y)...)
		}
	case *ast.CallExpr:
		if _, ok := literalText(x); ok {
			return x.Args[1:]
		}
	}
	return []ast.Expr{e}
}

// scoped is an expression as a door's text reaches it: i is the result it
// stands for where it is a call of several, and env binds the parameters
// of the function it stands in to the arguments of the call that led
// there.
type scoped struct {
	e   ast.Expr
	i   int
	env map[types.Object]scoped
}

// doorSource is the typed code of one package, read for the texts behind
// a door's argument: sets holds what each variable and each struct field
// is set to, decls the package's functions.
type doorSource struct {
	info  *types.Info
	sets  map[types.Object][]scoped
	decls map[types.Object]*ast.FuncDecl
}

// readDoorSource checks files, one package's code, against the packages
// imp finds, and reads its assignments.
func readDoorSource(fs *token.FileSet, pkgPath string, files []*ast.File, imp types.Importer) *doorSource {
	p := &doorSource{info: &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}},
		sets: map[types.Object][]scoped{}, decls: map[types.Object]*ast.FuncDecl{}}
	// a package that fails a check still gives the types of what passed
	conf := types.Config{Importer: imp, Error: func(error) {}}
	_, _ = conf.Check(pkgPath, fs, files, p.info)
	set := func(l ast.Expr, v ast.Expr, i int) {
		if obj := p.object(l); obj != nil {
			p.sets[obj] = append(p.sets[obj], scoped{e: v, i: i})
		}
	}
	each := func(lhs, rhs []ast.Expr) {
		for i, l := range lhs {
			switch {
			case len(lhs) == len(rhs):
				set(l, rhs[i], 0)
			case len(rhs) == 1:
				set(l, rhs[0], i)
			}
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				p.decls[p.info.Defs[x.Name]] = x
			case *ast.AssignStmt:
				each(x.Lhs, x.Rhs)
			case *ast.ValueSpec:
				var names []ast.Expr
				for _, id := range x.Names {
					names = append(names, id)
				}
				each(names, x.Values)
			case *ast.CompositeLit:
				var st *types.Struct
				if typ := p.info.TypeOf(x); typ != nil {
					st, _ = typ.Underlying().(*types.Struct)
				}
				for i, elt := range x.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						set(kv.Key, kv.Value, 0)
					} else if st != nil && i < st.NumFields() {
						p.sets[st.Field(i)] = append(p.sets[st.Field(i)], scoped{e: elt})
					}
				}
			}
			return true
		})
	}
	return p
}

// object is the variable, field or function e names, nil for none.
func (p *doorSource) object(e ast.Expr) types.Object {
	switch x := ast.Unparen(e).(type) {
	case *ast.Ident:
		return cmp.Or(p.info.Uses[x], p.info.Defs[x])
	case *ast.SelectorExpr:
		return p.info.Uses[x.Sel]
	}
	return nil
}

// textual says whether e can hold a text: a string, a list of strings, or
// an expression the check gave no type.
func (p *doorSource) textual(e ast.Expr) bool {
	typ := p.info.TypeOf(e)
	if typ == nil {
		return true
	}
	if list, ok := typ.Underlying().(*types.Slice); ok {
		typ = list.Elem()
	}
	basic, ok := typ.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// returned is what the function of type ft and body returns as result i,
// called with args.
func (p *doorSource) returned(ft *ast.FuncType, body *ast.BlockStmt, args []scoped, i int) []scoped {
	env := map[types.Object]scoped{}
	k := 0
	for _, field := range ft.Params.List {
		for _, name := range field.Names {
			if k < len(args) {
				env[p.info.Defs[name]] = args[k]
			}
			k++
		}
	}
	var out []scoped
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			switch {
			case len(x.Results) == 1:
				out = append(out, scoped{x.Results[0], i, env})
			case i < len(x.Results):
				out = append(out, scoped{x.Results[i], 0, env})
			}
		}
		return true
	})
	return out
}

// texts is the literal texts expression s can write, each value in them as
// "X", and the texts behind each such value (filling): its own literal or
// constant; of a variable or a struct field, what
// the package sets it to; of a parameter, the argument of the call that
// led to it; of a call of the package's own function, what that returns;
// of any other call, a conversion, an index or a list, what its parts
// write. One word is a value, a key or a name, and no text. seen holds the
// expressions read.
func (p *doorSource) texts(s scoped, seen map[ast.Expr]bool) []string {
	e := ast.Unparen(s.e)
	if seen[e] {
		return nil
	}
	seen[e] = true
	var out []string
	from := func(next ...scoped) {
		for _, n := range next {
			out = append(out, p.texts(n, seen)...)
		}
	}
	parts := func(es []ast.Expr) {
		for _, a := range es {
			from(scoped{a, 0, s.env})
		}
	}
	text, lit := literalText(e)
	if tv := p.info.Types[e]; !lit && tv.Value != nil && tv.Value.Kind() == constant.String {
		text, lit = constant.StringVal(tv.Value), true
	}
	if lit && strings.ContainsFunc(text, unicode.IsLetter) && strings.Contains(strings.TrimSpace(text), " ") {
		out = append(out, text)
	}
	if lit {
		parts(filling(e))
	}
	switch x := e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		obj := p.object(x)
		if bound, ok := s.env[obj]; ok {
			from(bound)
		}
		for _, v := range p.sets[obj] {
			from(scoped{v.e, v.i, s.env})
		}
	case *ast.IndexExpr:
		parts([]ast.Expr{x.X})
	case *ast.CompositeLit:
		for _, elt := range x.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			parts([]ast.Expr{elt})
		}
	case *ast.CallExpr:
		args := make([]scoped, len(x.Args))
		for i, a := range x.Args {
			args[i] = scoped{a, 0, s.env}
		}
		callee := p.object(x.Fun)
		if callee != nil && callee.Pkg() != nil && callee.Parent() == callee.Pkg().Scope() &&
			slices.Contains(exactCalls, callee.Pkg().Path()+"."+callee.Name()) {
			break
		}
		own := false
		if decl := p.decls[callee]; decl != nil && decl.Body != nil {
			own = true
			from(p.returned(decl.Type, decl.Body, args, s.i)...)
		}
		for _, v := range p.sets[callee] {
			if fl, ok := v.e.(*ast.FuncLit); ok {
				own = true
				from(p.returned(fl.Type, fl.Body, args, s.i)...)
			}
		}
		if fl, ok := ast.Unparen(x.Fun).(*ast.FuncLit); ok {
			own = true
			from(p.returned(fl.Type, fl.Body, args, s.i)...)
		}
		if !own && !lit {
			// a function of another package writes the texts it is given
			parts(slices.DeleteFunc(slices.Clone(x.Args), func(a ast.Expr) bool { return !p.textual(a) }))
		}
	}
	return out
}

// exported finds the compiled form of each package the door packages
// import, as the go command built it.
func exported(t *testing.T, fs *token.FileSet, pkgs []string) types.Importer {
	t.Helper()
	args := []string{"list", "-export", "-deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}"}
	for _, pkg := range pkgs {
		args = append(args, "../"+pkg)
	}
	list := exec.Command("go", args...)
	var failure strings.Builder
	list.Stderr = &failure
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, failure.String())
	}
	files := map[string]string{}
	for _, line := range strings.Fields(string(out)) {
		path, file, _ := strings.Cut(line, "=")
		files[path] = file
	}
	return importer.ForCompiler(fs, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no compiled package %s", path)
		}
		return os.Open(file)
	})
}

// doorText is one call of a door: where it stands, the door, whether an
// agent alone meets it there, and the texts its argument can write.
type doorText struct {
	at    token.Position
	fn    string
	agent bool
	texts []string
}

// doorTexts are the calls of the doors calls names, with the index of
// their text among their arguments, in files, one package's code: each
// with the literal texts its argument can write (doorSource.texts),
// sorted, each once.
func doorTexts(fs *token.FileSet, pkgPath string, files []*ast.File, imp types.Importer, calls map[string]int) []doorText {
	source := readDoorSource(fs, pkgPath, files, imp)
	var out []doorText
	for _, file := range files {
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
			texts := source.texts(scoped{e: call.Args[i]}, map[ast.Expr]bool{})
			slices.Sort(texts)
			out = append(out, doorText{fs.Position(call.Pos()), fn, agent > 0, slices.Compact(texts)})
			return true
		})
	}
	return out
}

// A door's text is found where the door's argument is a variable the
// function adds to, a package's constant, a struct field, in a keyed and
// in a plain literal, a function's result, one of two, and a parameter a
// function passes on. A value that fills a text reads "X" in it, and the
// text behind the value is found too: a part of a concatenation and a
// value of a format. One word is no text, and neither is what stands in a
// code fence of package factoryd (exactCalls).
func TestDoorTextsFollowAFieldAndAFunction(t *testing.T) {
	const src = `package doors

type refusal struct {
	status int
	msg    string
}

func refuse(status int, msg string) error { return nil }

const held = "The card is on hold: release it first."

var table = []refusal{{msg: "The ask is answered already."}, {409, "The card merged."}}

func wait(name string) (int, string) {
	if name == "" {
		return 409, "Wait for the card to merge."
	}
	return 409, "Wait for card " + name + " to merge."
}

func passed(s string) string { return s }

type printer struct{}

func (printer) Sprintf(format string, a ...any) string { return "" }

var fmt printer

func fenced(s string) string { return "` + "```" + `\n" + s + "\n` + "```" + `" }

func cause(n int) string {
	if n > 1 {
		return "its checks are red"
	}
	return "red"
}

func press(no refusal, name string) {
	_ = refuse(no.status, no.msg)
	_, why := wait(name)
	_ = refuse(409, why)
	_ = refuse(409, held)
	_ = refuse(409, passed("Close the plan first."))
	line := "red"
	line += "The plan is closed already."
	_ = refuse(409, line)
	_ = refuse(409, name)
	_ = refuse(409, "The card waits: "+cause(2)+".")
	_ = refuse(409, fmt.Sprintf("Card %d waits, and %s.", 3, cause(1)))
	_ = refuse(409, "The run's log:\n\n"+fenced("exit 1; the run ended"))
}
`
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "doors.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	texts := func(pkgPath string) [][]string {
		var got [][]string
		for _, d := range doorTexts(fs, pkgPath, []*ast.File{file}, nil, map[string]int{"refuse": 1}) {
			got = append(got, d.texts)
		}
		return got
	}
	want := [][]string{
		{"The ask is answered already.", "The card merged."},
		{"Wait for card X to merge.", "Wait for the card to merge."},
		{"The card is on hold: release it first."},
		{"Close the plan first."},
		{"The plan is closed already."},
		nil,
		{"The card waits: X.", "its checks are red"},
		{"Card X waits, and X.", "its checks are red"},
		{"The run's log:\n\nX"},
	}
	if got := texts(factorydPath); !reflect.DeepEqual(got, want) {
		t.Errorf("as package factoryd, the doors' texts are\n%q\nwant\n%q", got, want)
	}
	// a fenced of another package is a function like any other: its
	// argument is read, and the fence around one value is no text
	want[len(want)-1] = []string{"The run's log:\n\nX", "exit 1; the run ended"}
	if got := texts("doors"); !reflect.DeepEqual(got, want) {
		t.Errorf("as another package, the doors' texts are\n%q\nwant\n%q", got, want)
	}
}

// No refusal a door gives and no comment the factory posts shows a
// machine artefact in the words the code holds, as its reader reads it (doorFaults): each call of doorCalls, the literal texts
// its argument can write (doorTexts), through a variable, a struct field
// or a function of its package, with each value as "X".
func TestEveryRefusalAndCommentShowsNoMachineArtefact(t *testing.T) {
	fs := token.NewFileSet()
	imp := exported(t, fs, slices.Sorted(maps.Keys(doorCalls)))
	for pkg, calls := range doorCalls {
		names, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		var files []*ast.File
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fs, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		for _, d := range doorTexts(fs, path.Dir(factorydPath)+"/"+pkg, files, imp, calls) {
			for _, text := range d.texts {
				if faults := doorFaults(pkg, d.fn, d.agent, text); len(faults) > 0 {
					t.Errorf("%s: %q\n\t%s", d.at, text, strings.Join(faults, " "))
				}
			}
		}
	}
}
