package factoryd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// ProjectKube is the slice of the Kubernetes API registration uses.
type ProjectKube interface {
	Apply(ctx context.Context, path string, obj map[string]any) (Change, error)
	GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error)
	Deployments(ctx context.Context, namespace string) ([]DeploymentState, error)
}

// Registrar keeps a managed project's objects (rulings 156, 164). A
// project is its own Forgejo organisation <name>, owned by
// factory-projects, with the repository <name>/<name>, the team ci holding
// the user ci-<name> with package write, and nothing else. What only the
// Forgejo site admin can do (the organisation, the repository, the CI
// user, every token) happens in Register, run by a human; Reconcile keeps
// the rest with factory-projects' token (write:repository, unlimited: a
// repository-limited token never passes Forgejo's admin checks, and this
// user owns project organisations and nothing in sf). In the cluster: each
// namespace with its label, quota, limit range, default-deny network
// policy and the Flux account, the Flux source and one Kustomization per
// environment (ruling 157). Every run logs one line per object: created,
// updated, no change, or what it waits for.
type Registrar struct {
	Forgejo *Forgejo
	Kube    ProjectKube
	Store   ProjectStore
	Policy  func() policy.Document
	// WebhookURL is factoryd's intake as Forgejo reaches it;
	// WebhookSecret signs the deliveries.
	WebhookURL, WebhookSecret string
	// FactoryNamespace is factoryd's own; PullSecret the name of the image
	// pull secret in a project's namespaces.
	FactoryNamespace, PullSecret string
	// GitURL is the Forgejo base Flux clones from; RegistryHost the image
	// registry's host.
	GitURL, RegistryHost string
	// MergeUser is factory-merge; ProjectsUser is factory-projects.
	MergeUser, ProjectsUser string

	mu sync.Mutex
}

// Project objects' fixed names.
const (
	ProjectLabel      = "factory/project"
	FluxAccount       = "flux"
	DeployerRole      = "factory-project-deployer"
	ProjectReaderRole = "factoryd-project"
	projectObject     = "project"
	ciTeam            = "ci"
	deployKeyTitle    = "factory-roll"
)

// ProjectHookEvents are every issue and pull request event.
var ProjectHookEvents = []string{"issues", "issue_assign", "issue_label", "issue_milestone", "issue_comment",
	"pull_request", "pull_request_assign", "pull_request_label", "pull_request_milestone", "pull_request_comment",
	"pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment", "pull_request_sync",
	"pull_request_review_request"}

// A project's Actions secrets: the registry login of its CI user, the
// factory read token, and the deploy key its main workflow pushes its
// roll commits with (the job token pushes nothing to a whitelisted main).
const (
	SecretRegistryUser     = "REGISTRY_USER"
	SecretRegistryPassword = "REGISTRY_PASSWORD"
	SecretFactoryRead      = "FACTORY_READ_TOKEN"
	SecretDeployKey        = "DEPLOY_KEY"
)

// Waiting is an object that needs register-project first.
const Waiting Change = "waiting: register-project creates it with the Forgejo admin's login"

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

// ReconcileAll reconciles every project row: at boot, and when the
// policy's approvers may have changed.
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

func (run *reconcileRun) approvers() []string { return run.r.Policy().Approvers }

