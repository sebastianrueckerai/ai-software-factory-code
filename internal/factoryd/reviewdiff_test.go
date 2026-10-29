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
	pieces := cutFile(parseFileDiff(b.String()), 48<<10)
	if len(pieces) < 3 {
		t.Fatalf("%d pieces, want at least 3", len(pieces))
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
