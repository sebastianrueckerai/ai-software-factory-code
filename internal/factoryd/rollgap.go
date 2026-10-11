package factoryd

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"slices"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// RollHold is a roll task's hold on new agent sessions (ruling 269): set
// while its merge waits for a session gap, in force until Until.
type RollHold struct {
	TaskID       string
	Since, Until time.Time
}

func (s *PGStore) SetRollHold(ctx context.Context, taskID string, since, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO roll_holds (task_id, since, until) VALUES ($1, $2, $3)
		ON CONFLICT (task_id) DO UPDATE SET since = EXCLUDED.since, until = EXCLUDED.until`, taskID, since, until)
	return err
}

func (s *PGStore) ClearRollHold(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM roll_holds WHERE task_id = $1`, taskID)
	return err
}

// RollHold is the newest hold in force at now, if any.
func (s *PGStore) RollHold(ctx context.Context, now time.Time) (RollHold, bool, error) {
	var h RollHold
	err := s.db.QueryRowContext(ctx, `SELECT task_id, since, until FROM roll_holds WHERE until > $1
		ORDER BY since DESC, task_id LIMIT 1`, now).Scan(&h.TaskID, &h.Since, &h.Until)
	if errors.Is(err, sql.ErrNoRows) {
		return RollHold{}, false, nil
	}
	return h, err == nil, err
}

// rollPoll is how often a roll's wait for a session gap and a held
// session's wait read their state.
const rollPoll = 15 * time.Second

// rollGapSlack is how much longer than the longest agent time in force a
// roll waits for a session gap before it merges anyway.
const rollGapSlack = 30 * time.Minute

// rollWaitSlack is how long a roll's wait may run past its own bound
// before Temporal ends it, and before GateWatch alerts.
const rollWaitSlack = 5 * time.Minute

// rollAdmission is how long a session AwaitRollHold let start counts as
// live before its RunSession counts it.
const rollAdmission = time.Minute

// RollGapNote is on a roll's merge card: the approval does not merge at once.
const RollGapNote = "after the approval the roll merges in the next session gap, when no agent session runs"

var errStopping = errors.New("factoryd stopping; the wait goes on in the next")

// gapBound is how long a roll waits for a session gap: the longest agent
// time in force at any level a task's agent time comes from (the limits,
// a class, a process, a project) plus rollGapSlack.
func gapBound(v policy.Version) time.Duration {
	longest := v.Document.Limits.AgentTime
	var times []*policy.Time
	for _, c := range v.Document.Classes {
		times = append(times, c.Time)
	}
	for _, p := range v.Processes {
		times = append(times, p.Time)
	}
	for _, p := range v.Projects {
		times = append(times, p.Time)
	}
	for _, t := range times {
		if t != nil {
			longest = max(longest, t.AgentTime)
		}
	}
	return longest + rollGapSlack
}

// AwaitSessionGap is a roll's wait before its merge (ruling 269): its hold
// keeps new sessions from starting until the bound, and it returns once no
// session runs here, the roll's own aside, or at the bound, when the merge
// goes ahead and stops the sessions still running.
func (r *Runner) AwaitSessionGap(ctx context.Context, taskID string) error {
	start, bound := r.clock(), gapBound(r.cfg.Policy())
	until := start.Add(bound)
	if err := r.store.SetRollHold(ctx, taskID, start, until); err != nil {
		return err
	}
	live := r.liveSessions(taskID)
	if len(live) > 0 {
		r.record(ctx, taskID, "roll_waiting", map[string]any{"live": live})
	}
	for len(live) > 0 {
		now := r.clock()
		if !now.Before(until) {
			log.Printf("roll %s: no session gap within %s: merging with %v running", taskID, bound, live)
			break
		}
		activity.RecordHeartbeat(ctx)
		if err := r.pause(ctx, min(rollPoll, until.Sub(now))); err != nil {
			return err
		}
		live = r.liveSessions(taskID)
	}
	r.record(ctx, taskID, "roll_gap", map[string]any{"waited_s": int(r.clock().Sub(start) / time.Second)})
	return nil
}

// AwaitRollHold lets a task's next session start once no roll holds
// sessions, the task's own hold and a chat task's aside: before the hold
// is read the task counts as admitted, so a roll that sets its hold after
// the read finds it live; while held it is not.
func (r *Runner) AwaitRollHold(ctx context.Context, taskID string) error {
	t, _, err := r.task(ctx, taskID)
	if err != nil || t.Kind == KindChat {
		return err
	}
	held := false
	for {
		r.admit(taskID, true)
		h, ok, err := r.store.RollHold(ctx, r.clock())
		if err == nil && (!ok || h.TaskID == taskID) {
			return nil
		}
		r.admit(taskID, false)
		if err != nil {
			return err
		}
		if !held {
			held = true
			r.record(ctx, taskID, "held", map[string]any{"roll": h.TaskID})
		}
		activity.RecordHeartbeat(ctx)
		if err := r.pause(ctx, rollPoll); err != nil {
			return err
		}
	}
}

// ClearRollHold ends a roll's hold: its merge ended.
func (r *Runner) ClearRollHold(ctx context.Context, taskID string) error {
	return r.store.ClearRollHold(ctx, taskID)
}

// admit counts taskID's next session as live from now, or no longer.
func (r *Runner) admit(taskID string, in bool) {
	at := r.clock()
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if !in {
		delete(r.admitted, taskID)
		return
	}
	if r.admitted == nil {
		r.admitted = map[string]time.Time{}
	}
	r.admitted[taskID] = at
}

// sessionRuns counts a session of taskID as running here until the
// returned func; it replaces the task's admission.
func (r *Runner) sessionRuns(taskID string) (done func()) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.running == nil {
		r.running = map[string]int{}
	}
	r.running[taskID]++
	delete(r.admitted, taskID)
	return func() {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		if r.running[taskID]--; r.running[taskID] <= 0 {
			delete(r.running, taskID)
		}
	}
}

// liveSessions are the tasks, except except, whose session runs here or
// was admitted within rollAdmission, sorted.
func (r *Runner) liveSessions(except string) []string {
	now := r.clock()
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	var out []string
	for id := range r.running {
		if id != except {
			out = append(out, id)
		}
	}
	for id, at := range r.admitted {
		switch {
		case now.Sub(at) >= rollAdmission:
			delete(r.admitted, id)
		case id != except && r.running[id] == 0:
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func (r *Runner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// pause sleeps d; it ends early with an error when ctx ends or factoryd
// stops, and the activity's retry waits on in the next factoryd.
func (r *Runner) pause(ctx context.Context, d time.Duration) error {
	if r.sleep != nil {
		return r.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-activity.GetWorkerStopChannel(ctx):
		return errStopping
	case <-t.C:
		return nil
	}
}
