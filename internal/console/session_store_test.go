package console

// fakeSessions is the in-memory console_sessions for the tests: rows by
// id hash, shared between two Console values the way one Postgres table
// is shared across a restart (roadmap 164). Its rows hold the tokens as
// the console stored them: sealed, never in clear.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

type fakeSessions struct {
	mu  sync.Mutex
	now func() time.Time
	// rows are the table's rows by id hash.
	rows map[string]factoryd.ConsoleSession
	// saved, deleted, touched and purged count the calls; tokens counts
	// the token updates.
	saved   int
	deleted []string
	touched int
	tokens  int
	purged  int
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{rows: map[string]factoryd.ConsoleSession{}, now: time.Now}
}

func (f *fakeSessions) SaveConsoleSession(_ context.Context, sess factoryd.ConsoleSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]factoryd.ConsoleSession{}
	}
	f.saved++
	f.rows[sess.IDHash] = sess
	return nil
}

func (f *fakeSessions) ConsoleSessionByHash(_ context.Context, hash string) (factoryd.ConsoleSession, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[hash]
	if !ok {
		return factoryd.ConsoleSession{}, false, nil
	}
	if f.now().After(row.ExpiresAt) {
		delete(f.rows, hash)
		return factoryd.ConsoleSession{}, false, nil
	}
	return row, true, nil
}

func (f *fakeSessions) DeleteConsoleSession(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, hash)
	f.deleted = append(f.deleted, hash)
	return nil
}

func (f *fakeSessions) TouchConsoleSession(_ context.Context, hash string, seenAt time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched++
	row, ok := f.rows[hash]
	if !ok {
		return false, nil
	}
	// as the Postgres store does: the row moves when it still says the value
	// its owner read, to the fake's now(); a row whose last_seen already
	// moved is one seen within the minute and holds.
	if !row.LastSeen.Equal(seenAt) {
		return false, nil
	}
	row.LastSeen = f.now()
	f.rows[hash] = row
	return true, nil
}

func (f *fakeSessions) UpdateConsoleSessionTokens(_ context.Context, hash, access, refresh string, expiry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[hash]
	if !ok {
		return nil
	}
	row.Access, row.Refresh, row.TokenExpiry = access, refresh, expiry
	f.rows[hash] = row
	f.tokens++
	return nil
}

func (f *fakeSessions) PurgeConsoleSessions(_ context.Context, at time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for hash, row := range f.rows {
		if row.ExpiresAt.Before(at) {
			delete(f.rows, hash)
			n++
		}
	}
	f.purged++
	return n, nil
}

// row reads the stored row of the hash under the fake's lock.
func (f *fakeSessions) row(hash string) (factoryd.ConsoleSession, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[hash]
	return row, ok
}

func TestSessionSurvivesARestart(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	before := r.events.count()
	r.tasks.set("cannot reach the database",
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13})
	// a second Console value over the same store: factoryd rolled, the
	// human's cookie did not change.
	restarted, err := New(r.c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.writers, restarted.writersT = r.c.writers, time.Now().Add(time.Hour)
	post := func(target string, form url.Values) (int, string) {
		rec := httptest.NewRecorder()
		req := post(target, form)
		req.AddCookie(ck)
		restarted.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	form := url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}
	if code, body := post("/tasks/t-e/answer", form); code != http.StatusSeeOther {
		t.Fatalf("an answer on the restarted console = %d, want 303: %s", code, body)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "use the replica" {
		t.Fatalf("comment = %q", got)
	}
	if r.events.count() != before+1 {
		t.Fatalf("the restart's action left %d event rows, want one", r.events.count()-before)
	}
	// and the third Console value reads the same login too
	if r.readSession(t, ck) == nil {
		t.Fatal("the login does not read back after the restart's own write")
	}
}

func TestExpiredSessionRowIsRefusedAndDeleted(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	hash := sessionIDHash(ck.Value)
	row, ok := r.sessions.row(hash)
	if !ok {
		t.Fatal("the fresh login left no row")
	}
	r.sessions.mu.Lock()
	row.ExpiresAt = time.Now().Add(-time.Minute)
	r.sessions.rows[hash] = row
	r.sessions.mu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(ck)
	s, err := r.c.session(req)
	if err != nil {
		t.Fatalf("the session read: %v", err)
	}
	if s != nil {
		t.Fatal("an expired row answered a session")
	}
	if _, ok := r.sessions.row(hash); ok {
		t.Fatal("the expired row stayed after its read")
	}
	if rec := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("GET / with the expired cookie = %d, want 303 to /login", rec.Code)
	}
}

