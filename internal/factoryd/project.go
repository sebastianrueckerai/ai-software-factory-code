package factoryd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Project is a repository the factory works on, the namespaces it deploys
// into and its settings: its file projects/<name>.yaml in the
// configuration (rulings 146, 179) joined with its row, which keeps what
// registration minted. objects factory makes the project its own Forgejo
// organisation <name>/<name> (ruling 164) whose Forgejo and cluster
// objects factoryd keeps (ruling 156); objects seed leaves them to the
// infra repository and the seed, and factoryd only reads.
type Project struct {
	Name         string
	Repository   string // owner/name
	MergeRule    string // auto | human
	Objects      string // factory | seed
	Agents       string // coder | none
	Notify       []string
	Namespaces   []string
	Environments []Environment
	Quota        policy.Quota
	// Secrets are "secret/key" entries: each namespace gets the secret with
	// a random value for the key when it is missing.
	Secrets []string
	// Egress are the host names outside the cluster its namespaces reach
	// on 443.
	Egress []string
	// Envelope is the file's envelope and standing block, else the policy's.
	Envelope policy.Envelope
	// Budget is the file's budget block; nil without one.
	Budget *policy.ProjectBudget
	// Tokens name a seed project's seeded secrets.
	Tokens policy.SeedTokens
	// Process names the process the project's cards follow and Lanes are
	// its lanes, both in the version in force; "" and none on agents none.
	Process string
	Lanes   []policy.Lane
	// Template names the template registration seeds an empty repository
	// with; TemplateFiles are its files in the same configuration version.
	Template      string
	TemplateFiles map[string]string
	// File is whether the configuration holds projects/<name>.yaml: a row
	// without one is kept for its tokens and nothing acts on it.
	File      bool
	CreatedAt time.Time
	UpdatedAt time.Time
	// ReadyAt is when a reconcile first found every object in place; zero:
	// not yet.
	ReadyAt time.Time

	// ReadTokenHash and HookSecretHash are sha256 hex of FACTORY_READ_TOKEN
	// and of the webhook secret last set. The tokens registration
	// mints, never shown: ci-<name>'s read:package (PullToken, the image
	// pull secret), factory-projects' read:repository limited to the
	// repository (GitToken, Flux's source), factory-merge's
	// write:repository limited to it (MergeToken, the merge) and
	// factory-gate's (GateToken, the merge gate's approval).
	ReadTokenHash  string
	HookSecretHash string
	// HostKeyHash is sha256 hex of the FACTORY_SSH_HOST_KEY secret set;
	// StagingHash of STAGING_URL and STAGING_SECRETS.
	HostKeyHash string
	StagingHash string
	PullToken   string
	GitToken    string
	MergeToken  string
	GateToken   string
}

// Minted reports whether registration has run: the organisation, the
// repository and the tokens exist.
func (p Project) Minted() bool { return p.GitToken != "" }

// Active reports whether factoryd works on the project's repository: it
// has a file, and a factory project's registration has run.
func (p Project) Active() bool {
	return p.File && (p.Objects == policy.ObjectsSeed || p.Minted())
}

// Coder reports whether agent tasks run on the project.
func (p Project) Coder() bool { return p.Active() && p.Agents == policy.AgentsCoder }

// CIUser is the project's own CI user, member of its organisation only
// with package write (ruling 164).
func (p Project) CIUser() string { return "ci-" + p.Name }

// Environment is one Flux Kustomization of a project.
type Environment = policy.Environment

// StagingPath is the environment a template's main CI rolls and
// smoke-tests (templates/ts-go/.forgejo/workflows/ci.yml).
const StagingPath = "deploy/staging"

// Staging is the project's environment at StagingPath.
func (p Project) Staging() (Environment, bool) {
	i := slices.IndexFunc(p.Environments, func(e Environment) bool { return e.Path == StagingPath })
	if i < 0 {
		return Environment{}, false
	}
	return p.Environments[i], true
}

