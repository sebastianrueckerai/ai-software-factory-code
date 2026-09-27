package factoryd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CIConfig carries the untrusted PR-runner spawner's facts (task 41, ruling
// 78): one ephemeral gVisor pod per waiting Actions job, no repository or
// registry credentials, default-deny egress.
type CIConfig struct {
	FactoryNamespace string // where the pull secret lives
	NamespacePrefix  string // "factory-agents-ci-"
	Owner            string // Forgejo repo owner, e.g. "sf"
	Repo             string // e.g. "ai-software-factory-code"
	Label            string // "pr-untrusted"; the runner also declares ":host"
	RunnerImage      string // pinned digest, host-mode (Dockerfile.runner)
	PullSecret       string // registry pull secret copied into the CI namespace
	GitBaseURL       string // in-cluster Forgejo — same host RunnerConfig.GitBaseURL uses, not the public API host
	PollInterval     time.Duration
	JobTimeout       time.Duration // activeDeadlineSeconds
}

func (c *CIConfig) setDefaults() {
	if c.NamespacePrefix == "" {
		c.NamespacePrefix = "factory-agents-ci-"
	}
	if c.Label == "" {
		c.Label = "pr-untrusted"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 10 * time.Second
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 45 * time.Minute
	}
}

// CISpawner polls Forgejo for Actions jobs waiting on cfg.Label and, for each
// one not yet served, registers an ephemeral runner and spawns a namespace
// holding one gVisor pod running `forgejo-runner one-job --wait`. It never
// binds a specific job to a specific runner instance: Forgejo's own
// scheduler decides which waiting job a connected runner ends up serving.
// The namespace name and its factory.ai/task label carry the job id, so a
// restart or a second poll tick never spawns a second runner for the same
// job.
type CISpawner struct {
	cfg     CIConfig
	kube    Kube
	forgejo *Forgejo
}

const (
	ciPodName    = "ci-runner"
	ciSecretName = "ci-runner-registration"
)

func NewCISpawner(cfg CIConfig, forgejo *Forgejo, kube Kube) (*CISpawner, error) {
	cfg.setDefaults()
	if cfg.FactoryNamespace == "" || cfg.Owner == "" || cfg.Repo == "" || cfg.RunnerImage == "" || cfg.GitBaseURL == "" {
		return nil, fmt.Errorf("ci spawner: FactoryNamespace, Owner, Repo, RunnerImage, GitBaseURL are required")
	}
	if u, err := url.Parse(cfg.GitBaseURL); err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("ci spawner: GitBaseURL %q is not an absolute URL", cfg.GitBaseURL)
	}
	if forgejo.CIRegistrarToken == "" {
		return nil, fmt.Errorf("ci spawner: forgejo.CIRegistrarToken is required")
	}
	return &CISpawner{cfg: cfg, kube: kube, forgejo: forgejo}, nil
}

