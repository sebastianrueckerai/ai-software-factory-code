package factoryd

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// fileDiff is one file's part of a unified git diff, its header and every
// hunk byte for byte. Path is the new path (a deleted file's old one);
// OldPath is the old path of a rename or copy. Added and Removed count the
// hunks' + and - lines.
type fileDiff struct {
	Path, OldPath  string
	Text           string
	Added, Removed int
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
// stay within limit; a file larger than limit alone is tooBig.
func packFiles(files []fileDiff, limit int) (chunks [][]fileDiff, tooBig []fileDiff) {
	size := 0
	for _, f := range files {
		switch n := len(f.Text); {
		case n > limit:
			tooBig = append(tooBig, f)
		case len(chunks) == 0 || size+n > limit:
			chunks, size = append(chunks, []fileDiff{f}), n
		default:
			chunks[len(chunks)-1] = append(chunks[len(chunks)-1], f)
			size += n
		}
	}
	return chunks, tooBig
}

// diffParts packs diff's files into at most reviewChunks parts of
// reviewDiffMax, one model call each, at least one part. why says why the
// model cannot read the whole diff so ("" when it can); reader names the
// gate in it.
func diffParts(diff, reader string) (files []fileDiff, parts [][]fileDiff, why string) {
	files = splitDiff(diff)
	parts, tooBig := packFiles(files, reviewDiffMax)
	size := fmt.Sprintf("%s, %d KiB", textutil.Plural(len(files), "file", "files"), kib(len(diff)))
	var reason string
	switch {
	case strings.Contains(diff, diffCut):
		size, reason = "more than 1 MiB of diff", "Forgejo's answer is cut at 1 MiB"
	case len(tooBig) > 0:
		reason = fmt.Sprintf("%s alone is %d KiB", tooBig[0].Path, kib(len(tooBig[0].Text)))
	case len(parts) > reviewChunks:
		reason = "too many parts"
	case len(parts) == 0:
		return files, [][]fileDiff{nil}, ""
	default:
		return files, parts, ""
	}
	return files, nil, fmt.Sprintf("%s; the %s reads at most %d parts of %d KiB (%s)", size, reader, reviewChunks, reviewDiffMax>>10, reason)
}

func kib(n int) int { return (n + 1023) >> 10 }

// partLabel names part i of parts and its files among all.
func partLabel(parts [][]fileDiff, i, all int) string {
	first := 1
	for _, p := range parts[:i] {
		first += len(p)
	}
	files := fmt.Sprintf("files %d to %d", first, first+len(parts[i])-1)
	if len(parts[i]) == 1 {
		files = fmt.Sprintf("file %d", first)
	}
	return fmt.Sprintf("%s of %d, part %d of %d", files, all, i+1, len(parts))
}

// fileList renders every file of parts, one line each with its added and
// removed lines and its part, at most maxListLines.
func fileList(parts [][]fileDiff) string {
	var lines []string
	n := 0
	for k, part := range parts {
		for _, f := range part {
			if n++; n > maxListLines {
				continue
			}
			name := f.Path
			if f.OldPath != "" {
				name = f.OldPath + " -> " + f.Path
			}
			lines = append(lines, fmt.Sprintf("%s +%d -%d (part %d)", name, f.Added, f.Removed, k+1))
		}
	}
	if n > maxListLines {
		lines = append(lines, fmt.Sprintf("... and %d more", n-maxListLines))
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
