// agent-worker is the in-sandbox half of the runner (architecture 6.4). It
// clones the task's repository, drives the opencode adapter against the
// model gateway with the projected task token, commits and pushes the
// result, and reports progress to factoryd as JSONL envelopes on stdout —
// stdout is the only channel out of the sandbox, so no egress rule beyond
// the allowlist is ever needed.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// the token travels as a per-command header, so .git/config keeps the
	// plain URL and nothing the agent can read holds the credential
	if err := env.gitRun("", "clone", env.repoURL, repoDir); err != nil {
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

	emit := func(ev harness.Event) {
		line, err := json.Marshal(harness.WorkerEnvelope{Type: "event", Event: &ev})
		if err != nil {
			return
		}
		fmt.Println(string(line))
	}
	res := oc.Run(ctx, brief, emit)

	if res.Success {
		commit, err := commitAndPush(repoDir, env, base)
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
	json.Unmarshal([]byte(str("FINDINGS")), &e.findings)
	return e, nil
}

func (e workerEnv) authHeader() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+e.gitToken))
}

// gitRun runs git with the task token as a per-command http.extraHeader
// (environment config, so it never lands in args or .git/config) and scrubs
// the token from failures, because worker stderr reaches the factoryd log.
func (e workerEnv) gitRun(dir string, args ...string) error {
	_, err := e.git(dir, args...)
	return err
}

func (e workerEnv) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0="+e.authHeader(),
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		redact := func(s string) string {
			if e.gitToken == "" {
				return s
			}
			s = strings.ReplaceAll(s, e.authHeader(), "Authorization: ***")
			return strings.ReplaceAll(s, e.gitToken, "***")
		}
		return "", fmt.Errorf("git %s: %v: %s", redact(strings.Join(args, " ")), err, redact(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// prepareRepo puts the clone on the task branch at the task SHA and returns
// that base commit. The commit identity goes into the repo config so the
// agent's own commits carry it too.
func prepareRepo(repoDir string, e workerEnv) (string, error) {
	// the adapter writes brief.json into the work tree; it must never be committed
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte("/brief.json\n"), 0o644); err != nil {
		return "", err
	}
	steps := [][]string{{"config", "user.name", "factory-agent"}, {"config", "user.email", "factory-agent@factory.local"}}
	if e.repoSHA != "" {
		steps = append(steps, []string{"checkout", "-q", e.repoSHA})
	}
	steps = append(steps, []string{"checkout", "-q", "-B", e.branch})
	for _, args := range steps {
		if err := e.gitRun(repoDir, args...); err != nil {
			return "", err
		}
	}
	return e.git(repoDir, "rev-parse", "HEAD")
}

// commitAndPush commits whatever the agent left uncommitted and pushes the
// task branch if it moved past base, the agent's own commits included.
// Returns the pushed commit, empty when there was nothing to push.
func commitAndPush(repoDir string, e workerEnv, base string) (string, error) {
	status, err := e.git(repoDir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if err := commitAll(repoDir, e); err != nil {
			return "", err
		}
	}
	commit, err := e.git(repoDir, "rev-parse", "HEAD")
	if err != nil || commit == base {
		return "", err
	}
	if err := e.gitRun(repoDir, "push", "origin", "HEAD:refs/heads/"+e.branch); err != nil {
		return "", err
	}
	return commit, nil
}

func commitAll(repoDir string, e workerEnv) error {
	subject := e.objective
	if i := strings.IndexAny(subject, "\r\n"); i >= 0 {
		subject = subject[:i]
	}
	if len(subject) > 72 {
		subject = subject[:72]
	}
	if err := e.gitRun(repoDir, "add", "-A"); err != nil {
		return err
	}
	return e.gitRun(repoDir, "commit", "-m", fmt.Sprintf("factory task %s: %s", e.taskID, subject))
}
