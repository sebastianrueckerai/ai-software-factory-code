package console

import (
	"context"
	"io"
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Messengers is the Telegram webhook's console half (roadmap 145): the
// header secret Telegram signs its calls with, and Update for one
// update's bytes (factoryd's Telegram.Update).
type Messengers struct {
	Secret string
	Update func(ctx context.Context, body []byte) error
}

// telegramBody is the most Telegram may post at the webhook.
const telegramBody = 64 << 10

// telegramWebhook is Telegram's only inbound route: the header secret
// checked in constant time before the body is read, the body capped, and
// 200 always after the check so Telegram does not retry.
func (c *Console) telegramWebhook(w http.ResponseWriter, r *http.Request) {
	m := c.cfg.Messengers
	if !factoryd.SameSecret(r.Header.Get("X-Telegram-Bot-Api-Secret-Token"), m.Secret) {
		http.Error(w, "wrong secret", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, telegramBody))
	if err != nil {
		log.Printf("messengers: the webhook body: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := m.Update(r.Context(), body); err != nil {
		log.Printf("messengers: %v", err)
	}
	w.WriteHeader(http.StatusOK)
}
