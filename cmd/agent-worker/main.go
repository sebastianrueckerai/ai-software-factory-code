// agent-worker is the in-sandbox half of the runner (architecture 6.4). It
// clones the task's repository, drives the opencode adapter against the
// model gateway with the projected task token, commits and pushes the
// result, and reports progress to factoryd as JSONL envelopes on stdout —
// stdout is the only channel out of the sandbox, so no egress rule beyond
// the allowlist is ever needed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

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
	if err := env.gitRun("", "clone", env.repoURLWithToken(), repoDir); err != nil {
		log("clone: %v", err)
		return 1
	}
	// the adapter writes brief.json into the work tree; it must never be committed
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "info", "exclude"), []byte("/brief.json\n"), 0o644); err != nil {
		log("exclude brief.json: %v", err)
		return 1
	}
	if env.repoSHA != "" {
		if err := env.gitRun(repoDir, "checkout", env.repoSHA); err != nil {
			log("checkout %s: %v", env.repoSHA, err)
			return 1
		}
	}
	if err := env.gitRun(repoDir, "checkout", "-B", env.branch); err != nil {
		log("branch %s: %v", env.branch, err)
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
		pushed, err := commitAndPush(repoDir, env)
		if err != nil {
			res = harness.Result{Success: false, Summary: "push failed: " + err.Error()}
		} else if pushed {
			res.Summary = "pushed branch " + env.branch
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

func (e workerEnv) repoURLWithToken() string {
	u, err := url.Parse(e.repoURL)
	if err != nil {
		return e.repoURL
	}
	u.User = url.UserPassword("x-access-token", e.gitToken)
	return u.String()
}

// gitRun runs git and scrubs the task token from failures: git echoes the
// authenticated URL in args and stderr, and worker stderr reaches the
// factoryd log stream.
func (e workerEnv) gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=user.name",
		"GIT_CONFIG_VALUE_0=factory-agent",
		"GIT_CONFIG_KEY_1=user.email",
		"GIT_CONFIG_VALUE_1=factory-agent@factory.local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		redact := func(s string) string {
			if e.gitToken == "" {
				return s
			}
			return strings.ReplaceAll(s, e.gitToken, "***")
		}
		return fmt.Errorf("git %s: %v: %s", redact(strings.Join(args, " ")), err, redact(string(out)))
	}
	return nil
}

// commitAndPush commits everything the adapter changed and pushes the task
// branch. Reports whether a commit was created; a no-change task is fine.
func commitAndPush(repoDir string, e workerEnv) (bool, error) {
	status, err := exec.Command("git", "-C", repoDir, "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	if len(status) == 0 {
		return false, nil
	}
	subject := e.objective
	if i := strings.IndexAny(subject, "\r\n"); i >= 0 {
		subject = subject[:i]
	}
	if len(subject) > 72 {
		subject = subject[:72]
	}
	if err := e.gitRun(repoDir, "add", "-A"); err != nil {
		return false, err
	}
	if err := e.gitRun(repoDir, "commit", "-m", fmt.Sprintf("factory task %s: %s", e.taskID, subject)); err != nil {
		return false, err
	}
	if err := e.gitRun(repoDir, "push", e.repoURLWithToken(), "HEAD:refs/heads/"+e.branch); err != nil {
		return false, err
	}
	return true, nil
}
