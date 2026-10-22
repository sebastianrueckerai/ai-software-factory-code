//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	for name, val := range map[string]string{
		"TASK_ID": "t1", "OBJECTIVE": "do it", "REPOSITORY": "sf/repo",
		"BRANCH": "factory/t1", "PUSH_URL": "https://example/fork.git", "GATEWAY_URL": "http://gw",
		"TOKEN_FILE": "/tmp/token", "MODEL": "openrouter/x", "ANSWER_TOKENS": "100", "HEADROOM_TOKENS": "10", "STEPS": "200",
		"MESSAGE_TOKEN_SHA256": testMessageHash, "EGRESS_CANARY": "example.com:443", "EGRESS_DIRECT_IP": "192.0.2.7:443",
		// the worker's own sandbox exports REPO_SHA; the seeded repos of
		// the tests hold no such commit, so none is pinned here
		"REPO_SHA": "",
		// a start puts its executable's directory first on PATH
		"PATH": os.Getenv("PATH"),
	} {
		t.Setenv(name, val)
	}
}

// The pod's harness volume holds the worker's executable, opencode beside
// it and the opencode bake one level up, in any image: opencode resolves
// to the one beside the worker, ahead of an agent-writable directory on
// PATH, and the bake fills a HOME without an opencode config; a HOME
// with one keeps it.
func TestUseHarnessBesideTheExecutable(t *testing.T) {
	root := t.TempDir()
	bin, bake, planted := filepath.Join(root, "bin"), filepath.Join(root, "opencode-home", ".config", "opencode"), t.TempDir()
	for _, dir := range []string{bin, bake} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(bin, "opencode"):      "#!/bin/sh\n",
		filepath.Join(planted, "opencode"):  "#!/bin/sh\n",
		filepath.Join(bake, "package.json"): `{"dependencies":{"@opencode-ai/plugin":"1.18.32"}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", planted+":/usr/bin:/bin")
	home := t.TempDir()
	if err := useHarness(filepath.Join(bin, "agent-worker"), home); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH"); got != bin+":"+planted+":/usr/bin:/bin" {
		t.Errorf("PATH = %q, want the executable's directory first", got)
	}
	if got, err := exec.LookPath("opencode"); err != nil || got != filepath.Join(bin, "opencode") {
		t.Errorf("opencode resolves to %q (%v), want %s", got, err, filepath.Join(bin, "opencode"))
	}
	if got, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "package.json")); err != nil ||
		string(got) != `{"dependencies":{"@opencode-ai/plugin":"1.18.32"}}` {
		t.Errorf("restored config = %q (%v), want the bake's package.json", got, err)
	}

	configured := t.TempDir()
	own := filepath.Join(configured, ".config", "opencode")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := useHarness(filepath.Join(bin, "agent-worker"), configured); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(own); err != nil || len(entries) != 0 {
		t.Errorf("a HOME with an opencode config holds %v (%v) after the start, want it kept as it was", entries, err)
	}
}

// A malformed FINDINGS must fail loudly, not silently become a first
// attempt with no findings.
func TestLoadEnvRejectsMalformedFindings(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FINDINGS", "not json")
	if _, err := loadEnv(); err == nil {
		t.Fatal("want an error for malformed FINDINGS, got nil")
	}
}

// Roadmap 237: LEFT_OUT, factoryd's notes on what the env could not
// carry, reaches the worker as a list for the brief.
func TestLoadEnvReadsWhatWasLeftOut(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LEFT_OUT", `["1 of the 33 findings, the oldest"]`)
	e, err := loadEnv()
	if err != nil || !slices.Equal(e.leftOut, []string{"1 of the 33 findings, the oldest"}) {
		t.Fatalf("leftOut = %q, %v; want the note", e.leftOut, err)
	}
}

// IMAGE_MANIFEST, what the session's project image holds, reaches the
// brief; absent, the session runs the factory's image.
func TestLoadEnvReadsTheImage(t *testing.T) {
	setRequiredEnv(t)
	if e, err := loadEnv(); err != nil || e.image != nil {
		t.Fatalf("no IMAGE_MANIFEST: %+v, %v; want the factory's image", e.image, err)
	}
	setRequiredEnv(t)
	t.Setenv("IMAGE_MANIFEST", `{"tools":["chromium"],"egress":["cdn.example.org"]}`)
	e, err := loadEnv()
	if err != nil || e.image == nil || !slices.Equal(e.image.Tools, []string{"chromium"}) || !slices.Equal(e.image.Egress, []string{"cdn.example.org"}) {
		t.Fatalf("image = %+v, %v; want chromium reaching cdn.example.org", e.image, err)
	}
	setRequiredEnv(t)
	t.Setenv("IMAGE_MANIFEST", "not json")
	if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "IMAGE_MANIFEST") {
		t.Fatalf("malformed IMAGE_MANIFEST: %v, want an error naming it", err)
	}
}

// The subagents' cheap model reaches opencode from factoryd's CHEAP_MODEL
// (roadmap 222, item 6). Without it the worker runs opencode with no cheap
// alias and every subagent falls back to the session's own model.
func TestLoadEnvReadsTheCheapModel(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("STRONGER_MODEL", "factory/big")
	t.Setenv("CHEAP_MODEL", "factory/small")
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if e.strongerModel != "factory/big" || e.cheapModel != "factory/small" {
		t.Fatalf("stronger = %q, cheap = %q; want the env values", e.strongerModel, e.cheapModel)
	}
}

// The step cap reaches opencode's config from factoryd's STEPS; a session
// without one must not run uncapped.
func TestLoadEnvStepCap(t *testing.T) {
	setRequiredEnv(t)
	e, err := loadEnv()
	if err != nil || e.steps != 200 {
		t.Fatalf("steps = %d, %v; want 200", e.steps, err)
	}
	for _, v := range []string{"", "0", "-3", "many"} {
		setRequiredEnv(t) // loadEnv unsets the tokens
		t.Setenv("STEPS", v)
		if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "STEPS") {
			t.Errorf("STEPS=%q: err = %v, want a STEPS error", v, err)
		}
	}
}

func TestLoadEnvFindingsAbsentMeansFirstAttempt(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FINDINGS", "")
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if e.findings != nil {
		t.Fatalf("findings = %v, want nil", e.findings)
	}
}

func TestLoadEnvParsesFindings(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FINDINGS", `["missing test", "wrong error code"]`)
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if len(e.findings) != 2 || e.findings[0] != "missing test" || e.findings[1] != "wrong error code" {
		t.Fatalf("findings = %v", e.findings)
	}
}

// The card's directories (DIRECTORIES) become the brief's map, read from
// the clone: a hand-filed card's internal/console and internal/policy, the
// second not in the repository.
func TestCardMapReadsTheClone(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DIRECTORIES", `["internal/console","internal/policy"]`)
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "internal", "console"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "console", "chat.go"), []byte("// Package console serves the board.\npackage console\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := cardMap(repo, e.directories)
	want := "Repository map of the card's directories (guidance, not a boundary) at the commit this session starts from, each file with its first doc line:\n" +
		"internal/console/chat.go: Package console serves the board.\ninternal/policy/: not in the repository"
	if err != nil || got != want {
		t.Fatalf("map = %q, %v; want %q", got, err, want)
	}
	if got, err := cardMap(repo, nil); got != "" || err != nil {
		t.Fatalf("no directories: map = %q, %v; want none", got, err)
	}
}

// A resumed session starts from the branch head an earlier session pushed
// to the fork: the base clone from the task's repository lacks it, so the
// worker fetches the branch from the fork before the checkout.
func TestCloneBaseFetchesAResumedHeadFromTheFork(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, remote, fork := t.TempDir(), filepath.Join(t.TempDir(), "remote.git"), filepath.Join(t.TempDir(), "fork.git")
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	run("-C", seed, "checkout", "-q", "-b", "factory/t1")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "attempt 1")
	run("-C", seed, "push", "-q", fork, "factory/t1")
	head := run("-C", seed, "rev-parse", "HEAD")

	e := workerEnv{taskID: "t1", repoSHA: head, branch: "factory/t1", repoURL: remote, pushURL: fork}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := cloneBase(e, dir); err != nil {
		t.Fatal(err)
	}
	if base, _, err := prepareRepo(dir, e); err != nil || base != head {
		t.Fatalf("prepareRepo = (%s, %v), want the fork's branch head %s", base, err, head)
	}
}

// A revived task whose pull request closed and whose branch left the fork
// resumes from the head the task's repository keeps as refs/pull/<n>/head
// (roadmap 214); with no pull request to read, or none holding the start,
// the clone fails naming where it looked. The remotes are file:// URLs: a
// plain path clone would copy every object, the pull request's included.
func TestCloneBaseRecoversAClosedPullRequestsHead(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, remote, fork := t.TempDir(), filepath.Join(t.TempDir(), "remote.git"), filepath.Join(t.TempDir(), "fork.git")
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	run("-C", seed, "checkout", "-q", "-b", "factory/t1")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "attempt 1")
	run("-C", seed, "push", "-q", remote, "factory/t1:refs/pull/7/head")
	head := run("-C", seed, "rev-parse", "HEAD")

	e := workerEnv{taskID: "t1", repoSHA: head, branch: "factory/t1", repoURL: "file://" + remote, pushURL: "file://" + fork, taskPR: "7"}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := cloneBase(e, dir); err != nil {
		t.Fatal(err)
	}
	if base, _, err := prepareRepo(dir, e); err != nil || base != head {
		t.Fatalf("prepareRepo = (%s, %v), want the pull request's head %s", base, err, head)
	}

	e.taskPR = ""
	if err := cloneBase(e, filepath.Join(t.TempDir(), "repo")); err == nil || !strings.Contains(err.Error(), "the start "+head+" is on no branch the session can fetch") {
		t.Fatalf("no pull request: err = %v, want the start named", err)
	}
	e.taskPR = "8"
	if err := cloneBase(e, filepath.Join(t.TempDir(), "repo")); err == nil || !strings.Contains(err.Error(), "nor pull request #8's head") {
		t.Fatalf("no such pull request: err = %v, want both places named", err)
	}
}

func TestGitRunRedactsToken(t *testing.T) {
	e := workerEnv{pushToken: "s3cret-tok"}
	err := e.gitAuth(e.pushToken, "", "clone", "https://x-access-token:s3cret-tok@git.example/repo.git", "/nonexistent-dir")
	if err == nil {
		t.Fatal("clone into a nonexistent dir should fail")
	}
	if strings.Contains(err.Error(), "s3cret-tok") {
		t.Fatalf("git error leaks the token: %s", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("git error not redacted: %s", err)
	}
}

// The worker container starts with no Forgejo token: the clone init
// container holds the read token, and the push token arrives on POST /push
// once the agent is gone. A token the worker gets carries into no git
// config file.
func TestTokenStaysOutOfEnvAndGitConfig(t *testing.T) {
	for k, v := range map[string]string{
		"TASK_ID": "t1", "OBJECTIVE": "o", "REPOSITORY": "sf/r", "BRANCH": "b", "PUSH_URL": "https://example/fork.git",
		"GATEWAY_URL": "http://gw", "TOKEN_FILE": "/tok", "MODEL": "m",
		"ANSWER_TOKENS": "1", "HEADROOM_TOKENS": "1", "STEPS": "1", "MESSAGE_TOKEN_SHA256": testMessageHash,
		"EGRESS_CANARY": "example.com:443", "EGRESS_DIRECT_IP": "192.0.2.7:443",
	} {
		t.Setenv(k, v)
	}
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	e, err := loadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.pushToken != "" || e.repoToken != "" || e.repoURL != "" {
		t.Fatalf("push token %q, read token %q, repository URL %q; want none", e.pushToken, e.repoToken, e.repoURL)
	}

	dst := filepath.Join(t.TempDir(), "repo")
	if err := e.gitAuth("pu5h-tok", "", "clone", src, dst); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(dst, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "pu5h-tok") || strings.Contains(string(cfg), "extraHeader") {
		t.Fatalf(".git/config carries the credential:\n%s", cfg)
	}
	// only gitAuth carries the header: neither the worker's other git nor the agent's sees one
	if header, err := e.git(dst, "config", "--get", "http.extraHeader"); err == nil || header != "" {
		t.Fatalf("token-free worker git sees a header: %q", header)
	}
	if out, _ := exec.Command("git", "-C", dst, "config", "--get", "http.extraHeader").Output(); len(out) != 0 {
		t.Fatalf("plain git sees a header: %q", out)
	}
	// Basic auth for x-access-token:s3cret-tok, base64 derived by hand
	if authHeader("s3cret-tok") != "Authorization: Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0LXRvaw==" {
		t.Fatalf("authHeader = %q", authHeader("s3cret-tok"))
	}
}

// The clone init container (MODE=clone) alone holds the read token, on the
// task's repository and the bot's fork: it clones the repository into the
// workspace, fetches a resumed session's branch head from the fork and a
// split's pull request head from the repository with that same token, and
// leaves no token in the clone's config. A failed
// clone exits 0 and leaves its failure, scrubbed of the token, for the
// worker container to report.
func TestCloneModeClones(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	backend := filepath.Join(run("--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("git-http-backend: %v", err)
	}
	root, seed := t.TempDir(), t.TempDir()
	remote, fork := filepath.Join(root, "repo.git"), filepath.Join(root, "fork.git")
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	head := run("-C", seed, "rev-parse", "HEAD")
	run("-C", seed, "checkout", "-q", "-b", "factory/t1")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "work in progress")
	run("-C", seed, "push", "-q", fork, "factory/t1")
	wip := run("-C", seed, "rev-parse", "HEAD")

	var refused int
	git := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic auth for x-access-token:r3ad-tok, base64 derived by hand
		if r.Header.Get("Authorization") != "Basic eC1hY2Nlc3MtdG9rZW46cjNhZC10b2s=" {
			refused++
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		git.ServeHTTP(w, r)
	}))
	defer srv.Close()

	for _, tc := range []struct{ name, sha string }{{"at the base", head}, {"resumed from the fork", wip}} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range map[string]string{"MODE": "clone", "REPO_URL": srv.URL + "/repo.git", "PUSH_URL": srv.URL + "/fork.git",
				"REPO_SHA": tc.sha, "BRANCH": "factory/t1", "REPO_TOKEN": "r3ad-tok"} {
				t.Setenv(k, v)
			}
			ws := t.TempDir()
			if code := runClone(nolog, ws); code != 0 {
				t.Fatalf("clone exit %d", code)
			}
			if err := fetched(ws); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(ws, "repo")
			if got := run("-C", repo, "rev-parse", "HEAD"); got != head {
				t.Fatalf("HEAD = %s, want the remote's HEAD %s", got, head)
			}
			if got := run("-C", repo, "cat-file", "-t", tc.sha); got != "commit" {
				t.Fatalf("%s is a %q in the clone, want the session's start commit", tc.sha, got)
			}
			cfg, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"r3ad-tok", "cjNhZC10b2s", "extraHeader", "x-access-token"} {
				if strings.Contains(string(cfg), secret) {
					t.Fatalf(".git/config holds %s:\n%s", secret, cfg)
				}
			}
		})
	}
	// a split's session: the pull request's head, which Forgejo keeps in
	// the repository as refs/pull/7/head, is the clone's branch pull/7
	run("-C", seed, "push", "-q", remote, "factory/t1:refs/pull/7/head")
	for k, v := range map[string]string{"MODE": "clone", "REPO_URL": srv.URL + "/repo.git", "PUSH_URL": "", "PULL": "7",
		"REPO_SHA": head, "BRANCH": "factory/t1", "REPO_TOKEN": "r3ad-tok"} {
		t.Setenv(k, v)
	}
	ws := t.TempDir()
	if code := runClone(nolog, ws); code != 0 || fetched(ws) != nil {
		t.Fatalf("clone exit %d: %v", code, fetched(ws))
	}
	if got := run("-C", filepath.Join(ws, "repo"), "rev-parse", "pull/7"); got != wip {
		t.Fatalf("pull/7 = %s, want the pull request's head %s", got, wip)
	}
	if refused != 0 {
		t.Fatalf("%d requests without the read token", refused)
	}
	t.Setenv("PULL", "")

	t.Run("failed", func(t *testing.T) {
		for k, v := range map[string]string{"MODE": "clone", "REPO_URL": strings.Replace(srv.URL, "http://", "http://x-access-token:r3ad-tok@", 1) + "/missing.git",
			"REPO_SHA": head, "BRANCH": "factory/t1", "REPO_TOKEN": "r3ad-tok"} {
			t.Setenv(k, v)
		}
		ws := t.TempDir()
		if code := runClone(nolog, ws); code != 0 {
			t.Fatalf("clone exit %d, want 0: the worker reports the failure", code)
		}
		err := fetched(ws)
		if err == nil || !strings.Contains(err.Error(), "missing.git") || strings.Contains(err.Error(), "r3ad-tok") {
			t.Fatalf("cloned = %v, want the clone's failure without the token", err)
		}
	})
}

// The clone init container tries once and leaves its failure for the
// session's start: one on the way to Forgejo, a 5xx or a network error, is
// transient, which the session activity's retry answers; a repository,
// ref or commit that is missing is not, and goes to a human at once.
func TestCloneModeLeavesATransientFailureForTheRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root, seed := t.TempDir(), t.TempDir()
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, filepath.Join(root, "repo.git"))
	head := run("-C", seed, "rev-parse", "HEAD")
	git := &cgi.Handler{Path: filepath.Join(run("--exec-path"), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	for name, tc := range map[string]struct {
		down      bool   // Forgejo answers 502
		repo      string // the repository asked for
		failed    string // in the failure the session reads
		transient bool
	}{
		"Forgejo down":      {down: true, repo: "repo.git", failed: "The requested URL returned error: 502", transient: true},
		"a missing project": {repo: "missing.git", failed: "not found"},
	} {
		t.Run(name, func(t *testing.T) {
			tries := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/info/refs") {
					tries++
				}
				if tc.down {
					http.Error(w, "upstream down", http.StatusBadGateway)
					return
				}
				git.ServeHTTP(w, r)
			}))
			defer srv.Close()
			for k, v := range map[string]string{"MODE": "clone", "REPO_URL": srv.URL + "/" + tc.repo, "REPO_SHA": head, "BRANCH": "factory/t1", "REPO_TOKEN": "r3ad-tok"} {
				t.Setenv(k, v)
			}
			ws := t.TempDir()
			if code := runClone(nolog, ws); code != 0 {
				t.Fatalf("clone exit %d, want 0: the worker reports the failure", code)
			}
			err := fetched(ws)
			if tries != 1 || err == nil || !strings.Contains(err.Error(), tc.failed) || errors.As(err, new(transient)) != tc.transient {
				t.Fatalf("%d clones, fetched = %v; want one clone and a failure with %q, transient %v", tries, err, tc.failed, tc.transient)
			}
		})
	}
}

// Git's reports of a fetch that failed on the way to Forgejo, a 5xx or a
// network error, are transient; a refused request (4xx) and a repository,
// ref or commit that is missing are not.
func TestTransientFetchFailures(t *testing.T) {
	const clone = "git clone -q http://forgejo:3000/sf/repo.git /workspace/repo: exit status 128: "
	for text, want := range map[string]bool{
		clone + "fatal: unable to access 'http://forgejo:3000/sf/repo.git/': The requested URL returned error: 502":                                                                                                     true,
		clone + "error: RPC failed; HTTP 500 curl 22 The requested URL returned error: 500\nfatal: expected flush after ref listing":                                                                                    true,
		clone + "fatal: unable to access 'http://forgejo:3000/sf/repo.git/': Failed to connect to forgejo port 3000 after 2 ms: Couldn't connect to server":                                                             true,
		clone + "fatal: unable to access 'http://forgejo:3000/sf/repo.git/': Could not resolve host: forgejo":                                                                                                           true,
		clone + "error: RPC failed; curl 56 Recv failure: Connection reset by peer\nfatal: early EOF":                                                                                                                   true,
		clone + "fatal: the remote end hung up unexpectedly":                                                                                                                                                            true,
		clone + "remote: Not found.\nfatal: repository 'http://forgejo:3000/sf/repo.git/' not found":                                                                                                                    false,
		clone + "fatal: unable to access 'http://forgejo:3000/sf/repo.git/': The requested URL returned error: 403":                                                                                                     false,
		"the start abc123 is on no branch the session can fetch: git fetch -q http://forgejo:3000/factory-agent/repo.git refs/heads/factory/t1: exit status 128: fatal: couldn't find remote ref refs/heads/factory/t1": false,
		"pull request #8's head does not hold the start abc123, and the fork's branch factory/t1 is gone":                                                                                                               false,
		"REPO_URL is required": false,
	} {
		if got := transientFetch(errors.New(text)); got != want {
			t.Errorf("transientFetch(%q) = %v, want %v", text, got, want)
		}
	}
}

// A session whose start fails before the harness runs leaves a result
// naming the failed step, in Step and in its summary, and the step's
// error, whatever the step: the card reads why, not a pod that exited
// without a result. The init checks are the first step; the clone's
// failure is the text its init container left in the workspace; an error
// too long for factoryd's 1 MiB log line is cut with its size. A sandbox
// more open than its policy fails the egress probe, before the clone is
// read; a gateway that does not answer it, as while it restarts, fails it
// as transient.
func TestSessionStartFailureLeavesAResult(t *testing.T) {
	const cloneErr = "git clone -q http://forgejo/sf/repo.git /workspace/repo: exit status 128: fatal: repository 'http://forgejo/sf/repo.git/' not found"
	const cloneDown = "git clone -q http://forgejo/sf/repo.git /workspace/repo: exit status 128: fatal: unable to access 'http://forgejo/sf/repo.git/': The requested URL returned error: 502"
	huge := strings.Repeat("\x01", 2<<20)
	closed := closedSandbox(t)
	open := closed
	open.directIP = listening(t)
	silent := closed
	silent.gateway = restarting(t)
	for name, tc := range map[string]struct {
		initErr                         error
		probe                           egressProbe
		taskID, fetchFailed, step, want string
		transient                       bool
	}{
		"init": {initErr: errors.New("must run as the container's init (pid 1), runs as pid 7"), taskID: "t1",
			step: "init", want: "start failed: init: must run as the container's init (pid 1), runs as pid 7"},
		"environment": {step: "environment", want: "start failed: environment: TASK_ID is required"},
		"egress probe": {taskID: "t1", probe: open, fetchFailed: cloneErr, step: "egress probe",
			want: "start failed: egress probe: direct IP " + open.directIP + " answered: connected to " + open.directIP},
		"egress probe, the gateway restarting": {taskID: "t1", probe: silent, step: "egress probe", transient: true,
			want: "start failed: egress probe: gateway: " + silent.gateway + "/healthz answered 503 Service Unavailable"},
		"clone": {taskID: "t1", probe: closed, fetchFailed: cloneErr, step: "clone", want: "start failed: clone: " + cloneErr},
		"clone, Forgejo down": {taskID: "t1", probe: closed, fetchFailed: cloneDown, step: "clone", transient: true,
			want: "start failed: clone: " + cloneDown},
		"prepare repo": {taskID: "t1", probe: closed, step: "prepare repo", want: "start failed: prepare repo: open WS/repo/.git/info/exclude: no such file or directory"},
		// "clone: " and 2 MiB, cut at 16 KiB
		"huge clone error": {taskID: "t1", probe: closed, fetchFailed: huge, step: "clone",
			want: "start failed: clone: " + huge[:16384-7] + "… (cut from 2097159 bytes)"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("TASK_ID", tc.taskID)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			home := t.TempDir()
			t.Setenv("HOME", home)
			// the restored opencode config: the baked one exists in the image only
			if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o700); err != nil {
				t.Fatal(err)
			}
			ws := t.TempDir()
			if tc.fetchFailed != "" {
				if err := os.WriteFile(fetchFailed(ws), []byte(tc.fetchFailed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if code := runSession(context.Background(), nolog, ws, &out, tc.initErr, func(workerEnv) egressProbe { return tc.probe }); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			want := harness.Result{Summary: strings.Replace(tc.want, "WS", ws, 1), Step: tc.step, Transient: tc.transient}
			var got harness.WorkerEnvelope
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != 1 || len(lines[0]) >= 1<<20 || json.Unmarshal([]byte(lines[0]), &got) != nil ||
				got.Type != "result" || got.Result == nil || *got.Result != want {
				t.Fatalf("stdout = %.300q (%d bytes), want the one result %.300q", out.String(), out.Len(), fmt.Sprintf("%+v", want))
			}
		})
	}
}

// The worker pushes one commit of its own on the session's start commit:
// the agent's work, committed or not, is its tree; the agent's own commits
// and their messages never reach the branch, and the subject, the issue's
// title, is sanitised. A resumed session's start is the work in progress
// an earlier session pushed, which stays the parent. The commit's trailers
// name the agent's role, the task, the session and the issue author
// behind it (roadmap 41), as git reads them back; a trailer the agent
// wrote into its own commit is gone.
func TestCommitAndPushSquashesOnTheStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	sha := run("-C", seed, "rev-parse", "HEAD")
	run("-C", seed, "checkout", "-q", "-b", "factory/resumed")
	os.WriteFile(filepath.Join(seed, "wip.txt"), []byte("wip\n"), 0o644)
	run("-C", seed, "add", "wip.txt")
	run("-C", seed, "-c", "user.name=factory-agent", "-c", "user.email=factory-agent@factory.local", "commit", "-q", "-m", "factory task t1: work in progress")
	// each resumed case pushes onto its own branch at the work in progress
	run("-C", seed, "push", "-q", fork, "factory/resumed", "factory/resumed:factory/agent-orphaned-its-history")
	wip := run("-C", seed, "rev-parse", "HEAD")

	agentCommits := func(dir string) {
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
		run("-C", dir, "add", "a.txt")
		run("-C", dir, "-c", "user.name=mallory", "-c", "user.email=m@m", "commit", "-q", "-m", "fixes #3 @admin")
		os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
		run("-C", dir, "add", "b.txt")
		run("-C", dir, "-c", "user.name=mallory", "-c", "user.email=m@m", "commit", "-q", "-m", "closes #4", "-m", "Requested-by: mallory")
	}
	for _, tc := range []struct {
		name, start string
		agent       func(dir string)
		files       string // the pushed tree's files; "" = nothing pushed
	}{
		{name: "no changes", start: sha, agent: func(string) {}},
		{name: "agent committed", start: sha, agent: agentCommits, files: "a.txt\nb.txt"},
		{name: "agent committed and left changes", start: sha, files: "a.txt\nb.txt\nc.txt", agent: func(dir string) {
			agentCommits(dir)
			os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644)
		}},
		{name: "resumed", start: wip, agent: agentCommits, files: "a.txt\nb.txt\nwip.txt"},
		{name: "agent orphaned its history", start: wip, files: "a.txt\nb.txt\nwip.txt", agent: func(dir string) {
			run("-C", dir, "checkout", "-q", "--orphan", "other")
			agentCommits(dir)
			run("-C", dir, "branch", "-q", "-D", "factory/agent-orphaned-its-history")
			run("-C", dir, "reflog", "expire", "--expire=now", "--all")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/" + strings.ReplaceAll(tc.name, " ", "-")
			e := workerEnv{taskID: "t1", sessionID: "s-1", author: "alice", objective: "Fixes #3 for @admin\nbody", repoSHA: tc.start, branch: branch, repoURL: remote, pushURL: fork}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := cloneBase(e, dir); err != nil {
				t.Fatal(err)
			}
			base, mainHead, err := prepareRepo(dir, e)
			if err != nil || base != tc.start {
				t.Fatalf("base = %s (%v), want %s", base, err, tc.start)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
			if err != nil {
				t.Fatal(err)
			}
			if out, _ := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output(); len(out) != 0 {
				t.Fatalf("branch %s pushed to the base repository", branch)
			}
			out, _ := exec.Command("git", "-C", fork, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
			remoteHead := strings.TrimSpace(string(out))
			if tc.files == "" {
				if commit != "" || remoteHead != "" {
					t.Fatalf("commit = %q, remote branch = %q, want nothing pushed", commit, remoteHead)
				}
				return
			}
			if commit == "" || remoteHead != commit {
				t.Fatalf("commit = %q, remote branch = %q, want the pushed commit", commit, remoteHead)
			}
			if parent := run("-C", fork, "rev-parse", commit+"^"); parent != tc.start {
				t.Fatalf("parent = %s, want the session's start %s", parent, tc.start)
			}
			if got := run("-C", fork, "log", "--format=%an <%ae>|%B", tc.start+".."+commit); got != "factory-agent <factory-agent@factory.local>|factory task t1: (Fixes) #3 for ＠admin\n\n"+
				"Factory-Role: agent\nFactory-Task: t1\nFactory-Session: s-1\nRequested-by: alice" {
				t.Fatalf("pushed history = %q, want one factory-agent commit with the sanitised subject and the trailers", got)
			}
			if got := run("-C", fork, "log", "-1", "--format=%(trailers)", commit); got != "Factory-Role: agent\nFactory-Task: t1\nFactory-Session: s-1\nRequested-by: alice" {
				t.Fatalf("trailers = %q, want the agent's role, the task, the session and the issue author", got)
			}
			if got := run("-C", fork, "ls-tree", "--name-only", commit); got != tc.files {
				t.Fatalf("pushed files = %q, want %q", got, tc.files)
			}
		})
	}
}

// An agent that pulls main mid-session (merge or rebase) must not push
// main's newer commits as its own: the pushed commit sits on the newest main
// commit its HEAD holds, next to a resumed session's work in progress, so
// the pull request's diff is the agent's change only. Main's change to
// .forgejo/ comes with the pull and is not the agent's. Main is the
// clone's origin/HEAD, recorded before the agent starts: the worker
// container holds no read token to fetch it again.
func TestCommitAndPushKeepsPulledMainOutOfTheChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	human := []string{"-c", "user.name=Human", "-c", "user.email=h@h"}
	agentID := []string{"-c", "user.name=mallory", "-c", "user.email=m@m"}
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, ".forgejo", "workflows"), 0o755)
	os.WriteFile(filepath.Join(seed, ".forgejo", "workflows", "pr.yml"), []byte("v1\n"), 0o644)
	run("-C", seed, "add", "-A")
	run(append([]string{"-C", seed}, append(human, "commit", "-q", "-m", "init")...)...)
	m0 := run("-C", seed, "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	run("-C", seed, "checkout", "-q", "-b", "wip")
	os.WriteFile(filepath.Join(seed, "wip.txt"), []byte("wip\n"), 0o644)
	run("-C", seed, "add", "wip.txt")
	run(append([]string{"-C", seed}, append(human, "commit", "-q", "-m", "factory task t1: work in progress")...)...)
	wip := run("-C", seed, "rev-parse", "HEAD")
	run("-C", seed, "push", "-q", fork, "wip:factory/resumed-merged-main")
	run("-C", seed, "push", "-q", fork, "wip:factory/resumed-merged-main-resolved-to-main's-tree")
	run("-C", seed, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(seed, ".forgejo", "workflows", "pr.yml"), []byte("v2\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "main.txt"), []byte("main\n"), 0o644)
	run("-C", seed, "add", "-A")
	run(append([]string{"-C", seed}, append(human, "commit", "-q", "-m", "main moves on")...)...)
	m1 := run("-C", seed, "rev-parse", "HEAD")
	run("-C", seed, "push", "-q", remote, "main")

	change := func(dir string) {
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
		run("-C", dir, "add", "a.txt")
		run(append([]string{"-C", dir}, append(agentID, "commit", "-q", "-m", "fix")...)...)
	}
	pull := func(dir string) {
		run("-C", dir, "fetch", "-q", "origin", "main")
		run(append([]string{"-C", dir}, append(agentID, "merge", "-q", "--no-edit", "FETCH_HEAD")...)...)
	}
	for _, tc := range []struct {
		name, start string
		agent       func(dir string)
		parents     string // the pushed commit's parents; "" = nothing pushed
		diff        string // files the commit changes against m1
		refused     bool
	}{
		{name: "merged main", start: m0, parents: m1, diff: "a.txt", agent: func(dir string) { change(dir); pull(dir) }},
		{name: "rebased onto main", start: m0, parents: m1, diff: "a.txt", agent: func(dir string) {
			change(dir)
			run("-C", dir, "fetch", "-q", "origin", "main")
			run(append([]string{"-C", dir}, append(agentID, "rebase", "-q", "FETCH_HEAD")...)...)
		}},
		{name: "resumed merged main", start: wip, parents: wip + " " + m1, diff: "a.txt\nwip.txt", agent: func(dir string) { pull(dir); change(dir) }},
		{name: "resumed merged main resolved to main's tree", start: wip, parents: wip + " " + m1, diff: "", agent: func(dir string) {
			pull(dir)
			os.Remove(filepath.Join(dir, "wip.txt"))
		}},
		{name: "only pulled main", start: m0, agent: pull},
		{name: "pulled main and undid its workflow", start: m0, refused: true, agent: func(dir string) {
			pull(dir)
			os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "pr.yml"), []byte("v1\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/" + strings.ReplaceAll(tc.name, " ", "-")
			dir := filepath.Join(t.TempDir(), "repo")
			if err := cloneBase(workerEnv{repoSHA: tc.start, branch: branch, repoURL: remote, pushURL: fork}, dir); err != nil {
				t.Fatal(err)
			}
			// the worker container's: no repository URL, no read token
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: tc.start, branch: branch, pushURL: fork}
			base, mainHead, err := prepareRepo(dir, e)
			if err != nil {
				t.Fatal(err)
			}
			if mainHead != m1 {
				t.Fatalf("main = %s, want the clone's origin/HEAD %s", mainHead, m1)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "refusing to push: .forgejo/") {
					t.Fatalf("commitAndPush = %q, %v; want the workflow refusal", commit, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.parents == "" {
				if commit != "" {
					t.Fatalf("commit = %q, want nothing pushed: main's commits are not the agent's change", commit)
				}
				return
			}
			if commit == "" {
				t.Fatalf("commit = %q, want the pushed merge onto %s", commit, tc.parents)
			}
			if got := run("-C", fork, "rev-list", "--parents", "-n", "1", commit); got != commit+" "+tc.parents {
				t.Fatalf("commit and parents = %q, want %q", got, commit+" "+tc.parents)
			}
			if got := run("-C", fork, "diff", "--name-only", m1, commit); got != tc.diff {
				t.Fatalf("change against main = %q, want %q", got, tc.diff)
			}
		})
	}
}

// The work guide asks an agent to write its reproduction as a scratch test
// named zz_repro_*_test.go; that file is the agent's evidence, not the
// change. The worker commits the agent's work tree, so it must leave the
// scratch file out of the pushed commit, and push nothing when the only
// change is a scratch file.
func TestCommitAndPushLeavesScratchReproTestsOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, "internal", "x"), 0o755)
	os.WriteFile(filepath.Join(seed, "internal", "x", "fix.go"), []byte("package x\n"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "init")
	sha := run("-C", seed, "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	fork := filepath.Join(t.TempDir(), "fork.git")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)

	for _, tc := range []struct {
		name       string
		agent      func(dir string)
		wantPushed bool
	}{
		{name: "fix-and-scratch", wantPushed: true, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "internal", "x", "fix.go"), []byte("package x\n\nfunc F() {}\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "internal", "x", "zz_repro_fix_test.go"), []byte("package x\n"), 0o644)
		}},
		{name: "scratch-only", agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "internal", "x", "zz_repro_fix_test.go"), []byte("package x\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/" + tc.name
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch, repoURL: remote, pushURL: fork}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := cloneBase(e, dir); err != nil {
				t.Fatal(err)
			}
			base, mainHead, err := prepareRepo(dir, e)
			if err != nil {
				t.Fatal(err)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantPushed {
				if commit != "" {
					t.Fatalf("commit = %q, want nothing pushed for a scratch-only tree", commit)
				}
				return
			}
			if commit == "" {
				t.Fatal("nothing pushed, want the fix")
			}
			tree := run("-C", fork, "ls-tree", "-r", "--name-only", commit)
			if !strings.Contains(tree, "internal/x/fix.go") {
				t.Fatalf("pushed tree lost the fix:\n%s", tree)
			}
			if strings.Contains(tree, "zz_repro") {
				t.Fatalf("pushed tree carries the scratch repro test:\n%s", tree)
			}
		})
	}
}

// An agent that plants every git-driven program it can must see none of
// them run while the worker commits and pushes, and the token must still
// reach the server on the push.
func TestCommitAndPushIgnoresAgentPlantedGitPrograms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	backend := filepath.Join(run("--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("git-http-backend: %v", err)
	}

	root := t.TempDir()
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	remote := filepath.Join(root, "repo.git")
	run("clone", "-q", "--bare", seed, remote)
	run("-C", remote, "config", "http.receivepack", "true")
	sha := run("-C", seed, "rev-parse", "HEAD")

	e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: "factory/t1", repoToken: "r3ad-tok"}
	var pushes, refused, spyHeaders int
	git := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic auth for x-access-token:r3ad-tok on reads and
		// x-access-token:s3cret-tok on the push, base64 derived by hand
		want := "Basic eC1hY2Nlc3MtdG9rZW46cjNhZC10b2s="
		if strings.Contains(r.URL.RequestURI(), "git-receive-pack") {
			want = "Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0LXRvaw=="
		}
		if r.Header.Get("Authorization") != want {
			refused++
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			pushes++
		}
		if r.Header.Get("X-Spy") != "" {
			spyHeaders++
		}
		git.ServeHTTP(w, r)
	}))
	defer srv.Close()
	e.repoURL = srv.URL + "/repo.git"
	e.pushURL = e.repoURL

	dir := filepath.Join(t.TempDir(), "repo")
	if err := cloneBase(e, dir); err != nil {
		t.Fatal(err)
	}
	// the worker container's: no repository URL, no read token
	e.repoURL, e.repoToken = "", ""
	base, mainHead, err := prepareRepo(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("-C", dir, "add", "a.txt")
	run("-C", dir, "commit", "-q", "-m", "agent")

	// every planted program appends its environment to spy.log
	spyLog := filepath.Join(t.TempDir(), "spy.log")
	spy := filepath.Join(t.TempDir(), "spy")
	os.WriteFile(spy, []byte("#!/bin/sh\nenv >> "+spyLog+"\ncat\n"), 0o755)
	hooks := filepath.Join(t.TempDir(), "hooks")
	os.Mkdir(hooks, 0o755)
	for _, h := range []string{"pre-commit", "commit-msg", "post-commit", "pre-push", "reference-transaction", "post-checkout", "post-index-change", "pre-auto-gc"} {
		for _, d := range []string{hooks, filepath.Join(dir, ".git", "hooks")} {
			os.WriteFile(filepath.Join(d, h), []byte("#!/bin/sh\n"+spy+" </dev/null\n"), 0o755)
		}
	}
	included := filepath.Join(t.TempDir(), "included.cfg")
	os.WriteFile(included, []byte("[core]\n\tfsmonitor = "+spy+"\n"), 0o644)
	planted := [][]string{
		{"core.hooksPath", hooks}, {"core.fsmonitor", spy}, {"core.sshCommand", spy},
		{"credential.helper", "!" + spy}, {"filter.spy.clean", spy}, {"filter.spy.smudge", spy},
		{"filter.spy.required", "true"}, {"diff.spy.textconv", spy}, {"include.path", included},
		{"url.http://127.0.0.1:1/.insteadOf", srv.URL + "/"}, {"remote.origin.pushurl", "http://127.0.0.1:1/x.git"},
	}
	for _, kv := range planted {
		run("-C", dir, "config", kv[0], kv[1])
		run("config", "--global", kv[0], kv[1])
	}
	if got := run("config", "--global", "core.fsmonitor"); got != spy {
		t.Fatalf("planted global config not read by plain git: %q", got)
	}
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=spy diff=spy\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)

	// git traces the effective hooksPath and fsmonitor of every process,
	// including the upload-pack side of the worker's fetch from the agent repo
	trace := filepath.Join(t.TempDir(), "trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	t.Setenv("GIT_TRACE2_CONFIG_PARAMS", "core.hooksPath,core.fsmonitor")

	// a git planted ahead on PATH, where the pod's agent-writable
	// /cache/go/bin sits, must not run the push either: the worker runs
	// the image's own binary
	plantedBin := filepath.Join(t.TempDir(), "bin")
	os.Mkdir(plantedBin, 0o755)
	os.WriteFile(filepath.Join(plantedBin, "git"), []byte("#!/bin/sh\nenv >> "+spyLog+"\nexec /usr/bin/git \"$@\"\n"), 0o755)
	t.Setenv("PATH", plantedBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	killed := false
	commit, err := commitAndPush(dir, e, base, mainHead, func() error {
		if pushes != 0 {
			t.Error("push ran before the agent was killed")
		}
		killed = true
		// the worker repo's config as a process running during the fetch could leave it
		own, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".push-*"))
		if len(own) != 1 {
			t.Fatalf("worker repos = %v, want one", own)
		}
		spyHeader := filepath.Join(t.TempDir(), "header.cfg")
		os.WriteFile(spyHeader, []byte("[http]\n\textraHeader = X-Spy: 1\n"), 0o644)
		cfg, err := os.OpenFile(filepath.Join(own[0], ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(cfg, "[core]\n\thooksPath = %s\n\tfsmonitor = %s\n\tsshCommand = %s\n"+
			"[credential]\n\thelper = !%s\n[http]\n\tproxy = http://127.0.0.1:1\n\textraHeader = X-Spy: 1\n"+
			"[include]\n\tpath = %s\n[url \"http://127.0.0.1:1/\"]\n\tinsteadOf = %s/\n",
			hooks, spy, spy, spy, spyHeader, srv.URL)
		cfg.Close()
		for _, h := range []string{"pre-push", "reference-transaction"} {
			os.WriteFile(filepath.Join(own[0], ".git", "hooks", h), []byte("#!/bin/sh\n"+spy+" </dev/null\n"), 0o755)
		}
		return nil
	}, fixedToken("s3cret-tok"))
	if err != nil {
		t.Fatal(err)
	}
	if !killed {
		t.Fatal("agent processes were not killed before the push")
	}
	if b, err := os.ReadFile(spyLog); err == nil {
		t.Fatalf("an agent-planted program ran (token in its env: %v)", strings.Contains(string(b), "czNjcmV0LXRvaw") || strings.Contains(string(b), "s3cret-tok"))
	}
	if pushes != 1 || refused != 0 || spyHeaders != 0 {
		t.Fatalf("pushes = %d, refused = %d, spy headers = %d, want one authenticated push", pushes, refused, spyHeaders)
	}
	// every git process, the fetch's pack-objects in the agent repo among
	// them, ends with hooks at /dev/null and fsmonitor off
	f, err := os.Open(trace)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	last := map[string]string{}
	packObjects := map[string]bool{}
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var ev struct {
			Event, Sid, Param, Value string
			Argv                     []string
		}
		json.Unmarshal(sc.Bytes(), &ev)
		switch ev.Event {
		case "start":
			if len(ev.Argv) > 1 && ev.Argv[1] == "pack-objects" {
				packObjects[ev.Sid] = true
			}
		case "def_param":
			last[ev.Sid+" "+ev.Param] = ev.Value
		}
	}
	if len(packObjects) == 0 {
		t.Fatal("trace shows no pack-objects: the fetch's upload-pack side was not traced")
	}
	for sid := range packObjects {
		if last[sid+" core.hookspath"] != "/dev/null" || last[sid+" core.fsmonitor"] != "false" {
			t.Fatalf("pack-objects under upload-pack: hooksPath = %q, fsmonitor = %q, want /dev/null and false",
				last[sid+" core.hookspath"], last[sid+" core.fsmonitor"])
		}
	}
	for k, v := range last {
		if strings.HasSuffix(k, " core.hookspath") && v != "/dev/null" || strings.HasSuffix(k, " core.fsmonitor") && v != "false" {
			t.Fatalf("git process %s = %q", k, v)
		}
	}
	if got := run("-C", remote, "rev-parse", "refs/heads/factory/t1"); got != commit {
		t.Fatalf("remote branch = %s, want %s", got, commit)
	}
	if run("-C", remote, "rev-parse", commit+"^") != base {
		t.Fatal("the pushed commit's parent is not the session's start")
	}
	if got := run("-C", remote, "log", "-1", "--format=%an <%ae>", commit); got != "factory-agent <factory-agent@factory.local>" {
		t.Fatalf("leftover commit author = %q", got)
	}
	if got := run("-C", remote, "ls-tree", "--name-only", commit); got != ".gitattributes\na.txt\nb.txt" {
		t.Fatalf("pushed tree = %q", got)
	}
	// control: the same plants do run under the agent's own git
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644)
	exec.Command("git", "-C", dir, "add", "-A").Run()
	if _, err := os.Stat(spyLog); err != nil {
		t.Fatalf("control: planted programs did not run under plain git: %v", err)
	}
}

// Forgejo's protected_file_patterns rejects every new factory/* branch
// outright, so branch protection cannot keep an agent from editing a
// workflow directory (rulings 90, 100): the worker itself must refuse the
// push, by comparing the tree entries of .forgejo, .gitea and .github,
// never by parsing a diff's path listing.
func TestCommitAndPushRefusesWorkflowDirChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, ".forgejo", "workflows"), 0o755)
	os.WriteFile(filepath.Join(seed, ".forgejo", "workflows", "pr.yml"), []byte("gate: true\n"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	sha := run("-C", seed, "rev-parse", "HEAD")

	commitAll := func(dir string) {
		run("-C", dir, "add", "-A")
		run("-C", dir, "commit", "-q", "-m", "agent")
	}
	write := func(dir string, rel ...string) {
		path := filepath.Join(append([]string{dir}, rel...)...)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("evil: true\n"), 0o644)
	}
	for _, tc := range []struct {
		name       string
		agent      func(dir string)
		wantRefuse string // the directory the refusal must name; "" = the push goes through
	}{
		{name: "agent-commit-edits-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			write(dir, ".forgejo", "workflows", "pr.yml")
			commitAll(dir)
		}},
		{name: "leftover-change-touches-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
			commitAll(dir)
			write(dir, ".forgejo", "config.yml")
		}},
		{name: "non-ascii-name-under-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			write(dir, ".forgejo", "workflows", "b\u00f6se \"pr\".yml")
			commitAll(dir)
		}},
		{name: "rename-out-of-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			run("-C", dir, "mv", ".forgejo/workflows/pr.yml", "pr.yml")
			commitAll(dir)
		}},
		{name: "new-github-workflow", wantRefuse: ".github", agent: func(dir string) {
			write(dir, ".github", "workflows", "x.yml")
			commitAll(dir)
		}},
		{name: "new-gitea-workflow", wantRefuse: ".gitea", agent: func(dir string) {
			write(dir, ".gitea", "workflows", "x.yml")
			commitAll(dir)
		}},
		{name: "case-variant-beside-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			write(dir, ".FORGEJO", "workflows", "x.yml")
			commitAll(dir)
		}},
		{name: "symlink-replaces-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			run("-C", dir, "mv", ".forgejo", "ci")
			os.Symlink("ci", filepath.Join(dir, ".forgejo"))
			commitAll(dir)
		}},
		{name: "write-through-a-symlink-into-forgejo", wantRefuse: ".forgejo", agent: func(dir string) {
			os.Symlink(".forgejo", filepath.Join(dir, "ci"))
			write(dir, "ci", "workflows", "pr.yml")
			commitAll(dir)
		}},
		{name: "lookalike-path-is-not-forgejo", agent: func(dir string) {
			write(dir, ".forgejo-lookalike", "x")
			commitAll(dir)
		}},
		{name: "unrelated-change-passes", agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
			commitAll(dir)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/" + tc.name
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch, repoURL: remote, pushURL: remote}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := cloneBase(e, dir); err != nil {
				t.Fatal(err)
			}
			base, mainHead, err := prepareRepo(dir, e)
			if err != nil {
				t.Fatal(err)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
			remoteHead, _ := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
			if tc.wantRefuse != "" {
				if err == nil {
					t.Fatalf("commitAndPush succeeded, commit = %q, want refusal", commit)
				}
				if !strings.Contains(err.Error(), tc.wantRefuse+"/") {
					t.Fatalf("error = %q, want it to name %s/", err, tc.wantRefuse)
				}
				if len(remoteHead) != 0 {
					t.Fatalf("remote branch = %q, want nothing pushed", remoteHead)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if commit == "" || strings.TrimSpace(string(remoteHead)) != commit {
				t.Fatalf("commit = %q, remote branch = %q, want the pushed HEAD", commit, remoteHead)
			}
		})
	}
}

// The guide's migration-name rule became a guard here (roadmap 186): the
// worker, trusted and running with the agent dead, refuses a push that
// adds a .sql file under a migration directory glob (a glob with ** in
// it) whose name is off the timestamped pattern. The refusal reaches the
// next session as a finding, like every no-push. An exact path glob
// (internal/policy/schema.sql) marks one file for review, not a
// directory of timestamped migrations, so it judges no name; an edited
// migration keeps its name.
func TestCommitAndPushRefusesMisnamedMigrations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	seed := t.TempDir()
	run("init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, "internal", "policy", "migrations"), 0o755)
	os.WriteFile(filepath.Join(seed, "internal", "policy", "migrations", "20260102030405_seed.sql"), []byte("create table x;\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "internal", "policy", "migrations", "0016_legacy.sql"), []byte("alter table x;\n"), 0o644)
	run("-C", seed, "add", "-A")
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	sha := run("-C", seed, "rev-parse", "HEAD")

	globs := []string{"internal/policy/migrations/**", "internal/policy/schema.sql"}
	commitAll := func(dir string) {
		run("-C", dir, "add", "-A")
		run("-C", dir, "commit", "-q", "-m", "agent")
	}
	add := func(dir string, content string, rel ...string) {
		path := filepath.Join(append([]string{dir}, rel...)...)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	migrations := func(dir, name string) { add(dir, "select 1;\n", "internal", "policy", "migrations", name) }
	for _, tc := range []struct {
		name       string
		globs      []string
		agent      func(dir string)
		wantRefuse string // the path the refusal must name; "" = the push goes through
	}{
		{name: "next-number-refused", globs: globs, wantRefuse: "internal/policy/migrations/0017_add_users.sql", agent: func(dir string) {
			migrations(dir, "0017_add_users.sql")
			commitAll(dir)
		}},
		{name: "dashes-and-short-stamp-refused", globs: globs, wantRefuse: "internal/policy/migrations/20261012_add-users.sql", agent: func(dir string) {
			migrations(dir, "20261012_add-users.sql")
			commitAll(dir)
		}},
		{name: "timestamped-name-passes", globs: globs, agent: func(dir string) {
			migrations(dir, "20261012090000_add_users.sql")
			commitAll(dir)
		}},
		{name: "edited-legacy-migration-passes", globs: globs, agent: func(dir string) {
			add(dir, "alter table x, y;\n", "internal", "policy", "migrations", "0016_legacy.sql")
			commitAll(dir)
		}},
		{name: "exact-glob-judges-no-name", globs: globs, agent: func(dir string) {
			add(dir, "create table z;\n", "internal", "policy", "schema.sql")
			commitAll(dir)
		}},
		{name: "non-sql-under-globs-passes", globs: globs, agent: func(dir string) {
			add(dir, "the pattern\n", "internal", "policy", "migrations", "README.md")
			commitAll(dir)
		}},
		{name: "outside-globs-passes", globs: globs, agent: func(dir string) {
			add(dir, "select 1;\n", "db", "0017_add_users.sql")
			commitAll(dir)
		}},
		{name: "rename-into-bad-name-refused", globs: globs, wantRefuse: "internal/policy/migrations/0017_nextnum.sql", agent: func(dir string) {
			run("-C", dir, "mv", "internal/policy/migrations/0016_legacy.sql", "internal/policy/migrations/0017_nextnum.sql")
			commitAll(dir)
		}},
		{name: "non-ascii-name-refused", globs: globs, wantRefuse: "internal/policy/migrations/0017_café.sql", agent: func(dir string) {
			migrations(dir, "0017_café.sql")
			commitAll(dir)
		}},
		{name: "no-globs-passes", agent: func(dir string) {
			migrations(dir, "0017_add_users.sql")
			commitAll(dir)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/mig-" + tc.name
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch, repoURL: remote, pushURL: remote, migrationGlobs: tc.globs}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := cloneBase(e, dir); err != nil {
				t.Fatal(err)
			}
			base, mainHead, err := prepareRepo(dir, e)
			if err != nil {
				t.Fatal(err)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, mainHead, func() error { return nil }, fixedToken("t"))
			remoteHead, _ := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
			if tc.wantRefuse != "" {
				if err == nil {
					t.Fatalf("commitAndPush succeeded, commit = %q, want refusal", commit)
				}
				if !strings.Contains(err.Error(), tc.wantRefuse) {
					t.Fatalf("error = %q, want it to name %q", err, tc.wantRefuse)
				}
				if len(remoteHead) != 0 {
					t.Fatalf("remote branch = %q, want nothing pushed", remoteHead)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if commit == "" || strings.TrimSpace(string(remoteHead)) != commit {
				t.Fatalf("commit = %q, remote branch = %q, want the pushed HEAD", commit, remoteHead)
			}
		})
	}
}

// MIGRATION_GLOBS reaches the worker as a JSON array like FINDINGS; a
// malformed value must fail loudly, not silently guard nothing.
func TestLoadEnvParsesMigrationGlobs(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MIGRATION_GLOBS", `["internal/policy/migrations/**","internal/policy/schema.sql"]`)
	e, err := loadEnv()
	if err != nil || !slices.Equal(e.migrationGlobs, []string{"internal/policy/migrations/**", "internal/policy/schema.sql"}) {
		t.Fatalf("migrationGlobs = %v, %v; want the two globs", e.migrationGlobs, err)
	}
	setRequiredEnv(t) // loadEnv unsets the tokens
	t.Setenv("MIGRATION_GLOBS", "not json")
	if _, err := loadEnv(); err == nil {
		t.Fatal("want an error for malformed MIGRATION_GLOBS, got nil")
	}
}

func TestCheckpointMeasuresTheAgentDiffWithoutRunningAgentPrograms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, dir := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	run("init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("1\n2\n3\n"), 0o644)
	run("-C", seed, "add", "a.txt")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "base")
	run("clone", "-q", seed, dir)
	e := workerEnv{taskID: "t1", branch: "factory/t1"}
	base, _, err := prepareRepo(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	ck, err := newCheckpointer(dir, e, base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if stat, _, err := ck.diff(ctx); err != nil || stat != (diffStat{}) {
		t.Fatalf("untouched tree: stat = %+v, err = %v, want an empty diff", stat, err)
	}

	// the agent edits a line, adds a file (untracked), and the adapter's brief lands
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1\nX\n3\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("n1\nn2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "brief.json"), []byte("{}\n"), 0o600)
	stat, patch, err := ck.diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stat != (diffStat{Files: 2, Insertions: 3, Deletions: 1}) {
		t.Fatalf("stat = %+v, want 2 files, 3 insertions, 1 deletion", stat)
	}
	if !strings.Contains(patch, "+X") || !strings.Contains(patch, "+n2") || strings.Contains(patch, "brief.json") {
		t.Fatalf("patch = %q", patch)
	}

	// every program the agent can plant for git to run appends to spy.log
	spyLog := filepath.Join(t.TempDir(), "spy.log")
	spy := filepath.Join(t.TempDir(), "spy")
	os.WriteFile(spy, []byte("#!/bin/sh\necho ran >> "+spyLog+"\ncat\n"), 0o755)
	hooks := filepath.Join(t.TempDir(), "hooks")
	os.Mkdir(hooks, 0o755)
	for _, h := range []string{"post-index-change", "reference-transaction", "pre-auto-gc"} {
		os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\n"+spy+" </dev/null\n"), 0o755)
		os.WriteFile(filepath.Join(dir, ".git", "hooks", h), []byte("#!/bin/sh\n"+spy+" </dev/null\n"), 0o755)
	}
	for _, kv := range [][]string{
		{"core.hooksPath", hooks}, {"core.fsmonitor", spy}, {"filter.spy.clean", spy},
		{"filter.spy.required", "true"}, {"diff.spy.textconv", spy}, {"diff.external", spy},
	} {
		run("-C", dir, "config", kv[0], kv[1])
		run("config", "--global", kv[0], kv[1])
	}
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=spy diff=spy\n"), 0o644)
	// a program running between two checkpoints may rewrite the base store's config too
	cfg, err := os.OpenFile(filepath.Join(ck.store, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(cfg, "[core]\n\thooksPath = %s\n\tfsmonitor = %s\n[diff]\n\texternal = %s\n[filter \"spy\"]\n\tclean = %s\n", hooks, spy, spy, spy)
	cfg.Close()

	stat, _, err = ck.diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stat != (diffStat{Files: 3, Insertions: 4, Deletions: 1}) {
		t.Fatalf("stat = %+v, want 3 files, 4 insertions, 1 deletion", stat)
	}
	if _, err := os.Stat(spyLog); err == nil {
		t.Fatal("an agent-planted program ran during a checkpoint")
	}
	// control: the plants do run under plain git in the agent repo
	exec.Command("git", "-C", dir, "add", "-A").Run()
	if _, err := os.Stat(spyLog); err != nil {
		t.Fatalf("control: planted programs did not run under plain git: %v", err)
	}
}

func TestParseShortstat(t *testing.T) {
	for in, want := range map[string]diffStat{
		"":                                {},
		" 1 file changed, 1 insertion(+)": {Files: 1, Insertions: 1},
		" 1 file changed, 2 deletions(-)": {Files: 1, Deletions: 2},
		" 3 files changed, 40 insertions(+), 7 deletions(-)": {Files: 3, Insertions: 40, Deletions: 7},
	} {
		if got := parseShortstat(in); got != want {
			t.Fatalf("parseShortstat(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// TestCheckpointToleratesFileRemovedDuringStaging is the contract for plan
// #495 (roadmap 182): a file the agent deletes from the work tree while the
// checkpoint stages it must not drop the checkpoint. git add -A fails with
// exit status 128 on the vanished file; diff must tolerate that, use the
// index as-is, and carry the deletion in the stat and the patch.
//
// The staging steps are injectable through checkpointer.stage, and this test
// uses it: this git reads a tracked file's absence as its deletion and
// succeeds, so removing the file alone would never make add -A fail. The hook
// removes the file when diff runs git add -A, lets the real command stage the
// work tree as it is, and then makes only that command return the failure the
// agent's removal caused — exit status 128, fatal: unable to stat. Without
// the fix that error propagates and the checkpoint is dropped; with it diff
// carries the staged tree.
func TestCheckpointToleratesFileRemovedDuringStaging(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, dir := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	run("init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("1\n2\n3\n"), 0o644)
	run("-C", seed, "add", "a.txt")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "base")
	run("clone", "-q", seed, dir)
	e := workerEnv{taskID: "t1", branch: "factory/t1"}
	base, _, err := prepareRepo(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	ck, err := newCheckpointer(dir, e, base)
	if err != nil {
		t.Fatal(err)
	}

	addFailed, addStaged := 0, false
	ck.stage = func(ctx context.Context, d string, env []string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "add" && args[1] == "-A" {
			// the agent removes the committed file while git add -A works
			if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
				t.Fatalf("remove during staging: %v", err)
			}
			out, runErr := e.runGitCtx(ctx, d, env, args...)
			addStaged = runErr == nil
			// and the command reports the vanished file as Stage 17's did
			addFailed++
			return out, errors.New("git add -A: exit status 128: fatal: unable to stat 'a.txt'")
		}
		return e.runGitCtx(ctx, d, env, args...)
	}
	stat, patch, err := ck.diff(context.Background())
	if err != nil {
		t.Fatalf("diff with a vanished file: %v", err)
	}
	if addFailed != 1 || !addStaged {
		t.Fatalf("git add -A ran %d times, staged = %v; want once, staged", addFailed, addStaged)
	}
	if stat.Files < 1 || stat.Deletions < 1 {
		t.Fatalf("stat = %+v, want Files >= 1 and Deletions >= 1", stat)
	}
	if !strings.Contains(patch, "-1\n-2\n-3") || !strings.Contains(patch, "deleted file mode") {
		t.Fatalf("patch does not carry the deletion: %q", patch)
	}
}

func TestCheckpointLoopEmitsOnlyChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seed, dir := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", seed}, {"-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "base"},
		{"clone", "-q", seed, dir}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	e := workerEnv{branch: "b"}
	base, _, err := prepareRepo(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	ck, err := newCheckpointer(dir, e, base)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan harness.Event, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ck.loop(ctx, 20*time.Millisecond, func(ev harness.Event) { events <- ev }, t.Logf)
	}()
	time.Sleep(200 * time.Millisecond)
	if len(events) != 0 {
		t.Fatalf("%d checkpoints for an untouched tree, want none", len(events))
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\nb\n"), 0o644)
	var ev harness.Event
	select {
	case ev = <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("no checkpoint after a change")
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if len(events) != 0 {
		t.Fatalf("%d more checkpoints without a further change", len(events))
	}
	diff, _ := ev.Detail["diff"].(map[string]any)
	if ev.Kind != harness.EventCheckpoint || diff["files"] != 1 || diff["insertions"] != 2 || diff["deletions"] != 0 {
		t.Fatalf("checkpoint = %+v", ev)
	}
}

// Every session end pushes the work: a failed session's too (the
// provider's refusal mid-session, Stage 15's lost diff), its summary
// keeping the failure.
func TestDeliverPushesEverySessionsWork(t *testing.T) {
	var emitted []harness.Event
	emit := func(ev harness.Event) { emitted = append(emitted, ev) }
	pushes := 0
	push := func(commit string) func() (string, error) {
		return func() (string, error) { pushes++; return commit, nil }
	}
	const refused = "APIError: Repetitive tool calls detected in the conversation history (status 400)"
	res := deliver(harness.Result{Success: false, Summary: refused}, false, push("c1"), "factory/t1", emit)
	if res.Success || res.Summary != refused+"; work in progress pushed to factory/t1 at c1" || pushes != 1 {
		t.Fatalf("failed session: %+v after %d pushes, want its work pushed", res, pushes)
	}
	if len(emitted) != 1 || emitted[0].Kind != harness.EventPushed || emitted[0].Detail["commit"] != "c1" {
		t.Fatalf("events = %+v, want the pushed event", emitted)
	}
	if res := deliver(harness.Result{Success: false, Summary: "model refused"}, false, push(""), "factory/t1", emit); res.Summary != "model refused" || pushes != 2 {
		t.Fatalf("failed session without changes: %+v after %d pushes", res, pushes)
	}
	emitted = nil
	res = deliver(harness.Result{Success: false, Summary: "opencode exited: signal: killed"}, true, push("c-wip"), "factory/t1", emit)
	if res.Success || res.Summary != "stopped; work in progress pushed to factory/t1 at c-wip" || pushes != 3 {
		t.Fatalf("stopped session: %+v after %d pushes", res, pushes)
	}
	if len(emitted) != 1 || emitted[0].Kind != harness.EventPushed || emitted[0].Detail["commit"] != "c-wip" {
		t.Fatalf("events = %+v, want the pushed event", emitted)
	}
	if res := deliver(harness.Result{Success: false}, true, push(""), "factory/t1", emit); res.Summary != "stopped; no work in progress to push" {
		t.Fatalf("stopped without changes: %+v", res)
	}
	if res := deliver(harness.Result{Success: true, Summary: "done"}, false, push("c2"), "factory/t1", emit); !res.Success || res.Summary != "pushed branch factory/t1 at c2" {
		t.Fatalf("finished session: %+v", res)
	}
	failing := func() (string, error) { return "", fmt.Errorf("refusing to push: .forgejo/x changes .forgejo/") }
	emitted = nil
	if res := deliver(harness.Result{Success: true, Summary: "done"}, false, failing, "factory/t1", emit); res.Success || !strings.Contains(res.Summary, "push failed: refusing") {
		t.Fatalf("refused push: %+v", res)
	}
	// the session record must say the push failed, not end on the agent's "finished"
	if len(emitted) != 1 || emitted[0].Kind != harness.EventPushFailed || emitted[0].Detail["branch"] != "factory/t1" ||
		emitted[0].Detail["error"] != "refusing to push: .forgejo/x changes .forgejo/" {
		t.Fatalf("events = %+v, want one push_failed event with branch and error", emitted)
	}
	if res := deliver(harness.Result{Success: false, Summary: refused}, false, failing, "factory/t1", emit); res.Success ||
		res.Summary != refused+"; push failed: refusing to push: .forgejo/x changes .forgejo/" {
		t.Fatalf("failed session, refused push: %+v", res)
	}
}

func TestLoadEnvParsesTools(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TOOLS", `["request_info","web_search"]`)
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if len(e.tools) != 2 || e.tools[0] != "request_info" || e.tools[1] != "web_search" {
		t.Fatalf("tools = %v", e.tools)
	}
}

func TestLoadEnvParsesAnswers(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ANSWERS", `["sebastian: b", "sebastian: a"]`)
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if len(e.answers) != 2 || e.answers[0] != "sebastian: b" || e.answers[1] != "sebastian: a" {
		t.Fatalf("answers = %v", e.answers)
	}
	t.Setenv("ANSWERS", "not json")
	if _, err := loadEnv(); err == nil {
		t.Fatal("malformed ANSWERS accepted")
	}
}

// A report session (ruling 185) gets no push URL or token and needs none;
// its guide arrives as REPORT_GUIDE. A coding session still needs both.
func TestLoadEnvReportModeHasNoPushCredentials(t *testing.T) {
	// loadEnv unsets the tokens it read: each call gets them anew
	env := func(mode, guide string) {
		setRequiredEnv(t)
		t.Setenv("PUSH_URL", "")
		t.Setenv("MODE", mode)
		t.Setenv("REPORT_GUIDE", guide)
	}
	env("", "")
	if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "PUSH_") {
		t.Fatalf("coding session without push credentials: err = %v", err)
	}
	env("report", "write, do not code")
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if e.report != "write, do not code" || e.pushToken != "" {
		t.Fatalf("env = %+v", e)
	}
	env("report", "")
	if _, err := loadEnv(); err == nil || !strings.Contains(err.Error(), "REPORT_GUIDE") {
		t.Fatalf("report mode without a guide: err = %v", err)
	}
}

// fixedToken is factoryd answering every push request with tok at once.
func fixedToken(tok string) func(string) (string, error) {
	return func(string) (string, error) { return tok, nil }
}

// changedSession is a session's clone with one new file in the work tree,
// and the bare fork its push goes to.
func changedSession(t *testing.T) (dir string, e workerEnv, base, mainHead, fork string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	fork = filepath.Join(t.TempDir(), "fork.git")
	run("init", "-q", "-b", "main", seed)
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	run("clone", "-q", "--bare", remote, fork)
	e = workerEnv{taskID: "t1", objective: "o", repoSHA: run("-C", seed, "rev-parse", "HEAD"), branch: "factory/t1", repoURL: remote, pushURL: fork}
	dir = filepath.Join(t.TempDir(), "repo")
	if err := cloneBase(e, dir); err != nil {
		t.Fatal(err)
	}
	e.repoURL = ""
	var err error
	if base, mainHead, err = prepareRepo(dir, e); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, e, base, mainHead, fork
}

// A factoryd that never answers the push request (one that died without
// its shutdown, ruling 231) costs the session its push: the worker waits
// its bound, pushes nothing and says why.
func TestNoPushTokenNoPush(t *testing.T) {
	dir, e, base, mainHead, fork := changedSession(t)
	gate := newPushGate()
	var request bytes.Buffer
	var events []harness.Event
	res := deliver(harness.Result{Success: true, Summary: "done"}, false, func() (string, error) {
		return commitAndPush(dir, e, base, mainHead, func() error { return nil }, func(commit string) (string, error) {
			return gate.ask(commit, 50*time.Millisecond, &request)
		})
	}, e.branch, func(ev harness.Event) { events = append(events, ev) })
	if res.Success || res.Summary != "push failed: no push token from factoryd within 50ms" {
		t.Fatalf("result = %+v, want the push failed for want of a token", res)
	}
	if len(events) != 1 || events[0].Kind != harness.EventPushFailed {
		t.Fatalf("events = %+v, want one push_failed", events)
	}
	if !strings.HasPrefix(request.String(), `{"type":"push","push":{"commit":"`) {
		t.Fatalf("stdout = %q, want the push request", request.String())
	}
	if out, _ := exec.Command("git", "-C", fork, "rev-parse", "--verify", "-q", "refs/heads/factory/t1").Output(); len(out) != 0 {
		t.Fatalf("fork branch = %s, want nothing pushed", out)
	}
}

func TestLoadEnvReadsTheFacts(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FACTS", "factoryd's facts for this session: budget 3.20 of 5.00 EUR left")
	e, err := loadEnv()
	if err != nil || e.facts != "factoryd's facts for this session: budget 3.20 of 5.00 EUR left" {
		t.Fatalf("facts = %q, %v", e.facts, err)
	}
}

func TestLoadEnvReadsTheContextCap(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("CONTEXT_TOKENS", "100000")
	e, err := loadEnv()
	if err != nil || e.contextTokens != 100000 {
		t.Fatalf("contextTokens = %d, %v", e.contextTokens, err)
	}
	t.Setenv("CONTEXT_TOKENS", "lots")
	if _, err := loadEnv(); err == nil {
		t.Fatal("a malformed CONTEXT_TOKENS was accepted")
	}
}

// The writing guide rides only with a report session's REPORT_GUIDE: a
// coding session's env carries no guide at all (the question format's
// context rule), for its words reach no person.
func TestLoadEnvCodingSessionCarriesNoWritingGuide(t *testing.T) {
	setRequiredEnv(t)
	e, err := loadEnv()
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if e.report != "" {
		t.Fatalf("a coding session carries guide %q; work sessions get none", e.report)
	}
}
