package factoryd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// RunnerConfig carries the per-cluster sandbox facts. Defaults are set in
// newRunnerConfig-from-env (cmd/factoryd); tests set them explicitly.
type RunnerConfig struct {
	AgentNamespacePrefix string // "factory-agents-"
	WorkerImage          string // pinned digest
	PullSecret           string // registry pull secret copied into task namespaces
	FactoryNamespace     string // where the pull secret lives
	GatewayURL           string // http://factory-gateway.factory.svc.cluster.local:8081
	GitBaseURL           string // http://forgejo-http.forgejo.svc.cluster.local:3000 — sandbox clones; the public host's in-cluster DNS answer is the gateway ClusterIP, which FQDN policy cannot authorize
	Model                string
	RetryModel           string
	AnswerTokens         int
	HeadroomTokens       int
	MaxSessions          int
	PollInterval         time.Duration
	Supervisor           SupervisorConfig
}

// Runner owns the task lifecycle from queued to done: per-task namespace,
// credentials, sandbox pods, supervision, session records, and completion.
// One task runs at a time — the factory's throughput target is one box.
type Runner struct {
	cfg        RunnerConfig
	store      Store
	kube       Kube
	forgejo    *Forgejo
	completion *Completion
	objects    ObjectStore
}

func NewRunner(cfg RunnerConfig, store Store, kube Kube, forgejo *Forgejo, completion *Completion, objects ObjectStore) (*Runner, error) {
	if cfg.AgentNamespacePrefix == "" || cfg.WorkerImage == "" || cfg.GatewayURL == "" || cfg.GitBaseURL == "" {
		return nil, fmt.Errorf("runner: AgentNamespacePrefix, WorkerImage, GatewayURL, GitBaseURL are required")
	}
	for _, raw := range []string{cfg.GatewayURL, cfg.GitBaseURL} {
		if u, err := url.Parse(raw); err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("runner: %q is not an absolute URL", raw)
		}
	}
	if _, err := (TokenBudget{AnswerTokens: cfg.AnswerTokens, HeadroomTokens: cfg.HeadroomTokens}).OutputLimit(); err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	if err := cfg.Supervisor.validate(); err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 3
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Runner{cfg: cfg, store: store, kube: kube, forgejo: forgejo, completion: completion, objects: objects}, nil
}

// Run polls for queued tasks and executes them serially until ctx ends. A
// task in flight when ctx ends is handed back to the queue.
func (r *Runner) Run(ctx context.Context) {
	tick := time.NewTicker(r.cfg.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			queued, err := r.store.ListTasksByStatus(ctx, "queued")
			if err != nil {
				log.Printf("runner: list queued: %v", err)
				continue
			}
			for _, t := range queued {
				if ctx.Err() != nil {
					return
				}
				r.execute(ctx, t)
			}
		}
	}
}

// BootSweep clears the residue of a crashed factoryd: interrupted tasks are
// failed (their namespaces die, their credentials get revoked), and
// namespaces whose task no longer needs one are deleted.
func (r *Runner) BootSweep(ctx context.Context) {
	// A crash leaves tasks running (interrupted) or closing (cleanup was
	// already underway and failed); Complete is idempotent, so both are
	// finished the same way.
	for _, status := range []string{"running", "closing"} {
		stuck, err := r.store.ListTasksByStatus(ctx, status)
		if err != nil {
			log.Printf("boot sweep: list %s: %v", status, err)
		}
		for _, t := range stuck {
			r.forgetNamespace(ctx, t.ID)
			if err := r.completion.Complete(ctx, t.ID, "failed"); err != nil {
				log.Printf("boot sweep: task %s: %v", t.ID, err)
			} else {
				log.Printf("boot sweep: task %s failed (interrupted run)", t.ID)
			}
		}
	}
	live, err := r.kube.NamespacesByLabel(ctx, "factory.ai/task", "")
	if err != nil {
		log.Printf("boot sweep: list agent namespaces: %v", err)
		return
	}
	for _, ns := range live {
		id := strings.TrimPrefix(ns, r.cfg.AgentNamespacePrefix)
		t, ok, err := r.store.Task(ctx, id)
		if err != nil {
			log.Printf("boot sweep: task %s: %v", id, err)
			continue
		}
		if ok && (t.Status == "queued" || t.Status == "running") {
			continue
		}
		r.forgetNamespace(ctx, id)
	}
}

