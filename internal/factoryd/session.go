package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
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
// Every string in the event's detail is scrubbed first.
func (r *SessionRecorder) Record(ctx context.Context, ev harness.Event) error {
	ev.Detail = scrubDetail(ev.Detail)
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	r.history.Write(line)
	r.history.WriteByte('\n')
	return r.objects.Put(ctx, "sessions/"+r.taskID+"/"+r.id+".jsonl", r.history.Bytes())
}

var recordScrubber = redact.New()

// scrubDetail is a copy of d with every string value, nested ones included,
// scrubbed. Digests under a key ending "sha256" are not secrets and stay.
func scrubDetail(d map[string]any) map[string]any {
	if d == nil {
		return nil
	}
	out := make(map[string]any, len(d))
	for k, v := range d {
		if strings.HasSuffix(k, "sha256") {
			out[k] = v
			continue
		}
		out[k] = scrubValue(v)
	}
	return out
}

func scrubValue(v any) any {
	switch v := v.(type) {
	case string:
		return recordScrubber.String(v)
	case map[string]any:
		return scrubDetail(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = scrubValue(e)
		}
		return out
	}
	return v
}

// Snapshot persists the compact restart state a resumed session continues from.
func (r *SessionRecorder) Snapshot(ctx context.Context, findings []string, restartState []byte) error {
	return r.store.UpdateSession(ctx, r.id, findings, restartState)
}

// Transcript is the live session's record so far: the in-session
// check-in reads it factoryd-side, where the pod's copy does not reach.
func (r *SessionRecorder) Transcript() []byte {
	return bytes.Clone(r.history.Bytes())
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
