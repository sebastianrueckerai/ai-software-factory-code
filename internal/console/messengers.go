package console

import (
	"context"
	"crypto/subtle"
	"io"
	"log"
	"net/http"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// Messengers is the messengers' console half (roadmap 145): Telegram's
// webhook (the header secret it signs its calls with, and Update for one
// update's bytes, factoryd's Telegram.Update), Discord's OAuth2 connect
// and the notifications panel's store and bot name. A nil Discord seats
// no Discord routes; a nil Store seats no panel.
type Messengers struct {
	Secret       string
	Update       func(ctx context.Context, body []byte) error
	Discord      *DiscordConnect
	Store        MessengerStore
	TelegramName string
}

// MessengerStore is the panel's half of the messenger tables: the codes
// and the connections, and the member's notify levels. The PGStore
// carries it.
type MessengerStore interface {
	IssueMessengerCode(ctx context.Context, login string) (string, error)
	MessengerLinks(ctx context.Context, login string) ([]factoryd.MessengerLink, error)
	UnlinkMessenger(ctx context.Context, login, venue string) error
	NotifyLevelsOf(ctx context.Context, login string) (map[string]string, error)
	SetNotifyLevel(ctx context.Context, login, repository, level string) error
}

// DiscordConnect is the console's handle on Discord's OAuth2 flow: the
// page it sends the member to, and the bind the callback makes of the
// code (factoryd's Discord.Connect).
type DiscordConnect struct {
	AuthorizeURL func(state string) string
	Connect      func(ctx context.Context, login, code string) error
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

// discord is the seated Discord connect, or nil when the configuration
// seats none and the routes answer 404.
func (c *Console) discord() *DiscordConnect {
	if c.cfg.Messengers == nil {
		return nil
	}
	return c.cfg.Messengers.Discord
}

// discordStart sends the member to Discord's OAuth2 page with a state
// bound to their session.
func (c *Console) discordStart(w http.ResponseWriter, r *http.Request) {
	d := c.discord()
	if d == nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, d.AuthorizeURL(c.discordState(current(r))), http.StatusSeeOther)
}

// discordCallback binds the DM channel Discord's code opens to the
// member, once the state proves this session started the connect.
func (c *Console) discordCallback(w http.ResponseWriter, r *http.Request) {
	d := c.discord()
	if d == nil {
		http.NotFound(w, r)
		return
	}
	s := current(r)
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(c.discordState(s))) != 1 {
		c.fail(w, r, refuse(http.StatusForbidden, "This connect did not start from your session. Start it again in the console."))
		return
	}
	if q.Get("code") == "" {
		c.fail(w, r, refuse(http.StatusBadRequest, "Discord sent no code. Start the connect again in the console."))
		return
	}
	if err := d.Connect(r.Context(), s.user, q.Get("code")); err != nil {
		log.Printf("messengers: the discord connect of %s: %v", s.user, err)
		c.fail(w, r, refuse(http.StatusBadGateway, "Discord refused the connect. Try again in a minute."))
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// discordState is the OAuth2 state of one session: a MAC under the
// session key over the session's row, so only the session that started
// a connect can finish it.
func (c *Console) discordState(s *session) string {
	return c.mac("discord-connect|" + s.hash)
}
