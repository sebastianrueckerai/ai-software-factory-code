package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Alert kinds (ruling 140): an activity past its policy bound, a session
// supervisor trip, a human escalation, an agent asking the issue author.
const (
	AlertGateBound      = "gate_bound"
	AlertSupervisorTrip = "supervisor_trip"
	AlertEscalation     = "escalation"
	AlertInfoRequest    = "info_request"
	// AlertSigNoz is a SigNoz alert in a project's namespace, filed as a
	// bug issue (Task 70).
	AlertSigNoz = "alert"
	// AlertConfigRejected is a commit of the configuration tree factoryd
	// refused, filed as a human-lane issue on the tree's repository.
	AlertConfigRejected = "config_rejected"
	// AlertScreening is the screening stage's suspect verdict (ruling 182):
	// real when a human closes the issue, noise when one lets it run.
	AlertScreening = "screening"
	// AlertMonthCap is the month's variable spend at 50, 80 and 100 percent
	// of budget.month_eur, one row each per UTC month (fingerprint
	// month/percent); the cap's also files a human-lane issue.
	AlertMonthCap = "month_cap"
)

// An alert's verdict: it pointed at a real problem, or it was noise.
const (
	VerdictReal  = "real"
	VerdictNoise = "noise"
	// VerdictUnjudged closes an escalation or info_request alert whose
	// task ended before an approver judged it.
	VerdictUnjudged = "unjudged"
)

// Alert is an alerts row. A machine that decides at once (the supervisor)
// writes the verdict with the row; an escalation's verdict is a human's.
type Alert struct {
	ID         int64
	Kind       string
	Source     string // gate_bound: the activity; supervisor_trip: the detector; escalation: agent, supervisor, cap (the session cap's card), month_cap or project_cap; month_cap: the percent
	TaskID     string
	RaisedAt   time.Time
	BoundS     int // gate_bound only
	ResolvedAt *time.Time
	Verdict    string // "" while open
	ResolvedBy string
	// Fingerprint is the SigNoz alert's (kind alert), the refused commit
	// (kind config_rejected).
	Fingerprint string
	Detail      map[string]any
}

// tripVerdict is a supervisor decision as an alert verdict: a continue or
// a gate's wait found the trip was noise, every other action acted on a
// real problem.
func tripVerdict(a SupervisorAction) string {
	if a == ActionContinue || a == ActionWait {
		return VerdictNoise
	}
	return VerdictReal
}

// escalationSource names who asked for the human.
func escalationSource(reason string) string {
	switch {
	case strings.HasPrefix(reason, agentAsks):
		return "agent"
	case strings.HasPrefix(reason, capReached):
		return "cap"
	case strings.HasPrefix(reason, monthCapReached):
		return SourceMonthCap
	case strings.HasPrefix(reason, projectCapReached):
		return "project_cap"
	}
	return "supervisor"
}

func (s *PGStore) InsertAlert(ctx context.Context, a Alert) (int64, error) {
	if a.Detail == nil {
		a.Detail = map[string]any{}
	}
	detail, err := json.Marshal(a.Detail)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO alerts (kind, source, task_id, bound_s, verdict, resolved_by, resolved_at, detail, fingerprint)
		 VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, 0), NULLIF($5, ''), NULLIF($6, ''), CASE WHEN $5 <> '' THEN now() END, $7::jsonb, NULLIF($8, ''))
		 ON CONFLICT DO NOTHING RETURNING id`, a.Kind, a.Source, a.TaskID, a.BoundS, a.Verdict, a.ResolvedBy, detail, a.Fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // the task's open screening alert, or the month's month_cap alert at the percent, exists
	}
	return id, err
}

const alertColumns = `id, kind, source, COALESCE(task_id, ''), raised_at, COALESCE(bound_s, 0), resolved_at,
	COALESCE(verdict, ''), COALESCE(resolved_by, ''), COALESCE(fingerprint, ''), detail`

func scanAlert(row interface{ Scan(...any) error }) (Alert, error) {
	var a Alert
	var detail []byte
	if err := row.Scan(&a.ID, &a.Kind, &a.Source, &a.TaskID, &a.RaisedAt, &a.BoundS, &a.ResolvedAt,
		&a.Verdict, &a.ResolvedBy, &a.Fingerprint, &detail); err != nil {
		return Alert{}, err
	}
	return a, json.Unmarshal(detail, &a.Detail)
}

// Alerts lists the newest limit alerts, newest first.
func (s *PGStore) Alerts(ctx context.Context, limit int) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+alertColumns+` FROM alerts ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlertByFingerprint is the newest SigNoz alert row of fingerprint.
func (s *PGStore) AlertByFingerprint(ctx context.Context, kind, fingerprint string) (Alert, bool, error) {
	a, err := scanAlert(s.db.QueryRowContext(ctx,
		`SELECT `+alertColumns+` FROM alerts WHERE fingerprint = $1 AND kind = $2 ORDER BY id DESC LIMIT 1`, fingerprint, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, false, nil
	}
	return a, err == nil, err
}

// MergeAlertDetail merges detail into alert id's.
func (s *PGStore) MergeAlertDetail(ctx context.Context, id int64, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE alerts SET detail = detail || $2::jsonb WHERE id = $1`, id, b)
	return oneRow(res, err, "alert")
}

// JudgeTaskAlerts sets verdict on task's alert rows of kind that have none.
func (s *PGStore) JudgeTaskAlerts(ctx context.Context, taskID, kind, verdict, by string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET verdict = $2, resolved_by = $3, resolved_at = now()
		 WHERE task_id = $1 AND kind = $4 AND verdict IS NULL`, taskID, verdict, by, kind)
	return err
}

// SetAlertVerdict is a human's verdict on an open escalation alert;
// ErrNotFound when there is none (judged already, or a machine's alert).
func (s *PGStore) SetAlertVerdict(ctx context.Context, id int64, verdict, by string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET verdict = $2, resolved_by = $3, resolved_at = now()
		 WHERE id = $1 AND kind = $4 AND verdict IS NULL`, id, verdict, by, AlertEscalation)
	return oneRow(res, err, "open escalation alert")
}

// ResolveAlert is a machine's verdict on an open alert of kind (the gate
// supervisor's on a gate_bound, a loaded commit's on a config_rejected),
// detail merged in; ErrNotFound when there is none.
func (s *PGStore) ResolveAlert(ctx context.Context, id int64, kind, verdict, by string, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET verdict = $2, resolved_by = $3, resolved_at = now(), detail = detail || $4::jsonb
		 WHERE id = $1 AND kind = $5 AND verdict IS NULL`, id, verdict, by, b, kind)
	return oneRow(res, err, "open "+kind+" alert")
}

// SetAlertTask links alert id to the task filed for it, detail merged in.
func (s *PGStore) SetAlertTask(ctx context.Context, id int64, taskID string, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE alerts SET task_id = $2, detail = detail || $3::jsonb WHERE id = $1`, id, taskID, b)
	return oneRow(res, err, "alert")
}

// AlertCount is the number of alerts of one kind and verdict ("open" for
// none yet).
type AlertCount struct {
	Kind, Verdict string
	N             int64
}

func (s *PGStore) AlertCounts(ctx context.Context) ([]AlertCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, coalesce(verdict, 'open'), count(*) FROM alerts GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertCount
	for rows.Next() {
		var c AlertCount
		if err := rows.Scan(&c.Kind, &c.Verdict, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
