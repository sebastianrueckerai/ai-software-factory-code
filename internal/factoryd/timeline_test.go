package factoryd

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestTimelineAppendsLinesAcrossWriters(t *testing.T) {
	objects := &fakeObjectStore{}
	ctx := context.Background()
	if err := (&Timeline{Objects: objects}).Append(ctx, "t1", "intake", map[string]any{"source": "api", "issue": 4}); err != nil {
		t.Fatal(err)
	}
	// a second writer (another activity, maybe another factoryd pod) keeps the first line
	if err := (&Timeline{Objects: objects}).Append(ctx, "t1", "pr_opened", map[string]any{"number": 9}); err != nil {
		t.Fatal(err)
	}
	body, _ := objects.Get(ctx, "sessions/t1/timeline.jsonl")
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want 2", lines)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first["step"] != "intake" || first["source"] != "api" || first["issue"] != float64(4) || first["time"] == nil {
		t.Fatalf("first = %v", first)
	}
	if second["step"] != "pr_opened" || second["number"] != float64(9) {
		t.Fatalf("second = %v", second)
	}
}

// race() runs two activities of one task at once and both may record;
// concurrent appends to one task's timeline lose no line.
func TestTimelineSerializesConcurrentAppends(t *testing.T) {
	objects := &fakeObjectStore{}
	tl := &Timeline{Objects: objects}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tl.Append(context.Background(), "t1", "step", map[string]any{"i": i}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	body, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if lines := strings.Split(strings.TrimSpace(string(body)), "\n"); len(lines) != 32 {
		t.Fatalf("lines = %d, want all 32 appends kept", len(lines))
	}
}
