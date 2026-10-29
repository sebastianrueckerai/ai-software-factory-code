//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// With DOCS_URL set the clone container also clones the document
// repository's text files into ws/docs and leaves its images out; a
// document repository that cannot be cloned leaves the task repository's
// clone intact and records why, for the brief.
func TestCloneModeClonesTheDocuments(t *testing.T) {
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
	root := t.TempDir()
	task, docs := filepath.Join(root, "task"), filepath.Join(root, "docs")
	commit := []string{"-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "init"}
	for _, repo := range []string{task, docs} {
		run("init", "-q", "-b", "main", repo)
	}
	run("-C", task, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "--allow-empty", "-m", "init")
	files := map[string]string{"roadmap.md": "# roadmap", "design/round/index.html": "<html>", "design/round/shot.png": "png", "notes": "no extension"}
	for name, body := range files {
		path := filepath.Join(docs, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("-C", docs, "add", ".")
	run(append([]string{"-C", docs}, commit...)...)
	taskBare, docsBare := filepath.Join(root, "task.git"), filepath.Join(root, "docs.git")
	run("clone", "-q", "--bare", task, taskBare)
	run("clone", "-q", "--bare", docs, docsBare)
	run("-C", docsBare, "config", "uploadpack.allowFilter", "true")
	run("-C", docsBare, "config", "uploadpack.allowAnySHA1InWant", "true")
	head := run("-C", task, "rev-parse", "HEAD")

	for k, v := range map[string]string{"MODE": "clone", "REPO_URL": "file://" + taskBare, "PUSH_URL": "", "REPO_SHA": head,
		"BRANCH": "factory/t1", "REPO_TOKEN": "r3ad-tok"} {
		t.Setenv(k, v)
	}

	t.Run("cloned", func(t *testing.T) {
		t.Setenv("DOCS_URL", "file://"+docsBare)
		ws := t.TempDir()
		if code := runClone(nolog, ws); code != 0 {
			t.Fatalf("clone exit %d", code)
		}
		if err := fetched(ws); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"roadmap.md", "design/round/index.html"} {
			got, err := os.ReadFile(filepath.Join(ws, "docs", name))
			if err != nil || string(got) != files[name] {
				t.Fatalf("docs/%s = %q, %v, want %q", name, got, err, files[name])
			}
		}
		for _, name := range []string{"design/round/shot.png", "notes"} {
			if _, err := os.Stat(filepath.Join(ws, "docs", name)); !os.IsNotExist(err) {
				t.Fatalf("docs/%s: %v, want it left out", name, err)
			}
		}
		if cloned, failed := docsState(ws, "/workspace/docs"); !cloned || failed != "" {
			t.Fatalf("docsState = %v, %q, want cloned", cloned, failed)
		}
	})

	t.Run("failed", func(t *testing.T) {
		t.Setenv("DOCS_URL", "file://"+filepath.Join(root, "missing.git"))
		ws := t.TempDir()
		if code := runClone(nolog, ws); code != 0 {
			t.Fatalf("clone exit %d, want 0", code)
		}
		if err := fetched(ws); err != nil {
			t.Fatalf("the task repository's fetch failed: %v", err)
		}
		if got := run("-C", filepath.Join(ws, "repo"), "rev-parse", "HEAD"); got != head {
			t.Fatalf("HEAD = %s, want the task repository's %s", got, head)
		}
		cloned, failed := docsState(ws, "/workspace/docs")
		if cloned || !strings.Contains(failed, "does not appear to be a git repository") || strings.Contains(failed, "r3ad-tok") {
			t.Fatalf("docsState = %v, %q, want the clone's failure without the token", cloned, failed)
		}
	})

	t.Run("none", func(t *testing.T) {
		ws := t.TempDir()
		if cloned, failed := docsState(ws, ""); cloned || failed != "" {
			t.Fatalf("docsState = %v, %q, want nothing for a project without documents", cloned, failed)
		}
	})
}

// A card's relative document links resolve against the clone, so the
// agent reads the linked file instead of searching for it; neither .git
// nor a symlink is a document. A session without a cloned document
// repository has no links.
func TestBriefResolvesTheIssuesDocumentLinks(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"docs/plans/2026-10-14-lane-collapse.md", ".git/HEAD"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("plans/2026-10-14-lane-collapse.md", filepath.Join(dir, "docs/link.md")); err != nil {
		t.Fatal(err)
	}
	const objective = "Read [the plan](../plans/2026-10-14-lane-collapse.md) and [the gone one](../plans/none.md)."

	brief := harness.TaskBrief{Objective: objective, AuthorStanding: harness.StandingWriter, Docs: true}
	resolveDocLinks(nolog, &brief, dir)
	want := []string{
		"../plans/2026-10-14-lane-collapse.md is " + dir + "/docs/plans/2026-10-14-lane-collapse.md",
		"../plans/none.md is not among the text files at " + dir,
	}
	if !slices.Equal(brief.DocLinks, want) {
		t.Fatalf("DocLinks =\n%q\nwant\n%q", brief.DocLinks, want)
	}

	brief = harness.TaskBrief{Objective: "[head](HEAD) [plans](../plans/) [link](link.md)", Docs: true}
	resolveDocLinks(nolog, &brief, dir)
	want = []string{"HEAD is not among the text files at " + dir, "../plans/ is " + dir + "/docs/plans", "link.md is not among the text files at " + dir}
	if !slices.Equal(brief.DocLinks, want) {
		t.Fatalf("DocLinks =\n%q\nwant\n%q", brief.DocLinks, want)
	}

	for name, b := range map[string]harness.TaskBrief{
		"failed": {Objective: objective, DocsFailed: "git clone: repository not found"},
		"none":   {Objective: objective},
	} {
		resolveDocLinks(nolog, &b, dir)
		if b.DocLinks != nil {
			t.Errorf("%s: DocLinks = %q, want nil", name, b.DocLinks)
		}
	}
	brief = harness.TaskBrief{Objective: objective, Docs: true}
	resolveDocLinks(nolog, &brief, filepath.Join(dir, "missing"))
	if brief.DocLinks != nil {
		t.Fatalf("DocLinks of an unreadable clone = %q, want nil", brief.DocLinks)
	}
}
