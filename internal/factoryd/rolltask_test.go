package factoryd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The fakeStore's roll requests, as PGStore keeps them.

func (s *fakeStore) RollRequestSeen(ctx context.Context, sha string) (bool, error) {
	return slices.ContainsFunc(s.rolls, func(r RollRequest) bool { return r.SHA == sha }), nil
}

func (s *fakeStore) RollRuns(ctx context.Context, repository string) (first, carried int64, err error) {
	for _, r := range s.rolls {
		if r.Repository != repository {
			continue
		}
		if first == 0 || r.RunID < first {
			first = r.RunID
		}
		if r.State == RollOpened || r.State == RollMerged {
			carried = max(carried, r.RunID)
		}
	}
	return first, carried, nil
}

func (s *fakeStore) OpenRollTask(ctx context.Context, repository string) (Task, bool, error) {
	var open RollRequest
	for _, r := range s.rolls {
		if t, ok := s.tasks[r.TaskID]; ok && r.Repository == repository && r.State == RollOpened && !Final(t.Status) && r.RunID > open.RunID {
			open = r
		}
	}
	if open.TaskID == "" {
		return Task{}, false, nil
	}
	return s.tasks[open.TaskID], true, nil
}

func (s *fakeStore) InsertRollRequest(ctx context.Context, r RollRequest) error {
	if seen, _ := s.RollRequestSeen(ctx, r.SHA); seen {
		return fmt.Errorf("roll request %s exists", r.SHA)
	}
	s.rolls = append(s.rolls, r)
	return nil
}

func (s *fakeStore) DeleteRollRequest(ctx context.Context, sha string) error {
	s.rolls = slices.DeleteFunc(s.rolls, func(r RollRequest) bool { return r.SHA == sha })
	return nil
}

func (s *fakeStore) SettleRollRequests(ctx context.Context) error { return nil }

func (s *fakeStore) LastMergedRoll(ctx context.Context, repository string) (string, bool, error) {
	var last RollRequest
	for _, r := range s.rolls {
		if r.Repository == repository && r.State == RollMerged && r.RunID > last.RunID {
			last = r
		}
	}
	return last.SHA, last.SHA != "", nil
}

func (s *fakeStore) UncommittedRolls(ctx context.Context, taskID string) ([]RollRequest, error) {
	var out []RollRequest
	for _, r := range s.rolls {
		if r.TaskID == taskID && !r.Committed {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b RollRequest) int { return int(a.RunID - b.RunID) })
	return out, nil
}

func (s *fakeStore) MarkRollsCommitted(ctx context.Context, taskID string, shas []string, head string) error {
	for i, r := range s.rolls {
		if r.TaskID == taskID && slices.Contains(shas, r.SHA) {
			s.rolls[i].Committed, s.rolls[i].Head = true, head
		}
	}
	return nil
}

func (s *fakeStore) RollHead(ctx context.Context, taskID string) (string, error) {
	var last RollRequest
	for _, r := range s.rolls {
		if r.TaskID == taskID && r.Committed && r.RunID > last.RunID {
			last = r
		}
	}
	return last.Head, nil
}

func (s *fakeStore) RebaseRolls(ctx context.Context, taskID, head string) (bool, error) {
	recorded := slices.ContainsFunc(s.rolls, func(r RollRequest) bool {
		return r.TaskID == taskID && r.Committed && r.Head == head
	})
	if recorded {
		for i, r := range s.rolls {
			if r.TaskID == taskID && r.Committed && r.Head != head {
				s.rolls[i].Committed, s.rolls[i].Head = false, ""
			}
		}
	}
	return recorded, nil
}

func (s *fakeStore) RollTaskPRs(ctx context.Context, repository string) ([]int64, error) {
	var out []int64
	for _, t := range s.tasks {
		if t.Source == SourceRoll && t.Repository == repository && t.PRNumber != 0 {
			out = append(out, t.PRNumber)
		}
	}
	return out, nil
}

func (s *fakeStore) SetTaskSHA(ctx context.Context, id, sha string) error {
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	t.SHA = sha
	s.tasks[id] = t
	return nil
}

// imageIndex is an index as main CI pushes it: the image manifest
// platform and an attestation, whose build timestamps differ per build.
func imageIndex(platform, attestation string) string {
	return strings.NewReplacer("IMG", platform, "sha256:ATT", attestation).Replace(testIndexTemplate)
}

// Two builds of each image: old (pinned on main) and new, and a rebuild of
// each with no input change (the same image manifest, a new attestation).
var (
	gwOld, gwNew         = sha256Of(`{"img":"gw-old"}`), sha256Of(`{"img":"gw-new"}`)
	workerOld, workerNew = sha256Of(`{"img":"worker-old"}`), sha256Of(`{"img":"worker-new"}`)
	runnerImg            = sha256Of(`{"img":"runner"}`)

	gwOldIdx       = imageIndex(gwOld, "sha256:a1")
	gwNewIdx       = imageIndex(gwNew, "sha256:a2")
	gwNewRebuilt   = imageIndex(gwNew, "sha256:a3")
	workerOldIdx   = imageIndex(workerOld, "sha256:a4")
	workerRebuilt  = imageIndex(workerOld, "sha256:a5")
	workerNewIdx   = imageIndex(workerNew, "sha256:a6")
	runnerIdx      = imageIndex(runnerImg, "sha256:a7")
	runnerRebuilt  = imageIndex(runnerImg, "sha256:a8")
	runnerRebuilt2 = imageIndex(runnerImg, "sha256:a9")
)

const (
	codeRepo  = "sf/ai-software-factory-code"
	forkRepo  = "factory-agent/ai-software-factory-code"
	commitC1  = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	commitC2  = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
	commitHR  = "4040404040404040404040404040404040404040" // the last hand roll
	commitC0  = "c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0c0" // the last roll task's
	mergePR12 = "1212121212121212121212121212121212121212"
	mergePR14 = "1414141414141414141414141414141414141414"
	agentA1   = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	rollR0    = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
)

