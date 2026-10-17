package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Discord is the factory's other venue (roadmap 145): a member of the
// factory's server connects through OAuth2 in the browser and the bind
// is their DM channel. The client secret and the bot token never reach
// a log or an error.

// discordClient is the API client every Discord call goes out on.
var discordClient = &http.Client{Timeout: 15 * time.Second}

// Discord talks to Discord's API. BaseURL and Client exist for tests.
type Discord struct {
	ClientID     string
	ClientSecret string
	BotToken     string
	PublicURL    string
	BaseURL      string
	Store        *PGStore
	Client       *http.Client
}

// DiscordError is one refused call: its HTTP status and Discord's own
// words, with the secrets already gone, and a rate limit's wait.
type DiscordError struct {
	Status     int
	Message    string
	RetryAfter int
}

func (e *DiscordError) Error() string {
	return fmt.Sprintf("discord: %d: %s", e.Status, e.Message)
}

// AuthorizeURL is the page the member's browser opens: identify alone,
// the least the DM needs, returning to the console's callback.
func (d *Discord) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {d.ClientID},
		"redirect_uri":  {d.PublicURL + "/messengers/discord/callback"},
		"response_type": {"code"},
		"scope":         {"identify"},
		"state":         {state},
	}
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// ErrNoSharedServer marks the connect of a member who shares no server
// with the bot: the DM cannot open until they join the factory's server.
var ErrNoSharedServer = errors.New("no server shared with the bot")

// Connect finishes the flow: the code becomes the member's user token,
// which opens their DM channel, the member already sharing the factory's
// server with the bot. The bind is that channel, and the token leaves
// with the return.
func (d *Discord) Connect(ctx context.Context, login, code string) error {
	token, err := d.exchange(ctx, code)
	if err != nil {
		return err
	}
	var user struct {
		ID string `json:"id"`
	}
	if err := d.call(ctx, "GET", "/users/@me", "", "Bearer "+token, nil, &user); err != nil {
		return err
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := d.call(ctx, "POST", "/users/@me/channels", "", "Bot "+d.BotToken,
		[]byte(`{"recipient_id":`+strconv.Quote(user.ID)+`}`), &channel); err != nil {
		var de *DiscordError
		if errors.As(err, &de) && de.Status == http.StatusForbidden {
			return fmt.Errorf("%w: %v", ErrNoSharedServer, err)
		}
		return err
	}
	return d.Store.BindMessenger(ctx, login, VenueDiscord, channel.ID, user.ID)
}

// exchange trades the OAuth2 code for the member's user token.
func (d *Discord) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {d.ClientID},
		"client_secret": {d.ClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {d.PublicURL + "/messengers/discord/callback"},
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := d.call(ctx, "POST", "/oauth2/token", "application/x-www-form-urlencoded", "",
		[]byte(form.Encode()), &out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

// Send writes one notification to the channel with the card's link
// button and returns the sent message's ref.
func (d *Discord) Send(ctx context.Context, channel, text, cardURL string) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	if err := d.call(ctx, "POST", "/channels/"+channel+"/messages", "application/json", "Bot "+d.BotToken,
		[]byte(`{"components":`+discordButton(cardURL)+`,"content":`+strconv.Quote(text)+`}`), &res); err != nil {
		return "", err
	}
	return res.ID, nil
}

// Edit rewrites a sent notification, keeping its button.
func (d *Discord) Edit(ctx context.Context, channel, ref, text, cardURL string) error {
	return d.call(ctx, "PATCH", "/channels/"+channel+"/messages/"+ref, "application/json", "Bot "+d.BotToken,
		[]byte(`{"components":`+discordButton(cardURL)+`,"content":`+strconv.Quote(text)+`}`), nil)
}

// discordButton is the one link button every notification carries, as
// the components row around it: a link to the card, never answer
// buttons (the ruling of 2026-10-17).
func discordButton(cardURL string) string {
	return `[{"components":[{"label":"Open the card","style":5,"type":2,"url":` + strconv.Quote(cardURL) + `}],"type":1}]`
}

// call makes one API call, auth as the Authorization header; any 2xx
// answer does, and out takes a JSON body. Errors never name the secrets.
func (d *Discord) call(ctx context.Context, method, path, contentType, auth string, body []byte, out any) error {
	base := d.BaseURL
	if base == "" {
		base = "https://discord.com/api"
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return d.quiet(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	client := d.Client
	if client == nil {
		client = discordClient
	}
	res, err := client.Do(req)
	if err != nil {
		return d.quiet(err)
	}
	defer res.Body.Close() // #nosec G104 -- the body is read next
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return d.quiet(err)
	}
	if res.StatusCode/100 != 2 {
		e := &DiscordError{Status: res.StatusCode}
		if wait, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil {
			e.RetryAfter = wait
		}
		var words struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &words) == nil && words.Message != "" {
			e.Message = words.Message
		} else {
			e.Message = strings.TrimSpace(string(b))
		}
		e.Message = d.scrub(e.Message)
		return e
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return d.quiet(fmt.Errorf("discord %s: %w", path, err))
		}
	}
	return nil
}

// quiet strips the client secret and the bot token from an error.
func (d *Discord) quiet(err error) error {
	return errors.New(d.scrub(err.Error()))
}

// scrub replaces the secrets with their names.
func (d *Discord) scrub(s string) string {
	return strings.NewReplacer(d.ClientSecret, "<secret>", d.BotToken, "<token>").Replace(s)
}
