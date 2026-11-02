package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sealed"
)

// chatTokenKeyLabel derives the key that seals the Forgejo token behind a
// chat token from Config.SessionKey.
const chatTokenKeyLabel = "console-chat-tokens-v1" // #nosec G101 -- a key label

var chatTokenLives = map[string]int{"7": 7, "30": 30, "90": 90}

// newChatToken is a fresh bearer, sfc_ and 32 random bytes in base64url,
// and the SHA-256 hex the store keeps.
func newChatToken() (bearer, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	bearer = "sfc_" + base64.RawURLEncoding.EncodeToString(b)
	return bearer, chatTokenHash(bearer), nil
}

func chatTokenHash(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:])
}

// checkForgejoToken refuses a Forgejo token that is not login's or that
// reaches beyond read:user, write:repository and write:issue: the admin
// and the notification endpoints must answer 401 or 403.
func (c *Console) checkForgejoToken(ctx context.Context, login, tok string) error {
	var me struct {
		Login string `json:"login"`
	}
	if err := c.api(ctx, tok, http.MethodGet, "/api/v1/user", nil, http.StatusOK, &me); err != nil {
		log.Printf("console: the Forgejo token of %s: %v", login, err)
		return refuse(http.StatusBadRequest, "Forgejo refused this token")
	}
	if !strings.EqualFold(me.Login, login) {
		return refuse(http.StatusBadRequest, "this Forgejo token belongs to "+me.Login+", not to you")
	}
	for _, path := range []string{"/api/v1/admin/users?limit=1", "/api/v1/notifications?limit=1"} {
		_, err := c.call(ctx, tok, http.MethodGet, path, nil, http.StatusOK, answerMax)
		var ae *apiError
		switch {
		case err == nil:
			return refuse(http.StatusBadRequest, "this Forgejo token reaches more than read:user, write:repository and write:issue; make one with only those three scopes")
		case !errors.As(err, &ae) || ae.status != http.StatusUnauthorized && ae.status != http.StatusForbidden:
			log.Printf("console: the Forgejo token of %s: %v", login, err)
			return refuse(http.StatusBadRequest, "Forgejo refused this token")
		}
	}
	return nil
}

// shownOnce is the block the answer to a made token renders: the bearer
// and the line that adds the door to Claude Code. Nothing stores it.
type shownOnce struct {
	Name, Bearer, Line string
}

// makeChatToken answers POST /notifications/tokens: the human makes a
// chat token for a project they write, from a Forgejo token they paste.
func (c *Console) makeChatToken(w http.ResponseWriter, r *http.Request) {
	s := current(r)
	repository := strings.TrimSpace(r.PostFormValue("project"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	days, lifeOK := chatTokenLives[r.PostFormValue("life")]
	switch n := utf8.RuneCountInString(name); {
	case n < 1 || n > 64:
		c.fail(w, r, refuse(http.StatusBadRequest, "the name needs 1 to 64 characters"))
		return
	case !lifeOK:
		c.fail(w, r, refuse(http.StatusBadRequest, "choose 7, 30 or 90 days"))
		return
	}
	p, ok, err := c.projectAt(r.Context(), repository)
	if err != nil {
		log.Printf("console: the project at %s: %v", repository, err)
		c.fail(w, r, refuse(http.StatusServiceUnavailable, "cannot read the projects"))
		return
	}
	if ok {
		wrote, err := c.writesRepo(r.Context(), s.user, repository)
		if err != nil {
			log.Printf("console: may %s write %s: %v", s.user, repository, err)
			c.fail(w, r, refuse(http.StatusServiceUnavailable, "cannot check the project's writers"))
			return
		}
		ok = wrote
	}
	if !ok {
		c.fail(w, r, refuse(http.StatusForbidden, "you do not write "+repository))
		return
	}
	forgejo := strings.TrimSpace(r.PostFormValue("forgejo"))
	if err := c.checkForgejoToken(r.Context(), s.user, forgejo); err != nil {
		c.fail(w, r, err)
		return
	}
	bearer, hash, err := newChatToken()
	if err == nil {
		var box string
		if box, err = sealed.Seal(c.cfg.SessionKey, chatTokenKeyLabel, forgejo, hash); err == nil {
			var id int64
			now := time.Now()
			id, err = c.cfg.ChatTokens.AddChatToken(r.Context(), factoryd.ChatToken{Hash: hash, Login: s.user, Repository: repository,
				Name: name, Sealed: box, Expires: now.AddDate(0, 0, days)})
			if err == nil {
				c.record(r.Context(), button(r), repository, "", 0, factoryd.EventChatTokenMade,
					map[string]any{"token": id, "name": name, "project": repository})
			}
		}
	}
	if err != nil {
		log.Printf("console: the chat token of %s: %v", s.user, err)
		c.fail(w, r, refuse(http.StatusInternalServerError, "cannot make the chat token"))
		return
	}
	line := fmt.Sprintf("claude mcp add --transport http factory-%s %s/mcp --header \"Authorization: Bearer %s\"",
		p.Name, strings.TrimSuffix(c.cfg.PublicURL, "/"), bearer)
	c.notificationsPage(w, r, &shownOnce{Name: name, Bearer: bearer, Line: line})
}

// revokeChatToken answers POST /notifications/tokens/revoke: the human
// revokes one of their chat tokens.
func (c *Console) revokeChatToken(w http.ResponseWriter, r *http.Request) {
	s := current(r)
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	rows, err := c.cfg.ChatTokens.ChatTokensOf(r.Context(), s.user)
	if err != nil {
		log.Printf("console: the chat tokens of %s: %v", s.user, err)
		c.fail(w, r, refuse(http.StatusInternalServerError, "cannot read the chat tokens"))
		return
	}
	var t factoryd.ChatToken
	for _, row := range rows {
		if row.ID == id {
			t = row
		}
	}
	if t.ID == 0 {
		c.fail(w, r, refuse(http.StatusBadRequest, "you have no chat token "+r.PostFormValue("id")))
		return
	}
	if _, err := c.cfg.ChatTokens.RevokeChatToken(r.Context(), id, s.user, time.Now()); err != nil {
		log.Printf("console: revoke chat token %d of %s: %v", id, s.user, err)
		c.fail(w, r, refuse(http.StatusInternalServerError, "cannot revoke the chat token"))
		return
	}
	c.record(r.Context(), button(r), t.Repository, "", 0, factoryd.EventChatTokenRevoked,
		map[string]any{"token": id, "name": t.Name, "project": t.Repository})
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}
