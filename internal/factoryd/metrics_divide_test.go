package factoryd

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// dcLegacyGateHead records one merge-gate row exactly as detail says,
// as producers around the fact contract wrote them: keyed false leaves
// it identity-less. The row is the reading's whole truth — whatever it
// lacks, the reading lacks.
func dcLegacyGateHead(t *testing.T, s *PGStore, card, repository, head string, keyed bool, detail map[string]any, at time.Time) int64 {
	t.Helper()
	e := Event{Repository: repository, TaskID: card, Kind: EventGateHead, Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}
	if keyed {
		e.IdentityKey = identityKey(EventGateHead, card, head)
	}
	return dcRecord(t, s, e, at)
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

// dcDoneCard inserts a done card of repository with size and sha — the
// head the card actually built on, which the economics reading matches —
// finished at created: the economics cohort, the sessions cohort and the
// deck's throughput all read it.
func dcDoneCard(t *testing.T, s *PGStore, id, repository, size, sha string, issue int64, created time.Time) {
	t.Helper()
	task := Task{ID: id, Status: "done", Objective: "the combined report's fixture", Repository: repository,
		Kind: "agent", Source: "api", Track: "direct", BudgetEUR: 5, IssueNumber: issue,
		CreatedAt: created, SHA: sha}
	if size != "" {
		task.Size = size
	}
	if err := s.insertTask(context.Background(), task, ""); err != nil {
		t.Fatal(err)
	}
}

// dcReadingFull records one merge-gate reading of card's head at at, through
// the fact payload the recorder writes: the size the gates judged, the
// bound that size planned under and the lines they counted.
func dcReadingFull(t *testing.T, s *PGStore, card, repository, head, size string, bound, insertions, deletions int, issue int64, at time.Time) int64 {
	t.Helper()
	return dcRecord(t, s, GateHeadFact{Task: card, Head: head, Gate: GateMerge, Decision: "auto",
		Size: size, Bound: bound, PolicyVersion: 7, Files: 9,
		Insertions: insertions, Deletions: deletions}.Event(repository, issue), at)
}

// dcSpend writes one lifetime ledger row of card's.
func dcSpend(t *testing.T, s *PGStore, card, day string, eur float64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO ledger (task_id, day, kind, ref, eur) VALUES ($1, $2, 'model', 'big', $3)`, card, day, eur); err != nil {
		t.Fatal(err)
	}
}

// TestDivideConquerSizeBoundsSQL — panel 1: distinct cards that reached
// the merge gate above their size bound. Bound 300 with 301 lines
// crosses, with 300 exactly it does not; a retried head records once, a
// card crossing on many heads counts once, the window's bounds hold,
// other repositories and other gates stay out, and a span no reading
// covers is unknown, not zero. The readings are history: the policy in
// force now and the card's size now change nothing.
// [size_bound_escape]
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
	// the window holds four readings, every one a complete fact: no
	// hole to name
	if cov := r.DivideConquer.GateBoundCoverage; cov != (GateBoundCoverage{Readings: 4, Measured: 4}) {
		t.Fatalf("gate bound coverage = %+v, want four measured readings", cov)
	}
	// the readings are history: a new policy in force (large_lines 50,
	// small 15) and the card re-sized change nothing — the bound stays
	// the one the gates read
	if _, _, err := policy.Record(context.Background(), s.db, "dc-size-bounds", policy.Tree{Document: policy.Document{Risk: policy.Risk{LargeLines: 50}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), `UPDATE tasks SET size = 'large' WHERE id = 't-escape'`); err != nil {
		t.Fatal(err)
	}
	again, err := s.ProjectMetrics(context.Background(), "sf/dc1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if again.DivideConquer.GateAboveBound != r.DivideConquer.GateAboveBound {
		t.Fatalf("gate above bound after the change = %+v, want the historical %+v",
			again.DivideConquer.GateAboveBound, r.DivideConquer.GateAboveBound)
	}
	if again.DivideConquer.GateBoundCoverage != r.DivideConquer.GateBoundCoverage {
		t.Fatalf("gate bound coverage after the change = %+v, want the historical %+v",
			again.DivideConquer.GateBoundCoverage, r.DivideConquer.GateBoundCoverage)
	}
	// a repository no reading covers reads unknown, never zero
	empty, err := s.ProjectMetrics(context.Background(), "sf/dc-none1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if empty.DivideConquer.GateAboveBound != (Figure{}) {
		t.Fatalf("gate above bound without evidence = %+v, want unknown", empty.DivideConquer.GateAboveBound)
	}
	if empty.DivideConquer.GateBoundCoverage != (GateBoundCoverage{}) {
		t.Fatalf("gate bound coverage without evidence = %+v, want none", empty.DivideConquer.GateBoundCoverage)
	}
}

// TestDivideConquerSizeEvidenceSQL — panel 1's evidence: a reading
// measures its card only complete, the fact's identity, a bound, both
// line counts and the size whose bound it planned. A missing bound,
// missing line statistics, a size the row never carried, an explicitly
// unsized card and an identity-less legacy row each name their hole in
// the coverage, never borrow the policy in force now or the card's
// metadata, and never pass as measured — a span only holes cover is
// unknown, not a zero, a span nothing covers is unknown with no
// readings to name, and a complete reading at exactly its bound still
// proves the zero. [size_bound_escape]
func TestDivideConquerSizeEvidenceSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	// the policy in force now, its bounds small enough that any fill
	// from it would read every hole over: nothing is filled from it
	if _, _, err := policy.Record(context.Background(), s.db, "dc-size-evidence", policy.Tree{Document: policy.Document{Risk: policy.Risk{LargeLines: 50}}}); err != nil {
		t.Fatal(err)
	}
	// every hole on its own card, each card sized small so a fill from
	// the task's metadata now would read over too
	dcCard(t, s, "t-nobound", "sf/dc6", "small", "", 91)
	if id := dcLegacyGateHead(t, s, "t-nobound", "sf/dc6", "h1", true,
		map[string]any{"gate": GateMerge, "head": "h1", "decision": "auto", "size": "small", "insertions": 400, "deletions": 20},
		from.AddDate(0, 0, 1)); id == 0 {
		t.Fatal("the boundless reading recorded no row")
	}
	dcCard(t, s, "t-nolines", "sf/dc6", "small", "", 92)
	dcLegacyGateHead(t, s, "t-nolines", "sf/dc6", "h1", true,
		map[string]any{"gate": GateMerge, "head": "h1", "decision": "auto", "size": "small", "bound": 300},
		from.AddDate(0, 0, 2))
	dcCard(t, s, "t-nosize", "sf/dc6", "small", "", 93)
	dcLegacyGateHead(t, s, "t-nosize", "sf/dc6", "h1", true,
		map[string]any{"gate": GateMerge, "head": "h1", "decision": "auto", "bound": 300, "insertions": 400, "deletions": 20},
		from.AddDate(0, 0, 3))
	dcCard(t, s, "t-unsized", "sf/dc6", "small", "", 94)
	dcLegacyGateHead(t, s, "t-unsized", "sf/dc6", "h1", true,
		map[string]any{"gate": GateMerge, "head": "h1", "decision": "auto", "size": "", "bound": 300, "insertions": 400, "deletions": 20},
		from.AddDate(0, 0, 4))
	// an identity-less legacy row, complete in every field it carries
	dcCard(t, s, "t-legacy", "sf/dc6", "small", "", 95)
	if id := dcLegacyGateHead(t, s, "t-legacy", "sf/dc6", "h1", false,
		map[string]any{"gate": GateMerge, "head": "h1", "decision": "auto", "size": "small", "bound": 300, "insertions": 400, "deletions": 20},
		from.AddDate(0, 0, 5)); id == 0 {
		t.Fatal("the legacy reading recorded no row")
	}
	// the one complete reading: 300 lines at exactly the small bound
	// 300, a measured zero — the bound is a bound, not a target
	dcCard(t, s, "t-zero", "sf/dc6", "small", "", 96)
	if id := dcGateHead(t, s, "t-zero", "sf/dc6", "z1", GateMerge, 300, 150, 150, from.AddDate(0, 0, 6)); id == 0 {
		t.Fatal("the measured reading recorded no row")
	}
	// a repository a hole alone covers: unknown, with the hole named
	dcCard(t, s, "u-nobound", "sf/dc-unknown6", "small", "", 97)
	if id := dcLegacyGateHead(t, s, "u-nobound", "sf/dc-unknown6", "h1", true,
		map[string]any{"gate": GateMerge, "head": "h1", "decision": "auto", "size": "small", "insertions": 400, "deletions": 20},
		from.AddDate(0, 0, 1)); id == 0 {
		t.Fatal("the boundless reading recorded no row")
	}

	r, err := s.ProjectMetrics(context.Background(), "sf/dc6", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// the panel stands on the one measured reading: a measured zero,
	// with the four over-looking holes beside it measuring nothing —
	// none of them escaped, and none of them passed
	if r.DivideConquer.GateAboveBound != (Figure{Has: true}) {
		t.Fatalf("gate above bound = %+v, want a measured zero on the one complete reading", r.DivideConquer.GateAboveBound)
	}
	if cov := r.DivideConquer.GateBoundCoverage; cov != (GateBoundCoverage{Readings: 6, Measured: 1,
		MissingBound: 1, MissingLines: 1, UnknownSize: 1, UnsizedSize: 1, Identityless: 1}) {
		t.Fatalf("gate bound coverage = %+v, want one measured reading and every hole named", cov)
	}
	// a span only holes cover reads unknown, never a measured zero —
	// and stays tellable from a span nothing covers: both figures are
	// unknown, only the coverage counts the readings behind them
	unknown, err := s.ProjectMetrics(context.Background(), "sf/dc-unknown6", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.DivideConquer.GateAboveBound != (Figure{}) {
		t.Fatalf("gate above bound on holes alone = %+v, want unknown", unknown.DivideConquer.GateAboveBound)
	}
	if cov := unknown.DivideConquer.GateBoundCoverage; cov != (GateBoundCoverage{Readings: 1, MissingBound: 1}) {
		t.Fatalf("gate bound coverage on holes alone = %+v, want the one hole named", cov)
	}
	none, err := s.ProjectMetrics(context.Background(), "sf/dc-none6", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if none.DivideConquer.GateAboveBound != (Figure{}) {
		t.Fatalf("gate above bound without evidence = %+v, want unknown", none.DivideConquer.GateAboveBound)
	}
	if none.DivideConquer.GateBoundCoverage != (GateBoundCoverage{}) {
		t.Fatalf("gate bound coverage without evidence = %+v, want none", none.DivideConquer.GateBoundCoverage)
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

// TestDivideConquerReviewCoverageSQL — panel 9: reviews that answered
// not reviewed (target: none). Two of the window's three genuine
// answers never judged the head — the explicit note and the gateway
// refusal — the third a valid negative verdict that did, so 2 of 3;
// their reasons bounded where the payload wrote them. Replaying one
// attempt's persistence adds no answer; a second genuine round of that
// same head adds its own, and another head stays distinct. A span every
// genuine answer of which reviewed is a proven zero, a span no genuine
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
	nr2 := ReviewAttemptFact{PR: 9, Commit: "c2", Round: 1, Model: "big", Note: "the gateway refused the review call"}
	if id := dcRecord(t, s, nr2.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 3)); id == 0 {
		t.Fatal("the second answer recorded no row")
	}
	// the round a retry re-answers keeps one row
	if id := dcRecord(t, s, nr2.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 3).Add(time.Hour)); id != 0 {
		t.Fatalf("the re-answered round recorded %d, want 0: one round, one row", id)
	}
	// a valid negative verdict: concerns judged the change, reviewed
	ok := ReviewAttemptFact{PR: 9, Commit: "c3", Round: 1, Model: "big", Verdict: "concerns"}
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

	// 2 of 3: the window's two answers that never judged over its three
	// genuine answers, the replay having added none; the previous
	// window's one answer reviewed, a proven zero; the spark names both
	// its weeks — the early unjudged answer bucket 0 (Aug 26, the spark
	// opens Aug 24), the previous window's reviewed answer bucket 3
	// (Sep 20) — and the window's two answers bucket 5
	r, err := s.ProjectMetrics(context.Background(), "sf/dc3", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if r.DivideConquer.ReviewsNotReviewed != (Figure{Value: 2, Has: true, PrevHas: true, Weeks: [7]float64{1, 0, 0, 0, 0, 2, 0}}) {
		t.Fatalf("reviews not reviewed = %+v, want 2 of 3", r.DivideConquer.ReviewsNotReviewed)
	}
	if r.DivideConquer.ReviewEvidence.Answers != (Figure{Value: 3, Has: true, Prev: 1, PrevHas: true, Weeks: [7]float64{1, 0, 0, 1, 0, 3, 0}}) {
		t.Fatalf("review answers = %+v, want the measured denominator 3", r.DivideConquer.ReviewEvidence.Answers)
	}
	// a separate genuine round of that same head adds its own answer,
	// and another head stays distinct: the head alone never collapses
	// rounds, the round alone never collapses heads
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "c2", Round: 2, Model: "big", Verdict: "approve"}.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 5))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "c8", Round: 1, Model: "big", Note: "malformed answer"}.Event("t-r1", "sf/dc3", 71), from.AddDate(0, 0, 7))
	again, err := s.ProjectMetrics(context.Background(), "sf/dc3", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// three of five never judged: nr1 and nr2 of the first three, the
	// other head's malformed answer beside them in bucket 6
	if again.DivideConquer.ReviewsNotReviewed != (Figure{Value: 3, Has: true, PrevHas: true, Weeks: [7]float64{1, 0, 0, 0, 0, 2, 1}}) {
		t.Fatalf("reviews not reviewed = %+v, want 3 of 5", again.DivideConquer.ReviewsNotReviewed)
	}
	if again.DivideConquer.ReviewEvidence.Answers != (Figure{Value: 5, Has: true, Prev: 1, PrevHas: true, Weeks: [7]float64{1, 0, 0, 1, 0, 4, 1}}) {
		t.Fatalf("review answers = %+v, want the measured denominator 5", again.DivideConquer.ReviewEvidence.Answers)
	}
	// every row a genuine answer: no hole to name, a measured zero of
	// unknown coverage
	if again.DivideConquer.ReviewEvidence.Unknown != (Figure{Has: true, PrevHas: true}) {
		t.Fatalf("review unknown = %+v, want a measured zero", again.DivideConquer.ReviewEvidence.Unknown)
	}
	// a repository no answer covers reads unknown, never zero
	empty, err := s.ProjectMetrics(context.Background(), "sf/dc-none3", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if empty.DivideConquer.ReviewsNotReviewed != (Figure{}) || empty.DivideConquer.ReviewEvidence != (ReviewEvidence{}) {
		t.Fatalf("reviews without evidence = %+v, want unknown", empty.DivideConquer.ReviewEvidence)
	}
}

// dcLegacyReview records one model_review row exactly as detail says,
// as producers around the fact contract wrote them: keyed false leaves
// it identity-less, and the answer's classification lands only where
// the detail carries one. The row is the answer's whole truth —
// whatever it lacks, the answer lacks.
func dcLegacyReview(t *testing.T, s *PGStore, card, repository, commit, round string, keyed bool, detail map[string]any, at time.Time) int64 {
	t.Helper()
	e := Event{Repository: repository, TaskID: card, Kind: EventModelReview, Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}
	if keyed {
		e.IdentityKey = identityKey(EventModelReview, commit, round)
	}
	return dcRecord(t, s, e, at)
}

// TestDivideConquerReviewEvidenceSQL — panel 9's evidence: only a
// genuine answer is usable — the attempt's identity and the
// classification the payload wrote. A row without its identity,
// however complete its detail, and a row whose classification is
// missing or ambiguous are unknown evidence, never an inferred
// success: the count stands on the genuine rest, the coverage names
// the holes over the window, the window before it and the spark's
// weeks, answers outside the window and of other repositories count
// nowhere, and an empty cohort stays unknown beside a measured zero.
// [not_reviewed_answers] [window_and_existing_figures]
func TestDivideConquerReviewEvidenceSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	dcCard(t, s, "t-ev1", "sf/dc7", "small", "", 74)
	// the window's one genuine answer never judged, its reason bounded
	// where the payload wrote it (Sep 29, bucket 5)
	if id := dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "k1", Round: 1, Model: "big",
		Note: "the gateway refused the review call"}.Event("t-ev1", "sf/dc7", 74), from.AddDate(0, 0, 1)); id == 0 {
		t.Fatal("the genuine answer recorded no row")
	}
	// a legacy row without its attempt identity, however complete its
	// detail: the store cannot hold it as one attempt, so no panel can
	// count it an answer (Oct 2, bucket 5)
	if id := dcLegacyReview(t, s, "t-ev1", "sf/dc7", "k2", "1", false,
		map[string]any{"pr": 9, "commit": "k2", "round": 1, "model": "big", "verdict": "concerns"},
		from.AddDate(0, 0, 4)); id == 0 {
		t.Fatal("the identity-less answer recorded no row")
	}
	// a keyed row whose classification never landed (Oct 7, bucket 6)
	dcLegacyReview(t, s, "t-ev1", "sf/dc7", "k3", "1", true,
		map[string]any{"pr": 9, "commit": "k3", "round": 1, "model": "big"}, from.AddDate(0, 0, 9))
	// the previous window's one row answers "maybe": neither reviewed
	// nor not — ambiguous, so that span is unknown, never a proven zero
	// (Sep 20, bucket 3)
	dcLegacyReview(t, s, "t-ev1", "sf/dc7", "k4", "1", true,
		map[string]any{"pr": 9, "commit": "k4", "round": 1, "model": "big", "reviewed": "maybe"},
		from.AddDate(0, 0, -8))
	// a legacy row before the previous window fills the spark's first
	// week with unknown evidence alone (Aug 29, bucket 0)
	dcLegacyReview(t, s, "t-ev1", "sf/dc7", "k5", "1", false,
		map[string]any{"pr": 9, "commit": "k5", "round": 1, "model": "big", "verdict": "approve"},
		from.AddDate(0, 0, -30))
	// a genuine answer exactly at To: outside every span
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "k6", Round: 1, Model: "big", Verdict: "approve"}.Event("t-ev1", "sf/dc7", 74), to.AddDate(0, 0, 1))
	// another repository's answer, however genuine, stays out
	dcCard(t, s, "t-ev9", "sf/other7", "small", "", 75)
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "k7", Round: 1, Model: "big", Note: "elsewhere"}.Event("t-ev9", "sf/other7", 75), from.AddDate(0, 0, 2))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc7", from, to)
	if err != nil {
		t.Fatal(err)
	}
	// the window's one genuine answer never judged, over the denominator
	// 1; the two holes measure nothing; the previous window's ambiguous
	// row alone proves nothing, so its zero is unknown, never measured
	if r.DivideConquer.ReviewsNotReviewed != (Figure{Value: 1, Has: true, PrevHas: false, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}) {
		t.Fatalf("reviews not reviewed = %+v, want 1 of 1 and an unknown previous window", r.DivideConquer.ReviewsNotReviewed)
	}
	if r.DivideConquer.ReviewEvidence.Answers != (Figure{Value: 1, Has: true, Prev: 0, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}) {
		t.Fatalf("review answers = %+v, want one genuine answer and a measured zero before the window", r.DivideConquer.ReviewEvidence.Answers)
	}
	// the holes over the three spans: two in the window, one in the
	// window before it, and the spark's buckets 0, 3, 5 and 6 each name
	// their own
	if r.DivideConquer.ReviewEvidence.Unknown != (Figure{Value: 2, Has: true, Prev: 1, PrevHas: true, Weeks: [7]float64{1, 0, 0, 1, 0, 1, 1}}) {
		t.Fatalf("review unknown = %+v, want the holes over the three spans", r.DivideConquer.ReviewEvidence.Unknown)
	}
	// the other repository's answer answers its own report alone — one
	// not-reviewed of one genuine answer, its own spark week (Sep 30,
	// bucket 5) and nothing before the window
	other, err := s.ProjectMetrics(context.Background(), "sf/other7", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if other.DivideConquer.ReviewsNotReviewed != (Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}) ||
		other.DivideConquer.ReviewEvidence.Answers.Value != 1 {
		t.Fatalf("other repository reviews = %+v %+v, want its one answer alone",
			other.DivideConquer.ReviewsNotReviewed, other.DivideConquer.ReviewEvidence.Answers)
	}
	// a repository no answer of any kind covers: every figure unknown,
	// never zero
	none, err := s.ProjectMetrics(context.Background(), "sf/dc-none7", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if none.DivideConquer.ReviewsNotReviewed != (Figure{}) || none.DivideConquer.ReviewEvidence != (ReviewEvidence{}) {
		t.Fatalf("reviews without rows = %+v, want the unknown block", none.DivideConquer.ReviewEvidence)
	}
	// a repository whose every answer judged: a proven zero over its
	// measured denominator, no hole to name
	dcCard(t, s, "t-zero7", "sf/dc-zero7", "small", "", 76)
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "z1", Round: 1, Model: "big", Verdict: "approve"}.Event("t-zero7", "sf/dc-zero7", 76), from.AddDate(0, 0, 2))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "z2", Round: 1, Model: "big", Verdict: "concerns"}.Event("t-zero7", "sf/dc-zero7", 76), from.AddDate(0, 0, 6))
	zero, err := s.ProjectMetrics(context.Background(), "sf/dc-zero7", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if zero.DivideConquer.ReviewsNotReviewed != (Figure{Has: true}) {
		t.Fatalf("reviews of judged answers = %+v, want a measured zero", zero.DivideConquer.ReviewsNotReviewed)
	}
	// two judged answers, both of the window's week (Sep 30 and Oct 4,
	// bucket 5): no hole to name over the measured denominator
	if zero.DivideConquer.ReviewEvidence != (ReviewEvidence{Answers: Figure{Value: 2, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 2, 0}}, Unknown: Figure{Has: true}}) {
		t.Fatalf("review evidence of judged answers = %+v, want two answers and no hole", zero.DivideConquer.ReviewEvidence)
	}
}

// TestDivideConquerFlowWindowsSQL — the block's spans and neighbours:
// every panel reads the window, the equal-length window before it and
// the seven weekly buckets, holds the repository to its own rows,
// answers an empty repository with the whole block unknown, and leaves
// the deck's existing figures standing. The size-bound panel's evidence
// spans the same three: its window's reading measures, its previous
// window's does, and a hole in a weekly bucket measures nothing there;
// so does the review panel's, whose previous window a legacy row alone
// leaves unknown, never a measured zero.
// [window_and_existing_figures]
func TestDivideConquerFlowWindowsSQL(t *testing.T) {
	s := metricsStore(t)
	from, to := metricsTestWindow()
	dcCard(t, s, "c1", "sf/dc4", "small", "", 81)
	dcGateHead(t, s, "c1", "sf/dc4", "g1", GateMerge, 300, 350, 0, from.AddDate(0, 0, 8))  // window, bucket 6
	dcGateHead(t, s, "c1", "sf/dc4", "g0", GateMerge, 300, 350, 0, from.AddDate(0, 0, -8)) // previous window, bucket 3
	dcSessionEnd(t, s, "se1", string(OutcomeWallClock), "c1", "sf/dc4", 1, from.AddDate(0, 0, 9))
	// the window's one genuine answer (Oct 7, bucket 6): its own fact
	// identity — commit f1, round 1 — because the package's tests share
	// one database and the identity index holds one row per key, so a
	// commit another test's answer already holds would drop this row
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "f1", Round: 1, Model: "big", Verdict: "approve"}.Event("c1", "sf/dc4", 81), from.AddDate(0, 0, 9))
	// a legacy identity-less row, over-looking in the window (Sep 30,
	// bucket 5) — it measures nothing: the panel keeps its one escape
	// and the bucket its zero, and the coverage names the hole
	dcCard(t, s, "c2", "sf/dc4", "small", "", 84)
	if id := dcLegacyGateHead(t, s, "c2", "sf/dc4", "g8", false,
		map[string]any{"gate": GateMerge, "head": "g8", "decision": "auto", "size": "small", "bound": 300, "insertions": 900, "deletions": 900},
		from.AddDate(0, 0, 2)); id == 0 {
		t.Fatal("the legacy reading recorded no row")
	}
	// a legacy review row without identity or classification, alone in
	// the previous window (Sep 20, bucket 3): it proves nothing there —
	// the review panel's previous window stays unknown, never a
	// measured zero — and the unknown coverage names the hole
	if id := dcLegacyReview(t, s, "c2", "sf/dc4", "g8", "1", false,
		map[string]any{"pr": 9, "commit": "g8", "round": 1, "model": "big", "verdict": "approve"},
		from.AddDate(0, 0, -8)); id == 0 {
		t.Fatal("the legacy answer recorded no row")
	}
	// another repository's rows the same instants — ow1, not o1 again:
	// the package's tests share one database, one task id once
	dcCard(t, s, "ow1", "sf/other4", "small", "", 82)
	dcGateHead(t, s, "ow1", "sf/other4", "g1", GateMerge, 300, 999, 0, from.AddDate(0, 0, 8))
	dcSessionEnd(t, s, "oe1", string(OutcomePushed), "ow1", "sf/other4", 1, from.AddDate(0, 0, 9))
	dcCard(t, s, "ow2", "sf/other4", "small", "", 85)
	dcLegacyGateHead(t, s, "ow2", "sf/other4", "g8", false,
		map[string]any{"gate": GateMerge, "head": "g8", "decision": "auto", "size": "small", "bound": 300, "insertions": 900, "deletions": 900},
		from.AddDate(0, 0, 2))
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
	// the window's two readings: g1 measured, g8 an identity-less hole;
	// the other repository's rows, measured or holed, stay out
	if d.GateBoundCoverage != (GateBoundCoverage{Readings: 2, Measured: 1, Identityless: 1}) {
		t.Fatalf("gate bound coverage = %+v, want one measured reading and its identity-less hole", d.GateBoundCoverage)
	}
	if len(d.SessionEnds) != 1 || d.SessionEnds[0] != (NamedCount{Name: string(OutcomeWallClock), Count: 1}) {
		t.Fatalf("session ends = %+v, want the window's wall_clock alone", d.SessionEnds)
	}
	if d.ReviewsNotReviewed != (Figure{Has: true, PrevHas: false}) {
		t.Fatalf("reviews not reviewed = %+v, want a measured zero of one reviewed answer", d.ReviewsNotReviewed)
	}
	// the window's one genuine answer over its one row — its hole count
	// a measured zero, the row covers the span — the previous window's
	// legacy row a measured zero of genuine answers beside its one hole,
	// and the spark's third week names that hole alone
	if d.ReviewEvidence != (ReviewEvidence{
		Answers: Figure{Value: 1, Has: true, Prev: 0, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}},
		Unknown: Figure{Has: true, Prev: 1, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 0}},
	}) {
		t.Fatalf("review evidence = %+v, want one answer in the window and the legacy hole before it", d.ReviewEvidence)
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
		e.GateAboveBound != (Figure{}) || e.GateBoundCoverage != (GateBoundCoverage{}) ||
		e.ReviewsNotReviewed != (Figure{}) || e.ReviewEvidence != (ReviewEvidence{}) {
		t.Fatalf("divide conquer without evidence = %+v, want the zero block", e)
	}
}

// TestDivideConquerCombinedReportSQL — the composed report: one window's
// cards carry nonzero flow facts and nonzero economics facts together,
// and the full ProjectMetrics answers both families at once. Four
// measured cards — small (100,3) and (300,5), medium (200,5) and
// (400,7), changed lines against whole-life euros — hold EUR per merged
// line at 0.0200 and the size intercepts at small 2.00 and medium 3.00
// through the flow block's composition, while the same cards' gate
// readings, sessions and review answers fill the flow panels; a later
// abandoned head never supplies a card's final lines, a reading without
// diff statistics stays unavailable — never zero — and the deck's own
// spend, throughput and dates read unchanged. The other project's card
// answers only its own report. [merged_economics]
// [window_and_existing_figures]
func TestDivideConquerCombinedReportSQL(t *testing.T) {
	s := seedEconStore(t)
	from, to := metricsTestWindow()
	eq := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	// cb1: small, 100 merged lines, 3 EUR. Its abandoned head crossed the
	// small bound (700 lines) and its reading landed after the merged
	// head's own row: the crossing counts the card once for panel 1, the
	// abandoned lines never reach the economics panels
	dcDoneCard(t, s, "cb1", "sf/dc5", "small", "cb1-h", 91, from.AddDate(0, 0, 7))
	dcMerged(t, s, "cb1", "sf/dc5", from.AddDate(0, 0, 7).Add(10*time.Hour))
	dcReadingFull(t, s, "cb1", "sf/dc5", "cb1-h", "small", 300, 80, 20, 91, from.AddDate(0, 0, 7).Add(10*time.Hour+30*time.Minute))
	dcReadingFull(t, s, "cb1", "sf/dc5", "cb1-x", "small", 300, 500, 200, 91, from.AddDate(0, 0, 9).Add(9*time.Hour))
	dcSpend(t, s, "cb1", "2026-10-05", 3)
	dcStarted(t, s, "c1a", "cb1", "sf/dc5", 1, from.AddDate(0, 0, 7).Add(2*time.Hour))
	dcStarted(t, s, "c1b", "cb1", "sf/dc5", 2, from.AddDate(0, 0, 7).Add(3*time.Hour))
	dcSessionEnd(t, s, "c1a", string(OutcomePushed), "cb1", "sf/dc5", 1, from.AddDate(0, 0, 7).Add(12*time.Hour))
	dcSessionEnd(t, s, "c1b", string(OutcomeWallClock), "cb1", "sf/dc5", 2, from.AddDate(0, 0, 7).Add(13*time.Hour))
	// cb2: small, 300 lines exactly at its bound — a bound is not a
	// target — 5 EUR; one session, and the window's two review answers,
	// one that judged the head and one that never did
	dcDoneCard(t, s, "cb2", "sf/dc5", "small", "cb2-h", 92, from.AddDate(0, 0, 7))
	dcMerged(t, s, "cb2", "sf/dc5", from.AddDate(0, 0, 7).Add(11*time.Hour))
	dcReadingFull(t, s, "cb2", "sf/dc5", "cb2-h", "small", 300, 240, 60, 92, from.AddDate(0, 0, 7).Add(11*time.Hour+30*time.Minute))
	dcSpend(t, s, "cb2", "2026-10-05", 5)
	dcStarted(t, s, "c2a", "cb2", "sf/dc5", 1, from.AddDate(0, 0, 7).Add(4*time.Hour))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "cb2-r1", Round: 1, Model: "big", Verdict: "approve"}.Event("cb2", "sf/dc5", 92), from.AddDate(0, 0, 7).Add(14*time.Hour))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "cb2-r2", Round: 1, Model: "big", Note: "the gateway refused"}.Event("cb2", "sf/dc5", 92), from.AddDate(0, 0, 8).Add(14*time.Hour))
	// cb3: medium, 200 lines, 5 EUR; two sessions, one pushed
	dcDoneCard(t, s, "cb3", "sf/dc5", "medium", "cb3-h", 93, from.AddDate(0, 0, 8))
	dcMerged(t, s, "cb3", "sf/dc5", from.AddDate(0, 0, 8).Add(10*time.Hour))
	dcReadingFull(t, s, "cb3", "sf/dc5", "cb3-h", "medium", 600, 150, 50, 93, from.AddDate(0, 0, 8).Add(10*time.Hour+30*time.Minute))
	dcSpend(t, s, "cb3", "2026-10-06", 5)
	dcStarted(t, s, "c3a", "cb3", "sf/dc5", 1, from.AddDate(0, 0, 8).Add(2*time.Hour))
	dcStarted(t, s, "c3b", "cb3", "sf/dc5", 2, from.AddDate(0, 0, 8).Add(3*time.Hour))
	dcSessionEnd(t, s, "c3a", string(OutcomePushed), "cb3", "sf/dc5", 1, from.AddDate(0, 0, 8).Add(12*time.Hour))
	// cb4: medium, 400 lines, 7 EUR; one session, no end inside the window
	dcDoneCard(t, s, "cb4", "sf/dc5", "medium", "cb4-h", 94, from.AddDate(0, 0, 8))
	dcMerged(t, s, "cb4", "sf/dc5", from.AddDate(0, 0, 8).Add(11*time.Hour))
	dcReadingFull(t, s, "cb4", "sf/dc5", "cb4-h", "medium", 600, 300, 100, 94, from.AddDate(0, 0, 8).Add(11*time.Hour+30*time.Minute))
	dcSpend(t, s, "cb4", "2026-10-06", 7)
	dcStarted(t, s, "c4a", "cb4", "sf/dc5", 1, from.AddDate(0, 0, 8).Add(5*time.Hour))
	// cb5: merged and spent, its reading at the merged head carrying no
	// diff statistics — the lines stay unavailable, a hole the coverage
	// names, never a zero the panels divide by
	dcDoneCard(t, s, "cb5", "sf/dc5", "small", "cb5-h", 95, from.AddDate(0, 0, 9))
	dcMerged(t, s, "cb5", "sf/dc5", from.AddDate(0, 0, 9).Add(11*time.Hour))
	if _, err := s.db.ExecContext(context.Background(), `INSERT INTO events (at, repository, task_id, kind, actor, via, detail)
		VALUES ($1, 'sf/dc5', 'cb5', 'gate_head', 'factoryd', 'factoryd', $2)`,
		from.AddDate(0, 0, 9).Add(11*time.Hour+30*time.Minute),
		`{"gate":"merge","head":"cb5-h","decision":"auto","size":"small","bound":300,"policy_version":7,"files":9}`); err != nil {
		t.Fatal(err)
	}
	dcSpend(t, s, "cb5", "2026-10-07", 2)
	// cx1, the other project's card: the same days, crossing its own
	// bound — its rows answer sf/other5's report alone
	dcDoneCard(t, s, "cx1", "sf/other5", "small", "cx1-h", 96, from.AddDate(0, 0, 8))
	dcMerged(t, s, "cx1", "sf/other5", from.AddDate(0, 0, 8).Add(15*time.Hour))
	dcReadingFull(t, s, "cx1", "sf/other5", "cx1-h", "small", 300, 900, 100, 96, from.AddDate(0, 0, 8).Add(15*time.Hour+30*time.Minute))
	dcSpend(t, s, "cx1", "2026-10-06", 50)
	dcStarted(t, s, "x1a", "cx1", "sf/other5", 1, from.AddDate(0, 0, 8).Add(16*time.Hour))
	dcSessionEnd(t, s, "x1a", string(OutcomePushed), "cx1", "sf/other5", 1, from.AddDate(0, 0, 8).Add(17*time.Hour))
	dcRecord(t, s, ReviewAttemptFact{PR: 9, Commit: "cx1-r", Round: 1, Model: "big", Note: "elsewhere"}.Event("cx1", "sf/other5", 96), from.AddDate(0, 0, 8).Add(18*time.Hour))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc5", from, to)
	if err != nil {
		t.Fatal(err)
	}
	d := r.DivideConquer
	// 20 EUR over the merged heads' 1000 changed lines, all merged into
	// the last bucket: the economics panels survive the flow block's
	// composition, cb5's absent statistics included on neither side
	if d.EurPerMergedLine != (Figure{Value: 0.02, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 0.02}}) {
		t.Fatalf("eur per merged line = %+v, want 0.0200 after composition", d.EurPerMergedLine)
	}
	// small fits (100,3) and (300,5) at 2.00, medium (200,5) and (400,7)
	// at 3.00: the abandoned head's 700 lines never enter the fit
	fits := d.FixedEurPerCard
	if len(fits) != 2 || fits[0].Size != "medium" || fits[1].Size != "small" {
		t.Fatalf("fixed eur per card = %+v, want medium then small", fits)
	}
	for _, fit := range fits {
		w := 3.0
		if fit.Size == "small" {
			w = 2.0
		}
		if !fit.Available || fit.Cards != 2 || !eq(fit.EUR, w) {
			t.Fatalf("%s = %+v, want the intercept %.2f over 2 cards", fit.Size, fit, w)
		}
	}
	if cov := d.EconomicsCoverage; cov != (EconomicsCoverage{Cards: 5, MissingLines: 1}) {
		t.Fatalf("coverage = %+v, want the five merged cards, cb5's statistics absent", cov)
	}
	// the same cards answer the flow panels beside the economics ones:
	// the abandoned head's crossing counts its card once over five
	// readings' evidence, the sessions fill both sizes, the ends name
	// their causes and one answer of two never judged
	if !d.CoveredFrom.Equal(from.AddDate(0, 0, 7).Add(10*time.Hour + 30*time.Minute)) {
		t.Fatalf("covered from = %v, want the earliest fact row", d.CoveredFrom)
	}
	if d.GateAboveBound != (Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}) {
		t.Fatalf("gate above bound = %+v, want cb1's crossing alone", d.GateAboveBound)
	}
	wantSessions := []SizeFigure{
		{Size: "medium", Figure: Figure{Value: 1.5, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1.5}}},
		{Size: "small", Figure: Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}},
	}
	if got := d.SessionsPerCard; len(got) != len(wantSessions) || got[0] != wantSessions[0] || got[1] != wantSessions[1] {
		t.Fatalf("sessions per card = %+v, want %+v", got, wantSessions)
	}
	if len(d.SessionEnds) != 2 || d.SessionEnds[0] != (NamedCount{Name: string(OutcomePushed), Count: 2}) ||
		d.SessionEnds[1] != (NamedCount{Name: string(OutcomeWallClock), Count: 1}) {
		t.Fatalf("session ends = %+v, want pushed 2 and wall_clock 1", d.SessionEnds)
	}
	if d.ReviewsNotReviewed != (Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}) {
		t.Fatalf("reviews not reviewed = %+v, want one of the two answers", d.ReviewsNotReviewed)
	}
	// the additive named comparisons compose beside the economics: no
	// plan fact seeds this fixture, so the planning panels stay empty —
	// measured zeros where their own panels read, absence here
	if len(d.PlansByRounds) != 0 || len(d.PlansByRoundsSpans) != 0 || len(d.CardsByRound) != 0 ||
		len(d.ReplansByOrigin) != 0 || len(d.ReplansByOriginSpans) != 0 {
		t.Fatalf("planning panels = %+v %+v %+v %+v %+v, want none beside the economics",
			d.PlansByRounds, d.PlansByRoundsSpans, d.CardsByRound, d.ReplansByOrigin, d.ReplansByOriginSpans)
	}
	// the deck's own figures stand: the dates asked, the window's five
	// done cards, the window's 22 ledger euros — cb5's included, whatever
	// its absent statistics
	if r.Repository != "sf/dc5" || !r.From.Equal(from) || !r.To.Equal(to) {
		t.Fatalf("report = %s %v %v", r.Repository, r.From, r.To)
	}
	if r.Throughput != (Figure{Value: 5, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 5}}) {
		t.Fatalf("throughput = %+v", r.Throughput)
	}
	if r.Spend != (Figure{Value: 22, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 22}}) {
		t.Fatalf("spend = %+v", r.Spend)
	}
	// the other project's card answers only its own report
	ro, err := s.ProjectMetrics(context.Background(), "sf/other5", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if f := ro.DivideConquer.EurPerMergedLine; !f.Has || f.Value != 0.05 || f.PrevHas || f.Prev != 0 {
		t.Fatalf("sf/other5 eur per merged line = %+v, want 50 EUR over 1000 lines", f)
	}
	if ro.DivideConquer.GateAboveBound != (Figure{Value: 1, Has: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}) {
		t.Fatalf("sf/other5 gate above bound = %+v, want its own crossing alone", ro.DivideConquer.GateAboveBound)
	}
	if ro.Throughput != (Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}) ||
		ro.Spend != (Figure{Value: 50, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 50}}) {
		t.Fatalf("sf/other5 throughput = %+v spend = %+v, want its own alone", ro.Throughput, ro.Spend)
	}
}

// dcPlan inserts a plan of repository on its own issue, done as a
// closed plan's row stands.
func dcPlan(t *testing.T, s *PGStore, id, repository string, issue int64) {
	t.Helper()
	if err := s.insertTask(context.Background(), Task{ID: id, Status: "done", Objective: "the divide panels' fixture",
		Repository: repository, Kind: "agent", Source: "api", Track: policy.TrackPlan, BudgetEUR: 5, IssueNumber: issue}, ""); err != nil {
		t.Fatal(err)
	}
}

// dcPlanChild inserts a card a plan filed — source plan, its parent the
// plan's id, so GroupOf names the plan — on its own issue.
func dcPlanChild(t *testing.T, s *PGStore, id, plan, repository string, issue int64) {
	t.Helper()
	if err := s.insertTask(context.Background(), Task{ID: id, Status: "done", Objective: "the divide panels' fixture",
		Repository: repository, Kind: "agent", Source: SourcePlan, Parent: plan, Track: "direct", BudgetEUR: 5, IssueNumber: issue}, ""); err != nil {
		t.Fatal(err)
	}
}

// dcClosed records the plan's plan_closed row at at, as the verdict
// pass does when a plan's last card ends.
func dcClosed(t *testing.T, s *PGStore, plan, repository string, at time.Time) {
	t.Helper()
	dcRecord(t, s, Event{Repository: repository, TaskID: plan, Kind: EventPlanClosed,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"criteria": 3}}, at)
}

// dcLegacyReplan records a plan_replanned row exactly as detail says,
// the shape producers before the identity contract wrote: no identity
// key, the row its own whole truth.
func dcLegacyReplan(t *testing.T, s *PGStore, plan, repository string, issue int64, detail map[string]any, at time.Time) int64 {
	t.Helper()
	return dcRecord(t, s, Event{Repository: repository, TaskID: plan, IssueNumber: issue, Kind: EventPlanReplanned,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}, at)
}

// dcFiling records a canonical filing row through the fact payload:
// revision-level for a replan (no card), the card's own for a filing,
// extra carrying what the filing names — its round.
func dcFiling(t *testing.T, s *PGStore, plan, repository string, revision int, origin string, card int64, key string, issue int64, extra map[string]any, at time.Time) int64 {
	t.Helper()
	return dcRecord(t, s, PlanFilingFact{Plan: plan, Revision: revision, Origin: origin, Card: card, Key: key}.
		Event(repository, issue, extra), at)
}

// dcPlanRevision stores one plan revision as the store kept it, the
// plan json stamped with the round factoryd filed, at created.
func dcPlanRevision(t *testing.T, s *PGStore, plan string, revision, round int, created time.Time) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO plan_revisions (task_id, revision, plan, sha256, created_at) VALUES ($1, $2, $3, $4, $5)`,
		plan, revision, fmt.Sprintf(`{"round": %d}`, round), fmt.Sprintf("sha-%s-%d", plan, revision), created); err != nil {
		t.Fatal(err)
	}
}

