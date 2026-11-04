//go:build unix

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
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

// proofsEnv is the proofs mode's: the list, each entry's timeout, and the
// budget all the entries share.
type proofsEnv struct {
	proofs          []harness.Proof
	timeout, budget time.Duration
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

func reproTimeout() (time.Duration, error) {
	secs, err := strconv.Atoi(os.Getenv("REPRO_TIMEOUT"))
	if err != nil || secs <= 0 {
		return 0, fmt.Errorf("REPRO_TIMEOUT: want positive seconds")
	}
	return time.Duration(secs) * time.Second, nil
}

func loadReproEnv() (reproEnv, error) {
	v, err := required("REPRO_COMMAND")
	if err != nil {
		return reproEnv{}, err
	}
	timeout, err := reproTimeout()
	if err != nil {
		return reproEnv{}, err
	}
	return reproEnv{command: v["REPRO_COMMAND"], timeout: timeout}, nil
}

func loadProofsEnv() (proofsEnv, error) {
	v, err := required("PROOFS")
	if err != nil {
		return proofsEnv{}, err
	}
	var e proofsEnv
	if err := json.Unmarshal([]byte(v["PROOFS"]), &e.proofs); err != nil {
		return proofsEnv{}, fmt.Errorf("PROOFS: %w", err)
	}
	if e.timeout, err = reproTimeout(); err != nil {
		return proofsEnv{}, err
	}
	secs, err := strconv.Atoi(os.Getenv("PROOFS_BUDGET"))
	if err != nil || secs <= 0 {
		return proofsEnv{}, fmt.Errorf("PROOFS_BUDGET: want positive seconds")
	}
	e.budget = time.Duration(secs) * time.Second
	return e, nil
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

// runProofs is the worker's proofs mode: runRepro's, with a line for each
// entry as it ends before the closing envelope. Each line goes out once
// the processes its check left are dead, so none reaches the next entry.
func runProofs(log func(string, ...any), initErr error, probe func(workerEnv) egressProbe) int {
	emit := func(env harness.WorkerEnvelope) {
		if initErr == nil {
			if err := killOthers(); err != nil {
				log("stop the checks' processes: %v", err)
			}
		}
		line, _ := json.Marshal(env)
		fmt.Println(string(line))
	}
	emit(proofsReport(context.Background(), workspace, initErr, probe, emit))
	return 0
}

// reproEnvelope is the repro mode's report on ws: sandboxStarted's
// failure, else the repro report.
func reproEnvelope(ctx context.Context, ws string, initErr error, probe func(workerEnv) egressProbe) harness.WorkerEnvelope {
	if err := sandboxStarted(ctx, ws, initErr, probe); err != nil {
		res := startFailed(err)
		return harness.WorkerEnvelope{Type: "result", Result: &res}
	}
	rep := repro(ws)
	return harness.WorkerEnvelope{Type: "repro", Repro: &rep}
}

// proofsReport is the proofs mode's report on ws: sandboxStarted's
// failure, else the closing envelope of proofs, which emits each entry's
// run.
func proofsReport(ctx context.Context, ws string, initErr error, probe func(workerEnv) egressProbe, emit func(harness.WorkerEnvelope)) harness.WorkerEnvelope {
	if err := sandboxStarted(ctx, ws, initErr, probe); err != nil {
		res := startFailed(err)
		return harness.WorkerEnvelope{Type: "result", Result: &res}
	}
	return proofs(ws, emit)
}

// sandboxStarted is the start failure, a startError, of a worker that is
// not its container's init (initErr), of a sandbox other than its policy
// (the egress probe runs before the repository's code) or of a fetch that
// failed on the way to Forgejo; nil when none failed.
func sandboxStarted(ctx context.Context, ws string, initErr error, probe func(workerEnv) egressProbe) error {
	if initErr != nil {
		return startError{"init", initErr}
	}
	if err := probeRepro(ctx, probe); err != nil {
		return err
	}
	if f := fetched(ws); errors.As(f, new(transient)) {
		return startError{"fetch", f}
	}
	return nil
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

// maxReproOutput is the output tail kept in memory and reported, of a
// reproduction command or of one proof's check test.
const maxReproOutput = 8 << 10

const (
	maxArtifact = 256 << 10
	// maxReport bounds the report read into memory
	maxReport = 64 << 20
	// maxProofLine bounds one proof's line, the report first and then the
	// artifacts that fit, under the 1 MiB that factoryd reads of one: the
	// rest holds the note on what was left out
	maxProofLine = 960 << 10
)

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

// reproduce runs the command under the image's /bin/sh in dir.
func reproduce(e reproEnv, dir string) harness.Repro {
	return runBounded(dir, nil, e.timeout, fmt.Sprintf("timed out after %s", e.timeout), "/bin/sh", "-c", e.command)
}

// runBounded runs name with args in dir, under env (nil: the worker's
// own), its output (stdout and stderr) tail kept, killed with its process
// group at timeout, when late ends the output. Exit -1: it could not run
// or ran out of time.
func runBounded(dir string, env []string, timeout time.Duration, late, name string, args ...string) harness.Repro {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out := &tailBuffer{max: maxReproOutput}
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- the task's reproduction command or a proof's check test, run on purpose in the agent-less sandbox
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, out
	// its own process group, killed whole at the timeout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return harness.Repro{Exit: -1, Output: out.String() + "\n(" + late + ")"}
	case errors.As(err, &exit):
		return harness.Repro{Exit: exit.ExitCode(), Output: out.String()}
	case err != nil:
		return harness.Repro{Exit: -1, Output: fmt.Sprintf("run: %v", err)}
	}
	return harness.Repro{Exit: 0, Output: out.String()}
}

// proofs is the proofs mode's run on ws: the fetch's failure or a bad
// environment ends it before any entry.
func proofs(ws string, emit func(harness.WorkerEnvelope)) harness.WorkerEnvelope {
	failed := func(why string) harness.WorkerEnvelope {
		return harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{Error: "proofs mode: " + why}}
	}
	if b, err := os.ReadFile(fetchFailed(ws)); err == nil {
		return failed(string(b))
	}
	e, err := loadProofsEnv()
	if err != nil {
		return failed(err.Error())
	}
	runProofList(e, ws, emit)
	return harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{}}
}

