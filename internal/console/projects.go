package console

import (
	"cmp"
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

	"golang.org/x/sync/errgroup"

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
	// Mirror is a factory project's GitHub export, nil for none.
	Mirror *factoryd.Mirror
	// Approver: the viewer approves the project, so sets its export.
	Approver bool
	// Envs are a factory project's environments with their deploy state.
	Envs []envRow
}

// envRow is an environment with its deploy state (deployState).
type envRow struct {
	factoryd.Environment
	State string
}

// Words is the environment as the projects page and its tool say it.
func (e envRow) Words() string {
	s := e.Namespace + ": " + e.Path
	if e.Hostname != "" {
		s += " at " + e.Hostname
	}
	if e.State != "" {
		s += " (" + e.State + ")"
	}
	return s
}

// fluxReads bounds the Kustomization reads that run at once.
const fluxReads = 8

// envRows are the environments of each factory project of ps with their
// deploy states, by ps's index; the Kustomizations are read at once, at
// most fluxReads at a time, so a slow read does not add up across them.
func (c *Console) envRows(ctx context.Context, ps []factoryd.Project) [][]envRow {
	rows := make([][]envRow, len(ps))
	var g errgroup.Group
	g.SetLimit(fluxReads)
	for i, p := range ps {
		if p.Objects != policy.ObjectsFactory {
			continue
		}
		rows[i] = make([]envRow, len(p.Environments))
		for j, e := range p.Environments {
			rows[i][j].Environment = e
			g.Go(func() error {
				rows[i][j].State = c.deployState(ctx, p, e)
				return nil
			})
		}
	}
	_ = g.Wait() // deployState says a failed read in its words
	return rows
}

// fluxPathNotFound starts kustomize-controller's Ready message when the
// source's revision lacks the Kustomization's path.
const fluxPathNotFound = "kustomization path not found"

