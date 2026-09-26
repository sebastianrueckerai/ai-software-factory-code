package factoryd

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

type fakeObjectStore struct {
	puts []struct {
		key  string
		body []byte
	}
}

func (s *fakeObjectStore) Put(ctx context.Context, key string, body []byte) error {
	s.puts = append(s.puts, struct {
		key  string
		body []byte
	}{key, body})
	return nil
}

var _ ObjectStore = (*fakeObjectStore)(nil)

func TestSessionRecorderStreamsHistoryToS3(t *testing.T) {
	store := newFakeStore()
	objects := &fakeObjectStore{}
	r, err := NewSessionRecorder(context.Background(), store, objects, "t1", "s1", "s0")
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
	r, err := NewSessionRecorder(context.Background(), store, &fakeObjectStore{}, "t1", "s1", "")
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
