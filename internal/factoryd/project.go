package factoryd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// Project is a repository the factory works on, the namespaces it deploys
// into and its settings (ruling 146). A managed project gets its Forgejo
// and cluster objects from factoryd (ruling 156); the factory's own
// repositories are unmanaged rows whose objects the infra repository owns.
type Project struct {
	Name         string
	Repository   string // owner/name
	MergeRule    string // auto | human
	Notify       []string
	Namespaces   []string
	Environments []Environment
	Quota        policy.Quota
	// Secrets are "secret/key" entries: each namespace gets the secret with
	// a random value for the key when it is missing.
	Secrets   []string
	Managed   bool
	CreatedAt time.Time
	UpdatedAt time.Time

	// ReadTokenHash is sha256 of FACTORY_READ_TOKEN, hex; CIToken is
	// factory-ci's token limited to the repository (its Actions secrets and
	// runners); PullToken is factory-ci's read:package token. Never shown.
	ReadTokenHash string
	CIToken       string
	PullToken     string
}

// Environment is one Flux Kustomization: path of the repository applied
// into namespace (ruling 157), and the public hostname, if any, the infra
// commit gives it (ruling 155).
type Environment struct {
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
	Hostname  string `json:"hostname,omitempty"`
}

// Repo is the project's repository.
func (p Project) Repo() Repo {
	owner, name, _ := strings.Cut(p.Repository, "/")
	return Repo{Owner: owner, Name: name}
}

var (
	projectName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	repoPart     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	deployPath   = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	hostname     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	forgejoLogin = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)
	secretEntry  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?/[A-Za-z0-9_.-]{1,64}$`)
)

// reservedNamespace: namespaces a project may never name, the cluster's
// and the factory's own.
func reservedNamespace(ns string) bool {
	switch ns {
	case "default", "factory", "flux-system", "forgejo", "platform", "cnpg-system", "local-path-storage", "cilium-secrets":
		return true
	}
	return strings.HasPrefix(ns, "kube-") || strings.HasPrefix(ns, "factory-")
}

// Validate checks a managed project's row before it is stored.
func (p Project) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !projectName.MatchString(p.Name) || strings.HasPrefix(p.Name, "factory-") || p.Name == "flux-system" {
		bad("name %q: lowercase letters, digits and dashes, at most 40, not factory-* or flux-system", p.Name)
	}
	owner, name, ok := strings.Cut(p.Repository, "/")
	if !ok || !repoPart.MatchString(owner) || !repoPart.MatchString(name) || owner == "." || name == "." || name == ".." {
		bad("repository %q: want owner/name", p.Repository)
	}
	if p.MergeRule != policy.MergeAuto && p.MergeRule != policy.MergeHuman {
		bad("merge rule %q: want auto or human", p.MergeRule)
	}
	for _, u := range p.Notify {
		if !forgejoLogin.MatchString(u) {
			bad("notify %q: not a Forgejo user name", u)
		}
	}
	for i, ns := range p.Namespaces {
		if !dnsLabel.MatchString(ns) || reservedNamespace(ns) || slices.Contains(p.Namespaces[:i], ns) {
			bad("namespace %q: a distinct DNS label, not a cluster or factory namespace", ns)
		}
	}
	var hosts []string
	for i, e := range p.Environments {
		if !slices.Contains(p.Namespaces, e.Namespace) {
			bad("environment %q: not one of the project's namespaces", e.Namespace)
		}
		if slices.ContainsFunc(p.Environments[:i], func(o Environment) bool { return o.Namespace == e.Namespace }) {
			bad("environment %q: given twice", e.Namespace)
		}
		if !deployPath.MatchString(e.Path) {
			bad("environment %s: path %q: a relative directory such as deploy/staging", e.Namespace, e.Path)
		}
		if e.Hostname != "" {
			if !hostname.MatchString(e.Hostname) || slices.Contains(hosts, e.Hostname) {
				bad("environment %s: hostname %q: a distinct DNS name", e.Namespace, e.Hostname)
			}
			hosts = append(hosts, e.Hostname)
		}
	}
	if len(p.Namespaces) > 0 {
		if err := p.Quota.Validate(); err != nil {
			bad("%v", err)
		}
	}
	for i, s := range p.Secrets {
		if !secretEntry.MatchString(s) || slices.Contains(p.Secrets[:i], s) {
			bad("secret %q: want secret-name/key, each once", s)
		}
	}
	return errors.Join(errs...)
}

// ParseEnvironments reads namespace=path pairs; hostnames come from
// namespace=hostname pairs, each namespace at most once.
func ParseEnvironments(pairs, hostnames []string) ([]Environment, error) {
	var envs []Environment
	for _, p := range pairs {
		ns, path, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok {
			return nil, fmt.Errorf("environment %q: want namespace=path", p)
		}
		envs = append(envs, Environment{Namespace: ns, Path: strings.Trim(path, "/")})
	}
	for _, h := range hostnames {
		ns, host, ok := strings.Cut(strings.TrimSpace(h), "=")
		if !ok {
			return nil, fmt.Errorf("hostname %q: want namespace=hostname", h)
		}
		i := slices.IndexFunc(envs, func(e Environment) bool { return e.Namespace == ns })
		if i < 0 {
			return nil, fmt.Errorf("hostname %q: %s is no environment", h, ns)
		}
		if envs[i].Hostname != "" {
			return nil, fmt.Errorf("hostname %q: %s has one already", h, ns)
		}
		envs[i].Hostname = host
	}
	return envs, nil
}

// NotifyOrApprovers is who the project's pings go to: its notify list,
// else the policy's approvers.
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

// ProjectStore is the projects table.
type ProjectStore interface {
	Projects(ctx context.Context) ([]Project, error)
	Project(ctx context.Context, name string) (Project, bool, error)
	// CreateProject inserts p; ErrProjectExists when the name or the
	// repository is registered already.
	CreateProject(ctx context.Context, p Project) error
	// SetProjectSettings changes what the console edits.
	SetProjectSettings(ctx context.Context, name, mergeRule string, notify []string, quota policy.Quota) error
	SetProjectTokens(ctx context.Context, name string, t ProjectTokens) error
}

// ProjectTokens: an empty field leaves the stored one.
type ProjectTokens struct {
	ReadTokenHash, CIToken, PullToken string
}

// ErrProjectExists refuses a second registration of a name or repository.
var ErrProjectExists = errors.New("project already registered")

const projectColumns = `name, repository, merge_rule, notify, namespaces, environments, quota_cpu, quota_memory, quota_storage,
	secrets, managed, read_token_hash, ci_token, pull_token, created_at, updated_at`

func (s *PGStore) Projects(ctx context.Context) ([]Project, error) {
	return s.projects(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY name`)
}

