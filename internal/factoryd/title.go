package factoryd

import (
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
)

// CardTitle is the face's title of t: the summary a writer set, the plan
// key of a planned card, else the objective's first paragraph.
func CardTitle(t Task) string {
	if t.HumanSummary != "" {
		return t.HumanSummary
	}
	if t.PlanKey != "" {
		return t.PlanKey
	}
	return TitleClause(t.Objective)
}

// TitleCap bounds a card face's title in runes; TitleEllipsis ends a title
// cut to it.
const (
	TitleCap      = 80
	TitleEllipsis = "…"
)

// TitleClause is a card face's title from the objective, on one line: the
// objective's first paragraph, the title as the issue was filed, in full —
// a blank line ends the paragraph, a full stop never does. Only a title
// longer than TitleCap runes is shortened, at a word boundary with an
// ellipsis, never mid-word. The objective is sanitised as a brief carries
// it, so a hidden comment in it shows on no face.
func TitleClause(objective string) string {
	var words []string
	for _, line := range strings.Split(sanitize.Inbound(objective), "\n") {
		if strings.TrimSpace(line) == "" {
			if len(words) > 0 {
				break
			}
			continue
		}
		words = append(words, strings.Fields(line)...)
	}
	s := strings.Join(words, " ")
	if r := []rune(s); len(r) > TitleCap {
		cut := r[:TitleCap-len([]rune(TitleEllipsis))]
		end := len(cut)
		for end > 0 && cut[end-1] != ' ' {
			end--
		}
		if end == 0 {
			end = len(cut) // one word longer than the cap: cut it
		}
		s = strings.TrimRight(string(cut[:end]), " ,;:") + TitleEllipsis
	}
	return s
}
