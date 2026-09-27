// Package cloudres is the registry of cloud resources the factory creates
// (table cloud_resources, factoryd's migrations) and the collector that
// deletes what is expired, finished or unknown. A row is written before the
// provider create call, so every resource the factory made is findable by
// its `resource` label even when the process dies before recording the
// provider id.
package cloudres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	StateRequested = "requested"
	StateLive      = "live"
	StateDeleting  = "deleting"
	StateDeleted   = "deleted"
	StateVanished  = "vanished"

	KindServer   = "server"
	KindFirewall = "firewall"
)

// OpenStates are the states whose resource may exist at the provider.
var OpenStates = []string{StateRequested, StateLive, StateDeleting}

// allowedFrom maps a target state to the states it may be entered from.
// requested -> deleted is a create that definitively failed.
var allowedFrom = map[string][]string{
	StateLive:     {StateRequested},
	StateDeleting: {StateRequested, StateLive},
	StateDeleted:  {StateRequested, StateDeleting},
	StateVanished: {StateRequested, StateLive, StateDeleting},
}

type Resource struct {
	ID              int64
	Project         string // ci | agent
	Kind            string // server | firewall | ...
	ProviderID      string
	Name            string
	Labels          map[string]string
	OwnerRef        string // ci: the Forgejo job id
	PricePerHourEUR float64
	CreatedAt       time.Time
	LeaseExpires    time.Time
	State           string
	DeletedAt       *time.Time
}

// Registry is the durable record. Postgres in production; Memory in tests.
type Registry interface {
	// Insert records r in state requested and returns it with its id and
	// its labels completed by factory=<project> and resource=<id>: the
	// labels the provider resource must carry.
	Insert(ctx context.Context, r Resource) (Resource, error)
	// SetLive records the provider id of a requested row.
	SetLive(ctx context.Context, id int64, providerID string) (bool, error)
	// Transition moves a row to `to` if its current state allows it and
	// reports whether it moved; entering deleted or vanished stamps
	// deleted_at.
	Transition(ctx context.Context, id int64, to string) (bool, error)
	// Open lists the project's rows in an open state, oldest first.
	Open(ctx context.Context, project string) ([]Resource, error)
	// OpenByOwner lists the open rows of one owner, oldest first.
	OpenByOwner(ctx context.Context, project, ownerRef string) ([]Resource, error)
}

var errUnknownState = errors.New("cloudres: unknown target state")

// ProviderLabels are the labels Insert adds to every resource.
func providerLabels(r Resource, id int64) map[string]string {
	out := make(map[string]string, len(r.Labels)+2)
	for k, v := range r.Labels {
		out[k] = v
	}
	out["factory"] = r.Project
	out["resource"] = strconv.FormatInt(id, 10)
	return out
}

// PG is the Postgres registry.
type PG struct{ DB *sql.DB }

func (p PG) Insert(ctx context.Context, r Resource) (Resource, error) {
	var id int64
	if err := p.DB.QueryRowContext(ctx, `SELECT nextval('cloud_resources_id_seq')`).Scan(&id); err != nil {
		return Resource{}, err
	}
	r.ID = id
	r.Labels = providerLabels(r, id)
	r.State = StateRequested
	labels, err := json.Marshal(r.Labels)
	if err != nil {
		return Resource{}, err
	}
	err = p.DB.QueryRowContext(ctx,
		`INSERT INTO cloud_resources (id, project, kind, name, labels, owner_ref, price_per_hour_eur, lease_expires, state)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9) RETURNING created_at`,
		id, r.Project, r.Kind, r.Name, labels, r.OwnerRef, r.PricePerHourEUR, r.LeaseExpires, r.State).Scan(&r.CreatedAt)
	if err != nil {
		return Resource{}, err
	}
	return r, nil
}

func (p PG) SetLive(ctx context.Context, id int64, providerID string) (bool, error) {
	res, err := p.DB.ExecContext(ctx,
		`UPDATE cloud_resources SET state = 'live', provider_id = $2 WHERE id = $1 AND state = 'requested'`, id, providerID)
	return affected(res, err)
}

