//go:build unix

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// The session learns the push rule at once: a file change under a workflow
// directory sends it one message naming the file and the push rule's
// sentence, once per file; a change elsewhere sends nothing.
func TestWorkflowDirGuardMessagesAtFirstWorkflowDirChange(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		want       string // the exact message, empty when none
	}{
		{"relative forgejo path", ".forgejo/workflows/ci.yml",
			"you changed .forgejo/workflows/ci.yml under .forgejo/: the push will refuse it — refusing to push: .forgejo/ differs from the base. Revert that file and do the work without touching it."},
		{"absolute path in the clone", "/workspace/repo/.gitea/workflows/x.yml",
			"you changed .gitea/workflows/x.yml under .gitea/: the push will refuse it — refusing to push: .gitea/ differs from the base. Revert that file and do the work without touching it."},
		{"workflow directory itself", ".github", "you changed .github under .github/: the push will refuse it — refusing to push: .github/ differs from the base. Revert that file and do the work without touching it."},
		{"upper case directory", ".FORGEJO/x",
			"you changed .FORGEJO/x under .forgejo/: the push will refuse it — refusing to push: .forgejo/ differs from the base. Revert that file and do the work without touching it."},
		{"a file outside the clone", "/tmp/notes.md", ""},
		{"a path outside the workflow directories", "cmd/agent-worker/main.go", ""},
		{"a name that only looks like it", "src/.forgejo-helper/ci.yml", ""},
		{"no path", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newWorkflowDirGuard("/workspace/repo", func(context.Context, string) error { return nil }, func(string, ...any) {})
			sent := g.watch(t)
			g.observe(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": tc.path}})
			got := sent()
			if got != tc.want {
				t.Fatalf("message = %q; want %q", got, tc.want)
			}
		})
	}
}

// The message goes once per file: a second change of the same file sends
// nothing more, another file's first change does.
func TestWorkflowDirGuardWarnsOncePerFile(t *testing.T) {
	g := newWorkflowDirGuard("/workspace/repo", func(context.Context, string) error { return nil }, func(string, ...any) {})
	sent := g.watch(t)
	change := func(p string) {
		g.observe(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": p}})
	}
	change(".forgejo/ci.yml")
	change(".forgejo/ci.yml")
	if got := sent(); strings.Count(got, "refusing to push") != 1 {
		t.Fatalf("one file's first change sent %d rule sentences, want 1:\n%s", strings.Count(got, "refusing to push"), got)
	}
	change(".github/x.yml")
	if got := sent(); strings.Count(got, "refusing to push") != 1 {
		t.Fatalf("the second file's first change sent %d rule sentences, want 1:\n%s", strings.Count(got, "refusing to push"), got)
	}
}

// The guard passes every event on to the wrapped emitter, whatever it
// observes.
func TestWorkflowDirGuardWrapsTheEmitter(t *testing.T) {
	var mu sync.Mutex
	var seen []harness.Event
	g := newWorkflowDirGuard("/workspace/repo", func(context.Context, string) error { return nil }, func(string, ...any) {})
	done := g.watch(t)
	emit := g.wrap(func(ev harness.Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev)
	})
	emit(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": "main.go"}})
	emit(harness.Event{Kind: harness.EventFileChange, Detail: map[string]any{"path": ".forgejo/ci.yml"}})
	done()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the wrapped emitter saw %d events, want 2", len(seen))
	}
}

// watch collects the guard's messages, which its sends deliver
// asynchronously, and returns a func that waits for a new message and
// answers the text of every message sent since the previous call.
func (g *workflowDirGuard) watch(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var texts []string
	g.send = func(_ context.Context, text string) error {
		mu.Lock()
		defer mu.Unlock()
		texts = append(texts, text)
		return nil
	}
	return func() string {
		for i := 0; i < 200; i++ {
			mu.Lock()
			n := len(texts)
			mu.Unlock()
			if n != 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		got := strings.Join(texts, "\n")
		texts = nil
		return got
	}
}
