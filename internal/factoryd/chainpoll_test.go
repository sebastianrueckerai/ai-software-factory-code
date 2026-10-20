package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
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

// MergedCardCommits mirrors PGStore's read: the card of each of shas
// that has a pull request and a recorded merge commit.
func (s *fakeStore) MergedCardCommits(ctx context.Context, repository string, shas []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range s.tasks {
		if t.Repository == repository && t.PRNumber > 0 && t.MergeCommit != "" && slices.Contains(shas, t.MergeCommit) {
			out[t.MergeCommit] = t.PRNumber
		}
	}
	return out, nil
}

// UncarriedMerged mirrors PGStore's read: the merged cards with a pull
// request and no carry row, the merge commit or none.
func (s *fakeStore) UncarriedMerged(ctx context.Context, repository string) ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		if t.Repository != repository || t.Status != "merged" || t.PRNumber <= 0 {
			continue
		}
		carried := false
		for _, c := range s.chain {
			carried = carried || c.repo == repository && c.pr == t.PRNumber
		}
		if !carried {
			out = append(out, t)
		}
	}
	return out, nil
}

// ResolveChainUncarried mirrors PGStore's: the repository's open
// chain_uncarried rows no standing fingerprint names are resolved real.
func (s *fakeStore) ResolveChainUncarried(ctx context.Context, repository string, standing []string) error {
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.Kind == AlertChainUncarried && a.Source == repository && a.Verdict == "" && !slices.Contains(standing, a.Fingerprint) {
			s.alerts[i].Verdict = VerdictReal
			s.alerts[i].ResolvedBy = "factoryd"
		}
	}
	return nil
}

// seedCards gives store the cards of f.merged: the merged pull requests
// as the cards that own them, each with its merge commit.
func (f *chainForgejo) seedCards(store *fakeStore) {
	for repo, prs := range f.merged {
		for _, pr := range prs {
			n, commit := int64(pr["number"].(int)), pr["merge_commit_sha"].(string)
			id := fmt.Sprintf("t-%s-%d", repo, n)
			for _, t := range store.tasks {
				if t.Repository == repo && t.PRNumber == n {
					id = t.ID
				}
			}
			t, ok := store.tasks[id]
			if !ok {
				t = Task{ID: id, Repository: repo, PRNumber: n, Status: "in-review"}
			}
			t.MergeCommit = commit
			store.tasks[id] = t
		}
	}
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
	merged    map[string][]map[string]any               // repository -> its merged pull requests, seeded as cards
	statuses  map[string]map[string][]map[string]string // repository -> sha -> the statuses reported on it
	workflows map[string][]string                       // repository -> its workflow files; one the map does not name has ci.yml
	invisible map[string]bool                           // repositories the projects token cannot see: every call answers 404
	issues    []string                                  // "title\n\nbody" of every issue filed
	reads     []string                                  // "repository dir" of every workflow-list read
	failing   map[string]bool                           // "base...head" compares that answer 500
	compares  int                                       // the compare calls served
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
		if !ok || owner == "" || name == "" {
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
			f.compares++
			base, head, _ := strings.Cut(strings.TrimPrefix(p, "/compare/"), "...")
			if f.failing[base+"..."+head] {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			reply(map[string]any{"commits": f.carries[repo][base+"..."+head]})
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
			if f.invisible[repo] {
				http.NotFound(w, r)
				return
			}
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
		case r.Method == "GET" && p == "":
			if f.invisible[repo] {
				http.NotFound(w, r)
				return
			}
			reply(map[string]any{"name": repo})
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
	f.seedCards(store)
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
	if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 62, status: "success"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 62, status: "success"},
	}) {
		t.Fatalf("chain after the re-run %+v, want every row green", got)
	}
}

