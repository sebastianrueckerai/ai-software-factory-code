package factoryd

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// drawStore is an empty store with agent card id of budget eur on sf/r,
// issue 9.
func drawStore(t *testing.T, id string, eur float64) *PGStore {
	t.Helper()
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "draws"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, events CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	drawCard(t, s, id, 9, eur)
	return s
}

func drawCard(t *testing.T, s *PGStore, id string, issue int64, eur float64) {
	t.Helper()
	if err := s.CreateTask(context.Background(), Task{ID: id, Kind: "agent", BudgetEUR: eur, Status: "running", Repository: "sf/r", IssueNumber: issue, Track: "direct"}); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
}

func recordAll(t *testing.T, s *PGStore, events ...Event) {
	t.Helper()
	for _, e := range events {
		if _, err := s.Record(context.Background(), e); err != nil {
			t.Fatalf("Record %+v: %v", e, err)
		}
	}
}

func drawAt(d string) time.Time {
	at, err := time.Parse(time.RFC3339, d)
	if err != nil {
		panic(err)
	}
	return at
}

// The month's pool is the sum of the repository's draw rows in the UTC
// month: 1 session, 1800 s and 0 EUR of the extend plus 2.5 EUR of the
// fund. The fund with no pool key, last month's draw, next month's and
// another repository's are left out.
func TestPoolDrawnSumsTheMonthsPoolRows(t *testing.T) {
	ctx := context.Background()
	s := drawStore(t, "t-p", 5)
	pooled := func(at, repository, kind string, detail map[string]any) Event {
		return Event{At: drawAt(at), Repository: repository, TaskID: "t-p", Kind: kind, Actor: "sebastian", Via: ViaConsole, Detail: detail}
	}
	recordAll(t, s,
		pooled("2026-10-01T00:00:00Z", "sf/r", EventCardExtended, map[string]any{"sessions": 1, "agent_time_s": 1800, "amount_eur": 0, "pool": true}),
		pooled("2026-10-31T23:59:59Z", "sf/r", EventCardFunded, map[string]any{"amount_eur": 2.5, "pool": true}),
		pooled("2026-10-15T12:00:00Z", "sf/r", EventCardFunded, map[string]any{"amount_eur": 9}),
		pooled("2026-09-30T23:59:59Z", "sf/r", EventCardFunded, map[string]any{"amount_eur": 4, "pool": true}),
		pooled("2026-11-01T00:00:00Z", "sf/r", EventCardExtended, map[string]any{"sessions": 3, "agent_time_s": 60, "amount_eur": 1, "pool": true}),
		pooled("2026-10-15T12:00:00Z", "sf/other", EventCardFunded, map[string]any{"amount_eur": 7, "pool": true}),
	)
	got, err := s.PoolDrawn(ctx, "sf/r", drawAt("2026-10-20T08:00:00Z"))
	if want := (policy.Draw{Sessions: 1, AgentTime: 30 * time.Minute, EUR: 2.5}); err != nil || got != want {
		t.Fatalf("PoolDrawn = %+v, %v; want %+v", got, err, want)
	}
	if got, err := s.PoolDrawn(ctx, "sf/none", drawAt("2026-10-20T08:00:00Z")); err != nil || !got.IsZero() {
		t.Fatalf("PoolDrawn of a repository with none = %+v, %v; want zero", got, err)
	}
}

// A card's life draws are its pool rows of rungs 0 and 1: 1 session, 600
// s and 0.5 + 1 EUR. Rungs 2 and 3, a row with no pool key and another
// card's draw are left out.
func TestCardDrawnCountsRungsZeroAndOne(t *testing.T) {
	ctx := context.Background()
	s := drawStore(t, "t-d", 5)
	drawCard(t, s, "t-o", 10, 5)
	row := func(task, kind string, detail map[string]any) Event {
		return Event{Repository: "sf/r", TaskID: task, Kind: kind, Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}
	}
	recordAll(t, s,
		row("t-d", EventCardExtended, map[string]any{"sessions": 1, "agent_time_s": 600, "amount_eur": 0.5, "pool": true, "rung": 0}),
		row("t-d", EventCardFunded, map[string]any{"amount_eur": 1, "pool": true, "rung": 1}),
		row("t-d", EventCardExtended, map[string]any{"sessions": 2, "agent_time_s": 1200, "amount_eur": 0, "pool": true, "rung": 2}),
		row("t-d", EventCardFunded, map[string]any{"amount_eur": 4, "pool": true, "rung": 3}),
		row("t-d", EventCardFunded, map[string]any{"amount_eur": 8, "rung": 0}),
		row("t-o", EventCardFunded, map[string]any{"amount_eur": 16, "pool": true, "rung": 0}),
	)
	got, err := s.CardDrawn(ctx, "t-d")
	if want := (policy.Draw{Sessions: 1, AgentTime: 10 * time.Minute, EUR: 1.5}); err != nil || got != want {
		t.Fatalf("CardDrawn = %+v, %v; want %+v", got, err, want)
	}
	if got, err := s.CardDrawn(ctx, "t-none"); err != nil || !got.IsZero() {
		t.Fatalf("CardDrawn of a card with none = %+v, %v; want zero", got, err)
	}
}

