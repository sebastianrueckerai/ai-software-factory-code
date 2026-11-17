package factoryd

import (
	"fmt"
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

// goDiff is the diff of path, its header and hunks.
func goDiff(path, hunks string) []fileDiff {
	return splitDiff("diff --git a/" + path + " b/" + path + "\nindex 1111111..2222222 100644\n--- a/" + path + "\n+++ b/" + path + "\n" + hunks)
}

// headOf reads files, path to content at the head; nil for any other path.
func headOf(files map[string]string) func(string) []byte {
	return func(p string) []byte {
		if c, ok := files[p]; ok {
			return []byte(c)
		}
		return nil
	}
}

// The pair at line 11 becomes the context line that holds the added line;
// the header stays, as old and new side still count 3 and 4 lines.
func TestQuietRenamesTurnsACaseRenameIntoContext(t *testing.T) {
	files := goDiff("a.go", "@@ -10,3 +10,4 @@\n a\n-\tr.kube.Create(x)\n+\tr.Kube.Create(x)\n b\n+\tc()\n")
	got := quietRenames(files, headOf(map[string]string{"a.go": strings.Repeat("\n", 9) + "a\n\tr.Kube.Create(x)\nb\n\tc()\n"}))
	want := strings.Replace(files[0].Text, "@@ -10,3 +10,4 @@\n a\n-\tr.kube.Create(x)\n+\tr.Kube.Create(x)\n", "@@ -10,3 +10,4 @@\n a\n \tr.Kube.Create(x)\n", 1)
	if len(got) != 1 || got[0].Text != want || got[0].Added != 1 || got[0].Removed != 0 || got[0].Path != "a.go" {
		t.Fatalf("quietRenames = %+v, want the text %q, +1 -0", got, want)
	}
}

// A file whose one hunk is a rename is left out; of two hunks, the one
// that changes nothing after the rename goes and the other stays as it is.
func TestQuietRenamesDropsWhatChangesNothing(t *testing.T) {
	head := headOf(map[string]string{"a.go": "package a\nvar Kube = 1\n" + strings.Repeat("\n", 17) + "\tx := 1\n\ty := 2\n}\n"})
	if got := quietRenames(goDiff("a.go", "@@ -2 +2 @@\n-var kube = 1\n+var Kube = 1\n"), head); len(got) != 0 {
		t.Fatalf("quietRenames = %+v, want no file", got)
	}
	files := goDiff("a.go", "@@ -1,3 +1,3 @@\n package a\n-var kube = 1\n+var Kube = 1\n \n@@ -20,2 +20,3 @@ func f() {\n \tx := 1\n+\ty := 2\n }\n")
	got := quietRenames(files, head)
	want := "diff --git a/a.go b/a.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/a.go\n@@ -20,2 +20,3 @@ func f() {\n \tx := 1\n+\ty := 2\n }\n"
	if len(got) != 1 || got[0].Text != want || got[0].Added != 1 || got[0].Removed != 0 {
		t.Fatalf("quietRenames = %+v, want the second hunk alone", got)
	}
}

// A removed line and the added line that differs from it in a literal, in
// a number, or a block of two removed lines against one added line, stay
// as they are.
func TestQuietRenamesKeepsARealChange(t *testing.T) {
	for name, hunk := range map[string]string{
		"literal": "@@ -1 +1 @@\n-\treturn \"kube\"\n+\treturn \"Kube\"\n",
		"number":  "@@ -1 +1 @@\n-\tx := 1\n+\tx := 2\n",
		"2 for 1": "@@ -1,2 +1 @@\n-\tr.kube.A()\n-\tr.kube.B()\n+\tr.Kube.A()\n",
	} {
		files := goDiff("a.go", hunk)
		_, added, _ := strings.Cut(hunk, "\n+")
		if got := quietRenames(files, headOf(map[string]string{"a.go": added})); !reflect.DeepEqual(got, files) {
			t.Errorf("%s: quietRenames = %+v, want the diff as it is", name, got)
		}
	}
}

// Of three pairs, the rename becomes context in its place and the real
// changes stay, a removed run before its added run.
func TestQuietRenamesKeepsAMixedHunk(t *testing.T) {
	for name, tc := range map[string]struct{ hunk, head, want string }{
		"between": {
			"@@ -1,4 +1,4 @@\n-\ty := 1\n-\tr.kube.Create(x)\n-\tz := 1\n+\ty := 2\n+\tr.Kube.Create(x)\n+\tz := 2\n }\n",
			"\ty := 2\n\tr.Kube.Create(x)\n\tz := 2\n}\n",
			"@@ -1,4 +1,4 @@\n-\ty := 1\n+\ty := 2\n \tr.Kube.Create(x)\n-\tz := 1\n+\tz := 2\n }\n",
		},
		"last": {
			"@@ -1,4 +1,4 @@\n-\ty := 1\n-\tz := 1\n-\tr.kube.Create(x)\n+\ty := 2\n+\tz := 2\n+\tr.Kube.Create(x)\n }\n",
			"\ty := 2\n\tz := 2\n\tr.Kube.Create(x)\n}\n",
			"@@ -1,4 +1,4 @@\n-\ty := 1\n-\tz := 1\n+\ty := 2\n+\tz := 2\n \tr.Kube.Create(x)\n }\n",
		},
	} {
		got := quietRenames(goDiff("a.go", tc.hunk), headOf(map[string]string{"a.go": tc.head}))
		if len(got) != 1 || !strings.HasSuffix(got[0].Text, "+++ b/a.go\n"+tc.want) || got[0].Added != 2 || got[0].Removed != 2 {
			t.Errorf("%s: quietRenames = %+v, want the hunk %q, +2 -2", name, got, tc.want)
		}
	}
}

// A rename of the file, or a mode change, is no hunk: its header stays
// when its one hunk was a case rename, and a file renamed without a hunk
// stays as it is.
func TestQuietRenamesKeepsAFileRename(t *testing.T) {
	bare := splitDiff("diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n")
	if got := quietRenames(bare, headOf(nil)); !reflect.DeepEqual(got, bare) {
		t.Fatalf("quietRenames = %+v, want the rename as it is", got)
	}
	for name, header := range map[string]string{
		"rename": "diff --git a/old.go b/new.go\nsimilarity index 90%\nrename from old.go\nrename to new.go\nindex 1111111..2222222 100644\n--- a/old.go\n+++ b/new.go\n",
		"mode":   "diff --git a/new.go b/new.go\nold mode 100644\nnew mode 100755\nindex 1111111..2222222\n--- a/new.go\n+++ b/new.go\n",
	} {
		got := quietRenames(splitDiff(header+"@@ -1 +1 @@\n-var kube = 1\n+var Kube = 1\n"), headOf(map[string]string{"new.go": "var Kube = 1\n"}))
		if len(got) != 1 || got[0].Text != header || got[0].Path != "new.go" || got[0].Added != 0 || got[0].Removed != 0 {
			t.Errorf("%s: quietRenames = %+v, want the header alone", name, got)
		}
	}
}

// The pairs go/scanner reads as a rename of identifiers by the case of
// their first ASCII letter, and nothing else, are quiet; every other pair
// stays, the ones a tokenizer that reads less than Go would misread too.
func TestQuietRenamesReadsTheLineAsGo(t *testing.T) {
	for _, tc := range []struct {
		name, removed, added string
		quiet                bool
	}{
		{"a selector and a field key", "\te.runner = &Runner{kube: kube}", "\te.runner = &Runner{Kube: kube}", true},
		{"after a literal holding //", "\tf(\"http://x\", kube)", "\tf(\"http://x\", Kube)", true},
		{"after the rune literal '\"'", "\ts := '\"' + kube + \"x\"", "\ts := '\"' + Kube + \"x\"", true},
		{"beside a literal key", "\t\"kube\": kube,", "\t\"kube\": Kube,", true},
		{"two names", "\tr.kube.refusal(x)", "\tr.Kube.Refusal(x)", true},
		{"inside a literal holding //", "\tx := \"a // kube\"", "\tx := \"a // Kube\"", false},
		{"in a literal after the rune literal '\"'", "\ts := '\"' + \"kube\"", "\ts := '\"' + \"Kube\"", false},
		{"in a line comment", "\tx() // kube.Run()", "\tx() // Kube.Run()", false},
		{"in a block comment", "\t/* kube */ f()", "\t/* Kube */ f()", false},
		{"an unclosed block comment", "\t/* kube", "\t/* Kube", false},
		{"an unclosed raw literal", "\tx := `kube", "\tx := `Kube", false},
		{"an unclosed literal", "\tx := \"kube", "\tx := \"Kube", false},
		{"a character Go does not know", "\t$kube", "\t$Kube", false},
		{"a letter past the first", "\tuserID := 1", "\tuserId := 1", false},
		{"all but the first", "\tURL := 1", "\tUrl := 1", false},
		{"a letter outside ASCII", "\tärger := 1", "\tÄrger := 1", false},
		{"the Kelvin sign", "\tkube := 1", "\t\u212aube := 1", false},
		{"a keyword", "\tfunc f() {}", "\tFunc f() {}", false},
		{"a predeclared name", "\treturn nil", "\treturn Nil", false},
		{"a builtin", "\tdefer close(ch)", "\tdefer Close(ch)", false},
		{"white space", "\ta := kube", "\ta :=  Kube", false},
		{"nothing", "\ta := kube", "\ta := kube", false},
	} {
		files := goDiff("a.go", "@@ -1 +1 @@\n-"+tc.removed+"\n+"+tc.added+"\n")
		got := quietRenames(files, headOf(map[string]string{"a.go": tc.added + "\n"}))
		if quiet := len(got) == 0; quiet != tc.quiet || !quiet && !reflect.DeepEqual(got, files) {
			t.Errorf("%s: quietRenames = %+v, want quiet %t", tc.name, got, tc.quiet)
		}
	}
}

// A pair stays where the head does not show it as code at its line: inside
// a literal or a comment of more than one line, whatever //line directive
// says the lines are, at a line that holds something else, in a file
// unread or one go/scanner cannot read. A file whose path does not end in
// .go, or whose old one does not, stays, and so does a rename to a name an
// added line of the diff brings.
func TestQuietRenamesReadsTheLineAtTheHead(t *testing.T) {
	pair := "@@ -4 +4 @@\n-from: selector\n+from: Selector\n"
	for _, tc := range []struct {
		name  string
		files []fileDiff
		head  map[string]string
	}{
		{"a raw literal of lines", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\nconst m = `\nfrom: Selector\n`\n"}},
		{"a //line directive", goDiff("a.go", pair), map[string]string{"a.go": "package a\n//line x.go:1\nconst m = `\nfrom: Selector\n`\n"}},
		{"a block comment of lines", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\n/*\nfrom: Selector\n*/\n"}},
		{"another line at the head", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\n\nfrom: selector\n"}},
		{"an unread head", goDiff("a.go", pair), nil},
		{"a head go/scanner cannot read", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\n\nfrom: Selector\n`\n"}},
		{"YAML", goDiff("k8s/a.yaml", pair), map[string]string{"k8s/a.yaml": "a:\n\n\nfrom: Selector\n"}},
		{"Go from a text file", splitDiff("diff --git a/a.txt b/a.go\nsimilarity index 90%\nrename from a.txt\nrename to a.go\n--- a/a.txt\n+++ b/a.go\n" + pair),
			map[string]string{"a.go": "package a\n\n\nfrom: Selector\n"}},
		{"a name the diff adds", slices.Concat(goDiff("a.go", pair), goDiff("b.go", "@@ -1,0 +2 @@\n+var Selector = wide()\n")),
			map[string]string{"a.go": "package a\n\n\nfrom: Selector\n", "b.go": "package a\nvar Selector = wide()\n"}},
	} {
		if got := quietRenames(tc.files, headOf(tc.head)); !reflect.DeepEqual(got, tc.files) {
			t.Errorf("%s: quietRenames = %+v, want the diff as it is", tc.name, got)
		}
	}
	if got := quietRenames(goDiff("a.go", pair), headOf(map[string]string{"a.go": "package a\n\n\nfrom: Selector\n"})); len(got) != 0 {
		t.Fatalf("quietRenames = %+v, want the pair quiet where the head reads it as code", got)
	}
}