func TestSessionRowHoldsTheHashNotTheIDAndNoClearToken(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	sum := sha256.Sum256([]byte(ck.Value))
	hash := hex.EncodeToString(sum[:])
	row, ok := r.sessions.row(hash)
	if !ok {
		t.Fatalf("no row under the SHA-256 hex of the cookie: the table has %d rows", len(r.sessions.rows))
	}
	if row.IDHash != hash {
		t.Errorf("id_hash = %q, want %q", row.IDHash, hash)
	}
	var joined bytes.Buffer
	for _, v := range []string{row.IDHash, row.Login, row.CSRF, row.Access, row.Refresh} {
		joined.WriteString(v)
	}
	if strings.Contains(joined.String(), ck.Value) {
		t.Error("the raw cookie id appears in the row")
	}
	if row.Access == "access-1" || strings.Contains(row.Access, "access-1") {
		t.Errorf("the access token is stored in clear: %q", row.Access)
	}
	if row.Refresh == "refresh-1" || strings.Contains(row.Refresh, "refresh-1") {
		t.Errorf("the refresh token is stored in clear: %q", row.Refresh)
	}
	if access, err := openToken(r.c.cfg.SessionKey, row.Access, row.IDHash); err != nil || access != "access-1" {
		t.Errorf("the stored access token opens to %q, %v; want access-1", access, err)
	}
	if refresh, err := openToken(r.c.cfg.SessionKey, row.Refresh, row.IDHash); err != nil || refresh != "refresh-1" {
		t.Errorf("the stored refresh token opens to %q, %v; want refresh-1", refresh, err)
	}
	other := append([]byte(nil), r.c.cfg.SessionKey...)
	other[len(other)-1] ^= 1
	if _, err := openToken(other, row.Access, row.IDHash); err == nil {
		t.Error("another session key decrypts the stored token")
	}
}

func TestSealTokenNonceIsFreshPerValue(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	hash := "test-hash"
	a, err := sealToken(key, "same-token", hash)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sealToken(key, "same-token", hash)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two seals of one token are equal: the nonce is not random")
	}
	for _, sealed := range []string{a, b} {
		if got, err := openToken(key, sealed, hash); err != nil || got != "same-token" {
			t.Fatalf("open(%q) = %q, %v", sealed, got, err)
		}
	}
	if got, err := sealToken(key, "", hash); err != nil || got != "" {
		t.Fatalf(`the empty token seals to %q, %v`, got, err)
	}
}

