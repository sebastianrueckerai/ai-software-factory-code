package writingguide

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestLint(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"an em dash", "The card waits — the plan holds it.",
			[]string{"The text holds an em dash. Write two sentences, or use a comma."}},
		{"an en dash as punctuation", "The card waits – the plan holds it.",
			[]string{"The text holds an en dash as punctuation. Write two sentences, or use a comma."}},
		{"a semicolon", "The card waits; the plan holds it.",
			[]string{"The text holds a semicolon. Write two sentences."}},
		{"a contraction", "The card doesn't wait, and it's done.",
			[]string{`The text holds the contraction "doesn't". Write the words out in full.`,
				`The text holds the contraction "it's". Write the words out in full.`}},
		{"a long word of the guide", "Leverage the cache to ensure a hit.",
			[]string{`The text holds "Leverage". Write "use" instead.`, `The text holds "ensure". Write "make sure" instead.`}},
		{"a guide word with no short form", "A robust fix of the gate.",
			[]string{`The text holds "robust", which the writing guide bans. Say what happens, with a number where one fits.`}},
		{"a phrasal verb", "Kick off the build.",
			[]string{`The text holds "Kick off", which the writing guide bans. Say what happens, with a number where one fits.`}},
		{"rung", "The card climbs a rung.",
			[]string{`The text holds "rung", a word of the factory's own machinery. Say what it means for the reader.`}},
		{"a case number", "The ask of case 5631 waits.",
			[]string{`The text holds "case 5631", a word of the factory's own machinery. Say what it means for the reader.`}},
		{"premade", "Pick a premade answer.",
			[]string{`The text holds "premade", a word of the factory's own machinery. Say what it means for the reader.`}},
		{"a case number with its hash", "Case #131 is open.",
			[]string{`The text holds "Case #131", a word of the factory's own machinery. Say what it means for the reader.`}},
		{"a fault once per class", "One; two; three.",
			[]string{"The text holds a semicolon. Write two sentences."}},
	} {
		if got := Lint(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("%s: Lint(%q) = %q, want %q", tc.name, tc.text, got, tc.want)
		}
	}
}

func TestLintAcceptsPlainText(t *testing.T) {
	for _, text := range []string{
		"Narrow the close refusal to plans that hold criteria.",
		"The card's title names the work, and the why line gives the reason.",
		"The title has 1 to 70 characters.",
		"A human reads the read-only view of the plan.",
		"So a plan with no criteria closes without a waiver.",
		"Pages 3–5 hold the list, and the beginner starts at running tests.",
		"Read [the goals](docs/case-5631;slot.md#offer) and `don't; rung` as they stand.",
		"In case of a red check, the card waits.",
		"The console offers a retry.",
		"In case 2 runs fail, the card waits.",
		"The card waits for a free slot in the queue.",
	} {
		if got := Lint(text); len(got) != 0 {
			t.Errorf("Lint(%q) = %q, want none", text, got)
		}
	}
}

// Every word Lint refuses for the guide stands in the guide's bullet of
// its list, as a whole word.
func TestLintWordsAreTheGuides(t *testing.T) {
	for _, list := range guideLists {
		var bullet string
		for _, line := range strings.Split(Guide, "\n") {
			if strings.HasPrefix(line, "- "+list.bullet) {
				bullet = line
			}
		}
		if bullet == "" {
			t.Errorf("the guide has no bullet %q", list.bullet)
			continue
		}
		for _, w := range list.words {
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(w.word) + `\b`).MatchString(bullet) {
				t.Errorf("%q is not in the guide's list %q", w.word, list.bullet)
			}
		}
	}
}

// An answer a writer offers a human is a button the human presses: short,
// in the guide's words, and never in the writer's own voice.
func TestChoiceFaults(t *testing.T) {
	for choice, want := range map[string]bool{
		"Send the new pins to the agent":                 true,
		"Open the upstream hosts to this sandbox":        true,
		"Move them to the US region":                     true,
		"Provide the new pins and I apply them":          false,
		"Let me retry with a stronger model":             false,
		"We wait for the release":                        false,
		"Use our mirror":                                 false,
		"Provide the new pins; then test them":           false,
		"":                                               false,
		strings.Repeat("Send the pins to the agent ", 3): false,
	} {
		if got := len(ChoiceFaults(choice)) == 0; got != want {
			t.Errorf("ChoiceFaults(%q) = %v, want it to pass: %v", choice, ChoiceFaults(choice), want)
		}
	}
}