func (p PG) Transition(ctx context.Context, id int64, to string) (bool, error) {
	from, ok := allowedFrom[to]
	if !ok {
		return false, errUnknownState
	}
	res, err := p.DB.ExecContext(ctx,
		`UPDATE cloud_resources SET state = $2,
		        deleted_at = CASE WHEN $2 IN ('deleted', 'vanished') THEN now() ELSE deleted_at END
		 WHERE id = $1 AND state = ANY($3)`, id, to, from)
	return affected(res, err)
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const resourceColumns = `id, project, kind, provider_id, name, labels, owner_ref, price_per_hour_eur::float8,
	created_at, lease_expires, state, deleted_at`

func (p PG) Open(ctx context.Context, project string) ([]Resource, error) {
	return p.query(ctx, `SELECT `+resourceColumns+` FROM cloud_resources
		WHERE project = $1 AND state = ANY($2) ORDER BY id`, project, OpenStates)
}

func (p PG) OpenByOwner(ctx context.Context, project, ownerRef string) ([]Resource, error) {
	return p.query(ctx, `SELECT `+resourceColumns+` FROM cloud_resources
		WHERE project = $1 AND owner_ref = $2 AND state = ANY($3) ORDER BY id`, project, ownerRef, OpenStates)
}

// Get reads one row; false when it does not exist.
func (p PG) Get(ctx context.Context, id int64) (Resource, bool, error) {
	out, err := p.query(ctx, `SELECT `+resourceColumns+` FROM cloud_resources WHERE id = $1`, id)
	if err != nil || len(out) == 0 {
		return Resource{}, false, err
	}
	return out[0], true, nil
}

func (p PG) query(ctx context.Context, q string, args ...any) ([]Resource, error) {
	rows, err := p.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		var r Resource
		var labels []byte
		if err := rows.Scan(&r.ID, &r.Project, &r.Kind, &r.ProviderID, &r.Name, &labels, &r.OwnerRef,
			&r.PricePerHourEUR, &r.CreatedAt, &r.LeaseExpires, &r.State, &r.DeletedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(labels, &r.Labels); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Memory is the in-memory Registry for tests.
type Memory struct {
	mu   sync.Mutex
	rows map[int64]Resource
	Now  func() time.Time // time.Now when nil
}

func NewMemory() *Memory { return &Memory{rows: map[int64]Resource{}} }

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) Insert(ctx context.Context, r Resource) (Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.ID = int64(len(m.rows) + 1)
	r.Labels = providerLabels(r, r.ID)
	r.State = StateRequested
	r.CreatedAt = m.now()
	m.rows[r.ID] = r
	return r, nil
}

func (m *Memory) SetLive(ctx context.Context, id int64, providerID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || r.State != StateRequested {
		return false, nil
	}
	r.State, r.ProviderID = StateLive, providerID
	m.rows[id] = r
	return true, nil
}

func (m *Memory) Transition(ctx context.Context, id int64, to string) (bool, error) {
	from, ok := allowedFrom[to]
	if !ok {
		return false, errUnknownState
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok || !slices.Contains(from, r.State) {
		return false, nil
	}
	r.State = to
	if to == StateDeleted || to == StateVanished {
		t := m.now()
		r.DeletedAt = &t
	}
	m.rows[id] = r
	return true, nil
}

func (m *Memory) Open(ctx context.Context, project string) ([]Resource, error) {
	return m.filter(func(r Resource) bool { return r.Project == project && slices.Contains(OpenStates, r.State) }), nil
}

func (m *Memory) OpenByOwner(ctx context.Context, project, ownerRef string) ([]Resource, error) {
	return m.filter(func(r Resource) bool {
		return r.Project == project && r.OwnerRef == ownerRef && slices.Contains(OpenStates, r.State)
	}), nil
}

// Get reads one row.
func (m *Memory) Get(id int64) (Resource, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	return r, ok
}

func (m *Memory) filter(keep func(Resource) bool) []Resource {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Resource
	for _, r := range m.rows {
		if keep(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
