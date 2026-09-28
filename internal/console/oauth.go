package console

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// errLoggedOut: the session's tokens are gone (refresh refused); the human
// logs in again.
var errLoggedOut = errors.New("forgejo refused the session's refresh token")

func (c *Console) redirectURI() string {
	return strings.TrimSuffix(c.cfg.PublicURL, "/") + "/auth/callback"
}

// loginCookieValue is state.verifier.expiry.mac, the mac an HMAC-SHA256
// over the first three parts under the session key.
func (c *Console) loginCookieValue(state, verifier string, expires time.Time) string {
	v := state + "." + verifier + "." + strconv.FormatInt(expires.Unix(), 10)
	return v + "." + c.mac(v)
}

func (c *Console) mac(v string) string {
	m := hmac.New(sha256.New, c.cfg.SessionKey)
	m.Write([]byte(v)) // #nosec G104 -- a hash write never fails
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// pendingLogin reads the login cookie: its state and verifier when the
// signature holds and it has not expired.
func (c *Console) pendingLogin(r *http.Request) (state, verifier string, ok bool) {
	ck, err := r.Cookie(loginCookie)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(ck.Value, ".")
	if len(parts) != 4 || !hmac.Equal([]byte(parts[3]), []byte(c.mac(strings.Join(parts[:3], ".")))) {
		return "", "", false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// login starts the authorization-code flow with PKCE (S256). The state and
// the verifier travel in a signed host-only cookie, so an anonymous visit
// keeps nothing on the server; the state also goes in the query.
func (c *Console) login(w http.ResponseWriter, r *http.Request) {
	if c.session(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	state, verifier := randomToken(), randomToken()
	setCookie(w, loginCookie, c.loginCookieValue(state, verifier, time.Now().Add(loginTTL)), int(loginTTL.Seconds()))
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {c.redirectURI()},
		"response_type":         {"code"},
		"scope":                 {Scopes},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, strings.TrimSuffix(c.cfg.ForgejoPublicURL, "/")+"/login/oauth/authorize?"+q.Encode(), http.StatusSeeOther)
}

// callback completes the flow: the signed login cookie must hold the
// query's state, the code is exchanged in-cluster with the client secret
// and the cookie's verifier, and only a writer gets a session.
func (c *Console) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, verifier, ok := c.pendingLogin(r)
	setCookie(w, loginCookie, "", -1)
	if !ok || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
		http.Error(w, "login state mismatch or expired; start again at /login", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		http.Error(w, "Forgejo refused the login: "+e, http.StatusForbidden)
		return
	}
	s := &session{}
	if err := c.token(r.Context(), s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"redirect_uri":  {c.redirectURI()},
		"code_verifier": {verifier},
	}); err != nil {
		log.Printf("console: code exchange: %v", err)
		http.Error(w, "login failed at the code exchange", http.StatusBadGateway)
		return
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := c.api(r.Context(), s.access, http.MethodGet, "/api/v1/user", nil, http.StatusOK, &user); err != nil || user.Login == "" {
		log.Printf("console: who logged in: %v", err)
		http.Error(w, "login failed reading the user", http.StatusBadGateway)
		return
	}
	s.user = user.Login
	ok, err := c.writer(r.Context(), s.user)
	if err != nil {
		log.Printf("console: writers of %s: %v", c.cfg.Repo, err)
		http.Error(w, "cannot check repository writers", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		log.Printf("console: %s is not a writer of %s: login refused", s.user, c.cfg.Repo)
		c.forbidden(w, s.user)
		return
	}
	c.newSession(w, s)
	log.Printf("console: %s logged in", s.user)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// token runs a token request (code exchange or refresh) and stores the
// result on s.
func (c *Console) token(ctx context.Context, s *session, form url.Values) error {
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.cfg.ForgejoURL, "/")+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		if res.StatusCode >= 400 && res.StatusCode < 500 {
			return fmt.Errorf("%w: status %d", errLoggedOut, res.StatusCode)
		}
		return fmt.Errorf("token endpoint: status %d", res.StatusCode)
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return fmt.Errorf("token endpoint: %w", err)
	}
	if t.AccessToken == "" {
		return errors.New("token endpoint: no access token")
	}
	s.access, s.refresh = t.AccessToken, t.RefreshToken
	s.expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return nil
}

// accessToken returns the session's access token, refreshed when it
// expires within a minute (Forgejo issues 1 h tokens with rotating refresh
// tokens).
func (c *Console) accessToken(ctx context.Context, s *session) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Until(s.expiry) > time.Minute {
		return s.access, nil
	}
	if err := c.token(ctx, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {s.refresh}}); err != nil {
		return "", err
	}
	return s.access, nil
}

// asUser calls the Forgejo API as the session's human, decoding the answer
// into out when non-nil. A refused refresh ends the session.
func (c *Console) asUser(r *http.Request, method, path string, payload any, want int, out any) error {
	s := current(r)
	tok, err := c.accessToken(r.Context(), s)
	if err != nil {
		if errors.Is(err, errLoggedOut) {
			c.drop(r)
		}
		return err
	}
	return c.api(r.Context(), tok, method, path, payload, want, out)
}

func (c *Console) api(ctx context.Context, token, method, path string, payload any, want int, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.ForgejoURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("forgejo %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("forgejo %s %s: %w", method, path, err)
	}
	if res.StatusCode != want {
		return fmt.Errorf("forgejo %s %s: status %d: %s", method, path, res.StatusCode, bytes.TrimSpace(b))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("forgejo %s %s: %w", method, path, err)
		}
	}
	return nil
}
