//go:build unix

package main

// The check-in's endpoints on the worker's message server (the escalation
// ladder's plan D): factoryd runs the check-in conversation against this
// workspace before the coding conversation starts. Its tools read the
// workspace, the handover it lands becomes the brief's first section, and
// the parent session's record is delivered here for the coding agent to
// grep.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// checkinWait is how long the session's start holds for the check-in's
// handover before the conversation begins without one. A var, like
// pushRetryEvery, so a test can shorten it.
var checkinWait = 10 * time.Minute

const (
	// checkinOutputCap cuts every tool's output.
	checkinOutputCap = 32 << 10
	// checkinRecordCap bounds the parent record the endpoint accepts.
	checkinRecordCap = 64 << 20
)

// checkinServe carries the check-in's endpoints: the tool executions run
// in the session's workspace, the handover and the parent record arrive
// before the coding conversation starts.
type checkinServe struct {
	ws       string                 // the workspace root
	wait     time.Duration          // how long oc.Run's start holds for /brief
	timeout  time.Duration          // one tool's cap
	handover chan harness.BriefPush // exactly one send: the pushed brief, or the zero push at the deadline
	log      func(string, ...any)
	// closed: the wait ended, with a brief or without one, so a later push
	// lands nothing in a clone whose conversation runs
	mu     sync.Mutex
	closed bool
}

func newCheckinServe(ws string, log func(string, ...any)) *checkinServe {
	return &checkinServe{ws: ws, wait: checkinWait, timeout: harness.CheckinToolTimeout, handover: make(chan harness.BriefPush, 1), log: log}
}

