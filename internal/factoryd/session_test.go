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
	r, err := NewSessionRecorder(context.Background(), store, objects, "t1", "s1", "s0", "factory/small")
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
	r, err := NewSessionRecorder(context.Background(), store, &fakeObjectStore{}, "t1", "s1", "", "factory/small")
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
	r, err := NewSessionRecorder(context.Background(), newFakeStore(), objects, "t1", "s1", "", "factory/small")
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
