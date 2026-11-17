package factoryd

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// fileDiff is one file's part of a unified git diff, its header and every
// hunk byte for byte. Path is the new path (a deleted file's old one);
// OldPath is the old path of a rename or copy. Added and Removed count the
// hunks' + and - lines. Piece and Pieces are 0 for a whole file; else the
// file is piece Piece of Pieces (1-based) and the counts are the piece's own.
type fileDiff struct {
	Path, OldPath  string
	Text           string
	Added, Removed int
	Piece, Pieces  int
}

// splitDiff cuts diff at its "diff --git" lines; text before the first one
// is a file without a path.
func splitDiff(diff string) []fileDiff {
	var files []fileDiff
	for rest := diff; rest != ""; {
		end := len(rest)
		if i := strings.Index(rest[1:], "\ndiff --git "); i >= 0 {
			end = i + 2
		}
		files = append(files, parseFileDiff(rest[:end]))
		rest = rest[end:]
	}
	return files
}

func parseFileDiff(text string) fileDiff {
	f := fileDiff{Text: text}
	var oldPath, newPath string
	inHunk := false
	for _, l := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(l, "@@ "):
			inHunk = true
		case inHunk && strings.HasPrefix(l, "+"):
			f.Added++
		case inHunk && strings.HasPrefix(l, "-"):
			f.Removed++
		case inHunk:
		case strings.HasPrefix(l, "diff --git "):
			oldPath, newPath = gitHeaderPaths(strings.TrimPrefix(l, "diff --git "))
		case strings.HasPrefix(l, "rename from "), strings.HasPrefix(l, "copy from "):
			_, oldPath, _ = strings.Cut(l, " from ")
			oldPath = unquoteGit(oldPath)
		case strings.HasPrefix(l, "rename to "), strings.HasPrefix(l, "copy to "):
			_, newPath, _ = strings.Cut(l, " to ")
			newPath = unquoteGit(newPath)
		case l == "--- /dev/null":
			oldPath = ""
		case strings.HasPrefix(l, "--- a/"), strings.HasPrefix(l, `--- "a/`):
			oldPath = strings.TrimPrefix(unquoteGit(strings.TrimSuffix(strings.TrimPrefix(l, "--- "), "\t")), "a/")
		case l == "+++ /dev/null":
			newPath = ""
		case strings.HasPrefix(l, "+++ b/"), strings.HasPrefix(l, `+++ "b/`):
			newPath = strings.TrimPrefix(unquoteGit(strings.TrimSuffix(strings.TrimPrefix(l, "+++ "), "\t")), "b/")
		}
	}
	f.Path = cmp.Or(newPath, oldPath)
	if oldPath != f.Path {
		f.OldPath = oldPath
	}
	return f
}

