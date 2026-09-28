package factoryd

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// ProjectKube is the slice of the Kubernetes API registration uses.
type ProjectKube interface {
	Apply(ctx context.Context, path string, obj map[string]any) (Change, error)
	GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error)
	Deployments(ctx context.Context, namespace string) ([]DeploymentState, error)
}

// Registrar keeps a managed project's objects (ruling 156): in Forgejo
// the repository, its webhook to factoryd, factory-agent and
// factory-merge as writers, main's protection, the bot's fork and labels,
// and the Actions secrets; in the cluster each namespace with its label,
// quota, limit range and the Flux account, the GitRepository and one
// Kustomization per environment (ruling 157). Every run logs one line per
// object: created, updated or no change.
type Registrar struct {
	Forgejo *Forgejo
	Kube    ProjectKube
	Store   ProjectStore
	Policy  func() policy.Document
	// WebhookURL is factoryd's intake as Forgejo reaches it;
	// WebhookSecret signs the deliveries.
	WebhookURL, WebhookSecret string
	// FactoryNamespace holds factoryd's account and PullSecret, whose
	// registry login the projects' REGISTRY_USER and REGISTRY_PASSWORD
	// Actions secrets carry.
	FactoryNamespace, PullSecret string
	// GitURL is the Forgejo base Flux clones from; RegistryHost the image
	// registry's host.
	GitURL, RegistryHost string
	// MergeUser and CIUser are factory-merge and factory-ci;
	// ProjectsUser is factory-projects.
	MergeUser, CIUser, ProjectsUser string

	mu sync.Mutex
}

// Project objects' fixed names.
const (
	ProjectLabel      = "factory/project"
	FluxAccount       = "flux"
	DeployerRole      = "factory-project-deployer"
	ProjectReaderRole = "factoryd-project"
	projectObject     = "project"
)

// ProjectHookEvents are every issue and pull request event.
var ProjectHookEvents = []string{"issues", "issue_assign", "issue_label", "issue_milestone", "issue_comment",
	"pull_request", "pull_request_assign", "pull_request_label", "pull_request_milestone", "pull_request_comment",
	"pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment", "pull_request_sync",
	"pull_request_review_request"}

// Registry secrets and the factory read token in a project's Actions.
const (
	SecretRegistryUser     = "REGISTRY_USER"
	SecretRegistryPassword = "REGISTRY_PASSWORD"
	SecretFactoryRead      = "FACTORY_READ_TOKEN"
)

type reconcileRun struct {
	r    *Registrar
	p    Project
	errs []error
}

func (run *reconcileRun) line(object string, c Change) {
	log.Printf("project %s: %s: %s", run.p.Name, object, c)
}

// fail records err for object and reports whether there was one.
func (run *reconcileRun) fail(object string, err error) bool {
	if err == nil {
		return false
	}
	log.Printf("project %s: %s: %v", run.p.Name, object, err)
	run.errs = append(run.errs, fmt.Errorf("%s: %w", object, err))
	return true
}

// ReconcileAll reconciles every project row, the boot pass.
func (r *Registrar) ReconcileAll(ctx context.Context) error {
	ps, err := r.Store.Projects(ctx)
	if err != nil {
		log.Printf("projects: %v", err)
		return err
	}
	var errs []error
	for _, p := range ps {
		errs = append(errs, r.Reconcile(ctx, p))
	}
	return errors.Join(errs...)
}

// Reconcile brings p's objects to its row. An unmanaged row has none of
// factoryd's. A failed Forgejo step skips the Forgejo steps after it; the
// cluster steps run regardless.
func (r *Registrar) Reconcile(ctx context.Context, p Project) error {
	if !p.Managed {
		log.Printf("project %s: unmanaged: its objects are the infra repository's", p.Name)
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run := &reconcileRun{r: r, p: p}
	run.forgejo(ctx)
	run.cluster(ctx)
	return errors.Join(run.errs...)
}

func (run *reconcileRun) forgejo(ctx context.Context) {
	f, repo := run.r.Forgejo, run.p.Repo()
	for _, step := range []struct {
		object string
		do     func() (Change, error)
	}{
		{"repository " + repo.String(), func() (Change, error) { return run.repository(ctx) }},
		{"webhook", func() (Change, error) { return run.webhook(ctx) }},
		{"collaborator " + f.BotUser, func() (Change, error) { return run.collaborator(ctx, f.BotUser) }},
		{"collaborator " + run.r.MergeUser, func() (Change, error) { return run.collaborator(ctx, run.r.MergeUser) }},
		{"protection main", func() (Change, error) { return run.protection(ctx) }},
		{"fork " + f.Fork(repo).String(), func() (Change, error) { return "ready", f.EnsureFork(ctx, repo) }},
		{"labels", func() (Change, error) { return "ensured", f.EnsureLabels(ctx, repo.Owner, repo.Name, BootLabels) }},
	} {
		c, err := step.do()
		if run.fail(step.object, err) {
			return
		}
		run.line(step.object, c)
	}
	run.actionSecrets(ctx)
}

func (run *reconcileRun) repository(ctx context.Context) (Change, error) {
	repo := run.p.Repo()
	exists, admin, err := run.r.Forgejo.ProjectRepoAccess(ctx, repo)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return Created, run.r.Forgejo.CreateProjectRepo(ctx, repo)
	case !admin:
		return "", fmt.Errorf("%s is no admin of it: factoryd register-project makes it one with the Forgejo admin's login", run.r.ProjectsUser)
	}
	return Unchanged, nil
}

