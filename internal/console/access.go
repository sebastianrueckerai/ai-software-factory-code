package console

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// role is a role the console's access is judged by (roadmap 185): the
// login's member floor, a writer of the project a request is scoped to,
// a writer of the factory's own code, an approver of the scoped project.
type role uint8

const (
	member role = 1 << iota
	writer
	factoryWriter
	approver
)

// scope is where a rule's roles are judged: no project, the factory's
// own, the project a route's path ({project}, else {name}) or a chat's
// turn names, the project of the card a route or a tool's arguments
// names, of an approval request, or of any task an id names, a chat
// task's included (a chat task is a card of its project).
type scope uint8

const (
	scNone scope = iota
	scFactory
	scProject
	scChat
	scCard
	scApproval
	scTask
)

// accessRule is one route's or tool's entry in accessTable: every role
// in need must hold, judged in the order the wrappers had (the factory's
// writers, the project's, its approvers), or for a lane move one of
// either. Scope is where; what names an approver's action in its
// refusal; hide answers a GET's refusal as 404, so a card's existence
// stays hidden from a member who may not see it.
type accessRule struct {
	need, either role
	scope        scope
	what         string
	hide         bool
}

// The table's repeated rules.
var (
	rMember  = accessRule{need: member}
	rFactory = accessRule{need: factoryWriter, scope: scFactory}
	rCard    = accessRule{need: writer, scope: scCard}
	rProject = accessRule{need: writer, scope: scProject}
	rLane    = accessRule{need: member, either: writer | approver, scope: scCard}
)

