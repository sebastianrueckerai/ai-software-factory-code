//go:build unix

// agent-worker is the in-sandbox half of the runner (architecture 6.4). It
// clones the task's repository, drives the opencode adapter against the
// model gateway with the projected task token, commits and pushes the
// result, and reports progress to factoryd as JSONL envelopes on stdout —
// stdout is the only channel out of the sandbox, so no egress rule beyond
// the allowlist is ever needed.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/doclinks"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/imagegen"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/sanitize"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "wait" {
		os.Exit(runWait(os.Args[2:], os.Stdout, time.Second))
	}
	log := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "agent-worker: "+format+"\n", args...)
	}
	code := run(log)
	os.Exit(code)
}

func run(log func(string, ...any)) int {
	initErr := becomeInit()
	mode := os.Getenv("MODE")
	switch {
	case (mode == "clone" || mode == "repro-fetch") && initErr != nil:
		// left for the worker container to report, like a failed fetch
		return recordFetch(log, workspace, initErr)
	case mode == "clone":
		return runClone(log, workspace)
	case mode == "repro-fetch":
		return runReproFetch(log, workspace)
	case mode == "repro":
		return runRepro(log, initErr, sandboxProbe)
	}
	return runSession(context.Background(), log, workspace, os.Stdout, initErr, sandboxProbe)
}

// becomeInit checks the worker runs as its container's init and makes it
// non-dumpable. killOthers relies on every agent process being in this
// PID namespace and reparenting to the worker. The agent runs as the same
// uid; a non-dumpable worker keeps its /proc/1 (its memory holds the push
// token at the end) unreadable to it.
func becomeInit() error {
	if pid := os.Getpid(); pid != 1 {
		return fmt.Errorf("must run as the container's init (pid 1), runs as pid %d", pid)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return fmt.Errorf("prctl PR_SET_DUMPABLE: %v", errno)
	}
	return nil
}

// session is what a session's start hands its run.
type session struct {
	env            workerEnv
	repoDir        string
	base, mainHead string
	brief          harness.TaskBrief
	oc             *factoryd.Opencode
	gate           *pushGate
	ck             *checkpointer // nil for a report session
	tel            *sessionTelemetry
	outcome        string // for the session's duration metric
	// closeTelemetry ends the session span and sends the last metrics,
	// once; stopMessages closes the message endpoint
	closeTelemetry, stopMessages func()
}

