package writingguide

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

type guideWord struct{ word, short string }

// guideLists are the guide's lists of words that Lint refuses, each by
// the opening of its bullet in the guide, every word with the short word
// the guide takes instead, "" where it names none. Words whose meaning
// depends on the sentence stay out: "clean" as a verb, and member,
// operator and resource where they name no human.
var guideLists = []struct {
	bullet string
	words  []guideWord
}{
	{"Take the short common word:", []guideWord{{"begin", "start"}, {"commence", "start"}, {"utilise", "use"}, {"leverage", "use"},
		{"facilitate", "help"}, {"ensure", "make sure"}, {"prior to", "before"}, {"subsequent to", "after"}, {"regarding", "about"},
		{"obtain", "get"}, {"demonstrate", "show"}, {"additionally", "also"}, {"furthermore", "also"}}},
	{"No phrasal verbs:", []guideWord{{"spin up", ""}, {"dive into", ""}, {"kick off", ""}, {"roll out", ""}}},
	{"Avoid as self-praise:", []guideWord{{"honest", ""}, {"robust", ""}, {"comprehensive", ""}}},
	{"Never as marketing:", []guideWord{{"seamless", ""}, {"powerful", ""}, {"cutting-edge", ""}, {"effortless", ""},
		{"world-class", ""}, {"next-generation", ""}}},
	{"No intensifiers", []guideWord{{"significantly", ""}, {"dramatically", ""}, {"a significant impact", ""}}},
	{"No weasel words:", []guideWord{{"may potentially", ""}, {"can help to", ""}, {"might be able to", ""}}},
	{"Nothing", []guideWord{{"bites", ""}, {"burns", ""}, {"load-bearing", ""}}},
	{"No credibility framing", []guideWord{{"frankly", ""}}},
}

// factoryWords are the factory's own words, which no face shows. "case"
// is one only before a case number.
var factoryWords = []string{"rung", "premade"}

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
		for _, list := range guideLists {
			for _, w := range list.words {
				line := func(f string) string {
					return fmt.Sprintf("The text holds %q, which the writing guide bans. Say what happens, with a number where one fits.", f)
				}
				if w.short != "" {
					line = func(f string) string { return fmt.Sprintf("The text holds %q. Write %q instead.", f, w.short) }
				}
				rules = append(rules, wordRule{wordPattern(w.word), line})
			}
		}
		machinery := func(f string) string {
			return fmt.Sprintf("The text holds %q, a word of the factory's own machinery. Say what it means for the reader.", f)
		}
		for _, w := range factoryWords {
			rules = append(rules, wordRule{wordPattern(w), machinery})
		}
		return append(rules, wordRule{regexp.MustCompile(`(?i)\bcases?\s+#?\d{3,}\b`), machinery})
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
// contraction, a word of the guide's lists, and the factory's words rung,
// premade and case before a number of 3 digits or more. Link targets and
// code spans are no prose and pass. Each line names the fault and the fix
// and starts with "The text", so a caller may name the field in its
// place. A fault shows once.
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

// MaxChoice bounds an answer an agent offers a human, in runes: a button's
// label.
const MaxChoice = 60

// ownVoice finds a writer speaking as itself: I, me, my, we, us, our.
var ownVoice = regexp.MustCompile(`\b(?:I|[Mm]e|[Mm]y|[Mm]yself|[Ww]e|us|[Oo]urs?)\b`)

// ChoiceFaults are the faults of choice, an answer an agent offers a human
// as a button: none when it is 1 to MaxChoice runes that Lint passes and
// the writer does not speak as itself, since the human presses it.
func ChoiceFaults(choice string) []string {
	switch n := len([]rune(strings.TrimSpace(choice))); {
	case n == 0:
		return []string{"The answer is empty."}
	case n > MaxChoice:
		return []string{fmt.Sprintf("The answer has %d characters, at most %d.", n, MaxChoice)}
	}
	out := Lint(choice)
	if w := ownVoice.FindString(choice); w != "" {
		out = append(out, fmt.Sprintf("The answer says %q, but the human presses it. Name what happens, for example \"Send the new pins to the agent\".", w))
	}
	return out
}
