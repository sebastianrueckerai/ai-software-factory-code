package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
)

// prForgejo plays the pull request, status and issue endpoints the
// workflow activities use.
type prForgejo struct {
	mu         sync.Mutex
	forgejo    map[string]string // commit → its top-level .forgejo tree id ("" = none); unlisted commits share "f0"
	closed     []int64           // PATCH /pulls/{n} closes
	closeFail  int               // PATCH /pulls/{n} answers 500 this many times first
	branchGone bool              // DELETE /branches/{b} answers 404
	deleted    []string          // "owner/repo branch" of every DELETE /branches/{b}
	pulls      map[int64]string  // GET /pulls/{n} bodies
	issueOff   bool              // GET /issues/{n} reports closed
	open       []map[string]any  // open pull requests
	statuses   []string          // combined-status bodies, served in order, the last repeating
	polls      int
	created    []map[string]any   // POST /pulls bodies
	comments   []string           // issue comment bodies
	labels     map[int64][]string // issue → labels
	calls      []string
}

func (f *prForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		p := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/repo"), "/api/v1/repos/factory-agent/repo")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/users/factory-agent/tokens":
			io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasPrefix(p, "/git/trees/"):
			sha, ok := f.forgejo[strings.TrimPrefix(p, "/git/trees/")]
			if !ok {
				sha = "f0"
			}
			tree := []map[string]string{{"path": "Makefile", "sha": "m"}}
			if sha != "" {
				tree = append(tree, map[string]string{"path": ".forgejo", "sha": sha})
			}
			json.NewEncoder(w).Encode(map[string]any{"truncated": false, "total_count": len(tree), "tree": tree})
		case r.Method == "GET" && strings.HasPrefix(p, "/pulls/"):
			var n int64
			fmt.Sscanf(p, "/pulls/%d", &n)
			io.WriteString(w, f.pulls[n])
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/") && !strings.HasSuffix(p, "/labels") && !strings.HasSuffix(p, "/comments"):
			state := "open"
			if f.issueOff {
				state = "closed"
			}
			fmt.Fprintf(w, `{"state":%q}`, state)
		case r.Method == "PATCH" && strings.HasPrefix(p, "/pulls/"):
			if f.closeFail > 0 {
				f.closeFail--
				http.Error(w, "forgejo down", http.StatusInternalServerError)
				return
			}
			var n int64
			fmt.Sscanf(p, "/pulls/%d", &n)
			f.closed = append(f.closed, n)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == "GET" && p == "/pulls":
			json.NewEncoder(w).Encode(f.open)
		case r.Method == "DELETE" && strings.HasPrefix(p, "/branches/"):
			if f.branchGone {
				http.NotFound(w, r)
				return
			}
			f.deleted = append(f.deleted, strings.Replace(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/branches/", " ", 1))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && p == "/pulls":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.created = append(f.created, body)
			headOwner, headRef, _ := strings.Cut(body["head"].(string), ":")
			pr := map[string]any{"number": 12, "html_url": "https://git.example/sf/repo/pulls/12",
				"head": map[string]any{"ref": headRef, "repo": map[string]any{"full_name": headOwner + "/repo"}}}
			f.open = append(f.open, pr)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(pr)
		case r.Method == "GET" && strings.HasPrefix(p, "/commits/"):
			body := f.statuses[min(f.polls, len(f.statuses)-1)]
			f.polls++
			io.WriteString(w, body)
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			f.comments = append(f.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 700+len(f.comments))
		case r.Method == "GET" && strings.HasSuffix(p, "/labels"):
			var n int64
			fmt.Sscanf(p, "/issues/%d/labels", &n)
			out := []map[string]string{}
			for _, l := range f.labels[n] {
				out = append(out, map[string]string{"name": l})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == "PUT" && strings.HasSuffix(p, "/labels"):
			var n int64
			fmt.Sscanf(p, "/issues/%d/labels", &n)
			var body struct{ Labels []string }
			json.NewDecoder(r.Body).Decode(&body)
			if f.labels == nil {
				f.labels = map[int64][]string{}
			}
			f.labels[n] = body.Labels
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newActivityRunner(t *testing.T, task Task, fj *prForgejo) (*Runner, *fakeStore, *fakeObjectStore) {
	t.Helper()
	runner, _, store := newTestRunner(t, task, newFakeKube(), &fakeObjectStore{})
	runner.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw"}
	runner.completion.Forgejo = runner.forgejo
	return runner, store, runner.objects.(*fakeObjectStore)
}

func execActivity(t *testing.T, r *Runner, name string, args ...any) (converter.EncodedValue, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	r.RegisterActivities(env)
	return env.ExecuteActivity(name, args...)
}

var prTask = Task{ID: "t1", Status: "running", Objective: "Add a make check target\n\nand wire it into CI", Repository: "sf/repo",
	SHA: "base", Branch: "factory/t1", IssueNumber: 4, Source: "api"}

func TestOpenPullRequestOpensOnceThenReuses(t *testing.T) {
	fj := &prForgejo{}
	r, _, objects := newActivityRunner(t, prTask, fj)

	var first, second PullRequest
	val, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
	if err != nil || val.Get(&first) != nil {
		t.Fatalf("first OpenPullRequest: %v", err)
	}
	val, err = execActivity(t, r, ActivityOpenPullRequest, "t1", "c2")
	if err != nil || val.Get(&second) != nil {
		t.Fatalf("second OpenPullRequest: %v", err)
	}
	if len(fj.created) != 1 {
		t.Fatalf("pull requests created = %d, want 1: the open PR for the branch is reused", len(fj.created))
	}
	if first.Number != 12 || second != first || first.URL != "https://git.example/sf/repo/pulls/12" {
		t.Fatalf("PRs = %+v, %+v", first, second)
	}
	if got, _, _ := r.store.Task(context.Background(), "t1"); got.PRNumber != 12 {
		t.Fatalf("recorded pr_number = %d, want 12", got.PRNumber)
	}
	body := fj.created[0]
	if body["head"] != "factory-agent:factory/t1" || body["base"] != "main" || body["title"] != "Add a make check target" ||
		!strings.Contains(body["body"].(string), "Closes #4") {
		t.Fatalf("create body = %v", body)
	}
	// the ruling 90 check compares the base tree on sf with the pushed commit's tree on the fork
	if !slices.Contains(fj.calls, "GET /api/v1/repos/sf/repo/git/trees/base") || !slices.Contains(fj.calls, "GET /api/v1/repos/factory-agent/repo/git/trees/c1") {
		t.Fatalf("calls = %v", fj.calls)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"step":"pr_opened"`) || !strings.Contains(string(timeline), `"number":12`) {
		t.Fatalf("timeline = %s", timeline)
	}
}

func TestOpenPullRequestRefusesForgejoChanges(t *testing.T) {
	// edited, deleted (renamed away), and added: each makes the entry differ from the base's
	for name, trees := range map[string]map[string]string{
		"edited":  {"base": "f0", "c1": "f1"},
		"deleted": {"base": "f0", "c1": ""},
		"added":   {"base": "", "c1": "f1"},
	} {
		fj := &prForgejo{forgejo: trees}
		r, _, _ := newActivityRunner(t, prTask, fj)
		_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
		if err == nil || !strings.Contains(err.Error(), "changes .forgejo/") || !isTerminal(err) {
			t.Fatalf("%s: err = %v, want a terminal refusal", name, err)
		}
		if len(fj.created) != 0 {
			t.Fatalf("%s: a pull request was opened for a .forgejo change", name)
		}
	}
}

// A later attempt whose push touches .forgejo/ finds the PR an earlier
// attempt opened: CI already ran from the refused tree, so the PR is
// closed with the reason, not left open behind a failed task.
func TestOpenPullRequestRefusalClosesTheOpenPR(t *testing.T) {
	fj := &prForgejo{forgejo: map[string]string{"c2": "f9"},
		open: []map[string]any{{"number": 12, "html_url": "https://git.example/sf/repo/pulls/12", "head": forkHead("factory/t1", "")}}}
	r, _, _ := newActivityRunner(t, prTask, fj)

	_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c2")
	if err == nil || !isTerminal(err) {
		t.Fatalf("err = %v, want terminal", err)
	}
	if len(fj.closed) != 1 || fj.closed[0] != 12 {
		t.Fatalf("closed = %v, want PR 12", fj.closed)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "changes .forgejo/") {
		t.Fatalf("comments = %q, want the reason on the PR", fj.comments)
	}
}

func statusBody(state string, statuses ...[4]string) string {
	list := []map[string]string{}
	for _, s := range statuses {
		list = append(list, map[string]string{"status": s[0], "context": s[1], "target_url": s[2], "description": s[3]})
	}
	b, _ := json.Marshal(map[string]any{"state": state, "statuses": list})
	return string(b)
}

func TestWaitChecksMapsCombinedStatus(t *testing.T) {
	test := [4]string{"failure", "pr / test (pull_request)", "/sf/repo/actions/runs/7/jobs/0", "Failure after 2m"}
	for name, tc := range map[string]struct {
		statuses []string
		want     ChecksState
		findings int
	}{
		"no status yet, then pending, then green": {[]string{statusBody(""), statusBody("pending", [4]string{"pending", "pr / test", "", ""}),
			statusBody("success", [4]string{"success", "pr / test", "", ""})}, ChecksSuccess, 0},
		"a failed job settles it while others run": {[]string{statusBody("failure", test, [4]string{"pending", "pr / image-build", "", ""})}, ChecksFailure, 1},
		"error counts as failure":                  {[]string{statusBody("error", [4]string{"error", "pr / vet", "", "boom"})}, ChecksFailure, 1},
		"warning passes":                           {[]string{statusBody("warning", [4]string{"warning", "pr / vet", "", ""})}, ChecksSuccess, 0},
	} {
		fj := &prForgejo{statuses: tc.statuses}
		r, _, _ := newActivityRunner(t, prTask, fj)
		r.cfg.ChecksTimeout = time.Second
		var res ChecksResult
		val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
		if err != nil || val.Get(&res) != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.State != tc.want || len(res.Findings) != tc.findings {
			t.Fatalf("%s: result = %+v, want %s with %d findings", name, res, tc.want, tc.findings)
		}
	}
	fj := &prForgejo{statuses: []string{statusBody("failure", test)}}
	r, _, _ := newActivityRunner(t, prTask, fj)
	// API calls go to the in-cluster address, links for people to the public one
	r.forgejo.PublicURL = "https://git.example.org"
	var res ChecksResult
	val, _ := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	val.Get(&res)
	want := `check "pr / test (pull_request)": failure — Failure after 2m (https://git.example.org/sf/repo/actions/runs/7/jobs/0)`
	if len(res.Findings) != 1 || res.Findings[0] != want {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
}

func TestWaitChecksTimesOutWhilePending(t *testing.T) {
	fj := &prForgejo{statuses: []string{statusBody("pending", [4]string{"pending", "pr / image-build", "", ""})}}
	r, _, _ := newActivityRunner(t, prTask, fj)
	r.cfg.ChecksTimeout = 50 * time.Millisecond

	var res ChecksResult
	val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	if err != nil || val.Get(&res) != nil || res.State != ChecksTimeout {
		t.Fatalf("result = (%+v, %v), want timeout", res, err)
	}
	if fj.polls < 2 {
		t.Fatalf("polls = %d, want it to keep polling until the timeout", fj.polls)
	}
}

func TestSetStateMovesTaskAndMirrorsLabel(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/queued"}}}
	task := prTask
	task.Status = "queued"
	r, store, _ := newActivityRunner(t, task, fj)

	for _, state := range []string{"running", "in-review", "running"} {
		if _, err := execActivity(t, r, ActivitySetState, "t1", state); err != nil {
			t.Fatalf("SetState %s: %v", state, err)
		}
		if got, _, _ := store.Task(context.Background(), "t1"); got.Status != state {
			t.Fatalf("status = %q, want %s", got.Status, state)
		}
		if labels := fj.labels[4]; len(labels) != 2 || labels[0] != "agent" || labels[1] != "state/"+state {
			t.Fatalf("labels = %v, want agent + state/%s", labels, state)
		}
	}
}

func TestEnsureIssueRequiresLinkedIssueAndOpensTimeline(t *testing.T) {
	r, _, objects := newActivityRunner(t, prTask, &prForgejo{})
	if _, err := execActivity(t, r, ActivityEnsureIssue, "t1"); err != nil {
		t.Fatal(err)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"step":"intake"`) || !strings.Contains(string(timeline), `"source":"api"`) ||
		!strings.Contains(string(timeline), `"issue":4`) {
		t.Fatalf("timeline = %s", timeline)
	}

	orphan := prTask
	orphan.ID, orphan.IssueNumber = "t2", 0
	r2, store, _ := newActivityRunner(t, orphan, &prForgejo{})
	store.CreateTask(context.Background(), orphan)
	if _, err := execActivity(t, r2, ActivityEnsureIssue, "t2"); err == nil || !isTerminal(err) {
		t.Fatalf("err = %v, want terminal: no issue to mirror onto", err)
	}
}

func TestFinishFailedCompletesCommentsAndIsIdempotent(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
	task := prTask
	task.Status = "in-review"
	r, store, objects := newActivityRunner(t, task, fj)

	for range 2 {
		if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "3 attempts exhausted; last: checks failed"); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "failed" {
		t.Fatalf("status = %q", got.Status)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "3 attempts exhausted; last: checks failed") {
		t.Fatalf("comments = %q, want one with the reason", fj.comments)
	}
	if labels := fj.labels[4]; len(labels) != 2 || labels[1] != "state/failed" {
		t.Fatalf("labels = %v", labels)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if strings.Count(string(timeline), `"step":"final"`) != 1 || !strings.Contains(string(timeline), `"state":"failed"`) {
		t.Fatalf("timeline = %s", timeline)
	}
}

func TestFinishDoneFromInReview(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
	task := prTask
	task.Status = "in-review"
	r, store, _ := newActivityRunner(t, task, fj)

	if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "done" {
		t.Fatalf("status = %q", got.Status)
	}
	if len(fj.comments) != 0 || fj.labels[4][1] != "state/done" {
		t.Fatalf("comments = %q, labels = %v", fj.comments, fj.labels[4])
	}
}

// A workflow restarted by the heal pass for a task already closing (its
// first workflow died mid-completion) cannot run it again: the refusal is
// terminal, so the new workflow goes straight to Finish.
func TestSetStateRefusesAFinishingTaskTerminally(t *testing.T) {
	for _, status := range []string{"closing", "done", "failed"} {
		task := prTask
		task.Status = status
		r, _, _ := newActivityRunner(t, task, &prForgejo{})
		if _, err := execActivity(t, r, ActivitySetState, "t1", "running"); err == nil || !isTerminal(err) {
			t.Fatalf("%s: err = %v, want terminal", status, err)
		}
	}
}

// Finish never fails terminally on a task it cannot fully read: a bad
// repository still completes it (uncommented), a vanished task is a no-op.
// A terminal Finish would fail the workflow and the heal pass would
// restart it forever.
func TestFinishCompletesATaskWithABrokenRepository(t *testing.T) {
	task := prTask
	task.Repository = "no-slash"
	fj := &prForgejo{}
	r, store, _ := newActivityRunner(t, task, fj)
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "repository is not owner/name"); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "failed" {
		t.Fatalf("status = %q", got.Status)
	}
	if _, err := execActivity(t, r, ActivityFinish, "gone", "failed", "x"); err != nil {
		t.Fatalf("Finish of an unknown task = %v, want no-op", err)
	}
}

// forkHead is a pull request head on the bot's fork (ruling 97).
func forkHead(ref, sha string) map[string]any {
	return map[string]any{"ref": ref, "sha": sha, "repo": map[string]any{"full_name": "factory-agent/repo"}}
}

// A failed task closes the pull request it opened (a closed issue must not
// leave the PR behind); an already closed or merged one is left alone.
func TestFinishFailedClosesTheOpenPR(t *testing.T) {
	open := `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t1","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	for name, tc := range map[string]struct {
		task   Task
		fj     *prForgejo
		closed []int64
	}{
		"open":   {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: open}}, []int64{12}},
		"closed": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: `{"number":12,"state":"closed","merged":false}`}}, nil},
		"merged": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: `{"number":12,"state":"closed","merged":true}`}}, nil},
		// a crash between opening the PR and recording its number: found by
		// its branch on the fork
		"unrecorded, open": {prTask, &prForgejo{open: []map[string]any{{"number": 12, "html_url": "u12", "state": "open", "head": forkHead("factory/t1", "c7")}}}, []int64{12}},
		"unrecorded, none": {prTask, &prForgejo{}, nil},
	} {
		fj := tc.fj
		r, store, _ := newActivityRunner(t, withStatus(tc.task, "in-review"), fj)
		if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "issue closed"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "failed" {
			t.Fatalf("%s: status = %q", name, got.Status)
		}
		if !slices.Equal(fj.closed, tc.closed) {
			t.Fatalf("%s: closed = %v, want %v", name, fj.closed, tc.closed)
		}
		if want := 1 + len(tc.closed); len(fj.comments) != want || !strings.Contains(fj.comments[want-1], "issue closed") {
			t.Fatalf("%s: comments = %q, want the reason on the issue and on a closed PR", name, fj.comments)
		}
		if !slices.Equal(fj.deleted, []string{"factory-agent/repo factory/t1"}) {
			t.Fatalf("%s: deleted = %v, want the task's branch on the fork", name, fj.deleted)
		}
	}
}

