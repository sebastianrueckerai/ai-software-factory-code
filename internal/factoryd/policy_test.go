package factoryd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
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
  month_eur: 50
limits: {sessions: 3, agent_time: 10m, supervisor_calls: 3, supervisor_timeout: 6m, stronger_after: 1}
classes:
  coder:
    session: code
    models: [coding-cheap, coding-frontier]
    tools: {}
    review_model: review-frontier
    supervisor_model: supervisor
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

const testProcessYAML = `lanes:
  - {name: backlog}
  - {name: ready, class: coder, moved_by: writers}
merge: {queue: none}
stages:
  - {name: checks, on_findings: agent, enabled: true}
  - {name: policy, on_findings: fail, enabled: true}
  - {name: review, on_findings: agent, enabled: true}
  - {name: approval, on_findings: human, enabled: true}
  - {name: merge, on_findings: human, enabled: true}
`

// testFiles is a configuration tree with policyYAML as its policy.yaml.
func testFiles(policyYAML string) map[string][]byte {
	return map[string][]byte{
		"policy.yaml":                []byte(policy.Header("policy.yaml") + "\n" + policyYAML),
		"processes/maintenance.yaml": []byte(policy.Header("processes/maintenance.yaml") + "\n" + testProcessYAML),
		"projects/ai-software-factory-infra.yaml": []byte(policy.Header("projects/ai-software-factory-infra.yaml") +
			"\nrepository: sf/ai-software-factory-infra\nmerge_rule: human\nobjects: seed\nagents: none\ntokens: {read: forgejo-config}\n"),
	}
}

// testTree is testFiles(policyYAML) parsed; it panics on an invalid one.
func testTree(policyYAML string) policy.Tree {
	t, err := policy.ParseTree(testFiles(policyYAML))
	if err != nil {
		panic(err)
	}
	return t
}

// memPolicy is the in-memory PolicyStore: versions keyed by commit, in
// order.
type memPolicy struct {
	mu       sync.Mutex
	versions []policy.Version
	current  int64
	records  int
	costs    [][2]time.Time
	sweeps   int
}

func (m *memPolicy) SweepHolds(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweeps++
	return 0, nil
}