func (run *reconcileRun) forgejo(ctx context.Context) {
	f, repo := run.r.Forgejo, run.p.Repo()
	object := "repository " + repo.String()
	exists, admin, err := f.ProjectRepoAccess(ctx, repo)
	if run.fail(object, err) {
		return
	}
	if !exists || !admin {
		run.line(object, Waiting)
		return
	}
	run.line(object, Unchanged)
	steps := []struct {
		object string
		do     func() (Change, error)
	}{
		{"webhook", func() (Change, error) { return run.webhook(ctx) }},
		{"collaborator " + f.BotUser, func() (Change, error) { return run.collaborator(ctx, f.BotUser) }},
		{"collaborator " + run.r.MergeUser, func() (Change, error) { return run.collaborator(ctx, run.r.MergeUser) }},
	}
	for _, a := range run.approvers() {
		steps = append(steps, struct {
			object string
			do     func() (Change, error)
		}{"collaborator " + a, func() (Change, error) { return run.collaborator(ctx, a) }})
	}
	steps = append(steps, []struct {
		object string
		do     func() (Change, error)
	}{
		{"protection main", func() (Change, error) { return run.protection(ctx) }},
		{"fork " + f.Fork(repo).String(), func() (Change, error) { return "ready", f.EnsureFork(ctx, repo) }},
		{"labels", func() (Change, error) { return "ensured", f.EnsureLabels(ctx, repo.Owner, repo.Name, BootLabels) }},
	}...)
	for _, step := range steps {
		c, err := step.do()
		if run.fail(step.object, err) {
			return
		}
		run.line(step.object, c)
	}
	names, err := f.ActionSecretNames(ctx, repo)
	if run.fail("actions secrets", err) {
		return
	}
	if c, err := run.deployKey(ctx, names); !run.fail("deploy key", err) {
		run.line("deploy key", c)
	}
	if c, err := run.readToken(ctx, names); !run.fail("actions secret "+SecretFactoryRead, err) {
		run.line("actions secret "+SecretFactoryRead, c)
	}
	for _, name := range []string{SecretRegistryUser, SecretRegistryPassword} {
		c := Unchanged
		if !slices.Contains(names, name) {
			c = Waiting
		}
		run.line("actions secret "+name, c)
	}
}

// webhook keeps the hook to factoryd; the row keeps the sha256 of the
// secret last set, so a re-seeded intake secret reaches the hook.
func (run *reconcileRun) webhook(ctx context.Context) (Change, error) {
	repo := run.p.Repo()
	hooks, err := run.r.Forgejo.ProjectHooks(ctx, repo)
	if err != nil {
		return "", err
	}
	secretHash := HashToken(run.r.WebhookSecret)
	i := slices.IndexFunc(hooks, func(h Hook) bool { return h.Config.URL == run.r.WebhookURL })
	c, id := Created, int64(0)
	if i >= 0 {
		h := hooks[i]
		if h.Active && h.Config.ContentType == "json" && run.p.HookSecretHash == secretHash &&
			slices.Equal(slices.Sorted(slices.Values(h.Events)), slices.Sorted(slices.Values(ProjectHookEvents))) {
			return Unchanged, nil
		}
		c, id = Updated, h.ID
	}
	if err := run.r.Forgejo.SaveProjectHook(ctx, repo, id, run.r.WebhookURL, run.r.WebhookSecret, ProjectHookEvents); err != nil {
		return "", err
	}
	return c, run.r.Store.SetProjectTokens(ctx, run.p.Name, ProjectTokens{HookSecretHash: secretHash})
}

// collaborator keeps user a collaborator with write or more. Membership
// is read first: the permission a site admin reads as is admin whether or
// not it is a collaborator, and Forgejo drops a non-collaborator from the
// whitelists.
func (run *reconcileRun) collaborator(ctx context.Context, user string) (Change, error) {
	f, repo := run.r.Forgejo, run.p.Repo()
	is, err := f.IsCollaborator(ctx, repo, user)
	if err != nil {
		return "", err
	}
	if !is {
		return Created, f.AddProjectCollaborator(ctx, repo, user, "write")
	}
	perm, err := f.CollaboratorPermission(ctx, repo, user)
	switch {
	case err != nil:
		return "", err
	case perm == "write" || perm == "admin" || perm == "owner":
		return Unchanged, nil
	}
	return Updated, f.AddProjectCollaborator(ctx, repo, user, "write")
}

// Protection is main's protection for a project: the code repository's
// (infra README, "The merge bot"), and the project's deploy key may push
// (its main workflow's roll commits; Forgejo admits no Actions job token
// to a whitelist). A project on auto needs no approval, so factory-merge
// can merge a clean pull request (ruling 149). Forgejo keeps only users
// with write in a whitelist, so the approvers are write collaborators.
func Protection(mergeRule string, approvers []string, mergeUser string) BranchProtection {
	required := 1
	if mergeRule == policy.MergeAuto {
		required = 0
	}
	return BranchProtection{
		EnablePush: true, EnablePushWhitelist: true, PushWhitelistUsernames: slices.Clone(approvers), PushWhitelistDeployKeys: true,
		EnableMergeWhitelist: true, MergeWhitelistUsernames: append(slices.Clone(approvers), mergeUser),
		EnableApprovalsWhitelist: true, ApprovalsWhitelistUsernames: slices.Clone(approvers),
		RequiredApprovals: required, ApplyToAdmins: true, BlockOnRejectedReviews: true, DismissStaleApprovals: true,
		ProtectedFilePatterns: ProtectedWorkflows,
	}
}

