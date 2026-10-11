package factoryd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFixture writes files (path to content) under a fresh directory and
// returns its root's file system, as the worker opens its clone.
func repoFixture(t *testing.T, files map[string]string) *os.Root {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// Each file of a directory with its first doc line, sorted by name: a Go
// file's first comment line before its package clause (directives
// skipped), markdown's first heading, any other text's first # line past
// a shebang; a binary and a Go file without a leading comment by name, a
// subdirectory by name with a slash and none of its files, a long line
// cut at 120 bytes. A missing or outside path reads as not in the
// repository, a name no path has is dropped, a repeat is listed once.
func TestRepoMapListsEachFileWithItsFirstDocLine(t *testing.T) {
	root := repoFixture(t, map[string]string{
		"internal/demo/a.go":             "// Package demo serves the demo pages.\n// It has a second line.\npackage demo\n",
		"internal/demo/b.go":             "//go:build linux\n\n// b.go holds the handlers.\n\n// Package demo is said here too.\npackage demo\n",
		"internal/demo/c.go":             "package demo\n\n// Run runs.\nfunc Run() {}\n",
		"internal/demo/README.md":        "\n# Demo pages\n\nSome text.\n\n## More\n",
		"internal/demo/config.yaml":      "key: v # not this one\n# Settings of the demo.\nother: 1\n",
		"internal/demo/logo.png":         "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\n# not text\n",
		"internal/demo/run.sh":           "#!/bin/sh\n# run.sh starts the demo.\nexec demo\n",
		"internal/demo/long.md":          "# " + strings.Repeat("word ", 40) + "\n",
		"internal/demo/templates/x.html": "<p># not listed</p>\n",
		".git/HEAD":                      "ref: refs/heads/main\n",
	})
	got := RepoMap(root.FS(), []string{"internal/demo/", "internal/gone", "../etc", "/etc", "bad name", "internal/demo"})
	want := mapHeading + "\n" +
		"internal/demo/README.md: Demo pages\n" +
		"internal/demo/a.go: Package demo serves the demo pages.\n" +
		"internal/demo/b.go: b.go holds the handlers.\n" +
		"internal/demo/c.go\n" +
		"internal/demo/config.yaml: Settings of the demo.\n" +
		"internal/demo/logo.png\n" +
		"internal/demo/long.md: " + strings.TrimSuffix(strings.Repeat("word ", 19), " ") + "…\n" +
		"internal/demo/run.sh: run.sh starts the demo.\n" +
		"internal/demo/templates/\n" +
		"internal/gone/: not in the repository\n" +
		"../etc/: not in the repository\n" +
		"/etc/: not in the repository"
	if got != want {
		t.Fatalf("map =\n%s\nwant\n%s", got, want)
	}
	if l := len("internal/demo/long.md: " + strings.TrimSuffix(strings.Repeat("word ", 19), " ") + "…"); l != 120 {
		t.Fatalf("the cut line is %d bytes, want 120", l)
	}
	// the root lists its entries without .git, which is no part of the commit
	if got := RepoMap(root.FS(), []string{"."}); got != mapHeading+"\ninternal/" {
		t.Fatalf("root map = %q", got)
	}
	if got := RepoMap(root.FS(), nil); got != "" {
		t.Fatalf("no directories gave %q, want no map", got)
	}
}

// The section stops at 4 KiB with a tail counting the lines left out. The
// heading is 114 bytes and each of 500 name-only lines "d/fNNN.md" 9 bytes
// plus its newline: k lines and the tail "... and N more files" (22 bytes
// for a three-digit N, plus its newline) take 114 + 10k + 23 bytes, at
// most 4096 for k = 395 (4087 bytes); k = 396 would take 4097.
func TestRepoMapStopsAt4KiB(t *testing.T) {
	files := map[string]string{}
	for i := range 500 {
		files[fmt.Sprintf("d/f%03d.md", i)] = "no heading\n"
	}
	root := repoFixture(t, files)
	got := RepoMap(root.FS(), []string{"d"})
	lines := []string{mapHeading}
	for i := range 395 {
		lines = append(lines, fmt.Sprintf("d/f%03d.md", i))
	}
	want := strings.Join(append(lines, "... and 105 more files"), "\n")
	if len(mapHeading) != 114 || got != want || len(got) != 4087 {
		t.Fatalf("map (%d bytes) ends %q, want %d bytes ending %q", len(got), got[max(0, len(got)-60):], len(want), want[len(want)-60:])
	}
}
