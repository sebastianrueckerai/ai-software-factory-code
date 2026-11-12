package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpstreamCopyExitsOneWithAScrubbedReasonWhenTheStoreFails(t *testing.T) {
	dir := t.TempDir()
	const password = "store-password-123"
	for name, v := range map[string]string{
		"user": "site-admin", "password": password,
		"lines": "docker.io/library/golang:1.26.11-alpine@sha256:" + strings.Repeat("a", 64),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	code := runUpstreamCopy(t.Context(), []string{"-store", "http://site-admin:" + password + "@127.0.0.1:1", "-dir", dir}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.HasPrefix(errOut.String(), "upstream copy:") || strings.Contains(errOut.String(), password) {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestUpstreamCopyWithoutItsFilesExitsOne(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runUpstreamCopy(t.Context(), []string{"-store", "http://127.0.0.1:1", "-dir", t.TempDir()}, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestUpstreamCopyWithoutStoreIsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runUpstreamCopy(t.Context(), nil, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
