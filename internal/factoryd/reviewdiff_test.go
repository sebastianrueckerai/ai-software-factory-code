package factoryd

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const splitTestDiff = "diff --git a/a.go b/a.go\n" +
	"index 1111111..2222222 100644\n" +
	"--- a/a.go\n" +
	"+++ b/a.go\n" +
	"@@ -1,5 +1,8 @@\n" +
	" package a\n" +
	" \n" +
	"+import \"sync\"\n" +
	"+\n" +
	" func A() {\n" +
	"-\treturn\n" +
	"+\tvar mu sync.Mutex\n" +
	"+\t_ = &mu\n" +
	" }\n" +
	"@@ -20,3 +23,5 @@ func B() {\n" +
	" \tx := 1\n" +
	"-\ty := 2\n" +
	"+\ty := 3\n" +
	"+\tz := 4\n" +
	"+\t_ = z\n" +
	" }\n" +
	"diff --git a/old_test.go b/b_test.go\n" +
	"similarity index 80%\n" +
	"rename from old_test.go\n" +
	"rename to b_test.go\n" +
	"index 3333333..4444444 100644\n" +
	"--- a/old_test.go\n" +
	"+++ b/b_test.go\n" +
	"@@ -1,2 +1,5 @@\n" +
	" package a\n" +
	"+\n" +
	"+func TestB(t *testing.T) {\n" +
	"+}\n" +
	" // end\n" +
	"diff --git a/c.md b/c.md\n" +
	"index 5555555..6666666 100644\n" +
	"--- a/c.md\n" +
	"+++ b/c.md\n" +
	"@@ -1,2 +1,2 @@\n" +
	" # C\n" +
	"--- old note\n" +
	"+++ new note\n"

// a.go: hunk one adds 4 and removes 1, hunk two adds 3 and removes 1;
// b_test.go, renamed from old_test.go, adds 3; c.md's body lines "--- old
// note" and "+++ new note" are a removal and an addition, not headers.
func TestSplitDiff(t *testing.T) {
	files := splitDiff(splitTestDiff)
	type got struct {
		Path, OldPath  string
		Added, Removed int
	}
	var gotFiles []got
	var joined strings.Builder
	for _, f := range files {
		gotFiles = append(gotFiles, got{f.Path, f.OldPath, f.Added, f.Removed})
		joined.WriteString(f.Text)
	}
	want := []got{{"a.go", "", 7, 2}, {"b_test.go", "old_test.go", 3, 0}, {"c.md", "", 1, 1}}
	if !slices.Equal(gotFiles, want) {
		t.Fatalf("files = %+v, want %+v", gotFiles, want)
	}
	if joined.String() != splitTestDiff {
		t.Fatal("the files' texts do not join to the diff")
	}
	if !strings.HasPrefix(files[2].Text, "diff --git a/c.md b/c.md\n") || !strings.HasSuffix(files[0].Text, "+\t_ = z\n }\n") {
		t.Fatalf("a file's text is not its own: %q, %q", files[0].Text, files[2].Text)
	}
}

// A new and a deleted file are named by their one path; text before the
// first file header is a part of its own, without a path.
func TestSplitDiffNewDeletedAndPreamble(t *testing.T) {
	diff := "x\n" +
		"diff --git a/n.go b/n.go\nnew file mode 100644\n--- /dev/null\n+++ b/n.go\n@@ -0,0 +1 @@\n+package n\n" +
		"diff --git a/d.go b/d.go\ndeleted file mode 100644\n--- a/d.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-package d\n"
	files := splitDiff(diff)
	if len(files) != 3 || files[0].Path != "" || files[0].Text != "x\n" ||
		files[1].Path != "n.go" || files[1].OldPath != "" || files[1].Added != 1 ||
		files[2].Path != "d.go" || files[2].OldPath != "" || files[2].Removed != 1 {
		t.Fatalf("files = %+v", files)
	}
	if len(splitDiff("")) != 0 {
		t.Fatal("an empty diff has files")
	}
}

func sized(kib ...int) []fileDiff {
	var out []fileDiff
	for i, k := range kib {
		out = append(out, fileDiff{Path: fmt.Sprintf("f%d", i+1), Text: strings.Repeat("x", k<<10)})
	}
	return out
}

func paths(chunks [][]fileDiff) [][]string {
	var out [][]string
	for _, c := range chunks {
		var p []string
		for _, f := range c {
			p = append(p, f.Path)
		}
		out = append(out, p)
	}
	return out
}

func TestPackFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		kib    []int
		chunks string
		tooBig []string
	}{
		"three of 30":   {[]int{30, 30, 30}, "[[f1] [f2] [f3]]", nil},
		"five of 10":    {[]int{10, 10, 10, 10, 10}, "[[f1 f2 f3 f4] [f5]]", nil},
		"60 then 10":    {[]int{60, 10}, "[[f2]]", []string{"f1"}},
		"exactly 48":    {[]int{48, 1}, "[[f1] [f2]]", nil},
		"none":          {nil, "[]", nil},
		"too big alone": {[]int{49}, "[]", []string{"f1"}},
	} {
		t.Run(name, func(t *testing.T) {
			chunks, tooBig := packFiles(sized(tc.kib...), 48<<10)
			var big []string
			for _, f := range tooBig {
				big = append(big, f.Path)
			}
			if got := fmt.Sprint(paths(chunks)); got != tc.chunks || !slices.Equal(big, tc.tooBig) {
				t.Fatalf("chunks %s, too big %v; want %s, %v", got, big, tc.chunks, tc.tooBig)
			}
		})
	}
}

const concurrencyTestDiff = "diff --git a/w.go b/w.go\n" +
	"--- a/w.go\n" +
	"+++ b/w.go\n" +
	"@@ -10,3 +10,4 @@ func run() {\n" +
	" \tx := 1\n" +
	" \ty := 2\n" +
	"+\tgo func() {\n" +
	" \tz := 3\n" +
	"@@ -18,2 +19,3 @@\n" +
	" \ta := 1\n" +
	"+\tch := make(chan int, 1)\n" +
	" \tb := 2\n" +
	"@@ -28,3 +30,3 @@\n" +
	" \tc := 1\n" +
	"-\tgo worker()\n" +
	"+\tvar mu sync.Mutex\n" +
	" \td := 2\n" +
	"@@ -36,6 +38,9 @@\n" +
	" \tg := 0\n" +
	" \te := 1\n" +
	"+\tselect {\n" +
	"+\tdefault:\n" +
	" \tf := 2\n" +
	" \th := 3\n" +
	" \ti := 4\n" +
	"+\tatomic.AddInt64(&n, 1)\n" +
	" \tj := 5\n"

func TestConcurrencySites(t *testing.T) {
	got := concurrencySites(splitDiff(concurrencyTestDiff))
	want := []site{
		{File: "w.go", Line: 12, Kind: "goroutine", Text: "go func() {"},
		{File: "w.go", Line: 20, Kind: "channel", Text: "ch := make(chan int, 1)"},
		{File: "w.go", Line: 31, Kind: "lock", Text: "var mu sync.Mutex"},
		{File: "w.go", Line: 40, Kind: "select", Text: "select {"},
		{File: "w.go", Line: 45, Kind: "atomic", Text: "atomic.AddInt64(&n, 1)"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sites = %+v, want %+v", got, want)
	}
	block := "every goroutine, channel, lock, select and atomic the diff adds, with file and line:\n" +
		"w.go:12 goroutine: go func() {\n" +
		"w.go:20 channel: ch := make(chan int, 1)\n" +
		"w.go:31 lock: var mu sync.Mutex\n" +
		"w.go:40 select: select {\n" +
		"w.go:45 atomic: atomic.AddInt64(&n, 1)"
	if b := concurrencyBlock(got); b != block {
		t.Fatalf("block = %q, want %q", b, block)
	}
	aGo := concurrencySites(splitDiff(splitTestDiff[:strings.Index(splitTestDiff, "diff --git a/old_test.go")]))
	if len(aGo) != 1 || aGo[0].Kind != "lock" || aGo[0].Line != 6 {
		t.Fatalf("a.go's sites = %+v, want its one sync.Mutex at line 6", aGo)
	}
	if got := concurrencySites(splitDiff("diff --git a/c.md b/c.md\n--- a/c.md\n+++ b/c.md\n@@ -1 +1 @@\n-a\n+b\n")); len(got) != 0 || concurrencyBlock(got) != "none" {
		t.Fatalf("sites = %+v, block %q", got, concurrencyBlock(got))
	}
}

// Each kind's shapes, on added lines only, one site per line.
func TestConcurrencySiteKinds(t *testing.T) {
	for text, kind := range map[string]string{
		"go func() {":               "goroutine",
		"go s.worker(ctx)":          "goroutine",
		"go run(ch)":                "goroutine",
		"x := <-done":               "channel",
		"ch <- v":                   "channel",
		"var events chan Event":     "channel",
		"func f(out chan<- int) {":  "channel",
		"mu sync.RWMutex":           "lock",
		"r.mu.Lock()":               "lock",
		"r.mu.RLock()":              "lock",
		"once sync.Once":            "lock",
		"var wg sync.WaitGroup":     "lock",
		"select {":                  "select",
		"n := atomic.LoadInt64(&c)": "atomic",
		"var hits atomic.Int64":     "atomic",
		"// going on":               "",
		"cargo build":               "",
		"channel := 3":              "",
		"r.mu.Unlock()":             "",
	} {
		diff := "diff --git a/k.go b/k.go\n--- a/k.go\n+++ b/k.go\n@@ -1 +1,2 @@\n x\n+" + text + "\n"
		got := concurrencySites(splitDiff(diff))
		switch {
		case kind == "" && len(got) != 0:
			t.Errorf("%q: sites %+v, want none", text, got)
		case kind != "" && (len(got) != 1 || got[0].Kind != kind || got[0].Line != 2):
			t.Errorf("%q: sites %+v, want one %s at line 2", text, got, kind)
		}
	}
}

// The block names at most 200 sites under its first line and counts the
// rest.
func TestConcurrencyBlockIsCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/g.go b/g.go\n--- a/g.go\n+++ b/g.go\n@@ -0,0 +1,205 @@\n")
	for range 205 {
		b.WriteString("+go f()\n")
	}
	sites := concurrencySites(splitDiff(b.String()))
	lines := strings.Split(concurrencyBlock(sites), "\n")
	if len(sites) != 205 || len(lines) != 202 || lines[0] != concurrencyHead || lines[200] != "g.go:200 goroutine: go f()" || lines[201] != "... and 5 more" {
		t.Fatalf("%d sites, %d lines, last %q", len(sites), len(lines), lines[len(lines)-1])
	}
}

