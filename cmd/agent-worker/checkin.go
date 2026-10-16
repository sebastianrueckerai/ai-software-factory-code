package main

// The check-in's endpoints on the worker's message server (the escalation
// ladder's plan D): factoryd runs the check-in conversation against this
// workspace before the coding conversation starts. Its tools read the
// workspace, the handover it lands becomes the brief's first section, and
// the parent session's record is delivered here for the coding agent to
// grep.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/textutil"
)

// checkinWait is how long the session's start holds for the check-in's
// handover before the conversation begins without one. A var, like
// pushRetryEvery, so a test can shorten it.
var checkinWait = 10 * time.Minute

const (
	// checkinRunTimeout caps one tool run.
	checkinRunTimeout = 15 * time.Minute
	// checkinOutputCap cuts every tool's output.
	checkinOutputCap = 32 << 10
	// checkinRecordCap bounds the parent record the endpoint accepts.
	checkinRecordCap = 64 << 20
)

// checkinServe carries the check-in's endpoints: the tool executions run
// in the session's workspace, the handover and the parent record arrive
// before the coding conversation starts.
type checkinServe struct {
	ws       string        // the workspace root
	wait     time.Duration // how long oc.Run's start holds for /brief
	timeout  time.Duration // a run tool's cap
	handover chan string   // exactly one send: the pushed note, or "" at the deadline
}

func newCheckinServe(ws string) *checkinServe {
	return &checkinServe{ws: ws, wait: checkinWait, timeout: checkinRunTimeout, handover: make(chan string, 1)}
}

// waitBrief holds the conversation's start for the check-in's handover:
// the note when it arrived within the wait, "" when none did.
func (c *checkinServe) waitBrief(log func(string, ...any)) string {
	select {
	case note := <-c.handover:
		return note
	case <-time.After(c.wait):
		log("no check-in handover within %s; the conversation starts without one", c.wait)
		return ""
	}
}

// serve mounts the check-in's three endpoints under the /message bearer:
// the tools the check-in's model calls, the handover that starts the
// conversation, and the parent session's record.
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
	mux.HandleFunc("POST /brief", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var in struct {
			Handover string `json:"handover"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			http.Error(w, `want {"handover": "..."}`, http.StatusBadRequest)
			return
		}
		select {
		case c.handover <- in.Handover:
		default: // a retry, or a push past the wait: the first note stands
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
			http.Error(w, "session must be a bare id, no \"/\" or \"..\"", http.StatusBadRequest)
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

// badSession refuses a session id that could leave the sessions directory.
func badSession(session string) bool {
	return session == "" || strings.Contains(session, "/") || strings.Contains(session, "..")
}

var errOutside = errors.New("outside the workspace")

// tool runs one of the check-in's four workspace tools; anything else is
// refused. Every output is cut at checkinOutputCap.
func (c *checkinServe) tool(ctx context.Context, name string, arg func(string) string) (string, bool, error) {
	switch name {
	case "read_file":
		return c.readFile(arg("path"))
	case "grep":
		return c.grep(arg("pattern"), arg("ref"), arg("path"))
	case "git_log":
		return c.gitLog(arg("args"))
	case "run":
		return c.run(ctx, arg("command"))
	}
	return "", false, fmt.Errorf("unknown tool %q", name)
}

// repo is the workspace's clone, where the git tools and run work.
func (c *checkinServe) repo() string { return filepath.Join(c.ws, "repo") }

// under resolves path under the workspace root, refusing one that is
// absolute or climbs out of it.
func (c *checkinServe) under(path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errOutside
	}
	p := filepath.Clean(filepath.Join(c.ws, path))
	if !strings.HasPrefix(p, c.ws+string(filepath.Separator)) {
		return "", errOutside
	}
	return p, nil
}

func (c *checkinServe) readFile(path string) (string, bool, error) {
	p, err := c.under(path)
	if err != nil {
		return "", false, err
	}
	f, err := os.Open(p) // #nosec G304 -- under() keeps p inside the workspace
	if err != nil {
		return "", false, err
	}
	defer f.Close() // #nosec G307 -- a read-only file's close failure is not worth an answer
	b, err := io.ReadAll(io.LimitReader(f, checkinOutputCap+1))
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
		return "", false, nil // git grep's no-match exit is an answer
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

func (c *checkinServe) run(ctx context.Context, command string) (string, bool, error) {
	if strings.TrimSpace(command) == "" {
		return "", false, fmt.Errorf("want a command")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command) // #nosec G204 -- the check-in's run tool: the pod's own shell under the sandbox's egress policy, as the coding agent's
	cmd.Dir = c.repo()
	// the shell's children (a pipeline) must die with it: they hold the
	// output pipe, and WaitDelay bounds whatever slips the group all the
	// same, so the timeout holds however the command is shaped.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out := &capBuffer{max: checkinOutputCap}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), out.cut, fmt.Errorf("run timed out after %s", c.timeout)
	}
	if err != nil {
		return out.String(), out.cut, fmt.Errorf("run: %w: %s", err, textutil.Truncate(out.String(), 1024))
	}
	return out.String(), out.cut, nil
}

func (c *checkinServe) execInRepo(argv []string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- only this file's fixed git subcommands, their arguments checked here
	cmd.Dir = c.repo()
	out := &capBuffer{max: checkinOutputCap}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	name := filepath.Base(argv[0])
	if ctx.Err() != nil {
		return out.String(), out.cut, fmt.Errorf("%s timed out after %s", name, c.timeout)
	}
	if err != nil {
		return out.String(), out.cut, fmt.Errorf("%s: %w: %s", name, err, textutil.Truncate(out.String(), 1024))
	}
	return out.String(), out.cut, nil
}
