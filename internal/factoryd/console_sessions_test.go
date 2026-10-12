package factoryd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func consoleSessionStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_console_sessions"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.ExecContext(t.Context(), `TRUNCATE console_sessions`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A row one
// store writes reads back on another over the same table; an expired row
// is refused and deleted by its read; last_seen moves at most once a
// minute; the token update and the purge do their rows (roadmap 164).
func TestPGStoreConsoleSessions(t *testing.T) {
	s, ctx := consoleSessionStore(t), context.Background()
	hash := strings.Repeat("a", 64)
	now := time.Now().UTC().Truncate(time.Millisecond)
	// The row was written an hour ago, so the minute gate below can pass.
	at := now.Add(-time.Hour)
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: hash, Login: "sebastian", CSRF: "tok",
		Access: "sealed-access", Refresh: "sealed-refresh", TokenExpiry: at.Add(2 * time.Hour),
		CreatedAt: at, LastSeen: at, ExpiresAt: at.Add(13 * time.Hour)}); err != nil {
		t.Fatalf("SaveConsoleSession: %v", err)
	}
	other, err := OpenStore(ctx, schematest.DSN(t, "factoryd_console_sessions"))
	if err != nil {
		t.Fatalf("the second store: %v", err)
	}
	defer other.db.Close()
	row, ok, err := other.ConsoleSessionByHash(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("the other store's read = %v, %v: want the row", ok, err)
	}
	if row.Login != "sebastian" || row.CSRF != "tok" || row.Access != "sealed-access" || row.Refresh != "sealed-refresh" ||
		!within(row.TokenExpiry, at.Add(2*time.Hour)) || !within(row.ExpiresAt, at.Add(13*time.Hour)) {
		t.Fatalf("the row reads back as %+v, written at %s", row, at)
	}
	// last_seen moves at most once a minute. The row reads back an hour
	// old, so the caller's threshold — its read plus a minute — is still
	// past the stored value and the column moves to now. The next request
	// reads the moved row; its threshold sits a minute past the fresh
	// last_seen, the gate refuses, and the column holds (roadmap 164).
	moved, err := other.TouchConsoleSession(ctx, hash, row.LastSeen.Add(time.Minute))
	if err != nil {
		t.Fatalf("TouchConsoleSession: %v", err)
	}
	if !moved {
		t.Fatal("last_seen did not move after a minute of quiet")
	}
	seen, _, _ := other.ConsoleSessionByHash(ctx, hash)
	if !seen.LastSeen.After(now.Add(-time.Minute)) {
		t.Fatalf("last_seen did not move to now: %s", seen.LastSeen)
	}
	// A caller still holding the stale read of an hour ago cannot move the
	// row twice: its threshold sits behind the fresh last_seen and holds.
	if moved, err := other.TouchConsoleSession(ctx, hash, row.LastSeen.Add(time.Minute)); err != nil || moved {
		t.Fatalf("a second touch on the same stale read = %v, %v: want it held", moved, err)
	}
	if row2, _, _ := other.ConsoleSessionByHash(ctx, hash); !row2.LastSeen.Equal(seen.LastSeen) {
		t.Fatalf("the held touch moved last_seen anyway: %s then %s", seen.LastSeen, row2.LastSeen)
	}
	if err := other.UpdateConsoleSessionTokens(ctx, hash, "sealed-access-2", "sealed-refresh-2", at.Add(2*time.Hour)); err != nil {
		t.Fatalf("UpdateConsoleSessionTokens: %v", err)
	}
	row, _, _ = other.ConsoleSessionByHash(ctx, hash)
	if row.Access != "sealed-access-2" || row.Refresh != "sealed-refresh-2" || !within(row.TokenExpiry, at.Add(2*time.Hour)) {
		t.Fatalf("the refreshed tokens did not reach the row: %+v", row)
	}
	// logout deletes the row.
	if err := other.DeleteConsoleSession(ctx, hash); err != nil {
		t.Fatalf("DeleteConsoleSession: %v", err)
	}
	if _, ok, err := other.ConsoleSessionByHash(ctx, hash); err != nil || ok {
		t.Fatalf("the deleted row still reads: %v, %v", ok, err)
	}
	// an expired row is refused and deleted by its read.
	expired := strings.Repeat("b", 64)
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: expired, Login: "sebastian", CSRF: "t",
		CreatedAt: at.Add(-13 * time.Hour), LastSeen: at.Add(-13 * time.Hour), ExpiresAt: at.Add(-time.Minute)}); err != nil {
		t.Fatalf("the expired row: %v", err)
	}
	if _, ok, err := s.ConsoleSessionByHash(ctx, expired); err != nil || ok {
		t.Fatalf("the expired row answered %v (%v)", ok, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM console_sessions WHERE id_hash = $1`, expired).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the expired row stayed: %d read, %v", n, err)
	}
	// the purge takes every row expired before now; the read above already
	// swept its own row, so a second expired row proves the sweep.
	gone := strings.Repeat("d", 64)
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: gone, Login: "s", CSRF: "t",
		CreatedAt: at, LastSeen: at, ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	live := strings.Repeat("c", 64)
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: live, Login: "s", CSRF: "t", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PurgeConsoleSessions(ctx, time.Now()); err != nil || got < 1 {
		t.Fatalf("PurgeConsoleSessions = %d, %v: want the expired row gone", got, err)
	}
	if _, ok, _ := s.ConsoleSessionByHash(ctx, live); !ok {
		t.Fatal("the purge took a live row")
	}
}

// within reports whether got names want to the millisecond: Postgres and
// Go round a stored timestamptz differently.
func within(got, want time.Time) bool { return got.Sub(want).Abs() < time.Millisecond }
