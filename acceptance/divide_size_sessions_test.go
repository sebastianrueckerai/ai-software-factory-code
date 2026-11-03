package acceptance

import (
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// TestDivideConquerSizeBounds proves panel 1 twice: once over the
// scripted Figures port, once over a real PostgreSQL store wired as the
// console's port and seeded through the gate reading's own fact payload.
// A card that crossed on two heads counts once, a retried head keeps one
// row, a reading at exactly the bound does not cross, and another gate's,
// another repository's and out-of-window readings stay out. Readings
// without a bound, without a size key or of an unsized card measure
// nothing, and the page words each hole apart from a measured zero.
func TestDivideConquerSizeBounds(t *testing.T) {
	const name = "cards over size bound"
	rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
	holds(t, rig.page(), []string{"Divide and conquer (roadmap 336)"}, nil)
	holds(t, panel(t, rig.page(), name), []string{
		">1 · target 0<",
		"3 readings · 2 measured · 1 unknown size",
		"TestDivideConquerSizeBounds in acceptance",
	}, []string{">— · target 0<", ">0 · target 0<"})
	if last := rig.figures.(*accFigures).asked[len(rig.figures.(*accFigures).asked)-1]; last != "sf/code 2026-09-28T00:00:00Z 2026-10-11T00:00:00Z" {
		t.Fatalf("asked = %q, want the fixture window's dates", last)
	}

	s, _ := divideStore(t)
	card := func(id string, issue int64) { accCard(t, s, id, "sf/code", "small", "", issue) }
	// t-esc crosses on two heads: 301 lines (281 + 20) over the bound 300,
	// then 350; the first head's retry answers id 0
	card("t-esc", 51)
	if id := accReading(t, s, "t-esc", "sf/code", "h1", "small", 300, 281, 20, 51, accDay(1)); id == 0 {
		t.Fatal("the crossed head recorded no row")
	}
	if id := accReading(t, s, "t-esc", "sf/code", "h1", "small", 300, 281, 20, 51, accDay(1)); id != 0 {
		t.Fatalf("the retried head recorded %d, want 0: one head, one row", id)
	}
	accReading(t, s, "t-esc", "sf/code", "h2", "small", 300, 300, 50, 51, accDay(2))
	// t-eq reads exactly 300: the bound is a bound, not a target
	card("t-eq", 52)
	accReading(t, s, "t-eq", "sf/code", "h3", "small", 300, 150, 150, 52, accDay(3))
	card("t-ok", 53)
	accReading(t, s, "t-ok", "sf/code", "h4", "medium", 600, 100, 20, 53, accDay(8))
	// three readings that measure nothing: no bound, an unsized card, no
	// size key at all
	card("t-nobound", 54)
	accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "t-nobound", IssueNumber: 54, Kind: factoryd.EventGateHead,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, IdentityKey: "gate_head t-nobound h5",
		Detail: map[string]any{"gate": factoryd.GateMerge, "head": "h5", "size": "small", "insertions": 400, "deletions": 50}}, accDay(4))
	card("t-unsized", 55)
	accReading(t, s, "t-unsized", "sf/code", "h6", "", 300, 400, 50, 55, accDay(5))
	card("t-nosize", 56)
	accRecord(t, s, factoryd.Event{Repository: "sf/code", TaskID: "t-nosize", IssueNumber: 56, Kind: factoryd.EventGateHead,
		Actor: factoryd.FactoryDecider, Via: factoryd.ViaFactoryd, IdentityKey: "gate_head t-nosize h7",
		Detail: map[string]any{"gate": factoryd.GateMerge, "head": "h7", "bound": 300, "insertions": 400, "deletions": 50}}, accDay(6))
	// outside the window: the day before it crossed (the previous span's),
	// and one at its exclusive end
	card("t-old", 57)
	accReading(t, s, "t-old", "sf/code", "h8", "small", 300, 400, 50, 57, accDay(-1))
	card("t-late", 58)
	accReading(t, s, "t-late", "sf/code", "h9", "small", 300, 400, 50, 58, accDay(14))
	// another gate's reading of the crossed card, and another repository's
	accRecord(t, s, factoryd.GateHeadFact{Task: "t-esc", Head: "h10", Gate: factoryd.GateReview, Decision: "auto",
		Size: "small", Bound: 300, PolicyVersion: 7, Files: 9, Insertions: 400, Deletions: 50}.Event("sf/code", 51), accDay(3))
	accCard(t, s, "t-afar", "sf/other", "small", "", 59)
	accReading(t, s, "t-afar", "sf/other", "o1", "small", 300, 400, 50, 59, accDay(4))

	rig = newDivideRig(t, s)
	dc := rig.report()
	same(t, "gate_above_bound", dc.GateAboveBound, accFig(1, 1, [7]float64{0, 0, 0, 0, 1, 1, 0}))
	same(t, "gate_bound_coverage", dc.GateBoundCoverage, factoryd.GateBoundCoverage{
		Readings: 7, Measured: 4, MissingBound: 1, UnknownSize: 1, UnsizedSize: 1})
	holds(t, panel(t, rig.page(), name), []string{
		">1 · target 0<",
		"7 readings · 4 measured · 1 no bound · 1 unknown size · 1 unsized",
		"TestDivideConquerSizeBounds in acceptance",
	}, []string{">— · target 0<", ">0 · target 0<"})
	// the last week holds only t-ok's reading: a measured zero, not a dash
	holds(t, panel(t, rig.pageQuery(accSpan(accDay(7), accDay(13))), name), []string{">0 · target 0<", "1 readings · 1 measured"}, []string{">— · target 0<"})
	// a span no reading covers is unknown
	holds(t, panel(t, rig.pageQuery(accSpan(accDay(100), accDay(106))), name), []string{">— · target 0<", "0 readings · 0 measured"}, []string{">0 · target 0<"})
}