func TestLastSeenMovesAtMostOnceAMinute(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	hash := sessionIDHash(ck.Value)
	row, _ := r.sessions.row(hash)
	first := row.LastSeen
	get := func() *session {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(ck)
		s, err := r.c.session(req)
		if err != nil {
			t.Fatalf("the session read: %v", err)
		}
		return s
	}
	// five reads within a minute leave the column where it was.
	for range 5 {
		if get() == nil {
			t.Fatal("a live session stopped answering")
		}
	}
	if row, _ := r.sessions.row(hash); !row.LastSeen.Equal(first) {
		t.Fatalf("last_seen moved within a minute: %s, was %s", row.LastSeen, first)
	}
	if r.sessions.touched != 0 {
		t.Fatalf("TouchConsoleSession ran %d times within a minute", r.sessions.touched)
	}
	// the row seen a minute ago moves on the next read, once. The touch
	// sets last_seen to the fake's now(), as the Postgres store does.
	stale := time.Now().Add(-time.Minute)
	r.sessions.mu.Lock()
	row = r.sessions.rows[hash]
	row.LastSeen = stale
	r.sessions.rows[hash] = row
	r.sessions.mu.Unlock()
	var before time.Time
	r.sessions.mu.Lock()
	before = r.sessions.now()
	r.sessions.mu.Unlock()
	if get() == nil {
		t.Fatal("a live session stopped answering")
	}
	row, _ = r.sessions.row(hash)
	if row.LastSeen.Before(before) || !row.LastSeen.After(stale) {
		t.Fatalf("last_seen after a minute = %s, want the fake's now() around %s", row.LastSeen, before)
	}
	if r.sessions.touched != 1 {
		t.Fatalf("TouchConsoleSession ran %d times, want once", r.sessions.touched)
	}
	moved := row.LastSeen
	if s := get(); s == nil {
		t.Fatal("a live session stopped answering")
	} else if !s.seen.Equal(moved) {
		t.Fatalf("the read of the moved row did not take its last_seen: %s, row %s", s.seen, moved)
	}
	if row, _ := r.sessions.row(hash); !row.LastSeen.Equal(moved) {
		t.Fatalf("last_seen moved twice in a minute: %s, then %s", moved, row.LastSeen)
	}
	// the purge takes the expired rows: a login leaves them behind.
	r.sessions.mu.Lock()
	r.sessions.rows["gone"] = factoryd.ConsoleSession{IDHash: "gone", ExpiresAt: time.Now().Add(-time.Second)}
	r.sessions.mu.Unlock()
	r.c.purgeSessions(context.Background())
	if _, ok := r.sessions.row("gone"); ok {
		t.Fatal("the purge kept an expired row")
	}
}

func TestLogoutDeletesTheRow(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	hash := sessionIDHash(ck.Value)
	if _, ok := r.sessions.row(hash); !ok {
		t.Fatal("the login left no row")
	}
	if rec := r.do(post("/logout", url.Values{"csrf": {s.csrf}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d", rec.Code)
	}
	if _, ok := r.sessions.row(hash); ok {
		t.Fatal("logout kept the row")
	}
	if !slices.Contains(r.sessions.deleted, hash) {
		t.Fatalf("deletes %v: want the row's hash", r.sessions.deleted)
	}
}

// A login's tokens live in the store: a second Console value over the same
// table refreshes them and writes the new pair back, so the roll keeps the
// member's Forgejo power (roadmap 164).
func TestRestartRefreshesTheStoredTokens(t *testing.T) {
	r := newRig(t)
	r.fg.expiresIn = 30 // under the one-minute margin
	ck, s := r.loggedIn(t)
	r.fg.expiresIn = 3600
	restarted, err := New(r.c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.writers, restarted.writersT = r.c.writers, time.Now().Add(time.Hour)
	r.tasks.set("cannot reach the database",
		factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13})
	req := post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}})
	req.AddCookie(ck)
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the answer on the restarted console = %d: %s", rec.Code, rec.Body)
	}
	g := r.fg.grants[len(r.fg.grants)-1]
	if g.Get("grant_type") != "refresh_token" || g.Get("refresh_token") != "refresh-1" {
		t.Fatalf("the restart's refresh grant %v", g)
	}
	last := r.fg.calls[len(r.fg.calls)-1]
	if !strings.HasSuffix(last, "Bearer access-2") {
		t.Fatalf("the restart acted with %q, want the refreshed token", last)
	}
	if access, refresh := r.tokens(t, ck); access != "access-2" || refresh != "refresh-2" {
		t.Fatalf("the row after the restart's refresh holds %q/%q, want access-2/refresh-2", access, refresh)
	}
}

// The cookie's id is never stored: the raw id appears in no row of the
// table, only its SHA-256 hex.
func TestNoRowHoldsTheRawCookieID(t *testing.T) {
	r := newRig(t)
	var ids []string
	for range 3 {
		ck, _ := r.loggedIn(t)
		ids = append(ids, ck.Value)
	}
	if len(r.sessions.rows) != 3 {
		t.Fatalf("three logins left %d rows", len(r.sessions.rows))
	}
	for _, id := range ids {
		for hash, row := range r.sessions.rows {
			if strings.Contains(hash+row.Login+row.CSRF+row.Access+row.Refresh, id) {
				t.Fatalf("the row of %s holds the raw cookie id", hash)
			}
		}
	}
}
