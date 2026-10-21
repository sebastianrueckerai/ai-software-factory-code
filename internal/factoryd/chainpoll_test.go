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
	// the head row's stall ladder, kept while its commit stays
	rerunAt   time.Time
	dismissed bool
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
		row := fakeChainRun{repo: repository, pr: 0, sha: sha, run: run, status: status, updated: s.stamp()}
		if c.sha == sha {
			row.rerunAt, row.dismissed = c.rerunAt, c.dismissed
		}
		s.chain[i] = row
		return nil
	}
	s.chain = append(s.chain, fakeChainRun{repo: repository, pr: 0, sha: sha, run: run, status: status, updated: s.stamp()})
	return nil
}

// UnsettledChainCommits mirrors PGStore's read: the commits of shas with
// a carry row not yet success.
func (s *fakeStore) UnsettledChainCommits(ctx context.Context, repository string, shas []string) (map[string]bool, error) {
	s.unsettledReads++
	out := map[string]bool{}
	for _, c := range s.chain {
		if c.repo == repository && c.pr > 0 && c.status != "success" && slices.Contains(shas, c.sha) {
			out[c.sha] = true
		}
	}
	return out, nil
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

// ChainCard is repository's newest card of source.
func (s *fakeStore) ChainCard(ctx context.Context, repository, source string) (Task, bool, error) {
	var newest Task
	ok := false
	for _, t := range s.tasks {
		if t.Repository == repository && t.Source == source && (!ok || t.CreatedAt.After(newest.CreatedAt)) {
			newest, ok = t, true
		}
	}
	return newest, ok, nil
}

// ChainLadder is the head row of sha's stall ladder.
func (s *fakeStore) ChainLadder(ctx context.Context, repository, sha string) (ChainLadder, error) {
	for _, c := range s.chain {
		if c.repo == repository && c.pr == 0 && c.sha == sha {
			return ChainLadder{RerunAt: c.rerunAt, Dismissed: c.dismissed}, nil
		}
	}
	return ChainLadder{}, nil
}

// MarkChainRerun stamps the head row of sha re-run at the fake's clock,
// which a test that re-runs wires.
func (s *fakeStore) MarkChainRerun(ctx context.Context, repository, sha string) error {
	if s.chainNow == nil {
		panic("fixture: a re-run stamps the head row; wire chainNow")
	}
	for i, c := range s.chain {
		if c.repo == repository && c.pr == 0 && c.sha == sha {
			s.chain[i].rerunAt = s.stamp()
		}
	}
	return nil
}

// DismissChainStall mirrors PGStore's: the head row of sha dismissed,
// the open chain_stalled row resolved noise by the member; a head moved
// on or concluded refuses.
func (s *fakeStore) DismissChainStall(ctx context.Context, repository, sha, by string) error {
	for i, c := range s.chain {
		if c.repo != repository || c.pr != 0 {
			continue
		}
		switch {
		case c.sha != sha:
			return ErrStallGone{Head: c.sha}
		case c.status != "pending" && c.status != "stalled":
			return ErrStallGone{Head: c.sha, Verdict: verdictWord(c.status)}
		}
		s.chain[i].dismissed = true
	}
	s.alertsMu.Lock()
	defer s.alertsMu.Unlock()
	for i, a := range s.alerts {
		if a.Kind == AlertChainStalled && a.Fingerprint == repository && a.Verdict == "" {
			s.alerts[i].Verdict, s.alerts[i].ResolvedBy = VerdictNoise, by
		}
	}
	return nil
}

// ChainHeldIssues mirrors PGStore's: the issue numbers of the merged
// cards ChainCovered does not name, ascending.
func (s *fakeStore) ChainHeldIssues(ctx context.Context, repository string) ([]int64, error) {
	covered, _ := s.ChainCovered(ctx, repository)
	var out []int64
	for _, t := range s.tasks {
		if t.Repository == repository && t.Status == "merged" && t.PRNumber > 0 && t.IssueNumber > 0 && covered[t.ID] == 0 {
			out = append(out, t.IssueNumber)
		}
	}
	slices.Sort(out)
	return out, nil
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
			if t.FinishedAt == nil { // the database's tasks_final_finished_at check refuses such a row
				panic("fixture " + t.ID + ": merged without finished_at")
			}
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
	open      map[string][]map[string]any               // repository -> its open issues
	status    []string                                  // the sha of every combined status read
	reads     []string                                  // "repository dir" of every workflow-list read
	failing   map[string]bool                           // "base...head" compares that answer 500
	compares  int                                       // the compare calls served
	rerun     int                                       // the status a re-run answers; 0 is 204
	reruns    []string                                  // "repository run" of every re-run asked
	comments  []string                                  // "repository#issue body" of every comment posted
	closed    []string                                  // "repository#issue" of every issue closed
	labelled  []string                                  // "repository#issue label..." of every label set
	runReads  []string                                  // the repository of every run list read
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
			f.runReads = append(f.runReads, repo)
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
			f.status = append(f.status, sha)
			state := "success"
			for _, s := range f.statuses[repo][sha] {
				if s["status"] == "failure" {
					state = "failure"
				}
			}
			reply(map[string]any{"state": state, "statuses": f.statuses[repo][sha]})
		case r.Method == "GET" && p == "/issues":
			open := []map[string]any{}
			if r.URL.Query().Get("page") == "1" {
				open = append(open, f.open[repo]...)
			}
			reply(open)
		case r.Method == "POST" && p == "/issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.issues = append(f.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 30 + len(f.issues)})
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			var in struct {
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			f.labelled = append(f.labelled, repo+"#"+strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/labels")+" "+strings.Join(in.Labels, " "))
			reply([]any{})
		case r.Method == "POST" && strings.HasPrefix(p, "/actions/runs/") && strings.HasSuffix(p, "/rerun"):
			f.reruns = append(f.reruns, repo+" "+strings.TrimSuffix(strings.TrimPrefix(p, "/actions/runs/"), "/rerun"))
			w.WriteHeader(cmp.Or(f.rerun, http.StatusNoContent))
		case r.Method == "POST" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, repo+"#"+strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")+" "+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"id": len(f.comments)})
		case r.Method == "PATCH" && strings.HasPrefix(p, "/issues/"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["state"] == "closed" {
				f.closed = append(f.closed, repo+"#"+strings.TrimPrefix(p, "/issues/"))
			}
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{})
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
	tk := Task{ID: "t-wait", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12, BudgetEUR: 1, Kind: "agent"}
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
	end := func(ctx context.Context, taskID string, run int64) error {
		ended = append(ended, fmt.Sprintf("%s run %d", taskID, run))
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
	if !slices.Equal(ended, []string{"t-wait run 61"}) {
		t.Fatalf("ended %q, want t-wait once, on the tick its context concluded", ended)
	}
}

// The refresh re-reads only the finished runs whose carry rows are not
// yet green: a green row is final, a red one can still turn green by a
// manual re-run. One store read of the window's rows a tick; with a
// required context, one combined status read per refreshed commit and the
// head's own.
func TestTheRefreshReadsOnlyRunsNotYetGreen(t *testing.T) {
	const c60, c61, c62, c63 = "6060606060", "6161616161", "6262626262", "6363636363"
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {
			pushRun(63, c63, "success"), pushRun(62, c62, "failure"), pushRun(61, c61, "success"), pushRun(60, c60, "success"),
		}},
		statuses: map[string]map[string][]map[string]string{codeRepo: {
			c60: {{"status": "success", "context": "factory/roll"}},
			c61: {{"status": "success", "context": "factory/roll"}},
			c62: {{"status": "success", "context": "factory/roll"}},
			c63: {{"status": "pending", "context": "factory/roll"}},
		}},
	}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: codeRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}}},
		}}}
	}
	p.End = func(context.Context, string, int64) error { return nil }
	store.chain = []fakeChainRun{
		{repo: codeRepo, pr: 4, sha: c60, run: 60, status: "success"},
		{repo: codeRepo, pr: 1, sha: c61, run: 61, status: "success"},
		{repo: codeRepo, pr: 2, sha: c62, run: 62, status: "failure"},
		{repo: codeRepo, pr: 3, sha: c63, run: 63, status: "pending"},
		{repo: codeRepo, pr: 0, sha: c63, run: 63, status: "pending"},
	}
	for tick := 1; tick <= 2; tick++ {
		f.status, store.unsettledReads = nil, 0
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if want := []string{c62, c63, c63}; !slices.Equal(f.status, want) || store.unsettledReads != 1 {
			t.Fatalf("tick %d: status reads %q, store reads %d; want %q and one", tick, f.status, store.unsettledReads, want)
		}
	}
}