// An extend raises the sessions, the agent time, the line bound and the
// budget in one write, and its card_extended row carries each part: 20
// minutes is 1200 s. The row is a pool draw, so the month's pool reads it.
func TestExtendCardRaisesTimeAndWritesTheDraw(t *testing.T) {
	ctx := context.Background()
	s := drawStore(t, "t-x", 2)
	e := Event{Repository: "sf/r", TaskID: "t-x", IssueNumber: 9, Kind: EventCardExtended, Actor: "sebastian", Via: ViaConsole,
		Detail: map[string]any{"pool": true, "rung": 3}}
	from, to, err := s.ExtendCard(ctx, "t-x", policy.Draw{Sessions: 1, AgentTime: 20 * time.Minute, EUR: 0.5}, 100, e)
	if err != nil || from != 2 || to != 2.5 {
		t.Fatalf("ExtendCard = (%v, %v, %v), want (2, 2.5, nil)", from, to, err)
	}
	card, _, err := s.Task(ctx, "t-x")
	if err != nil || card.Usage.RaisedSessions != 1 || card.Usage.RaisedTime != 20*time.Minute || card.Usage.ExtraLines != 100 || card.BudgetEUR != 2.5 {
		t.Fatalf("the card = %+v, %v; want 1 session, 20m, 100 lines raised and 2.5 EUR", card.Usage, err)
	}
	if card.Usage.TimeAdded() != 20*time.Minute {
		t.Fatalf("TimeAdded = %v, want 20m", card.Usage.TimeAdded())
	}
	rows, err := s.TaskEvents(ctx, "t-x", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the rows = %+v, %v; want the one card_extended row", rows, err)
	}
	d := rows[0].Detail
	if rows[0].Kind != EventCardExtended || d["sessions"] != 1.0 || d["agent_time_s"] != 1200.0 || d["lines"] != 100.0 ||
		d["amount_eur"] != 0.5 || d["from_eur"] != 2.0 || d["to_eur"] != 2.5 || d["pool"] != true || d["rung"] != 3.0 {
		t.Fatalf("the row = %+v, want sessions 1, agent_time_s 1200, lines 100, amount_eur 0.5 from 2 to 2.5, pool true, rung 3", rows[0])
	}
	if got, err := s.PoolDrawn(ctx, "sf/r", time.Now()); err != nil || got != (policy.Draw{Sessions: 1, AgentTime: 20 * time.Minute, EUR: 0.5}) {
		t.Fatalf("PoolDrawn = %+v, %v; want the extend's draw", got, err)
	}
	if _, _, err := s.ExtendCard(ctx, "nope", policy.Draw{Sessions: 1}, 0, e); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ExtendCard of no task = %v, want ErrNotFound", err)
	}
}

