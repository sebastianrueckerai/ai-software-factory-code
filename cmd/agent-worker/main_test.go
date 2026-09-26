package main

import (
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

func TestRepoURLWithTokenBothSchemes(t *testing.T) {
	for in, want := range map[string]string{
		"https://git.example/sf/repo.git":                  "https://x-access-token:tok@git.example/sf/repo.git",
		"http://forgejo-http.forgejo.svc:3000/sf/repo.git": "http://x-access-token:tok@forgejo-http.forgejo.svc:3000/sf/repo.git",
	} {
		e := workerEnv{repoURL: in, gitToken: "tok"}
		if got := e.repoURLWithToken(); got != want {
			t.Errorf("repoURLWithToken(%q) = %q, want %q", in, got, want)
		}
	}
}
