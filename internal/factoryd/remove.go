package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Roadmap 272: remove-project, register's inverse. The removal of
// 2026-10-15 went by hand in a fixed order; this runs the same order, each
// step idempotent, so a second run after a partial failure finishes the
// rest. It runs as a Kubernetes Job of its own (k8s/remove-project.yaml),
// with its own ServiceAccount whose Role holds exactly the deletes here
// and whose ServiceAccount the admission policy that refuses DELETE on a
// project's objects admits (k8s/factoryd.yaml's policy excepts it).

// Deleted and Gone are a removal step's results: Deleted the object went
// this run, Gone it was already missing (a former run's, or a hand's).
const (
	Deleted Change = "deleted"
	Gone    Change = "already gone"
)

// RemovalStore is what remove-project reads and deletes of the store: the
// project's row (the configuration's files joined in, so a file still in
// the version in force is visible), its cards (the open ones refuse the
// removal, the rest name their LiteLLM keys), and its rows (the project's
// and its mirrors'; the tasks, events, ledger, audit and chat rows stay
// as history).
type RemovalStore interface {
	Project(ctx context.Context, name string) (Project, bool, error)
	ProjectTasks(ctx context.Context, repository string) ([]Task, error)
	DeleteProject(ctx context.Context, name string) error
	// DeleteProjectMirrors removes project's mirror rows and says how
	// many (0: none were left).
	DeleteProjectMirrors(ctx context.Context, project string) (int64, error)
}

// RemovalKube is the slice of the Kubernetes API removal uses: the
// project's namespaces by their factory/project label (the file that held
// them is gone before the removal may run, so the cluster is what names
// them) and one delete, which answers whether the object was there (an
// object already gone is no error).
type RemovalKube interface {
	NamespacesByLabel(ctx context.Context, key, value string) ([]string, error)
	Delete(ctx context.Context, path string) (bool, error)
}

// LiteLLMKeyDeleter deletes tasks' virtual keys from LiteLLM
// (LiteLLMAdmin.DeleteTaskKeys).
type LiteLLMKeyDeleter interface {
	DeleteTaskKeys(ctx context.Context, taskIDs []string) (int, error)
}

// Remover is `factoryd remove-project <name>` (roadmap 272): register's
// inverse. What only the Forgejo site admin can undo (the packages, the
// repository, the forks, the organisation, the CI user, the tokens)
// happens with the admin's login, read from OpenBao for this run only
// (ruling 212), as register does.
type Remover struct {
	Forgejo *Forgejo
	Kube    RemovalKube
	Store   RemovalStore
	// OpenBao holds the Forgejo site admin's login at AdminSecret.
	OpenBao SecretReader
	// LiteLLM deletes the project's tasks' keys; nil deletes none.
	LiteLLM LiteLLMKeyDeleter
	// ProjectsUser is factory-projects, the owner of every project
	// organisation.
	ProjectsUser string
}

// removalRun is one remove-project run: its steps' lines, in order, and
// each failed step's error.
type removalRun struct {
	rem   *Remover
	name  string
	lines []string
	errs  []error
}

func (run *removalRun) line(object string, c Change) {
	run.lines = append(run.lines, fmt.Sprintf("project %s: %s: %s", run.name, object, c))
	log.Printf("project %s: %s: %s", run.name, object, c)
}

// fail records err for object, prefixing it as register's runs do.
func (run *removalRun) fail(object string, err error) bool {
	if err == nil {
		return false
	}
	run.lines = append(run.lines, fmt.Sprintf("project %s: %s: %v", run.name, object, err))
	log.Printf("project %s: %s: %v", run.name, object, err)
	run.errs = append(run.errs, fmt.Errorf("%s: %w", object, err))
	return true
}