// ProtectedWorkflows are the paths no push to a project's main may touch,
// the deploy key's included (review-task65 n1: every non-fork job holds
// DEPLOY_KEY); Forgejo refuses a merge touching them for everyone too, so
// a human changes a project's workflows by lifting the protection.
const ProtectedWorkflows = ".forgejo/**;.gitea/**;.github/**"

func (run *reconcileRun) protection(ctx context.Context) (Change, error) {
	want := Protection(run.p.MergeRule, run.approvers(), run.r.MergeUser)
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

// deployKey keeps one write deploy key, its private half only in the
// Actions secret DEPLOY_KEY; a key or secret missing makes both anew.
func (run *reconcileRun) deployKey(ctx context.Context, secrets []string) (Change, error) {
	f, repo := run.r.Forgejo, run.p.Repo()
	keys, err := f.DeployKeys(ctx, repo)
	if err != nil {
		return "", err
	}
	var ours []DeployKey
	for _, k := range keys {
		if k.Title == deployKeyTitle {
			ours = append(ours, k)
		}
	}
	if len(ours) == 1 && !ours[0].ReadOnly && slices.Contains(secrets, SecretDeployKey) {
		return Unchanged, nil
	}
	for _, k := range ours {
		if err := f.DeleteDeployKey(ctx, repo, k.ID); err != nil {
			return "", err
		}
	}
	public, private, err := NewDeployKey(run.p.Name + " roll")
	if err != nil {
		return "", err
	}
	if err := f.PutActionSecret(ctx, repo, SecretDeployKey, private); err != nil {
		return "", err
	}
	if err := f.AddDeployKey(ctx, repo, deployKeyTitle, public); err != nil {
		return "", err
	}
	if len(ours) > 0 || slices.Contains(secrets, SecretDeployKey) {
		return Updated, nil
	}
	return Created, nil
}

// NewDeployKey is an ed25519 key pair: the authorized_keys line and the
// OpenSSH private key.
func NewDeployKey(comment string) (public, private string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), string(pem.EncodeToMemory(block)), nil
}

// readToken keeps FACTORY_READ_TOKEN and its hash on the row. A secret or
// hash missing makes a new token; only the reconcile that stores its hash
// sets the secret, so two racing never leave them apart.
func (run *reconcileRun) readToken(ctx context.Context, secrets []string) (Change, error) {
	has := slices.Contains(secrets, SecretFactoryRead)
	if has && run.p.ReadTokenHash != "" {
		return Unchanged, nil
	}
	store := run.r.Store
	if run.p.ReadTokenHash != "" {
		if err := store.ForgetReadToken(ctx, run.p.Name, run.p.ReadTokenHash); err != nil {
			return "", err
		}
	}
	plain, hash := NewReadToken()
	claimed, err := store.ClaimReadToken(ctx, run.p.Name, hash)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "set by another reconcile", nil
	}
	f := run.r.Forgejo
	if err := f.PutActionSecret(ctx, run.p.Repo(), SecretFactoryRead, plain); err != nil {
		return "", errors.Join(err, store.ForgetReadToken(ctx, run.p.Name, hash))
	}
	run.p.ReadTokenHash = hash
	if has {
		return Updated, nil
	}
	return Created, nil
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
		run.apply(ctx, ns+" networkpolicy", "/apis/cilium.io/v2/namespaces/"+ns+"/ciliumnetworkpolicies/"+projectObject, map[string]any{
			"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
			"metadata": map[string]any{"name": projectObject, "namespace": ns, "labels": labels},
			"spec":     ProjectNetworkPolicy(ns),
		})
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
		if p.PullToken == "" {
			run.line(ns+" secret "+run.r.PullSecret, Waiting)
		} else {
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
	if p.GitToken == "" {
		run.line("flux source", Waiting)
		return
	}
	gitSecret := p.Name + "-git"
	if !run.apply(ctx, "flux secret "+gitSecret, "/api/v1/namespaces/flux-system/secrets/"+gitSecret, map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": gitSecret, "namespace": "flux-system", "labels": labels},
		"data": map[string]string{"username": base64.StdEncoding.EncodeToString([]byte(run.r.ProjectsUser)),
			"password": base64.StdEncoding.EncodeToString([]byte(p.GitToken))},
	}) {
		return
	}
	if !run.apply(ctx, "flux gitrepository", "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/"+p.Name, map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
		"metadata": map[string]any{"name": p.Name, "namespace": "flux-system", "labels": labels},
		"spec": map[string]any{"interval": "1m", "url": strings.TrimSuffix(run.r.GitURL, "/") + "/" + p.Repository,
			"ref": map[string]any{"branch": "main"}, "secretRef": map[string]any{"name": gitSecret}},
	}) {
		return
	}
	for _, e := range p.Environments {
		run.apply(ctx, e.Namespace+" flux kustomization", "/apis/kustomize.toolkit.fluxcd.io/v1/namespaces/"+e.Namespace+"/kustomizations/"+p.Name,
			Kustomization(p.Name, e, labels))
	}
}

