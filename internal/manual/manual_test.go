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
		"queued: waits for a free work slot, in filing order",
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
	if strings.Contains(text, "backlog") || strings.Contains(text, "lane") {
		t.Fatalf("Text names a backlog or a lane, which no card is in:\n%s", text)
	}
	if !strings.Contains(text, "escalate_to_human") || !strings.Contains(text, "request_info") {
		t.Fatalf("Text lacks the ask-for-a-human tools:\n%s", text)
	}
	if !strings.Contains(text, "message you through the card's chat") {
		t.Fatalf("Text lacks who writes to the session:\n%s", text)
	}
}

// Text carries the section on sandbox images: what an image is, the
// default, how a session and the project agent ask for a tool, the tool
// card's merge and waiting-image.
func TestTextCarriesTheSandboxImages(t *testing.T) {
	want := "\nSandbox images:\n" +
		"A sandbox image is the sandbox base with the tools a project's cards need. " +
		"A project declares at most 4 in factory/images.yaml, and the factory builds, signs and pins each one. " +
		"A card names one in image, or runs the default: the project's image default where it is pinned, else the factory image. " +
		"A code session asks for a missing tool with request_tool, the project agent with request_image. " +
		"Either files a tool card that adds the tool to an image; a second ask for that image joins the open tool card while no session has started it. " +
		"A tool card's pull request merges by the project's merge rule and autonomy, as any card's does. " +
		"A card in waiting-image waits, at no cost, until the factory pins its image, after its tool card merges where it asked for a tool. " +
		"Then it restarts on the image; if no image comes, its next session is told why. After 24 hours of waiting it asks a human.\n"
	if text := Text(); !strings.Contains(text, want) {
		t.Fatalf("Text lacks the images section %q:\n%s", want, text)
	}
}
