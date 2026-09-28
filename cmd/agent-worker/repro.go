package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// reproEnv is the repro mode's contract with factoryd (GateReproduction):
// fetch ref (the pull request head on the task's repository) with a read
// token, check it is sha, run command there. No agent, no gateway token,
// no push token.
type reproEnv struct {
	repoURL, repoToken, ref, sha, command string
	timeout                               time.Duration
}

func loadReproEnv() (reproEnv, error) {
	e := reproEnv{
		repoURL: os.Getenv("REPO_URL"), repoToken: os.Getenv("REPO_TOKEN"), ref: os.Getenv("PR_REF"),
		sha: os.Getenv("REPO_SHA"), command: os.Getenv("REPRO_COMMAND"),
	}
	// the command inherits this process's environment
	os.Unsetenv("REPO_TOKEN")
	for name, val := range map[string]string{"REPO_URL": e.repoURL, "REPO_TOKEN": e.repoToken, "PR_REF": e.ref, "REPO_SHA": e.sha, "REPRO_COMMAND": e.command} {
		if val == "" {
			return e, fmt.Errorf("%s is required", name)
		}
	}
	secs, err := strconv.Atoi(os.Getenv("REPRO_TIMEOUT"))
	if err != nil || secs <= 0 {
		return e, fmt.Errorf("REPRO_TIMEOUT: want positive seconds")
	}
	e.timeout = time.Duration(secs) * time.Second
	return e, nil
}

// runRepro is the worker's repro mode: it reports one repro envelope on
// stdout, whatever happens, and exits 0 once it reported.
func runRepro(log func(string, ...any)) int {
	e, err := loadReproEnv()
	rep := harness.Repro{Exit: -1}
	if err != nil {
		rep.Output = "repro mode: " + err.Error()
	} else {
		rep = reproduce(e, filepath.Join(workspace, "repo"))
	}
	if err := killOthers(); err != nil {
		log("stop the command's processes: %v", err)
	}
	line, _ := json.Marshal(harness.WorkerEnvelope{Type: "repro", Repro: &rep})
	fmt.Println(string(line))
	return 0
}

// maxReproOutput is the output tail kept in memory and reported.
const maxReproOutput = 4 << 10

// reproduce fetches the head into dir and runs the command there under
// sh, its output (stdout and stderr) tail kept. Exit -1: it could not run
// or ran out of time.
func reproduce(e reproEnv, dir string) harness.Repro {
	w := workerEnv{repoToken: e.repoToken}
	fail := func(format string, args ...any) harness.Repro {
		return harness.Repro{Exit: -1, Output: fmt.Sprintf(format, args...)}
	}
	if _, err := w.git("", "init", "-q", dir); err != nil {
		return fail("init: %v", err)
	}
	if err := w.gitAuth(e.repoToken, dir, "fetch", "-q", "--no-tags", e.repoURL, e.ref); err != nil {
		return fail("fetch %s: %v", e.ref, err)
	}
	if _, err := w.git(dir, "checkout", "-q", "--detach", "FETCH_HEAD"); err != nil {
		return fail("checkout: %v", err)
	}
	if head, err := w.git(dir, "rev-parse", "HEAD"); err != nil || head != e.sha {
		return fail("%s is at %s, not the gated head %s: the pull request moved", e.ref, head, e.sha)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()
	out := &tailBuffer{max: maxReproOutput}
	cmd := exec.CommandContext(ctx, "sh", "-c", e.command) // #nosec G204 -- the task's reproduction command, run on purpose in the agent-less sandbox
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	// its own process group, killed whole at the timeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return harness.Repro{Exit: -1, Output: out.String() + fmt.Sprintf("\n(timed out after %s)", e.timeout)}
	case errors.As(err, &exit):
		return harness.Repro{Exit: exit.ExitCode(), Output: out.String()}
	case err != nil:
		return fail("run: %v", err)
	}
	return harness.Repro{Exit: 0, Output: out.String()}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