// accessTable is every console route's and chat tool's access in one
// place (roadmap 185): the route's pattern or the tool's name, and the
// roles it requires beyond the login. New refuses to start a route or
// tool that is not in it, and the parity test fails an entry nothing
// registered or offered has. No handler or action judges a role itself:
// both dispatchers read this table.
var accessTable = map[string]accessRule{
	// GET routes
	"GET /static/":                               {},
	"GET /login":                                 {},
	"GET /notifications":                         rMember,
	"GET /messengers/discord/start":              rMember,
	"GET /messengers/discord/callback":           rMember,
	"GET /auth/callback":                         {},
	"GET /{$}":                                   rMember,
	"GET /board":                                 rMember,
	"GET /board/finished":                        rMember,
	"GET /tasks/new":                             rFactory,
	"GET /tasks/{id}/timeline":                   {need: writer, scope: scTask},
	"GET /costs":                                 rFactory,
	"GET /policy":                                rFactory,
	"GET /resources":                             rFactory,
	"GET /settings":                              rFactory,
	"GET /settings/new":                          rFactory,
	"GET /projects/{name}/status":                rFactory,
	"GET /projects/{name}/events":                rProject,
	"GET /projects/{name}/metrics":               rProject,
	"GET /projects/{name}/events/stream":         rMember, // follows() judges the project's sight
	"GET /cards/{id}":                            {need: writer, scope: scCard, hide: true},
	"GET /cards/{id}/drawer":                     rCard,
	"GET /routines/{project}/{name}/drawer":      rProject,
	"GET /projects/{name}/chat":                  rProject,
	"GET /projects/{name}/chat/conversations":    rProject,
	"GET /projects/{name}/chat/attachments/{id}": rProject, // any member reads any thread's images, as its rows
	"GET /projects/{name}/docs/{path...}":        rMember,  // docRead judges the project's sight, as the stream does
	"POST /projects/{name}/docs/{path...}":       rMember,  // docWrite judges the project's sight; Forgejo the member's commit

	// POST routes
	"POST /messengers/telegram":                 {}, // the header secret is the route's own check
	"POST /notifications/telegram/code":         rMember,
	"POST /notifications/disconnect":            rMember,
	"POST /notifications/level":                 rMember, // follows() judges the project's sight, as the stream does
	"POST /logout":                              rMember,
	"POST /tasks":                               rFactory,
	"POST /tasks/{id}/answer":                   rCard,
	"POST /tasks/{id}/fund":                     {need: approver, scope: scCard, what: "fund a card"},
	"POST /tasks/{id}/summary":                  rCard,
	"POST /tasks/{id}/accent":                   rCard,
	"POST /cards/{id}/lane":                     rLane,
	"POST /cards/{id}/thread":                   rCard,
	"POST /tasks/{id}/start-plan":               {need: approver, scope: scCard, what: "start a plan"},
	"POST /tasks/{id}/plan/accept":              {need: approver, scope: scCard, what: "accept a plan"},
	"POST /tasks/{id}/plan/answer":              rCard,
	"POST /tasks/{id}/plan/holdout":             rCard,
	"POST /tasks/{id}/plan/changes":             rCard,
	"POST /tasks/{id}/plan/reject":              rCard,
	"POST /tasks/{id}/retry":                    {need: approver, scope: scCard, what: "retry a card"},
	"POST /tasks/{id}/close":                    rCard,
	"POST /tasks/{id}/acceptance-close":         {need: approver, scope: scCard, what: "close a card on its acceptance"},
	"POST /tasks/{id}/steer":                    rCard,
	"POST /tasks/{id}/split":                    {need: approver, scope: scCard, what: "split a card"},
	"POST /approvals/{id}/approve":              {need: approver, scope: scApproval, what: "authorize spend"},
	"POST /approvals/{id}/deny":                 {need: writer, scope: scApproval},
	"POST /approvals/{id}/waive":                {need: approver, scope: scApproval, what: "waive a criterion"},
	"POST /approvals/{id}/rerun":                {need: writer, scope: scApproval},
	"POST /resources/{id}/promote":              {need: factoryWriter | approver, scope: scFactory, what: "authorize spend"},
	"POST /resources/{id}/release":              rFactory,
	"POST /projects":                            {need: factoryWriter | approver, scope: scFactory, what: "add a project"},
	"POST /projects/{name}/merge":               {need: factoryWriter | approver, scope: scFactory, what: "merge a project's file"},
	"POST /alerts/{id}/verdict":                 {need: factoryWriter | approver, scope: scFactory, what: "judge an alert"},
	"POST /projects/{name}/chat":                rProject,
	"POST /projects/{name}/chat/new":            rProject,
	"POST /projects/{name}/chat/rename":         rProject,
	"POST /projects/{name}/chat/seen":           rProject,
	"POST /projects/{name}/chat/clear":          rProject,
	"POST /projects/{name}/chat/archive":        rProject,
	"POST /projects/{name}/chat/delete":         rProject,
	"POST /projects/{name}/chat/answer":         rProject,
	"POST /projects/{name}/chat/fund":           {need: approver, scope: scProject, what: "fund the chat"},
	"POST /projects/{name}/chat/proposals/{id}": rProject,
	"POST /projects/{name}/chat/undo/{token}":   rProject,
	"POST /projects/{name}/autonomy":            {need: approver, scope: scProject, what: "change the autonomy level"},
	"POST /projects/{name}/intake":              {need: approver, scope: scProject, what: "switch the project's intake"},
	"POST /projects/{name}/export":              {need: writer | approver, scope: scProject, what: "export it to GitHub"},
	"POST /projects/{name}/export/remove":       {need: writer | approver, scope: scProject, what: "remove its GitHub export"},
	"POST /projects/{name}/export/sync":         {need: writer | approver, scope: scProject, what: "sync it from GitHub"},
	"POST /routines/{project}/{name}/skip":      rProject,
	"POST /routines/{project}/{name}/run":       rProject,
	"POST /routines/{project}/{name}/needs-me":  rProject,

	// chat tools: reads
	"system": rMember, "board": rMember, "card": rMember, "history": rMember, "card_history": rMember,
	"diff": rMember, "plan": rMember, "pr": rMember, "file": rMember, "session": rMember,
	"session_steps": rMember, "session_step": rMember, "session_search": rMember, "ci": rMember,
	"read_doc": rMember, "notes_read": rMember, "active_threads": rMember, "chat_search": rMember, "routines": rMember,
	"board_view": rMember, "board_view_reset": rMember,
	"watch_card": rMember, "unwatch_card": rMember, "set_timer": rMember, "analyze_image": rMember, "read_attachment": rMember, "summarize_attachment": rMember,
	"costs": rFactory, "alerts": rFactory, "deployment_health": rMember, "resources": rFactory, "settings": rFactory, "policy": rFactory,
	"project_images": rMember,

	// chat tools: writes
	"alert_verdict":    {need: factoryWriter | approver, scope: scFactory, what: "judge an alert"},
	"promote_resource": {need: factoryWriter | approver, scope: scFactory, what: "authorize spend"},
	"release_resource": rFactory,
	"new_card":         {need: writer, scope: scChat},
	"request_image":    {need: writer, scope: scChat},
	"move_card":        rLane,
	"answer":           rCard,
	"tell_card":        rCard,
	"comment":          rCard,
	"steer":            rCard,
	"retry":            {need: approver, scope: scCard, what: "retry a card"},
	"start_plan":       {need: approver, scope: scCard, what: "start a plan"},
	"accept_plan":      {need: approver, scope: scCard, what: "accept a plan"},
	"request_changes":  rCard,
	"reject_plan":      rCard,
	"approve_merge":    {need: approver, scope: scCard, what: "authorize spend"},
	"deny_merge":       rCard,
	"rerun_checks":     rCard,
	"set_summary":      rCard,
	"set_accent":       rCard,
	"invite":           {need: approver, scope: scChat, what: "invite a member"},
	"github_export":    {need: writer | approver, scope: scChat, what: "export it to GitHub"},
	"github_sync":      {need: writer | approver, scope: scChat, what: "sync it from GitHub"},
	"close":            rCard,
	"edit":             rCard,
	"grant_session":    rCard,
	"fund":             {need: approver, scope: scCard, what: "fund a card"},
	"set_dependency":   rCard,
	"split_card":       {need: approver, scope: scCard, what: "split a card"},
	"unmove_card":      rCard,
	"delete_comment":   rCard,
	"set_autonomy":     {need: approver, scope: scChat, what: "change the autonomy level"},
	"routine_write":    {need: approver, scope: scChat, what: "change a routine"},
	"routine_delete":   {need: approver, scope: scChat, what: "delete a routine"},
	"routine_run":      {need: writer, scope: scChat},
	"routine_skip":     {need: writer, scope: scChat},
	"write_doc":        {need: writer, scope: scChat},
	"notes_write":      {need: writer, scope: scChat},
	"escalate":         {need: writer, scope: scChat},
	"ask":              {need: writer, scope: scChat},
	"attach_analysis":  {need: writer, scope: scChat},
}

