package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// A key-like run of 32 characters or more with a digit, which the scrubber's
// generic rule masks wherever it stands.
const bearerValue = "abcdefgh12345678abcdefgh12345678abcd"

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
	// the dial error quotes the URL, path and all: both values stand in it
	code := runUpstreamCopy(t.Context(), []string{"-store", "http://127.0.0.1:1/" + password + "/" + bearerValue, "-dir", dir}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.HasPrefix(errOut.String(), "upstream copy:") || !strings.Contains(errOut.String(), "127.0.0.1:1") {
		t.Fatalf("stderr = %q, want the dial error", errOut.String())
	}
	for _, leak := range []string{password, bearerValue} {
		if strings.Contains(errOut.String(), leak) {
			t.Errorf("stderr %q carries %q", errOut.String(), leak)
		}
	}
}

func TestUpstreamCopyReportScrubsFindingsAndFailures(t *testing.T) {
	const password = "store-password-123"
	text := "a line: " + password + " Authorization: Bearer " + bearerValue + "\nnext"
	var out, errOut bytes.Buffer
	if code := report([]string{text}, nil, redact.New(password), &out, &errOut); code != 0 {
		t.Fatalf("exit %d, want 0 for findings", code)
	}
	if !strings.HasPrefix(out.String(), factoryd.UpstreamFinding+"a line:") || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("stdout = %q, want one finding line", out.String())
	}
	for _, leak := range []string{password, bearerValue} {
		if strings.Contains(out.String(), leak) {
			t.Errorf("finding %q carries %q", out.String(), leak)
		}
	}
	if code := report(nil, errors.New(text), redact.New(password), &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	for _, leak := range []string{password, bearerValue} {
		if strings.Contains(errOut.String(), leak) {
			t.Errorf("error %q carries %q", errOut.String(), leak)
		}
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