// A run the chain skipped never ran the change: it holds its rows
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
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "pending"},
	}) {
		t.Fatalf("chain of a skipped run %+v, want the rows pending", got)
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
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "pending"},
	}) {
		t.Fatalf("chain of a cancelled run %+v, want the rows pending", got)
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
		if err := p.Tick(context.Background()); err != nil {
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
	})); n != 4 {
		t.Fatalf("%d workflow-list reads of %s, want the two directories read on each of two ticks", n, demoRepo)
	}
}

// A required context missing on the commit holds its rows pending;
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
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "pending"},
	}) {
		t.Fatalf("chain under a missing context %+v, want the rows pending", got)
	}
	if strings.Contains(logs.String(), "stalled") {
		t.Fatalf("stall called within the bound: %s", logs.String())
	}
	// the context still missing a minute past the bound: the head stalled
	clock = clock.Add(2 * time.Minute)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "stalled"); n != 1 {
		t.Fatalf("stall called %d times past the bound, want once: %s", n, logs.String())
	}
	// the context reports success: the head concludes
	f.statuses = map[string]map[string][]map[string]string{
		codeRepo: {commitC2: {{"status": "success", "context": "factory/roll"}}},
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); got[0].status != "success" {
		t.Fatalf("chain after the context concluded %+v, want the head row green", got)
	}
}