func (m *memPolicy) RecordPolicy(_ context.Context, commit string, tree policy.Tree) (policy.Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records++
	for _, v := range m.versions {
		if v.Commit == commit {
			m.current = v.ID
			return v, false, nil
		}
	}
	v := policy.Version{ID: int64(len(m.versions) + 1), Commit: commit, Tree: tree, LoadedAt: time.Now()}
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

func (m *memPolicy) PolicyVersion(_ context.Context, id int64) (policy.Version, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || id > int64(len(m.versions)) {
		return policy.Version{}, false, nil
	}
	return m.versions[id-1], true, nil
}

func (m *memPolicy) Costs(_ context.Context, from, to time.Time) (policy.Costs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.costs = append(m.costs, [2]time.Time{from, to})
	return policy.Costs{From: from.Format(policy.DateLayout), To: to.Format(policy.DateLayout)}, nil
}

// memSource is a branch of commits of the configuration tree; failing
// makes every read fail, as an unreachable Forgejo would.
type memSource struct {
	mu      sync.Mutex
	head    string
	commits map[string]map[string][]byte
	failing bool
	reads   int
}

func (s *memSource) push(commit string, files map[string][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commits == nil {
		s.commits = map[string]map[string][]byte{}
	}
	s.head, s.commits[commit] = commit, files
}

func (s *memSource) Head(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing || s.head == "" {
		return "", errors.New("forgejo unreachable")
	}
	return s.head, nil
}

func (s *memSource) Files(_ context.Context, commit string) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failing {
		return nil, errors.New("forgejo unreachable")
	}
	return maps.Clone(s.commits[commit]), nil
}

// rejections records PolicyLoader.Rejected's calls; fail makes them fail.
type rejections struct {
	calls []string // "commit current-id"
	fail  error
}

func (r *rejections) raise(_ context.Context, commit string, reason error, current policy.Version, loaded bool) error {
	r.calls = append(r.calls, fmt.Sprintf("%s %d %v", commit, current.ID, loaded))
	return r.fail
}

func TestPolicyLoader(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	store := &memPolicy{}
	rej := &rejections{}
	l := &PolicyLoader{Source: src, Store: store, Rejected: rej.raise}

	src.push("c1", testFiles(testPolicyYAML))
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 1 || cur.Commit != "c1" || cur.Document.Budget.DayEUR != 20 || cur.Projects["ai-software-factory-infra"].Agents != policy.AgentsNone {
		t.Fatalf("after boot current = %+v, want version 1 of commit c1", cur)
	}

	// the same head: no-op, the files are not even read
	if err := l.Load(ctx); err != nil || store.records != 1 || src.reads != 1 {
		t.Fatalf("reload of the same head = %v with %d records after %d reads, want nil with 1 after 1", err, store.records, src.reads)
	}

	// a new commit: a new version, current
	src.push("c2", testFiles(strings.Replace(testPolicyYAML, "day_eur: 20", "day_eur: 7", 1)))
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 2 || cur.Commit != "c2" || cur.Document.Budget.DayEUR != 7 {
		t.Fatalf("after a push current = %+v, want version 2 with day_eur 7", cur)
	}

	// Forgejo unreachable: nothing changes, nothing is raised
	src.failing = true
	if err := l.Load(ctx); err == nil || l.Current().ID != 2 || len(rej.calls) != 0 {
		t.Fatalf("unreachable source = %v, current %d, %d rejections", err, l.Current().ID, len(rej.calls))
	}
	src.failing = false

	// an invalid tree: rejected and raised, version 2 stays current
	bad := testFiles(testPolicyYAML)
	bad["processes/maintenance.yaml"] = []byte(strings.Replace(string(bad["processes/maintenance.yaml"]), "on_findings: fail", "on_findings: maybe", 1))
	src.push("c3", bad)
	if err := l.Load(ctx); err == nil || !strings.Contains(err.Error(), `on_findings "maybe"`) {
		t.Fatalf("invalid tree = %v, want the on_findings reason", err)
	}
	if cur := l.Current(); cur.ID != 2 || !slices.Equal(rej.calls, []string{"c3 2 true"}) {
		t.Fatalf("after a bad push current = %d, rejections %v, want 2 and [c3 2 true]", cur.ID, rej.calls)
	}
	// the same bad head again: neither read nor raised twice
	reads := src.reads
	if err := l.Load(ctx); err != nil || src.reads != reads || len(rej.calls) != 1 || store.records != 2 {
		t.Fatalf("second look at the bad head = %v, %d reads, %d rejections, %d records", err, src.reads-reads, len(rej.calls), store.records)
	}

	// the fix loads
	src.push("c4", testFiles(testPolicyYAML))
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 3 || cur.Commit != "c4" {
		t.Fatalf("after the fix current = %+v, want version 3 of c4", cur)
	}

	// a restart on a bad head keeps the stored current version
	src.push("c5", bad)
	restarted := &PolicyLoader{Source: src, Store: store, Rejected: rej.raise}
	if err := restarted.Boot(ctx); err != nil {
		t.Fatalf("boot with a stored version and a bad head = %v, want nil", err)
	}
	if cur := restarted.Current(); cur.ID != 3 || rej.calls[len(rej.calls)-1] != "c5 3 true" {
		t.Fatalf("restarted current = %d, rejections %v", cur.ID, rej.calls)
	}
}

// Stricter wins: a project file looser than policy.yaml's bound is
// rejected like any invalid tree and the version in force stays.
func TestPolicyLoaderRejectsALooserProjectFile(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	rej := &rejections{}
	l := &PolicyLoader{Source: src, Store: &memPolicy{}, Rejected: rej.raise}
	src.push("c1", testFiles(testPolicyYAML))
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	looser := testFiles(testPolicyYAML)
	looser["projects/demo.yaml"] = []byte(policy.Header("projects/demo.yaml") +
		"\nrepository: demo/demo\nmerge_rule: human\nobjects: factory\nagents: coder\nnamespaces: [demo]\nquota: {cpu: \"3\", memory: 4Gi, storage: 10Gi}\n")
	src.push("c2", looser)
	if err := l.Load(ctx); err == nil || !strings.Contains(err.Error(), `project demo: quota.cpu "3" is looser than policy.yaml's project_default.quota.cpu "2"`) {
		t.Fatalf("looser project = %v, want the quota refused", err)
	}
	if cur := l.Current(); cur.Commit != "c1" || !slices.Equal(rej.calls, []string{"c2 1 true"}) {
		t.Fatalf("current = %s, rejections %v, want c1 kept and c2 raised", cur.Commit, rej.calls)
	}
}

// Each valid head, once loaded, is settled (Loaded: the open rejections
// close); a refused head settles nothing, and a failed settling is tried
// again at the next poll.
func TestPolicyLoaderSettlesEachLoadedHead(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	var settled []string
	var fail error
	l := &PolicyLoader{Source: src, Store: &memPolicy{}, Rejected: (&rejections{}).raise,
		Loaded: func(_ context.Context, v policy.Version) error {
			settled = append(settled, v.Commit)
			return fail
		}}
	src.push("c1", testFiles(testPolicyYAML))
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	src.push("bad", testFiles("version: 1\n"))
	l.Load(ctx) // #nosec G104 -- the rejection is the point
	fail = errors.New("forgejo down")
	src.push("c2", testFiles(testPolicyYAML))
	l.Load(ctx) // #nosec G104
	l.Load(ctx) // #nosec G104
	fail = nil
	l.Load(ctx) // #nosec G104
	l.Load(ctx) // #nosec G104
	if !slices.Equal(settled, []string{"c1", "c2", "c2", "c2"}) {
		t.Fatalf("settled = %v, want c1 once, c2 until it succeeds", settled)
	}
}

// Ten reads in a row that fail (a revoked token, Forgejo down: about five
// minutes) raise one alert; a read that works starts the count again.
func TestPolicyLoaderRaisesAStreakOfReadFailures(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	var raised []string
	l := &PolicyLoader{Source: src, Store: &memPolicy{}, ReadFailing: func(_ context.Context, err error) error {
		raised = append(raised, err.Error())
		return nil
	}}
	src.push("c1", testFiles(testPolicyYAML))
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	src.failing = true
	for i := range 25 {
		l.Load(ctx) // #nosec G104 -- the failures are the point
		if want := min(1, (i+1)/10); len(raised) != want {
			t.Fatalf("after %d failures %d alerts, want %d", i+1, len(raised), want)
		}
	}
	src.failing = false
	l.Load(ctx) // #nosec G104
	src.failing = true
	for range 10 {
		l.Load(ctx) // #nosec G104
	}
	if !slices.Equal(raised, []string{"forgejo unreachable", "forgejo unreachable"}) {
		t.Fatalf("raised = %v, want one per streak", raised)
	}
}

// A read failure streak is an alerts row of kind config_rejected, source
// read, judged real, with no issue: the reason may be that nothing can
// reach Forgejo.
func TestConfigAlarmReadFailing(t *testing.T) {
	store := newFakeStore()
	alarm := &ConfigAlarm{Store: store, Forgejo: &Forgejo{}, Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Dir: "factory"}
	if err := alarm.ReadFailing(context.Background(), errors.New("GET branches/main: forgejo not found")); err != nil {
		t.Fatal(err)
	}
	rows := store.alertRows()
	if len(rows) != 1 || rows[0].Kind != AlertConfigRejected || rows[0].Source != "read" || rows[0].Verdict != VerdictReal ||
		rows[0].Detail["error"] != "GET branches/main: forgejo not found" || len(store.tasks) != 0 {
		t.Fatalf("alerts = %+v, tasks %d", rows, len(store.tasks))
	}
}

// A rejection that cannot be raised is raised again at the next poll.
func TestPolicyLoaderRaisesUntilItSucceeds(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	rej := &rejections{fail: errors.New("forgejo down")}
	l := &PolicyLoader{Source: src, Store: &memPolicy{}, Rejected: rej.raise}
	src.push("c1", testFiles(testPolicyYAML))
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	src.push("c2", testFiles("version: 1\n"))
	l.Load(ctx) // #nosec G104 -- the rejection is the point
	l.Load(ctx) // #nosec G104
	rej.fail = nil
	l.Load(ctx) // #nosec G104
	l.Load(ctx) // #nosec G104
	if !slices.Equal(rej.calls, []string{"c2 1 true", "c2 1 true", "c2 1 true"}) {
		t.Fatalf("rejections = %v, want three tries until one succeeds", rej.calls)
	}
}

// A version whose coder class leaves out a ladder model is rejected: the
// ladder's next rung would be refused mid-task.
func TestPolicyLoaderKeepsTheLadderAllowed(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	store := &memPolicy{}
	ladder := ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}
	if got := ladder.Aliases(); !slices.Equal(got, []string{"coding-cheap", "coding-frontier"}) {
		t.Fatalf("aliases = %v", got)
	}
	l := &PolicyLoader{Source: src, Store: store, LadderModels: ladder.Aliases()}
	narrow := strings.Replace(testPolicyYAML, "[coding-cheap, coding-frontier]", "[coding-cheap]", 1)

	// the only valid version leaves out a rung: no boot
	src.push("narrow", testFiles(narrow))
	if err := l.Boot(ctx); err == nil || store.records != 0 {
		t.Fatalf("boot with a policy that drops coding-frontier = %v after %d records, want a refusal", err, store.records)
	}
	src.push("wide", testFiles(testPolicyYAML))
	l = &PolicyLoader{Source: src, Store: store, LadderModels: ladder.Aliases()}
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	// a later push that drops a rung is rejected, version 1 stays
	src.push("narrow-2", testFiles(narrow))
	if err := l.Load(ctx); err == nil || !strings.Contains(err.Error(), "coding-frontier") || l.Current().ID != 1 {
		t.Fatalf("push dropping a rung = %v with version %d current, want a rejection with 1", err, l.Current().ID)
	}
	// a stored version that drops a rung does not count as valid at boot
	store.RecordPolicy(ctx, "narrow-stored", testTree(narrow)) // #nosec G104 -- the fake never fails
	restarted := &PolicyLoader{Source: src, Store: store, LadderModels: ladder.Aliases()}
	if err := restarted.Boot(ctx); err == nil {
		t.Fatal("boot with only a stored narrow version and a narrow head succeeded")
	}
}

