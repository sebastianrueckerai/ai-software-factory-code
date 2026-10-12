package factoryd

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
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

// The console's own sealing, kept here so the store's test needs no import
// of the console (which would be a cycle): the same AES-256-GCM key the
// console derives from Config.SessionKey with the fixed label, and its
// nonce-first base64url seal. A row this test writes is a row the console
// reads back, and one the console writes is one it opens here.
const consoleTokenKeyLabel = "console-session-tokens-v1"

var consoleTestSessionKey = []byte("0123456789abcdef0123456789abcdef")

func consoleTokenKey() []byte {
	m := hmac.New(sha256.New, consoleTestSessionKey)
	m.Write([]byte(consoleTokenKeyLabel)) // #nosec G104 -- a hash write never fails
	return m.Sum(nil)
}

// sealConsoleToken seals tok as the console does: AES-256-GCM under the
// key of the session key, a fresh random nonce in front, base64url without
// padding.
func sealConsoleToken(t *testing.T, tok string) string {
	t.Helper()
	if tok == "" {
		return ""
	}
	block, err := aes.NewCipher(consoleTokenKey())
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(tok), nil))
}

// openConsoleToken is sealConsoleToken's inverse.
func openConsoleToken(t *testing.T, sealed string) string {
	t.Helper()
	if sealed == "" {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("the stored token is not base64url: %v", err)
	}
	block, err := aes.NewCipher(consoleTokenKey())
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < gcm.NonceSize() {
		t.Fatal("the stored token is shorter than a nonce")
	}
	pt, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
	if err != nil {
		t.Fatalf("the stored token does not decrypt under the session key: %v", err)
	}
	return string(pt)
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL). A row one
// store writes reads back on another over the same table; an expired row
// is refused and deleted by its read; last_seen moves at most once a
// minute; the token update and the purge do their rows (roadmap 164).
func TestPGStoreConsoleSessions(t *testing.T) {
	s, ctx := consoleSessionStore(t), context.Background()
	// the id_hash is the SHA-256 hex of a cookie id, as the console stores
	// it: the raw id appears nowhere in the row.
	id := "kR7Qp2zXwYvB4mNcT1sLdAeFgHjKlMnOpQrStUvWxYz"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
	clearAccess, clearRefresh := "ghp_access_1234567890", "gho_refresh_0987654321"
	now := time.Now().UTC().Truncate(time.Microsecond)
	// The row was written an hour ago, so the minute gate below can pass.
	at := now.Add(-time.Hour)
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: hash, Login: "sebastian", CSRF: "tok",
		Access: sealConsoleToken(t, clearAccess), Refresh: sealConsoleToken(t, clearRefresh),
		TokenExpiry: at.Add(2 * time.Hour), CreatedAt: at, LastSeen: at, ExpiresAt: at.Add(13 * time.Hour)}); err != nil {
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
	if row.Login != "sebastian" || row.CSRF != "tok" ||
		!within(row.TokenExpiry, at.Add(2*time.Hour)) || !within(row.ExpiresAt, at.Add(13*time.Hour)) {
		t.Fatalf("the row reads back as %+v, written at %s", row, at)
	}
	// the tokens travel to and from the store sealed, never in clear, and
	// open back to what the member got from Forgejo.
	if row.Access == clearAccess || strings.Contains(row.Access, clearAccess) ||
		row.Refresh == clearRefresh || strings.Contains(row.Refresh, clearRefresh) {
		t.Fatalf("the row holds a clear token: %q / %q", row.Access, row.Refresh)
	}
	if got := openConsoleToken(t, row.Access); got != clearAccess {
		t.Fatalf("the stored access token opens to %q, want %q", got, clearAccess)
	}
	if got := openConsoleToken(t, row.Refresh); got != clearRefresh {
		t.Fatalf("the stored refresh token opens to %q, want %q", got, clearRefresh)
	}
	// the raw cookie id is nowhere in the row, and the key is its hash.
	if row.IDHash != hash {
		t.Fatalf("id_hash = %q, want the SHA-256 hex %q", row.IDHash, hash)
	}
	var joined bytes.Buffer
	for _, v := range []string{row.IDHash, row.Login, row.CSRF, row.Access, row.Refresh} {
		joined.WriteString(v)
	}
	if strings.Contains(joined.String(), id) {
		t.Fatal("the row holds the raw cookie id")
	}
	// last_seen moves at most once a minute. The touch takes the value the
	// caller read and moves the column a minute past it, the next time its
	// owner may touch again; a second touch on the same read finds the row
	// saying something else and holds — whatever either clock says.
	moved, err := other.TouchConsoleSession(ctx, hash, row.LastSeen)
	if err != nil {
		t.Fatalf("TouchConsoleSession: %v", err)
	}
	if !moved {
		t.Fatal("last_seen did not move on the value the caller read")
	}
	seen, _, _ := other.ConsoleSessionByHash(ctx, hash)
	if !within(seen.LastSeen, row.LastSeen.Add(time.Minute)) {
		t.Fatalf("last_seen after the touch = %s, want %s", seen.LastSeen, row.LastSeen.Add(time.Minute))
	}
	if moved, err := other.TouchConsoleSession(ctx, hash, row.LastSeen); err != nil || moved {
		t.Fatalf("a second touch on the same read = %v, %v: want it held", moved, err)
	}
	if row2, _, _ := other.ConsoleSessionByHash(ctx, hash); !within(row2.LastSeen, seen.LastSeen) {
		t.Fatalf("the held touch moved last_seen anyway: %s then %s", seen.LastSeen, row2.LastSeen)
	}
	// a minute of quiet, and the row's owner touches it again.
	if moved, err := other.TouchConsoleSession(ctx, hash, seen.LastSeen); err != nil || !moved {
		t.Fatalf("the touch a minute later = %v, %v: want the move", moved, err)
	}
	newAccess, newRefresh := "ghp_access_refreshed_2", "gho_refresh_rotated_2"
	if err := other.UpdateConsoleSessionTokens(ctx, hash, sealConsoleToken(t, newAccess), sealConsoleToken(t, newRefresh), at.Add(2*time.Hour)); err != nil {
		t.Fatalf("UpdateConsoleSessionTokens: %v", err)
	}
	row, _, _ = other.ConsoleSessionByHash(ctx, hash)
	if row.Access == newAccess || strings.Contains(row.Access, newAccess) {
		t.Fatalf("the refreshed access token reached the row in clear: %q", row.Access)
	}
	if got := openConsoleToken(t, row.Access); got != newAccess {
		t.Fatalf("the refreshed access token opens to %q, want %q", got, newAccess)
	}
	if got := openConsoleToken(t, row.Refresh); got != newRefresh {
		t.Fatalf("the refreshed refresh token opens to %q, want %q", got, newRefresh)
	}
	if !within(row.TokenExpiry, at.Add(2*time.Hour)) {
		t.Fatalf("the refreshed expiry did not reach the row: %s", row.TokenExpiry)
	}
	// logout deletes the row.
	if err := other.DeleteConsoleSession(ctx, hash); err != nil {
		t.Fatalf("DeleteConsoleSession: %v", err)
	}
	if _, ok, err := other.ConsoleSessionByHash(ctx, hash); err != nil || ok {
		t.Fatalf("the deleted row still reads: %v, %v", ok, err)
	}
	// an expired row is refused and deleted by its read.
	expired := fmt.Sprintf("%x", sha256.Sum256([]byte("expired-cookie-id")))
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
	gone := fmt.Sprintf("%x", sha256.Sum256([]byte("gone-cookie-id")))
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: gone, Login: "s", CSRF: "t",
		CreatedAt: at, LastSeen: at, ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	live := fmt.Sprintf("%x", sha256.Sum256([]byte("live-cookie-id")))
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

// TestPGStoreConsoleSessionColumnTypes pins what the driver gives back for
// the columns of console_sessions, which decides how a touch must be
// guarded: a timestamptz comes back as Go's local wall clock of the instant
// Postgres stored, to the microsecond, so a comparison of a read value with
// the column is exact while a comparison with a second clock is not.
func TestPGStoreConsoleSessionColumnTypes(t *testing.T) {
	s, ctx := consoleSessionStore(t), context.Background()
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("column-types")))
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.SaveConsoleSession(ctx, ConsoleSession{IDHash: hash, Login: "s", CSRF: "t",
		CreatedAt: at, LastSeen: at, ExpiresAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var colType string
	if err := s.db.QueryRowContext(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_name = 'console_sessions' AND column_name = 'last_seen'`).Scan(&colType); err != nil {
		t.Fatal(err)
	}
	if colType != "timestamp with time zone" {
		t.Fatalf("last_seen is %s, want timestamp with time zone", colType)
	}
	row, ok, err := s.ConsoleSessionByHash(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("the read = %v, %v", ok, err)
	}
	if !within(row.LastSeen, at) {
		t.Fatalf("last_seen reads back %s, written %s", row.LastSeen, at)
	}
	// the equality a touch guards on holds across a save and a read, even
	// though the driver hands back a local time: the compare-and-set below
	// moves the row, and a stale value does not.
	if moved, err := s.TouchConsoleSession(ctx, hash, at.UTC().Truncate(time.Microsecond)); err != nil || !moved {
		t.Fatalf("the touch on the value written = %v, %v: want the move", moved, err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE console_sessions SET last_seen = $2 WHERE id_hash = $1`, hash, at)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, _ := s.ConsoleSessionByHash(ctx, hash)
	if moved, err := s.TouchConsoleSession(ctx, hash, at.Add(-time.Second)); err != nil || moved {
		t.Fatalf("a touch on a value the row does not hold = %v, %v: want it held", moved, err)
	}
	if row2, _, _ := s.ConsoleSessionByHash(ctx, hash); !within(row2.LastSeen, fresh.LastSeen) {
		t.Fatalf("the stale touch moved the row: %s then %s", fresh.LastSeen, row2.LastSeen)
	}
}

// within reports whether got names want to the microsecond: Postgres keeps
// a timestamptz to the microsecond, Go reads it back as a local time.
func within(got, want time.Time) bool { return got.Sub(want).Abs() < time.Microsecond }
