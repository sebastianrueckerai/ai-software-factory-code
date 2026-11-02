package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sealed"
)

// door is a client of the console's /mcp: the header and the cookie it
// adds to the SDK's requests, and the last refusal it read.
type door struct {
	auth   string
	cookie *http.Cookie
	status int
	body   string
	header http.Header
}

func (d *door) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if d.auth != "" {
		req.Header.Set("Authorization", d.auth)
	}
	if d.cookie != nil {
		req.AddCookie(d.cookie)
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil || res.StatusCode < 400 {
		return res, err
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	d.status, d.body, d.header = res.StatusCode, string(b), res.Header
	res.Body = io.NopCloser(bytes.NewReader(b))
	return res, err
}

// open connects d to srv's /mcp.
func (d *door) open(srv *httptest.Server) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: d}, DisableStandaloneSSE: true}, nil)
}

// chatTokenOf stores login's chat token on repository, named laptop and
// live for a day unless edit changes the row, and answers its bearer.
func (r *rig) chatTokenOf(t *testing.T, login, repository string, edit func(*factoryd.ChatToken)) string {
	t.Helper()
	bearer, hash, err := newChatToken()
	if err != nil {
		t.Fatal(err)
	}
	box, err := sealed.Seal(r.c.cfg.SessionKey, chatTokenKeyLabel, "fj-"+login, hash)
	if err != nil {
		t.Fatal(err)
	}
	row := factoryd.ChatToken{Hash: hash, Login: login, Repository: repository, Name: "laptop", Sealed: box, Expires: time.Now().Add(24 * time.Hour)}
	if edit != nil {
		edit(&row)
	}
	if _, err := r.chatTokens.AddChatToken(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	return bearer
}

// mcpRig is the rig behind a server and sebastian's chat token on sf/code.
func mcpRig(t *testing.T) (*rig, *httptest.Server, string) {
	t.Helper()
	r := newRig(t)
	srv := httptest.NewServer(r.c)
	t.Cleanup(srv.Close)
	return r, srv, r.chatTokenOf(t, "sebastian", "sf/code", nil)
}

// opened is d's session on srv; the test fails when the door refuses.
func opened(t *testing.T, srv *httptest.Server, d *door) *mcp.ClientSession {
	t.Helper()
	session, err := d.open(srv)
	if err != nil {
		t.Fatalf("open = %v (%d %q)", err, d.status, d.body)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// threadsOf is chat_threads' answer on session.
func threadsOf(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "chat_threads", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("chat_threads: %v", err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		t.Fatalf("chat_threads refused: %s", text)
	}
	return text
}

func TestMCPRefusesWithoutValidBearer(t *testing.T) {
	r, srv, _ := mcpRig(t)
	unknown, _, err := newChatToken()
	if err != nil {
		t.Fatal(err)
	}
	expired := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Expires = time.Now().Add(-time.Minute) })
	revoked := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Revoked = time.Now().Add(-time.Minute) })
	for _, tc := range []struct{ name, auth string }{
		{"none", ""}, {"unknown", "Bearer " + unknown}, {"expired", "Bearer " + expired}, {"revoked", "Bearer " + revoked},
	} {
		d := &door{auth: tc.auth}
		if _, err := d.open(srv); err == nil || d.status != http.StatusUnauthorized || d.body != "unknown, expired or revoked chat token\n" {
			t.Errorf("%s: open = %v, %d %q; want 401 unknown, expired or revoked chat token", tc.name, err, d.status, d.body)
		}
		if got := d.header.Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("%s: WWW-Authenticate = %q, want Bearer", tc.name, got)
		}
	}
	r.chatTokens.mu.Lock()
	defer r.chatTokens.mu.Unlock()
	for _, row := range r.chatTokens.rows {
		if !row.LastUsed.IsZero() {
			t.Errorf("a refused token's last use moved: %+v", row)
		}
	}
}

func TestMCPRefusesTokenScheme(t *testing.T) {
	_, srv, bearer := mcpRig(t)
	d := &door{auth: "token " + bearer}
	if _, err := d.open(srv); err == nil || d.status != http.StatusUnauthorized || d.body != "unknown, expired or revoked chat token\n" {
		t.Fatalf("open = %v, %d %q; want 401 unknown, expired or revoked chat token", err, d.status, d.body)
	}
}

