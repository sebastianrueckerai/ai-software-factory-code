package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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
	// OpenBao, when set, probes the secret store (OpenBaoUnsealed).
	OpenBao   func(ctx context.Context) error
	Interval  time.Duration // <= 0: a minute
	SlowAfter time.Duration // <= 0: 2 s, the degraded threshold
	Cap       time.Duration // <= 0: 10 s, one probe's deadline

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

// depProbe is one dependency's probe.
type depProbe struct {
	name string
	run  func(context.Context) error
}

// probes are the dependencies' probes in the order the pass runs them.
func (p *DepProber) probes() []depProbe {
	probes := []depProbe{
		{"object storage", func(ctx context.Context) error {
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
		}},
		{"provider api", p.Control.Probe},
		{"forgejo", func(ctx context.Context) error {
			_, _, err := p.Forgejo.LatestCommit(ctx, p.Owner, p.RepoName, p.Branch, "k8s")
			return err
		}},
		{"postgres", p.PG},
		{"temporal", p.Temporal},
	}
	if p.OpenBao != nil {
		probes = append(probes, depProbe{"openbao", p.OpenBao})
	}
	return probes
}

// probe runs one pass over every dependency, serial, and joins the store
// writes' errors.
func (p *DepProber) probe(ctx context.Context) error {
	var errs []error
	for _, d := range p.probes() {
		if err := p.one(ctx, d.name, d.run); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.name, err))
		}
	}
	return errors.Join(errs...)
}

// Check runs every probe once now, each under the cap, and answers the
// failing ones as "<dependency>: <error>". It writes no state and applies no
// flap rule: a judgment asks whether the dependencies answer at this
// moment.
func (p *DepProber) Check(ctx context.Context) []string {
	var failing []string
	for _, d := range p.probes() {
		cctx, cancel := context.WithTimeout(ctx, p.cap())
		err := d.run(cctx)
		cancel()
		if err != nil {
			failing = append(failing, d.name+": "+clip(err.Error(), 300))
		}
	}
	return failing
}

// OpenBaoUnsealed is the probe of an OpenBao at baseURL: its seal status
// answers sealed false. A sealed OpenBao holds back every secret.
func OpenBaoUnsealed(baseURL string, client *http.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/v1/sys/seal-status", nil)
		if err != nil {
			return err
		}
		if client == nil {
			client = http.DefaultClient
		}
		res, err := client.Do(req) // #nosec G704 -- the configured OpenBao address
		if err != nil {
			return err
		}
		defer res.Body.Close()
		var st struct {
			Sealed *bool `json:"sealed"`
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&st); err != nil || res.StatusCode != http.StatusOK || st.Sealed == nil {
			return fmt.Errorf("seal status: status %d, no sealed flag", res.StatusCode)
		}
		if *st.Sealed {
			return errors.New("sealed")
		}
		return nil
	}
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
