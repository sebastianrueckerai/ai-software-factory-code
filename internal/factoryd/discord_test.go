package factoryd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// Discord's connect flow (roadmap 145) against a fake API: the authorize
// page asks for identify and guilds.join and returns to the console; the
// callback's code becomes the member's DM channel after the guild gains
// them, the bind replaces an earlier one, and a refused call never names
// the client secret or the bot token.
func TestDiscord(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	dms, joins, msgs := 0, 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/oauth2/token":
			_ = r.ParseForm()
			calls = append(calls, fmt.Sprintf("token %v %v", r.PostForm.Get("grant_type"), r.PostForm.Get("code")))
			fmt.Fprint(w, `{"access_token":"user-token"}`)
		case r.Method == "GET" && r.URL.Path == "/users/@me":
			calls = append(calls, "identify "+r.Header.Get("Authorization"))
			fmt.Fprint(w, `{"id":"42","username":"ste"}`)
		case r.Method == "PUT" && r.URL.Path == "/guilds/g-1/members/42":
			b, _ := io.ReadAll(r.Body)
			joins++
			calls = append(calls, fmt.Sprintf("join %v %s %d", r.Header.Get("Authorization"), strings.TrimSpace(string(b)), joins))
			if joins > 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == "POST" && r.URL.Path == "/users/@me/channels":
			b, _ := io.ReadAll(r.Body)
			dms++
			calls = append(calls, fmt.Sprintf("dm %v %s", r.Header.Get("Authorization"), strings.TrimSpace(string(b))))
			fmt.Fprintf(w, `{"id":"dm-%d"}`, dms)
		case r.Method == "POST" && r.URL.Path == "/channels/dm-1/messages":
			b, _ := io.ReadAll(r.Body)
			msgs++
			calls = append(calls, fmt.Sprintf("say %v %s", r.Header.Get("Authorization"), strings.TrimSpace(string(b))))
			fmt.Fprintf(w, `{"id":"m-%d"}`, msgs)
		case r.Method == "PATCH" && r.URL.Path == "/channels/dm-1/messages/m-1":
			b, _ := io.ReadAll(r.Body)
			calls = append(calls, fmt.Sprintf("edit %v %s", r.Header.Get("Authorization"), strings.TrimSpace(string(b))))
			fmt.Fprint(w, `{"id":"m-1"}`)
		case r.Method == "POST" && r.URL.Path == "/channels/rated/messages":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"You are being rate limited.","retry_after":5}`)
		default:
			t.Errorf("the unexpected call %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	dsn := schematest.DSN(t, "discord")
	ctx := context.Background()
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.db.Close()
	if _, err := s.db.ExecContext(ctx, `TRUNCATE messenger_codes, messenger_links, notify_levels, notify_deliveries CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	d := &Discord{ClientID: "cid", ClientSecret: "s3cret", BotToken: "bot-token", GuildID: "g-1",
		BaseURL: api.URL, PublicURL: "https://console.example", Store: s}
	u, err := url.Parse(d.AuthorizeURL("st-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != "https://discord.com/oauth2/authorize" ||
		q.Get("client_id") != "cid" || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != "https://console.example/messengers/discord/callback" ||
		q.Get("scope") != "identify guilds.join" || q.Get("state") != "st-1" {
		t.Fatalf("the authorize page = %q", d.AuthorizeURL("st-1"))
	}

	if err := d.Connect(ctx, "sebastian", "abc"); err != nil {
		t.Fatal(err)
	}
	links, err := s.MessengerLinks(ctx, "sebastian")
	if err != nil || len(links) != 1 || links[0].Venue != VenueDiscord || links[0].Address != "dm-1" || links[0].VenueUser != "42" {
		t.Fatalf("the bind = %+v, %v", links, err)
	}
	equalStrings(t, "the first connect", calls, []string{
		`token authorization_code abc`, "identify Bearer user-token",
		`join Bot bot-token {"access_token":"user-token"} 1`,
		`dm Bot bot-token {"recipient_id":"42"}`})

	// a second connect replaces the link, and a member already in the
	// guild joins no more
	mu.Lock()
	calls = nil
	mu.Unlock()
	if err := d.Connect(ctx, "sebastian", "zzz"); err != nil {
		t.Fatal(err)
	}
	if links, _ = s.MessengerLinks(ctx, "sebastian"); len(links) != 1 || links[0].Address != "dm-2" {
		t.Fatalf("after the second connect = %+v", links)
	}
	equalStrings(t, "the second connect", calls, []string{
		`token authorization_code zzz`, "identify Bearer user-token",
		`join Bot bot-token {"access_token":"user-token"} 2`,
		`dm Bot bot-token {"recipient_id":"42"}`})

	// a notification carries the card's link, its ref edits it, and a
	// rate limit names its wait
	mu.Lock()
	calls = nil
	mu.Unlock()
	ref, err := d.Send(ctx, "dm-1", "api: the card asks a question", "https://console.example/cards/T-1")
	if err != nil || ref != "m-1" {
		t.Fatalf("the send = %q, %v", ref, err)
	}
	if err := d.Edit(ctx, "dm-1", ref, "api: answered by sebastian", "https://console.example/cards/T-1"); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "the notification", calls, []string{
		`say Bot bot-token {"components":[{"components":[{"label":"Open the card","style":5,"type":2,"url":"https://console.example/cards/T-1"}],"type":1}],"content":"api: the card asks a question"}`,
		`edit Bot bot-token {"components":[{"components":[{"label":"Open the card","style":5,"type":2,"url":"https://console.example/cards/T-1"}],"type":1}],"content":"api: answered by sebastian"}`})
	_, err = d.Send(ctx, "rated", "x", "https://console.example/cards/T-1")
	var de *DiscordError
	if !errors.As(err, &de) || de.Status != 429 || de.RetryAfter != 7 || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("the rate limit = %v", err)
	}

	// a refused exchange never names the secrets
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer refused.Close()
	d.BaseURL = refused.URL
	err = d.Connect(ctx, "sebastian", "abc")
	if err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "bot-token") {
		t.Fatalf("the refused connect = %v", err)
	}
}
