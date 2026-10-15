package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
)

// SessionRecorder keeps the two session records: the full event history as
// one JSONL object per session under sessions/<task-id>/, re-PUT on every
// event so a killed session leaves its partial history durable in S3; and
// the compact restart state (findings + resume blob) in Postgres.
type SessionRecorder struct {
	store   Store
	objects ObjectStore
	taskID  string
	id      string
	history bytes.Buffer
}

func NewSessionRecorder(ctx context.Context, store Store, objects ObjectStore, taskID, sessionID, parentSessionID, model string) (*SessionRecorder, error) {
	if err := store.InsertSession(ctx, Session{ID: sessionID, TaskID: taskID, ParentSessionID: parentSessionID, Model: aliasOf(model)}); err != nil {
		return nil, err
	}
	return &SessionRecorder{store: store, objects: objects, taskID: taskID, id: sessionID}, nil
}

// ResumeSessionRecorder continues a session's record from its object: a
// reattached session appends to the same history.
func ResumeSessionRecorder(ctx context.Context, store Store, objects ObjectStore, taskID, sessionID string) (*SessionRecorder, error) {
	body, err := objects.Get(ctx, "sessions/"+taskID+"/"+sessionID+".jsonl")
	if err != nil {
		return nil, err
	}
	r := &SessionRecorder{store: store, objects: objects, taskID: taskID, id: sessionID}
	r.history.Write(body)
	return r, nil
}

// Record appends the event to the session history and ships the whole object.
func (r *SessionRecorder) Record(ctx context.Context, ev harness.Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	r.history.Write(line)
	r.history.WriteByte('\n')
	return r.objects.Put(ctx, "sessions/"+r.taskID+"/"+r.id+".jsonl", r.history.Bytes())
}

// Snapshot persists the compact restart state a resumed session continues from.
func (r *SessionRecorder) Snapshot(ctx context.Context, findings []string, restartState []byte) error {
	return r.store.UpdateSession(ctx, r.id, findings, restartState)
}

// SupervisorTail is the history's last events within n bytes, whole
// lines, as the supervisor reads them beside its digest and messages
// (roadmap 201): no supervisor event, whose guidance the messages carry,
// no command's output and no checkpoint's patch, the digest carrying the
// failures' outputs and the diff so far.
func (r *SessionRecorder) SupervisorTail(n int) string {
	b := r.history.Bytes()
	var lines [][]byte
	size := 0
	for end := len(b); end > 0; {
		start := bytes.LastIndexByte(b[:end-1], '\n') + 1
		var ev harness.Event
		err := json.Unmarshal(b[start:end], &ev)
		end = start
		if err != nil || ev.Kind == harness.EventSupervisor {
			continue
		}
		switch ev.Kind {
		case harness.EventCommand:
			delete(ev.Detail, "output")
		case harness.EventCheckpoint:
			delete(ev.Detail, "patch")
		}
		line, _ := json.Marshal(ev)
		if size += len(line) + 1; size > n {
			break
		}
		lines = append(lines, append(line, '\n'))
	}
	slices.Reverse(lines)
	return string(bytes.Join(lines, nil))
}
