package factoryd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// pkgConst is a package-level constant: its name, its value when it is a
// string literal (ok), the type names its const declaration states for any
// of its specs, and its position.
type pkgConst struct {
	name, value, pos string
	ok               bool
	blockTypes       []string
}

// pkgConsts are the package-level constants of dir's non-test files.
func pkgConsts(t *testing.T, dir string) []pkgConst {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []pkgConst
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			var types []string
			for _, spec := range g.Specs {
				if id, ok := spec.(*ast.ValueSpec).Type.(*ast.Ident); ok {
					types = append(types, id.Name)
				}
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					c := pkgConst{name: name.Name, blockTypes: types, pos: fset.Position(name.Pos()).String()}
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, err := strconv.Unquote(lit.Value)
							c.value, c.ok = v, err == nil
						}
					}
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// grouped says whether name is prefix followed by a capital letter, as
// SourcePlan is of Source and SourceX is not of Sour.
func grouped(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	r, _ := utf8.DecodeRuneInString(rest)
	return ok && unicode.IsUpper(r)
}

// raisedFlags are the flag names carddigest_flags.go's add raises.
func raisedFlags(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "carddigest_flags.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "add" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s: a flag raised by a name that is no string literal", fset.Position(call.Pos()))
			return true
		}
		name, _ := strconv.Unquote(lit.Value)
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
		return true
	})
	if len(out) == 0 {
		t.Fatal("carddigest_flags.go raises no flag through add: the walk no longer finds its flags")
	}
	return out
}

// sameSet fails unless list holds every value of want once and nothing
// else.
func sameSet(t *testing.T, group string, want, list []string) {
	t.Helper()
	for _, v := range want {
		if !slices.Contains(list, v) {
			t.Errorf("%s: the list lacks %q", group, v)
		}
	}
	for i, v := range list {
		if !slices.Contains(want, v) {
			t.Errorf("%s: the list holds %q, which no constant declares", group, v)
		}
		if slices.Contains(list[:i], v) {
			t.Errorf("%s: the list holds %q twice", group, v)
		}
	}
}

// Each kind list holds every constant of its group, and nothing else: a
// kind added without its list entry fails the build's tests.
func TestKindListsHoldEveryConstant(t *testing.T) {
	consts := slices.Concat(pkgConsts(t, "."), pkgConsts(t, filepath.Join("..", "policy")))
	values := func(group string, in func(c pkgConst) bool) []string {
		var out []string
		for _, c := range consts {
			if !in(c) {
				continue
			}
			if !c.ok {
				t.Errorf("%s: %s at %s is no string literal", group, c.name, c.pos)
			}
			out = append(out, c.value)
		}
		if len(out) == 0 {
			t.Fatalf("%s: the walk finds no constant", group)
		}
		return out
	}
	var cases []string
	for _, k := range CaseKinds() {
		cases = append(cases, string(k))
	}
	sameSet(t, "case kinds", values("case kinds", func(c pkgConst) bool { return slices.Contains(c.blockTypes, "CaseKind") }), cases)
	sameSet(t, "card sources", values("card sources", func(c pkgConst) bool { return grouped(c.name, "Source") }), CardSources())
	sameSet(t, "approval kinds", values("approval kinds", func(c pkgConst) bool { return grouped(c.name, "Approval") }), ApprovalKinds())
	var actions []string
	for _, a := range Actions {
		actions = append(actions, a.Name)
	}
	sameSet(t, "actions", values("actions", func(c pkgConst) bool { return grouped(c.name, "Act") }), actions)
	sameSet(t, "monitor flags", raisedFlags(t), MonitorFlags())

	// route_approval and withdraw are the factory's alone
	var human []string
	for _, a := range Actions {
		if a.Name != "route_approval" && a.Name != "withdraw" {
			human = append(human, a.Name)
		}
	}
	if got := HumanActions(); !slices.Equal(got, human) {
		t.Errorf("HumanActions = %v, want every action but route_approval and withdraw, in table order: %v", got, human)
	}
}

// Every case kind has its words, so no face falls back to the kind's
// identifier.
func TestEveryCaseKindHasWords(t *testing.T) {
	for _, k := range CaseKinds() {
		if kindRow(k).Words == "" {
			t.Errorf("case kind %s has no words in the kind table", k)
		}
	}
}

// A chat row whose birth kind BirthKinds lacks is refused by every writer,
// a poke's among them, so no event kind reaches the inbox unlisted; every
// listed kind and a row no event wrote are stored.
func TestChatRowWriteRefusesAnUnlistedKind(t *testing.T) {
	s, ctx := chatStore(t), t.Context()
	const kind = "card_renamed"
	unlisted := ChatMessage{Role: "assistant", Kind: MessagePoke, Content: "#21 is renamed.", TaskID: "t-1", BirthKind: kind}
	if _, err := s.AddChatMessage(ctx, "sf/code", "anna", unlisted); err == nil || !strings.Contains(err.Error(), kind) {
		t.Errorf("AddChatMessage of birth kind %s: err %v, want a refusal naming the kind", kind, err)
	}
	if _, err := s.Poke(ctx, 1, "sf/code", []string{"anna"}, []bool{false}, unlisted, PokeDelivery{}); err == nil || !strings.Contains(err.Error(), kind) {
		t.Errorf("Poke of birth kind %s: err %v, want a refusal naming the kind", kind, err)
	}
	var stored int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat_messages`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("rows after the refusals = %d (%v), want 0", stored, err)
	}
	for _, k := range append(BirthKinds(), "") {
		if _, err := s.AddChatMessage(ctx, "sf/code", "anna", ChatMessage{Role: "user", Content: "row of " + k, BirthKind: k}); err != nil {
			t.Errorf("AddChatMessage of birth kind %q: %v", k, err)
		}
	}
}