func (run *reconcileRun) webhook(ctx context.Context) (Change, error) {
	repo := run.p.Repo()
	hooks, err := run.r.Forgejo.ProjectHooks(ctx, repo)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(hooks, func(h Hook) bool { return h.Config.URL == run.r.WebhookURL })
	if i < 0 {
		return Created, run.r.Forgejo.SaveProjectHook(ctx, repo, 0, run.r.WebhookURL, run.r.WebhookSecret, ProjectHookEvents)
	}
	h := hooks[i]
	if h.Active && h.Config.ContentType == "json" && slices.Equal(slices.Sorted(slices.Values(h.Events)), slices.Sorted(slices.Values(ProjectHookEvents))) {
		return Unchanged, nil
	}
	return Updated, run.r.Forgejo.SaveProjectHook(ctx, repo, h.ID, run.r.WebhookURL, run.r.WebhookSecret, ProjectHookEvents)
}

func (run *reconcileRun) collaborator(ctx context.Context, user string) (Change, error) {
	perm, err := run.r.Forgejo.CollaboratorPermission(ctx, run.p.Repo(), user)
	switch {
	case err != nil:
		return "", err
	case perm == "write":
		return Unchanged, nil
	case perm == "" || perm == "none":
		return Created, run.r.Forgejo.AddProjectCollaborator(ctx, run.p.Repo(), user, "write")
	}
	return Updated, run.r.Forgejo.AddProjectCollaborator(ctx, run.p.Repo(), user, "write")
}

// Protection is main's protection for a project: the code repository's
// (infra README, "The merge bot") with two changes Forgejo forces. The
// push whitelist is off, because Forgejo admits no Actions job token to
// one and the project's own main workflow commits its rolls; agents never
// push upstream anyway (their tokens reach only the bot's fork). And a
// project on auto needs no approval, so factory-merge can merge a clean
// pull request (ruling 149).
func Protection(mergeRule string, approvers []string, mergeUser string) BranchProtection {
	required := 1
	if mergeRule == policy.MergeAuto {
		required = 0
	}
	return BranchProtection{
		EnablePush:           true,
		EnableMergeWhitelist: true, MergeWhitelistUsernames: append(slices.Clone(approvers), mergeUser),
		EnableApprovalsWhitelist: true, ApprovalsWhitelistUsernames: slices.Clone(approvers),
		RequiredApprovals: required, ApplyToAdmins: true, BlockOnRejectedReviews: true, DismissStaleApprovals: true,
		PushWhitelistUsernames: []string{},
	}
}

func (run *reconcileRun) protection(ctx context.Context) (Change, error) {
	want := Protection(run.p.MergeRule, run.r.Policy().Approvers, run.r.MergeUser)
	have, exists, err := run.r.Forgejo.ProjectBranchProtection(ctx, run.p.Repo(), "main")
	switch {
	case err != nil:
		return "", err
	case !exists:
		return Created, run.r.Forgejo.SaveProjectBranchProtection(ctx, run.p.Repo(), "main", want, false)
	case have.equal(want):
		return Unchanged, nil
	}
	return Updated, run.r.Forgejo.SaveProjectBranchProtection(ctx, run.p.Repo(), "main", want, true)
}