// waitBrief holds the conversation's start for the check-in's handover:
// the push when it arrived within the wait, the zero push when none did.
// Either way the wait is closed after it.
func (c *checkinServe) waitBrief(log func(string, ...any)) harness.BriefPush {
	select {
	case push := <-c.handover:
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		return push
	case <-time.After(c.wait):
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	select {
	case push := <-c.handover: // it landed as the wait ended
		return push
	default:
	}
	log("no check-in handover within %s; the conversation starts without one", c.wait)
	return harness.BriefPush{}
}

// serve mounts the check-in's three endpoints under the /message bearer:
// the tools the check-in's model calls, the handover that starts the
// conversation, and the parent session's record; and POST /image, a
// workspace image's bytes for a look, and POST /file, a workspace text
// file's bytes for a digest.
func (c *checkinServe) serve(mux *http.ServeMux, authorized func(http.ResponseWriter, *http.Request) bool) {
	mux.HandleFunc("POST /tool", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.Name == "" {
			http.Error(w, `want {"name": "...", "args": {...}}`, http.StatusBadRequest)
			return
		}
		arg := func(name string) string { s, _ := in.Args[name].(string); return s }
		out, cut, err := c.tool(r.Context(), in.Name, arg)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errOutside) {
				status = http.StatusForbidden
			}
			http.Error(w, err.Error(), status)
			return
		}
		json.NewEncoder(w).Encode(struct { // #nosec G104 -- best-effort response write
			Output string `json:"output"`
			Cut    bool   `json:"cut"`
		}{out, cut})
	})
	mux.HandleFunc("POST /image", serveRead(authorized, c.image))
	mux.HandleFunc("POST /file", serveRead(authorized, c.text))
	mux.HandleFunc("POST /brief", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in harness.BriefPush
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, harness.MaxBriefPush)).Decode(&in); err != nil {
			http.Error(w, `want {"handover": "...", "prior": {...}, "files": [...], "proofs": [...]}`, http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed {
			http.Error(w, "the conversation started: its wait for a brief ended, and a later push lands nothing", http.StatusConflict)
			return
		}
		due, held, err := c.sortProofs(in.Proofs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := c.landPrior(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := landProofs(c.repo(), due); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		in.InClone = held
		select {
		case c.handover <- in:
		default: // a retry before the wait took the first: the first brief stands
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /records", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in struct {
			Session string `json:"session"`
			Record  string `json:"record"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, checkinRecordCap)).Decode(&in); err != nil {
			http.Error(w, `want {"session": "...", "record": "..."}`, http.StatusBadRequest)
			return
		}
		if badSession(in.Session) {
			http.Error(w, fmt.Sprintf("session %q %s", in.Session, unfitName), http.StatusBadRequest)
			return
		}
		dir := filepath.Join(c.ws, ".factory", "sessions")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, in.Session+".jsonl"), []byte(in.Record), 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// landPrior writes the push's report files under .factory/prior/<session>,
// outside the repository, and points the prior state at them. It writes
// nothing when any name is unfit.
func (c *checkinServe) landPrior(in *harness.BriefPush) error {
	if len(in.Files) == 0 {
		return nil
	}
	if in.Prior == nil {
		return errors.New("files need a prior state; send \"prior\" with them")
	}
	if badSession(in.Prior.Session) {
		return fmt.Errorf("prior session %q %s", in.Prior.Session, unfitName)
	}
	names := make([]string, len(in.Files))
	for i, f := range in.Files {
		if badSession(f.Name) {
			return fmt.Errorf("file name %q %s", f.Name, unfitName)
		}
		names[i] = f.Name
	}
	dir := filepath.Join(c.ws, ".factory", "prior", in.Prior.Session)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, f := range in.Files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Text), 0o600); err != nil {
			return err
		}
	}
	in.Prior.Dir, in.Prior.Reports = dir, names
	return nil
}

// sortProofs takes the push's proofs in order: due are the ones to write,
// whose path holds no file, and held the paths of every proof the clone
// will hold, the due ones and those whose path holds a file already. A
// path that leaves the tree refuses the push whole. A path whose Lstat
// fails otherwise, such as under a file, is left out with a log line.
func (c *checkinServe) sortProofs(proofs []harness.ProofFile) (due []harness.ProofFile, held []string, err error) {
	if len(proofs) == 0 {
		return nil, nil, nil
	}
	tree, err := os.OpenRoot(c.repo())
	if err != nil {
		return nil, nil, err
	}
	defer tree.Close() // read only
	for _, p := range proofs {
		if !filepath.IsLocal(p.Path) {
			return nil, nil, fmt.Errorf("proof %q: %w", p.Path, errOutside)
		}
		switch _, err := tree.Lstat(p.Path); {
		case errors.Is(err, fs.ErrNotExist):
			due = append(due, p)
		case err != nil:
			c.log("opened proof %s is not in the clone: %v", p.Path, err)
			continue
		}
		held = append(held, p.Path)
	}
	return due, held, nil
}

// landProofs writes each proof into repo at its path.
func landProofs(repo string, proofs []harness.ProofFile) error {
	for _, p := range proofs {
		if err := writeRegular(repo, p.Path, p.Content); err != nil {
			return fmt.Errorf("write proof %q: %w", p.Path, err)
		}
	}
	return nil
}

// badSession refuses a name that could leave its directory: a session id
// or a report file's name. unfitName is the rule its refusals state.
func badSession(session string) bool {
	return session == "" || session == "." || strings.Contains(session, "/") || strings.Contains(session, "..")
}

const unfitName = `must be a bare name: not empty, not "." and without "/" or ".."`

var errOutside = errors.New("outside the workspace")

// tool runs one of the check-in's three workspace tools; anything else is
// refused. Every output is cut at checkinOutputCap.
func (c *checkinServe) tool(ctx context.Context, name string, arg func(string) string) (string, bool, error) {
	switch name {
	case "read_file":
		return c.readFile(arg("path"))
	case "grep":
		return c.grep(arg("pattern"), arg("ref"), arg("path"))
	case "git_log":
		return c.gitLog(arg("args"))
	}
	return "", false, fmt.Errorf("unknown tool %q", name)
}

// repo is the workspace's clone, where the git tools work.
func (c *checkinServe) repo() string { return filepath.Join(c.ws, "repo") }

// serveRead answers {"path"} with that workspace file's bytes as read
// returns them, typed as they sniff; a refusal is read's sentence and
// status. The body holds a path the gateway admits, 4096 bytes, even with
// every byte JSON-escaped to six.
func serveRead(authorized func(http.ResponseWriter, *http.Request) bool, read func(string) ([]byte, int, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&in); err != nil || in.Path == "" {
			http.Error(w, `want {"path": "..."}`, http.StatusBadRequest)
			return
		}
		b, status, err := read(in.Path)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(b))
		w.Write(b) // #nosec G104 -- best-effort response write
	}
}

// image reads the image file at path for a look: PNG, JPEG, WebP or GIF
// of at most policy.MaxLookBytes.
func (c *checkinServe) image(path string) ([]byte, int, error) {
	b, status, err := c.read(path, policy.MaxLookBytes, path+" is a directory, not an image file")
	switch {
	case err != nil:
		return nil, status, err
	case len(b) > policy.MaxLookBytes:
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("%s is larger than %d MiB", path, policy.MaxLookBytes>>20)
	}
	if ct := http.DetectContentType(b); !slices.Contains(policy.LookTypes, ct) {
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("%s is %s, not a PNG, JPEG, WebP or GIF image", path, ct)
	}
	return b, http.StatusOK, nil
}

// text reads the text file at path for a digest: UTF-8 without a NUL
// byte, not empty, of at most policy.MaxDigestBytes.
func (c *checkinServe) text(path string) ([]byte, int, error) {
	b, status, err := c.read(path, policy.MaxDigestBytes, path+" is a directory: digest one file in it")
	switch {
	case err != nil:
		return nil, status, err
	case len(b) > policy.MaxDigestBytes:
		return nil, http.StatusRequestEntityTooLarge, fmt.Errorf("%s is larger than %d KiB, more than one digest reads: read the part you need with the read tool's offset and limit",
			path, policy.MaxDigestBytes>>10)
	case len(b) == 0:
		return nil, http.StatusBadRequest, fmt.Errorf("%s is empty: there is nothing to digest", path)
	case !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0:
		return nil, http.StatusUnsupportedMediaType, fmt.Errorf("%s is not UTF-8 text, which a digest reads", path)
	}
	return b, http.StatusOK, nil
}

// read reads at most max+1 bytes of the file at path, absolute inside the
// workspace or relative to the repo, through the workspace's os.Root: a
// symlink out of it refuses at the open itself, so the agent cannot swap
// one in after a check. A FIFO or another non-regular file refuses
// without a read, a directory with dir. A refusal is one sentence for the
// agent, with the status to answer it by.
func (c *checkinServe) read(path string, max int, dir string) ([]byte, int, error) {
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.repo(), p)
	}
	outside := fmt.Errorf("%s is %w", path, errOutside)
	rel, err := filepath.Rel(c.ws, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, http.StatusForbidden, outside
	}
	root, err := os.OpenRoot(c.ws)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	defer root.Close() // #nosec G307 -- a read-only root's close failure is not worth an answer
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return nil, http.StatusNotFound, fmt.Errorf("no file %s in the workspace", path)
	case errors.Is(err, syscall.ENAMETOOLONG), errors.Is(err, syscall.EINVAL):
		return nil, http.StatusBadRequest, fmt.Errorf("%s is not a path a file can have: name a file of the workspace", path)
	case errors.Is(err, os.ErrPermission):
		return nil, http.StatusForbidden, fmt.Errorf("%s cannot be read", path)
	case err != nil:
		return nil, http.StatusForbidden, fmt.Errorf("%s goes through a symlink that is absolute or leaves the workspace: name the file by its own path", path)
	}
	defer f.Close() // #nosec G307 -- a read-only file's close failure is not worth an answer
	fi, err := f.Stat()
	switch {
	case err != nil:
		return nil, http.StatusBadRequest, fmt.Errorf("%s: %w", path, err)
	case fi.IsDir():
		return nil, http.StatusBadRequest, errors.New(dir)
	case !fi.Mode().IsRegular():
		return nil, http.StatusBadRequest, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("%s: %w", path, err)
	}
	return b, http.StatusOK, nil
}

// readFile reads the file at path as grep prints it, relative to the
// repo, or absolute inside the workspace (read).
func (c *checkinServe) readFile(path string) (string, bool, error) {
	b, _, err := c.read(path, checkinOutputCap, path+" is a directory: grep it or name a file in it")
	if err != nil {
		return "", false, err
	}
	if len(b) <= checkinOutputCap {
		return string(b), false, nil
	}
	return string(b[:checkinOutputCap]), true, nil
}

func (c *checkinServe) grep(pattern, ref, path string) (string, bool, error) {
	if pattern == "" {
		return "", false, fmt.Errorf("want a pattern")
	}
	switch ref {
	case "", "head":
		ref = "HEAD"
	case "main":
	default:
		return "", false, fmt.Errorf("ref %q: want head or main", ref)
	}
	args := []string{"grep", "-n", "-w", "-F", "-I", "-e", pattern, ref}
	if path != "" {
		args = append(args, "--", path)
	}
	out, cut, err := c.git(args...)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		// git grep's no-match exit is an answer, said as one
		return fmt.Sprintf("no line matches the fixed string %q as a whole word at %s", pattern, ref), false, nil
	}
	return out, cut, err
}

func (c *checkinServe) gitLog(args string) (string, bool, error) {
	allowed := map[string]bool{"-n": true, "--oneline": true, "--": true}
	var argv []string
	for _, a := range strings.Fields(args) {
		if strings.HasPrefix(a, "-") && !allowed[a] {
			return "", false, fmt.Errorf("git_log takes only -n, --oneline and -- <path>, not %s", a)
		}
		argv = append(argv, a)
	}
	return c.git(append([]string{"log"}, argv...)...)
}

func (c *checkinServe) git(args ...string) (string, bool, error) {
	return c.execInRepo(append([]string{gitBin}, append(append([]string(nil), safeGit...), args...)...))
}

func (c *checkinServe) execInRepo(argv []string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- only this file's fixed git subcommands, their arguments checked here
	cmd.Dir = c.repo()
	cmd.Env = gitEnv(nil)
	out := &capBuffer{max: checkinOutputCap}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	name := filepath.Base(argv[0])
	if ctx.Err() != nil {
		return out.String(), out.cut, fmt.Errorf("%s took longer than %s: narrow it, with -n <count> and a path after -- for git_log, or a path for grep", name, c.timeout)
	}
	if err != nil {
		return out.String(), out.cut, fmt.Errorf("%s: %w: %s", name, err, textutil.Truncate(out.String(), 1024))
	}
	return out.String(), out.cut, nil
}
