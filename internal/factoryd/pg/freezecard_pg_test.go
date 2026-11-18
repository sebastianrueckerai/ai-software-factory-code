package pg

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// SetFreezeCursor persists the watcher's cursor against PostgreSQL, the
// read gives it back, and a fresh watcher's FreezeEvents reads forward
// only from it: no replay of the events behind the cursor, no duplicate
// re-arm comments and asks after a restart. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestFreezeCardCursorPersistsPG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_freezecard"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	if _, err := s.DB().ExecContext(ctx, `TRUNCATE events CASCADE; UPDATE freeze_cursor SET cursor = 0 WHERE id = 1`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var ids []int64
	for i, kind := range []string{"class_frozen", "class_frozen", "class_resumed"} {
		var id int64
		if err := s.DB().QueryRowContext(ctx, `INSERT INTO events (at, repository, task_id, kind, actor, via, detail)
			VALUES ($1, 'sf/acme', '', $2, 'gateway', 'gateway', $3) RETURNING id`,
			base.Add(time.Duration(i)*time.Minute), kind,
			`{"class":"big","reset":"`+base.Add(4*time.Hour).Format(time.RFC3339)+`","words":"free quota exhausted"}`).Scan(&id); err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
		ids = append(ids, id)
	}
	if err := s.SetFreezeCursor(ctx, ids[1]); err != nil {
		t.Fatalf("SetFreezeCursor: %v", err)
	}
	var stored int64
	if err := s.DB().QueryRowContext(ctx, `SELECT cursor FROM freeze_cursor WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != ids[1] {
		t.Fatalf("the stored cursor is %d, want %d: SetFreezeCursor did not persist", stored, ids[1])
	}
	// at the cursor the class's newest row is its second class_frozen
	if standing, err := s.StandingFreezes(ctx); err != nil || len(standing) != 1 || standing[0].ID != ids[1] || standing[0].Class != "big" ||
		standing[0].Repository != "sf/acme" || standing[0].Reset != base.Add(4*time.Hour).Format(time.RFC3339) || standing[0].Words != "free quota exhausted" {
		t.Fatalf("StandingFreezes at the second arm = %+v, %v; want that arm alone", standing, err)
	}
	// a fresh watcher reads forward only from the persisted cursor
	events, cursor, err := s.FreezeEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != ids[2] {
		t.Fatalf("cursor = %d, want %d", cursor, ids[2])
	}
	if len(events) != 1 || events[0].Kind != "class_resumed" || events[0].Class != "big" {
		t.Fatalf("events = %+v, want only the class_resumed row behind the cursor", events)
	}
	if err := s.SetFreezeCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	events, cursor, err = s.FreezeEvents(ctx)
	if err != nil || cursor != ids[2] || len(events) != 0 {
		t.Fatalf("events = %+v, cursor %d, err %v; want a quiet read at the cursor", events, cursor, err)
	}
	// past its class_resumed the class stands frozen no more; a class whose
	// newest row at the cursor is its class_frozen does
	if standing, err := s.StandingFreezes(ctx); err != nil || len(standing) != 0 {
		t.Fatalf("StandingFreezes past the resume = %+v, %v; want none", standing, err)
	}
	var small int64
	if err := s.DB().QueryRowContext(ctx, `INSERT INTO events (at, repository, task_id, kind, actor, via, detail)
		VALUES ($1, 'sf/other', '', 'class_frozen', 'gateway', 'gateway', '{"class":"small","reset":"2026-11-22T12:00:00Z","words":"daily limit"}') RETURNING id`,
		base.Add(time.Hour)).Scan(&small); err != nil {
		t.Fatal(err)
	}
	if standing, err := s.StandingFreezes(ctx); err != nil || len(standing) != 0 {
		t.Fatalf("StandingFreezes with small's arm past the cursor = %+v, %v; want none", standing, err)
	}
	if err := s.SetFreezeCursor(ctx, small); err != nil {
		t.Fatal(err)
	}
	if standing, err := s.StandingFreezes(ctx); err != nil || len(standing) != 1 || standing[0].ID != small || standing[0].Class != "small" || standing[0].Repository != "sf/other" {
		t.Fatalf("StandingFreezes at small's arm = %+v, %v; want that arm alone", standing, err)
	}
}

// ListFrozenCards matches the class against a card's classes, never its
// model alias, and lists the running and queued cards of every repository
// in id order; which of a card's classes the hold reads is the watcher's
// policy rule, not the read's. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestFrozenCardsMatchTheClassPG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_frozencards"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	for _, tk := range []Task{
		{ID: "f-run", Status: "running", Repository: "sf/acme", RequiredCapabilities: []string{"coder"}},
		{ID: "f-other", Status: "running", Repository: "sf/other", RequiredCapabilities: []string{"coder"}},
		{ID: "f-queued", Status: "queued", Repository: "sf/acme", RequiredCapabilities: []string{"coder"}},
		{ID: "f-two", Status: "running", Repository: "sf/acme", RequiredCapabilities: []string{"analyzer", "coder"}},
		{ID: "f-review", Status: "in-review", Repository: "sf/acme", RequiredCapabilities: []string{"coder"}},
		{ID: "f-analyzer", Status: "running", Repository: "sf/acme", RequiredCapabilities: []string{"analyzer"}},
	} {
		tk.BudgetEUR, tk.Kind, tk.Track, tk.Source = 1, "agent", policy.TrackDirect, "webhook"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	// the last sessions' models: the analyzer card's alias spells the class
	if _, err := s.DB().ExecContext(ctx, `UPDATE tasks SET model_alias = CASE id WHEN 'f-analyzer' THEN 'coder' ELSE 'big' END`); err != nil {
		t.Fatal(err)
	}
	cards, err := s.ListFrozenCards(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cards {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"f-other", "f-queued", "f-run", "f-two"}) {
		t.Fatalf("ListFrozenCards(coder) = %q, want the running and queued coder cards of both repositories", ids)
	}
}

// FreezeCards reads the freeze cards by their source and status, every
// repository's, in id order, and no other card; ForgetFactoryClose deletes
// the confirmed record of a card's close, so the next close records anew.
// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestFreezeCardsReadTheSourcePG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_freezecards"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	for i, tk := range []Task{
		{ID: "fz-b", Status: "waiting-human", Repository: "sf/other", Source: factoryd.SourceFreezeCard},
		{ID: "fz-a", Status: "waiting-human", Repository: "sf/acme", Source: factoryd.SourceFreezeCard},
		{ID: "fz-done", Status: "done", Repository: "sf/acme", Source: factoryd.SourceFreezeCard},
		{ID: "hu-a", Status: "waiting-human", Repository: "sf/acme", Source: "chat"},
	} {
		tk.Kind, tk.Track, tk.IssueNumber = "human", policy.TrackDirect, int64(i+1)
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
	}
	for status, want := range map[string][]string{"waiting-human": {"fz-a", "fz-b"}, "done": {"fz-done"}, "running": nil} {
		cards, err := s.FreezeCards(ctx, status)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, c := range cards {
			ids = append(ids, c.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("FreezeCards(%s) = %q, want %q", status, ids, want)
		}
	}
	record := IntakeEvent{TaskID: "fz-done", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 3, Body: "factoryd: closed"}
	if id, err := s.InsertIntakeEvent(ctx, record); err != nil || id == 0 {
		t.Fatalf("the close's record = %d, %v", id, err)
	}
	if err := s.ConfirmFactoryClose(ctx, "fz-done", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetFactoryClose(ctx, "fz-done", 3); err != nil {
		t.Fatal(err)
	}
	if events, err := s.IntakeEvents(ctx, "fz-done"); err != nil || len(events) != 1 || events[0].Kind != IntakeFactoryClosed || events[0].CommentID != 0 {
		t.Fatalf("after the forget the card's intake = %+v, %v; want the close's record off the issue", events, err)
	}
	if id, err := s.InsertIntakeEvent(ctx, record); err != nil || id == 0 {
		t.Fatalf("the next close's record = %d, %v; want a new one", id, err)
	}
}
