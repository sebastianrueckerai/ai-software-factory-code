package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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

// callTool is session's call of tool with args: its text, and whether
// the tool refused.
func callTool(session *mcp.ClientSession, tool string, args map[string]any) (string, bool, error) {
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", false, err
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError, nil
}

// answerOf is tool's answer to args on session; the test fails when the
// tool refuses.
func answerOf(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	text, refused, err := callTool(session, tool, args)
	if err != nil || refused {
		t.Fatalf("%s(%v) = %q, %v", tool, args, text, err)
	}
	return text
}

// threadsOf is chat_threads' answer on session.
func threadsOf(t *testing.T, session *mcp.ClientSession) string {
	t.Helper()
	return answerOf(t, session, "chat_threads", map[string]any{})
}

// tokenSession serves r and opens sebastian's chat token on sf/code.
func tokenSession(t *testing.T, r *rig) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(r.c)
	t.Cleanup(srv.Close)
	return opened(t, srv, &door{auth: "Bearer " + r.chatTokenOf(t, "sebastian", "sf/code", nil)})
}

// rowsOf is login's rows of conversation n on sf/code as "role: content".
func rowsOf(t *testing.T, chat *fakeChat, login string, n int64) []string {
	t.Helper()
	rows, err := chat.ChatMessages(t.Context(), "sf/code", login, n, 50)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, m := range rows {
		out = append(out, m.Role+": "+m.Content)
	}
	return out
}