// Run polls until ctx ends. A poll failure logs and waits for the next
// tick rather than retrying in a tight loop.
func (s *CISpawner) Run(ctx context.Context) {
	tick := time.NewTicker(s.cfg.PollInterval)
	defer tick.Stop()
	for {
		s.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *CISpawner) poll(ctx context.Context) {
	s.reconcile(ctx)

	jobs, err := s.forgejo.WaitingRunJobs(ctx, s.cfg.Owner, s.cfg.Repo, s.cfg.Label)
	if err != nil {
		log.Printf("ci spawner: waiting jobs: %v", err)
		return
	}
	for _, job := range jobs {
		served, err := s.served(ctx, job.ID)
		if err != nil {
			log.Printf("ci spawner: check job %d served: %v", job.ID, err)
			continue
		}
		if served {
			continue
		}
		if err := s.spawn(ctx, job.ID); err != nil {
			log.Printf("ci spawner: spawn for job %d: %v", job.ID, err)
		}
	}
}

func (s *CISpawner) served(ctx context.Context, jobID int64) (bool, error) {
	names, err := s.kube.NamespacesByLabel(ctx, "factory.ai/task", jobLabel(jobID))
	if err != nil {
		return false, err
	}
	return len(names) > 0, nil
}

func jobLabel(jobID int64) string { return "ci-" + strconv.FormatInt(jobID, 10) }

// namespaceFor names the namespace after the Forgejo job id (ruling 89 #1),
// not the runner registration id: the job id is known up front from the
// waiting-jobs poll, and it is what a second poll or a restart must
// recognize to avoid spawning twice.
func (s *CISpawner) namespaceFor(jobID int64) string {
	return fmt.Sprintf("%s%d", s.cfg.NamespacePrefix, jobID)
}

func (s *CISpawner) spawn(ctx context.Context, jobID int64) error {
	name := fmt.Sprintf("ci-job-%d", jobID)
	reg, err := s.forgejo.RegisterCIRunner(ctx, s.cfg.Owner, s.cfg.Repo, name)
	if err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	ns := s.namespaceFor(jobID)
	if err := s.setupNamespace(ctx, ns, jobID, reg); err != nil {
		if derr := s.forgejo.DeleteCIRunner(ctx, s.cfg.Owner, s.cfg.Repo, reg.ID); derr != nil {
			log.Printf("ci spawner: rollback delete runner %d: %v", reg.ID, derr)
		}
		return fmt.Errorf("setup namespace %s: %w", ns, err)
	}
	return nil
}

func (s *CISpawner) setupNamespace(ctx context.Context, ns string, jobID int64, reg CIRunnerRegistration) error {
	if err := s.kube.CreateNamespace(ctx, ns, map[string]string{
		"factory.ai/task":                    jobLabel(jobID),
		"pod-security.kubernetes.io/enforce": "restricted",
	}); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	core := "/api/v1/namespaces/" + ns
	steps := []struct {
		name string
		do   func() error
	}{
		{"role binding", func() error {
			return s.kube.Create(ctx, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+ns+"/rolebindings", map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
				"metadata": map[string]any{"name": "factoryd"},
				"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "factoryd-task"},
				"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": s.cfg.FactoryNamespace}},
			})
		}},
		{"resource quota", func() error {
			return s.kube.Create(ctx, core+"/resourcequotas", map[string]any{
				"apiVersion": "v1", "kind": "ResourceQuota",
				"metadata": map[string]any{"name": "ci"},
				"spec":     map[string]any{"hard": ciQuota},
			})
		}},
		{"limit range", func() error {
			return s.kube.Create(ctx, core+"/limitranges", map[string]any{
				"apiVersion": "v1", "kind": "LimitRange",
				"metadata": map[string]any{"name": "ci"},
				"spec": map[string]any{"limits": []any{map[string]any{
					"type":           "Container",
					"default":        ciLimits,
					"defaultRequest": ciRequests,
					"max":            ciLimits,
				}}},
			})
		}},
		{"service account", func() error {
			return s.kube.CreateServiceAccount(ctx, ns, "ci-runner")
		}},
		{"egress policy", func() error {
			return s.kube.CreateCiliumNetworkPolicy(ctx, ns, "ci-egress", s.ciEgressSpec())
		}},
		{"runner registration secret", func() error {
			// forgejo-runner reads its token from a file (--token-url
			// file:...), never from argv or an env var that would land in
			// the stored pod spec; the uuid and factoryd's own runner id
			// ride the same secret since neither is repository or
			// registry-scoped.
			return s.kube.CreateSecret(ctx, ns, ciSecretName, "", map[string]string{
				"token":     reg.Secret,
				"uuid":      reg.UUID,
				"runner_id": strconv.FormatInt(reg.ID, 10),
			})
		}},
		{"pull secret", func() error {
			return copyPullSecret(ctx, s.kube, s.cfg.FactoryNamespace, s.cfg.PullSecret, ns)
		}},
	}
	for _, step := range steps {
		if err := retryForbidden(ctx, step.do); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	return s.kube.CreatePod(ctx, ns, ciPodName, s.runnerPodSpec())
}

// reconcile tears down CI namespaces whose pod has finished — successfully,
// on failure, or because activeDeadlineSeconds elapsed (kubelet marks that
// Failed with reason DeadlineExceeded, already covered by phase !=
// Succeeded && phase != Failed). A namespace whose pod never got created
// (a setup crash between the namespace step and the pod step) reads as
// "not found", torn down the same way.
func (s *CISpawner) reconcile(ctx context.Context) {
	names, err := s.kube.NamespacesByLabel(ctx, "pod-security.kubernetes.io/enforce", "restricted")
	if err != nil {
		log.Printf("ci spawner: reconcile list: %v", err)
		return
	}
	for _, ns := range names {
		if !isCINamespace(s.cfg.NamespacePrefix, ns) {
			continue
		}
		phase, _, err := s.kube.PodStatus(ctx, ns, ciPodName)
		if err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("ci spawner: reconcile %s status: %v", ns, err)
			continue
		}
		if err == nil && phase != "Succeeded" && phase != "Failed" {
			continue // still running
		}
		s.teardown(ctx, ns)
	}
	s.sweepOrphanRunners(ctx)
}