// unquoteGit is s with git's C-style quoting undone: git quotes a path
// that holds a quote, a backslash, a control character or, by default, a
// byte above 0x7f. An unquoted s is returned as it is.
func unquoteGit(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

// gitHeaderPaths are the old and new path of a "diff --git" line's
// remainder. Of an unquoted header whose paths differ the first " b/" cut
// is a guess that a rename line corrects; one whose two paths are equal
// is found exactly, even where the path holds " b/".
func gitHeaderPaths(rest string) (oldPath, newPath string) {
	if strings.HasPrefix(rest, `"`) {
		end := 1
		for end < len(rest) && rest[end] != '"' {
			if rest[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(rest) {
			return "", ""
		}
		oldPath = strings.TrimPrefix(unquoteGit(rest[:end+1]), "a/")
		return oldPath, strings.TrimPrefix(unquoteGit(strings.TrimPrefix(rest[end+1:], " ")), "b/")
	}
	if strings.HasPrefix(rest, "a/") {
		for i := 0; i < len(rest); i++ {
			if strings.HasPrefix(rest[i:], " b/") && rest[2:i] == rest[i+3:] {
				return rest[2:i], rest[i+3:]
			}
		}
	}
	if a, b, ok := strings.Cut(rest, ` "b/`); ok {
		return strings.TrimPrefix(a, "a/"), strings.TrimPrefix(unquoteGit(`"b/`+b), "b/")
	}
	a, b, _ := strings.Cut(rest, " b/")
	return strings.TrimPrefix(a, "a/"), b
}

// packFiles fills chunks in order, each with files whose texts together
// stay within limit; a file larger than limit alone is a chunk of its own.
func packFiles(files []fileDiff, limit int) (chunks [][]fileDiff) {
	size := 0
	for _, f := range files {
		if n := len(f.Text); len(chunks) == 0 || size+n > limit {
			chunks, size = append(chunks, []fileDiff{f}), n
		} else {
			chunks[len(chunks)-1] = append(chunks[len(chunks)-1], f)
			size += n
		}
	}
	return chunks
}

// diffParts cuts the files larger than limit (cutFile) and packs the
// result into parts of at most limit bytes, one model call each, at least
// one part: no files are one empty part.
func diffParts(files []fileDiff, limit int) [][]fileDiff {
	var cut []fileDiff
	for _, f := range files {
		cut = append(cut, cutFile(f, limit)...)
	}
	if parts := packFiles(cut, limit); len(parts) > 0 {
		return parts
	}
	return [][]fileDiff{nil}
}

// RenameCounts are the case renames quietRenames turned into context, the
// pairs and the files that held them, and the reads of a file at the head
// that failed, the caller's to count: a file it cannot read keeps its
// renames.
type RenameCounts struct {
	Pairs       int `json:"pairs"`
	Files       int `json:"files"`
	ReadsFailed int `json:"reads_failed,omitempty"`
}

// auditByPath are the path prefixes the audit checklist's item 4 judges by
// path: no rename under them is quiet.
var auditByPath = []string{"factory/", ".forgejo/", ".gitea/", ".github/"}

// quietRenames is files with each case rename of a .go file that only uses
// a renamed name turned into one context line that holds the added line,
// so the audit reads each rename once, where it is declared. A changed
// block, a run of removed lines and a run of added lines of one length,
// pairs its lines in order; a pair is a case rename when caseRenames finds
// one, the added line stands at its line of head(path), the file at the
// head (nil: unread), outside every literal and comment of more than one
// line, which one diff line cannot show, and no other added line of a .go
// file names a new spelling it brings. A pair that declares a renamed name
// stays (identRoles); one that uses it is quiet only when a pair the diff
// keeps declares the same rename where the use reaches it: a local in the
// same file, a package-level name for a bare use, a field or method for a
// selector, either for a composite literal's key, both in the same
// directory. A hunk left with no change is dropped, and a file left with
// no hunk unless its header holds more than its paths.
func quietRenames(files []fileDiff, head func(path string) []byte) ([]fileDiff, RenameCounts) {
	type use struct {
		old, new string
		role     identRole
	}
	type block struct {
		at, n, newLine int // the first removed line's index, the run's length, the first added line's line in the new file
		renames        [][]caseRename
		uses           [][]use // a pair's renames that it uses and declares none of; nil: not quiet
	}
	type decl struct {
		old, new, scope string
		role            identRole
	}
	blocks := make([][]block, len(files))
	named := map[string]bool{}
	name := func(l string) {
		ids := identsAt(lineBody(l))
		if ids == nil {
			for _, w := range goWord.FindAllString(l, -1) {
				named[w] = true
			}
		}
		for _, id := range ids {
			named[id] = true
		}
	}
	for i, f := range files {
		if !strings.HasSuffix(f.Path, ".go") {
			continue
		}
		renames := (f.OldPath == "" || strings.HasSuffix(f.OldPath, ".go")) && !slices.ContainsFunc(auditByPath, func(p string) bool {
			return strings.HasPrefix(f.Path, p) || strings.HasPrefix(f.OldPath, p)
		})
		lines := strings.SplitAfter(f.Text, "\n")
		newLine := -1
		for k := 0; k < len(lines); k++ {
			l := lines[k]
			switch {
			case strings.HasPrefix(l, "@@ "):
				newLine = -1
				if m := hunkHeader.FindStringSubmatch(l); m != nil {
					newLine, _ = strconv.Atoi(m[3])
				}
				continue
			case newLine < 0:
				continue
			case strings.HasPrefix(l, "+"):
				name(l)
				fallthrough
			case strings.HasPrefix(l, " "):
				newLine++
				continue
			case !strings.HasPrefix(l, "-"):
				continue
			}
			n := runOf(lines[k:], "-")
			m := runOf(lines[k+n:], "+")
			b, found := block{at: k, n: n, newLine: newLine}, false
			for j := range m {
				var r []caseRename
				if renames && n == m {
					r = caseRenames(lineBody(lines[k+j]), lineBody(lines[k+n+j]))
				}
				if r == nil {
					name(lines[k+n+j])
				}
				b.renames, found = append(b.renames, r), found || r != nil
			}
			if found {
				blocks[i] = append(blocks[i], b)
			}
			newLine += m
			k += n + m - 1
		}
	}
	declared := map[decl]bool{}
	for i, f := range files {
		if len(blocks[i]) == 0 {
			continue
		}
		src := head(f.Path)
		multi, scanned := multiLine(src)
		roles, parsed := identRoles(src)
		if src == nil || !scanned || !parsed {
			continue
		}
		at, starts := strings.Split(string(src), "\n"), []int{0}
		for k, c := range src {
			if c == '\n' {
				starts = append(starts, k+1)
			}
		}
		lines := strings.SplitAfter(f.Text, "\n")
		for bi := range blocks[i] {
			b := &blocks[i][bi]
			b.uses = make([][]use, b.n)
			for j, rs := range b.renames {
				line := b.newLine + j
				if rs == nil || line < 1 || line > len(at) || at[line-1] != lineBody(lines[b.at+b.n+j]) || multi[line] ||
					slices.ContainsFunc(rs, func(r caseRename) bool { return named[r.new] }) {
					continue
				}
				var uses []use
				for _, r := range rs {
					role := roles[starts[line-1]+r.off]
					switch role {
					case declLocal:
						declared[decl{r.old, r.new, "file " + f.Path, role}] = true
					case declPackage, declMember:
						declared[decl{r.old, r.new, "dir " + path.Dir(f.Path), role}] = true
					}
					uses = append(uses, use{r.old, r.new, role})
				}
				if !slices.ContainsFunc(uses, func(u use) bool { return u.role >= declLocal }) {
					b.uses[j] = uses
				}
			}
		}
	}
	reaches := func(f fileDiff, u use) bool {
		file, dir := "file "+f.Path, "dir "+path.Dir(f.Path)
		local, pkg, member := declared[decl{u.old, u.new, file, declLocal}], declared[decl{u.old, u.new, dir, declPackage}], declared[decl{u.old, u.new, dir, declMember}]
		switch u.role {
		case useSelector:
			return member
		case useKey:
			return local || pkg || member
		}
		return local || pkg
	}
	var out []fileDiff
	var counts RenameCounts
	for i, f := range files {
		quiet := func(b block, j int) bool {
			return b.uses != nil && b.uses[j] != nil && !slices.ContainsFunc(b.uses[j], func(u use) bool { return !reaches(f, u) })
		}
		pairs := 0
		for _, b := range blocks[i] {
			for j := range b.n {
				if quiet(b, j) {
					pairs++
				}
			}
		}
		if pairs == 0 {
			out = append(out, f)
			continue
		}
		counts.Pairs, counts.Files = counts.Pairs+pairs, counts.Files+1
		lines := strings.SplitAfter(f.Text, "\n")
		var kept []string
		k := 0
		for _, b := range blocks[i] {
			kept = append(kept, lines[k:b.at]...)
			removed, added := lines[b.at:b.at+b.n], lines[b.at+b.n:b.at+2*b.n]
			for j := 0; j < b.n; {
				if quiet(b, j) {
					kept = append(kept, " "+added[j][1:])
					j++
					continue
				}
				e := j + 1
				for e < b.n && !quiet(b, e) {
					e++
				}
				kept = append(append(kept, removed[j:e]...), added[j:e]...)
				j = e
			}
			k = b.at + 2*b.n
		}
		if text := withChanges(append(kept, lines[k:]...)); text != "" {
			out = append(out, parseFileDiff(text))
		}
	}
	return out, counts
}

var goWord = regexp.MustCompile(`[\p{L}_][\p{L}\p{N}_]*`)

// runOf is how many of lines, from the first, start with prefix.
func runOf(lines []string, prefix string) int {
	n := 0
	for n < len(lines) && strings.HasPrefix(lines[n], prefix) {
		n++
	}
	return n
}

// lineBody is a diff line without its marker and its newline.
func lineBody(l string) string {
	return strings.TrimSuffix(l[min(1, len(l)):], "\n")
}

// caseRename is an identifier a line pair renames by the case of its first
// letter: at off in the line, old to new.
type caseRename struct {
	off      int
	old, new string
}

// caseRenames are the identifiers removed and added spell with the other
// case of an ASCII first letter, when that is all the two lines differ in:
// nil when they differ in anything else or in nothing, in a byte that
// starts no identifier of both, in a letter outside ASCII, in a
// predeclared name (true, nil, len) or a name Go or its tests run by
// itself (entryName), or go/scanner cannot read a line whole.
func caseRenames(removed, added string) []caseRename {
	if len(removed) != len(added) || removed == added {
		return nil
	}
	was, is := identsAt(removed), identsAt(added)
	if was == nil || is == nil {
		return nil
	}
	var out []caseRename
	for i := range len(removed) {
		if removed[i] == added[i] {
			continue
		}
		a, b, lower := was[i], is[i], removed[i]|0x20
		if a == "" || b == "" || removed[i]^added[i] != 0x20 || lower < 'a' || lower > 'z' ||
			types.Universe.Lookup(a) != nil || types.Universe.Lookup(b) != nil || entryName(a) || entryName(b) {
			return nil
		}
		out = append(out, caseRename{i, a, b})
	}
	return out
}

// entryName reports whether Go or go test runs name by itself: init, main,
// or a test, benchmark, fuzz test or example.
func entryName(name string) bool {
	if name == "init" || name == "main" {
		return true
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && (rest == "" || !unicode.IsLower([]rune(rest)[0])) {
			return true
		}
	}
	return false
}

// identRole is what an identifier is where it stands: a use as a bare
// name, a selector or a composite literal's key, or the declaration of a
// local (a parameter, a := or a local var, a label, an import name), a
// package-level name, or a member (a field, an embedded type, a method).
type identRole int

const (
	useBare identRole = iota
	useSelector
	useKey
	declLocal
	declPackage
	declMember
)

// identRoles maps the offset in src of each identifier that is no bare use
// to its role; read is false when go/parser cannot read src.
func identRoles(src []byte) (roles map[int]identRole, read bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	tf := fset.File(file.Pos())
	roles = map[int]identRole{}
	set := func(role identRole, ids ...*ast.Ident) {
		for _, id := range ids {
			if id != nil && (role >= declLocal || roles[tf.Offset(id.Pos())] == useBare) {
				roles[tf.Offset(id.Pos())] = role
			}
		}
	}
	fields := func(role identRole, l *ast.FieldList) {
		for _, f := range fieldsOf(l) {
			set(role, f.Names...)
			if len(f.Names) == 0 && role == declMember {
				ast.Inspect(f.Type, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						set(role, id)
					}
					return true
				})
			}
		}
	}
	specs := func(role identRole, g *ast.GenDecl) {
		for _, s := range g.Specs {
			switch s := s.(type) {
			case *ast.TypeSpec:
				set(role, s.Name)
			case *ast.ValueSpec:
				set(role, s.Names...)
			case *ast.ImportSpec:
				set(declLocal, s.Name)
			}
		}
	}
	for _, d := range file.Decls {
		if g, ok := d.(*ast.GenDecl); ok {
			specs(declPackage, g)
		}
	}
	idents := func(es ...ast.Expr) (out []*ast.Ident) {
		for _, e := range es {
			if id, ok := e.(*ast.Ident); ok {
				out = append(out, id)
			}
		}
		return out
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Recv == nil {
				set(declPackage, n.Name)
			} else {
				set(declMember, n.Name)
				fields(declLocal, n.Recv)
			}
		case *ast.FuncType:
			fields(declLocal, n.TypeParams)
			fields(declLocal, n.Params)
			fields(declLocal, n.Results)
		case *ast.TypeSpec:
			fields(declLocal, n.TypeParams)
		case *ast.DeclStmt:
			specs(declLocal, n.Decl.(*ast.GenDecl))
		case *ast.StructType:
			fields(declMember, n.Fields)
		case *ast.InterfaceType:
			fields(declMember, n.Methods)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				set(declLocal, idents(n.Lhs...)...)
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				set(declLocal, idents(n.Key, n.Value)...)
			}
		case *ast.LabeledStmt:
			set(declLocal, n.Label)
		case *ast.SelectorExpr:
			set(useSelector, n.Sel)
		case *ast.KeyValueExpr:
			set(useKey, idents(n.Key)...)
		}
		return true
	})
	return roles, true
}

