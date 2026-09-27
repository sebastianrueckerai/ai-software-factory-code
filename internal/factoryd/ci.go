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
	// BuildLabel jobs (ruling 91) get the same restricted runner pod plus a
	// second, sandbox-root buildkitd pod in the same namespace: no rootless
	// BuildKit path works under gVisor (task-41-report.md, design decision
	// #4; research-stage6-buildkit-gvisor.md in the build-plan ledger).
	BuildLabel   string // "pr-untrusted-build"
	RunnerImage  string // pinned digest, host-mode (Dockerfile.runner); also runs buildkitd
	PullSecret   string // registry pull secret copied into the CI namespace
	GitBaseURL   string // in-cluster Forgejo — same host RunnerConfig.GitBaseURL uses, not the public API host
	PollInterval time.Duration
	JobTimeout   time.Duration // activeDeadlineSeconds
	// MaxConcurrent caps the live CI namespaces; jobs beyond it stay
	// waiting in Forgejo for a later tick. 0 = 8.
	MaxConcurrent int
}

func (c *CIConfig) setDefaults() {
	if c.NamespacePrefix == "" {
		c.NamespacePrefix = "factory-agents-ci-"
	}
	if c.Label == "" {
		c.Label = "pr-untrusted"
	}
	if c.BuildLabel == "" {
		c.BuildLabel = "pr-untrusted-build"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 10 * time.Second
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 45 * time.Minute
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 8
	}
}

// CISpawner polls Forgejo for Actions jobs waiting on cfg.Label or
// cfg.BuildLabel and, for each one not yet served, registers an ephemeral
// runner and spawns a namespace holding one gVisor pod running
// `forgejo-runner one-job --wait` (plus, for a build-label job, a second
// buildkitd pod). It never binds a specific job to a specific runner
// instance: Forgejo's own scheduler decides which waiting job a connected
// runner ends up serving. The namespace name and its factory.ai/task label
// carry the job id, so a restart or a second poll tick never spawns a second
// runner for the same job.
type CISpawner struct {
	cfg     CIConfig
	kube    Kube
	forgejo *Forgejo
}

