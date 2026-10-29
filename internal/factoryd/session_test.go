package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

type fakeObjectStore struct {
	mu   sync.Mutex
	puts []struct {
		key  string
		body []byte
	}
}

func (s *fakeObjectStore) Put(ctx context.Context, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, struct {
		key  string
		body []byte
	}{key, body})
	return nil
}

// Get returns the last body put under key, nil when none was.
func (s *fakeObjectStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.puts) - 1; i >= 0; i-- {
		if s.puts[i].key == key {
			return s.puts[i].body, nil
		}
	}
	return nil, nil
}

// Delete forgets every body put under key.
func (s *fakeObjectStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.puts {
		if s.puts[i].key == key {
			s.puts[i].body = nil
		}
	}
	return nil
}

var _ ObjectStore = (*fakeObjectStore)(nil)

func TestSessionRecorderStreamsHistoryToS3(t *testing.T) {
	store := newFakeStore()
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), store, objects, redact.New(), "t1", "s1", "s0", "factory/small")
	if err != nil {
		t.Fatalf("NewSessionRecorder: %v", err)
	}

	events := []harness.Event{
		{Time: time.Unix(1000, 0).UTC(), Kind: harness.EventCheckpoint},
		{Time: time.Unix(1001, 0).UTC(), Kind: harness.EventCommand, Detail: map[string]any{"command": "go test ./...", "exit": 0}},
		{Time: time.Unix(1002, 0).UTC(), Kind: harness.EventModelCall, Detail: map[string]any{"cost": 0.00021}},
	}
	for _, ev := range events {
		if err := r.Record(context.Background(), ev); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	if len(objects.puts) == 0 || objects.puts[len(objects.puts)-1].key != "sessions/t1/s1.jsonl" {
		t.Fatalf("object keys = %+v", objects.puts)
	}
	final := string(objects.puts[len(objects.puts)-1].body)
	lines := strings.Split(strings.TrimSuffix(final, "\n"), "\n")
	if len(lines) != len(events) {
		t.Fatalf("final object has %d lines, want %d: %q", len(lines), len(events), final)
	}
	for i, line := range lines {
		var got harness.Event
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not an event: %v (%q)", i, err, line)
		}
		if got.Kind != events[i].Kind || !got.Time.Equal(events[i].Time) {
			t.Fatalf("line %d = %+v, want %+v", i, got, events[i])
		}
	}
	// Every earlier PUT must be a byte prefix of the final object: a killed
	// session leaves its partial history readable in S3.
	for i, p := range objects.puts {
		if !strings.HasPrefix(final, string(p.body)) {
			t.Fatalf("PUT %d is not a prefix of the final object", i)
		}
	}

	sess, ok, err := store.Session(context.Background(), "s1")
	if err != nil || !ok {
		t.Fatalf("Session = (%v, %v)", ok, err)
	}
	if sess.TaskID != "t1" || sess.ParentSessionID != "s0" {
		t.Fatalf("session row = %+v", sess)
	}
}

func TestSessionRecorderSnapshotGoesToPostgres(t *testing.T) {
	store := newFakeStore()
	r, err := NewSessionRecorder(context.Background(), store, &fakeObjectStore{}, redact.New(), "t1", "s1", "", "factory/small")
	if err != nil {
		t.Fatal(err)
	}
	findings := []string{"repo cloned at abc123", "go test ./... green"}
	restart := []byte(`{"todo":["finish store"]}`)
	if err := r.Snapshot(context.Background(), findings, restart); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	sess, _, _ := store.Session(context.Background(), "s1")
	if !reflect.DeepEqual(sess.Findings, findings) || string(sess.RestartState) != string(restart) {
		t.Fatalf("session after snapshot = %+v", sess)
	}
}

// Transcript is the live session's record for the in-session check-in:
// the history so far, whole, as a copy the caller cannot corrupt.
func TestTranscript(t *testing.T) {
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), newFakeStore(), objects, redact.New(), "t1", "s1", "", "factory/small")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Transcript(); len(got) != 0 {
		t.Fatalf("a fresh recorder's transcript = %q, want nothing", got)
	}
	ev := harness.Event{Time: time.Unix(1000, 0).UTC(), Kind: harness.EventCheckpoint, Detail: map[string]any{"diff": map[string]any{"files": 1}}}
	if err := r.Record(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	want := objects.puts[len(objects.puts)-1].body
	if got := r.Transcript(); !bytes.Equal(got, want) {
		t.Fatalf("transcript = %q, want the history as it was put", got)
	}
	got := r.Transcript()
	got[0] = 'X'
	if again := r.Transcript(); !bytes.Equal(again, want) {
		t.Fatalf("a mutated transcript reached the history: %q", again)
	}
}