func (s *PGStore) Project(ctx context.Context, name string) (Project, bool, error) {
	ps, err := s.projects(ctx, `SELECT `+projectColumns+` FROM projects WHERE name = $1`, name)
	if err != nil || len(ps) == 0 {
		return Project{}, false, err
	}
	return ps[0], true, nil
}

func (s *PGStore) projects(ctx context.Context, q string, args ...any) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := pgtype.NewMap()
	var out []Project
	for rows.Next() {
		var p Project
		var envs []byte
		if err := rows.Scan(&p.Name, &p.Repository, &p.MergeRule, m.SQLScanner(&p.Notify), m.SQLScanner(&p.Namespaces), &envs,
			&p.Quota.CPU, &p.Quota.Memory, &p.Quota.Storage, m.SQLScanner(&p.Secrets), &p.Managed,
			&p.ReadTokenHash, &p.CIToken, &p.PullToken, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(envs, &p.Environments); err != nil {
			return nil, fmt.Errorf("project %s environments: %w", p.Name, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PGStore) CreateProject(ctx context.Context, p Project) error {
	envs, err := json.Marshal(nonNil(p.Environments))
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO projects (name, repository, merge_rule, notify, namespaces, environments,
		quota_cpu, quota_memory, quota_storage, secrets, managed)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, $10, $11) ON CONFLICT DO NOTHING`,
		p.Name, p.Repository, p.MergeRule, nonNil(p.Notify), nonNil(p.Namespaces), envs,
		p.Quota.CPU, p.Quota.Memory, p.Quota.Storage, nonNil(p.Secrets), p.Managed)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(err, fmt.Errorf("%s (%s): %w", p.Name, p.Repository, ErrProjectExists))
	}
	return nil
}

func (s *PGStore) SetProjectSettings(ctx context.Context, name, mergeRule string, notify []string, quota policy.Quota) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET merge_rule = $2, notify = $3, quota_cpu = $4, quota_memory = $5,
		quota_storage = $6, updated_at = now() WHERE name = $1`,
		name, mergeRule, nonNil(notify), quota.CPU, quota.Memory, quota.Storage)
	return oneRow(res, err, "project "+name)
}

func (s *PGStore) SetProjectTokens(ctx context.Context, name string, t ProjectTokens) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET read_token_hash = COALESCE(NULLIF($2, ''), read_token_hash),
		ci_token = COALESCE(NULLIF($3, ''), ci_token), pull_token = COALESCE(NULLIF($4, ''), pull_token), updated_at = now()
		WHERE name = $1`, name, t.ReadTokenHash, t.CIToken, t.PullToken)
	return oneRow(res, err, "project "+name)
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