// sinceHandRoll is main's history from the last hand roll to a commit,
// newest first: a direct push, the merge of agent pull request #12 and
// its branch commit, and the merge of roll #14 and its roll commit.
var sinceHandRoll = []map[string]any{
	{"sha": commitC1, "commit": map[string]string{"message": "Tighten the gateway's upstream timeout\n\nbody"}, "parents": []any{map[string]any{}}},
	{"sha": mergePR12, "commit": map[string]string{"message": "Merge pull request 'Keys are no longer blocked' (#12) from factory-agent/x:factory/t-agent1 into main"},
		"parents": []any{map[string]any{}, map[string]any{}}},
	{"sha": agentA1, "commit": map[string]string{"message": "Stop blocking keys at every session, fixes #3 for @admin, see https://evil.test/x"}, "parents": []any{map[string]any{}}},
	{"sha": mergePR14, "commit": map[string]string{"message": "Merge pull request 'roll 0a0a0a0: worker' (#14) from factory-agent/x:factory/t-roll0 into main"},
		"parents": []any{map[string]any{}, map[string]any{}}},
	{"sha": rollR0, "commit": map[string]string{"message": "roll 0a0a0a0: worker\n\nMain CI's push run 50 published"}, "parents": []any{map[string]any{}}},
}

// rollForgejo plays Forgejo for the roller and RollCommit: the code
// repository and the bot's fork, their files by ref, the push runs, the
// registry, and records every write.
type rollForgejo struct {
	mu       sync.Mutex
	runs     []map[string]any
	files    map[string]map[string]string // "owner/name@ref" -> path -> content
	blobs    map[string]string            // "owner/name@ref path" -> blob sha
	registry map[string]string            // "sf/<image>/manifests/<reference>" -> body
	branches map[string]string            // fork branch -> head
	behind   int                          // the fork's main behind upstream

	issues      []string // "title\n\nbody"
	comments    []string // "#n body"
	commits     []map[string]any
	synced      int
	calls       []string
	touched     map[string][]string            // commit -> the files it changes against its first parent (the no-roll-merged fallback); unlisted: main.go
	rolled      map[string][]string            // commit -> the files it changes against the commit the last merged roll rolled; unlisted: touched, else main.go
	forkParents map[string][]string            // fork commit -> its parents (a merge commit has several)
	forkDiffs   map[string][]string            // fork "base...head" -> the files it changes
	statuses    map[string][]map[string]string // commit -> its statuses (status, context)
	pulls       []string                       // "head title" of every pull request opened
	deps        []string                       // "issue on dependency" of every issue dependency added
}

// mergeCommit is a test's multi-parent (merge) commit: it lists its
// parents in forkParents under its own name, so /git/commits/{sha} serves
// them.
func mergeCommit(parents ...string) map[string][]string {
	sha := "merge-" + strings.Join(parents, "-")
	return map[string][]string{sha: parents}
}

