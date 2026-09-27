// Package console is the human's view of the factory at
// console.aisoftwarefactory.world (task 54): Forgejo OAuth2 login for the
// repository's writers, the board, new tasks filed as the human, and the
// human's answers to escalations. It is served by factoryd on its own port;
// every Forgejo write goes out with the logged-in human's token.
package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Scopes are requested on every authorization: an authorization without an
// explicit scope yields a token with the user's full power, instance admin
// included (research-stage7-forgejo Q1). Forgejo fixes a grant's scope at
// the first authorization per user and app, so this list only grows with a
// new app registration.
const Scopes = "read:user write:repository write:issue"

const (
	sessionCookie = "__Host-console"
	loginCookie   = "__Host-console-login"
	sessionTTL    = 12 * time.Hour
	loginTTL      = 10 * time.Minute
	writersTTL    = time.Minute
	finishedSince = 7 * 24 * time.Hour
)

// Tasks is the task state the board reads.
type Tasks interface {
	Task(ctx context.Context, id string) (factoryd.Task, bool, error)
	// BoardTasks lists every task not done or failed, and those finished
	// after since.
	BoardTasks(ctx context.Context, since time.Time) ([]factoryd.Task, error)
}

// Writers is the trust gate's writer list (factoryd.Forgejo.RepoWriters):
// only these users may log in.
type Writers interface {
	RepoWriters(ctx context.Context, owner, repo string) ([]string, error)
}

// Config wires the console. ForgejoURL serves the code exchange and API
// calls (in-cluster); ForgejoPublicURL is what browsers are sent to.
type Config struct {
	PublicURL        string // https://console.aisoftwarefactory.world
	ForgejoURL       string
	ForgejoPublicURL string
	ClientID         string
	ClientSecret     string
	// Repo is where new tasks are filed and whose writers may log in.
	Repo      factoryd.Repo
	Tasks     Tasks
	Writers   Writers
	Timelines factoryd.ObjectStore
	// Approvals feeds the human lane's approval cards; nil shows none.
	Approvals Approvals
	HTTP      *http.Client // nil = 15 s timeout
}

// Console is the console's http.Handler.
type Console struct {
	cfg  Config
	tmpl *template.Template
	mux  *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*session
	logins   map[string]pendingLogin // state -> PKCE verifier
	writers  []string
	writersT time.Time
}

type session struct {
	user    string
	csrf    string
	expires time.Time

	mu      sync.Mutex // guards the tokens across a refresh
	access  string
	refresh string
	expiry  time.Time
}

type pendingLogin struct {
	verifier string
	expires  time.Time
}

// New builds the console handler.
func New(cfg Config) (*Console, error) {
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	c := &Console{cfg: cfg, tmpl: tmpl, mux: http.NewServeMux(),
		sessions: map[string]*session{}, logins: map[string]pendingLogin{}}
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	c.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	c.mux.HandleFunc("GET /login", c.login)
	c.mux.HandleFunc("GET /auth/callback", c.callback)
	c.mux.Handle("POST /logout", c.auth(c.logout))
	c.mux.Handle("GET /{$}", c.auth(c.board))
	c.mux.Handle("GET /board", c.auth(c.board))
	c.mux.Handle("GET /tasks/new", c.auth(c.newTaskForm))
	c.mux.Handle("POST /tasks", c.auth(c.createTask))
	c.mux.Handle("POST /tasks/{id}/answer", c.auth(c.answer))
	c.mux.Handle("GET /tasks/{id}/timeline", c.auth(c.timeline))
	return c, nil
}

func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; "+
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	c.mux.ServeHTTP(w, r)
}

type ctxKey struct{}

// auth admits a request with a live session of a current writer and, for
// anything but GET, the session's CSRF token in the form or the
// X-CSRF-Token header (htmx sends it from hx-headers).
func (c *Console) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := c.session(r)
		if s == nil {
			if r.Method != http.MethodGet {
				http.Error(w, "not logged in", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method != http.MethodGet {
			got := r.Header.Get("X-CSRF-Token")
			if got == "" {
				got = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
				http.Error(w, "missing or stale form token; reload the page", http.StatusForbidden)
				return
			}
		}
		ok, err := c.writer(r.Context(), s.user)
		if err != nil {
			log.Printf("console: writers of %s: %v", c.cfg.Repo, err)
			http.Error(w, "cannot check repository writers", http.StatusServiceUnavailable)
			return
		}
		if !ok {
			c.drop(r)
			c.forbidden(w, s.user)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	})
}

func current(r *http.Request) *session { return r.Context().Value(ctxKey{}).(*session) }

func (c *Console) session(r *http.Request) *session {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[ck.Value]
	if s == nil || time.Now().After(s.expires) {
		delete(c.sessions, ck.Value)
		return nil
	}
	return s
}

func (c *Console) drop(r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil {
		c.mu.Lock()
		delete(c.sessions, ck.Value)
		c.mu.Unlock()
	}
}

// writer reports whether user is on the repository's writer list, read at
// most once a minute.
func (c *Console) writer(ctx context.Context, user string) (bool, error) {
	c.mu.Lock()
	writers, fresh := c.writers, time.Since(c.writersT) < writersTTL
	c.mu.Unlock()
	if !fresh {
		var err error
		if writers, err = c.cfg.Writers.RepoWriters(ctx, c.cfg.Repo.Owner, c.cfg.Repo.Name); err != nil {
			return false, err
		}
		c.mu.Lock()
		c.writers, c.writersT = writers, time.Now()
		c.mu.Unlock()
	}
	return slices.Contains(writers, user), nil
}

func (c *Console) newSession(w http.ResponseWriter, s *session) {
	id := randomToken()
	s.csrf = randomToken()
	s.expires = time.Now().Add(sessionTTL)
	c.mu.Lock()
	for k, old := range c.sessions {
		if time.Now().After(old.expires) {
			delete(c.sessions, k)
		}
	}
	c.sessions[id] = s
	c.mu.Unlock()
	setCookie(w, sessionCookie, id, int(sessionTTL.Seconds()))
}

func (c *Console) logout(w http.ResponseWriter, r *http.Request) {
	c.drop(r)
	setCookie(w, sessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (c *Console) forbidden(w http.ResponseWriter, user string) {
	setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusForbidden)
	c.render(w, "forbidden.html", map[string]any{"User": user, "Repo": c.cfg.Repo.String()})
}

func (c *Console) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := c.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("console: render %s: %v", name, err)
	}
}

// setCookie sets a host-only session cookie; maxAge < 0 deletes it.
func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
