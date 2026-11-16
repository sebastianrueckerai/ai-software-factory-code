package factoryd

import (
	"strings"
	"testing"
)

// CardTitle is the objective's first paragraph, the title as the issue
// was filed, whole: never the body's first sentence run into it, never
// cut, and never the plan's key. Hand-derived: the objective a filing
// stores is the issue title, a blank line, then the body (IssueObjective).
func TestCardTitle(t *testing.T) {
	long := strings.TrimSuffix(strings.Repeat("abcdefghij ", 10), " ")
	for _, tc := range []struct {
		name string
		t    Task
		want string
	}{
		{"a period-less title over a long body first sentence",
			Task{Objective: IssueObjective("Merge approval only on a green staged head",
				"A merge approval request reaches the merge gate after its checks pass, and the approver should see a green staged head before approving it.")},
			"Merge approval only on a green staged head"},
		{"a title with an internal period shows in full",
			Task{Objective: IssueObjective("Fix the cache. It drops entries.", "Then update the tests.")},
			"Fix the cache. It drops entries."},
		{"a title of 109 runes shows whole", Task{Objective: IssueObjective(long, "Then the tests.")}, long},
		{"a plan's card shows its title, not its key",
			Task{PlanKey: "web-pages", Objective: IssueObjective("Build the web pages", "")}, "Build the web pages"},
		{"an objective with an empty body shows its title",
			Task{Objective: IssueObjective("Speed up the board", "")}, "Speed up the board"},
	} {
		if got := CardTitle(tc.t); got != tc.want {
			t.Errorf("%s: CardTitle = %q, want %q", tc.name, got, tc.want)
		}
	}
}
