package writingguide

import (
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
		{"offer", "The face offers three answers.",
			[]string{`The text holds "offers", a word of the factory's own machinery. Say what it means for the reader.`}},
		{"slot", "The card waits for a slot.",
			[]string{`The text holds "slot", a word of the factory's own machinery. Say what it means for the reader.`}},
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
	} {
		if got := Lint(text); len(got) != 0 {
			t.Errorf("Lint(%q) = %q, want none", text, got)
		}
	}
}

// Every word Lint refuses for the guide is a word of the guide's lists.
func TestLintWordsAreTheGuides(t *testing.T) {
	for _, w := range guideWords {
		if !strings.Contains(Guide, w.word) {
			t.Errorf("%q is in no list of the guide", w.word)
		}
	}
}
