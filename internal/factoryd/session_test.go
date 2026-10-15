package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
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

var _ ObjectStore = (*fakeObjectStore)(nil)

func TestSessionRecorderStreamsHistoryToS3(t *testing.T) {
	store := newFakeStore()
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), store, objects, "t1", "s1", "s0", "factory/coding-cheap")
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
	r, err := NewSessionRecorder(context.Background(), store, &fakeObjectStore{}, "t1", "s1", "", "factory/coding-cheap")
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

// Roadmap 201: the supervisor's tail of the record holds each fact once
// beside its digest and messages: no supervisor event, no command output,
// no checkpoint patch and no model call's tokens and cost. It is one JSON
// array of whole lines within n bytes, the newest last: its brackets and
// each line with its comma count. The record itself keeps them all.
func TestSupervisorTailLeavesTheDigestsFactsOut(t *testing.T) {
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), newFakeStore(), objects, "t1", "s1", "", "factory/coding-cheap")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []harness.Event{
		{Time: time.Unix(1000, 0).UTC(), Kind: harness.EventModelCall, Detail: map[string]any{"reason": "tool-calls", "text": "run the test",
			"tokens": map[string]any{"total": 41000, "input": 40000}, "cost": 0.004}},
		{Time: time.Unix(1001, 0).UTC(), Kind: harness.EventCommand, Detail: map[string]any{"command": "go test ./...", "exit": 1, "output": "FAIL x"}},
		{Time: time.Unix(1002, 0).UTC(), Kind: harness.EventSupervisor, Detail: map[string]any{"decision": "message", "guidance": "read x"}},
		{Time: time.Unix(1003, 0).UTC(), Kind: harness.EventCheckpoint, Detail: map[string]any{"diff": map[string]any{"files": 1, "insertions": 2, "deletions": 0}, "patch": "+x"}},
		{Time: time.Unix(1004, 0).UTC(), Kind: harness.EventFileChange, Detail: map[string]any{"path": "a.go"}},
	} {
		if err := r.Record(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	lines := []string{
		`{"time":"1970-01-01T00:16:40Z","kind":"model_call","detail":{"reason":"tool-calls","text":"run the test"}}`,
		`{"time":"1970-01-01T00:16:41Z","kind":"command","detail":{"command":"go test ./...","exit":1}}`,
		`{"time":"1970-01-01T00:16:43Z","kind":"checkpoint","detail":{"diff":{"deletions":0,"files":1,"insertions":2}}}`,
		`{"time":"1970-01-01T00:16:44Z","kind":"file_change","detail":{"path":"a.go"}}`,
	}
	if got, want := string(r.SupervisorTail(maxRecordTail)), "["+strings.Join(lines, ",")+"]"; got != want {
		t.Fatalf("tail =\n%s\nwant\n%s", got, want)
	}
	n := len("[") + len(lines[2]) + len(",") + len(lines[3]) + len("]")
	if got, want := string(r.SupervisorTail(n)), "["+lines[2]+","+lines[3]+"]"; got != want {
		t.Fatalf("tail of %d bytes = %s, want the last two lines", n, got)
	}
	if got, want := string(r.SupervisorTail(n-1)), "["+lines[3]+"]"; got != want {
		t.Fatalf("tail of %d bytes = %s, want the last line", n-1, got)
	}
	if record := string(objects.puts[len(objects.puts)-1].body); !strings.Contains(record, `"output":"FAIL x"`) || !strings.Contains(record, `"patch":"+x"`) ||
		!strings.Contains(record, `"kind":"supervisor"`) || !strings.Contains(record, `"cost":0.004`) {
		t.Fatalf("record = %s, want it whole", record)
	}
}
