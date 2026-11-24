package factoryd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type IncidentKind string

const (
	IncidentGuardKill    IncidentKind = "guard_kill"
	IncidentKernelOOM    IncidentKind = "kernel_oom"
	IncidentRestart      IncidentKind = "restart"
	IncidentNodeBoot     IncidentKind = "node_boot"
	IncidentNodeNotReady IncidentKind = "node_not_ready"
)

const IncidentRetention = 30 * 24 * time.Hour

// PlatformIncident is one thing the platform did, once per Key. Until is
// nil for an incident of one moment.
type PlatformIncident struct {
	ID                               int64
	Key                              string
	At                               time.Time
	Until                            *time.Time
	Kind                             IncidentKind
	Subject, Node, Namespace, PodUID string
	Detail                           map[string]any
}

// PodDeath is a pod's death record. Kind is "session", "ci" or "gate"; the
// key is session/<task>/<session>, ci/<runner> or gate/<pod uid>.
type PodDeath struct {
	Kind                      string
	TaskID, SessionID, Runner string
	Record                    DeathRecord
}

func (d PodDeath) Key() (string, error) {
	switch {
	case d.Kind == "session" && d.TaskID != "" && d.SessionID != "":
		return "session/" + d.TaskID + "/" + d.SessionID, nil
	case d.Kind == "ci" && d.Runner != "":
		return "ci/" + d.Runner, nil
	case d.Kind == "gate" && d.Record.PodUID != "":
		return "gate/" + d.Record.PodUID, nil
	}
	return "", fmt.Errorf("pod death of kind %q lacks the parts of its key", d.Kind)
}

type ClusterStore interface {
	// AddPlatformIncidents writes each incident once per key, and the end
	// of one written without it; it answers how many rows it wrote.
	AddPlatformIncidents(ctx context.Context, in []PlatformIncident) (int, error)
	PlatformIncidents(ctx context.Context, from, to time.Time, limit int) ([]PlatformIncident, error)
	PodIncidents(ctx context.Context, podUID string) ([]PlatformIncident, error)
	PruneClusterFacts(ctx context.Context, age time.Duration) error
	PutPodDeath(ctx context.Context, d PodDeath) error
	SessionDeath(ctx context.Context, taskID, sessionID string) (DeathRecord, bool, error)
	CIDeath(ctx context.Context, runner string) (DeathRecord, bool, error)
	PlatformDeaths(ctx context.Context, since time.Time) ([]PodDeath, error)
}

var _ ClusterStore = (*PGStore)(nil)

func (s *PGStore) AddPlatformIncidents(ctx context.Context, in []PlatformIncident) (int, error) {
	added := 0
	for _, p := range in {
		detail, err := json.Marshal(p.Detail)
		if err != nil {
			return added, err
		}
		if p.Detail == nil {
			detail = []byte("{}")
		}
		res, err := s.db.ExecContext(ctx, `INSERT INTO platform_incidents (key, at, until, kind, subject, node, namespace, pod_uid, detail)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (key) DO UPDATE SET until = EXCLUDED.until WHERE platform_incidents.until IS NULL AND EXCLUDED.until IS NOT NULL`,
			p.Key, p.At, p.Until, string(p.Kind), p.Subject, p.Node, p.Namespace, p.PodUID, detail)
		if err != nil {
			return added, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return added, err
		}
		added += int(n)
	}
	return added, nil
}

const incidentSelect = `SELECT id, key, at, until, kind, subject, node, namespace, pod_uid, detail FROM platform_incidents `

func (s *PGStore) incidents(ctx context.Context, query string, args ...any) ([]PlatformIncident, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformIncident
	for rows.Next() {
		var p PlatformIncident
		var kind string
		var detail []byte
		if err := rows.Scan(&p.ID, &p.Key, &p.At, &p.Until, &kind, &p.Subject, &p.Node, &p.Namespace, &p.PodUID, &detail); err != nil {
			return nil, err
		}
		p.Kind = IncidentKind(kind)
		if err := json.Unmarshal(detail, &p.Detail); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PlatformIncidents returns the incidents that overlap [from, to], oldest
// first; limit <= 0 means all.
func (s *PGStore) PlatformIncidents(ctx context.Context, from, to time.Time, limit int) ([]PlatformIncident, error) {
	return s.incidents(ctx, incidentSelect+`WHERE at <= $1 AND coalesce(until, at) >= $2 ORDER BY at, id LIMIT CASE WHEN $3 > 0 THEN $3 END`, to, from, limit)
}

func (s *PGStore) PodIncidents(ctx context.Context, podUID string) ([]PlatformIncident, error) {
	if podUID == "" {
		return nil, nil
	}
	return s.incidents(ctx, incidentSelect+`WHERE pod_uid = $1 ORDER BY at, id`, podUID)
}

// PruneClusterFacts deletes the incidents that ended before now-age, except
// each node's newest node_boot row: a node that stays up keeps its boot. It
// deletes the CI and gate deaths older than age too, which only the checks
// gate and the platform card read; a session's death stays for
// cluster_facts.
func (s *PGStore) PruneClusterFacts(ctx context.Context, age time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM platform_incidents p WHERE coalesce(p.until, p.at) < now() - make_interval(secs => $1)
		AND NOT (p.kind = 'node_boot' AND NOT EXISTS (
			SELECT 1 FROM platform_incidents q WHERE q.kind = 'node_boot' AND q.subject = p.subject AND (q.at, q.id) > (p.at, p.id)))`, age.Seconds()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM pod_deaths WHERE kind IN ('ci', 'gate') AND at < now() - make_interval(secs => $1)`, age.Seconds())
	return err
}

func (s *PGStore) PutPodDeath(ctx context.Context, d PodDeath) error {
	key, err := d.Key()
	if err != nil {
		return err
	}
	record, err := json.Marshal(d.Record)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pod_deaths (key, kind, task_id, session_id, runner, at, class, record)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8)
		ON CONFLICT (key) DO UPDATE SET kind = EXCLUDED.kind, task_id = EXCLUDED.task_id, session_id = EXCLUDED.session_id,
		runner = EXCLUDED.runner, at = EXCLUDED.at, class = EXCLUDED.class, record = EXCLUDED.record`,
		key, d.Kind, d.TaskID, d.SessionID, d.Runner, d.Record.At, string(d.Record.Class), record)
	return err
}

func (s *PGStore) deathByKey(ctx context.Context, key string) (DeathRecord, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT record FROM pod_deaths WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DeathRecord{}, false, nil
	}
	if err != nil {
		return DeathRecord{}, false, err
	}
	var r DeathRecord
	return r, true, json.Unmarshal(raw, &r)
}

func (s *PGStore) SessionDeath(ctx context.Context, taskID, sessionID string) (DeathRecord, bool, error) {
	return s.deathByKey(ctx, "session/"+taskID+"/"+sessionID)
}

func (s *PGStore) CIDeath(ctx context.Context, runner string) (DeathRecord, bool, error) {
	return s.deathByKey(ctx, "ci/"+runner)
}

func (s *PGStore) PlatformDeaths(ctx context.Context, since time.Time) ([]PodDeath, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, coalesce(task_id, ''), session_id, runner, record FROM pod_deaths
		WHERE class = 'platform' AND at >= $1 ORDER BY at DESC, key COLLATE "C"`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PodDeath
	for rows.Next() {
		var d PodDeath
		var raw []byte
		if err := rows.Scan(&d.Kind, &d.TaskID, &d.SessionID, &d.Runner, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &d.Record); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
