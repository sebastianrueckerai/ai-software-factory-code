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

// RunnerConfig carries the per-cluster sandbox facts and the task policy the
// workflow runs under. Defaults are set from the environment in
// cmd/factoryd; tests set them explicitly.
type RunnerConfig struct {
	AgentNamespacePrefix string // "factory-agents-"
	WorkerImage          string // pinned digest
	PullSecret           string // registry pull secret copied into task namespaces
	FactoryNamespace     string // where the pull secret lives
	GatewayURL           string // http://factory-gateway.factory.svc.cluster.local:8081
	GitBaseURL           string // http://forgejo-http.forgejo.svc.cluster.local:3000 — sandbox clones; the public host's in-cluster DNS answer is the gateway ClusterIP, which FQDN policy cannot authorize
	Models               ModelLadder
	AnswerTokens         int
	HeadroomTokens       int
	MaxSessions          int
	ChecksTimeout        time.Duration // how long WaitChecks waits for the PR head's combined status
	ChecksPoll           time.Duration
	Supervisor           SupervisorConfig
}

// Runner owns the sandbox side of a task: per-session namespace,
// credentials, worker pod, supervision and session records. Its exported
// methods are the Temporal activities TaskWorkflow calls.
type Runner struct {
	cfg        RunnerConfig
	store      Store
	kube       Kube
	forgejo    *Forgejo
	completion *Completion
	objects    ObjectStore
	timeline   Timeline
	supervisor *SupervisorModel
	stopWait   time.Duration
}

func NewRunner(cfg RunnerConfig, store Store, kube Kube, forgejo *Forgejo, completion *Completion, objects ObjectStore, supervisor *SupervisorModel) (*Runner, error) {
	if supervisor == nil {
		return nil, fmt.Errorf("runner: a supervisor model is required")
	}
	if cfg.AgentNamespacePrefix == "" || cfg.WorkerImage == "" || cfg.GatewayURL == "" || cfg.GitBaseURL == "" {
		return nil, fmt.Errorf("runner: AgentNamespacePrefix, WorkerImage, GatewayURL, GitBaseURL are required")
	}
	if cfg.Models.Start == "" || cfg.Models.Stronger == "" {
		return nil, fmt.Errorf("runner: both model ladder rungs are required")
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
	if cfg.ChecksTimeout <= 0 {
		cfg.ChecksTimeout = 45 * time.Minute
	}
	if cfg.ChecksPoll <= 0 {
		cfg.ChecksPoll = 30 * time.Second
	}
	return &Runner{cfg: cfg, store: store, kube: kube, forgejo: forgejo, completion: completion, supervisor: supervisor,
		objects: objects, timeline: Timeline{Objects: objects}, stopWait: (workerTerminationGrace + 5) * time.Second}, nil
}

// WorkflowConfig is the task policy TaskWorkflow captures at start.
func (r *Runner) WorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		Models:        r.cfg.Models,
		MaxSessions:   r.cfg.MaxSessions,
		WallClockCap:  r.cfg.Supervisor.WallClockCap,
		ChecksTimeout: r.cfg.ChecksTimeout,
	}
}

