package facefixtures

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// Forbidden are the words no text a human reads shows: a task id, a
// hash and a branch anywhere, and outside a path the factory's own nouns,
// a snake_case name and a gate's step name.
var Forbidden = []*regexp.Regexp{
	regexp.MustCompile(`\bt-[0-9a-f]{6,}`),
	regexp.MustCompile(`\b[0-9a-f]{12,}\b`),
	regexp.MustCompile(`\bcard/\d+`),
	regexp.MustCompile(`(?i)\b(rungs?|cases?|offers?|premade|slots?)\b`),
	regexp.MustCompile(`\b[a-z]+_[a-z_]+\b`),
	regexp.MustCompile(`\b(` + strings.Join([]string{factoryd.ActivityRunSession, factoryd.ActivityWaitChecks, factoryd.ActivityGateReproduction,
		factoryd.ActivityGateCriteria, factoryd.ActivityModelReview, factoryd.ActivityGateAudit}, "|") + `)\b`),
}

var (
	codeSpan = regexp.MustCompile("`[^`]*`")
	// a path or a URL names a place, not code
	place = regexp.MustCompile(`\S*/\S*|\S+\.(?:go|md|ya?ml|json|js|ts|html|sql|sh|tf)\b(?::\d+)?`)
	// codeWord is a name of code in prose: a word with a capital inside
	// that starts small, or that starts capital and has three or more
	// capitals (a product's name, GitHub or SigNoz, has two)
	codeWord  = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z0-9]*\b|\b[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]+){2,}\b`)
	notAHuman = regexp.MustCompile(`(?i)\b(members?|operators?)\b`)
	// selfNamed is a text that opens with the service's name as its writer
	selfNamed = regexp.MustCompile(`^factoryd:`)
	// quoteLine is a line of markup that quotes a writer's words
	quoteLine = regexp.MustCompile(`(?m)^>.*$`)
)

// TextFaults are the faults of text a human reads, by the guide's checks
// (writingguide.Lint) and the face's: a forbidden word, a name of the
// factory's machinery, a code word, a human called something else, and a
// text that signs itself with the service's name. A code span is no
// prose, and neither is a path or a URL. A writer's words quoted in markup
// pass as written.
func TextFaults(text string) []string {
	text = quoteLine.ReplaceAllString(text, "")
	faults := writingguide.Lint(text)
	prose := codeSpan.ReplaceAllString(text, " ")
	words := place.ReplaceAllString(prose, " ")
	for i, re := range Forbidden {
		in := words
		if i < 3 {
			in = prose
		}
		if m := re.FindString(in); m != "" {
			faults = append(faults, fmt.Sprintf("The text names %q.", m))
		}
	}
	if m := factoryd.FactoryName(words); m != "" {
		faults = append(faults, fmt.Sprintf("The text names the machinery's %q.", m))
	}
	if m := codeWord.FindString(words); m != "" {
		faults = append(faults, fmt.Sprintf("The text names the code word %q.", m))
	}
	if m := notAHuman.FindString(prose); m != "" {
		faults = append(faults, fmt.Sprintf("The text calls a human %q.", m))
	}
	if selfNamed.MatchString(strings.TrimSpace(text)) {
		faults = append(faults, "The text signs itself with the service's name.")
	}
	return faults
}

// HumanFaults are the faults of text a human reads at a door, a refusal or
// a comment, as factoryd.ForHuman renders it for a human: TextFaults, and
// a code span that names no path or URL, as a human reads an answer by its
// label and types no key.
func HumanFaults(text string) []string {
	faults := TextFaults(text)
	for _, span := range codeSpan.FindAllString(quoteLine.ReplaceAllString(text, ""), -1) {
		if inner := strings.Trim(span, "`"); place.ReplaceAllString(inner, "") != "" {
			faults = append(faults, fmt.Sprintf("The text names the key %s, which a human does not type.", span))
		}
	}
	return faults
}
