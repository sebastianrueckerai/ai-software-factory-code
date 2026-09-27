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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func main() {
	log := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "agent-worker: "+format+"\n", args...)
	}
	code := run(log)
	os.Exit(code)
}

func run(log func(string, ...any)) int {
	// killOthers relies on every agent process being in this PID namespace
	// and reparenting to the worker.
	if os.Getpid() != 1 {
		log("must run as the container's init (pid 1), runs as pid %d", os.Getpid())
		return 1
	}
	// The agent runs as the same uid; a non-dumpable worker keeps its
	// /proc/<pid>/environ (which still holds the git token) unreadable to it.
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		log("prctl PR_SET_DUMPABLE: %v", errno)
		return 1
	}
	env, err := loadEnv()
	if err != nil {
		log("%v", err)
		return 1
	}
	ctx := context.Background()
	log("task %s session %s", env.taskID, env.sessionID)

	// The pod mounts an emptyDir over HOME (the image root is read-only), so
	// restore the image-baked opencode provider packages into it.
	if _, err := os.Stat(filepath.Join(env.home, ".config", "opencode")); os.IsNotExist(err) {
		if err := os.CopyFS(filepath.Join(env.home, ".config", "opencode"), os.DirFS(bakedConfigDir)); err != nil {
			log("restore pre-baked opencode config: %v", err)
			return 1
		}
	}

	repoDir := filepath.Join(workspace, "repo")
	// no agent process exists yet; the header stays out of .git/config
	if err := env.gitAuth("", "clone", "-q", env.repoURL, repoDir); err != nil {
		log("clone: %v", err)
		return 1
	}
	base, err := prepareRepo(repoDir, env)
	if err != nil {
		log("prepare repo: %v", err)
		return 1
	}

	brief := harness.TaskBrief{
		Objective:  env.objective,
		Repository: env.repository,
		SHA:        env.repoSHA,
		Findings:   env.findings,
	}
	oc := &factoryd.Opencode{
		WorkDir:    repoDir,
		GatewayURL: env.gatewayURL,
		TokenFile:  env.tokenFile,
		Model:      env.model,
		Budget:     factoryd.TokenBudget{AnswerTokens: env.answerTokens, HeadroomTokens: env.headroomTokens},
	}
	if err := oc.Prepare(ctx, harness.SandboxEnv{}, brief); err != nil {
		log("prepare: %v", err)
		return 1
	}

	ck, err := newCheckpointer(repoDir, env, base)
	if err != nil {
		log("checkpoint store: %v", err)
		return 1
	}
	var emitMu sync.Mutex
	emit := func(ev harness.Event) {
		line, err := json.Marshal(harness.WorkerEnvelope{Type: "event", Event: &ev})
		if err != nil {
			return
		}
		emitMu.Lock()
		defer emitMu.Unlock()
		fmt.Println(string(line))
	}
	ckCtx, stopCheckpoints := context.WithCancel(ctx)
	ckDone := make(chan struct{})
	go func() {
		defer close(ckDone)
		ck.loop(ckCtx, checkpointEvery, emit, log)
	}()
	res := oc.Run(ctx, brief, emit)
	stopCheckpoints()
	<-ckDone
	if err := killOthers(); err != nil {
		res = harness.Result{Success: false, Summary: "stop agent processes: " + err.Error()}
	}

	if res.Success {
		commit, err := commitAndPush(repoDir, env, base, killOthers)
		if err != nil {
			res = harness.Result{Success: false, Summary: "push failed: " + err.Error()}
		} else if commit != "" {
			emit(harness.Event{Time: time.Now(), Kind: harness.EventPushed, Detail: map[string]any{"branch": env.branch, "commit": commit}})
			res.Summary = "pushed branch " + env.branch + " at " + commit
		} else {
			res.Summary = "no changes to push"
		}
	}
	line, _ := json.Marshal(harness.WorkerEnvelope{Type: "result", Result: &res})
	fmt.Println(string(line))
	if !res.Success {
		log("result: %s", res.Summary)
		return 1
	}
	return 0
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

