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
