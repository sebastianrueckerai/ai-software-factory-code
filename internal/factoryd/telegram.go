package factoryd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Telegram is the factory's bot on one venue (roadmap 145): it connects a
// member's private chat with a one-time code and carries the outbox's
// notifications there. The bot token never reaches a log or an error.

// TelegramSecret is the webhook's header secret, derived from the bot
// token: no secret to seed, and a new token rotates it.
func TelegramSecret(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("webhook"))
	return hex.EncodeToString(mac.Sum(nil))
}

// telegramClient is the API client every Telegram call goes out on.
var telegramClient = &http.Client{Timeout: 15 * time.Second}

// Telegram talks to the Bot API. BaseURL and Client exist for tests; the
// Name is what Boot reads.
type Telegram struct {
	Token     string
	PublicURL string
	BaseURL   string
	Name      string
	Store     *PGStore
	Client    *http.Client
}

// TelegramError is one refused call: its HTTP status, the API's own words
// and its retry hint.
type TelegramError struct {
	Status      int
	Description string
	RetryAfter  int
}

func (e *TelegramError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("telegram: %d: %s: retry in %ds", e.Status, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("telegram: %d: %s", e.Status, e.Description)
}

// Boot reads the bot's name and points Telegram's updates at the
// console's webhook route.
func (t *Telegram) Boot(ctx context.Context, publicURL string) error {
	var me struct {
		Username string `json:"username"`
	}
	if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return err
	}
	t.Name = me.Username
	return t.call(ctx, "setWebhook", map[string]any{
		"url":             publicURL + "/messengers/telegram",
		"secret_token":    TelegramSecret(t.Token),
		"allowed_updates": []string{"message"},
	}, nil)
}

// Update handles one webhook update: /start with a code binds the private
// chat to the member whose code it is, /stop drops the bind, anything
// else draws the one fixed line. A chat that is not private gets nothing.
func (t *Telegram) Update(ctx context.Context, body []byte) error {
	var u struct {
		Message *struct {
			Text string `json:"text"`
			Chat struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			} `json:"chat"`
			From struct {
				ID int64 `json:"id"`
			} `json:"from"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return err
	}
	if u.Message == nil || u.Message.Chat.Type != "private" {
		return nil
	}
	m := u.Message
	chat := strconv.FormatInt(m.Chat.ID, 10)
	fields := strings.Fields(m.Text)
	switch {
	case len(fields) == 2 && fields[0] == "/start":
		login, ok, err := t.Store.ConsumeMessengerCode(ctx, fields[1], VenueTelegram, chat, strconv.FormatInt(m.From.ID, 10))
		if err != nil {
			return err
		}
		if !ok {
			return t.send(ctx, chat, "That code is missing, used or expired. Get a new one in the console.")
		}
		return t.send(ctx, chat, "Connected to "+login+". Opt in per project in the console.")
	case len(fields) == 1 && fields[0] == "/stop":
		link, ok, err := t.Store.MessengerLinkOf(ctx, VenueTelegram, chat)
		if err != nil {
			return err
		}
		if ok {
			if err := t.Store.UnlinkMessenger(ctx, link.Login, VenueTelegram); err != nil {
				return err
			}
		}
		return t.send(ctx, chat, "Disconnected.")
	default:
		return t.send(ctx, chat, "This bot only carries notifications; it never reads replies. The console: "+t.PublicURL)
	}
}

// send writes text to chat.
func (t *Telegram) send(ctx context.Context, chat, text string) error {
	return t.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text}, nil)
}

// call posts one Bot API method; out, when not nil, takes the call's
// result. Errors never name the token.
func (t *Telegram) call(ctx context.Context, method string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/bot"+t.Token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := t.Client
	if client == nil {
		client = telegramClient
	}
	res, err := client.Do(req)
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), t.Token, "<token>"))
	}
	defer res.Body.Close() // #nosec G104 -- the body is read next
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), t.Token, "<token>"))
	}
	var envelope struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Parameters  *struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("telegram %s: %d: %s", method, res.StatusCode, strings.ReplaceAll(err.Error(), t.Token, "<token>"))
	}
	if !envelope.OK {
		e := &TelegramError{Status: res.StatusCode, Description: envelope.Description}
		if envelope.Parameters != nil {
			e.RetryAfter = envelope.Parameters.RetryAfter
		}
		return e
	}
	if out != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("telegram %s: %w", method, err)
		}
	}
	return nil
}