// runProofList runs e's entries in ws/repo in order, emitting one "proof"
// envelope per entry as it ends.
func runProofList(e proofsEnv, ws string, emit func(harness.WorkerEnvelope)) {
	deadline := time.Now().Add(e.budget)
	for i, p := range e.proofs {
		run := harness.ProofRun{ID: p.ID, Exit: -1, Output: fmt.Sprintf("proofs mode: the run's %s ran out before this entry", e.budget)}
		if left := time.Until(deadline); left > 0 {
			timeout, late := e.timeout, fmt.Sprintf("timed out after %s", e.timeout)
			if left < timeout {
				timeout, late = left, fmt.Sprintf("the run's %s ran out", e.budget)
			}
			run = runProof(p, filepath.Join(ws, "repo"), filepath.Join(ws, "proofs", strconv.Itoa(i)), timeout, late)
		}
		emit(harness.WorkerEnvelope{Type: "proof", Proof: &run})
	}
}

// runProof runs one entry in repo: its file first, then the tree's bash
// check test, which runBounded ends at timeout with late; own is the
// entry's own directory, outside the tree, for the report and the
// artifacts.
func runProof(p harness.Proof, repo, own string, timeout time.Duration, late string) harness.ProofRun {
	failed := func(err error) harness.ProofRun {
		return harness.ProofRun{ID: p.ID, Exit: -1, Output: "proofs mode: " + err.Error()}
	}
	err := os.MkdirAll(filepath.Dir(own), 0o750)
	if err == nil {
		// new, so nothing a process left there reads as this entry's
		err = os.Mkdir(own, 0o750)
	}
	if err == nil {
		err = os.Mkdir(filepath.Join(own, "artifacts"), 0o750)
	}
	if err != nil {
		return failed(err)
	}
	if f := p.File; f != nil {
		if err := writeRegular(repo, f.Path, f.Content); err != nil {
			return failed(fmt.Errorf("write %s: %w", f.Path, err))
		}
	}
	args := []string{"./check", "test", p.Suite}
	if p.Name != "" {
		args = append(args, p.Name)
	}
	// PROOFS holds every entry's file
	env := append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "PROOFS=") }),
		"CHECK_REPORT="+filepath.Join(own, "report.xml"), "CHECK_ARTIFACTS="+filepath.Join(own, "artifacts"))
	r := runBounded(repo, env, timeout, late, "/bin/bash", args...)
	run := harness.ProofRun{ID: p.ID, Exit: r.Exit, Output: r.Output}
	collect(&run, own)
	return run
}

