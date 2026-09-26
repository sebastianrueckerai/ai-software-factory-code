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
	GatewayURL           string // http://factory-gateway.factory.svc:8081
	GitBaseURL           string // http://forgejo-http.forgejo.svc:3000 — sandbox clones; the public host's in-cluster DNS answer is the gateway ClusterIP, which FQDN policy cannot authorize
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

// Run polls for queued tasks and executes them serially until ctx ends.
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
	if err := r.store.SetTaskStatus(ctx, t.ID, "running"); err != nil {
		log.Printf("task %s: mark running: %v", t.ID, err)
		return
	}
	repo, err := splitRepo(t.Repository)
	if err != nil {
		r.fail(ctx, t.ID, err)
		return
	}
	tok, err := r.forgejo.MintToken(ctx, TokenName(t.ID), []Repo{repo})
	if err != nil {
		r.fail(ctx, t.ID, err)
		return
	}
	ns := r.agentNamespace(t.ID)
	if err := r.setupNamespace(ctx, t, tok); err != nil {
		r.fail(ctx, t.ID, err)
		return
	}
	defer r.forgetNamespace(context.Background(), t.ID)

	findings := []string{}
	parent := ""
	model := r.cfg.Model
	final, reason := "failed", "no session produced a result"
	for attempt := 0; attempt < r.cfg.MaxSessions; attempt++ {
		sessionID := newID("s-")
		out, err := r.runSession(ctx, t, ns, sessionID, parent, findings, model)
		if err != nil {
			reason = err.Error()
			continue
		}
		if out.result != nil {
			final, reason = "failed", out.result.Summary
			if out.result.Success {
				final = "done"
			}
			break
		}
		if out.trip != nil {
			parent = sessionID
			findings = out.trip.Findings
			if out.trip.RetryStrongerModel {
				model = r.cfg.RetryModel
			}
			reason = "supervision trip: " + string(out.trip.Kind)
			continue
		}
		if out.noResult != "" {
			parent = sessionID
			findings = append(append([]string(nil), findings...), out.noResult)
			reason = out.noResult
			continue
		}
	}
	log.Printf("task %s: %s (%s)", t.ID, final, reason)
	if err := r.completion.Complete(ctx, t.ID, final); err != nil {
		log.Printf("task %s: completion stuck in closing: %v", t.ID, err)
	}
}

func (r *Runner) fail(ctx context.Context, taskID string, cause error) {
	log.Printf("task %s: %v", taskID, cause)
	if err := r.completion.Complete(ctx, taskID, "failed"); err != nil {
		log.Printf("task %s: completion stuck in closing: %v", taskID, err)
	}
}

// sessionOutcome is how one sandbox session ended: a clean adapter verdict,
// a supervision trip (resume), or the pod dying without either (retry).
type sessionOutcome struct {
	result   *harness.Result
	trip     *Trip
	noResult string
}

func (r *Runner) runSession(ctx context.Context, t Task, ns, sessionID, parent string, findings []string, model string) (sessionOutcome, error) {
	out := sessionOutcome{}
	recorder, err := NewSessionRecorder(ctx, r.store, r.objects, t.ID, sessionID, parent)
	if err != nil {
		return out, err
	}
	sup, err := NewSupervisor(r.cfg.Supervisor, time.Now())
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
	// image pull can take minutes.
	if err := r.awaitPodStart(ctx, ns, podName, 10*time.Minute); err != nil {
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
				log.Printf("task %s: unreadable worker line: %q", t.ID, string(line))
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
				if trip := sup.Event(*env.Event, env.Event.Time); trip != nil {
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
	steps := []struct {
		name string
		do   func() error
	}{
		{"namespace", func() error {
			return r.kube.CreateNamespace(ctx, ns, map[string]string{"factory.ai/task": t.ID})
		}},
		{"service account", func() error {
			return r.kube.CreateServiceAccount(ctx, ns, "task-"+t.ID)
		}},
		{"egress policy", func() error {
			return r.kube.CreateCiliumNetworkPolicy(ctx, ns, "worker-egress", workerEgressSpec())
		}},
		{"git token secret", func() error {
			return r.kube.CreateSecret(ctx, ns, "task-git", "", map[string]string{"GIT_TOKEN": tok.Secret})
		}},
		{"pull secret", func() error {
			return r.copyPullSecret(ctx, ns)
		}},
	}
	for _, step := range steps {
		if err := step.do(); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	return nil
}

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
			"restartPolicy":                 "Never",
			"runtimeClassName":              "gvisor",
			"serviceAccountName":            "task-" + t.ID,
			"automountServiceAccountToken":  false,
			"imagePullSecrets":              pullSecrets,
			"terminationGracePeriodSeconds": 30,
			"containers": []any{map[string]any{
				"name":    "worker",
				"image":   r.cfg.WorkerImage,
				"command": []any{"/usr/local/bin/agent-worker"},
				"env":     env,
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
				map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
				map[string]any{"name": "home", "emptyDir": map[string]any{}},
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
// address, and exactly four allows — the model gateway, Forgejo git, the Go
// module proxy/checksum hosts (the npm provider is pre-baked into the image,
// so no npm/pypi host is needed at runtime), and kube-dns so Cilium's DNS
// proxy can learn the toFQDNs identities. Workers reach Forgejo through its
// in-cluster service, not the public host: in-cluster DNS answers that host
// with the gateway ClusterIP, which Cilium's toFQDNs cannot authorize (the
// envoy fronting it answers 403), while the service path is
// identity-scoped. The live gateway pods carry the plain `app` label, not
// app.kubernetes.io/name.
func workerEgressSpec() map[string]any {
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
				"toFQDNs": []any{
					map[string]any{"matchName": "proxy.golang.org"},
					map[string]any{"matchName": "sum.golang.org"},
				},
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
					"rules": map[string]any{
						"dns": []any{map[string]any{"matchPattern": "*"}},
					},
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
