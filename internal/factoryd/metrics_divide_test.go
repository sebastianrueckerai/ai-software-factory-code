package factoryd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// The divide-and-conquer panels' SQL (roadmap 336): the store answers
// the block from the fact rows the recorders write. Every test seeds
// real tables through the production payloads — the facts' Event
// builders and the store's Record — and reads ProjectMetrics.

// dcCard inserts a card of repository with size, source s ("" is the
// common api), on its own issue.
func dcCard(t *testing.T, s *PGStore, id, repository, size, source string, issue int64) {
	t.Helper()
	task := Task{ID: id, Status: "in-review", Objective: "the divide panels' fixture", Repository: repository,
		Kind: "agent", Source: source, Track: "direct", BudgetEUR: 5, IssueNumber: issue}
	if source == "" {
		task.Source = "api"
	}
	if size != "" {
		task.Size = size
	}
	if err := s.insertTask(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
}

// dcRecord records e at at and answers its id: 0 for the row an
// identity already holds.
func dcRecord(t *testing.T, s *PGStore, e Event, at time.Time) int64 {
	t.Helper()
	e.At = at
	id, err := s.Record(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// dcGateHead records one merge-gate reading of card's head at at,
// through the fact payload the recorder writes.
func dcGateHead(t *testing.T, s *PGStore, card, repository, head, gate string, bound, insertions, deletions int, at time.Time) int64 {
	t.Helper()
	return dcRecord(t, s, GateHeadFact{Task: card, Head: head, Gate: gate, Decision: "auto",
		Size: "small", Bound: bound, Insertions: insertions, Deletions: deletions}.Event(repository, 9), at)
}

// dcSessionEnd records a session's terminal end at at.
func dcSessionEnd(t *testing.T, s *PGStore, session, outcome, task, repository string, attempt int, at time.Time) int64 {
	t.Helper()
	return dcRecord(t, s, SessionEndFact{Session: session, Attempt: attempt, Outcome: outcome,
		Started: at.Add(-time.Hour)}.Event(task, repository, 9, at), at)
}

// dcStarted records a session_started row, as SessionAnswered does when
// a session got its model answer: retries and reattachments leave more
// rows of the same session.
func dcStarted(t *testing.T, s *PGStore, session, task, repository string, attempt int, at time.Time) {
	t.Helper()
	dcRecord(t, s, Event{Repository: repository, TaskID: task, Kind: EventSessionStarted,
		Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"session": session, "model": "big", "attempt": attempt}}, at)
}

// dcMerged records the card's pull_request_merged row at at.
func dcMerged(t *testing.T, s *PGStore, task, repository string, at time.Time) {
	t.Helper()
	dcRecord(t, s, Event{Repository: repository, TaskID: task, Kind: EventPullRequestMerged,
		Actor: MergeUser, Via: ViaFactoryd, Detail: map[string]any{"pr": 9, "commit": "m"}}, at)
}

// TestDivideConquerSizeBoundsSQL — panel 1: distinct cards that reached
// the merge gate above their size bound. Bound 300 with 301 lines
// crosses, with 300 exactly it does not; a retried head records once, a
// card crossing on many heads counts once, the window's bounds hold,
// other repositories and other gates stay out, and a span no reading
// covers is unknown, not zero. [size_bound_escape]
func TestDivideConquerSizeBoundsSQL(t *testing.T) {
	s := metricsStore(t)
	// the package's tests share one database: clear the tables the
	// panels read, as the metrics suite's own seeds do, so earlier
	// files' cards cannot collide with these fixtures' ids
	for _, table := range []string{"events", "alerts", "ledger", "audit", "project_rolls", "ci_runs", "rolls", "chat_messages", "tasks"} {
		if _, err := s.db.ExecContext(context.Background(), `TRUNCATE `+table+` CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	from, to := metricsTestWindow()
	dcCard(t, s, "t-escape", "sf/dc1", "small", "", 51)
	// the crossed head, 301 lines against the small bound 300, retried
	// once: the identity keeps one row
	if id := dcGateHead(t, s, "t-escape", "sf/dc1", "h1", GateMerge, 300, 281, 20, from.AddDate(0, 0, 1)); id == 0 {
		t.Fatal("the first reading recorded no row")
	}
	if id := dcGateHead(t, s, "t-escape", "sf/dc1", "h1", GateMerge, 300, 281, 20, from.AddDate(0, 0, 1)); id != 0 {
		t.Fatalf("the retried head recorded %d, want 0: one head, one row", id)
	}
	// 300 lines exactly: the bound is a bound, not a target
	dcGateHead(t, s, "t-escape", "sf/dc1", "h2", GateMerge, 300, 150, 150, from.AddDate(0, 0, 2))
	// a second crossing head: the card already counts, once
	dcGateHead(t, s, "t-escape", "sf/dc1", "h3", GateMerge, 300, 400, 50, to.AddDate(0, 0, -1))
	// the window's bounds: before it, at its end
	dcGateHead(t, s, "t-escape", "sf/dc1", "h0", GateMerge, 300, 400, 50, from.AddDate(0, 0, -1)) // the previous window
	dcGateHead(t, s, "t-escape", "sf/dc1", "h4", GateMerge, 300, 400, 50, to.AddDate(0, 0, 1))    // outside: To itself
	dcCard(t, s, "t-within", "sf/dc1", "small", "", 52)
	dcGateHead(t, s, "t-within", "sf/dc1", "w1", GateMerge, 300, 100, 100, from.AddDate(0, 0, 3))
	// a reading at another gate, and one of another repository, cross
	// for no panel here
	dcCard(t, s, "t-review", "sf/dc1", "small", "", 53)
	dcGateHead(t, s, "t-review", "sf/dc1", "r1", GateReview, 300, 400, 50, from.AddDate(0, 0, 4))
	// t-afar, not t-other again: the package's tests share one
	// database, and console_store_pg_test's t-other already sits in it
	dcCard(t, s, "t-afar", "sf/other1", "small", "", 54)
	dcGateHead(t, s, "t-afar", "sf/other1", "o1", GateMerge, 300, 400, 50, from.AddDate(0, 0, 5))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// one card crossed in the window whatever its heads, one in the
	// previous; the crossings of h0 (Sep 27), h1 (Sep 29) and h3 (Oct
	// 10) fill buckets 4, 5 and 6
	if r.DivideConquer.GateAboveBound != (Figure{Value: 1, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 1, 1, 1}}) {
		t.Fatalf("gate above bound = %+v", r.DivideConquer.GateAboveBound)
	}
	// a repository no reading covers reads unknown, never zero
	empty, err := s.ProjectMetrics(context.Background(), "sf/dc-none1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if empty.DivideConquer.GateAboveBound != (Figure{}) {
		t.Fatalf("gate above bound without evidence = %+v, want unknown", empty.DivideConquer.GateAboveBound)
	}
}

// dcHeadCard inserts a card of repository with size and the head it
// actually built on, on its own issue: the head the merge gate read and
// the panels' historical size hangs on.
func dcHeadCard(t *testing.T, s *PGStore, id, repository, size, head string, issue int64) {
	t.Helper()
	task := Task{ID: id, Status: "in-review", Objective: "the divide panels' fixture", Repository: repository,
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: issue, SHA: head}
	if size != "" {
		task.Size = size
	}
	if err := s.insertTask(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
}

// dcReading records the merge-gate reading of card's head at at, the
// size the gates judged — the historical size the sessions panel keeps,
// whatever the task row says later.
func dcReading(t *testing.T, s *PGStore, card, repository, head, size string, at time.Time) int64 {
	t.Helper()
	return dcRecord(t, s, GateHeadFact{Task: card, Head: head, Gate: GateMerge, Decision: "auto",
		Size: size, Bound: 300, Insertions: 90, Deletions: 10}.Event(repository, 9), at)
}

// TestDivideConquerSessionsSQL — panels 2 and 3: mean lifetime distinct
// named starts per merged card by the size its merged head's reading
// planned — small 2.0 over the cards with two, two, one and three
// starts, medium 2.0 over two and two — and the window's session ends
// by cause — pushed 2, wall_clock 1. Starts from before the window and
// the spark count, a reattachment's repeated row does not; a detached
// session's reattached end leaves one end, an ongoing session gains
// none, and a never-started attempt — outside the window or inside it —
// inflates no terminal distribution. [session_efficiency]
func TestDivideConquerSessionsSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	dcHeadCard(t, s, "s1", "sf/dc2", "small", "h-s1", 61) // merged Oct 1, sessions sa and sb
	dcReading(t, s, "s1", "sf/dc2", "h-s1", "small", from.AddDate(0, 0, 3))
	dcMerged(t, s, "s1", "sf/dc2", from.AddDate(0, 0, 3))
	dcMerged(t, s, "s1", "sf/dc2", from.AddDate(0, 0, 10)) // a later landing of the same card: the first merge is the cohort's
	dcStarted(t, s, "sa", "s1", "sf/dc2", 1, from.AddDate(0, 0, 1))
	dcStarted(t, s, "sa", "s1", "sf/dc2", 1, from.AddDate(0, 0, 1).Add(12*time.Hour)) // the retry's row
	dcStarted(t, s, "sb", "s1", "sf/dc2", 2, from.AddDate(0, 0, 2))
	if id := dcSessionEnd(t, s, "sa", string(OutcomePushed), "s1", "sf/dc2", 1, from.AddDate(0, 0, 3).Add(2*time.Hour)); id == 0 {
		t.Fatal("the end recorded no row")
	}
	if id := dcSessionEnd(t, s, "sa", string(OutcomePushed), "s1", "sf/dc2", 1, from.AddDate(0, 0, 3).Add(3*time.Hour)); id != 0 {
		t.Fatalf("the reattached end recorded %d, want 0: one session, one end", id)
	}
	dcSessionEnd(t, s, "sb", string(OutcomeWallClock), "s1", "sf/dc2", 2, from.AddDate(0, 0, 4))

	dcHeadCard(t, s, "s2", "sf/dc2", "small", "h-s2", 62) // merged Oct 6, sessions sc and sd
	dcReading(t, s, "s2", "sf/dc2", "h-s2", "small", from.AddDate(0, 0, 8))
	dcMerged(t, s, "s2", "sf/dc2", from.AddDate(0, 0, 8))
	dcStarted(t, s, "sc", "s2", "sf/dc2", 1, from.AddDate(0, 0, 8))
	dcStarted(t, s, "sc", "s2", "sf/dc2", 1, from.AddDate(0, 0, 8).Add(time.Hour)) // the reattachment's row
	dcStarted(t, s, "sd", "s2", "sf/dc2", 2, from.AddDate(0, 0, 9))
	// the detached session's end lands exactly at To: outside, and the
	// session is ongoing through the window
	dcSessionEnd(t, s, "sc", string(OutcomePushed), "s2", "sf/dc2", 1, to.AddDate(0, 0, 1))

	dcHeadCard(t, s, "s3", "sf/dc2", "medium", "h-s3", 63) // merged Oct 2, sessions se and sf
	dcReading(t, s, "s3", "sf/dc2", "h-s3", "medium", from.AddDate(0, 0, 4))
	dcMerged(t, s, "s3", "sf/dc2", from.AddDate(0, 0, 4))
	dcStarted(t, s, "se", "s3", "sf/dc2", 1, from.AddDate(0, 0, 4))
	dcStarted(t, s, "sf", "s3", "sf/dc2", 2, from.AddDate(0, 0, 5))
	dcSessionEnd(t, s, "se", string(OutcomePushed), "s3", "sf/dc2", 1, from.AddDate(0, 0, 4).Add(5*time.Hour))
	// a session the worker never started ends before the window: it is
	// no started session, and its end is no end of the window
	dcSessionEnd(t, s, "sn", string(OutcomeNotStarted), "s3", "sf/dc2", 3, from.AddDate(0, 0, -8))

	dcHeadCard(t, s, "s4", "sf/dc2", "", "h-s4", 64) // unsized, merged Oct 4, no session ever started
	dcReading(t, s, "s4", "sf/dc2", "h-s4", "", from.AddDate(0, 0, 6))
	dcMerged(t, s, "s4", "sf/dc2", from.AddDate(0, 0, 6))
	// a never-started attempt ending inside the window: a pod that never
	// became a session inflates no terminal distribution
	dcSessionEnd(t, s, "sx", string(OutcomeNotStarted), "s4", "sf/dc2", 1, from.AddDate(0, 0, 7))

	dcHeadCard(t, s, "s5", "sf/dc2", "large", "h-s5", 65) // merged in the window's last instant, lifetime sessions from before the spark
	dcReading(t, s, "s5", "sf/dc2", "h-s5", "large", to)
	dcMerged(t, s, "s5", "sf/dc2", to)
	for _, sess := range []string{"sl1", "sl2", "sl3"} {
		dcStarted(t, s, sess, "s5", "sf/dc2", 1, from.AddDate(0, 0, -58))
	}

	dcHeadCard(t, s, "s9", "sf/dc2", "small", "h-s9", 66) // merged Oct 7, one lifetime start
	dcReading(t, s, "s9", "sf/dc2", "h-s9", "small", from.AddDate(0, 0, 9))
	dcMerged(t, s, "s9", "sf/dc2", from.AddDate(0, 0, 9))
	dcStarted(t, s, "si", "s9", "sf/dc2", 1, from.AddDate(0, 0, 9))

	dcHeadCard(t, s, "s10", "sf/dc2", "small", "h-s10", 67) // merged Oct 8, three distinct lifetime starts
	dcReading(t, s, "s10", "sf/dc2", "h-s10", "small", from.AddDate(0, 0, 10))
	dcMerged(t, s, "s10", "sf/dc2", from.AddDate(0, 0, 10))
	for i, sess := range []string{"sj1", "sj2", "sj3"} {
		dcStarted(t, s, sess, "s10", "sf/dc2", i+1, from.AddDate(0, 0, 10).Add(time.Duration(i)*time.Hour))
	}

	dcHeadCard(t, s, "s11", "sf/dc2", "medium", "h-s11", 68) // merged Oct 9, two distinct lifetime starts
	dcReading(t, s, "s11", "sf/dc2", "h-s11", "medium", from.AddDate(0, 0, 11))
	dcMerged(t, s, "s11", "sf/dc2", from.AddDate(0, 0, 11))
	dcStarted(t, s, "sk1", "s11", "sf/dc2", 1, from.AddDate(0, 0, 11))
	dcStarted(t, s, "sk2", "s11", "sf/dc2", 2, from.AddDate(0, 0, 11).Add(time.Hour))

	dcHeadCard(t, s, "s6", "sf/dc2", "small", "h-s6", 69) // merged in the previous window
	dcReading(t, s, "s6", "sf/dc2", "h-s6", "small", from.AddDate(0, 0, -8))
	dcMerged(t, s, "s6", "sf/dc2", from.AddDate(0, 0, -8))
	dcStarted(t, s, "sp", "s6", "sf/dc2", 1, from.AddDate(0, 0, -12))

	dcHeadCard(t, s, "s7", "sf/dc2", "small", "h-s7", 70) // merged exactly at To: no cohort anywhere
	dcMerged(t, s, "s7", "sf/dc2", to.AddDate(0, 0, 1))
	dcStarted(t, s, "sq", "s7", "sf/dc2", 1, to.AddDate(0, 0, 1))
	// a session still running when the window closes: its start is a
	// fact, and it gains no terminal end
	dcStarted(t, s, "s-ongo", "s7", "sf/dc2", 2, from.AddDate(0, 0, 12))

	dcCard(t, s, "s8", "sf/dc2", "small", SourceRoll, 71) // the factory's own roll card: no panel's cohort
	dcMerged(t, s, "s8", "sf/dc2", from.AddDate(0, 0, 7))
	dcStarted(t, s, "sr1", "s8", "sf/dc2", 1, from.AddDate(0, 0, 7))
	dcStarted(t, s, "sr2", "s8", "sf/dc2", 2, from.AddDate(0, 0, 7))

	dcCard(t, s, "o1", "sf/other2", "small", "", 72) // another repository's card
	dcMerged(t, s, "o1", "sf/other2", from.AddDate(0, 0, 7).Add(time.Hour))
	dcStarted(t, s, "so1", "o1", "sf/other2", 1, from.AddDate(0, 0, 7).Add(2*time.Hour))
	dcSessionEnd(t, s, "so1", string(OutcomePushed), "o1", "sf/other2", 1, from.AddDate(0, 0, 7).Add(3*time.Hour))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc2", from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []SizeFigure{
		{Size: "", Figure: Figure{Has: true}}, // the unsized card's measured zero
		{Size: "large", Figure: Figure{Value: 3, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 3}}},
		{Size: "medium", Figure: Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 2, 2}}},
		{Size: "small", Figure: Figure{Value: 2, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 2, 2}}},
	}
	got := r.DivideConquer.SessionsPerCard
	if len(got) != len(want) {
		t.Fatalf("sessions per card = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sessions per card[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// the window's eight first-merged cards, one unsized, no hole: the
	// starts the recorder holds cover them all
	if cov := r.DivideConquer.SessionsCoverage; cov != (SessionsCoverage{Cards: 8, Unsized: 1}) {
		t.Fatalf("sessions coverage = %+v, want the eight cards and the unsized one alone", cov)
	}
	// the window's ends by cause: sa and se pushed, sb against the clock
	ends := r.DivideConquer.SessionEnds
	if len(ends) != 2 || ends[0] != (NamedCount{Name: string(OutcomePushed), Count: 2}) || ends[1] != (NamedCount{Name: string(OutcomeWallClock), Count: 1}) {
		t.Fatalf("session ends = %+v, want pushed 2 and wall_clock 1", ends)
	}
}

// TestDivideConquerReviewCoverageSQL — panel 9: reviews that answered
// not reviewed (target: none). Two of the window's three answers never
// judged the head, their reasons bounded where the payload wrote them;
// a span every answer of which reviewed is a proven zero, a span no
// answer covers stays unknown, and the window's bounds and the
// repository's hold. [not_reviewed_answers]
// TestDivideConquerSessionEvidenceSQL — panel 2's evidence: the size a
// card counts by is the reading its merged head's snapshot planned, so
// the task row's later size moves no merged card, an abandoned head's
// reading never stands in for the merged head's, and a card no reading
// covers is unknown, never a guessed bucket. A card merged before the
// repository's first named start holds missing start evidence —
// silence the recorder owns — and one whose ends name sessions no start
// row names holds partial evidence; both sit the averages out and are
// counted in the coverage, while a merged card after recording began
// with no start rows is a measured zero in the average. The end a
// partial card holds stays the fact it is. [session_evidence]
func TestDivideConquerSessionEvidenceSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	// e1's head was read small and merged; the task row moved to medium
	// afterwards — the snapshot the merge gate planned is the size the
	// panel keeps
	dcHeadCard(t, s, "e1", "sf/dc5", "small", "h-e1", 91)
	dcReading(t, s, "e1", "sf/dc5", "h-e1", "small", from.AddDate(0, 0, 1))
	dcMerged(t, s, "e1", "sf/dc5", from.AddDate(0, 0, 2))
	dcStarted(t, s, "ea1", "e1", "sf/dc5", 1, from.AddDate(0, 0, 2))
	dcStarted(t, s, "ea2", "e1", "sf/dc5", 2, from.AddDate(0, 0, 2))
	dcSessionEnd(t, s, "ea1", string(OutcomePushed), "e1", "sf/dc5", 1, from.AddDate(0, 0, 2).Add(3*time.Hour))
	if err := s.SetTaskSizeBudget(context.Background(), "e1", "medium", 500, 5); err != nil {
		t.Fatal(err)
	}

	// e2's row says small and its head's reading medium: the reading
	// decides, against the row the other way
	dcHeadCard(t, s, "e2", "sf/dc5", "small", "h-e2", 92)
	dcReading(t, s, "e2", "sf/dc5", "h-e2", "medium", from.AddDate(0, 0, 3))
	dcMerged(t, s, "e2", "sf/dc5", from.AddDate(0, 0, 3))
	dcStarted(t, s, "eb1", "e2", "sf/dc5", 1, from.AddDate(0, 0, 3))
	dcStarted(t, s, "eb2", "e2", "sf/dc5", 2, from.AddDate(0, 0, 3))

	// e3's reading sits on an abandoned head; the head it merged on was
	// never read: no historical size, not the abandoned head's
	dcHeadCard(t, s, "e3", "sf/dc5", "small", "h-e3", 93)
	dcReading(t, s, "e3", "sf/dc5", "h-e3x", "small", from.AddDate(0, 0, 4))
	dcMerged(t, s, "e3", "sf/dc5", from.AddDate(0, 0, 4))
	dcStarted(t, s, "ec1", "e3", "sf/dc5", 1, from.AddDate(0, 0, 4))
	dcStarted(t, s, "ec2", "e3", "sf/dc5", 2, from.AddDate(0, 0, 4))

	// e4's head was read unsized: its measured figure sits in the
	// unsized bucket, covered
	dcHeadCard(t, s, "e4", "sf/dc5", "", "h-e4", 94)
	dcReading(t, s, "e4", "sf/dc5", "h-e4", "", from.AddDate(0, 0, 5))
	dcMerged(t, s, "e4", "sf/dc5", from.AddDate(0, 0, 5))
	dcStarted(t, s, "ed1", "e4", "sf/dc5", 1, from.AddDate(0, 0, 5))

	// e5's end names a session no start row names: partial evidence, the
	// large card sits the average out, and its end stays an end
	dcHeadCard(t, s, "e5", "sf/dc5", "large", "h-e5", 95)
	dcReading(t, s, "e5", "sf/dc5", "h-e5", "large", from.AddDate(0, 0, 6))
	dcMerged(t, s, "e5", "sf/dc5", from.AddDate(0, 0, 6))
	dcStarted(t, s, "ee1", "e5", "sf/dc5", 1, from.AddDate(0, 0, 6))
	dcSessionEnd(t, s, "ee2", string(OutcomePushed), "e5", "sf/dc5", 2, from.AddDate(0, 0, 6).Add(2*time.Hour))

	// e6 merged Sep 29, before the repository's first named start (e1's,
	// Sep 30): its silence is the recorder's, missing evidence — not a
	// zero, not in the average
	dcHeadCard(t, s, "e6", "sf/dc5", "small", "h-e6", 96)
	dcReading(t, s, "e6", "sf/dc5", "h-e6", "small", from.AddDate(0, 0, 1))
	dcMerged(t, s, "e6", "sf/dc5", from.AddDate(0, 0, 1))

	// e7 merged after recording began and started no session ever: a
	// measured zero inside the small average
	dcHeadCard(t, s, "e7", "sf/dc5", "small", "h-e7", 97)
	dcReading(t, s, "e7", "sf/dc5", "h-e7", "small", from.AddDate(0, 0, 7))
	dcMerged(t, s, "e7", "sf/dc5", from.AddDate(0, 0, 7))

	// e8 merged in the previous window; its session row is dated inside
	// the current one — the panel counts lifetime starts wherever they
	// fall
	dcHeadCard(t, s, "e8", "sf/dc5", "medium", "h-e8", 98)
	dcReading(t, s, "e8", "sf/dc5", "h-e8", "medium", from.AddDate(0, 0, -8))
	dcMerged(t, s, "e8", "sf/dc5", from.AddDate(0, 0, -8))
	dcStarted(t, s, "eh1", "e8", "sf/dc5", 1, from.AddDate(0, 0, 4))

	// another repository's card, however complete, is no figure here
	dcHeadCard(t, s, "e9", "sf/other9", "small", "h-e9", 99)
	dcReading(t, s, "e9", "sf/other9", "h-e9", "small", from.AddDate(0, 0, 5))
	dcMerged(t, s, "e9", "sf/other9", from.AddDate(0, 0, 5))
	dcStarted(t, s, "ei1", "e9", "sf/other9", 1, from.AddDate(0, 0, 5))
	dcStarted(t, s, "ei2", "e9", "sf/other9", 2, from.AddDate(0, 0, 5))
	dcSessionEnd(t, s, "ei1", string(OutcomePushed), "e9", "sf/other9", 1, from.AddDate(0, 0, 5).Add(time.Hour))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc5", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// small holds e1 (2 starts) and e7 (a measured zero): 1.0, e6's
	// missing evidence aside; medium holds e2's 2 with e8's 1 before the
	// window; the unsized bucket holds e4; no large figure — e5's
	// partial evidence sits it out
	want := []SizeFigure{
		{Size: "", Figure: Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}},
		{Size: "medium", Figure: Figure{Value: 2, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 2, 0}}},
		{Size: "small", Figure: Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 2, 0}}},
	}
	got := r.DivideConquer.SessionsPerCard
	if len(got) != len(want) {
		t.Fatalf("sessions per card = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sessions per card[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// the seven window cards, one of each hole the evidence can hold
	if cov := r.DivideConquer.SessionsCoverage; cov != (SessionsCoverage{Cards: 7, UnknownSize: 1, Unsized: 1,
		MissingStarts: 1, PartialStarts: 1}) {
		t.Fatalf("sessions coverage = %+v, want the seven cards with one unknown, unsized, missing and partial", cov)
	}
	// the partial card's end stays a fact of the window beside e1's
	ends := r.DivideConquer.SessionEnds
	if len(ends) != 1 || ends[0] != (NamedCount{Name: string(OutcomePushed), Count: 2}) {
		t.Fatalf("session ends = %+v, want the two pushed ends, evidence aside", ends)
	}
	// the other repository's figures answer alone
	other, err := s.ProjectMetrics(context.Background(), "sf/other9", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if o := other.DivideConquer.SessionsPerCard; len(o) != 1 || o[0] != (SizeFigure{Size: "small",
		Figure: Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 2, 0}}}) {
		t.Fatalf("other repository sessions per card = %+v, want its small card's 2 alone", o)
	}
	if other.DivideConquer.SessionsCoverage != (SessionsCoverage{Cards: 1}) {
		t.Fatalf("other repository sessions coverage = %+v, want its one covered card", other.DivideConquer.SessionsCoverage)
	}
}

func TestDivideConquerReviewCoverageSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	dcCard(t, s, "t-r1", "sf/dc3", "small", "", 71)
	// two answers that never judged: at From itself, and within
	nr1 := ReviewAttemptFact{PR: 9, Commit: "c1", Round: 1, Model: "big", Note: strings.Repeat("n", 600)}
	if id := dcRecord(t, s, nr1.Event("t-r1", "sf/dc3", 71), from); id == 0 {
		t.Fatal("the first answer recorded no row")
	}
	nr2 := ReviewAttemptFact{PR: 9, Commit: "c2", Round: 1, Model: "big", Note: "no verdict"}
	if id := dcRecord(t, s, nr2.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 3)); id == 0 {
		t.Fatal("the second answer recorded no row")
	}
	// the round a retry re-answers keeps one row
	if id := dcRecord(t, s, nr2.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 3).Add(time.Hour)); id != 0 {
		t.Fatalf("the re-answered round recorded %d, want 0: one round, one row", id)
	}
	ok := ReviewAttemptFact{PR: 9, Commit: "c3", Round: 1, Model: "big", Verdict: "approve"}
	dcRecord(t, s, ok.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 4))
	// exactly To: outside
	late := ReviewAttemptFact{PR: 9, Commit: "c4", Round: 1, Model: "big", Note: "outside"}
	dcRecord(t, s, late.Event("t-r1", "sf/dc3", 71), to.AddDate(0, 0, 1))
	// the previous window: a reviewed answer makes its zero proven, an
	// earlier unjudged one fills the spark's first weeks
	dcCard(t, s, "t-r2", "sf/dc3", "small", "", 72)
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "c5", Round: 1, Model: "big", Verdict: "concerns"}.Event("t-r2", "sf/dc3", 72), from.AddDate(0, 0, -8))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "c6", Round: 1, Model: "big", Note: "early"}.Event("t-r2", "sf/dc3", 72), from.AddDate(0, 0, -33))
	// another repository's answers stay out
	dcCard(t, s, "t-o", "sf/other3", "small", "", 73)
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "c7", Round: 1, Model: "big", Note: "elsewhere"}.Event("t-o", "sf/other3", 73), from.AddDate(0, 0, 5))

	// the reason a fact row carries is bounded where it was written:
	// the payload's own cut of the 600-note, ellipsis marked — compared
	// as text, so no database encoding's length() is trusted
	var reason string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT detail->>'reason' FROM events WHERE kind = $1 AND repository = $2 AND detail->>'commit' = 'c1'`,
		EventModelReview, "sf/dc3").Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if want := textutil.Truncate(strings.Repeat("n", 600), maxFactReason); reason != want {
		t.Fatalf("the stored reason is %q, want the payload's bounded %q", reason, want)
	}

	r, err := s.ProjectMetrics(context.Background(), "sf/dc3", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// two of the window's three answers never judged; the previous
	// window's one answer reviewed, a proven zero; the early unjudged
	// answer fills bucket 0 (Aug 26, the spark opens Aug 24) and the
	// window's two bucket 5
	if r.DivideConquer.ReviewsNotReviewed != (Figure{Value: 2, Has: true, PrevHas: true, Weeks: [7]float64{1, 0, 0, 0, 0, 2, 0}}) {
		t.Fatalf("reviews not reviewed = %+v", r.DivideConquer.ReviewsNotReviewed)
	}
	// a repository no answer covers reads unknown, never zero
	empty, err := s.ProjectMetrics(context.Background(), "sf/dc-none3", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if empty.DivideConquer.ReviewsNotReviewed != (Figure{}) {
		t.Fatalf("reviews not reviewed without evidence = %+v, want unknown", empty.DivideConquer.ReviewsNotReviewed)
	}
}

// TestDivideConquerFlowWindowsSQL — the block's spans and neighbours:
// every panel reads the window, the equal-length window before it and
// the seven weekly buckets, holds the repository to its own rows,
// answers an empty repository with the whole block unknown, and leaves
// the deck's existing figures standing. [window_and_existing_figures]
func TestDivideConquerFlowWindowsSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	dcCard(t, s, "c1", "sf/dc4", "small", "", 81)
	dcGateHead(t, s, "c1", "sf/dc4", "g1", GateMerge, 300, 350, 0, from.AddDate(0, 0, 8))  // window, bucket 6
	dcGateHead(t, s, "c1", "sf/dc4", "g0", GateMerge, 300, 350, 0, from.AddDate(0, 0, -8)) // previous window, bucket 3
	dcSessionEnd(t, s, "se1", string(OutcomeWallClock), "c1", "sf/dc4", 1, from.AddDate(0, 0, 9))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "k1", Round: 1, Model: "big", Verdict: "approve"}.Event("c1", "sf/dc4", 81), from.AddDate(0, 0, 9))
	// another repository's rows the same instants — ow1, not o1 again:
	// the package's tests share one database, one task id once
	dcCard(t, s, "ow1", "sf/other4", "small", "", 82)
	dcGateHead(t, s, "ow1", "sf/other4", "g1", GateMerge, 300, 999, 0, from.AddDate(0, 0, 8))
	dcSessionEnd(t, s, "oe1", string(OutcomePushed), "ow1", "sf/other4", 1, from.AddDate(0, 0, 9))
	// a done card of the window: the deck's throughput still counts it
	done := Task{ID: "c-done", Status: "done", Objective: "the deck's figure", Repository: "sf/dc4", Kind: "agent",
		Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: 83, Size: "small",
		CreatedAt: from.AddDate(0, 0, 8)}
	if err := s.insertTask(context.Background(), done, ""); err != nil {
		t.Fatal(err)
	}

	r, err := s.ProjectMetrics(context.Background(), "sf/dc4", from, to)
	if err != nil {
		t.Fatal(err)
	}
	d := r.DivideConquer
	// Equal, not ==: the store's timestamp comes back in the driver's
	// location, an equal instant beside time.UTC's pointer
	if !d.CoveredFrom.Equal(from.AddDate(0, 0, -8)) {
		t.Fatalf("covered from = %v, want the earliest fact row %v", d.CoveredFrom, from.AddDate(0, 0, -8))
	}
	if d.GateAboveBound != (Figure{Value: 1, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 1}}) {
		t.Fatalf("gate above bound = %+v", d.GateAboveBound)
	}
	if len(d.SessionEnds) != 1 || d.SessionEnds[0] != (NamedCount{Name: string(OutcomeWallClock), Count: 1}) {
		t.Fatalf("session ends = %+v, want the window's wall_clock alone", d.SessionEnds)
	}
	if d.ReviewsNotReviewed != (Figure{Has: true, PrevHas: false}) {
		t.Fatalf("reviews not reviewed = %+v, want a measured zero of one reviewed answer", d.ReviewsNotReviewed)
	}
	// the deck's own figures stand: the window's done card, the dates asked
	if r.Repository != "sf/dc4" || !r.From.Equal(from) || !r.To.Equal(to) {
		t.Fatalf("report = %s %v %v", r.Repository, r.From, r.To)
	}
	if r.Throughput != (Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}) {
		t.Fatalf("throughput = %+v, want the done card of bucket 6", r.Throughput)
	}
	// the empty repository: the whole block unknown, never zero
	empty, err := s.ProjectMetrics(context.Background(), "sf/dc-none4", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if e := empty.DivideConquer; !e.CoveredFrom.IsZero() || len(e.SessionsPerCard) != 0 || len(e.SessionEnds) != 0 ||
		e.GateAboveBound != (Figure{}) || e.ReviewsNotReviewed != (Figure{}) {
		t.Fatalf("divide conquer without evidence = %+v, want the zero block", e)
	}
}
