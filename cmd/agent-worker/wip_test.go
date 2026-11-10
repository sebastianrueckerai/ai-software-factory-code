//go:build unix

package main

// The worker's half of the interim push: POST /wip answers with one WIP
// commit of the worker's own on the session's start commit and its objects
// as a bundle, pushes nothing itself, and no push token is in the worker,
// its files or its children while interim pushes run — the token appears
// only at the end gate, as today.

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func gitRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedWipSession seeds a session whose start commit is the base: the task
// repository's bare remote, the bare PUSH_URL holding the task branch at
// the base, a base-holding clone for the answers' bundles, and the
// session's own clone with its wip endpoint.
func seedWipSession(t *testing.T) (h http.Handler, wip *wipServe, dir string, e workerEnv, base, mainHead, pushURL, holder string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string { return gitRun(t, args...) }
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "m0")
	remote, holderSeed := filepath.Join(t.TempDir(), "remote.git"), t.TempDir()
	pushURL = filepath.Join(t.TempDir(), "push.git")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, pushURL)
	base = run("-C", seed, "rev-parse", "HEAD")
	// the PUSH_URL holds the task branch at the base: 0 refs past it
	run("-C", seed, "push", "-q", pushURL, base+":refs/heads/factory/t-wip")
	// the base-holding repo every answer's bundle fetches into
	run("clone", "-q", remote, holderSeed)
	holder = holderSeed

	e = workerEnv{taskID: "t1", sessionID: "s-1", author: "sebastian", objective: "o",
		repoSHA: base, branch: "factory/t-wip", repoURL: remote, pushURL: pushURL}
	dir = filepath.Join(t.TempDir(), "repo")
	if err := cloneBase(e, dir); err != nil {
		t.Fatal(err)
	}
	e.repoURL = ""
	pBase, mainHead, perr := prepareRepo(dir, e)
	if perr != nil || pBase != base || mainHead != base {
		t.Fatalf("prepareRepo = %s, %s, %v; want the seeded base %s", pBase, mainHead, perr, base)
	}
	wip = newWipServe(e, dir, base, mainHead, nolog)
	h = messageHandler(mustHash(t, testMessageHash), newPushGate(), func(context.Context, string, string) error { return nil },
		newCheckinServe(t.TempDir(), nolog), wip)
	return h, wip, dir, e, base, mainHead, pushURL, holder
}

