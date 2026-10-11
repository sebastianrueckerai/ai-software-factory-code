// Package console is the human's view of the factory at
// console.aisoftwarefactory.world (task 54): Forgejo OAuth2 login for the
// projects' writers (ruling 254), the board, new tasks filed as the human, the
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
	// BoardTask is task id as BoardTasks reads it, listed there or not.
	BoardTask(ctx context.Context, id string) (factoryd.BoardTask, bool, error)
	// PlanRevision is revision n of plan task id's plan, the newest for 0.
	PlanRevision(ctx context.Context, id string, n int) (factoryd.PlanRevision, bool, error)
	// PlanCards are the cards plan task plan filed.
	PlanCards(ctx context.Context, plan string) ([]factoryd.Task, error)
	// Dependencies are the tasks task id depends on, those not done
	// first; Dependents those that depend on it.
	Dependencies(ctx context.Context, id string) ([]factoryd.Dependency, error)
	Dependents(ctx context.Context, id string) ([]factoryd.Dependency, error)
	// SetHumanSummary is a writer's summary of task id; "" restores the
	// generated title (the column back to NULL).
	SetHumanSummary(ctx context.Context, id, summary string) error
	// SetTaskAccent is a writer's colour label on task id; "" removes it.
	SetTaskAccent(ctx context.Context, id, accent string) error
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
	// Writers are each project's writers by repository, and own is the
	// factory's own code project's repository (the one deploying into
	// factoryd's namespace): a writer of any project may log in, a writer
	// of own also reaches the factory-level pages (ruling 254).
	Writers   func(ctx context.Context) (own string, writers map[string][]string, err error)
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
	// Bus is the in-process event broadcaster: the stream subscribes to it
	// so every Record fans out without polling the table (factoryd.EventBus).
	Bus *factoryd.EventBus
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
	// Steer hands a writer's message to a task's running session
	// (factoryd's Runner.MessageSession); nil offers the project agent no
	// steer.
	Steer func(ctx context.Context, taskID, user, text string) error
	// Chat keeps the project agent's conversations and chat tasks, and
	// Operator is its model; without either the chat answers 404.
	Chat     Chat
	Operator *factoryd.OperatorModel
	// Pulses are the projects' pulses (factoryd.PGStore); nil shows none.
	Pulses Pulses
	// Mirrors are the projects' GitHub exports; nil offers none.
	Mirrors Mirrors
	// AddCollaborator makes user a collaborator of repo with permission
	// as the token registration keeps project repositories with
	// (factoryd's Forgejo.AddProjectCollaborator): an invited member.
	AddCollaborator func(ctx context.Context, repo factoryd.Repo, user, permission string) error
}

// Console is the console's http.Handler.
type Console struct {
	cfg  Config
	tmpl *template.Template
	mux  *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*session
	writers  writerLists
	writersT time.Time
	flight   singleflight.Group
	moves    map[string]answer // the board's MayMove answers
	checks   map[string]checks // the cards' checks counts, by task
	// runs are the new projects' merge refusals and registrations, in
	// memory for the status panel.
	runs map[string]*projectRun
	held map[string]heldToken // the chat's proposals' tokens: a restart forgets them
}

// writerLists are each project's writers by repository and the factory's
// own code project's repository (Config.Writers).
type writerLists struct {
	own string
	by  map[string][]string
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
		sessions: map[string]*session{}, runs: map[string]*projectRun{}, held: map[string]heldToken{}}
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
	c.mux.Handle("GET /tasks/new", c.auth(c.factory(c.newTaskForm)))
	c.mux.Handle("POST /tasks", c.auth(c.createTask))
	c.mux.Handle("POST /tasks/{id}/answer", c.auth(c.answer))
	c.mux.Handle("POST /tasks/{id}/summary", c.auth(c.setSummary))
	c.mux.Handle("POST /tasks/{id}/accent", c.auth(c.setAccent))
	c.mux.Handle("POST /cards/{id}/lane", c.auth(c.moveCard))
	c.mux.Handle("POST /tasks/{id}/start-plan", c.auth(c.startPlan))
	c.mux.Handle("POST /tasks/{id}/plan/accept", c.auth(c.acceptPlan))
	c.mux.Handle("POST /tasks/{id}/plan/changes", c.auth(c.planChanges))
	c.mux.Handle("POST /tasks/{id}/plan/reject", c.auth(c.rejectPlan))
	c.mux.Handle("POST /tasks/{id}/retry", c.auth(c.retryCard))
	c.mux.Handle("GET /tasks/{id}/timeline", c.auth(c.timeline))
	c.mux.Handle("GET /costs", c.auth(c.factory(c.costs)))
	c.mux.Handle("GET /policy", c.auth(c.factory(c.policy)))
	c.mux.Handle("POST /approvals/{id}/approve", c.auth(c.approve))
	c.mux.Handle("POST /approvals/{id}/deny", c.auth(c.deny))
	c.mux.Handle("GET /resources", c.auth(c.factory(c.resources)))
	c.mux.Handle("POST /resources/{id}/promote", c.auth(c.factory(c.spend("promote", c.promote))))
	c.mux.Handle("POST /resources/{id}/release", c.auth(c.factory(c.release)))
	c.mux.Handle("GET /alerts", c.auth(c.factory(c.alerts)))
	c.mux.Handle("GET /projects", c.auth(c.factory(c.projects)))
	c.mux.Handle("GET /projects/new", c.auth(c.factory(c.newProjectForm)))
	c.mux.Handle("POST /projects", c.auth(c.factory(c.spend("add a project", c.createProject))))
	c.mux.Handle("GET /projects/{name}/status", c.auth(c.factory(c.projectStatus)))
	c.mux.Handle("GET /projects/{name}/events", c.auth(c.projectEvents))
	c.mux.Handle("GET /projects/{name}/events/stream", c.auth(c.projectEventsStream))
	c.mux.Handle("GET /cards/{id}", c.auth(c.cardFragment))
	c.mux.Handle("GET /cards/{id}/drawer", c.auth(c.cardDrawer))
	c.mux.Handle("POST /projects/{name}/merge", c.auth(c.factory(c.spend("merge a project's file", c.mergeProject))))
	c.mux.Handle("POST /alerts/{id}/verdict", c.auth(c.factory(c.verdict)))
	c.mux.Handle("GET /projects/{name}/chat", c.auth(c.chatGet))
	c.mux.Handle("POST /projects/{name}/chat", c.auth(c.chatPost))
	c.mux.Handle("POST /projects/{name}/chat/new", c.auth(c.chatNew))
	c.mux.Handle("GET /projects/{name}/chat/conversations", c.auth(c.chatConversations))
	c.mux.Handle("POST /projects/{name}/chat/proposals/{id}", c.auth(c.chatProposal))
	c.mux.Handle("POST /projects/{name}/chat/undo/{token}", c.auth(c.chatUndo))
	c.mux.Handle("GET /projects/{name}/pulse", c.auth(c.pulseGet))
	if cfg.Mirrors != nil {
		c.mux.Handle("POST /projects/{name}/export", c.auth(c.githubExport))
		c.mux.Handle("POST /projects/{name}/export/remove", c.auth(c.removeGithubExport))
	}
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