// runSession runs a session on the workspace ws, its envelopes on stdout;
// initErr is becomeInit's, and probe builds the egress probe of the
// sandbox its env describes. A step before the harness that fails ends
// the session with a result that names the step and its error.
func runSession(ctx context.Context, log func(string, ...any), ws string, stdout io.Writer, initErr error, probe func(workerEnv) egressProbe) int {
	s, err := start(ctx, log, ws, initErr, probe)
	defer s.closeTelemetry()
	defer s.stopMessages()
	if err != nil {
		log("%v", err)
		writeResult(stdout, startFailed(err), s.closeTelemetry)
		return 1
	}
	if s.env.report != "" {
		return runReport(ctx, s, stdout, log)
	}
	var emitMu sync.Mutex
	emit := func(ev harness.Event) {
		s.tel.observe(ev)
		line, err := json.Marshal(harness.WorkerEnvelope{Type: "event", Event: &ev})
		if err != nil {
			return
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		fmt.Fprintln(stdout, string(line))
	}
	ckCtx, stopCheckpoints := context.WithCancel(ctx)
	ckDone := make(chan struct{})
	go func() {
		defer close(ckDone)
		s.ck.loop(ckCtx, checkpointEvery, emit, log)
	}()
	// factoryd stops a session (a supervisor decision, the wall-clock cap)
	// by deleting the pod: SIGTERM ends the agent, and its work in progress
	// is committed and pushed like a finished session's before the grace
	// period runs out.
	runCtx, stopRun := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stopRun()
	endRun := s.tel.phase("run")
	res := s.oc.Run(runCtx, s.brief, emit)
	stopped := runCtx.Err() != nil
	stopCheckpoints()
	<-ckDone
	if err := killOthers(); err != nil {
		res = harness.Result{Success: false, Summary: "stop agent processes: " + err.Error()}
	}
	if res.Success || stopped {
		endRun(nil)
	} else {
		endRun(errors.New("the agent did not finish"))
	}
	var commit string
	var pushErr error
	res = deliver(res, stopped, func() (string, error) {
		endPush := s.tel.phase("push")
		commit, pushErr = commitAndPush(s.repoDir, s.env, s.base, s.mainHead, killOthers, func(commit string) (string, error) {
			return s.gate.ask(commit, pushWait, stdout)
		})
		endPush(pushErr)
		return commit, pushErr
	}, s.env.branch, emit)
	s.outcome = sessionOutcome(res, stopped, commit, pushErr)
	writeResult(stdout, res, s.closeTelemetry)
	if !res.Success {
		log("result: %s", res.Summary)
		return 1
	}
	return 0
}

// startError is the step of the session's start that failed, and why.
type startError struct {
	step string
	err  error
}

func (e startError) Error() string { return e.step + ": " + e.err.Error() }
func (e startError) Unwrap() error { return e.err }

// maxStartError bounds a start failure's text in its result: escaped as
// JSON, at most 6 bytes a byte, the line stays far under the 1 MiB that
// factoryd reads of one.
const maxStartError = 16 << 10

// startFailed is the result of a start that failed with err, a startError.
func startFailed(err error) harness.Result {
	var se startError
	errors.As(err, &se)
	text := err.Error()
	if len(text) > maxStartError {
		text = fmt.Sprintf("%s (cut from %d bytes)", textutil.Truncate(text, maxStartError), len(text))
	}
	return harness.Result{Success: false, Summary: "start failed: " + text, Step: se.step, Transient: errors.As(err, new(transient))}
}

// transient is a start failure a later start may not meet: a service the
// sandbox needs that did not answer, as while it restarts.
type transient struct{ error }

// start runs the session's steps before the harness, in order, on the
// workspace ws; an error is a startError. s is never nil: its closers
// undo what started.
func start(ctx context.Context, log func(string, ...any), ws string, initErr error, probe func(workerEnv) egressProbe) (s *session, err error) {
	s = &session{outcome: "failed", closeTelemetry: func() {}, stopMessages: func() {}}
	if initErr != nil {
		return s, startError{"init", initErr}
	}
	if s.env, err = loadEnv(); err != nil {
		return s, startError{"environment", err}
	}
	log("task %s session %s", s.env.taskID, s.env.sessionID)
	otelProviders, err := telemetry.Setup(ctx, "agent-worker")
	if err != nil {
		return s, startError{"telemetry", err}
	}
	if s.tel, err = newSessionTelemetry(otelProviders.Tracer, otelProviders.Meter, s.env.taskID, s.env.sessionID, s.env.model); err != nil {
		return s, startError{"telemetry", err}
	}
	s.tel.start()
	// factoryd deletes the pod once it reads the result line, so the session
	// span and the last metrics leave before it
	s.closeTelemetry = sync.OnceFunc(func() {
		s.tel.end(s.outcome)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		otelProviders.Shutdown(ctx) // #nosec G104 -- fire and forget: the last spans may be lost
	})
	endProbe := s.tel.phase("egress probe")
	err = probe(s.env).run(ctx)
	endProbe(err)
	if err != nil {
		return s, startError{factoryd.StepEgressProbe, err}
	}
	endBrief := s.tel.phase("brief")
	defer func() { endBrief(err) }()

	exe, err := os.Executable()
	if err == nil {
		err = useHarness(exe, s.env.home)
	}
	if err != nil {
		return s, startError{"harness", err}
	}
	s.repoDir = filepath.Join(ws, "repo")
	if err := fetched(ws); err != nil {
		return s, startError{"clone", err}
	}
	if s.base, s.mainHead, err = prepareRepo(s.repoDir, s.env); err != nil {
		return s, startError{"prepare repo", err}
	}
	s.brief = harness.TaskBrief{
		Objective:      s.env.objective,
		Repository:     s.env.repository,
		SHA:            s.env.repoSHA,
		Findings:       s.env.findings,
		Answers:        s.env.answers,
		PosterInfo:     s.env.posterInfo,
		Author:         s.env.author,
		AuthorStanding: s.env.authorStanding,
		Report:         s.env.report,
		Tools:          s.env.tools,
		Spec:           s.env.spec,
		CardRole:       s.env.cardRole,
		LeftOut:        s.env.leftOut,
		Facts:          s.env.facts,
		Image:          s.env.image,
	}
	s.brief.Docs, s.brief.DocsFailed = docsState(ws, s.env.docsDir)
	resolveDocLinks(log, &s.brief, s.env.docsDir)
	if s.brief.Map, err = cardMap(s.repoDir, s.env.directories); err != nil {
		return s, startError{"repository map", err}
	}
	if s.brief.Map != "" {
		log("repository map: %d directories, %d bytes", len(s.env.directories), len(s.brief.Map))
	}
	s.oc = &factoryd.Opencode{
		WorkDir:       s.repoDir,
		GatewayURL:    s.env.gatewayURL,
		TokenFile:     s.env.tokenFile,
		Model:         s.env.model,
		Stronger:      s.env.strongerModel,
		Cheap:         s.env.cheapModel,
		Budget:        factoryd.TokenBudget{AnswerTokens: s.env.answerTokens, HeadroomTokens: s.env.headroomTokens},
		Steps:         s.env.steps,
		ContextTokens: s.env.contextTokens,
	}
	if err := s.oc.Prepare(ctx, harness.SandboxEnv{}, s.brief); err != nil {
		return s, startError{"prepare brief", err}
	}
	s.gate = newPushGate()
	checkin := newCheckinServe(ws)
	stop, err := serveMessages(s.env.messageHash, s.gate, func(ctx context.Context, text, model string) error {
		if model != "" {
			s.oc.SetModel(model)
		}
		return s.oc.Message(ctx, text)
	}, checkin, log)
	if err != nil {
		return s, startError{"message endpoint", err}
	}
	s.stopMessages = stop
	// the check-in runs before the coding conversation; its handover, when
	// one arrives within the wait, is the brief's first section
	push := checkin.waitBrief(log)
	s.brief.Handover, s.brief.Prior = push.Handover, push.Prior
	if s.env.report == "" {
		if s.ck, err = newCheckpointer(s.repoDir, s.env, s.base, s.mainHead); err != nil {
			return s, startError{"checkpoint store", err}
		}
	}
	return s, nil
}

// runReport runs a report session: the agent writes, nothing is committed
// or pushed, and its last message is the result's summary, which factoryd
// posts on the issue. A stopped session reports what it has.
func runReport(ctx context.Context, s *session, stdout io.Writer, log func(string, ...any)) int {
	emit := func(ev harness.Event) {
		s.tel.observe(ev)
		if line, err := json.Marshal(harness.WorkerEnvelope{Type: "event", Event: &ev}); err == nil {
			fmt.Fprintln(stdout, string(line))
		}
	}
	runCtx, stopRun := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stopRun()
	endRun := s.tel.phase("run")
	res := s.oc.Run(runCtx, s.brief, emit)
	if err := killOthers(); err != nil {
		res = harness.Result{Success: false, Summary: "stop agent processes: " + err.Error()}
	}
	if res.Success {
		endRun(nil)
		s.outcome = "reported"
	} else {
		endRun(errors.New("the agent did not finish"))
	}
	writeResult(stdout, res, s.closeTelemetry)
	if !res.Success {
		log("result: %s", res.Summary)
		return 1
	}
	return 0
}

// writeResult runs before, then writes the result envelope.
func writeResult(w io.Writer, res harness.Result, before func()) {
	before()
	line, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &res})
	fmt.Fprintln(w, string(line))
}