func isCINamespace(prefix, ns string) bool {
	return len(ns) > len(prefix) && ns[:len(prefix)] == prefix
}

// teardown deregisters ns's runner and reclaims the namespace. A Forbidden
// or otherwise failed read of the registration secret must not be treated
// like "no secret was ever written": the namespace is left for the next
// reconcile tick to retry, rather than deleted with the registration still
// live in Forgejo.
func (s *CISpawner) teardown(ctx context.Context, ns string) {
	data, err := s.kube.GetSecretData(ctx, ns, ciSecretName)
	switch {
	case err == nil:
		if id, convErr := strconv.ParseInt(data["runner_id"], 10, 64); convErr == nil {
			if err := s.forgejo.DeleteCIRunner(ctx, s.cfg.Owner, s.cfg.Repo, id); err != nil {
				log.Printf("ci spawner: delete runner registration for %s: %v", ns, err)
			}
		}
	case errors.Is(err, ErrNotFound):
		// no registration secret was ever written; nothing to deregister
	default:
		log.Printf("ci spawner: read registration secret for %s: %v", ns, err)
		return
	}
	if err := s.kube.DeleteNamespace(ctx, ns); err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("ci spawner: delete namespace %s: %v", ns, err)
	}
}

// sweepOrphanRunners deregisters runners that spawn's own naming convention
// (jobIDFromRunnerName) ties to a job, but for which no namespace exists.
// spawn can crash after RegisterCIRunner succeeds but before the namespace
// (or its registration secret) is ever written, leaving nothing local to
// reconcile from; Forgejo's own runner list is the only record that
// survives that crash. The single poll loop runs reconcile to completion
// before spawning any job in the same tick, so a runner this finds with no
// namespace can only be left over from a previous, crashed process.
func (s *CISpawner) sweepOrphanRunners(ctx context.Context) {
	runners, err := s.forgejo.ListCIRunners(ctx, s.cfg.Owner, s.cfg.Repo)
	if err != nil {
		log.Printf("ci spawner: sweep orphan runners: list: %v", err)
		return
	}
	for _, r := range runners {
		jobID, ok := jobIDFromRunnerName(r.Name)
		if !ok {
			continue
		}
		names, err := s.kube.NamespacesByLabel(ctx, "factory.ai/task", jobLabel(jobID))
		if err != nil {
			log.Printf("ci spawner: sweep orphan runner %d: %v", r.ID, err)
			continue
		}
		if len(names) > 0 {
			continue
		}
		if err := s.forgejo.DeleteCIRunner(ctx, s.cfg.Owner, s.cfg.Repo, r.ID); err != nil {
			log.Printf("ci spawner: sweep delete orphan runner %d: %v", r.ID, err)
		}
	}
}

// jobIDFromRunnerName recovers the job id spawn named the runner for
// ("ci-job-<id>"); a name that doesn't match this shape was not spawn's.
func jobIDFromRunnerName(name string) (int64, bool) {
	const prefix = "ci-job-"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(name[len(prefix):], 10, 64)
	return id, err == nil
}

// CI namespace resources: `go test -race` and gosec need more headroom than
// the LLM-harness worker sandbox, but the namespace holds exactly one pod
// for its whole life, never two.
var (
	ciRequests = map[string]any{"cpu": "500m", "memory": "1Gi"}
	ciLimits   = map[string]any{"cpu": "2", "memory": "4Gi"}
	ciQuota    = map[string]any{
		"pods":            "1",
		"requests.cpu":    "500m",
		"requests.memory": "1Gi",
		"limits.cpu":      "2",
		"limits.memory":   "4Gi",
	}
)

