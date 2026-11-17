package factoryd

import (
	"fmt"
	"maps"
	"path"
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

// declHunks rename, in d.go of declHead, kube to Kube (a package-level
// var and a field of T), refusal to Refusal (a field) and allow to Allow
// (a package-level func): the declarations a use of them needs.
const (
	declHunks = "@@ -3 +3 @@\n-var kube = 1\n+var Kube = 1\n@@ -6,2 +6,2 @@ type T struct {\n-\tkube    int\n-\trefusal func(int)\n+\tKube    int\n+\tRefusal func(int)\n" +
		"@@ -10 +10 @@\n-func allow(host string) bool { return true }\n+func Allow(host string) bool { return true }\n"
	declHead = "package a\n\nvar Kube = 1\n\ntype T struct {\n\tKube    int\n\tRefusal func(int)\n}\n\nfunc Allow(host string) bool { return true }\n"
)

// useIn is a.go's diff of one line of f's body, line 4, and a.go at the
// head.
func useIn(removed, added string) ([]fileDiff, string) {
	return goDiff("a.go", "@@ -4 +4 @@\n-"+removed+"\n+"+added+"\n"), "package a\n\nfunc f() {\n" + added + "\n}\n"
}

// fileAt is the file of files at path p.
func fileAt(files []fileDiff, p string) (fileDiff, bool) {
	i := slices.IndexFunc(files, func(f fileDiff) bool { return f.Path == p })
	if i < 0 {
		return fileDiff{}, false
	}
	return files[i], true
}

// A use of a renamed field becomes the context line that holds the added
// line, its declaration staying; the header stays, as old and new side
// still count 4 and 5 lines.
func TestQuietRenamesTurnsAUseIntoContext(t *testing.T) {
	decl := goDiff("d.go", declHunks)
	use := goDiff("a.go", "@@ -4,4 +4,5 @@ func f() {\n \tx := 1\n-\tr.kube.Create(x)\n+\tr.Kube.Create(x)\n \tx++\n+\tc()\n }\n")
	got, counts := quietRenames(slices.Concat(decl, use), headOf(map[string]string{"d.go": declHead,
		"a.go": "package a\n\nfunc f() {\n\tx := 1\n\tr.Kube.Create(x)\n\tx++\n\tc()\n}\n"}))
	want := "diff --git a/a.go b/a.go\nindex 1111111..2222222 100644\n--- a/a.go\n+++ b/a.go\n@@ -4,4 +4,5 @@ func f() {\n \tx := 1\n \tr.Kube.Create(x)\n \tx++\n+\tc()\n }\n"
	if len(got) != 2 || !reflect.DeepEqual(got[0], decl[0]) || got[1].Text != want || got[1].Added != 1 || got[1].Removed != 0 {
		t.Fatalf("quietRenames = %+v, want d.go as it is and a.go's text %q, +1 -0", got, want)
	}
	if counts != (RenameCounts{Pairs: 1, Files: 1}) {
		t.Fatalf("counts = %+v, want one pair in one file", counts)
	}
}

// A file whose one hunk is a use is left out; of two hunks, the one that
// changes nothing after the use goes and the other stays as it is.
func TestQuietRenamesDropsWhatChangesNothing(t *testing.T) {
	a, aHead := useIn("\tf(kube)", "\tf(Kube)")
	b := goDiff("b.go", "@@ -3,3 +3,3 @@\n func g() {\n-\tf(kube)\n+\tf(Kube)\n }\n@@ -20,3 +20,4 @@\n func h() {\n \tx := 1\n+\ty := 2\n }\n")
	got, _ := quietRenames(slices.Concat(goDiff("d.go", declHunks), a, b), headOf(map[string]string{"d.go": declHead, "a.go": aHead,
		"b.go": "package a\n\nfunc g() {\n\tf(Kube)\n}\n" + strings.Repeat("\n", 14) + "func h() {\n\tx := 1\n\ty := 2\n}\n"}))
	want := "diff --git a/b.go b/b.go\nindex 1111111..2222222 100644\n--- a/b.go\n+++ b/b.go\n@@ -20,3 +20,4 @@\n func h() {\n \tx := 1\n+\ty := 2\n }\n"
	if len(got) != 2 || got[0].Path != "d.go" || got[1].Text != want || got[1].Added != 1 || got[1].Removed != 0 {
		t.Fatalf("quietRenames = %+v, want d.go and b.go's second hunk alone", got)
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
		if got, _ := quietRenames(files, headOf(map[string]string{"a.go": added})); !reflect.DeepEqual(got, files) {
			t.Errorf("%s: quietRenames = %+v, want the diff as it is", name, got)
		}
	}
}

// Of three pairs, the use becomes context in its place and the real
// changes stay, a removed run before its added run.
func TestQuietRenamesKeepsAMixedHunk(t *testing.T) {
	for name, tc := range map[string]struct{ hunk, head, want string }{
		"between": {
			"@@ -4,4 +4,4 @@ func f() {\n-\ty := 1\n-\tr.kube.Create(x)\n-\tz := 1\n+\ty := 2\n+\tr.Kube.Create(x)\n+\tz := 2\n }\n",
			"package a\n\nfunc f() {\n\ty := 2\n\tr.Kube.Create(x)\n\tz := 2\n}\n",
			"@@ -4,4 +4,4 @@ func f() {\n-\ty := 1\n+\ty := 2\n \tr.Kube.Create(x)\n-\tz := 1\n+\tz := 2\n }\n",
		},
		"last": {
			"@@ -4,4 +4,4 @@ func f() {\n-\ty := 1\n-\tz := 1\n-\tr.kube.Create(x)\n+\ty := 2\n+\tz := 2\n+\tr.Kube.Create(x)\n }\n",
			"package a\n\nfunc f() {\n\ty := 2\n\tz := 2\n\tr.Kube.Create(x)\n}\n",
			"@@ -4,4 +4,4 @@ func f() {\n-\ty := 1\n-\tz := 1\n+\ty := 2\n+\tz := 2\n \tr.Kube.Create(x)\n }\n",
		},
	} {
		got, _ := quietRenames(slices.Concat(goDiff("d.go", declHunks), goDiff("a.go", tc.hunk)), headOf(map[string]string{"d.go": declHead, "a.go": tc.head}))
		a, ok := fileAt(got, "a.go")
		if !ok || !strings.HasSuffix(a.Text, "+++ b/a.go\n"+tc.want) || a.Added != 2 || a.Removed != 2 {
			t.Errorf("%s: quietRenames = %+v, want a.go's hunk %q, +2 -2", name, got, tc.want)
		}
	}
}

// A rename of the file, or a mode change, is no hunk: its header stays
// when its one hunk was a use, and a file renamed without a hunk stays as
// it is.
func TestQuietRenamesKeepsAFileRename(t *testing.T) {
	bare := splitDiff("diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n")
	if got, _ := quietRenames(bare, headOf(nil)); !reflect.DeepEqual(got, bare) {
		t.Fatalf("quietRenames = %+v, want the rename as it is", got)
	}
	for name, header := range map[string]string{
		"rename": "diff --git a/old.go b/new.go\nsimilarity index 90%\nrename from old.go\nrename to new.go\nindex 1111111..2222222 100644\n--- a/old.go\n+++ b/new.go\n",
		"mode":   "diff --git a/new.go b/new.go\nold mode 100644\nnew mode 100755\nindex 1111111..2222222\n--- a/new.go\n+++ b/new.go\n",
	} {
		got, _ := quietRenames(slices.Concat(goDiff("d.go", declHunks), splitDiff(header+"@@ -4 +4 @@\n-\tf(kube)\n+\tf(Kube)\n")),
			headOf(map[string]string{"d.go": declHead, "new.go": "package a\n\nfunc f() {\n\tf(Kube)\n}\n"}))
		f, ok := fileAt(got, "new.go")
		if !ok || f.Text != header || f.Added != 0 || f.Removed != 0 {
			t.Errorf("%s: quietRenames = %+v, want new.go's header alone", name, got)
		}
	}
}

// A use is quiet only when the diff renames its declaration where the use
// reaches it: a twin in the base, or a declaration of another kind, keeps
// it.
func TestQuietRenamesQuietsAUseItsDeclarationReaches(t *testing.T) {
	for _, tc := range []struct {
		name, removed, added string
		decls, quiet         bool
	}{
		{"a bare use of a package-level var", "\tf(kube)", "\tf(Kube)", true, true},
		{"no declaration in the diff", "\tf(kube)", "\tf(Kube)", false, false},
		{"a twin in the base", "\tif !allow(host) { return }", "\tif !Allow(host) { return }", false, false},
		{"its declaration in the diff", "\tif !allow(host) { return }", "\tif !Allow(host) { return }", true, true},
		{"a selector of a field", "\tt.kube = 1", "\tt.Kube = 1", true, true},
		{"a selector of a package-level func", "\tt.allow(h)", "\tt.Allow(h)", true, false},
		{"a bare use of a field", "\trefusal(1)", "\tRefusal(1)", true, false},
		{"a composite literal's key", "\tv := T{kube: 1}", "\tv := T{Kube: 1}", true, true},
	} {
		use, head := useIn(tc.removed, tc.added)
		files, heads := use, map[string]string{"a.go": head}
		if tc.decls {
			files, heads["d.go"] = slices.Concat(goDiff("d.go", declHunks), use), declHead
		}
		got, _ := quietRenames(files, headOf(heads))
		a, kept := fileAt(got, "a.go")
		if kept == tc.quiet || kept && !reflect.DeepEqual(a, use[0]) {
			t.Errorf("%s: quietRenames = %+v, want quiet %t", tc.name, got, tc.quiet)
		}
	}
}

// A pair that declares a renamed name stays, whatever the name runs:
// initializers, tests, interface methods, fields reflection reads, locals,
// parameters, types and embedded fields.
func TestQuietRenamesKeepsEveryDeclaration(t *testing.T) {
	type pair struct{ name, head, removed, added string }
	cases := []pair{
		{"init", "package a\n\nfunc Init() { installEgressGuard() }\n", "func init() { installEgressGuard() }", "func Init() { installEgressGuard() }"},
		{"a test", "package a\n\nfunc testEgressRefusesPrivate(t *testing.T) {\n}\n", "func TestEgressRefusesPrivate(t *testing.T) {", "func testEgressRefusesPrivate(t *testing.T) {"},
		{"TestMain", "package a\n\nfunc testMain(m *testing.M) {\n}\n", "func TestMain(m *testing.M) {", "func testMain(m *testing.M) {"},
		{"a field JSON writes", "package a\n\ntype Config struct {\n\tToken string\n}\n", "\ttoken string", "\tToken string"},
		{"a field YAML reads", "package a\n\ntype Config struct {\n\tname string `yaml:\"name\"`\n}\n", "\tName string `yaml:\"name\"`", "\tname string `yaml:\"name\"`"},
		{"a local", "package a\n\nfunc f() {\n\tKube := New()\n}\n", "\tkube := New()", "\tKube := New()"},
		{"a parameter", "package a\n\nfunc f(Kube int) {\n}\n", "func f(kube int) {", "func f(Kube int) {"},
		{"a type", "package a\n\ntype Kube struct{}\n", "type kube struct{}", "type Kube struct{}"},
		{"an interface method", "package a\n\ntype I interface {\n\tlogValue() int\n}\n", "\tLogValue() int", "\tlogValue() int"},
		{"an embedded field", "package a\n\ntype S struct {\n\tInner\n}\n", "\tinner", "\tInner"},
	}
	for _, m := range []string{"LogValue", "MarshalJSON", "ServeHTTP", "Unwrap", "Format", "GoString"} {
		low := strings.ToLower(m[:1]) + m[1:]
		cases = append(cases, pair{m, "package a\n\nfunc (s Secret) " + low + "() {}\n", "func (s Secret) " + m + "() {}", "func (s Secret) " + low + "() {}"})
	}
	for _, tc := range cases {
		line := strings.Count(tc.head[:strings.Index(tc.head, tc.added)], "\n") + 1
		files := goDiff("a.go", fmt.Sprintf("@@ -%d +%d @@\n-%s\n+%s\n", line, line, tc.removed, tc.added))
		if got, counts := quietRenames(files, headOf(map[string]string{"a.go": tc.head})); !reflect.DeepEqual(got, files) || counts != (RenameCounts{}) {
			t.Errorf("%s: quietRenames = %+v, %+v, want the declaration as it is", tc.name, got, counts)
		}
	}
}

// A local's declaration reaches its uses in its own file alone, and a
// package-level one those in its own directory alone.
func TestQuietRenamesScopesADeclaration(t *testing.T) {
	use, head := useIn("\tif !allow(host) { return }", "\tif !Allow(host) { return }")
	b := goDiff("b.go", "@@ -4,2 +4,2 @@ func g() {\n-\tallow := 1\n-\t_ = allow\n+\tAllow := 1\n+\t_ = Allow\n")
	got, _ := quietRenames(slices.Concat(b, use), headOf(map[string]string{"a.go": head, "b.go": "package a\n\nfunc g() {\n\tAllow := 1\n\t_ = Allow\n}\n"}))
	want := "diff --git a/b.go b/b.go\nindex 1111111..2222222 100644\n--- a/b.go\n+++ b/b.go\n@@ -4,2 +4,2 @@ func g() {\n-\tallow := 1\n+\tAllow := 1\n \t_ = Allow\n"
	if len(got) != 2 || got[0].Text != want || !reflect.DeepEqual(got[1], use[0]) {
		t.Fatalf("quietRenames = %+v, want b.go's use quiet and a.go as it is", got)
	}
	use, head = useIn("\tf(kube)", "\tf(Kube)")
	got, _ = quietRenames(slices.Concat(goDiff("x/d.go", declHunks), use), headOf(map[string]string{"x/d.go": declHead, "a.go": head}))
	if a, ok := fileAt(got, "a.go"); !ok || !reflect.DeepEqual(a, use[0]) {
		t.Fatalf("quietRenames = %+v, want a.go as it is: x/d.go is another package", got)
	}
}

// The pairs go/scanner reads as a rename of identifiers by the case of
// their first ASCII letter, and nothing else, are quiet where their
// declaration is renamed; every other pair stays, the ones a tokenizer
// that reads less than Go would misread too.
func TestQuietRenamesReadsTheLineAsGo(t *testing.T) {
	for _, tc := range []struct {
		name, removed, added string
		quiet                bool
	}{
		{"a field key", "\te.runner = &Runner{kube: kube}", "\te.runner = &Runner{Kube: kube}", true},
		{"after a literal holding //", "\tf(\"http://x\", kube)", "\tf(\"http://x\", Kube)", true},
		{"after the rune literal '\"'", "\ts := '\"' + kube + \"x\"", "\ts := '\"' + Kube + \"x\"", true},
		{"beside a literal key", "\tm := map[string]int{\"kube\": kube}", "\tm := map[string]int{\"kube\": Kube}", true},
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
		{"a test", "\tTestKube(t)", "\ttestKube(t)", false},
		{"white space", "\ta := kube", "\ta :=  Kube", false},
		{"nothing", "\ta := kube", "\ta := kube", false},
	} {
		use, head := useIn(tc.removed, tc.added)
		got, _ := quietRenames(slices.Concat(goDiff("d.go", declHunks), use), headOf(map[string]string{"d.go": declHead, "a.go": head}))
		a, kept := fileAt(got, "a.go")
		if kept == tc.quiet || kept && !reflect.DeepEqual(a, use[0]) {
			t.Errorf("%s: quietRenames = %+v, want quiet %t", tc.name, got, tc.quiet)
		}
	}
}

// A use of a predeclared name, or of a name go test runs by itself, stays,
// its declaration renamed beside it or not.
func TestQuietRenamesKeepsAUseOfAPredeclaredOrTestName(t *testing.T) {
	for _, tc := range []struct{ name, declWas, declIs, removed, added string }{
		{"a builtin", "func close(ch chan int) {}", "func Close(ch chan int) {}", "\tdefer close(ch)", "\tdefer Close(ch)"},
		{"a test", "func TestKube(t *testing.T) {}", "func testKube(t *testing.T) {}", "\tTestKube(t)", "\ttestKube(t)"},
	} {
		files := goDiff("d.go", "@@ -3 +3 @@\n-"+tc.declWas+"\n+"+tc.declIs+"\n")
		use, head := useIn(tc.removed, tc.added)
		files = append(files, use...)
		if got, _ := quietRenames(files, headOf(map[string]string{"d.go": "package a\n\n" + tc.declIs + "\n", "a.go": head})); !reflect.DeepEqual(got, files) {
			t.Errorf("%s: quietRenames = %+v, want both as they are", tc.name, got)
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

// A use stays where the head does not show it as code at its line: inside
// a literal or a comment of more than one line, whatever //line directive
// says the lines are, at a line that holds something else, in a file
// unread or one go/scanner or go/parser cannot read. A file whose path
// does not end in .go, or whose old one does not, or that the checklist
// judges by its path, stays, and so does a rename to a name an added line
// of the diff brings.
func TestQuietRenamesReadsTheLineAtTheHead(t *testing.T) {
	pair, code := "@@ -4 +4 @@\n-from: selector\n+from: Selector\n", "package a\n\nfunc f() {\nfrom: Selector\n}\n"
	decl := func(dir string) []fileDiff {
		return goDiff(dir+"b.go", "@@ -3 +3 @@\n-var selector = 1\n+var Selector = 1\n")
	}
	declHead := "package a\n\nvar Selector = 1\n"
	for _, tc := range []struct {
		name, path string
		files      []fileDiff
		heads      map[string]string
	}{
		{"a raw literal of lines", "a.go", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\nconst m = `\nfrom: Selector\n`\n"}},
		{"a //line directive", "a.go", goDiff("a.go", pair), map[string]string{"a.go": "package a\n//line x.go:1\nconst m = `\nfrom: Selector\n`\n"}},
		{"a block comment of lines", "a.go", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\n/*\nfrom: Selector\n*/\n"}},
		{"another line at the head", "a.go", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\nfunc f() {\nfrom: selector\n}\n"}},
		{"an unread head", "a.go", goDiff("a.go", pair), nil},
		{"a head go/scanner cannot read", "a.go", goDiff("a.go", pair), map[string]string{"a.go": code + "`\n"}},
		{"a head go/parser cannot read", "a.go", goDiff("a.go", pair), map[string]string{"a.go": "package a\n\n\nfrom: Selector\n"}},
		{"YAML", "k8s/a.yaml", goDiff("k8s/a.yaml", pair), map[string]string{"k8s/a.yaml": "a:\n\n\nfrom: Selector\n"}},
		{"Go from a text file", "a.go", splitDiff("diff --git a/a.txt b/a.go\nsimilarity index 90%\nrename from a.txt\nrename to a.go\n--- a/a.txt\n+++ b/a.go\n" + pair),
			map[string]string{"a.go": code}},
		{"a path the checklist judges", "factory/a.go", goDiff("factory/a.go", pair), map[string]string{"factory/a.go": code}},
		{"a name the diff adds", "a.go", slices.Concat(goDiff("a.go", pair), goDiff("c.go", "@@ -1,0 +2 @@\n+var wide = Selector\n")),
			map[string]string{"a.go": code, "c.go": "package a\nvar wide = Selector\n"}},
	} {
		dir := strings.TrimSuffix(tc.path, path.Base(tc.path))
		heads := map[string]string{dir + "b.go": declHead}
		maps.Copy(heads, tc.heads)
		got, _ := quietRenames(slices.Concat(decl(dir), tc.files), headOf(heads))
		if f, ok := fileAt(got, tc.path); !ok || !reflect.DeepEqual(f, tc.files[0]) {
			t.Errorf("%s: quietRenames = %+v, want %s as it is", tc.name, got, tc.path)
		}
	}
	got, _ := quietRenames(slices.Concat(decl(""), goDiff("a.go", pair)), headOf(map[string]string{"b.go": declHead, "a.go": code}))
	if _, ok := fileAt(got, "a.go"); ok {
		t.Fatalf("quietRenames = %+v, want the use quiet where the head reads it as code", got)
	}
}
