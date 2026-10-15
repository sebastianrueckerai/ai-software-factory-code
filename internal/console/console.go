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
	// TaskByIssue is repository's card of issue number issue.
	TaskByIssue(ctx context.Context, repository string, issue int64) (factoryd.Task, bool, error)
	// BoardTasks lists repository's open tasks, each with its latest
	// escalation's reason.
	BoardTasks(ctx context.Context, repository string) ([]factoryd.BoardTask, error)
	// FinishedTasks lists the page of a project's finished cards q asks
	// for, and how many the read leaves from the page on.
	FinishedTasks(ctx context.Context, q factoryd.FinishedQuery) ([]factoryd.BoardTask, int, error)
	// BoardTask is task id as BoardTasks reads it, listed there or not.
	BoardTask(ctx context.Context, id string) (factoryd.BoardTask, bool, error)
	// PlanRevision is revision n of plan task id's plan, the newest for 0.
	PlanRevision(ctx context.Context, id string, n int) (factoryd.PlanRevision, bool, error)
	// PlanQuestions are revision's questions of plan task id, in order.
	PlanQuestions(ctx context.Context, id string, revision int) ([]factoryd.PlanQuestion, error)
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
	// SetObjective is task id's objective after a writer edited its issue.
	SetObjective(ctx context.Context, id, objective string) error
	// RaiseTaskBudget raises task id's budget_eur by eur in one
	// transaction and answers the budget before and after the raise;
	// ErrNotFound when there is no such task.
	RaiseTaskBudget(ctx context.Context, id string, eur float64) (from, to float64, err error)
	// AddTaskDeps and RemoveTaskDeps make task id depend on the tasks
	// dependsOn, or no longer.
	AddTaskDeps(ctx context.Context, id string, dependsOn []string) error
	RemoveTaskDeps(ctx context.Context, id string, dependsOn []string) error
	// CountAdmitted and CountBacklog count the project's work slots and
	// its unstarted filings, the ready check's room (roadmap 239).
	CountAdmitted(ctx context.Context, repository string) (int, error)
	CountBacklog(ctx context.Context, repository string) (int, error)
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
	// Sessions keeps the console's logins across a restart of factoryd:
	// the rows of console_sessions (factoryd.PGStore). Without it nobody
	// can be logged in.
	Sessions Sessions
	Tasks    Tasks
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
	// RerunChecks and CloseFactoryPR decide a factory pull request card's
	// merge request as a member's re-run of its red checks or close with
	// their words (factoryd.RerunFactoryChecks, factoryd.CloseFactoryPR).
	RerunChecks    MergeApprover
	CloseFactoryPR FactoryCloser
	Resources      Resources
	Alerts         Alerts
	Projects       Projects
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
	// AcceptPlan, AnswerPlan and RejectPlan signal a plan task's workflow
	// (factoryd.AcceptPlan, factoryd.AnswerPlan, factoryd.RejectPlan).
	AcceptPlan func(ctx context.Context, taskID string, revision int, sha, by string) error
	AnswerPlan func(ctx context.Context, taskID string, revision, idx int, answer, by string) error
	// AnswerPlanQuestions signals a plan task's workflow with every answer
	// of one turn at once (factoryd.AnswerPlanQuestions).
	AnswerPlanQuestions func(ctx context.Context, taskID string, revision int, answers []factoryd.PlanAnswer, by string) error
	RejectPlan          func(ctx context.Context, taskID, by, why string) error
	// RetryPlanCard files failed plan card taskID again for by and returns
	// the replacement's issue (factoryd's Runner.RetryPlanCard): a refusal
	// is factoryd.ErrNoRetry.
	RetryPlanCard func(ctx context.Context, taskID, by string) (int64, error)
	// CloseCard ends card taskID, which has no issue, closed for by
	// (factoryd's Intake.CloseCard).
	CloseCard func(ctx context.Context, taskID, by string) error
	// CloseCardDone ends card taskID done for by on evidence (factoryd's
	// Runner.CloseCardDone): a card whose sessions ran one of its plan's
	// acceptance commands green (roadmap 208) closes on the fact, no
	// answer owed.
	CloseCardDone func(ctx context.Context, taskID, by, evidence string) error
	// SplitCard signals card taskID's workflow to split its large pull
	// request for by (factoryd.SignalSplit).
	SplitCard func(ctx context.Context, taskID, by string) error
	// AnswerWhole signals card taskID's workflow that comment, which the
	// answer box posted for by, is a whole answer
	// (factoryd.SignalAnswerWhole): the card resumes on it without the
	// quiet window an answer in parts waits out. nil sends none.
	AnswerWhole func(ctx context.Context, taskID string, comment int64, by string) error
	// InfraRepo is the infra repository (owner/name), where the new-project
	// button files its pull request; InfraToken reads its pull requests
	// (factoryd's forgejo-config token).
	InfraRepo  factoryd.Repo
	InfraToken string
	// DocsToken is the docs writer's Forgejo token, which write_doc
	// commits documents with (FORGEJO_DOCS_TOKEN); empty: no chat writes
	// documents.
	DocsToken string
	// CheckProject judges projects/<name>.yaml with file's text the
	// loader's way against the tree at infra's head
	// (factoryd.CheckProjectFile): the refusal's lines, none when it
	// passes; err when the tree could not be read.
	CheckProject func(ctx context.Context, name string, file []byte) (refused []string, err error)
	// CheckChange judges the tree at infra's head with changes made, each
	// a path under the configuration directory to its new content, nil
	// deleting the file, the loader's way (factoryd.CheckChange): the
	// refusal's lines, none when it passes; err when the tree could not
	// be read.
	CheckChange func(ctx context.Context, changes map[string][]byte) (refused []string, err error)
	// MainExists reports whether a project's repository has main.
	MainExists func(ctx context.Context, repo factoryd.Repo) (bool, error)
	// ReconcileError is Registrar.ReconcileError: why the project's last
	// reconcile failed, "" when it was clean.
	ReconcileError func(name string) string
	// Kustomization reads the Flux Kustomization name in namespace
	// (factoryd's InClusterKube.KustomizationState); factoryd.ErrNotFound
	// when there is none. nil shows no environment's deploy state.
	Kustomization func(ctx context.Context, namespace, name string) (factoryd.FluxState, error)
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
	// AttemptFunded is the answer path's floor (ruling 322): "" when the
	// task's remaining budget can fund a call on model, else the refusal
	// the answer carries (factoryd's Runner.AttemptFunded); nil checks
	// nothing.
	AttemptFunded func(ctx context.Context, taskID, model string) (string, error)
	// SessionFunded is the fund's floor (roadmap 243): "" when the task's
	// free headroom can fund a session's opening calls on model, else the
	// refusal (factoryd's Runner.SessionFunded); nil offers no fund.
	SessionFunded func(ctx context.Context, taskID, model string) (string, error)
	// Chat keeps the project agent's conversations and chat tasks, and
	// Operator is its model; without either the chat answers 404.
	Chat     Chat
	Operator *factoryd.OperatorModel
	// Pulses are the projects' pulses (factoryd.PGStore); nil shows none.
	Pulses Pulses
	// Notes are the projects' scratchpads (factoryd.PGStore); nil: the
	// chat's notes tools say there is none.
	Notes Notes
	// Escalate files the chat agent's escalation on repository as a
	// human-lane card in Needs you, its question and choices on the
	// inbox's poke, and answers its issue (factoryd's EscalateToHuman);
	// nil: the escalate tool says the factory files none.
	Escalate func(ctx context.Context, repository, question string, choices []string, by string) (int64, error)
	// Mirrors are the projects' GitHub exports; nil offers none.
	Mirrors Mirrors
	// Routines are the projects' routines; nil offers no routine act.
	Routines Routines
	// GitHubSync brings a project's GitHub default branch back as a pull
	// request to main (factoryd's Registrar.GitHubSync); nil offers none.
	GitHubSync func(ctx context.Context, project, by string) (string, error)
	// AddCollaborator makes user a collaborator of repo with permission
	// as the token registration keeps project repositories with
	// (factoryd's Forgejo.AddProjectCollaborator): an invited member.
	AddCollaborator func(ctx context.Context, repo factoryd.Repo, user, permission string) error
	// SetIssueDependency makes issue number of repo depend on issue
	// dependency, or with remove no longer, as the bot, which keeps
	// Forgejo's dependencies as it does a plan's; an issue already as asked
	// is left alone (factoryd's Forgejo.SetIssueDependency).
	SetIssueDependency func(ctx context.Context, repo factoryd.Repo, number, dependency int64, remove bool) error
}

