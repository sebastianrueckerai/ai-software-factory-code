package factoryd

import (
	"context"
	"encoding/json"
	"log"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
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
	// Events receives a row beside every event timeline line (the store);
	// nil records none.
	Events eventRecorder
	// Store reads the task of an event, for its repository and issue; nil
	// (or no task) leaves no row.
	Store Store
	// Metrics receives factory.task.gate.duration for every step that
	// carries started; nil = none.
	Metrics *telemetry.Metrics
	now     func() time.Time
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
	now := time.Now().UTC()
	if tl.now != nil {
		now = tl.now()
	}
	entry := map[string]any{}
	maps.Copy(entry, detail)
	entry["time"] = now
	entry["step"] = step
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := tl.Objects.Put(ctx, key, slices.Concat(prev, line, []byte("\n"))); err != nil {
		return err
	}
	tl.gate(ctx, step, detail, now)
	return nil
}

// gate emits the duration of the gate a step ends (StepGates) when the
// step carries started; commissioning ends at the server's live, not at
// the step's own time.
func (tl *Timeline) gate(ctx context.Context, step string, detail map[string]any, end time.Time) {
	gate, ok := StepGates[step]
	raw, carries := detail["started"].(string)
	if !ok || !carries || tl.Metrics == nil {
		return
	}
	started, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		log.Printf("timeline %s: started %q: %v", step, raw, err)
		return
	}
	if live, ok := detail["live"].(string); ok {
		if end, err = time.Parse(time.RFC3339, live); err != nil {
			log.Printf("timeline %s: live %q: %v", step, live, err)
			return
		}
	}
	outcome := ""
	for _, k := range []string{"outcome", "state", "verdict", "signal"} {
		// the named string types (ChecksState, SessionOutcome) too
		if v := reflect.ValueOf(detail[k]); v.Kind() == reflect.String && v.String() != "" {
			outcome = v.String()
			break
		}
	}
	model, _ := detail["model"].(string)
	tl.Metrics.GateDuration.Record(ctx, end.Sub(started).Seconds(), metric.WithAttributes(
		attribute.String("gate", gate), attribute.String("outcome", outcome), attribute.String("model_alias", model)))
}
