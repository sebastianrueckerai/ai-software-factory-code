package factoryd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	mu       sync.Mutex
	files    []string         // compare main...<sha>
	open     []map[string]any // open pull requests
	statuses []string         // combined-status bodies, served in order, the last repeating
	polls    int
	created  []map[string]any   // POST /pulls bodies
	comments []string           // issue comment bodies
	labels   map[int64][]string // issue → labels
	calls    []string
}

func (f *prForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/repo")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/users/factory-agent/tokens":
			io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasPrefix(p, "/compare/"):
			files := []map[string]string{}
			for _, name := range f.files {
				files = append(files, map[string]string{"filename": name})
			}
			json.NewEncoder(w).Encode(map[string]any{"files": files})
		case r.Method == "GET" && p == "/pulls":
			json.NewEncoder(w).Encode(f.open)
		case r.Method == "POST" && p == "/pulls":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.created = append(f.created, body)
			pr := map[string]any{"number": 12, "html_url": "https://git.example/sf/repo/pulls/12", "head": map[string]any{"ref": body["head"]}}
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
			io.WriteString(w, `{}`)
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
	fj := &prForgejo{files: []string{"Makefile"}}
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
	body := fj.created[0]
	if body["head"] != "factory/t1" || body["base"] != "main" || body["title"] != "Add a make check target" ||
		!strings.Contains(body["body"].(string), "Closes #4") {
		t.Fatalf("create body = %v", body)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"step":"pr_opened"`) || !strings.Contains(string(timeline), `"number":12`) {
		t.Fatalf("timeline = %s", timeline)
	}
}

func TestOpenPullRequestRefusesForgejoChanges(t *testing.T) {
	fj := &prForgejo{files: []string{"Makefile", ".forgejo/workflows/pr.yml"}}
	r, _, _ := newActivityRunner(t, prTask, fj)

	_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
	if err == nil || !strings.Contains(err.Error(), ".forgejo/workflows/pr.yml") || !isTerminal(err) {
		t.Fatalf("err = %v, want a terminal refusal naming the file", err)
	}
	if len(fj.created) != 0 {
		t.Fatal("a pull request was opened for a .forgejo change")
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
	var res ChecksResult
	val, _ := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	val.Get(&res)
	want := `check "pr / test (pull_request)": failure — Failure after 2m (` + r.forgejo.BaseURL + `/sf/repo/actions/runs/7/jobs/0)`
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
