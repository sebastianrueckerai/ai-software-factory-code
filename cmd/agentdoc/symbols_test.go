package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeFixtureModule writes the smallest module the resolver and the page
// renderer can point provenance at, and loads its symbol table.
func writeFixtureModule(t *testing.T) (string, *symbolTable) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "internal", "fixture")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte(`package fixture

// Foo does.
func Foo() {}

// R is a receiver.
type R struct{}

// M is a method.
func (r *R) M() {}

// offered offers.
func (r *R) offered() {}

const Bar = "bar"

const askConst = "ask"

// buildOpencodeConfig builds.
func buildOpencodeConfig() {}

// S holds a field.
type S struct {
	Field string
}
`), 0o640); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "internal", "other")
	if err := os.MkdirAll(other, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "b.go"), []byte(`package other

// Guide is in another package.
const Guide = "guide"
`), 0o640); err != nil {
		t.Fatal(err)
	}
	syms, err := loadSymbols(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, syms
}

func TestLoadSymbolsResolvesMethodsAndQualifiedNames(t *testing.T) {
	_, syms := writeFixtureModule(t)
	for _, tc := range []struct{ dir, symbol, want string }{
		{"internal/fixture", "(*R).M", "internal/fixture/a.go:10"},
		{"internal/fixture", "(R).M", "internal/fixture/a.go:10"},
		{"internal/fixture", "Foo", "internal/fixture/a.go:4"},
		{"internal/fixture", "fixture.Bar", "internal/fixture/a.go:15"},
		{"internal/fixture", "other.Guide", "internal/other/b.go:4"},
		{"internal/fixture", "fixture.Bar+scrubbed", "internal/fixture/a.go:15"},
		{"internal/fixture", "fixture.S.Field", "internal/fixture/a.go:24"},
		{"internal/fixture", "Nope", ""},
	} {
		rel, line, ok := syms.locate(tc.dir, tc.symbol)
		if tc.want == "" {
			if ok {
				t.Fatalf("%s resolved to %s:%d, want nothing", tc.symbol, rel, line)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s did not resolve", tc.symbol)
		}
		if got := rel + ":" + strconv.Itoa(line); got != tc.want {
			t.Fatalf("%s resolved to %s, want %s", tc.symbol, got, tc.want)
		}
	}
}

// A sibling worktree inside the repo is not source: its copy of a file
// must never win a permalink.
func TestLoadSymbolsIgnoresSiblingWorktrees(t *testing.T) {
	root, _ := writeFixtureModule(t)
	wt := filepath.Join(root, ".worktrees", "closeout", "internal", "fixture")
	if err := os.MkdirAll(wt, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "a.go"), []byte("package fixture\n\n// Foo is a worktree copy.\nfunc Foo() {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	syms, err := loadSymbols(root)
	if err != nil {
		t.Fatal(err)
	}
	rel, _, ok := syms.locate("internal/gateway", "Foo")
	if !ok || rel != "internal/fixture/a.go" {
		t.Fatalf("Foo resolved to %q (ok=%v), want internal/fixture/a.go", rel, ok)
	}
}

// A bare name declared in two packages the page does not live in cannot
// be pinned to one file honestly; that is rot, not the alphabetically
// first hit.
func TestLocateRefusesAmbiguousCrossPackageNames(t *testing.T) {
	root, _ := writeFixtureModule(t)
	third := filepath.Join(root, "internal", "third")
	if err := os.MkdirAll(third, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(third, "c.go"), []byte("package third\n\n// Guide is a second bare Guide.\nconst Guide = \"guide3\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	syms, err := loadSymbols(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := syms.locate("internal/fixture", "Guide"); ok {
		t.Fatal("a bare name declared in two other packages resolved; want rot")
	}
	if _, _, ok := syms.locate("internal/other", "Guide"); !ok {
		t.Fatal("the in-package hit must still win")
	}
}

// A subpackage's file sits under the page's package directory but is
// another package: its declaration of the same name, sorted first, never
// wins over the page's own.
func TestLocateKeepsThePagesOwnPackage(t *testing.T) {
	root, _ := writeFixtureModule(t)
	sub := filepath.Join(root, "internal", "fixture", "pg")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	for path, src := range map[string]string{
		filepath.Join(root, "internal", "fixture", "z.go"): "package fixture\n\n// Zed is the fixture's own.\nvar Zed = 1\n",
		filepath.Join(sub, "pg.go"):                        "package pg\n\n// Zed re-exports the fixture's Zed.\nvar Zed = 1\n",
	} {
		if err := os.WriteFile(path, []byte(src), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	syms, err := loadSymbols(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range []string{"Zed", "fixture.Zed"} {
		if rel, line, ok := syms.locate("internal/fixture", symbol); !ok || rel+":"+strconv.Itoa(line) != "internal/fixture/z.go:4" {
			t.Errorf("%s resolved to %s:%d (ok=%v), want internal/fixture/z.go:4", symbol, rel, line, ok)
		}
	}
}