// Remove is `factoryd remove-project <name>`: register's inverse, the
// removal's fixed order (roadmap 272). dry lists the steps and their
// targets without acting. The lines name each step and its result; the
// error joins every failed step's. A failed step stops the run: the
// steps before it stand, a second run finishes the rest.
func (rem *Remover) Remove(ctx context.Context, name string, dry bool) ([]string, error) {
	run := &removalRun{rem: rem, name: name}
	repo, err := run.refusals(ctx)
	if err != nil {
		return run.lines, err
	}
	for _, step := range []func(){
		func() { run.exportOff(ctx, repo, dry) },
		func() { run.cluster(ctx, dry) },
	} {
		step()
		if len(run.errs) > 0 {
			return run.lines, errors.Join(run.errs...)
		}
	}
	var a AdminAuth
	if !dry {
		if a, err = rem.adminAuth(ctx); err != nil {
			run.fail("the Forgejo admin's login", err)
			return run.lines, errors.Join(run.errs...)
		}
	}
	for _, step := range []func(){
		func() { run.forgejo(ctx, a, repo, dry) },
		func() { run.litellmKeys(ctx, repo, dry) },
		func() { run.rows(ctx, dry) },
	} {
		step()
		if len(run.errs) > 0 {
			return run.lines, errors.Join(run.errs...)
		}
	}
	return run.lines, errors.Join(run.errs...)
}

// refusals are step 1: the removal refuses while the project's file is
// still in the configuration in force (the hourly reconcile would
// register the project again; the factory's own two projects, whose
// files are the configuration's repository itself, never pass this —
// they are projects like any other, found through their files, ruling
// 179) and while a card of the project is open. Each refusal names the
// state and what to do. It answers the project's repository: the row's,
// or the factory ruling's <name>/<name> for a row already gone.
func (run *removalRun) refusals(ctx context.Context) (Repo, error) {
	rem, name := run.rem, run.name
	p, ok, err := rem.Store.Project(ctx, name)
	if err != nil {
		return Repo{}, err
	}
	if ok && p.File {
		return Repo{}, fmt.Errorf("projects/%s.yaml is in the configuration in force: remove the file from the configuration repository and let factoryd load the new version first, else the hourly reconcile registers the project again", name)
	}
	repo := Repo{Owner: name, Name: name}
	if ok && p.Repository != "" {
		repo = p.Repo()
	}
	tasks, err := rem.Store.ProjectTasks(ctx, repo.String())
	if err != nil {
		return Repo{}, err
	}
	var open []string
	for _, t := range tasks {
		if t.Status != "done" && t.Status != "closed" && t.Status != "failed" {
			open = append(open, t.ID)
		}
	}
	if len(open) > 0 {
		return Repo{}, fmt.Errorf("%d card%s of %s still open (%s): finish or close them first, the removal keeps the tasks as history but an open card's session would fail mid-work",
			len(open), map[bool]string{true: "", false: "s"}[len(open) == 1], repo, strings.Join(open, ", "))
	}
	return repo, nil
}

// exportOff is step 2: the project's GitHub export (Forgejo's push mirror
// of its repository) off, if one exists.
func (run *removalRun) exportOff(ctx context.Context, repo Repo, dry bool) {
	rem := run.rem
	pms, err := rem.Forgejo.PushMirrors(ctx, repo)
	switch {
	case errors.Is(err, ErrNotFound): // the repository is already gone
		run.line("github export", Gone)
		return
	case run.fail("github export", err):
		return
	}
	if len(pms) == 0 {
		run.line("github export", Unchanged+": no push mirror")
		return
	}
	for _, pm := range pms {
		object := "github export " + pm.RemoteAddress
		if dry {
			run.line(object, Change("would delete mirror "+pm.RemoteName))
			continue
		}
		if err := rem.Forgejo.DeletePushMirror(ctx, repo, pm.RemoteName); run.fail(object, err) {
			return
		}
		run.line(object, Deleted)
	}
}