// actionSecrets sets the registry login and a FACTORY_READ_TOKEN whose
// hash the row keeps, with the project's CI token. A secret's value
// cannot be read back, so one that exists is left alone; the read token is
// minted again when the row has no hash.
func (run *reconcileRun) actionSecrets(ctx context.Context) {
	const object = "actions secret "
	if run.p.CIToken == "" {
		log.Printf("project %s: actions secrets: waiting: no CI token yet (factoryd register-project mints it with the Forgejo admin's login)", run.p.Name)
		return
	}
	f, repo, token := run.r.Forgejo, run.p.Repo(), run.p.CIToken
	names, err := f.ActionSecretNames(ctx, token, repo)
	if run.fail("actions secrets", err) {
		return
	}
	var user, password string
	for _, name := range []string{SecretRegistryUser, SecretRegistryPassword} {
		if slices.Contains(names, name) {
			run.line(object+name, Unchanged)
			continue
		}
		if user == "" {
			if user, password, err = run.registryLogin(ctx); run.fail(object+name, err) {
				return
			}
		}
		value := user
		if name == SecretRegistryPassword {
			value = password
		}
		if run.fail(object+name, f.PutActionSecret(ctx, token, repo, name, value)) {
			return
		}
		run.line(object+name, Created)
	}
	if slices.Contains(names, SecretFactoryRead) && run.p.ReadTokenHash != "" {
		run.line(object+SecretFactoryRead, Unchanged)
		return
	}
	plain, hash := NewReadToken()
	if run.fail(object+SecretFactoryRead, f.PutActionSecret(ctx, token, repo, SecretFactoryRead, plain)) {
		return
	}
	if run.fail(object+SecretFactoryRead, run.r.Store.SetProjectTokens(ctx, run.p.Name, ProjectTokens{ReadTokenHash: hash})) {
		return
	}
	run.p.ReadTokenHash = hash
	if slices.Contains(names, SecretFactoryRead) {
		run.line(object+SecretFactoryRead, Updated)
		return
	}
	run.line(object+SecretFactoryRead, Created)
}

// registryLogin is the registry user and password of the factory's pull
// secret, the credential the code repository's main CI pushes with.
func (run *reconcileRun) registryLogin(ctx context.Context) (string, string, error) {
	data, err := run.r.Kube.GetSecretData(ctx, run.r.FactoryNamespace, run.r.PullSecret)
	if err != nil {
		return "", "", err
	}
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(data[".dockerconfigjson"]), &cfg); err != nil {
		return "", "", fmt.Errorf("pull secret %s: %w", run.r.PullSecret, err)
	}
	a, ok := cfg.Auths[run.r.RegistryHost]
	if !ok {
		return "", "", fmt.Errorf("pull secret %s has no login for %s", run.r.PullSecret, run.r.RegistryHost)
	}
	if a.Username == "" && a.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(a.Auth)
		if err != nil {
			return "", "", fmt.Errorf("pull secret %s: %w", run.r.PullSecret, err)
		}
		a.Username, a.Password, _ = strings.Cut(string(raw), ":")
	}
	if a.Username == "" || a.Password == "" {
		return "", "", fmt.Errorf("pull secret %s: an empty login for %s", run.r.PullSecret, run.r.RegistryHost)
	}
	return a.Username, a.Password, nil
}

func (run *reconcileRun) apply(ctx context.Context, object, path string, obj map[string]any) bool {
	c, err := run.r.Kube.Apply(ctx, path, obj)
	if run.fail(object, err) {
		return false
	}
	run.line(object, c)
	return true
}