func TestMCPRefusesWithoutValidBearer(t *testing.T) {
	r, srv, _ := mcpRig(t)
	unknown, _, err := newChatToken()
	if err != nil {
		t.Fatal(err)
	}
	expired := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Expires = time.Now().Add(-time.Minute) })
	revoked := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Revoked = time.Now().Add(-time.Minute) })
	foreign, err := sealed.Seal(r.c.cfg.SessionKey, chatTokenKeyLabel, "fj-sebastian", "another hash")
	if err != nil {
		t.Fatal(err)
	}
	misSealed := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Sealed = foreign })
	unsealed := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Sealed = "" })
	for _, tc := range []struct{ name, auth string }{
		{"none", ""}, {"unknown", "Bearer " + unknown}, {"expired", "Bearer " + expired}, {"revoked", "Bearer " + revoked},
		{"sealed under another hash", "Bearer " + misSealed}, {"sealed empty", "Bearer " + unsealed},
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

// deadlined is a recorder whose write deadline moves, as a server
// connection's does.
type deadlined struct{ *httptest.ResponseRecorder }

func (deadlined) SetWriteDeadline(time.Time) error { return nil }

// A pasted bearer keeps its doubled space and its line end, which no
// client sends over the wire, so the request is served in process.
func TestMCPTrimsPastedBearer(t *testing.T) {
	r, _, bearer := mcpRig(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chat_threads","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer  "+bearer+"\n")
	rec := httptest.NewRecorder()
	r.c.ServeHTTP(deadlined{rec}, req)
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

// The writer check comes before the seal opens: a former writer's token
// with no Forgejo token behind it gets the writer's refusal too.
func TestMCPRefusesFormerWriter(t *testing.T) {
	r, srv, bearer := mcpRig(t)
	unsealed := r.chatTokenOf(t, "sebastian", "sf/code", func(row *factoryd.ChatToken) { row.Sealed = "" })
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
		for _, b := range []string{bearer, unsealed} {
			d := &door{auth: "Bearer " + b}
			if _, err := d.open(srv); err == nil || d.status != http.StatusForbidden || d.body != "sebastian no longer writes sf/code\n" {
				t.Errorf("%s: open = %v, %d %q; want 403 sebastian no longer writes sf/code", tc.name, err, d.status, d.body)
			}
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

// mcpHandler mounted without bearer serves no tool: its server needs the
// token's call.
func TestMCPServesNothingWithoutBearer(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chat_threads","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	r.c.mcpHandler().ServeHTTP(deadlined{rec}, req)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != "no server available\n" {
		t.Fatalf("POST /mcp without bearer = %d %q, want 400 no server available", rec.Code, rec.Body.String())
	}
}

// sent is the answer chat_send gives when nothing but its reply happened.
func sent(conversation int64, reply, failure string, ended bool) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return fmt.Sprintf(`{"conversation":%d,"reply":%s,"tools":[],"proposals":[],"applied":[],"budget":"","error":%s,"ended":%t}`,
		conversation, q(reply), q(failure), ended)
}

func TestMCPChatSendAnswersReply(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("hel"), say("lo")}}
	session := tokenSession(t, r)
	if got, want := answerOf(t, session, "chat_send", map[string]any{"message": "hi"}),
		`{"conversation":2,"reply":"hello","tools":[],"proposals":[],"applied":[],"budget":"","error":"","ended":true}`; got != want {
		t.Fatalf("chat_send = %s\nwant %s", got, want)
	}
	equal(t, "the side thread", rowsOf(t, chat, "sebastian", 2), []string{"user: hi", "assistant: hello"})
	if cur, _ := chat.CurrentConversation(t.Context(), "sf/code", "sebastian"); cur != 1 {
		t.Fatalf("the current conversation = %d, want 1 still", cur)
	}
	list, _ := chat.Conversations(t.Context(), "sf/code", "sebastian")
	if len(list) != 1 || list[0].Number != 2 || list[0].Title != "laptop" || list[0].Current {
		t.Fatalf("the threads = %+v, want the token's thread 2 titled laptop, not current", list)
	}
	r.chatTokens.mu.Lock()
	defer r.chatTokens.mu.Unlock()
	if got := r.chatTokens.rows[0].Conversation; got != 2 {
		t.Fatalf("the token's conversation = %d, want 2", got)
	}
}

// racedChat is the rig's chat store whose side threads each wait for
// the other caller's, so two first turns both make one before either
// claims.
type racedChat struct {
	*fakeChat
	both *sync.WaitGroup
}

func (s racedChat) SideConversation(ctx context.Context, repository, login, title string) (int64, error) {
	n, err := s.fakeChat.SideConversation(ctx, repository, login, title)
	s.both.Done()
	s.both.Wait()
	return n, err
}

func TestMCPChatSendFirstTurnsShareOneThread(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("one")}, {say("two")}}
	cfg := r.c.cfg
	both := &sync.WaitGroup{}
	both.Add(2)
	cfg.Chat = racedChat{chat, both}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	session := tokenSession(t, r)
	answers := make(chan string, 2)
	for _, msg := range []string{"first", "second"} {
		go func() {
			text, refused, err := callTool(session, "chat_send", map[string]any{"message": msg})
			if err != nil || refused {
				t.Errorf("chat_send %s = %q, %v", msg, text, err)
			}
			answers <- text
		}()
	}
	var convs []int64
	for range 2 {
		var a struct{ Conversation int64 }
		if err := json.Unmarshal([]byte(<-answers), &a); err != nil {
			t.Fatal(err)
		}
		convs = append(convs, a.Conversation)
	}
	list, _ := chat.Conversations(t.Context(), "sf/code", "sebastian")
	if convs[0] != convs[1] || len(list) != 1 || list[0].Number != convs[0] || list[0].Title != "laptop" {
		t.Fatalf("the answers name threads %v, the human has %+v; want one thread laptop that both name", convs, list)
	}
	rows := rowsOf(t, chat, "sebastian", convs[0])
	slices.Sort(rows)
	if !slices.Contains(rows, "user: first") || !slices.Contains(rows, "user: second") {
		t.Fatalf("thread %d holds %q, want both messages", convs[0], rows)
	}
}

func TestMCPChatSendRaisesProposal(t *testing.T) {
	r, g := newWritesRig(t)
	args := `{"task_id":"t-b","depends_on":"t-a"}`
	g.replies = [][]string{{call(1, "set_dependency", args)}}
	session := tokenSession(t, r)
	before := time.Now()
	var a struct {
		Proposals []struct {
			ID, Tool, What string
			Expires        time.Time
		}
		Tools, Applied []any
		Reply          string
		Ended          bool
	}
	if err := json.Unmarshal([]byte(answerOf(t, session, "chat_send", map[string]any{"message": "#22 waits on #21"})), &a); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if len(a.Proposals) != 1 || len(a.Tools) != 0 || len(a.Applied) != 0 || a.Reply != "" || !a.Ended {
		t.Fatalf("chat_send = %+v, want one proposal, no tool, nothing applied, ended", a)
	}
	p := a.Proposals[0]
	if p.Tool != "set_dependency" || p.What != `Set a dependency, #22 t-b "Add the web UI.", depends on #21 t-a "Add the API."` ||
		p.Expires.Before(before.Add(30*time.Minute)) || p.Expires.After(after.Add(30*time.Minute)) {
		t.Fatalf("the proposal = %+v, want set_dependency's, expiring 30 minutes on", p)
	}
	stored, ok := r.chat.proposal(p.ID)
	if !ok || stored.Login != "sebastian" || stored.Conversation != 2 || !stored.used.IsZero() {
		t.Fatalf("the stored proposal = %+v (%v), want sebastian's unused one in thread 2", stored, ok)
	}
	if len(r.issueDeps) != 0 {
		t.Fatalf("a proposal acted: %q", r.issueDeps)
	}
	if got, want := answerOf(t, session, "chat_read", map[string]any{"after": 1}),
		`[{"id":2,"role":"assistant","text":"","at":"2026-10-11T09:30:02Z","source":"console","proposal":"`+p.ID+`","state":"pending"}]`; got != want {
		t.Fatalf("chat_read = %s\nwant %s", got, want)
	}
}