// UnsettledChainCommits names the asked commits with a carry row not yet
// green, of the repository asked: a green row, a head row alone and a
// commit not asked name none. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestUnsettledChainCommitsOverPostgres(t *testing.T) {
	ctx := context.Background()
	pg, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer pg.db.Close()
	if _, err := pg.db.ExecContext(ctx, `TRUNCATE chain_runs`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, row := range []struct {
		repo   string
		pr     int64
		sha    string
		run    int64
		status string
	}{
		{"sf/r", 1, "a1", 1, "success"},
		{"sf/r", 2, "b2", 2, "failure"},
		{"sf/r", 3, "c3", 3, "pending"},
		{"sf/r", 0, "e5", 5, "pending"},
		{"sf/r", 4, "d4", 4, "pending"},
		{"sf/o", 5, "a1", 5, "pending"},
	} {
		if err := pg.RecordChainRun(ctx, row.repo, row.pr, row.sha, row.run, row.status); err != nil {
			t.Fatalf("RecordChainRun %+v: %v", row, err)
		}
	}
	got, err := pg.UnsettledChainCommits(ctx, "sf/r", []string{"a1", "b2", "c3", "e5"})
	if want := map[string]bool{"b2": true, "c3": true}; err != nil || !maps.Equal(got, want) {
		t.Fatalf("UnsettledChainCommits = %v, %v; want %v", got, err, want)
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
		"t-green": {ID: "t-green", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12},
		"t-later": {ID: "t-later", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 13},
		"t-wait":  {ID: "t-wait", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 14},
	}
	// pr 13 rode the red run 11, pr 12 the green run 12 after it, pr 14
	// the run 14 still running
	store.chain = []fakeChainRun{
		{repo: codeRepo, pr: 13, sha: "c11", run: 11, status: "failure"},
		{repo: codeRepo, pr: 12, sha: "c12", run: 12, status: "success"},
		{repo: codeRepo, pr: 14, sha: "c14", run: 14, status: "pending"},
	}
	var ended []string
	p.End = func(ctx context.Context, taskID string, run int64) error {
		ended = append(ended, fmt.Sprintf("%s run %d", taskID, run))
		return errors.New("forgejo is down")
	}
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	want := []string{"t-green run 12", "t-later run 12"}
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
	store.tasks = map[string]Task{"t-green": {ID: "t-green", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12}}
	store.chain = []fakeChainRun{{repo: codeRepo, pr: 12, sha: "c12", run: 12, status: "success"}}
	down := errors.New("forgejo is down")
	ends := []error{down, down, nil, down}
	p.End = func(ctx context.Context, taskID string, run int64) error {
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
	end := func(ctx context.Context, taskID string, run int64) error {
		ends++
		return r.EndCovered(ctx, taskID, run)
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
	store.tasks["t-x"] = Task{ID: "t-x", Repository: demoRepo, PRNumber: 3, IssueNumber: 77, Status: "merged", FinishedAt: justNow(), MergeCommit: demoMerge}
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
		"find the cause, fix it on this card's branch from main's head, and push",
	} {
		if !strings.Contains(card.Objective, want) {
			t.Fatalf("objective misses %q:\n%s", want, card.Objective)
		}
	}
	if card.Lane != policy.LaneReady || card.Status != "queued" || card.Kind != "agent" || card.SHA != demoCommit {
		t.Fatalf("card %+v, want a queued agent card of lane ready on the head", card)
	}
}

// A stalled head records the stall on its row and, its pending part a
// required status, asks a human at once: no repair card. The stall clock
// is the head row's own stamp — frozen while its verdict stands still —
// so a head pending since before this factoryd started stalls on the
// first poll back, not after a fresh bound; a head superseded while
// pending leaves its episode behind.
func TestStalledHeadRecordsAndAsks(t *testing.T) {
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
	if cards := repairCards(store); len(cards) != 0 {
		t.Fatalf("repair cards %+v, want none: a stall is no code work", cards)
	}
	if cards := stallCards(store); len(cards) != 1 || !strings.Contains(cards[0].Objective, "since 2026-10-20 08:58 UTC") {
		t.Fatalf("stall cards %+v, want one naming the stall's start", cards)
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

// stallCards are the store's stall cards, oldest first.
func stallCards(s *fakeStore) []Task {
	var out []Task
	for _, t := range s.tasks {
		if t.Source == SourceChainStall {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b Task) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// stallRig is demo's chain at one-minute stall bound: run 21 on its
// head, status as given, carrying card #77, and requires as the file's
// required contexts; the poller, its store, its Forgejo and its clock.
func stallRig(t *testing.T, status string, requires ...string) (*ChainPoller, *fakeStore, *chainForgejo, *time.Time) {
	t.Helper()
	f := &chainForgejo{
		runs: map[string][]map[string]any{demoRepo: {chainRun(21, demoCommit, status)}},
		carries: map[string]map[string][]map[string]any{
			demoRepo: {"parent-of-" + demoCommit + "..." + demoCommit: {{"sha": demoMerge}}},
		},
	}
	p, store := newTestPoller(t, f, seedProject(demoRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Requires: requires, Stall: time.Minute}},
		}}}
	}
	p.Workflows = &fakeWorkflows{}
	p.End = func(context.Context, string, int64) error { return nil } // a green head covers #77
	p.Forgejo.PublicURL = "https://git.example"
	store.tasks["t-x"] = Task{ID: "t-x", Repository: demoRepo, PRNumber: 3, IssueNumber: 77, Status: "merged", FinishedAt: justNow(), MergeCommit: demoMerge}
	clock := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	store.chainNow = p.Now
	return p, store, f, &clock
}

// tickAt advances the clock by d and polls once.
func tickAt(t *testing.T, p *ChainPoller, clock *time.Time, d time.Duration) {
	t.Helper()
	*clock = clock.Add(d)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// stallChoices are the stall card's three answers.
var stallChoices = []QuestionChoice{
	{Value: AnswerRerunAgain, Label: "Re-run again"},
	{Value: AnswerCheckAgain, Label: "Check again"},
	{Value: AnswerDismiss, Label: "Dismiss"},
}

// askedQuestion is the question the escalation of card id carries.
func askedQuestion(t *testing.T, s *fakeStore, id string) HumanQuestion {
	t.Helper()
	for _, e := range s.intakeEvents {
		if e.TaskID == id && e.Kind == IntakeEscalation && e.Question != nil {
			return *e.Question
		}
	}
	t.Fatalf("card %s asks nothing: %+v", id, s.intakeEvents)
	return HumanQuestion{}
}

// The stall ladder: a run stalled past its bound is re-run once, with no
// card; a bound later, the head still pending, one human card asks with
// three choices, naming what is pending since when, the run's link and
// the cards it holds; a later tick adds nothing. No repair card files.
func TestAStallRerunsOnceThenAsks(t *testing.T) {
	p, store, f, clock := stallRig(t, "running")
	// #76 rode an earlier commit whose run went red: the stall holds it too
	store.tasks["t-old"] = Task{ID: "t-old", Repository: demoRepo, PRNumber: 2, IssueNumber: 76, Status: "merged", FinishedAt: justNow(), MergeCommit: "0b0b"}
	store.chain = append(store.chain, fakeChainRun{repo: demoRepo, pr: 2, sha: "0b0b", run: 20, status: "failure"})
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	if !slices.Equal(f.reruns, []string{demoRepo + " 21"}) || len(stallCards(store)) != 0 {
		t.Fatalf("first stall tick: reruns %q, stall cards %+v; want run 21 re-run and no card", f.reruns, stallCards(store))
	}
	if head := chainOf(store, demoRepo)[0]; head.status != "stalled" || len(store.alerts) != 1 || store.alerts[0].Kind != AlertChainStalled {
		t.Fatalf("head %+v, alerts %+v; want the stall recorded and alerted", head, store.alerts)
	}
	// within the bound since the re-run: the ladder waits
	tickAt(t, p, clock, 30*time.Second)
	if len(f.reruns) != 1 || len(stallCards(store)) != 0 {
		t.Fatalf("a tick after the re-run: reruns %q, stall cards %+v; want nothing new", f.reruns, stallCards(store))
	}
	// the bound passed again since the re-run, the head still pending
	tickAt(t, p, clock, time.Minute)
	cards := stallCards(store)
	if len(f.reruns) != 1 || len(cards) != 1 {
		t.Fatalf("a bound after the re-run: reruns %q, stall cards %+v; want one card, no second re-run", f.reruns, cards)
	}
	card := cards[0]
	if card.Kind != "human" || card.Status != "waiting-human" || card.Repository != demoRepo {
		t.Fatalf("stall card %+v, want a human card of %s waiting on a person", card, demoRepo)
	}
	q := askedQuestion(t, store, card.ID)
	if !slices.Equal(q.Choices, stallChoices) {
		t.Fatalf("choices %+v, want %+v", q.Choices, stallChoices)
	}
	if card.SHA != demoCommit {
		t.Fatalf("stall card on %q, want the head's commit %s", card.SHA, demoCommit)
	}
	for _, want := range []string{"since 2026-10-20 09:00 UTC", "run 21 running", "https://git.example/" + demoRepo + "/actions/runs/21", "The stall holds #76, #77:"} {
		if !strings.Contains(card.Objective, want) {
			t.Fatalf("the card misses %q:\n%s", want, card.Objective)
		}
	}
	if cards := repairCards(store); len(cards) != 0 {
		t.Fatalf("repair cards %+v, want none for a stall", cards)
	}
	tickAt(t, p, clock, 5*time.Minute)
	if len(f.reruns) != 1 || len(stallCards(store)) != 1 || len(f.issues) != 1 {
		t.Fatalf("a later tick: reruns %q, issues %d; want nothing new", f.reruns, len(f.issues))
	}
}

// A Forgejo that offers no re-run moves the ladder to the card at once,
// and the re-run is asked once only: 15 has no route (404), and on 17 a
// token without the right (403, the infra project's read-only actions
// token) offers none either.
func TestAStallWithNoRerunAsksAtOnce(t *testing.T) {
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	// the cause as Forgejo.do words a 404 and a 403, the fake's body empty
	causes := map[int]string{
		http.StatusNotFound:  "POST /api/v1/repos/sf/demo-app/actions/runs/21/rerun: forgejo: not found",
		http.StatusForbidden: "forgejo POST /api/v1/repos/sf/demo-app/actions/runs/21/rerun: forgejo: refused (forbidden): status 403: ",
	}
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		logs.Reset()
		p, store, f, clock := stallRig(t, "running")
		f.rerun = status
		tickAt(t, p, clock, 0)
		tickAt(t, p, clock, 2*time.Minute)
		if len(f.reruns) != 1 || len(stallCards(store)) != 1 {
			t.Fatalf("%d: reruns %q, stall cards %+v; want the re-run asked and the card at once", status, f.reruns, stallCards(store))
		}
		if want := "re-run at " + demoCommit + ": run 21 of sf/demo-app: forgejo offers no re-run of an action run: " + causes[status] + ": the stall card at once"; !strings.Contains(logs.String(), want) {
			t.Fatalf("%d: logs %q, want %q", status, logs.String(), want)
		}
		tickAt(t, p, clock, 2*time.Minute)
		if len(f.reruns) != 1 || len(stallCards(store)) != 1 {
			t.Fatalf("%d: a later tick: reruns %q, stall cards %+v; want nothing new", status, f.reruns, stallCards(store))
		}
	}
}

// A re-run Forgejo refuses otherwise is logged, and the next bound moves
// the ladder to the card: a failed re-run never holds it.
func TestAFailedRerunAsksABoundLater(t *testing.T) {
	p, store, f, clock := stallRig(t, "running")
	f.rerun = http.StatusBadRequest // a run Forgejo will not re-run now
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	tickAt(t, p, clock, 30*time.Second)
	if len(f.reruns) != 1 || len(stallCards(store)) != 0 {
		t.Fatalf("reruns %q, stall cards %+v; want one refused re-run and no card yet", f.reruns, stallCards(store))
	}
	tickAt(t, p, clock, time.Minute)
	if len(f.reruns) != 1 || len(stallCards(store)) != 1 {
		t.Fatalf("a bound later: reruns %q, stall cards %+v; want the card", f.reruns, stallCards(store))
	}
}

// A stall whose pending part is a required status is its deployer's: no
// re-run, the card at once, naming the status.
func TestAStalledRequiredStatusIsNotRerun(t *testing.T) {
	p, store, f, clock := stallRig(t, "success", "factory/roll")
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	cards := stallCards(store)
	if len(f.reruns) != 0 || len(cards) != 1 {
		t.Fatalf("reruns %q, stall cards %+v; want no re-run and the card at once", f.reruns, cards)
	}
	if !strings.Contains(cards[0].Objective, "required context factory/roll: not reported") {
		t.Fatalf("the card misses the pending status:\n%s", cards[0].Objective)
	}
}

// A skipped run concluded: no re-run moves it. A head it holds pending
// past the bound asks on the card at once, naming the run, and the card's
// re-run again finds nothing to re-run.
func TestAStalledSkippedRunIsNotRerun(t *testing.T) {
	p, store, f, clock := stallRig(t, "skipped")
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	cards := stallCards(store)
	if len(f.reruns) != 0 || len(cards) != 1 {
		t.Fatalf("reruns %q, stall cards %+v; want no re-run and the card at once", f.reruns, cards)
	}
	if !strings.Contains(cards[0].Objective, "run 21 skipped") {
		t.Fatalf("the card misses the skipped run:\n%s", cards[0].Objective)
	}
	if err := p.RerunStall(context.Background(), demoRepo, demoCommit); !errors.Is(err, ErrNothingToRerun) || len(f.reruns) != 0 {
		t.Fatalf("RerunStall on a skipped run = %v, reruns %q; want ErrNothingToRerun and none", err, f.reruns)
	}
}

// A green head resolves the alert and closes the open stall card as the
// bot, its close recorded as factoryd's own first, once.
func TestAGreenHeadClosesTheStallCard(t *testing.T) {
	p, store, f, clock := stallRig(t, "success", "factory/roll")
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	cards := stallCards(store)
	if len(cards) != 1 {
		t.Fatalf("stall cards %+v, want one", cards)
	}
	f.statuses = map[string]map[string][]map[string]string{
		demoRepo: {demoCommit: {{"status": "success", "context": "factory/roll"}}},
	}
	tickAt(t, p, clock, time.Minute)
	issue := fmt.Sprintf("%s#%d", demoRepo, cards[0].IssueNumber)
	if !slices.Equal(f.closed, []string{issue}) || len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], issue+" ") {
		t.Fatalf("closed %q, comments %q; want the card's issue closed with one comment", f.closed, f.comments)
	}
	if !botCloseRecorded(store.intakeEvents, cards[0]) {
		t.Fatalf("intake events %+v, want the close recorded as factoryd's own", store.intakeEvents)
	}
	if store.alerts[0].Verdict != VerdictReal {
		t.Fatalf("alerts %+v, want the stall resolved", store.alerts)
	}
	// the close's webhook not in yet: a second green tick posts nothing new
	tickAt(t, p, clock, time.Minute)
	if len(f.comments) != 1 {
		t.Fatalf("comments %q, want the one", f.comments)
	}
}

