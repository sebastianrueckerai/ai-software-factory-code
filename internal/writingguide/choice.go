package writingguide

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxChoice bounds an answer an agent offers a human, in runes: a button's
// label.
const MaxChoice = 60

// ownVoice finds the first person: I, me, my, we, us, our.
var ownVoice = regexp.MustCompile(`\b(?:I|[Mm]e|[Mm]y|[Mm]yself|[Ww]e|us|[Oo]urs?)\b`)

// ChoiceFaults are the faults of choice, an answer an agent offers a human
// as a button: none when it is 1 to MaxChoice runes and it names what
// happens with no one in the first person.
func ChoiceFaults(choice string) []string {
	switch n := len([]rune(strings.TrimSpace(choice))); {
	case n == 0:
		return []string{"The answer is empty."}
	case n > MaxChoice:
		return []string{fmt.Sprintf("The answer has %d characters, at most %d.", n, MaxChoice)}
	}
	if w := ownVoice.FindString(choice); w != "" {
		return []string{fmt.Sprintf("The answer says %q. Name what happens, for example \"Send the new pins to the agent\".", w)}
	}
	return nil
}
