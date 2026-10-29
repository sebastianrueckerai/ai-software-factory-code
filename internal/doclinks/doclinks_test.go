package doclinks

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestLinks(t *testing.T) {
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"brief", "See [plan](../plans/x.md), [a](docs/a.md#part-2), [b](https://x/y.md), [c](/abs.md), [d](#anchor), " +
			"![img](assets/i.png) and [plan again](../plans/x.md).", []string{"../plans/x.md", "docs/a.md", "assets/i.png"}},
		{"title", `[a](x.md "the title") and [b](y.md 'other')`, []string{"x.md", "y.md"}},
		{"query", "[a](a.md?raw=1) [b](?only) [m](mailto:h@h) [r](roadmap.md)", []string{"a.md", "roadmap.md"}},
		{"none", "no links, [just brackets] (and parens)", nil},
		{"long", "[300](" + strings.Repeat("a", 297) + ".md) [301](" + strings.Repeat("b", 298) + ".md)", []string{strings.Repeat("a", 297) + ".md"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Links(c.text); !slices.Equal(got, c.want) {
				t.Fatalf("Links = %q, want %q", got, c.want)
			}
		})
	}
}

func TestLinksStopsAtTwenty(t *testing.T) {
	var b strings.Builder
	for i := range 25 {
		fmt.Fprintf(&b, "[%d](f%d.md) ", i, i)
	}
	got := Links(b.String())
	if len(got) != 20 || got[0] != "f0.md" || got[19] != "f19.md" {
		t.Fatalf("Links = %q, want f0.md to f19.md", got)
	}
}

func TestResolve(t *testing.T) {
	plans := []string{"docs/plans/x.md", "docs/plans/y.md"}
	for _, c := range []struct {
		link string
		tree []string
		want []string
	}{
		{"../plans/x.md", plans, []string{"docs/plans/x.md"}},
		{"x.md", []string{"b/x.md", "a/x.md"}, []string{"a/x.md", "b/x.md"}},
		{"docs/plans/x.md", plans, []string{"docs/plans/x.md"}},
		{"./../../plans/./x.md", plans, []string{"docs/plans/x.md"}},
		{"lans/x.md", plans, nil},
		{"gone.md", plans, nil},
	} {
		if got := Resolve(c.link, c.tree); !slices.Equal(got, c.want) {
			t.Errorf("Resolve(%q) = %q, want %q", c.link, got, c.want)
		}
	}
}

func TestLines(t *testing.T) {
	plans := []string{"docs/plans/x.md", "docs/plans/y.md"}
	for _, c := range []struct {
		text string
		tree []string
		want []string
	}{
		{"[plan](../plans/x.md)", plans, []string{"../plans/x.md is /workspace/docs/docs/plans/x.md"}},
		{"[gone](gone.md)", plans, []string{"gone.md is not among the text files at /workspace/docs"}},
		{"[x](x.md)", []string{"a/x.md", "b/x.md"}, []string{"x.md matches 2 files: a/x.md, b/x.md"}},
		{"[r](r.md)", []string{"d/r.md", "c/r.md", "b/r.md", "a/r.md"}, []string{"r.md matches 4 files: a/r.md, b/r.md, c/r.md"}},
		{"[plan](../plans/x.md) then [gone](gone.md)", plans,
			[]string{"../plans/x.md is /workspace/docs/docs/plans/x.md", "gone.md is not among the text files at /workspace/docs"}},
		{"no links", plans, nil},
	} {
		if got := Lines(c.text, c.tree, "/workspace/docs"); !slices.Equal(got, c.want) {
			t.Errorf("Lines(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
