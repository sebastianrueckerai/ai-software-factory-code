package factoryd

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// mapHeading opens a card's repository map.
const mapHeading = "Repository map (the card's directories at the commit this session starts from, each file with its first doc line):"

const (
	mapMax     = 4 << 10
	mapLineMax = 120
	// mapHead is how much of a file is read for its doc line, and where a
	// NUL byte marks it binary, as git judges.
	mapHead = 8000
)

// mapDir is a directory name the map prints: an issue author writes the
// Directories line, and the map stands outside the brief's markers.
var mapDir = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// RepoMap is the brief's section "Repository map": for each of dirs in
// fsys, the work tree a session starts from, its files with their first
// doc line and its subdirectories with a slash, not recursive, one line
// each cut at 120 bytes, the section at most 4 KiB with the lines past it
// counted in its last; "" for no directories (ruling 273).
func RepoMap(fsys fs.FS, dirs []string) string {
	var lines, seen []string
	for _, d := range dirs {
		dir := path.Clean(d)
		if !mapDir.MatchString(d) || slices.Contains(seen, dir) {
			continue
		}
		seen = append(seen, dir)
		entries, err := fs.ReadDir(fsys, dir)
		if !fs.ValidPath(dir) || err != nil {
			lines = append(lines, dir+"/: not in the repository")
			continue
		}
		for _, e := range entries {
			name := path.Join(dir, e.Name())
			switch {
			case e.Name() == ".git":
			case e.IsDir():
				lines = append(lines, name+"/")
			case e.Type().IsRegular():
				lines = append(lines, mapLine(name, docLine(fsys, name)))
			default:
				lines = append(lines, name)
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	out, size := []string{mapHeading}, len(mapHeading)
	for i, l := range lines {
		need := size + 1 + len(l)
		if rest := len(lines) - i - 1; rest > 0 {
			need += 1 + len(moreFiles(rest))
		}
		if need > mapMax {
			out = append(out, moreFiles(len(lines)-i))
			break
		}
		out, size = append(out, l), size+1+len(l)
	}
	return strings.Join(out, "\n")
}

func moreFiles(n int) string { return fmt.Sprintf("... and %d more files", n) }

func mapLine(name, doc string) string {
	l := name
	if doc != "" {
		l += ": " + doc
	}
	if len(l) <= mapLineMax {
		return l
	}
	return textutil.Truncate(l, mapLineMax-len("…"))
}

// docLine is file name's first doc line: a Go file's first comment line
// before its package clause, else the first # line that is no shebang (a
// markdown heading, a YAML or shell comment); "" for none, a binary or an
// unreadable file.
func docLine(fsys fs.FS, name string) string {
	f, err := fsys.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, mapHead)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return ""
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return ""
	}
	if path.Ext(name) == ".go" {
		return strings.ToValidUTF8(goDoc(head), "")
	}
	for l := range strings.Lines(string(head)) {
		l = strings.TrimSpace(l)
		if t := strings.TrimSpace(strings.TrimLeft(l, "#")); strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "#!") && t != "" {
			return strings.ToValidUTF8(t, "")
		}
	}
	return ""
}

// goDoc is the first line of src's first comment before its package
// clause, directives left out.
func goDoc(src []byte) string {
	f, _ := parser.ParseFile(token.NewFileSet(), "", src, parser.PackageClauseOnly|parser.ParseComments)
	if f == nil {
		return ""
	}
	for _, g := range f.Comments {
		if f.Package.IsValid() && g.Pos() > f.Package {
			break
		}
		if t := g.Text(); t != "" {
			first, _, _ := strings.Cut(t, "\n")
			return first
		}
	}
	return ""
}
