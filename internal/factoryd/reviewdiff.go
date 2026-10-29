package factoryd

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
			a, b, _ := strings.Cut(strings.TrimPrefix(l, "diff --git "), " b/")
			oldPath, newPath = strings.TrimPrefix(a, "a/"), b
		case strings.HasPrefix(l, "rename from "), strings.HasPrefix(l, "copy from "):
			_, oldPath, _ = strings.Cut(l, " from ")
		case strings.HasPrefix(l, "rename to "), strings.HasPrefix(l, "copy to "):
			_, newPath, _ = strings.Cut(l, " to ")
		case l == "--- /dev/null":
			oldPath = ""
		case strings.HasPrefix(l, "--- a/"):
			oldPath = strings.TrimSuffix(strings.TrimPrefix(l, "--- a/"), "\t")
		case l == "+++ /dev/null":
			newPath = ""
		case strings.HasPrefix(l, "+++ b/"):
			newPath = strings.TrimSuffix(strings.TrimPrefix(l, "+++ b/"), "\t")
		}
	}
	f.Path = cmp.Or(newPath, oldPath)
	if oldPath != f.Path {
		f.OldPath = oldPath
	}
	return f
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

// subHeaderRoom is the room a recomputed hunk header needs.
const subHeaderRoom = 80

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// cutFile cuts f into pieces of at most limit bytes: f itself when it
// fits. Each piece starts with f's header, then whole hunks while they fit.
// A hunk that does not fit beside the header is split at line boundaries
// into sub-hunks with recomputed headers, and a line that does not fit is
// cut at rune boundaries into lines that keep its prefix.
func cutFile(f fileDiff, limit int) []fileDiff {
	if len(f.Text) <= limit {
		return []fileDiff{f}
	}
	var texts []string
	if at := strings.Index(f.Text, "\n@@ "); at < 0 || at+1+subHeaderRoom+8 > limit {
		for rest := f.Text; rest != ""; {
			n := runeCut(rest, limit)
			texts, rest = append(texts, rest[:n]), rest[n:]
		}
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
		return []string{hunk}
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
	oldN, newN := 0, 0
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
		nextOld, nextNew = nextOld+oldN, nextNew+newN
		lines.Reset()
		oldN, newN = 0, 0
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
		for _, p := range pieces {
			if lines.Len()+len(p) > room {
				flush()
			}
			lines.WriteString(p)
			switch p[0] {
			case '+':
				newN++
			case '-':
				oldN++
			case '\\':
			default:
				oldN++
				newN++
			}
		}
	}
	flush()
	return out
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
// them are one line a directory. unit names a group: "part" or "card".
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
	for _, d := range dirs {
		t := totals[d]
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