// collect reads into run the report and the artifacts the check left in
// own, regular files only, within maxProofLine: the report first, then
// the artifacts that fit. The output names what was left out.
func collect(run *harness.ProofRun, own string) {
	root, err := os.OpenRoot(own)
	if err != nil {
		run.Output += "\nproofs mode: " + err.Error()
		return
	}
	defer root.Close()
	switch report, err := readRegular(root, "report.xml", maxReport+1); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		run.Output += "\nproofs mode: " + err.Error()
	case len(report) > maxReport:
		run.Output += fmt.Sprintf("\nproofs mode: the report passed %d MiB", maxReport>>20)
	default:
		run.Report = gzipped(report)
	}
	line, _ := json.Marshal(harness.WorkerEnvelope{Type: "proof", Proof: run})
	if len(line) > maxProofLine {
		run.Output += fmt.Sprintf("\nproofs mode: the report, %d bytes gzipped, does not fit one log line", len(run.Report))
		run.Report = nil
		line, _ = json.Marshal(harness.WorkerEnvelope{Type: "proof", Proof: run})
	}
	room := maxProofLine - len(line)
	entries, _ := fs.ReadDir(root.FS(), "artifacts")
	var left []string
	for _, d := range entries {
		if !d.Type().IsRegular() {
			continue
		}
		b, err := readRegular(root, filepath.Join("artifacts", d.Name()), maxArtifact)
		if err != nil {
			continue
		}
		// the name escaped, at most six bytes a byte, the content in
		// base64, two pairs of quotes, a colon and a comma
		size := 6*len(d.Name()) + base64.StdEncoding.EncodedLen(len(b)) + 6
		if size > room {
			left = append(left, d.Name())
			continue
		}
		room -= size
		if run.Artifacts == nil {
			run.Artifacts = map[string][]byte{}
		}
		run.Artifacts[d.Name()] = b
	}
	if len(left) > 0 {
		run.Output += textutil.Truncate("\nproofs mode: one log line has no room for these artifacts: "+strings.Join(left, ", "), 4<<10)
	}
}

// openRegular opens name in root with flag, a regular file: a FIFO there
// blocks neither a read nor a write.
func openRegular(root *os.Root, name string, flag int) (*os.File, error) {
	f, err := root.OpenFile(name, flag|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// readRegular reads at most max bytes of name in root, a regular file.
func readRegular(root *os.Root, name string, max int64) ([]byte, error) {
	f, err := openRegular(root, name, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// writeRegular writes content to name in dir's tree, a regular file, and
// makes its directory; a path out of the tree is refused.
func writeRegular(dir, name, content string) error {
	tree, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer tree.Close()
	if err := tree.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return err
	}
	f, err := openRegular(tree, name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	return errors.Join(err, f.Close())
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	// a bytes.Buffer takes every write
	_, _ = z.Write(b)
	_ = z.Close()
	return buf.Bytes()
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
