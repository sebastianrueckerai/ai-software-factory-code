package bump

import (
	"fmt"
	"strings"
)

// The words after a list line's reference and its kind that refine its
// line: patch keeps it in its minor, hold never moves it, and with <name>
// gives it the version of the line of that image name.
const (
	RulePatch = "patch"
	RuleHold  = "hold"
	RuleWith  = "with"
)

// imageLine is a line of upstream-images.txt: a reference, name:tag@digest
// or an https URL@sha256:<sum>, what the store makes of it, and its rule.
type imageLine struct {
	ref  string
	kind string // "", bootstrap, helm or onto
	base string // onto's image, name:tag
	rule string // "", patch, hold or with
	lead string // with's image name
}

// parseImageLine reads a pin line of upstream-images.txt.
func parseImageLine(line string) (imageLine, error) {
	f := strings.Fields(line)
	l := imageLine{ref: f[0]}
	f = f[1:]
	bad := fmt.Errorf("%q: want a reference, then bootstrap, helm or onto <name>:<tag>, then patch, hold or with <name>", line)
	switch {
	case len(f) > 0 && (f[0] == "bootstrap" || f[0] == "helm"):
		l.kind, f = f[0], f[1:]
	case len(f) > 1 && f[0] == "onto":
		l.kind, l.base, f = f[0], f[1], f[2:]
	}
	switch {
	case len(f) == 1 && (f[0] == RulePatch || f[0] == RuleHold):
		l.rule = f[0]
	case len(f) == 2 && f[0] == RuleWith:
		l.rule, l.lead = f[0], f[1]
	case len(f) != 0:
		return imageLine{}, bad
	}
	return l, nil
}

// pinLine says whether a list line pins something: not blank, no comment.
func pinLine(line string) bool {
	f := strings.Fields(line)
	return len(f) > 0 && !strings.HasPrefix(f[0], "#")
}

// splitImage splits name:tag@sha256:<hex>.
func splitImage(ref string) (name, tag, digest string, ok bool) {
	image, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return "", "", "", false
	}
	at := strings.LastIndex(image, ":")
	if at < 0 || strings.Contains(image[at:], "/") {
		return "", "", "", false
	}
	return image[:at], image[at+1:], digest, true
}

// rewriteImages moves each pin line of list whose reference moved holds,
// old to new, keeping its words, and an onto base with its image.
func rewriteImages(list string, moved map[string]string) string {
	bases := map[string]string{}
	for old, now := range moved {
		if o, _, ok := strings.Cut(old, "@"); ok && !strings.HasPrefix(old, "https://") {
			bases[o], _, _ = strings.Cut(now, "@")
		}
	}
	lines := strings.Split(list, "\n")
	for i, line := range lines {
		if !pinLine(line) {
			continue
		}
		f := strings.Fields(line)
		if now, ok := moved[f[0]]; ok {
			f[0] = now
		}
		if len(f) > 2 && f[1] == "onto" && bases[f[2]] != "" {
			f[2] = bases[f[2]]
		}
		lines[i] = strings.Join(f, " ")
	}
	return strings.Join(lines, "\n")
}

// fileLine is a line of upstream-files.txt: a file's package, version,
// file name, URL and sha256.
type fileLine struct {
	pkg, version, file, url, sum string
}

func parseFileLine(line string) (fileLine, error) {
	f := strings.Fields(line)
	if len(f) != 5 {
		return fileLine{}, fmt.Errorf("%q: want <package> <version> <file> <url> <sha256>", line)
	}
	return fileLine{f[0], f[1], f[2], f[3], f[4]}, nil
}

func (l fileLine) String() string {
	return strings.Join([]string{l.pkg, l.version, l.file, l.url, l.sum}, " ")
}