// fieldsOf is l's fields, none for a nil l.
func fieldsOf(l *ast.FieldList) []*ast.Field {
	if l == nil {
		return nil
	}
	return l.List
}

// identsAt maps the offset of each identifier of line to the identifier;
// nil when go/scanner cannot read line whole.
func identsAt(line string) map[int]string {
	out := map[int]string{}
	if !goTokens([]byte(line), func(off, _ int, tok token.Token, lit string) {
		if tok == token.IDENT {
			out[off] = lit
		}
	}) {
		return nil
	}
	return out
}

// multiLine are the lines, from 1, of src that a literal or a comment of
// more than one line covers; read is false when go/scanner cannot read src
// whole.
func multiLine(src []byte) (lines map[int]bool, read bool) {
	lines = map[int]bool{}
	read = goTokens(src, func(_, line int, tok token.Token, lit string) {
		if n := strings.Count(lit, "\n"); n > 0 && (tok == token.STRING || tok == token.COMMENT) {
			for l := line; l <= line+n; l++ {
				lines[l] = true
			}
		}
	})
	return lines, read
}

// goTokens calls each with every token of src, comments too, its offset
// and its line as written (no //line directive moves it); false when
// go/scanner reports an error, such as a literal or comment src does not
// close or a character Go does not know.
func goTokens(src []byte, each func(off, line int, tok token.Token, lit string)) bool {
	file := token.NewFileSet().AddFile("", -1, len(src))
	failed := false
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) { failed = true }, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return !failed
		}
		each(file.Offset(pos), file.PositionFor(pos, false).Line, tok, lit)
	}
}

