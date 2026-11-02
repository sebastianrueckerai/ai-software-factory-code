package console

import (
	"log"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// notifyLevels are the levels the panel offers: off, needs-you, all.
var notifyLevels = []string{factoryd.LevelOff, factoryd.LevelNeedsYou, factoryd.LevelAll}

// projectLevel is one project's row of the notifications panel: the
// level the member set, "" for none, and the derived default.
type projectLevel struct {
	Name, Repository, Level string
	Default                 string
	Writes                  bool // the human writes the project: what a chat token needs
}

// notifications answers GET /notifications: the member's connections
// and their level per project they read, with the derived default
// where they set none. Without the messengers there is no panel.
func (c *Console) notifications(w http.ResponseWriter, r *http.Request) {
	data, status, msg := c.notificationsData(r)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	c.render(w, "notifications.html", data)
}

// notificationsData reads the panel's data; shown, the bearer of the token
// the request just made, is added by the caller. A failure answers its
// status and words, status 0 on success.
func (c *Console) notificationsData(r *http.Request) (data map[string]any, status int, msg string) {
	m := c.cfg.Messengers
	if m == nil || m.Store == nil {
		return nil, http.StatusNotFound, "404 page not found"
	}
	s := current(r)
	links, err := m.Store.MessengerLinks(r.Context(), s.user)
	if err != nil {
		log.Printf("console: the messenger links of %s: %v", s.user, err)
		return nil, http.StatusInternalServerError, "cannot read the connections"
	}
	levels, err := m.Store.NotifyLevelsOf(r.Context(), s.user)
	if err != nil {
		log.Printf("console: the notify levels of %s: %v", s.user, err)
		return nil, http.StatusInternalServerError, "cannot read the levels"
	}
	ps, err := c.cfg.Projects.Projects(r.Context())
	if err != nil {
		log.Printf("console: projects: %v", err)
		return nil, http.StatusInternalServerError, "cannot read the projects"
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		return nil, http.StatusInternalServerError, "cannot read the policy"
	}
	var rows []projectLevel
	for _, p := range ps {
		if p.Repository == "" {
			continue
		}
		sees, writes, err := c.follows(r, p)
		if err != nil {
			return nil, http.StatusServiceUnavailable, "cannot check the project's writers"
		}
		if !sees {
			continue
		}
		def := factoryd.LevelOff
		if slices.Contains(p.NotifyOrApprovers(v.ApproversOf(p.Repository)), s.user) {
			def = factoryd.LevelNeedsYou
		}
		rows = append(rows, projectLevel{Name: p.Name, Repository: p.Repository, Level: levels[p.Repository], Default: def, Writes: writes})
	}
	var tokens []factoryd.ChatToken
	if c.chatTokensOn() {
		if tokens, err = c.cfg.ChatTokens.ChatTokensOf(r.Context(), s.user); err != nil {
			log.Printf("console: the chat tokens of %s: %v", s.user, err)
			return nil, http.StatusInternalServerError, "cannot read the chat tokens"
		}
	}
	return map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Links": links,
		"Projects": rows, "Levels": notifyLevels, "Code": r.URL.Query().Get("code"),
		"TelegramName": m.TelegramName, "Discord": m.Discord != nil,
		"ChatTokens": c.chatTokensOn(), "Tokens": tokens, "Lives": []string{"7", "30", "90"}, "Now": time.Now()}, 0, ""
}

// chatTokensOn: the chat tokens live on the notifications page, which
// needs the messengers' store as well as the tokens' own.
func (c *Console) chatTokensOn() bool {
	m := c.cfg.Messengers
	return c.cfg.ChatTokens != nil && m != nil && m.Store != nil
}

// telegramCode answers POST /notifications/telegram/code: it hands the
// member a one-time code the bot's Start carries back.
func (c *Console) telegramCode(w http.ResponseWriter, r *http.Request) {
	m := c.cfg.Messengers
	if m == nil || m.Store == nil || m.TelegramName == "" {
		http.NotFound(w, r)
		return
	}
	s := current(r)
	code, err := m.Store.IssueMessengerCode(r.Context(), s.user)
	if err != nil {
		log.Printf("console: the messenger code of %s: %v", s.user, err)
		http.Error(w, "cannot make a code", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/notifications?code="+url.QueryEscape(code), http.StatusSeeOther)
}

// disconnect answers POST /notifications/disconnect: the member takes
// one of their connections.
func (c *Console) disconnect(w http.ResponseWriter, r *http.Request) {
	m := c.cfg.Messengers
	if m == nil || m.Store == nil {
		http.NotFound(w, r)
		return
	}
	venue := r.PostFormValue("venue")
	if venue != factoryd.VenueTelegram && venue != factoryd.VenueDiscord {
		http.Error(w, "Choose a messenger.", http.StatusBadRequest)
		return
	}
	if err := m.Store.UnlinkMessenger(r.Context(), current(r).user, venue); err != nil {
		log.Printf("console: the disconnect of %s on %s: %v", current(r).user, venue, err)
		http.Error(w, "cannot disconnect", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// notifyLevel answers POST /notifications/level: the member sets their
// level for one project they read, as follows judges the reading.
func (c *Console) notifyLevel(w http.ResponseWriter, r *http.Request) {
	m := c.cfg.Messengers
	if m == nil || m.Store == nil {
		http.NotFound(w, r)
		return
	}
	level := r.PostFormValue("level")
	if !slices.Contains(notifyLevels, level) {
		http.Error(w, "Choose off, needs-you or all.", http.StatusBadRequest)
		return
	}
	p, ok, err := c.projectOf(r.Context(), r.PostFormValue("project"))
	switch {
	case err != nil:
		log.Printf("console: the project %s: %v", r.PostFormValue("project"), err)
		http.Error(w, "cannot read the projects", http.StatusServiceUnavailable)
		return
	case !ok || p.Repository == "":
		http.NotFound(w, r)
		return
	}
	sees, _, err := c.follows(r, p)
	switch {
	case err != nil:
		http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
		return
	case !sees:
		http.NotFound(w, r)
		return
	}
	if err := m.Store.SetNotifyLevel(r.Context(), current(r).user, p.Repository, level); err != nil {
		log.Printf("console: the notify level of %s on %s: %v", current(r).user, p.Repository, err)
		http.Error(w, "cannot set the level", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}
