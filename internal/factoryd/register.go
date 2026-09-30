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
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// ProjectKube is the slice of the Kubernetes API registration uses.
type ProjectKube interface {
	Apply(ctx context.Context, path string, obj map[string]any) (Change, error)
	GetSecretData(ctx context.Context, namespace, name string) (map[string]string, error)
	Deployments(ctx context.Context, namespace string) ([]DeploymentState, error)
	NamespaceLabel(ctx context.Context, name, key string) (string, error)
}

// Registrar keeps every project's objects (rulings 156, 164, 179). A
// seed project's are the infra repository's and the seed's: the Registrar
// keeps only its labels and the bot's fork, as the bot, and reports its
// webhook and its namespaces' factory/project label. A factory project is
// its own Forgejo organisation <name>, owned by
// factory-projects, with the repository <name>/<name>, the team ci holding
// the user ci-<name> with package write, and nothing else. What only the
// Forgejo site admin can do (the organisation, the repository, the CI
// user, every token) happens in the reconcile of a project whose row has
// no tokens yet, with the admin's login read from OpenBao for that run
// only (ruling 212); Reconcile keeps the rest with factory-projects' token (write:repository, unlimited: a
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
	// HostKey scans Forgejo's SSH host key from inside the cluster (ruling
	// 165); nil: no FACTORY_SSH_HOST_KEY.
	HostKey func(ctx context.Context) (string, error)
	// OpenBao holds the Forgejo site admin's login at AdminSecret.
	OpenBao SecretReader

	mu sync.Mutex
	// regErrs is each project's last failed registration, by name.
	regErrs sync.Map
}

// SecretReader reads a kv-v2 secret's fields (openbao.Client).
type SecretReader interface {
	KV(ctx context.Context, path string) (map[string]string, error)
}

// AdminSecret is the kv-v2 secret with the Forgejo site admin's username
// and password, seeded by infra's seed-secrets.sh.
const AdminSecret = "factory/forgejo-admin" // #nosec G101 -- a secret's path, not a credential value

// Project objects' fixed names.
const (
	ProjectLabel = "factory/project"
	// ObjectsLabel=seed marks a seed project's namespace: the infra
	// repository's, never factoryd's to act in (k8s/factoryd.yaml).
	ObjectsLabel      = "factory/objects"
	FluxAccount       = "flux"
	DeployerRole      = "factory-project-deployer"
	ProjectReaderRole = "factoryd-project"
	projectObject     = "project"
	ciTeam            = "ci"
	deployKeyTitle    = "factory-roll"
)

// ProjectHookEvents are every issue and pull request event, and pushes
// (main's first push makes DeployBranch).
var ProjectHookEvents = []string{"push", "issues", "issue_assign", "issue_label", "issue_milestone", "issue_comment",
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
	// SecretSSHHostKey is Forgejo's SSH host key as a scan finds it, type
	// and key, so no workflow pins a key a rebuild changes (ruling 165);
	// Forgejo refuses secret names starting FORGEJO_, GITEA_ or GITHUB_
	SecretSSHHostKey = "FACTORY_SSH_HOST_KEY" // #nosec G101 -- an Actions secret name, not a credential value
)

// Waiting is an object that needs the project's registration first.
const Waiting Change = "waiting: registration makes it with the Forgejo admin's login from OpenBao"

type reconcileRun struct {
	r    *Registrar
	p    Project
	errs []error
	// unclean: a line waited, failed or reported anything but in place
	unclean bool
}

func (run *reconcileRun) line(object string, c Change) {
	run.unclean = run.unclean || !cleanLine(c)
	log.Printf("project %s: %s: %s", run.p.Name, object, c)
}

// cleanLine reports whether c leaves its object in place: created,
// updated, no change, ensured or ready (stage11-design.md section 5,
// "Board ready").
func cleanLine(c Change) bool {
	verb, _, _ := strings.Cut(string(c), " ")
	return c == Unchanged || slices.Contains([]string{string(Created), string(Updated), "ensured", "ready"}, verb)
}