// BootSweep deletes agent namespaces no session can still be using: those of
// tasks that are neither queued nor running, and of unknown tasks. A running
// task's namespace belongs to a RunSession attempt Temporal will retry, and
// the retry replaces it itself. CI namespaces (label factory.ai/task
// ci-<job>, see jobLabel) belong to the CI spawner: their PR check jobs run
// on across a factoryd restart.
func (r *Runner) BootSweep(ctx context.Context) {
	live, err := r.kube.NamespacesByLabel(ctx, "factory.ai/task", "")
	if err != nil {
		log.Printf("boot sweep: list agent namespaces: %v", err)
		return
	}
	for _, ns := range live {
		id := strings.TrimPrefix(ns, r.cfg.AgentNamespacePrefix)
		if strings.HasPrefix(id, "ci-") {
			continue
		}
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

// sessionSpec is what one worker pod runs: its identity, the chain it
// continues, and where it starts.
type sessionSpec struct {
	id          string
	parent      string
	findings    []string
	model       string
	startRef    string
	halfWayDone bool
}

// sessionOutcome is how one sandbox session ended: a clean adapter verdict,
// a trip (with the supervisor's decision unless it was the wall-clock
// cap), or the pod dying without either.
type sessionOutcome struct {
	result      *harness.Result
	commit      string
	trip        *Trip
	decision    *Decision
	digest      string
	noResult    string
	findings    []string
	halfWayDone bool
}

// workerTerminationGrace is how long a stopped worker gets to commit and
// push its work in progress before the kubelet kills it; runSession reads
// its log five seconds longer.
const workerTerminationGrace = 60

// runSession runs one worker pod in the task namespace until it yields a
// result, hits the wall-clock cap (deadline), or dies. Every other trip
// asks the supervisor model; continue lets the session run on, any other
// decision stops it. A stopped worker commits and pushes its work in
// progress before it exits. progress receives the event count and
// findings so far, for the activity heartbeat.
func (r *Runner) runSession(ctx context.Context, t Task, ns string, s sessionSpec, deadline time.Time, progress func(events int, findings []string)) (sessionOutcome, error) {
	out := sessionOutcome{}
	recorder, err := NewSessionRecorder(ctx, r.store, r.objects, t.ID, s.id, s.parent)
	if err != nil {
		return out, err
	}
	sup, err := NewSupervisor(r.cfg.Supervisor, deadline.Add(-r.cfg.Supervisor.WallClockCap), time.Now())
	if err != nil {
		return out, err
	}
	sup.halfWayDone = s.halfWayDone
	defer func() { out.findings, out.halfWayDone = sup.Findings(), sup.HalfWayDone() }()
	digest := newDigestBuilder(time.Now())
	supervision := &sessionSupervision{max: sup.cfg.MaxCalls, decide: func(ctx context.Context, trip *Trip, call int) Decision {
		return r.askSupervisor(ctx, t, s, recorder, digest, trip, call, sup.cfg.MaxCalls)
	}}
	podName := "worker-" + s.id
	pod := r.workerPodSpec(t, s)
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
	// image pull can take minutes, but never past the session deadline.
	wait := min(10*time.Minute, time.Until(deadline))
	if err := r.awaitPodStart(ctx, ns, podName, wait); err != nil {
		out.noResult = err.Error()
		return out, nil
	}
	progress(0, nil)

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

	// read takes one log line: the result ends the session; an event is
	// recorded and returned for supervision.
	events := 0
	read := func(line []byte) (ev *harness.Event, done bool) {
		var env harness.WorkerEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			// the worker's stderr log shares the stream with its envelopes
			log.Printf("task %s: worker: %s", t.ID, line)
			return nil, false
		}
		if env.Result != nil {
			res := *env.Result
			out.result = &res
			return nil, true
		}
		if env.Event == nil {
			return nil, false
		}
		if env.Event.Kind == harness.EventSupervisor {
			log.Printf("task %s: dropped a supervisor event from the worker", t.ID)
			return nil, false
		}
		if err := recorder.Record(ctx, *env.Event); err != nil {
			log.Printf("task %s: record event: %v", t.ID, err)
		}
		if env.Event.Kind == harness.EventPushed {
			out.commit, _ = env.Event.Detail["commit"].(string)
		}
		events++
		return env.Event, false
	}
	// stop asks the worker to end (SIGTERM through the pod's deletion) and
	// reads on until it has pushed its work in progress and reported, or
	// its grace period is over.
	stop := func(trip *Trip) {
		out.trip = r.abandon(recorder, trip)
		if err := r.kube.DeletePod(ctx, ns, podName); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("task %s: stop pod %s: %v", t.ID, podName, err)
		}
		timeout := time.After(r.stopWait)
		for {
			select {
			case <-ctx.Done():
				return
			case <-timeout:
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				if _, done := read(line); done {
					return
				}
			}
		}
	}
	// onTrip returns true when the session stops.
	onTrip := func(trip *Trip) bool {
		if trip.Kind == TripWallClock {
			stop(trip)
			return true
		}
		d, called := supervision.onTrip(ctx, trip)
		out.digest = digest.digest().Summary()
		r.recordDecision(ctx, t, s, recorder, trip, d, called, out.digest)
		if d.Action == ActionContinue {
			sup.Resume(time.Now())
			return false
		}
		out.decision = &d
		stop(trip)
		return true
	}

	tick := time.NewTicker(r.supervisionTick())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-tick.C:
			if cur, ok, err := r.store.Task(ctx, t.ID); err == nil && ok {
				sup.SetSpend(cur.SpentEUR, cur.BudgetEUR)
			}
			if trip := sup.Tick(time.Now()); trip != nil && onTrip(trip) {
				return out, ctx.Err()
			}
		case line, ok := <-lines:
			if !ok {
				if out.result == nil {
					out.noResult = "pod exited without a result"
				}
				return out, nil
			}
			ev, done := read(line)
			if done {
				return out, nil
			}
			if ev == nil {
				continue
			}
			now := time.Now()
			digest.add(*ev, now)
			trip := sup.Event(*ev, now)
			progress(events, sup.Findings())
			if trip != nil && onTrip(trip) {
				return out, ctx.Err()
			}
		}
	}
}