func (s *CISpawner) runnerPodSpec() map[string]any {
	pullSecrets := []any{}
	if s.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": s.cfg.PullSecret})
	}
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				"app.kubernetes.io/name": "ci-runner",
			},
		},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"activeDeadlineSeconds":        int64(s.cfg.JobTimeout / time.Second),
			"runtimeClassName":             "gvisor",
			"serviceAccountName":           "ci-runner",
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"imagePullSecrets":              pullSecrets,
			"terminationGracePeriodSeconds": 30,
			"dnsConfig":                     map[string]any{"options": []any{map[string]any{"name": "ndots", "value": "1"}}},
			"containers": []any{map[string]any{
				"name":    "runner",
				"image":   s.cfg.RunnerImage,
				"command": []any{"forgejo-runner"},
				"args": []any{
					"one-job",
					"--url", s.cfg.GitBaseURL,
					"--uuid", "$(RUNNER_UUID)",
					"--token-url", "file:/var/run/secrets/ci/token",
					"--label", s.cfg.Label + ":host",
					"--wait",
				},
				"env": []any{
					map[string]any{"name": "HOME", "value": "/home/runner"},
					map[string]any{"name": "RUNNER_UUID", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": ciSecretName, "key": "uuid"}}},
				},
				"resources": map[string]any{
					"requests": ciRequests,
					"limits":   ciLimits,
				},
				"volumeMounts": []any{
					map[string]any{"name": "workspace", "mountPath": "/workspace"},
					map[string]any{"name": "home", "mountPath": "/home/runner"},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
					map[string]any{"name": "registration", "mountPath": "/var/run/secrets/ci", "readOnly": true},
				},
				"workingDir": "/workspace",
				"securityContext": map[string]any{
					"runAsNonRoot":             true,
					"runAsUser":                1000,
					"runAsGroup":               1000,
					"allowPrivilegeEscalation": false,
					"capabilities":             map[string]any{"drop": []any{"ALL"}},
					"readOnlyRootFilesystem":   true,
				},
			}},
			"volumes": []any{
				map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "8Gi"}},
				map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "2Gi"}},
				map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
				map[string]any{"name": "registration", "secret": map[string]any{"secretName": ciSecretName}},
			},
		},
	}
}

// ciEgressSpec allows exactly what a host-mode Actions job needs (ruling 89
// #5): the in-cluster Forgejo service (checkout, job protocol — same
// toEndpoints reasoning as workerEgressSpec, not toFQDNs), the Go module
// proxy and checksum database, the host actions/checkout resolves against,
// and the registry hosts the repo's Dockerfiles' base images need. No
// Kubernetes API, no other namespace, no LiteLLM, no gateway. Rootless
// BuildKit's own build step is not run by pr.yml (blocked — see the task 41
// report), but the network path is provisioned for when it is.
func (s *CISpawner) ciEgressSpec() map[string]any {
	u, _ := url.Parse(s.cfg.GitBaseURL) // validated in NewCISpawner
	fqdns := []string{
		"proxy.golang.org", "sum.golang.org", // go module proxy + checksum db
		"data.forgejo.org",                                                           // DEFAULT_ACTIONS_URL: where actions/checkout resolves from
		"registry-1.docker.io", "auth.docker.io", "production.cloudflare.docker.com", // Docker Hub, for the repo's Dockerfile base images
	}
	dnsNames := make([]any, 0, len(fqdns))
	for _, host := range fqdns {
		dnsNames = append(dnsNames, map[string]any{"matchName": host})
	}
	dnsNames = append(dnsNames, map[string]any{"matchName": u.Hostname()})

	toFQDNs := make([]any, 0, len(fqdns))
	for _, host := range fqdns {
		toFQDNs = append(toFQDNs, map[string]any{"matchName": host})
	}

	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": "ci-runner"},
		},
		"egressDeny": []any{
			map[string]any{"toCIDR": []string{"169.254.169.254/32"}},
		},
		"egress": []any{
			map[string]any{
				"toEndpoints": []any{map[string]any{
					"matchLabels": map[string]string{
						"k8s:io.kubernetes.pod.namespace": "forgejo",
						"k8s:app.kubernetes.io/name":      "forgejo",
					},
				}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "3000", "protocol": "TCP"}},
				}},
			},
			map[string]any{
				"toFQDNs": toFQDNs,
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}},
				}},
			},
			map[string]any{
				"toEndpoints": []any{map[string]any{
					"matchLabels": map[string]string{
						"k8s:io.kubernetes.pod.namespace": "kube-system",
						"k8s:k8s-app":                     "kube-dns",
					},
				}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
					"rules": map[string]any{"dns": dnsNames},
				}},
			},
		},
	}
}