func TestMCPChatSendRefusesForeignConversation(t *testing.T) {
	r, chat, g := newChatRig(t)
	if _, err := chat.AddChatMessage(t.Context(), "sf/code", "alice", factoryd.ChatMessage{Conversation: 2, Role: "user", Content: "Alice's thread"}); err != nil {
		t.Fatal(err)
	}
	session := tokenSession(t, r)
	for tool, args := range map[string]map[string]any{
		"chat_send": {"message": "hi", "conversation": 2},
		"chat_read": {"conversation": 2},
	} {
		if text, refused, err := callTool(session, tool, args); err != nil || !refused || text != "conversation 2 is not yours in sf/code" {
			t.Errorf("%s on alice's thread = %q refused %v, %v; want conversation 2 is not yours in sf/code", tool, text, refused, err)
		}
	}
	equal(t, "sebastian's rows", rowsOf(t, chat, "sebastian", 2), []string{})
	equal(t, "alice's rows", rowsOf(t, chat, "alice", 2), []string{"user: Alice's thread"})
	if n := len(g.calls()); n != 0 {
		t.Fatalf("%d model calls, want none", n)
	}
}

func TestMCPChatSendOutlastsWait(t *testing.T) {
	r, _, g := newChatRig(t)
	wait := chatSendWait
	chatSendWait = 50 * time.Millisecond
	t.Cleanup(func() { chatSendWait = wait })
	g.replies = [][]string{{say("late hello")}}
	g.hold = make(chan struct{})
	session := tokenSession(t, r)
	if got := answerOf(t, session, "chat_send", map[string]any{"message": "hi"}); got != sent(2, "", "", false) {
		t.Fatalf("chat_send = %s\nwant %s", got, sent(2, "", "", false))
	}
	close(g.hold)
	want := `[{"id":1,"role":"user","text":"hi","at":"2026-10-11T09:30:01Z","source":"console"},` +
		`{"id":2,"role":"assistant","text":"late hello","at":"2026-10-11T09:30:02Z","source":"console"}]`
	got := ""
	for deadline := time.Now().Add(5 * time.Second); got != want && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got = answerOf(t, session, "chat_read", map[string]any{})
	}
	if got != want {
		t.Fatalf("chat_read after the turn = %s\nwant %s", got, want)
	}
}

func TestMCPChatSendFailureLandsAsRow(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{`STATUS 500 {"error":{"message":"upstream exploded"}}`}}
	const failed = "The model could not be reached; try again."
	if got := answerOf(t, tokenSession(t, r), "chat_send", map[string]any{"message": "hi"}); got != sent(2, "", failed, true) {
		t.Fatalf("chat_send = %s\nwant %s", got, sent(2, "", failed, true))
	}
	equal(t, "the thread", rowsOf(t, chat, "sebastian", 2), []string{"user: hi", "assistant: This turn failed: " + failed})
}

func TestMCPChatSendBudgetStop(t *testing.T) {
	r, chat, g := newChatRig(t)
	const reason = "free headroom 0.000000 EUR (day 20.000000 of 20.000000); the day cap budget.day_eur in policy.yaml is spent, and only a human raises it, by a commit"
	g.replies = [][]string{{refuseFor(policy.RefusedDayCap, reason)}}
	stop := "The factory's day cap is used up: the chat goes on when the UTC day turns, or once a commit raises budget.day_eur in policy.yaml.\n" + reason
	var a struct {
		Budget, Error string
		Ended         bool
	}
	if err := json.Unmarshal([]byte(answerOf(t, tokenSession(t, r), "chat_send", map[string]any{"message": "hi"})), &a); err != nil {
		t.Fatal(err)
	}
	if a.Budget != stop || a.Error != "" || !a.Ended {
		t.Fatalf("chat_send = %+v, want the stop %q, ended", a, stop)
	}
	equal(t, "the thread", rowsOf(t, chat, "sebastian", 2), []string{"user: hi", "assistant: " + stop})
}

