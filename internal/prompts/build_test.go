package prompts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCatalog(t *testing.T, facts string) *catalog {
	t.Helper()
	profile := strings.NewReplacer("step: x", "step: "+facts, "facts: x", "facts: "+facts, "guide: false", "guide: true").Replace(validProfile)
	fsys := profileFS(profile)
	fsys["profiles/"+facts+".yaml"] = fsys["profiles/x.yaml"]
	delete(fsys, "profiles/x.yaml")
	c, err := load(fsys, testWriting)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func init() {
	RegisterFacts("test-join", func(string) (string, error) { return "F", nil })
	RegisterFacts("test-nil", func(string) (string, error) { panic("the renderer of nil facts ran") })
	RegisterFacts("test-type", func(string) (string, error) { return "F", nil })
}

func TestBuildJoinsInFixedOrder(t *testing.T) {
	p, err := testCatalog(t, "test-join").build("test-join", "facts")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Text(), "G\n\nL\n\nC\n\nP\n\nF"; got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
	if got, want := p.Standing(), "G\n\nL\n\nC\n\nP"; got != want {
		t.Errorf("Standing = %q, want %q", got, want)
	}
	if got := p.Facts(); got != "F" {
		t.Errorf("Facts = %q, want F", got)
	}
	if got, want := p.Parts(), (Parts{1, 1, 1, 1, 1}); got != want {
		t.Errorf("Parts = %+v, want %+v", got, want)
	}
	if got := p.Parts().Total(); got != 5 {
		t.Errorf("Total = %d, want 5", got)
	}
	if p.Step() != "test-join" || p.Purpose() != "review" {
		t.Errorf("Step, Purpose = %q, %q, want test-join, review", p.Step(), p.Purpose())
	}
}

func TestBuildNilFactsRendersNothing(t *testing.T) {
	p, err := testCatalog(t, "test-nil").build("test-nil", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Facts() != "" || p.Text() != p.Standing() {
		t.Errorf("Facts = %q, Text = %q, want no facts", p.Facts(), p.Text())
	}
}

func TestBuildRefusesWrongFactsType(t *testing.T) {
	_, err := testCatalog(t, "test-type").build("test-type", 5)
	if want := "step test-type: facts of type int, want string"; err == nil || err.Error() != want {
		t.Errorf("build = %v, want %s", err, want)
	}
}

func TestBuildRefusesAnUnknownStep(t *testing.T) {
	_, err := testCatalog(t, "test-join").build("nope", nil)
	if want := "step nope has no profile: write internal/prompts/profiles/nope.yaml"; err == nil || err.Error() != want {
		t.Errorf("build = %v, want %s", err, want)
	}
}

// A Prompt comes from Build alone: its fields are unexported, and no code
// writes the literal prompts.Prompt{}, which would make an empty one.
func TestPromptIsClosed(t *testing.T) {
	for _, tree := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(tree, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Prompt" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "prompts" {
						t.Errorf("%s writes a prompts.Prompt literal: only prompts.Build makes a Prompt", fset.Position(lit.Pos()))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
