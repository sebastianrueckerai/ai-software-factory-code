package factoryd

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"
)

// Timeline is the task-level record sessions/<task-id>/timeline.jsonl: one
// JSON line per lifecycle step (intake, sessions, pushes, PR, checks,
// signals, final state), next to the per-session histories. Activities of
// one workflow run one at a time, so read-append-write needs no lock.
type Timeline struct {
	Objects ObjectStore
}

func (tl Timeline) Append(ctx context.Context, taskID, step string, detail map[string]any) error {
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