const (
	bakedConfigDir = "/opt/opencode-home/.config/opencode"
	workspace      = "/workspace"
)

type workerEnv struct {
	taskID, sessionID, objective, repository, repoSHA, branch, gitToken string
	repoURL, gatewayURL, tokenFile, model                               string
	answerTokens, headroomTokens                                        int
	findings                                                            []string
	home                                                                string
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
	e.gitToken = str("GIT_TOKEN")
	// opencode inherits this process's environment
	os.Unsetenv("GIT_TOKEN")
	e.repoURL = str("REPO_URL")
	e.gatewayURL = str("GATEWAY_URL")
	e.tokenFile = str("TOKEN_FILE")
	e.model = str("MODEL")
	e.home = str("HOME")
	for name, val := range map[string]string{
		"TASK_ID": e.taskID, "OBJECTIVE": e.objective, "REPOSITORY": e.repository, "REPO_URL": e.repoURL,
		"BRANCH": e.branch, "GIT_TOKEN": e.gitToken, "GATEWAY_URL": e.gatewayURL,
		"TOKEN_FILE": e.tokenFile, "MODEL": e.model,
	} {
		if val == "" {
			return e, fmt.Errorf("%s is required", name)
		}
	}
	if e.home == "" {
		e.home = "/home/worker"
	}
	var err error
	if e.answerTokens, err = strconv.Atoi(str("ANSWER_TOKENS")); err != nil {
		return e, fmt.Errorf("ANSWER_TOKENS: %w", err)
	}
	if e.headroomTokens, err = strconv.Atoi(str("HEADROOM_TOKENS")); err != nil {
		return e, fmt.Errorf("HEADROOM_TOKENS: %w", err)
	}
	// FINDINGS arrives as a JSON array; absent means a first attempt.
	if raw := str("FINDINGS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &e.findings); err != nil {
			return e, fmt.Errorf("FINDINGS: %w", err)
		}
	}
	return e, nil
}

func (e workerEnv) authHeader() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+e.gitToken))
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

// gitAuth runs git with the task token as an http.extraHeader in the
// environment (never args, never a config file). Only the initial clone,
// before the agent exists, and the final push from the worker's own repo,
// after every agent process is dead, use it.
func (e workerEnv) gitAuth(dir string, args ...string) error {
	_, err := e.runGit(dir, []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + e.authHeader(),
	}, args...)
	return err
}

func (e workerEnv) runGit(dir string, env []string, args ...string) (string, error) {
	return e.runGitCtx(context.Background(), dir, env, args...)
}

// maxGitOutput bounds what one worker git command may print into memory.
const maxGitOutput = 1 << 20