// admit refuses a unless they hold rule's roles over repository, whose
// subject the refusal names: the one check both dispatchers make, so no
// handler or action judges a role by hand (roadmap 185).
func (c *Console) admit(ctx context.Context, rule accessRule, a actor, repository, subject string) error {
	if rule.need&factoryWriter != 0 {
		if err := c.factoryAccess(ctx, a); err != nil {
			return err
		}
	}
	if rule.need&writer != 0 {
		if err := c.writes(ctx, a, repository, subject); err != nil {
			return err
		}
	} else if rule.either&(writer|approver) != 0 {
		// a lane move takes a writer or an approver; the lane's moved_by,
		// which the move itself judges, decides which
		w, werr := c.mayMoveCached(ctx, repository, a.s.user, policy.MovedByWriters)
		p, perr := c.mayMoveCached(ctx, repository, a.s.user, policy.MovedByApprovers)
		if werr != nil && perr != nil {
			return refuse(http.StatusServiceUnavailable, "cannot check the project's writers")
		}
		if !w && !p {
			return c.writes(ctx, a, repository, subject)
		}
	}
	if rule.need&approver != 0 {
		if err := c.approves(ctx, a, repository, rule.what); err != nil {
			return err
		}
	}
	return nil
}

// scopeOf resolves sc's repository and the subject its refusal names
// from the request's path; a missing project, card or request refuses as
// its handler did.
func (c *Console) scopeOf(sc scope, r *http.Request) (string, string, error) {
	ctx := r.Context()
	switch sc {
	case scFactory:
		return c.writersOwn(), "", nil
	case scProject:
		name := cmp.Or(r.PathValue("project"), r.PathValue("name"))
		p, ok, err := c.projectOf(ctx, name)
		switch {
		case err != nil:
			return "", "", refuse(http.StatusInternalServerError, "cannot read the projects")
		case !ok:
			return "", "", refuse(http.StatusNotFound, "There is no project "+name+".")
		}
		return p.Repository, p.Name, nil
	case scCard:
		t, err := c.cardIn(ctx, button(r), r.PathValue("id"))
		if err != nil {
			return "", "", err
		}
		return t.Repository, "task " + t.ID, nil
	case scApproval:
		q, err := c.pendingRequest(ctx, button(r), pathID(r))
		if err != nil {
			return "", "", err
		}
		return q.Project, fmt.Sprintf("approval request %d", q.ID), nil
	case scTask:
		t, ok, err := c.cfg.Tasks.Task(ctx, r.PathValue("id"))
		switch {
		case err != nil:
			log.Printf("console: task %s: %v", r.PathValue("id"), err)
			return "", "", refuse(http.StatusInternalServerError, "cannot read the task")
		case !ok:
			return "", "", refuse(http.StatusNotFound, "There is no card "+r.PathValue("id")+" in this project.")
		}
		return t.Repository, "task " + t.ID, nil
	}
	return "", "", nil
}