// Console is the console's http.Handler.
type Console struct {
	cfg  Config
	tmpl *template.Template
	mux  *http.ServeMux

	mu       sync.Mutex
	writers  writerLists
	writersT time.Time
	flight   singleflight.Group
	moves    map[string]answer // the board's MayMove answers
	checks   map[string]checks // the cards' checks counts, by task
	// runs are the new projects' merge refusals and registrations, in
	// memory for the status panel.
	runs map[string]*projectRun
	// turns holds each chat thread's turn in flight and those queued
	// behind it, and each project's model turns (roadmap 239).
	turns *turnGate
	// refreshMu serializes token refreshes across requests: two concurrent
	// requests presenting the same refresh token would both try to use it,
	// Forgejo invalidates refresh tokens on use (INVALIDATE_REFRESH_TOKENS
	// default true), and the second gets 400, which deletes the row.
	refreshMu sync.Mutex
}

// writerLists are each project's writers by repository and the factory's
// own code project's repository (Config.Writers).
type writerLists struct {
	own string
	by  map[string][]string
}

type session struct {
	user string
	csrf string

	// hash is the SHA-256 hex of the cookie's id: what the row of
	// console_sessions is keyed by (roadmap 164).
	hash  string
	expir time.Time

	mu      sync.Mutex // guards the tokens across a refresh
	access  string
	refresh string
	expiry  time.Time
	// seen is when last_seen was moved last; it moves at most once a
	// minute.
	seen time.Time
}

