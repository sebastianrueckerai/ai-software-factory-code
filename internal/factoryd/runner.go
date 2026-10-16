package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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
	ChecksTimeout        time.Duration // how long WaitChecks waits for the PR head's combined status
	ChecksPoll           time.Duration
	// Bus receives a session_said event for each model call's text, for
	// the drawer's Chat; nil publishes none.
	Bus *EventBus
	// Supervisor is the session supervisor's configuration; its agent
	// time, call count and progress detector come from the policy.
	Supervisor SupervisorConfig
	// Policy is the configuration version in force: the risk rules, the
	// limits a workflow captures at its start, the supervisor's.
	Policy func() policy.Version
	// PolicyVersion reads version id, the one a task's workflow started
	// under (PolicyStore.PolicyVersion).
	PolicyVersion func(ctx context.Context, id int64) (policy.Version, bool, error)
	// ReproTimeout bounds one reproduction command run.
	ReproTimeout time.Duration
	// Leases reads the commissioned servers each task holds; their IPv4s
	// join the task's sandbox egress on port 22 while the lease lives.
	Leases LeaseReader
	// CIQueue holds the spawner's queue times for the checks step's jobs;
	// nil = none measured.
	CIQueue *CIQueue
	// Metrics receives the timeline's gate durations; nil = the no-op meter.
	Metrics *telemetry.Metrics
	// OTLPEndpoint is SigNoz's collector (OTLP HTTP, port 4318), where the
	// workers send their telemetry (ruling 158); "" = none.
	OTLPEndpoint string
	// EgressCanary (a public name and port, off the sandbox's DNS
	// allowlist) and EgressDirectIP (a public IP and port) are what the
	// worker's egress probe must not reach before a session starts
	// (roadmap 42).
	EgressCanary, EgressDirectIP string
	// Gates is the worker's GateWatch: the gate supervisor acts on the
	// attempts it watches.
	Gates *GateWatch
	// LastFailure is the last failure of a workflow's pending activity
	// run, "" for none (Temporal's pending activity info).
	LastFailure func(ctx context.Context, workflowID, activityID string) (string, error)
	// Workflows starts the workflows of the report tasks the screening
	// stage opens.
	Workflows Workflows
	// ProjectsUser is factory-projects, whose fork a template update pull
	// request comes from: a re-run of its checks writes there with
	// Forgejo's ProjectsToken.
	ProjectsUser string
	// Prices is the alias price book the attempt grant's floor reads
	// (ruling 322): whether the task's remaining budget can fund a call at
	// all. nil = every attempt is funded; the per-call hold still guards
	// the money.
	Prices Pricer
}

// Pricer is an alias's worst-case price, the figure the gateway's hold
// math prices a call with; implemented by policy.PriceBook.
type Pricer interface {
	Price(ctx context.Context, alias string) (policy.Price, error)
}

// LeaseReader is the live leased servers' IPv4s by owning task, and the
// servers gone live since the last claim (cloudres.AgentStore on the
// shared database).
type LeaseReader interface {
	LiveAddresses(ctx context.Context) (map[string][]string, error)
	ClaimCommissioned(ctx context.Context) ([]cloudres.Commissioned, error)
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
	cheapCap   time.Duration // cheapAttemptCap; a test shortens it
	// startPoll is how often a pod's start is read, configGrace how long a
	// container may wait with a configuration error (configErrorGrace);
	// a test shortens both
	startPoll, configGrace time.Duration
	// workerPort replaces harness.MessagePort in tests
	workerPort int

	liveMu  sync.Mutex
	live    map[string]*liveSession // by task: its running session
	sandbox map[string][2]string    // by task: namespace and name of its running sandbox pod
	// what a roll's session gap waits for, by task: its sessions running
	// here, and when AwaitRollHold let its next session start
	running  map[string]int
	admitted map[string]time.Time
	// filed is what a running session's file_plan tool stored, by task:
	// its answer, taken by the loop that ran the session
	filed map[string]PlanResult
	// now and sleep are the roll gap's and the roll hold's clock; nil is
	// the wall clock
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	// booted is when this factoryd started: for rollBoot after it, the
	// sessions its restart stopped may not be retried here yet
	booted time.Time

	// Routines keeps the routines' runs, and reads the project a run files
	// on (roadmap 267). TemplatePass runs the template pass over
	// every project with a template and counts the cards it filed
	// (Registrar.TemplateUpdates). Both are set before the worker starts.
	Routines     RoutineRunStore
	TemplatePass func(ctx context.Context) (cards int, err error)
}

// liveSession takes the agent's escalations: ch is unbuffered, so a send
// completes only when the session reads it; done closes when the session
// takes no more. worker is where a writer's message to it goes.
type liveSession struct {
	ch     chan AgentAsk
	done   chan struct{}
	worker sessionWorker
	// basics records that the session was given the factory basics on its
	// first human-sourced message.
	basics bool
}

// sessionWorker is a running session's pod and the bearer of its worker's
// message endpoint.
type sessionWorker struct{ session, ns, pod, token string }

// ErrNoLiveSession: the task has no session running in this factoryd.
var ErrNoLiveSession = errors.New("the task has no running session")

// EscalateSession hands an agent's escalate_to_human to its task's running
// session, which stops the way a supervisor's escalate stops it. It returns
// nil only once the session took the escalation; a session that ends or
// stops first answers ErrNoLiveSession.
func (r *Runner) EscalateSession(ctx context.Context, taskID string, ask AgentAsk) error {
	r.liveMu.Lock()
	ls, ok := r.live[taskID]
	r.liveMu.Unlock()
	if !ok {
		return ErrNoLiveSession
	}
	select {
	case ls.ch <- ask:
		return nil
	case <-ls.done:
		return ErrNoLiveSession
	case <-ctx.Done():
		return ctx.Err()
	}
}

// noteFiled holds the plan a running session filed through the file_plan
// tool; takeFiled takes it, once: it is that session's answer, and a
// filing no session takes (one an earlier session left) does not outlive
// it.
func (r *Runner) noteFiled(taskID string, res PlanResult) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.filed == nil {
		r.filed = map[string]PlanResult{}
	}
	r.filed[taskID] = res
}

func (r *Runner) takeFiled(taskID string) (PlanResult, bool) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	res, ok := r.filed[taskID]
	delete(r.filed, taskID)
	return res, ok
}

// trackPod names the task's running sandbox pod for the gate supervisor
// until the returned func.
func (r *Runner) trackPod(taskID, ns, name string) (untrack func()) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.sandbox == nil {
		r.sandbox = map[string][2]string{}
	}
	r.sandbox[taskID] = [2]string{ns, name}
	return func() {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		if r.sandbox[taskID] == [2]string{ns, name} {
			delete(r.sandbox, taskID)
		}
	}
}

func (r *Runner) sandboxPod(taskID string) (ns, name string, ok bool) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	p, ok := r.sandbox[taskID]
	return p[0], p[1], ok
}