func (f *rollForgejo) srv(t *testing.T) (*Forgejo, *Registry) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		q := r.URL.Query()
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		p, repo := r.URL.Path, ""
		for _, name := range []string{codeRepo, forkRepo} {
			if rest, ok := strings.CutPrefix(p, "/api/v1/repos/"+name); ok {
				p, repo = rest, name
			}
		}
		switch {
		case p == "/v2/token":
			reply(map[string]string{"token": "bearer-tok"})
		case strings.HasPrefix(p, "/v2/"):
			body, ok := f.registry[strings.TrimPrefix(p, "/v2/")]
			if !ok {
				http.Error(w, "manifest unknown", http.StatusNotFound)
				return
			}
			io.WriteString(w, body)
		case repo == codeRepo && r.Method == "POST" && p == "/pulls":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.pulls = append(f.pulls, in["head"]+" "+in["title"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 70 + len(f.pulls), "html_url": "https://git.example/pr", "head": map[string]any{"sha": "rev1"}, "base": map[string]any{"sha": "m1"}, "state": "open"})
		case repo == codeRepo && strings.HasPrefix(p, "/commits/") && strings.HasSuffix(p, "/status"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, "/commits/"), "/status")
			state := "success"
			for _, s := range f.statuses[sha] {
				if s["status"] == "failure" {
					state = "failure"
				}
			}
			reply(map[string]any{"state": state, "statuses": f.statuses[sha]})
		case repo == codeRepo && r.Method == "POST" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/dependencies"):
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.deps = append(f.deps, strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/dependencies")+" on "+fmt.Sprint(in["index"]))
			w.WriteHeader(http.StatusCreated)
		case repo == codeRepo && p == "/actions/runs":
			if q.Get("event") != "push" {
				t.Errorf("runs query %v", q)
			}
			reply(map[string]any{"workflow_runs": f.runs})
		case r.Method == "GET" && strings.HasPrefix(p, "/raw/"):
			content, ok := f.files[repo+"@"+q.Get("ref")][strings.TrimPrefix(p, "/raw/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, content)
		case r.Method == "GET" && strings.HasPrefix(p, "/contents/"):
			path := strings.TrimPrefix(p, "/contents/")
			content, ok := f.files[repo+"@"+q.Get("ref")][path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64",
				"sha": f.blobs[repo+"@"+q.Get("ref")+" "+path]})
		case repo == codeRepo && p == "/commits" && q.Get("path") == "k8s":
			reply([]map[string]any{{"sha": commitHR, "parents": []map[string]string{{"sha": "p"}}}})
		case repo == forkRepo && strings.HasPrefix(p, "/git/commits/"):
			var parents []map[string]string
			for _, sha := range f.forkParents[strings.TrimPrefix(p, "/git/commits/")] {
				parents = append(parents, map[string]string{"sha": sha})
			}
			reply(map[string]any{"parents": parents})
		case repo == forkRepo && strings.HasPrefix(p, "/compare/"):
			var out []map[string]string
			for _, name := range f.forkDiffs[strings.TrimPrefix(p, "/compare/")] {
				out = append(out, map[string]string{"filename": name})
			}
			reply(map[string]any{"files": out})
		case repo == codeRepo && strings.HasPrefix(p, "/git/commits/"):
			reply(map[string]any{"parents": []map[string]string{{"sha": "parent-of-" + strings.TrimPrefix(p, "/git/commits/")}}})
		case repo == codeRepo && strings.HasPrefix(p, "/compare/"):
			base, head, _ := strings.Cut(strings.TrimPrefix(p, "/compare/"), "...")
			filesOf := func(names []string) []map[string]string {
				if names == nil {
					names = []string{"main.go"}
				}
				var out []map[string]string
				for _, name := range names {
					out = append(out, map[string]string{"filename": name})
				}
				return out
			}
			switch {
			case base == "parent-of-"+head:
				reply(map[string]any{"files": filesOf(f.touched[head])})
			case base == commitHR || base == commitC0:
				files := f.rolled[head]
				if files == nil {
					files = f.touched[head]
				}
				reply(map[string]any{"commits": sinceHandRoll, "files": filesOf(files)})
			default:
				t.Errorf("compare %s", p)
			}
		case repo == codeRepo && p == "/pulls" && q.Get("state") == "closed":
			reply([]map[string]any{
				{"number": 14, "title": "roll 0a0a0a0: worker", "merged": true, "merge_commit_sha": mergePR14, "head": map[string]any{"sha": rollR0}},
				{"number": 13, "title": "closed unmerged", "merged": false},
				{"number": 12, "title": "Keys are no longer blocked at every session (closes #4, @bob)", "merged": true, "merge_commit_sha": mergePR12,
					"head": map[string]any{"sha": agentA1, "ref": "refs/pull/12/head"}},
				{"number": 11, "title": "merged before the last roll", "merged": true, "merge_commit_sha": "0b0b"},
			})
		case r.Method == "POST" && repo == codeRepo && p == "/issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.issues = append(f.issues, in["title"]+"\n\n"+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"number": 30 + len(f.issues), "html_url": "https://git.example/sf/code/issues/31"})
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			reply([]any{})
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			f.comments = append(f.comments, strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")+" "+in["body"])
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"id": 900})
		case repo == forkRepo && p == "":
			reply(map[string]any{"fork": true, "private": true, "has_actions": false, "parent": map[string]string{"full_name": codeRepo}})
		case repo == forkRepo && p == "/sync_fork" && r.Method == "GET":
			reply(map[string]any{"allowed": f.behind > 0, "commits_behind": f.behind})
		case repo == forkRepo && p == "/sync_fork" && r.Method == "POST":
			f.synced++
			w.WriteHeader(http.StatusNoContent)
		case repo == forkRepo && strings.HasPrefix(p, "/branches/"):
			head, ok := f.branches[strings.TrimPrefix(p, "/branches/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(map[string]any{"commit": map[string]string{"id": head}})
		case repo == forkRepo && p == "/contents" && r.Method == "POST":
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			f.commits = append(f.commits, in)
			w.WriteHeader(http.StatusCreated)
			reply(map[string]any{"commit": map[string]string{"sha": fmt.Sprintf("roll-commit-%d", len(f.commits))}})
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Forgejo{BaseURL: srv.URL, BotUser: "factory-agent", BotPassword: "bot-pw"},
		&Registry{BaseURL: srv.URL, User: "puller", Password: "pw"}
}

func pushRun(id int64, sha, status string) map[string]any {
	return map[string]any{"id": id, "commit_sha": sha, "event": "push", "status": status, "prettyref": "main", "created": "2026-09-29T10:00:00Z"}
}

// registryOf serves each index under its digest and under the tags given.
func registryOf(tags map[string]string, indexes ...string) map[string]string {
	out := map[string]string{}
	for _, image := range []string{"factory-gateway", "opencode-worker", "ci-runner"} {
		for _, idx := range indexes {
			out["sf/"+image+"/manifests/"+sha256Of(idx)] = idx
		}
	}
	for key, idx := range tags {
		out[key] = idx
	}
	return out
}

func newTestRoller(t *testing.T, f *rollForgejo) (*Roller, *fakeStore, *fakeWorkflows) {
	t.Helper()
	fj, reg := f.srv(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	return &Roller{Forgejo: fj, Registry: reg, Store: store, Workflows: wf, Repo: Repo{Owner: "sf", Name: "ai-software-factory-code"}, Token: "registrar-tok",
		Budget: testBudget}, store, wf
}

// mainPins is the code repository's main before the roll: every image at
// its old build.
func mainPins() map[string]string {
	return k8sManifests(sha256Of(gwOldIdx), sha256Of(gwOldIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
}

// The newest green push run of main whose images changed files one roll
// task: the issue names the changed images, their new references and the
// merged pull requests since the last roll (the roll tasks' own left
// out); an image rebuilt with no input change is no change. A second poll
// files nothing: each commit once.
func TestRollerFilesOneRollPerCommit(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{
		runs:  []map[string]any{pushRun(58, commitC2, "running"), pushRun(57, commitC1, "success"), pushRun(56, "c0c0c0c0", "success")},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-roll0"] = Task{ID: "t-roll0", Source: SourceRoll, Status: "done", Repository: codeRepo, PRNumber: 14}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	newGw := registryHost + "factory-gateway@" + sha256Of(gwNewIdx)
	want := "roll c1c1c1c: factoryd, gateway\n\n" +
		"Main CI's push run 57 published the images of " + commitC1 + ". The digests under k8s/ that change:\n\n" +
		"- factoryd: " + newGw + "\n- gateway: " + newGw + "\n\n" +
		"Commits it carries since the last roll:\n\n- a1a1a1a Stop blocking keys at every session, (fixes) #3 for ＠admin, see hxxps://evil[.]test/x\n- c1c1c1c Tighten the gateway's upstream timeout\n\n" +
		"Merged pull requests it carries:\n\n- #12 Keys are no longer blocked at every session ((closes) #4, ＠bob)"
	if len(f.issues) != 1 || f.issues[0] != want {
		t.Fatalf("issues %q, want one:\n%s", f.issues, want)
	}
	if len(store.rolls) != 1 {
		t.Fatalf("roll requests %+v, want one", store.rolls)
	}
	req := store.rolls[0]
	task := store.tasks[req.TaskID]
	if req.SHA != commitC1 || req.RunID != 57 || req.State != RollOpened || req.Summary != want ||
		len(req.Changes) != 2 || req.Changes["factoryd"] != sha256Of(gwNewIdx) || req.Changes["gateway"] != sha256Of(gwNewIdx) {
		t.Fatalf("roll request %+v", req)
	}
	if task.Source != SourceRoll || task.Objective != want || task.SHA != commitC1 || task.Status != "queued" || task.Kind != "agent" ||
		task.IssueNumber != 31 || task.AgentSummary != want || task.BudgetEUR != testBudget().TaskDefaultEUR {
		t.Fatalf("roll task %+v", task)
	}
	if !slices.Equal(wf.started, []string{task.ID}) {
		t.Fatalf("started %v, want the roll task's workflow", wf.started)
	}
	if !strings.Contains(logs.String(), "roll task "+task.ID+", issue #31: roll c1c1c1c: factoryd, gateway") {
		t.Fatalf("log %q", logs.String())
	}
	if n := slices.Index(f.calls, "GET /v2/token"); n < 0 || slices.Contains(f.calls[n+1:], "GET /v2/token") {
		t.Fatalf("registry tokens read %v, want one per poll", f.calls)
	}

	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.issues) != 1 || len(store.rolls) != 1 || len(wf.started) != 1 {
		t.Fatalf("the second poll filed again: issues %d, rolls %d, starts %d", len(f.issues), len(store.rolls), len(wf.started))
	}
}

// A green commit whose images are rebuilds of the pinned ones (the same
// image manifest under a new attestation) records a skip and files
// nothing; a run older than a judged one is never judged.
func TestRollerSkipsACommitThatChangesNoImage(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{
		runs:  []map[string]any{pushRun(60, commitC2, "success")},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c2c2c2c": imageIndex(gwOld, "sha256:b1"),
			"sf/opencode-worker/manifests/c2c2c2c": workerRebuilt,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt2,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || !reflect.DeepEqual(store.rolls[0], RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 60, State: RollSkipped}) {
		t.Fatalf("roll requests %+v, want one skip", store.rolls)
	}
	if len(f.issues) != 0 || len(wf.started) != 0 || !strings.Contains(logs.String(), commitC2+" (run 60) changed no image") {
		t.Fatalf("issues %v, starts %v, log %q", f.issues, wf.started, logs.String())
	}

	f.runs = []map[string]any{pushRun(59, commitC1, "success")}
	calls := len(f.calls)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || len(f.calls) != calls+1 {
		t.Fatalf("an older run was judged: rolls %+v, calls %v", store.rolls, f.calls[calls:])
	}
}

// While a roll task is open, a newer green commit whose images differ
// from the roll branch's is folded into it: a request on the same task, a
// comment on its pull request and the change request that has its
// workflow commit it. No second task, and none before its pull request
// is open.
func TestRollerFoldsNewImagesIntoTheOpenRoll(t *testing.T) {
	branchPins := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
	f := &rollForgejo{
		runs:  []map[string]any{pushRun(61, commitC2, "success"), pushRun(57, commitC1, "success")},
		files: map[string]map[string]string{codeRepo + "@main": mainPins(), forkRepo + "@factory/t-open": branchPins},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c2c2c2c": gwNewRebuilt,
			"sf/opencode-worker/manifests/c2c2c2c": workerNewIdx,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt,
		}, gwOldIdx, gwNewIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-open"] = Task{ID: "t-open", Source: SourceRoll, Status: "in-review", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-open", Committed: true,
		Changes: map[string]string{"factoryd": sha256Of(gwNewIdx), "gateway": sha256Of(gwNewIdx)}}}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || len(f.calls) != 1 {
		t.Fatalf("folded before the pull request was open: rolls %+v, calls %v", store.rolls, f.calls)
	}

	open := store.tasks["t-open"]
	open.PRNumber = 21
	store.tasks["t-open"] = open
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 {
		t.Fatalf("roll requests %+v, want the fold", store.rolls)
	}
	fold := store.rolls[1]
	if fold.SHA != commitC2 || fold.TaskID != "t-open" || fold.State != RollOpened || fold.Committed ||
		len(fold.Changes) != 1 || fold.Changes["worker"] != sha256Of(workerNewIdx) {
		t.Fatalf("fold %+v, want the worker only (the gateway's rebuild is no change against the branch)", fold)
	}
	if !strings.HasPrefix(fold.Summary, "roll c2c2c2c: worker\n\n") || !strings.Contains(fold.Summary, "- worker: "+registryHost+"opencode-worker@"+sha256Of(workerNewIdx)) {
		t.Fatalf("fold summary %q", fold.Summary)
	}
	if len(f.issues) != 0 || len(wf.started) != 0 {
		t.Fatalf("a second task: issues %v, starts %v", f.issues, wf.started)
	}
	if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], "21 factoryd: folded into this roll:\n\nroll c2c2c2c: worker") {
		t.Fatalf("comments %q", f.comments)
	}
	if !slices.Equal(wf.signals, []string{"t-open roll_folded factory-agent #21: roll c2c2c2c: worker folded in"}) {
		t.Fatalf("signals %q", wf.signals)
	}
}

// bumpDigest is the roll's exact edit: the one pinned digest, nothing else.
func TestBumpDigestEditsOnlyThePinnedDigest(t *testing.T) {
	before := k8sManifests(digestOld, digestOld, digestWorkerOld, digestRunner)
	got, err := bumpDigest([]byte(before["k8s/factoryd.yaml"]), "opencode-worker", digestWorkerNew)
	if err != nil {
		t.Fatal(err)
	}
	if want := k8sManifests(digestOld, digestOld, digestWorkerNew, digestRunner)["k8s/factoryd.yaml"]; string(got) != want {
		t.Fatalf("bumped:\n%s\nwant:\n%s", got, want)
	}
	if _, err := bumpDigest([]byte(before["k8s/deployment.yaml"]), "opencode-worker", digestWorkerNew); err == nil {
		t.Fatal("a manifest without the image was bumped")
	}
}

func newRollRunner(t *testing.T, f *rollForgejo, rolls ...RollRequest) (*Runner, *fakeStore) {
	t.Helper()
	runner, _, store := newTestRunner(t, Task{ID: "t-roll", Source: SourceRoll, Status: "running", Repository: codeRepo, Kind: "agent"},
		newFakeKube(), &fakeObjectStore{})
	runner.forgejo, _ = f.srv(t)
	store.rolls = rolls
	return runner, store
}

// RollCommit writes the roll's first commit from main's head on a new
// branch of the synced fork, a fold as a second commit on the branch, and
// nothing when the branch carries every request. The commit's files are
// the exact edits and its message the requests' summaries.
func TestRollCommitWritesTheBranch(t *testing.T) {
	before := mainPins()
	f := &rollForgejo{behind: 3, branches: map[string]string{"main": "fork-main-head"},
		files: map[string]map[string]string{forkRepo + "@main": before},
		blobs: map[string]string{forkRepo + "@main k8s/factoryd.yaml": "blob-f", forkRepo + "@main k8s/deployment.yaml": "blob-d"}}
	newGw := sha256Of(gwNewIdx)
	runner, store := newRollRunner(t, f, RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll",
		Changes: map[string]string{"factoryd": newGw, "gateway": newGw}, Summary: "roll c1c1c1c: factoryd, gateway\n\nbody one"})

	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "roll-commit-1" {
		t.Fatalf("first commit %q %v", head, err)
	}
	after := k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx))
	wantFirst := map[string]any{"branch": "main", "new_branch": "factory/t-roll", "message": "roll c1c1c1c: factoryd, gateway\n\nbody one",
		"files": []any{
			map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f", "content": base64.StdEncoding.EncodeToString([]byte(after["k8s/factoryd.yaml"]))},
			map[string]any{"operation": "update", "path": "k8s/deployment.yaml", "sha": "blob-d", "content": base64.StdEncoding.EncodeToString([]byte(after["k8s/deployment.yaml"]))},
		}}
	if len(f.commits) != 1 || fmt.Sprint(f.commits[0]) != fmt.Sprint(wantFirst) {
		t.Fatalf("commit %v\nwant %v", f.commits, wantFirst)
	}
	if f.synced != 1 || !store.rolls[0].Committed || store.rolls[0].Head != "roll-commit-1" {
		t.Fatalf("synced %d, committed %v at %q", f.synced, store.rolls[0].Committed, store.rolls[0].Head)
	}
	// the base the workflow-directory guard compares with is the head
	// the branch was cut from
	if base := store.tasks["t-roll"].SHA; base != "fork-main-head" {
		t.Fatalf("task base %q, want the synced main head", base)
	}

	f.branches["factory/t-roll"] = "roll-commit-1"
	f.files[forkRepo+"@factory/t-roll"] = after
	f.blobs[forkRepo+"@factory/t-roll k8s/factoryd.yaml"] = "blob-f2"
	store.rolls = append(store.rolls, RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
		Changes: map[string]string{"worker": sha256Of(workerNewIdx)}, Summary: "roll c2c2c2c: worker\n\nbody two"})
	if out, err = execActivity(t, runner, ActivityRollCommit, "t-roll"); err != nil || out.Get(&head) != nil || head != "roll-commit-2" {
		t.Fatalf("fold commit %q %v", head, err)
	}
	folded := k8sManifests(newGw, newGw, sha256Of(workerNewIdx), sha256Of(runnerIdx))
	wantFold := map[string]any{"branch": "factory/t-roll", "message": "roll c2c2c2c: worker\n\nbody two",
		"files": []any{map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f2",
			"content": base64.StdEncoding.EncodeToString([]byte(folded["k8s/factoryd.yaml"]))}}}
	if len(f.commits) != 2 || fmt.Sprint(f.commits[1]) != fmt.Sprint(wantFold) || f.synced != 1 {
		t.Fatalf("fold commit %v\nwant %v (synced %d)", f.commits[1:], wantFold, f.synced)
	}

	f.branches["factory/t-roll"] = "roll-commit-2"
	if store.rolls[1].Head != "roll-commit-2" {
		t.Fatalf("fold recorded at %q", store.rolls[1].Head)
	}
	if out, err = execActivity(t, runner, ActivityRollCommit, "t-roll"); err != nil || out.Get(&head) != nil || head != "roll-commit-2" {
		t.Fatalf("nothing to commit: %q %v", head, err)
	}
	if len(f.commits) != 2 {
		t.Fatalf("a commit with nothing to write: %v", f.commits[2:])
	}
}