// sessionOutcome is the session's outcome for its duration metric.
func sessionOutcome(res harness.Result, stopped bool, commit string, pushErr error) string {
	switch {
	case pushErr != nil:
		return "failed"
	case res.Success && commit != "":
		return "pushed"
	case res.Success:
		return "no_changes"
	case stopped:
		return "stopped"
	}
	return "failed"
}

// deliver pushes the session's work at every end: a finished session's
// result, a stopped or failed session's work in progress, and says so in
// the result; a failed session's summary keeps its failure.
func deliver(res harness.Result, stopped bool, push func() (string, error), branch string, emit func(harness.Event)) harness.Result {
	failed := !res.Success && !stopped
	commit, err := push()
	switch {
	case err != nil:
		emit(harness.Event{Time: time.Now(), Kind: harness.EventPushFailed, Detail: map[string]any{"branch": branch, "error": err.Error()}})
		summary := "push failed: " + err.Error()
		if failed {
			summary = res.Summary + "; " + summary
		}
		return harness.Result{Success: false, Summary: summary}
	case commit != "":
		emit(harness.Event{Time: time.Now(), Kind: harness.EventPushed, Detail: map[string]any{"branch": branch, "commit": commit}})
	}
	switch {
	case failed && commit != "":
		res.Summary += "; work in progress pushed to " + branch + " at " + commit
	case failed:
	case res.Success && commit != "":
		res.Summary = "pushed branch " + branch + " at " + commit
	case res.Success:
		res.Summary = "no changes to push"
	case commit != "":
		res.Summary = "stopped; work in progress pushed to " + branch + " at " + commit
	default:
		res.Summary = "stopped; no work in progress to push"
	}
	return res
}

// killOthers SIGKILLs every other process in the container and reaps them.
// The worker is init, so every orphan the agent leaves reparents to it, and
// wait4 answering ECHILD means no other process is left. Nothing the agent
// started, including one that left opencode's process group, survives.
func killOthers() error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(-1, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("kill(-1): %w", err)
		}
		for {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if err == syscall.ECHILD {
				return nil
			}
			if err != nil && err != syscall.EINTR {
				return fmt.Errorf("wait4: %w", err)
			}
			if pid <= 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			return errors.New("agent processes survive SIGKILL")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// useHarness readies the harness the pod's harness volume holds beside
// the worker's executable exe, whatever the image: opencode resolves from
// exe's directory first, and, as the pod mounts an emptyDir over HOME,
// the opencode bake one level up fills home's opencode config when home
// has none.
func useHarness(exe, home string) error {
	bin := filepath.Dir(exe)
	if err := os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		return err
	}
	config := filepath.Join(home, ".config", "opencode")
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		return nil
	}
	return os.CopyFS(config, os.DirFS(filepath.Join(bin, "..", "opencode-home", ".config", "opencode")))
}

const (
	workspace = "/workspace"
	// pushWait bounds the wait for factoryd's push token: the pod's 60 s
	// grace period after a stop also holds the commit and the push
	pushWait = 30 * time.Second
)

// workerEnv is the session's contract with factoryd. repoURL/repoToken read
// the task's repository and the bot's fork, in the clone init container
// only; pushURL/pushToken write the task branch to the fork, the only place
// a worker can write (ruling 97), with the token factoryd posts at the push.
type workerEnv struct {
	taskID, sessionID, objective, repository, repoSHA, branch string
	repoURL, repoToken, pushURL, pushToken                    string
	gatewayURL, tokenFile, model                              string
	strongerModel                                             string
	cheapModel                                                string
	answerTokens, headroomTokens                              int
	steps                                                     int
	contextTokens                                             int
	findings, answers, posterInfo, tools                      []string
	author, authorStanding                                    string
	home                                                      string
	// messageHash is sha256 of the session's message bearer, which only
	// factoryd holds
	messageHash [32]byte
	// report is a report session's guide (MODE report, ruling 185): no
	// push URL or token, nothing pushed, the last message the result
	report string
	// spec and cardRole are a plan's card's (SPEC, CARD_ROLE)
	spec, cardRole string
	// directories are the card's (DIRECTORIES), for the brief's map
	directories []string
	// migrationGlobs are the policy's migration directory globs
	// (MIGRATION_GLOBS) the push guard judges migration names against
	migrationGlobs []string
	// leftOut are factoryd's notes on what the env could not carry
	// (LEFT_OUT), for the brief
	leftOut []string
	// facts (FACTS) is factoryd's line for the brief
	facts string
	// gitTrace is a test's GIT_TRACE2 settings for every worker git; nil
	// in a pod
	gitTrace []string
	// image (IMAGE_MANIFEST) is what the session's project image holds;
	// nil on the factory's image
	image *imagegen.Manifest
	// pull is the pull request a split reads (PULL), whose head the clone
	// fetches as branch pull/<pull>
	pull string
	// taskPR is the task's own pull request (TASK_PR): a closed one's head
	// stays on the task's repository after its branch left the fork
	taskPR string
	// docsDir (DOCS_DIR) is where the clone put the project's document
	// repository; "" when the project has none
	docsDir string
	// docsURL (DOCS_URL) is the clone container's source for it
	docsURL string
	// egressCanary (EGRESS_CANARY, a name and a port) and egressDirectIP
	// (EGRESS_DIRECT_IP, an IP and a port) are public targets the egress
	// probe must not reach
	egressCanary, egressDirectIP string
}