// A carry row carries its commit's verdict, not its run's own status:
// a run concluding success while a required context is still pending
// writes its carry row pending, so the coverage read holds the card —
// re-read after a restart, it still holds — and the context concluding
// green is what covers the card. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestTheCarryRowCarriesItsVerdict(t *testing.T) {
	dsn := schematest.DSN(t, "factoryd")
	ctx := context.Background()
	pg, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer pg.db.Close()
	if _, err := pg.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs, alerts CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	chainPolicy := func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: codeRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}, Stall: time.Hour}},
		}}}
	}
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}},
		},
		statuses: map[string]map[string][]map[string]string{
			codeRepo: {commitC2: {{"status": "pending", "context": "factory/roll"}}},
		},
	}
	tk := Task{ID: "t-wait", Status: "merged", Repository: codeRepo, PRNumber: 12, BudgetEUR: 1, Kind: "agent"}
	if err := pg.CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := pg.SetTaskMergeCommit(ctx, "t-wait", mergePR12); err != nil {
		t.Fatal(err)
	}
	projects := newFakeStore()
	projects.projects = []Project{seedProject(codeRepo)}
	fj := f.srv(t)
	var ended []string
	end := func(ctx context.Context, taskID, final, reason string) error {
		ended = append(ended, taskID+" "+reason)
		return nil
	}
	poll := func() { // a fresh poller: factoryd restarted
		t.Helper()
		p := &ChainPoller{Forgejo: fj, Store: pg, Projects: projects, Policy: chainPolicy, End: end}
		if err := p.Tick(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
	}
	row := func() string {
		var status string
		if err := pg.db.QueryRowContext(ctx, `SELECT status FROM chain_runs WHERE repository = $1 AND pr_number = 12`, codeRepo).Scan(&status); err != nil {
			t.Fatalf("carry row: %v", err)
		}
		return status
	}
	covered := func() int64 { // the run that covers t-wait, 0 for none
		t.Helper()
		m, err := pg.ChainCovered(ctx, codeRepo)
		if err != nil {
			t.Fatalf("ChainCovered: %v", err)
		}
		return m["t-wait"]
	}
	// the run concluded success, the context has not: the carry row holds
	// the commit's verdict, the card stays uncovered
	poll()
	if got := row(); got != "pending" {
		t.Fatalf("carry row = %s, want pending: the pending context holds the commit", got)
	}
	if run := covered(); run != 0 {
		t.Fatalf("t-wait covered by run %d, a chain whose required context is pending", run)
	}
	// the restart reads the recorded facts again: they hold
	poll()
	if got := row(); got != "pending" {
		t.Fatalf("carry row = %s after the restart, want pending", got)
	}
	if run := covered(); run != 0 {
		t.Fatalf("after the restart: covered by run %d, want the card still held", run)
	}
	// the context concludes success: the same row concludes, the card is
	// covered and live at the run that carried it
	f.statuses[codeRepo][commitC2] = []map[string]string{{"status": "success", "context": "factory/roll"}}
	poll()
	if got := row(); got != "success" {
		t.Fatalf("carry row = %s after the context concluded, want success", got)
	}
	if run := covered(); run != 61 {
		t.Fatalf("after the context concluded: covered by run %d, want live at run 61", run)
	}
	if !slices.Equal(ended, []string{"t-wait live: covered by run 61"}) {
		t.Fatalf("ended %q, want t-wait once, on the tick its context concluded", ended)
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
		if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "pending"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "pending"},
	}) {
		t.Fatalf("chain after the first recording %+v, want pr 12 pending under run 61", got)
	}
	// the run finished, and a newer one arrived carrying another pull
	// request: the old row moves to the final status, the new run's pull
	// request is mapped beside it
	f.runs[codeRepo] = []map[string]any{pushRun(62, d3, "success"), pushRun(61, commitC2, "success")}
	if err := p.Tick(ctx); err != nil {
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
	if err := p.Tick(ctx); err != nil {
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

// Every tick ends every merged card a green chain covers through End,
// naming the run: a card the chain has not covered yet is left, an End
// that fails stops no other card, and the next tick ends a card still
// merged again — End on a card already done is the no-op that carries
// idempotence.
func TestEveryTickEndsTheCoveredMergedCards(t *testing.T) {
	p, store := newTestPoller(t, &chainForgejo{}, seedProject(codeRepo))
	store.tasks = map[string]Task{
		"t-green": {ID: "t-green", Status: "merged", Repository: codeRepo, PRNumber: 12},
		"t-later": {ID: "t-later", Status: "merged", Repository: codeRepo, PRNumber: 13},
		"t-wait":  {ID: "t-wait", Status: "merged", Repository: codeRepo, PRNumber: 14},
	}
	// pr 13 rode the red run 11, pr 12 the green run 12 after it, pr 14
	// the run 14 still running
	store.chain = []fakeChainRun{
		{repo: codeRepo, pr: 13, sha: "c11", run: 11, status: "failure"},
		{repo: codeRepo, pr: 12, sha: "c12", run: 12, status: "success"},
		{repo: codeRepo, pr: 14, sha: "c14", run: 14, status: "pending"},
	}
	var ended []string
	p.End = func(ctx context.Context, taskID, final, reason string) error {
		ended = append(ended, taskID+" "+final+" "+reason)
		return errors.New("forgejo is down")
	}
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	want := []string{"t-green done live: covered by run 12", "t-later done live: covered by run 12"}
	for tick := 1; tick <= 2; tick++ {
		ended = nil
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		slices.Sort(ended)
		if !slices.Equal(ended, want) {
			t.Fatalf("tick %d ended %q, want %q", tick, ended, want)
		}
	}
	for _, id := range []string{"t-green", "t-later"} {
		if n := strings.Count(logs.String(), "chain poller: "+codeRepo+": end "+id+": forgejo is down\n"); n != 1 {
			t.Fatalf("the failed end of %s logged %d times, want once: %s", id, n, logs.String())
		}
	}
}

// A covered card whose End keeps failing logs its failure once, not every
// tick; an End that succeeds forgets it, so the next failure logs again.
func TestAFailingEndLogsOnceUntilItSucceeds(t *testing.T) {
	p, store := newTestPoller(t, &chainForgejo{}, seedProject(codeRepo))
	store.tasks = map[string]Task{"t-green": {ID: "t-green", Status: "merged", Repository: codeRepo, PRNumber: 12}}
	store.chain = []fakeChainRun{{repo: codeRepo, pr: 12, sha: "c12", run: 12, status: "success"}}
	down := errors.New("forgejo is down")
	ends := []error{down, down, nil, down}
	p.End = func(ctx context.Context, taskID, final, reason string) error {
		err := ends[0]
		ends = ends[1:]
		return err
	}
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	line := "chain poller: " + codeRepo + ": end t-green: forgejo is down\n"
	for tick, want := range []int{1, 1, 1, 2} {
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(logs.String(), line); n != want {
			t.Fatalf("after tick %d the failed end logged %d times, want %d: %s", tick+1, n, want, logs.String())
		}
	}
}

// A card merged while the head is already green (the review's focus):
// the green run that carried its change was recorded a tick before the
// card reached merged, so no new head names it. The next tick ends it
// done all the same, through the runner's Finish: one task_ended, so one
// done poke, and no alert. A later tick finds no merged card to end: the
// end and its poke stay one.
func TestACardMergedUnderAGreenHeadEndsOnTheNextTick(t *testing.T) {
	r, hub, store, objects := chainRunner(t, withStatus(mergedCard, "in-review"), policy.ChainModeRun)
	hub.pulls = map[int64]string{7: mergedAt(7, mergePR12)}
	f := &chainForgejo{
		runs: map[string][]map[string]any{"sf/repo": {pushRun(61, commitC2, "success")}},
		carries: map[string]map[string][]map[string]any{
			"sf/repo": {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}}},
		},
		merged: map[string][]map[string]any{
			"sf/repo": {{"number": 7, "merged": true, "merge_commit_sha": mergePR12}},
		},
	}
	store.projects = []Project{seedProject("sf/repo")}
	f.seedCards(store)
	ends := 0
	end := func(ctx context.Context, taskID, final, reason string) error {
		ends++
		return r.Finish(ctx, taskID, final, reason)
	}
	p := &ChainPoller{Forgejo: f.srv(t), Store: store, Projects: store, Policy: r.cfg.Policy, End: end}
	ctx := context.Background()
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, "sf/repo"); !slices.Equal(got, []fakeChainRun{
		{repo: "sf/repo", pr: 0, sha: commitC2, run: 61, status: "success"},
		{repo: "sf/repo", pr: 7, sha: commitC2, run: 61, status: "success"},
	}) {
		t.Fatalf("chain %+v, want the head and pr 7 green at run 61", got)
	}
	if ends != 0 {
		t.Fatalf("ended %d times before the merge, want none", ends)
	}
	if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(ctx, "t1"); got.Status != "merged" {
		t.Fatalf("status at the merge = %q, want merged", got.Status)
	}
	for tick := 1; tick <= 2; tick++ {
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := store.Task(ctx, "t1"); got.Status != "done" {
			t.Fatalf("status after tick %d past the merge = %q, want done", tick, got.Status)
		}
		if ends != 1 {
			t.Fatalf("ended %d times after tick %d past the merge, want once", ends, tick)
		}
		if got := taskEnds(t, objects, "t1"); !slices.Equal(got, []string{"done: live: covered by run 61"}) {
			t.Fatalf("task_ended after tick %d = %q, want one naming run 61", tick, got)
		}
		if len(store.alerts) != 0 {
			t.Fatalf("alerts after tick %d = %+v, want none", tick, store.alerts)
		}
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
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if open(AlertChainRed, codeRepo) != 1 || open(AlertChainRed, demoRepo) != 0 {
		t.Fatalf("alerts %+v, want one open chain_red of the red head alone", store.alerts)
	}
	// past the stall bound both pending heads stall: one chain_stalled
	// each, and the red head's second tick adds no row
	clock = clock.Add(2 * time.Minute)
	for range 2 {
		if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(context.Background()); err != nil {
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
		if err := p.Tick(context.Background()); err != nil {
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
	store.tasks["t-x"] = Task{ID: "t-x", Repository: demoRepo, PRNumber: 3, IssueNumber: 77, Status: "merged", MergeCommit: demoMerge}
	if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(context.Background()); err != nil {
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
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.episodes) != 1 || p.episodes[headKey{demoRepo, demoNext}].since.IsZero() {
		t.Fatalf("episodes %+v, want only the new head's, armed", p.episodes)
	}
	// the context reports success on the new head: the episode ends
	f.statuses = map[string]map[string][]map[string]string{
		demoRepo: {demoNext: {{"status": "success", "context": "factory/roll"}}},
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, demoRepo); got[0].status != "success" {
		t.Fatalf("head row %+v, want the concluded head green", got[0])
	}
	if len(p.episodes) != 0 {
		t.Fatalf("episodes %+v, want the concluded head's ended", p.episodes)
	}
}

// A repository the projects token cannot see answers every call with a
// 404, which reads exactly like a repository without workflows. The
// chain must not mistake one for the other: an invisible repository is
// a configuration fault — logged every poll, alerted once, never
// cached — while a workflow-less one degrades once and falls silent.
func TestChainPollerAlertsOnAnInvisibleRepository(t *testing.T) {
	f := &chainForgejo{
		invisible: map[string]bool{demoRepo: true},
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
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	for range 2 {
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := chainOf(store, demoRepo); len(got) != 0 {
		t.Fatalf("chain of the invisible %s %+v, want nothing recorded", demoRepo, got)
	}
	if n := strings.Count(logs.String(), demoRepo+" is invisible"); n != 2 {
		t.Fatalf("the invisible repository logged %d times, want every poll: %s", n, logs.String())
	}
	alerts := 0
	for _, a := range store.alerts {
		if a.Kind == AlertChainStalled && a.Source == demoRepo {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("the invisible repository raised %d chain_stalled alerts, want one deduped row", alerts)
	}
	if got := chainOf(store, codeRepo); len(got) != 2 {
		t.Fatalf("chain of %s %+v, want the workflow-bearing repository recorded", codeRepo, got)
	}
}

// A run carries the cards whose own merge commit lies in its compare
// range: the card merged in the range gets a row, the one merged
// elsewhere none, and a pull request nobody holds a card for none.
func TestChainCarriedMapsByMergeCommit(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}, {"sha": "9f9f"}}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	store.tasks["t-in"] = Task{ID: "t-in", Repository: codeRepo, PRNumber: 12, Status: "in-review", MergeCommit: mergePR12}
	store.tasks["t-out"] = Task{ID: "t-out", Repository: codeRepo, PRNumber: 14, Status: "in-review", MergeCommit: mergePR14}
	store.tasks["t-open"] = Task{ID: "t-open", Repository: codeRepo, PRNumber: 15, Status: "in-review"}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 61, status: "success"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 61, status: "success"},
	}) {
		t.Fatalf("chain %+v, want the head and pr 12 only", got)
	}
}

// Forgejo cancels the run of a commit a newer push superseded: the cards
// it carried keep their row under the cancelled run, and the next green
// run covers them, not their own.
func TestACancelledRunsCardIsCoveredByTheNextGreen(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(10, commitC1, "cancelled")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {
				"parent-of-" + commitC1 + "..." + commitC1: {{"sha": mergePR12}},
				commitC1 + "..." + commitC2:                {},
			},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	store.tasks["t-7"] = Task{ID: "t-7", Repository: codeRepo, PRNumber: 7, Status: "merged", MergeCommit: mergePR12}
	var ended []string
	p.End = func(ctx context.Context, taskID, final, reason string) error {
		ended = append(ended, taskID+" "+final+" "+reason)
		return nil
	}
	ctx := context.Background()
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.ChainCovered(ctx, codeRepo); len(got) != 0 || len(ended) != 0 {
		t.Fatalf("covered %v, ended %v after the cancelled run, want none", got, ended)
	}
	f.runs[codeRepo] = []map[string]any{pushRun(11, commitC2, "success"), pushRun(10, commitC1, "cancelled")}
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.ChainCovered(ctx, codeRepo); !maps.Equal(got, map[string]int64{"t-7": 11}) {
		t.Fatalf("covered %v, want t-7 by run 11", got)
	}
	if !slices.Equal(ended, []string{"t-7 done live: covered by run 11"}) {
		t.Fatalf("ended %v, want t-7 done by run 11", ended)
	}
}

// A card whose merge commit is recorded after the poller mapped the run
// that carries it still gets its row, at that run, with the head left as
// it was; a row a card already has never moves; a card that stays
// uncarried costs no compare the poller made before.
func TestACardRecordedLateGetsItsCarryRow(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(62, commitC2, "success"), pushRun(61, commitC1, "success")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {
				"parent-of-" + commitC1 + "..." + commitC1: {{"sha": "b0b0"}},
				commitC1 + "..." + commitC2:                {{"sha": mergePR12}, {"sha": mergePR14}},
			},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.End = func(ctx context.Context, taskID, final, reason string) error { return nil }
	store.tasks["t-late"] = Task{ID: "t-late", Repository: codeRepo, PRNumber: 12, Status: "merged"}
	store.tasks["t-never"] = Task{ID: "t-never", Repository: codeRepo, PRNumber: 13, Status: "merged", MergeCommit: "0c0c"}
	store.tasks["t-held"] = Task{ID: "t-held", Repository: codeRepo, PRNumber: 14, Status: "merged", MergeCommit: mergePR14}
	ctx := context.Background()
	if err := store.RecordChainRun(ctx, codeRepo, 14, commitC1, 61, "success"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordChainRun(ctx, codeRepo, 0, commitC2, 62, "success"); err != nil {
		t.Fatal(err)
	}
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range chainOf(store, codeRepo) {
		if c.pr == 12 {
			t.Fatalf("chain %+v, want no row for pr 12 before its merge commit is recorded", chainOf(store, codeRepo))
		}
	}
	task := store.tasks["t-late"]
	task.MergeCommit = mergePR12
	store.tasks["t-late"] = task
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 62, status: "success"},
		{repo: codeRepo, pr: 12, sha: commitC2, run: 62, status: "success"},
		{repo: codeRepo, pr: 14, sha: commitC1, run: 61, status: "success"},
	}) {
		t.Fatalf("chain %+v, want pr 12 at run 62, the head and pr 14 (its commit lies in run 62's range) as they were", got)
	}
	before := f.compares
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.compares != before {
		t.Fatalf("a third tick made %d compare calls for the pairs already compared (t-never stays uncarried)", f.compares-before)
	}
}

