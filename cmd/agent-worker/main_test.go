package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	for name, val := range map[string]string{
		"TASK_ID": "t1", "OBJECTIVE": "do it", "REPOSITORY": "sf/repo", "REPO_URL": "https://example/repo.git",
		"BRANCH": "factory/t1", "GIT_TOKEN": "tok", "GATEWAY_URL": "http://gw",
		"TOKEN_FILE": "/tmp/token", "MODEL": "openrouter/x", "ANSWER_TOKENS": "100", "HEADROOM_TOKENS": "10",
	} {
		t.Setenv(name, val)
	}
}

// A malformed FINDINGS must fail loudly, not silently become a first
// attempt with no findings (fix round 1).
func TestLoadEnvRejectsMalformedFindings(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FINDINGS", "not json")
	if _, err := loadEnv(); err == nil {
		t.Fatal("want an error for malformed FINDINGS, got nil")
	}
}

func TestLoadEnvFindingsAbsentMeansFirstAttempt(t *testing.T) {
	setRequiredEnv(t)
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

func TestGitRunRedactsToken(t *testing.T) {
	e := workerEnv{gitToken: "s3cret-tok"}
	err := e.gitAuth("", "clone", "https://x-access-token:s3cret-tok@git.example/repo.git", "/nonexistent-dir")
	if err == nil {
		t.Fatal("clone into a nonexistent dir should fail")
	}
	if strings.Contains(err.Error(), "s3cret-tok") {
		t.Fatalf("git error leaks the token: %s", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("git error not redacted: %s", err)
	}
}

func TestTokenStaysOutOfEnvAndGitConfig(t *testing.T) {
	for k, v := range map[string]string{
		"TASK_ID": "t1", "OBJECTIVE": "o", "REPOSITORY": "sf/r", "BRANCH": "b",
		"GIT_TOKEN": "s3cret-tok", "GATEWAY_URL": "http://gw", "TOKEN_FILE": "/tok", "MODEL": "m",
		"ANSWER_TOKENS": "1", "HEADROOM_TOKENS": "1",
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
	t.Setenv("REPO_URL", src)

	e, err := loadEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.gitToken != "s3cret-tok" {
		t.Fatalf("gitToken = %q", e.gitToken)
	}
	if _, set := os.LookupEnv("GIT_TOKEN"); set {
		t.Fatal("GIT_TOKEN still in the environment the agent inherits")
	}

	dst := filepath.Join(t.TempDir(), "repo")
	if err := e.gitAuth("", "clone", e.repoURL, dst); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(dst, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "s3cret-tok") || strings.Contains(string(cfg), "extraHeader") {
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
	if e.authHeader() != "Authorization: Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0LXRvaw==" {
		t.Fatalf("authHeader = %q", e.authHeader())
	}
}

// An agent that commits its own work must still get it pushed, under the
// factory-agent identity rather than one it borrows from the history.
func TestCommitAndPushKeepsAgentCommits(t *testing.T) {
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
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	sha := run("-C", seed, "rev-parse", "HEAD")

	for _, tc := range []struct {
		name       string
		agent      func(dir string)
		wantPushed bool
		wantParent bool // pushed commit's parent is the agent's commit
	}{
		{name: "no changes", agent: func(string) {}},
		{name: "agent committed", wantPushed: true, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
			run("-C", dir, "add", "a.txt")
			run("-C", dir, "commit", "-q", "-m", "agent")
		}},
		{name: "agent committed and left changes", wantPushed: true, wantParent: true, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
			run("-C", dir, "add", "a.txt")
			run("-C", dir, "commit", "-q", "-m", "agent")
			os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/" + strings.ReplaceAll(tc.name, " ", "-")
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch, repoURL: remote}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := e.gitAuth("", "clone", remote, dir); err != nil {
				t.Fatal(err)
			}
			base, err := prepareRepo(dir, e)
			if err != nil {
				t.Fatal(err)
			}
			if base != sha {
				t.Fatalf("base = %s, want %s", base, sha)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			remoteHead, _ := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
			if !tc.wantPushed {
				if commit != "" || len(remoteHead) != 0 {
					t.Fatalf("commit = %q, remote branch = %q, want nothing pushed", commit, remoteHead)
				}
				return
			}
			if commit == "" || strings.TrimSpace(string(remoteHead)) != commit {
				t.Fatalf("commit = %q, remote branch = %q, want the pushed HEAD", commit, remoteHead)
			}
			for _, author := range strings.Split(run("-C", remote, "log", "--format=%an <%ae>", base+".."+commit), "\n") {
				if author != "factory-agent <factory-agent@factory.local>" {
					t.Fatalf("pushed commit author = %q, want factory-agent", author)
				}
			}
			if tc.wantParent && run("-C", remote, "log", "-1", "--format=%s", commit+"^") != "agent" {
				t.Fatal("leftover changes were not committed on top of the agent's commit")
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

	e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: "factory/t1", gitToken: "s3cret-tok"}
	var pushes, refused, spyHeaders int
	git := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic auth for x-access-token:s3cret-tok, base64 derived by hand
		if r.Header.Get("Authorization") != "Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0LXRvaw==" {
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

	dir := filepath.Join(t.TempDir(), "repo")
	if err := e.gitAuth("", "clone", "-q", e.repoURL, dir); err != nil {
		t.Fatal(err)
	}
	base, err := prepareRepo(dir, e)
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

	killed := false
	commit, err := commitAndPush(dir, e, base, func() error {
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
	})
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
	if run("-C", remote, "log", "-1", "--format=%s", commit+"^") != "agent" {
		t.Fatal("leftovers were not committed on top of the agent's commit")
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
// outright, so branch protection cannot keep an agent from editing
// .forgejo/ (ruling 90): the worker itself must refuse the push instead.
func TestCommitAndPushRefusesForgejoPathChanges(t *testing.T) {
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
	run("-C", seed, "-c", "user.name=Human", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	run("clone", "-q", "--bare", seed, remote)
	sha := run("-C", seed, "rev-parse", "HEAD")

	for _, tc := range []struct {
		name       string
		agent      func(dir string)
		wantRefuse bool
	}{
		{name: "agent-commit-touches-forgejo", wantRefuse: true, agent: func(dir string) {
			os.MkdirAll(filepath.Join(dir, ".forgejo", "workflows"), 0o755)
			os.WriteFile(filepath.Join(dir, ".forgejo", "workflows", "pr.yml"), []byte("evil: true\n"), 0o644)
			run("-C", dir, "add", "-A")
			run("-C", dir, "commit", "-q", "-m", "agent")
		}},
		{name: "leftover-change-touches-forgejo", wantRefuse: true, agent: func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
			run("-C", dir, "add", "a.txt")
			run("-C", dir, "commit", "-q", "-m", "agent")
			os.MkdirAll(filepath.Join(dir, ".forgejo"), 0o755)
			os.WriteFile(filepath.Join(dir, ".forgejo", "config.yml"), []byte("evil: true\n"), 0o644)
		}},
		{name: "lookalike-path-is-not-forgejo", agent: func(dir string) {
			os.MkdirAll(filepath.Join(dir, ".forgejo-lookalike"), 0o755)
			os.WriteFile(filepath.Join(dir, ".forgejo-lookalike", "x"), []byte("x\n"), 0o644)
			run("-C", dir, "add", "-A")
			run("-C", dir, "commit", "-q", "-m", "agent")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := "factory/" + tc.name
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch, repoURL: remote}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := e.gitAuth("", "clone", remote, dir); err != nil {
				t.Fatal(err)
			}
			base, err := prepareRepo(dir, e)
			if err != nil {
				t.Fatal(err)
			}
			tc.agent(dir)

			commit, err := commitAndPush(dir, e, base, func() error { return nil })
			remoteHead, _ := exec.Command("git", "-C", remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
			if tc.wantRefuse {
				if err == nil {
					t.Fatalf("commitAndPush succeeded, commit = %q, want refusal", commit)
				}
				if !strings.Contains(err.Error(), ".forgejo") {
					t.Fatalf("error = %q, want it to name .forgejo", err)
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
	dir := filepath.Join(t.TempDir(), "repo")
	run("init", "-q", "-b", "main", dir)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1\n2\n3\n"), 0o644)
	run("-C", dir, "add", "a.txt")
	run("-C", dir, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "base")
	e := workerEnv{taskID: "t1", branch: "factory/t1"}
	base, err := prepareRepo(dir, e)
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

func TestCheckpointLoopEmitsOnlyChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", dir}, {"-C", dir, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	e := workerEnv{branch: "b"}
	base, err := prepareRepo(dir, e)
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
