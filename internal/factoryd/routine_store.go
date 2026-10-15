package factoryd

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SourceRoutine is the source of a card a routine's run files.
const SourceRoutine = "routine"

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
	Outcome          string // filed, held, skipped, no change, folded, merged, closed, failed, done
	Note             string
	CostEUR          float64
}

// RoutineStore keeps the routines' state rows, their runs and the merge
// pass's cursor.
type RoutineStore interface {
	// RoutineState reads the routine's row, creating it at zero when missing.
	RoutineState(ctx context.Context, project, routine string) (RoutineState, error)
	RoutineStates(ctx context.Context, project string) ([]RoutineState, error)
	// BumpRoutineMerges adds one merge to every state row of project and
	// returns them.
	BumpRoutineMerges(ctx context.Context, project string) ([]RoutineState, error)
	SetRoutineMerges(ctx context.Context, project, routine string, merges int) error
	SetRoutineSkip(ctx context.Context, project, routine string, skip bool, by string) error
	// BeginRoutineRun writes run Runs+1 of r's routine and returns it with
	// began true. A run of the same due returns that run with began false.
	// A pending Skip once is consumed: the new run's outcome is "skipped"
	// with the note "skipped once by <SkipBy>".
	BeginRoutineRun(ctx context.Context, r RoutineRun) (run RoutineRun, began bool, err error)
	EndRoutineRun(ctx context.Context, project, routine string, run int, outcome, note, taskID string, issue int64, costEUR float64) error
	// RoutineRuns are the routine's last limit runs, newest first.
	RoutineRuns(ctx context.Context, project, routine string, limit int) ([]RoutineRun, error)
	// RoutineRunOfTask is the run that filed the task.
	RoutineRunOfTask(ctx context.Context, taskID string) (RoutineRun, bool, error)
	// OpenRoutineTask is the newest task of the routine whose status is
	// not final.
	OpenRoutineTask(ctx context.Context, project, routine string) (Task, bool, error)
	RoutineMergeCursor(ctx context.Context) (int64, error)
	SetRoutineMergeCursor(ctx context.Context, id int64) error
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

// routineState reads the routine's row through db, creating it at zero
// first. FOR UPDATE holds the row to the end of BeginRoutineRun's
// transaction.
func routineState(ctx context.Context, db sqlRunner, project, routine string) (RoutineState, error) {
	if _, err := db.ExecContext(ctx, `INSERT INTO routine_state (project, routine) VALUES ($1, $2) ON CONFLICT DO NOTHING`, project, routine); err != nil {
		return RoutineState{}, err
	}
	return scanRoutineState(db.QueryRowContext(ctx, `SELECT `+routineStateColumns+` FROM routine_state
		WHERE project = $1 AND routine = $2 FOR UPDATE`, project, routine).Scan)
}

func (s *PGStore) RoutineState(ctx context.Context, project, routine string) (RoutineState, error) {
	return routineState(ctx, s.db, project, routine)
}

func (s *PGStore) RoutineStates(ctx context.Context, project string) ([]RoutineState, error) {
	return s.routineStates(ctx, `SELECT `+routineStateColumns+` FROM routine_state WHERE project = $1 ORDER BY routine`, project)
}

func (s *PGStore) BumpRoutineMerges(ctx context.Context, project string) ([]RoutineState, error) {
	return s.routineStates(ctx, `WITH bumped AS (UPDATE routine_state SET merges = merges + 1 WHERE project = $1
		RETURNING `+routineStateColumns+`) SELECT `+routineStateColumns+` FROM bumped ORDER BY routine`, project)
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

func (s *PGStore) BeginRoutineRun(ctx context.Context, r RoutineRun) (RoutineRun, bool, error) {
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
	if st.SkipNext {
		outcome, note = "skipped", "skipped once by "+st.SkipBy
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
	if _, err := tx.ExecContext(ctx, `UPDATE routine_state SET runs = runs + 1, skip_next = false, skip_by = ''
		WHERE project = $1 AND routine = $2`, r.Project, r.Routine); err != nil {
		return RoutineRun{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return RoutineRun{}, false, err
	}
	return run, true, nil
}

func (s *PGStore) EndRoutineRun(ctx context.Context, project, routine string, run int, outcome, note, taskID string, issue int64, costEUR float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE routine_runs SET ended = now(), outcome = $4, note = $5, task_id = $6, issue_number = $7, cost_eur = $8
		WHERE project = $1 AND routine = $2 AND run = $3`, project, routine, run, outcome, note, taskID, issue, costEUR)
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

// RoutineRunOfTask is the first run that names the task: the later runs
// held or folded into its open card name it too.
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

func (s *PGStore) SetRoutineMergeCursor(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE routine_merge_cursor SET last_event_id = $1`, id)
	return err
}