// A finding goes to the part holding the longest path it names, sub/b.go's
// though it names b.go too; else to the one part holding a file whose base
// name it names, stock.go in part 3 and domain.go in part 2, though
// main.go in part 3 is a path within domain.go's name; and to part 1 when
// it names no file or a base name, util.go, two parts hold.
func TestPartOf(t *testing.T) {
	parts := [][]fileDiff{
		{{Path: "a.go"}, {Path: "sub/b.go"}},
		{{Path: "b.go"}, {Path: "internal/x/domain.go"}, {Path: "a/util.go"}},
		{{Path: "main.go"}, {Path: "internal/shop/stock.go"}, {Path: "b/util.go"}},
	}
	for s, want := range map[string]int{
		"sub/b.go:3: x":                  0,
		"b.go:3: y":                      1,
		"a.go: z":                        0,
		"the change lacks a test":        0,
		"stock.go:12: the read lock":     2,
		"`shop/stock.go`: the lock":      2,
		"domain.go:3: x":                 1,
		"util.go: two parts hold it":     0,
		"main.go: in part 3":             2,
		"stock.golden: no file it holds": 0,
	} {
		if got := partOf(s, parts); got != want {
			t.Errorf("partOf(%q) = %d, want %d", s, got, want)
		}
	}
}

// The file list names every file of the whole diff with its counts and
// part, a rename as old -> new.
func TestFileList(t *testing.T) {
	files := splitDiff(splitTestDiff)
	want := "a.go +7 -2 (part 1)\n" +
		"old_test.go -> b_test.go +3 -0 (part 1)\n" +
		"c.md +1 -1 (part 2)"
	if got := fileList([][]fileDiff{files[:2], files[2:]}); got != want {
		t.Fatalf("file list = %q, want %q", got, want)
	}
	var many []fileDiff
	for i := range 205 {
		many = append(many, fileDiff{Path: fmt.Sprintf("f%d", i+1), Added: 1})
	}
	lines := strings.Split(fileList([][]fileDiff{many}), "\n")
	if len(lines) != 201 || lines[199] != "f200 +1 -0 (part 1)" || lines[200] != "... and 5 more" {
		t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

// git ends the --- and +++ lines of a path with a space with a tab; the
// path has none.
func TestSplitDiffPathWithASpace(t *testing.T) {
	diff := "diff --git a/my file.go b/my file.go\n--- a/my file.go\t\n+++ b/my file.go\t\n@@ -1 +1,2 @@\n x\n+go f()\n"
	files := splitDiff(diff)
	if len(files) != 1 || files[0].Path != "my file.go" || files[0].OldPath != "" || files[0].Added != 1 {
		t.Fatalf("files = %+v", files)
	}
	if sites := concurrencySites(files); len(sites) != 1 || sites[0].File != "my file.go" || sites[0].Line != 2 {
		t.Fatalf("sites = %+v", sites)
	}
}