// The close comes before completion and its error is returned: a Forgejo
// blip retries the whole step, so no failed task keeps an open PR.
func TestFinishRetriesUntilThePullRequestIsClosed(t *testing.T) {
	open := `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t1","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	fj := &prForgejo{pulls: map[int64]string{12: open}, closeFail: 1}
	r, store, _ := newActivityRunner(t, withPR(withStatus(prTask, "in-review"), 12), fj)
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "issue closed"); err == nil {
		t.Fatal("Finish with the close refused = nil, want the error for a retry")
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "in-review" || len(fj.closed) != 0 || len(fj.comments) != 0 {
		t.Fatalf("after the refused close: status = %q, closed = %v, comments = %q, want in-review and nothing closed or commented", got.Status, fj.closed, fj.comments)
	}
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "issue closed"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "failed" || !slices.Equal(fj.closed, []int64{12}) {
		t.Fatalf("after the retry: status = %q, closed = %v, want failed and #12 closed", got.Status, fj.closed)
	}
	if len(fj.comments) != 2 {
		t.Fatalf("after the retry: comments = %q, want the reason once on the PR and once on the issue", fj.comments)
	}
}

// A finished task's branch on the fork goes with it (its PR is merged or
// closed by then); a branch already gone is done.
func TestFinishDeletesTheForkBranch(t *testing.T) {
	for name, tc := range map[string]struct {
		fj      *prForgejo
		deleted []string
	}{
		"deleted":      {&prForgejo{}, []string{"factory-agent/repo factory/t1"}},
		"already gone": {&prForgejo{branchGone: true}, nil},
	} {
		fj := tc.fj
		r, store, _ := newActivityRunner(t, withStatus(prTask, "in-review"), fj)
		if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "done" {
			t.Fatalf("%s: status = %q", name, got.Status)
		}
		if !slices.Equal(fj.deleted, tc.deleted) || len(fj.closed) != 0 || len(fj.comments) != 0 {
			t.Fatalf("%s: deleted = %v, closed = %v, comments = %q", name, fj.deleted, fj.closed, fj.comments)
		}
	}
}

func TestReconcileReadsWhereTheTaskStands(t *testing.T) {
	open := `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t1","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	for name, tc := range map[string]struct {
		task Task
		fj   *prForgejo
		want RunState
	}{
		"fresh": {prTask, &prForgejo{}, RunState{Status: "running"}},
		"open PR by branch": {prTask, &prForgejo{open: []map[string]any{{"number": 12, "html_url": "u12", "state": "open", "head": forkHead("factory/t1", "c7")}}},
			RunState{Status: "running", PR: PullRequest{Number: 12, URL: "u12"}, Head: "c7"}},
		"open PR by branch from a stranger's repository": {prTask, &prForgejo{open: []map[string]any{{"number": 13, "html_url": "u13", "state": "open",
			"head": map[string]any{"ref": "factory/t1", "sha": "c8", "repo": map[string]any{"full_name": "stranger/repo"}}}}},
			RunState{Status: "running"}},
		"recorded PR open": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: open}},
			RunState{Status: "running", PR: PullRequest{Number: 12, URL: "u12"}, Head: "c7"}},
		"recorded PR merged, issue closed": {withPR(prTask, 12), &prForgejo{issueOff: true, pulls: map[int64]string{12: `{"number":12,"state":"closed","merged":true}`}},
			RunState{Status: "running", Merged: true, IssueClosed: true}},
		"recorded PR closed": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: `{"number":12,"state":"closed","merged":false}`}},
			RunState{Status: "running", PRClosed: true}},
		"done":  {withStatus(prTask, "done"), &prForgejo{}, RunState{Status: "done", Skip: true}},
		"human": {withKind(prTask, "human"), &prForgejo{}, RunState{Status: "running", Skip: true}},
	} {
		r, _, _ := newActivityRunner(t, tc.task, tc.fj)
		var got RunState
		val, err := execActivity(t, r, ActivityReconcile, "t1")
		if err != nil || val.Get(&got) != nil || got != tc.want {
			t.Fatalf("%s: Reconcile = (%+v, %v), want %+v", name, got, err, tc.want)
		}
	}
}