// Every class a starting lane of any process names must allow the whole
// ladder (N7): the planner, not only the coder.
func TestCheckTreeLadderPerLane(t *testing.T) {
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	ladder := ModelLadder{Start: "factory/coding-cheap", Stronger: "factory/coding-frontier"}.Aliases()
	if _, err := CheckTree(files, ladder); err != nil {
		t.Fatalf("the fixture tree: %v", err)
	}
	const planner = "    session: plan\n    models: [coding-cheap, coding-frontier]\n"
	if !strings.Contains(string(files["policy.yaml"]), planner) {
		t.Fatal("policy.yaml lacks the planner's models")
	}
	files["policy.yaml"] = []byte(strings.Replace(string(files["policy.yaml"]), planner, "    session: plan\n    models: [coding-cheap]\n", 1))
	_, err = CheckTree(files, ladder)
	if err == nil || err.Error() != `policy.yaml: capability class "planner" does not allow the ladder model "coding-frontier" (lane plan of processes/spec.yaml starts it)` {
		t.Fatalf("CheckTree = %v", err)
	}
}

// A stored version from before the tree (a policy file's document) is no
// valid version: the head's tree replaces it at boot, and without a valid
// head the boot is refused.
func TestPolicyLoaderBootRevalidatesTheStoredVersion(t *testing.T) {
	ctx := context.Background()
	src := &memSource{}
	store := &memPolicy{}
	old := testTree(testPolicyYAML)
	coder := old.Document.Classes["coder"]
	coder.Session = ""
	old.Document.Classes = map[string]policy.Class{"coder": coder}
	store.RecordPolicy(ctx, "before-the-tree", old) // #nosec G104 -- the fake never fails
	src.push("bad", testFiles("version: 2\n"))
	if err := (&PolicyLoader{Source: src, Store: store}).Boot(ctx); err == nil {
		t.Fatal("boot with only a stored version without a class session succeeded")
	}
	src.push("good", testFiles(testPolicyYAML))
	l := &PolicyLoader{Source: src, Store: store}
	if err := l.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if cur := l.Current(); cur.ID != 2 || len(cur.Stages()) != 5 || cur.Document.Classes["coder"].Session != policy.SessionCode {
		t.Fatalf("current = %d with %d stages, want version 2 with the head's process", cur.ID, len(cur.Stages()))
	}
}