func (r *Runner) execute(ctx context.Context, t Task) {
	elapsed, claimed, err := r.store.ClaimTask(ctx, t.ID)
	if err != nil {
		log.Printf("task %s: claim: %v", t.ID, err)
		return
	}
	if !claimed {
		return
	}
	// the store measured the elapsed time on its own clock; every deadline
	// from here on runs on this process's monotonic clock
	started := time.Now().Add(-elapsed)
	final, reason := r.runTask(ctx, t, started)

	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if final == "" {
		log.Printf("task %s: shutdown mid-run, requeued (%s)", t.ID, reason)
		if err := r.completion.Requeue(cleanup, t.ID); err != nil {
			log.Printf("task %s: requeue stuck in closing: %v", t.ID, err)
		}
		return
	}
	log.Printf("task %s: %s (%s)", t.ID, final, reason)
	if err := r.completion.Complete(cleanup, t.ID, final); err != nil {
		log.Printf("task %s: completion stuck in closing: %v", t.ID, err)
	}
}

// runTask provisions the task namespace and runs sessions until one yields
// a verdict, the task's wall-clock cap (anchored to the task's first start)
// passes, or the attempts run out. final is empty when shutdown cut the run
// short before any verdict: the task goes back to the queue. A verdict
// reached before shutdown stands.
func (r *Runner) runTask(ctx context.Context, t Task, started time.Time) (final, reason string) {
	verdict := false
	defer func() {
		if !verdict && ctx.Err() != nil {
			final = ""
		}
	}()
	repo, err := splitRepo(t.Repository)
	if err != nil {
		return "failed", err.Error()
	}
	// registered first: a half-built namespace must not outlive the task
	defer r.forgetNamespace(context.WithoutCancel(ctx), t.ID)
	tok, err := r.forgejo.MintToken(ctx, TokenName(t.ID), []Repo{repo})
	if err != nil {
		return "failed", err.Error()
	}
	ns := r.agentNamespace(t.ID)
	if err := r.setupNamespace(ctx, t, tok); err != nil {
		return "failed", err.Error()
	}

	findings := []string{}
	parent := ""
	model := r.cfg.Model
	final, reason = "failed", "no session produced a result"
	for attempt := 0; attempt < r.cfg.MaxSessions && ctx.Err() == nil; attempt++ {
		if time.Since(started) >= r.cfg.Supervisor.WallClockCap {
			verdict = true
			return "failed", "supervision trip: " + string(TripWallClock)
		}
		sessionID := newID("s-")
		out, err := r.runSession(ctx, t, ns, sessionID, parent, findings, model, started)
		if err != nil {
			reason = err.Error()
			continue
		}
		if out.result != nil {
			verdict = true
			final, reason = "failed", out.result.Summary
			if out.result.Success {
				final = "done"
			}
			return final, reason
		}
		if out.trip != nil {
			reason = "supervision trip: " + string(out.trip.Kind)
			if out.trip.Kind == TripWallClock {
				verdict = true
				return "failed", reason
			}
			parent = sessionID
			findings = out.trip.Findings
			if out.trip.RetryStrongerModel {
				model = r.cfg.RetryModel
			}
			continue
		}
		if out.noResult != "" {
			parent = sessionID
			findings = append(append([]string(nil), findings...), out.noResult)
			reason = out.noResult
		}
	}
	return final, reason
}

// sessionOutcome is how one sandbox session ended: a clean adapter verdict,
// a supervision trip (resume), or the pod dying without either (retry).
type sessionOutcome struct {
	result   *harness.Result
	trip     *Trip
	noResult string
}

