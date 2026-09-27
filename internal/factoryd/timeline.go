package factoryd

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sync"
	"time"
)

// Timeline is the task-level record sessions/<task-id>/timeline.jsonl: one
// JSON line per lifecycle step (intake, sessions, pushes, PR, checks,
// signals, final state), next to the per-session histories. race() runs
// two activities of one task side by side and both record, so the
// read-append-write is serialized per task (one factoryd replica,
// strategy Recreate). A task's lock lives while an append holds or awaits
// it.
type Timeline struct {
	Objects ObjectStore
	mu      sync.Mutex
	locks   map[string]*taskLock
}

type taskLock struct {
	sync.Mutex
	users int // appends holding or awaiting it
}

// lock takes taskID's lock and returns its release.
func (tl *Timeline) lock(taskID string) (unlock func()) {
	tl.mu.Lock()
	l := tl.locks[taskID]
	if l == nil {
		if tl.locks == nil {
			tl.locks = map[string]*taskLock{}
		}
		l = &taskLock{}
		tl.locks[taskID] = l
	}
	l.users++
	tl.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		tl.mu.Lock()
		if l.users--; l.users == 0 {
			delete(tl.locks, taskID)
		}
		tl.mu.Unlock()
	}
}

func (tl *Timeline) Append(ctx context.Context, taskID, step string, detail map[string]any) error {
	defer tl.lock(taskID)()
	key := "sessions/" + taskID + "/timeline.jsonl"
	prev, err := tl.Objects.Get(ctx, key)
	if err != nil {
		return err
	}
	entry := map[string]any{}
	maps.Copy(entry, detail)
	entry["time"] = time.Now().UTC()
	entry["step"] = step
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return tl.Objects.Put(ctx, key, slices.Concat(prev, line, []byte("\n")))
}
