package factoryd

import (
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// TitledRef is ref with the card's title, "#1250 - Fix the cache", the
// ref alone when the card has no title to name.
func TitledRef(ref string, t Task) string {
	if title := CardTitle(t); title != "" {
		return ref + " - " + title
	}
	return ref
}

// CardTitle is t's title, the opening paragraph of its objective on one
// line and whole: a blank line ends the paragraph, a full stop never
// does. Its writer keeps it short, so no code cuts it. The objective is
// sanitised as a brief carries it, so a hidden comment in it shows on no
// face.
func CardTitle(t Task) string {
	var words []string
	for _, line := range strings.Split(sanitize.Inbound(t.Objective), "\n") {
		if strings.TrimSpace(line) == "" {
			if len(words) > 0 {
				break
			}
			continue
		}
		words = append(words, strings.Fields(line)...)
	}
	return strings.Join(words, " ")
}
