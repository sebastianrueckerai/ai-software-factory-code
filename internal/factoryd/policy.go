package factoryd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// PolicyStore holds the loaded policy versions, the holds and the cost
// ledger.
type PolicyStore interface {
	// RecordPolicy stores doc under sha unless known and activates it.
	RecordPolicy(ctx context.Context, sha string, doc policy.Document) (policy.Version, bool, error)
	CurrentPolicy(ctx context.Context) (policy.Version, bool, error)
	Costs(ctx context.Context, from, to time.Time) (policy.Costs, error)
	// SweepHolds books the holds whose caller never released them.
	SweepHolds(ctx context.Context) (int, error)
}

// PolicyLoader turns the mounted policy file into policy versions: each new
// valid content becomes an immutable row and the version in force; invalid
// content is logged and the previous version stays in force. A version must
// let the agent capability classes call every model of the ladder
// (LadderModels), else a task would be refused mid-run.
type PolicyLoader struct {
	Path         string
	Store        PolicyStore
	LadderModels []string

	mu      sync.Mutex
	current policy.Version
	loaded  bool
	seen    string // sha256 of the content last handled, valid or not
}

// Boot makes the stored current version current, then loads the file. It
// fails only when no valid version exists at all. The stored version is
// validated again: one written under an older schema (no risk rules) is
// not a valid version for this binary.
func (l *PolicyLoader) Boot(ctx context.Context) error {
	v, ok, err := l.Store.CurrentPolicy(ctx)
	if err != nil {
		return fmt.Errorf("read current policy: %w", err)
	}
	if ok {
		err := v.Document.Validate()
		if err == nil {
			err = l.fitsLadder(v.Document)
		}
		if err != nil {
			log.Printf("POLICY REJECTED: stored version %d: %v", v.ID, err)
			ok = false
		} else {
			l.mu.Lock()
			l.current, l.loaded = v, true
			l.mu.Unlock()
		}
	}
	if err := l.Load(ctx); err != nil && !ok {
		return fmt.Errorf("no valid policy version: %w", err)
	}
	return nil
}

// Load reads the file and records it if its content changed since the last
// call. It logs every failure itself.
func (l *PolicyLoader) Load(ctx context.Context) error {
	b, err := os.ReadFile(l.Path)
	if err != nil {
		return l.reject("unreadable", err)
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	l.mu.Lock()
	seen := l.seen
	l.mu.Unlock()
	if sha == seen {
		return nil
	}
	doc, err := policy.Parse(b)
	if err == nil {
		err = l.fitsLadder(doc)
	}
	if err != nil {
		l.mu.Lock()
		l.seen = sha
		l.mu.Unlock()
		return l.reject("invalid (sha256 "+sha+")", err)
	}
	v, inserted, err := l.Store.RecordPolicy(ctx, sha, doc)
	if err != nil {
		log.Printf("policy: record sha256 %s: %v", sha, err)
		return fmt.Errorf("record policy: %w", err)
	}
	l.mu.Lock()
	l.current, l.loaded, l.seen = v, true, sha
	l.mu.Unlock()
	how := "loaded"
	if !inserted {
		how = "current (content known)"
	}
	log.Printf("policy: version %d %s, sha256 %s", v.ID, how, v.SHA256)
	return nil
}

func (l *PolicyLoader) reject(what string, err error) error {
	l.mu.Lock()
	cur, ok := l.current, l.loaded
	l.mu.Unlock()
	stays := "no version is current"
	if ok {
		stays = fmt.Sprintf("version %d stays current", cur.ID)
	}
	log.Printf("POLICY REJECTED: %s %s: %v; %s", l.Path, what, err, stays)
	return err
}

// Watch reloads the file every interval; kubelet updates a mounted
// ConfigMap in place.
func (l *PolicyLoader) Watch(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			l.Load(ctx) // #nosec G104 -- Load logs every failure itself
		}
	}
}

// Current is the version in force; Boot guarantees there is one.
func (l *PolicyLoader) Current() policy.Version {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

func (l *PolicyLoader) Budget() policy.Budget { return l.Current().Document.Budget }

func (l *PolicyLoader) fitsLadder(d policy.Document) error {
	for _, m := range l.LadderModels {
		if !d.AllowsModel(AgentCapabilities, m) {
			return fmt.Errorf("capability classes %q do not allow the ladder model %q", AgentCapabilities, m)
		}
	}
	return nil
}

// SweepLoop books expired holds every interval until ctx ends.
func SweepLoop(ctx context.Context, store PolicyStore, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := store.SweepHolds(ctx)
			if err != nil {
				log.Printf("sweep holds: %v", err)
			}
			if n > 0 {
				log.Printf("sweep holds: booked %d expired holds at their full amount", n)
			}
		}
	}
}

// PolicyHandler serves GET /policy: the version in force.
func PolicyHandler(store PolicyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok, err := store.CurrentPolicy(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no policy version loaded", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, v)
	}
}

// maxCostDays bounds one GET /costs range.
const maxCostDays = 366

// CostsHandler serves GET /costs?from=YYYY-MM-DD&to=YYYY-MM-DD (UTC days,
// both included, each defaulting to today): per-day totals by kind and
// per-task totals from the ledger.
func CostsHandler(store PolicyStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		today := time.Now().UTC().Format(policy.DateLayout)
		day := func(name string) (time.Time, error) {
			v := r.URL.Query().Get(name)
			if v == "" {
				v = today
			}
			t, err := time.Parse(policy.DateLayout, v)
			if err != nil {
				return t, fmt.Errorf("%s: want YYYY-MM-DD", name)
			}
			return t, nil
		}
		from, err := day("from")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		to, err := day("to")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if to.Before(from) || to.Sub(from) > maxCostDays*24*time.Hour {
			http.Error(w, fmt.Sprintf("want from <= to, at most %d days apart", maxCostDays), http.StatusBadRequest)
			return
		}
		c, err := store.Costs(r.Context(), from, to)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, c)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) // #nosec G104 -- best-effort response write
}

// the PGStore side of PolicyStore

func (s *PGStore) RecordPolicy(ctx context.Context, sha string, doc policy.Document) (policy.Version, bool, error) {
	return policy.Record(ctx, s.db, sha, doc)
}

func (s *PGStore) CurrentPolicy(ctx context.Context) (policy.Version, bool, error) {
	return policy.Current(ctx, s.db)
}

func (s *PGStore) SweepHolds(ctx context.Context) (int, error) {
	return policy.SweepHolds(ctx, s.db)
}

func (s *PGStore) Costs(ctx context.Context, from, to time.Time) (policy.Costs, error) {
	return policy.LedgerCosts(ctx, s.db, from, to)
}

var _ PolicyStore = (*PGStore)(nil)

// Aliases are the ladder's models as the gateway sees them: without the
// opencode provider prefix ("factory/coding-cheap" is "coding-cheap").
func (l ModelLadder) Aliases() []string {
	var out []string
	for _, m := range []string{l.Start, l.Stronger} {
		if _, alias, ok := strings.Cut(m, "/"); ok {
			m = alias
		}
		out = append(out, m)
	}
	return out
}