// TestDivideConquerCutsSQL — panel 6: the window's applied cuts and the
// cut groups its own end saw closed. The cut is the fact the replan that
// applied it records: a retried filing counts once, the replan's own row
// and the asks — the console's card_replanned, a plan_replanned row of
// origin cut — never count. A group cut twice is one group; a closure
// later than the span's end (t-cut-b's, Oct 13) never rewrites the
// window or the bucket that ended before it; other projects answer their
// own reports alone.
// [cut_group_completion]
func TestDivideConquerCutsSQL(t *testing.T) {
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
	dcPlan(t, s, "t-cut-a", "sf/dc8", 301)
	dcPlanChild(t, s, "t-cut-a1", "t-cut-a", "sf/dc8", 311)
	dcPlanChild(t, s, "t-cut-a2", "t-cut-a", "sf/dc8", 312)
	dcPlan(t, s, "t-cut-b", "sf/dc8", 302)
	dcPlanChild(t, s, "t-cut-b1", "t-cut-b", "sf/dc8", 321)
	// the first cut, Sep 30, retried whole: the identity holds it to
	// one row, and the replan that applied it records its own
	// plan_replanned row — a replan, never a cut
	cut := CutFact{Plan: "t-cut-a", Card: "t-cut-a1", Issue: 311, Group: "t-cut-a", Revision: 2,
		Into: []int64{313, 314}, Session: 3, Spent: 4.5}
	if id := dcRecord(t, s, cut.Event("sf/dc8", 311), from.AddDate(0, 0, 2)); id == 0 {
		t.Fatal("the cut recorded no row")
	}
	if id := dcRecord(t, s, cut.Event("sf/dc8", 311), from.AddDate(0, 0, 2)); id != 0 {
		t.Fatalf("the retried cut recorded %d, want 0: one cut, one row", id)
	}
	dcFiling(t, s, "t-cut-a", "sf/dc8", 2, ReplanCut, 0, "", 301,
		map[string]any{"kept": 1, "changed": 0, "dropped": 1, "added": 2, "by": "", "comment": ""}, from.AddDate(0, 0, 2))
	// the asks: a console card_replanned on the cut card, and a legacy
	// plan_replanned row of origin cut — requests, both, not cuts
	dcRecord(t, s, Event{Repository: "sf/dc8", TaskID: "t-cut-a1", IssueNumber: 311, Kind: EventCardReplanned,
		Actor: "someone", Via: ViaFactoryd, Detail: map[string]any{"into": []int64{313, 314}}}, from.AddDate(0, 0, 3))
	dcLegacyReplan(t, s, "t-cut-a", "sf/dc8", 301,
		map[string]any{"revision": 9, "origin": ReplanCut, "kept": 1, "changed": 0, "dropped": 0, "added": 0}, from.AddDate(0, 0, 3))
	// the second cut of the same group, Oct 6: two cuts, one group
	dcRecord(t, s, CutFact{Plan: "t-cut-a", Card: "t-cut-a2", Issue: 312, Group: "t-cut-a", Revision: 3,
		Into: []int64{315}, Session: 4, Spent: 1}.Event("sf/dc8", 312), from.AddDate(0, 0, 8))
	dcClosed(t, s, "t-cut-a", "sf/dc8", from.AddDate(0, 0, 10))
	// the previous window's cut, its group closed only Oct 13: no span
	// of this window's report sees that closure, and no cut of the
	// group falls in the window itself
	dcRecord(t, s, CutFact{Plan: "t-cut-b", Card: "t-cut-b1", Issue: 321, Group: "t-cut-b", Revision: 2,
		Into: []int64{322}, Session: 2, Spent: 1}.Event("sf/dc8", 321), from.AddDate(0, 0, -8))
	dcClosed(t, s, "t-cut-b", "sf/dc8", to.AddDate(0, 0, 2))
	// the other project's cut and closure answer its report alone
	dcPlan(t, s, "t-cut-far", "sf/other8", 391)
	dcPlanChild(t, s, "t-cut-far1", "t-cut-far", "sf/other8", 392)
	dcRecord(t, s, CutFact{Plan: "t-cut-far", Card: "t-cut-far1", Issue: 392, Group: "t-cut-far", Revision: 2,
		Into: []int64{393}, Session: 1, Spent: 1}.Event("sf/other8", 392), from.AddDate(0, 0, 4))
	dcClosed(t, s, "t-cut-far", "sf/other8", from.AddDate(0, 0, 5))

	r, err := s.ProjectMetrics(context.Background(), "sf/dc8", from, to)
	if err != nil {
		t.Fatal(err)
	}
	d := r.DivideConquer
	// two applied cuts in the window (Sep 30 bucket 5, Oct 6 bucket 6),
	// one in the previous (Sep 20, bucket 3); the asks never counted
	if d.Cuts != (Figure{Value: 2, Prev: 1, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 1, 0, 1, 1}}) {
		t.Fatalf("cuts = %+v, want two cuts, one before the window", d.Cuts)
	}
	// one closed cut group: t-cut-a closed Oct 8, before the window's
	// end Oct 12 — but after bucket 5 ended Oct 5, so only bucket 6
	// sees it; t-cut-b's Oct 13 closure changes nothing here
	if d.CutGroupsClosed != (Figure{Value: 1, Prev: 0, Has: true, PrevHas: true,
		Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}) {
		t.Fatalf("cut groups closed = %+v, want the one group the window's end saw closed", d.CutGroupsClosed)
	}
	// the other project's cut and closure answer its report alone
	ro, err := s.ProjectMetrics(context.Background(), "sf/other8", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if ro.DivideConquer.Cuts != (Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}) {
		t.Fatalf("sf/other8 cuts = %+v, want its own cut alone", ro.DivideConquer.Cuts)
	}
	if ro.DivideConquer.CutGroupsClosed != (Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 0}}) {
		t.Fatalf("sf/other8 cut groups closed = %+v, want its own group, closed inside its bucket",
			ro.DivideConquer.CutGroupsClosed)
	}
	// a repository no cut covers reads a measured zero, not an unknown
	empty, err := s.ProjectMetrics(context.Background(), "sf/dc-none8", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if empty.DivideConquer.Cuts != (Figure{Has: true, PrevHas: true}) ||
		empty.DivideConquer.CutGroupsClosed != (Figure{Has: true, PrevHas: true}) {
		t.Fatalf("without cuts: cuts %+v groups %+v, want measured zeros", empty.DivideConquer.Cuts, empty.DivideConquer.CutGroupsClosed)
	}
	// closures arriving after the report was taken — t-cut-a's group
	// closed again beyond the window, t-cut-b's still later — rewrite no
	// older answer: the window's, the previous window's and every
	// bucket's figures stand as they were read
	dcClosed(t, s, "t-cut-a", "sf/dc8", to.AddDate(0, 0, 5))
	dcClosed(t, s, "t-cut-b", "sf/dc8", to.AddDate(0, 0, 4))
	again, err := s.ProjectMetrics(context.Background(), "sf/dc8", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if again.DivideConquer.Cuts != d.Cuts || again.DivideConquer.CutGroupsClosed != d.CutGroupsClosed {
		t.Fatalf("after later closures: cuts %+v groups %+v, want the older answers %+v %+v",
			again.DivideConquer.Cuts, again.DivideConquer.CutGroupsClosed, d.Cuts, d.CutGroupsClosed)
	}
}

// TestDivideConquerPlanningSQL — panels 10, 11 and 12: the window's
// closed plans by the rounds each ran to its closure, its new card
// filings of the first round against later ones, and its applied replans
// by who asked them, the rounds and the filings compared name by name
// over the window, the equal window before it and all seven weeks
// (PlansByRoundsSpans, CardsByRound, ReplansByOriginSpans — the
// unknown round and the unknown origin explicit buckets, rounds past
// the first and every origin each their own name). Rounds are
// explicit round evidence as of the first closure — a filing's stamped
// round, a round replan's row, the plan revisions the store kept —
// never a count of revisions, never a round a revision filed after the
// close (t-plan-2's round-5 revision), never a duplicate close (t-plan-2
// in-window, t-plan-3 after its own); a card filed again keeps its
// original filing's round and instant wherever it was filed (e6, first
// in the previous window); a revision filing several cards is one
// replan, the replan itself the revision's original row — earliest
// first, so a duplicate observation landing in a later window files no
// second replan and belongs, wherever the panels read it, to the window
// its original instant falls in, its origin resolved as of the span's
// own end — the week's own bucket its cutoff (the origin spans' own
// fixture below holds the duplicates); a canonical card row neither
// suppresses a legacy
// revision-level replan (t-plan-5) nor counts as one, and a legacy row
// doubles no canonical one; an originless gap keeps its own unknown
// bucket; a canonical replacement arriving after the closure and the
// window (t-plan-6's round 9) rewrites no older answer, one recorded
// exactly at the window's own exclusive end (t-plan-9) suppresses
// nothing it cannot count, and one another repository recorded
// (t-plan-10) suppresses no round or origin evidence here, and wins
// none. The same definitions answer the previous window and a week of
// the spark.
// [planning_rounds_origins]
func TestDivideConquerPlanningSQL(t *testing.T) {
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
	{
		// t-plan-1, two rounds to its Oct 5 closure: round 1's two cards
		// filed Sep 29, a person's replan Oct 2 that filed no new card,
		// its round 2 the stored revision stamps — and a legacy row of
		// the same revision that doubles nothing
		dcPlan(t, s, "t-plan-1", "sf/dc9", 201)
		dcFiling(t, s, "t-plan-1", "sf/dc9", 1, "", 101, "a1", 101, map[string]any{"round": 1}, from.AddDate(0, 0, 1))
		dcFiling(t, s, "t-plan-1", "sf/dc9", 1, "", 102, "a2", 102, map[string]any{"round": 1}, from.AddDate(0, 0, 1))
		dcFiling(t, s, "t-plan-1", "sf/dc9", 2, ReplanPerson, 0, "", 201,
			map[string]any{"kept": 1, "changed": 0, "dropped": 1, "added": 0, "by": "the planner", "comment": "drop the tail"}, from.AddDate(0, 0, 4))
		dcPlanRevision(t, s, "t-plan-1", 1, 1, from.AddDate(0, 0, 1))
		dcPlanRevision(t, s, "t-plan-1", 2, 2, from.AddDate(0, 0, 4))
		dcLegacyReplan(t, s, "t-plan-1", "sf/dc9", 201,
			map[string]any{"revision": 2, "origin": ReplanPerson, "kept": 1, "changed": 0, "dropped": 0, "added": 0}, from.AddDate(0, 0, 2))
		dcClosed(t, s, "t-plan-1", "sf/dc9", from.AddDate(0, 0, 7))
		// t-plan-2, one round, closed Oct 6: two cards Oct 3, a revision
		// after the closure whose stored plan stamps round 5 — never a
		// round the plan ran — and a re-file of a held card the panel
		// must not read: b1's original filing, round 1, counts once
		dcPlan(t, s, "t-plan-2", "sf/dc9", 202)
		dcFiling(t, s, "t-plan-2", "sf/dc9", 1, "", 111, "b1", 111, map[string]any{"round": 1}, from.AddDate(0, 0, 5))
		dcFiling(t, s, "t-plan-2", "sf/dc9", 1, "", 112, "b2", 112, map[string]any{"round": 1}, from.AddDate(0, 0, 5))
		dcFiling(t, s, "t-plan-2", "sf/dc9", 2, "", 111, "b1", 111, map[string]any{"round": 2}, from.AddDate(0, 0, 9))
		dcPlanRevision(t, s, "t-plan-2", 1, 1, from.AddDate(0, 0, -8))
		dcPlanRevision(t, s, "t-plan-2", 2, 5, from.AddDate(0, 0, 11))
		dcClosed(t, s, "t-plan-2", "sf/dc9", from.AddDate(0, 0, 8))
		// t-plan-3, the previous window's closure (Sep 20): round 1
		// filed Sep 15, a round replan Sep 16 whose row and card name
		// round 2, then a cut's replan Oct 3 — the plan reopened — whose
		// two replacement cards are one replan of round 2
		dcPlan(t, s, "t-plan-3", "sf/dc9", 203)
		dcFiling(t, s, "t-plan-3", "sf/dc9", 1, "", 121, "e1", 121, map[string]any{"round": 1}, from.AddDate(0, 0, -13))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 1, "", 122, "e2", 122, map[string]any{"round": 1}, from.AddDate(0, 0, -13))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 2, ReplanRound, 0, "", 203,
			map[string]any{"round": 2, "cards": []int64{123}}, from.AddDate(0, 0, -12))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 2, ReplanRound, 123, "e3", 123, map[string]any{"round": 2}, from.AddDate(0, 0, -12))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 3, ReplanCut, 0, "", 203,
			map[string]any{"kept": 2, "changed": 0, "dropped": 1, "added": 2, "by": "", "comment": ""}, from.AddDate(0, 0, 5))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 3, ReplanCut, 124, "e4", 124, map[string]any{"round": 2}, from.AddDate(0, 0, 5))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 3, ReplanCut, 125, "e5", 125, map[string]any{"round": 2}, from.AddDate(0, 0, 5))
		dcClosed(t, s, "t-plan-3", "sf/dc9", from.AddDate(0, 0, -8))
		// t-plan-4, never closed: one card Sep 17, a round replan Oct 6,
		// and a legacy row Oct 5 of a revision no canonical row holds —
		// an origin the record lost, its own unknown bucket
		dcPlan(t, s, "t-plan-4", "sf/dc9", 204)
		dcFiling(t, s, "t-plan-4", "sf/dc9", 1, "", 131, "f1", 131, map[string]any{"round": 1}, from.AddDate(0, 0, -11))
		dcFiling(t, s, "t-plan-4", "sf/dc9", 2, ReplanRound, 0, "", 204,
			map[string]any{"round": 2, "cards": []int64{132}}, from.AddDate(0, 0, 8))
		dcFiling(t, s, "t-plan-4", "sf/dc9", 2, ReplanRound, 132, "f2", 132, map[string]any{"round": 2}, from.AddDate(0, 0, 8))
		dcLegacyReplan(t, s, "t-plan-4", "sf/dc9", 204, map[string]any{"revision": 3, "kept": 1}, from.AddDate(0, 0, 7))
		// duplicate closes inflate nothing: t-plan-2's second close Oct 9
		// lands in-window, t-plan-3's Oct 6 after its first (Sep 20, the
		// previous window's own) — each plan keeps its first closure
		dcClosed(t, s, "t-plan-2", "sf/dc9", from.AddDate(0, 0, 11))
		dcClosed(t, s, "t-plan-3", "sf/dc9", from.AddDate(0, 0, 8))
		// t-plan-3's key e6, first filed Sep 16 round 1, refiled Oct 4
		// round 2: the original filing alone counts, at its own instant
		// and round, in whichever window and week the panels read — the
		// refile adds nothing anywhere
		dcFiling(t, s, "t-plan-3", "sf/dc9", 1, "", 126, "e6", 126, map[string]any{"round": 1}, from.AddDate(0, 0, -12))
		dcFiling(t, s, "t-plan-3", "sf/dc9", 3, ReplanCut, 126, "e6", 126, map[string]any{"round": 2}, from.AddDate(0, 0, 6))
		// t-plan-4's j2, a round-4 filing inside the spark alone (Sep 10,
		// bucket 2): a name only a weekly bucket holds, not lost
		dcFiling(t, s, "t-plan-4", "sf/dc9", 2, ReplanRound, 135, "j2", 135, map[string]any{"round": 4}, from.AddDate(0, 0, -18))
		// t-plan-5, closed Oct 7: the canonical card row of its revision
		// — roundless, its own filing — beside the legacy revision-level
		// replan row of the same revision: separate identities, the card
		// row never suppresses the revision's, and the legacy origin
		// counts once while the filing answers the filings panels alone
		dcPlan(t, s, "t-plan-5", "sf/dc9", 205)
		dcFiling(t, s, "t-plan-5", "sf/dc9", 2, ReplanPerson, 142, "h1", 142, nil, from.AddDate(0, 0, 8))
		dcLegacyReplan(t, s, "t-plan-5", "sf/dc9", 205,
			map[string]any{"revision": 2, "origin": ReplanPerson, "round": 2}, from.AddDate(0, 0, 8))
		dcClosed(t, s, "t-plan-5", "sf/dc9", from.AddDate(0, 0, 9))
		// t-plan-6, closed Oct 7: its replan a legacy row alone, the
		// canonical replacement arrives only after the window and the
		// closure — later evidence must not rewrite the answers read
		// before it
		dcPlan(t, s, "t-plan-6", "sf/dc9", 206)
		dcLegacyReplan(t, s, "t-plan-6", "sf/dc9", 206,
			map[string]any{"revision": 3, "origin": ReplanCut, "round": 2}, from.AddDate(0, 0, 8))
		dcClosed(t, s, "t-plan-6", "sf/dc9", from.AddDate(0, 0, 9))
		// t-plan-7, closed Oct 9 with no round any row or revision names:
		// the plan keeps the unknown bucket, its roundless filing the
		// filings' own unknown name
		dcPlan(t, s, "t-plan-7", "sf/dc9", 207)
		dcFiling(t, s, "t-plan-7", "sf/dc9", 1, "", 143, "i1", 143, nil, from.AddDate(0, 0, 10))
		dcClosed(t, s, "t-plan-7", "sf/dc9", from.AddDate(0, 0, 11))
		// t-plan-8, the previous window's rounds-3 closure (Sep 18): a
		// name only the previous window and its week hold
		dcPlan(t, s, "t-plan-8", "sf/dc9", 208)
		dcFiling(t, s, "t-plan-8", "sf/dc9", 2, ReplanCut, 144, "j1", 144, map[string]any{"round": 3}, from.AddDate(0, 0, -12))
		dcClosed(t, s, "t-plan-8", "sf/dc9", from.AddDate(0, 0, -10))
		// t-plan-9, never closed: its revision 2 replan a legacy row
		// alone (Oct 9, a person's), the canonical replacement recorded
		// exactly at the window's exclusive end Oct 12 — outside the
		// cohort, so it suppresses nothing it cannot count: the person's
		// replan stays counted, and no closure ever reads the plan
		dcPlan(t, s, "t-plan-9", "sf/dc9", 209)
		dcLegacyReplan(t, s, "t-plan-9", "sf/dc9", 209,
			map[string]any{"revision": 2, "origin": ReplanPerson, "round": 2}, from.AddDate(0, 0, 11))
		dcFiling(t, s, "t-plan-9", "sf/dc9", 2, ReplanPerson, 0, "", 209,
			map[string]any{"kept": 1, "changed": 0, "dropped": 0, "added": 0, "by": "", "comment": ""}, to.AddDate(0, 0, 1))
		// t-plan-10, closed Oct 7 with its round evidence a legacy row
		// alone (round 2, Oct 5): a canonical revision-level row of the
		// same revision recorded under another repository's name (Oct 4,
		// before the closure) suppresses no round evidence here — the
		// plan keeps the two rounds its own row names, and the revision
		// counts once as a replan on its own repository's row, the cut
		// its detail names: the other repository's canonical row, though
		// its task's reach holds it, neither wins the revision's origin
		// nor suppresses the local one
		dcPlan(t, s, "t-plan-10", "sf/dc9", 210)
		dcLegacyReplan(t, s, "t-plan-10", "sf/dc9", 210,
			map[string]any{"revision": 2, "origin": ReplanCut, "round": 2}, from.AddDate(0, 0, 7))
		if id := dcRecord(t, s, Event{Repository: "sf/other9", TaskID: "t-plan-10", IssueNumber: 260,
			Kind: EventPlanReplanned, Actor: FactoryDecider, Via: ViaFactoryd,
			Detail:      map[string]any{"revision": 2, "origin": ReplanRound},
			IdentityKey: identityKey(EventPlanReplanned, "t-plan-10", "2")}, from.AddDate(0, 0, 6)); id == 0 {
			t.Fatal("the other repository's replacement recorded no row")
		}
		dcClosed(t, s, "t-plan-10", "sf/dc9", from.AddDate(0, 0, 9))
		// the other project's plan answers its report alone
		dcPlan(t, s, "t-plan-far", "sf/other9", 251)
		dcFiling(t, s, "t-plan-far", "sf/other9", 1, "", 151, "g1", 151, map[string]any{"round": 1}, from.AddDate(0, 0, 6))
		dcFiling(t, s, "t-plan-far", "sf/other9", 2, ReplanPerson, 0, "", 251,
			map[string]any{"kept": 1, "changed": 0, "dropped": 0, "added": 0, "by": "", "comment": ""}, from.AddDate(0, 0, 6))
		dcClosed(t, s, "t-plan-far", "sf/other9", from.AddDate(0, 0, 6))
		// the origins' own fixture, a third repository apart: one replan
		// of each origin in the window and one of each before it, the
		// unknown bucket the window's alone — op-person's revision files
		// two cards beside its legacy row and stays one replan, op-cut's
		// replan beside the console's request on the card it filed and
		// two retained cards no row holds, each still one replan
		dcPlan(t, s, "op-person", "sf/dc-origin", 431)
		dcPlanChild(t, s, "op-card-1", "op-person", "sf/dc-origin", 432)
		dcPlanChild(t, s, "op-card-2", "op-person", "sf/dc-origin", 433)
		dcLegacyReplan(t, s, "op-person", "sf/dc-origin", 431,
			map[string]any{"revision": 2, "origin": ReplanPerson, "by": "ana", "comment": "too wide"}, from.AddDate(0, 0, 2))
		dcFiling(t, s, "op-person", "sf/dc-origin", 2, ReplanPerson, 432, "oc1", 432, nil, from.AddDate(0, 0, 2))
		dcFiling(t, s, "op-person", "sf/dc-origin", 2, ReplanPerson, 433, "oc2", 433, nil, from.AddDate(0, 0, 2))
		dcPlan(t, s, "op-cut", "sf/dc-origin", 434)
		dcPlanChild(t, s, "op-card-3", "op-cut", "sf/dc-origin", 435)
		dcFiling(t, s, "op-cut", "sf/dc-origin", 2, ReplanCut, 0, "", 434,
			map[string]any{"kept": 2, "changed": 0, "dropped": 0, "added": 1, "by": "", "comment": ""}, from.AddDate(0, 0, 8))
		dcFiling(t, s, "op-cut", "sf/dc-origin", 2, ReplanCut, 435, "oc3", 435, nil, from.AddDate(0, 0, 8))
		dcRecord(t, s, Event{Repository: "sf/dc-origin", TaskID: "op-card-3", IssueNumber: 435,
			Kind: EventCardReplanned, Actor: "someone", Via: ViaFactoryd,
			Detail: map[string]any{"into": []int64{436}}}, from.AddDate(0, 0, 9))
		dcPlan(t, s, "op-round", "sf/dc-origin", 437)
		dcFiling(t, s, "op-round", "sf/dc-origin", 2, ReplanRound, 0, "", 437,
			map[string]any{"round": 2, "cards": []int64{}}, from.AddDate(0, 0, 10))
		dcPlan(t, s, "op-unknown", "sf/dc-origin", 438)
		dcLegacyReplan(t, s, "op-unknown", "sf/dc-origin", 438,
			map[string]any{"revision": 2, "kept": 1}, from.AddDate(0, 0, 11))
		// the previous window's own three: each origin's earliest
		dcPlan(t, s, "op-early-person", "sf/dc-origin", 441)
		dcLegacyReplan(t, s, "op-early-person", "sf/dc-origin", 441,
			map[string]any{"revision": 2, "origin": ReplanPerson, "by": "ana"}, from.AddDate(0, 0, -12))
		dcPlan(t, s, "op-early-cut", "sf/dc-origin", 442)
		dcFiling(t, s, "op-early-cut", "sf/dc-origin", 2, ReplanCut, 0, "", 442,
			map[string]any{"kept": 1, "changed": 0, "dropped": 0, "added": 1, "by": "", "comment": ""}, from.AddDate(0, 0, -6))
		dcPlan(t, s, "op-early-round", "sf/dc-origin", 443)
		dcFiling(t, s, "op-early-round", "sf/dc-origin", 2, ReplanRound, 0, "", 443,
			map[string]any{"round": 2, "cards": []int64{}}, from.AddDate(0, 0, -11))
	}
	r, err := s.ProjectMetrics(context.Background(), "sf/dc9", from, to)
	if err != nil {
		t.Fatal(err)
	}
	d := r.DivideConquer
	// t-plan-2 ran one round to its closure, t-plan-1, t-plan-5,
	// t-plan-6 and t-plan-10 two, t-plan-7 none any row names; t-plan-3's
	// duplicate close moved it nowhere, t-plan-4 and t-plan-9 never
	// closed, and the other repository's replacement moved no round
	if len(d.PlansByRounds) != 3 || d.PlansByRounds[0] != (NamedCount{Name: "1", Count: 1}) ||
		d.PlansByRounds[1] != (NamedCount{Name: "2", Count: 4}) ||
		d.PlansByRounds[2] != (NamedCount{Name: "unknown", Count: 1}) {
		t.Fatalf("plans by rounds = %+v, want one plan of one round, four of two and one unknown", d.PlansByRounds)
	}
	// the same names compared over the window, the equal window before
	// it and all seven weeks: explicit rounds fewest first, unknown
	// last, a name only the previous window or a week holds not lost —
	// round 3 is t-plan-8's closure alone (Sep 18, bucket 3) — and an
	// empty span a zero, never an absence
	wantPlanSpans := []NamedFigure{
		{Name: "1", Figure: Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}},
		{Name: "2", Figure: Figure{Value: 4, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 4}}},
		{Name: "3", Figure: Figure{Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 0}}},
		{Name: "unknown", Figure: Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}},
	}
	if !slices.Equal(d.PlansByRoundsSpans, wantPlanSpans) {
		t.Fatalf("plans by rounds spans = %+v, want %+v", d.PlansByRoundsSpans, wantPlanSpans)
	}
	// four first-round filings of the window (a1, a2, b1, b2 — b1's
	// re-file never counted, e6's original is the previous window's
	// own), four of the previous (e1, e2, e6, f1); three later ones of
	// the window (e4, e5, f2), two of the previous (e3, j1); the spark
	// holds the weeks' filings too, j2's round 4 among them (bucket 2)
	if d.CardsFirstRound != (Figure{Value: 4, Prev: 4, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 4, 0, 4, 0}}) {
		t.Fatalf("cards first round = %+v, want four of the window's filings and four before it", d.CardsFirstRound)
	}
	if d.CardsLaterRounds != (Figure{Value: 3, Prev: 2, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 1, 2, 0, 2, 1}}) {
		t.Fatalf("cards later rounds = %+v, want three of the window's filings and two before it", d.CardsLaterRounds)
	}
	// each round its own named comparison, the totals above agreeing
	// with it name for name: e6 contributes only at its original
	// instant and round (the previous window's, bucket 3), the refiled
	// round 2 nowhere; j2's round 4 is a week's alone (bucket 2); the
	// roundless h1 and i1 keep the explicit unknown bucket
	wantCardsByRound := []NamedFigure{
		{Name: "1", Figure: Figure{Value: 4, Prev: 4, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 4, 0, 4, 0}}},
		{Name: "2", Figure: Figure{Value: 3, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 2, 1}}},
		{Name: "3", Figure: Figure{Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 0}}},
		{Name: "4", Figure: Figure{Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 1, 0, 0, 0, 0}}},
		{Name: "unknown", Figure: Figure{Value: 2, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 2}}},
	}
	if !slices.Equal(d.CardsByRound, wantCardsByRound) {
		t.Fatalf("cards by round = %+v, want %+v", d.CardsByRound, wantCardsByRound)
	}
	byRound := map[string]Figure{}
	roundWeeks := map[string][7]float64{}
	for _, nf := range d.CardsByRound {
		byRound[nf.Name] = nf.Figure
		roundWeeks[nf.Name] = nf.Figure.Weeks
	}
	if byRound["1"] != d.CardsFirstRound {
		t.Fatalf("round 1 %+v disagrees with the first-round total %+v", byRound["1"], d.CardsFirstRound)
	}
	// Value and Weeks agree: each round's spark total is its window and
	// previous-window counts plus nothing else — e6's refile counted
	// anywhere would lift round 1's total past its eight originals
	totals := metricNamedWeeksTotals(roundWeeks)
	if totals["1"] != 8 || totals["2"] != 4 || totals["unknown"] != 2 {
		t.Fatalf("round totals = %+v, want round 1's eight, round 2's four and the two unknown", totals)
	}
	// three replans by a person (t-plan-1's canonical row, t-plan-5's
	// legacy one its card row never suppresses, t-plan-9's legacy one the
	// replacement at the window's own end cannot), three by a cut
	// (t-plan-3's multi-card revision once, t-plan-6's legacy row,
	// t-plan-10's own row the other repository's canonical neither wins
	// nor suppresses), one by a round (t-plan-4's) and the lost origin of
	// t-plan-4's revision 3 its own unknown bucket — the legacy double of
	// revision 2 dropped
	if len(d.ReplansByOrigin) != 4 || d.ReplansByOrigin[0] != (NamedCount{Name: ReplanCut, Count: 3}) ||
		d.ReplansByOrigin[1] != (NamedCount{Name: ReplanPerson, Count: 3}) ||
		d.ReplansByOrigin[2] != (NamedCount{Name: ReplanRound, Count: 1}) ||
		d.ReplansByOrigin[3] != (NamedCount{Name: "unknown", Count: 1}) {
		t.Fatalf("replans by origin = %+v, want cut and person thrice each, round once and one unknown", d.ReplansByOrigin)
	}
	// the same origins compared over the window, the equal window before
	// it and all seven weeks: alphabetical, the unknown bucket among
	// them, a measured zero wherever an origin holds no replan, and
	// round the previous window's alone (Sep 16, bucket 3) beside the
	// window's own (Oct 6, bucket 6)
	wantReplanSpans := []NamedFigure{
		{Name: ReplanCut, Figure: Figure{Value: 3, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 2}}},
		{Name: ReplanPerson, Figure: Figure{Value: 3, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 1, 2}}},
		{Name: ReplanRound, Figure: Figure{Value: 1, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 1}}},
		{Name: "unknown", Figure: Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}},
	}
	if !slices.Equal(d.ReplansByOriginSpans, wantReplanSpans) {
		t.Fatalf("replans by origin spans = %+v, want %+v", d.ReplansByOriginSpans, wantReplanSpans)
	}
	// the same definitions answer the previous window: its closures
	// (t-plan-3 Sep 20, t-plan-8 Sep 18) ran two rounds and three — its
	// replans are the round's alone — and its filings carry e6's
	// original among the first and j1 among the later
	prevFrom, prevTo := from.AddDate(0, 0, -14), to.AddDate(0, 0, -14)
	rp, err := s.ProjectMetrics(context.Background(), "sf/dc9", prevFrom, prevTo)
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.DivideConquer.PlansByRounds) != 2 || rp.DivideConquer.PlansByRounds[0] != (NamedCount{Name: "2", Count: 1}) ||
		rp.DivideConquer.PlansByRounds[1] != (NamedCount{Name: "3", Count: 1}) {
		t.Fatalf("previous plans by rounds = %+v, want t-plan-3's two rounds and t-plan-8's three", rp.DivideConquer.PlansByRounds)
	}
	if len(rp.DivideConquer.ReplansByOrigin) != 1 || rp.DivideConquer.ReplansByOrigin[0] != (NamedCount{Name: ReplanRound, Count: 1}) {
		t.Fatalf("previous replans by origin = %+v, want the round replan alone", rp.DivideConquer.ReplansByOrigin)
	}
	if rp.DivideConquer.CardsFirstRound.Value != 4 || rp.DivideConquer.CardsLaterRounds.Value != 2 {
		t.Fatalf("previous filings = %+v / %+v, want four first and two later",
			rp.DivideConquer.CardsFirstRound, rp.DivideConquer.CardsLaterRounds)
	}
	if rp.DivideConquer.CardsFirstRound.Value != d.CardsFirstRound.Prev || rp.DivideConquer.CardsLaterRounds.Value != d.CardsLaterRounds.Prev {
		t.Fatalf("previous filings %+v / %+v disagree with the report's own prev %v / %v",
			rp.DivideConquer.CardsFirstRound, rp.DivideConquer.CardsLaterRounds, d.CardsFirstRound.Prev, d.CardsLaterRounds.Prev)
	}
	// the origins' own fixture, a repository apart: one replan of each
	// origin in the window and one of each before it, the unknown bucket
	// the window's alone — the several-cards revision one replan, the
	// console's request and the retained cards adding nothing — so each
	// origin's name answers one beside an equal previous, and every week
	// carries its own instant alone
	rOrigin, err := s.ProjectMetrics(context.Background(), "sf/dc-origin", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got := rOrigin.DivideConquer.ReplansByOrigin; len(got) != 4 ||
		got[0] != (NamedCount{Name: ReplanCut, Count: 1}) || got[1] != (NamedCount{Name: ReplanPerson, Count: 1}) ||
		got[2] != (NamedCount{Name: ReplanRound, Count: 1}) || got[3] != (NamedCount{Name: "unknown", Count: 1}) {
		t.Fatalf("dc-origin replans by origin = %+v, want each origin once and the unknown separate", got)
	}
	wantOriginSpans := []NamedFigure{
		{Name: ReplanCut, Figure: Figure{Value: 1, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 1, 0, 1}}},
		{Name: ReplanPerson, Figure: Figure{Value: 1, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 1, 0}}},
		{Name: ReplanRound, Figure: Figure{Value: 1, Prev: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 1, 0, 0, 1}}},
		{Name: "unknown", Figure: Figure{Value: 1, Has: true, PrevHas: true, Weeks: [7]float64{0, 0, 0, 0, 0, 0, 1}}},
	}
	if !slices.Equal(rOrigin.DivideConquer.ReplansByOriginSpans, wantOriginSpans) {
		t.Fatalf("dc-origin replans by origin spans = %+v, want %+v", rOrigin.DivideConquer.ReplansByOriginSpans, wantOriginSpans)
	}
	// the previous window's own report answers the equal previous: each
	// origin one replan there, the unknown a measured zero alone
	oprev, err := s.ProjectMetrics(context.Background(), "sf/dc-origin", prevFrom, prevTo)
	if err != nil {
		t.Fatal(err)
	}
	if got := oprev.DivideConquer.ReplansByOrigin; len(got) != 3 ||
		got[0] != (NamedCount{Name: ReplanCut, Count: 1}) || got[1] != (NamedCount{Name: ReplanPerson, Count: 1}) ||
		got[2] != (NamedCount{Name: ReplanRound, Count: 1}) {
		t.Fatalf("dc-origin previous replans by origin = %+v, want each origin once", got)
	}
	for _, nf := range rOrigin.DivideConquer.ReplansByOriginSpans {
		if nf.Name == "unknown" {
			if nf.Figure.Prev != 0 {
				t.Fatalf("dc-origin unknown previous = %v, want the measured zero", nf.Figure.Prev)
			}
			continue
		}
		if nf.Figure.Value != 1 || nf.Figure.Prev != 1 {
			t.Fatalf("dc-origin %s = %+v, want one replan beside an equal previous", nf.Name, nf.Figure)
		}
	}
	// and one week of the spark, [Sep 29, Oct 6): t-plan-1 closes
	// inside it at two rounds, t-plan-2's Oct 6 closure stays outside;
	// the filings lose f2's Oct 6, the replans the round replan's
	rw, err := s.ProjectMetrics(context.Background(), "sf/dc9", from.AddDate(0, 0, 1), from.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(rw.DivideConquer.PlansByRounds) != 1 || rw.DivideConquer.PlansByRounds[0] != (NamedCount{Name: "2", Count: 1}) {
		t.Fatalf("weekly plans by rounds = %+v, want t-plan-1 alone", rw.DivideConquer.PlansByRounds)
	}
	if rw.DivideConquer.CardsFirstRound.Value != 4 || rw.DivideConquer.CardsLaterRounds.Value != 2 {
		t.Fatalf("weekly filings = %+v / %+v, want four first and two later",
			rw.DivideConquer.CardsFirstRound, rw.DivideConquer.CardsLaterRounds)
	}
	// the week's replans: the cut twice — t-plan-3's Oct 3 row and
	// t-plan-10's own Oct 5 one, the other repository's Oct 4 canonical
	// row winning nothing here — the person, the unknown above, the
	// round's Oct 6 row outside
	if got := rw.DivideConquer.ReplansByOrigin; len(got) != 3 || got[0] != (NamedCount{Name: ReplanCut, Count: 2}) ||
		got[1] != (NamedCount{Name: ReplanPerson, Count: 1}) || got[2] != (NamedCount{Name: "unknown", Count: 1}) {
		t.Fatalf("weekly replans by origin = %+v, want the cut twice, the person and the unknown once each", got)
	}
	// the other project's plan answers its report alone
	ro, err := s.ProjectMetrics(context.Background(), "sf/other9", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(ro.DivideConquer.PlansByRounds) != 1 || ro.DivideConquer.PlansByRounds[0] != (NamedCount{Name: "1", Count: 1}) {
		t.Fatalf("sf/other9 plans by rounds = %+v, want its own one-round plan alone", ro.DivideConquer.PlansByRounds)
	}
	if ro.DivideConquer.CardsFirstRound.Value != 1 || len(ro.DivideConquer.ReplansByOrigin) != 1 ||
		ro.DivideConquer.ReplansByOrigin[0] != (NamedCount{Name: ReplanPerson, Count: 1}) {
		t.Fatalf("sf/other9 filings %v replans %+v, want its own alone",
			ro.DivideConquer.CardsFirstRound.Value, ro.DivideConquer.ReplansByOrigin)
	}
	// the origins' own later evidence: a duplicate observation of
	// op-early-person's revision lands in the window naming another
	// origin, its canonical replacement arrives after the previous
	// window's own end — as does another repository's row on
	// op-unknown's revision — and none of it refiles anything: the
	// revision stays the previous window's replan wherever the panels
	// read it, its origin the one the older spans named
	dcLegacyReplan(t, s, "op-early-person", "sf/dc-origin", 441,
		map[string]any{"revision": 2, "origin": ReplanCut}, from.AddDate(0, 0, 12))
	dcFiling(t, s, "op-early-person", "sf/dc-origin", 2, ReplanRound, 0, "", 441,
		map[string]any{"round": 9, "cards": []int64{}}, from.AddDate(0, 0, 12))
	if id := dcRecord(t, s, Event{Repository: "sf/other-origin", TaskID: "op-unknown", IssueNumber: 438,
		Kind: EventPlanReplanned, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail:      map[string]any{"revision": 2, "origin": ReplanPerson},
		IdentityKey: identityKey(EventPlanReplanned, "op-unknown", "2")}, from.AddDate(0, 0, 13)); id == 0 {
		t.Fatal("the other repository's replacement recorded no row")
	}
	// the canonical replacement of t-plan-6's revision 3 arrives after
	// the window and the closure, naming a round the plan never ran:
	// no earlier answer moves — the revision-level origin stays counted
	// once, the closed plan keeps the rounds its closure saw, the
	// filings and their rounds stand, and the later row adds nothing
	dcFiling(t, s, "t-plan-6", "sf/dc9", 3, ReplanRound, 0, "", 206,
		map[string]any{"round": 9, "cards": []int64{}}, to.AddDate(0, 0, 3))
	again, err := s.ProjectMetrics(context.Background(), "sf/dc9", from, to)
	if err != nil {
		t.Fatal(err)
	}
	a := again.DivideConquer
	if !slices.Equal(a.PlansByRounds, d.PlansByRounds) || !slices.Equal(a.PlansByRoundsSpans, d.PlansByRoundsSpans) ||
		!slices.Equal(a.ReplansByOrigin, d.ReplansByOrigin) || !slices.Equal(a.ReplansByOriginSpans, d.ReplansByOriginSpans) {
		t.Fatalf("after the late replacement: plans %+v spans %+v replans %+v %+v, want the older answers %+v %+v %+v %+v",
			a.PlansByRounds, a.PlansByRoundsSpans, a.ReplansByOrigin, a.ReplansByOriginSpans,
			d.PlansByRounds, d.PlansByRoundsSpans, d.ReplansByOrigin, d.ReplansByOriginSpans)
	}
	if a.CardsFirstRound != d.CardsFirstRound || a.CardsLaterRounds != d.CardsLaterRounds ||
		!slices.Equal(a.CardsByRound, d.CardsByRound) {
		t.Fatalf("after the late replacement: first %+v later %+v by round %+v, want the older answers %+v %+v %+v",
			a.CardsFirstRound, a.CardsLaterRounds, a.CardsByRound, d.CardsFirstRound, d.CardsLaterRounds, d.CardsByRound)
	}
	// and the origins' answers stand after all the later evidence: the
	// window's counts and spans identical — the re-observed revision
	// moved nowhere, the cross-repository row won nothing — and the
	// previous windows' own reports answer identically too, their
	// origins and rounds never rewritten by what arrived after their
	// ends
	againOrigin, err := s.ProjectMetrics(context.Background(), "sf/dc-origin", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(againOrigin.DivideConquer.ReplansByOrigin, rOrigin.DivideConquer.ReplansByOrigin) ||
		!slices.Equal(againOrigin.DivideConquer.ReplansByOriginSpans, rOrigin.DivideConquer.ReplansByOriginSpans) {
		t.Fatalf("after the later evidence: dc-origin replans %+v spans %+v, want the older answers %+v %+v",
			againOrigin.DivideConquer.ReplansByOrigin, againOrigin.DivideConquer.ReplansByOriginSpans,
			rOrigin.DivideConquer.ReplansByOrigin, rOrigin.DivideConquer.ReplansByOriginSpans)
	}
	againPrev, err := s.ProjectMetrics(context.Background(), "sf/dc-origin", prevFrom, prevTo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(againPrev.DivideConquer.ReplansByOrigin, oprev.DivideConquer.ReplansByOrigin) ||
		!slices.Equal(againPrev.DivideConquer.ReplansByOriginSpans, oprev.DivideConquer.ReplansByOriginSpans) {
		t.Fatalf("after the later evidence: dc-origin previous replans %+v spans %+v, want the older answers %+v %+v",
			againPrev.DivideConquer.ReplansByOrigin, againPrev.DivideConquer.ReplansByOriginSpans,
			oprev.DivideConquer.ReplansByOrigin, oprev.DivideConquer.ReplansByOriginSpans)
	}
	againPP, err := s.ProjectMetrics(context.Background(), "sf/dc9", prevFrom, prevTo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(againPP.DivideConquer.PlansByRounds, rp.DivideConquer.PlansByRounds) ||
		!slices.Equal(againPP.DivideConquer.ReplansByOrigin, rp.DivideConquer.ReplansByOrigin) ||
		!slices.Equal(againPP.DivideConquer.ReplansByOriginSpans, rp.DivideConquer.ReplansByOriginSpans) {
		t.Fatalf("after the later evidence: sf/dc9 previous rounds %+v replans %+v spans %+v, want the older answers %+v %+v %+v",
			againPP.DivideConquer.PlansByRounds, againPP.DivideConquer.ReplansByOrigin, againPP.DivideConquer.ReplansByOriginSpans,
			rp.DivideConquer.PlansByRounds, rp.DivideConquer.ReplansByOrigin, rp.DivideConquer.ReplansByOriginSpans)
	}
}