// A head that concludes red ends the stall too: the stall card closes,
// and the red files its repair card as built.
func TestARedHeadClosesTheStallCardAndRepairs(t *testing.T) {
	p, store, f, clock := stallRig(t, "running")
	f.rerun = http.StatusNotFound
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	cards := stallCards(store)
	if len(cards) != 1 {
		t.Fatalf("stall cards %+v, want one", cards)
	}
	f.runs[demoRepo] = []map[string]any{chainRun(21, demoCommit, "failure")}
	tickAt(t, p, clock, time.Minute)
	if want := fmt.Sprintf("%s#%d", demoRepo, cards[0].IssueNumber); !slices.Equal(f.closed, []string{want}) {
		t.Fatalf("closed %q, want the stall card's issue %s", f.closed, want)
	}
	if repairs := repairCards(store)[demoRepo]; len(repairs) != 1 || !strings.Contains(repairs[0].Objective, "run 21 failure") {
		t.Fatalf("repair cards %+v, want the red head's", repairs)
	}
}

// The factory's own repository gets its stall card too; a red there
// stays mainfix's, so no repair card files.
func TestTheOwnRepositoryGetsAStallCard(t *testing.T) {
	p, store, _, clock := stallRig(t, "success", "factory/roll")
	p.Own, _ = SplitRepo(demoRepo)
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	if cards := stallCards(store); len(cards) != 1 {
		t.Fatalf("stall cards %+v, want the own repository's", cards)
	}
	if cards := repairCards(store); len(cards) != 0 {
		t.Fatalf("repair cards %+v, want none", cards)
	}
}