// liveSession registers a running session for EscalateSession and
// MessageSession until the returned func (idempotent) ends it.
func (r *Runner) liveSession(taskID string, worker sessionWorker) (<-chan AgentAsk, func()) {
	ls := &liveSession{ch: make(chan AgentAsk), done: make(chan struct{}), worker: worker}
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
	urls := []string{cfg.GatewayURL, cfg.GitBaseURL}
	if cfg.OTLPEndpoint != "" {
		urls = append(urls, cfg.OTLPEndpoint)
	}
	for _, raw := range urls {
		if u, err := url.Parse(raw); err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("runner: %q is not an absolute URL", raw)
		}
	}
	if err := CheckEgressTargets(cfg.EgressCanary, cfg.EgressDirectIP); err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	canary, _, _ := net.SplitHostPort(cfg.EgressCanary)
	if slices.ContainsFunc((&Runner{cfg: cfg}).sandboxNames(), func(name string) bool { return strings.EqualFold(name, strings.TrimSuffix(canary, ".")) }) {
		return nil, fmt.Errorf("runner: the egress canary %s is on the sandbox's DNS allowlist, so its probe would fail every session's start", canary)
	}
	if _, err := (TokenBudget{AnswerTokens: cfg.AnswerTokens, HeadroomTokens: cfg.HeadroomTokens}).OutputLimit(); err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	if cfg.Policy == nil || cfg.PolicyVersion == nil {
		return nil, fmt.Errorf("runner: the policy in force and the version reader are required")
	}
	d := cfg.Policy().Document
	if err := (&Runner{cfg: cfg}).supervisorConfig(d, d.Limits.AgentTime, d.Limits.CheckIn).validate(); err != nil {
		return nil, fmt.Errorf("runner: %w", err)
	}
	if cfg.Gates == nil || cfg.LastFailure == nil || cfg.Workflows == nil {
		return nil, fmt.Errorf("runner: the gate watch, the last failure reader and the workflows are required")
	}
	if cfg.ReproTimeout <= 0 {
		cfg.ReproTimeout = 10 * time.Minute
	}
	if cfg.ReproTimeout > MaxReproTimeout {
		return nil, fmt.Errorf("runner: a reproduction timeout of %s exceeds %s", cfg.ReproTimeout, MaxReproTimeout)
	}
	if cfg.ChecksTimeout <= 0 {
		cfg.ChecksTimeout = 45 * time.Minute
	}
	if cfg.Metrics == nil {
		cfg.Metrics = telemetry.Noop()
	}
	if cfg.CIQueue == nil {
		cfg.CIQueue = NewCIQueue(cfg.Metrics)
	}
	if cfg.ChecksPoll <= 0 {
		// each poll heartbeats: a terminal signal's cancel reaches the wait
		// within one
		cfg.ChecksPoll = HeartbeatEvery
	}
	return &Runner{cfg: cfg, store: store, kube: kube, forgejo: forgejo, completion: completion, supervisor: supervisor,
		objects: objects, timeline: &Timeline{Objects: objects, Metrics: cfg.Metrics,
			Events: eventStoreOf(store), Store: store}, stopWait: (workerTerminationGrace + 5) * time.Second, cheapCap: cheapAttemptCap,
		startPoll: 2 * time.Second, configGrace: configErrorGrace, booted: time.Now()}, nil
}

