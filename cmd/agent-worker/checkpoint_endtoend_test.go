//go:build unix

package main

// checkpoint_endtoend_test.go is plan #495's integration card: the full
// acceptance scenario run end-to-end — a checkpoint taken while a committed
// file has been removed from the work tree, and the emitted checkpoint
// event carrying the diff stat and full patch for the deletion.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// TestCheckpointCarriesTreeAfterFileRemovedEndToEnd runs the real
// checkpoint path — seed repo, clone, prepareRepo, newCheckpointer, loop —
// with the agent's mid-session removal of a committed file done through
// that same path: between two ticks the worker stages the work tree exactly
// as it finds it, so the tick after the removal sees the vanished file and
// must carry its deletion instead of dropping the checkpoint. This git
// reads a tracked file's absence as its deletion and stages it fine; the
// Stage 17 failure was a race inside one staging step, which
// TestCheckpointToleratesFileRemovedDuringStaging injects.
func TestCheckpointCarriesTreeAfterFileRemovedEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed, dir := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	run("init", "-q", "-b", "main", seed)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("1\n2\n3\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "keep.txt"), []byte("k\n"), 0o644)
	run("-C", seed, "add", "a.txt", "keep.txt")
	run("-C", seed, "-c", "user.name=H", "-c", "user.email=h@h", "commit", "-q", "-m", "base")
	run("clone", "-q", seed, dir)
	e := workerEnv{taskID: "t1", branch: "factory/t1"}
	base, _, err := prepareRepo(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	ck, err := newCheckpointer(dir, e, base)
	if err != nil {
		t.Fatal(err)
	}

	events := make(chan harness.Event, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ck.loop(ctx, 20*time.Millisecond, func(ev harness.Event) { events <- ev }, t.Logf)
	}()
	// the agent edits a line, then removes a committed file from the work
	// tree mid-session; each change reaches the loop through its own tick
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("k\nx\n"), 0o644)
	var edit harness.Event
	select {
	case edit = <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("no checkpoint after an edit")
	}
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	var ev harness.Event
	select {
	case ev = <-events:
	case <-time.After(10 * time.Second):
		t.Fatal("no checkpoint after removing a committed file")
	}
	cancel()
	<-done

	diff, _ := ev.Detail["diff"].(map[string]any)
	patch, _ := ev.Detail["patch"].(string)
	if ev.Kind != harness.EventCheckpoint {
		t.Fatalf("event kind = %q, want checkpoint", ev.Kind)
	}
	// both files changed against the base: keep.txt edited (+1), a.txt gone (-3)
	if diff["files"] != 2 || diff["insertions"] != 1 || diff["deletions"] != 3 {
		t.Fatalf("diff = %+v, want 2 files, 1 insertion, 3 deletions", diff)
	}
	if !strings.Contains(patch, "deleted file mode 100644") || !strings.Contains(patch, "-1\n-2\n-3") ||
		!strings.Contains(patch, "+++ /dev/null") || !strings.Contains(patch, "+x") {
		t.Fatalf("patch does not carry the deletion: %q", patch)
	}
	// the earlier checkpoint saw no deletion: the event under test is the tree after the removal
	if firstDiff, _ := edit.Detail["diff"].(map[string]any); firstDiff["deletions"] != 0 {
		t.Fatalf("edit checkpoint = %+v, want no deletions", firstDiff)
	}
}
