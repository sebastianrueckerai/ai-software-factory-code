// Package console is the human's view of the factory at
// console.aisoftwarefactory.world (task 54): Forgejo OAuth2 login for the
// repository's writers, the board, new tasks filed as the human, the
// human's answers to escalations, approvals, and the agent resources. It is served by factoryd on its own port;
// every Forgejo write goes out with the logged-in human's token.
package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

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
	writersStale  = 5 * time.Minute // a failed refresh serves the last list this long
	finishedSince = 7 * 24 * time.Hour
)

// Tasks is the task state the board reads.
type Tasks interface {
	Task(ctx context.Context, id string) (factoryd.Task, bool, error)
	// BoardTasks lists at most limit tasks: those not done or failed
	// first, then those finished after since, each with its latest
	// escalation's reason.
	BoardTasks(ctx context.Context, since time.Time, limit int) ([]factoryd.BoardTask, error)
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
	// SessionKey signs the login cookie (HMAC-SHA256), at least 32 bytes.
	SessionKey []byte
	// Repo is where new tasks are filed and whose writers may log in.
	Repo      factoryd.Repo
	Tasks     Tasks
	Writers   Writers
	Timelines factoryd.ObjectStore
	Policies  Policies
	Costs     Costs
	// ConfigRepo is the infra repository's web URL and ConfigDir the
	// configuration tree's directory in it, where every change is made
	// (ruling 183).
	ConfigRepo   string
	ConfigDir    string
	Approvals    Approvals
	ApproveMerge MergeApprover
	Resources    Resources
	Alerts       Alerts
	Projects     Projects
	// Reconcile brings a project's objects to its row (factoryd.Registrar).
	Reconcile func(ctx context.Context, p factoryd.Project) error
	HTTP      *http.Client // nil = 15 s timeout
}

// Console is the console's http.Handler.
type Console struct {
	cfg  Config
	tmpl *template.Template
	mux  *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*session
	writers  []string
	writersT time.Time
	flight   singleflight.Group
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

// New builds the console handler.
func New(cfg Config) (*Console, error) {
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	if len(cfg.SessionKey) < 32 {
		return nil, errors.New("console: the session key must be at least 32 bytes")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	c := &Console{cfg: cfg, tmpl: tmpl, mux: http.NewServeMux(),
		sessions: map[string]*session{}}
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
	c.mux.Handle("GET /costs", c.auth(c.costs))
	c.mux.Handle("GET /policy", c.auth(c.policy))
	c.mux.Handle("POST /approvals/{id}/approve", c.auth(c.spend("approve", c.approve)))
	c.mux.Handle("POST /approvals/{id}/deny", c.auth(c.deny))
	c.mux.Handle("GET /resources", c.auth(c.resources))
	c.mux.Handle("POST /resources/{id}/promote", c.auth(c.spend("promote", c.promote)))
	c.mux.Handle("POST /resources/{id}/release", c.auth(c.release))
	c.mux.Handle("POST /envelopes", c.auth(c.spend("envelope edit", c.setEnvelope)))
	c.mux.Handle("GET /alerts", c.auth(c.alerts))
	c.mux.Handle("GET /projects", c.auth(c.projects))
	c.mux.Handle("POST /projects", c.auth(c.spend("project registration", c.registerProject)))
	c.mux.Handle("POST /projects/{name}/settings", c.auth(c.spend("project settings", c.projectSettings)))
	c.mux.Handle("POST /alerts/{id}/verdict", c.auth(c.verdict))
	return c, nil
}

func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; "+
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
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
// most once a minute by one request while the others wait for it; when
// Forgejo fails, the last list serves for up to five minutes.
func (c *Console) writer(ctx context.Context, user string) (bool, error) {
	c.mu.Lock()
	writers, at := c.writers, c.writersT
	c.mu.Unlock()
	if time.Since(at) >= writersTTL {
		v, err, _ := c.flight.Do("writers", func() (any, error) {
			list, err := c.cfg.Writers.RepoWriters(context.WithoutCancel(ctx), c.cfg.Repo.Owner, c.cfg.Repo.Name)
			if err == nil {
				c.mu.Lock()
				c.writers, c.writersT = list, time.Now()
				c.mu.Unlock()
			}
			return list, err
		})
		switch {
		case err == nil:
			writers = v.([]string)
		case writers != nil && time.Since(at) < writersStale:
			log.Printf("console: writers of %s: %v; serving the list of %s", c.cfg.Repo, err, at.UTC().Format(time.RFC3339))
		default:
			return false, err
		}
	}
	return slices.Contains(writers, user), nil
}

// approver reports whether the session's human may authorize spend: a name
// on the policy's approvers (ruling 128). Unreadable policy: no.
func (c *Console) approver(r *http.Request) bool {
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy for the approvers: loaded %v, %v", ok, err)
		return false
	}
	return slices.Contains(v.Document.Approvers, current(r).user)
}

// spend refuses a spend action to anyone but the policy's approvers.
func (c *Console) spend(what string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.approver(r) {
			log.Printf("console: %s is no approver: %s refused", current(r).user, what)
			http.Error(w, "Only the policy's approvers may authorize spend.", http.StatusForbidden)
			return
		}
		next(w, r)
	}
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
