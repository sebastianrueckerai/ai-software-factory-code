package console

// The console's lookup path over the real Postgres store: the login of a
// member is read, written and touched in console_sessions by factoryd's own
// SQL, so the once-a-minute gate of last_seen is proved end to end and not
// only through the fake (roadmap 164). These tests need a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL); without it schematest skips them.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// pgStore is a factoryd.PGStore on its own database of the test server,
// migrated by OpenStore and closed with the test.
func pgStore(t *testing.T) *factoryd.PGStore {
	t.Helper()
	s, err := factoryd.OpenStore(context.Background(), schematest.DSN(t, "console_sessions_pg"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// loginOf runs one member's OAuth login on c: /login, then the callback as
// Forgejo answers it. The console's session store is whatever c.cfg holds —
// the Postgres rows of these tests are written by the real newSession.
func loginOf(t *testing.T, r *rig, c *Console) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("/login = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	r.fg.mu.Lock()
	r.fg.challenge = loc.Query().Get("code_challenge")
	r.fg.mu.Unlock()
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=the-code&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == loginCookie {
			cbReq.AddCookie(ck)
		}
	}
	cb := httptest.NewRecorder()
	c.ServeHTTP(cb, cbReq)
	if cb.Code != http.StatusSeeOther {
		t.Fatalf("the callback = %d: %s", cb.Code, cb.Body.String())
	}
	for _, ck := range cb.Result().Cookies() {
		if ck.Name == sessionCookie {
			return ck
		}
	}
	t.Fatal("the callback set no session cookie")
	return nil
}

// getOn serves GET /tasks/t-1/timeline with ck on c and returns the status:
// auth reads and touches the session first, then the handler answers 404 for
// a card the fake store does not hold, so the answer says only whether the
// login served.
func getOn(t *testing.T, c *Console, ck *http.Cookie) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil)
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec.Code
}

// The lookup path touches the row at most once a minute, in the store's
// own SQL: five requests inside a minute move last_seen once, and the next
// move comes only when the row's owner holds a value a minute old. The
// column moves a minute past the value it held, so a wall clock that lags
// the database's still moves it on the sixth request (roadmap 164).
func TestLookupTouchesTheRowOnceAMinuteOverPostgres(t *testing.T) {
	store := pgStore(t)
	r := newRig(t)
	r.c.cfg.Sessions = store
	ck := loginOf(t, r, r.c)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(ck.Value)))
	ctx := context.Background()
	row, ok, err := store.ConsoleSessionByHash(ctx, hash)
	if err != nil || !ok {
		t.Fatalf("the login left no row in Postgres: %v, %v", ok, err)
	}
	if row.IDHash != hash {
		t.Fatalf("id_hash = %q, want the SHA-256 hex of the cookie", row.IDHash)
	}
	// the minute gate is the console's own clock: a session whose row says a
	// future instant holds it shut. The row's clock, though, is Postgres',
	// and this process may sit a moment behind it — a touch writes a value a
	// moment ahead of here — so the test asks the database where its now()
	// is before it predicts anything, and reads the page once on a fresh
	// Console first: that read carries the row's future value into a session
	// nobody touches again, and leaves this Console at the login's own write.
	var dbNow time.Time
	if err := store.QueryRowScan(ctx, `SELECT now()`, &dbNow); err != nil {
		t.Fatalf("the database's clock: %v", err)
	}
	get := func() {
		if code := getOn(t, r.c, ck); code != http.StatusNotFound {
			t.Fatalf("GET /tasks/t-1/timeline with a live login = %d, want 404 past the session read", code)
		}
	}
	getFresh := func(c *Console) {
		req := httptest.NewRequest(http.MethodGet, "/tasks/t-1/timeline", nil)
		req.AddCookie(ck)
		c.ServeHTTP(httptest.NewRecorder(), req)
	}
	fresh, err := New(r.c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fresh.writers, fresh.writersT = r.c.writers, time.Now().Add(time.Hour)
	getFresh(fresh)
	seenBy := func() time.Time {
		row, _, err := store.ConsoleSessionByHash(ctx, hash)
		if err != nil {
			t.Fatalf("the row: %v", err)
		}
		return row.LastSeen.UTC().Truncate(time.Microsecond)
	}
	// ninety seconds at the keyboard, then one request: the read moves
	// last_seen a minute past the value it held — not to any process's clock
	// — and the row reads exactly that.
	if err := store.SetConsoleSessionSeen(ctx, hash, dbNow.Add(-90*time.Second)); err != nil {
		t.Fatalf("SetConsoleSessionSeen: %v", err)
	}
	held := dbNow.Add(-90 * time.Second).UTC().Truncate(time.Microsecond).Add(time.Minute)
	get()
	if seen := seenBy(); !seen.Equal(held) {
		t.Fatalf("the request after ninety seconds wrote %s, want the value it read plus a minute (%s)", seen, held)
	}
	// twelve requests inside the minute that followed — six from the console
	// that logged the member in, six from a fresh one over the same table,
	// the member's next requests after a roll — leave the column where it
	// is: the gate is the row's, not either session's.
	for range 6 {
		get()
	}
	for range 6 {
		getFresh(fresh)
	}
	if again := seenBy(); !again.Equal(held) {
		t.Fatalf("twelve requests in one minute moved last_seen twice: %s then %s", held, again)
	}
	// a minute of quiet on the row — what an idle member's next request
	// finds — moves it once more, and the request right after that move
	// leaves it standing.
	var dbNow2 time.Time
	if err := store.QueryRowScan(ctx, `SELECT now()`, &dbNow2); err != nil {
		t.Fatalf("the database's clock: %v", err)
	}
	if err := store.SetConsoleSessionSeen(ctx, hash, dbNow2.Add(-90*time.Second)); err != nil {
		t.Fatalf("SetConsoleSessionSeen: %v", err)
	}
	want2 := dbNow2.Add(-90 * time.Second).UTC().Truncate(time.Microsecond).Add(time.Minute)
	get()
	if moved := seenBy(); !moved.Equal(want2) {
		t.Fatalf("the second minute of quiet wrote %s, want %s", moved, want2)
	}
	get()
	if again := seenBy(); !again.Equal(want2) {
		t.Fatalf("the request right after the move touched again: %s then %s", want2, again)
	}
	// fourteen requests over two minutes of quiet moved the column twice:
	// once per minute, whichever Console asked. The fresh Console holds the
	// login's own write and reads the row as it stands, so its next request
	// asks the gate too — and the column holds: a row whose last_seen moved
	// since a session read it is a row seen within the minute.
	getFresh(fresh)
	if again := seenBy(); !again.Equal(want2) {
		t.Fatalf("a second Console's request after the move touched again: %s then %s", want2, again)
	}
}

