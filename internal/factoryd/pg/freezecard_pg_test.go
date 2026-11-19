package pg

import (
	"context"
	"errors"
	"slices"
	"strconv"
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
// repository's, in id order, and no other card. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
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
}

// freezeEventRow inserts one gateway freeze row and answers its id.
func freezeEventRow(t *testing.T, s *PGStore, repo, kind, class, reset, words string) int64 {
	t.Helper()
	var id int64
	if err := s.DB().QueryRowContext(context.Background(), `INSERT INTO events (repository, task_id, kind, actor, via, detail)
		VALUES ($1, '', $2, 'gateway', 'gateway', jsonb_build_object('class', $3::text, 'reset', $4::text, 'words', $5::text)) RETURNING id`,
		repo, kind, class, reset, words).Scan(&id); err != nil {
		t.Fatalf("insert %s: %v", kind, err)
	}
	return id
}

// Hand-derived: StandingFreezes answers one event per repository that
// armed a class since the class's last class_resumed, each with the id,
// reset and words of the class's newest class_frozen. Six rows: big armed
// by sf/acme and sf/other, big resumed, big armed by sf/other and then by
// sf/acme with the newest reset, small armed by sf/acme. At the second
// row both repositories stand on that row; at the resume none; at the
// fourth sf/other alone; at the sixth both of big on the fifth row and
// small on the sixth. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestStandingFreezesPerRepositoryPG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_freezestanding"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	if _, err := s.DB().ExecContext(ctx, `TRUNCATE events CASCADE; UPDATE freeze_cursor SET cursor = 0 WHERE id = 1`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	ids := []int64{
		freezeEventRow(t, s, "sf/acme", "class_frozen", "big", "R1", "first"),
		freezeEventRow(t, s, "sf/other", "class_frozen", "big", "R1", "second"),
		freezeEventRow(t, s, "sf/acme", "class_resumed", "big", "R1", "second"),
		freezeEventRow(t, s, "sf/other", "class_frozen", "big", "R2", "fourth"),
		freezeEventRow(t, s, "sf/acme", "class_frozen", "big", "R3", "newest"),
		freezeEventRow(t, s, "sf/acme", "class_frozen", "small", "R4", "small's"),
	}
	type stand struct {
		id                        int64
		repo, class, reset, words string
	}
	for _, c := range []struct {
		cursor int64
		want   []stand
	}{
		{ids[1], []stand{{ids[1], "sf/acme", "big", "R1", "second"}, {ids[1], "sf/other", "big", "R1", "second"}}},
		{ids[2], nil},
		{ids[3], []stand{{ids[3], "sf/other", "big", "R2", "fourth"}}},
		{ids[5], []stand{{ids[4], "sf/acme", "big", "R3", "newest"}, {ids[4], "sf/other", "big", "R3", "newest"}, {ids[5], "sf/acme", "small", "R4", "small's"}}},
	} {
		if err := s.SetFreezeCursor(ctx, c.cursor); err != nil {
			t.Fatal(err)
		}
		standing, err := s.StandingFreezes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var got []stand
		for _, ev := range standing {
			if ev.Kind != "class_frozen" {
				t.Fatalf("at cursor %d a standing event is a %s", c.cursor, ev.Kind)
			}
			got = append(got, stand{ev.ID, ev.Repository, ev.Class, ev.Reset, ev.Words})
		}
		if !slices.Equal(got, c.want) {
			t.Fatalf("StandingFreezes at cursor %d = %+v, want %+v", c.cursor, got, c.want)
		}
	}
}

