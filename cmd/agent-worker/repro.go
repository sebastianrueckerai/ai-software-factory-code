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
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

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
	// overlay is a package directory whose test files of sha are written
	// over the tree of at
	overlay string
}

type reproEnv struct {
	command string
	argv    []string
	timeout time.Duration
}

// proofsEnv is the proofs mode's: the list factoryd posts (awaitProofs),
// each entry's timeout, and the budget all the entries share.
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
	return fetchEnv{repoURL: v["REPO_URL"], repoToken: v["REPO_TOKEN"], ref: v["PR_REF"], sha: v["REPO_SHA"], at: os.Getenv("CHECKOUT_SHA"), overlay: os.Getenv("OVERLAY_PKG")}, nil
}

func reproTimeout() (time.Duration, error) {
	secs, err := strconv.Atoi(os.Getenv("REPRO_TIMEOUT"))
	if err != nil || secs <= 0 {
		return 0, fmt.Errorf("REPRO_TIMEOUT: want positive seconds")
	}
	return time.Duration(secs) * time.Second, nil
}

// loadReproEnv reads the command a shell runs, or REPRO_ARGV, a JSON list
// executed directly with no shell.
func loadReproEnv() (reproEnv, error) {
	timeout, err := reproTimeout()
	if err != nil {
		return reproEnv{}, err
	}
	if list := os.Getenv("REPRO_ARGV"); list != "" {
		var argv []string
		if err := json.Unmarshal([]byte(list), &argv); err != nil || len(argv) == 0 {
			return reproEnv{}, fmt.Errorf("REPRO_ARGV: want a non-empty JSON list of strings")
		}
		return reproEnv{argv: argv, timeout: timeout}, nil
	}
	v, err := required("REPRO_COMMAND")
	if err != nil {
		return reproEnv{}, err
	}
	return reproEnv{command: v["REPRO_COMMAND"], timeout: timeout}, nil
}

// loadProofsEnv reads the timeout and the budget; the list is not in the
// environment.
func loadProofsEnv() (proofsEnv, error) {
	var e proofsEnv
	var err error
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
// entry as it ends before the closing envelope, its list taken over the
// message port. Each line goes out once the processes its check left are
// dead, so none reaches the next entry.
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
	emit(proofsReport(context.Background(), workspace, initErr, probe, func() ([]harness.Proof, error) { return awaitProofs(log) }, emit))
	return 0
}

// proofsListWait bounds the proofs mode's wait for its list: past the 10
// minutes factoryd retries its post once the pod runs. The wait counts
// against the run's budget. A var, so a test can shorten it.
var proofsListWait = 12 * time.Minute

// awaitProofs takes the list factoryd posts to POST /proofs on the message
// port, under the bearer whose hash is MESSAGE_TOKEN_SHA256, within
// proofsListWait. The list stays in memory, never on disk or in the
// environment, and the port is closed before any check runs.
func awaitProofs(log func(string, ...any)) ([]harness.Proof, error) {
	hash, err := parseHash(os.Getenv("MESSAGE_TOKEN_SHA256"))
	if err != nil {
		return nil, err
	}
	// every check inherits this process's environment
	os.Unsetenv("MESSAGE_TOKEN_SHA256")
	lists := make(chan []harness.Proof, 1)
	stop, err := servePort(proofsHandler(hash, lists), log)
	if err != nil {
		return nil, fmt.Errorf("message endpoint: %w", err)
	}
	select {
	case list := <-lists:
		stop()
		return list, nil
	case <-time.After(proofsListWait):
		stop()
		// a list taken while the port closed was answered 204
		select {
		case list := <-lists:
			return list, nil
		default:
			return nil, fmt.Errorf("no proofs list from factoryd within %s", proofsListWait)
		}
	}
}