// RollCommit refuses a task that is no roll.
func TestRollCommitRefusesAnAgentTask(t *testing.T) {
	runner, _, _ := newTestRunner(t, Task{ID: "t1", Source: "webhook", Status: "running", Repository: codeRepo}, newFakeKube(), &fakeObjectStore{})
	if _, err := execActivity(t, runner, ActivityRollCommit, "t1"); err == nil || !strings.Contains(err.Error(), "no roll task") {
		t.Fatalf("err %v", err)
	}
}

// A roll's model review makes no model call (the test runner's supervisor
// endpoint fails the test on any call): the card reads the note.
func TestModelReviewOfARollCallsNoModel(t *testing.T) {
	runner, _, _ := newTestRunner(t, Task{ID: "t-roll", Source: SourceRoll, Status: "in-review", Repository: codeRepo, Kind: "agent",
		RequiredCapabilities: []string{"coder"}}, newFakeKube(), &fakeObjectStore{})
	var rv Review
	out, err := execActivity(t, runner, ActivityModelReview, "t-roll", PullRequest{Number: 9}, "r1", GateResults{})
	if err != nil || out.Get(&rv) != nil {
		t.Fatal(err)
	}
	if rv.Verdict != "" || rv.Note != "no model review: a roll task only bumps image digests main CI built, and makes no model call (ruling 178)" {
		t.Fatalf("review %+v", rv)
	}
}

