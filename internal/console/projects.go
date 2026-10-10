package console

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

type projectRow struct {
	factoryd.Project
	// NotifyList is who gets pinged, NotifyDefault when that is the
	// policy's approvers.
	NotifyList    []string
	NotifyDefault bool
	Envelope      envelopeView
	GatewayYAML   string
	// File links projects/<name>.yaml at the version in force.
	File string
}

// projects shows every project read-only (ruling 183): its file's
// settings with a link to the file, where every change is made, its
// registration's state and its envelope's use.
func (c *Console) projects(w http.ResponseWriter, r *http.Request) {
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
	var rows []projectRow
	for _, p := range ps {
		row := projectRow{Project: p, NotifyList: p.NotifyOrApprovers(v.Document.Approvers), NotifyDefault: len(p.Notify) == 0,
			GatewayYAML: factoryd.ListenerYAML(p)}
		if p.File {
			row.File = c.fileLink(v, p.Name)
			if row.Envelope, err = c.envelopeOf(r.Context(), v, p.Repository); err != nil {
				log.Printf("console: envelope use of %s: %v", p.Repository, err)
				http.Error(w, "cannot read the envelopes' use", http.StatusInternalServerError)
				return
			}
		}
		rows = append(rows, row)
	}
	s := current(r)
	c.render(w, "projects.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Projects": rows,
		"New": c.cfg.ConfigRepo + "/src/branch/main/" + c.cfg.ConfigDir + "/projects"})
}

// projectRun is what the status panel keeps in memory of a new project:
// the last merge Forgejo refused.
type projectRun struct {
	merge string
}

var (
	newProjectName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,35}[a-z0-9]$`)
	treeName       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,35}[a-z0-9])?$`)
)

type newProject struct {
	Name, Process, Template, MergeRule string
}

func (c *Console) newProjectForm(w http.ResponseWriter, r *http.Request) {
	c.renderNewProject(w, r, http.StatusOK, newProject{MergeRule: policy.MergeHuman}, nil)
}

func (c *Console) renderNewProject(w http.ResponseWriter, r *http.Request, code int, p newProject, refused []string) {
	v, ok, err := c.cfg.Policies.CurrentPolicy(r.Context())
	if err != nil || !ok {
		log.Printf("console: current policy: loaded %v, %v", ok, err)
		http.Error(w, "cannot read the policy", http.StatusInternalServerError)
		return
	}
	s := current(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	c.render(w, "project_new.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Form": p, "Refused": refused,
		"Processes": slices.Sorted(maps.Keys(v.Tree.Processes)), "Templates": slices.Sorted(maps.Keys(v.Tree.Templates)),
		"Approver": c.approver(r)})
}

// projectFile is the file section 5 of stage11-design.md shows: the form's
// four fields, one staging namespace deployed from deploy/staging, no
// hostname, every other key the policy's default.
func projectFile(p newProject, user string, day time.Time) []byte {
	var b strings.Builder
	b.WriteString("# yaml-language-server: $schema=../schema/project.schema.json\n")
	fmt.Fprintf(&b, "# Filed from the console by %s on %s.\n", user, day.UTC().Format(time.DateOnly))
	fmt.Fprintf(&b, "repository: %s/%s\nmerge_rule: %s\nobjects: factory\nagents: coder\nprocess: %s\n", p.Name, p.Name, p.MergeRule, p.Process)
	if p.Template != "" {
		fmt.Fprintf(&b, "template: %s\n", p.Template)
	}
	fmt.Fprintf(&b, "namespaces: [%s-staging]\nenvironments:\n  - {namespace: %s-staging, path: deploy/staging}\n", p.Name, p.Name)
	return []byte(b.String())
}