// A card closed by "check again" lets the watch run on: the head still
// pending, the next card comes a stall bound after the close.
func TestACheckedStallAsksAgainABoundLater(t *testing.T) {
	p, store, _, clock := stallRig(t, "success", "factory/roll")
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	first := stallCards(store)[0]
	closed := *clock
	first.Status, first.FinishedAt = "done", &closed
	store.tasks[first.ID] = first
	tickAt(t, p, clock, 30*time.Second)
	if n := len(stallCards(store)); n != 1 {
		t.Fatalf("%d stall cards within the bound after the check, want the one", n)
	}
	tickAt(t, p, clock, time.Minute)
	if n := len(stallCards(store)); n != 2 {
		t.Fatalf("%d stall cards a bound after the check, want a second", n)
	}
}

// "Dismiss" holds the ladder until the head changes: no card, no alert,
// across a restart too; the next head climbs afresh.
func TestADismissedStallAsksNothingUntilTheHeadChanges(t *testing.T) {
	p, store, f, clock := stallRig(t, "running")
	f.rerun = http.StatusNotFound
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	first := stallCards(store)[0]
	if err := store.DismissChainStall(context.Background(), demoRepo, first.SHA, "sebastian"); err != nil {
		t.Fatal(err)
	}
	closed := *clock
	first.Status, first.FinishedAt = "done", &closed
	store.tasks[first.ID] = first
	tickAt(t, p, clock, 5*time.Minute)
	restarted := &ChainPoller{Forgejo: p.Forgejo, Store: store, Projects: store, Policy: p.Policy, Workflows: p.Workflows, Now: p.Now}
	tickAt(t, restarted, clock, 5*time.Minute)
	open := 0
	for _, a := range store.alerts {
		if a.Verdict == "" {
			open++
		}
	}
	if len(stallCards(store)) != 1 || open != 0 || len(f.reruns) != 1 {
		t.Fatalf("after the dismissal: stall cards %+v, alerts %+v, reruns %q; want nothing new", stallCards(store), store.alerts, f.reruns)
	}
	// a new head, pending past its bound, starts its own ladder
	f.runs[demoRepo] = []map[string]any{chainRun(22, demoNext, "running"), chainRun(21, demoCommit, "running")}
	tickAt(t, restarted, clock, 0)
	tickAt(t, restarted, clock, 2*time.Minute)
	if len(f.reruns) != 2 || f.reruns[1] != demoRepo+" 22" || len(stallCards(store)) != 2 {
		t.Fatalf("the new head: reruns %q, stall cards %d; want run 22 re-run and its card", f.reruns, len(stallCards(store)))
	}
}

// "Re-run again" re-runs the pending runs of the repository's head and
// stamps the head row; a head whose pending part is a required status
// has nothing to re-run, and a Forgejo without the API says so.
func TestRerunStallRerunsTheHeadsPendingRun(t *testing.T) {
	ctx := context.Background()
	p, store, f, clock := stallRig(t, "running")
	tickAt(t, p, clock, 0)
	if err := p.RerunStall(ctx, demoRepo, demoCommit); err != nil {
		t.Fatal(err)
	}
	if l, _ := store.ChainLadder(ctx, demoRepo, demoCommit); !slices.Equal(f.reruns, []string{demoRepo + " 21"}) || !l.RerunAt.Equal(*clock) {
		t.Fatalf("reruns %q, ladder %+v; want run 21 re-run and stamped", f.reruns, l)
	}
	f.rerun = http.StatusNotFound
	if err := p.RerunStall(ctx, demoRepo, demoCommit); !errors.Is(err, ErrNoRerun) {
		t.Fatalf("RerunStall on a 404 = %v, want ErrNoRerun", err)
	}
	// the head moved on: the card's commit stalls no more
	f.runs[demoRepo] = []map[string]any{chainRun(22, demoNext, "running"), chainRun(21, demoCommit, "running")}
	var gone ErrStallGone
	if err := p.RerunStall(ctx, demoRepo, demoCommit); !errors.As(err, &gone) ||
		err.Error() != "the head moved on to e5e5e5e; this stall no longer stands" {
		t.Fatalf("RerunStall of a superseded commit = %v, want the head moved on", err)
	}
	// the head concluded: green or red, nothing stands to re-run
	f.runs[demoRepo] = []map[string]any{chainRun(21, demoCommit, "success")}
	if err := p.RerunStall(ctx, demoRepo, demoCommit); err == nil || err.Error() != "the head concluded green; this stall no longer stands" {
		t.Fatalf("RerunStall of a concluded head = %v, want the head concluded green", err)
	}
	if len(f.reruns) != 2 {
		t.Fatalf("reruns %q, want none past the 404", f.reruns)
	}
	p, _, _, clock = stallRig(t, "success", "factory/roll")
	tickAt(t, p, clock, 0)
	if err := p.RerunStall(ctx, demoRepo, demoCommit); !errors.Is(err, ErrNothingToRerun) {
		t.Fatalf("RerunStall on a pending required status = %v, want ErrNothingToRerun", err)
	}
}

// A stall card outlives no head: a newer commit taking the head closes
// it, an answer on it refuses with the head it moved on to, and the new
// head's own stall still alerts and asks.
func TestASupersededHeadClosesTheStallCard(t *testing.T) {
	ctx := context.Background()
	p, store, f, clock := stallRig(t, "success", "factory/roll")
	tickAt(t, p, clock, 0)
	tickAt(t, p, clock, 2*time.Minute)
	first := stallCards(store)[0]
	if first.SHA != demoCommit {
		t.Fatalf("stall card on %q, want %s", first.SHA, demoCommit)
	}
	f.runs[demoRepo] = []map[string]any{chainRun(22, demoNext, "success"), chainRun(21, demoCommit, "success")}
	tickAt(t, p, clock, 15*time.Second)
	issue := fmt.Sprintf("%s#%d", demoRepo, first.IssueNumber)
	if !slices.Equal(f.closed, []string{issue}) || len(f.comments) != 1 || !strings.Contains(f.comments[0], "moved on to e5e5e5e") {
		t.Fatalf("closed %q, comments %q; want A's card closed naming the new head", f.closed, f.comments)
	}
	var gone ErrStallGone
	if err := store.DismissChainStall(ctx, demoRepo, first.SHA, "sebastian"); !errors.As(err, &gone) || gone.Head != demoNext {
		t.Fatalf("a dismissal on A's card = %v, want the head moved on to %s", err, demoNext)
	}
	// the close's webhook lands; B stays pending past its bound
	closed := *clock
	first.Status, first.FinishedAt = "done", &closed
	store.tasks[first.ID] = first
	tickAt(t, p, clock, 2*time.Minute)
	cards := stallCards(store)
	if len(cards) != 2 || cards[1].SHA != demoNext {
		t.Fatalf("stall cards %+v, want B's own", cards)
	}
	open := 0
	for _, a := range store.alerts {
		if a.Kind == AlertChainStalled && a.Verdict == "" {
			open++
		}
	}
	if head := chainOf(store, demoRepo)[0]; open != 1 || head.dismissed || head.status != "stalled" {
		t.Fatalf("alerts %+v, head %+v; want B's stall alerted and recorded, undismissed", store.alerts, head)
	}
}