// A restart of factoryd is a second Console value over the same Postgres
// table: the member's cookie keeps working, its row still holds the sealed
// tokens of the first login, and the logout of the restarted console
// deletes the row the first one wrote (roadmap 164).
func TestLoginSurvivesARestartOverPostgres(t *testing.T) {
	store := pgStore(t)
	r := newRig(t)
	r.c.cfg.Sessions = store
	ck := loginOf(t, r, r.c)
	s := r.readSession(t, ck)
	if s == nil {
		t.Fatal("the fresh login does not read back from Postgres")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(ck.Value)))
	restarted, err := New(r.c.cfg) // the same Config, the same store: factoryd rolled
	if err != nil {
		t.Fatal(err)
	}
	if code := getOn(t, restarted, ck); code != http.StatusNotFound {
		t.Fatalf("a page on the restarted console = %d, want 404 past the session read", code)
	}
	row, ok, err := store.ConsoleSessionByHash(context.Background(), hash)
	if err != nil || !ok {
		t.Fatalf("the restart lost the row: %v, %v", ok, err)
	}
	if row.Login != "sebastian" || row.CSRF != s.csrf {
		t.Fatalf("the row reads back %+v", row)
	}
	if access, err := openToken(r.c.cfg.SessionKey, row.Access); err != nil || access != "access-1" {
		t.Fatalf("the stored access token opens to %q, %v; want access-1", access, err)
	}
	// the restarted console ends the login: the row goes.
	logout := post("/logout", url.Values{"csrf": {s.csrf}})
	logout.AddCookie(ck)
	lrec := httptest.NewRecorder()
	restarted.ServeHTTP(lrec, logout)
	if lrec.Code != http.StatusSeeOther {
		t.Fatalf("the restart's logout = %d: %s", lrec.Code, lrec.Body.String())
	}
	if _, ok, _ := store.ConsoleSessionByHash(context.Background(), hash); ok {
		t.Fatal("the logout of the restarted console kept the row")
	}
}