// freezeCardState reads card id's status and face, its intake events and
// the closes left unconfirmed.
func freezeCardState(t *testing.T, s *PGStore, id string) (Task, []IntakeEvent, []IntakeEvent) {
	t.Helper()
	ctx := context.Background()
	card, ok, err := s.Task(ctx, id)
	if err != nil || !ok {
		t.Fatalf("card %s = %v, %v", id, ok, err)
	}
	events, err := s.IntakeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	half, err := s.UnconfirmedFreezeCloses(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	return card, events, slices.DeleteFunc(half, func(e IntakeEvent) bool { return e.TaskID != id })
}

// Hand-derived: EndFreezeCard moves a waiting freeze card to done, writes
// the face, records the close with the comment as its body, unconfirmed,
// and deletes the card's owed reopen, all or nothing. An empty face keeps
// the face and an empty comment records no close. It writes nothing and
// answers false for a card that no longer waits, for a card that is no
// freeze card, and, with a comment, for a card on whose issue a close
// record stands; without a comment that record does not stop it.
// ReopenFreezeCard moves a done card back, writes the face, takes the
// close's record off the issue, confirmed, and records the owed reopen
// with its comment; a card that is not done is ErrNotFound and nothing is
// written. Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestFreezeCardEndsAndReopensInOneWritePG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_freezeatomic"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	cards := map[string]Task{}
	for i, tk := range []Task{
		{ID: "fz-a", Status: "waiting-human", Source: factoryd.SourceFreezeCard},
		{ID: "fz-human", Status: "waiting-human", Source: factoryd.SourceFreezeCard},
		{ID: "fz-gone", Status: "waiting-human", Source: factoryd.SourceFreezeCard},
		{ID: "fz-done", Status: "done", Source: factoryd.SourceFreezeCard},
		{ID: "hu-a", Status: "waiting-human", Source: "chat"},
		{ID: "hu-done", Status: "done", Source: "chat"},
	} {
		tk.Kind, tk.Track, tk.Repository, tk.IssueNumber, tk.Objective = "human", policy.TrackDirect, "sf/acme", int64(i+1), "the holding face"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
		cards[tk.ID] = tk
	}
	owed := func() []IntakeEvent {
		t.Helper()
		out, err := s.OwedFreezeReopens(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// the guards: nothing is written
	for id, comment := range map[string]string{"fz-done": "factoryd: closed", "hu-a": "factoryd: closed"} {
		moved, err := s.EndFreezeCard(ctx, cards[id], "the pressed face", comment)
		card, events, _ := freezeCardState(t, s, id)
		if err != nil || moved || card.Status != cards[id].Status || card.Objective != "the holding face" || len(events) != 0 {
			t.Fatalf("EndFreezeCard on %s = %v, %v; the card stands %s under %q with %+v; want nothing written", id, moved, err, card.Status, card.Objective, events)
		}
	}
	if _, err := s.ReopenFreezeCard(ctx, cards["fz-a"], "the face again", "factoryd: re-armed"); !errors.Is(err, factoryd.ErrNotFound) {
		t.Fatalf("ReopenFreezeCard on a waiting card = %v, want ErrNotFound", err)
	}
	if card, events, _ := freezeCardState(t, s, "fz-a"); card.Status != "waiting-human" || card.Objective != "the holding face" || len(events) != 0 || len(owed()) != 0 {
		t.Fatalf("after the refused reopen fz-a stands %s under %q with %+v, owed %+v; want nothing written", card.Status, card.Objective, events, owed())
	}
	if _, err := s.ReopenFreezeCard(ctx, cards["hu-done"], "the face again", "factoryd: re-armed"); !errors.Is(err, factoryd.ErrNotFound) {
		t.Fatalf("ReopenFreezeCard on a done card of another source = %v, want ErrNotFound", err)
	}
	if card, events, _ := freezeCardState(t, s, "hu-done"); card.Status != "done" || card.Objective != "the holding face" || len(events) != 0 || len(owed()) != 0 {
		t.Fatalf("after the refused reopen hu-done stands %s under %q with %+v, owed %+v; want nothing written", card.Status, card.Objective, events, owed())
	}
	human := IntakeEvent{TaskID: "fz-human", Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: 2, Body: "sebastian closed this card."}
	if id, err := s.InsertIntakeEvent(ctx, human); err != nil || id == 0 {
		t.Fatalf("the human's close record = %d, %v", id, err)
	}
	moved, err := s.EndFreezeCard(ctx, cards["fz-human"], "the pressed face", "factoryd: the press")
	if card, events, _ := freezeCardState(t, s, "fz-human"); err != nil || moved || card.Status != "waiting-human" || card.Objective != "the holding face" ||
		len(events) != 1 || events[0].Body != "sebastian closed this card." {
		t.Fatalf("EndFreezeCard over a human's close in flight = %v, %v; the card stands %s under %q with %+v; want it left to that close", moved, err, card.Status, card.Objective, events)
	}

	// a close with no comment and no face: the row alone, whatever close
	// is in flight
	for _, id := range []string{"fz-gone", "fz-human"} {
		moved, err := s.EndFreezeCard(ctx, cards[id], "", "")
		card, events, _ := freezeCardState(t, s, id)
		if err != nil || !moved || card.Status != "done" || card.Objective != "the holding face" || card.FinishedAt == nil ||
			slices.ContainsFunc(events, func(e IntakeEvent) bool { return e.Body != "sebastian closed this card." }) {
			t.Fatalf("EndFreezeCard with no comment on %s = %v, %v; the card stands %s under %q with %+v; want it done, its face kept and no record of its own", id, moved, err, card.Status, card.Objective, events)
		}
	}

	// the press's close, the re-arm's reopen and the next close
	moved, err = s.EndFreezeCard(ctx, cards["fz-a"], "the pressed face", "factoryd: the press")
	card, events, half := freezeCardState(t, s, "fz-a")
	if err != nil || !moved || card.Status != "done" || card.Objective != "the pressed face" || card.FinishedAt == nil ||
		len(events) != 1 || events[0].Kind != IntakeFactoryClosed || events[0].CommentID != 1 || events[0].Body != "factoryd: the press" || events[0].Author != "factoryd" || len(half) != 1 {
		t.Fatalf("EndFreezeCard = %v, %v; the card stands %s under %q with %+v, unconfirmed %+v; want it done under the pressed face with its close recorded, unconfirmed", moved, err, card.Status, card.Objective, events, half)
	}
	if moved, err := s.EndFreezeCard(ctx, cards["fz-a"], "another face", "factoryd: again"); err != nil || moved {
		t.Fatalf("a second EndFreezeCard = %v, %v; want false", moved, err)
	}
	// the press's close is not confirmed, so the reopen owes its comment
	// too, ahead of its own
	if got, err := s.ReopenFreezeCard(ctx, cards["fz-a"], "the holding face again", "factoryd: re-armed"); err != nil || got != "factoryd: the press\n\x1f\nfactoryd: re-armed" {
		t.Fatalf("ReopenFreezeCard = %q, %v; want the unconfirmed close's comment, the split and the re-arm's", got, err)
	}
	card, events, half = freezeCardState(t, s, "fz-a")
	if o := owed(); card.Status != "waiting-human" || card.Objective != "the holding face again" || card.FinishedAt != nil || len(half) != 0 ||
		len(events) != 2 || events[0].Kind != IntakeFactoryClosed || events[0].CommentID != 0 || events[0].Body != "factoryd: the press" ||
		len(o) != 1 || o[0].TaskID != "fz-a" || o[0].Kind != factoryd.IntakeFactoryReopened || o[0].CommentID != 1 || o[0].Body != "factoryd: the press\n\x1f\nfactoryd: re-armed" {
		t.Fatalf("after ReopenFreezeCard the card stands %s under %q with %+v, unconfirmed %+v, owed %+v; want it waiting, the press's record off its issue and confirmed, and the reopen owed", card.Status, card.Objective, events, half, o)
	}
	moved, err = s.EndFreezeCard(ctx, cards["fz-a"], "", "factoryd: the freeze ended")
	card, events, half = freezeCardState(t, s, "fz-a")
	if err != nil || !moved || card.Status != "done" || card.Objective != "the holding face again" || len(owed()) != 0 ||
		len(events) != 2 || events[1].Kind != IntakeFactoryClosed || events[1].CommentID != 1 || events[1].Body != "factoryd: the freeze ended" ||
		len(half) != 1 || half[0].Body != "factoryd: the freeze ended" {
		t.Fatalf("the next EndFreezeCard = %v, %v; the card stands %s under %q with %+v, unconfirmed %+v, owed %+v; want it done, its own close recorded and no reopen owed", moved, err, card.Status, card.Objective, events, half, owed())
	}

	// a confirmed close leaves its comment out of the owed reopen. A reopen
	// that is still owed when the card closes and opens again is replaced:
	// one owed reopen stands, the newest
	if err := s.ConfirmFactoryClose(ctx, "fz-a", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReopenFreezeCard(ctx, cards["fz-a"], "the holding face a third time", "factoryd: re-armed again"); err != nil || got != "factoryd: re-armed again" {
		t.Fatalf("the second ReopenFreezeCard = %q, %v; want the re-arm's comment alone", got, err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE tasks SET status = 'done', finished_at = now() WHERE id = 'fz-a'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReopenFreezeCard(ctx, cards["fz-a"], "the holding face a fourth time", "factoryd: re-armed once more"); err != nil || got != "factoryd: re-armed once more" {
		t.Fatalf("ReopenFreezeCard over an owed reopen = %q, %v; want the new re-arm's comment", got, err)
	}
	if o := owed(); len(o) != 1 || o[0].Body != "factoryd: re-armed once more" {
		t.Fatalf("owed after a reopen over an owed reopen = %+v, want the newest alone", o)
	}
	if err := s.SettleFreezeReopen(ctx, "fz-a", 1); err != nil {
		t.Fatal(err)
	}
	if o := owed(); len(o) != 0 {
		t.Fatalf("owed after the settle = %+v, want none", o)
	}
	if _, _, half := freezeCardState(t, s, "fz-a"); len(half) != 0 {
		t.Fatalf("after the reopens the unconfirmed closes = %+v, want none: a record off its issue counts as confirmed", half)
	}
}

// Hand-derived: three cards each carry a close recorded and not confirmed:
// freeze card fz-new's, just recorded, freeze card fz-old's, recorded
// three minutes ago, and chat card hu-old's, also three minutes old.
// UnconfirmedFreezeCloses with a two-minute age lists fz-old's alone, and
// UnconfirmedFactoryCloses, the heal pass's read, lists hu-old's alone. A
// freeze card's close that a reopen took off its issue is in neither.
// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL).
func TestUnconfirmedFreezeClosesAreTheWatchersPG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_freezecloses"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	for i, tk := range []Task{
		{ID: "fz-new", Source: factoryd.SourceFreezeCard}, {ID: "fz-old", Source: factoryd.SourceFreezeCard},
		{ID: "fz-off", Source: factoryd.SourceFreezeCard}, {ID: "hu-old", Source: "chat"},
	} {
		tk.Kind, tk.Track, tk.Repository, tk.IssueNumber, tk.Status = "human", policy.TrackDirect, "sf/acme", int64(i+1), "done"
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask %s: %v", tk.ID, err)
		}
		if id, err := s.InsertIntakeEvent(ctx, IntakeEvent{TaskID: tk.ID, Kind: IntakeFactoryClosed, Author: "factoryd", CommentID: int64(i + 1), Body: "closed"}); err != nil || id == 0 {
			t.Fatalf("the close record of %s = %d, %v", tk.ID, id, err)
		}
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE intake_events SET created_at = now() - interval '3 minutes' WHERE task_id <> 'fz-new';
		UPDATE intake_events SET comment_id = 0 WHERE task_id = 'fz-off'`); err != nil {
		t.Fatal(err)
	}
	ids := func(rows []IntakeEvent, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range rows {
			if slices.Contains([]string{"fz-new", "fz-old", "fz-off", "hu-old"}, e.TaskID) {
				out = append(out, e.TaskID)
			}
		}
		return out
	}
	if got := ids(s.UnconfirmedFreezeCloses(ctx, 2*time.Minute)); !slices.Equal(got, []string{"fz-old"}) {
		t.Fatalf("UnconfirmedFreezeCloses = %q, want fz-old's close alone", got)
	}
	if got := ids(s.UnconfirmedFactoryCloses(ctx, 2*time.Minute, 100)); !slices.Equal(got, []string{"hu-old"}) {
		t.Fatalf("UnconfirmedFactoryCloses = %q, want hu-old's close alone: a freeze card's close is its watcher's", got)
	}
}

// Hand-derived: HeldResumes lists the class_resumed rows that are the
// newest row of their class at the cursor and whose wake is not recorded.
// Four rows: big frozen, big resumed, small resumed, small frozen. At the
// fourth row big's resume is held and small's is not, as small froze
// again. RecordFreezeWake records big's wake on the events table, and the
// resume is held no more. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestHeldResumesAreWokenOncePG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "pg_freezewakes"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.DB().Close() })
	if _, err := s.DB().ExecContext(ctx, `TRUNCATE events CASCADE; UPDATE freeze_cursor SET cursor = 0 WHERE id = 1`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	ids := []int64{
		freezeEventRow(t, s, "sf/acme", "class_frozen", "big", "R1", "words"),
		freezeEventRow(t, s, "sf/acme", "class_resumed", "big", "R1", "words"),
		freezeEventRow(t, s, "sf/other", "class_resumed", "small", "R2", "words"),
		freezeEventRow(t, s, "sf/other", "class_frozen", "small", "R3", "words"),
	}
	held := func() []int64 {
		t.Helper()
		rows, err := s.HeldResumes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, ev := range rows {
			if ev.Kind != "class_resumed" {
				t.Fatalf("a held resume is a %s", ev.Kind)
			}
			out = append(out, ev.ID)
		}
		return out
	}
	for cursor, want := range map[int64][]int64{ids[0]: nil, ids[2]: {ids[1], ids[2]}, ids[3]: {ids[1]}} {
		if err := s.SetFreezeCursor(ctx, cursor); err != nil {
			t.Fatal(err)
		}
		if got := held(); !slices.Equal(got, want) {
			t.Fatalf("HeldResumes at cursor %d = %v, want %v", cursor, got, want)
		}
	}
	if err := s.SetFreezeCursor(ctx, ids[3]); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFreezeWake(ctx, factoryd.FreezeEvent{ID: ids[1], Repository: "sf/acme", Class: "big"}); err != nil {
		t.Fatal(err)
	}
	var woken int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM events WHERE kind = 'class_woken' AND repository = 'sf/acme' AND actor = 'factoryd'
		AND detail->>'class' = 'big' AND detail->>'resumed' = $1`, strconv.FormatInt(ids[1], 10)).Scan(&woken); err != nil {
		t.Fatal(err)
	}
	if got := held(); woken != 1 || len(got) != 0 {
		t.Fatalf("after RecordFreezeWake %d class_woken rows name big's resume and the held resumes are %v; want 1 and none", woken, got)
	}
}