// pathID is the path's {id} as a number; a bad one names nothing.
func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// route is handler under the access table's rule for pattern: the login
// and CSRF check of auth, then the table's roles over the pattern's
// scope. A rule of no role takes no session: the sign-in pages.
func (c *Console) route(pattern string, handler http.Handler) http.Handler {
	rule := accessTable[pattern]
	if rule.need == 0 && rule.either == 0 && rule.scope == scNone {
		return handler
	}
	return c.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repository, subject, err := c.scopeOf(rule.scope, r)
		if err == nil {
			err = c.admit(r.Context(), rule, button(r), repository, subject)
		}
		if err != nil && rule.hide {
			var no *refusal
			if errors.As(err, &no) && no.status == http.StatusForbidden {
				err = refuse(http.StatusNotFound, "There is no card "+r.PathValue("id")+" in this project.")
			}
		}
		if err != nil {
			c.fail(w, r, err)
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

// checkAccess fails a route or tool the access table misses, and an
// entry nothing registered or offered has: New refuses to start without
// the table whole (roadmap 185).
func (c *Console) checkAccess() error {
	routes := map[string]bool{}
	gets, err := c.getRoutes()
	if err != nil {
		return err
	}
	for _, g := range gets {
		routes[g.pattern] = true
	}
	for _, p := range c.postRoutes() {
		routes[p.pattern] = true
	}
	var errs []error
	for pattern := range routes {
		if _, ok := accessTable[pattern]; !ok {
			errs = append(errs, fmt.Errorf("console: route %s is not in the access table", pattern))
		}
	}
	for _, tool := range tools {
		if _, ok := accessTable[tool.name]; !ok {
			errs = append(errs, fmt.Errorf("console: tool %s is not in the access table", tool.name))
		}
	}
	for key := range accessTable {
		if !routes[key] && !slices.ContainsFunc(tools, func(t chatTool) bool { return t.name == key }) {
			errs = append(errs, fmt.Errorf("console: the access table names %s, which is no route or tool", key))
		}
	}
	return errors.Join(errs...)
}

// admit refuses the turn's member tool name's table roles, its card
// scope read from in: the chat dispatch's one check (roadmap 185).
func (t *turn) admitTool(name string, in toolInput) error {
	if !policy.OffersTool(name, t.p.Namespaces) {
		return refuse(http.StatusConflict, "project "+t.p.Name+" lists no namespaces: it runs nothing in the cluster for "+name+" to read")
	}
	rule, ok := accessTable[name]
	if !ok {
		return nil
	}
	repository, subject, err := t.scopeOf(rule.scope, in)
	if err != nil {
		return err
	}
	return t.c.admit(t.r.Context(), rule, t.a, repository, subject)
}

// offers reports whether the turn's member holds tool name's table roles
// over the chat's project, as admitTool judges them before a card is
// named: a card the turn reaches is the chat project's. A tool that reads
// where the project runs is offered only where it runs (policy.OffersTool).
func (t *turn) offers(name string) bool {
	if !policy.OffersTool(name, t.p.Namespaces) {
		return false
	}
	rule := accessTable[name]
	repository := t.p.Repository
	if rule.scope == scFactory {
		repository = t.c.writersOwn()
	}
	// judged here first, so a tool left unoffered logs no refused action
	if rule.need&approver != 0 && !t.c.isApprover(t.r.Context(), t.a.s.user, repository) {
		return false
	}
	return t.c.admit(t.r.Context(), rule, t.a, repository, t.p.Name) == nil
}

func (t *turn) scopeOf(sc scope, in toolInput) (string, string, error) {
	switch sc {
	case scFactory:
		return t.c.writersOwn(), "", nil
	case scChat:
		return t.p.Repository, t.p.Name, nil
	case scCard:
		task, err := t.c.cardIn(t.r.Context(), t.a, in.TaskID)
		if err != nil {
			return "", "", err
		}
		return task.Repository, "task " + in.TaskID, nil
	}
	return "", "", nil
}