// withChanges joins lines, one file's diff, leaving out each hunk that
// changes no line: "" when no hunk is left and the header holds nothing
// but the paths.
func withChanges(lines []string) string {
	first := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "@@ ") })
	if first < 0 {
		return strings.Join(lines, "")
	}
	var hunks strings.Builder
	for k := first; k < len(lines); {
		e := k + 1
		for e < len(lines) && !strings.HasPrefix(lines[e], "@@ ") {
			e++
		}
		if slices.ContainsFunc(lines[k+1:e], func(l string) bool { return strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") }) {
			hunks.WriteString(strings.Join(lines[k:e], ""))
		}
		k = e
	}
	header := lines[:first]
	if hunks.Len() == 0 && !slices.ContainsFunc(header, func(l string) bool {
		return !strings.HasPrefix(l, "diff --git ") && !strings.HasPrefix(l, "index ") && !strings.HasPrefix(l, "--- ") && !strings.HasPrefix(l, "+++ ")
	}) {
		return ""
	}
	return strings.Join(header, "") + hunks.String()
}

// subHeaderRoom is the room a recomputed hunk header needs.
const subHeaderRoom = 80

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// cutFile cuts f into pieces of at most limit bytes: f itself when it
// fits. Each piece starts with f's header, then whole hunks while they fit.
// A hunk that does not fit beside the header is split at line boundaries
// into sub-hunks with recomputed headers, and a line that does not fit is
// cut at rune boundaries into lines that keep its prefix. A file without a
// hunk, or with a header too large to leave room for one, is cut at rune
// boundaries into pieces that have no header.
func cutFile(f fileDiff, limit int) []fileDiff {
	if len(f.Text) <= limit {
		return []fileDiff{f}
	}
	var texts []string
	if at := strings.Index(f.Text, "\n@@ "); at < 0 || at+1+subHeaderRoom+8 > limit {
		texts = cutRaw(f.Text, limit)
	} else {
		header, rest := f.Text[:at+1], f.Text[at+1:]
		var cur strings.Builder
		cur.WriteString(header)
		hunks := 0
		for rest != "" {
			end := len(rest)
			if i := strings.Index(rest[1:], "\n@@ "); i >= 0 {
				end = i + 2
			}
			var group []string
			if hunk := rest[:end]; len(header)+len(hunk) <= limit {
				group = []string{hunk}
			} else {
				group = splitHunk(hunk, limit-len(header))
			}
			for _, h := range group {
				if hunks > 0 && cur.Len()+len(h) > limit {
					texts = append(texts, cur.String())
					cur.Reset()
					cur.WriteString(header)
					hunks = 0
				}
				cur.WriteString(h)
				hunks++
			}
			rest = rest[end:]
		}
		texts = append(texts, cur.String())
	}
	pieces := make([]fileDiff, len(texts))
	for i, t := range texts {
		p := parseFileDiff(t)
		p.Path, p.OldPath = f.Path, f.OldPath
		if len(texts) > 1 {
			p.Piece, p.Pieces = i+1, len(texts)
		}
		pieces[i] = p
	}
	return pieces
}

