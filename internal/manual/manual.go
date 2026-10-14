// Package manual is the factory's one source for its own description:
// the loop sentence, the card states and the gates with their meanings.
// The project agent's manual, the worker sessions' manual tool and the
// message a card's first human message carries all read it here, so none
// of them can drift from the others.
package manual

import (
	"fmt"
	"strings"
)

// Loop is what the factory does, in one sentence; callers add their own
// tail after it.
const Loop = "Signals and members file cards; a card runs agent sessions in a sandbox; its pull request passes the gates (checks, policy, review, audit, approval), merges and deploys"

// Text is the manual a worker session reads: the loop, the card states,
// the gates, and how it reaches a human.
func Text() string {
	var b strings.Builder
	b.WriteString(Loop + ".\n\nCard states:\n")
	for _, s := range Statuses {
		fmt.Fprintf(&b, "  %s: %s\n", s, StatusMeaning(s))
	}
	b.WriteString("\nGates:\n")
	for _, g := range Gates {
		fmt.Fprintf(&b, "  %s: %s\n", g, GateMeaning(g))
	}
	b.WriteString("\nA writer of your project can message you through the card's chat. Ask for a decision with escalate_to_human; ask the issue's author with request_info.\n")
	return b.String()
}