// WorkflowConfig is what TaskWorkflow captures at its start: the limits of
// the version in force and the version itself, which the task keeps.
func (r *Runner) WorkflowConfig() WorkflowConfig {
	v := r.cfg.Policy()
	l := v.Document.Limits
	return WorkflowConfig{
		Models:        r.cfg.Models,
		MaxSessions:   l.Sessions,
		WallClockCap:  l.AgentTime,
		ChecksTimeout: r.cfg.ChecksTimeout,
		StrongerAfter: l.StrongerAfter,
		PolicyVersion: v.ID,
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

// readable is the repositories the session's read token covers: its task
// repository, extra, and the document repository when there is one.
func (s sessionSpec) readable(extra ...Repo) []Repo {
	repos := append([]Repo{s.repo}, extra...)
	if s.docs != (Repo{}) {
		repos = append(repos, s.docs)
	}
	return repos
}

// sessionSpec is what one worker pod runs: its identity, the chain it
// continues, and where it starts.
type sessionSpec struct {
	id         string
	parent     string
	findings   []string
	answers    []string
	posterInfo []string
	model      string
	startRef   string
	// base is the task's base commit: a session resumed from pushed work in
	// progress starts past it
	base string
	repo Repo
	// docs is the project's document repository (chat.doc_repo), which the
	// session reads at /workspace/docs; the zero Repo when it has none
	docs Repo
	// cache is the project's cache claim in the task namespace
	cache string
	// policy is the task's pinned version's: its limits and detectors
	policy policy.Document
	// attempt is the session's attempt number (1 for the first)
	attempt int
	// sessions is the cap the attempt number counts against: the policy's
	// session limit and the attempts a human answer granted
	sessions int
	// planner marks a plan card's session: its warning says file the plan
	planner bool
	// facts is factoryd's FACTS line for the brief
	facts string
	// agentTime and checkIn are the task's resolved values (policy.Time,
	// via TaskConfig for every session kind, RunSession's and
	// reportSession's alike): supervisorConfig's WallClockCap and
	// CheckIn, and the session's deadline.
	agentTime, checkIn time.Duration
	// report is a report session's guide (ruling 185); "" for a coding
	// session
	report string
	// standing overrides the task's author standing: a report reads its
	// parent's issue
	standing string
	// spec and cardRole are a plan's card's brief section and role
	spec, cardRole string
	// directories are the card's, which the worker maps from its clone
	directories []string
	// pull is the pull request a split reads, whose head the clone holds
	// as branch pull/<pull>; 0 for none
	pull int64
	// messageToken is the bearer of the worker's message endpoint, made
	// per session; the pod gets its hash
	messageToken string
}

// sessionOutcome is how one sandbox session ended: a clean adapter verdict,
// a trip (with the supervisor's decision unless it was the wall-clock
// cap), or the pod dying without either.
type sessionOutcome struct {
	result *harness.Result
	// agentText is the agent's last message (the adapter's finished event)
	agentText string
	commit    string
	trip      *Trip
	decision  *Decision
	digest    string
	noResult  string
	start     *startFailure // the session could not start
	pending   *podPending   // its pod never left Pending: no session ran
	model     string        // the model the session ended on
	overHold  *OverHoldCall // the session's newest call booked beyond its hold
}

// stopReport is what a stopped worker left for the next session beside
// the branch and the agent's last message (handoverLines): its report when
// nothing reached the branch, a refused push's reason among them, or why no
// report came, which says the work is not pushed only when no push was.
func (out sessionOutcome) stopReport() []string {
	switch {
	case out.result != nil && out.commit == "":
		return []string{"the stopped worker reported: " + out.result.Summary}
	case out.result == nil && out.noResult != "" && out.commit != "":
		return []string{out.noResult}
	case out.result == nil && out.noResult != "":
		return []string{"work in progress not pushed: " + out.noResult}
	}
	return nil
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
// asks the supervisor model; continue and a delivered message let the
// session run on, any other decision stops it. A stopped worker commits
// and pushes its work in progress before it exits. progress receives the event count and
// findings so far, for the activity heartbeat.
// out is a named result so the deferred findings reach the caller on
// every return.
// runSession supervises one worker pod until it reports, a trip stops it,
// ctx is cancelled (the workflow decided) or workerStop closes (factoryd
// is stopping: the worker is stopped gracefully first, so its work in
// progress is pushed and reported for the retry).
// runSession runs the session in a new pod, or, with live set, reattaches
// to the pod a stopped factoryd left running: the loop reads the pod's
// log on from the lines that factoryd consumed, with the same session
// record (roadmap 222, item 2).
func (r *Runner) runSession(ctx context.Context, t Task, ns string, s sessionSpec, deadline time.Time, progress func(events int), workerStop <-chan struct{}, live *LiveSession) (out sessionOutcome, err error) {
	var recorder *SessionRecorder
	if live != nil {
		recorder, err = ResumeSessionRecorder(ctx, r.store, r.objects, t.ID, s.id)
	} else {
		recorder, err = NewSessionRecorder(ctx, r.store, r.objects, t.ID, s.id, s.parent, s.model)
	}
	if err != nil {
		return out, err
	}
	// stopAt is when workerStop closed: the whole stop, an in-flight
	// supervisor call included, ends within r.stopWait of it
	var stopAt time.Time
	podName := gateway.SessionPod + s.id
	// consumed is how many of the pod's log lines the loop took: the row
	// keeps it for the reattach
	consumed := 0
	if live != nil {
		podName, s.messageToken, consumed = live.Pod, live.MessageToken, live.Lines
	} else {
		s.messageToken = rand.Text()
		s.facts = SessionFacts{BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR, TimeLeft: time.Until(deadline),
			Attempt: s.attempt, Sessions: s.sessions}.Line()
		if err := r.kube.CreatePod(ctx, ns, podName, r.workerPodSpec(t, s)); err != nil {
			return out, fmt.Errorf("create pod: %w", err)
		}
		if err := r.store.PutLiveSession(ctx, LiveSession{TaskID: t.ID, SessionID: s.id, Namespace: ns, Pod: podName,
			MessageToken: s.messageToken, StartedAt: time.Now(), Deadline: deadline}); err != nil {
			return out, err
		}
	}
	defer r.trackPod(t.ID, ns, podName)()
	// detached: factoryd is stopping and the pod runs on for the next
	// factoryd's retry of the activity; every other end deletes the pod
	detached := false
	defer func() {
		if detached {
			return
		}
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.kube.DeletePod(delCtx, ns, podName); err != nil && !errors.Is(err, ErrNotFound) {
			log.Printf("task %s: delete pod %s: %v", t.ID, podName, err)
		}
		if err := r.store.DeleteLiveSession(delCtx, t.ID); err != nil {
			log.Printf("task %s: forget live session %s: %v", t.ID, s.id, err)
		}
	}()

	// The pod must leave Pending before its log endpoint is useful; a first
	// image pull can take minutes, but never past the session deadline.
	wait := min(10*time.Minute, time.Until(deadline))
	if err := r.awaitPodStart(ctx, ns, podName, wait, workerStop); err != nil {
		if errors.Is(err, errWorkerStop) {
			detached = true
			return out, err
		}
		out.noResult = err.Error()
		if !errors.As(err, &out.start) {
			errors.As(err, &out.pending)
		}
		return out, nil
	}
	progress(0)

	// the supervisor's clocks start with the pod: a slow pull is no time
	// the agent had
	supCfg := r.supervisorConfig(s.policy, s.agentTime, s.checkIn)
	supCfg.ReadOnly = s.report != ""
	if s.attempt == 1 && r.canClimb(s) {
		supCfg.CheapCap = r.cheapCap
	}
	sup, err := NewSupervisor(supCfg, deadline.Add(-supCfg.WallClockCap), time.Now())
	if err != nil {
		return out, err
	}
	defer func() { out.model = s.model }()
	watch := newLimitWatch(s.policy.Limits, s.planner, time.Now(), time.Until(deadline))
	digest := newDigestBuilder(time.Now())
	// messages: the guidance this session's delivered messages carried;
	// briefed: a supervisor call of this session read the whole brief
	var messages []string
	briefed := false
	supervision := &sessionSupervision{max: sup.cfg.MaxCalls, decide: func(ctx context.Context, trip *Trip, call int) Decision {
		d := r.askSupervisor(ctx, t, s, recorder, digest, trip, call, sup.cfg.MaxCalls, slices.Clone(messages), !briefed)
		briefed = true
		return d
	}}

	sessCtx, cancelLog := context.WithCancel(ctx)
	defer cancelLog()
	// the worker asks for its push token once its agent is dead; the log
	// follower answers at once, as the session loop may be waiting on a
	// supervisor call, and re-asks cover a failed answer
	var pushToken string
	answer := func(line []byte) {
		var env struct {
			Push *harness.PushRequest `json:"push"`
		}
		if json.Unmarshal(line, &env) != nil || env.Push == nil {
			return
		}
		if err := r.answerPush(sessCtx, t, s, ns, podName, &pushToken); err != nil {
			log.Printf("task %s: push token for %s: %v", t.ID, env.Push.Commit, err)
		}
	}
	followed := followPodLogFrom(sessCtx, r.kube, ns, podName, t.ID, consumed, answer)
	lines := followed.lines
	// ended says why the log ended without a result, plain the generic
	// words: how the pod ended when its diagnosis says, else the log's
	// own error
	ended := func(plain string) string {
		if d, err := r.kube.PodDiagnosis(ctx, ns, podName); err == nil {
			if how := exitedHow(plain, d); how != "" {
				return how
			}
		}
		if err := followed.err(); err != nil {
			return err.Error()
		}
		return plain
	}

	// read takes one log line: the result ends the session; an event is
	// recorded and returned for supervision.
	events := 0
	// retry is a start that failed on a fault a later start may not meet
	var retry error
	read := func(line []byte) (ev *harness.Event, done bool) {
		consumed++
		var env harness.WorkerEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			// the worker's stderr log shares the stream with its envelopes
			log.Printf("task %s: worker: %s", t.ID, line)
			return nil, false
		}
		if env.Result != nil {
			res := *env.Result
			out.result = &res
			if err := workerStartFailure(res); !errors.As(err, &out.start) {
				retry = err
			}
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
		if text, _ := env.Event.Detail["text"].(string); env.Event.Kind == harness.EventModelCall && text != "" && r.cfg.Bus != nil {
			r.cfg.Bus.Publish(Event{At: env.Event.Time, Repository: t.Repository, TaskID: t.ID, IssueNumber: t.IssueNumber, Kind: EventSessionSaid,
				Actor: FactoryDecider, Via: ViaFactoryd, Detail: map[string]any{"session": s.id, "text": text}})
		}
		switch env.Event.Kind {
		case harness.EventPushed:
			out.commit, _ = env.Event.Detail["commit"].(string)
		case harness.EventFinished:
			out.agentText, _ = env.Event.Detail["summary"].(string)
		}
		events++
		return env.Event, false
	}
	// stop asks the worker to end (SIGTERM through the pod's deletion) and
	// reads on until it has pushed its work in progress and reported, or
	// its grace period is over.
	// the agent's escalate_to_human reaches the session here until it stops
	escalations, unregister := r.liveSession(t.ID, sessionWorker{session: s.id, ns: ns, pod: podName, token: s.messageToken})
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
	// shutdown is factoryd's own stop: the session detaches, the worker
	// runs on, and the activity's retry on the next factoryd reattaches
	// from the row (the lines consumed so far) and the heartbeat.
	shutdown := func() error {
		detached = true
		unregister()
		if err := r.store.SetLiveSessionLines(context.WithoutCancel(ctx), t.ID, consumed); err != nil {
			log.Printf("task %s: record the lines consumed of session %s: %v", t.ID, s.id, err)
		}
		progress(events)
		return errWorkerStop
	}
	// onTrip returns true when the session ends, with the session's error.
	onTrip := func(trip *Trip) (bool, error) {
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
		var esc *AgentAsk
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
			trip = sup.AgentAsk(*esc)
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
		case d.Action == ActionRetryStronger && r.canClimb(s):
			// the climb in place (roadmap 222, item 5): the session goes on
			// on the stronger model; a switch that fails ends it as before
			if err := r.climb(ctx, t, &s, ns, podName, sup, trip, d); err == nil {
				sup.Climbed()
				sup.Resume(time.Now())
				r.commentDecision(ctx, t, s, trip, d, out.digest, "The session continues on the stronger model from here, without a new session.")
				return false, nil
			} else {
				out.decision = &d
				stop(trip)
				note = fmt.Sprintf("The switch to the stronger model failed (%v): the session stopped, and the next one starts on it.", err)
			}
		case d.Action == ActionMessage:
			err := r.message(ctx, t, s, ns, podName, d.Guidance)
			if err == nil || errors.Is(err, harness.ErrNotRunning) {
				r.commentDecision(ctx, t, s, trip, d, out.digest, "")
				sup.Messaged(time.Now())
				messages = append(messages, d.Guidance)
				return false, nil
			}
			out.decision = &Decision{Action: ActionRedirect, Guidance: d.Guidance}
			stop(trip)
			note = fmt.Sprintf("The message could not be delivered (%v): the session stopped, and the next one starts with the guidance.", err)
		default:
			out.decision = &d
			stop(trip)
		}
		if trip.decided != nil && d.Action == ActionRetryStronger {
			note = "The agent is stuck after the supervisor's message: the session ends without a supervisor call, and a new session resumes from the work in progress."
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
			if end, err := onTrip(sup.AgentAsk(esc)); end {
				return out, err
			}
		case <-workerStop:
			stopAt = time.Now()
			return out, shutdown()
		case <-tick.C:
			trip := sup.Tick(time.Now())
			if trip == nil {
				trip = r.watchLimits(ctx, t, s, ns, podName, watch)
			}
			if trip != nil {
				if end, err := onTrip(trip); end {
					return out, err
				}
			}
		case line, ok := <-lines:
			if !ok {
				// a cancellation answers as itself even when the log ended
				// with it: the caller reads the difference (a cancel is the
				// workflow's decision, a closed log the worker's death)
				if err := ctx.Err(); err != nil {
					return out, err
				}
				if out.result == nil {
					out.noResult = ended("pod exited without a result")
				}
				return out, nil
			}
			ev, done := read(line)
			if done {
				return out, retry
			}
			if ev == nil {
				continue
			}
			if err := r.store.SetLiveSessionLines(ctx, t.ID, consumed); err != nil {
				log.Printf("task %s: record the lines consumed of session %s: %v", t.ID, s.id, err)
			}
			now := time.Now()
			digest.add(*ev, now)
			trip := sup.Event(*ev, now)
			if n := sup.TakeNudge(); n != nil && trip == nil {
				trip = r.nudge(ctx, t, s, sup, ns, podName, s.messageToken, *n)
			}
			progress(events)
			if trip != nil {
				if end, err := onTrip(trip); end {
					return out, err
				}
			}
		}
	}
}

// cheapAttemptCap is how long the start rung's first attempt has to push
// before the session climbs in place (roadmap 222, item 5).
const cheapAttemptCap = 8 * time.Minute

// canClimb: the session runs on the ladder's start and a stronger rung
// exists to switch to.
func (r *Runner) canClimb(s sessionSpec) bool {
	return s.model == r.cfg.Models.Start && r.cfg.Models.Stronger != "" && r.cfg.Models.Stronger != s.model
}

// climb switches the running session to the stronger model: the gateway
// admits its calls on the new alias from the next one (tasks.model_alias)
// and the worker names the model on every later prompt. The session's
// record and the events table keep the switch.
func (r *Runner) climb(ctx context.Context, t Task, s *sessionSpec, ns, pod string, sup *Supervisor, trip *Trip, d Decision) error {
	from, to := s.model, r.cfg.Models.Stronger
	if err := r.store.SetTaskModelAlias(ctx, t.ID, aliasOf(to)); err != nil {
		return err
	}
	text := fmt.Sprintf("factoryd: this session continues on the stronger model, %s: %s. Carry on from where you are.", aliasOf(to), d.Reason)
	if err := r.postWorker(ctx, ns, pod, s.messageToken, "/message", map[string]string{"text": text, "model": to}); err != nil {
		if undo := r.store.SetTaskModelAlias(ctx, t.ID, aliasOf(from)); undo != nil {
			log.Printf("task %s: restore the session's alias: %v", t.ID, undo)
		}
		return err
	}
	s.model = to
	minutes := time.Since(sup.started).Minutes()
	detail := map[string]any{"session": s.id, "from": aliasOf(from), "to": aliasOf(to), "trip": string(trip.Kind), "reason": d.Reason, "minutes": math.Round(minutes*10) / 10}
	r.record(ctx, t.ID, "model_switched", detail)
	r.timeline.recordEvent(ctx, t.ID, EventModelSwitched, detail)
	return nil
}

// supervisorConfig is the session supervisor's configuration with the
// agent time and check-in (the task's resolved policy.Time), the call
// count and the progress detectors of d, the task's pinned policy.
func (r *Runner) supervisorConfig(d policy.Document, agentTime, checkIn time.Duration) SupervisorConfig {
	c := r.cfg.Supervisor
	c.NoWriteAfter = d.Detector(policy.DetectorNoWrite)
	c.WallClockCap, c.MaxCalls = agentTime, d.Limits.SupervisorCalls
	c.CheckIn = checkIn
	c.NudgeAfter, c.Nudges = d.Limits.Nudge()
	c.SameCall = d.Limits.SameCallAfter()
	return c
}

// pinned is policy version id, the one a task's workflow started under;
// the version in force for id 0 (a workflow from before the pin).
func (r *Runner) pinned(ctx context.Context, id int64) (policy.Document, error) {
	v, err := r.pinnedVersion(ctx, id)
	return v.Document, err
}

// pinnedVersion is pinned's whole version, its ID with it.
func (r *Runner) pinnedVersion(ctx context.Context, id int64) (policy.Version, error) {
	if id == 0 {
		return r.cfg.Policy(), nil
	}
	v, ok, err := r.cfg.PolicyVersion(ctx, id)
	if err != nil {
		return policy.Version{}, fmt.Errorf("read policy version %d: %w", id, err)
	}
	if !ok {
		return policy.Version{}, terminalError("policy version %d, which the task started under, does not exist", id)
	}
	return v, nil
}

// factoryModel is the alias the task's pinned document names for purpose
// on t's capability classes; "" when it names none.
func factoryModel(d policy.Document, t Task, purpose string) string {
	return d.FactoryModel(t.RequiredCapabilities, purpose)
}

// askSupervisor builds the supervisor's input and calls it through the
// gateway on the task's account. The session's first call reads the whole
// brief, a later one (whole false) the issue's text and the answers that
// amend it: the findings the session started from are judged once
// (roadmap 201).
func (r *Runner) askSupervisor(ctx context.Context, t Task, s sessionSpec, recorder *SessionRecorder, digest *digestBuilder, trip *Trip, call, maxCalls int, messages []string, whole bool) Decision {
	brief := harness.TaskBrief{Objective: briefObjective(t.Objective, false), Repository: t.Repository, SHA: s.startRef,
		Answers: newest(s.answers, maxCarriedAnswers, false), PosterInfo: newest(s.posterInfo, maxCarriedAnswers, true),
		Author: t.Author, AuthorStanding: cmp.Or(s.standing, t.Standing())}
	if whole {
		brief.Findings = newest(s.findings, maxCarriedFindings, true)
	}
	return r.supervisor.Decide(ctx, t.ID, factoryModel(s.policy, t, policy.PurposeSupervisor), s.policy.Limits.SupervisorTimeout, SupervisorInput{
		Brief:    InboundBrief(brief),
		Trip:     TripReport{Detector: trip.Kind, Detail: trip.Detail},
		Digest:   digest.digest(),
		Record:   recorder.SupervisorTail(maxRecordTail),
		Model:    s.model,
		Calls:    call,
		MaxCall:  maxCalls,
		Messages: messages,
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
	if trip.Kind == TripAgentEscalation || trip.Kind == TripAgentInfoRequest || trip.Kind == TripCheckIn {
		return // the agent's question (the workflow's escalation or info_request alert) or no detector
	}
	a := Alert{Kind: AlertSupervisorTrip, Source: string(trip.Kind), TaskID: t.ID, Verdict: tripVerdict(d.Action), ResolvedBy: "supervisor",
		Detail: map[string]any{"session": s.id, "action": string(d.Action), "diagnosis": cmp.Or(d.Reason, d.Guidance), "trip": trip.Detail, "called": called}}
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
	if _, err := r.forgejo.CreateIssueComment(ctx, repo.Owner, repo.Name, t.IssueNumber, r.untrusted(t, body)); err != nil {
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

// awaitPodStart waits for the pod to leave Pending. A container waiting
// on a cause no wait removes ends it with a *startFailure: at once, or,
// for a configuration error, once the same container has waited with it
// for r.configGrace, since the kubelet's view of a secret written just
// before the pod can lag. factoryd stopping ends it with errWorkerStop
// (the retry creates the pod anew); a timeout is a *podPending, which says
// what the pod waits on.
func (r *Runner) awaitPodStart(ctx context.Context, ns, podName string, timeout time.Duration, workerStop <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	// configSince is when each container's configuration error began
	configSince := map[string]time.Time{}
	var last PodDiagnosis
	for now := time.Now(); now.Before(deadline); now = time.Now() {
		d, err := r.kube.PodDiagnosis(ctx, ns, podName)
		if err == nil && d.Phase != "" && d.Phase != "Pending" {
			return nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			last = d
		}
		for _, c := range d.Containers {
			step := startStep(c)
			if step == stepContainerConfig {
				since, seen := configSince[c.Name]
				if !seen {
					configSince[c.Name] = now
				}
				if !seen || now.Sub(since) < r.configGrace {
					continue
				}
			} else {
				delete(configSince, c.Name)
			}
			if step != "" {
				return &startFailure{step: step, text: "start failed: " + step + ": " + c.waiting()}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-workerStop:
			return errWorkerStop
		case <-time.After(min(r.startPoll, time.Until(deadline))):
		}
	}
	return &podPending{pod: podName, after: timeout, on: waitsOn(last)}
}

// podPending is a pod still Pending when its start's wait ran out: on is
// what it waits on (waitsOn), a node with the capacity it requests or a
// pull that has not failed for good. No container of it ran.
type podPending struct {
	pod   string
	after time.Duration
	on    string
}

func (p *podPending) Error() string {
	why := fmt.Sprintf("pod %s still pending after %s", p.pod, p.after)
	if p.on != "" {
		why += ": " + p.on
	}
	return why
}

// configErrorGrace is how long a container may wait with a configuration
// error before the start fails.
const configErrorGrace = 30 * time.Second

// waitsOn says what a pending pod waits on: the scheduler's refusal, or
// its first container not done, init containers first.
func waitsOn(d PodDiagnosis) string {
	for _, c := range d.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" {
			return c.Reason + ": " + c.Message
		}
	}
	for _, c := range d.Containers {
		switch c.State {
		case "waiting":
			return c.waiting()
		case "running":
			return "container " + c.Name + " runs"
		}
	}
	return ""
}

// exitedHow says how a pod ended without a result, plain the generic
// words: its eviction, or plain and the first container that ended in
// failure, init containers first; "" when its diagnosis says no more.
func exitedHow(plain string, d PodDiagnosis) string {
	if d.Reason == "Evicted" {
		return "evicted: " + d.Message
	}
	for _, c := range d.Containers {
		if c.State == "terminated" && (c.ExitCode != 0 || c.Reason != "Completed") {
			s := fmt.Sprintf("%s: container %s terminated with %s (exit code %d)", plain, c.Name, c.Reason, c.ExitCode)
			if c.Message != "" {
				s += ": " + c.Message
			}
			return s
		}
	}
	return ""
}

// The start steps a pod can fail at for good.
const (
	stepImagePull       = "image pull"
	stepContainerConfig = "container config"
)

// StepEgressProbe is the worker's start step that holds the sandbox to its
// egress policy (roadmap 42).
const StepEgressProbe = "egress probe"

// startStep is the step container c waits at when no wait gets it past:
// an image reference that does not parse, an image or manifest the
// registry does not hold, or a configuration the kubelet will not create
// the container with; "" while a wait may (a registry that does not
// answer, a pull still running). ImagePullBackOff's message carries the
// last pull's error.
func startStep(c ContainerState) string {
	if c.State != "waiting" {
		return ""
	}
	switch c.Reason {
	case "InvalidImageName":
		return stepImagePull
	case "ErrImagePull", "ImagePullBackOff":
		if strings.Contains(c.Message, "code = NotFound") || strings.Contains(c.Message, "manifest unknown") {
			return stepImagePull
		}
	case "CreateContainerConfigError":
		return stepContainerConfig
	}
	return ""
}

// startFailure is a session that cannot start: the start step that
// failed, the pod's or the worker's, and its text, "start failed: <step>:
// <cause>".
type startFailure struct {
	step, text string
}

func (f *startFailure) Error() string { return f.text }

// workerStartFailure is the start failure a worker's result reports: a
// *startFailure, or, for a fault a later start may not meet (Transient),
// a plain error the activity's retry answers, as any other transient
// start fault; nil when its start did not fail.
func workerStartFailure(res harness.Result) error {
	switch {
	case res.Success || res.Step == "":
		return nil
	case res.Transient:
		return errors.New(res.Summary)
	}
	return &startFailure{step: res.Step, text: res.Summary}
}

// startRefused opens the escalation of a session that cannot start.
const startRefused = "the sandbox cannot start at "

// The ways on a start failure's escalation: a code or plan session's, and
// a split's, which a split of the pull request starts again.
const (
	thenAnswer = "then answer to start a session again, or close the card"
	thenSplit  = "then split the pull request again, or approve or deny it"
	thenRepro  = "then answer to run the reproduction again, or close the card"
)

// escalation is the failure's escalation reason, then the way on: its
// first line is the card's question row (escalationQuestion), with the
// action.
func (f *startFailure) escalation(then string) string {
	act := "fix the cause below"
	switch f.step {
	case stepImagePull:
		act = "push the worker image or correct its reference"
	case stepContainerConfig:
		act = "correct the sandbox pod's configuration"
	case StepEgressProbe:
		act = "check the task namespace's " + workerEgress + " policy, or the pod's uid, against the probe named below"
	}
	return fmt.Sprintf("%s%s, and no session can fix it: %s, %s.\n%s", startRefused, f.step, act, then, f.text)
}

// setupNamespace builds the session's sandbox: base is the read token of
// the pod's clone or fetch init container.
func (r *Runner) setupNamespace(ctx context.Context, t Task, base ForgejoToken) (string, error) {
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", terminalError("task %s: repository %s is no project's", t.ID, t.Repository)
	}
	cache := "cache-agent-" + p.Name
	ns := r.agentNamespace(t.ID)
	if err := r.freshNamespace(ctx, ns, map[string]string{
		"factory.ai/task":                    t.ID,
		"pod-security.kubernetes.io/enforce": "restricted",
	}); err != nil {
		return "", fmt.Errorf("setup namespace: %w", err)
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
			return r.kube.CreateSecret(ctx, ns, "task-git", "", map[string]string{"REPO_TOKEN": base.Secret})
		}},
		{"pull secret", func() error {
			return copyPullSecret(ctx, r.kube, r.cfg.FactoryNamespace, r.cfg.PullSecret, ns)
		}},
		{"cache claim", func() error {
			return r.kube.Create(ctx, core+"/persistentvolumeclaims", cacheClaim(cache))
		}},
	}
	for _, step := range steps {
		if err := retryForbidden(ctx, step.do); err != nil && !errors.Is(err, ErrAlreadyExists) {
			return "", fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	return cache, nil
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
// terminating when the resumed one starts). 4Gi: a go test of two large
// packages beside the harness passed 2Gi, and a cgroup OOM kills the whole
// container with the session's unpushed work.
var (
	sandboxRequests = map[string]any{"cpu": "250m", "memory": "512Mi"}
	sandboxLimits   = map[string]any{"cpu": "2", "memory": "4Gi"}
	sandboxQuota    = map[string]any{
		"pods":            "2",
		"requests.cpu":    "1",
		"requests.memory": "2Gi",
		"limits.cpu":      "4",
		"limits.memory":   "8Gi",
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
	deleteReleasedCaches(ctx, r.kube)
}

func (r *Runner) agentNamespace(taskID string) string {
	return r.cfg.AgentNamespacePrefix + taskID
}

// taskBranch is task t's branch on the bot's fork.
func taskBranch(t Task) string { return "factory/" + t.ID }

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

// maxEnvString is Linux's bound on one "NAME=value" string of a
// process's environment, its NUL included (MAX_ARG_STRLEN): exec refuses
// a longer one, and the session's container never starts.
const maxEnvString = 128 << 10

// envRoom is how many bytes env var name's value may take.
func envRoom(name string) int { return maxEnvString - len(name) - len("=") - len("\x00") }

// The carried caps (roadmap 201): the most of each list a card's sessions
// build up that a session's models read, the oldest left out first, as a
// session keeps its own last 20 findings (noteFinding): room for the
// findings from outside the sessions and the one session's own lines the
// workflow carries, and for 20 answers of each kind. They bound the
// context, envFit the bytes exec carries: it trims a capped list only
// when its entries are long (20 answers of MaxAnswer take about 90 KiB, a
// finding has no bound of its own), and one note counts what both left
// out.
const (
	maxCarriedFindings = 60
	maxCarriedAnswers  = 20
)

// newest is items' newest n, items oldest first when oldestFirst.
func newest(items []string, n int, oldestFirst bool) []string {
	switch {
	case len(items) <= n:
		return items
	case oldestFirst:
		return items[len(items)-n:]
	}
	return items[:n]
}

// envFit fits the worker env's values that grow with their input within
// envRoom, and keeps factoryd's note on each it shortened: the worker gets
// them as LEFT_OUT and the brief says them in factoryd's own words,
// outside every quoted block.
type envFit struct{ notes []string }

// list is items' JSON for env var name, at most most of them: its whole
// items left out from the front (dropFront) or the back until it fits;
// which names the end they leave from in the note, what the items.
func (f *envFit) list(name string, items []string, most int, dropFront bool, which, what string) string {
	for n := max(len(items)-most, 0); ; n++ {
		keep := items[:len(items)-n]
		if dropFront {
			keep = items[n:]
		}
		b, _ := json.Marshal(keep)
		if len(b) <= envRoom(name) || n == len(items) {
			if n > 0 {
				f.notes = append(f.notes, fmt.Sprintf("%d of the %d %s, the %s", n, len(items), what, which))
			}
			return string(b)
		}
	}
}

// text is s for env var name, cut to fit; what names it in the note.
func (f *envFit) text(name, s, what string) string {
	if len(s) <= envRoom(name) {
		return s
	}
	cut := textutil.Truncate(s, envRoom(name)-len("…"))
	f.notes = append(f.notes, fmt.Sprintf("%s past its first %d of %d bytes", what, len(cut)-len("…"), len(s)))
	return cut
}

func (r *Runner) workerPodSpec(t Task, s sessionSpec) map[string]any {
	toolsJSON, _ := json.Marshal(s.policy.Tools(t.RequiredCapabilities))
	var fit envFit
	env := []map[string]any{
		{"name": "TASK_ID", "value": t.ID},
		{"name": "SESSION_ID", "value": s.id},
		{"name": "OBJECTIVE", "value": fit.text("OBJECTIVE", briefObjective(t.Objective, len(s.directories) > 0), "the issue's text")},
		{"name": "REPOSITORY", "value": t.Repository},
		{"name": "REPO_SHA", "value": s.startRef},
		{"name": "BRANCH", "value": taskBranch(t)},
		{"name": "TOKEN_FILE", "value": "/var/run/secrets/tokens/gateway"},
		{"name": "MODEL", "value": s.model},
		{"name": "STRONGER_MODEL", "value": r.cfg.Models.Stronger},
		{"name": "CHEAP_MODEL", "value": r.cfg.Models.Start},
		{"name": "ANSWER_TOKENS", "value": strconv.Itoa(r.cfg.AnswerTokens)},
		{"name": "HEADROOM_TOKENS", "value": strconv.Itoa(r.cfg.HeadroomTokens)},
		{"name": "STEPS", "value": strconv.Itoa(s.policy.Limits.StepCap())},
		{"name": "MESSAGE_TOKEN_SHA256", "value": messageTokenHash(s.messageToken)},
		{"name": "FINDINGS", "value": fit.list("FINDINGS", s.findings, maxCarriedFindings, true, "oldest", "findings")},
		{"name": "ANSWERS", "value": fit.list("ANSWERS", s.answers, maxCarriedAnswers, false, "oldest", "answers to the escalations")},
		{"name": "POSTER_INFO", "value": fit.list("POSTER_INFO", s.posterInfo, maxCarriedAnswers, true, "oldest", "answers of the issue's author")},
		{"name": "TOOLS", "value": string(toolsJSON)},
		{"name": "AUTHOR", "value": t.Author},
		{"name": "AUTHOR_STANDING", "value": cmp.Or(s.standing, t.Standing())},
	}
	cloneEnv := []map[string]any{
		{"name": "MODE", "value": "clone"},
		{"name": "REPO_URL", "value": r.repoURL(s.repo)},
		{"name": "REPO_SHA", "value": s.startRef},
		{"name": "BRANCH", "value": taskBranch(t)},
		{"name": "REPO_TOKEN", "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": "task-git", "key": "REPO_TOKEN"}}},
	}
	if s.docs != (Repo{}) {
		cloneEnv = append(cloneEnv, map[string]any{"name": "DOCS_URL", "value": r.repoURL(s.docs)})
		env = append(env, map[string]any{"name": "DOCS_DIR", "value": "/workspace/docs"})
	}
	if s.pull != 0 {
		cloneEnv = append(cloneEnv, map[string]any{"name": "PULL", "value": strconv.FormatInt(s.pull, 10)})
	}
	if t.PRNumber != 0 {
		cloneEnv = append(cloneEnv, map[string]any{"name": "TASK_PR", "value": strconv.FormatInt(t.PRNumber, 10)})
	}
	if s.report != "" {
		env = append(env, map[string]any{"name": "MODE", "value": "report"}, map[string]any{"name": "REPORT_GUIDE", "value": fit.text("REPORT_GUIDE", s.report, "the report's guide")})
	} else {
		fork := r.repoURL(r.forgejo.Fork(s.repo))
		cloneEnv = append(cloneEnv, map[string]any{"name": "PUSH_URL", "value": fork})
		env = append(env, map[string]any{"name": "PUSH_URL", "value": fork})
	}
	if s.parent != "" {
		env = append(env, map[string]any{"name": "PARENT_SESSION_ID", "value": s.parent})
	}
	if s.facts != "" {
		env = append(env, map[string]any{"name": "FACTS", "value": s.facts})
	}
	if s.spec != "" {
		env = append(env, map[string]any{"name": "SPEC", "value": fit.text("SPEC", s.spec, "the spec and the plan")}, map[string]any{"name": "CARD_ROLE", "value": s.cardRole})
	}
	if len(s.directories) > 0 {
		env = append(env, map[string]any{"name": "DIRECTORIES", "value": fit.list("DIRECTORIES", s.directories, len(s.directories), false, "last", "directories")})
	}
	// the worker's migration-name guard reads these (roadmap 186), where
	// the guide's naming sentence used to be
	if globs := s.policy.Risk.MigrationGlobs(); len(globs) > 0 {
		globsJSON, _ := json.Marshal(globs)
		env = append(env, map[string]any{"name": "MIGRATION_GLOBS", "value": string(globsJSON)})
	}
	if r.cfg.OTLPEndpoint != "" {
		env = append(env, map[string]any{"name": "OTEL_EXPORTER_OTLP_ENDPOINT", "value": r.cfg.OTLPEndpoint})
	}
	if len(fit.notes) > 0 {
		notes, _ := json.Marshal(fit.notes)
		env = append(env, map[string]any{"name": "LEFT_OUT", "value": string(notes)})
	}
	pod := r.sandboxPodSpec(t, s.cache, env, true)
	// the clone init container alone holds the read token: the agent's
	// container gets the clone in the workspace and no token (the push
	// token reaches the worker on POST /push once the agent is dead)
	spec := pod["spec"].(map[string]any)
	clone := maps.Clone(spec["containers"].([]any)[0].(map[string]any))
	clone["name"] = "clone"
	clone["env"] = cloneEnv
	clone["volumeMounts"] = []any{map[string]any{"name": "workspace", "mountPath": "/workspace"}}
	spec["initContainers"] = []any{clone}
	return requestEmptyDirs(pod)
}

// SandboxUID is the sandbox's uid and gid: its pods' runAsUser and
// runAsGroup, Dockerfile.sandbox's USER, and what the worker's egress
// probe holds itself to.
const SandboxUID = 1000

// sandboxPodSpec is a gVisor worker pod in the task namespace; only an
// agent session gets the gateway token, and cache names the project's
// shared cache claim ("" none). Every sandbox pod carries the
// worker label the egress policy selects: without it no policy would
// confine its egress.
func (r *Runner) sandboxPodSpec(t Task, cache string, env []map[string]any, gatewayToken bool) map[string]any {
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
		// the agent runs the checks CI runs, so its tmp is the CI pod's:
		// 1 GiB evicted a session building factoryd's race tests (#816)
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "4Gi"}},
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
	if cache != "" {
		mounts, volumes = append(mounts, cacheMounts...), append(volumes, cacheVolumes(cache)...)
		env = append(env, cacheEnv...)
	}
	// the worker probes the sandbox's egress before the repository's code
	// runs, in a session and in a reproduction alike (roadmap 42)
	env = append(env,
		map[string]any{"name": "GATEWAY_URL", "value": r.cfg.GatewayURL},
		map[string]any{"name": "EGRESS_CANARY", "value": r.cfg.EgressCanary},
		map[string]any{"name": "EGRESS_DIRECT_IP", "value": r.cfg.EgressDirectIP})
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
				"command": []any{"/bin/sh", "-c", cacheUmaskSh, "/usr/local/bin/agent-worker"},
				"env":     env,
				"resources": map[string]any{
					"requests": sandboxRequests,
					"limits":   sandboxLimits,
				},
				"volumeMounts": mounts,
				"securityContext": map[string]any{
					"runAsNonRoot":             true,
					"runAsUser":                SandboxUID,
					"runAsGroup":               SandboxUID,
					"allowPrivilegeEscalation": false,
					"capabilities":             map[string]any{"drop": []any{"ALL"}},
					"readOnlyRootFilesystem":   true,
				},
			}},
			"volumes": volumes,
		},
	}
}

