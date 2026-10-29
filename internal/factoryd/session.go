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
// the compact restart state (findings + resume blob) in Postgres. Every
// event is scrubbed with scrub before it is kept.
type SessionRecorder struct {
	store   Store
	objects ObjectStore
	scrub   *redact.Scrubber
	taskID  string
	id      string
	history bytes.Buffer
}

func NewSessionRecorder(ctx context.Context, store Store, objects ObjectStore, scrub *redact.Scrubber, taskID, sessionID, parentSessionID, model string) (*SessionRecorder, error) {
	if err := store.InsertSession(ctx, Session{ID: sessionID, TaskID: taskID, ParentSessionID: parentSessionID, Model: aliasOf(model)}); err != nil {
		return nil, err
	}
	return &SessionRecorder{store: store, objects: objects, scrub: scrub, taskID: taskID, id: sessionID}, nil
}

// ResumeSessionRecorder continues a session's record from its object: a
// reattached session appends to the same history.
func ResumeSessionRecorder(ctx context.Context, store Store, objects ObjectStore, scrub *redact.Scrubber, taskID, sessionID string) (*SessionRecorder, error) {
	body, err := objects.Get(ctx, "sessions/"+taskID+"/"+sessionID+".jsonl")
	if err != nil {
		return nil, err
	}
	r := &SessionRecorder{store: store, objects: objects, scrub: scrub, taskID: taskID, id: sessionID}
	r.history.Write(body)
	return r, nil
}

// Record appends the event to the session history and ships the whole object.
// Every string in the event's detail is scrubbed first.
func (r *SessionRecorder) Record(ctx context.Context, ev harness.Event) error {
	detail, err := scrubDetail(r.scrub, ev.Detail)
	if err != nil {
		return err
	}
	ev.Detail = detail
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	r.history.Write(line)
	r.history.WriteByte('\n')
	return r.objects.Put(ctx, "sessions/"+r.taskID+"/"+r.id+".jsonl", r.history.Bytes())
}

// scrubDetail is d as the JSON it is stored as, with every string value in
// it, nested and typed ones included, scrubbed; numbers stay exact.
// Digests under a key ending "sha256" are not secrets and stay.
func scrubDetail(scrub *redact.Scrubber, d map[string]any) (map[string]any, error) {
	if d == nil {
		return nil, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var plain map[string]any
	if err := dec.Decode(&plain); err != nil {
		return nil, err
	}
	scrubValue(scrub, plain)
	return plain, nil
}

// scrubValue scrubs v, a decoded JSON value, in place, and returns it.
func scrubValue(scrub *redact.Scrubber, v any) any {
	switch v := v.(type) {
	case string:
		return scrub.String(v)
	case map[string]any:
		for k, e := range v {
			if !strings.HasSuffix(k, "sha256") {
				v[k] = scrubValue(scrub, e)
			}
		}
	case []any:
		for i, e := range v {
			v[i] = scrubValue(scrub, e)
		}
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