// The head row keeps its stall ladder while its commit stays: the re-run
// stamp and the dismissal, which resolves the repository's open
// chain_stalled row noise by the member and leaves chain_red and other
// repositories' rows open; a new commit taking the row clears both. A
// dismissal of a commit the head moved on from, or of a concluded head,
// refuses and touches nothing. ChainCard reads a repository's newest card
// of a source. Requires a disposable Postgres
// (FACTORYD_TEST_DATABASE_URL).
func TestChainLadderRows(t *testing.T) {
	ctx := context.Background()
	pg, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer pg.db.Close()
	if _, err := pg.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs, alerts CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	ladder := func(sha string) ChainLadder {
		t.Helper()
		l, err := pg.ChainLadder(ctx, demoRepo, sha)
		if err != nil {
			t.Fatalf("ChainLadder: %v", err)
		}
		return l
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pg.RecordChainHead(ctx, demoRepo, demoCommit, 21, "pending"))
	if l := ladder(demoCommit); !l.RerunAt.IsZero() || l.Dismissed {
		t.Fatalf("a fresh head's ladder = %+v, want none", l)
	}
	must(pg.MarkChainRerun(ctx, demoRepo, demoNext)) // no row of that commit: nothing moves
	if l := ladder(demoCommit); !l.RerunAt.IsZero() {
		t.Fatalf("ladder %+v after another commit's re-run, want none", l)
	}
	must(pg.MarkChainRerun(ctx, demoRepo, demoCommit))
	if l := ladder(demoCommit); l.RerunAt.IsZero() || time.Since(l.RerunAt) > time.Minute {
		t.Fatalf("ladder %+v after the re-run, want it stamped now", l)
	}
	for _, a := range []Alert{
		{Kind: AlertChainStalled, Source: demoRepo, Fingerprint: demoRepo},
		{Kind: AlertChainRed, Source: demoRepo, Fingerprint: demoRepo},
		{Kind: AlertChainStalled, Source: webRepo, Fingerprint: webRepo},
	} {
		if _, err := pg.InsertAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	must(pg.DismissChainStall(ctx, demoRepo, demoCommit, "sebastian"))
	if l := ladder(demoCommit); !l.Dismissed || l.RerunAt.IsZero() {
		t.Fatalf("ladder %+v after the dismissal, want dismissed, the re-run kept", l)
	}
	alerts, err := pg.Alerts(ctx, 10)
	must(err)
	got := map[string]string{}
	for _, a := range alerts {
		got[a.Kind+" "+a.Fingerprint] = a.Verdict + " " + a.ResolvedBy
	}
	if want := map[string]string{
		AlertChainStalled + " " + demoRepo: VerdictNoise + " sebastian",
		AlertChainRed + " " + demoRepo:     " ",
		AlertChainStalled + " " + webRepo:  " ",
	}; !maps.Equal(got, want) {
		t.Fatalf("alerts %v, want %v", got, want)
	}
	// a newer run on the same commit keeps the ladder, a new commit clears it
	must(pg.RecordChainHead(ctx, demoRepo, demoCommit, 22, "pending"))
	if l := ladder(demoCommit); !l.Dismissed || l.RerunAt.IsZero() {
		t.Fatalf("ladder %+v after the commit's newer run, want it kept", l)
	}
	must(pg.RecordChainHead(ctx, demoRepo, demoNext, 23, "pending"))
	if l := ladder(demoNext); l.Dismissed || !l.RerunAt.IsZero() {
		t.Fatalf("ladder %+v of the new commit, want none", l)
	}
	if _, err := pg.InsertAlert(ctx, Alert{Kind: AlertChainStalled, Source: demoRepo, Fingerprint: demoRepo}); err != nil {
		t.Fatal(err)
	}
	openStalls := func() int {
		t.Helper()
		var n int
		must(pg.db.QueryRowContext(ctx, `SELECT count(*) FROM alerts WHERE kind = $1 AND fingerprint = $2 AND verdict IS NULL`,
			AlertChainStalled, demoRepo).Scan(&n))
		return n
	}
	if err := pg.DismissChainStall(ctx, demoRepo, demoCommit, "sebastian"); err == nil ||
		err.Error() != "the head moved on to e5e5e5e; this stall no longer stands" {
		t.Fatalf("a dismissal of the old commit = %v, want the head moved on", err)
	}
	if l := ladder(demoNext); l.Dismissed || openStalls() != 1 {
		t.Fatalf("ladder %+v, %d open stalls after the refused dismissal; want both untouched", l, openStalls())
	}
	must(pg.RecordChainHead(ctx, demoRepo, demoNext, 23, "success"))
	if err := pg.DismissChainStall(ctx, demoRepo, demoNext, "sebastian"); err == nil ||
		err.Error() != "the head concluded green; this stall no longer stands" {
		t.Fatalf("a dismissal of a concluded head = %v, want the head concluded green", err)
	}
	if l := ladder(demoNext); l.Dismissed || openStalls() != 1 {
		t.Fatalf("ladder %+v, %d open stalls after the refused dismissal; want both untouched", l, openStalls())
	}

	old := time.Now().Add(-time.Hour)
	for _, c := range []Task{
		{ID: "t-old", Kind: "human", Status: "done", FinishedAt: &old, CreatedAt: old, Repository: demoRepo, Source: SourceChainStall, IssueNumber: 5},
		{ID: "t-new", Kind: "human", Status: "waiting-human", Repository: demoRepo, Source: SourceChainStall, IssueNumber: 6},
		{ID: "t-repair", Kind: "agent", Status: "queued", Repository: demoRepo, Source: SourceChainRepair, IssueNumber: 7, BudgetEUR: 1},
		{ID: "t-web", Kind: "human", Status: "waiting-human", Repository: webRepo, Source: SourceChainStall, IssueNumber: 8},
	} {
		must(pg.CreateTask(ctx, c))
	}
	for source, want := range map[string]string{SourceChainStall: "t-new", SourceChainRepair: "t-repair"} {
		if c, ok, err := pg.ChainCard(ctx, demoRepo, source); err != nil || !ok || c.ID != want {
			t.Fatalf("ChainCard %s = %s, %v, %v; want %s", source, c.ID, ok, err, want)
		}
	}
	if c, ok, err := pg.ChainCard(ctx, codeRepo, SourceChainStall); err != nil || ok {
		t.Fatalf("ChainCard of a repository without one = %s, %v, %v; want none", c.ID, ok, err)
	}
}

// The stall holds every merged card no green chain covers, whichever
// commit carried it: a card carried by a red run, one with no carry row
// yet, not one a green run covers, not a done card. Requires a disposable
// Postgres (FACTORYD_TEST_DATABASE_URL).
func TestChainHeldIssues(t *testing.T) {
	ctx := context.Background()
	pg, err := OpenStore(ctx, schematest.DSN(t, "factoryd"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer pg.db.Close()
	if _, err := pg.db.ExecContext(ctx, `TRUNCATE tasks, sessions, intake_events, chain_runs, alerts CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	now := time.Now()
	for _, c := range []Task{
		{ID: "t-green", PRNumber: 1, IssueNumber: 11, Status: "merged"},
		{ID: "t-red", PRNumber: 2, IssueNumber: 12, Status: "merged"},
		{ID: "t-head", PRNumber: 3, IssueNumber: 13, Status: "merged"},
		{ID: "t-none", PRNumber: 4, IssueNumber: 14, Status: "merged"},
		{ID: "t-done", PRNumber: 5, IssueNumber: 15, Status: "done"},
		{ID: "t-web", PRNumber: 6, IssueNumber: 16, Status: "merged", Repository: webRepo},
	} {
		c.Kind, c.BudgetEUR, c.FinishedAt, c.Repository = "agent", 1, &now, cmp.Or(c.Repository, demoRepo)
		if err := pg.CreateTask(ctx, c); err != nil {
			t.Fatalf("CreateTask %s: %v", c.ID, err)
		}
	}
	for _, row := range []struct {
		pr     int64
		sha    string
		run    int64
		status string
	}{{1, "a1", 10, "success"}, {2, "b2", 11, "failure"}, {3, "c3", 12, "stalled"}, {5, "a1", 10, "success"}} {
		if err := pg.RecordChainRun(ctx, demoRepo, row.pr, row.sha, row.run, row.status); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := pg.ChainHeldIssues(ctx, demoRepo); err != nil || !slices.Equal(got, []int64{12, 13, 14}) {
		t.Fatalf("ChainHeldIssues = %v, %v; want #12, #13, #14", got, err)
	}
}

// noAgents is repository's seed project on agents none: no process and
// no lanes, as the tree holds the infra project.
func noAgents(repository string) Project {
	p := seedProject(repository)
	p.Agents, p.Process, p.Lanes = policy.AgentsNone, "", nil
	return p
}

// redRig is demo's chain whose head run 21 is green and whose required
// context flux/platform reports state on it, Flux's verdict on the
// commit, for a project proj: the poller, its store, its Forgejo.
func redRig(t *testing.T, proj Project, state string) (*ChainPoller, *fakeStore, *chainForgejo) {
	t.Helper()
	f := &chainForgejo{
		runs:     map[string][]map[string]any{demoRepo: {chainRun(21, demoCommit, "success")}},
		statuses: map[string]map[string][]map[string]string{demoRepo: {demoCommit: {{"status": state, "context": "flux/platform"}}}},
	}
	p, store := newTestPoller(t, f, proj)
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"demo": {Repository: demoRepo, Chain: policy.ChainConfig{Requires: []string{"flux/platform"}}},
		}}}
	}
	p.Workflows = &fakeWorkflows{}
	return p, store, f
}

// A red head of a project without agents (the infra project, spec 6.7)
// files its repair card in the human lane, labelled human, starting no
// workflow: one per repository while it stands, ChainCard finds it, and
// a second red tick files none. The card asks its question: the ask names
// the red head, the info the run's link and the carried cards, and its
// one question_asked row is the needs-you poke at the messengers' level.
func TestARedHeadWithoutAgentsFilesAHumanRepairCard(t *testing.T) {
	ctx := context.Background()
	p, store, f := redRig(t, noAgents(demoRepo), "failure")
	rows := &fakeEventRecorder{}
	p.Timeline = &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: rows}
	for range 2 {
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cards := repairCards(store)[demoRepo]
	if len(cards) != 1 {
		t.Fatalf("repair cards %+v, want one", cards)
	}
	card := cards[0]
	if card.Kind != "human" || card.Status != "waiting-human" || card.SHA != demoCommit ||
		!strings.Contains(card.Objective, "required context flux/platform: failure") {
		t.Fatalf("repair card %+v, want a human card on the red head naming Flux", card)
	}
	if got, ok, err := store.ChainCard(ctx, demoRepo, SourceChainRepair); err != nil || !ok || got.ID != card.ID {
		t.Fatalf("ChainCard = %s, %v, %v; want %s", got.ID, ok, err, card.ID)
	}
	if want := fmt.Sprintf("%s#%d human state/waiting-human", demoRepo, card.IssueNumber); !slices.Equal(f.labelled, []string{want}) {
		t.Fatalf("labels %q, want %q", f.labelled, want)
	}
	if started := p.Workflows.(*fakeWorkflows).started; len(started) != 0 {
		t.Fatalf("started %v, want no workflow for a human card", started)
	}
	ask := "chain of sf/demo-app at 9d9d9d9: required context flux/platform: failure. Fix it from main's head and push."
	q := askedQuestion(t, store, card.ID)
	if q.Ask != ask || !strings.HasPrefix(card.Objective, ask+"\n\n") ||
		!strings.HasPrefix(q.Info, "The run: /sf/demo-app/actions/runs/21\n\nNo card's change rides this head") ||
		!strings.Contains(q.Info, "Closing it yourself dismisses the red at 9d9d9d9") ||
		!slices.Equal(q.Choices, []QuestionChoice{{AnswerClose, "Handled, close"}}) {
		t.Fatalf("question %+v, objective %q; want the red head's ask, the run's link and the dismissal", q, card.Objective)
	}
	var asked []Event
	for _, e := range rows.rows {
		if e.Kind == EventQuestionAsked {
			asked = append(asked, e)
		}
	}
	if len(asked) != 1 || asked[0].TaskID != card.ID || asked[0].Via != ViaFactoryd || asked[0].Detail["ask"] != ask || asked[0].Detail["source"] != SourceChainRepair {
		t.Fatalf("question_asked rows %+v, want the card's one", asked)
	}
	if m, ok := pokeOf(asked[0], card); !ok || m.Kind != MessageEscalation || !strings.HasPrefix(m.Content, fmt.Sprintf("#%d needs you: %s", card.IssueNumber, ask)) ||
		deliveryLevel(m) != LevelNeedsYou {
		t.Fatalf("the poke = %+v, %v; want the card's needs-you at the messengers' level", m, ok)
	}
}

// The human repair card names the cards whose change the red head
// carries: merged, they wait on the chain, and this card is their repair.
func TestTheHumanRepairCardNamesTheCarriedCards(t *testing.T) {
	p, store, f := redRig(t, noAgents(demoRepo), "failure")
	f.carries = map[string]map[string][]map[string]any{
		demoRepo: {"parent-of-" + demoCommit + "..." + demoCommit: {{"sha": demoMerge}}},
	}
	store.tasks["t-x"] = Task{ID: "t-x", Repository: demoRepo, PRNumber: 3, IssueNumber: 77, Status: "merged", FinishedAt: justNow(), MergeCommit: demoMerge}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	cards := repairCards(store)[demoRepo]
	if len(cards) != 1 || cards[0].Kind != "human" {
		t.Fatalf("repair cards %+v, want one human card", cards)
	}
	if q := askedQuestion(t, store, cards[0].ID); !strings.Contains(q.Info, "The head carries #77: merged, they wait for a green chain to cover them; this card is their repair.") {
		t.Fatalf("info %q, want the carried card #77 named", q.Info)
	}
}

// A human repair card closes once the red it was filed for is gone: its
// own commit turned green (Flux reconciled the same revision and healed),
// its close recorded as factoryd's own first, one comment; a second green
// tick posts none.
func TestAHealedHeadClosesTheRepairCard(t *testing.T) {
	ctx := context.Background()
	p, store, f := redRig(t, noAgents(demoRepo), "failure")
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	card := repairCards(store)[demoRepo][0]
	f.statuses[demoRepo][demoCommit] = []map[string]string{{"status": "success", "context": "flux/platform"}}
	for range 2 {
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	issue := fmt.Sprintf("%s#%d", demoRepo, card.IssueNumber)
	if !slices.Equal(f.closed, []string{issue, issue}) || len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], issue+" ") ||
		!strings.Contains(f.comments[0], "concluded green") {
		t.Fatalf("closed %q, comments %q; want the card's issue closed with one comment", f.closed, f.comments)
	}
	if !botCloseRecorded(store.intakeEvents, card) {
		t.Fatalf("intake events %+v, want the close recorded as factoryd's own", store.intakeEvents)
	}
}

// A repair card and a head that moves on: a human card closes, the red
// it names no longer the head's; an agent's keeps its work while the new
// head is pending and closes once a head is green; a merged one is the
// reconciler's and stays.
func TestARepairCardSettlesWhenTheHeadMovesOn(t *testing.T) {
	ctx := context.Background()
	moveOn := func(f *chainForgejo, state string) {
		f.runs[demoRepo] = []map[string]any{chainRun(22, demoNext, "success"), chainRun(21, demoCommit, "success")}
		f.statuses[demoRepo][demoNext] = []map[string]string{{"status": state, "context": "flux/platform"}}
	}
	tick := func(p *ChainPoller) {
		t.Helper()
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}

	p, store, f := redRig(t, noAgents(demoRepo), "failure")
	tick(p)
	human := repairCards(store)[demoRepo][0]
	moveOn(f, "pending")
	tick(p)
	if want := fmt.Sprintf("%s#%d", demoRepo, human.IssueNumber); !slices.Equal(f.closed, []string{want}) ||
		len(f.comments) != 1 || !strings.Contains(f.comments[0], "moved on to e5e5e5e") {
		t.Fatalf("human card: closed %q, comments %q; want it closed naming the new head", f.closed, f.comments)
	}

	p, store, f = redRig(t, seedProject(demoRepo), "failure")
	tick(p)
	agent := repairCards(store)[demoRepo][0]
	if agent.Kind != "agent" {
		t.Fatalf("repair card %+v, want an agent's", agent)
	}
	moveOn(f, "pending")
	tick(p)
	if len(f.closed) != 0 {
		t.Fatalf("closed %q while the new head is pending, want the agent's card kept", f.closed)
	}
	merged := store.tasks[agent.ID]
	merged.Status, merged.FinishedAt = "merged", justNow()
	store.tasks[agent.ID] = merged
	f.statuses[demoRepo][demoNext] = []map[string]string{{"status": "success", "context": "flux/platform"}}
	p.End = func(context.Context, string, int64) error { return nil }
	tick(p)
	if len(f.closed) != 0 {
		t.Fatalf("closed %q, want the merged card left to the reconciler", f.closed)
	}
	queued := store.tasks[agent.ID]
	queued.Status, queued.FinishedAt = "queued", nil
	store.tasks[agent.ID] = queued
	tick(p)
	if want := fmt.Sprintf("%s#%d", demoRepo, agent.IssueNumber); !slices.Equal(f.closed, []string{want}) {
		t.Fatalf("closed %q on green, want the agent's card", f.closed)
	}
}

// A repair card whose filing left its issue behind, the task row never
// written: the next red tick takes the bot's open issue of the card's
// title on instead of filing a second, its labels the card's; an issue of
// that title a task holds, or another poster's, is not taken.
func TestARepairCardTakesOnTheIssueAFailedFilingLeft(t *testing.T) {
	ctx := context.Background()
	p, store, f := redRig(t, noAgents(demoRepo), "failure")
	title := "chain of sf/demo-app at 9d9d9d9: required context flux/platform: failure. Fix it from main's head and push."
	f.open = map[string][]map[string]any{demoRepo: {
		{"number": 7, "title": title, "user": map[string]any{"login": "someone"}},
		{"number": 8, "title": title, "user": map[string]any{"login": "factory-agent"}},
		{"number": 9, "title": title, "user": map[string]any{"login": "factory-agent"}},
	}}
	store.tasks["t-held"] = Task{ID: "t-held", Kind: "human", Status: "waiting-human", Repository: demoRepo, IssueNumber: 9, Source: SourceAlert}
	if err := p.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	cards := repairCards(store)[demoRepo]
	if len(cards) != 1 || cards[0].IssueNumber != 8 || cards[0].Status != "waiting-human" || cards[0].Kind != "human" || len(f.issues) != 0 {
		t.Fatalf("repair cards %+v, issues filed %q; want the card on the left-behind #8 and no new issue", cards, f.issues)
	}
	if want := []string{demoRepo + "#8 human state/waiting-human"}; !slices.Equal(f.labelled, want) {
		t.Fatalf("labels %q, want %q", f.labelled, want)
	}
}

// A human repair card a person closes while its head stays red stands as
// that head's dismissal: the next red ticks file no card and poke
// nobody, the chain_red alert stays open; a new red head files a new
// card. factoryd's own close, on its head's green, dismisses nothing:
// the same commit red again files a card.
func TestAPersonsCloseDismissesTheRedHead(t *testing.T) {
	ctx := context.Background()
	tick := func(p *ChainPoller) {
		t.Helper()
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	asked := func(rows *fakeEventRecorder) int {
		n := 0
		for _, e := range rows.rows {
			if e.Kind == EventQuestionAsked {
				n++
			}
		}
		return n
	}
	end := func(store *fakeStore, card Task) {
		done := store.tasks[card.ID]
		done.Status, done.FinishedAt = "done", justNow()
		store.tasks[card.ID] = done
	}

	p, store, f := redRig(t, noAgents(demoRepo), "failure")
	rows := &fakeEventRecorder{}
	p.Timeline = &Timeline{Objects: &fakeObjectStore{}, Store: store, Events: rows}
	tick(p)
	card := repairCards(store)[demoRepo][0]
	end(store, card)
	tick(p)
	tick(p)
	if cards := repairCards(store)[demoRepo]; len(cards) != 1 || asked(rows) != 1 {
		t.Fatalf("repair cards %+v, %d needs-you rows after a person closed the card; want the one card and its one row", cards, asked(rows))
	}
	open := 0
	for _, a := range store.alerts {
		if a.Kind == AlertChainRed && a.Fingerprint == demoRepo && a.Verdict == "" {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("alerts %+v, want the chain_red alert open while the head is red", store.alerts)
	}
	f.runs[demoRepo] = []map[string]any{chainRun(22, demoNext, "success"), chainRun(21, demoCommit, "success")}
	f.statuses[demoRepo][demoNext] = []map[string]string{{"status": "failure", "context": "flux/platform"}}
	tick(p)
	cards := repairCards(store)[demoRepo]
	if len(cards) != 2 || asked(rows) != 2 || !slices.ContainsFunc(cards, func(c Task) bool { return c.SHA == demoNext && c.Status == "waiting-human" }) {
		t.Fatalf("repair cards %+v, %d needs-you rows on a new red head; want a second card on %s and its row", cards, asked(rows), demoNext)
	}

	p, store, f = redRig(t, noAgents(demoRepo), "failure")
	tick(p)
	card = repairCards(store)[demoRepo][0]
	f.statuses[demoRepo][demoCommit] = []map[string]string{{"status": "success", "context": "flux/platform"}}
	tick(p)
	if !botCloseRecorded(store.intakeEvents, card) {
		t.Fatalf("intake events %+v, want factoryd's close of the healed card", store.intakeEvents)
	}
	end(store, card)
	f.statuses[demoRepo][demoCommit] = []map[string]string{{"status": "failure", "context": "flux/platform"}}
	tick(p)
	if cards := repairCards(store)[demoRepo]; len(cards) != 2 {
		t.Fatalf("repair cards %+v, want a new card on the same commit red again after factoryd's own close", cards)
	}
}

// Only a repair card that can still act holds the one-per-repository
// slot: an agent's merged repair card holds while the red head is the
// commit it was filed for, its fix not yet main's head; the head red on
// its merge commit files a new card.
func TestAMergedRepairCardHoldsNoSlot(t *testing.T) {
	ctx := context.Background()
	tick := func(p *ChainPoller) {
		t.Helper()
		if err := p.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p, store, f := redRig(t, seedProject(demoRepo), "failure")
	tick(p)
	agent := repairCards(store)[demoRepo][0]
	merged := store.tasks[agent.ID]
	merged.Status, merged.MergeCommit = "merged", demoNext
	store.tasks[agent.ID] = merged
	tick(p)
	if cards := repairCards(store)[demoRepo]; len(cards) != 1 {
		t.Fatalf("repair cards %+v while the head is still the card's red commit, want the merged card alone", cards)
	}
	f.runs[demoRepo] = []map[string]any{chainRun(22, demoNext, "success"), chainRun(21, demoCommit, "success")}
	f.statuses[demoRepo][demoNext] = []map[string]string{{"status": "failure", "context": "flux/platform"}}
	tick(p)
	cards := repairCards(store)[demoRepo]
	if len(cards) != 2 || !slices.ContainsFunc(cards, func(c Task) bool { return c.SHA == demoNext && c.Kind == "agent" && c.Status == "queued" }) {
		t.Fatalf("repair cards %+v, want a new agent card on the merge commit %s red", cards, demoNext)
	}
	if started := p.Workflows.(*fakeWorkflows).started; len(started) != 2 {
		t.Fatalf("started %v, want the new card's workflow started too", started)
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
	store.tasks["t-7"] = Task{ID: "t-7", Repository: codeRepo, PRNumber: 7, Status: "merged", FinishedAt: justNow(), MergeCommit: mergePR12}
	var ended []string
	p.End = func(ctx context.Context, taskID string, run int64) error {
		ended = append(ended, fmt.Sprintf("%s run %d", taskID, run))
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
	if !slices.Equal(ended, []string{"t-7 run 11"}) {
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
	p.End = func(ctx context.Context, taskID string, run int64) error { return nil }
	store.tasks["t-late"] = Task{ID: "t-late", Repository: codeRepo, PRNumber: 12, Status: "merged", FinishedAt: justNow()}
	store.tasks["t-never"] = Task{ID: "t-never", Repository: codeRepo, PRNumber: 13, Status: "merged", FinishedAt: justNow(), MergeCommit: "0c0c"}
	store.tasks["t-held"] = Task{ID: "t-held", Repository: codeRepo, PRNumber: 14, Status: "merged", FinishedAt: justNow(), MergeCommit: mergePR14}
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
	p.End = func(ctx context.Context, taskID string, run int64) error {
		ended = append(ended, taskID)
		return nil
	}
	store.tasks["t-late"] = Task{ID: "t-late", Repository: codeRepo, PRNumber: 12, Status: "merged", FinishedAt: justNow(), MergeCommit: mergePR12}
	store.tasks["t-cov"] = Task{ID: "t-cov", Repository: codeRepo, PRNumber: 20, Status: "merged", FinishedAt: justNow(), MergeCommit: "0c0c"}
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
	p.End = func(ctx context.Context, taskID string, run int64) error { return nil }
	store.tasks["t-a"] = Task{ID: "t-a", Repository: codeRepo, PRNumber: 20, Status: "merged", FinishedAt: justNow(), MergeCommit: "b0b0"}
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
	p.End = func(context.Context, string, int64) error { return nil }
	store.chainNow = p.Now
	merged := clock
	store.tasks["t-served"] = Task{ID: "t-served", Repository: codeRepo, PRNumber: 12, Status: "in-review", MergeCommit: "f00d"}
	store.tasks["t-unserved"] = Task{ID: "t-unserved", Repository: demoRepo, PRNumber: 3, Status: "in-review"}
	for _, id := range []string{"t-served", "t-unserved"} {
		if err := store.TransitionTask(context.Background(), id, []string{"in-review"}, "merged"); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(10 * time.Minute)
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
	reads := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(f.reads), func(r string) bool { return !strings.HasPrefix(r, demoRepo+" ") }))
	}
	served := reads()
	f.mu.Lock()
	f.runs[demoRepo] = []map[string]any{pushRun(22, demoCommit, "success"), pushRun(21, demoCommit, "success")}
	f.mu.Unlock()
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, demoRepo); len(got) != 1 || got[0].run != 22 {
		t.Fatalf("chain %+v, want the served repository to go on recording", got)
	}
	if n := reads(); n != served {
		t.Fatalf("%d workflow-list reads after a served tick, want the %d it had: a served repository is never read again", n, served)
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

// A project file whose chain block cannot run leaves its repository
// unrecorded, but a card merged before the file broke still alerts
// after the default stall bound.
func TestAnUncarriedCardAlertsInARepositoryWhoseChainBlockIsBroken(t *testing.T) {
	f := &chainForgejo{runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}}}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Policy = func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: codeRepo, Chain: policy.ChainConfig{Mode: "sometimes"}},
		}}}
	}
	clock := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	store.chainNow = p.Now
	store.tasks["t-x"] = Task{ID: "t-x", Repository: codeRepo, PRNumber: 12, Status: "in-review"}
	if err := store.TransitionTask(context.Background(), "t-x", []string{"in-review"}, "merged"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(policy.DefaultChainStall + time.Minute)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := chainOf(store, codeRepo); len(got) != 0 {
		t.Fatalf("chain %+v, want the broken repository unrecorded", got)
	}
	if len(store.alerts) != 1 || store.alerts[0].Kind != AlertChainUncarried || store.alerts[0].Fingerprint != codeRepo+" t-x" {
		t.Fatalf("alerts %+v, want one chain_uncarried for t-x", store.alerts)
	}
}

func factoryChain(statuses ...map[string]string) (*chainForgejo, func() policy.Version) {
	f := &chainForgejo{
		runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}},
		carries: map[string]map[string][]map[string]any{
			codeRepo: {"parent-of-" + commitC2 + "..." + commitC2: {{"sha": mergePR12}, {"sha": mergePR14}}},
		},
		merged: map[string][]map[string]any{
			codeRepo: {{"number": 12, "merged": true, "merge_commit_sha": mergePR12}, {"number": 14, "merged": true, "merge_commit_sha": mergePR14}},
		},
		statuses: map[string]map[string][]map[string]string{codeRepo: {commitC2: statuses}},
	}
	return f, func() policy.Version {
		return policy.Version{Tree: policy.Tree{Projects: map[string]policy.ProjectConfig{
			"code": {Repository: codeRepo, Chain: policy.ChainConfig{Requires: []string{"factory/roll"}, Stall: 24 * time.Hour}},
		}}}
	}
}

// The three image jobs are the run; with the roll still pending the
// head, and every carry row, stay pending, and the 24 hour bound calls no
// stall before it passes.
func TestAGreenRunWithAPendingRequiredStatusStaysPending(t *testing.T) {
	f, pol := factoryChain(map[string]string{"status": "pending", "context": "factory/roll"})
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Policy = pol
	p.Own, _ = SplitRepo(codeRepo)
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return clock }
	ended := 0
	p.End = func(context.Context, string, int64) error { ended++; return nil }
	var logs bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logs)
	for _, at := range []time.Duration{0, 23 * time.Hour} {
		clock = clock.Add(at)
		if err := p.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, r := range chainOf(store, codeRepo) {
			if r.status != "pending" {
				t.Fatalf("at +%s row %+v, want every row pending", at, r)
			}
		}
	}
	if ended != 0 || strings.Contains(logs.String(), "stalled") {
		t.Fatalf("ended %d, log %q: a roll within 24h is neither live nor stalled", ended, logs.String())
	}
	clock = clock.Add(2 * time.Hour)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "stalled") {
		t.Fatalf("no stall 25h in: %q", logs.String())
	}
}

// The settle's success on the commit turns every row of it green on one
// tick, and every merged card it carried ends then.
func TestTheSettledStatusEndsTheCarriedCards(t *testing.T) {
	f, pol := factoryChain(map[string]string{"status": "pending", "context": "factory/roll"})
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	p.Policy = pol
	store.tasks["t-12"] = Task{ID: "t-12", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 12, MergeCommit: mergePR12}
	store.tasks["t-14"] = Task{ID: "t-14", Status: "merged", FinishedAt: justNow(), Repository: codeRepo, PRNumber: 14, MergeCommit: mergePR14}
	var ended []string
	p.End = func(_ context.Context, id string, run int64) error {
		ended = append(ended, fmt.Sprintf("%s run %d", id, run))
		return nil
	}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 0 {
		t.Fatalf("ended %q while the roll is pending", ended)
	}
	f.statuses[codeRepo][commitC2] = []map[string]string{{"status": "success", "context": "factory/roll"}}
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range chainOf(store, codeRepo) {
		if r.status != "success" {
			t.Fatalf("row %+v, want success", r)
		}
	}
	slices.Sort(ended)
	if want := []string{"t-12 run 61", "t-14 run 61"}; !slices.Equal(ended, want) {
		t.Fatalf("ended %q, want %q", ended, want)
	}
}

func (f *chainForgejo) readsOf(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.runReads {
		if r == repo {
			n++
		}
	}
	return n
}

func waitReads(t *testing.T, f *chainForgejo, repo string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.readsOf(repo) < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d run reads, want %d", repo, f.readsOf(repo), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A nudge ticks the one repository it names at once, whatever the poll
// interval; a repository no project names is ignored; nudges that pile
// up while one is pending drop, and never block the sender.
func TestANudgeTicksOneRepositoryNow(t *testing.T) {
	f := &chainForgejo{runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}, demoRepo: {pushRun(21, demoCommit, "success")}}}
	p, _ := newTestPoller(t, f, seedProject(codeRepo), seedProject(demoRepo))
	p.Poll = time.Hour
	for range 100 {
		p.Nudge(demoRepo)
	}
	p.Nudge("sf/unknown")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitReads(t, f, codeRepo, 1)
	waitReads(t, f, demoRepo, 2)
	p.Nudge(codeRepo)
	waitReads(t, f, codeRepo, 2)
	if got := f.readsOf(demoRepo); got != 2 {
		t.Fatalf("%s read %d times, want 2: 100 nudges while one is pending are one tick", demoRepo, got)
	}
	p.Nudge(demoRepo)
	waitReads(t, f, demoRepo, 3)
	if got := f.readsOf(codeRepo); got != 2 {
		t.Fatalf("%s read %d times, want 2: a nudge ticks only its own repository", codeRepo, got)
	}
}

type slowProjects struct {
	ChainProjects
	mu        sync.Mutex
	in, depth int
}

func (s *slowProjects) Projects(ctx context.Context) ([]Project, error) {
	s.mu.Lock()
	s.in++
	s.depth = max(s.depth, s.in)
	s.mu.Unlock()
	time.Sleep(time.Millisecond)
	defer func() { s.mu.Lock(); s.in--; s.mu.Unlock() }()
	return s.ChainProjects.Projects(ctx)
}

// The poller's caches assume one goroutine: a nudged tick and the
// ticker's never overlap.
func TestANudgedTickAndTheTickerNeverOverlap(t *testing.T) {
	f := &chainForgejo{runs: map[string][]map[string]any{codeRepo: {pushRun(61, commitC2, "success")}}}
	p, store := newTestPoller(t, f, seedProject(codeRepo))
	slow := &slowProjects{ChainProjects: store}
	p.Projects, p.Poll = slow, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	for range 200 {
		p.Nudge(codeRepo)
		time.Sleep(100 * time.Microsecond)
	}
	cancel()
	<-done
	if slow.depth != 1 {
		t.Fatalf("%d ticks ran at once, want 1", slow.depth)
	}
}
