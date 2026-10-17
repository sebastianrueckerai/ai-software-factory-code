package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Telegram's webhook route (roadmap 145): an empty access rule, the
// header secret checked in constant time before the body is read, the
// body handed to the update handler, and 200 always after the check so
// Telegram does not retry; without the configuration the route is not
// there.
func TestTelegramWebhook(t *testing.T) {
	r := newRig(t)
	var got []byte
	cfg := r.c.cfg
	cfg.Messengers = &Messengers{Secret: "s3cret", Update: func(_ context.Context, body []byte) error {
		got = body
		return nil
	}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// a wrong secret is refused before the body is read
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest("POST", "/messengers/telegram", strings.NewReader(`{"message":{}}`)))
	if rec.Code != http.StatusUnauthorized || got != nil {
		t.Fatalf("the wrong secret = %d %q", rec.Code, got)
	}

	// the right secret hands the body over and answers 200
	rec = httptest.NewRecorder()
	signed := httptest.NewRequest("POST", "/messengers/telegram", strings.NewReader(`{"message":{"text":"/stop"}}`))
	signed.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret")
	c.ServeHTTP(rec, signed)
	if rec.Code != http.StatusOK || string(got) != `{"message":{"text":"/stop"}}` {
		t.Fatalf("the right secret = %d %q", rec.Code, got)
	}

	// a handler error still answers 200
	cfg.Messengers.Update = func(_ context.Context, _ []byte) error { return errors.New("no parse") }
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	signed = httptest.NewRequest("POST", "/messengers/telegram", strings.NewReader(`{}`))
	signed.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret")
	c.ServeHTTP(rec, signed)
	if rec.Code != http.StatusOK {
		t.Fatalf("the handler error = %d", rec.Code)
	}

	// without the configuration the route is not there
	rec = httptest.NewRecorder()
	r.c.ServeHTTP(rec, httptest.NewRequest("POST", "/messengers/telegram", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without messengers = %d", rec.Code)
	}
}

// Discord's connect routes (roadmap 145): the start sends the member to
// the authorize page with a state, the callback binds the code's DM
// channel to the member only when the state is this session's, and a
// refused connect says so without detail.
func TestDiscordConnect(t *testing.T) {
	r := newRig(t)
	members(r)
	var states []string
	var gotLogin, gotCode string
	cfg := r.c.cfg
	cfg.Messengers = &Messengers{
		Discord: &DiscordConnect{
			AuthorizeURL: func(state string) string {
				states = append(states, state)
				return "https://discord.example/authorize?state=" + state
			},
			Connect: func(_ context.Context, login, code string) error {
				gotLogin, gotCode = login, code
				return nil
			},
		},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// without a session the start only leads to the sign-in page
	ck, _ := memberLogin(t, r, "bob")
	do := func(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		c.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("/messengers/discord/start"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("the anonymous start = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// the start redirects to the authorize page with a state
	rec := do("/messengers/discord/start", ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://discord.example/authorize?state="+states[len(states)-1] {
		t.Fatalf("the start = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	state := states[len(states)-1]
	if state == "" {
		t.Fatal("the state is empty")
	}

	// a state that is not this session's refuses the bind
	if gotLogin != "" {
		t.Fatal("a connect ran before the callback")
	}
	rec = do("/messengers/discord/callback?code=abc&state=forged", ck)
	if rec.Code != http.StatusForbidden || gotLogin != "" {
		t.Fatalf("the forged state = %d, connect %q", rec.Code, gotLogin)
	}

	// the right state binds the code to the member and returns to the panel
	rec = do("/messengers/discord/callback?code=abc&state="+url.QueryEscape(state), ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/notifications" {
		t.Fatalf("the callback = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if gotLogin != "bob" || gotCode != "abc" {
		t.Fatalf("the connect = %q %q", gotLogin, gotCode)
	}

	// a refused connect says so, without detail
	cfg.Messengers.Discord.Connect = func(_ context.Context, _, _ string) error { return errors.New("no") }
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec = do("/messengers/discord/callback?code=abc&state="+url.QueryEscape(state), ck)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the refused connect = %d", rec.Code)
	}

	// a member sharing no server with the bot is told to join it
	cfg.Messengers.Discord.Connect = func(_ context.Context, _, _ string) error {
		return fmt.Errorf("the dm: %w", factoryd.ErrNoSharedServer)
	}
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec = do("/messengers/discord/callback?code=abc&state="+url.QueryEscape(state), ck)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Join the factory's Discord server first") {
		t.Fatalf("the connect without a shared server = %d %s", rec.Code, rec.Body.String())
	}

	// without the configuration the routes are not there
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/messengers/discord/start", nil)
	req.AddCookie(ck)
	r.c.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("on the plain rig the start = %d", rec.Code)
	}
}
