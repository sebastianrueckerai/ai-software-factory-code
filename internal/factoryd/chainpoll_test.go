package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The chain rows, as PGStore keeps them: one per pull request the newest
// run it carries carries, plus the head row (pr 0) of the newest run's
// commit, per repository.

type fakeChainRun struct {
	repo   string
	pr     int64
	sha    string
	run    int64
	status string
}

func (s *fakeStore) ChainHead(ctx context.Context, repository string) (int64, error) {
	var head int64
	for _, c := range s.chain {
		if c.repo == repository && c.run > head {
			head = c.run
		}
	}
	return head, nil
}

func (s *fakeStore) RecordChainRun(ctx context.Context, repository string, pr int64, sha string, run int64, status string) error {
	for i, c := range s.chain {
		if c.repo != repository || c.pr != pr {
			continue
		}
		if c.run >= run {
			return nil
		}
		s.chain[i] = fakeChainRun{repo: repository, pr: pr, sha: sha, run: run, status: status}
		return nil
	}
	s.chain = append(s.chain, fakeChainRun{repo: repository, pr: pr, sha: sha, run: run, status: status})
	return nil
}

func (s *fakeStore) RecordChainHead(ctx context.Context, repository, sha string, run int64, status string) error {
	for i, c := range s.chain {
		if c.repo != repository || c.pr != 0 {
			continue
		}
		if c.run > run || (c.run == run && c.sha == sha && c.status == status) {
			return nil
		}
		s.chain[i] = fakeChainRun{repo: repository, pr: 0, sha: sha, run: run, status: status}
		return nil
	}
	s.chain = append(s.chain, fakeChainRun{repo: repository, pr: 0, sha: sha, run: run, status: status})
	return nil
}

func (s *fakeStore) RefreshChainStatus(ctx context.Context, repository, sha, status string) error {
	for i, c := range s.chain {
		if c.repo == repository && c.sha == sha && c.status != status {
			s.chain[i].status = status
		}
	}
	return nil
}

// ChainCovered mirrors PGStore's read: every merged card with a pull
// request the repository's newest green run covers, named by that run.
func (s *fakeStore) ChainCovered(ctx context.Context, repository string) (map[string]int64, error) {
	green := int64(0)
	for _, c := range s.chain {
		if c.repo == repository && c.status == "success" && c.run > green {
			green = c.run
		}
	}
	covered := map[string]int64{}
	for _, t := range s.tasks {
		if t.Repository != repository || t.Status != "merged" || t.PRNumber <= 0 {
			continue
		}
		mine, carried := int64(0), false
		for _, c := range s.chain {
			if c.repo != repository || c.pr != t.PRNumber {
				continue
			}
			carried = true
			if mine == 0 || c.run < mine {
				mine = c.run
			}
		}
		if carried && green >= mine && mine > 0 {
			covered[t.ID] = green
		}
	}
	return covered, nil
}

func (s *fakeStore) Projects(ctx context.Context) ([]Project, error) {
	return s.projects, nil
}

// chainOf is store's chain rows of repository, by pull request.
func chainOf(s *fakeStore, repository string) []fakeChainRun {
	var out []fakeChainRun
	for _, c := range s.chain {
		if c.repo == repository {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b fakeChainRun) int { return cmp.Compare(a.pr, b.pr) })
	return out
}

// chainForgejo plays Forgejo for the chain poller: every project
// repository's push runs, the compares that map a run's carry, its
// merged pull requests, its commit statuses and its workflow files.
type chainForgejo struct {
	mu        sync.Mutex
	runs      map[string][]map[string]any               // repository -> its push runs, newest first
	carries   map[string]map[string][]map[string]any    // repository -> compare base...head -> its commits
	merged    map[string][]map[string]any               // repository -> its closed pull requests
	statuses  map[string]map[string][]map[string]string // repository -> sha -> the statuses reported on it
	workflows map[string][]string                       // repository -> its workflow files; one the map does not name has ci.yml
	reads     []string                                  // "repository dir" of every workflow-list read
}