// cluster is step 3: the environments' Flux Kustomizations before their
// namespaces, so Flux's finalizer prunes what they applied, then the
// namespaces (each holding the labels, quota, limit range, network
// policy, Flux account, pull secret and generated secrets registration
// made), the GitRepository and its secret.
func (run *removalRun) cluster(ctx context.Context, dry bool) {
	rem, name := run.rem, run.name
	nss, err := rem.Kube.NamespacesByLabel(ctx, ProjectLabel, name)
	if run.fail("namespaces", err) {
		return
	}
	slices.Sort(nss)
	del := func(object, path string) {
		if dry {
			run.line(object, Change("would delete "+path))
			return
		}
		found, err := rem.Kube.Delete(ctx, path)
		if run.fail(object, err) {
			return
		}
		if found {
			run.line(object, Deleted)
			return
		}
		run.line(object, Gone)
	}
	for _, ns := range nss {
		del(ns+" flux kustomization", "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+ns+"/kustomizations/"+name)
	}
	for _, ns := range nss {
		del("namespace "+ns, "/api/v1/namespaces/"+ns)
	}
	del("flux gitrepository", "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/"+name)
	del("flux secret "+name+"-git", "/api/v1/namespaces/flux-system/secrets/"+name+"-git")
}

// forgejo is step 4, with the site admin's login: the packages, the
// repository, both forks, the organisation, the CI user, and the
// project's tokens on factory-projects, factory-merge and factory-gate.
// dry names each target without a call.
func (run *removalRun) forgejo(ctx context.Context, a AdminAuth, repo Repo, dry bool) {
	rem, name := run.rem, run.name
	f := rem.Forgejo
	ci := "ci-" + name
	forks := []Repo{{Owner: f.BotUser, Name: name}, {Owner: rem.ProjectsUser, Name: name}}
	tokens := []struct{ user, token string }{{rem.ProjectsUser, "git-" + name}, {MergeUser, "project-" + name}, {GateUser, "project-" + name}}
	if dry {
		run.line("packages", Change("would delete "+name+"'s packages"))
		run.line("repository "+repo.String(), Change("would delete"))
		for _, fork := range forks {
			run.line("fork "+fork.String(), Change("would delete"))
		}
		run.line("organisation "+name, Change("would delete"))
		run.line("user "+ci, Change("would delete"))
		for _, t := range tokens {
			run.line("tokens of "+t.user, Change("would delete "+t.token))
		}
		return
	}
	step := func(object string, c Change, err error) bool {
		if run.fail(object, err) {
			return false
		}
		run.line(object, c)
		return true
	}
	if n, err := f.AdminDeletePackages(ctx, a, name); !step("packages", count(n), err) {
		return
	}
	if c, err := f.AdminDeleteRepo(ctx, a, repo); !step("repository "+repo.String(), c, err) {
		return
	}
	for _, fork := range forks {
		if c, err := f.AdminDeleteRepo(ctx, a, fork); !step("fork "+fork.String(), c, err) {
			return
		}
	}
	if c, err := f.AdminDeleteOrg(ctx, a, name); !step("organisation "+name, c, err) {
		return
	}
	if c, err := f.AdminDeleteUser(ctx, a, ci); !step("user "+ci, c, err) {
		return
	}
	for _, t := range tokens {
		if n, err := f.AdminDeleteTokens(ctx, a, t.user, t.token); !step("tokens of "+t.user, count(n), err) {
			return
		}
	}
}

// litellmKeys is step 5: the LiteLLM keys of the project's tasks, closed
// or open alike (the open ones cannot be: step 1 refused), so no spend
// can happen after the removal.
func (run *removalRun) litellmKeys(ctx context.Context, repo Repo, dry bool) {
	rem := run.rem
	if rem.LiteLLM == nil {
		return
	}
	tasks, err := rem.Store.ProjectTasks(ctx, repo.String())
	if run.fail("litellm keys", err) {
		return
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	if dry {
		run.line("litellm keys", Change(fmt.Sprintf("would delete the keys of %d task%s", len(ids), map[bool]string{true: "", false: "s"}[len(ids) == 1])))
		return
	}
	if len(ids) == 0 {
		run.line("litellm keys", Unchanged+": no card ever ran")
		return
	}
	n, err := rem.LiteLLM.DeleteTaskKeys(ctx, ids)
	if run.fail("litellm keys", err) {
		return
	}
	run.line("litellm keys", count(n))
}

// rows is step 6: the projects row (with its tokens) and the project's
// project_mirrors rows. The tasks, events, ledger, audit and chat rows
// stay as history.
func (run *removalRun) rows(ctx context.Context, dry bool) {
	rem, name := run.rem, run.name
	_, ok, err := rem.Store.Project(ctx, name)
	if run.fail("row", err) {
		return
	}
	if dry {
		if ok {
			run.line("row", Change("would delete"))
		} else {
			run.line("row", Gone)
		}
		run.line("project_mirrors rows", Change("would delete"))
		return
	}
	if ok {
		if err := rem.Store.DeleteProject(ctx, name); run.fail("row", err) {
			return
		}
		run.line("row", Deleted)
	} else {
		run.line("row", Gone)
	}
	n, err := rem.Store.DeleteProjectMirrors(ctx, name)
	if run.fail("project_mirrors rows", err) {
		return
	}
	run.line("project_mirrors rows", count(int(n)))
}

func count(n int) Change {
	if n == 0 {
		return Gone
	}
	return Change(fmt.Sprintf("deleted %d", n))
}

// adminAuth reads the site admin's login from OpenBao, as register's does.
func (rem *Remover) adminAuth(ctx context.Context) (AdminAuth, error) {
	if rem.OpenBao == nil {
		return AdminAuth{}, errors.New("the Forgejo admin's login: no OpenBao client")
	}
	kv, err := rem.OpenBao.KV(ctx, AdminSecret)
	if err != nil {
		return AdminAuth{}, fmt.Errorf("the Forgejo admin's login: %w", err)
	}
	a := AdminAuth{User: kv["username"], Password: kv["password"]}
	if a.User == "" || a.Password == "" {
		return AdminAuth{}, fmt.Errorf("the Forgejo admin's login: %s lacks username or password", AdminSecret)
	}
	return a, nil
}

// AdminDeletePackages deletes every package owner holds (the project's
// container images in its organisation) and says how many.
func (f *Forgejo) AdminDeletePackages(ctx context.Context, a AdminAuth, owner string) (int, error) {
	call := f.adminCall(a)
	pkgs, err := GetPages[struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}](ctx, call, "/api/v1/packages/"+url.PathEscape(owner)+"?")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pkgs {
		path := fmt.Sprintf("/api/v1/packages/%s/%s/%s/%s", url.PathEscape(owner), url.PathEscape(p.Type), url.PathEscape(p.Name), url.PathEscape(p.Version))
		if _, err := call(ctx, http.MethodDelete, path, nil, http.StatusNoContent); err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// adminDeletePath deletes the object at path with the site admin's
// login; one already gone is Gone.
func (f *Forgejo) adminDeletePath(ctx context.Context, a AdminAuth, path string) (Change, error) {
	call := f.adminCall(a)
	_, err := call(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	if errors.Is(err, ErrNotFound) {
		return Gone, nil
	}
	if err != nil {
		return "", err
	}
	return Deleted, nil
}

// AdminDeleteRepo deletes repo as the site admin; one already gone is
// Gone (a removal's second run, or a hand's).
func (f *Forgejo) AdminDeleteRepo(ctx context.Context, a AdminAuth, repo Repo) (Change, error) {
	return f.adminDeletePath(ctx, a, "/api/v1/repos/"+repo.String())
}

// AdminDeleteOrg deletes organisation org as the site admin; one already
// gone is Gone.
func (f *Forgejo) AdminDeleteOrg(ctx context.Context, a AdminAuth, org string) (Change, error) {
	return f.adminDeletePath(ctx, a, "/api/v1/orgs/"+url.PathEscape(org))
}

// AdminDeleteUser deletes user as the site admin (its tokens go with
// it); one already gone is Gone.
func (f *Forgejo) AdminDeleteUser(ctx context.Context, a AdminAuth, user string) (Change, error) {
	return f.adminDeletePath(ctx, a, "/api/v1/admin/users/"+url.PathEscape(user))
}

// AdminDeleteTokens revokes user's tokens named name, as the site admin
// acting as user, and says how many; register minted git-<name> on
// factory-projects and project-<name> on factory-merge.
func (f *Forgejo) AdminDeleteTokens(ctx context.Context, a AdminAuth, user, name string) (int, error) {
	call := f.adminCall(a)
	tokens, err := GetPages[TokenRef](ctx, call, "/api/v1/users/"+url.PathEscape(user)+"/tokens?sudo="+url.QueryEscape(user)+"&")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tokens {
		if t.Name != name {
			continue
		}
		if _, err := call(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/users/%s/tokens/%d?sudo=%s",
			url.PathEscape(user), t.ID, url.QueryEscape(user)), nil, http.StatusNoContent); err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}