// createProject writes the new project's file, judges it the loader's way
// against infra's head, and files it as the human: a new branch
// project-<name> off main with the one file, and its pull request.
func (c *Console) createProject(w http.ResponseWriter, r *http.Request) {
	p := newProject{Name: r.PostFormValue("name"), Process: r.PostFormValue("process"),
		Template: r.PostFormValue("template"), MergeRule: r.PostFormValue("merge_rule")}
	var refused []string
	if !newProjectName.MatchString(p.Name) {
		refused = append(refused, "name: a DNS label of 2 to 37 lowercase letters, digits and dashes, a letter or digit at each end")
	}
	if !treeName.MatchString(p.Process) {
		refused = append(refused, "process: one of the configuration's processes")
	}
	if p.Template != "" && !treeName.MatchString(p.Template) {
		refused = append(refused, "template: one of the configuration's templates, or none")
	}
	if p.MergeRule != policy.MergeHuman && p.MergeRule != policy.MergeAuto {
		refused = append(refused, "merge rule: human or auto")
	}
	if len(refused) > 0 {
		c.renderNewProject(w, r, http.StatusUnprocessableEntity, p, refused)
		return
	}
	user := current(r).user
	file := projectFile(p, user, time.Now())
	refused, err := c.cfg.CheckProject(r.Context(), p.Name, file)
	if err != nil {
		log.Printf("console: check projects/%s.yaml against infra's head: %v", p.Name, err)
		http.Error(w, "cannot read the configuration at infra's head; see the factoryd log", http.StatusBadGateway)
		return
	}
	if len(refused) > 0 {
		c.renderNewProject(w, r, http.StatusUnprocessableEntity, p, refused)
		return
	}
	infra := "/api/v1/repos/" + url.PathEscape(c.cfg.InfraRepo.Owner) + "/" + url.PathEscape(c.cfg.InfraRepo.Name)
	branch := "project-" + p.Name
	if err := c.asUser(r, http.MethodPost, infra+"/contents", map[string]any{
		"branch": "main", "new_branch": branch,
		"message": fmt.Sprintf("Project %s: add its file (console, %s)", p.Name, user),
		"files": []map[string]string{{"operation": "create", "path": c.cfg.ConfigDir + "/" + policy.ProjectFile(p.Name),
			"content": base64.StdEncoding.EncodeToString(file)}},
	}, http.StatusCreated, nil); err != nil {
		c.forgejoFailed(w, r, "commit the project's file", err)
		return
	}
	var pull struct {
		Number int64 `json:"number"`
	}
	if err := c.asUser(r, http.MethodPost, infra+"/pulls", map[string]string{"head": branch, "base": "main", "title": "Project " + p.Name},
		http.StatusCreated, &pull); err != nil {
		c.forgejoFailed(w, r, "open the pull request", err)
		return
	}
	log.Printf("console: %s filed projects/%s.yaml as %s#%d", user, p.Name, c.cfg.InfraRepo, pull.Number)
	http.Redirect(w, r, "/projects/"+p.Name+"/status", http.StatusSeeOther)
}

type infraPull struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// pullOf reads the pull request from project-<name> into infra's main with
// factoryd's config token; ok false when there is none.
func (c *Console) pullOf(ctx context.Context, name string) (pr infraPull, ok bool, err error) {
	path := "/api/v1/repos/" + url.PathEscape(c.cfg.InfraRepo.Owner) + "/" + url.PathEscape(c.cfg.InfraRepo.Name) + "/pulls/main/project-" + name
	err = c.api(ctx, c.cfg.InfraToken, http.MethodGet, path, nil, http.StatusOK, &pr)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return pr, false, nil
	}
	return pr, err == nil, err
}

const (
	stepWaiting = "waiting"
	stepRunning = "running"
	stepDone    = "done"
	stepFailed  = "failed"
)

type step struct {
	Name, Tip, State, Detail, Link string
}

type statusPanel struct {
	Name, User, CSRF string
	Steps            []step
	Final            bool
	CanMerge         bool
	// Error is why the button just pressed did nothing.
	Error string
}

