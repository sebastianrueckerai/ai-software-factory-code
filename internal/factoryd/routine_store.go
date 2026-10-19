package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// SourceRoutine is the source of a card a routine's run files.
const SourceRoutine = "routine"

// TriggerRunNow is the trigger of a member's Run now, the one run that
// leaves a pending Skip once for the run it was set for.
const TriggerRunNow = "run-now"

// RoutineState is a routine's row in the store. Project is the project
// file's stem, "" for the factory's own routines of policy.yaml.
type RoutineState struct {
	Project, Routine string
	Merges           int // merges since the last merge-triggered run
	SkipNext         bool
	SkipBy           string
	Runs             int // runs so far, the next run's number is Runs+1
}

// RoutineRun is one run of a routine, written when it begins and ended
// when its card ends.
type RoutineRun struct {
	Project, Routine string
	Run              int
	Due              time.Time
	Started          time.Time
	Ended            time.Time // zero while the card is open
	Trigger          string    // cron, merges, configuration-version, push-main, run-now
	TaskID           string
	Issue            int64
	Outcome          string // filed, held, skipped, no change, reported, gone, merged, closed, failed, done
	Note             string
	CostEUR          float64
}

// RoutineStore keeps the routines' state rows, their runs and the merge
// pass's cursor.
type RoutineStore interface {
	// RoutineState reads the routine's row, creating it at zero when missing.
	RoutineState(ctx context.Context, project, routine string) (RoutineState, error)
	RoutineStates(ctx context.Context, project string) ([]RoutineState, error)
	SetRoutineMerges(ctx context.Context, project, routine string, merges int) error
	SetRoutineSkip(ctx context.Context, project, routine string, skip bool, by string) error
	// BeginRoutineRun writes run Runs+1 of r's routine and returns it with
	// began true. A run of the same due returns that run with began false.
	// With skip a pending Skip once is consumed: the new run's outcome is
	// "skipped" with the note "skipped once by <SkipBy>".
	BeginRoutineRun(ctx context.Context, r RoutineRun, skip bool) (run RoutineRun, began bool, err error)
	// RoutineRunFiled records the card a run filed and leaves the run open
	// until EndRoutineRun. ErrNotFound for a run that never began or ended.
	RoutineRunFiled(ctx context.Context, project, routine string, run int, taskID string, issue int64) error
	EndRoutineRun(ctx context.Context, project, routine string, run int, outcome, note, taskID string, issue int64, costEUR float64) error
	// RepointRoutineRun moves the open run that filed taskID onto
	// newTaskID and its issue: the go action's hand-over, so the hold
	// keeps holding and the run ends with the card that ran the work.
	RepointRoutineRun(ctx context.Context, taskID, newTaskID string, issue int64) error
	// RoutineRuns are the routine's last limit runs, newest first.
	RoutineRuns(ctx context.Context, project, routine string, limit int) ([]RoutineRun, error)
	// RoutineRunOfTask is the run that filed the task.
	RoutineRunOfTask(ctx context.Context, taskID string) (RoutineRun, bool, error)
	// OpenRoutineTask is the newest task of the routine whose status is
	// not final.
	OpenRoutineTask(ctx context.Context, project, routine string) (Task, bool, error)
	RoutineMergeCursor(ctx context.Context) (int64, error)
	// CountRoutineMerges adds merged[project] to the merges of every state
	// row of each project and moves the merge pass's cursor to through, in
	// one transaction.
	CountRoutineMerges(ctx context.Context, through int64, merged map[string]int) error
}

const routineStateColumns = `project, routine, merges, skip_next, skip_by, runs`

func scanRoutineState(scan func(dest ...any) error) (RoutineState, error) {
	var st RoutineState
	err := scan(&st.Project, &st.Routine, &st.Merges, &st.SkipNext, &st.SkipBy, &st.Runs)
	return st, err
}