// splitHunk cuts one hunk into sub-hunks of at most room bytes with the
// headers their lines call for.
func splitHunk(hunk string, room int) []string {
	head, body, _ := strings.Cut(hunk, "\n")
	m := hunkHeader.FindStringSubmatch(head)
	if m == nil {
		return cutRaw(hunk, room)
	}
	num := func(s string, absent int) int {
		if s == "" {
			return absent
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	oldStart, oldCount, newStart, newCount := num(m[1], 0), num(m[2], 1), num(m[3], 0), num(m[4], 1)
	// the next line to number: a side with no lines names the line before
	nextOld, nextNew := oldStart, newStart
	if oldCount == 0 {
		nextOld++
	}
	if newCount == 0 {
		nextNew++
	}
	var out []string
	var lines strings.Builder
	oldN, newN, oldAdv, newAdv := 0, 0, 0, 0
	flush := func() {
		if lines.Len() == 0 {
			return
		}
		at := func(next, n int) int {
			if n == 0 {
				return next - 1
			}
			return next
		}
		out = append(out, fmt.Sprintf("@@ -%d,%d +%d,%d @@\n%s", at(nextOld, oldN), oldN, at(nextNew, newN), newN, lines.String()))
		nextOld, nextNew = nextOld+oldAdv, nextNew+newAdv
		lines.Reset()
		oldN, newN, oldAdv, newAdv = 0, 0, 0, 0
	}
	room -= subHeaderRoom
	for _, l := range strings.SplitAfter(body, "\n") {
		if l == "" {
			continue
		}
		pieces := []string{l}
		if len(l) > room {
			payload := strings.TrimSuffix(l[1:], "\n")
			pieces = pieces[:0]
			for payload != "" || len(pieces) == 0 {
				n := runeCut(payload, room-2)
				pieces, payload = append(pieces, l[:1]+payload[:n]+"\n"), payload[n:]
			}
		}
		for k, p := range pieces {
			if lines.Len()+len(p) > room {
				flush()
			}
			lines.WriteString(p)
			// the fragments of a cut line count in the header, the line
			// itself advances the numbering once, after its last fragment
			last := k == len(pieces)-1
			switch p[0] {
			case '+':
				newN++
				newAdv += b2i(last)
			case '-':
				oldN++
				oldAdv += b2i(last)
			case '\\':
			default:
				oldN++
				newN++
				oldAdv += b2i(last)
				newAdv += b2i(last)
			}
		}
	}
	flush()
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cutRaw cuts s at rune boundaries into chunks of at most limit bytes.
func cutRaw(s string, limit int) (chunks []string) {
	for s != "" {
		n := runeCut(s, limit)
		chunks, s = append(chunks, s[:n]), s[n:]
	}
	return chunks
}

// runeCut is how many bytes of s, at most limit and at least one rune, end
// on a rune boundary.
func runeCut(s string, limit int) int {
	if len(s) <= limit {
		return len(s)
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		_, n = utf8.DecodeRuneInString(s)
	}
	return n
}

func kib(n int) int { return (n + 1023) >> 10 }

var nameShapes = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"functions", regexp.MustCompile(`^\s*(?:export\s+|async\s+|pub\s+)*(?:function\s+|func(?:\s+|\s*\([^)]*\)\s*)|def\s+|fn\s+)([\p{L}_][\p{L}\p{N}_]*)`)},
	{"types", regexp.MustCompile(`^\s*(?:export\s+|pub\s+)*(?:type\s+|class\s+|interface\s+|struct\s+|enum\s+|trait\s+)([\p{L}_][\p{L}\p{N}_]*)`)},
	{"routes", regexp.MustCompile(`\b(?:Handle|HandleFunc|Get|Post|Put|Patch|Delete|Route)\(\s*"(?:[A-Z]+ )?(/[^"\s]*)"`)},
	{"selectors", regexp.MustCompile(`^\s*([.#][\w-]+)[^{]*\{\s*$`)},
}

const maxListedNames = 300

// nameIndex holds the names each part's added lines define.
type nameIndex struct {
	parts []map[string][]string
}

// newNameIndex scans the added lines of every part once.
func newNameIndex(parts [][]fileDiff) *nameIndex {
	n := &nameIndex{}
	for _, part := range parts {
		byKind := map[string][]string{}
		seen := map[string]bool{}
		for _, f := range part {
			redact.Added(f.Text, func(_ string, _ int, text string) {
				for _, s := range nameShapes {
					if m := s.re.FindStringSubmatch(text); m != nil && !seen[s.kind+m[1]] {
						seen[s.kind+m[1]] = true
						byKind[s.kind] = append(byKind[s.kind], m[1])
					}
				}
			})
		}
		n.parts = append(n.parts, byKind)
	}
	return n
}

// Others lists the functions, types, routes and selectors the added lines
// of every part but part i define, one line a kind, so the call on part i
// knows a name it does not see is defined elsewhere in the diff.
func (n *nameIndex) Others(i int) string { return n.list(i) }

// All lists the names of every part.
func (n *nameIndex) All() string { return n.list(-1) }

func (n *nameIndex) list(skip int) string {
	var lines []string
	for _, s := range nameShapes {
		seen := map[string]bool{}
		var list []string
		for k, byKind := range n.parts {
			if k == skip {
				continue
			}
			for _, name := range byKind[s.kind] {
				if !seen[name] {
					seen[name] = true
					list = append(list, name)
				}
			}
		}
		if len(list) == 0 {
			continue
		}
		slices.Sort(list)
		more := ""
		if len(list) > maxListedNames {
			more, list = fmt.Sprintf(", ... and %d more", len(list)-maxListedNames), list[:maxListedNames]
		}
		lines = append(lines, s.kind+": "+strings.Join(list, ", ")+more)
	}
	return strings.Join(lines, "\n")
}

// partLabel names part i of parts and its files among all. A file's
// ordinal counts a cut file once, at its first piece.
func partLabel(parts [][]fileDiff, i, all int) string {
	ordinal := 0
	for _, p := range parts[:i] {
		for _, f := range p {
			if f.Piece <= 1 {
				ordinal++
			}
		}
	}
	first, last := ordinal+1, ordinal
	if parts[i][0].Piece > 1 {
		first = ordinal
	}
	for _, f := range parts[i] {
		if f.Piece <= 1 {
			last++
		}
	}
	last = max(last, first)
	if f := parts[i][0]; len(parts[i]) == 1 && f.Piece > 0 {
		return fmt.Sprintf("file %d of %d (%s, piece %d of %d), part %d of %d", first, all, f.Path, f.Piece, f.Pieces, i+1, len(parts))
	}
	files := fmt.Sprintf("files %d to %d", first, last)
	if first == last {
		files = fmt.Sprintf("file %d", first)
	}
	return fmt.Sprintf("%s of %d, part %d of %d", files, all, i+1, len(parts))
}

// fileList renders every entry of groups, one line each with its added
// and removed lines and its group, at most maxListLines; the entries past
// them are one line a directory, at most maxListDirs, then one line for the
// rest. unit names a group: "part" or "card".
func fileList(groups [][]fileDiff, unit string) string {
	type dirTotal struct {
		files, added, removed, from, to int
	}
	var lines []string
	var dirs []string
	totals := map[string]*dirTotal{}
	n := 0
	for k, group := range groups {
		for _, f := range group {
			if n++; n > maxListLines {
				d := path.Dir(f.Path) + "/"
				t := totals[d]
				if t == nil {
					t = &dirTotal{from: k + 1}
					totals[d] = t
					dirs = append(dirs, d)
				}
				if f.Piece <= 1 {
					t.files++
				}
				t.added, t.removed, t.to = t.added+f.Added, t.removed+f.Removed, k+1
				continue
			}
			name := f.Path
			if f.OldPath != "" {
				name = f.OldPath + " -> " + f.Path
			}
			if f.Piece > 0 {
				name += fmt.Sprintf(" piece %d of %d", f.Piece, f.Pieces)
			}
			lines = append(lines, fmt.Sprintf("%s +%d -%d (%s %d)", name, f.Added, f.Removed, unit, k+1))
		}
	}
	for k, d := range dirs {
		t := totals[d]
		if k == maxListDirs {
			var rest dirTotal
			for _, d := range dirs[k:] {
				rest.files += totals[d].files
			}
			lines = append(lines, fmt.Sprintf("and %s with %s", textutil.Plural(len(dirs)-k, "more directory", "more directories"), textutil.Plural(rest.files, "file", "files")))
			break
		}
		span := fmt.Sprintf("%ss %d to %d", unit, t.from, t.to)
		if t.from == t.to {
			span = fmt.Sprintf("%s %d", unit, t.from)
		}
		lines = append(lines, fmt.Sprintf("%s %s +%d -%d (%s)", d, textutil.Plural(t.files, "file", "files"), t.added, t.removed, span))
	}
	return strings.Join(lines, "\n")
}

// partOf is the index of the part holding the file s names: the longest
// path it names, else the one part holding a file whose base name it
// names; 0 when it names none, or a base name more than one part holds.
func partOf(s string, parts [][]fileDiff) int {
	at, longest := 0, 0
	var byBase []int
	for k, part := range parts {
		for _, f := range part {
			if f.Path == "" {
				continue
			}
			if len(f.Path) > longest && names(s, f.Path) {
				at, longest = k, len(f.Path)
			}
			if names(s, path.Base(f.Path)) && !slices.Contains(byBase, k) {
				byBase = append(byBase, k)
			}
		}
	}
	if longest == 0 && len(byBase) == 1 {
		return byBase[0]
	}
	return at
}

// names reports whether name occurs in s as a whole path: after no
// character a file name holds and before no letter, digit, _ or -, so
// main.go is not named in domain.go nor cart.go in cart.golden.
func names(s, name string) bool {
	word := func(c byte) bool {
		return c == '_' || c == '-' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
	}
	for i := 0; ; {
		j := strings.Index(s[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		if (start == 0 || !word(s[start-1]) && s[start-1] != '.') && (end == len(s) || !word(s[end])) {
			return true
		}
		i = start + 1
	}
}

// diffBytes is the length of files' diff text.
func diffBytes(files []fileDiff) int {
	n := 0
	for _, f := range files {
		n += len(f.Text)
	}
	return n
}

func joinFiles(files []fileDiff) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.Text)
	}
	return b.String()
}

