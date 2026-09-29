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

var shapes = []struct {
	kind string
	re   *regexp.Regexp
}{
	{KindPrivateKey, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{KindConnection, regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`)},
	{KindJWT, regexp.MustCompile(`\beyJ[\w-]{10,}\.[\w-]{10,}\.[\w-]*`)},
	{KindAPIKey, regexp.MustCompile(`\bsk-[\w-]{20,}`)},
	{KindGitHub, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{KindGitLab, regexp.MustCompile(`\bglpat-[\w-]{20,}`)},
	{KindAWS, regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{KindAssignment, regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|access_?key)\w*["']?\s*(:=|=|:)\s*["'][^"'\s]{8,}["']`)},
}

// hunkStart reads the new version's first line from a hunk header.
var hunkStart = regexp.MustCompile(`^@@ -[0-9,]+ \+([0-9]+)`)

// ScanDiff checks every added line of a unified git diff for the secret
// shapes: one hit per line, the first shape that matches.
func ScanDiff(diff string) []Hit {
	var hits []Hit
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
			if kind := shapeOf(l[1:]); kind != "" && file != "" {
				hits = append(hits, Hit{File: file, Line: line, Kind: kind})
			}
			line++
		case strings.HasPrefix(l, " "):
			line++
		}
	}
	return hits
}

func shapeOf(l string) string {
	for _, s := range shapes {
		if s.re.MatchString(l) {
			return s.kind
		}
	}
	if m := credential.FindStringSubmatch(l); m != nil && !shortWord.MatchString(m[2]) {
		return KindCredential
	}
	return ""
}