// getRoute is a GET route of the console with its handler, and the chat
// tool that reads what its page shows or the named exception's reason
// (roadmap 158, ruling 263). New registers every GET route from
// getRoutes, which the parity test reads.
type getRoute struct {
	pattern string
	handler http.HandlerFunc
	tool    string
	none    string
}

func (c *Console) getRoutes() ([]getRoute, error) {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	return []getRoute{
		{"GET /static/", func(w http.ResponseWriter, r *http.Request) {
			http.StripPrefix("/static/", http.FileServerFS(static)).ServeHTTP(w, r)
		}, "", "the static files"},
		{"GET /login", http.HandlerFunc(c.login), "", "the sign-in page: no chat before a session"},
		{"GET /auth/callback", http.HandlerFunc(c.callback), "", "the OAuth provider's callback: no page"},
		{"GET /{$}", c.board, "board", ""},
		{"GET /board", c.board, "board", ""},
		{"GET /board/finished", c.finishedPage, "", "the next page of a column of finished cards: a fragment of the board page, whose tool is board"},
		{"GET /tasks/new", c.newTaskForm, "", "the new task's form: the chat files cards (new_card), not tasks"},
		{"GET /tasks/{id}/timeline", c.timeline, "", "a card's timeline fragment: the card read names where its reviews, checks and plan are"},
		{"GET /costs", c.costs, "costs", ""},
		{"GET /policy", c.policy, "policy", ""},
		{"GET /resources", c.resources, "resources", ""},
		{"GET /alerts", c.alerts, "alerts", ""},
		{"GET /projects", c.projects, "projects", ""},
		{"GET /projects/new", c.newProjectForm, "", "the new project's form: the chat creates no project"},
		{"GET /projects/{name}/status", c.projectStatus, "", "a new project's steps after its form: the chat creates no project, and projects reads its state"},
		{"GET /projects/{name}/events", c.projectEvents, "", "a project's event feed fragment: the history tool reads the chat project's events"},
		{"GET /projects/{name}/events/stream", c.projectEventsStream, "", "the event stream: the live half of the events fragment"},
		{"GET /cards/{id}", c.cardFragment, "", "a card's face: a fragment of the board page, whose tool is card"},
		{"GET /cards/{id}/drawer", c.cardDrawer, "", "a card's drawer: a fragment of the board page, whose tool is card"},
		{"GET /projects/{name}/chat", c.chatGet, "", "the chat's own page"},
		{"GET /projects/{name}/chat/conversations", c.chatConversations, "", "the chat's conversation list: the chat's own route"},
		{"GET /projects/{name}/pulse", c.pulseGet, "", "the pulse sentence: every turn's starting context carries it"},
		{"GET /projects/{name}/docs/{path...}", c.docRead, "", "the drawer's document read: read_doc opens the same document to the agent"},
	}, nil
}