// requestEmptyDirs has pod request, as ephemeral-storage, the sum of its
// emptyDir sizeLimits (each in whole Gi), so the scheduler counts what
// they may fill against a node's allocatable. Its one container and its
// init containers each request it, as they share every other request: a
// pod's request is the larger of its init containers' and its containers'
// sum, so the pod's is the sum.
func requestEmptyDirs(pod map[string]any) map[string]any {
	spec := pod["spec"].(map[string]any)
	gi := 0
	for _, v := range spec["volumes"].([]any) {
		if dir, ok := v.(map[string]any)["emptyDir"].(map[string]any); ok {
			limit, _ := dir["sizeLimit"].(string)
			n, _ := strconv.Atoi(strings.TrimSuffix(limit, "Gi"))
			gi += n
		}
	}
	for _, key := range []string{"initContainers", "containers"} {
		list, _ := spec[key].([]any)
		for _, c := range list {
			res := c.(map[string]any)["resources"].(map[string]any)
			requests := maps.Clone(res["requests"].(map[string]any))
			requests["ephemeral-storage"] = fmt.Sprintf("%dGi", gi)
			res["requests"] = requests
		}
	}
	return pod
}

// workerEgressSpec is the sandbox network policy (architecture 6.4, research
// Q4): default-deny egress for workers, a hard deny to the cloud metadata
// address, and exactly four allows: the model gateway's port, Forgejo's
// HTTP port, the Go module proxy, its checksum database and the npm
// registry by name on 443 (ruling 99: the image carries the Go toolchain
// and Node, and a registry joins by name with the toolchain that needs
// it), and kube-dns for exactly those
// names. DNS is allowlisted by name because an open resolver is itself an
// exfiltration channel. No other registry is reachable. opencode's first-run
// npm install (its @opencode-ai/plugin dependency) is baked into the image at
// build time from opencode-plugin/'s checked-in lockfile, so opencode itself
// never installs at runtime. Workers reach
// Forgejo through its in-cluster service, not the public host: in-cluster
// DNS answers that host with the gateway ClusterIP, which Cilium's toFQDNs
// cannot authorize (the envoy fronting it answers 403), while the service
// path is identity-scoped. The live gateway pods carry the plain `app`
// label, not app.kubernetes.io/name. Its one ingress admits factoryd on the
// worker's message port; every other ingress is denied.
func (r *Runner) workerEgressSpec(leased []string) map[string]any {
	dnsNames := []any{}
	for _, name := range r.sandboxNames() {
		dnsNames = append(dnsNames, map[string]any{"matchName": name})
	}
	toFQDNs := []any{}
	for _, host := range sandboxRegistries {
		toFQDNs = append(toFQDNs, map[string]any{"matchName": host})
	}
	spec := map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": map[string]string{"app.kubernetes.io/name": "opencode-worker"},
		},
		// the one ingress: factoryd's messages into the running session
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": map[string]string{
				"k8s:io.kubernetes.pod.namespace": r.cfg.FactoryNamespace,
				"app":                             "factoryd",
			}}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": strconv.Itoa(harness.MessagePort), "protocol": "TCP"}},
			}},
		}},
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
	if r.cfg.OTLPEndpoint != "" {
		spec["egress"] = append(spec["egress"].([]any), map[string]any{
			"toEndpoints": []any{map[string]any{
				"matchLabels": map[string]string{
					"k8s:io.kubernetes.pod.namespace": "platform",
					"app.kubernetes.io/instance":      "signoz",
					"app.kubernetes.io/component":     "otel-collector",
				},
			}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "4318", "protocol": "TCP"}},
			}},
		})
	}
	for _, ip := range leased {
		spec["egress"] = append(spec["egress"].([]any), leasedServerRule(ip))
	}
	return spec
}

