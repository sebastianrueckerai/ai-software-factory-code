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

// SupervisorTail is the history's last events within n bytes as the
// supervisor reads them beside its digest and messages (roadmap 201): no
// supervisor event, whose guidance the messages carry; no command's
// output and no checkpoint's patch, the digest carrying the failures'
// outputs and the diff so far; no model call's tokens and cost, which no
// decision reads; a command's line as commandLine cuts it.
func (r *SessionRecorder) SupervisorTail(n int) json.RawMessage {
	return jsonlTail(r.history.Bytes(), n, func(line []byte) []byte {
		var ev harness.Event
		if json.Unmarshal(line, &ev) != nil || ev.Kind == harness.EventSupervisor {
			return nil
		}
		switch ev.Kind {
		case harness.EventCommand:
			delete(ev.Detail, "output")
			if cmd, ok := ev.Detail["command"].(string); ok {
				ev.Detail["command"] = commandLine(cmd)
			}
		case harness.EventCheckpoint:
			delete(ev.Detail, "patch")
		case harness.EventModelCall:
			delete(ev.Detail, "tokens")
			delete(ev.Detail, "cost")
		}
		out, _ := json.Marshal(ev)
		return out
	})
}

// jsonlTail is JSONL b's last lines as one JSON array of at most n bytes,
// so a model reads them as JSON, not escaped into a string: each line
// whole and as keep makes it, nil leaving it out; a line that is no JSON
// value is left out.
func jsonlTail(b []byte, n int, keep func(line []byte) []byte) json.RawMessage {
	var lines [][]byte
	size := len("[")
	for end := len(b); end > 0; {
		start := bytes.LastIndexByte(b[:end-1], '\n') + 1
		line := bytes.TrimSpace(b[start:end])
		end = start
		if !json.Valid(line) {
			continue
		}
		if line = keep(line); line == nil {
			continue
		}
		if size += len(line) + len(","); size > n {
			break
		}
		lines = append(lines, line)
	}
	slices.Reverse(lines)
	return json.RawMessage("[" + string(bytes.Join(lines, []byte(","))) + "]")
}