// askSupervisor builds the supervisor's input and calls it with the task's
// remaining budget as the key's cap.
func (r *Runner) askSupervisor(ctx context.Context, t Task, s sessionSpec, recorder *SessionRecorder, digest *digestBuilder, trip *Trip, call, maxCalls int) Decision {
	cur, ok, err := r.store.Task(ctx, t.ID)
	if err != nil || !ok {
		return Decision{Action: ActionEscalate, Reason: fmt.Sprintf("supervisor not called: task unreadable (%v)", err)}
	}
	return r.supervisor.Decide(ctx, t.ID, cur.BudgetEUR-cur.SpentEUR, SupervisorInput{
		Brief:   harness.TaskBrief{Objective: t.Objective, Repository: t.Repository, SHA: s.startRef, Findings: s.findings},
		Trip:    TripReport{Detector: trip.Kind, Detail: trip.Detail},
		Digest:  digest.digest(),
		Record:  recorder.Tail(maxRecordTail),
		Model:   s.model,
		TopRung: s.model == r.cfg.Models.Stronger,
		Calls:   call,
		MaxCall: maxCalls,
	})
}

// recordDecision writes a supervisor decision into the session record, the
// timeline and, except for an escalation (the workflow's Escalate comment
// carries it), onto the issue.
func (r *Runner) recordDecision(ctx context.Context, t Task, s sessionSpec, recorder *SessionRecorder, trip *Trip, d Decision, called bool, digest string) {
	detail := map[string]any{"detector": string(trip.Kind), "trip": trip.Detail, "digest": digest,
		"decision": string(d.Action), "guidance": d.Guidance, "reason": d.Reason, "called": called}
	if err := recorder.Record(ctx, harness.Event{Time: time.Now(), Kind: harness.EventSupervisor, Detail: detail}); err != nil {
		log.Printf("task %s: record supervisor decision: %v", t.ID, err)
	}
	detail["session"] = s.id
	r.record(ctx, t.ID, "supervisor", detail)
	if d.Action == ActionEscalate || t.IssueNumber == 0 {
		return
	}
	repo, err := SplitRepo(t.Repository)
	if err != nil {
		return
	}
	body := fmt.Sprintf("factoryd supervisor, session %s: **%s** after the %s detector tripped.\n\nDetector: %s\nProgress: %s",
		s.id, d.Action, trip.Kind, trip.Detail, digest)
	if d.Guidance != "" {
		body += "\nGuidance: " + d.Guidance
	}
	if d.Reason != "" {
		body += "\nReason: " + d.Reason
	}
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, body); err != nil {
		log.Printf("task %s: comment supervisor decision: %v", t.ID, err)
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
			return copyPullSecret(ctx, r.kube, r.cfg.FactoryNamespace, r.cfg.PullSecret, ns)
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

// copyPullSecret copies the factory namespace's registry pull secret into a
// task or CI namespace. Shared by the agent Runner and the CI spawner: both
// hand ephemeral pods a way to pull their own image, and nothing else.
func copyPullSecret(ctx context.Context, kube Kube, factoryNamespace, pullSecret, ns string) error {
	if pullSecret == "" {
		return nil
	}
	data, err := kube.GetSecretData(ctx, factoryNamespace, pullSecret)
	if err != nil {
		return err
	}
	// kubelet only honors a pull secret of the dockerconfigjson type
	return kube.CreateSecret(ctx, ns, pullSecret, "kubernetes.io/dockerconfigjson", data)
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

func (r *Runner) workerPodSpec(t Task, s sessionSpec) map[string]any {
	findingsJSON, _ := json.Marshal(s.findings)
	env := []map[string]any{
		{"name": "TASK_ID", "value": t.ID},
		{"name": "SESSION_ID", "value": s.id},
		{"name": "OBJECTIVE", "value": t.Objective},
		{"name": "REPOSITORY", "value": t.Repository},
		{"name": "REPO_URL", "value": r.cloneURL(t)},
		{"name": "REPO_SHA", "value": s.startRef},
		{"name": "BRANCH", "value": r.branch(t)},
		{"name": "GIT_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": "task-git", "key": "GIT_TOKEN"}}},
		{"name": "GATEWAY_URL", "value": r.cfg.GatewayURL},
		{"name": "TOKEN_FILE", "value": "/var/run/secrets/tokens/gateway"},
		{"name": "MODEL", "value": s.model},
		{"name": "ANSWER_TOKENS", "value": strconv.Itoa(r.cfg.AnswerTokens)},
		{"name": "HEADROOM_TOKENS", "value": strconv.Itoa(r.cfg.HeadroomTokens)},
		{"name": "FINDINGS", "value": string(findingsJSON)},
	}
	if s.parent != "" {
		env = append(env, map[string]any{"name": "PARENT_SESSION_ID", "value": s.parent})
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
			"terminationGracePeriodSeconds": workerTerminationGrace,
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

func SplitRepo(repository string) (Repo, error) {
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