// The late path never gates the main one: a compare it cannot make logs
// once, and the head row is written and the covered card ended all the
// same.
func TestALateCompareFailureStopsNothingElse(t *testing.T) {
	f := &chainForgejo{
		runs:    map[string][]map[string]any{codeRepo: {pushRun(62, commitC2, "success"), pushRun(61, commitC1, "success")}},
		failing: map[string]bool{"parent-of-" + commitC1 + "..." + commitC1: true},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	var ended []string
	p.End = func(ctx context.Context, taskID, final, reason string) error {
		ended = append(ended, taskID)
		return nil
	}
	store.tasks["t-late"] = Task{ID: "t-late", Repository: codeRepo, PRNumber: 12, Status: "merged", MergeCommit: mergePR12}
	store.tasks["t-cov"] = Task{ID: "t-cov", Repository: codeRepo, PRNumber: 20, Status: "merged", MergeCommit: "0c0c"}
	ctx := context.Background()
	for _, row := range []struct {
		pr  int64
		sha string
		run int64
	}{{20, commitC1, 61}, {0, commitC1, 61}} {
		if err := store.RecordChainRun(ctx, codeRepo, row.pr, row.sha, row.run, "success"); err != nil {
			t.Fatal(err)
		}
	}
	logs := captureLog(t)
	f.carries = map[string]map[string][]map[string]any{codeRepo: {commitC1 + "..." + commitC2: {}}}
	for range 2 {
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var head fakeChainRun
	for _, c := range chainOf(store, codeRepo) {
		if c.pr == 0 {
			head = c
		}
	}
	if head.run != 62 || head.sha != commitC2 {
		t.Fatalf("head row %+v, want run 62", head)
	}
	if !slices.Equal(slices.Compact(ended), []string{"t-cov"}) {
		t.Fatalf("ended %v, want t-cov covered by the green head", ended)
	}
	if n := strings.Count(logs.String(), "late carry rows"); n != 1 {
		t.Fatalf("the late failure logged %d times over two ticks, want once: %s", n, logs.String())
	}
}

// A restart empties the compare cache: the sweep stops at the run whose
// range holds the last uncarried commit, comparing nothing after it.
func TestTheLateSweepStopsAtTheLastCard(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(62, commitC2, "success"), pushRun(61, commitC1, "success")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC1 + "..." + commitC1: {{"sha": "b0b0"}}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.End = func(ctx context.Context, taskID, final, reason string) error { return nil }
	store.tasks["t-a"] = Task{ID: "t-a", Repository: codeRepo, PRNumber: 20, Status: "merged", MergeCommit: "b0b0"}
	ctx := context.Background()
	if err := store.RecordChainRun(ctx, codeRepo, 0, commitC2, 62, "success"); err != nil {
		t.Fatal(err)
	}
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if f.compares != 1 {
		t.Fatalf("%d compare calls, want only the run 61's", f.compares)
	}
	if got := chainOf(store, codeRepo); !slices.Equal(got, []fakeChainRun{
		{repo: codeRepo, pr: 0, sha: commitC2, run: 62, status: "success"},
		{repo: codeRepo, pr: 20, sha: commitC1, run: 61, status: "success"},
	}) {
		t.Fatalf("chain %+v, want pr 20 at run 61", got)
	}
}

// A merged card the chain never carries alerts once its age passes the
// project's stall bound: no row, no run in sight — in a served repository
// and in one the poller does not serve alike. The alert names the card,
// stands once however many ticks pass, and resolves when the card gains
// a carry row or leaves merged.
func TestAnUncarriedMergedCardAlertsAfterTheBound(t *testing.T) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{
			codeRepo: {pushRun(61, commitC2, "success")},
			demoRepo: {pushRun(21, demoCommit, "success")},
		},
		workflows: map[string][]string{demoRepo: {}},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	clock := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	p.End = func(context.Context, string, string, string) error { return nil }
	merged := clock.Add(-10 * time.Minute)
	store.tasks["t-served"] = Task{ID: "t-served", Repository: codeRepo, PRNumber: 12, Status: "merged", MergeCommit: "f00d", FinishedAt: &merged}
	store.tasks["t-unserved"] = Task{ID: "t-unserved", Repository: demoRepo, PRNumber: 3, Status: "merged", FinishedAt: &merged}
	tick := func() {
		t.Helper()
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	open := func() []Alert {
		var out []Alert
		for _, a := range store.alerts {
			if a.Kind == AlertChainUncarried && a.Verdict == "" {
				out = append(out, a)
			}
		}
		slices.SortFunc(out, func(a, b Alert) int { return cmp.Compare(a.Fingerprint, b.Fingerprint) })
		return out
	}
	tick()
	if got := open(); len(got) != 0 {
		t.Fatalf("alerts at 10 minutes %+v, want none: the stall bound is 30m", got)
	}
	clock = clock.Add(21 * time.Minute)
	tick()
	tick()
	got := open()
	if len(got) != 2 {
		t.Fatalf("alerts at 31 minutes %+v, want one per card, however many ticks", got)
	}
	for i, c := range []struct{ repo, card string }{{codeRepo, "t-served"}, {demoRepo, "t-unserved"}} {
		a := got[i]
		if a.Fingerprint != c.repo+" "+c.card || a.Source != c.repo || a.Detail["card"] != c.card || a.Detail["since"] != merged {
			t.Fatalf("alert %+v, want fingerprint %q, source %s and detail card %s since %v", a, c.repo+" "+c.card, c.repo, c.card, merged)
		}
	}
	if err := store.RecordChainRun(context.Background(), codeRepo, 12, commitC2, 61, "success"); err != nil {
		t.Fatal(err)
	}
	tick()
	if got := open(); len(got) != 1 || got[0].Source != demoRepo {
		t.Fatalf("alerts after t-served gained a row %+v, want only t-unserved's standing", got)
	}
	unserved := store.tasks["t-unserved"]
	unserved.Status = "closed"
	store.tasks["t-unserved"] = unserved
	tick()
	if got := open(); len(got) != 0 {
		t.Fatalf("alerts after t-unserved left merged %+v, want none", got)
	}
	for _, a := range store.alerts {
		if a.Verdict != VerdictReal || a.ResolvedBy != "factoryd" {
			t.Fatalf("alert %+v, want resolved real by factoryd", a)
		}
	}
}

// A repository without a workflow is read again on every tick: the
// first workflow it gains serves it on the next tick, recording its
// chain, with the degradation noted once; and once served, a failing
// list read leaves it served.
func TestARepositoryThatGainsAWorkflowIsServed(t *testing.T) {
	f := &chainForgejo{
		runs:      map[string][]map[string]any{demoRepo: {pushRun(21, demoCommit, "success")}},
		workflows: map[string][]string{demoRepo: {}},
	}
	p, store := newTestPoller(t, f, seedProject(demoRepo))
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	for range 2 {
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := chainOf(store, demoRepo); len(got) != 0 {
		t.Fatalf("chain %+v, want nothing recorded while no workflow exists", got)
	}
	if n := strings.Count(logs.String(), demoRepo+" has no workflow"); n != 1 {
		t.Fatalf("degradation noted %d times over two ticks, want once", n)
	}
	f.mu.Lock()
	f.workflows[demoRepo] = []string{"ci.yml"}
	f.mu.Unlock()
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, demoRepo); len(got) != 1 || got[0].run != 21 {
		t.Fatalf("chain %+v, want the head row of run 21 once the repository has a workflow", got)
	}
	f.mu.Lock()
	f.invisible = map[string]bool{demoRepo: true}
	f.mu.Unlock()
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.invisible = nil
	f.runs[demoRepo] = []map[string]any{pushRun(22, demoCommit, "success"), pushRun(21, demoCommit, "success")}
	f.mu.Unlock()
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, demoRepo); len(got) != 1 || got[0].run != 22 {
		t.Fatalf("chain %+v, want a served repository kept served through a failed read", got)
	}
}

// factory.chain.poll_age is computed when the meter collects: the
// seconds since the repository's last successful tick, so a poller that
// stopped reads older at every collection, and a tick resets it.
func TestThePollAgeGaugeGrowsWhileThePollerIsDead(t *testing.T) {
	f := &chainForgejo{runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}}}
	p, _ := newTestPoller(t, f, seedProject(codeRepo))
	clock := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	reader := sdkmetric.NewManualReader()
	if err := p.RegisterPollAge(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test")); err != nil {
		t.Fatal(err)
	}
	age := func() float64 {
		t.Helper()
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "factory.chain.poll_age" {
					continue
				}
				pts := m.Data.(metricdata.Gauge[float64]).DataPoints
				if len(pts) != 1 {
					t.Fatalf("poll_age points %+v, want one", pts)
				}
				if v, _ := pts[0].Attributes.Value("repository"); v.AsString() != codeRepo {
					t.Fatalf("poll_age attributes %v, want repository %s", pts[0].Attributes, codeRepo)
				}
				return pts[0].Value
			}
		}
		t.Fatal("no factory.chain.poll_age collected")
		return 0
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := age(); got != 0 {
		t.Fatalf("age right after a tick = %v, want 0", got)
	}
	clock = clock.Add(45 * time.Second)
	if got := age(); got != 45 {
		t.Fatalf("age 45 s after the last tick = %v, want 45", got)
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(15 * time.Second)
	if got := age(); got != 15 {
		t.Fatalf("age 15 s after a new tick = %v, want 15", got)
	}
}
