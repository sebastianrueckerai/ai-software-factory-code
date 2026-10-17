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
// page asks for identify only and returns to the console; the callback's
// code becomes the member's DM channel, the bind replaces an earlier
// one, a member sharing no server with the bot is told to join, and a
// refused call never names the client secret or the bot token.
func TestDiscord(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	refuseDM := false
	dms, msgs := 0, 0
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
			t.Errorf("the connect joins a member: %s %s", r.Method, r.URL.Path)
		case r.Method == "POST" && r.URL.Path == "/users/@me/channels":
			b, _ := io.ReadAll(r.Body)
			if refuseDM {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Missing Access"}`)
				return
			}
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

	d := &Discord{ClientID: "cid", ClientSecret: "s3cret", BotToken: "bot-token",
		BaseURL: api.URL, PublicURL: "https://console.example", Store: s}
	u, err := url.Parse(d.AuthorizeURL("st-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != "https://discord.com/oauth2/authorize" ||
		q.Get("client_id") != "cid" || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != "https://console.example/messengers/discord/callback" ||
		q.Get("scope") != "identify" || q.Get("state") != "st-1" {
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
		`dm Bot bot-token {"recipient_id":"42"}`})

	// a second connect replaces the link
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
		`dm Bot bot-token {"recipient_id":"42"}`})

	// a member sharing no server with the bot is told to join it, not
	// to retry
	mu.Lock()
	calls = nil
	refuseDM = true
	mu.Unlock()
	if err = d.Connect(ctx, "sebastian", "abc"); !errors.Is(err, ErrNoSharedServer) {
		t.Fatalf("the dm without a shared server = %v", err)
	}
	mu.Lock()
	refuseDM = false
	mu.Unlock()

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

// recordingTrips notes the URL of the one call it serves.
type recordingTrips struct{ url string }

func (f *recordingTrips) RoundTrip(r *http.Request) (*http.Response, error) {
	f.url = r.URL.String()
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
}

// The default REST base carries the /api prefix: Discord serves its API
// under discord.com/api and its browser pages under discord.com. A fake
// server in a test carries its own path and cannot catch a wrong default.
func TestDiscordDefaultBase(t *testing.T) {
	trips := &recordingTrips{}
	d := &Discord{Client: &http.Client{Transport: trips}}
	var out struct {
		ID string `json:"id"`
	}
	if err := d.call(context.Background(), "GET", "/users/@me", "", "Bearer x", nil, &out); err != nil {
		t.Fatal(err)
	}
	if trips.url != "https://discord.com/api/users/@me" {
		t.Fatalf("the default base = %s", trips.url)
	}
}