// A green commit that changes nothing outside k8s/ against its first
// parent (a roll's own merge, a hand roll) is skipped before any image is
// read: a cold build cache cannot make it look like a change.
func TestRollerSkipsACommitThatChangesOnlyK8s(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(62, commitC2, "success")},
		touched: map[string][]string{commitC2: {"k8s/factoryd.yaml", "k8s/deployment.yaml", "k8s/factoryd.yaml"}}}
	roller, store, wf := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || store.rolls[0].State != RollSkipped || store.rolls[0].RunID != 62 || len(wf.started) != 0 {
		t.Fatalf("rolls %+v, starts %v", store.rolls, wf.started)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "GET /v2/") || strings.Contains(c, "/raw/") {
			t.Fatalf("read images or manifests for a k8s-only commit: %v", f.calls)
		}
	}
	if !strings.Contains(logs.String(), commitC2+" (run 62) changes nothing outside k8s/") {
		t.Fatalf("log %q", logs.String())
	}
}

// A code merge C1 is followed by a roll merge R1 of C1, whose push cancels
// C1's own main CI run (item 126): comparing R1 with the commit the last
// merged roll rolled (C0), not with R1's first parent (C1, the cancelled
// run's commit, whose code R1's tree already carries), finds C1's code
// change, so R1's images, which hold it, are rolled.
func TestRollerRollsACodeMergeWhoseRunTheRollMergeCancelled(t *testing.T) {
	const commitR1 = "9191919191919191919191919191919191919191"
	f := &rollForgejo{
		runs: []map[string]any{pushRun(102, commitR1, "success"), pushRun(101, commitC1, "cancelled")},
		// Against its first parent C1 (the cancelled run's own commit), R1
		// looks k8s-only: C1's code is already in that tree. Against C0,
		// the last merged roll, R1 also carries C1's code change.
		touched: map[string][]string{commitR1: {"k8s/factoryd.yaml"}},
		rolled:  map[string][]string{commitR1: {"internal/factoryd/x.go", "k8s/factoryd.yaml"}},
		files:   map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/9191919": gwNewIdx,
			"sf/opencode-worker/manifests/9191919": workerRebuilt,
			"sf/ci-runner/manifests/9191919":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC0, Repository: codeRepo, RunID: 90, State: RollMerged, TaskID: "t-prev", Committed: true}}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitR1 || store.rolls[1].RunID != 102 || store.rolls[1].State != RollOpened || len(wf.started) != 1 {
		t.Fatalf("rolls %+v, starts %v", store.rolls, wf.started)
	}
	if changes := store.rolls[1].Changes; len(changes) != 2 || changes["factoryd"] != sha256Of(gwNewIdx) || changes["gateway"] != sha256Of(gwNewIdx) {
		t.Fatalf("changes %+v, want the gateway image only (worker and runner are rebuilds)", changes)
	}
}

