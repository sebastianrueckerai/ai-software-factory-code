package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The sweep owns the map's directory: pages and raw twins the registry
// no longer names go, everything else stays.
func TestSweepRemovesStrayPages(t *testing.T) {
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "raw"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"coding.md", "gone.md", filepath.Join("raw", "coding.json"), filepath.Join("raw", "gone.json"), "notes.txt"} {
		if err := os.WriteFile(filepath.Join(out, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := map[string]bool{"coding.md": true, "index.md": true, filepath.Join("raw", "coding.json"): true}
	if err := sweep(out, keep); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"gone.md", filepath.Join("raw", "gone.json")} {
		if _, err := os.Stat(filepath.Join(out, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", gone)
		}
	}
	for _, stays := range []string{"coding.md", filepath.Join("raw", "coding.json"), "notes.txt"} {
		if _, err := os.Stat(filepath.Join(out, stays)); err != nil {
			t.Errorf("%s was swept: %v", stays, err)
		}
	}
}

// The pinned harness is verified before any loop mode drives it: a
// binary of another version would label every loop page with a
// falsehood.
func TestCheckOpencodeVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-opencode")
	write := func(ver string) {
		t.Helper()
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"opencode version "+ver+"\"\n"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write("1.18.32")
	if err := checkOpencodeVersion(bin, "1.18.32"); err != nil {
		t.Fatalf("matching version refused: %v", err)
	}
	write("1.18.32 (abc123)")
	if err := checkOpencodeVersion(bin, "1.18.32"); err != nil {
		t.Fatalf("build metadata rejected: %v", err)
	}
	write("9.9.9")
	if err := checkOpencodeVersion(bin, "1.18.32"); err == nil {
		t.Fatal("a drifted binary passed as the pinned version")
	}
}

// A dirty tree ships pages under a SHA that does not hold the code they
// were read from; the tool refuses.
func TestRequireCleanTree(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	git("add", "go.mod")
	git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "x")
	if err := requireCleanTree(dir); err != nil {
		t.Fatalf("clean tree refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.go"), []byte("package x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := requireCleanTree(dir); err == nil {
		t.Fatal("a dirty tree passed as clean")
	}
}
