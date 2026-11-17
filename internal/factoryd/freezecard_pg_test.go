package factoryd

import (
	"context"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// SetFreezeCursor persists the watcher's cursor against PostgreSQL, the
// read gives it back, and a fresh watcher's FreezeEvents reads forward
// only from it: no replay of the events behind the cursor, no duplicate
// re-arm comments and asks after a restart. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestFreezeCardCursorPersistsPG(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE events CASCADE; UPDATE freeze_cursor SET cursor = 0 WHERE id = 1`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	var ids []int64
	for i, kind := range []string{"class_frozen", "class_frozen", "class_resumed"} {
		var id int64
		if err := s.db.QueryRowContext(ctx, `INSERT INTO events (at, repository, task_id, kind, actor, via, detail)
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
	if err := s.db.QueryRowContext(ctx, `SELECT cursor FROM freeze_cursor WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != ids[1] {
		t.Fatalf("the stored cursor is %d, want %d: SetFreezeCursor did not persist", stored, ids[1])
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
}