const (
	ciPodName    = "ci-runner"
	ciSecretName = "ci-runner-registration"

	// buildPodName is the buildkitd pod ruling 91 adds alongside ciPodName
	// for pr-untrusted-build jobs.
	buildPodName = "buildkitd"
	// buildPodLabelKey marks the one pod the VAP's build-pod carve-out
	// allows to run as root (k8s/factoryd.yaml).
	buildPodLabelKey = "factory.ai/build-pod"
	buildPodPort     = 1234
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

// ciJobKind distinguishes the ordinary restricted runner (build: false) from
// a pr-untrusted-build job, which additionally gets a sandbox-root buildkitd
// pod, a baseline-labeled namespace and a wider resource quota (ruling 91).
type ciJobKind struct {
	label string
	build bool
}

func (s *CISpawner) jobKinds() []ciJobKind {
	return []ciJobKind{
		{label: s.cfg.Label},
		{label: s.cfg.BuildLabel, build: true},
	}
}

func (s *CISpawner) poll(ctx context.Context) {
	slots := s.cfg.MaxConcurrent - s.reconcile(ctx)

	for _, kind := range s.jobKinds() {
		jobs, err := s.forgejo.WaitingRunJobs(ctx, s.cfg.Owner, s.cfg.Repo, kind.label)
		if err != nil {
			log.Printf("ci spawner: waiting jobs (%s): %v", kind.label, err)
			continue
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
			if slots <= 0 {
				log.Printf("ci spawner: job %d (%s) stays queued: %d runners live, the cap", job.ID, kind.label, s.cfg.MaxConcurrent)
				continue
			}
			if err := s.spawn(ctx, job.ID, kind); err != nil {
				log.Printf("ci spawner: spawn for job %d: %v", job.ID, err)
				continue
			}
			slots--
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

func (s *CISpawner) spawn(ctx context.Context, jobID int64, kind ciJobKind) error {
	name := fmt.Sprintf("ci-job-%d", jobID)
	reg, err := s.forgejo.RegisterCIRunner(ctx, s.cfg.Owner, s.cfg.Repo, name)
	if err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	ns := s.namespaceFor(jobID)
	if err := s.setupNamespace(ctx, ns, jobID, reg, kind); err != nil {
		if derr := s.forgejo.DeleteCIRunner(ctx, s.cfg.Owner, s.cfg.Repo, reg.ID); derr != nil {
			log.Printf("ci spawner: rollback delete runner %d: %v", reg.ID, derr)
		}
		return fmt.Errorf("setup namespace %s: %w", ns, err)
	}
	return nil
}

type ciSetupStep struct {
	name string
	do   func() error
}

func (s *CISpawner) setupNamespace(ctx context.Context, ns string, jobID int64, reg CIRunnerRegistration, kind ciJobKind) error {
	enforce := "restricted"
	if kind.build {
		// Sandbox-root buildkitd (ruling 91) needs runAsUser: 0, which
		// `restricted` forbids unconditionally; `baseline` still forbids
		// privileged containers, host namespaces, hostPath and added
		// capabilities, all of which the VAP's build-pod carve-out also
		// re-checks (k8s/factoryd.yaml).
		enforce = "baseline"
	}
	if err := s.kube.CreateNamespace(ctx, ns, map[string]string{
		"factory.ai/task":                    jobLabel(jobID),
		"pod-security.kubernetes.io/enforce": enforce,
	}); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	core := "/api/v1/namespaces/" + ns
	steps := []ciSetupStep{
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
				"spec":     map[string]any{"hard": ciQuotaFor(kind.build)},
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
			return s.kube.CreateCiliumNetworkPolicy(ctx, ns, "ci-egress", s.ciEgressSpec(ns, kind))
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
	if kind.build {
		steps = append(steps,
			ciSetupStep{"buildkitd service", func() error {
				return s.kube.Create(ctx, core+"/services", s.buildServiceSpec())
			}},
			ciSetupStep{"buildkitd egress policy", func() error {
				return s.kube.CreateCiliumNetworkPolicy(ctx, ns, "buildkitd-policy", s.buildEgressSpec())
			}},
		)
	}
	for _, step := range steps {
		if err := retryForbidden(ctx, step.do); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	if err := s.kube.CreatePod(ctx, ns, ciPodName, s.runnerPodSpec(ns, kind)); err != nil {
		return err
	}
	if !kind.build {
		return nil
	}
	// No token, no Secret, no SA token: the build pod never mounts
	// ciSecretName or the ci-runner ServiceAccount (ruling 91 #1).
	return s.kube.CreatePod(ctx, ns, buildPodName, s.buildPodSpec())
}

// reconcile tears down CI namespaces whose pod has finished — successfully,
// on failure, or because activeDeadlineSeconds elapsed (kubelet marks that
// Failed with reason DeadlineExceeded, already covered by phase !=
// Succeeded && phase != Failed). A namespace whose pod never got created
// (a setup crash between the namespace step and the pod step) reads as
// "not found", torn down the same way. Both PSA levels a CI namespace can
// carry — restricted (ordinary jobs) and baseline (build jobs, ruling 91)
// — are listed so build namespaces get reaped too. It reports how many
// namespaces are still live; one whose status cannot be read counts as
// live, so the cap fails closed.
func (s *CISpawner) reconcile(ctx context.Context) (live int) {
	var names []string
	for _, enforce := range []string{"restricted", "baseline"} {
		found, err := s.kube.NamespacesByLabel(ctx, "pod-security.kubernetes.io/enforce", enforce)
		if err != nil {
			log.Printf("ci spawner: reconcile list (%s): %v", enforce, err)
			continue
		}
		names = append(names, found...)
	}
	for _, ns := range names {
		if !isCINamespace(s.cfg.NamespacePrefix, ns) {
			continue
		}
		phase, _, err := s.kube.PodStatus(ctx, ns, ciPodName)
		if err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("ci spawner: reconcile %s status: %v", ns, err)
			live++
			continue
		}
		if err == nil && phase != "Succeeded" && phase != "Failed" {
			live++
			continue
		}
		s.teardown(ctx, ns)
	}
	s.sweepOrphanRunners(ctx)
	return live
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
// the LLM-harness worker sandbox, but an ordinary namespace holds exactly
// one pod for its whole life, never two.
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

// ciQuotaFor doubles the namespace-wide quota for a build-label job, whose
// namespace holds both the runner pod and the buildkitd pod at once (each
// still capped per-container by the LimitRange below, unchanged).
func ciQuotaFor(build bool) map[string]any {
	if !build {
		return ciQuota
	}
	return map[string]any{
		"pods":            "2",
		"requests.cpu":    "1",
		"requests.memory": "2Gi",
		"limits.cpu":      "4",
		"limits.memory":   "8Gi",
	}
}

func (s *CISpawner) runnerPodSpec(ns string, kind ciJobKind) map[string]any {
	pullSecrets := []any{}
	if s.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": s.cfg.PullSecret})
	}
	env := []any{
		map[string]any{"name": "HOME", "value": "/home/runner"},
		map[string]any{"name": "RUNNER_UUID", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": ciSecretName, "key": "uuid"}}},
	}
	if kind.build {
		// buildctl reads BUILDKIT_HOST itself; pr.yml's image-build job
		// needs no --addr flag. The FQDN is resolved once here, where ns is
		// already known, rather than guessed at by the runner from the
		// pod's own search domains.
		env = append(env, map[string]any{
			"name": "BUILDKIT_HOST", "value": "tcp://" + buildServiceFQDN(ns) + ":" + strconv.Itoa(buildPodPort),
		})
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
					"--label", kind.label + ":host",
					"--wait",
				},
				"env": env,
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

func buildServiceFQDN(ns string) string {
	return fmt.Sprintf("buildkitd.%s.svc.cluster.local", ns)
}

// buildServiceSpec exposes the buildkitd pod at a stable in-namespace DNS
// name so the runner pod's static pr.yml step never has to discover a pod
// IP (it has no Kubernetes API access to do so: automountServiceAccountToken
// is false and the ci-runner ServiceAccount grants nothing cluster-facing).
func (s *CISpawner) buildServiceSpec() map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "buildkitd"},
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]string{"app.kubernetes.io/name": "buildkitd"},
			"ports": []any{map[string]any{
				"port": int64(buildPodPort), "targetPort": int64(buildPodPort), "protocol": "TCP",
			}},
		},
	}
}

// buildkitdScript is the V4-wide recipe (research-stage6-buildkit-gvisor.md):
// under real gVisor, no rootless BuildKit path comes up at all — rootlesskit
// and unshare --map-users both fail at the newuidmap/uid_map write, and a
// single-uid --map-root-user mapping is too narrow for any real base image.
// The only working shape is a genuinely-root parent that writes a wide,
// full-range uid_map/gid_map directly into a nested `unshare --user --mount`
// child, then execs buildkitd inside it. setgroups is left at its default
// "allow": writing "deny" breaks every apt-get install in the build steps
// (apt drops privilege via setgroups for its own sandboxing), and the
// already-root writer does not need "deny" to set a multi-entry gid_map.
//
// unshare(1) (no --fork) performs the unshare(2) syscall itself before it
// execs the child command, so by the time that command's first instruction
// runs, the new user namespace already exists under the same PID — but
// nothing guarantees the parent's own next instruction (backgrounding,
// reading $!, then the uid_map write) runs any later than that. Live under
// gVisor, the parent routinely wins that race and writes to a /proc/$CPID
// that the child hasn't finished becoming yet, failing both writes with
// "printf: I/O error" (confirmed reproducible every time without the fix,
// zero times with it, across five live pods). The child now acks over a
// second fifo the instant it is running — which, per the above, proves its
// own unshare(2) already happened — and the parent waits for that ack
// before touching uid_map/gid_map at all.
func buildkitdScript() string {
	return fmt.Sprintf(`set -eu
mkfifo /tmp/ready.fifo /tmp/go.fifo
unshare --user --mount -- sh -c '
  echo ready > /tmp/ready.fifo
  read x < /tmp/go.fifo
  exec buildkitd --oci-worker-binary=/usr/local/bin/runc --oci-worker-no-process-sandbox --oci-worker-snapshotter=native --root /scratch/bk --addr tcp://0.0.0.0:%d
' &
CPID=$!
read x < /tmp/ready.fifo
printf "0 0 65536\n" > /proc/$CPID/uid_map
printf "0 0 65536\n" > /proc/$CPID/gid_map
echo go > /tmp/go.fifo
wait $CPID
`, buildPodPort)
}

// buildPodSpec is the sandbox-root buildkitd pod (ruling 91): fits Pod
// Security baseline, not restricted (root user is baseline-legal — no
// privileged container, no host namespaces, no added capabilities beyond
// the default set). Reuses the ci-runner image unmodified: it already ships
// buildctl, buildkitd and (Dockerfile.runner) runc. No job token, no
// registration Secret, no ServiceAccount token — this pod never touches
// Forgejo or the repository, it only builds what the runner pod hands it
// over the network.
func (s *CISpawner) buildPodSpec() map[string]any {
	pullSecrets := []any{}
	if s.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": s.cfg.PullSecret})
	}
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				"app.kubernetes.io/name": "buildkitd",
				buildPodLabelKey:         "true",
			},
		},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"activeDeadlineSeconds":        int64(s.cfg.JobTimeout / time.Second),
			"runtimeClassName":             "gvisor",
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"runAsUser":      int64(0),
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"imagePullSecrets":              pullSecrets,
			"terminationGracePeriodSeconds": 30,
			"containers": []any{map[string]any{
				"name":    "buildkitd",
				"image":   s.cfg.RunnerImage,
				"command": []any{"sh", "-c", buildkitdScript()},
				"ports": []any{map[string]any{
					"containerPort": int64(buildPodPort), "protocol": "TCP",
				}},
				// The container process (the unshare parent) starts well
				// before buildkitd itself is listening: it still has to
				// write the uid/gid maps and let the nested child exec.
				// Without this, kubelet marks the pod Ready — and the
				// Service adds it as an endpoint — the instant the shell
				// script starts, so the runner pod's first buildctl call
				// can lose the race and fail in seconds.
				"readinessProbe": map[string]any{
					"tcpSocket":        map[string]any{"port": int64(buildPodPort)},
					"periodSeconds":    int64(1),
					"failureThreshold": int64(30),
				},
				"resources": map[string]any{
					"requests": ciRequests,
					"limits":   ciLimits,
				},
				"volumeMounts": []any{
					map[string]any{"name": "scratch", "mountPath": "/scratch"},
					map[string]any{"name": "tmp", "mountPath": "/tmp"},
				},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": true,
				},
			}},
			"volumes": []any{
				// native snapshotter is copy-based, not copy-on-write (no
				// overlayfs under gVisor): sized well past what three
				// sequential Dockerfile builds accumulate, instead of the
				// default tmpfs sizing that exhausted mid-spike.
				map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": "20Gi"}},
				map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
			},
		},
	}
}