const workerEgress = "worker-egress"

// sandboxRegistries are the public names the sandbox reaches on 443;
// storage.googleapis.com: proxy.golang.org redirects zip downloads there.
var sandboxRegistries = []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "registry.npmjs.org"}

// sandboxNames are the names the sandbox's DNS allowlist answers: the
// gateway's, Forgejo's, the registries' and the collector's.
func (r *Runner) sandboxNames() []string {
	var names []string
	for _, raw := range []string{r.cfg.GatewayURL, r.cfg.GitBaseURL} {
		u, _ := url.Parse(raw) // validated in NewRunner
		names = append(names, u.Hostname())
	}
	names = append(names, sandboxRegistries...)
	if r.cfg.OTLPEndpoint != "" {
		u, _ := url.Parse(r.cfg.OTLPEndpoint) // validated in NewRunner
		names = append(names, u.Hostname())
	}
	return names
}

// CheckEgressTargets checks the egress probe's targets: the canary is a
// name and a port, as its name is the DNS probe's too, and the direct IP
// an IP and a port.
func CheckEgressTargets(canary, directIP string) error {
	if host, ok := hostPort(canary); !ok || host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("the egress canary must be a name and a port, is %q", canary)
	}
	if host, ok := hostPort(directIP); !ok || net.ParseIP(host) == nil {
		return fmt.Errorf("the egress direct IP must be an IP and a port, is %q", directIP)
	}
	return nil
}

