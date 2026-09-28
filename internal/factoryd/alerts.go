package factoryd

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Alert kinds (ruling 140): an activity past its policy bound, a session
// supervisor trip, a human escalation.
const (
	AlertGateBound      = "gate_bound"
	AlertSupervisorTrip = "supervisor_trip"
	AlertEscalation     = "escalation"
)

// An alert's verdict: it pointed at a real problem, or it was noise.
const (
	VerdictReal  = "real"
	VerdictNoise = "noise"
)

// Alert is an alerts row. A machine that decides at once (the supervisor)
// writes the verdict with the row; an escalation's verdict is a human's.
type Alert struct {
	ID         int64
	Kind       string
	Source     string // gate_bound: the activity; supervisor_trip: the detector; escalation: agent or supervisor
	TaskID     string
	RaisedAt   time.Time
	BoundS     int // gate_bound only
	ResolvedAt *time.Time
	Verdict    string // "" while open
	ResolvedBy string
	Detail     map[string]any
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
	if strings.HasPrefix(reason, agentAsks) {
		return "agent"
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
		`INSERT INTO alerts (kind, source, task_id, bound_s, verdict, resolved_by, resolved_at, detail)
		 VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, 0), NULLIF($5, ''), NULLIF($6, ''), CASE WHEN $5 <> '' THEN now() END, $7::jsonb)
		 RETURNING id`, a.Kind, a.Source, a.TaskID, a.BoundS, a.Verdict, a.ResolvedBy, detail).Scan(&id)
	return id, err
}

// Alerts lists the newest limit alerts, newest first.
func (s *PGStore) Alerts(ctx context.Context, limit int) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, source, COALESCE(task_id, ''), raised_at, COALESCE(bound_s, 0), resolved_at,
		        COALESCE(verdict, ''), COALESCE(resolved_by, ''), detail
		 FROM alerts ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		var detail []byte
		if err := rows.Scan(&a.ID, &a.Kind, &a.Source, &a.TaskID, &a.RaisedAt, &a.BoundS, &a.ResolvedAt,
			&a.Verdict, &a.ResolvedBy, &detail); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(detail, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAlertVerdict is a human's verdict on an open escalation alert;
// ErrNotFound when there is none (judged already, or a machine's alert).
func (s *PGStore) SetAlertVerdict(ctx context.Context, id int64, verdict, by string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET verdict = $2, resolved_by = $3, resolved_at = now()
		 WHERE id = $1 AND kind = $4 AND verdict IS NULL`, id, verdict, by, AlertEscalation)
	return oneRow(res, err, "open escalation alert")
}

// ResolveAlert is the gate supervisor's verdict on an open gate_bound
// alert, its action and diagnosis merged into the detail; ErrNotFound when
// there is none.
func (s *PGStore) ResolveAlert(ctx context.Context, id int64, verdict, by string, detail map[string]any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET verdict = $2, resolved_by = $3, resolved_at = now(), detail = detail || $4::jsonb
		 WHERE id = $1 AND kind = $5 AND verdict IS NULL`, id, verdict, by, b, AlertGateBound)
	return oneRow(res, err, "open gate_bound alert")
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
