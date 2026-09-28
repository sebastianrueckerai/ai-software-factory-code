package factoryd

import (
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
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
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
	AnswerSlice          time.Duration // agent time a human answer guarantees the attempt it buys
	ChecksTimeout        time.Duration // how long WaitChecks waits for the PR head's combined status
	ChecksPoll           time.Duration
	Supervisor           SupervisorConfig
	// Policy is the policy version in force (GatePolicy's risk rules).
	Policy func() policy.Version
	// ReviewModel is the model review's alias (review-frontier).
	ReviewModel string
	// ReproTimeout bounds one reproduction command run.
	ReproTimeout time.Duration
	// Leases reads the commissioned servers each task holds; their IPv4s
	// join the task's sandbox egress on port 22 while the lease lives.
	Leases LeaseReader
}

// LeaseReader is the live leased servers' IPv4s by owning task
// (cloudres.AgentStore on the shared database).
type LeaseReader interface {
	LiveAddresses(ctx context.Context) (map[string][]string, error)
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
	timeline   *Timeline
	supervisor *SupervisorModel
	stopWait   time.Duration

	liveMu sync.Mutex
	live   map[string]*liveSession // by task: its running session

}

// liveSession takes the agent's escalations: ch is unbuffered, so a send
// completes only when the session reads it; done closes when the session
// takes no more.
type liveSession struct {
	ch   chan agentEscalation
	done chan struct{}
}

type agentEscalation struct{ question, reason string }

// ErrNoLiveSession: the task has no session running in this factoryd.
var ErrNoLiveSession = errors.New("the task has no running session")

// EscalateSession hands an agent's escalate_to_human to its task's running
// session, which stops the way a supervisor's escalate stops it. It returns
// nil only once the session took the escalation; a session that ends or
// stops first answers ErrNoLiveSession.
func (r *Runner) EscalateSession(ctx context.Context, taskID, question, reason string) error {
	r.liveMu.Lock()
	ls, ok := r.live[taskID]
	r.liveMu.Unlock()
	if !ok {
		return ErrNoLiveSession
	}
	select {
	case ls.ch <- agentEscalation{question, reason}:
		return nil
	case <-ls.done:
		return ErrNoLiveSession
	case <-ctx.Done():
		return ctx.Err()
	}
}

// liveSession registers a running session for EscalateSession until the
// returned func (idempotent) ends it.
func (r *Runner) liveSession(taskID string) (<-chan agentEscalation, func()) {
	ls := &liveSession{ch: make(chan agentEscalation), done: make(chan struct{})}
	r.liveMu.Lock()
	if r.live == nil {
		r.live = map[string]*liveSession{}
	}
	r.live[taskID] = ls
	r.liveMu.Unlock()
	var once sync.Once
	return ls.ch, func() {
		once.Do(func() {
			r.liveMu.Lock()
			if r.live[taskID] == ls {
				delete(r.live, taskID)
			}
			r.liveMu.Unlock()
			close(ls.done)
		})
	}
}