// TestDivideConquerSessions proves panels 2 and 3 twice: the scripted
// port first, then a real store seeded through the sessions' own fact
// payloads. The small cards hold one and three distinct starts — the one
// began before the window — a retried start keeps one row, the medium
// card holds two, and a card no reading covers sits the averages out.
// The ends count the window's terminal results: a reattached end is one
// end, a never-started attempt's none, and an end at the window's
// exclusive end falls outside it. The real exported session producer
// runs over the seeded store at the last: its reattached replay adds
// neither a session nor a terminal end.
func TestDivideConquerSessions(t *testing.T) {
	rig := newDivideRig(t, &accFigures{report: accPanelsReport()})
	holds(t, rig.page(), []string{
		"small 2.0 · medium 2.0",
		"pushed 2 · wall_clock 1",
		"5 cards · 1 unknown size · 1 missing starts",
	}, nil)

	s, _ := divideStore(t)
	merged := func(id, size string, issue int64, at time.Time) {
		accCard(t, s, id, "sf/code", size, "h-"+id, issue)
		accReading(t, s, id, "sf/code", "h-"+id, size, 600, 90, 10, issue, at.Add(-time.Hour))
		accMerged(t, s, id, "sf/code", at)
	}
	start := func(session, card string, attempt int, issue int64, at time.Time) {
		accStarted(t, s, session, card, "sf/code", attempt, issue, at)
	}
	end := func(session, outcome, card string, attempt int, issue int64, at time.Time) int64 {
		return accEnded(t, s, session, outcome, card, "sf/code", attempt, issue, at)
	}
	// s1: small, one start, a day before the window opened
	merged("s1", "small", 61, accDay(3))
	start("sa", "s1", 1, 61, accDay(-1))
	if id := end("sa", string(factoryd.OutcomePushed), "s1", 1, 61, accDay(2)); id == 0 {
		t.Fatal("the end recorded no row")
	}
	// the reattached run ends the same session again: one session, one end
	if id := end("sa", string(factoryd.OutcomePushed), "s1", 1, 61, accDay(2).Add(3*time.Hour)); id != 0 {
		t.Fatalf("the reattached end recorded %d, want 0", id)
	}
	// an abandoned head's reading plans another size than the merged head's
	accReading(t, s, "s1", "sf/code", "h-s1-old", "medium", 600, 90, 10, 61, accDay(1))
	// s2: small, three starts, sc's retried; sd ends exactly at the window's end
	merged("s2", "small", 62, accDay(8))
	start("sb", "s2", 1, 62, accDay(3))
	start("sc", "s2", 2, 62, accDay(4))
	if id := accStarted(t, s, "sc", "s2", "sf/code", 2, 62, accDay(4).Add(12*time.Hour)); id != 0 {
		t.Fatalf("the reattached start recorded %d, want 0: one session, one row", id)
	}
	start("sd", "s2", 3, 62, accDay(5))
	end("sb", string(factoryd.OutcomeWallClock), "s2", 1, 62, accDay(5))
	end("sc", string(factoryd.OutcomePushed), "s2", 2, 62, accDay(6))
	end("sd", string(factoryd.OutcomePushed), "s2", 3, 62, accDay(14))
	// a never-started attempt ends inside the window: no session's end
	end("sx", string(factoryd.OutcomeNotStarted), "s2", 4, 62, accDay(7))
	// m1: medium, two starts
	merged("m1", "medium", 63, accDay(9))
	start("se", "m1", 1, 63, accDay(7))
	start("sf", "m1", 2, 63, accDay(8))
	// s3: merged on a head no reading covers: its size is unknown
	accCard(t, s, "s3", "sf/code", "small", "h-s3", 65)
	accMerged(t, s, "s3", "sf/code", accDay(10))
	start("sg", "s3", 1, 65, accDay(9))
	// s0: the window before's card, four starts, one end before the window
	merged("s0", "small", 64, accDay(-8))
	for i, id := range []string{"sp1", "sp2", "sp3", "sp4"} {
		start(id, "s0", i+1, 64, accDay(-12+i))
	}
	end("sp1", string(factoryd.OutcomePushed), "s0", 1, 64, accDay(-1))

	rig = newDivideRig(t, s)
	dc := rig.report()
	same(t, "sessions_per_card", dc.SessionsPerCard, []factoryd.SizeFigure{
		{Size: "medium", Figure: factoryd.Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 2}}},
		{Size: "small", Figure: accFig(2, 4, [7]float64{0, 0, 0, 4, 0, 1, 3})},
	})
	same(t, "sessions_coverage", dc.SessionsCoverage, factoryd.SessionsCoverage{Cards: 4, UnknownSize: 1})
	same(t, "session_ends", dc.SessionEnds, []factoryd.NamedCount{{Name: "pushed", Count: 2}, {Name: "wall_clock", Count: 1}})
	holds(t, rig.page(), []string{
		"medium 2.0 · small 2.0",
		"pushed 2 · wall_clock 1",
		"4 cards · 1 unknown size",
		"TestDivideConquerSessions in acceptance",
	}, []string{"pushed 3", "wall_clock 2", "not_started"})

	// the real exported session producer over the seeded store, the one
	// whose reattachment the workflow replays: it records no start the
	// seeded session's identity does not already hold, so neither the
	// session count nor a terminal end moves
	runner := newLedgerRunner(t, &accLedger{PGStore: s}, "", "")
	in := factoryd.SessionAnswerInput{TaskID: "s2", SessionID: "sc", Model: "factory/small", Attempt: 2, Since: accDay(4)}
	if ok, err := runner.SessionAnswered(t.Context(), in); err != nil || !ok {
		t.Fatalf("the reattached SessionAnswered = (%v, %v), want (true, nil)", ok, err)
	}
	dc = rig.report()
	same(t, "sessions_per_card after the reattachment", dc.SessionsPerCard, []factoryd.SizeFigure{
		{Size: "medium", Figure: factoryd.Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 2}}},
		{Size: "small", Figure: accFig(2, 4, [7]float64{0, 0, 0, 4, 0, 1, 3})},
	})
	same(t, "session_ends after the reattachment", dc.SessionEnds,
		[]factoryd.NamedCount{{Name: "pushed", Count: 2}, {Name: "wall_clock", Count: 1}})
}