// panel reads each step's state from the sources section 5's table names.
func (c *Console) panel(r *http.Request, name string) (statusPanel, error) {
	ctx := r.Context()
	s := current(r)
	out := statusPanel{Name: name, User: s.user, CSRF: s.csrf}
	pulled := step{Name: "Pull request opened", State: stepWaiting,
		Tip:    "The project's file as a pull request on the infra repository, filed as you; the factory checked it as its loader will.",
		Detail: "No pull request from branch project-" + name + " into main yet."}
	merged := step{Name: "Approved and merged", State: stepWaiting,
		Tip: "An approver merges the file into infra's main; the factory loads it within 30 seconds."}
	created := step{Name: "Repository created", State: stepWaiting,
		Tip:    "The factory registers the project with the Forgejo site admin's login from OpenBao: organisation, repository from the template, CI user, tokens. No one types a password.",
		Detail: "After the merge."}
	ready := step{Name: "Board ready", State: stepWaiting,
		Tip:    "Webhook, labels, bot fork, deploy branch, namespaces, network policy and the Flux Kustomization are in place.",
		Detail: "After the repository."}

	v, ok, err := c.cfg.Policies.CurrentPolicy(ctx)
	if err != nil || !ok {
		return out, errors.Join(errors.New("no version in force"), err)
	}
	c.mu.Lock()
	run := projectRun{}
	if p := c.runs[name]; p != nil {
		run = *p
	}
	c.mu.Unlock()
	approver := c.approver(r)

	if _, loaded := v.Tree.Projects[name]; loaded {
		pulled.State, pulled.Detail = stepDone, fmt.Sprintf("%s is in the configuration version %d.", policy.ProjectFile(name), v.ID)
		merged.State, merged.Detail = stepDone, fmt.Sprintf("Loaded in version %d (commit %s).", v.ID, v.Commit)
	} else {
		pr, found, err := c.pullOf(ctx, name)
		switch {
		case err != nil:
			log.Printf("console: pull request of project-%s: %v", name, err)
			pulled.State, pulled.Detail = stepFailed, "Cannot read the pull request; the factoryd log says why."
		case !found:
		case pr.Merged:
			pulled.State, pulled.Detail, pulled.Link = stepDone, fmt.Sprintf("#%d, filed by %s", pr.Number, pr.User.Login), pr.HTMLURL
			merged.State, merged.Detail = stepRunning, "Merged; the factory loads it within 30 seconds."
		case pr.State != "open":
			pulled.State, pulled.Detail, pulled.Link = stepFailed, fmt.Sprintf("#%d was closed without merging.", pr.Number), pr.HTMLURL
		default:
			pulled.State, pulled.Detail, pulled.Link = stepDone, fmt.Sprintf("#%d, filed by %s", pr.Number, pr.User.Login), pr.HTMLURL
			merged.Detail = fmt.Sprintf("Waiting for an approver to merge #%d.", pr.Number)
			if run.merge != "" {
				merged.State, merged.Detail = stepFailed, "Forgejo refused the merge: "+run.merge
			}
			out.CanMerge = approver
		}
	}

	if merged.State == stepDone {
		ps, err := c.cfg.Projects.Projects(ctx)
		if err != nil {
			return out, err
		}
		var row factoryd.Project
		if i := slices.IndexFunc(ps, func(p factoryd.Project) bool { return p.Name == name }); i >= 0 {
			row = ps[i]
		}
		hasMain, mainErr := false, error(nil)
		if row.Minted() {
			hasMain, mainErr = c.cfg.MainExists(ctx, row.Repo())
		}
		reconcileErr := ""
		if row.ReadyAt.IsZero() {
			reconcileErr = c.cfg.ReconcileError(name)
		}
		failed := "The reconcile failed: " + reconcileErr + ". It tries again at least hourly."
		switch {
		case mainErr != nil:
			log.Printf("console: main of %s: %v", row.Repository, mainErr)
			created.State, created.Detail = stepFailed, "Cannot read the repository's main; the factoryd log says why."
		case row.Minted() && hasMain:
			created.State, created.Detail = stepDone, row.Repository+" with main."
		case reconcileErr != "":
			created.State, created.Detail = stepFailed, failed
		case row.Minted():
			created.State, created.Detail = stepRunning, "Organisation, repository, CI user and tokens made; waiting for main's first commit."
		default:
			created.State, created.Detail = stepRunning, "The reconcile registers it within a minute of the load."
		}
		switch {
		case !row.ReadyAt.IsZero():
			ready.State, ready.Detail, ready.Link = stepDone,
				"Ready since "+row.ReadyAt.UTC().Format("2006-01-02 15:04")+" UTC: open the board", "/board?project="+url.QueryEscape(name)
		case created.State != stepDone:
		case reconcileErr != "":
			ready.State, ready.Detail = stepFailed, failed
		default:
			ready.State, ready.Detail = stepRunning, "Waiting for a reconcile that finds every object in place; one runs at least hourly."
		}
	}
	out.Steps = []step{pulled, merged, created, ready}
	out.Final = !slices.ContainsFunc(out.Steps, func(s step) bool { return s.State != stepDone })
	return out, nil
}

// projectStatus is the new project's panel: the whole page, or the panel
// alone for htmx's poll.
func (c *Console) projectStatus(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !newProjectName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	c.showPanel(w, r, name)
}

func (c *Console) showPanel(w http.ResponseWriter, r *http.Request, name string) {
	c.showPanelWith(w, r, name, "")
}

