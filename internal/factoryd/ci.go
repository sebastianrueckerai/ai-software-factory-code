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
	// BuildLabel jobs get the same runner pod plus a throwaway build VM in
	// the Hetzner CI project (ruling 110, buildvm.go).
	BuildLabel   string // "pr-untrusted-build"
	RunnerImage  string // pinned digest, host-mode (Dockerfile.runner)
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
// `forgejo-runner one-job --wait` (plus, for a build-label job, a build VM
// the pod reaches over mTLS). It never binds a specific job to a specific runner
// instance: Forgejo's own scheduler decides which waiting job a connected
// runner ends up serving. The namespace name and its factory.ai/task label
// carry the job id, so a restart or a second poll tick never spawns a second
// runner for the same job.
type CISpawner struct {
	cfg     CIConfig
	kube    Kube
	forgejo *Forgejo
	builds  *BuildVMs
	now     func() time.Time
	// tornDown remembers build jobs whose namespace was torn down, so one
	// Forgejo still lists as waiting gets no second VM within
	// ciReclaimWindow; the poll loop is the only user.
	tornDown map[int64]time.Time
}

// ciReclaimWindow: a job whose runner served another waiting job is still
// served by the next build runner that registers; after the window it is
// claimed for itself again.
const ciReclaimWindow = 3 * time.Minute

const (
	ciPodName    = "ci-runner"
	ciSecretName = "ci-runner-registration"
)

func NewCISpawner(cfg CIConfig, forgejo *Forgejo, kube Kube, builds *BuildVMs) (*CISpawner, error) {
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
	if builds == nil {
		return nil, fmt.Errorf("ci spawner: build VMs are required")
	}
	return &CISpawner{cfg: cfg, kube: kube, forgejo: forgejo, builds: builds, now: time.Now, tornDown: map[int64]time.Time{}}, nil
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

// ciJobKind distinguishes the ordinary runner (build: false) from a
// pr-untrusted-build job, which additionally gets a build VM.
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
			if kind.build {
				if at, ok := s.tornDown[job.ID]; ok && s.now().Sub(at) < ciReclaimWindow {
					continue
				}
				free, err := s.builds.FreeSlot(ctx)
				if err != nil {
					log.Printf("ci spawner: job %d stays queued: build vm slots: %v", job.ID, err)
					continue
				}
				if !free {
					log.Printf("ci spawner: job %d (%s) stays queued: no build VM slot (%d live at most, or backing off a provider failure)", job.ID, kind.label, s.builds.cfg.Max)
					continue
				}
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
	if err := s.kube.CreateNamespace(ctx, ns, map[string]string{
		"factory.ai/task":                    jobLabel(jobID),
		"factory.ai/ci":                      "true", // the build cache admits reads from these namespaces
		"pod-security.kubernetes.io/enforce": "restricted",
	}); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	// The VM comes after the namespace: the collector reads a missing
	// namespace as a finished job and would delete a VM made before it. A
	// failure from here on leaves the namespace podless for the next
	// reconcile, whose teardown releases the VM.
	var vm *BuildVM
	if kind.build {
		provisioned, err := s.builds.Provision(ctx, jobID)
		if err != nil {
			return fmt.Errorf("build vm: %w", err)
		}
		vm = &provisioned
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
			return s.kube.CreateCiliumNetworkPolicy(ctx, ns, "ci-egress", s.ciEgressSpec(vm))
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
	if vm != nil {
		steps = append(steps, ciSetupStep{"buildkit client secret", func() error {
			return s.kube.CreateSecret(ctx, ns, buildTLSSecret, "", buildTLSSecretData(vm.PKI))
		}})
	}
	for _, step := range steps {
		if err := retryForbidden(ctx, step.do); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	return s.kube.CreatePod(ctx, ns, ciPodName, s.runnerPodSpec(kind, vm))
}

// reconcile tears down CI namespaces whose pod has finished — successfully,
// on failure, or because activeDeadlineSeconds elapsed (kubelet marks that
// Failed with reason DeadlineExceeded, already covered by phase !=
// Succeeded && phase != Failed). A namespace whose pod never got created
// (a setup crash between the namespace step and the pod step) reads as
// "not found", torn down the same way. It reports how many namespaces are
// still live; one whose status cannot be read counts as live, so the cap
// fails closed.
func (s *CISpawner) reconcile(ctx context.Context) (live int) {
	for id, at := range s.tornDown {
		if s.now().Sub(at) >= ciReclaimWindow {
			delete(s.tornDown, id)
		}
	}
	names, err := s.kube.NamespacesByLabel(ctx, "pod-security.kubernetes.io/enforce", "restricted")
	if err != nil {
		log.Printf("ci spawner: reconcile list: %v", err)
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
		if err == nil { // a runner ran: its job may still be listed waiting
			if id, perr := strconv.ParseInt(strings.TrimPrefix(ns, s.cfg.NamespacePrefix), 10, 64); perr == nil {
				s.tornDown[id] = s.now()
			}
		}
		s.teardown(ctx, ns)
	}
	s.sweepOrphanRunners(ctx)
	return live
}

func isCINamespace(prefix, ns string) bool {
	return len(ns) > len(prefix) && ns[:len(prefix)] == prefix
}

// teardown deregisters ns's runner, releases its build VM and reclaims the
// namespace. A VM that fails to release is the collector's: its rows stay
// open and the missing namespace marks the job finished. A Forbidden
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
	if jobID, err := strconv.ParseInt(strings.TrimPrefix(ns, s.cfg.NamespacePrefix), 10, 64); err == nil {
		if err := s.builds.Release(ctx, jobID); err != nil {
			log.Printf("ci spawner: release build vm of %s: %v", ns, err)
		}
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
// the LLM-harness worker sandbox; a namespace holds exactly one pod.
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

func (s *CISpawner) runnerPodSpec(kind ciJobKind, vm *BuildVM) map[string]any {
	pullSecrets := []any{}
	if s.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": s.cfg.PullSecret})
	}
	env := []any{
		map[string]any{"name": "HOME", "value": "/home/runner"},
		map[string]any{"name": "RUNNER_UUID", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": ciSecretName, "key": "uuid"}}},
	}
	mounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/workspace"},
		map[string]any{"name": "home", "mountPath": "/home/runner"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "registration", "mountPath": "/var/run/secrets/ci", "readOnly": true},
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "8Gi"}},
		map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "2Gi"}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
		map[string]any{"name": "registration", "secret": map[string]any{"secretName": ciSecretName}},
	}
	if vm != nil {
		env = append(env, buildVMEnv(vm.IPv4)...)
		mounts = append(mounts, map[string]any{"name": buildTLSSecret, "mountPath": buildTLSDir, "readOnly": true})
		volumes = append(volumes, map[string]any{"name": buildTLSSecret, "secret": map[string]any{"secretName": buildTLSSecret}})
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
				"volumeMounts": mounts,
				"workingDir":   "/workspace",
				"securityContext": map[string]any{
					"runAsNonRoot":             true,
					"runAsUser":                1000,
					"runAsGroup":               1000,
					"allowPrivilegeEscalation": false,
					"capabilities":             map[string]any{"drop": []any{"ALL"}},
					"readOnlyRootFilesystem":   true,
				},
			}},
			"volumes": volumes,
		},
	}
}

