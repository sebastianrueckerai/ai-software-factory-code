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