// deployState is environment e of project p as its Flux Kustomization
// tells it (roadmap 257): ready, or not ready with Flux's message, and
// not deployed yet while the deploy branch lacks e's path, which only a
// green build of main writes. Registration makes the Kustomization once
// the project is minted. "" without a reader.
func (c *Console) deployState(ctx context.Context, p factoryd.Project, e factoryd.Environment) string {
	if c.cfg.FluxReady == nil {
		return ""
	}
	if !p.Minted() {
		return "no Flux Kustomization yet"
	}
	ready, msg, err := c.cfg.FluxReady(ctx, e.Namespace, p.Name)
	switch {
	case errors.Is(err, factoryd.ErrNotFound):
		return "no Flux Kustomization yet"
	case err != nil:
		log.Printf("console: the Kustomization %s/%s: %v", e.Namespace, p.Name, err)
		return "deploy state unreadable"
	case ready:
		return "ready"
	case strings.HasPrefix(msg, fluxPathNotFound):
		return "not deployed yet: no green build has written " + e.Path
	case msg == "":
		return "not ready: Flux has not reconciled it yet"
	}
	return "not ready: " + msg
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
	envs := c.envRows(r.Context(), ps)
	for i, p := range ps {
		row := projectRow{Project: p, NotifyList: p.NotifyOrApprovers(v.ApproversOf(p.Repository)), NotifyDefault: len(p.Notify) == 0,
			GatewayYAML: factoryd.ListenerYAML(p)}
		if p.File {
			row.File = c.fileLink(v, p.Name)
			if row.Envelope, err = c.envelopeOf(r.Context(), v, p.Repository); err != nil {
				log.Printf("console: envelope use of %s: %v", p.Repository, err)
				http.Error(w, "cannot read the envelopes' use", http.StatusInternalServerError)
				return
			}
		}
		row.Approver = slices.Contains(v.ApproversOf(p.Repository), current(r).user)
		row.Envs = envs[i]
		if c.cfg.Mirrors != nil && p.Objects == policy.ObjectsFactory {
			m, ok, err := c.cfg.Mirrors.Mirror(r.Context(), p.Name, factoryd.MirrorExport)
			if err != nil {
				log.Printf("console: the export of %s: %v", p.Name, err)
				http.Error(w, "cannot read the projects' exports", http.StatusInternalServerError)
				return
			}
			if ok {
				row.Mirror = &m
			}
		}
		rows = append(rows, row)
	}
	s := current(r)
	c.render(w, "projects.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Projects": rows,
		"New": c.cfg.ConfigRepo + "/src/branch/main/" + c.cfg.ConfigDir + "/projects", "Export": c.cfg.Mirrors != nil,
		"Sync": c.cfg.GitHubSync != nil})
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
	// Source is the GitHub repository, owner/name, registration copies.
	Name, Process, Template, MergeRule, Source string
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
	c.render(w, "project_new.html", map[string]any{"User": s.user, "CSRF": s.csrf, "Factory": true, "Form": p, "Refused": refused,
		"Processes": slices.Sorted(maps.Keys(v.Tree.Processes)), "Templates": slices.Sorted(maps.Keys(v.Tree.Templates)),
		"Approver": c.approver(r), "Import": c.cfg.Mirrors != nil})
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
// project-<name> off main with the one file, its pull request, and the
// project_filed event that records the pull request. A GitHub repository
// to start from is the name's import row, with its token, put once the
// branch exists, before the pull request, which it records once open.
// A pull request whose opening errs may have opened all the same: the
// open one from the branch is the filing's. The filing starts on a token
// that outlasts it (filingTime) and only while infra has no branch
// project-<name>; any write of it that fails, the file's commit included,
// undoes it (failFiling).
func (c *Console) createProject(w http.ResponseWriter, r *http.Request) {
	p := newProject{Name: r.PostFormValue("name"), Process: r.PostFormValue("process"),
		Template: r.PostFormValue("template"), MergeRule: r.PostFormValue("merge_rule"), Source: strings.TrimSpace(r.PostFormValue("source"))}
	token := r.PostFormValue("token")
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
	if p.Source != "" && (c.cfg.Mirrors == nil || !githubRepo.MatchString(p.Source)) {
		refused = append(refused, "GitHub repository: owner/name of a repository on GitHub, or none")
	}
	switch {
	case token != "" && p.Source == "":
		refused = append(refused, "GitHub token: only with a GitHub repository")
	case token != "" && !githubToken.MatchString(token):
		refused = append(refused, "GitHub token: 8 to 255 letters, digits, _ or -")
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
	switch _, err := c.tokenFor(r.Context(), current(r), filingTime); {
	case errors.Is(err, errLoggedOut):
		c.end(current(r))
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case err != nil:
		log.Printf("console: the Forgejo session of %s: %v", user, err)
		http.Error(w, "cannot refresh your Forgejo session; see the factoryd log", http.StatusServiceUnavailable)
		return
	}
	infra := c.infraAPI()
	branch := "project-" + p.Name
	switch _, exists, err := c.branchHead(r.Context(), branch); {
	case err != nil:
		log.Printf("console: branch %s of %s: %v", branch, c.cfg.InfraRepo, err)
		http.Error(w, "cannot read infra's branches; see the factoryd log", http.StatusBadGateway)
		return
	case exists:
		c.renderNewProject(w, r, http.StatusUnprocessableEntity, p,
			[]string{"name: infra has branch " + branch + " from an earlier filing; delete it to file the name again"})
		return
	}
	var committed struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.asUser(r, http.MethodPost, infra+"/contents", map[string]any{
		"branch": "main", "new_branch": branch,
		"message": button(r).attribution().Message("Project " + p.Name + ": add its file"),
		"files": []map[string]string{{"operation": "create", "path": c.cfg.ConfigDir + "/" + policy.ProjectFile(p.Name),
			"content": base64.StdEncoding.EncodeToString(file)}},
	}, http.StatusCreated, &committed); err != nil {
		log.Printf("console: %s could not commit projects/%s.yaml: %v", user, p.Name, err)
		c.failFiling(w, r, p, undo{none: true}, http.StatusBadGateway, "Forgejo refused to commit the project's file")
		return
	}
	made := committed.Commit.SHA
	if c.cfg.Mirrors != nil {
		var err error
		if p.Source == "" {
			err = c.cfg.Mirrors.DeleteMirror(r.Context(), p.Name, factoryd.MirrorImport)
		} else {
			err = c.cfg.Mirrors.PutMirror(r.Context(), factoryd.Mirror{Project: p.Name, Direction: factoryd.MirrorImport, Remote: p.Source,
				Token: token, State: "pending"})
		}
		if err != nil {
			log.Printf("console: the GitHub import of %s: %v", p.Name, err)
			c.failFiling(w, r, p, undo{commit: made, none: true}, http.StatusInternalServerError, "cannot store the GitHub import")
			return
		}
	}
	var pull infraPull
	if err := c.asUser(r, http.MethodPost, infra+"/pulls", map[string]string{"head": branch, "base": "main", "title": "Project " + p.Name},
		http.StatusCreated, &pull); err != nil {
		log.Printf("console: %s could not open the pull request from %s: %v", user, branch, err)
		open, found, rerr := c.openPullFrom(r.Context(), branch)
		if rerr != nil || !found {
			if rerr != nil {
				log.Printf("console: the open pull requests of %s: %v", c.cfg.InfraRepo, rerr)
			}
			c.failFiling(w, r, p, undo{commit: made, row: p.Source != "", none: rerr == nil}, http.StatusBadGateway, "Forgejo refused to open the pull request")
			return
		}
		log.Printf("console: %s#%d from %s is open all the same: the filing keeps it", c.cfg.InfraRepo, open.Number, branch)
		pull = open
	}
	if p.Source != "" {
		if err := c.cfg.Mirrors.SetImportPR(r.Context(), p.Name, pull.Number); err != nil {
			log.Printf("console: the GitHub import of %s: record %s#%d: %v", p.Name, c.cfg.InfraRepo, pull.Number, err)
			c.failFiling(w, r, p, undo{commit: made, row: true, pr: pull.Number}, http.StatusInternalServerError,
				fmt.Sprintf("cannot record %s#%d on the GitHub import", c.cfg.InfraRepo, pull.Number))
			return
		}
	}
	detail := map[string]any{"name": p.Name, "pr": pull.Number}
	if p.Source != "" {
		detail["source"] = p.Source
	}
	if _, err := c.cfg.Events.Record(r.Context(), factoryd.Event{Repository: p.Name + "/" + p.Name, Kind: factoryd.EventProjectFiled,
		Actor: user, Via: factoryd.ViaButton, Detail: detail}); err != nil {
		log.Printf("console: event %s of %s: %v", factoryd.EventProjectFiled, p.Name, err)
		c.failFiling(w, r, p, undo{commit: made, row: p.Source != "", pr: pull.Number}, http.StatusInternalServerError,
			fmt.Sprintf("cannot record the filing of %s#%d", c.cfg.InfraRepo, pull.Number))
		return
	}
	log.Printf("console: %s filed projects/%s.yaml as %s#%d", user, p.Name, c.cfg.InfraRepo, pull.Number)
	http.Redirect(w, r, "/projects/"+p.Name+"/status", http.StatusSeeOther)
}

// filingTime bounds a filing's writes as the human, its undo's included:
// the filing starts on a token that outlasts it, so the session cannot
// end between two of them.
const filingTime = 5 * time.Minute

// undo is how far a failed filing got: commit, the commit it made on its
// branch, "" when its commit errs; row, whether it put its import row;
// pr, its pull request, 0 for none known; none, that no pull request of
// it can exist.
type undo struct {
	commit string
	row    bool
	pr     int64
	none   bool
}

// failFiling undoes p's filing, failed for why, and answers code: it
// closes the filing's pull request and deletes its branch while the
// branch's head is the filing's commit, so the name can be filed again;
// a branch with another head, a racing filing's or one whose commit
// errs, is left. The import row the filing put goes with
// its token once no pull request of the filing can merge: none opened, it
// closed, or its branch is gone. Otherwise the row stays, so a merge still
// imports, and the answer names what is left.
func (c *Console) failFiling(w http.ResponseWriter, r *http.Request, p newProject, u undo, code int, why string) {
	user, branch := current(r).user, "project-"+p.Name
	closed := false
	if u.pr > 0 {
		err := c.asUser(r, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", c.infraAPI(), u.pr), map[string]string{"state": "closed"},
			http.StatusCreated, nil)
		if err != nil {
			log.Printf("console: %s could not close %s#%d: %v", user, c.cfg.InfraRepo, u.pr, err)
		}
		closed = err == nil
	}
	head, exists, err := c.branchHead(r.Context(), branch)
	mine := exists && u.commit != "" && head == u.commit
	if mine {
		err = c.asUser(r, http.MethodDelete, c.infraAPI()+"/branches/"+url.PathEscape(branch), nil, http.StatusNoContent, nil)
	}
	if err != nil {
		log.Printf("console: %s could not delete branch %s: %v", user, branch, err)
	}
	deleted := err == nil && (!exists || mine)
	gone := u.none || closed || deleted
	if gone && u.row {
		if err := c.cfg.Mirrors.DeleteMirror(r.Context(), p.Name, factoryd.MirrorImport); err != nil {
			log.Printf("console: the GitHub import of %s: delete: %v", p.Name, err)
		}
	}
	msg := why
	switch {
	case gone:
	case u.pr > 0:
		msg += fmt.Sprintf("; %s#%d may stay open: close it", c.cfg.InfraRepo, u.pr)
	default:
		msg += "; a pull request from " + branch + " may be open: close it"
	}
	switch {
	case deleted:
		msg += "; the filing was undone"
	case err == nil:
		msg += "; branch " + branch + " is left: its head is not this filing's commit"
	default:
		msg += "; delete branch " + branch + " to file the name again"
	}
	http.Error(w, msg+"; see the factoryd log", code)
}

// branchHead reads infra's branch, with factoryd's config token: its head
// commit, and whether it exists.
func (c *Console) branchHead(ctx context.Context, branch string) (string, bool, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	err := c.api(ctx, c.cfg.InfraToken, http.MethodGet, c.infraAPI()+"/branches/"+url.PathEscape(branch), nil, http.StatusOK, &b)
	if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status == http.StatusNotFound {
		return "", false, nil
	}
	return b.Commit.ID, err == nil, err
}

// infraAPI is the infra repository's API path.
func (c *Console) infraAPI() string {
	return "/api/v1/repos/" + url.PathEscape(c.cfg.InfraRepo.Owner) + "/" + url.PathEscape(c.cfg.InfraRepo.Name)
}

type infraPull struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// openPullFrom reads infra's open pull requests, newest first as Forgejo
// lists them, for the one from branch into main, with factoryd's config
// token; ok false when there is none.
func (c *Console) openPullFrom(ctx context.Context, branch string) (pr infraPull, ok bool, err error) {
	var open []infraPull
	if err := c.api(ctx, c.cfg.InfraToken, http.MethodGet, c.infraAPI()+"/pulls?state=open&limit=50", nil, http.StatusOK, &open); err != nil {
		return pr, false, err
	}
	i := slices.IndexFunc(open, func(o infraPull) bool {
		return o.Head.Ref == branch && o.Head.Repo.FullName == c.cfg.InfraRepo.String() && o.Base.Ref == "main"
	})
	if i < 0 {
		return pr, false, nil
	}
	return open[i], true, nil
}

// pullOf reads, with factoryd's config token, the infra pull request of
// name's last filing: the number its project_filed event records, never
// Forgejo's answer for branch project-<name>, which may be an earlier
// filing's. ok false when there is none.
func (c *Console) pullOf(ctx context.Context, name string) (pr infraPull, ok bool, err error) {
	number, filed, err := c.cfg.Events.FiledPR(ctx, name)
	if err != nil || !filed {
		return pr, false, err
	}
	path := fmt.Sprintf("%s/pulls/%d", c.infraAPI(), number)
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
	Factory          bool // the layout's factory-level links: the panel is a factory-level page
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
	out := statusPanel{Name: name, User: s.user, CSRF: s.csrf, Factory: true}
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
	if c.cfg.Mirrors != nil {
		m, ok, err := c.cfg.Mirrors.Mirror(ctx, name, factoryd.MirrorImport)
		if err != nil {
			return out, err
		}
		if ok {
			imported := step{Name: "Imported from GitHub", State: stepWaiting, Detail: "After the merge.",
				Tip: "Registration copies the GitHub repository into the new one with the token given at filing, then forgets the token; the template adds only its .forgejo/ files and AGENTS.md where the copy lacks them."}
			from := "github.com/" + m.Remote
			switch {
			case strings.HasPrefix(m.State, "imported"):
				imported.State, imported.Detail = stepDone, from+", "+m.State+"."
			case strings.HasPrefix(m.State, "failed"):
				imported.State, imported.Detail = stepFailed, "The copy of "+from+" "+m.State+". The reconcile tries again at least hourly."
			case strings.HasPrefix(m.State, "copying"):
				imported.State, imported.Detail = stepRunning, from+", "+m.State+"."
			case merged.State == stepDone:
				imported.State, imported.Detail = stepRunning, "Registration copies "+from+" into the repository within a minute of the load."
			}
			out.Steps = slices.Insert(out.Steps, 3, imported)
		}
	}
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
	err = c.mergeInfra(r.Context(), current(r), pr.Number)
	refusal := ""
	if ae := (*apiError)(nil); errors.As(err, &ae) {
		refusal = ae.reason()
	} else if err != nil {
		s, sErr := c.session(r)
		if sErr != nil {
			log.Printf("console: the session read: %v", sErr)
			http.Error(w, "cannot read the session", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("HX-Request") == "" || s == nil {
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
	c.act(r, c.projectRepository(r, name), "", 0, factoryd.EventProjectMerged, map[string]any{"name": name, "pr": pr.Number})
	c.acted(w, r, name)
}

// mergeInfra merges pull request number of the infra repository as
// member s, its branch deleted; Forgejo's 405 while it still checks the
// pull request's mergeability is retried.
func (c *Console) mergeInfra(ctx context.Context, s *session, number int64) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/merge", url.PathEscape(c.cfg.InfraRepo.Owner), url.PathEscape(c.cfg.InfraRepo.Name), number)
	body := map[string]any{"Do": "merge", "delete_branch_after_merge": true}
	err := c.as(ctx, s, http.MethodPost, path, body, http.StatusOK, nil)
	for attempts := 0; apiStatus(err) == http.StatusMethodNotAllowed && attempts < mergeRetryTries && ctx.Err() == nil; attempts++ {
		log.Printf("console: Forgejo refused %s's merge of %s#%d with 405; retry %d/%d", s.user, c.cfg.InfraRepo, number, attempts+1, mergeRetryTries)
		time.Sleep(c.mergeRetryPause())
		err = c.as(ctx, s, http.MethodPost, path, body, http.StatusOK, nil)
	}
	return err
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

var (
	githubRepo  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	githubLogin = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	githubToken = regexp.MustCompile(`^[A-Za-z0-9_-]{8,255}$`)
)

// doGithubExport exports project p's repository to GitHub repository
// remote (owner/name) as username with token, in place of a push mirror p
// had to it or to its last export's remote: in mode mirror (the default)
// Forgejo pushes it on every commit and every 8 h, in mode pull_requests
// factoryd takes each merged card there as a pull request. An approver's
// action, as a token leaves the factory.
func (c *Console) doGithubExport(ctx context.Context, a actor, p factoryd.Project, remote, username, token, mode string) (string, error) {
	mode = cmp.Or(mode, factoryd.ModeMirror)
	if err := c.exportable(ctx, a, p); err != nil {
		return "", err
	}
	switch {
	case !githubRepo.MatchString(remote):
		return "", refuse(http.StatusBadRequest, "remote must be a GitHub repository, owner/name")
	case !githubLogin.MatchString(username):
		return "", refuse(http.StatusBadRequest, "username must be a GitHub login")
	case !githubToken.MatchString(token):
		return "", refuse(http.StatusBadRequest, "token must be a GitHub token: 8 to 255 letters, digits, _ or -")
	case mode != factoryd.ModeMirror && mode != factoryd.ModePullRequests:
		return "", refuse(http.StatusBadRequest, "mode must be mirror or pull_requests")
	}
	old, _, err := c.cfg.Mirrors.Mirror(ctx, p.Name, factoryd.MirrorExport)
	if err != nil {
		log.Printf("console: the export of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the project's export")
	}
	repo, address := p.Repo(), factoryd.GitHubAddress(remote)
	if err := c.dropPushMirrors(ctx, repo, address, factoryd.GitHubAddress(old.Remote)); err != nil {
		return "", err
	}
	if mode == factoryd.ModeMirror {
		if err := c.cfg.Mirrors.AddPushMirror(ctx, repo, address, username, token); err != nil {
			return "", &forgejoRefusal{"add the push mirror", errors.New(strings.ReplaceAll(err.Error(), token, tokenMask))}
		}
	}
	if err := c.cfg.Mirrors.PutMirror(ctx, factoryd.Mirror{Project: p.Name, Direction: factoryd.MirrorExport, Remote: remote,
		Username: username, Token: token, State: "created", Mode: mode}); err != nil {
		log.Printf("console: the export of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot store the project's export")
	}
	if err := c.cfg.Mirrors.ClearGitHubFailures(ctx, p.Repository); err != nil {
		log.Printf("console: the GitHub failures of %s: %v", p.Name, err)
	}
	note := "GitHub export of " + p.Name + " to " + remote + " is set: Forgejo pushes every branch and tag on each commit and every 8 h."
	if mode == factoryd.ModePullRequests {
		note = "GitHub export of " + p.Name + " to " + remote + " is set: each card that merges from now on is pushed there as its branch and opened as a pull request."
	} else if err := c.cfg.Mirrors.SyncPushMirrors(ctx, repo); err != nil {
		log.Printf("console: the first push of %s to GitHub: %v", repo, err)
		note += " The first push waits for the next commit."
	}
	c.record(ctx, a, p.Repository, "", 0, factoryd.EventGithubExportSet, map[string]any{"remote": remote})
	return note, nil
}

// exportable refuses a GitHub export of a seed project; the access
// table has judged the approver.
func (c *Console) exportable(ctx context.Context, a actor, p factoryd.Project) error {
	if p.Objects != policy.ObjectsFactory {
		return refuse(http.StatusBadRequest, p.Name+" is the factory's own: infra's push-mirrors CronJob mirrors it (ruling 126)")
	}
	return nil
}

// exportForm is the sentence sending a member to the projects page's
// export form of project, where an approver sets the export's token.
func (c *Console) exportForm(project string) string {
	return "Set the export's token on the project page: " + strings.TrimSuffix(c.cfg.PublicURL, "/") + "/projects#export-" + project
}

// doGithubSync starts bringing project p's GitHub default branch back as
// a pull request to main, which outlasts the request: its result, or why
// it failed, is one github_synced event and the export row's last sync.
// An approver's action, as it reads with the export's token.
func (c *Console) doGithubSync(ctx context.Context, a actor, p factoryd.Project) (string, error) {
	_, ok, err := c.cfg.Mirrors.Mirror(ctx, p.Name, factoryd.MirrorExport)
	switch {
	case err != nil:
		log.Printf("console: the export of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the project's export")
	case !ok:
		return "", refuse(http.StatusNotFound, p.Name+" has no GitHub export")
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		result, err := c.cfg.GitHubSync(ctx, p.Name, a.s.user)
		if err != nil {
			log.Printf("console: the sync of %s from GitHub: %v", p.Name, err)
			result = "failed: " + err.Error()
		}
		c.record(ctx, a, p.Repository, "", 0, factoryd.EventGithubSynced, map[string]any{"result": result})
	}()
	return "Sync from GitHub of " + p.Name + " started: its result arrives as an event and on the projects page's export row.", nil
}

// githubSync answers POST /projects/{name}/export/sync, the export row's
// Sync latest.
func (c *Console) githubSync(w http.ResponseWriter, r *http.Request) {
	c.projectAction(w, r, func(p factoryd.Project) (string, error) { return c.doGithubSync(r.Context(), button(r), p) })
}

// doRemoveGithubExport deletes project p's push mirror to GitHub and its
// row: an approver's action.
func (c *Console) doRemoveGithubExport(ctx context.Context, a actor, p factoryd.Project) (string, error) {
	m, ok, err := c.cfg.Mirrors.Mirror(ctx, p.Name, factoryd.MirrorExport)
	switch {
	case err != nil:
		log.Printf("console: the export of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot read the project's export")
	case !ok:
		return "", refuse(http.StatusNotFound, p.Name+" has no GitHub export")
	}
	if err := c.dropPushMirrors(ctx, p.Repo(), factoryd.GitHubAddress(m.Remote)); err != nil {
		return "", err
	}
	if err := c.cfg.Mirrors.DeleteMirror(ctx, p.Name, factoryd.MirrorExport); err != nil {
		log.Printf("console: the export of %s: %v", p.Name, err)
		return "", refuse(http.StatusInternalServerError, "cannot delete the project's export")
	}
	c.record(ctx, a, p.Repository, "", 0, factoryd.EventGithubExportRemoved, map[string]any{"remote": m.Remote})
	return "GitHub export of " + p.Name + " to " + m.Remote + " is removed.", nil
}

// dropPushMirrors deletes repo's push mirrors to any of addresses.
func (c *Console) dropPushMirrors(ctx context.Context, repo factoryd.Repo, addresses ...string) error {
	ms, err := c.cfg.Mirrors.PushMirrors(ctx, repo)
	if err != nil {
		return &forgejoRefusal{"list the push mirrors", err}
	}
	for _, m := range ms {
		if slices.ContainsFunc(addresses, func(a string) bool { return strings.EqualFold(a, m.RemoteAddress) }) {
			if err := c.cfg.Mirrors.DeletePushMirror(ctx, repo, m.RemoteName); err != nil {
				return &forgejoRefusal{"delete push mirror " + m.RemoteName, err}
			}
		}
	}
	return nil
}

// githubExport answers POST /projects/{name}/export, the projects page's
// export form.
func (c *Console) githubExport(w http.ResponseWriter, r *http.Request) {
	c.projectAction(w, r, func(p factoryd.Project) (string, error) {
		return c.doGithubExport(r.Context(), button(r), p, r.PostFormValue("remote"), r.PostFormValue("username"), r.PostFormValue("token"),
			r.PostFormValue("mode"))
	})
}

// removeGithubExport answers POST /projects/{name}/export/remove.
func (c *Console) removeGithubExport(w http.ResponseWriter, r *http.Request) {
	c.projectAction(w, r, func(p factoryd.Project) (string, error) { return c.doRemoveGithubExport(r.Context(), button(r), p) })
}

// projectAction runs do on the project the path names, which the member
// writes, and sends them back to the projects page.
func (c *Console) projectAction(w http.ResponseWriter, r *http.Request, do func(factoryd.Project) (string, error)) {
	p, ok := c.pathProject(w, r)
	if !ok {
		return
	}
	if _, err := do(p); err != nil {
		c.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects#project-"+p.Name, http.StatusSeeOther)
}