// ProjectNetworkPolicy is a project namespace's default deny (the deployer
// role writes no network policy, so the project cannot widen it): in, from
// its own namespace, the edge gateway's Envoy and the CNPG operator; out,
// to its own namespace, DNS, the API server (CNPG's instance manager), the
// SigNoz collector and anything on 443.
func ProjectNetworkPolicy(ns string) map[string]any {
	own := []any{map[string]any{"matchLabels": map[string]string{"k8s:io.kubernetes.pod.namespace": ns}}}
	ports := func(proto string, ps ...string) []any {
		var out []any
		for _, p := range ps {
			out = append(out, map[string]any{"port": p, "protocol": proto})
		}
		return []any{map[string]any{"ports": out}}
	}
	return map[string]any{
		"endpointSelector": map[string]any{},
		"ingress": []any{
			map[string]any{"fromEndpoints": own},
			map[string]any{"fromEntities": []any{"ingress"}},
			map[string]any{"fromEndpoints": []any{map[string]any{"matchLabels": map[string]string{"k8s:io.kubernetes.pod.namespace": "cnpg-system"}}},
				"toPorts": ports("TCP", "8000", "5432")},
		},
		"egress": []any{
			map[string]any{"toEndpoints": own},
			map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]string{
				"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}},
				"toPorts": ports("ANY", "53")},
			map[string]any{"toEntities": []any{"kube-apiserver"}},
			map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]string{
				"k8s:io.kubernetes.pod.namespace": "platform", "app.kubernetes.io/component": "otel-collector"}}},
				"toPorts": ports("TCP", "4318")},
			map[string]any{"toEntities": []any{"world"}, "toPorts": ports("TCP", "443")},
		},
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
	user := run.p.CIUser()
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + run.p.PullToken))
	return mustJSON(map[string]any{"auths": map[string]any{run.r.RegistryHost: map[string]string{
		"username": user, "password": run.p.PullToken, "auth": auth}}})
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