func (r *Runner) runSession(ctx context.Context, t Task, ns, sessionID, parent string, findings []string, model string, taskStarted time.Time) (sessionOutcome, error) {
	out := sessionOutcome{}
	recorder, err := NewSessionRecorder(ctx, r.store, r.objects, t.ID, sessionID, parent)
	if err != nil {
		return out, err
	}
	sup, err := NewSupervisor(r.cfg.Supervisor, taskStarted, time.Now())
	if err != nil {
		return out, err
	}
	podName := "worker-" + sessionID
	pod := r.workerPodSpec(t, sessionID, parent, findings, model)
	if err := r.kube.CreatePod(ctx, ns, podName, pod); err != nil {
		return out, fmt.Errorf("create pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.kube.DeletePod(delCtx, ns, podName); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("task %s: delete pod %s: %v", t.ID, podName, err)
		}
	}()

	// The pod must leave Pending before its log endpoint is useful; a first
	// image pull can take minutes, but never past the task's wall-clock cap.
	wait := min(10*time.Minute, time.Until(taskStarted.Add(r.cfg.Supervisor.WallClockCap)))
	if err := r.awaitPodStart(ctx, ns, podName, wait); err != nil {
		out.noResult = err.Error()
		return out, nil
	}

	sessCtx, cancelLog := context.WithCancel(ctx)
	defer cancelLog()
	stream, err := r.kube.PodLog(sessCtx, ns, podName)
	if err != nil {
		out.noResult = "open pod log: " + err.Error()
		return out, nil
	}
	defer stream.Close()

	lines := make(chan []byte)
	doneReading := make(chan struct{})
	defer close(doneReading)
	go func() {
		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Bytes():
			case <-doneReading:
				return
			}
		}
		close(lines)
	}()

	tick := time.NewTicker(r.supervisionTick())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-tick.C:
			if trip := sup.Tick(time.Now()); trip != nil {
				out.trip = r.abandon(recorder, trip)
				return out, nil
			}
		case line, ok := <-lines:
			if !ok {
				if out.result == nil {
					out.noResult = "pod exited without a result"
				}
				return out, nil
			}
			var env harness.WorkerEnvelope
			if err := json.Unmarshal(line, &env); err != nil {
				// the worker's stderr log shares the stream with its envelopes
				log.Printf("task %s: worker: %s", t.ID, line)
				continue
			}
			if env.Result != nil {
				res := *env.Result
				out.result = &res
				return out, nil
			}
			if env.Event != nil {
				if err := recorder.Record(ctx, *env.Event); err != nil {
					log.Printf("task %s: record event: %v", t.ID, err)
				}
				if trip := sup.Event(*env.Event, time.Now()); trip != nil {
					out.trip = r.abandon(recorder, trip)
					return out, nil
				}
			}
		}
	}
}

// abandon snapshots the trip so the next session continues from it.
func (r *Runner) abandon(recorder *SessionRecorder, trip *Trip) *Trip {
	restart, _ := json.Marshal(map[string]string{"resume": "findings", "trip": string(trip.Kind)})
	if err := recorder.Snapshot(context.Background(), trip.Findings, restart); err != nil {
		log.Printf("snapshot trip: %v", err)
	}
	return trip
}

func (r *Runner) awaitPodStart(ctx context.Context, ns, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		phase, _, err := r.kube.PodStatus(ctx, ns, podName)
		if err == nil && phase != "" && phase != "Pending" {
			return nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("pod %s still pending after %s", podName, timeout)
}

func (r *Runner) setupNamespace(ctx context.Context, t Task, tok ForgejoToken) error {
	ns := r.agentNamespace(t.ID)
	if err := r.freshNamespace(ctx, ns, map[string]string{
		"factory.ai/task":                    t.ID,
		"pod-security.kubernetes.io/enforce": "restricted",
	}); err != nil {
		return fmt.Errorf("setup namespace: %w", err)
	}
	core := "/api/v1/namespaces/" + ns
	steps := []struct {
		name string
		do   func() error
	}{
		// factoryd holds no cluster-wide pod or secret rights; this binding
		// grants them inside the task namespace only (k8s/factoryd.yaml)
		{"role binding", func() error {
			return r.kube.Create(ctx, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+ns+"/rolebindings", map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
				"metadata": map[string]any{"name": "factoryd"},
				"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "factoryd-task"},
				"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": r.cfg.FactoryNamespace}},
			})
		}},
		{"resource quota", func() error {
			return r.kube.Create(ctx, core+"/resourcequotas", map[string]any{
				"apiVersion": "v1", "kind": "ResourceQuota",
				"metadata": map[string]any{"name": "sandbox"},
				"spec":     map[string]any{"hard": sandboxQuota},
			})
		}},
		{"limit range", func() error {
			return r.kube.Create(ctx, core+"/limitranges", map[string]any{
				"apiVersion": "v1", "kind": "LimitRange",
				"metadata": map[string]any{"name": "sandbox"},
				"spec": map[string]any{"limits": []any{map[string]any{
					"type":           "Container",
					"default":        sandboxLimits,
					"defaultRequest": sandboxRequests,
					"max":            sandboxLimits,
				}}},
			})
		}},
		{"service account", func() error {
			return r.kube.CreateServiceAccount(ctx, ns, "task-"+t.ID)
		}},
		{"egress policy", func() error {
			return r.kube.CreateCiliumNetworkPolicy(ctx, ns, "worker-egress", r.workerEgressSpec())
		}},
		{"git token secret", func() error {
			return r.kube.CreateSecret(ctx, ns, "task-git", "", map[string]string{"GIT_TOKEN": tok.Secret})
		}},
		{"pull secret", func() error {
			return r.copyPullSecret(ctx, ns)
		}},
	}
	for _, step := range steps {
		if err := retryForbidden(ctx, step.do); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	return nil
}