// A roll's own merge R2 of the commit the last merged roll rolled (C0)
// changes nothing outside k8s/ against C0 either: the old skip of a roll's
// own rebuild stays, now measured against the right base.
func TestRollerSkipsARollMergeOfTheLastRolledCommit(t *testing.T) {
	const commitR2 = "9292929292929292929292929292929292929292"
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(103, commitR2, "success")},
		touched: map[string][]string{commitR2: {"k8s/factoryd.yaml"}}}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC0, Repository: codeRepo, RunID: 90, State: RollMerged, TaskID: "t-prev", Committed: true}}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitR2 || store.rolls[1].RunID != 103 || store.rolls[1].State != RollSkipped || len(wf.started) != 0 {
		t.Fatalf("rolls %+v, starts %v", store.rolls, wf.started)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "GET /v2/") || strings.Contains(c, "/raw/") {
			t.Fatalf("read images or manifests for a k8s-only commit: %v", f.calls)
		}
	}
	if !strings.Contains(logs.String(), commitR2+" (run 103) changes nothing outside k8s/") {
		t.Fatalf("log %q", logs.String())
	}
}

// Every roll target is pinned exactly once in the repository's own
// manifests: an edit there that breaks the roller fails here.
func TestRollTargetsMatchTheManifests(t *testing.T) {
	for _, target := range RollTargets {
		content, err := os.ReadFile("../../" + target.File)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := pinnedRef(content, target.Image); err != nil {
			t.Errorf("%s in %s: %v", target.Name, target.File, err)
		}
	}
}

// Every agent task's token writes the bot's fork, a roll's branch included:
// a head that is not the commit RollCommit wrote is refused, for a fold and
// for a resumed run, so no agent commit rides under a roll's card.
func TestRollCommitRefusesABranchItDidNotWrite(t *testing.T) {
	branch := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
	for name, pending := range map[string]bool{"fold": true, "resumed": false} {
		f := &rollForgejo{branches: map[string]string{"factory/t-roll": "pushed-by-an-agent"},
			files: map[string]map[string]string{forkRepo + "@factory/t-roll": branch}}
		rolls := []RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1",
			Changes: map[string]string{"gateway": sha256Of(gwNewIdx)}}}
		if pending {
			rolls = append(rolls, RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
				Changes: map[string]string{"worker": sha256Of(workerNewIdx)}})
		}
		runner, _ := newRollRunner(t, f, rolls...)
		_, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
		if err == nil || !strings.Contains(err.Error(), "factory/t-roll is at pushed-by-an-agent, not at roll-commit-1, the commit factoryd wrote") ||
			!strings.Contains(err.Error(), errTerminal) {
			t.Fatalf("%s: err %v", name, err)
		}
		if len(f.commits) != 0 {
			t.Fatalf("%s: committed on a foreign head: %v", name, f.commits)
		}
	}
}

