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
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
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
	// PlanRevision is revision n of plan task id's plan, the newest for 0.
	PlanRevision(ctx context.Context, id string, n int) (factoryd.PlanRevision, bool, error)
	// PlanCards are the cards plan task plan filed.
	PlanCards(ctx context.Context, plan string) ([]factoryd.Task, error)
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
	Tasks      Tasks
	// Writers are the users who may log in: the writers of the factory's
	// own project, the one deploying into factoryd's namespace.
	Writers   func(ctx context.Context) ([]string, error)
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
	// Events records what the human's buttons did and answers a project's
	// feed (factoryd.PGStore).
	Events Events
	// MayMove is intake's moved_by judgement (factoryd.Intake.MayMove):
	// whether user may put a card of repository into a lane moved by
	// movedBy. The console asks it only to offer and pre-check a move.
	MayMove func(ctx context.Context, repository, user, movedBy string) (bool, error)
	// AcceptPlan and RejectPlan signal a plan task's workflow
	// (factoryd.AcceptPlan, factoryd.RejectPlan).
	AcceptPlan func(ctx context.Context, taskID string, revision int, sha, by string) error
	RejectPlan func(ctx context.Context, taskID, by, why string) error
	// RetryPlanCard files failed plan card taskID again for by and returns
	// the replacement's issue (factoryd's Runner.RetryPlanCard): a refusal
	// is factoryd.ErrNoRetry.
	RetryPlanCard func(ctx context.Context, taskID, by string) (int64, error)
	// InfraRepo is the infra repository (owner/name), where the new-project
	// button files its pull request; InfraToken reads its pull requests
	// (factoryd's forgejo-config token).
	InfraRepo  factoryd.Repo
	InfraToken string
	// CheckProject judges projects/<name>.yaml with file's text the
	// loader's way against the tree at infra's head
	// (factoryd.CheckProjectFile): the refusal's lines, none when it
	// passes; err when the tree could not be read.
	CheckProject func(ctx context.Context, name string, file []byte) (refused []string, err error)
	// MainExists reports whether a project's repository has main.
	MainExists func(ctx context.Context, repo factoryd.Repo) (bool, error)
	// ReconcileError is Registrar.ReconcileError: why the project's last
	// reconcile failed, "" when it was clean.
	ReconcileError func(name string) string
	// MergeRetryPause is the wait before a retry of a merge Forgejo refused
	// with 405 while it re-checks mergeability (nil = 1 s).
	MergeRetryPause *time.Duration
	HTTP            *http.Client // nil = 15 s timeout
	// Metrics counts the verdict clicks (factory.human.intervention); nil
	// counts none.
	Metrics *telemetry.Metrics
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
	moves    map[string]answer // the board's MayMove answers
	// runs are the new projects' merge refusals and registrations, in
	// memory for the status panel.
	runs map[string]*projectRun
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
		sessions: map[string]*session{}, runs: map[string]*projectRun{}}
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
	c.mux.Handle("POST /cards/{id}/lane", c.auth(c.moveCard))
	c.mux.Handle("POST /tasks/{id}/start-plan", c.auth(c.spend("start a plan", c.startPlan)))
	c.mux.Handle("POST /tasks/{id}/plan/accept", c.auth(c.spend("accept a plan", c.acceptPlan)))
	c.mux.Handle("POST /tasks/{id}/plan/changes", c.auth(c.planChanges))
	c.mux.Handle("POST /tasks/{id}/plan/reject", c.auth(c.rejectPlan))
	c.mux.Handle("POST /tasks/{id}/retry", c.auth(c.spend("retry a card", c.retryCard)))
	c.mux.Handle("GET /tasks/{id}/timeline", c.auth(c.timeline))
	c.mux.Handle("GET /costs", c.auth(c.costs))
	c.mux.Handle("GET /policy", c.auth(c.policy))
	c.mux.Handle("POST /approvals/{id}/approve", c.auth(c.spend("approve", c.approve)))
	c.mux.Handle("POST /approvals/{id}/deny", c.auth(c.deny))
	c.mux.Handle("GET /resources", c.auth(c.resources))
	c.mux.Handle("POST /resources/{id}/promote", c.auth(c.spend("promote", c.promote)))
	c.mux.Handle("POST /resources/{id}/release", c.auth(c.release))
	c.mux.Handle("GET /alerts", c.auth(c.alerts))
	c.mux.Handle("GET /projects", c.auth(c.projects))
	c.mux.Handle("GET /projects/new", c.auth(c.newProjectForm))
	c.mux.Handle("POST /projects", c.auth(c.spend("add a project", c.createProject)))
	c.mux.Handle("GET /projects/{name}/status", c.auth(c.projectStatus))
	c.mux.Handle("GET /projects/{name}/events", c.auth(c.projectEvents))
	c.mux.Handle("POST /projects/{name}/merge", c.auth(c.spend("merge a project's file", c.mergeProject)))
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
			log.Printf("console: writers: %v", err)
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
			list, err := c.cfg.Writers(context.WithoutCancel(ctx))
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
			log.Printf("console: writers: %v; serving the list of %s", err, at.UTC().Format(time.RFC3339))
		default:
			return false, err
		}
	}
	return slices.Contains(writers, user), nil
}

// approver reports whether the session's human may authorize spend: a name
// on the policy's approvers (ruling 128). Unreadable policy: no.
func (c *Console) approver(r *http.Request) bool {
	v, _ := c.inForce(r)
	return slices.Contains(v.Document.Approvers, current(r).user)
}

// inForce is the policy version being enforced; unreadable, an empty one
// (no approvers, no processes) and false.
func (c *Console) inForce(r *http.Request) (policy.Version, bool) {
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: the policy in force: loaded %v, %v", ok, err)
		return policy.Version{}, false
	}
	return v, true
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
	c.render(w, "forbidden.html", map[string]any{"User": user})
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