func NewRunner(cfg RunnerConfig, store Store, kube Kube, forgejo *Forgejo, completion *Completion, objects ObjectStore, supervisor *SupervisorModel) (*Runner, error) {
	if supervisor == nil {
		return nil, fmt.Errorf("runner: a supervisor model is required")
	}
	if cfg.AgentNamespacePrefix == "" || cfg.WorkerImage == "" || cfg.GatewayURL == "" || cfg.GitBaseURL == "" || cfg.Leases == nil {
		return nil, fmt.Errorf("runner: AgentNamespacePrefix, WorkerImage, GatewayURL, GitBaseURL and Leases are required")
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
	if cfg.Policy == nil || cfg.ReviewModel == "" {
		return nil, fmt.Errorf("runner: the policy in force and the review model are required")
	}
	if cfg.ReproTimeout <= 0 {
		cfg.ReproTimeout = 10 * time.Minute
	}
	if cfg.ReproTimeout > MaxReproTimeout {
		return nil, fmt.Errorf("runner: a reproduction timeout of %s exceeds %s", cfg.ReproTimeout, MaxReproTimeout)
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 3
	}
	if cfg.ChecksTimeout <= 0 {
		cfg.ChecksTimeout = 45 * time.Minute
	}
	if cfg.ChecksPoll <= 0 {
		// each poll heartbeats: a terminal signal's cancel reaches the wait
		// within one
		cfg.ChecksPoll = HeartbeatEvery
	}
	return &Runner{cfg: cfg, store: store, kube: kube, forgejo: forgejo, completion: completion, supervisor: supervisor,
		objects: objects, timeline: &Timeline{Objects: objects}, stopWait: (workerTerminationGrace + 5) * time.Second}, nil
}

// WorkflowConfig is the task policy TaskWorkflow captures at start.
func (r *Runner) WorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		Models:        r.cfg.Models,
		MaxSessions:   r.cfg.MaxSessions,
		WallClockCap:  r.cfg.Supervisor.WallClockCap,
		AnswerSlice:   r.cfg.AnswerSlice,
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
	id       string
	parent   string
	findings []string
	answers  []string
	model    string
	startRef string
	repo     Repo
}

// sessionOutcome is how one sandbox session ended: a clean adapter verdict,
// a trip (with the supervisor's decision unless it was the wall-clock
// cap), or the pod dying without either.
type sessionOutcome struct {
	result   *harness.Result
	commit   string
	trip     *Trip
	decision *Decision
	digest   string
	noResult string
	findings []string
}

// stopReport is what a stopped worker left for the next session: its
// pushed work in progress and its last word.
func (out sessionOutcome) stopReport() []string {
	var lines []string
	if out.commit != "" {
		lines = append(lines, "the stopped session's work in progress is on the branch at "+out.commit)
	}
	switch {
	case out.result != nil:
		lines = append(lines, "the stopped worker reported: "+out.result.Summary)
	case out.noResult != "":
		lines = append(lines, "work in progress not pushed: "+out.noResult)
	}
	return lines
}

