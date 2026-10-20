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
	// updated is the row's last change, as the store's now() stamps it;
	// a test wires s.chainNow to the poller's clock where it matters
	updated time.Time
}

// stamp is the time the fake's chain writes carry: zero, the clock a
// test that cares about the head row's stamps wires.
func (s *fakeStore) stamp() time.Time {
	if s.chainNow != nil {
		return s.chainNow()
	}
	return time.Time{}
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
		s.chain[i] = fakeChainRun{repo: repository, pr: pr, sha: sha, run: run, status: status, updated: s.stamp()}
		return nil
	}
	s.chain = append(s.chain, fakeChainRun{repo: repository, pr: pr, sha: sha, run: run, status: status, updated: s.stamp()})
	return nil
}

func (s *fakeStore) RecordChainHead(ctx context.Context, repository, sha string, run int64, status string) error {
	for i, c := range s.chain {
		if c.repo != repository || c.pr != 0 {
			continue
		}
		if c.run > run || (c.run == run && c.sha == sha && c.status == status) ||
			(c.status == "stalled" && status == "pending" && c.run == run && c.sha == sha) {
			return nil
		}
		s.chain[i] = fakeChainRun{repo: repository, pr: 0, sha: sha, run: run, status: status, updated: s.stamp()}
		return nil
	}
	s.chain = append(s.chain, fakeChainRun{repo: repository, pr: 0, sha: sha, run: run, status: status, updated: s.stamp()})
	return nil
}

func (s *fakeStore) RefreshChainStatus(ctx context.Context, repository, sha, status string) error {
	for i, c := range s.chain {
		if c.repo == repository && c.pr > 0 && c.sha == sha && c.status != status {
			s.chain[i].status = status
			s.chain[i].updated = s.stamp()
		}
	}
	return nil
}

// RecordChainStall marks repository's head row of sha stalled, moving no
// stamp: the row keeps the time its pending was first recorded.
func (s *fakeStore) ResolveChainAlerts(ctx context.Context, repository string) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, b := range s.alerts {
		if (b.Kind == AlertChainRed || b.Kind == AlertChainStalled) && b.Fingerprint == repository && b.Verdict == "" {
			s.alerts[i].Verdict = VerdictReal
			s.alerts[i].ResolvedBy = "factoryd"
		}
	}
	return nil
}

func (s *fakeStore) RecordChainStall(ctx context.Context, repository, sha string) error {
	for i, c := range s.chain {
		if c.repo == repository && c.pr == 0 && c.sha == sha && c.status == "pending" {
			s.chain[i].status = "stalled"
		}
	}
	return nil
}

// ChainHeadSince is the head row's last change: zero where no test wired
// the fake's clock.
func (s *fakeStore) ChainHeadSince(ctx context.Context, repository, sha string) (time.Time, bool, error) {
	for _, c := range s.chain {
		if c.repo == repository && c.pr == 0 && c.sha == sha {
			return c.updated, true, nil
		}
	}
	return time.Time{}, false, nil
}

// OpenRepairCard reports whether repository has a repair card open.
func (s *fakeStore) OpenRepairCard(ctx context.Context, repository string) (bool, error) {
	for _, t := range s.tasks {
		if t.Repository == repository && t.Source == SourceChainRepair && !slices.Contains(FinalStatuses, t.Status) {
			return true, nil
		}
	}
	return false, nil
}