func (s *PGStore) routineStates(ctx context.Context, query string, args ...any) ([]RoutineState, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineState
	for rows.Next() {
		st, err := scanRoutineState(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// routineState reads the routine's row in tx, creating it at zero first,
// and holds the row to the end of tx.
func routineState(ctx context.Context, tx *sql.Tx, project, routine string) (RoutineState, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO routine_state (project, routine) VALUES ($1, $2) ON CONFLICT DO NOTHING`, project, routine); err != nil {
		return RoutineState{}, err
	}
	return scanRoutineState(tx.QueryRowContext(ctx, `SELECT `+routineStateColumns+` FROM routine_state
		WHERE project = $1 AND routine = $2 FOR UPDATE`, project, routine).Scan)
}

func (s *PGStore) RoutineState(ctx context.Context, project, routine string) (RoutineState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoutineState{}, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	st, err := routineState(ctx, tx, project, routine)
	if err != nil {
		return RoutineState{}, err
	}
	return st, tx.Commit()
}

func (s *PGStore) RoutineStates(ctx context.Context, project string) ([]RoutineState, error) {
	return s.routineStates(ctx, `SELECT `+routineStateColumns+` FROM routine_state WHERE project = $1 ORDER BY routine`, project)
}

func (s *PGStore) SetRoutineMerges(ctx context.Context, project, routine string, merges int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO routine_state (project, routine, merges) VALUES ($1, $2, $3)
		ON CONFLICT (project, routine) DO UPDATE SET merges = EXCLUDED.merges`, project, routine, merges)
	return err
}

func (s *PGStore) SetRoutineSkip(ctx context.Context, project, routine string, skip bool, by string) error {
	if !skip {
		by = ""
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO routine_state (project, routine, skip_next, skip_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (project, routine) DO UPDATE SET skip_next = EXCLUDED.skip_next, skip_by = EXCLUDED.skip_by`, project, routine, skip, by)
	return err
}

const routineRunColumns = `project, routine, run, due, started, ended, trigger, task_id, issue_number, outcome, note, cost_eur::float8`

func scanRoutineRun(scan func(dest ...any) error) (RoutineRun, error) {
	var r RoutineRun
	var ended sql.NullTime
	err := scan(&r.Project, &r.Routine, &r.Run, &r.Due, &r.Started, &ended, &r.Trigger, &r.TaskID, &r.Issue, &r.Outcome, &r.Note, &r.CostEUR)
	r.Ended = ended.Time
	return r, err
}

func (s *PGStore) BeginRoutineRun(ctx context.Context, r RoutineRun, skip bool) (RoutineRun, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoutineRun{}, false, err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	st, err := routineState(ctx, tx, r.Project, r.Routine)
	if err != nil {
		return RoutineRun{}, false, err
	}
	outcome, note := "", ""
	if skip && st.SkipNext {
		outcome, note = "skipped", "skipped once by "+st.SkipBy
		st.SkipNext, st.SkipBy = false, ""
	}
	run, err := scanRoutineRun(tx.QueryRowContext(ctx, `INSERT INTO routine_runs (project, routine, run, due, trigger, outcome, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (project, routine, due) DO NOTHING RETURNING `+routineRunColumns,
		r.Project, r.Routine, st.Runs+1, r.Due, r.Trigger, outcome, note).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		run, err = scanRoutineRun(tx.QueryRowContext(ctx, `SELECT `+routineRunColumns+` FROM routine_runs
			WHERE project = $1 AND routine = $2 AND due = $3`, r.Project, r.Routine, r.Due).Scan)
		return run, false, err
	}
	if err != nil {
		return RoutineRun{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE routine_state SET runs = runs + 1, skip_next = $3, skip_by = $4
		WHERE project = $1 AND routine = $2`, r.Project, r.Routine, st.SkipNext, st.SkipBy); err != nil {
		return RoutineRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RoutineRun{}, false, err
	}
	return run, true, nil
}

func (s *PGStore) RoutineRunFiled(ctx context.Context, project, routine string, run int, taskID string, issue int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET outcome = 'filed', task_id = $4, issue_number = $5
		WHERE project = $1 AND routine = $2 AND run = $3 AND ended IS NULL`, project, routine, run, taskID, issue)
	return routineRunChanged(res, err, project, routine, run)
}

func (s *PGStore) EndRoutineRun(ctx context.Context, project, routine string, run int, outcome, note, taskID string, issue int64, costEUR float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET ended = now(), outcome = $4, note = $5, task_id = $6, issue_number = $7, cost_eur = $8
		WHERE project = $1 AND routine = $2 AND run = $3`, project, routine, run, outcome, note, taskID, issue, costEUR)
	return routineRunChanged(res, err, project, routine, run)
}

// RepointRoutineRun moves the open run that filed taskID onto newTaskID
// and its issue: the go action's hand-over, so the hold keeps holding
// and the run ends with the card that ran the work.
func (s *PGStore) RepointRoutineRun(ctx context.Context, taskID, newTaskID string, issue int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET task_id = $2, issue_number = $3
		WHERE task_id = $1 AND ended IS NULL`, taskID, newTaskID, issue)
	return err
}

// routineRunChanged is the update's error, ErrNotFound when no run matched.
func routineRunChanged(res sql.Result, err error, project, routine string, run int) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, fmt.Errorf("routine run %s/%s #%d: %w", project, routine, run, ErrNotFound))
	}
	return nil
}

func (s *PGStore) RoutineRuns(ctx context.Context, project, routine string, limit int) ([]RoutineRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+routineRunColumns+` FROM routine_runs
		WHERE project = $1 AND routine = $2 ORDER BY run DESC LIMIT $3`, project, routine, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutineRun
	for rows.Next() {
		r, err := scanRoutineRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoutineRunOfTask is the run that filed the task.
func (s *PGStore) RoutineRunOfTask(ctx context.Context, taskID string) (RoutineRun, bool, error) {
	r, err := scanRoutineRun(s.db.QueryRowContext(ctx, `SELECT `+routineRunColumns+` FROM routine_runs
		WHERE task_id = $1 AND task_id <> '' ORDER BY run LIMIT 1`, taskID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RoutineRun{}, false, nil
	}
	return r, err == nil, err
}

func (s *PGStore) OpenRoutineTask(ctx context.Context, project, routine string) (Task, bool, error) {
	return s.oneTask(ctx, `id = (SELECT r.task_id FROM routine_runs r JOIN tasks t ON t.id = r.task_id
		WHERE r.project = $1 AND r.routine = $2 AND t.status <> ALL($3) ORDER BY r.run DESC LIMIT 1)`, project, routine, FinalStatuses)
}

func (s *PGStore) RoutineMergeCursor(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT last_event_id FROM routine_merge_cursor`).Scan(&id)
	return id, err
}

// CountRoutineMerges updates the projects in name order, so two passes
// lock the rows in one order.
func (s *PGStore) CountRoutineMerges(ctx context.Context, through int64, merged map[string]int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // #nosec G104 -- a no-op after the commit
	for _, project := range slices.Sorted(maps.Keys(merged)) {
		if _, err := tx.ExecContext(ctx, `UPDATE routine_state SET merges = merges + $2 WHERE project = $1`, project, merged[project]); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE routine_merge_cursor SET last_event_id = $1`, through); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PGStore) MergedEvents(ctx context.Context, after int64, limit int) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events WHERE id > $1 AND kind = $2 ORDER BY id LIMIT $3`,
		after, EventPullRequestMerged, limit)
}

// RoutineRunStore is what a routine's run reads and writes besides the
// task rows: the runs and the project it files on.
type RoutineRunStore interface {
	RoutineStore
	Project(ctx context.Context, name string) (Project, bool, error)
}