func loadEnv() (workerEnv, error) {
	var e workerEnv
	str := func(name string) string { return os.Getenv(name) }
	e.taskID = str("TASK_ID")
	e.sessionID = str("SESSION_ID")
	e.objective = str("OBJECTIVE")
	e.repository = str("REPOSITORY")
	e.repoSHA = str("REPO_SHA")
	e.branch = str("BRANCH")
	messageHash := str("MESSAGE_TOKEN_SHA256")
	// opencode inherits this process's environment
	os.Unsetenv("MESSAGE_TOKEN_SHA256")
	e.pushURL = str("PUSH_URL")
	e.gatewayURL = str("GATEWAY_URL")
	e.tokenFile = str("TOKEN_FILE")
	e.model = str("MODEL")
	e.strongerModel = str("STRONGER_MODEL")
	e.cheapModel = str("CHEAP_MODEL")
	e.home = str("HOME")
	e.author, e.authorStanding = str("AUTHOR"), str("AUTHOR_STANDING")
	e.spec, e.cardRole = str("SPEC"), str("CARD_ROLE")
	e.docsDir = str("DOCS_DIR")
	e.facts = str("FACTS")
	e.egressCanary, e.egressDirectIP = str("EGRESS_CANARY"), str("EGRESS_DIRECT_IP")
	required := map[string]string{
		"TASK_ID": e.taskID, "OBJECTIVE": e.objective, "REPOSITORY": e.repository,
		"BRANCH": e.branch, "GATEWAY_URL": e.gatewayURL, "TOKEN_FILE": e.tokenFile, "MODEL": e.model,
		"EGRESS_CANARY": e.egressCanary, "EGRESS_DIRECT_IP": e.egressDirectIP,
	}
	if str("MODE") == "report" {
		e.report = str("REPORT_GUIDE")
		required["REPORT_GUIDE"] = e.report
	} else {
		required["PUSH_URL"] = e.pushURL
	}
	for name, val := range required {
		if val == "" {
			return e, fmt.Errorf("%s is required", name)
		}
	}
	if e.home == "" {
		e.home = "/home/worker"
	}
	if err := factoryd.CheckEgressTargets(e.egressCanary, e.egressDirectIP); err != nil {
		return e, fmt.Errorf("EGRESS_CANARY, EGRESS_DIRECT_IP: %w", err)
	}
	var err error
	if e.messageHash, err = parseHash(messageHash); err != nil {
		return e, err
	}
	if e.answerTokens, err = strconv.Atoi(str("ANSWER_TOKENS")); err != nil {
		return e, fmt.Errorf("ANSWER_TOKENS: %w", err)
	}
	if e.headroomTokens, err = strconv.Atoi(str("HEADROOM_TOKENS")); err != nil {
		return e, fmt.Errorf("HEADROOM_TOKENS: %w", err)
	}
	if e.steps, err = strconv.Atoi(str("STEPS")); err != nil || e.steps <= 0 {
		return e, fmt.Errorf("STEPS must be a positive step cap, is %q", str("STEPS"))
	}
	if raw := str("CONTEXT_TOKENS"); raw != "" {
		if e.contextTokens, err = strconv.Atoi(raw); err != nil || e.contextTokens < 0 {
			return e, fmt.Errorf("CONTEXT_TOKENS must be a token count, is %q", raw)
		}
	}
	// FINDINGS, ANSWERS, POSTER_INFO, TOOLS, DIRECTORIES, MIGRATION_GLOBS and LEFT_OUT arrive as JSON arrays; absent means none.
	for name, dst := range map[string]*[]string{"FINDINGS": &e.findings, "ANSWERS": &e.answers, "POSTER_INFO": &e.posterInfo, "TOOLS": &e.tools,
		"DIRECTORIES": &e.directories, "MIGRATION_GLOBS": &e.migrationGlobs, "LEFT_OUT": &e.leftOut} {
		if raw := str(name); raw != "" {
			if err := json.Unmarshal([]byte(raw), dst); err != nil {
				return e, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if raw := str("IMAGE_MANIFEST"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &e.image); err != nil {
			return e, fmt.Errorf("IMAGE_MANIFEST: %w", err)
		}
	}
	return e, nil
}

func authHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// safeGit is prepended to every worker git command: agent-planted hooks and
// fsmonitor never run. "-c" reaches nested gits through GIT_CONFIG_PARAMETERS,
// except upload-pack on a local fetch: git clears that variable for it, so
// uploadPack puts the flags on its command line.
var safeGit = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

var uploadPack = "--upload-pack=git " + strings.Join(safeGit, " ") + " upload-pack"

// git runs a token-free git command. HOME is agent-writable, so global and
// system config are ignored.
func (e workerEnv) git(dir string, args ...string) (string, error) {
	return e.runGit(dir, nil, args...)
}

// gitAuth runs git with token as an http.extraHeader in the environment
// (never args, never a config file). Only the clone init container, where
// no agent runs, and the final push from the worker's own repo, after
// every agent process is dead, use it.
func (e workerEnv) gitAuth(token, dir string, args ...string) error {
	_, err := e.runGit(dir, []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + authHeader(token),
	}, args...)
	return err
}

// runClone is the clone init container's mode, into ws: it alone holds
// the read token, on the task's repository, the bot's fork and the
// project's document repository (DOCS_URL), whose clone failing is recorded
// for the brief and does not fail the session.
func runClone(log func(string, ...any), ws string) int {
	v, err := required("REPO_URL", "REPO_TOKEN", "BRANCH")
	if err == nil {
		e := workerEnv{repoURL: v["REPO_URL"], repoToken: v["REPO_TOKEN"], branch: v["BRANCH"], repoSHA: os.Getenv("REPO_SHA"), pushURL: os.Getenv("PUSH_URL"), pull: os.Getenv("PULL"), taskPR: os.Getenv("TASK_PR"), docsURL: os.Getenv("DOCS_URL")}
		err = cloneBase(e, filepath.Join(ws, "repo"))
		if err == nil && e.docsURL != "" {
			recordDocs(log, ws, cloneDocs(e, filepath.Join(ws, "docs")))
		}
	}
	return recordFetch(log, ws, err)
}

// docPatterns are the files the document clone keeps: text, for reading.
var docPatterns = []string{"*.md", "*.html", "*.js", "*.css", "*.yaml", "*.yml", "*.txt", "*.json", "*.svg"}

// cloneDocs clones the default branch of the document repository
// into dir, shallow and partial, and checks out the text files only: the
// repository's images are most of its size. The checkout fetches the
// blobs, so it carries the token too.
func cloneDocs(e workerEnv, dir string) error {
	if err := e.gitAuth(e.repoToken, "", "clone", "-q", "--depth", "1", "--filter=blob:none", "--no-checkout", e.docsURL, dir); err != nil {
		return err
	}
	if _, err := e.git(dir, append([]string{"sparse-checkout", "set", "--no-cone"}, docPatterns...)...); err != nil {
		return err
	}
	return e.gitAuth(e.repoToken, dir, "checkout", "-q")
}

// docsFailed is where the clone container leaves why the document
// repository could not be cloned.
func docsFailed(ws string) string { return filepath.Join(ws, "docs-failed") }

func recordDocs(log func(string, ...any), ws string, err error) {
	if err == nil {
		return
	}
	if werr := os.WriteFile(docsFailed(ws), []byte(err.Error()), 0o600); werr != nil {
		log("record the document clone failure %q: %v", err, werr)
	}
}

// docsState is what the brief says of the document repository: cloned
// (docsDir set and no failure recorded), or why it was not.
func docsState(ws, docsDir string) (cloned bool, failed string) {
	if docsDir == "" {
		return false, ""
	}
	why, err := os.ReadFile(docsFailed(ws))
	switch {
	case err == nil:
		return false, textutil.Truncate(strings.TrimSpace(string(why)), 400)
	case errors.Is(err, fs.ErrNotExist):
		return true, ""
	default:
		return false, err.Error()
	}
}

// resolveDocLinks sets the brief's DocLinks from the clone at docsDir
// when the brief has one (factoryd.InboundBrief drops them for an
// untrusted author's card). A clone it cannot walk leaves them nil.
func resolveDocLinks(log func(string, ...any), brief *harness.TaskBrief, docsDir string) {
	if !brief.Docs {
		return
	}
	var tree []string
	err := filepath.WalkDir(docsDir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case p == docsDir, d.Type()&fs.ModeSymlink != 0:
			return nil
		}
		rel, err := filepath.Rel(docsDir, p)
		if err == nil {
			tree = append(tree, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		log("document links: %v", err)
		return
	}
	brief.DocLinks = doclinks.Lines(brief.Objective, tree, docsDir)
}

// cloneBase clones the task's repository at its base, with the head of a
// split's pull request as branch pull/<n>, and, when the session resumes a
// branch head that only the fork holds, fetches that branch from the fork,
// all with the read token. A closed pull request's branch can be gone from
// the fork (roadmap 214): its head then comes from the task's repository,
// which keeps it as refs/pull/<n>/head. The headers stay out of .git/config.
func cloneBase(e workerEnv, repoDir string) error {
	if err := e.gitAuth(e.repoToken, "", "clone", "-q", e.repoURL, repoDir); err != nil {
		return err
	}
	if e.pull != "" {
		if err := e.gitAuth(e.repoToken, repoDir, "fetch", "-q", e.repoURL, "+refs/pull/"+e.pull+"/head:refs/heads/pull/"+e.pull); err != nil {
			return err
		}
	}
	if e.repoSHA == "" {
		return nil
	}
	if _, err := e.git(repoDir, "cat-file", "-e", e.repoSHA+"^{commit}"); err == nil {
		return nil
	}
	err := e.gitAuth(e.repoToken, repoDir, "fetch", "-q", e.pushURL, "refs/heads/"+e.branch)
	if err == nil {
		return nil
	}
	if e.taskPR == "" {
		return fmt.Errorf("the start %s is on no branch the session can fetch: %w", e.repoSHA, err)
	}
	if perr := e.gitAuth(e.repoToken, repoDir, "fetch", "-q", e.repoURL, "+refs/pull/"+e.taskPR+"/head:refs/heads/"+e.branch); perr != nil {
		return fmt.Errorf("the start %s is on neither the fork's branch %s (%v) nor pull request #%s's head (%w)", e.repoSHA, e.branch, err, e.taskPR, perr)
	}
	if _, err := e.git(repoDir, "cat-file", "-e", e.repoSHA+"^{commit}"); err != nil {
		return fmt.Errorf("pull request #%s's head does not hold the start %s, and the fork's branch %s is gone", e.taskPR, e.repoSHA, e.branch)
	}
	return nil
}

func (e workerEnv) runGit(dir string, env []string, args ...string) (string, error) {
	return e.runGitCtx(context.Background(), dir, env, args...)
}

// maxGitOutput bounds what one worker git command may print into memory.
const maxGitOutput = 1 << 20

// gitBin is the image's own git: PATH leads with the agent-writable
// /cache/go/bin, where a planted git would otherwise take the push's call.
const gitBin = "/usr/bin/git"

// gitEnv is the whole environment of the worker's git, extra last: none
// of the image's, whose proxy, preload or git variables would reach the
// token's header.
func gitEnv(extra []string) []string {
	return append([]string{
		"PATH=/usr/bin:/bin",
		"HOME=" + os.Getenv("HOME"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}, extra...)
}

// runGitCtx scrubs the token from failures, because worker stderr reaches
// the factoryd log.
func (e workerEnv) runGitCtx(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitBin, append(append([]string(nil), safeGit...), args...)...) // #nosec G204 -- exec.CommandContext, no shell; args are fixed git subcommands plus this worker's own task-scoped values
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(gitEnv(e.gitTrace), env...)
	out := &capBuffer{max: maxGitOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err != nil {
		scrub := redact.New(e.repoToken, e.pushToken).String
		return "", fmt.Errorf("git %s: %v: %s", scrub(strings.Join(args, " ")), err, scrub(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// capBuffer keeps the first max bytes written to it and says whether it cut.
type capBuffer struct {
	bytes.Buffer
	max int
	cut bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		b.cut = true
	}
	b.Buffer.Write(p[:min(len(p), max(0, b.max-b.Len()))])
	return len(p), nil
}

// ReadFrom keeps io.Copy (os/exec's writer path) on Write: the buffer's own
// ReadFrom would grow past max unseen.
func (b *capBuffer) ReadFrom(r io.Reader) (int64, error) {
	var n int64
	p := make([]byte, 32<<10)
	for {
		k, err := r.Read(p)
		if k > 0 {
			n += int64(k)
			if _, werr := b.Write(p[:k]); werr != nil {
				return n, werr
			}
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// prepareRepo puts the clone on the task branch at the task SHA and returns
// that base commit and main's head at the clone (origin/HEAD), recorded
// before the agent can move it. The commit identity goes into the repo
// config so the agent's own commits carry it too.
func prepareRepo(repoDir string, e workerEnv) (base, mainHead string, err error) {
	// the adapter writes brief.json into the work tree; keep it out of the agent's own commits
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte("/brief.json\n"), 0o600); err != nil {
		return "", "", err
	}
	if mainHead, err = e.git(repoDir, "rev-parse", "--verify", "refs/remotes/origin/HEAD^{commit}"); err != nil {
		return "", "", err
	}
	steps := [][]string{{"config", "user.name", botName}, {"config", "user.email", botEmail}}
	if e.repoSHA != "" {
		steps = append(steps, []string{"checkout", "-q", e.repoSHA})
	}
	steps = append(steps, []string{"checkout", "-q", "-B", e.branch})
	for _, args := range steps {
		if _, err := e.git(repoDir, args...); err != nil {
			return "", "", err
		}
	}
	base, err = e.git(repoDir, "rev-parse", "HEAD")
	return base, mainHead, err
}

// cardMap is the brief's repository map of the card's directories in the
// clone, read at the session's start commit before the agent runs and
// before brief.json lands in the work tree.
func cardMap(repoDir string, dirs []string) (string, error) {
	root, err := os.OpenRoot(repoDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return factoryd.RepoMap(root.FS(), dirs), nil
}

const (
	botName  = "factory-agent"
	botEmail = "factory-agent@factory.local"
)

// commitAndPush never runs a token-bearing git in the agent's repo, whose
// config, hooks and work tree the agent controls. A fresh worker repo
// fetches the agent's HEAD, the start and mainHead (main at the clone) by
// their ids, stages the agent's work tree on top under the worker's own
// config (so no agent filter, hook or fsmonitor runs), commits any
// leftovers, and pushes to the task URL if the result moved past base,
// with the token pushToken gets for the commit. killAgent runs before
// pushToken, so no agent process is alive once a token is in the worker.
// Returns the pushed commit, empty when there was nothing to push.
func commitAndPush(agentRepo string, e workerEnv, base, mainHead string, killAgent func() error, pushToken func(commit string) (string, error)) (string, error) {
	own, err := os.MkdirTemp(filepath.Dir(agentRepo), ".push-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(own)
	if _, err := e.git(own, "init", "-q"); err != nil {
		return "", err
	}
	// the adapter writes brief.json into the work tree; it must never be committed
	if err := os.WriteFile(filepath.Join(own, ".git", "info", "exclude"), []byte("/brief.json\n"), 0o600); err != nil {
		return "", err
	}
	// the start commit and main too: an agent that reset its branch may no
	// longer have them in its HEAD's history
	if _, err := e.git(own, "fetch", "-q", "--no-tags", uploadPack, agentRepo, "+HEAD:refs/heads/agent", "+"+base+":refs/heads/start",
		"+"+mainHead+":refs/heads/main"); err != nil {
		return "", err
	}
	tree, err := stageWorkTree(own, agentRepo, e)
	if err != nil {
		return "", err
	}
	baseTree, err := e.git(own, "rev-parse", base+"^{tree}")
	if err != nil {
		return "", err
	}
	if tree == baseTree {
		return "", nil
	}
	agent, err := e.git(own, "rev-parse", "refs/heads/agent")
	if err != nil {
		return "", err
	}
	if err := killAgent(); err != nil {
		return "", fmt.Errorf("stop agent processes: %w", err)
	}
	// The fetch ran upload-pack under the agent's repo config, so the worker
	// repo's config is not trusted either: the commit and the token-bearing
	// push run from a bare repo made after every agent process is dead,
	// borrowing the worker repo's objects only.
	send, err := os.MkdirTemp(filepath.Dir(agentRepo), ".send-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(send)
	if _, err := e.git("", "init", "-q", "--bare", send); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(send, "objects", "info", "alternates"), []byte(filepath.Join(own, ".git", "objects")+"\n"), 0o600); err != nil {
		return "", err
	}
	parents, guard := sessionParents(e, send, base, mainHead, agent)
	guardTree, err := e.git(send, "rev-parse", guard+"^{tree}")
	if err != nil {
		return "", err
	}
	// a session that only pulled main has nothing of its own to push. One
	// that merged main onto this branch's own commits pushes the merge even
	// when its resolution leaves the tree identical to main's: the merge
	// commit is what lets the pull request merge — the branch's history
	// stays the task's while its content is main's.
	if tree == guardTree && !slices.Contains(parents, base) {
		return "", nil
	}
	// one commit of the worker's own: the agent's commits, and the mentions
	// or closing keywords their messages may carry, stay out
	head, err := e.runGit(send, botIdentity, append(append([]string{"commit-tree", tree}, parents...), "-m", commitMessage(e))...)
	if err != nil {
		return "", err
	}
	if err := refuseWorkflowDirChange(e, send, guard, head); err != nil {
		return "", err
	}
	if err := refuseMisnamedMigrations(e, send, guard, head); err != nil {
		return "", err
	}
	if e.pushToken, err = pushToken(head); err != nil {
		return "", err
	}
	if err := e.gitAuth(e.pushToken, "", "--git-dir="+send, "push", "-q", e.pushURL, head+":refs/heads/"+e.branch); err != nil {
		return "", err
	}
	return head, nil
}

// sessionParents answers the pushed commit's parents and the commit whose
// workflow directories it must keep. The start commit is the parent, unless
// the agent pulled main commits newer than the one the start is on (the
// merge base of its HEAD and mainHead descends from the start's): then
// that newest main commit is the parent, next to the start when the start
// is a resumed session's pushed work in progress, so main's changes stay
// main's in the pull request's diff. The agent holds no token, so the main
// it can pull is the clone's.
func sessionParents(e workerEnv, send, base, mainHead, agent string) ([]string, string) {
	start, err := e.git(send, "merge-base", base, mainHead)
	if err != nil {
		return []string{"-p", base}, base
	}
	tip, err := e.git(send, "merge-base", agent, mainHead)
	if err != nil || tip == start {
		return []string{"-p", base}, base
	}
	if _, err := e.git(send, "merge-base", "--is-ancestor", start, tip); err != nil {
		return []string{"-p", base}, base
	}
	if start == base {
		return []string{"-p", tip}, tip
	}
	return []string{"-p", base, "-p", tip}, tip
}

// refuseWorkflowDirChange stops a push whose tree entries for the workflow
// directories (.forgejo, .gitea, .github) differ from the base's (ruling
// 100): Forgejo's protected_file_patterns rejects every new factory/*
// branch outright, so branch protection cannot gate this — the worker,
// trusted and running with the agent already dead, is where the guard
// lives. Object ids are compared, never a diff's path listing, so a
// rename out of the directory, a quoted non-ASCII name or a mode change
// all show. Computed against own, the repo the worker itself fetched and
// staged, never against the agent-controlled repo.
func refuseWorkflowDirChange(e workerEnv, own, base, head string) error {
	was, err := workflowTreeEntries(e, own, base)
	if err != nil {
		return err
	}
	now, err := workflowTreeEntries(e, own, head)
	if err != nil {
		return err
	}
	for _, dir := range factoryd.WorkflowDirs {
		if was[dir] != now[dir] {
			return fmt.Errorf("refusing to push: %s/ differs from the base (%s)", dir, base)
		}
	}
	return nil
}

// workflowTreeEntries maps each workflow directory to the ls-tree lines
// (mode, type, object id, name) of every top-level entry in commit naming
// it in any case (factoryd.WorkflowDir); an absent directory has no key.
// A commit that does not exist fails, it never reads as "no workflow
// directories".
func workflowTreeEntries(e workerEnv, own, commit string) (map[string]string, error) {
	out, err := e.git(own, "ls-tree", commit)
	if err != nil {
		return nil, err
	}
	entries := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		_, name, _ := strings.Cut(line, "\t")
		if dir, ok := factoryd.WorkflowDir(name); ok {
			entries[dir] += line + "\n"
		}
	}
	return entries, nil
}

// migrationName is the pattern a migration file carries: a UTC timestamp
// and a slug, never the next number (roadmap 186's guide sentence, now a
// guard).
var migrationName = regexp.MustCompile(`^[0-9]{14}_[a-z0-9_]+\.sql$`)

// refuseMisnamedMigrations stops a push that adds a .sql file under one
// of the policy's migration directory globs — a glob with ** in it; an
// exact path like internal/policy/schema.sql marks one file for review,
// not a directory of timestamped migrations — whose name is off the
// pattern. Only added files are judged: an edited migration keeps its
// name. The listing runs with --no-renames and core.quotePath off, so a
// rename into a bad name lists as an add and a non-ASCII path prints raw
// (refuseWorkflowDirChange's comment names the same holes). Like it a
// guard of the trusted worker, run in the repo it built itself; the
// refusal reaches the next session as a finding.
func refuseMisnamedMigrations(e workerEnv, own, base, head string) error {
	var globs []string
	for _, g := range e.migrationGlobs {
		if strings.Contains(g, "**") {
			globs = append(globs, g)
		}
	}
	if len(globs) == 0 {
		return nil
	}
	out, err := e.git(own, "-c", "core.quotePath=off", "diff", "--name-only", "--diff-filter=A", "--no-renames", base, head)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(out, "\n") {
		if !strings.HasSuffix(p, ".sql") || !slices.ContainsFunc(globs, func(g string) bool { return policy.GlobMatch(g, p) }) {
			continue
		}
		if !migrationName.MatchString(path.Base(p)) {
			return fmt.Errorf("refusing to push: migration %s is misnamed: <UTC yyyymmddhhmmss>_<slug>.sql, never the next number", p)
		}
	}
	return nil
}

var botIdentity = []string{
	"GIT_AUTHOR_NAME=" + botName, "GIT_AUTHOR_EMAIL=" + botEmail,
	"GIT_COMMITTER_NAME=" + botName, "GIT_COMMITTER_EMAIL=" + botEmail,
}

// stageWorkTree indexes the agent's work tree, starting from the agent's
// HEAD tree, in the worker's repo and returns the tree ID. Scratch
// reproduction tests stay out of the index, and a tree that holds nothing
// else writes the agent's HEAD tree, so the caller pushes nothing.
func stageWorkTree(own, agentRepo string, e workerEnv) (string, error) {
	env := []string{"GIT_DIR=" + filepath.Join(own, ".git"), "GIT_WORK_TREE=" + agentRepo}
	for _, args := range [][]string{{"read-tree", "refs/heads/agent"}, {"add", "-A"}} {
		if _, err := e.runGit(own, env, args...); err != nil {
			return "", err
		}
	}
	if err := dropScratchReproTests(own, env, e); err != nil {
		return "", err
	}
	return e.runGit(own, env, "write-tree")
}

// dropScratchReproTests removes every scratch reproduction test from the
// staging index. The work guide asks an agent to write its reproduction as
// zz_repro_*_test.go; that file is the agent's evidence, not the change, and
// committing it copies a duplicate of a real test into the diff, as
// internal/factoryd/zz_repro_unjudged_test.go did. The index, not the work
// tree, is the commit's source, so the removal never touches the checkout.
func dropScratchReproTests(own string, env []string, e workerEnv) error {
	paths, err := e.runGit(own, env, "ls-files", "--", "*zz_repro_*_test.go", ":(glob)**/zz_repro_*_test.go")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(paths, "\n") {
		if path == "" {
			continue
		}
		if _, err := e.runGit(own, env, "rm", "--cached", "-q", "--", path); err != nil {
			return err
		}
	}
	return nil
}

// commitMessage is the worker's commit's message: the task and the
// issue's title, sanitised, and the trailers that attribute the commit,
// written here from the session's env and never by the agent.
func commitMessage(e workerEnv) string {
	subject := e.objective
	if i := strings.IndexAny(subject, "\r\n"); i >= 0 {
		subject = subject[:i]
	}
	if len(subject) > 72 {
		subject = subject[:72]
	}
	return factoryd.Attribution{Role: "agent", Task: e.taskID, Session: e.sessionID, Human: e.author}.Message(
		fmt.Sprintf("factory task %s: %s", e.taskID, sanitize.Outbound(subject, nil)))
}

type diffStat struct{ Files, Insertions, Deletions int }

// checkpointer measures the agent's diff against the base commit for the
// supervisor's churn detector and progress digest. Like the push path it
// never runs git with the agent repo as its repository: store holds the
// base objects, fetched before the agent starts, and every measurement
// indexes the agent's work tree into a fresh repo of the worker's own that
// borrows those objects. Hooks and fsmonitor are off, diffs run without
// external diff or textconv, system and global config are ignored, and
// no token is in the environment. The agent runs under the same uid and
// could still write to a fresh repo while it exists; what it could plant
// there runs with the rights it already has.
type checkpointer struct {
	e                       workerEnv
	work, store, base, fork string
	// stage runs the checkpoint's git commands. It defaults to the worker's
	// own runGitCtx; only a test replaces it, to make one staging step fail.
	stage func(ctx context.Context, dir string, env []string, args ...string) (string, error)
}

func newCheckpointer(agentRepo string, e workerEnv, base, mainHead string) (*checkpointer, error) {
	store, err := os.MkdirTemp(filepath.Dir(agentRepo), ".ckpt-base-")
	if err != nil {
		return nil, err
	}
	if _, err := e.git(store, "init", "-q"); err != nil {
		return nil, err
	}
	if _, err := e.git(store, "fetch", "-q", "--no-tags", uploadPack, agentRepo, "+HEAD:refs/heads/base", "+"+mainHead+":refs/heads/main"); err != nil {
		return nil, err
	}
	if got, err := e.git(store, "rev-parse", "refs/heads/base"); err != nil {
		return nil, err
	} else if got != base {
		return nil, fmt.Errorf("checkpoint base = %s, want %s", got, base)
	}
	fork, err := e.git(store, "merge-base", "refs/heads/main", base)
	if err != nil {
		return nil, fmt.Errorf("checkpoint store: %w", err)
	}
	return &checkpointer{e: e, work: agentRepo, store: store, base: base, fork: fork}, nil
}

// maxPatch bounds the diff text a checkpoint carries.
const maxPatch = 8 << 10

func (c *checkpointer) diff(ctx context.Context) (session, card diffStat, patch string, err error) {
	tmp, err := os.MkdirTemp(filepath.Dir(c.work), ".ckpt-")
	if err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	defer os.RemoveAll(tmp)
	if _, err := c.e.runGitCtx(ctx, tmp, nil, "init", "-q"); err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	gitDir := filepath.Join(tmp, ".git")
	if err := os.WriteFile(filepath.Join(gitDir, "objects", "info", "alternates"), []byte(filepath.Join(c.store, ".git", "objects")+"\n"), 0o600); err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte("/brief.json\n"), 0o600); err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	env := []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + c.work}
	run := c.stage
	if run == nil {
		run = c.e.runGitCtx
	}
	if _, err := run(ctx, tmp, env, "read-tree", c.base); err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	// A file the agent removes between read-tree and add makes git add -A
	// exit 128 (fatal: unable to stat). Ignore it: the index reflects the
	// work tree as it was at the moment of staging, a valid snapshot, and
	// the diff against the base below stays correct.
	_, _ = run(ctx, tmp, env, "add", "-A")
	diff := []string{"diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color"}
	short, err := run(ctx, tmp, env, append(diff, "--shortstat", c.base)...)
	if err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	cardShort, err := run(ctx, tmp, env, append(diff, "--shortstat", c.fork)...)
	if err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	patch, err = run(ctx, tmp, env, append(diff, c.base)...)
	if err != nil {
		return diffStat{}, diffStat{}, "", err
	}
	if len(patch) > maxPatch {
		patch = patch[:maxPatch] + "\n… (diff truncated)"
	}
	return parseShortstat(short), parseShortstat(cardShort), patch, nil
}

var shortstatPart = regexp.MustCompile(`(\d+) (file|insertion|deletion)`)

// parseShortstat reads git diff --shortstat: " 3 files changed, 40
// insertions(+), 7 deletions(-)", parts absent when zero.
func parseShortstat(s string) diffStat {
	var d diffStat
	for _, m := range shortstatPart.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "file":
			d.Files = n
		case "insertion":
			d.Insertions = n
		case "deletion":
			d.Deletions = n
		}
	}
	return d
}

// checkpointEvery is how often the worker measures the diff; a checkpoint
// event goes out only when the numbers changed.
const checkpointEvery = 15 * time.Second

func (c *checkpointer) loop(ctx context.Context, every time.Duration, emit func(harness.Event), log func(string, ...any)) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	var last, lastCard diffStat
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		measure, cancel := context.WithTimeout(ctx, time.Minute)
		stat, card, patch, err := c.diff(measure)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				log("checkpoint: %v", err)
			}
			continue
		}
		if stat == last && card == lastCard {
			continue
		}
		last, lastCard = stat, card
		emit(harness.Event{Time: time.Now(), Kind: harness.EventCheckpoint, Detail: map[string]any{
			"diff":      map[string]any{"files": stat.Files, "insertions": stat.Insertions, "deletions": stat.Deletions},
			"card_diff": map[string]any{"files": card.Files, "insertions": card.Insertions, "deletions": card.Deletions},
			"patch":     patch,
		}})
	}
}
