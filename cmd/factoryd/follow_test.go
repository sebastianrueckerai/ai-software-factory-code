package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	followImages = "docker.io/library/registry:3@sha256:1111111111111111111111111111111111111111111111111111111111111111\n" +
		"https://example.org/temporal-1.8.0.tgz@sha256:2222222222222222222222222222222222222222222222222222222222222222 helm\n"
	followFiles = "node v24.22.3 node.tar.xz https://nodejs.org/node.tar.xz 4f2b175a6d53a5c617a80dba2676238a0688cef28c6da4eba35c7d844e66fcfb\n"
	followParts = "repository: sf/x\nparts:\n  - {name: charts, lane: alone, paths: [k8s]}\n  - {name: nodes, lane: hand, paths: [talos]}\n"
)

func followTree(t *testing.T, chart string) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "upstream-images.txt"), followImages)
	write(t, filepath.Join(dir, "upstream-files.txt"), followFiles)
	write(t, filepath.Join(dir, "factory/projects/infra.yaml"), followParts)
	write(t, filepath.Join(dir, "k8s/t.yaml"), "# pin chart temporal\nversion: "+chart+"\n")
	write(t, filepath.Join(dir, "talos/n.yaml"), "cilium install --set image.digest=sha256:"+strings.Repeat("5", 64)+"\n")
	return dir
}

func runFollowCmd(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(append([]string{"follow"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestFollowPrintsAndWrites(t *testing.T) {
	t.Setenv("REGISTRY_USER", "")
	dir := followTree(t, "1.7.0")
	code, out, errs := runFollowCmd(dir)
	want := "charts (alone): 1 behind of 1 reference\n  chart temporal: 1.7.0 -> 1.8.0\n  would write k8s/t.yaml\n" +
		"nodes (hand): 0 behind of 0 references\n"
	if code != 0 || out != want || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want %q", code, out, errs, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "k8s/t.yaml")); !strings.Contains(string(b), "1.7.0") {
		t.Fatalf("a dry run wrote: %s", b)
	}
	if code, _, errs := runFollowCmd("-w", dir); code != 2 || !strings.Contains(errs, "REGISTRY_USER") {
		t.Fatalf("-w without a login: exit %d, %q", code, errs)
	}
}

func TestFollowCheck(t *testing.T) {
	base := followTree(t, "1.7.0")
	code, out, errs := runFollowCmd("check", base)
	if code != 0 || out != "1 reference behind\n" || errs != "" {
		t.Fatalf("exit %d, %q, %q", code, out, errs)
	}
	moved := followTree(t, "1.8.0")
	if code, out, _ := runFollowCmd("check", moved, "--base", base); code != 0 || out != "0 references behind\n" {
		t.Fatalf("a followed value: exit %d, %q", code, out)
	}
	wrong := followTree(t, "1.7.9")
	code, _, errs = runFollowCmd("check", wrong, "--base", base)
	if code != 1 || !strings.Contains(errs, "k8s/t.yaml:2: # pin chart temporal: changed from the base, and is not the list's") {
		t.Fatalf("a wrong value: exit %d, %q", code, errs)
	}
	write(t, filepath.Join(wrong, "k8s/u.yaml"), "x: sha256:"+strings.Repeat("3", 64)+"\n")
	if code, _, errs := runFollowCmd("check", wrong); code != 1 || !strings.Contains(errs, "k8s/u.yaml:1: a sha256 digest with no marker") {
		t.Fatalf("an unmarked digest: exit %d, %q", code, errs)
	}
	if code, _, errs := runFollowCmd("check"); code != 2 || !strings.Contains(errs, "usage: factoryd follow") {
		t.Fatalf("no dir: exit %d, %q", code, errs)
	}
	noParts := t.TempDir()
	if code, _, errs := runFollowCmd("check", noParts); code != 1 || !strings.Contains(errs, "no file of factory/projects holds a parts: list") {
		t.Fatalf("no parts: exit %d, %q", code, errs)
	}
}

func TestFollowWrites(t *testing.T) {
	t.Setenv("REGISTRY_USER", "bot")
	t.Setenv("REGISTRY_PASSWORD", "pw")
	t.Setenv("FORGEJO_PUBLIC_URL", "http://127.0.0.1:1")
	dir := followTree(t, "1.7.0")
	code, out, errs := runFollowCmd("-w", dir)
	if code != 0 || !strings.Contains(out, "  wrote k8s/t.yaml\n") || strings.Contains(out, "talos/n.yaml") || errs != "" {
		t.Fatalf("exit %d, %q, %q", code, out, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "k8s/t.yaml")); string(b) != "# pin chart temporal\nversion: 1.8.0\n" {
		t.Fatalf("file %q", b)
	}
}
