package console

import (
	"net/url"
	"slices"
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

// The routines control's choice: within reads its period, 1 to 999 days
// or hours, a number or a unit it does not take reading as the default
// 1 day; the period is read with within alone, and week is no choice.
func TestReadColsReadsTheRoutinesPeriod(t *testing.T) {
	for _, tc := range []struct {
		query    string
		routines string
		within   int
		unit     string
	}{
		{"routines=within&within=2&unit=hours", "within", 2, "hours"},
		{"routines=within&within=999&unit=days", "within", 999, "days"},
		{"routines=within", "within", 1, "days"},
		{"routines=within&within=0&unit=hours", "within", 1, "hours"},
		{"routines=within&within=1000&unit=hours", "within", 1, "hours"},
		{"routines=within&within=-3", "within", 1, "days"},
		{"routines=within&within=2.5&unit=minutes", "within", 1, "days"},
		{"routines=within&within=x&unit=weeks", "within", 1, "days"},
		{"routines=within&within=12&unit=Hours", "within", 12, "days"},
		{"routines=none&within=2&unit=hours", "none", 0, ""},
		{"routines=all&within=2&unit=hours", "", 0, ""},
		{"routines=week", "", 0, ""},
		{"within=2&unit=hours", "", 0, ""},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		var f boardFilter
		readCols(&f, q.Get)
		if f.Routines != tc.routines || f.Within != tc.within || f.Unit != tc.unit {
			t.Errorf("%s: read %q %d %q, want %q %d %q", tc.query, f.Routines, f.Within, f.Unit, tc.routines, tc.within, tc.unit)
		}
	}
}

// The query and the forms' hidden fields carry within and unit with
// within alone; the routines control's form leaves out all three. The
// period reads in words with the unit singular for 1.
func TestFilterCarriesTheRoutinesPeriod(t *testing.T) {
	within := boardFilter{Project: "p", Q: "x", Routines: "within", Within: 2, Unit: "hours"}
	none := boardFilter{Project: "p", Routines: "none", Within: 2, Unit: "hours"}
	for _, tc := range []struct {
		f    boardFilter
		want string
	}{
		{within, "?project=p&q=x&routines=within&unit=hours&within=2"},
		{none, "?project=p&routines=none"},
		{boardFilter{Project: "p"}, "?project=p"},
	} {
		if got := tc.f.query(); got != tc.want {
			t.Errorf("%+v: query %q, want %q", tc.f, got, tc.want)
		}
	}
	for _, tc := range []struct {
		f      boardFilter
		except []string
		want   []hidden
	}{
		{within, nil, []hidden{{"project", "p"}, {"q", "x"}, {"routines", "within"}, {"within", "2"}, {"unit", "hours"}}},
		{within, []string{"q"}, []hidden{{"project", "p"}, {"routines", "within"}, {"within", "2"}, {"unit", "hours"}}},
		{within, []string{"routines"}, []hidden{{"project", "p"}, {"q", "x"}}},
		{none, nil, []hidden{{"project", "p"}, {"routines", "none"}}},
		{none, []string{"routines"}, []hidden{{"project", "p"}}},
	} {
		if got := tc.f.Hidden(tc.except...); !slices.Equal(got, tc.want) {
			t.Errorf("%+v without %v: hidden %v, want %v", tc.f, tc.except, got, tc.want)
		}
	}
	for _, tc := range []struct {
		within int
		unit   string
		want   string
	}{{1, "days", "1 day"}, {3, "days", "3 days"}, {1, "hours", "1 hour"}, {12, "hours", "12 hours"}} {
		if got := (boardFilter{Routines: "within", Within: tc.within, Unit: tc.unit}).Period(); got != tc.want {
			t.Errorf("Period of %d %s = %q, want %q", tc.within, tc.unit, got, tc.want)
		}
	}
}
