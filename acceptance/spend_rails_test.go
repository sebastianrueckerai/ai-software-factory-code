package acceptance

// The spend-rail acceptance card of plan #1548: the manual page's marked
// section and the generated spend-rail table hold each other at the doc's
// surface. TestAccManualHoldsAllCaps reads ../docs/manual/policy.md the
// way an outsider does and holds its 36 rows against what
// policy.SpendRailTable() generates. TestAccSchemaKeyDriftAllCaps fails on
// copies of the page drift mutated each way - a cell hand-edited, a key
// renamed, a row removed - while the committed page passes. The row-truth
// card builds on the parsing here: rows keyed by their first column.

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// spendRailRow is one table row of the manual's marked section: its key
// (the first column) and the three authored columns.
type spendRailRow struct {
	key   string
	cells [3]string
}

// readManualSpendRails is the outsider read of the manual page: it splits
// the section between policy.SpendRailBegin and policy.SpendRailEnd into
// rows keyed by their first column, in the page's order.
func readManualSpendRails(t *testing.T) ([]spendRailRow, map[string]spendRailRow) {
	t.Helper()
	doc, err := os.ReadFile("../docs/manual/policy.md")
	if err != nil {
		t.Fatal(err)
	}
	return parseSpendRailRows(t, string(doc))
}

// parseSpendRailRows turns the marked section of a page copy into rows in
// order and keyed by their first column.
func parseSpendRailRows(t *testing.T, doc string) ([]spendRailRow, map[string]spendRailRow) {
	t.Helper()
	var rows []spendRailRow
	inSection := false
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case strings.HasPrefix(line, policy.SpendRailBegin):
			inSection = true
		case strings.HasPrefix(line, policy.SpendRailEnd):
			inSection = false
		case inSection && strings.HasPrefix(line, "| ") &&
			!strings.HasPrefix(line, "| key ") && !strings.HasPrefix(line, "| ---"):
			cells := strings.SplitN(strings.TrimSuffix(line[2:], " |"), " | ", 4)
			if len(cells) != 4 {
				t.Fatalf("a table row without four cells: %q", line)
			}
			row := spendRailRow{key: strings.TrimSpace(cells[0])}
			for i := 1; i < 4; i++ {
				row.cells[i-1] = strings.TrimSpace(cells[i])
			}
			rows = append(rows, row)
		}
	}
	byKey := make(map[string]spendRailRow, len(rows))
	for _, row := range rows {
		if _, dup := byKey[row.key]; dup {
			t.Fatalf("two rows carry the key %s", row.key)
		}
		byKey[row.key] = row
	}
	return rows, byKey
}

// wantSpendRailKeys is the generated table's rows in the schema's order.
func wantSpendRailKeys() []string {
	return []string{
		"limits.sessions", "limits.agent_time", "limits.check_in", "limits.supervisor_calls",
		"limits.supervisor_timeout", "limits.stronger_after", "limits.steps", "limits.nudge_after",
		"limits.nudges", "limits.same_call", "limits.review_diff_kib", "limits.checkin_calls",
		"limits.checkin_time", "limits.checkin_share", "limits.checkin_tail_kib", "limits.checkin_handover_chars",
		"limits.checkin_sessions", "limits.warn_at", "limits.plan_warn_at", "limits.no_push_spend",
		"limits.gate_reserve_eur",
		"budget.task_default_eur", "budget.task_max_eur", "budget.day_eur", "budget.month_eur",
		"budget.session_floor_cheap_eur", "budget.session_floor_frontier_eur", "budget.premium_month_eur",
		"escalation.pool.sessions", "escalation.pool.agent_time", "escalation.pool.eur",
		"escalation.card_draw.sessions", "escalation.card_draw.agent_time", "escalation.card_draw.eur",
		"chat.monthly_eur", "classes.<c>.tools", "envelope_default",
	}
}