// ciEgressSpec allows exactly what a host-mode Actions job needs (ruling 89
// #5): the in-cluster Forgejo service (checkout, job protocol — same
// toEndpoints reasoning as workerEgressSpec, not toFQDNs), the Go module
// proxy and checksum database, the host actions/checkout resolves against,
// and the registry hosts the repo's Dockerfiles' base images need. No
// Kubernetes API, no other namespace, no LiteLLM, no gateway. A build-label
// job (ruling 91) additionally reaches the buildkitd pod this same
// namespace holds, by endpoint selector and by its one DNS name — nothing
// else about the runner pod's own network posture changes.
func (s *CISpawner) ciEgressSpec(ns string, kind ciJobKind) map[string]any {
	u, _ := url.Parse(s.cfg.GitBaseURL) // validated in NewCISpawner
	fqdns := []string{
		"proxy.golang.org", "sum.golang.org", // go module proxy + checksum db
		"data.forgejo.org",                                                           // DEFAULT_ACTIONS_URL: where actions/checkout resolves from
		"registry-1.docker.io", "auth.docker.io", "production.cloudfront.docker.com", // Docker Hub, for the repo's Dockerfile base images
	}
	dnsNames := make([]any, 0, len(fqdns)+2)
	for _, host := range fqdns {
		dnsNames = append(dnsNames, map[string]any{"matchName": host})
	}
	dnsNames = append(dnsNames, map[string]any{"matchName": u.Hostname()})
	if kind.build {
		dnsNames = append(dnsNames, map[string]any{"matchName": buildServiceFQDN(ns)})
	}

	toFQDNs := make([]any, 0, len(fqdns))
	for _, host := range fqdns {
		toFQDNs = append(toFQDNs, map[string]any{"matchName": host})
	}

	egress := []any{
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
	}
	if kind.build {
		egress = append(egress, map[string]any{
			"toEndpoints": []any{map[string]any{
				"matchLabels": map[string]string{"app.kubernetes.io/name": "buildkitd"},
			}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": strconv.Itoa(buildPodPort), "protocol": "TCP"}},
			}},
		})
	}

	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": "ci-runner"},
		},
		"egressDeny": []any{
			map[string]any{"toCIDR": []string{"169.254.169.254/32"}},
		},
		"egress": egress,
	}
}