// freshNamespace creates the task namespace, deleting a leftover one first:
// a requeued task's old namespace may still be terminating.
func (r *Runner) freshNamespace(ctx context.Context, ns string, labels map[string]string) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		err := r.kube.CreateNamespace(ctx, ns, labels)
		if !errors.Is(err, ErrAlreadyExists) {
			return err
		}
		if err := r.kube.DeleteNamespace(ctx, ns); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s still terminating", ns)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// retryForbidden rides out the gap between creating the task RoleBinding
// and the authorizer seeing it.
func retryForbidden(ctx context.Context, do func() error) error {
	for i := 0; ; i++ {
		err := do()
		if !errors.Is(err, ErrForbidden) || i == 10 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Sandbox resources: the worker container's requests and limits, and the
// namespace ceiling (two pods: a finished session pod can still be
// terminating when the resumed one starts).
var (
	sandboxRequests = map[string]any{"cpu": "250m", "memory": "512Mi"}
	sandboxLimits   = map[string]any{"cpu": "2", "memory": "2Gi"}
	sandboxQuota    = map[string]any{
		"pods":            "2",
		"requests.cpu":    "1",
		"requests.memory": "2Gi",
		"limits.cpu":      "4",
		"limits.memory":   "4Gi",
	}
)

func (r *Runner) copyPullSecret(ctx context.Context, ns string) error {
	if r.cfg.PullSecret == "" {
		return nil
	}
	data, err := r.kube.GetSecretData(ctx, r.cfg.FactoryNamespace, r.cfg.PullSecret)
	if err != nil {
		return err
	}
	// kubelet only honors a pull secret of the dockerconfigjson type
	return r.kube.CreateSecret(ctx, ns, r.cfg.PullSecret, "kubernetes.io/dockerconfigjson", data)
}

func (r *Runner) forgetNamespace(ctx context.Context, taskID string) {
	if err := r.kube.DeleteNamespace(ctx, r.agentNamespace(taskID)); err != nil && !errors.Is(err, ErrNotFound) {
		log.Printf("task %s: delete namespace: %v", taskID, err)
	}
}

func (r *Runner) agentNamespace(taskID string) string {
	return r.cfg.AgentNamespacePrefix + taskID
}

func (r *Runner) branch(t Task) string {
	if t.Branch != "" {
		return t.Branch
	}
	return "factory/" + t.ID
}

func (r *Runner) cloneURL(t Task) string {
	base := strings.TrimSuffix(r.cfg.GitBaseURL, "/")
	return base + "/" + t.Repository + ".git"
}

func (r *Runner) supervisionTick() time.Duration {
	t := r.cfg.Supervisor.NoProgressAfter / 3
	if t < time.Second {
		t = time.Second
	}
	if t > 15*time.Second {
		t = 15 * time.Second
	}
	return t
}

func (r *Runner) workerPodSpec(t Task, sessionID, parent string, findings []string, model string) map[string]any {
	findingsJSON, _ := json.Marshal(findings)
	env := []map[string]any{
		{"name": "TASK_ID", "value": t.ID},
		{"name": "SESSION_ID", "value": sessionID},
		{"name": "OBJECTIVE", "value": t.Objective},
		{"name": "REPOSITORY", "value": t.Repository},
		{"name": "REPO_URL", "value": r.cloneURL(t)},
		{"name": "REPO_SHA", "value": t.SHA},
		{"name": "BRANCH", "value": r.branch(t)},
		{"name": "GIT_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": "task-git", "key": "GIT_TOKEN"}}},
		{"name": "GATEWAY_URL", "value": r.cfg.GatewayURL},
		{"name": "TOKEN_FILE", "value": "/var/run/secrets/tokens/gateway"},
		{"name": "MODEL", "value": model},
		{"name": "ANSWER_TOKENS", "value": strconv.Itoa(r.cfg.AnswerTokens)},
		{"name": "HEADROOM_TOKENS", "value": strconv.Itoa(r.cfg.HeadroomTokens)},
		{"name": "FINDINGS", "value": string(findingsJSON)},
	}
	if parent != "" {
		env = append(env, map[string]any{"name": "PARENT_SESSION_ID", "value": parent})
	}
	pullSecrets := []any{}
	if r.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": r.cfg.PullSecret})
	}
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]string{
				"app.kubernetes.io/name": "opencode-worker",
				"factory.ai/task":        t.ID,
			},
		},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"runtimeClassName":             "gvisor",
			"serviceAccountName":           "task-" + t.ID,
			"automountServiceAccountToken": false,
			"securityContext": map[string]any{
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"imagePullSecrets":              pullSecrets,
			"terminationGracePeriodSeconds": 30,
			// FQDN service names resolve on the first query; search-path
			// expansions would be refused by the DNS allowlist
			"dnsConfig": map[string]any{"options": []any{map[string]any{"name": "ndots", "value": "1"}}},
			"containers": []any{map[string]any{
				"name":    "worker",
				"image":   r.cfg.WorkerImage,
				"command": []any{"/usr/local/bin/agent-worker"},
				"env":     env,
				"resources": map[string]any{
					"requests": sandboxRequests,
					"limits":   sandboxLimits,
				},
				"volumeMounts": []any{
					map[string]any{"name": "workspace", "mountPath": "/workspace"},
					map[string]any{"name": "home", "mountPath": "/home/worker"},
					map[string]any{"name": "gateway-token", "mountPath": "/var/run/secrets/tokens", "readOnly": true},
				},
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
				map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "4Gi"}},
				map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
				map[string]any{"name": "gateway-token", "projected": map[string]any{
					"sources": []any{map[string]any{
						"serviceAccountToken": map[string]any{
							"path":              "gateway",
							"audience":          "factory-gateway",
							"expirationSeconds": 3600,
						},
					}},
				}},
			},
		},
	}
}