// TestAccManualHoldsAllCaps reads docs/manual/policy.md as an outsider and
// holds the section between the markers against what policy.SpendRailTable()
// generates: 37 rows in order - 21 limits, 7 budget, 3 escalation.pool, 3
// escalation.card_draw, 1 chat.monthly_eur, then the two matrix rows
// classes.<c>.tools and envelope_default - every row's three authored
// columns non-empty, and no key without a row and no row without a key.
func TestAccManualHoldsAllCaps(t *testing.T) {
	rows, byKey := readManualSpendRails(t)
	wantKeys := wantSpendRailKeys()
	gotKeys := make([]string, len(rows))
	for i, row := range rows {
		gotKeys[i] = row.key
	}
	if len(gotKeys) != len(wantKeys) || !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("the page holds %d rows, want %d in order; got:\n%v", len(gotKeys), len(wantKeys), gotKeys)
	}
	for _, block := range []struct {
		name  string
		count int
	}{
		{"limits", 21}, {"budget", 7},
		{"escalation.pool", 3}, {"escalation.card_draw", 3},
		{"chat.monthly_eur", 1},
	} {
		n := 0
		for _, key := range gotKeys {
			if strings.HasPrefix(key, block.name) || key == block.name {
				n++
			}
		}
		if n != block.count {
			t.Errorf("the %s block holds %d rows, want %d", block.name, n, block.count)
		}
	}
	for _, row := range rows {
		for i, cell := range row.cells {
			if cell == "" {
				t.Errorf("the row %s holds an empty authored column %d", row.key, i+1)
			}
		}
	}
	table, err := policy.SpendRailTable()
	if err != nil {
		t.Fatalf("SpendRailTable: %v", err)
	}
	genRows, genByKey := parseSpendRailRows(t, table)
	for _, key := range wantKeys {
		if _, ok := byKey[key]; !ok {
			t.Errorf("the generated key %s has no row on the page", key)
		}
		if _, ok := genByKey[key]; !ok {
			t.Errorf("the page's key %s has no row in the generated table", key)
		}
	}
	if len(genRows) != len(wantKeys) {
		t.Errorf("the generated table holds %d rows, want %d", len(genRows), len(wantKeys))
	}
}

// TestAccSchemaKeyDriftAllCaps drifts the page at its surface, each way:
// it fails on a copy with one table cell hand-edited, on one with a row's
// key renamed (a row the generated table no longer carries) and on one
// with a row removed (a generated key the page lacks), while the committed
// page passes. policy.SpendRailTable() reads the real schema and already
// refuses a schema and catalogue that disagree.
func TestAccSchemaKeyDriftAllCaps(t *testing.T) {
	doc, err := os.ReadFile("../docs/manual/policy.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(doc)
	if drift(t, page) {
		t.Fatal("the committed page drifts from the generated table")
	}
	for name, mutated := range map[string]string{
		"cell edited": strings.Replace(page, `"policy: limits.sessions must be positive, is %d"`, `"policy: limits.sessions must be positive, got %d"`, 1),
		"key renamed": strings.Replace(page, "| limits.sessions |", "| limits.session_cap |", 1),
		"row removed": removeSpendRailRow(t, page, "chat.monthly_eur"),
	} {
		if !drift(t, mutated) {
			t.Errorf("the page copy with the %s does not drift from the generated table", name)
		}
	}
}

// drift rewrites a page copy with policy.RewriteSpendRails and reports
// whether the generated table differs from the copy's marked section.
func drift(t *testing.T, page string) bool {
	t.Helper()
	rewritten, err := policy.RewriteSpendRails([]byte(page))
	if err != nil {
		t.Fatalf("RewriteSpendRails: %v", err)
	}
	return string(rewritten) != page
}

// removeSpendRailRow takes one row out of a page copy's marked section.
func removeSpendRailRow(t *testing.T, page, key string) string {
	t.Helper()
	var out []string
	inSection := false
	removed := false
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.HasPrefix(line, policy.SpendRailBegin):
			inSection = true
		case strings.HasPrefix(line, policy.SpendRailEnd):
			inSection = false
		case inSection && strings.HasPrefix(line, "| "+key+" "):
			removed = true
			continue
		}
		out = append(out, line)
	}
	if !removed {
		t.Fatalf("the page holds no row %s", key)
	}
	return strings.Join(out, "\n")
}