// errWorkerStop ends a session because factoryd is stopping: retryable,
// so Temporal runs the attempt on from the heartbeat on the next factoryd.
var errWorkerStop = errors.New("factoryd stopping; the session's work in progress is on the branch for the retry")

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
// out is a named result so the deferred findings reach the caller on
// every return.
// runSession supervises one worker pod until it reports, a trip stops it,
// ctx is cancelled (the workflow decided) or workerStop closes (factoryd
// is stopping: the worker is stopped gracefully first, so its work in
// progress is pushed and reported for the retry).
func (r *Runner) runSession(ctx context.Context, t Task, ns string, s sessionSpec, deadline time.Time, progress func(events int, findings []string), workerStop <-chan struct{}) (out sessionOutcome, err error) {
	recorder, err := NewSessionRecorder(ctx, r.store, r.objects, t.ID, s.id, s.parent)
	if err != nil {
		return out, err
	}
	sup, err := NewSupervisor(r.cfg.Supervisor, deadline.Add(-r.cfg.Supervisor.WallClockCap), time.Now())
	if err != nil {
		return out, err
	}
	// the half-way check-in fires once per task: the flag lives on the task
	sup.halfWayDone = t.HalfWayDone
	defer func() { out.findings = sup.Findings() }()
	digest := newDigestBuilder(time.Now())
	supervision := &sessionSupervision{max: sup.cfg.MaxCalls, decide: func(ctx context.Context, trip *Trip, call int) Decision {
		return r.askSupervisor(ctx, t, s, recorder, digest, trip, call, sup.cfg.MaxCalls)
	}}
	// stopAt is when workerStop closed: the whole stop, an in-flight
	// supervisor call included, ends within r.stopWait of it
	var stopAt time.Time
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
	if err := r.awaitPodStart(ctx, ns, podName, wait, workerStop); err != nil {
		if errors.Is(err, errWorkerStop) {
			return out, err
		}
		out.noResult = err.Error()
		return out, nil
	}
	progress(0, nil)

	sessCtx, cancelLog := context.WithCancel(ctx)
	defer cancelLog()
	followed := followPodLog(sessCtx, r.kube, ns, podName, t.ID)
	lines := followed.lines
	// ended names why the log ended without a result
	ended := func(plain string) string {
		if err := followed.err(); err != nil {
			return err.Error()
		}
		return plain
	}

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
	// the agent's escalate_to_human reaches the session here until it stops
	escalations, unregister := r.liveSession(t.ID)
	defer unregister()
	stop := func(trip *Trip) {
		// a stopping session takes no escalation: the gateway tells the
		// agent it did not arrive
		unregister()
		out.trip = r.abandon(recorder, trip)
		if err := r.kube.DeletePod(ctx, ns, podName); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("task %s: stop pod %s: %v", t.ID, podName, err)
		}
		grace := r.stopWait
		if !stopAt.IsZero() {
			grace = min(grace, time.Until(stopAt.Add(r.stopWait)))
		}
		timeout := time.After(grace)
		for {
			select {
			case <-ctx.Done():
				return
			case <-timeout:
				out.noResult = fmt.Sprintf("the stopped worker did not report within %s", grace.Round(time.Second))
				return
			case line, ok := <-lines:
				if !ok {
					out.noResult = ended("the stopped worker exited without a result")
					return
				}
				if _, done := read(line); done {
					return
				}
			}
		}
	}
	// shutdown is factoryd's own stop: the worker is stopped and the
	// activity retried on the next factoryd from its report.
	shutdown := func() error {
		stop(&Trip{Kind: TripShutdown, Findings: sup.Findings(), Detail: "factoryd is stopping"})
		progress(events, append(sup.Findings(), out.stopReport()...))
		return errWorkerStop
	}
	// onTrip returns true when the session ends, with the session's error.
	onTrip := func(trip *Trip) (bool, error) {
		if trip.Kind == TripHalfWay {
			if err := r.store.MarkHalfWay(ctx, t.ID); err != nil {
				log.Printf("task %s: record the half-way check-in: %v", t.ID, err)
			}
		}
		if trip.Kind == TripWallClock {
			stop(trip)
			return true, ctx.Err()
		}
		// the call never outlives the session: at the cap the cap decides,
		// and factoryd stopping cuts it, its decision unread
		callCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		var d Decision
		var called bool
		// the agent was told "escalated" once the session took it: its
		// escalation during the call wins over whatever the supervisor
		// answers, and cuts the call
		var esc *agentEscalation
		if trip.decided != nil {
			d = *trip.decided
		} else {
			answered := make(chan struct{})
			go func() {
				defer close(answered)
				d, called = supervision.onTrip(callCtx, trip)
			}()
			select {
			case <-answered:
				select {
				case e := <-escalations:
					esc = &e
				default:
				}
			case e := <-escalations:
				esc = &e
				cancel()
				<-answered
			case <-workerStop:
				stopAt = time.Now()
				cancel()
				<-answered
				log.Printf("task %s: factoryd stopping cut the supervisor call for %s", t.ID, trip.Kind)
				return true, shutdown()
			}
		}
		if esc != nil {
			log.Printf("task %s: the agent escalated during the supervisor call for %s", t.ID, trip.Kind)
			sup.tripped = nil
			trip = sup.AgentEscalation(esc.question, esc.reason)
			d = *trip.decided
		}
		out.digest = digest.digest().Summary()
		r.recordDecision(ctx, t, s, recorder, trip, d, called, out.digest)
		note := ""
		switch {
		case !time.Now().Before(deadline):
			stop(&Trip{Kind: TripWallClock, Findings: trip.Findings, Detail: "the task's wall-clock cap is reached"})
			note = "Superseded: the task's wall-clock cap was reached during the supervisor call."
		case d.Action == ActionContinue:
			r.commentDecision(ctx, t, s, trip, d, out.digest, "")
			sup.Resume(time.Now())
			return false, nil
		default:
			out.decision = &d
			stop(trip)
		}
		if out.result != nil && out.result.Success && out.commit != "" {
			note = fmt.Sprintf("Superseded: the session finished and pushed %s during the supervisor call; the task goes to verification.", out.commit)
		}
		// the agent's own escalation reaches the issue through Escalate
		if trip.decided == nil || note != "" {
			r.commentDecision(ctx, t, s, trip, d, out.digest, note)
		}
		return true, ctx.Err()
	}

	tick := time.NewTicker(r.supervisionTick())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case esc := <-escalations:
			if end, err := onTrip(sup.AgentEscalation(esc.question, esc.reason)); end {
				return out, err
			}
		case <-workerStop:
			stopAt = time.Now()
			return out, shutdown()
		case <-tick.C:
			if cur, ok, err := r.store.Task(ctx, t.ID); err == nil && ok {
				sup.SetSpend(cur.SpentEUR, cur.BudgetEUR)
			}
			if trip := sup.Tick(time.Now()); trip != nil {
				if end, err := onTrip(trip); end {
					return out, err
				}
			}
		case line, ok := <-lines:
			if !ok {
				if out.result == nil {
					out.noResult = ended("pod exited without a result")
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
			if trip != nil {
				if end, err := onTrip(trip); end {
					return out, err
				}
			}
		}
	}
}