// A card's ceiling, once set, bounds every fund: at 5 EUR under a ceiling
// of 6, a fund of 2 is refused with the budget it met and one of 1 lands
// on 6. SetCeiling writes a card_funded row that moves no money, and
// refuses a ceiling below a cent.
func TestFundCardStopsAtTheCardsCeiling(t *testing.T) {
	ctx := context.Background()
	s := drawStore(t, "t-f", 5)
	if err := s.SetCeiling(ctx, "t-f", 6, Event{Actor: "sebastian", Via: ViaConsole, Detail: map[string]any{"rung": 3}}); err != nil {
		t.Fatalf("SetCeiling: %v", err)
	}
	card, _, err := s.Task(ctx, "t-f")
	if err != nil || card.CeilingEUR != 6 || card.BudgetEUR != 5 {
		t.Fatalf("the card = ceiling %v, budget %v, %v; want 6 and 5", card.CeilingEUR, card.BudgetEUR, err)
	}
	rows, err := s.TaskEvents(ctx, "t-f", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the rows = %+v, %v; want SetCeiling's one row", rows, err)
	}
	if r := rows[0]; r.Kind != EventCardFunded || r.Repository != "sf/r" || r.IssueNumber != 9 || r.Actor != "sebastian" ||
		r.Detail["ceiling_eur"] != 6.0 || r.Detail["amount_eur"] != 0.0 || r.Detail["from_eur"] != 5.0 || r.Detail["to_eur"] != 5.0 || r.Detail["rung"] != 3.0 {
		t.Fatalf("SetCeiling's row = %+v, want card_funded on #9 of 0 EUR from 5 to 5 with ceiling_eur 6 and the caller's rung", r)
	}
	fund := Event{Repository: "sf/r", TaskID: "t-f", IssueNumber: 9, Kind: EventCardFunded, Actor: "sebastian", Via: ViaConsole}
	var capped *CardCapError
	if _, _, err := s.FundCard(ctx, "t-f", 2, 0, fund); !errors.As(err, &capped) || capped.Budget != 5 {
		t.Fatalf("FundCard 2 past the ceiling = %v, want CardCapError at 5", err)
	}
	if from, to, err := s.FundCard(ctx, "t-f", 1, 0, fund); err != nil || from != 5 || to != 6 {
		t.Fatalf("FundCard 1 = (%v, %v, %v), want (5, 6, nil)", from, to, err)
	}
	if err := s.SetCeiling(ctx, "nope", 6, Event{Actor: "sebastian", Via: ViaConsole}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCeiling of no task = %v, want ErrNotFound", err)
	}
	for _, eur := range []float64{0, -1, 0.004, math.NaN(), math.Inf(1)} {
		if err := s.SetCeiling(ctx, "t-f", eur, Event{Actor: "sebastian", Via: ViaConsole}); err == nil || !strings.Contains(err.Error(), "at least 0.01 EUR") {
			t.Fatalf("SetCeiling %v = %v, want the refusal naming the least ceiling", eur, err)
		}
	}
	if card, _, _ := s.Task(ctx, "t-f"); card.CeilingEUR != 6 {
		t.Fatalf("ceiling %v after the refused ceilings, want 6", card.CeilingEUR)
	}

	// the caller's ceiling is the policy's: it bounds a card with no
	// ceiling of its own, and the card's own ceiling wins over it
	drawCard(t, s, "t-g", 10, 2)
	fund.TaskID, fund.IssueNumber = "t-g", 10
	if _, _, err := s.FundCard(ctx, "t-g", 1.5, 3, fund); !errors.As(err, &capped) || capped.Budget != 2 {
		t.Fatalf("a fund of 1.5 on 2 under the policy's 3 = %v, want CardCapError at 2", err)
	}
	if _, to, err := s.FundCard(ctx, "t-g", 1, 3, fund); err != nil || to != 3 {
		t.Fatalf("a fund of 1 on 2 under the policy's 3 = %v, %v; want 3", to, err)
	}
	if err := s.SetCeiling(ctx, "t-g", 8, Event{Actor: "sebastian", Via: ViaConsole}); err != nil {
		t.Fatal(err)
	}
	if _, to, err := s.FundCard(ctx, "t-g", 5, 3, fund); err != nil || to != 8 {
		t.Fatalf("a fund of 5 on 3 under its own ceiling of 8 = %v, %v; want 8", to, err)
	}
}

// The size gate's budget move is a card_funded row by factoryd: 5 to 10
// EUR records 5. A size write that leaves the budget records none, and
// the digest finds the moved budget explained.
func TestSizeGateBudgetMoveRecordsAFund(t *testing.T) {
	ctx := context.Background()
	s := drawStore(t, "t-s", 5)
	recordAll(t, s, Event{Repository: "sf/r", TaskID: "t-s", IssueNumber: 9, Kind: EventCardCreated, Actor: FactoryDecider, Via: ViaFactoryd,
		Detail: map[string]any{"budget_eur": 5.0}})
	if err := s.SetTaskSizeBudget(ctx, "t-s", "large", 500, 10); err != nil {
		t.Fatalf("SetTaskSizeBudget: %v", err)
	}
	rows, err := s.TaskEvents(ctx, "t-s", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("the rows = %+v, %v; want card_created and the size gate's card_funded", rows, err)
	}
	if r := rows[0]; r.Kind != EventCardFunded || r.Actor != FactoryDecider || r.Via != ViaFactoryd || r.Repository != "sf/r" || r.IssueNumber != 9 ||
		r.Detail["size"] != "large" || r.Detail["by"] != "size gate" || r.Detail["amount_eur"] != 5.0 || r.Detail["from_eur"] != 5.0 || r.Detail["to_eur"] != 10.0 {
		t.Fatalf("the size gate's row = %+v, want card_funded by factoryd of 5 EUR from 5 to 10, size large, by size gate", r)
	}
	r := &Runner{store: s, objects: &fakeObjectStore{}, cfg: testRunnerConfig()}
	d, err := r.CardDigest(ctx, "t-s")
	if err != nil {
		t.Fatalf("CardDigest: %v", err)
	}
	if slices.ContainsFunc(d.Flags, func(f DigestFlag) bool { return f.Name == "budget_moved_no_event" }) {
		t.Fatalf("Flags = %v, want no budget_moved_no_event", d.Flags)
	}

	drawCard(t, s, "t-u", 10, 5)
	if err := s.SetTaskSizeBudget(ctx, "t-u", "medium", 200, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTaskSizeBudget(ctx, "t-u", "large", 500, 5); err != nil {
		t.Fatal(err)
	}
	if card, _, _ := s.Task(ctx, "t-u"); card.Size != "large" || card.BudgetEUR != 5 {
		t.Fatalf("the card = size %q, budget %v; want large and 5", card.Size, card.BudgetEUR)
	}
	if rows, err := s.TaskEvents(ctx, "t-u", 10); err != nil || len(rows) != 0 {
		t.Fatalf("the rows = %+v, %v; want none for a budget that did not move", rows, err)
	}
}