// Repo is the project's repository.
func (p Project) Repo() Repo {
	owner, name, _ := strings.Cut(p.Repository, "/")
	return Repo{Owner: owner, Name: name}
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// projectsOf joins the rows with version v's project files: a file's
// settings over its row's tokens (a file without a row has none yet), and
// a row without a file marked so, by name.
func projectsOf(rows []Project, v policy.Version) []Project {
	byName := map[string]Project{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	for name, c := range v.Projects {
		p := byName[name]
		p.Name, p.Repository, p.MergeRule, p.Objects, p.Agents = name, c.Repository, c.MergeRule, c.Objects, c.Agents
		p.Notify, p.Namespaces, p.Environments, p.Secrets, p.Egress = c.Notify, c.Namespaces, c.Environments, c.Secrets, c.Egress
		p.Quota, p.Envelope, p.Budget, p.File = v.Document.QuotaOf(c), v.Document.EnvelopeOf(c), c.Budget, true
		p.Process, p.Lanes = c.Process, v.Processes[c.Process].Lanes
		if c.Tokens != nil {
			p.Tokens = *c.Tokens
		}
		p.Template, p.TemplateFiles = c.Template, v.Templates[c.Template]
		byName[name] = p
	}
	out := slices.Collect(maps.Values(byName))
	slices.SortFunc(out, func(a, b Project) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// NotifyOrApprovers is who the project's pings go to: its notify list,
// else its approvers.
func (p Project) NotifyOrApprovers(approvers []string) []string {
	if len(p.Notify) > 0 {
		return p.Notify
	}
	return approvers
}

// NewReadToken is a fresh FACTORY_READ_TOKEN and the hash the row keeps.
func NewReadToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on Linux
	}
	token = hex.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken is the stored form of a bearer token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ReadTokenMatches compares a presented bearer against the stored hash in
// constant time; an empty stored hash matches nothing.
func (p Project) ReadTokenMatches(token string) bool {
	if p.ReadTokenHash == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(p.ReadTokenHash)) == 1
}

// ResourceQuotaHard is the quota's ResourceQuota: CPU and memory bound
// requests and limits alike, storage the claims' requests.
func ResourceQuotaHard(q policy.Quota) map[string]any {
	return map[string]any{
		"requests.cpu": q.CPU, "limits.cpu": q.CPU,
		"requests.memory": q.Memory, "limits.memory": q.Memory,
		"requests.storage": q.Storage,
	}
}

// FactoryPRCardStore is what a pass that turns a pull request factoryd
// opened into a card needs from the store (roadmap 187): to file the row
// with the open pull request linked, to read the cards a pull request already
// has and whether one reached the gates, so it files again only what its
// own failure ended, and to record its close of the card it replaces.
// PGStore answers to it; a test may give a Registrar both this and
// ProjectStore in one Store.
type FactoryPRCardStore interface {
	CreateTask(ctx context.Context, t Task) error
	// TasksByPR are the cards whose rows link pull request number on
	// repository, newest first; none means the pull request has none yet.
	TasksByPR(ctx context.Context, repository string, number int64) ([]Task, error)
	// ReachedReview reports whether task id ever moved to in-review: its
	// pull request reached the gates.
	ReachedReview(ctx context.Context, id string) (bool, error)
	InsertIntakeEvent(ctx context.Context, e IntakeEvent) (int64, error)
	IntakeEvents(ctx context.Context, taskID string) ([]IntakeEvent, error)
	ConfirmFactoryClose(ctx context.Context, taskID string, issue int64) error
	// a project without agents gets a human card (FileFactoryPRTask)
	TaskByIssue(ctx context.Context, repository string, issueNumber int64) (Task, bool, error)
}

// ProjectStore is the projects: the configuration's files joined with
// the projects table.
type ProjectStore interface {
	Projects(ctx context.Context) ([]Project, error)
	Project(ctx context.Context, name string) (Project, bool, error)
	// EnsureProjectRows inserts a row for each name that has none and
	// returns the names it inserted.
	EnsureProjectRows(ctx context.Context, names []string) ([]string, error)
	SetProjectTokens(ctx context.Context, name string, t ProjectTokens) error
	// ClaimReadToken stores hash as the read token's when none is stored
	// and reports whether it did: of two reconciles racing, only the one
	// that stored its hash sets the Actions secret.
	ClaimReadToken(ctx context.Context, name, hash string) (bool, error)
	ForgetReadToken(ctx context.Context, name, hash string) error
	// SetProjectReady sets ready_at unless it is set.
	SetProjectReady(ctx context.Context, name string) error
}

// RemovalStore's halves (roadmap 272): the cards a removal reads and the
// rows it deletes.

// ProjectTasks are every card of repository, open and ended, oldest
// first: remove-project refuses while one is open and names their tasks'
// LiteLLM keys.
func (s *PGStore) ProjectTasks(ctx context.Context, repository string) ([]Task, error) {
	return s.tasks(ctx, `SELECT `+taskColumns+` FROM tasks WHERE repository = $1 ORDER BY created_at, id`, repository)
}

// DeleteProject removes project's row, its minted tokens with it. Its
// tasks, events, ledger, audit and chat rows stay as history.
func (s *PGStore) DeleteProject(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE name = $1`, name)
	return err
}

// DeleteProjectMirrors removes project's project_mirrors rows, their
// sealed tokens with them, and says how many went (0: none were left).
func (s *PGStore) DeleteProjectMirrors(ctx context.Context, project string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_mirrors WHERE project = $1`, project)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ProjectTokens: an empty field leaves the stored one.
type ProjectTokens struct {
	ReadTokenHash, HookSecretHash, HostKeyHash, StagingHash, PullToken, GitToken, MergeToken, GateToken string
}

const projectColumns = `name, read_token_hash, hook_secret_hash, host_key_hash, staging_hash, pull_token, git_token, merge_token, gate_token, created_at, updated_at, ready_at`

// Projects are the current configuration's projects with their rows, and
// the rows no file names.
func (s *PGStore) Projects(ctx context.Context) ([]Project, error) {
	v, _, err := s.CurrentPolicy(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Project
	for rows.Next() {
		var p Project
		var ready sql.NullTime
		if err := rows.Scan(&p.Name, &p.ReadTokenHash, &p.HookSecretHash, &p.HostKeyHash, &p.StagingHash, &p.PullToken, &p.GitToken, &p.MergeToken, &p.GateToken,
			&p.CreatedAt, &p.UpdatedAt, &ready); err != nil {
			return nil, err
		}
		p.ReadyAt = ready.Time
		ps = append(ps, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return projectsOf(ps, v), nil
}

func (s *PGStore) Project(ctx context.Context, name string) (Project, bool, error) {
	return findProject(ctx, s, func(p Project) bool { return p.Name == name })
}

// ProjectByRepository is the project whose file names repository.
func (s *PGStore) ProjectByRepository(ctx context.Context, repository string) (Project, bool, error) {
	return findProject(ctx, s, func(p Project) bool { return p.File && p.Repository == repository })
}

func findProject(ctx context.Context, s interface {
	Projects(ctx context.Context) ([]Project, error)
}, match func(Project) bool) (Project, bool, error) {
	ps, err := s.Projects(ctx)
	if err != nil {
		return Project{}, false, err
	}
	if i := slices.IndexFunc(ps, match); i >= 0 {
		return ps[i], true, nil
	}
	return Project{}, false, nil
}

func (s *PGStore) EnsureProjectRows(ctx context.Context, names []string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `INSERT INTO projects (name) SELECT unnest($1::text[]) ON CONFLICT DO NOTHING RETURNING name`, nonNil(names))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var created []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		created = append(created, n)
	}
	slices.Sort(created)
	return created, rows.Err()
}

func (s *PGStore) SetProjectTokens(ctx context.Context, name string, t ProjectTokens) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET read_token_hash = COALESCE(NULLIF($2, ''), read_token_hash),
		hook_secret_hash = COALESCE(NULLIF($3, ''), hook_secret_hash), pull_token = COALESCE(NULLIF($4, ''), pull_token),
		git_token = COALESCE(NULLIF($5, ''), git_token), merge_token = COALESCE(NULLIF($6, ''), merge_token),
		host_key_hash = COALESCE(NULLIF($7, ''), host_key_hash), staging_hash = COALESCE(NULLIF($8, ''), staging_hash),
		gate_token = COALESCE(NULLIF($9, ''), gate_token), updated_at = now()
		WHERE name = $1`, name, t.ReadTokenHash, t.HookSecretHash, t.PullToken, t.GitToken, t.MergeToken, t.HostKeyHash, t.StagingHash, t.GateToken)
	return oneRow(res, err, "project "+name)
}

func (s *PGStore) ClaimReadToken(ctx context.Context, name, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET read_token_hash = $2, updated_at = now() WHERE name = $1 AND read_token_hash = ''`, name, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *PGStore) SetProjectReady(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET ready_at = now() WHERE name = $1 AND ready_at IS NULL`, name)
	return err
}

// ForgetReadToken clears the stored hash when it is still hash, so the
// next reconcile mints a fresh token.
func (s *PGStore) ForgetReadToken(ctx context.Context, name, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET read_token_hash = '' WHERE name = $1 AND read_token_hash = $2`, name, hash)
	return err
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ListenerYAML is what the infra commit for the project's hostnames adds
// to the edge gateway (ruling 155): one HTTPS listener per hostname
// admitting only its namespace, its Certificate, and the hostnames for
// the HTTP to HTTPS redirect route.
func ListenerYAML(p Project) string {
	var listeners, certs, hosts strings.Builder
	for _, e := range p.Environments {
		if e.Hostname == "" {
			continue
		}
		label, _, _ := strings.Cut(e.Hostname, ".")
		secret := strings.ReplaceAll(e.Hostname, ".", "-")
		fmt.Fprintf(&listeners, `    - name: https-%s
      protocol: HTTPS
      port: 443
      hostname: %s
      tls:
        mode: Terminate
        certificateRefs:
          - kind: Secret
            name: %s-tls
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels: {kubernetes.io/metadata.name: %s}
`, label, e.Hostname, secret, e.Namespace)
		fmt.Fprintf(&certs, `---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %s
  namespace: forgejo
spec:
  secretName: %s-tls
  dnsNames: [%s]
  issuerRef:
    name: letsencrypt-production
    kind: ClusterIssuer
`, secret, secret, e.Hostname)
		fmt.Fprintf(&hosts, ", %s", e.Hostname)
	}
	if listeners.Len() == 0 {
		return ""
	}
	return "# k8s/platform/gateway-api/edge.yaml: append to edge-gateway's listeners\n" + listeners.String() +
		"# add to the git-redirect route's hostnames: " + strings.TrimPrefix(hosts.String(), ", ") + "\n" + certs.String()
}

// OwnProject is the project whose namespaces hold ns, factoryd's own
// namespace: the factory's own code, found by its file (ruling 179).
func OwnProject(ctx context.Context, store interface {
	Projects(ctx context.Context) ([]Project, error)
}, ns string) (Project, bool, error) {
	return findProject(ctx, store, func(p Project) bool { return p.File && slices.Contains(p.Namespaces, ns) })
}
