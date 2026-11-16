package writingguide

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// guideWords are the words of the guide's lists that Lint refuses, each
// with the short word the guide takes instead, "" where it names none.
// Words whose meaning depends on the sentence stay out: "clean" as a verb,
// and member, operator and resource where they name no human.
var guideWords = []struct{ word, short string }{
	{"begin", "start"}, {"commence", "start"}, {"utilise", "use"}, {"leverage", "use"}, {"facilitate", "help"},
	{"ensure", "make sure"}, {"prior to", "before"}, {"subsequent to", "after"}, {"regarding", "about"},
	{"obtain", "get"}, {"demonstrate", "show"}, {"additionally", "also"}, {"furthermore", "also"},
	{"spin up", ""}, {"dive into", ""}, {"kick off", ""}, {"roll out", ""},
	{"honest", ""}, {"robust", ""}, {"comprehensive", ""},
	{"seamless", ""}, {"powerful", ""}, {"cutting-edge", ""}, {"effortless", ""}, {"world-class", ""}, {"next-generation", ""},
	{"significantly", ""}, {"dramatically", ""}, {"a significant impact", ""},
	{"may potentially", ""}, {"can help to", ""}, {"might be able to", ""},
	{"bites", ""}, {"burns", ""}, {"load-bearing", ""}, {"frankly", ""},
}

// factoryWords are the factory's own words, which no face shows.
var factoryWords = []string{"rung", "premade", "offer", "slot"}

type wordRule struct {
	re   *regexp.Regexp
	line func(found string) string
}

var (
	linkTarget  = regexp.MustCompile(`\]\([^)]*\)`)
	codeSpan    = regexp.MustCompile("`[^`]*`")
	contraction = regexp.MustCompile(`(?i)\b(?:[a-z]+n['’]t|[a-z]+['’](?:re|ve|ll|d|m)|(?:it|that|there|here|what|who|where|how|when|why|let|he|she)['’]s)\b`)
	wordRules   = func() []wordRule {
		rules := []wordRule{{contraction, func(f string) string {
			return fmt.Sprintf("The text holds the contraction %q. Write the words out in full.", f)
		}}}
		for _, w := range guideWords {
			line := func(f string) string {
				return fmt.Sprintf("The text holds %q, which the writing guide bans. Say what happens, with a number where one fits.", f)
			}
			if w.short != "" {
				line = func(f string) string { return fmt.Sprintf("The text holds %q. Write %q instead.", f, w.short) }
			}
			rules = append(rules, wordRule{wordPattern(w.word), line})
		}
		machinery := func(f string) string {
			return fmt.Sprintf("The text holds %q, a word of the factory's own machinery. Say what it means for the reader.", f)
		}
		for _, w := range factoryWords {
			rules = append(rules, wordRule{wordPattern(w), machinery})
		}
		return append(rules, wordRule{regexp.MustCompile(`(?i)\bcases?\s+#?\d+\b`), machinery})
	}()
)

// wordPattern matches w as a whole word, case-insensitive, with its
// common endings: a single word takes s, ed and ing, one that ends in e
// drops it before them. A phrase matches as written.
func wordPattern(w string) *regexp.Regexp {
	p := strings.ReplaceAll(regexp.QuoteMeta(w), " ", `\s+`)
	switch {
	case strings.ContainsAny(w, " -"):
	case strings.HasSuffix(w, "e"):
		p = p[:len(p)-1] + `(?:e|es|ed|ing)`
	default:
		p += `(?:s|es|ed|ing)?`
	}
	return regexp.MustCompile(`(?i)\b` + p + `\b`)
}

// Lint is every fault of text against the guide's rules a check can
// hold: an em dash, an en dash as punctuation, a semicolon, a
// contraction, a word of the guide's lists, and the factory's words
// rung, case N, premade, offer and slot. Link targets and code spans are
// no prose and pass. Each line names the fault and the fix and starts
// with "The text", so a caller may name the field in its place. A fault
// shows once.
func Lint(text string) []string {
	text = codeSpan.ReplaceAllString(linkTarget.ReplaceAllString(text, "]"), " ")
	var out []string
	if strings.ContainsRune(text, '—') {
		out = append(out, "The text holds an em dash. Write two sentences, or use a comma.")
	}
	if enDashPunctuation(text) {
		out = append(out, "The text holds an en dash as punctuation. Write two sentences, or use a comma.")
	}
	if strings.ContainsRune(text, ';') {
		out = append(out, "The text holds a semicolon. Write two sentences.")
	}
	type hit struct {
		at   int
		line string
	}
	var hits []hit
	seen := map[string]bool{}
	for _, r := range wordRules {
		for _, at := range r.re.FindAllStringIndex(text, -1) {
			found := text[at[0]:at[1]]
			if key := strings.ToLower(found); !seen[key] {
				seen[key] = true
				hits = append(hits, hit{at[0], r.line(found)})
			}
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Compare(a.at, b.at) })
	for _, h := range hits {
		out = append(out, h.line)
	}
	return out
}

// enDashPunctuation is whether text holds an en dash that is not a
// range between two digits.
func enDashPunctuation(text string) bool {
	r := []rune(text)
	for i, c := range r {
		if c == '–' && (i == 0 || i == len(r)-1 || !unicode.IsDigit(r[i-1]) || !unicode.IsDigit(r[i+1])) {
			return true
		}
	}
	return false
}
