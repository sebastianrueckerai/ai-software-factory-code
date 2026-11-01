package main

// The symbol table resolves a span's Go symbol to the file and line of
// its declaration, so every provenance line links at the recorded SHA.
// An unresolved symbol is rot — the builder moved or was renamed — and
// the tool says so instead of guessing.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

type location struct {
	file string
	line int
}

// symbolTable maps declaration names — "Foo" for top-level symbols,
// "Recv.Method" for methods — to where they are declared, sorted by file.
type symbolTable struct {
	names map[string][]location
}

// loadSymbols parses every non-test Go file under root and records where
// each declaration sits.
func loadSymbols(root string) (*symbolTable, error) {
	t := &symbolTable{names: map[string][]location{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			if d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		case !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		record := func(name string, line int) {
			t.names[name] = append(t.names[name], location{rel, line})
			t.names[f.Name.Name+"."+name] = append(t.names[f.Name.Name+"."+name], location{rel, line})
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				key := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) > 0 {
					key = recvName(d.Recv.List[0].Type) + "." + key
				}
				record(key, fset.Position(d.Pos()).Line)
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.ValueSpec:
						for _, n := range s.Names {
							record(n.Name, fset.Position(n.Pos()).Line)
						}
					case *ast.TypeSpec:
						record(s.Name.Name, fset.Position(s.Pos()).Line)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, locs := range t.names {
		sort.Slice(locs, func(i, j int) bool { return locs[i].file < locs[j].file })
	}
	return t, nil
}

// recvName is the receiver type's name, pointer or not.
func recvName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if i, ok := e.(*ast.Ident); ok {
		return i.Name
	}
	return ""
}

// locate answers where symbol, declared in the package at pkgDir, sits:
// a method "(*R).M" or "(R).M", a package-qualified "pkg.Name", or a
// bare name. Matches inside pkgDir win; anything else is a fallback
// that keeps the link honest when a helper lives in another package.
func (t *symbolTable) locate(pkgDir, symbol string) (string, int, bool) {
	key := symbol
	if qual, _, found := strings.Cut(key, "."); found && qual == filepath.Base(pkgDir) {
		// a package-qualified name of this package: strip the qualifier
		key = key[len(qual)+1:]
	}
	if m := methodForm(key); m != "" {
		key = m
	}
	// a composite names two origins; the first carries the permalink
	if first, _, found := strings.Cut(key, "+"); found {
		key = first
	}
	locs := t.names[key]
	for _, l := range locs { // sorted by file, so the in-package hit is stable
		if strings.HasPrefix(l.file, pkgDir+"/") {
			return l.file, l.line, true
		}
	}
	if len(locs) > 0 {
		return locs[0].file, locs[0].line, true
	}
	return "", 0, false
}

// methodForm turns "(*R).M" or "(R).M" into the table's "R.M" key; any
// other shape answers "".
func methodForm(symbol string) string {
	open := strings.IndexByte(symbol, '(')
	if open != 0 {
		return ""
	}
	close := strings.IndexByte(symbol, ')')
	if close < 0 || close+1 >= len(symbol) || symbol[close+1] != '.' {
		return ""
	}
	recv := symbol[1:close]
	recv = strings.TrimPrefix(recv, "*")
	if recv == "" || strings.ContainsAny(recv, ".()*") {
		return ""
	}
	return recv + symbol[close+1:]
}
