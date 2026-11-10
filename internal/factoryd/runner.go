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
	"io"
	"log"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/cloudres"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/gateway"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// RunnerConfig carries the per-cluster sandbox facts and the task policy the
// workflow runs under. Defaults are set from the environment in
// cmd/factoryd; tests set them explicitly.
type RunnerConfig struct {
	AgentNamespacePrefix string // "factory-agents-"
	WorkerImage          string // pinned digest
	SandboxBaseImage     string // pinned digest of sf/sandbox-base, what project images build FROM
	RegistryHost         string // the store's host, where project images live (REGISTRY_HOST)
	PullSecret           string // registry pull secret copied into task namespaces
	FactoryNamespace     string // where the pull secret lives
	GatewayURL           string // http://factory-gateway.factory.svc.cluster.local:8081
	GitBaseURL           string // http://forgejo-http.forgejo.svc.cluster.local:3000 — sandbox clones; the public host's in-cluster DNS answer is the gateway ClusterIP, which FQDN policy cannot authorize
	Models               ModelLadder
	AnswerTokens         int
	HeadroomTokens       int
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
	// Timeline records the cards' events; nil builds the store's own.
	Timeline *Timeline
	// OTLPEndpoint is SigNoz's collector (OTLP HTTP, port 4318), where the
	// workers send their telemetry; "" = none.
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
	// Prices is the alias price book the attempt grant's floor reads:
	// whether the task's remaining budget can fund a call at
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
	// deltaLast is the plan the delta pass visited last; only that pass reads it
	deltaLast  string
	cfg        RunnerConfig
	store      Store
	kube       Kube
	forgejo    *Forgejo
	completion *Completion
	objects    ObjectStore
	timeline   *Timeline
	supervisor *SupervisorModel
	// checkin is the check-in's model client, wired like the supervisor's
	// (the gateway under the task's account); a test scripts it
	checkin checkinChat
	// gateQueue holds the plans whose criteria gate is due, which the
	// close check fills under gateMu and CriteriaGatePass takes
	gateMu    sync.Mutex
	gateQueue []string
	// criteriaGate replaces GateCriteria in the criteria gate pass; a
	// test scripts it
	criteriaGate func(ctx context.Context, taskID string, pr int64, commit string) (CriteriaResult, error)
	// endpointWait bounds how long the worker's message endpoint is given
	// to come up for a brief, record or proofs list delivery; a test
	// shortens it
	endpointWait time.Duration
	stopWait     time.Duration
	// checksMargin is how long before its activity deadline WaitChecks ends
	// its poll with the checks timeout
	checksMargin time.Duration
	cheapCap     time.Duration // cheapAttemptCap; a test shortens it
	tick         time.Duration // the supervision tick; a test sets it
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
	// revertLocks holds a *sync.Mutex by project, so RevertGroup runs
	// once at a time per project
	revertLocks sync.Map
	// caseLocks holds a *sync.Mutex by case id, so Act answers one case
	// once at a time (one factoryd replica)
	caseLocks sync.Map
	// retryLocks holds a *sync.Mutex by task id, so RetryCard files a
	// failed card's replacement once at a time, however many presses
	// race it (one factoryd replica)
	retryLocks sync.Map
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
	// Imager verifies a project image before a pod runs it
	// (verifyProjectImage); set before the worker starts.
	Imager *Imager
	// Upstream copies a branch's new upstream image lines into the store
	// before its checks; nil copies nothing
	Upstream *UpstreamCopier
	// Scrub scrubs every session record. NewRunner's knows no secret
	// value; factoryd sets the one built from its boot secrets before the
	// worker starts.
	Scrub *redact.Scrubber
	// Writes reports whether login writes repository's code
	// (Intake.Writes), the trust rule of a person's close of a plan's
	// issue; factoryd sets it before the minute pass starts.
	Writes func(ctx context.Context, repository, login string) (bool, error)
	// Namer names a plan when its first revision files; nil names none.
	Namer *Namer
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
	// replan: a replan ask is on its way to the session or was taken.
	replan bool
}

// sessionWorker is a running session's pod and the bearer of its worker's
// message endpoint; report marks a report session, whose card restarts on
// no image, and cuttable a session whose card the planner can cut
// (cuttable).
type sessionWorker struct {
	session, ns, pod, token string
	report, cuttable        bool
}

// ErrNoLiveSession: the task has no session running in this factoryd.
var ErrNoLiveSession = errors.New("the task has no running session")

// errReplanAsked: the task's running session took a replan ask already,
// or one is on its way, and stops for it.
var errReplanAsked = errors.New("the session already stops for a replan")

// ErrNoImageWait refuses a report session's request_tool: its card
// restarts on no image.
var ErrNoImageWait = errors.New("this session cannot wait for a new image: finish with what the sandbox has")

// ErrNoCut refuses the agent's cut of a session whose card cannot be cut
// (cuttable).
var ErrNoCut = errors.New("this card cannot be cut: finish it as it is, or escalate")

