//go:build unix

package main

// The worker's half of the interim push: POST /wip on the message server
// answers factoryd with one WIP commit of the worker's own on the
// session's start commit and its objects as a git bundle. It runs in the
// worker process, kills nothing, asks for no token and pushes nothing —
// the push rules and the push itself are factoryd's, with its own
// credential. The commit is built the checkpointer's way: a fresh repo of
// the worker's own borrowing the agent repo's objects, safeGit on every
// git, brief.json excluded, scratch reproduction tests dropped — a
// snapshot of a moving tree is valid, as the checkpoint's comment says.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// wipBundleCap bounds the bundle one answer streams: the message server's
// other endpoints bound their bodies the same way.
const wipBundleCap = checkinRecordCap

// wipCommitHeader names the answer's commit to factoryd; the body is the
// bundle of base..commit.
const wipCommitHeader = "Wip-Commit"

// wipServe answers POST /wip for one session: the agent's clone, the
// session's start commit and main.
type wipServe struct {
	e       workerEnv
	repoDir string // the agent's clone
	// base is the session's start commit, mainHead the main it starts
	// from
	base, mainHead string
	log            func(string, ...any)
	mu             sync.Mutex // one /wip at a time
	// probe is a test's look at the staging repo before it is removed,
	// probeEnv at the environment of each git the /wip path spawns.
	probe, probeEnv func(dirOrEnv string)
}

func newWipServe(e workerEnv, repoDir, base, mainHead string, log func(string, ...any)) *wipServe {
	return &wipServe{e: e, repoDir: repoDir, base: base, mainHead: mainHead, log: log}
}

// serve mounts POST /wip under the session's bearer.
func (w *wipServe) serve(mux *http.ServeMux, authorized func(http.ResponseWriter, *http.Request) bool) {
	mux.HandleFunc("POST /wip", func(rw http.ResponseWriter, r *http.Request) {
		if !authorized(rw, r) {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		commit, bundle, done, err := w.stage()
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		defer done() // the staging repo outlives the body it answers
		if commit == "" {
			// the tree is the start's: there is nothing to ship
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		fi, err := os.Stat(bundle)
		if err != nil || fi.Size() > wipBundleCap {
			http.Error(rw, "the work tree's bundle is over the answer's bound", http.StatusInternalServerError)
			return
		}
		f, err := os.Open(bundle) // #nosec G304 -- a path this file built itself, inside the staging repo it made
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close() // #nosec G307 -- the body is done with; a read file's close failure is not worth an answer
		rw.Header().Set(wipCommitHeader, commit)
		rw.Header().Set("Content-Type", "application/x-git-bundle")
		rw.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		io.Copy(rw, f) // #nosec G104 -- best-effort response write
	})
}

// stage builds snapshot's answer with the staging repo held open: the
// bundle it returns sits inside that repo, so done — which gives a test
// its look and then removes the repo — must not run before the answer's
// body is written. commit is empty, with no bundle, when the tree is the
// start's.
func (w *wipServe) stage() (commit, bundle string, done func(), err error) {
	own, err := os.MkdirTemp(filepath.Dir(w.repoDir), ".wip-")
	if err != nil {
		return "", "", nil, err
	}
	done = func() {
		if w.probe != nil {
			w.probe(own)
		}
		os.RemoveAll(own) // #nosec G104 -- the staging repo is done with; a removal that fails leaves a temp dir
	}
	git := func(env []string, args ...string) (string, error) {
		if w.probeEnv != nil {
			w.probeEnv(strings.Join(append(gitEnv(w.e.gitTrace), env...), "\n"))
		}
		return w.e.runGit(own, env, args...)
	}
	if _, err := git(nil, "init", "-q"); err != nil {
		return "", "", done, err
	}
	// the adapter writes brief.json into the work tree; it must never be committed
	if err := os.WriteFile(filepath.Join(own, ".git", "info", "exclude"), []byte("/brief.json\n"), 0o600); err != nil {
		return "", "", done, err
	}
	if _, err := git(nil, "fetch", "-q", "--no-tags", uploadPack, w.repoDir,
		"+HEAD:refs/heads/agent", "+"+w.base+":refs/heads/start", "+"+w.mainHead+":refs/heads/main"); err != nil {
		return "", "", done, err
	}
	env := []string{"GIT_DIR=" + filepath.Join(own, ".git"), "GIT_WORK_TREE=" + w.repoDir}
	for _, args := range [][]string{{"read-tree", "refs/heads/agent"}, {"add", "-A"}} {
		if _, err := git(env, args...); err != nil {
			return "", "", done, err
		}
	}
	if err := dropScratchReproTests(own, env, w.e); err != nil {
		return "", "", done, err
	}
	tree, err := git(env, "write-tree")
	if err != nil {
		return "", "", done, err
	}
	startTree, err := git(nil, "rev-parse", w.base+"^{tree}")
	if err != nil {
		return "", "", done, err
	}
	if tree == startTree {
		return "", "", done, nil
	}
	agent, err := git(nil, "rev-parse", "refs/heads/agent")
	if err != nil {
		return "", "", done, err
	}
	parents, _ := sessionParents(w.e, own, w.base, w.mainHead, agent)
	if commit, err = git(botIdentity, append(append([]string{"commit-tree", tree}, parents...), "-m", commitMessage(w.e))...); err != nil {
		return "", "", done, err
	}
	// the staged tree is not reachable from the agent's HEAD, so the
	// bundle's tip is the WIP commit itself: the objects the answer ships
	// are the base's lack, not the agent's history
	if _, err := git(nil, "update-ref", "refs/heads/wip", commit); err != nil {
		return "", "", done, err
	}
	bundle = filepath.Join(own, "wip.bundle")
	if _, err := git(nil, "bundle", "create", "-q", bundle, w.base+"..refs/heads/wip"); err != nil {
		return "", "", done, err
	}
	return commit, bundle, done, nil
}
