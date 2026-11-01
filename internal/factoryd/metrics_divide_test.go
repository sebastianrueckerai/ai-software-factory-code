package factoryd

import (
	"context"
	"strings"
	"testing"
	"time"
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
	dcCard(t, s, "t-other", "sf/other1", "small", "", 54)
	dcGateHead(t, s, "t-other", "sf/other1", "o1", GateMerge, 300, 400, 50, from.AddDate(0, 0, 5))

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

// TestDivideConquerSessionsSQL — panels 2 and 3: mean lifetime
// actually-started sessions per merged card by size — small 2.0, medium
// 2.0 — and the window's session ends by cause — pushed 2, wall_clock
// 1. Duplicate session_started rows, never-started sessions and one
// session's reattached end leave every number standing. [session_efficiency]
func TestDivideConquerSessionsSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	dcCard(t, s, "s1", "sf/dc2", "small", "", 61) // merged Oct 1, sessions sa and sb
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

	dcCard(t, s, "s2", "sf/dc2", "small", "", 62) // merged Oct 6, sessions sc and sd
	dcMerged(t, s, "s2", "sf/dc2", from.AddDate(0, 0, 8))
	dcStarted(t, s, "sc", "s2", "sf/dc2", 1, from.AddDate(0, 0, 8))
	dcStarted(t, s, "sc", "s2", "sf/dc2", 1, from.AddDate(0, 0, 8).Add(time.Hour)) // the reattachment's row
	dcStarted(t, s, "sd", "s2", "sf/dc2", 2, from.AddDate(0, 0, 9))
	dcSessionEnd(t, s, "sc", string(OutcomePushed), "s2", "sf/dc2", 1, to.AddDate(0, 0, 1)) // exactly To: outside

	dcCard(t, s, "s3", "sf/dc2", "medium", "", 63) // merged Oct 2, sessions se and sf
	dcMerged(t, s, "s3", "sf/dc2", from.AddDate(0, 0, 4))
	dcStarted(t, s, "se", "s3", "sf/dc2", 1, from.AddDate(0, 0, 4))
	dcStarted(t, s, "sf", "s3", "sf/dc2", 2, from.AddDate(0, 0, 5))
	dcSessionEnd(t, s, "se", string(OutcomePushed), "s3", "sf/dc2", 1, from.AddDate(0, 0, 4).Add(5*time.Hour))
	// a session the worker never started ends before the window: it is
	// no started session, and its end is no end of the window
	dcSessionEnd(t, s, "sn", string(OutcomeNotStarted), "s3", "sf/dc2", 3, from.AddDate(0, 0, -8))

	dcCard(t, s, "s4", "sf/dc2", "", "", 64) // unsized, merged Oct 4, no session ever started
	dcMerged(t, s, "s4", "sf/dc2", from.AddDate(0, 0, 6))

	dcCard(t, s, "s5", "sf/dc2", "large", "", 65) // merged in the window's last instant, lifetime sessions from before the spark
	dcMerged(t, s, "s5", "sf/dc2", to)
	for _, sess := range []string{"sl1", "sl2", "sl3"} {
		dcStarted(t, s, sess, "s5", "sf/dc2", 1, from.AddDate(0, 0, -58))
	}

	dcCard(t, s, "s6", "sf/dc2", "small", "", 66) // merged in the previous window
	dcMerged(t, s, "s6", "sf/dc2", from.AddDate(0, 0, -8))
	dcStarted(t, s, "sp", "s6", "sf/dc2", 1, from.AddDate(0, 0, -12))

	dcCard(t, s, "s7", "sf/dc2", "small", "", 67) // merged exactly at To: no cohort anywhere
	dcMerged(t, s, "s7", "sf/dc2", to.AddDate(0, 0, 1))
	dcStarted(t, s, "sq", "s7", "sf/dc2", 1, to.AddDate(0, 0, 1))

	dcCard(t, s, "s8", "sf/dc2", "small", SourceRoll, 68) // the factory's own roll card: no panel's cohort
	dcMerged(t, s, "s8", "sf/dc2", from.AddDate(0, 0, 7))
	dcStarted(t, s, "sr1", "s8", "sf/dc2", 1, from.AddDate(0, 0, 7))
	dcStarted(t, s, "sr2", "s8", "sf/dc2", 2, from.AddDate(0, 0, 7))

	dcCard(t, s, "o1", "sf/other2", "small", "", 69) // another repository's card
	dcMerged(t, s, "o1", "sf/other2", from.AddDate(0, 0, 7).Add(time.Hour))
	dcStarted(t, s, "so1", "o1", "sf/other2", 1, from.AddDate(0, 0, 7).Add(2*time.Hour))
	dcSessionEnd(t, s, "so1", string(OutcomePushed), "o1", "sf/other2", 1, from.AddDate(0, 0, 7).Add(3*time.Hour))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc2", from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := []SizeFigure{
		{Size: "", Figure: Figure{Has: true}}, // the unsized card, no started session
		{Size: "large", Figure: Figure{Value: 3, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 3}}},
		{Size: "medium", Figure: Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 2, 0}}},
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

	// the reason a fact row carries is bounded where it was written
	var bounded int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT length(detail->>'reason') FROM events WHERE kind = $1 AND repository = $2 AND detail->>'commit' = 'c1'`,
		EventModelReview, "sf/dc3").Scan(&bounded); err != nil {
		t.Fatal(err)
	}
	if bounded != maxFactReason {
		t.Fatalf("the stored reason is %d long, want %d: bounded where the payload wrote it", bounded, maxFactReason)
	}

	r, err := s.ProjectMetrics(context.Background(), "sf/dc3", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// two of the window's three answers never judged; the previous
	// window's one answer reviewed, a proven zero; the early unjudged
	// answer fills bucket 1 and the window's two bucket 5
	if r.DivideConquer.ReviewsNotReviewed != (Figure{Has: true, PrevHas: true, Weeks: [7]float64{0, 1, 0, 0, 0, 2, 0}}) {
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
	// another repository's rows the same instants
	dcCard(t, s, "o1", "sf/other4", "small", "", 82)
	dcGateHead(t, s, "o1", "sf/other4", "g1", GateMerge, 300, 999, 0, from.AddDate(0, 0, 8))
	dcSessionEnd(t, s, "oe1", string(OutcomePushed), "o1", "sf/other4", 1, from.AddDate(0, 0, 9))
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
	if d.CoveredFrom != from.AddDate(0, 0, -8) {
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