// An empty commit (the recovery after a failed roll) changes nothing
// under k8s/ either: its images are compared, not skipped unread.
func TestRollerComparesTheImagesOfAnEmptyCommit(t *testing.T) {
	logs := captureLog(t)
	f := &rollForgejo{runs: []map[string]any{pushRun(63, commitC2, "success")}, touched: map[string][]string{commitC2: {}},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c2c2c2c": gwNewIdx,
			"sf/opencode-worker/manifests/c2c2c2c": workerRebuilt,
			"sf/ci-runner/manifests/c2c2c2c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx)}
	roller, store, wf := newTestRoller(t, f)
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 1 || store.rolls[0].State != RollOpened || len(wf.started) != 1 || strings.Contains(logs.String(), "outside k8s/") {
		t.Fatalf("rolls %+v, starts %v, log %q", store.rolls, wf.started, logs.String())
	}
}

// A code commit A whose run builds slowly, then B, a roll's own merge (a
// hand roll) that changes nothing outside k8s/ against the commit the last
// merged roll rolled: B goes green first and is skipped; A's run,
// finishing later with the lower id, is still judged and opens the roll of
// A's code.
func TestRollerRollsACodeCommitWhoseRunFinishesAfterAK8sOnlyOne(t *testing.T) {
	f := &rollForgejo{
		runs:    []map[string]any{pushRun(101, commitC2, "success"), pushRun(100, commitC1, "running")},
		touched: map[string][]string{commitC2: {"k8s/factoryd.yaml"}},
		files:   map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx,
			"sf/opencode-worker/manifests/c1c1c1c": workerRebuilt,
			"sf/ci-runner/manifests/c1c1c1c":       runnerRebuilt,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: commitC0, Repository: codeRepo, RunID: 90, State: RollMerged, TaskID: "t-prev", Committed: true}}
	ctx := context.Background()
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitC2 || store.rolls[1].State != RollSkipped || len(wf.started) != 0 {
		t.Fatalf("after B: rolls %+v, starts %v", store.rolls, wf.started)
	}
	f.runs = []map[string]any{pushRun(101, commitC2, "success"), pushRun(100, commitC1, "success")}
	if err := roller.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 3 || store.rolls[2].SHA != commitC1 || store.rolls[2].State != RollOpened || len(wf.started) != 1 ||
		len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "roll c1c1c1c: factoryd, gateway\n\n") {
		t.Fatalf("after A: rolls %+v, starts %v, issues %q", store.rolls, wf.started, f.issues)
	}
}

// A green run older than a commit a roll carries is never judged: its
// images would roll main back.
func TestRollerNeverJudgesARunOlderThanARolledOne(t *testing.T) {
	f := &rollForgejo{runs: []map[string]any{pushRun(104, commitC1, "success")}}
	roller, store, wf := newTestRoller(t, f)
	store.tasks["t-prev"] = Task{ID: "t-prev", Source: SourceRoll, Status: "done", Repository: codeRepo}
	store.rolls = []RollRequest{{SHA: "b0b0", Repository: codeRepo, RunID: 90, State: RollSkipped},
		{SHA: commitC0, Repository: codeRepo, RunID: 105, State: RollMerged, TaskID: "t-prev", Committed: true}}
	if err := roller.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.rolls) != 2 || len(wf.started) != 0 || len(f.calls) != 1 {
		t.Fatalf("rolls %+v, starts %v, calls %v", store.rolls, wf.started, f.calls)
	}
}

// A RollCommit that died between its write and its record finds its own
// commit on the retry: the head's parent is the recorded head (or the
// base the branch was cut from), it changes the roll's files only, and
// they hold exactly what the pending requests make. It adopts it. A head
// that changes anything more is refused.
func TestRollCommitAdoptsItsOwnUnrecordedCommit(t *testing.T) {
	newGw := sha256Of(gwNewIdx)
	first := k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx))
	folded := k8sManifests(newGw, newGw, sha256Of(workerNewIdx), sha256Of(runnerIdx))
	fold := RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
		Changes: map[string]string{"worker": sha256Of(workerNewIdx)}, Summary: "roll c2c2c2c: worker"}
	for name, tc := range map[string]struct {
		rolls  []RollRequest
		parent string
		base   map[string]string
		diff   []string
		adopt  bool
	}{
		"fold":              {[]RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"}, fold}, "roll-commit-1", first, []string{"k8s/factoryd.yaml"}, true},
		"first commit":      {[]RollRequest{fold}, "fork-main-head", k8sManifests(newGw, newGw, sha256Of(workerOldIdx), sha256Of(runnerIdx)), []string{"k8s/factoryd.yaml"}, true},
		"with agent code":   {[]RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"}, fold}, "roll-commit-1", first, []string{"k8s/factoryd.yaml", "internal/factoryd/gates.go"}, false},
		"on another parent": {[]RollRequest{{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"}, fold}, "agent-commit", first, []string{"k8s/factoryd.yaml"}, false},
	} {
		f := &rollForgejo{branches: map[string]string{"factory/t-roll": "crashed-write"},
			forkParents: map[string][]string{"crashed-write": {tc.parent}},
			forkDiffs:   map[string][]string{tc.parent + "...crashed-write": tc.diff},
			files:       map[string]map[string]string{forkRepo + "@" + tc.parent: tc.base, forkRepo + "@crashed-write": folded}}
		runner, store := newRollRunner(t, f, tc.rolls...)
		task := store.tasks["t-roll"]
		task.SHA = "fork-main-head"
		store.tasks["t-roll"] = task
		out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
		if !tc.adopt {
			if err == nil || !strings.Contains(err.Error(), "the commit factoryd wrote") {
				t.Fatalf("%s: err %v", name, err)
			}
			continue
		}
		var head string
		if err != nil || out.Get(&head) != nil || head != "crashed-write" || len(f.commits) != 0 {
			t.Fatalf("%s: head %q err %v commits %v", name, head, err, f.commits)
		}
		if got, _ := store.RollHead(context.Background(), "t-roll"); got != "crashed-write" {
			t.Fatalf("%s: recorded head %q", name, got)
		}
	}
}

// The queue's staging merge advanced main into the roll's branch after
// factoryd wrote its commit: the tip is a two-parent merge carrying the
// recorded commit, not that commit itself. The guard walks the ancestry,
// finds factoryd's commit and adopts the tip; no commit is written.
func TestRollCommitAcceptsAQueueStagedMerge(t *testing.T) {
	newGw := sha256Of(gwNewIdx)
	folded := k8sManifests(newGw, newGw, sha256Of(workerNewIdx), sha256Of(runnerIdx))
	f := &rollForgejo{branches: map[string]string{"factory/t-roll": "staged-merge"},
		forkParents: map[string][]string{"staged-merge": {"roll-commit-1", "fork-main-head"}},
		files:       map[string]map[string]string{forkRepo + "@staged-merge": folded}}
	runner, store := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"worker": sha256Of(workerNewIdx)}})
	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "staged-merge" {
		t.Fatalf("head %q err %v", head, err)
	}
	if len(f.commits) != 0 {
		t.Fatalf("committed on a staged branch: %v", f.commits)
	}
	if got, _ := store.RollHead(context.Background(), "t-roll"); got != "staged-merge" {
		t.Fatalf("recorded head %q, want the merged tip", got)
	}
	for _, r := range store.rolls {
		if !r.Committed {
			t.Fatalf("roll %+v left uncommitted", r)
		}
	}
}

// Two RollCommit runs overlapped (the later attempt read the branch
// before the earlier one's write landed and the store after its record):
// the branch sits at the roll's FIRST commit while the store's newest
// marking is past it. That head is factoryd's own, so the guard does not
// fail the task: the marking past the branch goes back to pending and the
// fold is written on top of the branch, recorded at the head actually
// written.
func TestRollCommitWritesAFoldOnARecordedOwnHead(t *testing.T) {
	first := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerOldIdx), sha256Of(runnerIdx))
	folded := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerNewIdx), sha256Of(runnerRebuilt))
	f := &rollForgejo{branches: map[string]string{"factory/t-roll": "roll-commit-1"},
		files: map[string]map[string]string{forkRepo + "@factory/t-roll": first},
		blobs: map[string]string{forkRepo + "@factory/t-roll k8s/factoryd.yaml": "blob-f1"}}
	runner, store := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1",
			Changes: map[string]string{"gateway": sha256Of(gwNewIdx)}},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-2",
			Changes: map[string]string{"worker": sha256Of(workerNewIdx)}, Summary: "roll c2c2c2c: worker"},
		RollRequest{SHA: commitHR, Repository: codeRepo, RunID: 63, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"ci-runner": sha256Of(runnerRebuilt)}, Summary: "roll c3c3c3c: runner"})
	var head string
	out, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err != nil || out.Get(&head) != nil || head != "roll-commit-1" {
		t.Fatalf("head %q err %v", head, err)
	}
	want := map[string]any{"branch": "factory/t-roll", "message": "roll c2c2c2c: worker\n\nroll c3c3c3c: runner",
		"files": []any{map[string]any{"operation": "update", "path": "k8s/factoryd.yaml", "sha": "blob-f1",
			"content": base64.StdEncoding.EncodeToString([]byte(folded["k8s/factoryd.yaml"]))}}}
	if len(f.commits) != 1 || fmt.Sprint(f.commits[0]) != fmt.Sprint(want) {
		t.Fatalf("commit %v\nwant %v", f.commits, want)
	}
	for _, r := range store.rolls {
		if !r.Committed || r.Head != "roll-commit-1" {
			t.Fatalf("roll %+v, want committed at roll-commit-1", r)
		}
	}
	if got, _ := store.RollHead(context.Background(), "t-roll"); got != "roll-commit-1" {
		t.Fatalf("recorded head %q", got)
	}
}

