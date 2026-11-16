package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// forgejoFake is a Forgejo git endpoint as the push sees it: a refused
// token answers 404 with a body git renders as "remote: Repository not
// found" — the refusal of a dead credential — and an accepted request is
// served by git http-backend, so a push that lands really lands.
type forgejoFake struct {
	t      *testing.T
	root   string
	refuse map[string]bool
	srv    *httptest.Server
}

func newForgejoFake(t *testing.T, refusedTokens ...string) *forgejoFake {
	f := &forgejoFake{t: t, root: t.TempDir(), refuse: map[string]bool{}}
	for _, tok := range refusedTokens {
		f.refuse[tok] = true
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// repo mints owner/name as a bare repository under the fake's root and
// answers with its git URL.
func (f *forgejoFake) repo(owner, name string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, owner, name+".git")
	if out, err := exec.Command("git", "init", "-q", "--bare", dir).CombinedOutput(); err != nil {
		f.t.Fatalf("git init %s: %v: %s", dir, err, out)
	}
	return f.srv.URL + "/" + owner + "/" + name + ".git"
}

func (f *forgejoFake) serve(w http.ResponseWriter, r *http.Request) {
	_, pass, ok := r.BasicAuth()
	if !ok || f.refuse[pass] {
		// Forgejo answers a token it refuses with the 404 whose body git
		// prints as "remote: Repository not found"
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "Repository not found")
		return
	}
	body, _ := io.ReadAll(r.Body)
	cmd := exec.Command("git", "http-backend")
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+f.root,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO="+r.URL.Path,
		"QUERY_STRING="+r.URL.RawQuery,
		"REQUEST_METHOD="+r.Method,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"CONTENT_LENGTH="+strconv.Itoa(len(body)),
		"REMOTE_USER=x-access-token",
		"GATEWAY_INTERFACE=CGI/1.1",
	)
	cmd.Stdin = bytes.NewReader(body)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		f.t.Logf("git http-backend %s: %v: %s", r.URL.Path, err, errOut.String())
	}
	head, rest, _ := bytes.Cut(out.Bytes(), []byte("\r\n\r\n"))
	status := http.StatusOK
	for _, line := range bytes.Split(head, []byte("\r\n")) {
		k, v, _ := bytes.Cut(line, []byte(": "))
		switch {
		case strings.EqualFold(string(k), "Status"):
			if n, err := strconv.Atoi(strings.Fields(string(v))[0]); err == nil {
				status = n
			}
		case len(k) > 0:
			w.Header().Set(string(k), string(v))
		}
	}
	w.WriteHeader(status)
	w.Write(rest)
}

// forkHead is the fork's branch head, read from its bare repository.
func forkHead(t *testing.T, dir, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", dir, "rev-parse", "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v: %s", branch, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A push refused as a dead credential re-asks, and the fresh token's push
// lands: the retry is the push, not the work (issue 1836).
func TestRefusedPushReasksAndLandsWithTheFreshToken(t *testing.T) {
	dir, e, base, mainHead, _ := changedSession(t)
	git := newForgejoFake(t, "tok-old")
	fork := git.repo("factory-agent", "x")
	e.pushURL, e.repoURL = fork, git.repo("sf", "x")

	var names []string
	commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, func(string) (pushCred, error) {
		cred := pushCred{token: "tok-old", name: "task-t1-push-own-1"}
		if len(names) > 0 {
			// the re-ask: factoryd answers each ask with a fresh mint
			cred = pushCred{token: "tok-fresh", name: "task-t1-push-own-2"}
		}
		names = append(names, cred.name)
		return cred, nil
	})
	if err != nil || commit == "" {
		t.Fatalf("commitAndPush = (%q, %v), want the push landed on the fresh token", commit, err)
	}
	if want := []string{"task-t1-push-own-1", "task-t1-push-own-2"}; !slices.Equal(names, want) {
		t.Fatalf("asks = %v, want the refused credential and the fresh one %v", names, want)
	}
	forkDir := filepath.Join(git.root, "factory-agent", "x.git")
	if got := forkHead(t, forkDir, e.branch); got != commit {
		t.Fatalf("fork branch head = %q, want the pushed commit %s", got, commit)
	}
}

// A push refused as a dead credential re-asks up to pushReasks times, then
// fails naming the refused credential — not as a missing repository.
func TestRefusedPushBoundNamesTheCredential(t *testing.T) {
	dir, e, base, mainHead, _ := changedSession(t)
	git := newForgejoFake(t, "tok-1", "tok-2", "tok-3", "tok-4")
	e.pushURL, e.repoURL = git.repo("factory-agent", "x"), git.repo("sf", "x")

	var names []string
	_, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, func(string) (pushCred, error) {
		cred := pushCred{token: "tok-" + strconv.Itoa(len(names)+1), name: "task-t1-push-own-" + strconv.Itoa(len(names)+1)}
		names = append(names, cred.name)
		return cred, nil
	})
	if err == nil || !strings.Contains(err.Error(), "push credential task-t1-push-own-4 refused by "+e.pushURL) ||
		!strings.Contains(err.Error(), "Repository not found") {
		t.Fatalf("err = %v, want the refused credential named with the refusal", err)
	}
	if len(names) != 1+pushReasks {
		t.Fatalf("asks = %v, want %d: the first and every re-ask the bound allows", names, 1+pushReasks)
	}
}

// A push that fails for no credential reason — the fork is genuinely
// missing — fails as before: no re-ask, no fresh token, the raw refusal.
func TestPushToAMissingRepoFailsAsBefore(t *testing.T) {
	dir, e, base, mainHead, _ := changedSession(t)
	git := newForgejoFake(t)
	e.repoURL = git.repo("sf", "x")
	e.pushURL = git.srv.URL + "/factory-agent/missing.git"

	var asks int
	_, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, func(string) (pushCred, error) {
		asks++
		return pushCred{token: "tok-fresh", name: "task-t1-push-own-1"}, nil
	})
	if err == nil || strings.Contains(err.Error(), "push credential") {
		t.Fatalf("err = %v, want the raw failure of a missing repository", err)
	}
	if asks != 1 {
		t.Fatalf("asks = %d, want no re-ask for a missing repository", asks)
	}
}