// postRoute is one POST route beside its GETs: the handler New registers
// under the access table's rule, and when, for the routes the
// configuration offers (the export forms).
type postRoute struct {
	pattern string
	handler http.Handler
	when    func() bool
}

// postRoutes is every POST route the console answers; the access table
// holds each one's rule and New refuses a route missing from it.
func (c *Console) postRoutes() []postRoute {
	mirrors, sync := func() bool { return c.cfg.Mirrors != nil }, func() bool { return c.cfg.GitHubSync != nil }
	routines := func() bool { return c.cfg.Routines != nil }
	return []postRoute{
		{"POST /logout", http.HandlerFunc(c.logout), nil},
		{"POST /tasks", http.HandlerFunc(c.createTask), nil},
		{"POST /tasks/{id}/answer", http.HandlerFunc(c.answer), nil},
		{"POST /tasks/{id}/fund", http.HandlerFunc(c.fundCard), nil},
		{"POST /tasks/{id}/summary", http.HandlerFunc(c.setSummary), nil},
		{"POST /tasks/{id}/accent", http.HandlerFunc(c.setAccent), nil},
		{"POST /cards/{id}/lane", http.HandlerFunc(c.moveCard), nil},
		{"POST /tasks/{id}/start-plan", http.HandlerFunc(c.startPlan), nil},
		{"POST /tasks/{id}/plan/accept", http.HandlerFunc(c.acceptPlan), nil},
		{"POST /tasks/{id}/plan/answer", http.HandlerFunc(c.planAnswer), nil},
		{"POST /tasks/{id}/plan/changes", http.HandlerFunc(c.planChanges), nil},
		{"POST /tasks/{id}/plan/reject", http.HandlerFunc(c.rejectPlan), nil},
		{"POST /tasks/{id}/retry", http.HandlerFunc(c.retryCard), nil},
		{"POST /tasks/{id}/close", http.HandlerFunc(c.closeCard), nil},
		{"POST /tasks/{id}/acceptance-close", http.HandlerFunc(c.acceptanceCloseCard), nil},
		{"POST /tasks/{id}/steer", http.HandlerFunc(c.steerCard), nil},
		{"POST /tasks/{id}/split", http.HandlerFunc(c.splitCard), nil},
		{"POST /approvals/{id}/approve", http.HandlerFunc(c.approve), nil},
		{"POST /approvals/{id}/deny", http.HandlerFunc(c.deny), nil},
		{"POST /approvals/{id}/rerun", http.HandlerFunc(c.rerun), nil},
		{"POST /resources/{id}/promote", c.resourceButton(c.doPromoteResource), nil},
		{"POST /resources/{id}/release", c.resourceButton(c.doReleaseResource), nil},
		{"POST /projects", http.HandlerFunc(c.createProject), nil},
		{"POST /projects/{name}/merge", http.HandlerFunc(c.mergeProject), nil},
		{"POST /alerts/{id}/verdict", http.HandlerFunc(c.verdict), nil},
		{"POST /projects/{name}/chat", http.HandlerFunc(c.chatPost), nil},
		{"POST /projects/{name}/chat/new", http.HandlerFunc(c.chatNew), nil},
		{"POST /projects/{name}/chat/rename", http.HandlerFunc(c.chatRename), nil},
		{"POST /projects/{name}/chat/seen", http.HandlerFunc(c.chatSeen), nil},
		{"POST /projects/{name}/chat/archive", http.HandlerFunc(c.chatArchive), nil},
		{"POST /projects/{name}/chat/delete", http.HandlerFunc(c.chatDelete), nil},
		{"POST /projects/{name}/chat/answer", http.HandlerFunc(c.chatAnswer), nil},
		{"POST /projects/{name}/chat/proposals/{id}", http.HandlerFunc(c.chatProposal), nil},
		{"POST /projects/{name}/chat/undo/{token}", http.HandlerFunc(c.chatUndo), nil},
		{"POST /projects/{name}/autonomy", http.HandlerFunc(c.autonomyButton), nil},
		{"POST /projects/{name}/export", http.HandlerFunc(c.githubExport), mirrors},
		{"POST /projects/{name}/export/remove", http.HandlerFunc(c.removeGithubExport), mirrors},
		{"POST /projects/{name}/export/sync", http.HandlerFunc(c.githubSync), sync},
		{"POST /routines/{project}/{name}/skip", http.HandlerFunc(c.routineSkip), routines},
		{"POST /routines/{project}/{name}/run", http.HandlerFunc(c.routineRun), routines},
	}
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
	if cfg.Sessions == nil {
		return nil, errors.New("console: no session store")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	c := &Console{cfg: cfg, tmpl: tmpl, mux: http.NewServeMux(), runs: map[string]*projectRun{}, turns: newTurnGate()}
	gets, err := c.getRoutes()
	if err != nil {
		return nil, err
	}
	for _, g := range gets {
		c.mux.Handle(g.pattern, c.route(g.pattern, g.handler))
	}
	for _, p := range c.postRoutes() {
		if p.when == nil || p.when() {
			c.mux.Handle(p.pattern, c.route(p.pattern, p.handler))
		}
	}
	if err := c.checkAccess(); err != nil {
		return nil, err
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
// X-CSRF-Token header (htmx sends it from hx-headers). A store error is
// answered as 503 so a database blip does not log the member out.
func (c *Console) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := c.session(r)
		if err != nil {
			log.Printf("console: the session read: %v", err)
			http.Error(w, "cannot read the session", http.StatusServiceUnavailable)
			return
		}
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

// session reads the request's login from console_sessions by the SHA-256
// hex of its cookie: an absent or expired row is no session, a store error
// is returned so auth can answer 503 instead of logging the member out.
// An expired row is deleted by the read; last_seen moves at most once a
// minute per login request.
func (c *Console) session(r *http.Request) (*session, error) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, nil
	}
	return c.sessionByCookie(r.Context(), ck.Value)
}

// sessionByCookie reads the login of the cookie's value; the stream,
// whose request outlives the read, uses it after taking the value once.
// A store error is returned so the caller can keep the stream or answer
// 503 instead of treating it as no session.
func (c *Console) sessionByCookie(ctx context.Context, value string) (*session, error) {
	if value == "" {
		return nil, nil
	}
	hash := sessionIDHash(value)
	row, ok, err := c.cfg.Sessions.ConsoleSessionByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	s, err := c.openSession(row)
	if err != nil {
		// the row's tokens are not this key's: the login is over.
		log.Printf("console: the session of %s: %v", row.Login, err)
		if err := c.cfg.Sessions.DeleteConsoleSession(ctx, hash); err != nil {
			log.Printf("console: the session delete: %v", err)
		}
		return nil, nil
	}
	now := time.Now()
	if now.Sub(s.seen) >= time.Minute {
		// the store moves last_seen only when the row still says the
		// value this session read, so a minute of requests over one or
		// many Console values moves the column at most once.
		if moved, err := c.cfg.Sessions.TouchConsoleSession(ctx, hash, s.seen); err != nil {
			log.Printf("console: the session's last seen: %v", err)
		} else if moved {
			s.seen = now
		}
	}
	return s, nil
}

// openSession is the row's session, its tokens decrypted.
func (c *Console) openSession(row factoryd.ConsoleSession) (*session, error) {
	access, err := openToken(c.cfg.SessionKey, row.Access, row.IDHash)
	if err != nil {
		return nil, err
	}
	refresh, err := openToken(c.cfg.SessionKey, row.Refresh, row.IDHash)
	if err != nil {
		return nil, err
	}
	return &session{
		hash: row.IDHash, user: row.Login, csrf: row.CSRF, expir: row.ExpiresAt,
		access: access, refresh: refresh, expiry: row.TokenExpiry, seen: row.LastSeen,
	}, nil
}

// drop ends the request's session (its row goes with it).
func (c *Console) drop(r *http.Request) {
	if ck, err := r.Cookie(sessionCookie); err == nil && ck.Value != "" {
		c.deleteSession(context.WithoutCancel(r.Context()), sessionIDHash(ck.Value))
	}
}

// end ends session s, whatever its cookie.
func (c *Console) end(s *session) {
	if s == nil {
		return
	}
	c.deleteSession(context.Background(), s.hash)
}

func (c *Console) deleteSession(ctx context.Context, hash string) {
	if err := c.cfg.Sessions.DeleteConsoleSession(ctx, hash); err != nil {
		log.Printf("console: the session delete: %v", err)
	}
}

// purgeSessions deletes the rows expired before now, as today's in-memory
// sweep did on every new login.
func (c *Console) purgeSessions(ctx context.Context) {
	if n, err := c.cfg.Sessions.PurgeConsoleSessions(ctx, time.Now()); err != nil {
		log.Printf("console: the session purge: %v", err)
	} else if n > 0 {
		log.Printf("console: purged %d expired console sessions", n)
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

// factoryAccess refuses a factory-level page, read or action to anyone but
// the writers of the factory's own code project (ruling 254).
func (c *Console) factoryAccess(ctx context.Context, a actor) error {
	switch _, ok, err := c.writer(ctx, a.s.user); {
	case err != nil:
		log.Printf("console: writers: %v", err)
		return refuse(http.StatusServiceUnavailable, "cannot check repository writers")
	case !ok:
		return refuse(http.StatusForbidden, "Only the writers of the factory's own code see the factory's pages.")
	}
	return nil
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
	return c.isApprover(r.Context(), current(r).user, c.writersOwn())
}

// writersOwn is the factory's own code project's repository, from the
// writer lists; "" when they were never read.
func (c *Console) writersOwn() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writers.own
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

// newSession stores s as a row of console_sessions under the SHA-256 hex
// of a fresh cookie id and sets the cookie; the lifetime is sessionTTL.
// The expired rows are purged on the way, as today's sweep was.
func (c *Console) newSession(w http.ResponseWriter, r *http.Request, s *session) error {
	ctx := context.WithoutCancel(r.Context())
	c.purgeSessions(ctx)
	id := randomToken()
	now := time.Now()
	s.hash = sessionIDHash(id)
	s.csrf = randomToken()
	s.expir = now.Add(sessionTTL)
	s.seen = now
	row, err := c.sessionRow(s)
	if err != nil {
		return err
	}
	if err := c.cfg.Sessions.SaveConsoleSession(ctx, row); err != nil {
		return err
	}
	setCookie(w, sessionCookie, id, int(sessionTTL.Seconds()))
	return nil
}

// sessionRow is s's row, its tokens sealed. The token fields are read
// under the session's mutex, which a refresh may hold.
func (c *Console) sessionRow(s *session) (factoryd.ConsoleSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	access, refresh, err := c.sealTokens(s)
	if err != nil {
		return factoryd.ConsoleSession{}, err
	}
	return factoryd.ConsoleSession{IDHash: s.hash, Login: s.user, CSRF: s.csrf,
		Access: access, Refresh: refresh, TokenExpiry: s.expiry,
		CreatedAt: s.seen, LastSeen: s.seen, ExpiresAt: s.expir}, nil
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
