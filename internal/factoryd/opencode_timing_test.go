package factoryd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// Every tool event carries its run time from the part's state.time; a
// command the bytes of its output, a file change the file's size after it.
func TestOpencodeToolEventsCarryDurationAndBytes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("edited-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []harness.Event
	for _, name := range []string{"opencode-run-edit.jsonl", "opencode-run-tools.jsonl", "opencode-run-mcp.jsonl"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, parseLines(&opencodeParser{workDir: dir}, strings.Split(strings.TrimSpace(string(b)), "\n")...)...)
	}
	type timing struct {
		kind     harness.EventKind
		duration any
		bytes    any
	}
	var got []timing
	for _, ev := range events {
		switch ev.Kind {
		case harness.EventCommand, harness.EventFileChange, harness.EventToolCall:
			got = append(got, timing{ev.Kind, ev.Detail["duration_ms"], ev.Detail["bytes"]})
		}
	}
	want := []timing{
		{harness.EventCommand, int64(37), 11},    // "(no output)"
		{harness.EventFileChange, int64(12), 15}, // "edited-fixture\n"
		{harness.EventCommand, int64(37), 11},
		{harness.EventToolCall, int64(2), nil},
		{harness.EventToolCall, int64(4), nil},
		{harness.EventToolCall, int64(3), nil},
	}
	if len(got) != len(want) {
		t.Fatalf("tool events %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
