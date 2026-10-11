package redact

import (
	"regexp"
	"strconv"
	"strings"
)

// Hit is an added line of a diff that holds a secret's shape: its file
// and line in the new version, and the shape. It never carries the value.
type Hit struct {
	File string
	Line int
	Kind string
}

// The secret shapes ScanDiff knows.
const (
	KindPrivateKey = "private key"
	KindConnection = "connection string with credentials"
	KindCredential = "bearer or basic credential" // #nosec G101 -- a shape's name
	KindJWT        = "JSON web token"
	KindAPIKey     = "API key (sk-)" // #nosec G101 -- a shape's name
	KindGitHub     = "GitHub token"
	KindGitLab     = "GitLab token"
	KindAWS        = "AWS access key"
	KindAssignment = "password, secret or token assigned a literal"
)

// shapes are the secret shapes; a named group v is the secret's value,
// else the whole match is; skip, when set, refuses a match by its key k
// and its value.
var shapes = []struct {
	kind string
	re   *regexp.Regexp
	skip func(key, value string) bool
}{
	{KindPrivateKey, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), nil},
	{KindConnection, regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:(?P<v>[^/\s@]+)@`), nil},
	{KindJWT, regexp.MustCompile(`\beyJ[\w-]{10,}\.[\w-]{10,}\.[\w-]*`), nil},
	{KindAPIKey, regexp.MustCompile(`\bsk-[\w-]{20,}`), nil},
	{KindGitHub, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), nil},
	{KindGitLab, regexp.MustCompile(`\bglpat-[\w-]{20,}`), nil},
	{KindAWS, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), nil},
	{KindAssignment, regexp.MustCompile(`(?i)(?P<k>[\w-]*?(password|passwd|secret|token|api[_-]?key|access[_-]?key)[\w-]*)["']?\s*(:=|=|:)\s*["'](?P<v>[^"'\s]{8,})["']`),
		func(key, value string) bool {
			return reference(key) || strings.ContainsAny(value[:1], "<${") || strings.Contains(value, "://")
		}},
	// an .env, YAML or Dockerfile line: a password unquoted, but no
	// ${...}, <placeholder>, call, path or dotted reference
	{KindAssignment, regexp.MustCompile(`(?i)^\s*(export\s+|ENV\s+)?(?P<k>[\w-]*(password|passwd)[\w-]*)\s*[=:]\s*(?P<v>[^\s"'$<{(,}]{8,})\s*$`),
		func(key, value string) bool {
			return reference(key) || strings.HasPrefix(value, "/") || strings.Contains(value, ".")
		}},
}

var (
	secretWord = regexp.MustCompile(`password|passwd|secret|token|api[_-]?key|access[_-]?key`)
	refSuffix  = regexp.MustCompile(`(file|secret|ref|name|url)$`)
)

// reference: the key names where a secret lives (password_file,
// existingPasswordSecret, secretName, tokenURL), not the secret: after
// its first secret word it ends in file, secret, ref, name or url.
func reference(key string) bool {
	k := strings.ToLower(key)
	loc := secretWord.FindStringIndex(k)
	return loc != nil && refSuffix.MatchString(k[loc[1]:])
}

// matches are re's matches on text that skip does not refuse, each with
// its value.
func matches(re *regexp.Regexp, skip func(key, value string) bool, text string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		v := m[0]
		if i := re.SubexpIndex("v"); i > 0 {
			v = m[i]
		}
		if skip != nil && skip(m[re.SubexpIndex("k")], v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// hunkStart reads the new version's first line from a hunk header.
var hunkStart = regexp.MustCompile(`^@@ -[0-9,]+ \+([0-9]+)`)

// Added calls f with every added line of a unified git diff, its file and
// its line in the new version.
func Added(diff string, f func(file string, line int, text string)) {
	file, line, inHunk := "", 0, false
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			file, inHunk = "", false
		case !inHunk && strings.HasPrefix(l, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(l, "@@ "):
			m := hunkStart.FindStringSubmatch(l)
			inHunk = m != nil
			if inHunk {
				line, _ = strconv.Atoi(m[1])
			}
		case !inHunk:
		case strings.HasPrefix(l, "+"):
			if file != "" {
				f(file, line, l[1:])
			}
			line++
		case strings.HasPrefix(l, " "):
			line++
		}
	}
}

// ScanDiff checks every added line of a unified git diff for the secret
// shapes: one hit per line, the first shape that matches.
func ScanDiff(diff string) []Hit {
	var hits []Hit
	Added(diff, func(file string, line int, text string) {
		if kind := shapeOf(text); kind != "" {
			hits = append(hits, Hit{File: file, Line: line, Kind: kind})
		}
	})
	return hits
}

// DiffSecrets are the values of every secret shape on the diff's added
// lines, and the body lines of an added PEM private key, for New: a
// scrubber that also hides what the pattern rules miss.
func DiffSecrets(diff string) []string {
	var out []string
	pem := false
	Added(diff, func(_ string, _ int, text string) {
		switch {
		case pem && strings.Contains(text, "-----END "):
			pem = false
			return
		case pem:
			if v := strings.TrimSpace(text); v != "" {
				out = append(out, v)
			}
			return
		}
		for _, s := range shapes {
			if s.kind == KindPrivateKey {
				pem = pem || s.re.MatchString(text) && !strings.Contains(text, "-----END ")
				continue
			}
			out = append(out, matches(s.re, s.skip, text)...)
		}
		if m := credential.FindStringSubmatch(text); m != nil && !shortWord.MatchString(m[2]) {
			out = append(out, m[2])
		}
	})
	return out
}

func shapeOf(l string) string {
	for _, s := range shapes {
		if len(matches(s.re, s.skip, l)) > 0 {
			return s.kind
		}
	}
	if m := credential.FindStringSubmatch(l); m != nil && !shortWord.MatchString(m[2]) {
		return KindCredential
	}
	return ""
}