// fail records err for object and reports whether there was one.
func (run *reconcileRun) fail(object string, err error) bool {
	if err == nil {
		return false
	}
	log.Printf("project %s: %s: %v", run.p.Name, object, err)
	run.errs = append(run.errs, fmt.Errorf("%s: %w", object, err))
	run.unclean = true
	return true
}

// ReconcileAll gives every project file its row, then reconciles every
// project: at boot, and at every new configuration version.
func (r *Registrar) ReconcileAll(ctx context.Context) error {
	ps, err := r.Store.Projects(ctx)
	if err != nil {
		log.Printf("projects: %v", err)
		return err
	}
	var names []string
	for _, p := range ps {
		if p.File {
			names = append(names, p.Name)
		}
	}
	created, err := r.Store.EnsureProjectRows(ctx, names)
	if err != nil {
		log.Printf("projects: rows: %v", err)
		return err
	}
	for _, name := range created {
		log.Printf("project %s: row: created from projects/%s.yaml", name, name)
	}
	var errs []error
	for _, p := range ps {
		errs = append(errs, r.Reconcile(ctx, p))
	}
	return errors.Join(errs...)
}

// ReconcileLoop reconciles every project at its start, at each new policy
// version (ruling 156) and every interval besides, so a hand edit in
// Forgejo or the cluster is set back within that interval. It polls the
// version every poll until ctx ends.
func (r *Registrar) ReconcileLoop(ctx context.Context, version func() int64, poll, every time.Duration) {
	seen, last := int64(0), time.Time{}
	for {
		if v := version(); v != seen || time.Since(last) >= every {
			seen, last = v, time.Now()
			r.ReconcileAll(ctx) // #nosec G104 -- each object's failure is logged by the pass
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// MainPushed reconciles the factory project whose repository this is when
// its main was pushed and DeployBranch does not exist yet: a new project's
// first push (ruling 166). Any other repository or push is nothing to do.
func (r *Registrar) MainPushed(ctx context.Context, repository string) error {
	repo, err := SplitRepo(strings.ToLower(repository))
	if err != nil {
		return err
	}
	p, ok, err := r.Store.Project(ctx, repo.Owner)
	if err != nil || !ok || !p.File || p.Objects != policy.ObjectsFactory || p.Repository != repo.String() {
		return err
	}
	exists, err := r.Forgejo.ProjectBranchExists(ctx, repo, DeployBranch)
	if err != nil || exists {
		return err
	}
	log.Printf("project %s: main pushed, no %s branch yet: reconcile", p.Name, DeployBranch)
	return r.Reconcile(ctx, p)
}

// Reconcile brings p's objects to its file. A row without a file is left
// alone. A seed project's objects are read and reported (seed); a factory
// project's are kept, registered first while its row lacks a token: a
// failed Forgejo step skips the Forgejo steps after it, the cluster steps
// run regardless.
func (r *Registrar) Reconcile(ctx context.Context, p Project) error {
	return r.reconcile(ctx, p, false)
}

func (r *Registrar) reconcile(ctx context.Context, p Project, rotate bool) error {
	if !p.File {
		log.Printf("project %s: no file projects/%s.yaml in the configuration: nothing reconciled, its row keeps its tokens", p.Name, p.Name)
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run := &reconcileRun{r: r, p: p}
	if p.Objects == policy.ObjectsSeed {
		run.seed(ctx)
		return errors.Join(run.errs...)
	}
	if rotate || p.PullToken == "" || p.GitToken == "" || p.MergeToken == "" {
		run.register(ctx, rotate)
	}
	run.forgejo(ctx)
	run.cluster(ctx)
	if !run.unclean {
		run.fail("ready_at", r.Store.SetProjectReady(ctx, p.Name))
	}
	return errors.Join(run.errs...)
}

// seed keeps what factoryd keeps on a seed project with its own tokens,
// the labels and (agents coder) the bot's fork, and reports what the seed
// owns: the webhook to factoryd, read with the project's read token, and
// each namespace's factory/project label. It creates nothing that needs
// factory-projects and nothing in the cluster.
func (run *reconcileRun) seed(ctx context.Context) {
	f, repo := run.r.Forgejo, run.p.Repo()
	if c, err := f.EnsureProjectLabels(ctx, repo.Owner, repo.Name, run.p.Lanes); !run.fail("labels", err) {
		run.line("labels", c)
	}
	fork := "fork " + f.Fork(repo).String()
	if run.p.Agents != policy.AgentsCoder {
		run.line(fork, "none: agents none")
	} else if !run.fail(fork, f.EnsureFork(ctx, repo)) {
		run.line(fork, "ready")
	}
	if c, err := run.seedWebhook(ctx); !run.fail("webhook", err) {
		run.line("webhook", c)
	}
	for _, ns := range run.p.Namespaces {
		object := "namespace " + ns + " labels " + ProjectLabel + ", " + ObjectsLabel
		label, err := run.r.Kube.NamespaceLabel(ctx, ns, ProjectLabel)
		var objects string
		if err == nil {
			objects, err = run.r.Kube.NamespaceLabel(ctx, ns, ObjectsLabel)
		}
		switch {
		case errors.Is(err, ErrNotFound):
			run.line(object, Change("missing: no namespace "+ns+"; the infra repository makes it"))
		case run.fail(object, err):
		case label == run.p.Name && objects == policy.ObjectsSeed:
			run.line(object, "present")
		case label == "" && objects == "":
			run.line(object, "missing: the infra repository sets them")
		default:
			run.line(object, Change(fmt.Sprintf("wrong: %q, %q, want %q, %q; the infra repository sets them", label, objects, run.p.Name, policy.ObjectsSeed)))
		}
	}
}

// seedWebhook reports whether the repository's webhook reaches factoryd,
// never making one: the seed does. A read token whose user does not
// administer the repository is refused the hooks (403): not checkable. A
// token that no longer works (401) is an error.
func (run *reconcileRun) seedWebhook(ctx context.Context) (Change, error) {
	token := run.r.Forgejo.Access(run.p).Read
	if token == "" {
		return "", fmt.Errorf("the file's tokens.read %q is no secret factoryd holds", run.p.Tokens.Read)
	}
	hooks, err := run.r.Forgejo.Hooks(ctx, run.p.Repo(), token)
	if errors.Is(err, errForgejoForbidden) {
		return Change("not checkable, no admin credential on " + run.p.Repository), nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot read the hooks with %s: %w", run.p.Tokens.Read, err)
	}
	if slices.ContainsFunc(hooks, func(h Hook) bool { return h.Active && h.Config.URL == run.r.WebhookURL }) {
		return "present", nil
	}
	return "missing: the seed makes it, factoryd never does", nil
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
	var deploy bool
	steps = append(steps, []struct {
		object string
		do     func() (Change, error)
	}{
		{"collaborators", func() (Change, error) { return run.pruneCollaborators(ctx) }},
		{"seed", func() (Change, error) { return run.seedFromTemplate(ctx) }},
		{"protection main", func() (Change, error) {
			return run.protection(ctx, "main", Protection(run.p.MergeRule, run.approvers(), run.r.MergeUser))
		}},
		// Forgejo refuses to create a branch its protection covers, even from
		// the API; roll.sh starts a missing deploy with the deploy key
		{"branch " + DeployBranch, func() (Change, error) {
			c, err := f.EnsureBranchFrom(ctx, repo, DeployBranch, "main")
			deploy = c != NoMain
			if err != nil {
				if _, covered, perr := f.ProjectBranchProtection(ctx, repo, DeployBranch); perr == nil && covered {
					deploy = true // the protection stays and must be kept; roll.sh starts the branch
					return "missing: its protection refuses the create; the next roll starts it", nil
				}
			}
			return c, err
		}},
		{"protection " + DeployBranch, func() (Change, error) {
			if !deploy {
				return NoMain, nil
			}
			return run.protection(ctx, DeployBranch, DeployProtection())
		}},
		{"fork " + f.Fork(repo).String(), func() (Change, error) { return "ready", f.EnsureFork(ctx, repo) }},
		{"labels", func() (Change, error) { return f.EnsureProjectLabels(ctx, repo.Owner, repo.Name, run.p.Lanes) }},
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
	if run.r.HostKey != nil {
		if c, err := run.hostKey(ctx, names); !run.fail("actions secret "+SecretSSHHostKey, err) {
			run.line("actions secret "+SecretSSHHostKey, c)
		}
	}
	for _, name := range []string{SecretRegistryUser, SecretRegistryPassword} {
		c := Unchanged
		if !slices.Contains(names, name) {
			c = Waiting
		}
		run.line("actions secret "+name, c)
	}
}

// seedFromTemplate commits the project's template, rendered, as main's
// first commit by factory-projects (stage11-design.md section 6). It runs
// before main's protection exists; a repository with main is never seeded,
// so no project is seeded twice.
func (run *reconcileRun) seedFromTemplate(ctx context.Context) (Change, error) {
	f, p := run.r.Forgejo, run.p
	has, err := f.ProjectBranchExists(ctx, p.Repo(), "main")
	switch {
	case err != nil:
		return "", err
	case has:
		return Unchanged, nil
	case p.Template == "":
		return "waiting: no template; main's first push starts the project", nil
	case p.TemplateFiles == nil:
		return "", fmt.Errorf("template %q is not in the configuration version in force", p.Template)
	}
	if _, protected, err := f.ProjectBranchProtection(ctx, p.Repo(), "main"); err != nil || protected {
		return "waiting: main's protection exists, which refuses the seed; push main's first commit", err
	}
	data := policy.TemplateData{Name: p.Name, Repository: p.Repository, Image: run.r.RegistryHost + "/" + p.Repository, Namespaces: map[string]string{}}
	for _, e := range p.Environments {
		data.Namespaces[e.Path] = e.Namespace
	}
	files, err := policy.RenderTemplate(p.TemplateFiles, data)
	if err != nil {
		return "", err
	}
	sha, err := f.CreateFiles(ctx, f.ProjectsToken, p.Repo(), "main", "Seed from template "+p.Template+" (registration)", files)
	if err != nil {
		return "", err
	}
	return Change(fmt.Sprintf("created %s from template %s", sha, p.Template)), nil
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

// pruneCollaborators removes every direct collaborator but the bot, the
// merge user and the approvers: factoryd owns a factory project's list
// (ruling 203), so an approver dropped from the policy leaves.
func (run *reconcileRun) pruneCollaborators(ctx context.Context) (Change, error) {
	f, repo := run.r.Forgejo, run.p.Repo()
	keep := append([]string{f.BotUser, run.r.MergeUser}, run.approvers()...)
	users, err := f.ProjectCollaborators(ctx, repo)
	if err != nil {
		return "", err
	}
	var gone []string
	for _, u := range users {
		if slices.ContainsFunc(keep, func(k string) bool { return strings.EqualFold(k, u) }) {
			continue
		}
		if err := f.RemoveProjectCollaborator(ctx, repo, u); err != nil {
			return "", err
		}
		gone = append(gone, u)
	}
	if len(gone) == 0 {
		return Unchanged, nil
	}
	return Change("removed " + strings.Join(gone, ", ")), nil
}

// Protection is main's protection for a project: the code repository's
// (infra README, "The merge bot"). factory-merge and the approvers may push;
// the deploy key may not, since the rolled state lives on DeployBranch
// (ruling 166). A project on auto needs no approval, so factory-merge can
// merge a clean pull request (ruling 149). Forgejo keeps only users with
// write in a whitelist, so the approvers are write collaborators.
func Protection(mergeRule string, approvers []string, mergeUser string) BranchProtection {
	required := 1
	if mergeRule == policy.MergeAuto {
		required = 0
	}
	return BranchProtection{
		EnablePush: true, EnablePushWhitelist: true, PushWhitelistUsernames: append(slices.Clone(approvers), mergeUser),
		EnableMergeWhitelist: true, MergeWhitelistUsernames: append(slices.Clone(approvers), mergeUser),
		EnableApprovalsWhitelist: true, ApprovalsWhitelistUsernames: slices.Clone(approvers),
		RequiredApprovals: required, ApplyToAdmins: true, BlockOnRejectedReviews: true, DismissStaleApprovals: true,
	}
}

// DeployBranch holds a project's rolled state: main merged in and the
// digests set by the main workflow's roll commit, pushed with the deploy
// key. Flux reads it.
const DeployBranch = "deploy"

// DeployProtection lets only the deploy key push to DeployBranch, and no
// push there may touch ProtectedWorkflows: every non-fork job holds
// DEPLOY_KEY (review-task65 n1).
func DeployProtection() BranchProtection {
	return BranchProtection{
		EnablePush: true, EnablePushWhitelist: true, PushWhitelistDeployKeys: true,
		EnableMergeWhitelist: true, ApplyToAdmins: true, ProtectedFilePatterns: ProtectedWorkflows,
	}
}

// ProtectedWorkflows are the paths no push to DeployBranch may touch, so
// its workflows are the ones it was created with from main.
const ProtectedWorkflows = ".forgejo/**;.gitea/**;.github/**"

func (run *reconcileRun) protection(ctx context.Context, branch string, want BranchProtection) (Change, error) {
	have, exists, err := run.r.Forgejo.ProjectBranchProtection(ctx, run.p.Repo(), branch)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return Created, run.r.Forgejo.SaveProjectBranchProtection(ctx, run.p.Repo(), branch, want, false)
	case have.equal(want):
		return Unchanged, nil
	}
	return Updated, run.r.Forgejo.SaveProjectBranchProtection(ctx, run.p.Repo(), branch, want, true)
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

// hostKey keeps FACTORY_SSH_HOST_KEY at the key a scan finds now; the row
// keeps its hash, since a secret cannot be read back.
func (run *reconcileRun) hostKey(ctx context.Context, secrets []string) (Change, error) {
	key, err := run.r.HostKey(ctx)
	if err != nil {
		return "", err
	}
	hash := HashToken(key)
	has := slices.Contains(secrets, SecretSSHHostKey)
	if has && run.p.HostKeyHash == hash {
		return Unchanged, nil
	}
	if err := run.r.Forgejo.PutActionSecret(ctx, run.p.Repo(), SecretSSHHostKey, key); err != nil {
		return "", err
	}
	if err := run.r.Store.SetProjectTokens(ctx, run.p.Name, ProjectTokens{HostKeyHash: hash}); err != nil {
		return "", err
	}
	if has {
		return Updated, nil
	}
	return Created, nil
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
			"spec":     ProjectNetworkPolicy(ns, p.Egress),
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
			"ref": map[string]any{"branch": DeployBranch}, "secretRef": map[string]any{"name": gitSecret}},
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
// SigNoz collector and the project file's egress hosts on 443.
func ProjectNetworkPolicy(ns string, egress []string) map[string]any {
	own := []any{map[string]any{"matchLabels": map[string]string{"k8s:io.kubernetes.pod.namespace": ns}}}
	ports := func(proto string, ps ...string) []any {
		var out []any
		for _, p := range ps {
			out = append(out, map[string]any{"port": p, "protocol": proto})
		}
		return []any{map[string]any{"ports": out}}
	}
	dns := ports("ANY", "53")
	out := []any{
		map[string]any{"toEndpoints": own},
		map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]string{
			"k8s:io.kubernetes.pod.namespace": "kube-system", "k8s:k8s-app": "kube-dns"}}},
			"toPorts": dns},
		map[string]any{"toEntities": []any{"kube-apiserver"}},
		map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]string{
			"k8s:io.kubernetes.pod.namespace": "platform", "app.kubernetes.io/component": "otel-collector"}}},
			"toPorts": ports("TCP", "4318")},
	}
	if len(egress) > 0 {
		// Cilium learns a name's addresses from the DNS answers its proxy sees
		dns[0].(map[string]any)["rules"] = map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}}
		names := make([]any, len(egress))
		for i, h := range egress {
			names[i] = map[string]any{"matchName": h}
		}
		out = append(out, map[string]any{"toFQDNs": names, "toPorts": ports("TCP", "443")})
	}
	return map[string]any{
		"endpointSelector": map[string]any{},
		"ingress": []any{
			map[string]any{"fromEndpoints": own},
			map[string]any{"fromEntities": []any{"ingress"}},
			map[string]any{"fromEndpoints": []any{map[string]any{"matchLabels": map[string]string{"k8s:io.kubernetes.pod.namespace": "cnpg-system"}}},
				"toPorts": ports("TCP", "8000", "5432")},
		},
		"egress": out,
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

// RegistrationError is why project name's last registration failed; ""
// when it worked or none ran since factoryd started.
func (r *Registrar) RegistrationError(name string) string {
	msg, _ := r.regErrs.Load(name)
	s, _ := msg.(string)
	return s
}

// register makes what only the site admin can (ruling 164).
func (run *reconcileRun) register(ctx context.Context, rotate bool) {
	r, name := run.r, run.p.Name
	err := r.admin(ctx, &run.p, rotate)
	if err != nil {
		r.regErrs.Store(name, err.Error())
	} else {
		r.regErrs.Delete(name)
	}
	run.fail("registration", err)
}

// Register is factoryd register-project: the factory project name, whose
// file the configuration holds, gets its row and its reconcile, which
// registers it when its row lacks a token or rotate is set (every token
// minted again). A second run changes nothing.
func (r *Registrar) Register(ctx context.Context, name string, rotate bool) (Project, error) {
	p, err := r.stored(ctx, name)
	if err != nil {
		return p, err
	}
	if !p.File || p.Objects != policy.ObjectsFactory {
		return p, fmt.Errorf("project %s: registration makes a factory project's objects; the configuration's projects/%s.yaml says objects %q", name, name, p.Objects)
	}
	created, err := r.Store.EnsureProjectRows(ctx, []string{name})
	if err != nil {
		return p, err
	}
	log.Printf("project %s: row: %s", name, map[bool]string{true: "created", false: "no change"}[len(created) == 1])
	if p, err = r.stored(ctx, name); err != nil {
		return p, err
	}
	err = r.reconcile(ctx, p, rotate)
	if stored, serr := r.stored(ctx, name); serr == nil {
		p = stored
	}
	return p, err
}

// admin makes, with the site admin's login, the organisation owned by
// factory-projects, the repository, the CI user in the organisation's ci
// team (package write), and the tokens: ci-<name>'s write:package into the
// registry Actions secrets and its read:package for the pull secret,
// factory-projects' read:repository and factory-merge's write:repository
// limited to the repository. A token p already has is kept unless rotate.
func (r *Registrar) admin(ctx context.Context, p *Project, rotate bool) error {
	a, err := r.adminAuth(ctx)
	if err != nil {
		return err
	}
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
	if err := r.Store.SetProjectTokens(ctx, p.Name, t); err != nil {
		return err
	}
	for have, minted := range map[*string]string{&p.PullToken: t.PullToken, &p.GitToken: t.GitToken, &p.MergeToken: t.MergeToken} {
		if minted != "" {
			*have = minted
		}
	}
	return nil
}

// adminAuth reads the site admin's login from OpenBao; the caller holds it
// for one registration and never logs it.
func (r *Registrar) adminAuth(ctx context.Context) (AdminAuth, error) {
	if r.OpenBao == nil {
		return AdminAuth{}, errors.New("the Forgejo admin's login: no OpenBao client")
	}
	kv, err := r.OpenBao.KV(ctx, AdminSecret)
	if err != nil {
		return AdminAuth{}, fmt.Errorf("the Forgejo admin's login: %w", err)
	}
	a := AdminAuth{User: kv["username"], Password: kv["password"]}
	if a.User == "" || a.Password == "" {
		return AdminAuth{}, fmt.Errorf("the Forgejo admin's login: %s lacks username or password", AdminSecret)
	}
	return a, nil
}

func (r *Registrar) stored(ctx context.Context, name string) (Project, error) {
	p, ok, err := r.Store.Project(ctx, name)
	if err == nil && !ok {
		err = fmt.Errorf("project %s: %w", name, ErrNotFound)
	}
	return p, err
}
