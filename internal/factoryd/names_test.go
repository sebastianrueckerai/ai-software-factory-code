package factoryd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var numberedField = regexp.MustCompile(`^v\d+$`)

func changeIDFindings(dir string) []string {
	var out []string
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return []string{err.Error()}
	}
	fset := token.NewFileSet()
	seen := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "GetVersion" || fmt.Sprint(sel.X) != "workflow" {
					return true
				}
				pos := fset.Position(n.Pos()).String()
				lit, ok := n.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					out = append(out, pos+": the change id is not a string literal")
					return true
				}
				id, _ := strconv.Unquote(lit.Value)
				if first, ok := seen[id]; ok {
					out = append(out, fmt.Sprintf("%s: change id %q again (first at %s)", pos, id, first))
				}
				seen[id] = pos
			case *ast.TypeSpec:
				st, ok := n.Type.(*ast.StructType)
				if !ok || n.Name.Name != "taskRun" {
					return true
				}
				for _, f := range st.Fields.List {
					for _, name := range f.Names {
						if numberedField.MatchString(name.Name) && !frozenField(name.Name) {
							out = append(out, fmt.Sprintf("%s: taskRun field %s; name a new change id's field after it", fset.Position(name.Pos()), name.Name))
						}
					}
				}
			}
			return true
		})
	}
	return out
}

func frozenField(name string) bool {
	n, err := strconv.Atoi(name[1:])
	return err == nil && n >= 2 && n <= 23
}

// A new Temporal change id is a name kept in a named taskRun field, never
// vNN: two parallel branches both took v21 in Stage 10. v to v23 stay.
func TestChangeIDsUnique(t *testing.T) {
	if f := changeIDFindings("."); len(f) > 0 {
		t.Fatalf("shipped workflow: %v", f)
	}
	src, err := os.ReadFile("workflow.go")
	if err != nil {
		t.Fatal(err)
	}
	const last = `v23: workflow.GetVersion(ctx, "merge-conflict", workflow.DefaultVersion, versionMergeConflict)`
	const fields = "v22, v23          workflow.Version"
	const depWait = `run.depWait = workflow.GetVersion(ctx, "dep-wait", workflow.DefaultVersion, versionDepWait)`
	for _, tc := range []struct{ name, old, new, bad string }{
		{"a named field passes", fields, fields + "\n\tmergeQueue workflow.Version", ""},
		{"dep-wait twice", depWait, depWait + "\n\trun.mergeQueue = " + depWait[len("run.depWait = "):], `"dep-wait"`},
		{"a repeated change id", last, last + `,
		v24: workflow.GetVersion(ctx, "poster-cap", workflow.DefaultVersion, versionPosterCap)`, `"poster-cap"`},
		{"a v24 field", fields, fields + "\n\tv24 workflow.Version", "v24"},
		{"a repeated change id in another file", "", "", `"roll"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			planted := string(src)
			if tc.old != "" {
				if !strings.Contains(planted, tc.old) {
					t.Fatalf("workflow.go lacks %q", tc.old)
				}
				planted = strings.Replace(planted, tc.old, tc.new, 1)
			} else {
				other := "package factoryd\n\nimport \"go.temporal.io/sdk/workflow\"\n\nfunc other(ctx workflow.Context) workflow.Version {\n\treturn workflow.GetVersion(ctx, \"roll\", workflow.DefaultVersion, 1)\n}\n"
				if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte(other), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "workflow.go"), []byte(planted), 0o600); err != nil {
				t.Fatal(err)
			}
			f := changeIDFindings(dir)
			switch {
			case tc.bad == "" && len(f) != 0:
				t.Fatalf("findings = %v, want none", f)
			case tc.bad != "" && (len(f) != 1 || !strings.Contains(f[0], tc.bad)):
				t.Fatalf("findings = %v, want one naming %s", f, tc.bad)
			}
		})
	}
}