func TestMCPChatSendRevokedMidTurn(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("still here")}}
	g.hold, g.arrived = make(chan struct{}), make(chan struct{}, 1)
	srv := httptest.NewServer(r.c)
	t.Cleanup(srv.Close)
	d := &door{auth: "Bearer " + r.chatTokenOf(t, "sebastian", "sf/code", nil)}
	session := opened(t, srv, d)
	answer := make(chan string, 1)
	go func() {
		text, refused, err := callTool(session, "chat_send", map[string]any{"message": "hi"})
		if err != nil || refused {
			t.Errorf("chat_send = %q, %v", text, err)
		}
		answer <- text
	}()
	select {
	case <-g.arrived:
	case got := <-answer:
		t.Fatalf("chat_send answered %q before its model call", got)
	}
	if ok, err := r.chatTokens.RevokeChatToken(t.Context(), 1, "sebastian", time.Now()); !ok || err != nil {
		t.Fatalf("revoke = %v, %v", ok, err)
	}
	close(g.hold)
	if got := <-answer; got != sent(2, "still here", "", true) {
		t.Fatalf("chat_send across the revoke = %s\nwant %s", got, sent(2, "still here", "", true))
	}
	equal(t, "the thread", rowsOf(t, chat, "sebastian", 2), []string{"user: hi", "assistant: still here"})
	if _, _, err := callTool(session, "chat_threads", map[string]any{}); err == nil || d.status != http.StatusUnauthorized {
		t.Fatalf("the call after the revoke = %v, %d; want 401", err, d.status)
	}
}

func TestMCPChatReadNeverMarksSeen(t *testing.T) {
	r, chat, g := newChatRig(t)
	g.replies = [][]string{{say("hello")}}
	session := tokenSession(t, r)
	answerOf(t, session, "chat_send", map[string]any{"message": "hi"})
	if got := answerOf(t, session, "chat_read", map[string]any{}); !strings.Contains(got, `"text":"hello"`) {
		t.Fatalf("chat_read = %s, want the reply", got)
	}
	answerOf(t, session, "chat_read", map[string]any{"conversation": 2})
	list, _ := chat.Conversations(t.Context(), "sf/code", "sebastian")
	if len(list) != 1 || list[0].Unread != 1 {
		t.Fatalf("the threads = %+v, want the token's thread unread", list)
	}
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if chat.seen != nil || chat.started != nil {
		t.Fatalf("the token marked seen %v or opened or made current %v", chat.seen, chat.started)
	}
}

func TestMCPChatReadSource(t *testing.T) {
	r, srv, bearer := mcpRig(t)
	for _, m := range []factoryd.ChatMessage{
		{Conversation: 1, Role: "user", Content: "The current thread"},
		{Conversation: 2, Role: "user", Content: "From the laptop", Token: "laptop"},
		{Conversation: 2, Role: "user", Content: "From the browser"},
	} {
		if _, err := r.chat.AddChatMessage(t.Context(), "sf/code", "sebastian", m); err != nil {
			t.Fatal(err)
		}
	}
	session := opened(t, srv, &door{auth: "Bearer " + bearer})
	if got := answerOf(t, session, "chat_read", map[string]any{}); got != "[]" {
		t.Fatalf("chat_read before the token's first turn = %s, want []", got)
	}
	if got, want := answerOf(t, session, "chat_read", map[string]any{"conversation": 2}),
		`[{"id":2,"role":"user","text":"From the laptop","at":"2026-10-11T09:30:02Z","source":"laptop"},`+
			`{"id":3,"role":"user","text":"From the browser","at":"2026-10-11T09:30:03Z","source":"console"}]`; got != want {
		t.Fatalf("chat_read = %s\nwant %s", got, want)
	}
	if got, want := answerOf(t, session, "chat_read", map[string]any{"conversation": 2, "after": 2}),
		`[{"id":3,"role":"user","text":"From the browser","at":"2026-10-11T09:30:03Z","source":"console"}]`; got != want {
		t.Fatalf("chat_read after 2 = %s\nwant %s", got, want)
	}
}