func TestPolicyLoaderBootNeedsAValidVersion(t *testing.T) {
	ctx := context.Background()
	if err := (&PolicyLoader{Source: &memSource{}, Store: &memPolicy{}}).Boot(ctx); err == nil {
		t.Fatal("boot without a readable head and without a stored version succeeded")
	}
	src := &memSource{}
	src.push("c1", testFiles("version: 1\n"))
	if err := (&PolicyLoader{Source: src, Store: &memPolicy{}}).Boot(ctx); err == nil {
		t.Fatal("boot with an invalid head and without a stored version succeeded")
	}
}

func TestPolicyHandler(t *testing.T) {
	store := &memPolicy{}
	rr := httptest.NewRecorder()
	PolicyHandler(store)(rr, httptest.NewRequest("GET", "/policy", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("without a version = %d, want 503", rr.Code)
	}
	store.RecordPolicy(context.Background(), "abc", testTree(testPolicyYAML)) // #nosec G104 -- the fake never fails
	rr = httptest.NewRecorder()
	PolicyHandler(store)(rr, httptest.NewRequest("GET", "/policy", nil))
	var got policy.Version
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("GET /policy = %d %s", rr.Code, rr.Body.String())
	}
	if got.ID != 1 || got.Commit != "abc" || got.Document.Budget.TaskMaxEUR != 5 || len(got.Stages()) != 5 || got.LoadedAt.IsZero() {
		t.Fatalf("GET /policy = %+v", got)
	}
	for _, key := range []string{`"id"`, `"commit"`, `"loaded_at"`, `"activated_at"`, `"policy"`, `"projects"`, `"processes"`, `"task_default_eur"`, `"coder"`} {
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

// infraForgejo is sf/ai-software-factory-infra: main at commit c9 with
// factory/ (policy.yaml, processes/, projects/, schema/) next to k8s/,
// readable with the config token only; issues filed by the bot.
func infraForgejo(t *testing.T, files map[string][]byte) (*Forgejo, *alertForgejo) {
	t.Helper()
	fj := &alertForgejo{labels: map[int64][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fj.mu.Lock()
		defer fj.mu.Unlock()
		const repo = "/api/v1/repos/sf/ai-software-factory-infra"
		path := strings.TrimPrefix(r.URL.Path, repo+"/")
		if r.Method == http.MethodGet && path == "issues" {
			// the open issues, one page
			var open []map[string]any
			for i, is := range fj.issues {
				if n := int64(41 + i); r.URL.Query().Get("page") == "1" && !slices.Contains(fj.closed, n) {
					title, body, _ := strings.Cut(is, "\n\n")
					open = append(open, map[string]any{"number": n, "title": title, "body": body, "user": map[string]string{"login": "factory-agent"}})
				}
			}
			json.NewEncoder(w).Encode(append([]map[string]any{}, open...)) // #nosec G104 -- test stub
			return
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("Authorization") != "token cfg-tok" {
				t.Errorf("GET %s without the config token", r.URL.Path)
			}
			switch {
			case path == "branches/main":
				io.WriteString(w, `{"commit":{"id":"c9"}}`)
			case path == "git/trees/c9" && r.URL.Query().Get("recursive") == "false":
				io.WriteString(w, `{"total_count":3,"tree":[{"path":"README.md","type":"blob","sha":"r"},{"path":"factory","type":"tree","sha":"f1"},{"path":"k8s","type":"tree","sha":"k"}]}`)
			case path == "git/trees/f1" && r.URL.Query().Get("recursive") == "true":
				var entries []string
				for _, name := range []string{"policy.yaml", "processes", "processes/maintenance.yaml", "projects", "projects/ai-software-factory-infra.yaml", "schema", "schema/policy.schema.json"} {
					typ, mode := "blob", "100644"
					if !strings.Contains(name, ".") {
						typ, mode = "tree", "040000"
					}
					entries = append(entries, fmt.Sprintf(`{"path":%q,"type":%q,"mode":%q,"sha":"x"}`, name, typ, mode))
				}
				fmt.Fprintf(w, `{"total_count":%d,"tree":[%s]}`, len(entries), strings.Join(entries, ","))
			case strings.HasPrefix(path, "raw/factory/") && r.URL.Query().Get("ref") == "c9":
				b, ok := files[strings.TrimPrefix(path, "raw/factory/")]
				if !ok {
					t.Errorf("read of %s", path)
					http.NotFound(w, r)
				}
				w.Write(b)
			default:
				t.Errorf("unexpected GET %s", r.URL)
				http.NotFound(w, r)
			}
			return
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "factory-agent" || p != "bot-pw" {
			t.Errorf("%s %s not as the bot", r.Method, r.URL.Path)
		}
		var n int64
		switch {
		case r.Method == http.MethodPost && path == "issues" && fj.failNext > 0:
			fj.failNext--
			http.Error(w, "down", http.StatusInternalServerError)
		case r.Method == http.MethodPost && fmtScan(path, "issues/%d/comments", &n):
			var in struct{ Body string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.comments = append(fj.comments, fmt.Sprintf("#%d %s", n, in.Body))
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodPatch && fmtScan(path, "issues/%d", &n):
			fj.closed = append(fj.closed, n)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && path == "issues":
			var in struct{ Title, Body string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.issues = append(fj.issues, in.Title+"\n\n"+in.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"number":%d}`, 40+len(fj.issues))
		case r.Method == http.MethodPut && fmtScan(path, "issues/%d/labels", &n):
			var in struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&in) // #nosec G104 -- test stub
			fj.labels[n] = in.Labels
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, PublicURL: "https://git.example", BotUser: "factory-agent", BotPassword: "bot-pw"}, fj
}

// The source reads the head of main and factory/'s files at it with the
// config token, the schema directory left out.
func TestForgejoConfigSource(t *testing.T) {
	want := testFiles(testPolicyYAML)
	forgejo, _ := infraForgejo(t, want)
	src := &ForgejoConfig{Forgejo: forgejo, Token: "cfg-tok", Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Branch: "main", Dir: "factory"}
	ctx := context.Background()
	head, err := src.Head(ctx)
	if err != nil || head != "c9" {
		t.Fatalf("Head = %q, %v", head, err)
	}
	got, err := src.Files(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Fatalf("Files = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
}

// A symbolic link in factory/ is refused, as config check refuses it.
func TestForgejoConfigRefusesSymbolicLinks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/sf/i/git/trees/c9":
			io.WriteString(w, `{"total_count":1,"tree":[{"path":"factory","type":"tree","mode":"040000","sha":"f1"}]}`)
		case "/api/v1/repos/sf/i/git/trees/f1":
			io.WriteString(w, `{"total_count":2,"tree":[{"path":"policy.yaml","type":"blob","mode":"100644","sha":"a"},{"path":"processes/maintenance.yaml","type":"blob","mode":"120000","sha":"b"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	src := &ForgejoConfig{Forgejo: &Forgejo{BaseURL: srv.URL}, Token: "t", Repo: Repo{Owner: "sf", Name: "i"}, Branch: "main", Dir: "factory"}
	if _, err := src.Files(context.Background(), "c9"); err == nil || !strings.Contains(err.Error(), "processes/maintenance.yaml: the tree holds regular files only") {
		t.Fatalf("Files with a symbolic link = %v", err)
	}
}

// A refused commit files one human-lane issue on the infra repository and
// one open alerts row, however often it is raised; the alerts row, the
// dedupe key, is written before the issue, so a failed filing is tried
// again without a second issue. The next valid commit closes the issue,
// ends the task and judges the alert real.
func TestConfigAlarm(t *testing.T) {
	forgejo, fj := infraForgejo(t, nil)
	store := newFakeStore()
	m, reader := testMetrics(t)
	alarm := &ConfigAlarm{Store: store, Forgejo: forgejo, Repo: Repo{Owner: "sf", Name: "ai-software-factory-infra"}, Dir: "factory", Metrics: m}
	ctx := context.Background()
	reason := errors.New("processes/maintenance.yaml: stages.policy: on_findings \"maybe\", want one of [agent human fail]")
	current := policy.Version{ID: 12, Commit: "c8c8c8c8c8c8"}
	fj.failNext = 1
	if err := alarm.Raise(ctx, "c9c9c9c9c9c9c9c9", reason, current, true); err == nil {
		t.Fatal("a failed filing raised nothing")
	}
	for range 2 {
		if err := alarm.Raise(ctx, "c9c9c9c9c9c9c9c9", reason, current, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(fj.issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(fj.issues))
	}
	for _, want := range []string{"config: factory/ at c9c9c9c9c9 rejected\n\n", "commit c9c9c9c9c9c9c9c9 (https://git.example/sf/ai-software-factory-infra/src/commit/c9c9c9c9c9c9c9c9/factory)",
		"Version 12 (commit c8c8c8c8c8c8) stays current.", "on_findings \"maybe\""} {
		if !strings.Contains(fj.issues[0], want) {
			t.Errorf("issue lacks %q:\n%s", want, fj.issues[0])
		}
	}
	if got := fj.labels[41]; !slices.Equal(got, []string{"human", "state/waiting-human"}) {
		t.Errorf("labels = %v", got)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(store.tasks))
	}
	var task Task
	for _, task = range store.tasks {
	}
	if task.Kind != "human" || task.Status != "waiting-human" || task.Repository != "sf/ai-software-factory-infra" || task.IssueNumber != 41 ||
		task.Source != SourceConfig || task.SHA != "c9c9c9c9c9c9c9c9" || len(task.RequiredCapabilities) != 0 {
		t.Fatalf("task = %+v", task)
	}
	a, ok, _ := store.AlertByFingerprint(ctx, AlertConfigRejected, "c9c9c9c9c9c9c9c9")
	if !ok || a.TaskID != task.ID || a.Verdict != "" || a.Detail["issue"] != int64(41) || a.Detail["current"] != int64(12) || len(store.alertRows()) != 1 {
		t.Fatalf("alert = %+v, %v (%d rows), want one open row on the task", a, ok, len(store.alertRows()))
	}

	if err := alarm.Resolve(ctx, policy.Version{ID: 13, Commit: "cacacacacaca"}); err != nil {
		t.Fatal(err)
	}
	wantSums(t, sums(t, reader, "factory.task.outcome"), map[string]float64{
		"attempts=0,merge_rule=human,model_alias=none,project=sf-ai-software-factory-infra,state=done": 1,
	})
	if !slices.Equal(fj.closed, []int64{41}) || len(fj.comments) != 1 ||
		!strings.Contains(fj.comments[0], "#41 factoryd loaded commit cacacacacaca (version 13)") {
		t.Fatalf("closed %v, comments %q", fj.closed, fj.comments)
	}
	if got := store.tasks[task.ID]; got.Status != "done" || got.FinishedAt == nil {
		t.Fatalf("task after the fix = %+v, want done", got)
	}
	a, _, _ = store.AlertByFingerprint(ctx, AlertConfigRejected, "c9c9c9c9c9c9c9c9")
	if a.Verdict != VerdictReal || a.ResolvedBy != "factoryd" || a.Detail["fixed_by"] != "cacacacacaca" {
		t.Fatalf("alert after the fix = %+v", a)
	}
	// nothing open: a second resolve does nothing
	if err := alarm.Resolve(ctx, policy.Version{ID: 14, Commit: "cbcb"}); err != nil || len(fj.closed) != 1 {
		t.Fatalf("second resolve = %v, closed %v", err, fj.closed)
	}
}
