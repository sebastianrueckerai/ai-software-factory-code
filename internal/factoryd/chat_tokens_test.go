package factoryd

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

func chatTokenStore(t *testing.T) *PGStore {
	t.Helper()
	s, err := OpenStore(context.Background(), schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	if _, err := s.db.Exec(`TRUNCATE chat_tokens, chat_messages, chat_conversations CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func newChatToken(hash, login string) ChatToken {
	return ChatToken{Hash: hash, Login: login, Repository: "acme/app", Name: "laptop", Sealed: "sealed-" + hash,
		Expires: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func addChatToken(t *testing.T, s *PGStore, hash, login string) int64 {
	t.Helper()
	id, err := s.AddChatToken(context.Background(), newChatToken(hash, login))
	if err != nil {
		t.Fatalf("AddChatToken %s: %v", hash, err)
	}
	return id
}

func TestChatTokenRefusesDuplicateHash(t *testing.T) {
	s := chatTokenStore(t)
	addChatToken(t, s, "h1", "sebastian")
	if _, err := s.AddChatToken(context.Background(), newChatToken("h1", "bob")); err == nil {
		t.Error("a second token with the same hash was stored")
	}
}

func TestChatTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	in := newChatToken("h1", "sebastian")
	id, err := s.AddChatToken(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.ChatTokenByHash(ctx, "h1")
	if err != nil || !ok {
		t.Fatalf("ChatTokenByHash: %v, %v", ok, err)
	}
	if got.ID != id || got.Hash != "h1" || got.Login != "sebastian" || got.Repository != "acme/app" || got.Name != "laptop" ||
		got.Sealed != "sealed-h1" || got.Conversation != 0 || !got.Expires.Equal(in.Expires) {
		t.Errorf("token = %+v", got)
	}
	if got.Created.IsZero() || !got.LastUsed.IsZero() || !got.Revoked.IsZero() {
		t.Errorf("created %v, last used %v, revoked %v", got.Created, got.LastUsed, got.Revoked)
	}
}

func TestChatTokenByHashMissesUnknown(t *testing.T) {
	s := chatTokenStore(t)
	_, ok, err := s.ChatTokenByHash(context.Background(), "nope")
	if ok || err != nil {
		t.Errorf("ok %v, err %v", ok, err)
	}
}

func TestRevokeChatTokenOnlyOwn(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	id := addChatToken(t, s, "h1", "sebastian")
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	if ok, err := s.RevokeChatToken(ctx, id, "bob", now); ok || err != nil {
		t.Fatalf("bob revoked: %v, %v", ok, err)
	}
	if got, _, _ := s.ChatTokenByHash(ctx, "h1"); !got.Revoked.IsZero() {
		t.Errorf("revoked at %v", got.Revoked)
	}
	if ok, err := s.RevokeChatToken(ctx, id, "sebastian", now); !ok || err != nil {
		t.Fatalf("own revoke: %v, %v", ok, err)
	}
	if ok, _ := s.RevokeChatToken(ctx, id, "sebastian", now); ok {
		t.Error("second revoke answered true")
	}
	if got, _, _ := s.ChatTokenByHash(ctx, "h1"); !got.Revoked.Equal(now) {
		t.Errorf("revoked at %v, want %v", got.Revoked, now)
	}
}

func TestChatTokensOfLeavesOutRevoked(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	a := addChatToken(t, s, "h1", "sebastian")
	b := addChatToken(t, s, "h2", "sebastian")
	addChatToken(t, s, "h3", "bob")
	got, err := s.ChatTokensOf(ctx, "sebastian")
	if err != nil || len(got) != 2 || got[0].ID != b || got[1].ID != a {
		t.Fatalf("newest first: tokens = %+v, %v", got, err)
	}
	if _, err := s.RevokeChatToken(ctx, a, "sebastian", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.ChatTokensOf(ctx, "sebastian")
	if err != nil || len(got) != 1 || got[0].ID != b {
		t.Errorf("tokens = %+v, %v", got, err)
	}
}

func TestTouchChatTokenAtMostOncePerMinute(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	id := addChatToken(t, s, "h1", "sebastian")
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct{ at, want time.Duration }{{0, 0}, {30 * time.Second, 0}, {61 * time.Second, 61 * time.Second}} {
		if err := s.TouchChatToken(ctx, id, t0.Add(c.at)); err != nil {
			t.Fatal(err)
		}
		got, _, _ := s.ChatTokenByHash(ctx, "h1")
		if !got.LastUsed.Equal(t0.Add(c.want)) {
			t.Errorf("after touch at +%v: last used %v, want +%v", c.at, got.LastUsed, c.want)
		}
	}
}

func TestClaimChatTokenConversationFirstWins(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	id := addChatToken(t, s, "h1", "sebastian")
	for _, n := range []int64{7, 9} {
		if got, err := s.ClaimChatTokenConversation(ctx, id, n); err != nil || got != 7 {
			t.Errorf("claim %d = %d, %v; want 7", n, got, err)
		}
	}
}

func TestSideConversationKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	for _, c := range []struct {
		n   int64
		cur bool
	}{{3, true}, {4, false}} {
		if _, err := s.db.Exec(`INSERT INTO chat_conversations (repository, login, conversation, current) VALUES ('acme/app', 'sebastian', $1, $2)`, c.n, c.cur); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SideConversation(ctx, "acme/app", "sebastian", "laptop")
	if err != nil || n != 5 {
		t.Fatalf("side = %d, %v; want 5", n, err)
	}
	if cur, _ := s.CurrentConversation(ctx, "acme/app", "sebastian"); cur != 3 {
		t.Errorf("current = %d, want 3", cur)
	}
	var title string
	if err := s.db.QueryRow(`SELECT title FROM chat_conversations WHERE repository = 'acme/app' AND login = 'sebastian' AND conversation = 5`).Scan(&title); err != nil || title != "laptop" {
		t.Errorf("title = %q, %v", title, err)
	}
}

func TestSideConversationSkipsLegacyOne(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	if _, err := s.db.Exec(`INSERT INTO chat_messages (repository, login, role, content) VALUES ('acme/app', 'sebastian', 'user', 'old')`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SideConversation(ctx, "acme/app", "sebastian", "laptop"); err != nil || n != 2 {
		t.Errorf("side = %d, %v; want 2", n, err)
	}
}

func TestSideConversationConcurrentCallersGetTwoThreads(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	got := make(chan int64, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			n, err := s.SideConversation(ctx, "acme/app", "sebastian", "laptop")
			got <- n
			errs <- err
		}()
	}
	a, b := <-got, <-got
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if a == b || a+b != 5 {
		t.Errorf("threads %d and %d, want 2 and 3", a, b)
	}
}

func TestChatMessageTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	if _, err := s.AddChatMessage(ctx, "acme/app", "sebastian", ChatMessage{Role: "user", Content: "a", Token: "laptop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChatMessage(ctx, "acme/app", "sebastian", ChatMessage{Role: "user", Content: "b"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ChatMessages(ctx, "acme/app", "sebastian", 0, 10)
	if err != nil || len(got) != 2 || got[0].Token != "laptop" || got[1].Token != "" {
		t.Errorf("messages = %+v, %v", got, err)
	}
}

func TestDeleteConversationFreesItsChatToken(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	own := addChatToken(t, s, "h1", "sebastian")
	other := addChatToken(t, s, "h2", "sebastian")
	alices := addChatToken(t, s, "h3", "alice")
	elsewhere := newChatToken("h4", "sebastian")
	elsewhere.Repository = "acme/web"
	elsewhereID, err := s.AddChatToken(ctx, elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id, n             int64
		repository, login string
	}{{own, 2, "acme/app", "sebastian"}, {other, 3, "acme/app", "sebastian"}, {alices, 2, "acme/app", "alice"}, {elsewhereID, 2, "acme/web", "sebastian"}} {
		if _, err := s.AddChatMessage(ctx, c.repository, c.login, ChatMessage{Role: "user", Content: "hi", Conversation: c.n}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimChatTokenConversation(ctx, c.id, c.n); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.DeleteConversation(ctx, "acme/app", "sebastian", 2); err != nil {
		t.Fatal(err)
	}
	for hash, want := range map[string]int64{"h1": 0, "h2": 3, "h3": 2, "h4": 2} {
		if got, _, err := s.ChatTokenByHash(ctx, hash); err != nil || got.Conversation != want {
			t.Errorf("token %s's conversation = %d, %v; want %d", hash, got.Conversation, err, want)
		}
	}
}

func TestChatMessagesAfterPagesForward(t *testing.T) {
	ctx := context.Background()
	s := chatTokenStore(t)
	var ids []int64
	for i := range 5 {
		id, err := s.AddChatMessage(ctx, "acme/app", "sebastian", ChatMessage{Role: "user", Content: fmt.Sprint("m", i), Conversation: 2})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.AddChatMessage(ctx, "acme/app", "sebastian", ChatMessage{Role: "user", Content: "elsewhere", Conversation: 3}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		after int64
		limit int
		want  []string
	}{
		{ids[0], 2, []string{"m1", "m2"}},
		{ids[2], 2, []string{"m3", "m4"}},
		{ids[4], 2, nil},
	} {
		rows, err := s.ChatMessagesAfter(ctx, "acme/app", "sebastian", 2, c.after, c.limit)
		var got []string
		for _, m := range rows {
			got = append(got, m.Content)
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("after %d, %d rows = %q, %v; want %q", c.after, c.limit, got, err, c.want)
		}
	}
}