// ChainCarriedIssues are the issue numbers of the cards whose change
// repository's head at sha carried, ascending.
func (s *fakeStore) ChainCarriedIssues(ctx context.Context, repository, sha string) ([]int64, error) {
	var out []int64
	for _, c := range s.chain {
		if c.repo != repository || c.sha != sha || c.pr <= 0 {
			continue
		}
		for _, t := range s.tasks {
			if t.Repository == repository && t.PRNumber == c.pr && t.IssueNumber > 0 && !slices.Contains(out, t.IssueNumber) {
				out = append(out, t.IssueNumber)
			}
		}
	}
	slices.Sort(out)
	return out, nil
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
	issues    []string                                  // "title\n\nbody" of every issue filed
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
		case r.Method == "POST" && p == "/issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.issues = append(f.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 30 + len(f.issues)})
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			reply([]any{})
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
	p.Own, _ = SplitRepo(codeRepo) // the factory's own repository: repair is mainfix's and the roll task's
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
	p.Own, _ = SplitRepo(codeRepo) // the factory's own repository: the stall records, no repair card files
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

// The chain's silences become alerts (Task 6): a red head raises one open
// chain_red per project — the factory's own repository's too, whose red
// mainfix owns and no card files — a head stalled past its bound raises
// chain_stalled, a later tick adds no row, and a green head resolves the
// repository's open rows real.
func TestChainAlertsRule(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(62, commitC2, "failure")},
			demoRepo: {pushRun(21, demoCommit, "success")},
			webRepo:  {pushRun(31, webCommit, "success")},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo), seedProject(webRepo))
	p.Own, _ = SplitRepo(codeRepo)
	p.Workflows = &fakeWorkflows{}
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}, Stall: time.Minute}},
			"web":  {Repository: webRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}, Stall: time.Minute}},
		}}}
	}
	clock := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	store.chainNow = p.Now
	open := func(kind, repository string) int {
		n := 0
		for _, a := range store.alerts {
			if a.Kind == kind && a.Fingerprint == repository && a.Source == repository && a.Verdict == "" {
				n++
			}
		}
		return n
	}
	resolved := func(kind, repository string) bool {
		for _, a := range store.alerts {
			if a.Kind == kind && a.Fingerprint == repository {
				return a.Verdict == VerdictReal
			}
		}
		return false
	}
	// the red head alerts at once; the two heads held pending by a
	// missing required context alert nothing yet
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if open(AlertChainRed, codeRepo) != 1 || open(AlertChainRed, demoRepo) != 0 {
		t.Fatalf("alerts %+v, want one open chain_red of the red head alone", store.alerts)
	}
	// past the stall bound both pending heads stall: one chain_stalled
	// each, and the red head's second tick adds no row
	clock = clock.Add(2 * time.Minute)
	for range 2 {
		if err := p.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if open(AlertChainRed, codeRepo) != 1 || open(AlertChainStalled, demoRepo) != 1 || open(AlertChainStalled, webRepo) != 1 {
		t.Fatalf("alerts %+v, want one open row per rule and project", store.alerts)
	}
	// the demo head concludes green: its stalled row resolves real, the
	// still-open ones stay
	f.statuses = map[string]map[string][]map[string]string{
		demoRepo: {demoCommit: {{"status": "success", "context": "factory/roll"}}},
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !resolved(AlertChainStalled, demoRepo) || open(AlertChainStalled, webRepo) != 1 || open(AlertChainRed, codeRepo) != 1 {
		t.Fatalf("alerts %+v, want the green head's rule resolved and the others open", store.alerts)
	}
	// the red head goes green too: every chain alert of the three stands
	// resolved
	f.runs[codeRepo] = []map[string]any{pushRun(62, commitC2, "success")}
	f.statuses = map[string]map[string][]map[string]string{
		demoRepo: {demoCommit: {{"status": "success", "context": "factory/roll"}}},
		webRepo:  {webCommit: {{"status": "success", "context": "factory/roll"}}},
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !resolved(AlertChainRed, codeRepo) || !resolved(AlertChainStalled, webRepo) {
		t.Fatalf("alerts %+v, want every rule resolved at its green head", store.alerts)
	}
}

// chainRun is a push run with its run number, which a repair card links.
func chainRun(id int64, sha, status string) map[string]any {
	m := pushRun(id, sha, status)
	m["index_in_repo"] = id
	return m
}

var (
	demoNext  = "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"
	webRepo   = "sf/demo-web"
	webCommit = "7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b7b"
)

// repairCards are the store's open-and-closed repair cards, by repository.
func repairCards(s *fakeStore) map[string][]Task {
	out := map[string][]Task{}
	for _, t := range s.tasks {
		if t.Source == SourceChainRepair {
			out[t.Repository] = append(out[t.Repository], t)
		}
	}
	return out
}

// A red head files one repair card per repository and no second while it
// stands: the factory's own repository files none — mainfix owns a red
// push run there, the roll task a red roll — every other project's red
// head files its own card, and the open card holds the next red tick
// back.
func TestRepairFilesOneCardPerRedHead(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(62, commitC2, "failure")},
			demoRepo: {chainRun(21, demoCommit, "failure")},
			webRepo:  {chainRun(31, webCommit, "failure")},
		},
	}
	wf := &fakeWorkflows{}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo), seedProject(webRepo))
	p.Workflows = wf
	p.Own, _ = SplitRepo(codeRepo)
	for range 2 {
		if err := p.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cards := repairCards(store)
	if _, ok := cards[codeRepo]; ok {
		t.Fatalf("repair cards of the factory's own repository: %+v", cards[codeRepo])
	}
	for _, repo := range []string{demoRepo, webRepo} {
		if len(cards[repo]) != 1 {
			t.Fatalf("%d repair cards of %s, want one: %+v", len(cards[repo]), repo, cards[repo])
		}
	}
	if len(wf.started) != 2 {
		t.Fatalf("started %v, want each repair card's workflow started", wf.started)
	}
}