// EscalateSession hands an agent's escalate to its task's running
// session, which stops the way a supervisor's escalate stops it, or its
// request_cut, which the session's check-in judges. It returns nil only
// once the session took the ask; a session that ends or stops first
// answers ErrNoLiveSession, a report session's request_tool
// ErrNoImageWait, and a cut of a session that cannot be cut ErrNoCut. A
// session takes one replan ask: a later one answers errReplanAsked at
// once.
func (r *Runner) EscalateSession(ctx context.Context, taskID string, ask AgentAsk) error {
	replan := ask.Kind == AskReplan
	r.liveMu.Lock()
	ls, ok := r.live[taskID]
	asked := ok && replan && ls.replan
	if ok && replan {
		ls.replan = true
	}
	r.liveMu.Unlock()
	switch {
	case !ok:
		return ErrNoLiveSession
	case asked:
		return errReplanAsked
	case ask.Kind == AskTool && ls.worker.report:
		return ErrNoImageWait
	case ask.Kind == AskCut && !ls.worker.cuttable:
		return ErrNoCut
	}
	select {
	case ls.ch <- ask:
		return nil
	case <-ls.done:
		return ErrNoLiveSession
	case <-ctx.Done():
		if replan {
			r.liveMu.Lock()
			ls.replan = false
			r.liveMu.Unlock()
		}
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
	if cfg.AgentNamespacePrefix == "" || cfg.WorkerImage == "" || cfg.SandboxBaseImage == "" || cfg.RegistryHost == "" || cfg.GatewayURL == "" || cfg.GitBaseURL == "" || cfg.Leases == nil {
		return nil, fmt.Errorf("runner: AgentNamespacePrefix, WorkerImage, SandboxBaseImage, RegistryHost, GatewayURL, GitBaseURL and Leases are required")
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
	if canary, ok := (&Runner{cfg: cfg}).reachesCanary((&Runner{cfg: cfg}).sandboxNames(sandboxRegistries)); ok {
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
	if cfg.Timeline == nil {
		cfg.Timeline = &Timeline{Objects: objects, Metrics: cfg.Metrics, Events: store, Store: store}
	}
	runner := &Runner{cfg: cfg, store: store, kube: kube, forgejo: forgejo, completion: completion, supervisor: supervisor,
		checkin: &checkinGateway{URL: supervisor.GatewayURL, Token: supervisor.Token}, endpointWait: checkinBriefWait,
		objects: objects, timeline: cfg.Timeline, stopWait: (workerTerminationGrace + 5) * time.Second, cheapCap: cheapAttemptCap,
		startPoll: 2 * time.Second, configGrace: configErrorGrace, booted: time.Now(), Scrub: redact.New(), checksMargin: checksPollMargin}
	return runner, nil
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
// repository, extra, the document repository when there is one, and each
// sandbox companion (a pin-only companion stays out: no clone, no read).
func (s sessionSpec) readable(extra ...Repo) []Repo {
	repos := append([]Repo{s.repo}, extra...)
	if s.docs != (Repo{}) {
		repos = append(repos, s.docs)
	}
	for _, c := range s.companions {
		if c.Sandbox {
			owner, name, _ := strings.Cut(c.Repository, "/")
			repos = append(repos, Repo{Owner: owner, Name: name})
		}
	}
	return repos
}

// sessionSpec is what one worker pod runs: its identity, the chain it
// continues, and where it starts.
type sessionSpec struct {
	id     string
	parent string
	// parentStop, parentOutcome and parentTrip are how the parent ended:
	// its stop reason and its trip, which the copied working state names,
	// and its outcome, which it names too and which decides whether one is
	// copied
	parentStop    string
	parentOutcome SessionOutcome
	parentTrip    string
	// parentDeadEnd: a check-in model judged the parent's approach wrong
	parentDeadEnd bool
	findings      []string
	answers       []string
	posterInfo    []string
	model         string
	startRef      string
	// base is the task's base commit: a session resumed from pushed work in
	// progress starts past it
	base string
	repo Repo
	// docs is the project's document repository (chat.doc_repo), which the
	// session reads at /workspace/docs; the zero Repo when it has none
	docs Repo
	// companions are the project's declared companions: a sandbox one the
	// session also reads, cloned to /workspace/companions/<name>
	companions []policy.Companion
	// cache is the project's cache claim in the task namespace
	cache string
	// image is what the session's project image holds; nil for the
	// factory's image
	image *imagegen.Manifest
	// policy is the task's pinned version's: its limits and detectors
	policy policy.Document
	// attempt is the session's attempt number (1 for the first)
	attempt int
	// sessions is the cap the attempt number counts against: the policy's
	// session limit and the attempts a human answer granted
	sessions int
	// planner marks a plan card's session: its warning says file the plan
	planner bool
	// planTrack: the card's project follows a process with a plan track,
	// so a direct card's rest can go to the planner
	planTrack bool
	// facts is factoryd's FACTS line for the brief
	facts string
	// agentTime and checkIn are the task's resolved values (policy.Time,
	// via TaskConfig for every session kind, RunSession's and
	// reportSession's alike): supervisorConfig's WallClockCap and
	// CheckIn, and the session's deadline.
	agentTime, checkIn time.Duration
	// report is a report session's guide; "" for a coding
	// session
	report string
	// standing overrides the task's author standing: a report reads its
	// parent's issue
	standing string
	// spec and cardRole are a plan's card's brief section and role
	spec, cardRole string
	// directories are the card's, which the worker maps from its clone
	directories []string
	// pull is the pull request a carry card carries, whose head the clone
	// holds as branch pull/<pull>; 0 for none
	pull int64
	// messageToken is the bearer of the worker's message endpoint, made
	// per session; the pod gets its hash
	messageToken string
	// checkInAsk is the check-in the workflow asks of the session's start;
	// nil: none runs
	checkInAsk *CheckInAsk
	// lastGates are the gates of the round that gated and sent this
	// session back to the agent; nil when no round gated
	lastGates *GateResults
	// handover is the note the workflow hands the session as its brief's
	// first section (a mid-session handover); "" when none
	handover string
	// branches are the fork's branches the clone fetches, each <fork
	// branch>:<local branch>: a replan planner's old cards' and salvage, a
	// plan child's salvage
	branches []string
	// record is what a planner session is handed in its brief's fence in
	// place of a parent's working state: the old cards' records, the work
	// so far; nil for none
	record *carryState
	// proofs are the plan's opened proofs the brief push carries into the
	// clone, each file the row's code: an integration or delta card's
	// alone
	proofs []harness.ProofFile
}

// sessionOutcome is how one sandbox session ended: a clean adapter verdict,
// a trip (with the supervisor's decision unless it was the wall-clock
// cap), or the pod dying without either.
type sessionOutcome struct {
	result *harness.Result
	// agentText is the agent's last message (the adapter's finished event)
	agentText string
	commit    string
	// wip is the last interim push's commit (wippush.go): the work the
	// session left on the branch without its own end push, which its
	// result keeps when the session died with no other
	wip      string
	trip     *Trip
	decision *Decision
	digest   string
	noResult string
	start    *startFailure   // the session could not start
	pending  *podPending     // its pod never left Pending: no session ran
	model    string          // the model the session ended on
	overHold *OverHoldCall   // the session's newest call booked beyond its hold
	reads    []string        // the paths a report session read
	checkIn  *CheckInOutcome // the check-in that ran at the session's start
}

// stopReport is what a stopped worker left for the next session beside
// the branch and the agent's last message (handoverLines): its report when
// nothing reached the branch, a refused push's reason among them, or why no
// report came, which says the work is not pushed only when no push was.
func (out sessionOutcome) stopReport() []string {
	switch {
	case out.result != nil && cmp.Or(out.commit, out.wip) == "":
		return []string{"the stopped worker reported: " + out.result.Summary}
	case out.result == nil && out.noResult != "" && cmp.Or(out.commit, out.wip) != "":
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
		recorder, err = ResumeSessionRecorder(ctx, r.store, r.objects, r.Scrub, t.ID, s.id)
	} else {
		recorder, err = NewSessionRecorder(ctx, r.store, r.objects, r.Scrub, t.ID, s.id, s.parent, s.model)
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
	// spentAtStart is the card's enforced spend at the session's real
	// start, nil where it could not be read
	var spentAtStart *float64
	if live != nil {
		podName, s.messageToken, consumed = live.Pod, live.MessageToken, live.Lines
		spentAtStart = live.SpentAtStart
	} else {
		if cur, ok, err := r.store.Task(ctx, t.ID); err != nil {
			log.Printf("task %s: session %s: read the card's spend at the session's start: %s", t.ID, s.id, r.Scrub.String(err.Error()))
		} else if ok {
			spent := cur.EnforcedSpent()
			spentAtStart = &spent
		}
		s.messageToken = rand.Text()
		s.facts = SessionFacts{BudgetEUR: t.BudgetEUR, SpentEUR: t.EnforcedSpent(), ReserveEUR: s.policy.Limits.GateReserve(), TimeLeft: time.Until(deadline),
			Attempt: s.attempt, Sessions: s.sessions, Size: t.Size, LargeLines: s.policy.Risk.LargeLines, Usage: t.Usage}.Line()
		if err := r.kube.CreatePod(ctx, ns, podName, r.workerPodSpec(t, s)); err != nil {
			return out, fmt.Errorf("create pod: %w", err)
		}
		if err := r.store.PutLiveSession(ctx, LiveSession{TaskID: t.ID, SessionID: s.id, Namespace: ns, Pod: podName,
			MessageToken: s.messageToken, StartedAt: time.Now(), Deadline: deadline, SpentAtStart: spentAtStart}); err != nil {
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

	// The worker holds the start of its coding conversation for a brief:
	// every session delivers one, with a handover note when there is one
	// and the stopped parent's working state. A reattached pod already
	// had its brief. A check-in that stops the session here stops it
	// before anything of the conversation is spent.
	if live == nil {
		// the check-in and the working state read the parent's record once
		parentRecord := sync.OnceValues(func() ([]byte, error) {
			rec, err := r.objects.Get(ctx, "sessions/"+t.ID+"/"+s.parent+".jsonl")
			if err != nil {
				log.Printf("task %s: read session %s's record: %v", t.ID, s.parent, err)
			}
			return rec, err
		})
		if s.checkInAsk != nil {
			var stop bool
			out.checkIn, stop = r.checkinBlock(ctx, t, s, ns, podName, workerStop, deadline, parentRecord)
			if stop {
				return out, nil
			}
		} else {
			r.pushBrief(ctx, workerStop, t.ID, ns, podName, s.messageToken, s.handover, priorState(s, parentRecord), s.proofs)
		}
	}

	// the supervisor's clocks start with the pod: a slow pull is no time
	// the agent had
	supCfg := r.supervisorConfig(s.policy, s.agentTime, s.checkIn)
	supCfg.ReadOnly = s.report != ""
	supCfg.PriorPush = s.startRef != s.base
	if spentAtStart != nil {
		supCfg.SpentAtStart = *spentAtStart
	} else {
		log.Printf("task %s: session %s: the card's spend at the session's start is unknown, so spend_no_push is off for this session", t.ID, s.id)
		supCfg.NoPushSpend = 0
	}
	if sizeBounded(t, s) {
		supCfg.SizeLines = EffectiveLineBound(t.Size, s.policy.Risk.LargeLines, t.Usage)
	}
	if s.attempt == 1 && r.canClimb(s) {
		supCfg.CheapCap = r.cheapCap
	}
	sup, err := NewSupervisor(supCfg, deadline.Add(-supCfg.WallClockCap), time.Now())
	if err != nil {
		return out, err
	}
	defer func() { out.model, out.reads = s.model, sup.Reads() }()
	watch := newLimitWatch(s.policy.Limits, s.planner, time.Now(), time.Until(deadline))
	digest := newDigestBuilder(time.Now())
	// messages: the guidance this session's delivered messages carried
	var messages []string

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
		case harness.EventCompacted:
			r.timeline.recordEvent(ctx, t.ID, EventCompacted, map[string]any{"session": s.id})
		}
		events++
		return env.Event, false
	}
	// stop asks the worker to end (SIGTERM through the pod's deletion) and
	// reads on until it has pushed its work in progress and reported, or
	// its grace period is over.
	// the agent's escalate reaches the session here until it stops
	escalations, unregister := r.liveSession(t.ID, sessionWorker{session: s.id, ns: ns, pod: podName, token: s.messageToken, report: s.report != "", cuttable: cuttable(t, s)})
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
	var onTrip func(trip *Trip) (bool, error)
	onTrip = func(trip *Trip) (end bool, err error) {
		if trip.Kind == TripWallClock {
			stop(trip)
			return true, ctx.Err()
		}
		// the agent's cut during the conversation does not cut it: the
		// answer stands, and the cut is the next trip of a session that runs
		// on. A later cut replaces it; a stopping session drops it.
		var held *AgentAsk
		defer func(judged TripKind) {
			switch {
			case held == nil:
			case end:
				log.Printf("task %s: the agent's cut during the check-in for %s is dropped: the session stops", t.ID, judged)
			default:
				end, err = onTrip(sup.AgentAsk(*held))
			}
		}(trip.Kind)
		// the conversation never outlives the session: the deadline cuts
		// it, and factoryd stopping cuts it, its answer unread
		callCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		var d Decision
		var ck *CheckInOutcome
		var ride bool
		// the agent was told "escalated" once the session took it: its
		// escalation during the conversation wins over whatever the
		// check-in answers, and cuts the conversation
		var esc *AgentAsk
		if trip.decided != nil {
			d = *trip.decided
		} else {
			// the session's work in progress ships before the check-in's
			// conversation opens: one interim push under factoryd's own
			// credential (interimPush, wippush.go), done — landed,
			// refused or given up — before the ask is built, so the ask
			// carries its outcome and the stop's end push cannot race it
			pctx, stopPush := context.WithCancel(callCtx)
			go func() {
				select {
				case <-workerStop:
					stopPush()
				case <-pctx.Done():
				}
			}()
			wip := r.interimPush(pctx, t, s, ns, podName, workerStop)
			stopPush()
			out.wip = cmp.Or(wip.head, out.wip)
			if wip.refusal != "" {
				// a push rule's refusal is a message the agent can act on:
				// it reaches the running session — a write answers it, so
				// an agent that ignores it meets the stuck rule — and the
				// ask's messages, so the check-in judges it too
				err := r.message(ctx, t, s, ns, podName, wip.refusal)
				if err == nil || errors.Is(err, harness.ErrNotRunning) {
					sup.messagedInTrip(time.Now())
					messages = append(messages, wip.refusal)
				} else {
					log.Printf("task %s: deliver the interim push's refusal to the session: %s", t.ID, r.Scrub.String(err.Error()))
				}
			}
			answered := make(chan struct{})
			lines, started := sup.CardLines(), sup.started
			go func() {
				defer close(answered)
				d, ck, ride = r.checkinLive(callCtx, t, s, ns, podName, recorder, trip, slices.Clone(messages), workerStop, deadline, lines, supCfg.SizeLines, started, wip.head)
			}()
			for answering := true; answering; {
				select {
				case <-answered:
					answering = false
					select {
					case e := <-escalations:
						esc = &e
					default:
					}
				case e := <-escalations:
					esc = &e
					if e.Kind != AskCut {
						cancel()
						<-answered
						answering = false
					}
				case <-workerStop:
					stopAt = time.Now()
					cancel()
					<-answered
					log.Printf("task %s: factoryd stopping cut the check-in for %s", t.ID, trip.Kind)
					return true, shutdown()
				}
				if esc != nil && esc.Kind == AskCut {
					held, esc = esc, nil
				}
			}
		}
		if esc != nil {
			log.Printf("task %s: the agent escalated during the check-in for %s", t.ID, trip.Kind)
			sup.tripped = nil
			trip = sup.AgentAsk(*esc)
			d = *trip.decided
			ck, ride = nil, false
		}
		// the judge says whether the session is stuck, code picks the model:
		// a stuck trip's handover is the stuck rule's climb
		if trip.Stuck != "" && ride && ck != nil && ck.Decision == CheckHandover {
			d, ride = Decision{Action: ActionRetryStronger, Reason: cmp.Or(ck.Handover, trip.Stuck), Guidance: ck.Handover, Judged: true}, false
		}
		out.digest = digest.digest().Summary()
		r.recordDecision(ctx, t, s, recorder, trip, d, ck, ride, out.digest)
		note := ""
		rode := false
		switch {
		case !time.Now().Before(deadline):
			ck, ride = nil, false
			stop(&Trip{Kind: TripWallClock, Findings: trip.Findings, Detail: "the task's wall-clock cap is reached"})
			note = "Superseded: the task's wall-clock cap was reached during the check-in."
		case ride:
			rode = true
			stop(trip)
		case d.Action == ActionContinue:
			sup.Resume(time.Now())
			r.commentDecision(ctx, t, s, trip, d, out.digest, "")
			if trip.Kind == TripAgentCut {
				// the agent was told the check-in judges its cut: it hears
				// the answer, and a failed delivery leaves it working on
				note := keepWhole(d.Reason)
				if err := r.message(ctx, t, s, ns, podName, note); err != nil {
					log.Printf("task %s: tell the agent its card stays whole: %v", t.ID, err)
				} else {
					messages = append(messages, note)
				}
			}
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
				sup.Messaged(time.Now())
				r.commentDecision(ctx, t, s, trip, d, out.digest, "")
				messages = append(messages, d.Guidance)
				return false, nil
			}
			out.decision = &Decision{Action: ActionRedirect, Guidance: d.Guidance}
			stop(trip)
			note = fmt.Sprintf("The message could not be delivered (%v): the session stopped, and the next one starts with the guidance.", err)
		default:
			out.decision = &d
			stop(trip)
			if d.Action == ActionRetryStronger {
				note = fmt.Sprintf("The session stopped on %s: %s. The next session resumes on the stronger model from the work in progress.", trip.Kind, cmp.Or(trip.Stuck, d.Reason))
			}
		}
		// a session that finished and pushed during the conversation goes to
		// verification: its answer is superseded, its record is kept
		if out.result != nil && out.result.Success && out.commit != "" {
			ck, ride = nil, false
			note = fmt.Sprintf("Superseded: the session finished and pushed %s during the check-in; the task goes to verification.", out.commit)
		}
		if ride {
			out.checkIn = ck
		}
		// the agent's own escalation reaches the issue through Escalate, and
		// a riding outcome's is the workflow's to deliver
		if note != "" || (trip.decided == nil && !rode) {
			r.commentDecision(ctx, t, s, trip, d, out.digest, note)
		}
		return true, ctx.Err()
	}

	tick := time.NewTicker(r.supervisionTick(supCfg))
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
				trip = r.watchLimits(ctx, t, s, ns, podName, watch, sup)
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

// keepWhole is the agent's answer when the check-in judges its cut and
// lets the session go on: the check-in's reason, and what to do.
func keepWhole(reason string) string {
	if reason = strings.TrimSuffix(strings.TrimSpace(reason), "."); reason != "" {
		reason = ": " + reason
	}
	return "The check-in keeps this card whole" + reason + ". Finish it as one card."
}

// checkinBlock runs the check-in the workflow asked of the session's
// start, inside the session's own pod and before its coding conversation:
// the limits are read before anything is bought, the parent session's
// record is delivered whole to the pod and its tail to the conversation,
// and a human or dropped-all decision stops the pod with nothing of the
// conversation spent. stop says the session ends here.
func (r *Runner) checkinBlock(ctx context.Context, t Task, s sessionSpec, ns, pod string, workerStop <-chan struct{}, deadline time.Time, parentRecord func() ([]byte, error)) (ck *CheckInOutcome, stop bool) {
	limits := s.policy.Limits
	ask := *s.checkInAsk
	ask.ParentSession = s.parent
	brief := func(note string) {
		r.pushBrief(ctx, workerStop, t.ID, ns, pod, s.messageToken, note, priorState(s, parentRecord), s.proofs)
	}
	share, eurLeft, _ := r.checkinBudget(ctx, t, limits)
	if eurLeft <= 0 {
		rec := &CheckInRecord{Model: r.callModel(t, policy.PurposeCheckin), Session: s.id,
			ParentSession: s.parent, Moment: ask.Moment, Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent}
		return &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
			Findings: ask.Findings, Review: ask.Review, Record: rec, Share: share}, true
	}
	if alias := r.callModel(t, policy.PurposeCheckin); alias != "" {
		lines := -1
		if ask.Gates != nil {
			lines = ask.Gates.Policy.Lines
		}
		bound := EffectiveLineBound(t.Size, s.policy.Risk.LargeLines, t.Usage)
		ask.Pace = r.pace(ctx, t, s, lines, bound, time.Now(), deadline)
		ask.CostState = r.costState(ctx, t, limits.GateReserve())
		var parent []byte
		if s.parent != "" {
			if rec, err := parentRecord(); err == nil {
				parent = rec
				if err := r.postWorkerUntil(ctx, workerStop, ns, pod, s.messageToken, "/records",
					map[string]string{"session": s.parent, "record": string(rec)}, r.endpointWait); err != nil && !errors.Is(err, errWorkerStop) {
					log.Printf("task %s: deliver session %s's record to the pod: %v", t.ID, s.parent, err)
				}
			}
		}
		tools := &runnerTools{r: r, taskID: t.ID, ns: ns, pod: pod, token: s.messageToken, parent: parent,
			facts: checkinFacts(t, s, ""), ask: &ask}
		out := r.runCheckin(ctx, t.ID, alias, &ask, cutTail(parent, limits.CheckinTailBytes()), r.checkin, tools,
			limits.CheckinCallCap(), eurLeft, limits.CheckinSessionCount(), limits.CheckinHandoverCap(), startCheckinTime(limits, deadline))
		out.Share = share
		if out.Record != nil {
			out.Record.Session = s.id
		}
		if out.Decision == CheckHandover {
			brief(out.Handover)
			return &out, false
		}
		return &out, true
	}
	// no model under the check-in's purpose: the session runs as if no
	// check-in was asked, and its brief carries no handover note
	brief("")
	return nil, false
}

// startCheckinTime is a start check-in's time cap: the policy's, cut at
// the session's deadline, which sits sessionGrace before the activity's,
// the room the time cap's stop takes to land; 0 once the deadline passed.
func startCheckinTime(limits policy.Limits, deadline time.Time) time.Duration {
	return max(0, min(limits.CheckinTimeCap(), time.Until(deadline).Round(time.Second)))
}

// pace states where the card stands for a check-in: the spend from a fresh
// read of the task, the agent time of the earlier sessions plus this one's.
// A report session has no card bound and no session count: nil.
func (r *Runner) pace(ctx context.Context, t Task, s sessionSpec, lines, bound int, started, deadline time.Time) *Pace {
	if s.report != "" {
		return nil
	}
	if fresh, ok, err := r.store.Task(ctx, t.ID); err == nil && ok {
		t = fresh
	}
	return &Pace{Lines: lines, BoundLines: bound, SpentEUR: t.EnforcedSpent(), BudgetEUR: t.BudgetEUR, ReserveEUR: s.policy.Limits.GateReserve(),
		SpentMinutes: int((t.Usage.AgentTime + time.Since(started)).Minutes()), LeftMinutes: int(time.Until(deadline).Minutes()),
		Attempt: s.attempt, Sessions: s.sessions}
}

// costState assembles the cost and state block from the store: the task
// row's figures and status, and the model-call and token totals of its
// audit rows. Never from the agent; nil when the totals cannot be read.
// SpentEUR is the settled spend and HeldEUR what outstanding calls hold.
// RemainingEUR is the room a session has: the budget less the spend and
// the holds (Task.Left), less the reserve (ReserveEUR, the review's and the
// audit's), through policy.SessionHeadroom and clamped at 0, as the pace's
// left reads it. The gates' supervisor passes no reserve.
func (r *Runner) costState(ctx context.Context, t Task, reserve float64) *CostState {
	if fresh, ok, err := r.store.Task(ctx, t.ID); err == nil && ok {
		t = fresh
	}
	totals, err := r.store.TaskCallTotals(ctx, t.ID)
	if err != nil {
		log.Printf("task %s: read the call totals: %v", t.ID, err)
		return nil
	}
	return &CostState{BudgetEUR: t.BudgetEUR, SpentEUR: t.SpentEUR, HeldEUR: t.HeldEUR, RemainingEUR: math.Round(max(policy.SessionHeadroom(t.Left(), reserve), 0)*100) / 100, ReserveEUR: reserve,
		Calls: totals.Calls, PromptTokens: totals.PromptTokens, CachedTokens: totals.CachedTokens, OutputTokens: totals.OutputTokens,
		State: t.Status}
}

// sizeBounded: the session codes a card the work bound covers, so the
// card's diff has the line bound its size plans. A report and a planner
// session write no card's code.
func sizeBounded(t Task, s sessionSpec) bool {
	return s.report == "" && !s.planner && t.Kind == "agent" && !t.Unbound()
}

// cuttable: the planner can cut the rest of the session's card into
// smaller cards: a plan's child goes back to its plan, a direct card only
// where its project's process has a plan track.
func cuttable(t Task, s sessionSpec) bool {
	return sizeBounded(t, s) && (t.Source == SourcePlan || s.planTrack)
}

// checkinLive is the mid-session check-in: a detector tripped, and the
// check-in decides over the running session from the transcript it reads
// factoryd-side. Nothing is pushed to the pod — the coding agent needs no
// record of its own audit. wipHead is the interim push's head when one
// landed before the conversation (wippush.go), "" when none did; the
// ask's facts name it. d is the action for the trip switch; ck the
// conversation's outcome with its record (nil only under no alias); ride
// says the outcome stops the session and rides SessionResult.CheckIn.
func (r *Runner) checkinLive(ctx context.Context, t Task, s sessionSpec, ns, pod string, rec *SessionRecorder, trip *Trip, messages []string, workerStop <-chan struct{}, deadline time.Time, lines, bound int, started time.Time, wipHead string) (d Decision, ck *CheckInOutcome, ride bool) {
	limits := s.policy.Limits
	// a handover's next session runs on the stronger rung, or on the
	// session_model of a plan or report class, which this session runs on
	climbs := r.cfg.Models.Stronger != "" && s.model != sessionModel(s.policy, t, r.cfg.Models.Stronger)
	ask := CheckInAsk{Moment: CheckInSession, ParentSession: s.id,
		Trip: &TripReport{Detector: trip.Kind, Detail: trip.Detail, Stuck: trip.Stuck, Climbs: climbs}, Messages: messages, Gates: s.lastGates,
		Replan: cuttable(t, s), Pace: r.pace(ctx, t, s, lines, bound, started, deadline), CostState: r.costState(ctx, t, limits.GateReserve())}
	share, eurLeft, _ := r.checkinBudget(ctx, t, limits)
	if eurLeft <= 0 {
		record := &CheckInRecord{Model: r.callModel(t, policy.PurposeCheckin), Session: s.id,
			ParentSession: s.id, Moment: CheckInSession, Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent}
		return Decision{Action: ActionEscalate, Reason: CheckinShareSpent}, &CheckInOutcome{Decision: CheckHuman, ReasonKind: CheckSpend, Reason: CheckinShareSpent,
			Findings: ask.Findings, Review: ask.Review, Record: record, Share: share}, true
	}
	alias := r.callModel(t, policy.PurposeCheckin)
	if alias == "" {
		d := unjudged(trip, "no model under the check-in's purpose")
		log.Printf("task %s: no model under the check-in's purpose: %s over %s", t.ID, d.Action, trip.Kind)
		return d, nil, false
	}
	tr := rec.Transcript()
	tools := &runnerTools{r: r, taskID: t.ID, ns: ns, pod: pod, token: s.messageToken, facts: checkinFacts(t, s, wipHead), ask: &ask, parent: tr, live: true}
	out := r.runCheckin(ctx, t.ID, alias, &ask, cutTail(tr, limits.CheckinTailBytes()), r.checkin, tools,
		limits.CheckinCallCap(), eurLeft, limits.CheckinSessionCount(), limits.CheckinHandoverCap(), limits.CheckinTimeCap())
	out.Record.Session = s.id
	out.Share = share
	switch out.Decision {
	case "continue":
		return Decision{Action: ActionContinue, Reason: out.Reason}, &out, false
	case "message":
		return Decision{Action: ActionMessage, Guidance: out.Guidance, Reason: out.Reason}, &out, false
	case CheckReplan:
		return Decision{Action: ActionReplan, Reason: out.Reason, Guidance: out.Guidance}, &out, false
	case CheckHandover:
		if out.Handover == "" && len(out.Verdicts) == 0 {
			// the refusal shape: the round runs as if no check-in existed
			return unjudged(trip, out.Reason), &out, false
		}
	}
	return Decision{Action: ActionEscalate, Reason: cmp.Or(out.Handover, out.Reason)}, &out, true
}

// unjudged is a trip's decision when the check-in gives no answer: the
// session runs on, but a stuck trip takes the stuck rule's climb.
func unjudged(trip *Trip, reason string) Decision {
	if trip.Stuck != "" {
		return Decision{Action: ActionRetryStronger, Reason: trip.Stuck}
	}
	return Decision{Action: ActionContinue, Reason: reason}
}

// checkinBudget is the card's check-in share of its budget and what is
// left of it: the policy's share widened by every share a human's answer
// to its spend stop granted (TaskUsage.ExtraCheckinShare;
// versionCheckinGrant). A read error is logged and reads as zero spent,
// as the check-in proceeds on it.
func (r *Runner) checkinBudget(ctx context.Context, t Task, limits policy.Limits) (share, eurLeft float64, err error) {
	spent, err := r.store.CheckinSpent(ctx, t.ID)
	if err != nil {
		log.Printf("task %s: read the check-in spend: %v", t.ID, err)
	}
	share = limits.CheckinShareOf(t.BudgetEUR) * float64(1+t.Usage.ExtraCheckinShare)
	return share, share - spent, err
}

// pushBrief delivers the session's brief: its handover note, "" for none,
// the parent's working state when carry holds one, and the opened proofs
// the worker writes into the clone. The worker holds its conversation's
// start for it.
func (r *Runner) pushBrief(ctx context.Context, workerStop <-chan struct{}, taskID, ns, pod, token, handover string, carry *carryState, proofs []harness.ProofFile) {
	push := harness.BriefPush{Handover: handover, Proofs: proofs}
	if carry != nil {
		push.Prior, push.Files = &carry.Prior, carry.Files
	}
	if err := r.postWorkerUntil(ctx, workerStop, ns, pod, token, "/brief", push, r.endpointWait); err != nil && !errors.Is(err, errWorkerStop) {
		log.Printf("task %s: deliver the brief: %v", taskID, err)
	}
}

// postWorkerUntil posts to the worker's message endpoint until it answers:
// the endpoint comes up only once the pod's clone and prepare are done, so
// a first post can meet connection refused. It gives up at ctx's end, a
// stopping factoryd, a post no retry changes (postFinal), or wait after
// the first try, with the last error said.
func (r *Runner) postWorkerUntil(ctx context.Context, workerStop <-chan struct{}, ns, pod, token, path string, body any, wait time.Duration) error {
	end := time.Now().Add(wait)
	var err error
	for {
		if err = r.postWorker(ctx, ns, pod, token, path, body); err == nil || postFinal(err) {
			return err
		}
		pause := min(r.startPoll, time.Until(end))
		if pause <= 0 {
			return err
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-workerStop:
			timer.Stop()
			return errWorkerStop
		case <-timer.C:
		}
	}
}

// postWorkerWip is postWorkerUntil's sibling for the ask whose answer is
// a body: it POSTs the running worker's /wip with the session's bearer
// until it answers, and hands back the WIP commit the answer names in its
// Wip-Commit header with the path of a temporary file that holds the
// bundle, streamed under wipBundleCap and never held in memory; the caller
// removes it. A 204 answer ships nothing: no commit, no error. The
// endpoint was up at the session's brief and the pod runs on through a
// check-in, so unlike the brief's delivery no answer is waited out: an
// answered refusal, of any status, or a timed-out ask is final — the
// interim push is given up, not retried — and only a transport that has
// not answered is retried until wait, as postWorkerUntil retries it.
func (r *Runner) postWorkerWip(ctx context.Context, workerStop <-chan struct{}, ns, pod, token string, wait time.Duration) (string, string, error) {
	end := time.Now().Add(wait)
	var commit, bundle string
	var err error
	for {
		commit, bundle, err = r.postWip(ctx, ns, pod, token)
		var answer *workerAnswer
		if err == nil || postFinal(err) || errors.As(err, &answer) || errors.Is(err, context.DeadlineExceeded) {
			return commit, bundle, err
		}
		pause := min(r.startPoll, time.Until(end))
		if pause <= 0 {
			return commit, bundle, err
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", "", ctx.Err()
		case <-workerStop:
			timer.Stop()
			return "", "", errWorkerStop
		case <-timer.C:
		}
	}
}

// wipCommitHeader names the commit the worker's /wip answer ships in its
// body's bundle.
const wipCommitHeader = "Wip-Commit"

// wipCommitID is the shape of a commit id the pod may name: it reaches
// git's arguments, so nothing else passes.
var wipCommitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// wipBundleCap bounds the bundle one /wip answer may ship, as the worker
// bounds the record a check-in hands factoryd. wipAskTimeout bounds one
// ask, which the worker answers by staging a commit: a worker that hangs
// costs one ask, not the check-in. A var, so a test hangs it shorter.
const wipBundleCap = 16 << 20

var wipAskTimeout = 30 * time.Second

// postWip posts one /wip to the running worker's endpoint and reads its
// answer: the WIP commit from the header, the bundle streamed from the body
// into a temporary file.
func (r *Runner) postWip(ctx context.Context, ns, pod, token string) (string, string, error) {
	ip, err := r.kube.PodIP(ctx, ns, pod)
	if errors.Is(err, errPodEnded) {
		return "", "", fmt.Errorf("%w: %w", harness.ErrNotRunning, err)
	}
	if err != nil {
		return "", "", fmt.Errorf("pod address: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, wipAskTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort(ip, strconv.Itoa(r.messagePort()))+"/wip", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := workerReads.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	commit := resp.Header.Get(wipCommitHeader)
	badAnswer := func(n int64) error {
		return &workerAnswer{code: resp.StatusCode, text: fmt.Sprintf("the /wip answer names commit %q over %d bytes of bundle", textutil.Truncate(commit, 80), n)}
	}
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return "", "", nil // the tree equals the start's: nothing to ship
	case resp.StatusCode != http.StatusOK:
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return "", "", &workerAnswer{code: resp.StatusCode, text: textutil.Truncate(string(bytes.TrimSpace(text)), 300)}
	case !wipCommitID.MatchString(commit):
		return "", "", badAnswer(0)
	}
	f, err := os.CreateTemp("", "wip-*.bundle")
	if err != nil {
		return "", "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, wipBundleCap+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n == 0 || n > wipBundleCap) {
		err = badAnswer(n)
	}
	if err != nil {
		os.Remove(f.Name()) // #nosec G104 -- the temporary file of a refused answer
		return "", "", err
	}
	return commit, f.Name(), nil
}

// checkinFacts is the small JSON of the card's facts the runner already
// holds; the check-in's card_facts answers it after the card's digest.
// wipHead, when an interim push landed before the conversation, names the
// head the session's work sits on.
func checkinFacts(t Task, s sessionSpec, wipHead string) string {
	facts := map[string]any{"budget_eur": t.BudgetEUR, "spent_eur": t.EnforcedSpent(),
		"attempt": s.attempt, "sessions": s.sessions}
	if wipHead != "" {
		facts["wip_head"] = wipHead
	}
	b, _ := json.Marshal(facts)
	return string(b)
}

// runnerTools is the check-in's tool set. The factoryd-side tools are
// answered locally — the parent record's lines, card taskID's digest, its
// steps and facts, the ask's gates — and the workspace tools are posted to
// the worker's /tool, every answer cut at the worker's own cap.
type runnerTools struct {
	r              *Runner
	taskID         string
	ns, pod, token string
	parent         []byte
	facts          string
	ask            *CheckInAsk
	// live says the pod holds a running conversation: the in-session
	// check-in reads it, and run would edit what the session is doing
	live bool
}

func (p *runnerTools) Tool(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name == "run" && p.live {
		return "", false, errors.New("run is not available to the check-in inside a live session; read instead")
	}
	switch name {
	case "transcript_search":
		query, _ := args["query"].(string)
		var found []string
		for _, line := range strings.Split(string(p.parent), "\n") {
			if query != "" && strings.Contains(line, query) {
				found = append(found, line)
			}
		}
		if len(found) == 0 {
			return fmt.Sprintf("no line of the record holds %q (the match is exact and case-sensitive)", query), false, nil
		}
		out, cut := cutToolOutput(strings.Join(found, "\n"))
		return out, cut, nil
	case "transcript_read":
		lines := strings.Split(strings.TrimSuffix(string(p.parent), "\n"), "\n")
		from, to := argInt(args["from"]), argInt(args["to"])
		if from < 1 {
			from = 1
		}
		if to > len(lines) {
			to = len(lines)
		}
		if from > to {
			return "", false, fmt.Errorf("the parent record holds %d lines", len(lines))
		}
		out, cut := cutToolOutput(strings.Join(lines[from-1:to], "\n"))
		return out, cut, nil
	case "card_facts":
		out, cut := cutToolOutput(DigestText(p.r.CardDigest(ctx, p.taskID)) + "\n" + p.facts)
		return out, cut, nil
	case "card_step":
		step, err := p.r.CardStep(ctx, p.taskID, argInt(args["step"]), "card_facts", func(session string, _ int) string {
			if session != p.ask.ParentSession {
				return ""
			}
			return "transcript_read (the parent session's record)"
		})
		if err != nil {
			return "", false, err
		}
		out, cut := cutToolOutput(step)
		return out, cut, nil
	case "gates":
		if p.ask.Gates == nil {
			return "no gates for the round", false, nil
		}
		b, err := json.Marshal(p.ask.Gates)
		if err != nil {
			return "", false, err
		}
		out, cut := cutToolOutput(string(b))
		return out, cut, nil
	}
	return p.post(ctx, name, args)
}

// post runs a workspace tool in the pod: the worker's /tool answers with
// its output and cut flag, and a non-200 answer is an error whose text the
// model sees.
func (p *runnerTools) post(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, checkinToolTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{"name": name, "args": args})
	if err != nil {
		return "", false, err
	}
	ip, err := p.r.kube.PodIP(ctx, p.ns, p.pod)
	if err != nil {
		return "", false, fmt.Errorf("pod address: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+net.JoinHostPort(ip, strconv.Itoa(p.r.messagePort()))+"/tool", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := workerReads.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("tool %s: worker answered %d: %s", name, resp.StatusCode,
			textutil.Truncate(string(bytes.TrimSpace(raw)), 300))
	}
	var answer struct {
		Output string `json:"output"`
		Cut    bool   `json:"cut"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", false, err
	}
	out, cut := cutToolOutput(answer.Output)
	return out, cut || answer.Cut, nil
}

// cheapAttemptCap is how long the start rung's first attempt has to push
// before the session climbs in place (roadmap 222, item 5).
const cheapAttemptCap = 8 * time.Minute

// frontier: a session on model runs above the ladder's start rung, on the
// stronger rung or a class's session_model, and starts on the frontier
// floor (budget.session_floor_frontier_eur).
func (r *Runner) frontier(model string) bool {
	return aliasOf(model) != aliasOf(r.cfg.Models.Start)
}

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
	c.NoProgressAfter = d.Detector(policy.DetectorNoProgress)
	c.OpenCallBound = d.Detector(policy.DetectorOpenCall)
	c.WallClockCap = agentTime
	c.CheckIn = checkIn
	c.NudgeAfter, c.Nudges = d.Limits.Nudge()
	c.SameCall = d.Limits.SameCallAfter()
	c.NoPushSpend = d.Limits.NoPushShare()
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

// callModel is the alias a factoryd model call uses for a purpose: the
// version in force names it, because the gateway's purpose check enforces
// that version — never the task's pinned document, whose alias a version
// loaded after the task pinned can strand — and no alias under the version
// in force means no call. The pinned document still governs limits and
// prompts.
func (r *Runner) callModel(t Task, purpose string) string {
	return r.cfg.Policy().Document.FactoryModel(t.RequiredCapabilities, purpose)
}

// recordDecision writes the check-in's decision into the session record,
// the timeline and, for a detector's trip, an alert the decision judged.
// The timeline step is the runner's only when the outcome does not ride —
// a riding outcome's step is the workflow's.
func (r *Runner) recordDecision(ctx context.Context, t Task, s sessionSpec, recorder *SessionRecorder, trip *Trip, d Decision, ck *CheckInOutcome, ride bool, digest string) {
	refused := ck != nil && ck.Decision == CheckHandover && ck.Handover == "" && len(ck.Verdicts) == 0
	called := ck != nil && !refused
	decision := string(d.Action)
	if called {
		decision = ck.Decision
	}
	detail := map[string]any{"detector": string(trip.Kind), "trip": trip.Detail, "digest": digest,
		"decision": decision, "guidance": d.Guidance, "reason": d.Reason, "called": called}
	if ck != nil && ck.Record != nil {
		detail["record"] = ck.Record
		detail["moment"] = ck.Record.Moment
		detail["parent_session"] = ck.Record.ParentSession
	}
	if err := recorder.Record(ctx, harness.Event{Time: time.Now(), Kind: harness.EventSupervisor, Detail: detail}); err != nil {
		log.Printf("task %s: record check-in decision: %v", t.ID, err)
	}
	if !ride && (ck != nil || trip.decided != nil) {
		detail["session"] = s.id
		r.record(ctx, t.ID, "checkin", detail)
	}
	if trip.Kind == TripAgentEscalation || trip.Kind == TripAgentInfoRequest || trip.Kind == TripAgentToolRequest || trip.Kind == TripAgentCut || trip.Kind == TripCheckIn || trip.Kind == TripReplan {
		return // the agent's ask (an escalation or info_request has the workflow's alert), a person's replan, or no detector
	}
	a := Alert{Kind: AlertSupervisorTrip, Source: string(trip.Kind), TaskID: t.ID, Verdict: tripVerdict(d.Action), ResolvedBy: "checkin",
		Detail: map[string]any{"session": s.id, "action": string(d.Action), "diagnosis": cmp.Or(d.Reason, d.Guidance), "trip": trip.Detail, "called": called}}
	if _, err := r.store.InsertAlert(ctx, a); err != nil {
		log.Printf("task %s: record the check-in trip alert: %v", t.ID, err)
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
	if d.Reason != "" && d.Reason != d.Guidance {
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

// The start steps a pod can fail at for good; stepImagePin a gate's
// sandbox whose card's image has no pin in force, stepBudget a session
// the card's own money cannot open.
const (
	stepImagePull       = "image pull"
	stepContainerConfig = "container config"
	stepImagePin        = "image pin"
	stepBudget          = "budget"
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

// The ways on a start failure's escalation.
const (
	thenAnswer   = "then answer to start a session again, or close the card"
	thenRepro    = "then answer to run the reproduction again, or close the card"
	thenCriteria = "then answer to run the criteria gate again, or close the card"
)

// escalation is the failure's escalation reason, then the way on: its
// first line is the card's question row (escalationQuestion), with the
// action. The budget's is the floor's refusal, which names its own way on.
func (f *startFailure) escalation(then string) string {
	if f.step == stepBudget {
		return f.text
	}
	act := "fix the cause below"
	switch f.step {
	case stepImagePull:
		act = "push the worker image or correct its reference"
	case stepContainerConfig:
		act = "correct the sandbox pod's configuration"
	case StepEgressProbe:
		act = "check the task namespace's " + workerEgress + " policy, or the pod's uid, against the probe named below"
	case stepImagePin:
		act = "see that " + imagesPath + " on main declares the image and that its build lands, so the factory pins it"
	}
	return fmt.Sprintf("%s%s, and no session can fix it: %s, %s.\n%s", startRefused, f.step, act, then, f.text)
}

// setupNamespace builds the session's sandbox on t's image (taskImage),
// which it records on t and its row with the registries the sandbox
// reaches: base is the read token of the pod's clone or fetch init
// container. It returns the project's cache claim and what the image
// holds, nil for the factory's.
func (r *Runner) setupNamespace(ctx context.Context, t *Task, base ForgejoToken) (string, *imagegen.Manifest, error) {
	p, ok, err := r.store.ProjectByRepository(ctx, t.Repository)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, terminalError("task %s: repository %s is no project's", t.ID, t.Repository)
	}
	image, m, err := r.taskImage(ctx, *t)
	if err != nil {
		return "", nil, err
	}
	egress := sandboxRegistries
	if m != nil {
		egress = m.Egress
	}
	if canary, ok := r.reachesCanary(egress); ok {
		return "", nil, terminalError("image %s (%s) reaches %s, the egress canary, so every sandbox on it fails its egress probe: "+
			"drop the name from the image's egress in %s, or move EGRESS_CANARY", cmp.Or(t.Image, "default"), image, canary, imagesPath)
	}
	if err := r.store.SetTaskImage(ctx, t.ID, image, egress); err != nil {
		return "", nil, fmt.Errorf("record the image: %w", err)
	}
	t.ImageDigest, t.ImageEgress = image, egress
	cache := "cache-agent-" + p.Name
	ns := r.agentNamespace(t.ID)
	if err := r.freshNamespace(ctx, ns, map[string]string{
		"factory.ai/task":                    t.ID,
		"pod-security.kubernetes.io/enforce": "restricted",
	}); err != nil {
		return "", nil, fmt.Errorf("setup namespace: %w", err)
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
			return r.kube.ApplyCiliumNetworkPolicy(ctx, ns, workerEgress, r.workerEgressSpec(egress, leased[t.ID]))
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
			return "", nil, fmt.Errorf("setup %s: %w", step.name, err)
		}
	}
	return cache, m, nil
}

// reachesCanary is the egress canary's host and whether names hold it: the
// sandbox's egress probe must find the canary unreachable.
func (r *Runner) reachesCanary(names []string) (string, bool) {
	host, _, _ := net.SplitHostPort(r.cfg.EgressCanary)
	return host, slices.ContainsFunc(names, func(name string) bool {
		return strings.EqualFold(strings.TrimSuffix(name, "."), strings.TrimSuffix(host, "."))
	})
}

// ErrImageNotPinned refuses a card whose image has no pin in force: not
// built yet, unpinned or no longer declared.
type ErrImageNotPinned struct{ Project, Image string }

func (e ErrImageNotPinned) Error() string {
	return fmt.Sprintf("image %s of project %s has no pin in force: no sandbox runs on it until the factory pins it", e.Image, e.Project)
}

// errImageNotPinned is ErrImageNotPinned's activity error type: no retry
// pins the image.
const errImageNotPinned = "image_not_pinned"

// taskImage is the image t's sandbox runs on, ref@sha256:..., and what it
// holds: t's image, else default, as the policy version in force pins it
// for t's project and the images key signed it. An unpinned default is
// the factory's image with a nil manifest; any other unpinned name is
// ErrImageNotPinned, as a non-retryable activity error. A pin the images
// key did not sign is refused for good and raises one alert per digest.
func (r *Runner) taskImage(ctx context.Context, t Task) (string, *imagegen.Manifest, error) {
	name := cmp.Or(t.Image, "default")
	project, c, _ := r.cfg.Policy().ProjectOf(t.Repository)
	pin, ok := c.Images[name]
	switch {
	case ok:
		var unsigned ErrUnsigned
		if err := r.verifyProjectImage(ctx, project, name, pin.Digest); errors.As(err, &unsigned) {
			r.Imager.alert(ctx, Alert{Kind: AlertImage, Source: "unsigned", Fingerprint: "unsigned " + imageRepository(project, name) + "@" + pin.Digest,
				Detail: map[string]any{"project": project, "image": name, "digest": pin.Digest}})
			return "", nil, terminalError("image %s of project %s is pinned at %s, which carries no signature by the images key: "+
				"no sandbox runs it until a human puts the pin or the store right (%s)", name, project, pin.Digest, err)
		} else if err != nil {
			return "", nil, err
		}
		return r.cfg.RegistryHost + "/" + imageRepository(project, name) + "@" + pin.Digest, &imagegen.Manifest{Tools: pin.Tools, Egress: pin.Egress}, nil
	case name == "default":
		return r.cfg.WorkerImage, nil, nil
	}
	e := ErrImageNotPinned{Project: project, Image: name}
	return "", nil, temporal.NewNonRetryableApplicationError(e.Error(), errImageNotPinned, e)
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

	// sandboxGoMemLimit is each go process's GOMEMLIMIT in a sandbox pod:
	// under half the pod's 4Gi, so two -p 2 test binaries racing cannot
	// together reach the limit.
	sandboxGoMemLimit = "1500MiB"
	sandboxQuota      = map[string]any{
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

// supervisionTick is the session loop's period: a third of the session's
// stall time c, between a second and 15.
func (r *Runner) supervisionTick(c SupervisorConfig) time.Duration {
	if r.tick > 0 {
		return r.tick
	}
	t := c.NoProgressAfter / 3
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
	tools := s.policy.Tools(t.RequiredCapabilities, t.Source)
	if s.report != "" {
		// a report session's card restarts on no image
		tools = slices.DeleteFunc(tools, func(tool string) bool { return tool == policy.ToolRequestTool })
	}
	if !cuttable(t, s) {
		tools = slices.DeleteFunc(tools, func(tool string) bool { return tool == policy.ToolRequestCut })
	}
	toolsJSON, _ := json.Marshal(tools)
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
		// each of these opencode fetches phones home past the egress
		// allowlist and stalls on the drop, whatever image the pod runs
		{"name": "OPENCODE_DISABLE_AUTOUPDATE", "value": "1"},
		{"name": "OPENCODE_DISABLE_MODELS_FETCH", "value": "1"},
		{"name": "OPENCODE_DISABLE_LSP_DOWNLOAD", "value": "1"},
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
	// a sandbox companion is cloned read-only like the documents, every
	// file of it (a companion is code and config, not documents); the URL
	// list goes to the clone container alone, the worker reads the clones
	var companionURLs, companionDirs []string
	for _, c := range s.companions {
		if !c.Sandbox {
			continue
		}
		owner, name, _ := strings.Cut(c.Repository, "/")
		companionURLs, companionDirs = append(companionURLs, r.repoURL(Repo{Owner: owner, Name: name})), append(companionDirs, "/workspace/companions/"+name)
	}
	if len(companionURLs) > 0 {
		cloneEnv = append(cloneEnv, map[string]any{"name": "COMPANIONS", "value": strings.Join(companionURLs, " ")})
		env = append(env, map[string]any{"name": "COMPANION_DIRS", "value": strings.Join(companionDirs, " ")})
	}
	if s.pull != 0 {
		cloneEnv = append(cloneEnv, map[string]any{"name": "PULL", "value": strconv.FormatInt(s.pull, 10)})
	}
	if t.PRNumber != 0 {
		cloneEnv = append(cloneEnv, map[string]any{"name": "TASK_PR", "value": strconv.FormatInt(t.PRNumber, 10)})
	}
	if s.report != "" {
		env = append(env, map[string]any{"name": "MODE", "value": "report"}, map[string]any{"name": "REPORT_GUIDE", "value": fit.text("REPORT_GUIDE", s.report, "the report's guide")})
		if len(s.branches) > 0 {
			cloneEnv = append(cloneEnv, map[string]any{"name": "PUSH_URL", "value": r.repoURL(r.forgejo.Fork(s.repo))})
		}
	} else {
		fork := r.repoURL(r.forgejo.Fork(s.repo))
		cloneEnv = append(cloneEnv, map[string]any{"name": "PUSH_URL", "value": fork})
		env = append(env, map[string]any{"name": "PUSH_URL", "value": fork})
	}
	if len(s.branches) > 0 {
		cloneEnv = append(cloneEnv, map[string]any{"name": "BRANCHES", "value": strings.Join(s.branches, " ")})
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
	// the worker's push rule reads the card's line bound: the same
	// effective number the gate and the supervisor check
	env = append(env, map[string]any{"name": "LINE_BOUND", "value": strconv.Itoa(EffectiveLineBound(t.Size, s.policy.Risk.LargeLines, t.Usage))})
	if r.cfg.OTLPEndpoint != "" {
		env = append(env, map[string]any{"name": "OTEL_EXPORTER_OTLP_ENDPOINT", "value": r.cfg.OTLPEndpoint})
	}
	if len(fit.notes) > 0 {
		notes, _ := json.Marshal(fit.notes)
		env = append(env, map[string]any{"name": "LEFT_OUT", "value": string(notes)})
	}
	if s.image != nil {
		manifest, _ := json.Marshal(s.image)
		env = append(env, map[string]any{"name": "IMAGE_MANIFEST", "value": string(manifest)})
	}
	pod := r.sandboxPodSpec(t, r.sandboxImage(t), s.cache, env, true)
	// the clone init container alone holds the read token: the agent's
	// container gets the clone in the workspace and no token (the push
	// token reaches the worker on POST /push once the agent is dead). It
	// runs the factory's image, as a project image's content is agents'.
	spec := pod["spec"].(map[string]any)
	clone := maps.Clone(spec["containers"].([]any)[0].(map[string]any))
	clone["name"] = "clone"
	clone["image"] = r.cfg.WorkerImage
	clone["env"] = cloneEnv
	clone["volumeMounts"] = []any{map[string]any{"name": "workspace", "mountPath": "/workspace"}, harnessMount()}
	spec["initContainers"] = append(spec["initContainers"].([]any), clone)
	return requestEmptyDirs(pod)
}

// sandboxImage is the image t's sandbox pods run: the one its latest
// sandbox resolved, the factory's before any did.
func (r *Runner) sandboxImage(t Task) string {
	return cmp.Or(t.ImageDigest, r.cfg.WorkerImage)
}

// SandboxUID is the sandbox's uid and gid: its pods' runAsUser and
// runAsGroup, Dockerfile.sandbox's USER, and what the worker's egress
// probe holds itself to.
const SandboxUID = 1000

// harnessCopy fills the pod's harness volume from the factory's image
// (WORKER_IMAGE): agent-worker, opencode and the opencode bake, which the
// worker runs from /factory whatever image it runs on.
const harnessCopy = "mkdir -p /factory/bin && cp /usr/local/bin/agent-worker /usr/local/bin/opencode /factory/bin/ && cp -a /opt/opencode-home /factory/"

// harnessMount is the harness volume in every container but the one that
// fills it: read-only, as the agent runs as the worker's uid.
func harnessMount() map[string]any {
	return map[string]any{"name": "harness", "mountPath": "/factory", "readOnly": true}
}

// sandboxSecurityContext confines each container of a sandbox pod.
func sandboxSecurityContext() map[string]any {
	return map[string]any{
		"runAsNonRoot":             true,
		"runAsUser":                SandboxUID,
		"runAsGroup":               SandboxUID,
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"readOnlyRootFilesystem":   true,
	}
}

// sandboxPodSpec is a gVisor worker pod in the task namespace running
// image, its harness copied in first from the factory's image; only an
// agent session gets the gateway token, and cache names the project's
// shared cache claim ("" none). Every sandbox pod carries the
// worker label the egress policy selects: without it no policy would
// confine its egress.
func (r *Runner) sandboxPodSpec(t Task, image, cache string, env []map[string]any, gatewayToken bool) map[string]any {
	pullSecrets := []any{}
	if r.cfg.PullSecret != "" {
		pullSecrets = append(pullSecrets, map[string]any{"name": r.cfg.PullSecret})
	}
	mounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/workspace"},
		map[string]any{"name": "home", "mountPath": "/home/worker"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		harnessMount(),
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "4Gi"}},
		map[string]any{"name": "home", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
		// the agent runs the checks CI runs, so its tmp is the CI pod's:
		// 1 GiB evicted a session building factoryd's race tests (#816)
		map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "4Gi"}},
		map[string]any{"name": "harness", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
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
		map[string]any{"name": "EGRESS_DIRECT_IP", "value": r.cfg.EgressDirectIP},
		// every go test in the session is bounded the way bash check is (#1542,
		// #1543): two test binaries at once and a heap soft limit under the
		// pod's 4Gi, so the command the agent types cannot exceed the pod's
		// memory. GOFLAGS must keep -mod=mod, the image's value a pod env
		// would otherwise override.
		map[string]any{"name": "GOFLAGS", "value": "-mod=mod -p=2"},
		map[string]any{"name": "GOMEMLIMIT", "value": sandboxGoMemLimit})
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
			"initContainers": []any{map[string]any{
				"name":    "harness",
				"image":   r.cfg.WorkerImage,
				"command": []any{"/bin/sh", "-c", harnessCopy},
				"resources": map[string]any{
					"requests": sandboxRequests,
					"limits":   sandboxLimits,
				},
				"volumeMounts":             []any{map[string]any{"name": "harness", "mountPath": "/factory"}},
				"securityContext":          sandboxSecurityContext(),
				"terminationMessagePolicy": "FallbackToLogsOnError",
			}},
			"containers": []any{map[string]any{
				"name":    "worker",
				"image":   image,
				"command": []any{"/bin/sh", "-c", cacheUmaskSh, "/factory/bin/agent-worker"},
				"env":     env,
				"resources": map[string]any{
					"requests": sandboxRequests,
					"limits":   sandboxLimits,
				},
				"volumeMounts":    mounts,
				"securityContext": sandboxSecurityContext(),
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
// HTTP port, the image's registries by name on 443 (the factory image's
// are the Go module proxy, its checksum database and the npm registry:
// the image carries the Go toolchain and Node, and a registry
// joins by name with the toolchain that needs it; a project image's are
// its egress), and kube-dns for exactly those
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
func (r *Runner) workerEgressSpec(registries, leased []string) map[string]any {
	dnsNames := []any{}
	for _, name := range r.sandboxNames(registries) {
		dnsNames = append(dnsNames, map[string]any{"matchName": name})
	}
	egress := []any{
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
	}
	// an empty toFQDNs would admit 443 to every destination
	if len(registries) > 0 {
		toFQDNs := []any{}
		for _, host := range registries {
			toFQDNs = append(toFQDNs, map[string]any{"matchName": host})
		}
		egress = append(egress, map[string]any{
			"toFQDNs": toFQDNs,
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}},
			}},
		})
	}
	egress = append(egress, map[string]any{
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
	})
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
		"egress": egress,
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

// sandboxRegistries are the public names a sandbox on the factory's image
// reaches on 443; storage.googleapis.com: proxy.golang.org redirects zip
// downloads there. A project image's sandbox reaches its own egress
// instead.
var sandboxRegistries = []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com", "registry.npmjs.org"}

// sandboxNames are the names the sandbox's DNS allowlist answers: the
// gateway's, Forgejo's, registries and the collector's.
func (r *Runner) sandboxNames(registries []string) []string {
	var names []string
	for _, raw := range []string{r.cfg.GatewayURL, r.cfg.GitBaseURL} {
		u, _ := url.Parse(raw) // validated in NewRunner
		names = append(names, u.Hostname())
	}
	names = append(names, registries...)
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
// its task's live leases and the registries its image reaches, as the
// task row records them: it reads the live policy and applies the desired
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
		t, _, err := r.store.Task(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ns, err))
			continue
		}
		egress := sandboxRegistries
		if t.ImageDigest != "" {
			egress = t.ImageEgress
		}
		want := r.workerEgressSpec(egress, leased[id])
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
// warning it is due; a warning the pod did not take stays due, and only its
// first failure is recorded on the timeline.
func (r *Runner) watchLimits(ctx context.Context, t Task, s sessionSpec, ns, pod string, w *limitWatch, sup *Supervisor) *Trip {
	cur, ok, err := r.store.Task(ctx, t.ID)
	if err != nil {
		log.Printf("task %s: read the card's spend: %v", t.ID, err)
		return nil
	}
	if !ok {
		return nil
	}
	for _, wn := range w.due(cur.EnforcedSpent(), cur.BudgetEUR, time.Now()) {
		err := r.messageWorker(ctx, ns, pod, s.messageToken, wn.text)
		if err == nil || !w.failed[wn.kind] {
			detail := map[string]any{"session": s.id}
			if err != nil {
				detail["error"] = err.Error()
			}
			r.record(ctx, t.ID, "message", detail)
		}
		if err != nil {
			w.failed[wn.kind] = true
			log.Printf("task %s: send the %s warning: %v", t.ID, wn.kind, err)
			continue
		}
		w.sent(wn.kind)
	}
	return sup.Spent(cur.EnforcedSpent(), cur.BudgetEUR)
}
