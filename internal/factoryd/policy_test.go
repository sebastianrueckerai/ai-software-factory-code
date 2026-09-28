package factoryd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const testPolicyYAML = `version: 1
budget:
  task_default_eur: 2
  task_max_eur: 5
  day_eur: 20
classes:
  coder:
    models: [coding-cheap, coding-frontier]
    tools: {}
risk:
  classes:
    - {name: forbidden, outcome: forbidden}
    - {name: migration, outcome: requires_approval, migration: true}
    - {name: high, outcome: requires_approval}
    - {name: normal, outcome: requires_approval}
  rules:
    - {class: forbidden, paths: [".forgejo/**", ".gitea/**", ".github/**"]}
    - {class: migration, paths: ["internal/*/migrations/**", "internal/policy/schema.sql"]}
    - {class: high, paths: ["k8s/**", "**/Dockerfile*", "go.mod", "go.sum"]}
  default: normal
envelope_default: {monthly_eur: 10, server_types: [cx23], max_count: 2, max_lease_hours: 4, volume_max_gb: 20}
`

// memPolicy is the in-memory PolicyStore: versions keyed by sha, in order.
type memPolicy struct {
	mu       sync.Mutex
	versions []policy.Version
	current  int64
	records  int
	costs    [][2]time.Time
	// free is the task's and the day's free headroom TakeHold fits into
	taskFree, dayFree float64
	holds             []policy.HoldRequest
	released          []releasedHold
	sweeps            int
}

type releasedHold struct {
	hold policy.Hold
	cost *float64
}

func (m *memPolicy) TakeHold(_ context.Context, r policy.HoldRequest) (policy.Hold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holds = append(m.holds, r)
	h, err := policy.Fit(m.taskFree, m.dayFree, r)
	h.ID = int64(len(m.holds))
	return h, err
}

func (m *memPolicy) ReleaseHold(_ context.Context, h policy.Hold, cost *float64) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.released = append(m.released, releasedHold{h, cost})
	return 0, nil
}

func (m *memPolicy) SweepHolds(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweeps++
	return 0, nil
}

func (m *memPolicy) RecordPolicy(_ context.Context, sha string, doc policy.Document) (policy.Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records++
	for _, v := range m.versions {
		if v.SHA256 == sha {
			m.current = v.ID
			return v, false, nil
		}
	}
	v := policy.Version{ID: int64(len(m.versions) + 1), SHA256: sha, Document: doc, LoadedAt: time.Now()}
	m.versions = append(m.versions, v)
	m.current = v.ID
	return v, true, nil
}

func (m *memPolicy) CurrentPolicy(context.Context) (policy.Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == 0 {
		return policy.Version{}, false, nil
	}
	return m.versions[m.current-1], true, nil
}

func (m *memPolicy) Costs(_ context.Context, from, to time.Time) (policy.Costs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.costs = append(m.costs, [2]time.Time{from, to})
	return policy.Costs{From: from.Format(policy.DateLayout), To: to.Format(policy.DateLayout)}, nil
}

func writePolicy(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyLoader(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	store := &memPolicy{}
	l := &PolicyLoader{Path: path, Store: store}

	writePolicy(t, path, testPolicyYAML)
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 1 || cur.Document.Budget.DayEUR != 20 {
		t.Fatalf("after boot current = %+v, want version 1", cur)
	}

	// same content: no-op, the store is not even asked
	if err := l.Load(ctx); err != nil || store.records != 1 {
		t.Fatalf("reload of the same content = %v with %d records, want nil with 1", err, store.records)
	}

	// new content: a new version, current
	writePolicy(t, path, strings.Replace(testPolicyYAML, "day_eur: 20", "day_eur: 7", 1))
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 2 || cur.Document.Budget.DayEUR != 7 {
		t.Fatalf("after edit current = %+v, want version 2 with day_eur 7", cur)
	}
	if l.DayEUR() != 7 {
		t.Fatalf("DayEUR = %v, want 7", l.DayEUR())
	}

	// invalid content: rejected, version 2 stays current, and not retried
	writePolicy(t, path, testPolicyYAML+"surprise: true\n")
	if err := l.Load(ctx); err == nil {
		t.Fatal("invalid policy loaded")
	}
	if cur := l.Current(); cur.ID != 2 {
		t.Fatalf("after invalid edit current = %d, want 2", cur.ID)
	}
	if err := l.Load(ctx); err != nil || store.records != 2 {
		t.Fatalf("second look at the same invalid content = %v with %d records, want nil with 2", err, store.records)
	}

	// revert: the known version is current again, no new row
	writePolicy(t, path, testPolicyYAML)
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 1 || len(store.versions) != 2 {
		t.Fatalf("after revert current = %d with %d versions, want 1 with 2", cur.ID, len(store.versions))
	}

	// a restart with an invalid file keeps the stored current version
	writePolicy(t, path, "version: 2\n")
	restarted := &PolicyLoader{Path: path, Store: store}
	if err := restarted.Boot(ctx); err != nil {
		t.Fatalf("boot with a stored version and an invalid file = %v, want nil", err)
	}
	if cur := restarted.Current(); cur.ID != 1 {
		t.Fatalf("restarted current = %d, want 1", cur.ID)
	}
}

// A version whose coder class leaves out a ladder model is rejected: the
// ladder's next rung would be refused mid-task.
func TestPolicyLoaderKeepsTheLadderAllowed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	store := &memPolicy{}
	ladder := ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}
	if got := ladder.Aliases(); !slices.Equal(got, []string{"coding-cheap", "coding-frontier"}) {
		t.Fatalf("aliases = %v", got)
	}
	l := &PolicyLoader{Path: path, Store: store, LadderModels: ladder.Aliases()}
	narrow := strings.Replace(testPolicyYAML, "[coding-cheap, coding-frontier]", "[coding-cheap]", 1)

	// the only valid version leaves out a rung: no boot
	writePolicy(t, path, narrow)
	if err := l.Boot(ctx); err == nil || store.records != 0 {
		t.Fatalf("boot with a policy that drops coding-frontier = %v after %d records, want a refusal", err, store.records)
	}
	writePolicy(t, path, testPolicyYAML)
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	// a later edit that drops a rung is rejected, version 1 stays
	writePolicy(t, path, narrow)
	if err := l.Load(ctx); err == nil || !strings.Contains(err.Error(), "coding-frontier") || l.Current().ID != 1 {
		t.Fatalf("edit dropping a rung = %v with version %d current, want a rejection with 1", err, l.Current().ID)
	}
	// a stored version that drops a rung does not count as valid at boot
	store.RecordPolicy(ctx, "narrow", func() policy.Document { d, _ := policy.Parse([]byte(narrow)); return d }()) // #nosec G104 -- the fake never fails
	restarted := &PolicyLoader{Path: path, Store: store, LadderModels: ladder.Aliases()}
	if err := restarted.Boot(ctx); err == nil {
		t.Fatal("boot with only a stored narrow version and a narrow file succeeded")
	}
}