func (run *reconcileRun) cluster(ctx context.Context) {
	p := run.p
	labels := map[string]string{ProjectLabel: p.Name}
	for _, ns := range p.Namespaces {
		core := "/api/v1/namespaces/" + ns
		rbac := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + ns + "/rolebindings/"
		if !run.apply(ctx, "namespace "+ns, core, map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": ns, "labels": map[string]string{
				ProjectLabel: p.Name, "pod-security.kubernetes.io/enforce": "restricted"}},
		}) {
			continue
		}
		// factoryd's own rights in the namespace come first: the rest uses them
		if !run.apply(ctx, ns+" rolebinding factoryd", rbac+"factoryd", roleBinding(ns, "factoryd", ProjectReaderRole, labels,
			map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": run.r.FactoryNamespace})) {
			continue
		}
		run.apply(ctx, ns+" resourcequota", core+"/resourcequotas/"+projectObject, map[string]any{
			"apiVersion": "v1", "kind": "ResourceQuota",
			"metadata": map[string]any{"name": projectObject, "namespace": ns, "labels": labels},
			"spec":     map[string]any{"hard": ResourceQuotaHard(p.Quota)},
		})
		run.apply(ctx, ns+" limitrange", core+"/limitranges/"+projectObject, map[string]any{
			"apiVersion": "v1", "kind": "LimitRange",
			"metadata": map[string]any{"name": projectObject, "namespace": ns, "labels": labels},
			"spec": map[string]any{"limits": []any{map[string]any{"type": "Container",
				"default": projectLimits, "defaultRequest": projectRequests}}},
		})
		run.apply(ctx, ns+" serviceaccount "+FluxAccount, core+"/serviceaccounts/"+FluxAccount, map[string]any{
			"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": FluxAccount, "namespace": ns, "labels": labels},
		})
		run.apply(ctx, ns+" rolebinding "+FluxAccount, rbac+FluxAccount, roleBinding(ns, FluxAccount, DeployerRole, labels,
			map[string]any{"kind": "ServiceAccount", "name": FluxAccount, "namespace": ns}))
		if p.PullToken != "" {
			run.apply(ctx, ns+" secret "+run.r.PullSecret, core+"/secrets/"+run.r.PullSecret, map[string]any{
				"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
				"metadata": map[string]any{"name": run.r.PullSecret, "namespace": ns, "labels": labels},
				"data":     map[string]string{".dockerconfigjson": base64.StdEncoding.EncodeToString(run.pullConfig())},
			})
		}
		for _, entry := range p.Secrets {
			run.generatedSecret(ctx, ns, entry, labels)
		}
	}
	if len(p.Environments) == 0 {
		return
	}
	if !run.apply(ctx, "flux gitrepository", "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/"+p.Name, map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
		"metadata": map[string]any{"name": p.Name, "namespace": "flux-system", "labels": labels},
		"spec": map[string]any{"interval": "1m", "url": strings.TrimSuffix(run.r.GitURL, "/") + "/" + p.Repository,
			"ref": map[string]any{"branch": "main"}, "secretRef": map[string]any{"name": "forgejo-pat"}},
	}) {
		return
	}
	for _, e := range p.Environments {
		run.apply(ctx, e.Namespace+" flux kustomization", "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+e.Namespace+"/kustomizations/"+p.Name,
			Kustomization(p.Name, e, labels))
	}
}

// Kustomization applies e's path into its namespace as the namespace's
// flux account (ruling 157). It lives in that namespace because Flux takes
// the account from the Kustomization's own namespace; the deployer role
// covers no Flux kind, so the project cannot change it.
func Kustomization(project string, e Environment, labels map[string]string) map[string]any {
	return map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": project, "namespace": e.Namespace, "labels": labels},
		"spec": map[string]any{"interval": "1m", "path": "./" + e.Path, "prune": true,
			"sourceRef":          map[string]any{"kind": "GitRepository", "name": project, "namespace": "flux-system"},
			"serviceAccountName": FluxAccount, "targetNamespace": e.Namespace},
	}
}

func roleBinding(ns, name, clusterRole string, labels map[string]string, subject map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": clusterRole},
		"subjects": []any{subject},
	}
}

// A project container's defaults when it sets none; the quota bounds the
// sum.
var (
	projectRequests = map[string]any{"cpu": "50m", "memory": "64Mi"}
	projectLimits   = map[string]any{"cpu": "500m", "memory": "512Mi"}
)

func (run *reconcileRun) pullConfig() []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(run.r.CIUser + ":" + run.p.PullToken))
	return mustJSON(map[string]any{"auths": map[string]any{run.r.RegistryHost: map[string]string{
		"username": run.r.CIUser, "password": run.p.PullToken, "auth": auth}}})
}

// generatedSecret gives ns the secret/key of entry with a random value
// when it lacks it; an existing value is never replaced.
func (run *reconcileRun) generatedSecret(ctx context.Context, ns, entry string, labels map[string]string) {
	name, key, _ := strings.Cut(entry, "/")
	object := ns + " secret " + entry
	data, err := run.r.Kube.GetSecretData(ctx, ns, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		run.fail(object, err)
		return
	}
	if _, ok := data[key]; ok {
		run.line(object, Unchanged)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	run.apply(ctx, object, "/api/v1/namespaces/"+ns+"/secrets/"+name, map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": labels},
		"data":     map[string]string{key: base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(b)))},
	})
}

// ErrRepoMissing: a repository-limited token needs its repository;
// reconcile creates it first.
var ErrRepoMissing = errors.New("the repository does not exist yet")