// proofsHandler takes factoryd's POST /proofs, the list as JSON, under the
// bearer whose hash is tokenSHA256, into lists: the first list stands, so
// a retry changes nothing.
func proofsHandler(tokenSHA256 [32]byte, lists chan<- []harness.Proof) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /proofs", func(w http.ResponseWriter, r *http.Request) {
		if !bearer(tokenSHA256, w, r) {
			return
		}
		var list []harness.Proof
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, harness.MaxProofsList)).Decode(&list); err != nil {
			http.Error(w, fmt.Sprintf("want the proofs list as JSON, at most %d MiB: %v", harness.MaxProofsList>>20, err), http.StatusBadRequest)
			return
		}
		select {
		case lists <- list:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
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

// proofsReport is the proofs mode's report on ws: list's list first, so
// factoryd's post lands before any report, and the run's budget runs from
// there; then sandboxStarted's failure, else the closing envelope of
// proofs, which emits each entry's run.
func proofsReport(ctx context.Context, ws string, initErr error, probe func(workerEnv) egressProbe, list func() ([]harness.Proof, error), emit func(harness.WorkerEnvelope)) harness.WorkerEnvelope {
	began := time.Now()
	entries, err := list()
	if err != nil {
		return proofsFailed(err.Error())
	}
	if err := sandboxStarted(ctx, ws, initErr, probe); err != nil {
		res := startFailed(err)
		return harness.WorkerEnvelope{Type: "result", Result: &res}
	}
	return proofs(ws, entries, began, emit)
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
	if e.overlay != "" {
		if err := overlayTests(w, scratch, e.sha, e.overlay, dir); err != nil {
			return err
		}
	}
	return os.RemoveAll(scratch)
}

// overlayTests makes pkg's *_test.go files and its testdata directory in
// dir the head's (sha's): the base's are removed, then the head's files of
// mode 100644 or 100755, none else (no symlink, no submodule), are written
// from scratch. A package directory the head lacks is an error: the claim
// names nothing there. The removal goes through a root on dir, so a
// symlink in the base's tree cannot lead it out.
func overlayTests(w workerEnv, scratch, sha, pkg, dir string) error {
	pkg = path.Clean(filepath.ToSlash(pkg))
	if !filepath.IsLocal(pkg) {
		return fmt.Errorf("overlay package %q is not inside the repository", pkg)
	}
	if pkg != "." {
		if kind, err := w.git(scratch, "cat-file", "-t", sha+":"+pkg); err != nil || kind != "tree" {
			return fmt.Errorf("the head has no package directory %s", pkg)
		}
	}
	prefix := ""
	if pkg != "." {
		prefix = pkg + "/"
	}
	var paths []string
	for _, testdata := range []bool{false, true} {
		args, where := []string{"ls-tree", "-z", sha}, prefix
		if testdata {
			args, where = []string{"ls-tree", "-r", "-z", sha}, prefix+"testdata/"
		}
		if where != "" {
			args = append(args, "--", where)
		}
		listing, err := w.git(scratch, args...)
		if err != nil {
			return fmt.Errorf("list the head's %s: %w", where, err)
		}
		for _, rec := range strings.Split(listing, "\x00") {
			meta, name, ok := strings.Cut(rec, "\t")
			f := strings.Fields(meta)
			if ok && len(f) == 3 && f[1] == "blob" && (f[0] == "100644" || f[0] == "100755") && (testdata || strings.HasSuffix(name, "_test.go")) {
				paths = append(paths, name)
			}
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), pkg)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.go") {
			if err := root.Remove(path.Join(pkg, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := root.RemoveAll(path.Join(pkg, "testdata")); err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	if _, err := w.git(scratch, append([]string{"--literal-pathspecs", "--work-tree=" + dir, "checkout", "-q", "-f", sha, "--"}, paths...)...); err != nil {
		return fmt.Errorf("overlay the head's tests of %s: %w", pkg, err)
	}
	return nil
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

// reproduce runs the command under the image's /bin/sh in dir, or the argv
// directly.
func reproduce(e reproEnv, dir string) harness.Repro {
	if e.argv != nil {
		return runBounded(dir, nil, e.timeout, fmt.Sprintf("timed out after %s", e.timeout), e.argv[0], e.argv[1:]...)
	}
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

// proofsFailed is the closing envelope of a run that ran no entry, and why.
func proofsFailed(why string) harness.WorkerEnvelope {
	return harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{Error: "proofs mode: " + why}}
}

// proofs is the proofs mode's run of list on ws, its budget from began:
// the fetch's failure or a bad environment ends it before any entry.
func proofs(ws string, list []harness.Proof, began time.Time, emit func(harness.WorkerEnvelope)) harness.WorkerEnvelope {
	if b, err := os.ReadFile(fetchFailed(ws)); err == nil {
		return proofsFailed(string(b))
	}
	e, err := loadProofsEnv()
	if err != nil {
		return proofsFailed(err.Error())
	}
	e.proofs = list
	runProofList(e, ws, began, emit)
	return harness.WorkerEnvelope{Type: "proofs", Proofs: &harness.Proofs{}}
}

// runProofList runs e's entries in ws/repo in order, within the budget
// from began, emitting one "proof" envelope per entry as it ends. An
// entry the budget cut short at the deadline had no try: it is not
// started, as the entries after it.
func runProofList(e proofsEnv, ws string, began time.Time, emit func(harness.WorkerEnvelope)) {
	deadline := began.Add(e.budget)
	for i, p := range e.proofs {
		run := harness.ProofRun{ID: p.ID, Exit: -1, Output: fmt.Sprintf("proofs mode: the run's %s ran out before this entry", e.budget)}
		if left := time.Until(deadline); left > 0 {
			timeout, late := e.timeout, fmt.Sprintf("timed out after %s", e.timeout)
			shortened := left < timeout
			if shortened {
				timeout, late = left, fmt.Sprintf("the run's %s ran out", e.budget)
			}
			run = runProof(p, filepath.Join(ws, "repo"), filepath.Join(ws, "proofs", strconv.Itoa(i)), timeout, late)
			if shortened && run.Exit == -1 && !time.Now().Before(deadline) {
				run.Started = false
			}
		}
		emit(harness.WorkerEnvelope{Type: "proof", Proof: &run})
	}
}

// runProof runs one entry in repo: its file first (placeFile), then the
// tree's bash check test, which runBounded ends at timeout with late,
// then the tree as it was; own is the entry's own directory, outside the
// tree, for the report and the artifacts.
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
	undo := func() error { return nil }
	if f := p.File; f != nil {
		if undo, err = placeFile(repo, f.Path, f.Content); err != nil {
			return failed(fmt.Errorf("write %s: %w", f.Path, err))
		}
	}
	args := []string{"./check", "test", p.Suite}
	if p.Name != "" {
		args = append(args, p.Name)
	}
	env := append(os.Environ(), "CHECK_REPORT="+filepath.Join(own, "report.xml"), "CHECK_ARTIFACTS="+filepath.Join(own, "artifacts"))
	r := runBounded(repo, env, timeout, late, "/bin/bash", args...)
	run := harness.ProofRun{ID: p.ID, Started: true, Exit: r.Exit, Output: r.Output}
	if err := undo(); err != nil {
		run.Output += fmt.Sprintf("\nproofs mode: put back %s: %v", p.File.Path, err)
	}
	collect(&run, own)
	return run
}

// collect reads into run the report and the artifacts the check left in
// own, regular files only, within maxProofLine: the report first, then
// the artifacts that fit, each cut at maxArtifact on a character
// boundary. The output names a report left out, Dropped the artifacts.
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
	dropped := 0
	for _, d := range entries {
		if !d.Type().IsRegular() {
			continue
		}
		b, err := readRegular(root, filepath.Join("artifacts", d.Name()), maxArtifact+1)
		if err != nil {
			continue
		}
		b = cutArtifact(b)
		// the name escaped, at most six bytes a byte, the content in
		// base64, two pairs of quotes, a colon and a comma
		size := 6*len(d.Name()) + base64.StdEncoding.EncodedLen(len(b)) + 6
		if size > room {
			// the names stay within the line's slack past maxProofLine
			if dropped += len(d.Name()); dropped <= 4<<10 {
				run.Dropped = append(run.Dropped, d.Name())
			}
			continue
		}
		room -= size
		if run.Artifacts == nil {
			run.Artifacts = map[string][]byte{}
		}
		run.Artifacts[d.Name()] = b
	}
}

// cutArtifact cuts b at maxArtifact without the first bytes of a
// character the cut runs through.
func cutArtifact(b []byte) []byte {
	if len(b) <= maxArtifact {
		return b
	}
	b = b[:maxArtifact]
	for i := len(b) - 1; i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
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

// placeFile writes content to name in repo's tree for one entry's run, and
// returns its undo: the tree's own file there back, or the file gone, so
// a later entry, a suite above all, runs on the tree as pushed.
func placeFile(repo, name, content string) (undo func() error, err error) {
	tree, err := os.OpenRoot(repo)
	if err != nil {
		return nil, err
	}
	own, err := readRegular(tree, name, maxReport+1)
	_ = tree.Close() // read only
	switch {
	case errors.Is(err, fs.ErrNotExist):
		undo = func() error {
			tree, err := os.OpenRoot(repo)
			if err != nil {
				return err
			}
			defer tree.Close()
			return tree.Remove(name)
		}
	case err != nil:
		return nil, err
	case len(own) > maxReport:
		return nil, fmt.Errorf("the tree's own file passes %d MiB", maxReport>>20)
	default:
		undo = func() error { return writeRegular(repo, name, string(own)) }
	}
	if err := writeRegular(repo, name, content); err != nil {
		return nil, err
	}
	return undo, nil
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