// The repair card carries the findings: the failing step, the failing
// run's link, and the issue numbers of the cards whose change the head
// carried — a queued agent card of the project's lane ready on the head.
func TestRepairCardCarriesTheFindings(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{demoRepo: {chainRun(21, demoCommit, "failure")}},
		carries: map[string]map[string][]map[string]any{
			demoRepo: {"parent-of-" + demoCommit + "..." + demoCommit: {{"sha": demoMerge}}},
		},
		merged: map[string][]map[string]any{
			demoRepo: {{"number": 3, "merged": true, "merge_commit_sha": demoMerge}},
		},
		statuses: map[string]map[string][]map[string]string{
			demoRepo: {demoCommit: {{"status": "failure", "context": "factory/roll"}}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(demoRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}}},
		}}}
	}
	p.Workflows = &fakeWorkflows{}
	store.tasks["t-x"] = Task{ID: "t-x", Repository: demoRepo, PRNumber: 3, IssueNumber: 77, Status: "merged"}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	cards := repairCards(store)
	if len(cards[demoRepo]) != 1 {
		t.Fatalf("repair cards %+v, want one", cards)
	}
	card := cards[demoRepo][0]
	for _, want := range []string{
		"run 21 failure",
		"required context factory/roll: failure",
		"/" + demoRepo + "/actions/runs/21",
		"#77",
	} {
		if !strings.Contains(card.Objective, want) {
			t.Fatalf("objective misses %q:\n%s", want, card.Objective)
		}
	}
	if card.Lane != policy.LaneReady || card.Status != "queued" || card.Kind != "agent" || card.SHA != demoCommit {
		t.Fatalf("card %+v, want a queued agent card of lane ready on the head", card)
	}
}

// A stalled head records the stall on its row and files the repair card.
// The stall clock is the head row's own stamp — frozen while its verdict
// stands still — so a head pending since before this factoryd started
// stalls on the first poll back, not after a fresh bound; a head
// superseded while pending leaves its episode behind.
func TestStalledHeadRecordsAndRepairs(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{demoRepo: {pushRun(21, demoCommit, "success")}},
	}
	p, store := newTestPoller(t, f, seedProject(demoRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}, Stall: time.Minute}},
		}}}
	}
	p.Workflows = &fakeWorkflows{}
	clock := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	store.chainNow = p.Now
	// the head row pending since two minutes before this factoryd started
	store.chain = []fakeChainRun{{repo: demoRepo, pr: 0, sha: demoCommit, run: 21, status: "pending", updated: clock.Add(-2 * time.Minute)}}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, demoRepo); got[0].status != "stalled" {
		t.Fatalf("head row %+v, want the stall recorded", got[0])
	}
	cards := repairCards(store)
	if len(cards[demoRepo]) != 1 || !strings.Contains(cards[demoRepo][0].Objective, "stalled past 1m0s at "+demoCommit) {
		t.Fatalf("repair cards %+v, want one naming the stall and its bound", cards[demoRepo])
	}
	// a newer head arrives still pending: the superseded episode ends, the
	// new one's clock is its own row's
	f.runs[demoRepo] = []map[string]any{pushRun(22, demoNext, "success")}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.episodes) != 1 || p.episodes[headKey{demoRepo, demoNext}].since.IsZero() {
		t.Fatalf("episodes %+v, want only the new head's, armed", p.episodes)
	}
	// the context reports success on the new head: the episode ends
	f.statuses = map[string]map[string][]map[string]string{
		demoRepo: {demoNext: {{"status": "success", "context": "factory/roll"}}},
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, demoRepo); got[0].status != "success" {
		t.Fatalf("head row %+v, want the concluded head green", got[0])
	}
	if len(p.episodes) != 0 {
		t.Fatalf("episodes %+v, want the concluded head's ended", p.episodes)
	}
}