func (f *chainForgejo) srv(t *testing.T) *Forgejo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/")
		owner, rest, ok := strings.Cut(rest, "/")
		name, after, ok2 := strings.Cut(rest, "/")
		if !ok || !ok2 || owner == "" || name == "" {
			t.Errorf("call outside a repository: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		repo := owner + "/" + name
		p := ""
		if ok2 {
			p = "/" + after
		}
		switch {
		case p == "/actions/runs":
			if r.URL.Query().Get("event") != "push" {
				t.Errorf("runs query %v", r.URL.Query())
			}
			reply(map[string]any{"workflow_runs": f.runs[repo]})
		case strings.HasPrefix(p, "/git/commits/"):
			reply(map[string]any{"parents": []map[string]string{{"sha": "parent-of-" + strings.TrimPrefix(p, "/git/commits/")}}})
		case strings.HasPrefix(p, "/compare/"):
			base, head, _ := strings.Cut(strings.TrimPrefix(p, "/compare/"), "...")
			reply(map[string]any{"commits": f.carries[repo][base+"..."+head]})
		case p == "/pulls":
			reply(f.merged[repo])
		case strings.HasPrefix(p, "/commits/") && strings.HasSuffix(p, "/status"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, "/commits/"), "/status")
			state := "success"
			for _, s := range f.statuses[repo][sha] {
				if s["status"] == "failure" {
					state = "failure"
				}
			}
			reply(map[string]any{"state": state, "statuses": f.statuses[repo][sha]})
		case p == "/contents/.forgejo/workflows" || p == "/contents/.gitea/workflows":
			f.reads = append(f.reads, repo+" "+p)
			files, named := f.workflows[repo]
			if !named {
				files = []string{"ci.yml"}
			}
			var out []map[string]any
			for _, name := range files {
				out = append(out, map[string]any{"name": name, "type": "file"})
			}
			reply(out)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
}

func newTestPoller(t *testing.T, f *chainForgejo, projects ...Project) (*ChainPoller, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	store.projects = projects
	return &ChainPoller{Forgejo: f.srv(t), Store: store, Projects: store,
		Policy: func() policy.Version { return policy.Version{} }}, store
}

const demoRepo = "sf/demo-app"

var (
	demoCommit = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d"
	demoMerge  = "3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

// The poller serves every project, not the roller's one repository: a
// run on each of two repositories records both chains.
func TestChainPollerRecordsEveryProject(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(61, commitC2, "success")},
			demoRepo: {pushRun(21, demoCommit, "success")},
		},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
			demoRepo: {"parent-of-" + demoCommit + "..." + demoCommit: {{"sha": demoMerge}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
			demoRepo: {{"number": 3, "merged": true, "merge_commit_sha": demoMerge}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "success"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "success"},
	}) {
		t.Fatalf("chain of %s %+v", codeRepo, got)
	}
	if got := chainOf(store, demoRepo); !slices.Equal(got, []fakeChainRun{
		{repo: demoRepo, pr: 0, sha: demoCommit, run: 21, status: "success"},
		{repo: demoRepo, pr: 3, sha: demoCommit, run: 21, status: "success"},
	}) {
		t.Fatalf("chain of %s %+v, want the run of the second repository recorded too", demoRepo, got)
	}
}

// The head row is the AND of every push run on the commit: a green run
// beside a red one fails the head, and both green conclude it.
func TestChainPollerAndsTheRuns(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(62, commitC2, "failure"), pushRun(61, commitC2, "success")},
		},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 62, status: "failure"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 62, status: "failure"},
	}) {
		t.Fatalf("chain beside a red run %+v, want the head row failed", got)
	}
	// the red run re-ran green: both runs on the commit concluded green
	f.runs[codeRepo] = []map[string]any{pushRun(62, commitC2, "success"), pushRun(61, commitC2, "success")}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 62, status: "success"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 62, status: "success"},
	}) {
		t.Fatalf("chain after the re-run %+v, want every row green", got)
	}
}