func TestSessionRecorderMasksSecretsBeforeStoring(t *testing.T) {
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), newFakeStore(), objects, redact.New(), "t1", "s1", "", "factory/small")
	if err != nil {
		t.Fatal(err)
	}
	key := "sk-abcdefghijklmnop1234"
	hash := strings.Repeat("ab", 32)
	for _, ev := range []harness.Event{
		{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "task", "result": "found it, key " + key, "args": map[string]any{"prompt": "use " + key}, "args_sha256": hash}},
		{Kind: harness.EventCommand, Detail: map[string]any{"command": "env", "output": "TOKEN=" + key + "\n"}},
		{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "task", "result": "found 3 files"}},
	} {
		if err := r.Record(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	got := string(r.Transcript())
	if strings.Contains(got, key) {
		t.Fatalf("a key shape reached the record: %s", got)
	}
	masked := redact.New().String(key)
	for _, want := range []string{`"result":"found it, key ` + masked + `"`, `"prompt":"use ` + masked + `"`, `"output":"TOKEN=` + masked + `\n"`, `"result":"found 3 files"`, `"args_sha256":"` + hash + `"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("record lacks %s:\n%s", want, got)
		}
	}
	if stored, _ := objects.Get(context.Background(), "sessions/t1/s1.jsonl"); string(stored) != got {
		t.Fatal("the stored object differs from the transcript")
	}
}

// The scrub keeps what the record is read for: an acceptance command that
// names a long test still proves the card through the stored record, and a
// path with capitals and a long identifier in a todo stay as written.
func TestSessionRecorderKeepsIdentifiersAndPaths(t *testing.T) {
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), newFakeStore(), objects, redact.New(), "t1", "s1", "", "factory/small")
	if err != nil {
		t.Fatal(err)
	}
	command := "go test ./internal/factoryd/ -run TestWorkingStateCutsOnRuneBoundaries -count=1"
	path := "/workspace/repo/web/src/components/ProjectBoardDrawer.tsx"
	todo := "name it TestSizeTripSeesACheckpointQueuedBehindAnAnswer"
	for _, ev := range []harness.Event{
		{Kind: harness.EventCommand, Detail: map[string]any{"command": command, "exit": 0}},
		{Kind: harness.EventFileChange, Detail: map[string]any{"path": path}},
		{Kind: harness.EventToolCall, Detail: map[string]any{"tool": "todowrite", "args": map[string]any{"todos": []any{map[string]any{"status": "pending", "content": todo}}}}},
	} {
		if err := r.Record(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	got := string(r.Transcript())
	for _, want := range []string{`"command":"` + command + `"`, `"path":"` + path + `"`, `"content":"` + todo + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %s:\n%s", want, got)
		}
	}
	runner := &Runner{timeline: &Timeline{Objects: objects}}
	hit := acceptanceHit(runner.sessionTranscript(context.Background(), "t1", "s1"), []string{command})
	if hit == nil || hit.Command != command || hit.Exit != 0 {
		t.Fatalf("acceptance hit = %+v, want %q with exit 0", hit, command)
	}
}

// Every value type passes the scrub: the strings of a typed value, of a
// string slice and of nested slices are masked, and numbers, bools and
// nulls are kept as they were.
func TestSessionRecorderScrubsEveryValueType(t *testing.T) {
	r, err := NewSessionRecorder(context.Background(), newFakeStore(), &fakeObjectStore{}, redact.New(), "t1", "s1", "", "factory/small")
	if err != nil {
		t.Fatal(err)
	}
	key := "sk-abcdefghijklmnop1234"
	ev := harness.Event{Kind: harness.EventSupervisor, Detail: map[string]any{
		"record": &CheckInRecord{Model: "big", Reason: "found " + key},
		"lines":  []string{"a", "key " + key},
		"nested": []any{[]any{"key " + key, 7}},
		"count":  int64(9007199254740993), "cost": 0.00021, "called": true, "none": nil,
	}}
	if err := r.Record(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	got := string(r.Transcript())
	if strings.Contains(got, key) {
		t.Fatalf("a key shape reached the record: %s", got)
	}
	for _, want := range []string{`"model":"big"`, `"reason":"found [redacted]"`, `"lines":["a","key [redacted]"]`, `"nested":[["key [redacted]",7]]`,
		`"count":9007199254740993`, `"cost":0.00021`, `"called":true`, `"none":null`} {
		if !strings.Contains(got, want) {
			t.Errorf("record lacks %s:\n%s", want, got)
		}
	}
}
