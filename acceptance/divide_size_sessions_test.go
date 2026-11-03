package acceptance

import (
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestDivideConquerSizeBounds proves panel 1 twice: once over the
// scripted Figures port, once over a real PostgreSQL store wired as the
// console's port and seeded through the gate reading's own fact payload
// — the crossed card counts once whatever its heads, the retried head's
// identity keeps it one row, a reading at exactly the bound does not
// cross, another gate's and another repository's readings stay out, and
// the panel keeps the target of none.
func TestDivideConquerSizeBounds(t *testing.T) {
	report := accPanelsReport()
	rig := newDivideRig(t, &accFigures{report: report})
	holds(t, rig.page(), []string{
		"Divide and conquer (roadmap 336)",
		">1 · target 0<",
		"3 readings · 2 measured · 1 unknown size",
		"TestDivideConquerSizeBounds in acceptance",
	}, []string{">— · target 0<", ">0 · target 0<"})
	if last := rig.figures.(*accFigures).asked[len(rig.figures.(*accFigures).asked)-1]; last != "sf/code 2026-09-28T00:00:00Z 2026-10-11T00:00:00Z" {
		t.Fatalf("asked = %q, want the fixture window's dates", last)
	}

	s, _ := divideStore(t)
	from, _ := divideWindow()
	accCard(t, s, "t-escape", "sf/code", "small", "", 51)
	if id := accReading(t, s, "t-escape", "sf/code", "h1", "small", 300, 281, 20, 51, from.AddDate(0, 0, 1)); id == 0 {
		t.Fatal("the crossed head recorded no row")
	}
	if id := accReading(t, s, "t-escape", "sf/code", "h1", "small", 300, 281, 20, 51, from.AddDate(0, 0, 1)); id != 0 {
		t.Fatalf("the retried head recorded %d, want 0: one head, one row", id)
	}
	// 300 lines exactly: the bound is a bound, not a target
	accReading(t, s, "t-escape", "sf/code", "h2", "small", 300, 150, 150, 51, from.AddDate(0, 0, 2))
	// the window's bounds: before it, and at its own exclusive end
	accReading(t, s, "t-escape", "sf/code", "h0", "small", 300, 400, 50, 51, from.AddDate(0, 0, -1))
	accReading(t, s, "t-escape", "sf/code", "h4", "small", 300, 400, 50, 51, from.AddDate(0, 0, 14))
	// another gate's reading of the same card crosses for no panel here
	accRecord(t, s, factoryd.GateHeadFact{Task: "t-escape", Head: "h5", Gate: factoryd.GateReview, Decision: "auto",
		Size: "small", Bound: 300, PolicyVersion: 7, Files: 9, Insertions: 400, Deletions: 50}.Event("sf/code", 51),
		from.AddDate(0, 0, 3))
	// another repository's crossing head answers its own report alone
	accCard(t, s, "t-afar", "sf/other", "small", "", 52)
	accReading(t, s, "t-afar", "sf/other", "o1", "small", 300, 400, 50, 52, from.AddDate(0, 0, 4))

	rig = newDivideRig(t, s)
	holds(t, rig.page(), []string{
		">1 · target 0<",
		"2 readings · 2 measured",
		"TestDivideConquerSizeBounds in acceptance",
	}, []string{">0 · target 0<", ">2 · target 0<"})
}

// TestDivideConquerSessions proves panels 2 and 3 twice: the scripted
// port first, then a real store seeded through the sessions' own fact
// payloads — two named starts per merged card, small and medium alike,
// the reattached start's identity one row, the reattached end one end,
// a never-started attempt inflates no terminal distribution, and the
// window's ends name their causes.
func TestDivideConquerSessions(t *testing.T) {
	rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
	holds(t, rig.page(), []string{
		"small 2.0 · medium 2.0",
		"pushed 2 · wall_clock 1",
		"5 cards · 1 unknown size · 1 missing starts",
	}, nil)

	s, _ := divideStore(t)
	from, to := divideWindow()
	// s1: small, merged Oct 1, sessions sa and sb; sa's start retried, one row
	accCard(t, s, "s1", "sf/code", "small", "h-s1", 61)
	accReading(t, s, "s1", "sf/code", "h-s1", "small", 300, 90, 10, 61, from.AddDate(0, 0, 3))
	accMerged(t, s, "s1", "sf/code", from.AddDate(0, 0, 3))
	if id := accStarted(t, s, "sa", "s1", "sf/code", 1, 61, from.AddDate(0, 0, 1)); id == 0 {
		t.Fatal("the first start recorded no row")
	}
	if id := accStarted(t, s, "sa", "s1", "sf/code", 1, 61, from.AddDate(0, 0, 1).Add(12*time.Hour)); id != 0 {
		t.Fatalf("the reattached start recorded %d, want 0: one session, one row", id)
	}
	accStarted(t, s, "sb", "s1", "sf/code", 2, 61, from.AddDate(0, 0, 2))
	if id := accEnded(t, s, "sa", string(factoryd.OutcomePushed), "s1", "sf/code", 1, 61, from.AddDate(0, 0, 3).Add(2*time.Hour)); id == 0 {
		t.Fatal("the end recorded no row")
	}
	if id := accEnded(t, s, "sa", string(factoryd.OutcomePushed), "s1", "sf/code", 1, 61, from.AddDate(0, 0, 3).Add(3*time.Hour)); id != 0 {
		t.Fatalf("the reattached end recorded %d, want 0: one session, one end", id)
	}
	accEnded(t, s, "sb", string(factoryd.OutcomeWallClock), "s1", "sf/code", 2, 61, from.AddDate(0, 0, 4))
	// s2: small, merged Oct 6, sessions sc and sd; sc's end lands exactly
	// at To — outside — and a never-started attempt ends inside the
	// window: no terminal distribution it could inflate
	accCard(t, s, "s2", "sf/code", "small", "h-s2", 62)
	accReading(t, s, "s2", "sf/code", "h-s2", "small", 300, 90, 10, 62, from.AddDate(0, 0, 8))
	accMerged(t, s, "s2", "sf/code", from.AddDate(0, 0, 8))
	accStarted(t, s, "sc", "s2", "sf/code", 1, 62, from.AddDate(0, 0, 8))
	accStarted(t, s, "sd", "s2", "sf/code", 2, 62, from.AddDate(0, 0, 9))
	accEnded(t, s, "sc", string(factoryd.OutcomePushed), "s2", "sf/code", 1, 62, to)
	accEnded(t, s, "sx", string(factoryd.OutcomeNotStarted), "s2", "sf/code", 3, 62, from.AddDate(0, 0, 7))
	// m1: medium, merged Oct 2, sessions se and sf
	accCard(t, s, "m1", "sf/code", "medium", "h-m1", 63)
	accReading(t, s, "m1", "sf/code", "h-m1", "medium", 600, 180, 20, 63, from.AddDate(0, 0, 4))
	accMerged(t, s, "m1", "sf/code", from.AddDate(0, 0, 4))
	accStarted(t, s, "se", "m1", "sf/code", 1, 63, from.AddDate(0, 0, 4))
	accStarted(t, s, "sf", "m1", "sf/code", 2, 63, from.AddDate(0, 0, 5))
	// the previous window's own card: no cohort of this window
	accCard(t, s, "s0", "sf/code", "small", "h-s0", 64)
	accReading(t, s, "s0", "sf/code", "h-s0", "small", 300, 90, 10, 64, from.AddDate(0, 0, -8))
	accMerged(t, s, "s0", "sf/code", from.AddDate(0, 0, -8))
	accStarted(t, s, "sp", "s0", "sf/code", 1, 64, from.AddDate(0, 0, -12))

	rig = newDivideRig(t, s)
	holds(t, rig.page(), []string{
		"medium 2.0 · small 2.0",
		"pushed 2 · wall_clock 1",
		"3 cards",
		"TestDivideConquerSessions in acceptance",
	}, []string{"pushed 3", "wall_clock 2", "not_started 1"})
}
