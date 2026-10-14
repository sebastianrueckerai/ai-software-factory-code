package manual

import (
	"strings"
	"testing"
)

// Text opens with the loop and carries every card state and gate with its
// meaning, so a worker session reads the factory from one place.
func TestTextCarriesTheFactory(t *testing.T) {
	text := Text()
	if !strings.HasPrefix(text, Loop+".\n") {
		t.Fatalf("Text opens %q, want the loop sentence", text[:60])
	}
	for _, want := range []string{
		"backlog: waits in the backlog",
		"running: runs an agent session",
		"done: is done",
		"intake: the issue becomes a card",
		"session: an agent works in a sandbox",
		"checks: the pull request's CI",
		"review: a model reviews the diff",
		"approval: a human approves the merge",
		"human: a human decides",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Text lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "escalate_to_human") || !strings.Contains(text, "request_info") {
		t.Fatalf("Text lacks the ask-for-a-human tools:\n%s", text)
	}
	if !strings.Contains(text, "message you through the card's chat") {
		t.Fatalf("Text lacks who writes to the session:\n%s", text)
	}
}
