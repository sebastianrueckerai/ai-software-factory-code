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
// strategy Recreate). A task's lock goes with its final line.
type Timeline struct {
	Objects ObjectStore
	locks   sync.Map // task id -> *sync.Mutex
}

func (tl *Timeline) Append(ctx context.Context, taskID, step string, detail map[string]any) error {
	mu, _ := tl.locks.LoadOrStore(taskID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if step == "final" {
		defer tl.locks.Delete(taskID)
	}
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