// buildCacheHost is the in-cluster registry pr.yml's image-build reads
// layer caches from (infra k8s/platform/build-cache, ruling 111).
const buildCacheHost = "build-cache.forgejo.svc.cluster.local"

// ciEgressSpec allows exactly what a host-mode Actions job needs (ruling 89
// #5): the in-cluster Forgejo service (checkout, job protocol — same
// toEndpoints reasoning as workerEgressSpec, not toFQDNs), the Go module
// proxy and checksum database, the host actions/checkout resolves against,
// and the read-only build cache (its own policy refuses writes). No
// Kubernetes API, no other namespace, no LiteLLM, no gateway.
// A build job additionally reaches its own VM's buildkitd and, since
// BuildKit fetches registry tokens through the client session, Docker Hub's
// token service; layers and release downloads are the VM's.
func (s *CISpawner) ciEgressSpec(vm *BuildVM) map[string]any {
	u, _ := url.Parse(s.cfg.GitBaseURL) // validated in NewCISpawner
	fqdns := []string{
		"proxy.golang.org", "sum.golang.org", // go module proxy + checksum db
		"storage.googleapis.com", // proxy.golang.org redirects zip downloads there
		"data.forgejo.org",       // DEFAULT_ACTIONS_URL: where actions/checkout resolves from
	}
	if vm != nil {
		fqdns = append(fqdns, "auth.docker.io") // buildctl's session fetches the build VM's registry tokens
	}
	dnsNames := make([]any, 0, len(fqdns)+1)
	for _, host := range fqdns {
		dnsNames = append(dnsNames, map[string]any{"matchName": host})
	}
	dnsNames = append(dnsNames, map[string]any{"matchName": u.Hostname()}, map[string]any{"matchName": buildCacheHost})

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
			"toEndpoints": []any{map[string]any{
				"matchLabels": map[string]string{
					"k8s:io.kubernetes.pod.namespace": "forgejo",
					"k8s:app.kubernetes.io/name":      "build-cache",
				},
			}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "5000", "protocol": "TCP"}},
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
	if vm != nil {
		egress = append(egress, buildVMEgressRule(vm.IPv4))
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
