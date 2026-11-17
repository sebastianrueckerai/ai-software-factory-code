package factoryd

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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

// Git quotes a path with a quote, a backslash or a non-ASCII byte; a path
// that holds " b/" has no quotes and no hunk to tell it by.
func TestParseFileDiffPaths(t *testing.T) {
	for name, c := range map[string]struct {
		diff, path, old string
	}{
		"quote":                    {"diff --git \"a/a\\\"b.go\" \"b/a\\\"b.go\"\nnew file mode 100644\n--- /dev/null\n+++ \"b/a\\\"b.go\"\n@@ -0,0 +1 @@\n+x\n", `a"b.go`, ""},
		"backslash":                {"diff --git \"a/a\\\\b.go\" \"b/a\\\\b.go\"\nnew file mode 100644\n--- /dev/null\n+++ \"b/a\\\\b.go\"\n@@ -0,0 +1 @@\n+x\n", `a\b.go`, ""},
		"octal non-ASCII":          {"diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\nindex 1..2 100644\n--- \"a/caf\\303\\251.go\"\n+++ \"b/caf\\303\\251.go\"\n@@ -1 +1 @@\n-x\n+y\n", "caf\u00e9.go", ""},
		"rename with quoted names": {"diff --git \"a/o\\\"ld.go\" \"b/n\\303\\251w.go\"\nsimilarity index 100%\nrename from \"o\\\"ld.go\"\nrename to \"n\\303\\251w.go\"\n", "n\u00e9w.go", `o"ld.go`},
		"quoted new name only":     {"diff --git a/old.go \"b/n\\303\\251w.go\"\nsimilarity index 100%\nrename from old.go\nrename to \"n\\303\\251w.go\"\n", "n\u00e9w.go", "old.go"},
		"path with b/ and no hunk": {"diff --git a/x b/y b/x b/y\nold mode 100644\nnew mode 100755\n", "x b/y", ""},
		"plain rename":             {"diff --git a/old.go b/new.go\nrename from old.go\nrename to new.go\n", "new.go", "old.go"},
	} {
		t.Run(name, func(t *testing.T) {
			f := parseFileDiff(c.diff)
			if f.Path != c.path || f.OldPath != c.old {
				t.Fatalf("path %q, old %q; want %q, %q", f.Path, f.OldPath, c.path, c.old)
			}
		})
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
	}{
		"three of 30": {[]int{30, 30, 30}, "[[f1] [f2] [f3]]"},
		"five of 10":  {[]int{10, 10, 10, 10, 10}, "[[f1 f2 f3 f4] [f5]]"},
		"exactly 48":  {[]int{48, 1}, "[[f1] [f2]]"},
		"none":        {nil, "[]"},
		"60 then 10":  {[]int{60, 10}, "[[f1] [f2]]"},
		"49 alone":    {[]int{49}, "[[f1]]"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fmt.Sprint(paths(packFiles(sized(tc.kib...), 48<<10))); got != tc.chunks {
				t.Fatalf("chunks %s; want %s", got, tc.chunks)
			}
		})
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
	if got := fileList([][]fileDiff{files[:2], files[2:]}, "part"); got != want {
		t.Fatalf("file list = %q, want %q", got, want)
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

func TestDiffPartsBoundary(t *testing.T) {
	for name, tc := range map[string]struct {
		max, second, parts int
	}{
		"exactly the limit":    {48 << 10, 18 << 10, 1},
		"one byte past":        {48 << 10, 18<<10 + 1, 2},
		"a larger limit holds": {256 << 10, 18<<10 + 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			parts := diffParts(splitDiff(diffFile("a.go", 30<<10)+diffFile("b.go", tc.second)), tc.max)
			if len(parts) != tc.parts {
				t.Fatalf("parts = %d, want %d", len(parts), tc.parts)
			}
		})
	}
}

func TestOthersOfNamesWhatTheOtherPartsAdd(t *testing.T) {
	a := splitDiff("diff --git a/store.go b/store.go\n--- a/store.go\n+++ b/store.go\n@@ -1,2 +1,5 @@\n context\n-func Old() {}\n+func (s *Store) TasksByPR(ctx int) {}\n+type Server struct {\n+\tmux.HandleFunc(\"GET /api/tasks\", h)\n")
	b := splitDiff("diff --git a/ui.css b/ui.css\n--- a/ui.css\n+++ b/ui.css\n@@ -1,1 +1,3 @@\n+.card-title {\n+  color: red;\n")
	c := splitDiff("diff --git a/c.go b/c.go\n--- /dev/null\n+++ b/c.go\n@@ -0,0 +1,2 @@\n+func Own() {}\n+go func() {\n")
	parts := [][]fileDiff{a, b, c}
	names := newNameIndex(parts)
	if got, want := names.Others(1), "functions: Own, TasksByPR\ntypes: Server\nroutes: /api/tasks"; got != want {
		t.Fatalf("others of part 2 = %q, want %q", got, want)
	}
	if got, want := names.Others(0), "functions: Own\nselectors: .card-title"; got != want {
		t.Fatalf("others of part 1 = %q, want %q", got, want)
	}
	if got, want := names.All(), "functions: Own, TasksByPR\ntypes: Server\nroutes: /api/tasks\nselectors: .card-title"; got != want {
		t.Fatalf("all names = %q, want %q", got, want)
	}
	if got := newNameIndex([][]fileDiff{a}).Others(0); got != "" {
		t.Fatalf("one part has no others, got %q", got)
	}
}

// hunkFile is a modified file of that many hunks, each of about hunkBytes:
// 100 added lines of 102 bytes for 10 KiB, after one context line.
func hunkFile(path string, hunks, hunkBytes int) fileDiff {
	n := hunkBytes / 102
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	for i := range hunks {
		fmt.Fprintf(&b, "@@ -%d,1 +%d,%d @@\n c\n", 10*i+1, 10*i+1+i*n, n+1)
		b.WriteString(strings.Repeat("+"+strings.Repeat("x", 100)+"\n", n))
	}
	return parseFileDiff(b.String())
}

func TestCutFileWholeWhenItFits(t *testing.T) {
	got := cutFile(hunkFile("a.go", 3, 10<<10), 48<<10)
	if len(got) != 1 || got[0].Piece != 0 || got[0].Pieces != 0 {
		t.Fatalf("pieces = %+v", got)
	}
}

func TestCutFileByHunks(t *testing.T) {
	f := hunkFile("big.go", 10, 10<<10)
	head := f.Text[:strings.Index(f.Text, "\n@@ ")+1]
	got := cutFile(f, 48<<10)
	if len(got) != 3 {
		t.Fatalf("%d pieces, want 3", len(got))
	}
	added := 0
	for i, p := range got {
		if want := []int{4, 4, 2}[i]; strings.Count(p.Text, "\n@@ ") != want {
			t.Errorf("piece %d holds %d hunks, want %d", i+1, strings.Count(p.Text, "\n@@ "), want)
		}
		if p.Piece != i+1 || p.Pieces != 3 || !strings.HasPrefix(p.Text, head) || len(p.Text) > 48<<10 || p.Path != "big.go" {
			t.Errorf("piece %d = %d of %d, %d bytes, path %q", i+1, p.Piece, p.Pieces, len(p.Text), p.Path)
		}
		added += p.Added
	}
	if added != f.Added {
		t.Fatalf("pieces add %d lines, the file %d", added, f.Added)
	}
}

func TestCutFileSplitsOneHugeHunk(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/n.go b/n.go\n--- /dev/null\n+++ b/n.go\n@@ -0,0 +1,3000 @@\n")
	var lines []string
	for i := range 3000 {
		l := fmt.Sprintf("+%038d", i)
		lines = append(lines, l)
		b.WriteString(l + "\n")
	}
	// the file header is 50 bytes, a sub-hunk keeps 80 for its header, so
	// 49152 - 50 - 80 = 49022 bytes hold 1225 lines of 40: 1225, 1225 and 550.
	pieces := cutFile(parseFileDiff(b.String()), 48<<10)
	if len(pieces) != 3 || pieces[0].Added != 1225 || pieces[1].Added != 1225 || pieces[2].Added != 550 {
		t.Fatalf("%d pieces: %+v", len(pieces), pieces)
	}
	before := 0
	var joined []string
	for i, p := range pieces {
		if len(p.Text) > 48<<10 {
			t.Fatalf("piece %d is %d bytes", i+1, len(p.Text))
		}
		hdr := fmt.Sprintf("\n@@ -0,0 +%d,%d @@\n", 1+before, p.Added)
		if !strings.Contains(p.Text, hdr) || strings.Count(p.Text, "\n@@ ") != 1 {
			t.Fatalf("piece %d lacks the header %q", i+1, hdr)
		}
		_, body, _ := strings.Cut(p.Text, hdr)
		joined = append(joined, strings.Split(strings.TrimSuffix(body, "\n"), "\n")...)
		before += p.Added
	}
	if before != 3000 || !slices.Equal(joined, lines) {
		t.Fatalf("pieces add %d lines, in order %t", before, slices.Equal(joined, lines))
	}
}

func TestCutFileSplitsAGiantLine(t *testing.T) {
	line := strings.Repeat("€", 100<<10/3)
	f := parseFileDiff("diff --git a/n.go b/n.go\n--- /dev/null\n+++ b/n.go\n@@ -0,0 +1 @@\n+" + line + "\n")
	pieces := cutFile(f, 48<<10)
	if len(pieces) < 3 {
		t.Fatalf("%d pieces, want at least 3", len(pieces))
	}
	var got strings.Builder
	for i, p := range pieces {
		if len(p.Text) > 48<<10 || !utf8.ValidString(p.Text) {
			t.Fatalf("piece %d: %d bytes, valid %t", i+1, len(p.Text), utf8.ValidString(p.Text))
		}
		_, body, _ := strings.Cut(p.Text, " @@\n")
		if !strings.HasPrefix(body, "+") || !strings.HasSuffix(body, "\n") {
			t.Fatalf("piece %d body = %.20q", i+1, body)
		}
		got.WriteString(body[1 : len(body)-1])
	}
	if got.String() != line {
		t.Fatal("the pieces do not make the line again")
	}
}

func TestDiffPartsHasNoCap(t *testing.T) {
	var files []fileDiff
	for i := range 40 {
		files = append(files, fileDiff{Path: fmt.Sprintf("f%d", i), Text: strings.Repeat("x", 20<<10)})
	}
	parts := diffParts(files, 48<<10)
	if len(parts) != 20 || len(parts[0]) != 2 {
		t.Fatalf("%d parts of %d files, want 20 of 2", len(parts), len(parts[0]))
	}
}

func TestDiffPartsOfNoFilesIsOneEmptyPart(t *testing.T) {
	if got := diffParts(nil, 48<<10); !reflect.DeepEqual(got, [][]fileDiff{nil}) {
		t.Fatalf("parts = %#v", got)
	}
}

func TestPartLabelNamesACutFile(t *testing.T) {
	parts := diffParts([]fileDiff{hunkFile("big.go", 13, 10<<10)}, 48<<10)
	if len(parts) != 4 {
		t.Fatalf("%d parts, want 4", len(parts))
	}
	if got, want := partLabel(parts, 1, 1), "file 1 of 1 (big.go, piece 2 of 4), part 2 of 4"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
	mixed := [][]fileDiff{{{Path: "a.go"}}, {{Path: "b.go", Piece: 1, Pieces: 2}}, {{Path: "b.go", Piece: 2, Pieces: 2}, {Path: "c.go"}}}
	for i, want := range []string{"file 1 of 3, part 1 of 3", "file 2 of 3 (b.go, piece 1 of 2), part 2 of 3", "files 2 to 3 of 3, part 3 of 3"} {
		if got := partLabel(mixed, i, 3); got != want {
			t.Errorf("label %d = %q, want %q", i+1, got, want)
		}
	}
}

func TestFileListGroupsTheTailByDirectory(t *testing.T) {
	var files []fileDiff
	for _, dir := range []string{"a", "b"} {
		for i := range 500 {
			files = append(files, fileDiff{Path: fmt.Sprintf("%s/f%03d.go", dir, i), Added: 2, Removed: 1})
		}
	}
	list := fileList([][]fileDiff{files[:600], files[600:]}, "part")
	lines := strings.Split(list, "\n")
	if len(lines) != 202 || len(list) >= 20<<10 || lines[199] != "a/f199.go +2 -1 (part 1)" {
		t.Fatalf("%d lines, %d bytes, line 200 %q", len(lines), len(list), lines[199])
	}
	if lines[200] != "a/ 300 files +600 -300 (part 1)" || lines[201] != "b/ 500 files +1000 -500 (parts 1 to 2)" {
		t.Fatalf("tail = %q", lines[200:])
	}
}

func TestFileListNamesAPiece(t *testing.T) {
	got := fileList([][]fileDiff{{{Path: "big.go", Piece: 2, Pieces: 3, Added: 5}}}, "part")
	if want := "big.go piece 2 of 3 +5 -0 (part 1)"; got != want {
		t.Fatalf("list = %q, want %q", got, want)
	}
}

// A hunk header the cutter cannot read is cut like a file without hunks:
// within the limit.
func TestCutFileCutsAHunkItCannotRead(t *testing.T) {
	f := parseFileDiff("diff --git a/n.go b/n.go\n--- a/n.go\n+++ b/n.go\n@@ garbled @@\n+" + strings.Repeat("x", 150<<10) + "\n")
	pieces := cutFile(f, 48<<10)
	if len(pieces) < 4 {
		t.Fatalf("%d pieces, want at least 4", len(pieces))
	}
	for i, p := range pieces {
		if len(p.Text) > 48<<10 {
			t.Fatalf("piece %d is %d bytes", i+1, len(p.Text))
		}
	}
}

// A cut line is one line: the line after it keeps its true number. Old
// line 1 is the context, the giant line is new line 2, "c2" is old line 2
// and new line 3; the last sub-hunk holds the giant line's last fragment
// and c2, and its header is -2,1 +2,2.
func TestCutFileNumbersAfterAGiantLine(t *testing.T) {
	f := parseFileDiff("diff --git a/n.go b/n.go\n--- a/n.go\n+++ b/n.go\n@@ -1,2 +1,3 @@\n c1\n+" + strings.Repeat("€", 100<<10/3) + "\n c2\n")
	pieces := cutFile(f, 48<<10)
	last := pieces[len(pieces)-1].Text
	hunk := last[strings.LastIndex(last, "\n@@ ")+1:]
	if !strings.HasPrefix(hunk, "@@ -2,1 +2,2 @@\n+") || !strings.HasSuffix(hunk, "\n c2\n") {
		t.Fatalf("last hunk = %.40q ... %q", hunk, hunk[len(hunk)-8:])
	}
}

func TestFileListBoundsTheDirectoryTail(t *testing.T) {
	var files []fileDiff
	for i := range 1000 {
		files = append(files, fileDiff{Path: fmt.Sprintf("d%03d/f.go", i), Added: 1})
	}
	list := fileList([][]fileDiff{files}, "part")
	lines := strings.Split(list, "\n")
	// 200 file lines, 50 directory lines, one closing line: files 201 to 1000
	// are 800 directories, 750 of them past the bound.
	if len(lines) != 251 || lines[249] != "d249/ 1 file +1 -0 (part 1)" || lines[250] != "and 750 more directories with 750 files" {
		t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

// renameDiff is git's diff from base to head, files by path, and the reader
// of either side's .go files in the directories asked for.
func renameDiff(t *testing.T, base, head map[string]string) ([]fileDiff, func(bool, []string) map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "color.ui=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	write := func(files map[string]string) {
		for p, c := range files {
			if err := os.MkdirAll(filepath.Join(dir, path.Dir(p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	git("init", "-q")
	write(base)
	git("add", "-A")
	git("commit", "-qm", "base")
	git("rm", "-rq", ".")
	write(head)
	git("add", "-A")
	git("commit", "-qm", "head")
	read := func(b bool, dirs []string) map[string][]byte {
		side, out := head, map[string][]byte{}
		if b {
			side = base
		}
		for p, c := range side {
			if strings.HasSuffix(p, ".go") && slices.Contains(dirs, path.Dir(p)) {
				out[p] = []byte(c)
			}
		}
		return out
	}
	return splitDiff(git("diff", "-M", "HEAD~1", "HEAD")), read
}

// addedLines are the added lines of files, each as path: line.
func addedLines(files []fileDiff) []string {
	var out []string
	for _, f := range files {
		for _, l := range strings.Split(f.Text, "\n") {
			if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++ ") {
				out = append(out, f.Path+": "+strings.TrimSpace(l[1:]))
			}
		}
	}
	return out
}

// A use of a renamed field becomes the context line that holds the added
// line, its declaration staying; the header stays, as the old and new side
// count the same lines.
func TestQuietRenamesTurnsAUseIntoContext(t *testing.T) {
	const was = "package a\n\ntype runner struct {\n\tkube int\n}\n\nfunc (r *runner) run(x int) {\n\tx = 1\n\tr.kube = x\n\tx++\n}\n"
	files, read := renameDiff(t, map[string]string{"a.go": was},
		map[string]string{"a.go": strings.ReplaceAll(strings.Replace(was, "\tx++\n", "\tx++\n\t_ = x\n", 1), "kube", "Kube")})
	got, counts := quietRenames(files, read)
	want := strings.Replace(files[0].Text, "-\tr.kube = x\n+\tr.Kube = x\n", " \tr.Kube = x\n", 1)
	if len(got) != 1 || got[0].Text != want || want == files[0].Text || got[0].Added != 2 || got[0].Removed != 1 || counts != (RenameCounts{Pairs: 1, Files: 1}) {
		t.Fatalf("quietRenames = %+v, %+v, want the text %q, +2 -1, one pair in one file", got, counts, want)
	}
}

// A file whose one change is a use is left out; of two hunks, the one that
// changes nothing after the use goes and the other stays.
func TestQuietRenamesDropsWhatChangesNothing(t *testing.T) {
	b := "package a\n\nfunc g() {\n\tf(kube)\n}\n" + strings.Repeat("\n", 14) + "func h() {\n\tx := 1\n\t_ = x\n}\n"
	files, read := renameDiff(t,
		map[string]string{"d.go": "package a\n\nvar kube = 1\n\nfunc f(int) {}\n", "a.go": "package a\n\nfunc e() { f(kube) }\n", "b.go": b},
		map[string]string{"d.go": "package a\n\nvar Kube = 1\n\nfunc f(int) {}\n", "a.go": "package a\n\nfunc e() { f(Kube) }\n",
			"b.go": strings.Replace(strings.Replace(b, "f(kube)", "f(Kube)", 1), "\t_ = x\n", "\t_ = x\n\ty := 2\n\t_ = y\n", 1)})
	got, _ := quietRenames(files, read)
	if want := []string{"b.go: _ = y", "b.go: y := 2", "d.go: var Kube = 1"}; !slices.Equal(sorted(addedLines(got)), want) || len(got) != 2 ||
		strings.Count(got[0].Text, "\n@@ ") != 1 || got[0].Path != "b.go" || got[0].Added != 2 || got[0].Removed != 0 {
		t.Fatalf("quietRenames = %+v, want b.go's second hunk and d.go alone, changing %v", got, want)
	}
}

func sorted(s []string) []string {
	slices.Sort(s)
	return s
}

// A removed line and the added line that differs from it in a literal, in
// a number, or a block of two removed lines against one added line, stay
// as they are.
func TestQuietRenamesKeepsARealChange(t *testing.T) {
	for name, tc := range map[string][2]string{
		"literal": {"package a\n\nfunc f() string { return \"kube\" }\n", "package a\n\nfunc f() string { return \"Kube\" }\n"},
		"number":  {"package a\n\nvar x = 1\n", "package a\n\nvar x = 2\n"},
		"2 for 1": {"package a\n\nvar r struct{ kube, b int }\n\nfunc f() {\n\tr.kube = 1\n\tr.b = 2\n}\n",
			"package a\n\nvar r struct{ kube, b int }\n\nfunc f() {\n\tr.Kube = 1\n}\n"},
	} {
		files, read := renameDiff(t, map[string]string{"a.go": tc[0]}, map[string]string{"a.go": tc[1]})
		if got, _ := quietRenames(files, read); !reflect.DeepEqual(got, files) {
			t.Errorf("%s: quietRenames = %+v, want the diff as it is", name, got)
		}
	}
}

// Of three pairs, the use becomes context in its place and the real
// changes stay, a removed run before its added run.
func TestQuietRenamesKeepsAMixedHunk(t *testing.T) {
	for name, tc := range map[string]struct{ was, is, want string }{
		"between": {"\ty := 1\n\tr.kube = 0\n\tz := 1\n", "\ty := 2\n\tr.Kube = 0\n\tz := 2\n",
			"-\ty := 1\n+\ty := 2\n \tr.Kube = 0\n-\tz := 1\n+\tz := 2\n"},
		"last": {"\ty := 1\n\tz := 1\n\tr.kube = 0\n", "\ty := 2\n\tz := 2\n\tr.Kube = 0\n",
			"-\ty := 1\n-\tz := 1\n+\ty := 2\n+\tz := 2\n \tr.Kube = 0\n"},
	} {
		fn := "package a\n\nvar r struct{ %s int }\n\nfunc f() {\n%s\t_, _ = y, z\n}\n"
		files, read := renameDiff(t, map[string]string{"a.go": fmt.Sprintf(fn, "kube", tc.was)}, map[string]string{"a.go": fmt.Sprintf(fn, "Kube", tc.is)})
		got, _ := quietRenames(files, read)
		if len(got) != 1 || !strings.Contains(got[0].Text, tc.want) || got[0].Added != 3 || got[0].Removed != 3 {
			t.Errorf("%s: quietRenames = %+v, want the block %q, +3 -3", name, got, tc.want)
		}
	}
}

// A rename of the file is no hunk: its header stays when its one hunk was
// a use.
func TestQuietRenamesKeepsAFileRename(t *testing.T) {
	body := "package a\n\n// f calls g once.\nfunc f() {\n\tg(%s)\n}\n\n// g takes one int.\nfunc g(int) {}\n\n// h does nothing.\nfunc h() {}\n"
	files, read := renameDiff(t, map[string]string{"d.go": "package a\n\nvar kube = 1\n", "old.go": fmt.Sprintf(body, "kube")},
		map[string]string{"d.go": "package a\n\nvar Kube = 1\n", "new.go": fmt.Sprintf(body, "Kube")})
	got, _ := quietRenames(files, read)
	i := slices.IndexFunc(got, func(f fileDiff) bool { return f.Path == "new.go" })
	if i < 0 || strings.Contains(got[i].Text, "@@") || !strings.Contains(got[i].Text, "rename from old.go\nrename to new.go\n") || got[i].OldPath != "old.go" {
		t.Fatalf("quietRenames = %+v, want new.go's header alone", got)
	}
}

// renameCase is testdata/renames/name's base and head trees, path to
// content.
func renameCase(t *testing.T, name string) (base, head map[string]string) {
	t.Helper()
	base, head = map[string]string{}, map[string]string{}
	for side, files := range map[string]map[string]string{"base": base, "head": head} {
		root := filepath.Join("testdata", "renames", name, side)
		if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			files[filepath.ToSlash(rel)] = string(b)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return base, head
}

// A use is quiet only where it denotes the declaration a pair renames at
// both sides of the diff. Each case base and head compiles: a decoy that
// makes the new spelling name another object (a twin in base, a local
// shadow, a method or field that moves to another type or depth, a twin
// in another package, a sibling block) keeps the use; uses of the renamed
// declaration where Go resolves them to it are quiet. The visible lines
// stay added; the quiet ones are context.
func TestQuietRenamesKeepsEveryUseADecoyReaches(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pairs          int
		visible, quiet []string
	}{
		{"a1_init", 0, []string{"x/i.go: func Init() {"}, nil},
		{"a2_test", 0, []string{"x/egress_test.go: func testEgressRefusesPrivate(t *testing.T) {"}, nil},
		{"a3_methods", 1, []string{`x/m.go: func (s secret) logValue() slog.Value { return slog.StringValue("[redacted]") }`,
			`x/m.go: func (s secret) marshalJSON() ([]byte, error) { return json.Marshal("[redacted]") }`,
			"x/m.go: func (g gate) serveHTTP(w http.ResponseWriter, r *http.Request) {", "x/m.go: func (d denied) unwrap() error { return d.err }"},
			[]string{"x/m.go: return s.logValue(), d.unwrap()"}},
		{"a4_field", 1, []string{"x/c.go: Token string"}, []string{`x/c.go: func header(c creds) string { return "Bearer " + c.Token }`}},
		{"a5a_use_alone", 0, []string{"x/proxy.go: if !Allow(host) {"}, nil},
		{"a5b_use_decoy_local_samefile", 1, []string{"x/proxy.go: if !Allow(host) {", "x/proxy.go: Allow := 3"}, []string{"x/proxy.go: return Allow"}},
		{"a5c_use_decoy_xtest_samedir", 2, []string{"x/proxy.go: if !Allow(host) {", "x/proxy_ext_test.go: var Allow = 1"},
			[]string{"x/proxy_ext_test.go: if Allow != 1 {", "x/proxy_ext_test.go: t.Fatal(Allow)"}},
		{"a5d_use_decoy_otherdir", 1, []string{"x/proxy.go: if !Allow(host) {"}, []string{"y/y.go: func Level() int { return Allow }"}},
		{"a5e_use_decoy_field_samedir", 1, []string{"x/proxy.go: if !Allow(host) {"}, []string{"x/opts.go: func (o opts) on() bool { return o.Allow }"}},
		{"d_declaration_forms", 7, []string{`x/d.go: Str "strings"`, "x/d.go: func run[Elem any](xs []Elem) (Err error) {"},
			[]string{"x/d.go: break Loop", "x/d.go: return Err", "x/d.go: f := (*set[int]).Add", "x/d.go: k.Hook = nil", "x/d.go: _ = TokenA + tokenB"}},
		{"d_funcvar_assign", 1, nil, []string{"x/h.go: hook = Allow"}},
		{"n1_embedded_depth", 1, []string{`x/pod.go: "privileged":   s.Privileged,`}, []string{"x/pod.go: func ciClass() limits { return limits{Privileged: true, cpu: 4} }"}},
		{"n1b_method_override", 1, []string{"x/upload.go: return g.Send(host)"}, []string{"x/transport.go: return g.transport.Send(host)"}},
		{"n2_receiver_type", 1, []string{`x/log.go: func (t Token) LogValue() slog.Value { return slog.StringValue("[redacted]") }`}, []string{"x/log.go: return Token"}},
		{"n2b_conversion_twin", 1, []string{`x/a.go: log.Printf("auth %s", Token(raw))`}, []string{"x/a.go: return Token"}},
		{"n4_package_clause", 1, []string{"egress/e.go: package Egress"}, []string{"egress/e.go: func Level() int { return Egress }"}},
		{"p_factory", 0, nil, nil},
		{"p_github", 0, nil, nil},
		{"p_move_in", 0, nil, nil},
		{"p_move_out", 0, nil, nil},
		{"p_nested_factory", 1, nil, nil},
		{"s1_shadow_local", 0, []string{"x/a.go: return Allow(host)"}, nil},
		{"s2_selector_twin", 0, []string{"x/a.go: return c.Egress, a.Egress"}, nil},
		{"t1_sibling_block", 1, []string{"x/proxy.go: if !Allow(host) {"}, []string{"x/proxy.go: _ = Allow"}},
	} {
		base, head := renameCase(t, tc.name)
		files, read := renameDiff(t, base, head)
		got, counts := quietRenames(files, read)
		left, all := addedLines(got), addedLines(files)
		for _, l := range tc.visible {
			if !slices.Contains(left, l) {
				t.Errorf("%s: %q is quiet or absent, want it visible; left %q", tc.name, l, left)
			}
		}
		for _, l := range tc.quiet {
			if slices.Contains(left, l) || !slices.Contains(all, l) {
				t.Errorf("%s: %q is visible or absent, want it quiet", tc.name, l)
			}
		}
		if counts.Pairs != tc.pairs {
			t.Errorf("%s: %d pairs quiet, want %d; left %q", tc.name, counts.Pairs, tc.pairs, left)
		}
	}
}

// More decoys, each compiling at both sides: a map literal's key that
// names a twin in base while a field of its spelling is renamed; a member
// reached through a type of another package, which could hold the new
// spelling no deeper (sync.Mutex's Lock), against one at depth 0 that
// wins over it; uses of a generic type's renamed field; a method
// receiver's type, which moves the method; a use in a file that imports
// with a dot.
func TestQuietRenamesKeepsAUseItCannotProve(t *testing.T) {
	for _, tc := range []struct {
		name, base, head string
		pairs            int
		visible, quiet   []string
	}{
		{"a map key",
			"package x\n\nvar allow = \"internal\"\n\nconst Allow = \"any\"\n\ntype opts struct{ allow bool }\n\nfunc on(o opts) bool { return o.allow }\n\nvar hosts = map[string]bool{allow: true}\n",
			"package x\n\nvar allow = \"internal\"\n\nconst Allow = \"any\"\n\ntype opts struct{ Allow bool }\n\nfunc on(o opts) bool { return o.Allow }\n\nvar hosts = map[string]bool{Allow: true}\n",
			1, []string{"x/a.go: var hosts = map[string]bool{Allow: true}"}, []string{"x/a.go: func on(o opts) bool { return o.Allow }"}},
		{"another package's type on the way",
			"package x\n\nimport \"sync\"\n\ntype deep struct{ lock int }\n\ntype mid struct{ deep }\n\ntype outer struct {\n\tsync.Mutex\n\tmid\n}\n\ntype near struct {\n\tsync.Mutex\n\tlock int\n}\n\nfunc f(o outer, n near) {\n\t_ = o.lock\n\t_ = n.lock\n}\n",
			"package x\n\nimport \"sync\"\n\ntype deep struct{ Lock int }\n\ntype mid struct{ deep }\n\ntype outer struct {\n\tsync.Mutex\n\tmid\n}\n\ntype near struct {\n\tsync.Mutex\n\tLock int\n}\n\nfunc f(o outer, n near) {\n\t_ = o.Lock\n\t_ = n.Lock\n}\n",
			1, []string{"x/a.go: _ = o.Lock"}, []string{"x/a.go: _ = n.Lock"}},
		{"a generic type",
			"package x\n\ntype box[T any] struct{ kube T }\n\nfunc f() int {\n\tb := box[int]{kube: 1}\n\treturn b.kube\n}\n",
			"package x\n\ntype box[T any] struct{ Kube T }\n\nfunc f() int {\n\tb := box[int]{Kube: 1}\n\treturn b.Kube\n}\n",
			2, nil, []string{"x/a.go: b := box[int]{Kube: 1}", "x/a.go: return b.Kube"}},
		{"a method receiver's type",
			"package x\n\ntype token string\n\nfunc (t token) LogValue() string { return \"[redacted]\" }\n",
			"package x\n\ntype Token string\n\nfunc (t Token) LogValue() string { return \"[redacted]\" }\n",
			0, []string{`x/a.go: func (t Token) LogValue() string { return "[redacted]" }`}, nil},
		{"a dot import",
			"package x\n\nimport . \"strings\"\n\nvar kube = \"k\"\n\nfunc f() string { return ToUpper(kube) }\n",
			"package x\n\nimport . \"strings\"\n\nvar Kube = \"k\"\n\nfunc f() string { return ToUpper(Kube) }\n",
			0, []string{"x/a.go: func f() string { return ToUpper(Kube) }"}, nil},
	} {
		files, read := renameDiff(t, map[string]string{"x/a.go": tc.base}, map[string]string{"x/a.go": tc.head})
		got, counts := quietRenames(files, read)
		left, all := addedLines(got), addedLines(files)
		for _, l := range tc.visible {
			if !slices.Contains(left, l) {
				t.Errorf("%s: %q is quiet or absent, want it visible; left %q", tc.name, l, left)
			}
		}
		for _, l := range tc.quiet {
			if slices.Contains(left, l) || !slices.Contains(all, l) {
				t.Errorf("%s: %q is visible or absent, want it quiet", tc.name, l)
			}
		}
		if counts.Pairs != tc.pairs {
			t.Errorf("%s: %d pairs quiet, want %d; left %q", tc.name, counts.Pairs, tc.pairs, left)
		}
	}
}

// A use stays where a build that leaves a file out reads it otherwise: a
// method of T in a test file, in a file of one platform, in a file a build
// line selects, in a cgo file and in a file go build skips renames T's own
// allow, while a build without it reaches E's allow at the base and F's
// Allow at the head. A use is quiet where every build reads it alike: the
// method in a file every build reads, a use in a test file of a method a
// test file declares, a file of one platform that declares a function.
// Last, E's allow is renamed while a file of one platform keeps T's: the
// use reaches E's at the head in every build, but T's at the base in that
// platform's.
func TestQuietRenamesKeepsAUseAnotherBuildReadsOtherwise(t *testing.T) {
	const types = "package x\n\ntype E struct{}\n\nfunc (E) allow() bool { return false }\n\ntype F struct{}\n\nfunc (F) Allow() bool { return true }\n\ntype T struct {\n\tE\n\tF\n}\n"
	const use = "x/u.go: func check(t T) bool { return t.Allow() }"
	for _, tc := range []struct {
		name, file, head, user string
		pairs                  int
	}{
		{"a test file", "x/m_test.go", "package x\n", "x/u.go", 0},
		{"a file of one platform", "x/m_linux.go", "package x\n", "x/u.go", 0},
		{"a build line", "x/m.go", "//go:build !darwin\n\npackage x\n", "x/u.go", 0},
		{"a cgo file", "x/m.go", "package x\n\nimport \"C\"\n", "x/u.go", 0},
		{"a file go build skips", "x/_m.go", "package x\n", "x/u.go", 0},
		{"a file every build reads", "x/m.go", "package x\n", "x/u.go", 1},
		{"a use in a test file", "x/m_test.go", "package x\n", "x/u_test.go", 1},
		{"a platform's function", "x/m.go", "package x\n", "x/u.go", 1},
	} {
		base := map[string]string{"x/t.go": types, tc.file: tc.head + "\nfunc (T) allow() bool { return false }\n",
			tc.user: "package x\n\nfunc check(t T) bool { return t.allow() }\n"}
		head := map[string]string{"x/t.go": types, tc.file: tc.head + "\nfunc (T) Allow() bool { return false }\n",
			tc.user: "package x\n\nfunc check(t T) bool { return t.Allow() }\n"}
		if tc.name == "a platform's function" {
			base["x/p_unix.go"] = "//go:build unix\n\npackage x\n\nfunc own() {}\n"
			head["x/p_unix.go"] = base["x/p_unix.go"]
		}
		files, read := renameDiff(t, base, head)
		got, counts := quietRenames(files, read)
		if counts.Pairs != tc.pairs {
			t.Errorf("%s: %d pairs quiet, want %d; left %q", tc.name, counts.Pairs, tc.pairs, addedLines(got))
		}
		if tc.pairs == 0 && !slices.Contains(addedLines(got), use) {
			t.Errorf("%s: %q is quiet, want it visible", tc.name, use)
		}
	}
	const was = "package x\n\ntype E struct{}\n\nfunc (E) allow() bool { return false }\n\ntype T struct{ E }\n"
	files, read := renameDiff(t,
		map[string]string{"x/t.go": was, "x/m_linux.go": "package x\n\nfunc (T) allow() bool { return true }\n",
			"x/u.go": "package x\n\nfunc check(t T) bool { return t.allow() }\n"},
		map[string]string{"x/t.go": strings.ReplaceAll(was, "allow", "Allow"), "x/m_linux.go": "package x\n\nfunc (T) allow() bool { return true }\n",
			"x/u.go": "package x\n\nfunc check(t T) bool { return t.Allow() }\n"})
	if got, counts := quietRenames(files, read); counts.Pairs != 0 || !slices.Contains(addedLines(got), use) {
		t.Errorf("a method of one platform's file, unchanged: %d pairs quiet, want the use visible; left %q", counts.Pairs, addedLines(got))
	}
}

// A use stays where a side cannot be read whole: an unread base or head, a
// line the side holds otherwise, a file of the package go/parser cannot
// read, a line inside a literal of more than one line; and wherever the
// checklist judges the path or the file is no Go at both sides.
func TestQuietRenamesReadsBothSides(t *testing.T) {
	base := map[string]string{"x/d.go": "package x\n\nvar kube = 1\n", "x/a.go": "package x\n\nfunc f() int { return kube }\n"}
	head := map[string]string{"x/d.go": "package x\n\nvar Kube = 1\n", "x/a.go": "package x\n\nfunc f() int { return Kube }\n"}
	files, read := renameDiff(t, base, head)
	if _, counts := quietRenames(files, read); counts.Pairs != 1 {
		t.Fatalf("%d pairs quiet with both sides read, want 1", counts.Pairs)
	}
	for name, side := range map[string]func(bool, []string) map[string][]byte{
		"an unread base": func(b bool, dirs []string) map[string][]byte {
			if b {
				return nil
			}
			return read(b, dirs)
		},
		"an unread head": func(b bool, dirs []string) map[string][]byte {
			if !b {
				return nil
			}
			return read(b, dirs)
		},
		"another line at the head": func(b bool, dirs []string) map[string][]byte {
			out := read(b, dirs)
			if !b {
				out["x/a.go"] = []byte("package x\n\nfunc f() int { return Kube + 0 }\n")
			}
			return out
		},
		"a file of the base go/parser cannot read": func(b bool, dirs []string) map[string][]byte {
			out := read(b, dirs)
			if b {
				out["x/e.go"] = []byte("package x\n\nfunc {\n")
			}
			return out
		},
	} {
		if got, _ := quietRenames(files, side); !reflect.DeepEqual(got, files) {
			t.Errorf("%s: quietRenames = %+v, want the diff as it is", name, got)
		}
	}
	for name, tc := range map[string][2]map[string]string{
		"a raw literal of lines":      {{"x/a.go": "package x\n\nvar kube = 1\n\nconst m = `\nx: kube\n`\n"}, {"x/a.go": "package x\n\nvar Kube = 1\n\nconst m = `\nx: Kube\n`\n"}},
		"a path the checklist judges": {{"factory/d.go": base["x/d.go"], "factory/a.go": base["x/a.go"]}, {"factory/d.go": head["x/d.go"], "factory/a.go": head["x/a.go"]}},
		"Go from a text file":         {{"x/d.go": base["x/d.go"], "x/a.txt": base["x/a.go"]}, {"x/d.go": head["x/d.go"], "x/a.go": head["x/a.go"]}},
	} {
		files, read := renameDiff(t, tc[0], tc[1])
		if _, counts := quietRenames(files, read); counts.Pairs != 0 {
			t.Errorf("%s: %d pairs quiet, want none", name, counts.Pairs)
		}
	}
}

// The line rule: two lines are a case rename when each byte they differ
// in is the first ASCII letter of an identifier both hold, flipped in
// case, and go/scanner reads both whole; the renames name each identifier
// with its spellings.
func TestCaseRenames(t *testing.T) {
	for _, tc := range []struct {
		name, removed, added string
		want                 []caseRename
	}{
		{"a field key", "\te.runner = &Runner{kube: kube}", "\te.runner = &Runner{Kube: kube}", []caseRename{{20, "kube", "Kube"}}},
		{"after a literal holding //", "\tf(\"http://x\", kube)", "\tf(\"http://x\", Kube)", []caseRename{{15, "kube", "Kube"}}},
		{"after the rune literal '\"'", "\ts := '\"' + kube + \"x\"", "\ts := '\"' + Kube + \"x\"", []caseRename{{12, "kube", "Kube"}}},
		{"beside a literal key", "\tm := map[string]int{\"kube\": kube}", "\tm := map[string]int{\"kube\": Kube}", []caseRename{{29, "kube", "Kube"}}},
		{"two names", "\tr.kube.refusal(x)", "\tr.Kube.Refusal(x)", []caseRename{{3, "kube", "Kube"}, {8, "refusal", "Refusal"}}},
		{"inside a literal holding //", "\tx := \"a // kube\"", "\tx := \"a // Kube\"", nil},
		{"in a literal after the rune literal '\"'", "\ts := '\"' + \"kube\"", "\ts := '\"' + \"Kube\"", nil},
		{"in a line comment", "\tx() // kube.Run()", "\tx() // Kube.Run()", nil},
		{"in a block comment", "\t/* kube */ f()", "\t/* Kube */ f()", nil},
		{"an unclosed block comment", "\t/* kube", "\t/* Kube", nil},
		{"an unclosed raw literal", "\tx := `kube", "\tx := `Kube", nil},
		{"an unclosed literal", "\tx := \"kube", "\tx := \"Kube", nil},
		{"a character Go does not know", "\t$kube", "\t$Kube", nil},
		{"a letter past the first", "\tuserID := 1", "\tuserId := 1", nil},
		{"all but the first", "\tURL := 1", "\tUrl := 1", nil},
		{"a letter outside ASCII", "\tärger := 1", "\tÄrger := 1", nil},
		{"the Kelvin sign", "\tkube := 1", "\tKube := 1", nil},
		{"a keyword", "\tfunc f() {}", "\tFunc f() {}", nil},
		{"a predeclared name", "\treturn nil", "\treturn Nil", nil},
		{"a builtin", "\tdefer close(ch)", "\tdefer Close(ch)", nil},
		{"a test", "\tTestKube(t)", "\ttestKube(t)", nil},
		{"white space", "\ta := kube", "\ta :=  Kube", nil},
		{"nothing", "\ta := kube", "\ta := kube", nil},
	} {
		if got := caseRenames(tc.removed, tc.added); !slices.Equal(got, tc.want) {
			t.Errorf("%s: caseRenames = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestEntryName(t *testing.T) {
	for name, want := range map[string]bool{"init": true, "main": true, "Test": true, "TestX": true, "Test_x": true, "BenchmarkX": true,
		"FuzzX": true, "Example": true, "ExampleX": true, "Testing": false, "Examples": false, "Init": false, "test": false} {
		if got := entryName(name); got != want {
			t.Errorf("entryName(%q) = %t, want %t", name, got, want)
		}
	}
}