// runGitCtx scrubs the token from failures, because worker stderr reaches
// the factoryd log.
func (e workerEnv) runGitCtx(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string(nil), safeGit...), args...)...) // #nosec G204 -- exec.CommandContext, no shell; args are fixed git subcommands plus this worker's own task-scoped values
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	), env...)
	out := &capBuffer{max: maxGitOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err != nil {
		redact := func(s string) string {
			if e.gitToken == "" {
				return s
			}
			s = strings.ReplaceAll(s, e.authHeader(), "Authorization: ***")
			return strings.ReplaceAll(s, e.gitToken, "***")
		}
		return "", fmt.Errorf("git %s: %v: %s", redact(strings.Join(args, " ")), err, redact(out.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// capBuffer keeps the first max bytes written to it.
type capBuffer struct {
	bytes.Buffer
	max int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.Buffer.Write(p[:min(len(p), max(0, b.max-b.Len()))])
	return len(p), nil
}

// prepareRepo puts the clone on the task branch at the task SHA and returns
// that base commit. The commit identity goes into the repo config so the
// agent's own commits carry it too.
func prepareRepo(repoDir string, e workerEnv) (string, error) {
	// the adapter writes brief.json into the work tree; keep it out of the agent's own commits
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte("/brief.json\n"), 0o600); err != nil {
		return "", err
	}
	steps := [][]string{{"config", "user.name", botName}, {"config", "user.email", botEmail}}
	if e.repoSHA != "" {
		steps = append(steps, []string{"checkout", "-q", e.repoSHA})
	}
	steps = append(steps, []string{"checkout", "-q", "-B", e.branch})
	for _, args := range steps {
		if _, err := e.git(repoDir, args...); err != nil {
			return "", err
		}
	}
	return e.git(repoDir, "rev-parse", "HEAD")
}

const (
	botName  = "factory-agent"
	botEmail = "factory-agent@factory.local"
)

// commitAndPush never runs a token-bearing git in the agent's repo, whose
// config, hooks and work tree the agent controls. A fresh worker repo
// fetches the agent's HEAD, stages the agent's work tree on top under the
// worker's own config (so no agent filter, hook or fsmonitor runs), commits
// any leftovers, and pushes to the task URL if the result moved past base.
// killAgent runs right before the push, so no agent process is alive while
// the token is in a git process's environment. Returns the pushed commit,
// empty when there was nothing to push.
func commitAndPush(agentRepo string, e workerEnv, base string, killAgent func() error) (string, error) {
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
	if _, err := e.git(own, "fetch", "-q", "--no-tags", uploadPack, agentRepo, "+HEAD:refs/heads/agent"); err != nil {
		return "", err
	}
	head, err := e.git(own, "rev-parse", "refs/heads/agent")
	if err != nil {
		return "", err
	}
	tree, err := stageWorkTree(own, agentRepo, e)
	if err != nil {
		return "", err
	}
	if headTree, err := e.git(own, "rev-parse", "refs/heads/agent^{tree}"); err != nil {
		return "", err
	} else if tree != headTree {
		if head, err = e.runGit(own, botIdentity, "commit-tree", tree, "-p", head, "-m", commitMessage(e)); err != nil {
			return "", err
		}
	}
	if head == base {
		return "", nil
	}
	if err := refuseForgejoPathChange(e, own, base, head); err != nil {
		return "", err
	}
	if err := killAgent(); err != nil {
		return "", fmt.Errorf("stop agent processes: %w", err)
	}
	// The fetch ran upload-pack under the agent's repo config, so the worker
	// repo's config is not trusted either: the token-bearing push runs from a
	// bare repo made after every agent process is dead, borrowing the worker
	// repo's objects only.
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
	if err := e.gitAuth("", "--git-dir="+send, "push", "-q", e.repoURL, head+":refs/heads/"+e.branch); err != nil {
		return "", err
	}
	return head, nil
}

// refuseForgejoPathChange stops a push whose diff against base touches
// .forgejo/: Forgejo's protected_file_patterns rejects every new factory/*
// branch outright (ruling 90), so branch protection cannot gate this — the
// worker, trusted and running with the agent already dead, is where the
// guard has to live instead. Computed against own, the repo the worker
// itself fetched and staged, never against the agent-controlled repo.
func refuseForgejoPathChange(e workerEnv, own, base, head string) error {
	diff, err := e.git(own, "diff", "--name-only", base, head)
	if err != nil {
		return err
	}
	for _, path := range strings.Split(diff, "\n") {
		if path == ".forgejo" || strings.HasPrefix(path, ".forgejo/") {
			return fmt.Errorf("refusing to push: %s changes .forgejo/", path)
		}
	}
	return nil
}

var botIdentity = []string{
	"GIT_AUTHOR_NAME=" + botName, "GIT_AUTHOR_EMAIL=" + botEmail,
	"GIT_COMMITTER_NAME=" + botName, "GIT_COMMITTER_EMAIL=" + botEmail,
}

// stageWorkTree indexes the agent's work tree, starting from the agent's
// HEAD tree, in the worker's repo and returns the tree ID.
func stageWorkTree(own, agentRepo string, e workerEnv) (string, error) {
	env := []string{"GIT_DIR=" + filepath.Join(own, ".git"), "GIT_WORK_TREE=" + agentRepo}
	for _, args := range [][]string{{"read-tree", "refs/heads/agent"}, {"add", "-A"}} {
		if _, err := e.runGit(own, env, args...); err != nil {
			return "", err
		}
	}
	return e.runGit(own, env, "write-tree")
}

func commitMessage(e workerEnv) string {
	subject := e.objective
	if i := strings.IndexAny(subject, "\r\n"); i >= 0 {
		subject = subject[:i]
	}
	if len(subject) > 72 {
		subject = subject[:72]
	}
	return fmt.Sprintf("factory task %s: %s", e.taskID, subject)
}

type diffStat struct{ Files, Insertions, Deletions int }

// checkpointer measures the agent's diff against the base commit for the
// supervisor's churn detector and progress digest. Like the push path it
// never runs git with the agent repo as its repository: store holds the
// base objects, fetched before the agent starts, and every measurement
// indexes the agent's work tree into a fresh repo of the worker's own that
// borrows those objects. Hooks and fsmonitor are off, diffs run without
// external diff or textconv, system and global config are ignored, and
// the token is not in the environment (loadEnv unset it). The agent runs
// under the same uid and could still write to a fresh repo while it
// exists; what it could plant there runs with the rights it already has.
type checkpointer struct {
	e                 workerEnv
	work, store, base string
}

func newCheckpointer(agentRepo string, e workerEnv, base string) (*checkpointer, error) {
	store, err := os.MkdirTemp(filepath.Dir(agentRepo), ".ckpt-base-")
	if err != nil {
		return nil, err
	}
	if _, err := e.git(store, "init", "-q"); err != nil {
		return nil, err
	}
	if _, err := e.git(store, "fetch", "-q", "--no-tags", uploadPack, agentRepo, "+HEAD:refs/heads/base"); err != nil {
		return nil, err
	}
	if got, err := e.git(store, "rev-parse", "refs/heads/base"); err != nil {
		return nil, err
	} else if got != base {
		return nil, fmt.Errorf("checkpoint base = %s, want %s", got, base)
	}
	return &checkpointer{e: e, work: agentRepo, store: store, base: base}, nil
}

// maxPatch bounds the diff text a checkpoint carries.
const maxPatch = 8 << 10

func (c *checkpointer) diff(ctx context.Context) (diffStat, string, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(c.work), ".ckpt-")
	if err != nil {
		return diffStat{}, "", err
	}
	defer os.RemoveAll(tmp)
	if _, err := c.e.runGitCtx(ctx, tmp, nil, "init", "-q"); err != nil {
		return diffStat{}, "", err
	}
	gitDir := filepath.Join(tmp, ".git")
	if err := os.WriteFile(filepath.Join(gitDir, "objects", "info", "alternates"), []byte(filepath.Join(c.store, ".git", "objects")+"\n"), 0o600); err != nil {
		return diffStat{}, "", err
	}
	if err := os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte("/brief.json\n"), 0o600); err != nil {
		return diffStat{}, "", err
	}
	env := []string{"GIT_DIR=" + gitDir, "GIT_WORK_TREE=" + c.work}
	for _, args := range [][]string{{"read-tree", c.base}, {"add", "-A"}} {
		if _, err := c.e.runGitCtx(ctx, tmp, env, args...); err != nil {
			return diffStat{}, "", err
		}
	}
	diff := []string{"diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color"}
	short, err := c.e.runGitCtx(ctx, tmp, env, append(diff, "--shortstat", c.base)...)
	if err != nil {
		return diffStat{}, "", err
	}
	patch, err := c.e.runGitCtx(ctx, tmp, env, append(diff, c.base)...)
	if err != nil {
		return diffStat{}, "", err
	}
	if len(patch) > maxPatch {
		patch = patch[:maxPatch] + "\n… (diff truncated)"
	}
	return parseShortstat(short), patch, nil
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
	last := diffStat{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		measure, cancel := context.WithTimeout(ctx, time.Minute)
		stat, patch, err := c.diff(measure)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				log("checkpoint: %v", err)
			}
			continue
		}
		if stat == last {
			continue
		}
		last = stat
		emit(harness.Event{Time: time.Now(), Kind: harness.EventCheckpoint, Detail: map[string]any{
			"diff":  map[string]any{"files": stat.Files, "insertions": stat.Insertions, "deletions": stat.Deletions},
			"patch": patch,
		}})
	}
}