// Register is registration by a human with the Forgejo admin's login
// (factoryd register-project): it records p, or finds it recorded with the
// same namespaces, environments and secrets (its settings are the
// console's), makes what only the admin can (ruling 164), and reconciles.
// A second run with the same project changes nothing.
func (r *Registrar) Register(ctx context.Context, p Project, a AdminAuth, rotate bool) (Project, error) {
	err := r.Store.CreateProject(ctx, p)
	switch {
	case errors.Is(err, ErrProjectExists):
		have, ok, err := r.Store.Project(ctx, p.Name)
		if err != nil || !ok {
			return p, errors.Join(fmt.Errorf("repository %s or one of the namespaces %v is another project's", p.Repository, p.Namespaces), err)
		}
		if have.Repository != p.Repository || !slices.Equal(have.Namespaces, p.Namespaces) || !have.Managed ||
			!slices.Equal(have.Environments, p.Environments) || !slices.Equal(have.Secrets, p.Secrets) {
			return p, fmt.Errorf("project %s is registered with other namespaces, environments or secrets", p.Name)
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
	if err := r.admin(ctx, a, &p, rotate); err != nil {
		return p, err
	}
	if p, err = r.stored(ctx, p.Name); err != nil {
		return p, err
	}
	return p, r.Reconcile(ctx, p)
}

// admin makes, with the site admin's login, the organisation owned by
// factory-projects, the repository, the CI user in the organisation's ci
// team (package write), and the tokens: ci-<name>'s write:package into the
// registry Actions secrets and its read:package for the pull secret,
// factory-projects' read:repository and factory-merge's write:repository
// limited to the repository. A token p already has is kept unless rotate.
func (r *Registrar) admin(ctx context.Context, a AdminAuth, p *Project, rotate bool) error {
	f, repo, org, ci := r.Forgejo, p.Repo(), p.Name, p.CIUser()
	logged := func(object string, c Change, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", object, err)
		}
		log.Printf("project %s: %s: %s", p.Name, object, c)
		return nil
	}
	c, err := f.AdminEnsureOrg(ctx, a, org, r.ProjectsUser)
	if err := logged("organisation "+org, c, err); err != nil {
		return err
	}
	c, err = f.AdminEnsureRepo(ctx, a, repo)
	if err := logged("repository "+repo.String(), c, err); err != nil {
		return err
	}
	c, err = f.AdminEnsureUser(ctx, a, ci)
	if err := logged("user "+ci, c, err); err != nil {
		return err
	}
	c, err = f.AdminEnsurePackageTeam(ctx, a, org, ciTeam, ci)
	if err := logged("team "+ciTeam, c, err); err != nil {
		return err
	}
	// a repository-limited token needs its user to see the repository
	if p.MergeToken == "" || rotate {
		if err := f.AdminAddCollaborator(ctx, a, repo, r.MergeUser, "write"); err != nil {
			return fmt.Errorf("collaborator %s: %w", r.MergeUser, err)
		}
	}
	var t ProjectTokens
	mint := func(have *string, into *string, user, name string, scopes []string, repos []Repo) error {
		if *have != "" && !rotate {
			return nil
		}
		tok, err := f.AdminMintToken(ctx, a, user, name, scopes, repos)
		if err != nil {
			return fmt.Errorf("token %s of %s: %w", name, user, err)
		}
		*into = tok
		log.Printf("project %s: token %s of %s: minted (%v%s)", p.Name, name, user, scopes, map[bool]string{true: " on " + repo.String()}[len(repos) > 0])
		return nil
	}
	if err := errors.Join(
		mint(&p.PullToken, &t.PullToken, ci, "pull", []string{"read:package"}, nil),
		mint(&p.GitToken, &t.GitToken, r.ProjectsUser, "git-"+p.Name, []string{"read:repository"}, []Repo{repo}),
		mint(&p.MergeToken, &t.MergeToken, r.MergeUser, "project-"+p.Name, []string{"write:repository"}, []Repo{repo}),
	); err != nil {
		return err
	}
	names, err := f.ActionSecretNames(ctx, repo)
	if err != nil {
		return fmt.Errorf("actions secrets: %w", err)
	}
	if rotate || !slices.Contains(names, SecretRegistryUser) || !slices.Contains(names, SecretRegistryPassword) {
		tok, err := f.AdminMintToken(ctx, a, ci, "registry", []string{"write:package"}, nil)
		if err != nil {
			return fmt.Errorf("token registry of %s: %w", ci, err)
		}
		if err := errors.Join(f.PutActionSecret(ctx, repo, SecretRegistryUser, ci),
			f.PutActionSecret(ctx, repo, SecretRegistryPassword, tok)); err != nil {
			return fmt.Errorf("registry secrets: %w", err)
		}
		log.Printf("project %s: token registry of %s: minted ([write:package]) into %s and %s", p.Name, ci, SecretRegistryUser, SecretRegistryPassword)
	}
	if t == (ProjectTokens{}) {
		log.Printf("project %s: tokens: no change", p.Name)
		return nil
	}
	return r.Store.SetProjectTokens(ctx, p.Name, t)
}

func (r *Registrar) stored(ctx context.Context, name string) (Project, error) {
	p, ok, err := r.Store.Project(ctx, name)
	if err == nil && !ok {
		err = fmt.Errorf("project %s: %w", name, ErrNotFound)
	}
	return p, err
}