// Mint gives p the tokens only the Forgejo site admin can make, at
// registration: factory-projects as an admin collaborator of a repository
// that exists already, factory-ci's token limited to the repository
// (Actions secrets and runners) and its read:package token (the image
// pull secret). A token p already has is kept unless rotate.
func (r *Registrar) Mint(ctx context.Context, a AdminAuth, p *Project, rotate bool) error {
	repo := p.Repo()
	exists, err := r.Forgejo.AdminRepoExists(ctx, a, repo)
	if err != nil {
		return err
	}
	if exists {
		perm, err := r.Forgejo.AdminCollaboratorPermission(ctx, a, repo, r.ProjectsUser)
		if err != nil {
			return err
		}
		c := Unchanged
		if perm != "admin" {
			c = Created
			if err := r.Forgejo.AdminAddCollaborator(ctx, a, repo, r.ProjectsUser, "admin"); err != nil {
				return fmt.Errorf("%s as admin of %s: %w", r.ProjectsUser, repo, err)
			}
		}
		log.Printf("project %s: collaborator %s (admin): %s", p.Name, r.ProjectsUser, c)
	}
	var t ProjectTokens
	if p.CIToken == "" || rotate {
		if !exists {
			return fmt.Errorf("%s: %w", repo, ErrRepoMissing)
		}
		if t.CIToken, err = r.Forgejo.AdminMintToken(ctx, a, r.CIUser, "project-"+p.Name, []string{"write:repository"}, []Repo{repo}); err != nil {
			return fmt.Errorf("ci token: %w", err)
		}
		log.Printf("project %s: ci token: minted (%s, write:repository on %s)", p.Name, r.CIUser, repo)
	}
	if p.PullToken == "" || rotate {
		if t.PullToken, err = r.Forgejo.AdminMintToken(ctx, a, r.CIUser, "project-"+p.Name+"-pull", []string{"read:package"}, nil); err != nil {
			return fmt.Errorf("pull token: %w", err)
		}
		log.Printf("project %s: pull token: minted (%s, read:package)", p.Name, r.CIUser)
	}
	if t == (ProjectTokens{}) {
		log.Printf("project %s: tokens: no change", p.Name)
		return nil
	}
	if err := r.Store.SetProjectTokens(ctx, p.Name, t); err != nil {
		return err
	}
	p.CIToken, p.PullToken = cmp.Or(t.CIToken, p.CIToken), cmp.Or(t.PullToken, p.PullToken)
	return nil
}

// Register is registration by a human with the Forgejo admin's login
// (factoryd register-project): it records p, or finds it recorded with the
// same repository, namespaces, environments and secrets (its settings are
// the console's), mints what only the admin can, and reconciles. A second
// run with the same project changes nothing.
func (r *Registrar) Register(ctx context.Context, p Project, admin AdminAuth, rotate bool) (Project, error) {
	err := r.Store.CreateProject(ctx, p)
	switch {
	case errors.Is(err, ErrProjectExists):
		have, ok, err := r.Store.Project(ctx, p.Name)
		if err != nil || !ok {
			return p, errors.Join(fmt.Errorf("repository %s is registered under another name", p.Repository), err)
		}
		if have.Repository != p.Repository || !slices.Equal(have.Namespaces, p.Namespaces) || !have.Managed ||
			!slices.Equal(have.Environments, p.Environments) || !slices.Equal(have.Secrets, p.Secrets) {
			return p, fmt.Errorf("project %s is registered with another repository, namespaces, environments or secrets", p.Name)
		}
		log.Printf("project %s: row: no change (its settings are the console's)", p.Name)
	case err != nil:
		return p, err
	default:
		log.Printf("project %s: row: created", p.Name)
	}
	if p, err = r.stored(ctx, p.Name); err != nil {
		return p, err
	}
	err = r.Mint(ctx, admin, &p, rotate)
	if errors.Is(err, ErrRepoMissing) {
		if err := r.Reconcile(ctx, p); err != nil {
			return p, err
		}
		err = r.Mint(ctx, admin, &p, rotate)
	}
	if err != nil {
		return p, err
	}
	if p, err = r.stored(ctx, p.Name); err != nil {
		return p, err
	}
	return p, r.Reconcile(ctx, p)
}

func (r *Registrar) stored(ctx context.Context, name string) (Project, error) {
	p, ok, err := r.Store.Project(ctx, name)
	if err == nil && !ok {
		err = fmt.Errorf("project %s: %w", name, ErrNotFound)
	}
	return p, err
}
