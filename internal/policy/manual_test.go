package policy

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSpendRail is the end-to-end pass over the spend-rail table: it builds
// from the real schema and the full catalogue, 36 rows in order,
// byte-stable across two runs, the join refuses a key set and a catalogue
// that disagree each way, naming the key, and a matrix anchor the schema
// lacks, and the rewrite replaces the marked section.
func TestSpendRail(t *testing.T) {
	first, err := SpendRailTable()
	if err != nil {
		t.Fatalf("SpendRailTable: %v", err)
	}
	second, err := SpendRailTable()
	if err != nil || first != second {
		t.Fatalf("the table is not byte-stable across two runs: %v", err)
	}
	want := []string{
		"limits.sessions", "limits.agent_time", "limits.check_in", "limits.supervisor_calls",
		"limits.supervisor_timeout", "limits.stronger_after", "limits.steps", "limits.nudge_after",
		"limits.nudges", "limits.same_call", "limits.review_diff_kib", "limits.checkin_calls",
		"limits.checkin_time", "limits.checkin_share", "limits.checkin_tail_kib", "limits.checkin_handover_chars",
		"limits.checkin_sessions", "limits.warn_at", "limits.plan_warn_at", "limits.no_push_spend",
		"budget.task_default_eur", "budget.task_max_eur", "budget.day_eur", "budget.month_eur",
		"budget.session_floor_cheap_eur", "budget.session_floor_frontier_eur", "budget.premium_month_eur",
		"escalation.pool.sessions", "escalation.pool.agent_time", "escalation.pool.eur",
		"escalation.card_draw.sessions", "escalation.card_draw.agent_time", "escalation.card_draw.eur",
		"chat.monthly_eur", "classes.<c>.tools", "envelope_default",
	}
	var got []string
	for _, line := range strings.Split(first, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| key ") || strings.HasPrefix(line, "| ---") {
			continue
		}
		got = append(got, strings.TrimSpace(strings.SplitN(line[2:], " |", 2)[0]))
	}
	if len(got) != len(want) || !slices.Equal(got, want) {
		t.Fatalf("the table holds %d rows, want %d in order; got:\n%v", len(got), len(want), got)
	}

	s, err := kindPolicy.schema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	keys, anchors, err := spendRailWalk(s)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, err := joinSpendRails(append(slices.Clone(keys), "limits.extra"), anchors); err == nil || !strings.Contains(err.Error(), "limits.extra") {
		t.Fatalf("a key the catalogue lacks joins, want it refused and named: %v", err)
	}
	dropped := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == "limits.sessions" })
	if _, err := joinSpendRails(dropped, anchors); err == nil || !strings.Contains(err.Error(), "limits.sessions") {
		t.Fatalf("a key the schema lacks joins, want it refused and named: %v", err)
	}
	if _, err := joinSpendRails(keys, nil); err == nil ||
		!strings.Contains(err.Error(), "classes.<c>.tools") || !strings.Contains(err.Error(), "envelope_default") {
		t.Fatalf("matrix anchors the schema lacks join, want them refused and named: %v", err)
	}

	doc := []byte("lead\n" + SpendRailBegin + "\nstale\n" + SpendRailEnd + "\ntail\n")
	rewritten, err := RewriteSpendRails(doc)
	if err != nil {
		t.Fatalf("RewriteSpendRails: %v", err)
	}
	if want := []byte("lead\n" + first + "tail\n"); !slices.Equal(rewritten, want) {
		t.Fatal("the rewrite did not replace the marked section with the table")
	}
	if _, err := RewriteSpendRails([]byte("no markers here")); err == nil {
		t.Fatal("a document without markers rewrites, want the refusal")
	}
}

// TestSpendRailNoDrift fails when the marked section of the manual page
// differs from the table this build generates, so a cap that lands in the
// schema or the catalogue without the table following cannot pass.
func TestSpendRailNoDrift(t *testing.T) {
	doc, err := os.ReadFile("../../docs/manual/policy.md")
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := RewriteSpendRails(doc)
	if err != nil {
		t.Fatalf("RewriteSpendRails: %v", err)
	}
	if !bytes.Equal(rewritten, doc) {
		t.Fatal("docs/manual/policy.md's spend-rail section differs from the generated table; run: go run ./cmd/factoryd manual --out docs/manual/policy.md")
	}
}
