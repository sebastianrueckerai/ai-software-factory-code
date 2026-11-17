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
	"sync"
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
// pairs and the files that held them, and the reads of a side of the diff
// that failed, the caller's to count: a side it cannot read keeps every
// rename.
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
// one. read(base, dirs) is every .go file directly in dirs at the diff's
// base or at its head, nil when unread. A pair that declares a renamed
// name stays. A pair of uses is quiet only when each renamed identifier,
// read in its package type-checked at either side (checkGo), denotes at
// the base the object a declaration pair renames and at the head that
// object renamed, found past the package's own types alone (knownPath):
// no other object can be the one either spelling names, wherever its
// twin stands. Each line must stand at its line of its side. A hunk left
// with no change is dropped, and a file left with no hunk unless its
// header holds more than its paths.
func quietRenames(files []fileDiff, read func(base bool, dirs []string) map[string][]byte) ([]fileDiff, RenameCounts) {
	type block struct {
		at, n, oldLine, newLine int // the first removed line's index, the run's length, the first removed and added lines' lines
		renames                 [][]caseRename
	}
	blocks := make([][]block, len(files))
	var headDirs, baseDirs []string
	for i, f := range files {
		if !strings.HasSuffix(f.Path, ".go") || f.OldPath != "" && !strings.HasSuffix(f.OldPath, ".go") ||
			slices.ContainsFunc(auditByPath, func(p string) bool { return strings.HasPrefix(f.Path, p) || strings.HasPrefix(f.OldPath, p) }) {
			continue
		}
		lines := strings.SplitAfter(f.Text, "\n")
		oldLine, newLine := -1, -1
		for k := 0; k < len(lines); k++ {
			l := lines[k]
			switch {
			case strings.HasPrefix(l, "@@ "):
				oldLine, newLine = -1, -1
				if m := hunkHeader.FindStringSubmatch(l); m != nil {
					oldLine, _ = strconv.Atoi(m[1])
					newLine, _ = strconv.Atoi(m[3])
				}
				continue
			case newLine < 0:
				continue
			case strings.HasPrefix(l, " "):
				oldLine, newLine = oldLine+1, newLine+1
				continue
			case strings.HasPrefix(l, "+"):
				newLine++
				continue
			case !strings.HasPrefix(l, "-"):
				continue
			}
			n := runOf(lines[k:], "-")
			m := runOf(lines[k+n:], "+")
			if n == m {
				b, found := block{at: k, n: n, oldLine: oldLine, newLine: newLine}, false
				for j := range n {
					r := caseRenames(lineBody(lines[k+j]), lineBody(lines[k+n+j]))
					b.renames, found = append(b.renames, r), found || r != nil
				}
				if found {
					blocks[i] = append(blocks[i], b)
					headDirs, baseDirs = append(headDirs, path.Dir(f.Path)), append(baseDirs, path.Dir(cmp.Or(f.OldPath, f.Path)))
				}
			}
			oldLine, newLine = oldLine+n, newLine+m
			k += n + m - 1
		}
	}
	if len(headDirs) == 0 {
		return files, RenameCounts{}
	}
	// what one side of the diff reads at a renamed identifier: the position
	// of the object it declares or denotes, "" for none
	type reading struct {
		found, declares, known bool
		denotes                string
	}
	type ident struct {
		r     caseRename
		sides [2]reading // the base's, the head's
	}
	ids := make([][][][]ident, len(files))
	for i := range files {
		ids[i] = make([][][]ident, len(blocks[i]))
		for bi, b := range blocks[i] {
			ids[i][bi] = make([][]ident, b.n)
			for j, rs := range b.renames {
				for _, r := range rs {
					ids[i][bi][j] = append(ids[i][bi][j], ident{r: r})
				}
			}
		}
	}
	// one side at a time, each type-checked package dropped before the next
	for side, dirs := range [][]string{baseDirs, headDirs} {
		keep := map[string]bool{}
		for i, f := range files {
			if len(blocks[i]) > 0 {
				keep[[]string{cmp.Or(f.OldPath, f.Path), f.Path}[side]] = true
			}
		}
		src := read(side == 0, slices.Compact(slices.Sorted(slices.Values(dirs))))
		typeChecks.Lock()
		checked := checkGo(src, keep)
		for i, f := range files {
			c, lines := checked[[]string{cmp.Or(f.OldPath, f.Path), f.Path}[side]], strings.SplitAfter(f.Text, "\n")
			for bi, b := range blocks[i] {
				for j := range b.n {
					line, body := b.oldLine+j, lineBody(lines[b.at+j])
					if side == 1 {
						line, body = b.newLine+j, lineBody(lines[b.at+b.n+j])
					}
					for k := range ids[i][bi][j] {
						id := c.identAt(line, ids[i][bi][j][k].r.off, body)
						if id == nil {
							continue
						}
						rd := reading{found: true, declares: c.declares(id), known: !c.dot && c.knownPath(id)}
						obj := c.info.Uses[id]
						if rd.declares {
							obj = c.info.Defs[id]
						}
						rd.denotes = c.position(obj)
						ids[i][bi][j][k].sides[side] = rd
					}
				}
			}
		}
		typeChecks.Unlock()
	}
	declared := map[[4]string]bool{}
	declares := func(pair []ident) bool {
		return slices.ContainsFunc(pair, func(id ident) bool { return id.sides[0].declares || id.sides[1].declares })
	}
	for i := range files {
		for bi := range blocks[i] {
			for _, pair := range ids[i][bi] {
				for _, id := range pair {
					if b, h := id.sides[0], id.sides[1]; b.declares && h.declares && b.denotes != "" && h.denotes != "" {
						declared[[4]string{id.r.old, id.r.new, b.denotes, h.denotes}] = true
					}
				}
			}
		}
	}
	renamed := func(id ident) bool {
		b, h := id.sides[0], id.sides[1]
		return b.found && h.found && b.known && h.known && declared[[4]string{id.r.old, id.r.new, b.denotes, h.denotes}]
	}
	var out []fileDiff
	var counts RenameCounts
	for i, f := range files {
		quiet := func(bi, j int) bool {
			pair := ids[i][bi][j]
			return len(pair) > 0 && !declares(pair) && !slices.ContainsFunc(pair, func(id ident) bool { return !renamed(id) })
		}
		pairs := 0
		for bi, b := range blocks[i] {
			for j := range b.n {
				if quiet(bi, j) {
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
		for bi, b := range blocks[i] {
			kept = append(kept, lines[k:b.at]...)
			removed, added := lines[b.at:b.at+b.n], lines[b.at+b.n:b.at+2*b.n]
			for j := 0; j < b.n; {
				if quiet(bi, j) {
					kept = append(kept, " "+added[j][1:])
					j++
					continue
				}
				e := j + 1
				for e < b.n && !quiet(bi, e) {
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

// typeChecks lets one side of one diff be type-checked at a time: a large
// package's check holds over a hundred MiB.
var typeChecks sync.Mutex

// checkedFile is a .go file of one side of a diff, parsed and type-checked
// with every file of its package.
type checkedFile struct {
	lines  []string
	starts []int
	idents map[int]*ast.Ident
	sels   map[*ast.Ident]*ast.SelectorExpr
	// decls are the identifiers that name a declaration Defs holds none
	// for: the package clause, and a method receiver's type, which moves
	// the method to another type
	decls map[*ast.Ident]bool
	dot   bool // the file imports a package with .
	info  *types.Info
	pkg   *types.Package
	fset  *token.FileSet
}

// identAt is the identifier of the file's syntax at off of line, from 1,
// when the line is body; nil for a nil c, and inside a literal or a
// comment, which holds none.
func (c *checkedFile) identAt(line, off int, body string) *ast.Ident {
	if c == nil || line < 1 || line > len(c.lines) || c.lines[line-1] != body {
		return nil
	}
	return c.idents[c.starts[line-1]+off]
}

// position is where obj is declared, "path:offset", a field or method of
// an instantiated generic type where the generic one is; "" for none, and
// for an object of another package, which checkGo reads empty.
func (c *checkedFile) position(obj types.Object) string {
	if obj == nil || !obj.Pos().IsValid() {
		return ""
	}
	p := c.fset.Position(obj.Pos())
	return p.Filename + ":" + strconv.Itoa(p.Offset)
}

// declares reports whether id names a declaration.
func (c *checkedFile) declares(id *ast.Ident) bool { return c.info.Defs[id] != nil || c.decls[id] }

// knownPath reports whether the member a selector's name id selects, if it
// is one, was found past only the package's own types: a type of another
// package, which checkGo reads with no members, could hold a member of
// that name no deeper and so be the one the name selects.
func (c *checkedFile) knownPath(id *ast.Ident) bool {
	sel, ok := c.sels[id]
	if !ok {
		return true
	}
	s := c.info.Selections[sel]
	if s == nil {
		return false
	}
	level := []types.Type{s.Recv()}
	for range len(s.Index()) - 1 {
		var next []types.Type
		for _, t := range level {
			st, ok := c.own(t).(*types.Struct)
			if !ok {
				return false
			}
			for k := range st.NumFields() {
				if f := st.Field(k); f.Embedded() {
					if c.own(f.Type()) == nil {
						return false
					}
					next = append(next, f.Type())
				}
			}
		}
		level = next
	}
	return true
}

// own is the struct or interface t is, past a pointer, when the package
// declares it, or it is a struct literal, and every interface it embeds
// is the package's own too; nil otherwise.
func (c *checkedFile) own(t types.Type) types.Type {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	if n, ok := t.(*types.Named); ok {
		if n.Obj().Pkg() != c.pkg {
			return nil
		}
		t = n.Underlying()
	}
	switch u := t.(type) {
	case *types.Struct:
		return u
	case *types.Interface:
		for k := range u.NumEmbeddeds() {
			if c.own(u.EmbeddedType(k)) == nil {
				return nil
			}
		}
		return u
	}
	return nil
}

// checkGo parses files, path to content, and type-checks each package of
// each directory, every other package read as an empty one (emptyImporter):
// the files of keep, checked, by path. A function body outside keep is
// dropped before the check, as no name in another file can denote what
// it declares. A directory with a file go/parser cannot read holds none.
func checkGo(files map[string][]byte, keep map[string]bool) map[string]*checkedFile {
	byDir := map[string][]string{}
	for p := range files {
		byDir[path.Dir(p)] = append(byDir[path.Dir(p)], p)
	}
	out := map[string]*checkedFile{}
	imports := emptyImporter{}
	for _, paths := range byDir {
		slices.Sort(paths)
		fset := token.NewFileSet()
		parsed := map[string]*ast.File{}
		byPkg := map[string][]string{}
		for _, p := range paths {
			f, err := parser.ParseFile(fset, p, files[p], parser.SkipObjectResolution)
			if err != nil {
				parsed = nil
				break
			}
			if !keep[p] {
				for _, d := range f.Decls {
					if fd, ok := d.(*ast.FuncDecl); ok {
						fd.Body = nil
					}
				}
			}
			parsed[p] = f
			byPkg[f.Name.Name] = append(byPkg[f.Name.Name], p)
		}
		for _, ps := range byPkg {
			if parsed == nil {
				break
			}
			asts := make([]*ast.File, len(ps))
			for k, p := range ps {
				asts[k] = parsed[p]
			}
			info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
			conf := types.Config{Importer: imports, FakeImportC: true, Error: func(error) {}}
			pkg, _ := conf.Check(path.Dir(ps[0]), fset, asts, info)
			for _, p := range ps {
				if keep[p] {
					out[p] = newCheckedFile(files[p], fset, parsed[p], info, pkg)
				}
			}
		}
	}
	return out
}

func newCheckedFile(src []byte, fset *token.FileSet, f *ast.File, info *types.Info, pkg *types.Package) *checkedFile {
	c := &checkedFile{lines: strings.Split(string(src), "\n"), starts: []int{0}, idents: map[int]*ast.Ident{},
		sels: map[*ast.Ident]*ast.SelectorExpr{}, decls: map[*ast.Ident]bool{f.Name: true}, info: info, pkg: pkg, fset: fset}
	for k, b := range src {
		if b == '\n' {
			c.starts = append(c.starts, k+1)
		}
	}
	tf := fset.File(f.Pos())
	for _, im := range f.Imports {
		c.dot = c.dot || im.Name != nil && im.Name.Name == "."
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			c.idents[tf.Offset(n.Pos())] = n
		case *ast.SelectorExpr:
			c.sels[n.Sel] = n
		case *ast.FuncDecl:
			if n.Recv != nil {
				ast.Inspect(n.Recv, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && !slices.ContainsFunc(n.Recv.List, func(f *ast.Field) bool { return slices.Contains(f.Names, id) }) {
						c.decls[id] = true
					}
					return true
				})
			}
		}
		return true
	})
	return c
}

// emptyImporter imports every package as an empty one, by its path, named
// by its path's last element: a name another package declares resolves to
// nothing, which keeps every pair that uses it.
type emptyImporter map[string]*types.Package

func (m emptyImporter) Import(p string) (*types.Package, error) {
	if pkg, ok := m[p]; ok {
		return pkg, nil
	}
	name, _, _ := strings.Cut(path.Base(p), ".")
	pkg := types.NewPackage(p, name)
	pkg.MarkComplete()
	m[p] = pkg
	return pkg, nil
}

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