// A tip that does not carry factoryd's commit (a foreign commit on the
// base) refuses with the existing message shape.
func TestRollCommitRefusesATipWithoutFactorydsCommit(t *testing.T) {
	folded := k8sManifests(sha256Of(gwNewIdx), sha256Of(gwNewIdx), sha256Of(workerNewIdx), sha256Of(runnerIdx))
	f := &rollForgejo{branches: map[string]string{"factory/t-roll": "rebased-by-an-agent"},
		forkParents: map[string][]string{"rebased-by-an-agent": {"agent-base-commit"}},
		files:       map[string]map[string]string{forkRepo + "@rebased-by-an-agent": folded}}
	runner, _ := newRollRunner(t, f,
		RollRequest{SHA: commitC1, Repository: codeRepo, RunID: 57, State: RollOpened, TaskID: "t-roll", Committed: true, Head: "roll-commit-1"},
		RollRequest{SHA: commitC2, Repository: codeRepo, RunID: 61, State: RollOpened, TaskID: "t-roll",
			Changes: map[string]string{"worker": sha256Of(workerNewIdx)}})
	_, err := execActivity(t, runner, ActivityRollCommit, "t-roll")
	if err == nil || !strings.Contains(err.Error(), "factory/t-roll is at rebased-by-an-agent, not at roll-commit-1, the commit factoryd wrote") ||
		!strings.Contains(err.Error(), errTerminal) {
		t.Fatalf("err %v", err)
	}
	if len(f.commits) != 0 {
		t.Fatalf("committed on a foreign tip: %v", f.commits)
	}
}

// Several green runs waiting and no roll open: the newest files the roll,
// which carries the older commits' code; no fold follows, and the older
// run falls below the roll.
func TestRollerFilesTheNewestOfSeveralGreenRuns(t *testing.T) {
	f := &rollForgejo{
		runs:  []map[string]any{pushRun(102, commitC2, "success"), pushRun(101, commitC1, "success")},
		files: map[string]map[string]string{codeRepo + "@main": mainPins()},
		registry: registryOf(map[string]string{
			"sf/factory-gateway/manifests/c1c1c1c": gwNewIdx, "sf/opencode-worker/manifests/c1c1c1c": workerRebuilt, "sf/ci-runner/manifests/c1c1c1c": runnerRebuilt,
			"sf/factory-gateway/manifests/c2c2c2c": gwNewRebuilt, "sf/opencode-worker/manifests/c2c2c2c": workerNewIdx, "sf/ci-runner/manifests/c2c2c2c": runnerRebuilt2,
		}, gwOldIdx, workerOldIdx, runnerIdx),
	}
	roller, store, wf := newTestRoller(t, f)
	store.rolls = []RollRequest{{SHA: "b0b0", Repository: codeRepo, RunID: 90, State: RollSkipped}}
	for range 2 {
		if err := roller.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.rolls) != 2 || store.rolls[1].SHA != commitC2 || store.rolls[1].State != RollOpened || len(wf.started) != 1 || len(wf.signals) != 0 ||
		len(f.issues) != 1 || !strings.HasPrefix(f.issues[0], "roll c2c2c2c: factoryd, gateway, worker\n\n") {
		t.Fatalf("rolls %+v, starts %v, signals %v, issues %q", store.rolls, wf.started, wf.signals, f.issues)
	}
}