// hostPort splits s into its host and a port from 1 to 65535.
func hostPort(s string) (string, bool) {
	host, port, err := net.SplitHostPort(s)
	n, perr := strconv.Atoi(port)
	return host, err == nil && perr == nil && n > 0 && n < 1<<16
}

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
	infos, err := r.kube.ListNamespacesByLabel(ctx, "factory.ai/task", "")
	if err != nil {
		return fmt.Errorf("list agent namespaces: %w", err)
	}
	var errs []error
	for _, info := range infos {
		ns := info.Name
		id := strings.TrimPrefix(ns, r.cfg.AgentNamespacePrefix)
		if info.Terminating {
			// its contents are being garbage-collected away: a policy
			// write there fails and the namespace needs none any more.
			// A wait that holds a live lease back is observable in
			// factoryd's log and on the task's timeline (roadmap 188);
			// an ordinary teardown holds nothing back and says nothing.
			if len(leased[id]) > 0 {
				r.observeNamespaceWait(ctx, ns, leased[id])
			}
			continue
		}
		if id == ns || strings.HasPrefix(id, "ci-") {
			continue
		}
		want := r.workerEgressSpec(leased[id])
		have, err := r.kube.CiliumNetworkPolicySpec(ctx, ns, workerEgress)
		if errors.Is(err, ErrForbidden) {
			// no RoleBinding yet or any more: setupNamespace applies the
			// policy itself, and a torn-down namespace needs none
			continue
		}
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

// observeNamespaceWait makes the lease admission's wait out of a terminating
// namespace observable: factoryd's log names the namespace and the live
// leases it holds back, and the task's timeline carries one namespace_wait
// step per round that still found it terminating.
func (r *Runner) observeNamespaceWait(ctx context.Context, ns string, leases []string) {
	id := strings.TrimPrefix(ns, r.cfg.AgentNamespacePrefix)
	if id == ns || id == "" {
		return // not an agent namespace: no task to record on
	}
	log.Printf("task %s: namespace %s is terminating: waited out; its egress policy admits none of the leased servers %v until it is gone or active again", id, ns, leases)
	r.record(ctx, id, "namespace_wait", map[string]any{"namespace": ns, "waited": "terminating", "leases": leases})
}

// BindTaskNamespace puts factoryd's RoleBinding into a task namespace, as
// setupNamespace does before it writes anything there: an acceptance test
// scripts the namespaces the production client reads and writes.
func (r *Runner) BindTaskNamespace(ctx context.Context, ns string) error {
	return r.kube.Create(ctx, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+ns+"/rolebindings", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "factoryd"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "factoryd-task"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "factoryd", "namespace": r.cfg.FactoryNamespace}},
	})
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

