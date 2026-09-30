package schema

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// kindConstants reads the string constants of package factoryd whose names
// match pattern from its source: factoryd imports this package, so the test
// cannot import factoryd, and a list kept here would miss the next kind.
func kindConstants(t *testing.T, pattern string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("../factoryd/*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(pattern)
	out := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !re.MatchString(name.Name) || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out[name.Name], _ = strconv.Unquote(lit.Value)
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no constant of factoryd matches %s", pattern)
	}
	return out
}

// Every migration that adds a kind rewrites the whole check, so of two
// parallel ones only the last applied keeps its kind: after all migrations,
// every alert kind and every approval kind factoryd names is accepted.
func TestKindChecksAccept(t *testing.T) {
	db, err := sql.Open("pgx", schematest.DSN(t, "schema_kinds"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM alerts; DELETE FROM approval_requests`); err != nil {
		t.Fatal(err)
	}
	for name, kind := range kindConstants(t, `^Alert[A-Z]`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO alerts (kind, source, task_id) VALUES ($1, 'kinds', $2)`, kind, "t-"+kind); err != nil {
			t.Errorf("alert kind %s (%q): %v", name, kind, err)
		}
	}
	for name, kind := range kindConstants(t, `^Approval[A-Z]`) {
		if _, err := db.ExecContext(ctx, `INSERT INTO approval_requests (kind, task_id, project) VALUES ($1, $2, 'o/r')`, kind, "t-"+kind); err != nil {
			t.Errorf("approval kind %s (%q): %v", name, kind, err)
		}
	}
}
