package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Telegram's connect flow (roadmap 145) against a fake API: boot reads the
// bot's name and points Telegram at the console's webhook with the
// token-derived secret; /start binds a private chat with its one-time code
// and says whose it is; /stop drops the bind; a group chat and a stray
// message get nothing but the fixed line; a refused call never names the
// token.
func TestTelegram(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			calls = append(calls, "getMe")
			fmt.Fprint(w, `{"ok":true,"result":{"username":"factory_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "/setWebhook"):
			calls = append(calls, fmt.Sprintf("setWebhook %v %v", payload["url"], payload["secret_token"]))
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			line := fmt.Sprintf("sendMessage %v %v", payload["chat_id"], payload["text"])
			if markup, ok := payload["reply_markup"].(map[string]any); ok {
				btn := markup["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
				line += fmt.Sprintf(" %v %v", btn["text"], btn["url"])
			}
			calls = append(calls, line)
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":11}}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			line := fmt.Sprintf("editMessageText %v %v %v", payload["chat_id"], payload["message_id"], payload["text"])
			if markup, ok := payload["reply_markup"].(map[string]any); ok {
				btn := markup["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
				line += fmt.Sprintf(" %v %v", btn["text"], btn["url"])
			}
			calls = append(calls, line)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		default:
			t.Errorf("the unexpected call %s", r.URL.Path)
		}
	}))
	defer api.Close()

	dsn := schematest.DSN(t, "telegram")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE messenger_codes, messenger_links, notify_levels, notify_deliveries CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	tg := &Telegram{Token: "bot-key", BaseURL: api.URL, PublicURL: "https://console.example", Store: s}
	if secret := TelegramSecret("bot-key"); len(secret) != 64 || secret == TelegramSecret("other-key") {
		t.Fatalf("the secret %q is not a distinct hash of the token", secret)
	}
	if err := tg.Boot(ctx, "https://console.example"); err != nil {
		t.Fatal(err)
	}
	if tg.Name != "factory_bot" {
		t.Fatalf("the bot's name = %q", tg.Name)
	}
	equalStrings(t, "boot", calls, []string{"getMe",
		"setWebhook https://console.example/messengers/telegram " + TelegramSecret("bot-key")})

	mark := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}
	took := func(from int) []string {
		mu.Lock()
		defer mu.Unlock()
		return calls[from:]
	}
	update := func(chat int64, chatType, text string) error {
		body, err := json.Marshal(map[string]any{"message": map[string]any{
			"text": text, "chat": map[string]any{"id": chat, "type": chatType}, "from": map[string]any{"id": 7}}})
		if err != nil {
			t.Fatal(err)
		}
		return tg.Update(ctx, body)
	}

	code, err := s.IssueMessengerCode(ctx, "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	n := mark()
	if err := update(4242, "private", "/start "+code); err != nil {
		t.Fatal(err)
	}
	links, err := s.MessengerLinks(ctx, "sebastian")
	if err != nil || len(links) != 1 || links[0].Venue != VenueTelegram || links[0].Address != "4242" || links[0].VenueUser != "7" {
		t.Fatalf("the bind = %+v, %v", links, err)
	}
	equalStrings(t, "the connect", took(n), []string{"sendMessage 4242 Connected to sebastian. Opt in per project in the console."})

	n = mark()
	if err := update(99, "private", "/start nope"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "the bad code", took(n), []string{"sendMessage 99 That code is missing, used or expired. Get a new one in the console."})

	if err := update(500, "group", "/start "+code); err != nil {
		t.Fatal(err)
	}
	n = mark()
	if err := update(4242, "private", "hello?"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "the group and the stray message", took(n),
		[]string{"sendMessage 4242 This bot only carries notifications; it never reads replies. The console: https://console.example"})

	n = mark()
	if err := update(4242, "private", "/stop"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "the stop", took(n), []string{"sendMessage 4242 Disconnected."})
	if links, _ = s.MessengerLinks(ctx, "sebastian"); len(links) != 0 {
		t.Fatalf("after the stop = %+v", links)
	}

	// a notification carries the card's link, and its ref edits it later
	n = mark()
	ref, err := tg.Send(ctx, "4242", "api: the card asks a question", "https://console.example/cards/T-1")
	if err != nil || ref != "11" {
		t.Fatalf("the send = %q, %v", ref, err)
	}
	if err := tg.Edit(ctx, "4242", ref, "api: answered by sebastian", "https://console.example/cards/T-1"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "the notification", took(n), []string{
		"sendMessage 4242 api: the card asks a question Open the card https://console.example/cards/T-1",
		"editMessageText 4242 11 api: answered by sebastian Open the card https://console.example/cards/T-1"})

	// a refused call never names the token
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer refused.Close()
	tg.BaseURL = refused.URL
	if err := tg.send(ctx, "4242", "x"); err == nil || strings.Contains(err.Error(), "bot-key") {
		t.Fatalf("the refused call = %v", err)
	}
}
