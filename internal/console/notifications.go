package console

import (
	"log"
	"net/http"
	"net/url"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// notifyLevels are the levels the panel offers: off, needs-you, all.
var notifyLevels = []string{factoryd.LevelOff, factoryd.LevelNeedsYou, factoryd.LevelAll}

// projectLevel is one project's row of the notifications panel: the
// level the member set, "" for none, and the derived default.
type projectLevel struct {
	Name, Repository, Level string
	Default                 string
}

// notifications answers GET /notifications: the member's connections
// and their level per project they read, with the derived default
// where they set none. Without the messengers there is no panel.
func (c *Console) notifications(w http.ResponseWriter, r *http.Request) {
	m := c.cfg.Messengers
	if m == nil || m.Store == nil {
		http.NotFound(w, r)
		return
	}
	s := current(r)
	links, err := m.Store.MessengerLinks(r.Context(), s.user)
	if err != nil {
		log.Printf("console: the messenger links of %s: %v", s.user, err)
		http.Error(w, "cannot read the connections", http.StatusInternalServerError)
		return
	}
	levels, err := m.Store.NotifyLevelsOf(r.Context(), s.user)
	if err != nil {
		log.Printf("console: the notify levels of %s: %v", s.user, err)
		http.Error(w, "cannot read the levels", http.StatusInternalServerError)
		return
	}
	ps, err := c.cfg.Projects.Projects(r.Context())
	if err != nil {
		log.Printf("console: projects: %v", err)
		http.Error(w, "cannot read the projects", http.StatusInternalServerError)
		return
	}
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	var rows []projectLevel
	for _, p := range ps {
		if p.Repository == "" {
			continue
		}
		sees, _, err := c.follows(r, p)
		if err != nil {
			http.Error(w, "cannot check the project's writers", http.StatusServiceUnavailable)
			return
		}
		if !sees {
			continue
		}
		def := factoryd.LevelOff
		if slices.Contains(p.NotifyOrApprovers(v.ApproversOf(p.Repository)), s.user) {
			def = factoryd.LevelNeedsYou
		}
		rows = append(rows, projectLevel{Name: p.Name, Repository: p.Repository, Level: levels[p.Repository], Default: def})
	}
	c.render(w, "notifications.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Links": links,
		"Projects": rows, "Levels": notifyLevels, "Code": r.URL.Query().Get("code"),
		"TelegramName": m.TelegramName, "Discord": m.Discord != nil})
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
