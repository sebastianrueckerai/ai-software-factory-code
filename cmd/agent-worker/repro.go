//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// The repro pod is factoryd's GateReproduction: an init container in
// repro-fetch mode, the only holder of the read token on the task's
// repository, fetches ref (the pull request head) into the workspace and
// checks it is sha, then checks out at, when given (the task's base, in
// the head's history), instead; the worker container in repro mode, with
// no token of any kind, runs command there. No agent, no gateway token, no
// push token.
type fetchEnv struct {
	repoURL, repoToken, ref, sha, at string
}

type reproEnv struct {
	command string
	timeout time.Duration
}

func required(names ...string) (map[string]string, error) {
	vals := map[string]string{}
	for _, name := range names {
		if vals[name] = os.Getenv(name); vals[name] == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	return vals, nil
}

func loadFetchEnv() (fetchEnv, error) {
	v, err := required("REPO_URL", "REPO_TOKEN", "PR_REF", "REPO_SHA")
	if err != nil {
		return fetchEnv{}, err
	}
	return fetchEnv{repoURL: v["REPO_URL"], repoToken: v["REPO_TOKEN"], ref: v["PR_REF"], sha: v["REPO_SHA"], at: os.Getenv("CHECKOUT_SHA")}, nil
}

func loadReproEnv() (reproEnv, error) {
	v, err := required("REPRO_COMMAND")
	if err != nil {
		return reproEnv{}, err
	}
	secs, err := strconv.Atoi(os.Getenv("REPRO_TIMEOUT"))
	if err != nil || secs <= 0 {
		return reproEnv{}, fmt.Errorf("REPRO_TIMEOUT: want positive seconds")
	}
	return reproEnv{command: v["REPRO_COMMAND"], timeout: time.Duration(secs) * time.Second}, nil
}

// fetchFailed is where an init container's fetch (clone, repro-fetch)
// leaves its failure for the worker container to report: the init
// container always exits 0, so the report is one path.
func fetchFailed(ws string) string { return filepath.Join(ws, "fetch-failed") }

// recordFetch records the fetch's failure, if any, and is the init
// container's exit code.
func recordFetch(log func(string, ...any), ws string, err error) int {
	if err == nil {
		return 0
	}
	if werr := os.WriteFile(fetchFailed(ws), []byte(err.Error()), 0o600); werr != nil { // #nosec G703 -- the path is the workspace's own file; only the text written names BRANCHES
		log("record the fetch failure %q: %v", err, werr)
		return 1
	}
	return 0
}

var (
	// gitStatus is the HTTP status git reports a refused request with
	gitStatus = regexp.MustCompile(`(?:returned error:|HTTP) ([0-9]{3})`)
	// gitNetwork is git's report of a request that got no answer, or lost
	// it midway
	gitNetwork = regexp.MustCompile(`unable to access|RPC failed|remote end hung up|early EOF|unexpected disconnect`)
)

// transientFetch reports whether a fetch failed on the way to Forgejo, a
// 5xx or a network error, which a later start may get past, unlike a
// repository, ref or commit that is missing.
func transientFetch(err error) bool {
	if m := gitStatus.FindStringSubmatch(err.Error()); m != nil {
		return m[1][0] == '5'
	}
	return gitNetwork.MatchString(err.Error())
}

// fetched is the init container's fetch failure, nil when it fetched; one
// on the way to Forgejo is transient (transientFetch).
func fetched(ws string) error {
	failed, err := os.ReadFile(fetchFailed(ws))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = errors.New(string(failed)); transientFetch(err) {
		return transient{err}
	}
	return err
}

// runReproFetch is the init container's repro-fetch mode, into ws.
func runReproFetch(log func(string, ...any), ws string) int {
	e, err := loadFetchEnv()
	if err == nil {
		err = fetchHead(e, filepath.Join(ws, "repo"))
	}
	return recordFetch(log, ws, err)
}

// runRepro is the worker's repro mode: it reports one envelope on stdout,
// whatever happens, and exits 0 once it reported; initErr is becomeInit's,
// and probe builds the egress probe of the sandbox. A worker that is not
// its container's init runs nothing, so it kills nothing.
func runRepro(log func(string, ...any), initErr error, probe func(workerEnv) egressProbe) int {
	env := reproEnvelope(context.Background(), workspace, initErr, probe)
	if initErr == nil {
		if err := killOthers(); err != nil {
			log("stop the command's processes: %v", err)
		}
	}
	line, _ := json.Marshal(env)
	fmt.Println(string(line))
	return 0
}

// reproEnvelope is the repro mode's report on ws: the start failure of a
// worker that is not its container's init (initErr), of a sandbox other
// than its policy (the egress probe runs before the repository's code) or
// of a fetch that failed on the way to Forgejo, else the repro report.
func reproEnvelope(ctx context.Context, ws string, initErr error, probe func(workerEnv) egressProbe) harness.WorkerEnvelope {
	var err error
	if initErr != nil {
		err = startError{"init", initErr}
	} else {
		err = probeRepro(ctx, probe)
	}
	if f := fetched(ws); err == nil && errors.As(f, new(transient)) {
		err = startError{"fetch", f}
	}
	if err != nil {
		res := startFailed(err)
		return harness.WorkerEnvelope{Type: "result", Result: &res}
	}
	rep := repro(ws)
	return harness.WorkerEnvelope{Type: "repro", Repro: &rep}
}

// probeRepro runs the egress probe of the sandbox its env describes; an
// error is a startError.
func probeRepro(ctx context.Context, probe func(workerEnv) egressProbe) error {
	v, err := required("GATEWAY_URL", "EGRESS_CANARY", "EGRESS_DIRECT_IP")
	if err == nil {
		err = factoryd.CheckEgressTargets(v["EGRESS_CANARY"], v["EGRESS_DIRECT_IP"])
	}
	if err != nil {
		return startError{"environment", err}
	}
	e := workerEnv{gatewayURL: v["GATEWAY_URL"], egressCanary: v["EGRESS_CANARY"], egressDirectIP: v["EGRESS_DIRECT_IP"]}
	if err := probe(e).run(ctx); err != nil {
		return startError{factoryd.StepEgressProbe, err}
	}
	return nil
}

// fetchHead fetches e.ref into a scratch repository, checks its head is
// e.sha, and writes the tree of e.sha, or of e.at when given (a commit of
// the head's history: the task's base), into dir, then removes the
// scratch repository: the command sees the commit's files and no git
// metadata, so only a change in the files can make it fail at the base
// and pass at the head.
func fetchHead(e fetchEnv, dir string) error {
	w := workerEnv{repoToken: e.repoToken} // git's failures are scrubbed of it
	scratch := dir + ".fetch"
	defer os.RemoveAll(scratch) // #nosec G104 -- a leftover scratch repository holds no token and is outside dir
	if _, err := w.git("", "init", "-q", "--bare", scratch); err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if err := w.gitAuth(e.repoToken, scratch, "fetch", "-q", "--no-tags", e.repoURL, e.ref); err != nil {
		return fmt.Errorf("fetch %s: %w", e.ref, err)
	}
	if head, err := w.git(scratch, "rev-parse", "FETCH_HEAD"); err != nil || head != e.sha {
		return fmt.Errorf("%s is at %s, not the gated head %s: the pull request moved", e.ref, head, e.sha)
	}
	commit := e.sha
	if e.at != "" {
		if _, err := w.git(scratch, "cat-file", "-e", e.at+"^{commit}"); err != nil {
			return fmt.Errorf("%s is not in the history of %s: %w", e.at, e.sha, err)
		}
		commit = e.at
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if _, err := w.git(scratch, "--work-tree="+dir, "checkout", "-q", "-f", commit, "--", "."); err != nil {
		return fmt.Errorf("check out %s: %w", commit, err)
	}
	return os.RemoveAll(scratch)
}

// maxReproOutput is the output tail kept in memory and reported. The
// criteria gate's marker lines ride this tail: one line per criterion, a
// plan of eight criteria stays whole under it.
const maxReproOutput = 8 << 10

// repro is the repro mode's report on ws: the fetch's failure, a bad
// environment (exit -1), or the command's run at the fetched head.
func repro(ws string) harness.Repro {
	if b, err := os.ReadFile(fetchFailed(ws)); err == nil {
		return harness.Repro{Exit: -1, Output: "repro mode: " + string(b)}
	}
	e, err := loadReproEnv()
	if err != nil {
		return harness.Repro{Exit: -1, Output: "repro mode: " + err.Error()}
	}
	return reproduce(e, filepath.Join(ws, "repo"))
}

// reproduce runs the command under the image's /bin/sh in dir, its output
// (stdout and stderr) tail kept. Exit -1: it could not run or ran out of
// time.
func reproduce(e reproEnv, dir string) harness.Repro {
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()
	out := &tailBuffer{max: maxReproOutput}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", e.command) // #nosec G204 -- the task's reproduction command, run on purpose in the agent-less sandbox
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
		return harness.Repro{Exit: -1, Output: fmt.Sprintf("run: %v", err)}
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
