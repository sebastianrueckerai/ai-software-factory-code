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

// Discord is the factory's other venue (roadmap 145): a member connects
// through OAuth2 in the browser and the bind is their DM channel. The
// client secret and the bot token never reach a log or an error.

// discordClient is the API client every Discord call goes out on.
var discordClient = &http.Client{Timeout: 15 * time.Second}

// Discord talks to Discord's API. BaseURL and Client exist for tests.
type Discord struct {
	ClientID     string
	ClientSecret string
	BotToken     string
	GuildID      string
	PublicURL    string
	BaseURL      string
	Store        *PGStore
	Client       *http.Client
}

// DiscordError is one refused call: its HTTP status and Discord's own
// words, with the secrets already gone.
type DiscordError struct {
	Status  int
	Message string
}

func (e *DiscordError) Error() string {
	return fmt.Sprintf("discord: %d: %s", e.Status, e.Message)
}

// AuthorizeURL is the page the member's browser opens: identify and
// guilds.join, returning to the console's callback.
func (d *Discord) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {d.ClientID},
		"redirect_uri":  {d.PublicURL + "/messengers/discord/callback"},
		"response_type": {"code"},
		"scope":         {"identify guilds.join"},
		"state":         {state},
	}
	return "https://discord.com/oauth2/authorize?" + q.Encode()
}

// Connect finishes the flow: the code becomes the member's user token,
// which joins them to the factory's guild and opens their DM channel.
// The bind is that channel, and the token leaves with the return.
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
	if err := d.call(ctx, "PUT", "/guilds/"+d.GuildID+"/members/"+user.ID, "", "Bot "+d.BotToken,
		[]byte(`{"access_token":`+strconv.Quote(token)+`}`), nil); err != nil {
		return err
	}
	var channel struct {
		ID string `json:"id"`
	}
	if err := d.call(ctx, "POST", "/users/@me/channels", "", "Bot "+d.BotToken,
		[]byte(`{"recipient_id":`+strconv.Quote(user.ID)+`}`), &channel); err != nil {
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

// call makes one API call, auth as the Authorization header; any 2xx
// answer does, and out takes a JSON body. Errors never name the secrets.
func (d *Discord) call(ctx context.Context, method, path, contentType, auth string, body []byte, out any) error {
	base := d.BaseURL
	if base == "" {
		base = "https://discord.com"
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