// A run the chain skipped never ran the change: it holds the head row
// pending — the stall watchdog's case, not repair's.
func TestChainPollerHoldsASkippedRunPending(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "skipped")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "skipped"},
	}) {
		t.Fatalf("chain of a skipped run %+v, want the head row pending", got)
	}
	if strings.Contains(logs.String(), "red:") {
		t.Fatalf("repair of a skipped run: %s", logs.String())
	}
}

// A cancelled run is the common transient: Forgejo cancels an in-flight
// main run the moment the next push's run is created, and the superseding
// run advances the head. The cancelled head holds pending — no repair.
func TestChainPollerHoldsACancelledRunPending(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "cancelled")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "cancelled"},
	}) {
		t.Fatalf("chain of a cancelled run %+v, want the head row pending", got)
	}
	if strings.Contains(logs.String(), "red:") {
		t.Fatalf("repair of a cancelled run: %s", logs.String())
	}
}

// A repository whose workflow list is empty degrades to none for itself:
// nothing recorded, and the degradation noted once however often it
// polls.
func TestChainPollerDegradesNoWorkflowRepos(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(61, commitC2, "success")},
			demoRepo: {pushRun(21, demoCommit, "success")},
		},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
			demoRepo: {"parent-of-" + demoCommit + "..." + demoCommit: {{"sha": demoMerge}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
			demoRepo: {{"number": 3, "merged": true, "merge_commit_sha": demoMerge}},
		},
		workflows: map[string][]string{demoRepo: {}},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	for range 2 {
		if err := p.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := chainOf(store, demoRepo); len(got) != 0 {
		t.Fatalf("chain of the workflow-less %s %+v, want nothing recorded", demoRepo, got)
	}
	if got := chainOf(store, codeRepo); len(got) != 2 {
		t.Fatalf("chain of %s %+v, want the workflow-bearing repository recorded", codeRepo, got)
	}
	if n := strings.Count(logs.String(), demoRepo+" has no workflow"); n != 1 {
		t.Fatalf("degradation noted %d times, want once: %s", n, logs.String())
	}
	if n := len(slices.DeleteFunc(slices.Clone(f.reads), func(r string) bool {
		return !strings.HasPrefix(r, demoRepo+" ")
	})); n != 2 {
		t.Fatalf("%d workflow-list reads of %s, want the two directories read once", n, demoRepo)
	}
}

// A required context missing on the commit holds the head row pending;
// pending past the project's stall bound calls the stalled hook, and the
// context reporting success concludes the head.
func TestChainPollerStallsAPendingHead(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(61, commitC2, "success")},
		},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: codeRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}, Stall: time.Minute}},
		}}}
	}
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "success"},
	}) {
		t.Fatalf("chain under a missing context %+v, want the head row pending", got)
	}
	if strings.Contains(logs.String(), "stalled") {
		t.Fatalf("stall called within the bound: %s", logs.String())
	}
	// the context still missing a minute past the bound: the head stalled
	clock = clock.Add(2 * time.Minute)
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "stalled"); n != 1 {
		t.Fatalf("stall called %d times past the bound, want once: %s", n, logs.String())
	}
	// the context reports success: the head concludes
	f.statuses = map[string]map[string][]map[string]string{
		codeRepo: {commitC2: {{"status": "success", "context": "factory/roll"}}},
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); got[0].status != "success" {
		t.Fatalf("chain after the context concluded %+v, want the head row green", got)
	}
}

