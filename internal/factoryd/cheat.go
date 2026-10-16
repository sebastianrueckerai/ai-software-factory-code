package factoryd

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const maxCheats = 10

var (
	unconditionalSkip = regexp.MustCompile(`^\t\w+\.Skip(?:f|Now)?\(`)
	testFunc          = regexp.MustCompile(`^func\s+((?:Test|Benchmark|Fuzz)[\p{L}\p{N}_]*)\(`)
	buildConstraint   = regexp.MustCompile(`^//(?:go:build|\s*\+build)\s`)
	hunkHead          = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)`)
)

// walkHunks calls fn with every added ('+', new line number) and removed
// ('-', old line number) line of a file's diff.
func walkHunks(text string, fn func(kind byte, line int, s string)) {
	oldLine, newLine, in := 0, 0, false
	for _, l := range strings.Split(text, "\n") {
		if m := hunkHead.FindStringSubmatch(l); m != nil {
			in = true
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			continue
		}
		if !in || l == "" {
			continue
		}
		switch l[0] {
		case '+':
			fn('+', newLine, l[1:])
			newLine++
		case '-':
			fn('-', oldLine, l[1:])
			oldLine++
		case ' ':
			oldLine++
			newLine++
		}
	}
}

// cheats are the named shapes of a diff that skips, removes or excludes a
// Go test, at most maxCheats, each a finding for the agent.
func cheats(files []fileDiff) []string {
	var out []string
	added := map[string]bool{}
	type removal struct {
		file, name string
		line       int
	}
	var removed []removal
	for _, f := range files {
		test := strings.HasSuffix(f.Path, "_test.go")
		oldTest := strings.HasSuffix(cmp.Or(f.OldPath, f.Path), "_test.go")
		isNew := strings.Contains(f.Text, "\n--- /dev/null")
		if f.OldPath != "" && oldTest && !test {
			out = append(out, fmt.Sprintf("anti-cheat: %s is renamed to %s and leaves the test suite. Keep the test file's name.", f.OldPath, f.Path))
		}
		walkHunks(f.Text, func(kind byte, line int, s string) {
			m := testFunc.FindStringSubmatch(s)
			switch {
			case kind == '+' && test && unconditionalSkip.MatchString(s):
				out = append(out, fmt.Sprintf("anti-cheat: %s:%d adds a skip to a test: %s. Make the test pass; do not skip it.", f.Path, line, strings.TrimSpace(s)))
			case kind == '+' && test && !isNew && buildConstraint.MatchString(s):
				out = append(out, fmt.Sprintf("anti-cheat: %s:%d adds a build constraint to a test file that ran on every build: %s. Keep the test in the build.", f.Path, line, strings.TrimSpace(s)))
			case kind == '+' && m != nil:
				added[m[1]] = true
			case kind == '-' && m != nil && oldTest:
				removed = append(removed, removal{f.Path, m[1], line})
			}
		})
	}
	for _, r := range removed {
		if !added[r.name] {
			out = append(out, fmt.Sprintf("anti-cheat: %s:%d (base) removes %s and the diff adds no test of that name. Restore the test or say in the code why it goes.", r.file, r.line, r.name))
		}
	}
	return out[:min(len(out), maxCheats)]
}