// goneWorkflows plays the Temporal half for cards whose workflow cannot
// take a signal: Alive answers for the cards it names, as Temporal does
// — missing, completed, failed and terminated are not alive.
type goneWorkflows struct {
	*fakeWorkflows
	alive map[string]bool
}

func (w *goneWorkflows) Alive(ctx context.Context, taskID string) (bool, error) {
	return w.alive[taskID], nil
}

// MigrateChainLimbo mirrors PGStore's migration read: every done card
// whose carry row no green run covers, moved to merged.
func (s *fakeStore) MigrateChainLimbo(ctx context.Context, repository string) ([]string, error) {
	green := int64(0)
	for _, c := range s.chain {
		if c.repo == repository && c.status == "success" && c.run > green {
			green = c.run
		}
	}
	var moved []string
	for _, t := range s.tasks {
		if t.Repository != repository || t.Status != "done" || t.PRNumber <= 0 {
			continue
		}
		mine := int64(0)
		for _, c := range s.chain {
			if c.repo == repository && c.pr == t.PRNumber && (mine == 0 || c.run < mine) {
				mine = c.run
			}
		}
		if mine == 0 || green >= mine {
			continue
		}
		if err := s.TransitionTask(ctx, t.ID, []string{"done"}, "merged"); err != nil {
			return nil, err
		}
		moved = append(moved, t.ID)
	}
	return moved, nil
}

// The startup pass moves the limbo (Task 9): a served repository's done
// cards the chain never confirmed live move to merged; a repository
// whose chain is none moves nothing, its cards held by the grandfather
// clause.
func TestStartupMigrationMovesTheLimbo(t *testing.T) {
	f := &chainForgejo{}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: codeRepo},
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Mode: chainModeNone}},
		}}}
	}
	store.tasks = map[string]Task{
		"t-limbo": {ID: "t-limbo", Status: "done", Repository: codeRepo, PRNumber: 12},
		"t-none":  {ID: "t-none", Status: "done", Repository: demoRepo, PRNumber: 5},
	}
	// each repository's carry row past its green run: limbo where served
	store.chain = []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: "c2", run: 12, status: "success"},
		{repo: codeRepo, pr: 12, sha: "c4", run: 14, status: "failure"},
		{repo: demoRepo, pr: 5, sha: "d3", run: 3, status: "failure"},
	}
	p.migrate(context.Background())
	if got := store.tasks["t-limbo"].Status; got != "merged" {
		t.Fatalf("t-limbo = %s, want merged", got)
	}
	if got := store.tasks["t-none"].Status; got != "done" {
		t.Fatalf("t-none = %s, want done: the repository's chain is none", got)
	}
	// the pass is idempotent: a second one finds nothing to move
	p.migrate(context.Background())
	if got := store.tasks["t-limbo"].Status; got != "merged" {
		t.Fatalf("t-limbo = %s after the second pass, want merged", got)
	}
}

// A covered card whose workflow is gone — completed done long before, a
// signal it drops and no restart that could take it (Task 9) — finishes
// in the store, naming the run that covers it; a card whose workflow
// stands takes the signal and decides itself.
func TestCoveredCardWithoutAWorkflowFinishesInTheStore(t *testing.T) {
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
	wf := &goneWorkflows{fakeWorkflows: &fakeWorkflows{}, alive: map[string]bool{"t-live": true}}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Workflows = wf
	store.tasks = map[string]Task{
		"t-gone": {ID: "t-gone", Status: "merged", Repository: codeRepo, PRNumber: 12},
		"t-live": {ID: "t-live", Status: "merged", Repository: codeRepo, PRNumber: 12},
	}
	if err := p.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := store.tasks["t-gone"].Status; got != "done" {
		t.Fatalf("t-gone = %s, want done in the store", got)
	}
	if !store.finished["t-gone"].After(time.Time{}) {
		t.Fatal("t-gone finished at nothing")
	}
	if got := store.tasks["t-live"].Status; got != "merged" {
		t.Fatalf("t-live = %s, want merged: its workflow decides", got)
	}
	if len(wf.signals) != 2 {
		t.Fatalf("signals = %v, want both cards told", wf.signals)
	}
}