func withPR(t Task, n int64) Task      { t.PRNumber = n; return t }
func withStatus(t Task, s string) Task { t.Status = s; return t }
func withKind(t Task, k string) Task   { t.Kind = k; return t }

// Escalate moves the task to waiting-human, mirrors it on the issue and
// asks there; its comment id is what an answer must be newer than. A
// healed run asking again keeps the state and asks again.
func TestEscalateAsksAHumanOnTheIssue(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}}
	r, store, objects := newActivityRunner(t, prTask, fj)

	var id int64
	val, err := execActivity(t, r, ActivityEscalate, "t1", "the check cannot pass\n\nProgress: 3 commands")
	if err != nil || val.Get(&id) != nil {
		t.Fatalf("Escalate: %v", err)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "waiting-human" {
		t.Fatalf("status = %q", got.Status)
	}
	if labels := fj.labels[4]; !slices.Equal(labels, []string{"agent", "state/waiting-human"}) {
		t.Fatalf("labels = %v", labels)
	}
	if id != 701 || len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "the check cannot pass") || !strings.Contains(fj.comments[0], "Reply on this issue") {
		t.Fatalf("id = %d, comments = %q", id, fj.comments)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"step":"escalated"`) || !strings.Contains(string(timeline), `"comment":701`) {
		t.Fatalf("timeline = %s", timeline)
	}
	if _, err := execActivity(t, r, ActivityEscalate, "t1", "asked again"); err != nil || len(fj.comments) != 2 {
		t.Fatalf("second Escalate: %v, comments = %d", err, len(fj.comments))
	}
	// a human answer starts the next attempt
	if _, err := execActivity(t, r, ActivitySetState, "t1", "running"); err != nil {
		t.Fatalf("SetState running from waiting-human: %v", err)
	}
}