// RecordCommissioned puts each agent server gone live on its task's
// timeline: the commissioning gate, from the request to the server's live
// moment.
func (r *Runner) RecordCommissioned(ctx context.Context) error {
	cs, err := r.cfg.Leases.ClaimCommissioned(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		r.record(ctx, c.TaskID, "commissioned", map[string]any{"resource": c.ID, "server_type": c.ServerType,
			"started": rfc3339(c.Requested), "live": rfc3339(c.Live)})
	}
	return nil
}

// LeaseLoop runs AdmitLeases and RecordCommissioned every interval until
// ctx ends.
func (r *Runner) LeaseLoop(ctx context.Context, every time.Duration) {
	for {
		if err := r.AdmitLeases(ctx); err != nil {
			log.Printf("lease admission: %v", err)
		}
		if err := r.RecordCommissioned(ctx); err != nil {
			log.Printf("commissioning on the timeline: %v", err)
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

// watchLimits reads the card's spend once per tick and sends the agent the
// warning it is due; a warning the pod did not take stays due.
func (r *Runner) watchLimits(ctx context.Context, t Task, s sessionSpec, ns, pod string, w *limitWatch) *Trip {
	cur, ok, err := r.store.Task(ctx, t.ID)
	if err != nil || !ok {
		log.Printf("task %s: read the card's spend: %v", t.ID, err)
		return nil
	}
	for _, wn := range w.due(cur.SpentEUR, cur.BudgetEUR, time.Now()) {
		if err := r.message(ctx, t, s, ns, pod, wn.text); err != nil {
			log.Printf("task %s: send the %s warning: %v", t.ID, wn.kind, err)
			continue
		}
		w.sent(wn.kind)
	}
	return nil
}