// buildEgressSpec is the buildkitd pod's own policy (ruling 91 #3): egress
// limited to exactly the hosts the three Dockerfiles' FROM/RUN/ADD lines
// need — read directly from Dockerfile, Dockerfile.sandbox and
// Dockerfile.runner — DNS matchName only (no wildcards), cloud metadata
// denied; ingress accepted only from the runner pod, on the buildkitd port.
// Docker Hub (base images for all three), the Go module proxy/sumdb (`go
// mod download` in Dockerfile and Dockerfile.sandbox), the Debian archive
// (apt in Dockerfile.sandbox and Dockerfile.runner — plain http, not
// https), and the release-download hosts Dockerfile.runner's ADD lines fetch
// forgejo-runner/BuildKit/runc/gosec and the Go toolchain from. GitHub
// redirects release-asset blobs to one of two CDN hosts depending on the
// release: BuildKit/runc/gosec land on objects.githubusercontent.com, but
// Dockerfile.sandbox's own opencode binary ADD (a hard requirement, unlike
// the tolerated-failure step below) redirects to
// release-assets.githubusercontent.com instead — confirmed live from a
// buildkitd "server misbehaving" DNS-block error, the same failure mode as
// the Docker Hub CDN hostname fix above.
// Dockerfile.sandbox's opencode-plugin stage bakes @opencode-ai/plugin with a
// hard-requiring `npm ci` (032029f restored this after the warm-cache/`||
// true` version it replaced proved unreliable), so registry.npmjs.org must be
// reachable for that build to succeed — it is not a tolerated-failure step.
func (s *CISpawner) buildEgressSpec() map[string]any {
	httpsHosts := []string{
		"registry-1.docker.io", "auth.docker.io", "production.cloudfront.docker.com", // Docker Hub base images
		"proxy.golang.org", "sum.golang.org", // go mod download
		"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com", // buildkit, runc, gosec, opencode release downloads
		"registry.npmjs.org",      // opencode-plugin stage's npm ci bake (Dockerfile.sandbox)
		"code.forgejo.org",        // forgejo-runner release download
		"go.dev", "dl.google.com", // Go toolchain download
	}
	httpHosts := []string{"deb.debian.org", "security.debian.org"} // apt, plain http by default in bookworm-slim

	dnsNames := make([]any, 0, len(httpsHosts)+len(httpHosts))
	for _, host := range httpsHosts {
		dnsNames = append(dnsNames, map[string]any{"matchName": host})
	}
	for _, host := range httpHosts {
		dnsNames = append(dnsNames, map[string]any{"matchName": host})
	}
	toFQDNsHTTPS := make([]any, 0, len(httpsHosts))
	for _, host := range httpsHosts {
		toFQDNsHTTPS = append(toFQDNsHTTPS, map[string]any{"matchName": host})
	}
	toFQDNsHTTP := make([]any, 0, len(httpHosts))
	for _, host := range httpHosts {
		toFQDNsHTTP = append(toFQDNsHTTP, map[string]any{"matchName": host})
	}

	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": "buildkitd"},
		},
		"egressDeny": []any{
			map[string]any{"toCIDR": []string{"169.254.169.254/32"}},
		},
		"ingress": []any{
			map[string]any{
				"fromEndpoints": []any{map[string]any{
					"matchLabels": map[string]string{"app.kubernetes.io/name": "ci-runner"},
				}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": strconv.Itoa(buildPodPort), "protocol": "TCP"}},
				}},
			},
		},
		"egress": []any{
			map[string]any{
				"toFQDNs": toFQDNsHTTPS,
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}},
				}},
			},
			map[string]any{
				"toFQDNs": toFQDNsHTTP,
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "80", "protocol": "TCP"}},
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
