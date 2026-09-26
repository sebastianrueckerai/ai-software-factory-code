package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRunRedactsToken(t *testing.T) {
	e := workerEnv{gitToken: "s3cret-tok"}
	err := e.gitRun("", "clone", "https://x-access-token:s3cret-tok@git.example/repo.git", "/nonexistent-dir")
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
	if err := e.gitRun("", "clone", e.repoURL, dst); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(dst, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "s3cret-tok") || strings.Contains(string(cfg), "extraHeader") {
		t.Fatalf(".git/config carries the credential:\n%s", cfg)
	}
	// git run by the worker sends the header; git run by the agent does not
	header, err := e.git(dst, "config", "--get", "http.extraHeader")
	if err != nil || header != e.authHeader() {
		t.Fatalf("worker git header = %q, %v", header, err)
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
			e := workerEnv{taskID: "t1", objective: "o", repoSHA: sha, branch: branch}
			dir := filepath.Join(t.TempDir(), "repo")
			if err := e.gitRun("", "clone", remote, dir); err != nil {
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

			commit, err := commitAndPush(dir, e, base)
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
			for _, author := range strings.Split(run("-C", dir, "log", "--format=%an <%ae>", base+".."+commit), "\n") {
				if author != "factory-agent <factory-agent@factory.local>" {
					t.Fatalf("pushed commit author = %q, want factory-agent", author)
				}
			}
			if tc.wantParent && run("-C", dir, "log", "-1", "--format=%s", commit+"^") != "agent" {
				t.Fatal("leftover changes were not committed on top of the agent's commit")
			}
		})
	}
}