// refuse answers a refused panel button: htmx swaps only a 2xx answer, so
// its request gets the panel with the reason; a plain POST gets the code.
func (c *Console) refuse(w http.ResponseWriter, r *http.Request, name, reason string, code int) {
	if r.Header.Get("HX-Request") == "" {
		http.Error(w, reason, code)
		return
	}
	c.showPanelWith(w, r, name, reason)
}

func (c *Console) showPanelWith(w http.ResponseWriter, r *http.Request, name, refusal string) {
	p, err := c.panel(r, name)
	p.Error = refusal
	if err != nil {
		log.Printf("console: status of project %s: %v", name, err)
		http.Error(w, "cannot read the project's state", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		c.render(w, "status-panel", p)
		return
	}
	c.render(w, "project_status.html", p)
}

// acted answers a panel button: the panel for htmx, else the page.
func (c *Console) acted(w http.ResponseWriter, r *http.Request, name string) {
	if r.Header.Get("HX-Request") != "" {
		c.showPanel(w, r, name)
		return
	}
	http.Redirect(w, r, "/projects/"+name+"/status", http.StatusSeeOther)
}

func (c *Console) run(name string) *projectRun {
	p := c.runs[name]
	if p == nil {
		p = &projectRun{}
		c.runs[name] = p
	}
	return p
}

// mergeProject merges the project's pull request on infra as the human; a
// refusal is kept for the panel in Forgejo's words and answered as a
// refusal, so a plain POST gets a non-2xx status instead of a 303 that
// shows nothing. A 405 (Forgejo re-checking mergeability right after
// another merge moved infra's main) is retried a few times first.
func (c *Console) mergeProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !newProjectName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	pr, ok, err := c.pullOf(r.Context(), name)
	if err != nil {
		log.Printf("console: pull request of project-%s: %v", name, err)
		c.refuse(w, r, name, "Cannot read the pull request; see the factoryd log.", http.StatusBadGateway)
		return
	}
	if !ok || pr.State != "open" || pr.Merged {
		c.refuse(w, r, name, "No open pull request from project-"+name+".", http.StatusConflict)
		return
	}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/merge", url.PathEscape(c.cfg.InfraRepo.Owner), url.PathEscape(c.cfg.InfraRepo.Name), pr.Number)
	body := map[string]any{"Do": "merge", "delete_branch_after_merge": true}
	err = c.asUser(r, http.MethodPost, path, body, http.StatusOK, nil)
	for attempts := 0; ; attempts++ {
		if apiStatus(err) != http.StatusMethodNotAllowed || attempts >= mergeRetryTries || r.Context().Err() != nil {
			break
		}
		log.Printf("console: Forgejo refused %s's merge of %s#%d with 405; retry %d/%d", current(r).user, c.cfg.InfraRepo, pr.Number, attempts+1, mergeRetryTries)
		time.Sleep(c.mergeRetryPause())
		err = c.asUser(r, http.MethodPost, path, body, http.StatusOK, nil)
	}
	refusal := ""
	if ae := (*apiError)(nil); errors.As(err, &ae) {
		refusal = ae.reason()
	} else if err != nil {
		if r.Header.Get("HX-Request") == "" || c.session(r) == nil {
			c.forgejoFailed(w, r, "merge the pull request", err)
			return
		}
		log.Printf("console: %s could not merge the pull request: %v", current(r).user, err)
		c.refuse(w, r, name, "Forgejo could not be reached for the merge; see the factoryd log.", http.StatusBadGateway)
		return
	}
	c.mu.Lock()
	c.run(name).merge = refusal
	c.mu.Unlock()
	if refusal != "" {
		log.Printf("console: Forgejo refused %s's merge of %s#%d: %s", current(r).user, c.cfg.InfraRepo, pr.Number, refusal)
		c.refuse(w, r, name, "Forgejo refused the merge: "+refusal, http.StatusConflict)
		return
	}
	log.Printf("console: %s merged %s#%d", current(r).user, c.cfg.InfraRepo, pr.Number)
	c.acted(w, r, name)
}

// mergeRetryTries are the retries of a merge Forgejo refused with 405.
const mergeRetryTries = 3

func (c *Console) mergeRetryPause() time.Duration {
	if c.cfg.MergeRetryPause != nil {
		return *c.cfg.MergeRetryPause
	}
	return time.Second
}

func apiStatus(err error) int {
	if ae := (*apiError)(nil); errors.As(err, &ae) {
		return ae.status
	}
	return 0
}