// askSupervisor builds the supervisor's input and calls it; the call is
// metered against the task's and the day's headroom.
func (r *Runner) askSupervisor(ctx context.Context, t Task, s sessionSpec, recorder *SessionRecorder, digest *digestBuilder, trip *Trip, call, maxCalls int) Decision {
	return r.supervisor.Decide(ctx, t.ID, SupervisorInput{
		Brief:   harness.TaskBrief{Objective: t.Objective, Repository: t.Repository, SHA: s.startRef, Findings: s.findings, Answers: s.answers},
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
// timeline and, for a detector's trip, an alert the decision judged.
func (r *Runner) recordDecision(ctx context.Context, t Task, s sessionSpec, recorder *SessionRecorder, trip *Trip, d Decision, called bool, digest string) {
	detail := map[string]any{"detector": string(trip.Kind), "trip": trip.Detail, "digest": digest,
		"decision": string(d.Action), "guidance": d.Guidance, "reason": d.Reason, "called": called}
	if err := recorder.Record(ctx, harness.Event{Time: time.Now(), Kind: harness.EventSupervisor, Detail: detail}); err != nil {
		log.Printf("task %s: record supervisor decision: %v", t.ID, err)
	}
	detail["session"] = s.id
	r.record(ctx, t.ID, "supervisor", detail)
	if trip.Kind == TripAgentEscalation {
		return // the agent's question: the workflow's escalation alert
	}
	a := Alert{Kind: AlertSupervisorTrip, Source: string(trip.Kind), TaskID: t.ID, Verdict: tripVerdict(d.Action), ResolvedBy: "supervisor",
		Detail: map[string]any{"session": s.id, "action": string(d.Action), "trip": trip.Detail, "called": called}}
	if _, err := r.store.InsertAlert(ctx, a); err != nil {
		log.Printf("task %s: record the supervisor trip alert: %v", t.ID, err)
	}
}

// commentDecision puts a decision on the issue, with a note when the
// session's end superseded it. An escalation is commented too: a session
// that pushed during the call ends pushed, and then no Escalate follows.
func (r *Runner) commentDecision(ctx context.Context, t Task, s sessionSpec, trip *Trip, d Decision, digest, note string) {
	if t.IssueNumber == 0 {
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
	if note != "" {
		body += "\n\n" + note
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

// awaitPodStart waits for the pod to leave Pending; factoryd stopping
// ends the wait with errWorkerStop (the retry creates the pod anew).
func (r *Runner) awaitPodStart(ctx context.Context, ns, podName string, timeout time.Duration, workerStop <-chan struct{}) error {
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
		case <-workerStop:
			return errWorkerStop
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("pod %s still pending after %s", podName, timeout)
}

// setupNamespace builds the session's sandbox: base is the read token on
// the task's repository (the clone), push the write token on the bot's fork
// (the branch push).
func (r *Runner) setupNamespace(ctx context.Context, t Task, base, push ForgejoToken) error {
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
			leased, err := r.cfg.Leases.LiveAddresses(ctx)
			if err != nil {
				return fmt.Errorf("read leases: %w", err)
			}
			return r.kube.ApplyCiliumNetworkPolicy(ctx, ns, workerEgress, r.workerEgressSpec(leased[t.ID]))
		}},
		{"git token secret", func() error {
			return r.kube.CreateSecret(ctx, ns, "task-git", "", map[string]string{"REPO_TOKEN": base.Secret, "PUSH_TOKEN": push.Secret})
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

func (r *Runner) branch(t Task) string { return "factory/" + t.ID }

func (r *Runner) repoURL(repo Repo) string {
	return strings.TrimSuffix(r.cfg.GitBaseURL, "/") + "/" + repo.String() + ".git"
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
	answersJSON, _ := json.Marshal(s.answers)
	env := []map[string]any{
		{"name": "TASK_ID", "value": t.ID},
		{"name": "SESSION_ID", "value": s.id},
		{"name": "OBJECTIVE", "value": t.Objective},
		{"name": "REPOSITORY", "value": t.Repository},
		{"name": "REPO_URL", "value": r.repoURL(s.repo)},
		{"name": "REPO_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": "task-git", "key": "REPO_TOKEN"}}},
		{"name": "REPO_SHA", "value": s.startRef},
		{"name": "BRANCH", "value": r.branch(t)},
		{"name": "PUSH_URL", "value": r.repoURL(r.forgejo.Fork(s.repo))},
		{"name": "PUSH_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": "task-git", "key": "PUSH_TOKEN"}}},
		{"name": "GATEWAY_URL", "value": r.cfg.GatewayURL},
		{"name": "TOKEN_FILE", "value": "/var/run/secrets/tokens/gateway"},
		{"name": "MODEL", "value": s.model},
		{"name": "ANSWER_TOKENS", "value": strconv.Itoa(r.cfg.AnswerTokens)},
		{"name": "HEADROOM_TOKENS", "value": strconv.Itoa(r.cfg.HeadroomTokens)},
		{"name": "FINDINGS", "value": string(findingsJSON)},
		{"name": "ANSWERS", "value": string(answersJSON)},
	}
	if s.parent != "" {
		env = append(env, map[string]any{"name": "PARENT_SESSION_ID", "value": s.parent})
	}
	return r.sandboxPodSpec(t, env, true)
}

// sandboxPodSpec is a gVisor worker pod in the task namespace; only an
// agent session gets the gateway token. Every sandbox pod carries the
// worker label the egress policy selects: without it no policy would
// confine its egress.
func (r *Runner) sandboxPodSpec(t Task, env []map[string]any, gatewayToken bool) map[string]any {
	pullSecrets := []any{}
	if r.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": r.cfg.PullSecret})
	}
	mounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/workspace"},
		map[string]any{"name": "home", "mountPath": "/home/worker"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "4Gi"}},
		map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
	}
	if gatewayToken {
		mounts = append(mounts, map[string]any{"name": "gateway-token", "mountPath": "/var/run/secrets/tokens", "readOnly": true})
		volumes = append(volumes, map[string]any{"name": "gateway-token", "projected": map[string]any{
			"sources": []any{map[string]any{
				"serviceAccountToken": map[string]any{
					"path":              "gateway",
					"audience":          "factory-gateway",
					"expirationSeconds": 3600,
				},
			}},
		}})
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
				"volumeMounts": mounts,
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

// workerEgressSpec is the sandbox network policy (architecture 6.4, research
// Q4): default-deny egress for workers, a hard deny to the cloud metadata
// address, and exactly four allows: the model gateway's port, Forgejo's
// HTTP port, the Go module proxy and checksum database by name on 443
// (ruling 99: the image carries the Go toolchain, and a registry joins by
// name with the toolchain that needs it), and kube-dns for exactly those
// names. DNS is allowlisted by name because an open resolver is itself an
// exfiltration channel. No other registry is reachable: opencode's first-run
// npm install (its @opencode-ai/plugin dependency) is baked into the image at
// build time from opencode-plugin/'s checked-in lockfile, so the worker never
// reaches npm at runtime — npm is unreachable here regardless. Workers reach
// Forgejo through its in-cluster service, not the public host: in-cluster
// DNS answers that host with the gateway ClusterIP, which Cilium's toFQDNs
// cannot authorize (the envoy fronting it answers 403), while the service
// path is identity-scoped. The live gateway pods carry the plain `app`
// label, not app.kubernetes.io/name.
func (r *Runner) workerEgressSpec(leased []string) map[string]any {
	dnsNames := []any{}
	for _, raw := range []string{r.cfg.GatewayURL, r.cfg.GitBaseURL} {
		u, _ := url.Parse(raw) // validated in NewRunner
		dnsNames = append(dnsNames, map[string]any{"matchName": u.Hostname()})
	}
	toFQDNs := []any{}
	// storage.googleapis.com: proxy.golang.org redirects zip downloads there.
	for _, host := range []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"} {
		toFQDNs = append(toFQDNs, map[string]any{"matchName": host})
		dnsNames = append(dnsNames, map[string]any{"matchName": host})
	}
	spec := map[string]any{
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
	for _, ip := range leased {
		spec["egress"] = append(spec["egress"].([]any), leasedServerRule(ip))
	}
	return spec
}

const workerEgress = "worker-egress"

// leasedServerRule admits SSH to one commissioned server: toCIDR and
// toPorts in one rule item, so the port applies to that address only.
func leasedServerRule(ipv4 string) map[string]any {
	return map[string]any{
		"toCIDR": []string{ipv4 + "/32"},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "22", "protocol": "TCP"}},
		}},
	}
}

