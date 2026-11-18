package factoryd

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/writingguide"
)

// Each factory term reads as one sentence of at most 140 characters that
// passes the writing guide, and the plan's ten terms and the words the faces use for the factory's
// own parts are among them.
func TestEveryTermIsOneSentence(t *testing.T) {
	for _, term := range []string{"session", "criterion", "hold-out", "check-in", "spend limit", "successor", "replan", "proof", "plan round", "merge approval",
		"promise", "planner", "heal pass", "model review", "factoryd", "signoz"} {
		if Terms[term] == "" {
			t.Errorf("Terms has no sentence for %q", term)
		}
	}
	for term, s := range Terms {
		if n := utf8.RuneCountInString(s); n > 140 {
			t.Errorf("%q: %d characters, want at most 140", term, n)
		}
		if !strings.HasSuffix(s, ".") || len(sentenceBreaks(s)) > 0 || strings.Contains(s, "\n") {
			t.Errorf("%q: %q is not one sentence", term, s)
		}
		if lines := writingguide.Lint(s); len(lines) > 0 {
			t.Errorf("%q: %v", term, lines)
		}
		if term != strings.ToLower(term) {
			t.Errorf("%q: a term is written in lower case, as it is matched", term)
		}
	}
}
