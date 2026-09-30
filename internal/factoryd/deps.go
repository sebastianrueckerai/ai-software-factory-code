package factoryd

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"
)

// The dependency wait (stage11-design.md section 1.5): a card of a process
// with the dependencies block starts once every card it depends on is done.
const (
	// SignalDepEnded wakes a waiting card: a card it depends on ended.
	SignalDepEnded = "dep_ended"
	// GateDependencies is the gate of a card in waiting-deps.
	GateDependencies = "dependencies"
	// ActivityDependencies asks where a card's dependencies stand.
	ActivityDependencies = "Dependencies"
)

// depRecheck is how often a waiting card asks again without a dep_ended.
const depRecheck = 10 * time.Minute

// Dependency is a card another waits on, with its status.
type Dependency struct {
	TaskID string
	Issue  int64
	Key    string // its plan key
	Status string
}

// DepState is where a card's dependencies stand: Ended, one that ended
// failed or closed; else Waiting, the open ones' issues ascending; neither:
// the card starts.
type DepState struct {
	Waiting []int64
	Ended   *Dependency
}

// ended is the first dependency that ended without done.
func ended(deps []Dependency) *Dependency {
	for _, d := range deps {
		if Final(d.Status) {
			return &d
		}
	}
	return nil
}

func (s *PGStore) AddTaskDeps(ctx context.Context, id string, dependsOn []string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO task_deps (task_id, depends_on) SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, id, dependsOn)
	return err
}

