package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