// A chain block the poller cannot use — a mode it does not know, a
// negative stall — is refused loudly, once, and the repository stays
// unrecorded while a usable block beside it records.
func TestChainPollerRefusesABrokenChainBlock(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(61, commitC2, "success")},
			demoRepo: {pushRun(21, demoCommit, "success")},
		},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
			demoRepo: {"parent-of-" + demoCommit + "..." + demoCommit: {{"sha": demoMerge}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
			demoRepo: {{"number": 3, "merged": true, "merge_commit_sha": demoMerge}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Mode: "runs"}},
		}}}
	}
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	for range 2 {
		if err := p.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := chainOf(store, demoRepo); len(got) != 0 {
		t.Fatalf("chain of the refused %s %+v, want nothing recorded", demoRepo, got)
	}
	if got := chainOf(store, codeRepo); len(got) != 2 {
		t.Fatalf("chain of %s %+v, want the repository with the usable block recorded", codeRepo, got)
	}
	if n := strings.Count(logs.String(), demoRepo); n != 1 {
		t.Fatalf("refusal noted %d times, want once: %s", n, logs.String())
	}
}

// The chain recording (roadmap 297): the first recording maps the newest
// run only, its carry read from its first parent; a later run's carry is
// read from the compare against the run before it, a finished run moves
// its rows to its final status, a run no pull request rides is recorded
// only as the head row, and a pull request already mapped keeps its own
// run: a fold settles the verdict of the run that carried it.
func TestChainPollerRecordsTheChainOfEveryMainRun(t *testing.T) {
	d3 := "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
	e4 := "e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4"
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "running")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {
				"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}},
				commitC2 + "..." + d3:                      {{"sha": mergePR14}},
				d3 + "..." + e4:                            {},
			},
		},
		merged: map[string][]map[string]any{
			codeRepo: {
				{"number": 14, "merged": true, "merge_commit_sha": mergePR14},
				{"number": 12, "merged": true, "merge_commit_sha": mergePR12},
			},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	ctx := context.Background()
	if err := p.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "running"},
	}) {
		t.Fatalf("chain after the first recording %+v, want pr 12 running under run 61", got)
	}
	// the run finished, and a newer one arrived carrying another pull
	// request: the old row moves to the final status, the new run's pull
	// request is mapped beside it
	f.runs[codeRepo] = []map[string]any{pushRun(62, d3, "success"), pushRun(61, commitC2, "success")}
	if err := p.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: d3, run: 62, status: "success"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "success"},
		{repo: codeRepo, pr: 14, sha: d3, run: 62, status: "success"},
	}) {
		t.Fatalf("chain after the second run %+v, want pr 12 success under 61 and pr 14 success under 62", got)
	}
	// a run no pull request rides leaves the head row alone on the new
	// commit; the pull requests already mapped keep their runs, their own
	// verdicts
	f.runs[codeRepo] = []map[string]any{pushRun(64, e4, "running"), pushRun(62, d3, "success")}
	if err := p.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: e4, run: 64, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "success"},
		{repo: codeRepo, pr: 14, sha: d3, run: 62, status: "success"},
	}) {
		t.Fatalf("chain after the third run %+v, want the head row alone under run 64 and pr 12 kept under 61", got)
	}
	if head, _ := store.ChainHead(ctx, codeRepo); head != 64 {
		t.Fatalf("chain head %d, want 64", head)
	}
}

// A green head tells every merged card it covers: one covered signal
// each, once per head — a workflow already done drops it, one that died
// is started by the signal and reads the facts on entry. A merged card
// no run carries yet is told nothing.
func TestChainPollerSignalsCoveredMerges(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(61, commitC2, "success")},
		},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
		},
	}
	wf := &fakeWorkflows{}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Workflows = wf
	store.tasks = map[string]Task{
		"t-cov":   {ID: "t-cov", Status: "merged", Repository: codeRepo, PRNumber: 12},
		"t-waits": {ID: "t-waits", Status: "merged", Repository: codeRepo, PRNumber: 13},
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(wf.signals) != 1 || !strings.HasPrefix(wf.signals[0], "t-cov "+SignalChainCovered+" "+FactoryDecider) {
		t.Fatalf("signals = %v; want one covered signal for t-cov alone", wf.signals)
	}
	// the same green head again signals nobody a second time
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(wf.signals) != 1 {
		t.Fatalf("signals = %v; the covered signal fires once per head", wf.signals)
	}
}