func (s *PGStore) OpenDependencies(ctx context.Context, id string) ([]Dependency, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.issue_number, COALESCE(t.plan_key, ''), t.status
		FROM task_deps d JOIN tasks t ON t.id = d.depends_on
		WHERE d.task_id = $1 AND t.status <> 'done' ORDER BY t.issue_number, t.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dependency
	for rows.Next() {
		var d Dependency
		if err := rows.Scan(&d.TaskID, &d.Issue, &d.Key, &d.Status); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PGStore) WaitingDependents(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.task_id FROM task_deps d JOIN tasks t ON t.id = d.task_id
		WHERE d.depends_on = $1 AND t.status = 'waiting-deps' ORDER BY d.task_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Dependencies tells the workflow where the card's dependencies stand. A
// card that has to wait enters waiting-deps in gate dependencies (once:
// the gate keeps the wait's start); one whose wait is over records its end.
func (r *Runner) Dependencies(ctx context.Context, taskID string) (DepState, error) {
	t, _, err := r.task(ctx, taskID)
	if err != nil {
		return DepState{}, err
	}
	deps, err := r.store.OpenDependencies(ctx, taskID)
	if err != nil {
		return DepState{}, err
	}
	if d := ended(deps); d != nil {
		return DepState{Ended: d}, nil
	}
	if len(deps) == 0 {
		if t.Status == "waiting-deps" {
			r.record(ctx, taskID, "dependencies_done", gateEnd(t, GateDependencies, map[string]any{}))
		}
		return DepState{}, nil
	}
	var st DepState
	for _, d := range deps {
		st.Waiting = append(st.Waiting, d.Issue)
	}
	if t.Status == "waiting-deps" {
		return st, nil
	}
	if err := r.store.TransitionTask(ctx, taskID, []string{"queued"}, "waiting-deps"); err != nil {
		return DepState{}, terminalError("task %s is %s, cannot wait on its dependencies: %v", taskID, t.Status, err)
	}
	if err := r.store.SetGate(ctx, taskID, GateDependencies, time.Now()); err != nil {
		return DepState{}, err
	}
	mirrorTaskState(ctx, r.forgejo, t, "waiting-deps")
	r.record(ctx, taskID, "dependencies_wait", map[string]any{"on": st.Waiting})
	return st, nil
}

// depEndedReason is the card's question when dependency d ended failed or
// closed.
func depEndedReason(d Dependency) string {
	return fmt.Sprintf("dependency #%d (%s) ended %s: close this card, or answer to start it anyway", d.Issue, d.Key, d.Status)
}

// signalDependents wakes the waiting cards that depend on taskID, which
// just turned final. A failed signal is logged: the heal pass and the
// wait's re-check cover it.
func signalDependents(ctx context.Context, store Store, wf Workflows, taskID string) {
	if wf == nil {
		return
	}
	ids, err := store.WaitingDependents(ctx, taskID)
	if err != nil {
		log.Printf("task %s: the cards waiting on it: %v", taskID, err)
		return
	}
	for _, id := range ids {
		if err := wf.Signal(ctx, id, SignalDepEnded, TaskSignal{}); err != nil {
			log.Printf("task %s: signal %s to %s: %v", taskID, SignalDepEnded, id, err)
		}
	}
}

// healDependencyWait signals dep_ended to waiting card t when its wait
// would end: every dependency done, or one ended failed or closed.
func healDependencyWait(ctx context.Context, store Store, wf Workflows, t Task) {
	deps, err := store.OpenDependencies(ctx, t.ID)
	if err != nil {
		log.Printf("heal pass: dependencies of %s: %v", t.ID, err)
		return
	}
	if len(deps) > 0 && ended(deps) == nil {
		return
	}
	if err := wf.Signal(ctx, t.ID, SignalDepEnded, TaskSignal{}); err != nil {
		log.Printf("heal pass: signal %s to %s: %v", SignalDepEnded, t.ID, err)
	}
}

// waitsForDependencies: the run has a card of a process with the
// dependencies block that no session has touched yet (versionDepWait).
func (t *taskRun) waitsForDependencies(st RunState, u TaskUsage) bool {
	return t.depWait >= versionDepWait && t.cfg.Task.Dependencies != nil && !st.Skip && !t.isReport(st) && !st.Roll &&
		u.Attempts == 0 && st.PR.Number == 0 && !st.Screening && !st.Merged && !st.IssueClosed && st.Status != "closing"
}

// awaitDependencies holds the card until its dependencies are done, at no
// cost: it asks, waits for dep_ended or depRecheck, and asks again. A
// dependency that ended failed or closed goes to a human (healed: asked
// again as the healed question): an answer starts the card with the reason
// as a finding, a close ends it closed.
func (t *taskRun) awaitDependencies(healed bool) (final, why string, findings []string) {
	for {
		var d DepState
		if err := workflow.ExecuteActivity(t.short, ActivityDependencies, t.taskID).Get(t.ctx, &d); err != nil {
			return "failed", "dependencies: " + errMessage(err), nil
		}
		if d.Ended != nil {
			reason := depEndedReason(*d.Ended)
			ask := reason
			if healed {
				ask = healedReason
			}
			final, why, answer := t.awaitHuman(ask, reason)
			if final != "" {
				return closedBeforeStart(final, why)
			}
			return "", "", append([]string{reason}, answer...)
		}
		if len(d.Waiting) == 0 {
			return "", "", nil
		}
		timerCtx, cancel := workflow.WithCancel(t.ctx)
		sel := workflow.NewSelector(t.ctx)
		for _, n := range []string{SignalDepEnded, SignalIssueClosed} {
			sel.AddReceive(workflow.GetSignalChannel(t.ctx, n), func(workflow.ReceiveChannel, bool) {})
		}
		sel.AddFuture(workflow.NewTimer(timerCtx, depRecheck), func(workflow.Future) {})
		sel.Select(t.ctx)
		cancel()
		for workflow.GetSignalChannel(t.ctx, SignalDepEnded).ReceiveAsync(nil) {
		}
		if final, why, ok := t.decided(); ok {
			return closedBeforeStart(final, why)
		}
	}
}

// closedBeforeStart: a card's issue closed before it started ends the card
// closed; any other end stays as it is.
func closedBeforeStart(final, why string) (string, string, []string) {
	if final == "failed" && strings.HasPrefix(why, ReasonIssueClosed) {
		return "closed", why, nil
	}
	return final, why, nil
}