// AdmitLeases brings every agent namespace's egress policy in line with
// its task's live leases: it reads the live policy and applies the desired
// one when they differ, so a new lease's address joins, an ended one
// leaves, and an edit or loss out of band is repaired on the next pass.
func (r *Runner) AdmitLeases(ctx context.Context) error {
	leased, err := r.cfg.Leases.LiveAddresses(ctx)
	if err != nil {
		return fmt.Errorf("read leases: %w", err)
	}
	names, err := r.kube.NamespacesByLabel(ctx, "factory.ai/task", "")
	if err != nil {
		return fmt.Errorf("list agent namespaces: %w", err)
	}
	var errs []error
	for _, ns := range names {
		id := strings.TrimPrefix(ns, r.cfg.AgentNamespacePrefix)
		if id == ns || strings.HasPrefix(id, "ci-") {
			continue
		}
		want := r.workerEgressSpec(leased[id])
		have, err := r.kube.CiliumNetworkPolicySpec(ctx, ns, workerEgress)
		if err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", ns, err))
			continue
		}
		same, err := sameJSON(have, want)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ns, err))
			continue
		}
		if same {
			continue
		}
		err = r.kube.ApplyCiliumNetworkPolicy(ctx, ns, workerEgress, want)
		switch {
		case errors.Is(err, ErrNotFound): // the namespace went meanwhile
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", ns, err))
		default:
			log.Printf("task %s: egress admits leased servers %v", id, leased[id])
		}
	}
	return errors.Join(errs...)
}

// sameJSON compares two objects as their JSON: the live policy's spec
// decoded from the API against the desired one built in Go.
func sameJSON(a, b any) (bool, error) {
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	var na, nb any
	if err := json.Unmarshal(ja, &na); err != nil {
		return false, err
	}
	if err := json.Unmarshal(jb, &nb); err != nil {
		return false, err
	}
	return reflect.DeepEqual(na, nb), nil
}

// LeaseLoop runs AdmitLeases every interval until ctx ends.
func (r *Runner) LeaseLoop(ctx context.Context, every time.Duration) {
	for {
		if err := r.AdmitLeases(ctx); err != nil {
			log.Printf("lease admission: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
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
