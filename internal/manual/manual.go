// Package manual is the factory's one source for its own description:
// the loop sentence, the card states and the gates with their meanings,
// and the sandbox images.
// The project agent's manual, the worker sessions' manual tool and the
// message a card's first human message carries all read it here, so none
// of them can drift from the others.
package manual

import (
	"fmt"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Loop is what the factory does, in one sentence; callers add their own
// tail after it.
const Loop = "Signals and members file cards; a card runs agent sessions in a sandbox; its pull request passes the gates (checks, policy, review, audit, approval), merges and deploys"

// Images is the manual's section on sandbox images.
var Images = "A sandbox image is the sandbox base with the tools a project's cards need. " +
	fmt.Sprintf("A project declares at most %d in factory/images.yaml, and the factory builds, signs and pins each one. ", policy.MaxImages) +
	"A card names one in image, or runs the default: the project's image default where it is pinned, else the factory image. " +
	"A code session asks for a missing tool with request_tool, the project agent with request_image. " +
	"Either files a tool card that adds the tool to an image; a second ask for that image joins the open tool card while no session has started it. " +
	"A tool card's pull request merges by the project's merge rule and autonomy, as any card's does. " +
	"A card in waiting-image waits, at no cost, until the factory pins its image, after its tool card merges where it asked for a tool. " +
	"Then it restarts on the image; if no image comes, its next session is told why. After 24 hours of waiting it asks a human."

// Text is the manual a worker session reads: the loop, the card states,
// the gates, the sandbox images, and how it reaches a human.
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
	b.WriteString("\nSandbox images:\n" + Images + "\n")
	b.WriteString("\nA writer of your project can message you through the card's chat. Ask for a decision with escalate_to_human; ask the issue's author with request_info. A code session asks to cut a card that is too big with request_cut.\n")
	return b.String()
}