// site is a line the diff adds that starts or synchronises concurrent
// work.
type site struct {
	File string
	Line int
	Kind string
	Text string
}

var siteKinds = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"goroutine", regexp.MustCompile(`\bgo\s+(func\b|[\p{L}_][\p{L}\p{N}_.]*\()`)},
	{"channel", regexp.MustCompile(`\bchan\b|<-`)},
	{"lock", regexp.MustCompile(`\bsync\.(Mutex|RWMutex|Once|WaitGroup)\b|\.R?Lock\(\)`)},
	{"select", regexp.MustCompile(`\bselect\s*\{`)},
	{"atomic", regexp.MustCompile(`\batomic\.`)},
}

const (
	maxListLines = 200
	maxListDirs  = 50
	maxSiteText  = 200
)

const concurrencyHead = "every goroutine, channel, lock, select and atomic the diff adds, with file and line:"

// concurrencySites are the diff's added lines of each kind, at most one
// site per line, in the new files' lines.
func concurrencySites(files []fileDiff) []site {
	var out []site
	for _, f := range files {
		redact.Added(f.Text, func(file string, line int, text string) {
			for _, k := range siteKinds {
				if k.re.MatchString(text) {
					out = append(out, site{File: file, Line: line, Kind: k.kind, Text: textutil.Truncate(strings.TrimSpace(text), maxSiteText)})
					return
				}
			}
		})
	}
	return out
}

// concurrencyBlock renders sites one per line, at most maxListLines, under
// a line that says what they are.
func concurrencyBlock(sites []site) string {
	if len(sites) == 0 {
		return "none"
	}
	lines := []string{concurrencyHead}
	for _, s := range sites[:min(len(sites), maxListLines)] {
		lines = append(lines, fmt.Sprintf("%s:%d %s: %s", s.File, s.Line, s.Kind, s.Text))
	}
	if n := len(sites) - maxListLines; n > 0 {
		lines = append(lines, fmt.Sprintf("... and %d more", n))
	}
	return strings.Join(lines, "\n")
}
