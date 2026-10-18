package factoryd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// The factory's own dependency health (roadmap 299): the fsn1 outage of
// 2026-10-17 went dark on every provider feed while our own probes of our
// own dependencies told the truth, so the truth is all the factory records.

// DependencyState is one dependency's latest health, as the prober wrote it
// and the console's status panel reads it. The state is one of up, degraded
// and down; detail carries the probe's error or why it was slow.
type DependencyState struct {
	Name      string // "object storage", "provider api", "forgejo", "postgres", "temporal"
	State     string
	Detail    string
	LatencyMS int
	CheckedAt time.Time
}

// SetDependencyState overwrites the dependency's row: the latest state
// only, no history.
func (s *PGStore) SetDependencyState(ctx context.Context, d DependencyState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dependency_health (name, state, detail, latency_ms, checked_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (name) DO UPDATE SET
			state = EXCLUDED.state, detail = EXCLUDED.detail,
			latency_ms = EXCLUDED.latency_ms, checked_at = EXCLUDED.checked_at`,
		d.Name, d.State, d.Detail, d.LatencyMS, d.CheckedAt)
	return err
}

// DependencyStates lists every dependency's latest state, by name.
func (s *PGStore) DependencyStates(ctx context.Context) ([]DependencyState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, state, detail, latency_ms, checked_at FROM dependency_health ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []DependencyState
	for rows.Next() {
		var d DependencyState
		if err := rows.Scan(&d.Name, &d.State, &d.Detail, &d.LatencyMS, &d.CheckedAt); err != nil {
			return nil, err
		}
		all = append(all, d)
	}
	return all, rows.Err()
}

// ControlPlane is the one provider-specific probe: the cloud provider's
// control-plane API. The hcloud client answers it today; a provider switch
// is one config value (FACTORY_PROVIDER) in factoryd's main.
type ControlPlane interface {
	Probe(ctx context.Context) error
}

// healthProbeKey is the object the storage probe writes and reads back in
// the timelines' own bucket. It is never deleted: the next pass overwrites
// it.
const healthProbeKey = "health/probe.txt"

// DepProber exercises the factory's own dependencies on a clock and writes
// each one's latest state: the object store's data path both ways, the
// provider's control-plane API, Forgejo, Postgres and Temporal. A healthy
// dependency whispers: only the console's expanded status names it. Two
// consecutive failures mark it down, the first success after marks it up,
// and an answer slower than SlowAfter marks it degraded.
type DepProber struct {
	Store   *PGStore
	Objects ObjectStore
	Control ControlPlane
	Forgejo *Forgejo
	// Owner, RepoName and Branch name the factory's own repository, the
	// Forgejo probe's read.
	Owner, RepoName, Branch string
	PG                      func(ctx context.Context) error
	Temporal                func(ctx context.Context) error
	Interval                time.Duration // <= 0: a minute
	SlowAfter               time.Duration // <= 0: 2 s, the degraded threshold
	Cap                     time.Duration // <= 0: 10 s, one probe's deadline

	fails map[string]int
}

func (p *DepProber) interval() time.Duration {
	if p.Interval <= 0 {
		return time.Minute
	}
	return p.Interval
}

func (p *DepProber) slowAfter() time.Duration {
	if p.SlowAfter <= 0 {
		return 2 * time.Second
	}
	return p.SlowAfter
}

func (p *DepProber) cap() time.Duration {
	if p.Cap <= 0 {
		return 10 * time.Second
	}
	return p.Cap
}

// Run probes every Interval until ctx ends. Work comes first: the rows
// exist right after boot. A failing store write logs once until it
// recovers, like the roller's poll.
func (p *DepProber) Run(ctx context.Context) {
	tick := time.NewTicker(p.interval())
	defer tick.Stop()
	var last string
	for {
		if err := p.probe(ctx); err != nil && err.Error() != last {
			log.Printf("dependency prober: %v", err)
			last = err.Error()
		} else if err == nil {
			last = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// probe runs one pass over every dependency, serial, and joins the store
// writes' errors.
func (p *DepProber) probe(ctx context.Context) error {
	var errs []error
	one := func(name string, probe func(context.Context) error) {
		if err := p.one(ctx, name, probe); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	one("object storage", func(ctx context.Context) error {
		nonce := []byte("factoryd dependency probe " + time.Now().UTC().Format(time.RFC3339Nano))
		if err := p.Objects.Put(ctx, healthProbeKey, nonce); err != nil {
			return err
		}
		back, err := p.Objects.Get(ctx, healthProbeKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(nonce, back) {
			return fmt.Errorf("read back %q", clip(string(back), 200))
		}
		return nil
	})
	one("provider api", p.Control.Probe)
	one("forgejo", func(ctx context.Context) error {
		_, _, err := p.Forgejo.LatestCommit(ctx, p.Owner, p.RepoName, p.Branch, "k8s")
		return err
	})
	one("postgres", p.PG)
	one("temporal", p.Temporal)
	return errors.Join(errs...)
}

// one probes one dependency under the cap and writes its state by the flap
// rule. It returns the store write's error, never the probe's: a probe
// failure is state, not a fault of the prober.
func (p *DepProber) one(ctx context.Context, name string, probe func(context.Context) error) error {
	if p.fails == nil {
		p.fails = map[string]int{}
	}
	cctx, cancel := context.WithTimeout(ctx, p.cap())
	start := time.Now()
	err := probe(cctx)
	took := time.Since(start)
	cancel()
	d := DependencyState{Name: name, LatencyMS: int(took.Milliseconds()), CheckedAt: time.Now().UTC()}
	if err != nil {
		p.fails[name]++
		if p.fails[name] < 2 {
			return nil // the first failure whispers: the previous row stands
		}
		d.State, d.Detail = "down", clip(err.Error(), 300)
	} else {
		p.fails[name] = 0
		d.State = "up"
		if took > p.slowAfter() {
			d.State, d.Detail = "degraded", fmt.Sprintf("answered in %.1f s", took.Seconds())
		}
	}
	return p.Store.SetDependencyState(ctx, d)
}