// workerEgressSpec is the sandbox network policy (architecture 6.4, research
// Q4): default-deny egress for workers, a hard deny to the cloud metadata
// address, and exactly three allows: the model gateway's port, Forgejo's
// HTTP port, and kube-dns for the two service names behind them. DNS is
// allowlisted by name because an open resolver is itself an exfiltration
// channel. No package registry is reachable: the npm provider is pre-baked
// into the image and the image carries no language toolchain. Workers reach
// Forgejo through its in-cluster service, not the public host: in-cluster
// DNS answers that host with the gateway ClusterIP, which Cilium's toFQDNs
// cannot authorize (the envoy fronting it answers 403), while the service
// path is identity-scoped. The live gateway pods carry the plain `app`
// label, not app.kubernetes.io/name.
func (r *Runner) workerEgressSpec() map[string]any {
	dnsNames := []any{}
	for _, raw := range []string{r.cfg.GatewayURL, r.cfg.GitBaseURL} {
		u, _ := url.Parse(raw) // validated in NewRunner
		dnsNames = append(dnsNames, map[string]any{"matchName": u.Hostname()})
	}
	return map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": "opencode-worker"},
		},
		"egressDeny": []any{
			map[string]any{"toCIDR": []string{"169.254.169.254/32"}},
		},
		"egress": []any{
			map[string]any{
				"toEndpoints": []any{map[string]any{
					"matchLabels": map[string]string{
						"k8s:io.kubernetes.pod.namespace": "factory",
						"app":                             "factory-gateway",
					},
				}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "8081", "protocol": "TCP"}},
				}},
			},
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

func splitRepo(repository string) (Repo, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("repository %q is not owner/name", repository)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// NewTaskID mints a task id for the HTTP intake.
func NewTaskID() string { return newID("t-") }

func newID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not survivable
	}
	return prefix + hex.EncodeToString(b)
}

// Namespace reads the pod's own namespace; the runner needs it to find the
// pull secret. Empty outside the cluster.
func (k *InClusterKube) Namespace() string {
	b, err := os.ReadFile(filepath.Join(saDir, "namespace"))
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}