// A stored version from before the risk section is no valid version: the
// file's version replaces it at boot, and without a valid file the boot
// is refused.
func TestPolicyLoaderBootRevalidatesTheStoredVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	store := &memPolicy{}
	old, _ := policy.Parse([]byte(testPolicyYAML))
	old.Risk = policy.Risk{}
	store.RecordPolicy(ctx, "before-risk", old) // #nosec G104 -- the fake never fails
	writePolicy(t, path, "version: 2\n")
	if err := (&PolicyLoader{Path: path, Store: store}).Boot(ctx); err == nil {
		t.Fatal("boot with only a stored version without risk rules succeeded")
	}
	writePolicy(t, path, testPolicyYAML)
	l := &PolicyLoader{Path: path, Store: store}
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 2 || cur.Document.Risk.Default != "normal" {
		t.Fatalf("current = %d with risk %+v, want version 2 with the file's risk rules", cur.ID, cur.Document.Risk)
	}
}

func TestPolicyLoaderBootNeedsAValidVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := (&PolicyLoader{Path: path, Store: &memPolicy{}}).Boot(ctx); err == nil {
		t.Fatal("boot without a file and without a stored version succeeded")
	}
	writePolicy(t, path, "version: 1\n")
	if err := (&PolicyLoader{Path: path, Store: &memPolicy{}}).Boot(ctx); err == nil {
		t.Fatal("boot with an invalid file and without a stored version succeeded")
	}
}

func TestPolicyHandler(t *testing.T) {
	store := &memPolicy{}
	rr := httptest.NewRecorder()
	PolicyHandler(store)(rr, httptest.NewRequest("GET", "/policy", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a version = %d, want 503", rr.Code)
	}
	doc, _ := policy.Parse([]byte(testPolicyYAML))
	store.RecordPolicy(context.Background(), "abc", doc) // #nosec G104 -- the fake never fails
	rr = httptest.NewRecorder()
	PolicyHandler(store)(rr, httptest.NewRequest("GET", "/policy", nil))
	var got policy.Version
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("GET /policy = %d %s", rr.Code, rr.Body.String())
	}
	if got.ID != 1 || got.SHA256 != "abc" || got.Document.Budget.TaskMaxEUR != 5 || got.LoadedAt.IsZero() {
		t.Fatalf("GET /policy = %+v", got)
	}
	for _, key := range []string{`"id"`, `"sha256"`, `"loaded_at"`, `"activated_at"`, `"document"`, `"task_default_eur"`, `"coder"`} {
		if !strings.Contains(rr.Body.String(), key) {
			t.Fatalf("GET /policy body lacks %s: %s", key, rr.Body.String())
		}
	}
}

func TestCostsHandler(t *testing.T) {
	today := time.Now().UTC().Format(policy.DateLayout)
	for _, tc := range []struct {
		query            string
		status           int
		wantFrom, wantTo string
	}{
		{"", http.StatusOK, today, today},
		{"?from=2026-09-01&to=2026-09-27", http.StatusOK, "2026-09-01", "2026-09-27"},
		{"?from=2026-09-27&to=2026-09-27", http.StatusOK, "2026-09-27", "2026-09-27"},
		{"?from=2026-09-28&to=2026-09-27", http.StatusBadRequest, "", ""},
		{"?from=2025-01-01&to=2026-09-27", http.StatusBadRequest, "", ""},
		{"?from=27.09.2026", http.StatusBadRequest, "", ""},
		{"?to=2026-9-27", http.StatusBadRequest, "", ""},
	} {
		store := &memPolicy{}
		rr := httptest.NewRecorder()
		CostsHandler(store)(rr, httptest.NewRequest("GET", "/costs"+tc.query, nil))
		if rr.Code != tc.status {
			t.Fatalf("GET /costs%s = %d %s, want %d", tc.query, rr.Code, rr.Body.String(), tc.status)
		}
		if tc.status != http.StatusOK {
			if len(store.costs) != 0 {
				t.Fatalf("GET /costs%s reached the store", tc.query)
			}
			continue
		}
		if len(store.costs) != 1 || store.costs[0][0].Format(policy.DateLayout) != tc.wantFrom || store.costs[0][1].Format(policy.DateLayout) != tc.wantTo {
			t.Fatalf("GET /costs%s asked the store for %v, want %s..%s", tc.query, store.costs, tc.wantFrom, tc.wantTo)
		}
	}
}