// postWip posts /wip with the session's bearer and answers the status, the
// commit the endpoint names and the bundle it ships.
func postWip(t *testing.T, h http.Handler) (int, string, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/wip", nil)
	req.Header.Set("Authorization", "Bearer session-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get(wipCommitHeader), rec.Body.Bytes()
}

// The interim push commits the work tree as one WIP commit of the worker's
// own on the session's start and replaces the task branch's tip with the
// newest tree: the worker's POST /wip answers with that commit and its
// objects and pushes nothing itself, factoryd's push of the shipped
// objects lands it on the branch, and each further interim push replaces
// the tip; the session's end push lands 1 further commit on that tip.
func TestInterimPushReplacesTip(t *testing.T) {
	h, _, dir, e, base, mainHead, pushURL, holder := seedWipSession(t)
	run := func(args ...string) string { return gitRun(t, args...) }
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// the adapter's brief.json and a scratch reproduction test stay out of
	// every WIP commit, as they stay out of the end push
	if err := os.WriteFile(filepath.Join(dir, "brief.json"), []byte(`{"objective":"o"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zz_repro_probe_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var commits []string
	pushed := base // the tip the interim pushes move, factoryd's push each time
	for i, body := range []string{"one\n", "one\ntwo\n"} {
		write(body)
		code, commit, bundle := postWip(t, h)
		if code != http.StatusOK || commit == "" {
			t.Fatalf("interim push %d: status %d, commit %q, body %q, want 200 with a commit", i+1, code, commit, bundle)
		}
		// factoryd fetches the shipped objects into the base-holding repo
		path := filepath.Join(t.TempDir(), "wip.bundle")
		if err := os.WriteFile(path, bundle, 0o600); err != nil {
			t.Fatal(err)
		}
		ref := "refs/heads/wip" + string(rune('1'+i))
		run("-C", holder, "fetch", "-q", path, "refs/heads/wip:"+ref)
		if got := run("-C", holder, "rev-parse", ref); got != commit {
			t.Fatalf("interim push %d: the fetched bundle holds %s, want the answer's commit %s", i+1, got, commit)
		}
		if parent := run("-C", holder, "rev-list", "--parents", "-n", "1", commit); parent != commit+" "+base {
			t.Fatalf("interim push %d: parents = %q, want the start %s alone", i+1, parent, base)
		}
		short := run("-C", holder, "diff", "--shortstat", base, commit)
		if d := parseShortstat(short); d.Files != 1 || d.Insertions != i+1 || d.Deletions != 0 {
			t.Fatalf("interim push %d: diff = %q, want 1 file, %d insertion(s)", i+1, short, i+1)
		}
		if names := run("-C", holder, "diff", "--name-only", base, commit); names != "b.txt" {
			t.Fatalf("interim push %d: changed %q, want b.txt alone: brief.json and scratch repro tests stay out", i+1, names)
		}
		// the worker pushes nothing itself: the PUSH_URL holds exactly what
		// factoryd last pushed, nothing the /wip answer added
		if got := run("-C", pushURL, "rev-parse", "refs/heads/"+e.branch); got != pushed {
			t.Fatalf("after interim push %d the PUSH_URL holds %s, want still %s", i+1, got, pushed)
		}
		// factoryd pushes the fetched commit with its own credential,
		// replacing the tip
		run("-C", holder, "push", "-q", pushURL, "+"+ref+":refs/heads/"+e.branch)
		commits = append(commits, commit)
		pushed = commit
	}

	// after the second interim push the branch holds exactly 1 commit past
	// the base with that diff
	if got := run("-C", pushURL, "rev-parse", "refs/heads/"+e.branch); got != commits[1] {
		t.Fatalf("the branch holds %s, want the second interim push's %s", got, commits[1])
	}
	if n := run("-C", pushURL, "rev-list", "--count", base+".."+commits[1]); n != "1" {
		t.Fatalf("the branch holds %s commits past the base, want exactly 1", n)
	}

	// the end push lands 1 further commit on the replaced tip: without the
	// forced refspec it would be refused as non-fast-forward, for its
	// parent is the start the interim push replaced
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	end, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
	if err != nil {
		t.Fatalf("the end push after a tip an interim push replaced: %v", err)
	}
	if end == commits[1] {
		t.Fatal("the end push pushed the interim commit again, not one of its own")
	}
	if got := run("-C", pushURL, "rev-parse", "refs/heads/"+e.branch); got != end {
		t.Fatalf("the branch holds %s after the end push, want its commit %s", got, end)
	}
	if n := run("-C", pushURL, "rev-list", "--count", base+".."+end); n != "1" {
		t.Fatalf("the branch holds %s commits past the base after the end push, want 1 further commit", n)
	}
	if d := parseShortstat(run("-C", pushURL, "diff", "--shortstat", base, end)); d.Files != 1 || d.Insertions != 3 {
		t.Fatalf("the end push's diff = %+v, want b.txt's 3 lines", d)
	}
}

// No push token sits in the worker's environment, its files or its
// children while interim pushes run; the token appears only at the end
// gate, as today, and the session's scrub masks it in an emitted line.
func TestWipPushTokenAbsentUntilEnd(t *testing.T) {
	h, wip, dir, e, base, mainHead, _, _ := seedWipSession(t)
	const tok = "w1p-secret-token-42"

	var staged bytes.Buffer
	wip.probe = func(dir string) {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { // #nosec G104 -- a file that cannot be read carries nothing
			if err == nil && d.Type().IsRegular() {
				if b, rerr := os.ReadFile(p); rerr == nil {
					staged.Write(b)
				}
			}
			return nil
		})
	}
	var envs bytes.Buffer
	wip.probeEnv = func(env string) { envs.WriteString(env + "\n") }

	// factoryd's push token endpoint answers 409 while no ask waits: the
	// /wip path must leave the gate unasked
	postPush := func() int {
		req := httptest.NewRequest(http.MethodPost, "/push", strings.NewReader(`{"token":"`+tok+`"}`))
		req.Header.Set("Authorization", "Bearer session-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i, body := range []string{"one\n", "one\ntwo\n"} {
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, commit, answer := postWip(t, h); code != http.StatusOK || commit == "" {
			t.Fatalf("interim push %d: status %d, commit %q, body %q", i+1, code, commit, answer)
		}
		if code := postPush(); code != http.StatusConflict {
			t.Fatalf("after interim push %d the push endpoint answered %d, want 409: no gate ask may wait", i+1, code)
		}
	}

	// the worker's own environment, the files the /wip path writes and the
	// environments of the children it spawns: none carries the token
	for name, probe := range map[string]string{
		"the worker's environment": strings.Join(os.Environ(), "\n"),
		"the /wip path's files":    staged.String(),
		"the children's":           envs.String(),
	} {
		if n := strings.Count(probe, tok); n != 0 {
			t.Fatalf("%s holds %d occurrences of the push token", name, n)
		}
	}
	if envs.Len() == 0 || staged.Len() == 0 {
		t.Fatal("the probes saw nothing: the /wip path's gits and files went unobserved")
	}

	// the end push asks through the gate exactly once, as today, and the
	// session's scrub masks the token in an emitted line
	tel, err := newSessionTelemetry(sdktrace.NewTracerProvider(), sdkmetric.NewMeterProvider(), "t1", "s-1", "factory/small")
	if err != nil {
		t.Fatal(err)
	}
	gate := newPushGate()
	s := &session{env: e, gate: gate, tel: tel, closeTelemetry: func() {}}
	var out bytes.Buffer
	emit := s.emitter(&out)
	var asks int
	commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, func(commit string) (string, error) {
		asks++
		return gate.ask(commit, time.Second, writerFunc(func(p []byte) (int, error) {
			// factoryd reads the ask and answers it
			if !gate.take(tok) {
				t.Error("no ask waited for factoryd's token")
			}
			return len(p), nil
		}))
	})
	if err != nil || commit == "" {
		t.Fatalf("the end push = %q, %v", commit, err)
	}
	if asks != 1 {
		t.Fatalf("the end push asked %d times, want exactly once", asks)
	}
	emit(harness.Event{Kind: harness.EventCommand, Detail: map[string]any{"command": "git push", "output": "auth " + tok + " refused"}})
	if strings.Contains(out.String(), tok) {
		t.Fatalf("a session token left the worker:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "[redacted]") {
		t.Fatalf("the scrub did not mask the token in the emitted line:\n%s", out.String())
	}
}

// A POST /wip whose stage fails after its temp repo exists — here the
// fetch of a repoDir that is not a git repository — answers 500 and
// leaves no .wip- dir behind: done is the staging repo's only remover,
// and the failing answer's path runs it too.
func TestWipFailedStageRemovesStagingDir(t *testing.T) {
	h, wip, dir, _, _, _, _, _ := seedWipSession(t)
	parent := filepath.Dir(dir)
	bad := filepath.Join(parent, "notarepo")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wip.repoDir = bad
	var probed []string
	wip.probe = func(dir string) { probed = append(probed, dir) }

	if code, _, answer := postWip(t, h); code != http.StatusInternalServerError {
		t.Fatalf("POST /wip against a non-repo answered %d with %q, want 500", code, answer)
	}
	if len(probed) == 0 {
		t.Fatal("the stage failed before its temp repo existed: the probe saw no staging dir")
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var leaks []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".wip-") {
			leaks = append(leaks, entry.Name())
		}
	}
	if len(leaks) != 0 {
		t.Fatalf("the failed POST left %d staging dir(s) behind: %v", len(leaks), leaks)
	}
}

// A session whose end push the line bound refused holds its result until
// factoryd's /wip ask is answered, so the finished work leaves the pod; with
// no ask it lets go at its bound.
func TestWipHoldForAskWaitsForTheNextAnswer(t *testing.T) {
	h, wip, _, _, _, _, _, _ := seedWipSession(t)
	if wip.holdForAsk(wip.started.Load(), 100*time.Millisecond) {
		t.Fatal("the hold ended with no ask")
	}
	since := wip.started.Load()
	done := make(chan bool)
	go func() { done <- wip.holdForAsk(since, 10*time.Second) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the hold ended before any ask")
	default:
	}
	postWip(t, h)
	if !<-done {
		t.Fatal("the hold ran out although an ask was answered")
	}
}

// An ask already under way when the agent stopped may have staged the tree
// before its last write: its answer does not end the hold.
func TestWipHoldForAskIgnoresAnAskBegunBeforeTheStop(t *testing.T) {
	h, wip, _, _, _, _, _, _ := seedWipSession(t)
	wip.started.Add(1) // an ask under way at the stop
	since := wip.started.Load()
	done := make(chan bool)
	go func() { done <- wip.holdForAsk(since, 10*time.Second) }()
	wip.ended.Add(1) // ... and it is answered
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the answer of an ask begun before the stop ended the hold")
	default:
	}
	postWip(t, h)
	if !<-done {
		t.Fatal("the hold ran out although a later ask was answered")
	}
}
