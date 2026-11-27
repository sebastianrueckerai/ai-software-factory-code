package console

import (
	"strings"
	"testing"
)

// maxChatDescription is the most words a chat tool's about holds.
const maxChatDescription = 20

// Every chat tool says what it does in at most 20 words.
func TestChatDescriptionsAreCapped(t *testing.T) {
	for _, tool := range tools {
		if n := len(strings.Fields(tool.about)); n > maxChatDescription {
			t.Errorf("%s: its about holds %d words, want at most %d", tool.name, n, maxChatDescription)
		}
	}
}
