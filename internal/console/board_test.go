package console

import (
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The face's title is the objective's first paragraph — the title as the
// issue was filed, in full — never the body's first sentence run into it;
// only a title longer than 80 runes is shortened, at a word boundary with
// an ellipsis, never mid-word. Hand-derived: the objective a filing stores
// is the issue title, a blank line, then the body (IssueObjective).
func TestTitleClause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		objective string
		want      string
	}{
		{"a period-less title over a long body first sentence",
			factoryd.IssueObjective("Merge approval only on a green staged head",
				"A merge approval request reaches the merge gate after its checks pass, and the approver should see a green staged head before approving it."),
			"Merge approval only on a green staged head"},
		{"a title with an internal period shows in full",
			factoryd.IssueObjective("Fix the cache. It drops entries.", "Then update the tests."),
			"Fix the cache. It drops entries."},
		{"a title over 80 runes cuts at a word boundary with an ellipsis",
			factoryd.IssueObjective(strings.Repeat("abcdefghij ", 10), "Then the tests."),
			strings.TrimSuffix(strings.Repeat("abcdefghij ", 7), " ") + "…"},
		{"an objective with an empty body shows its title",
			factoryd.IssueObjective("Speed up the board", ""),
			"Speed up the board"},
	} {
		if got := factoryd.TitleClause(tc.objective); got != tc.want {
			t.Errorf("%s: factoryd.TitleClause = %q, want %q", tc.name, got, tc.want)
		}
	}
}
