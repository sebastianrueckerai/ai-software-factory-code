package factoryd

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"
)

// fakeClusterStore is the ClusterStore in memory, with the semantics of
// the Postgres one.
type fakeClusterStore struct {
	mu        sync.Mutex
	nextID    int64
	incidents []PlatformIncident
	deaths    map[string]PodDeath
}

var _ ClusterStore = (*fakeClusterStore)(nil)

func newFakeClusterStore() *fakeClusterStore {
	return &fakeClusterStore{deaths: map[string]PodDeath{}}
}

func (f *fakeClusterStore) AddPlatformIncidents(_ context.Context, in []PlatformIncident) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	added := 0
	for _, p := range in {
		if slices.ContainsFunc(f.incidents, func(q PlatformIncident) bool { return q.Key == p.Key }) {
			continue
		}
		f.nextID++
		p.ID = f.nextID
		p.Detail = maps.Clone(p.Detail)
		f.incidents = append(f.incidents, p)
		added++
	}
	return added, nil
}

func (f *fakeClusterStore) matching(keep func(PlatformIncident) bool) []PlatformIncident {
	var out []PlatformIncident
	for _, p := range f.incidents {
		if keep(p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b PlatformIncident) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func (f *fakeClusterStore) PlatformIncidents(_ context.Context, from, to time.Time, limit int) ([]PlatformIncident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.matching(func(p PlatformIncident) bool {
		end := p.At
		if p.Until != nil {
			end = *p.Until
		}
		return !p.At.After(to) && !end.Before(from)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeClusterStore) PodIncidents(_ context.Context, podUID string) ([]PlatformIncident, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if podUID == "" {
		return nil, nil
	}
	return f.matching(func(p PlatformIncident) bool { return p.PodUID == podUID }), nil
}

func (f *fakeClusterStore) PrunePlatformIncidents(_ context.Context, age time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cutoff := time.Now().Add(-age)
	f.incidents = slices.DeleteFunc(f.incidents, func(p PlatformIncident) bool {
		end := p.At
		if p.Until != nil {
			end = *p.Until
		}
		return end.Before(cutoff)
	})
	return nil
}

func (f *fakeClusterStore) PutPodDeath(_ context.Context, d PodDeath) error {
	key, err := d.Key()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deaths[key] = d
	return nil
}

func (f *fakeClusterStore) death(key string) (DeathRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.deaths[key]
	return d.Record, ok, nil
}

func (f *fakeClusterStore) SessionDeath(_ context.Context, taskID, sessionID string) (DeathRecord, bool, error) {
	return f.death("session/" + taskID + "/" + sessionID)
}

func (f *fakeClusterStore) CIDeath(_ context.Context, runner string) (DeathRecord, bool, error) {
	return f.death("ci/" + runner)
}

func (f *fakeClusterStore) PlatformDeaths(_ context.Context, since time.Time) ([]PodDeath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PodDeath
	for _, d := range f.deaths {
		if d.Record.Class == DeathPlatform && !d.Record.At.Before(since) {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b PodDeath) int { return b.Record.At.Compare(a.Record.At) })
	return out, nil
}