// auth admits a request with a live session of a writer of any project and, for
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
		ok, _, err := c.writer(r.Context(), s.user)
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

// end ends session s, whatever its cookie.
func (c *Console) end(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.sessions {
		if v == s {
			delete(c.sessions, k)
		}
	}
}

// writer reports whether user writes any project and whether they write
// the factory's own, from the writer lists read at most once a minute by
// one request while the others wait for it; when Forgejo fails, the last
// lists serve for up to five minutes.
func (c *Console) writer(ctx context.Context, user string) (admitted, factory bool, err error) {
	c.mu.Lock()
	lists, at := c.writers, c.writersT
	c.mu.Unlock()
	if time.Since(at) >= writersTTL {
		v, err, _ := c.flight.Do("writers", func() (any, error) {
			own, by, err := c.cfg.Writers(context.WithoutCancel(ctx))
			l := writerLists{own, by}
			if err == nil {
				c.mu.Lock()
				c.writers, c.writersT = l, time.Now()
				c.mu.Unlock()
			}
			return l, err
		})
		switch {
		case err == nil:
			lists = v.(writerLists)
		case lists.by != nil && time.Since(at) < writersStale:
			log.Printf("console: writers: %v; serving the lists of %s", err, at.UTC().Format(time.RFC3339))
		default:
			return false, false, err
		}
	}
	for _, users := range lists.by {
		admitted = admitted || slices.Contains(users, user)
	}
	return admitted, slices.Contains(lists.by[lists.own], user), nil
}

// factory refuses a factory-level page or action to anyone but the
// writers of the factory's own code project (ruling 254).
func (c *Console) factory(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch _, ok, err := c.writer(r.Context(), current(r).user); {
		case err != nil:
			log.Printf("console: writers: %v", err)
			http.Error(w, "cannot check repository writers", http.StatusServiceUnavailable)
		case !ok:
			http.Error(w, "Only the writers of the factory's own code see the factory's pages.", http.StatusForbidden)
		default:
			next(w, r)
		}
	}
}

// factoryWriter reports whether the request's human writes the factory's
// own code project: the layout links the factory-level pages for them.
func (c *Console) factoryWriter(r *http.Request) bool {
	_, ok, _ := c.writer(r.Context(), current(r).user)
	return ok
}

// approver reports whether the session's human approves the factory's
// own code project: the factory-level pages' spend and verdicts.
func (c *Console) approver(r *http.Request) bool {
	c.mu.Lock()
	own := c.writers.own
	c.mu.Unlock()
	return c.isApprover(r.Context(), current(r).user, own)
}

// isApprover reports whether user approves repository's project: a name on
// its file's approvers, else the policy's (ruling 254). Unreadable
// policy: no.
func (c *Console) isApprover(ctx context.Context, user, repository string) bool {
	v, _ := c.inForce(ctx)
	return slices.Contains(v.ApproversOf(repository), user)
}

// inForce is the policy version being enforced; unreadable, an empty one
// (no approvers, no processes) and false.
func (c *Console) inForce(ctx context.Context) (policy.Version, bool) {
	v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	if err != nil || !ok {
		log.Printf("console: the policy in force: loaded %v, %v", ok, err)
		return policy.Version{}, false
	}
	return v, true
}

// spend refuses a factory-level spend action to anyone but the factory's
// approvers.
func (c *Console) spend(what string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.approver(r) {
			log.Printf("console: %s is no approver: %s refused", current(r).user, what)
			http.Error(w, "Only the factory's approvers may authorize spend.", http.StatusForbidden)
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
