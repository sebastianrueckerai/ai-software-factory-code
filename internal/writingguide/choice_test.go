package writingguide

import (
	"strings"
	"testing"
)

// An answer a writer offers a human is a button the human presses: short,
// and never in the writer's own voice.
func TestChoiceFaults(t *testing.T) {
	for choice, want := range map[string]bool{
		"Send the new pins to the agent":                 true,
		"Open the upstream hosts to this sandbox":        true,
		"Move them to the US region":                     true,
		"Provide the new pins and I apply them":          false,
		"Let me retry with a stronger model":             false,
		"We wait for the release":                        false,
		"Use our mirror":                                 false,
		"":                                               false,
		strings.Repeat("Send the pins to the agent ", 3): false,
	} {
		if got := len(ChoiceFaults(choice)) == 0; got != want {
			t.Errorf("ChoiceFaults(%q) = %v, want it to pass: %v", choice, ChoiceFaults(choice), want)
		}
	}
}