// A pasted bearer keeps its doubled space and its line end, which no
// client sends over the wire, so the request is served in process.
func TestMCPTrimsPastedBearer(t *testing.T) {
	r, _, bearer := mcpRig(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chat_threads","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer  "+bearer+"\n")
	rec := r.do(req)
	var answer struct {
		Result struct {
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); rec.Code != http.StatusOK || err != nil ||
		len(answer.Result.Content) != 1 || answer.Result.Content[0].Text != "[]" {
		t.Fatalf("POST /mcp = %d %q (%v), want 200 and chat_threads' []", rec.Code, rec.Body.String(), err)
	}
}

func TestMCPRefusesFormerWriter(t *testing.T) {
	r, srv, bearer := mcpRig(t)
	for _, tc := range []struct {
		name  string
		repos []string
	}{
		{"writes another project", []string{"sf/code"}},
		{"writes none", []string{"sf/code", "demo-app/demo-app", "linkshort/linkshort"}},
	} {
		r.writers.mu.Lock()
		for _, repo := range tc.repos {
			r.repoWriters[repo] = nil
		}
		r.writers.mu.Unlock()
		r.forgetWriters()
		d := &door{auth: "Bearer " + bearer}
		if _, err := d.open(srv); err == nil || d.status != http.StatusForbidden || d.body != "sebastian no longer writes sf/code\n" {
			t.Errorf("%s: open = %v, %d %q; want 403 sebastian no longer writes sf/code", tc.name, err, d.status, d.body)
		}
	}
}

func TestMCPRefusesGoneProject(t *testing.T) {
	r, srv, _ := mcpRig(t)
	d := &door{auth: "Bearer " + r.chatTokenOf(t, "sebastian", "gone/gone", nil)}
	if _, err := d.open(srv); err == nil || d.status != http.StatusUnauthorized || d.body != "project gone/gone is gone; make a new chat token\n" {
		t.Fatalf("open = %v, %d %q; want 401 project gone/gone is gone; make a new chat token", err, d.status, d.body)
	}
}

func TestMCPIgnoresCookie(t *testing.T) {
	r, srv, _ := mcpRig(t)
	ck, _ := r.loggedIn(t)
	d := &door{cookie: ck}
	if _, err := d.open(srv); err == nil || d.status != http.StatusUnauthorized || d.body != "unknown, expired or revoked chat token\n" {
		t.Fatalf("open with a cookie = %v, %d %q; want 401 unknown, expired or revoked chat token", err, d.status, d.body)
	}
}

func TestBearerOpensNoCookieRoute(t *testing.T) {
	r, _, bearer := mcpRig(t)
	for _, tc := range []struct {
		method, path string
		code         int
		location     string
	}{
		{http.MethodGet, "/board", http.StatusSeeOther, "/login"},
		{http.MethodGet, "/projects/ai-software-factory-code/chat", http.StatusSeeOther, "/login"},
		{http.MethodPost, "/projects/ai-software-factory-code/chat", http.StatusUnauthorized, ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := r.do(req)
		if rec.Code != tc.code || rec.Header().Get("Location") != tc.location {
			t.Errorf("%s %s with a bearer = %d to %q, want %d to %q", tc.method, tc.path, rec.Code, rec.Header().Get("Location"), tc.code, tc.location)
		}
	}
}

func TestMCPChatThreadsListsOnlyOwn(t *testing.T) {
	r, srv, bearer := mcpRig(t)
	ctx := context.Background()
	for _, m := range []struct {
		repository, login string
		msg               factoryd.ChatMessage
	}{
		{"sf/code", "sebastian", factoryd.ChatMessage{Conversation: 2, Role: "user", Content: "Fix the cache"}},
		{"sf/code", "sebastian", factoryd.ChatMessage{Conversation: 2, Role: "assistant", Content: "Filed #41."}},
		{"sf/code", "alice", factoryd.ChatMessage{Conversation: 2, Role: "user", Content: "Alice's thread"}},
		{"sf/code", "alice", factoryd.ChatMessage{Conversation: 2, Role: "assistant", Content: "Done."}},
		{"demo-app/demo-app", "sebastian", factoryd.ChatMessage{Conversation: 3, Role: "user", Content: "Another project"}},
	} {
		if _, err := r.chat.AddChatMessage(ctx, m.repository, m.login, m.msg); err != nil {
			t.Fatal(err)
		}
	}
	got := threadsOf(t, opened(t, srv, &door{auth: "Bearer " + bearer}))
	if want := `[{"number":2,"title":"Fix the cache","last":"2026-10-11T09:30:02Z","unread":true}]`; got != want {
		t.Fatalf("chat_threads = %s\nwant %s", got, want)
	}
	r.chat.mu.Lock()
	defer r.chat.mu.Unlock()
	if r.chat.seen != nil || r.chat.started != nil {
		t.Fatalf("the list marked seen %v or opened %v", r.chat.seen, r.chat.started)
	}
}

func TestMCPTouchesLastUsed(t *testing.T) {
	r, srv, bearer := mcpRig(t)
	before := time.Now()
	threadsOf(t, opened(t, srv, &door{auth: "Bearer " + bearer}))
	r.chatTokens.mu.Lock()
	defer r.chatTokens.mu.Unlock()
	if used := r.chatTokens.rows[0].LastUsed; used.Before(before) || used.After(time.Now()) {
		t.Fatalf("LastUsed = %v, want a time since %v", used, before)
	}
}

func TestTokenSessionNeverRefreshes(t *testing.T) {
	r := newRig(t)
	s := &session{user: "sebastian", access: "fj-sebastian", chatToken: 1, tokenName: "laptop"}
	tok, err := r.c.tokenFor(context.Background(), s, time.Minute)
	if err != nil || tok != "fj-sebastian" {
		t.Fatalf("tokenFor = %q, %v; want fj-sebastian", tok, err)
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if len(r.fg.grants) != 0 {
		t.Fatalf("Forgejo was asked for tokens: %v", r.fg.grants)
	}
}

func TestTokenSessionNamesRefusedForgejoToken(t *testing.T) {
	r := newRig(t)
	r.fg.tokens = map[string]string{"fj-live": "sebastian"}
	var me struct{ Login string }
	const want = "the Forgejo token behind chat token laptop was refused; make a new chat token"

	s := &session{user: "sebastian", access: "fj-deleted", chatToken: 1, tokenName: "laptop"}
	err := r.c.as(context.Background(), s, http.MethodGet, "/api/v1/user", nil, http.StatusOK, &me)
	if err == nil || err.Error() != want {
		t.Fatalf("as on a chat token = %v, want %q", err, want)
	}
	if got := (&turn{c: r.c, a: actor{s: s}}).sentence(&forgejoRefusal{"read the user", err}); got != want {
		t.Fatalf("the chat reads %q, want %q", got, want)
	}

	cookie := &session{user: "sebastian", access: "fj-deleted", expiry: time.Now().Add(time.Hour)}
	err = r.c.as(context.Background(), cookie, http.MethodGet, "/api/v1/user", nil, http.StatusOK, &me)
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != http.StatusUnauthorized {
		t.Fatalf("as on a cookie session = %v, want Forgejo's 401", err)
	}
}

// slowChat is the rig's chat store whose thread list takes two seconds.
type slowChat struct{ *fakeChat }

func (s slowChat) Conversations(ctx context.Context, repository, login string) ([]factoryd.ChatConversation, error) {
	time.Sleep(2 * time.Second)
	return s.fakeChat.Conversations(ctx, repository, login)
}

func TestMCPOutlivesWriteTimeout(t *testing.T) {
	r := newRig(t)
	cfg := r.c.cfg
	cfg.Chat = slowChat{r.chat}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(c)
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	bearer := r.chatTokenOf(t, "sebastian", "sf/code", nil)
	if got := threadsOf(t, opened(t, srv, &door{auth: "Bearer " + bearer})); got != "[]" {
		t.Fatalf("chat_threads past the write timeout = %q, want []", got)
	}
}
