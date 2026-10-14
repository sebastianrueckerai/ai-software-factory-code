package factoryd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/telemetry"
)

// prForgejo plays the pull request, status and issue endpoints the
// workflow activities use.
type prForgejo struct {
	mu            sync.Mutex
	forgejo       map[string]string   // commit → its top-level .forgejo tree id ("" = none); unlisted commits share "f0"
	github        map[string]string   // commit → its top-level .github tree id; unlisted commits have none
	parents       map[string][]string // commit → its parents, first first; unlisted commits answer base
	closed        []int64             // PATCH /pulls/{n} closes
	closeFail     int                 // PATCH /pulls/{n} answers 500 this many times first
	branchGone    bool                // GET /branches/{b} answers 404 (Forgejo 15: DELETE then 500s)
	refuse        bool                // PATCH /pulls/{n} and DELETE /branches/{b} answer 403
	deleted       []string            // "owner/repo branch" of every DELETE /branches/{b}
	pulls         map[int64]string    // GET /pulls/{n} bodies
	issueOff      bool                // GET /issues/{n} reports closed
	closer        string              // the close on GET /issues/{n}/timeline; "" = sebastian
	open          []map[string]any    // open pull requests
	byHead        map[string]string   // GET /pulls/main/{owner}/{repo}:{branch} bodies beyond f.open
	statuses      []string            // combined-status bodies, served in order, the last repeating
	statusesBySHA map[string][]string // sha -> combined-status bodies, served in order, the last repeating; wins over statuses
	polls         int
	pollsBySHA    map[string]int     // per-sha poll cursor of statusesBySHA
	tasks         string             // actions/tasks page 1 body (testdata file); later pages empty
	tasksAuth     string             // the token actions/tasks expects; "" = registrar-tok
	created       []map[string]any   // POST /pulls bodies
	comments      []string           // issue comment bodies
	labels        map[int64][]string // issue → labels
	head          string             // GET /branches/{b}'s commit; a POST /contents moves it to head+"e"
	mainHead      string             // GET /branches/main's commit; unlisted branches answer head
	contents      []map[string]any   // POST /contents bodies
	calls         []string
}

func (f *prForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(firstPage(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		p := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/repo"), "/api/v1/repos/factory-agent/repo"), "/api/v1/repos/demo-app/demo-app"), "/api/v1/repos/factory-agent/demo-app")
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
			if sha, ok := f.github[strings.TrimPrefix(p, "/git/trees/")]; ok {
				tree = append(tree, map[string]string{"path": ".github", "sha": sha})
			}
			json.NewEncoder(w).Encode(map[string]any{"truncated": false, "total_count": len(tree), "tree": tree})
		case r.Method == "GET" && strings.HasPrefix(p, "/git/commits/"):
			sha := strings.TrimPrefix(p, "/git/commits/")
			var parents []string
			if listed, ok := f.parents[sha]; ok {
				parents = listed
			} else if sha != "base" {
				parents = []string{"base"}
			}
			list := []map[string]string{}
			for _, parent := range parents {
				list = append(list, map[string]string{"sha": parent})
			}
			json.NewEncoder(w).Encode(map[string]any{"parents": list})
		case r.Method == "GET" && strings.HasPrefix(p, "/pulls/main/"):
			head := strings.TrimPrefix(p, "/pulls/main/")
			if body, ok := f.byHead[head]; ok {
				io.WriteString(w, body)
				return
			}
			for _, pr := range f.open {
				h := pr["head"].(map[string]any)
				if h["repo"].(map[string]any)["full_name"].(string)+":"+h["ref"].(string) == head {
					json.NewEncoder(w).Encode(pr)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == "GET" && strings.HasSuffix(p, "/files"):
			var n int64
			fmt.Sscanf(p, "/pulls/%d/files", &n)
			io.WriteString(w, `[{"filename":"k8s/factoryd.yaml"}]`)
		case r.Method == "GET" && strings.HasPrefix(p, "/pulls/"):
			var n int64
			fmt.Sscanf(p, "/pulls/%d", &n)
			if body, ok := f.pulls[n]; ok {
				io.WriteString(w, body)
			} else {
				http.NotFound(w, r)
			}
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/timeline"):
			fmt.Fprintf(w, `[{"type":"close","user":{"login":%q}}]`, cmp.Or(f.closer, "sebastian"))
		case r.Method == "GET" && strings.HasPrefix(p, "/issues/") && !strings.HasSuffix(p, "/labels") && !strings.HasSuffix(p, "/comments"):
			state := "open"
			if f.issueOff {
				state = "closed"
			}
			fmt.Fprintf(w, `{"state":%q}`, state)
		case r.Method == "PATCH" && strings.HasPrefix(p, "/pulls/"):
			if f.refuse {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
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
		case r.Method == "GET" && strings.HasPrefix(p, "/branches/"):
			if f.branchGone {
				http.NotFound(w, r)
				return
			}
			head := f.head
			if p == "/branches/main" && f.mainHead != "" {
				head = f.mainHead
			}
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"id": head}})
		case r.Method == "POST" && p == "/contents":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.contents = append(f.contents, body)
			f.head += "e"
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"commit": map[string]string{"sha": f.head}})
		case r.Method == "DELETE" && strings.HasPrefix(p, "/branches/"):
			if f.refuse {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if f.branchGone {
				w.WriteHeader(http.StatusInternalServerError)
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
		case r.Method == "GET" && p == "/actions/tasks":
			if r.Header.Get("Authorization") != "token "+cmp.Or(f.tasksAuth, "registrar-tok") {
				t.Errorf("actions/tasks auth %q", r.Header.Get("Authorization"))
			}
			body := `{"workflow_runs":[],"total_count":0}`
			if r.URL.Query().Get("page") == "1" && f.tasks != "" {
				b, err := os.ReadFile("testdata/" + f.tasks)
				if err != nil {
					t.Error(err)
				}
				body = string(b)
			}
			io.WriteString(w, body)
		case r.Method == "POST" && p == "/user/login",
			r.Method == "GET" && strings.Contains(p, "/actions/runs/"):
			// No session/log route modeled here: JobLog fails cleanly on the
			// 404, so a failed status's finding carries no tail through this
			// fake (checksResult's own tests cover the tail itself).
			http.NotFound(w, r)
		case r.Method == "GET" && strings.HasPrefix(p, "/commits/"):
			sha := strings.TrimSuffix(strings.TrimPrefix(p, "/commits/"), "/status")
			if listed, ok := f.statusesBySHA[sha]; ok {
				if f.pollsBySHA == nil {
					f.pollsBySHA = map[string]int{}
				}
				body := listed[min(f.pollsBySHA[sha], len(listed)-1)]
				f.pollsBySHA[sha]++
				io.WriteString(w, body)
				return
			}
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
	runner.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw", Seeded: testSeeded}
	runner.completion.Forgejo = runner.forgejo
	return runner, store, runner.objects.(*fakeObjectStore)
}

func execActivity(t *testing.T, r *Runner, name string, args ...any) (converter.EncodedValue, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{&TerminalForgejoErrors{},
		&GateWatch{Store: r.store, Bound: func(context.Context, string, int64) time.Duration { return time.Hour }}}})
	r.RegisterActivities(env)
	return env.ExecuteActivity(name, args...)
}

// A Forgejo refusal or a not-found that escapes an activity ends its
// retries, and one the activity already made terminal stays as it is;
// anything else stays retryable.
func TestTerminalForgejoErrorsEndRetries(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		terminal bool
	}{
		"refused":          {fmt.Errorf("forgejo GET /x: %w: status 422: no", ErrRefused), true},
		"forgejo 404":      {fmt.Errorf("forgejo GET /x: %w", errForgejoNotFound), true},
		"store not found":  {fmt.Errorf("task t1: %w", ErrNotFound), false},
		"kube not found":   {ErrNotFound, false},
		"already terminal": {terminalError("task gone"), true},
		"transient":        {fmt.Errorf("forgejo GET /x: status 502: bad gateway"), false},
		"none":             {nil, false},
	} {
		var suite testsuite.WorkflowTestSuite
		suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		env := suite.NewTestActivityEnvironment()
		env.SetWorkerOptions(worker.Options{Interceptors: []interceptor.WorkerInterceptor{&TerminalForgejoErrors{}}})
		env.RegisterActivityWithOptions(func(context.Context) error { return tc.err }, activity.RegisterOptions{Name: "probe"})
		_, err := env.ExecuteActivity("probe")
		if (err == nil) != (tc.err == nil) || isTerminal(err) != tc.terminal {
			t.Fatalf("%s: err = %v, terminal = %v, want %v", name, err, isTerminal(err), tc.terminal)
		}
		if tc.err != nil && !strings.Contains(err.Error(), tc.err.Error()) {
			t.Fatalf("%s: err = %v, want the cause kept", name, err)
		}
	}
}

// stubActivityNext is a fixed ExecuteActivity result, standing in for the
// rest of the interceptor chain so TerminalForgejoErrors is tested on its
// own, with no Temporal failure-conversion round trip to blur what it
// actually returned.
type stubActivityNext struct {
	interceptor.ActivityInboundInterceptorBase
	err error
}

func (s *stubActivityNext) ExecuteActivity(context.Context, *interceptor.ExecuteActivityInput) (any, error) {
	return nil, s.err
}

// Every activity error crossing the worker boundary is scrubbed of
// factoryd's configured secrets. A message a scrub changes keeps its
// ApplicationError type and non-retryable flag but carries no cause, so
// Temporal does not record it twice (today's bug); a message nothing
// changes is the exact same error value; a cancellation never reaches the
// scrub at all.
func TestActivityErrorsScrubbed(t *testing.T) {
	const secret = "the-forgejo-bot-password-value"
	run := func(err error) error {
		inbound := (&TerminalForgejoErrors{Scrub: redact.New(secret)}).
			InterceptActivity(context.Background(), &stubActivityNext{err: err})
		_, got := inbound.ExecuteActivity(context.Background(), &interceptor.ExecuteActivityInput{})
		return got
	}

	t.Run("scrubs a plain retryable error", func(t *testing.T) {
		got := run(fmt.Errorf("forgejo GET /x: status 502: token=%s leaked", secret))
		want := "forgejo GET /x: status 502: token=[redacted] leaked"
		if got == nil || got.Error() != want {
			t.Fatalf("err = %v, want %q", got, want)
		}
		if isTerminal(got) {
			t.Fatalf("err = %v, want it still retryable", got)
		}
	})

	t.Run("scrubs an already-built ApplicationError and drops its cause", func(t *testing.T) {
		in := temporal.NewApplicationErrorWithOptions("probe failed: token "+secret, "customType",
			temporal.ApplicationErrorOptions{NonRetryable: true, Cause: errors.New("some original cause")})
		got := run(in)
		var app *temporal.ApplicationError
		if !errors.As(got, &app) {
			t.Fatalf("err = %v, want an ApplicationError", got)
		}
		if app.Type() != "customType" || !app.NonRetryable() {
			t.Fatalf("type = %q, nonRetryable = %v, want customType and true", app.Type(), app.NonRetryable())
		}
		if app.Message() != "probe failed: token [redacted]" {
			t.Fatalf("message = %q, want the secret scrubbed", app.Message())
		}
		if errors.Unwrap(got) != nil || strings.Contains(got.Error(), "some original cause") {
			t.Fatalf("err = %v, want no cause", got)
		}
	})

	t.Run("scrubs the forgejo-terminal wrap without doubling the message", func(t *testing.T) {
		got := run(fmt.Errorf("forgejo GET /x: %w: status 422: token=%s leaked", ErrRefused, secret))
		want := "forgejo GET /x: forgejo: refused: status 422: token=[redacted] leaked (type: terminal, retryable: false)"
		if got == nil || got.Error() != want {
			t.Fatalf("err = %v, want %q", got, want)
		}
		if !isTerminal(got) {
			t.Fatalf("err = %v, want it terminal", got)
		}
		if errors.Unwrap(got) != nil {
			t.Fatalf("err = %v, want no cause", got)
		}
	})

	t.Run("leaves an error with nothing to scrub the same value", func(t *testing.T) {
		in := fmt.Errorf("task t1: %w", ErrNotFound)
		if got := run(in); got != in { //nolint:errorlint // same value, not just equal, is the point
			t.Fatalf("err = %v, want the identical value %v", got, in)
		}
	})

	t.Run("passes a cancellation through untouched", func(t *testing.T) {
		in := temporal.NewCanceledError("stopping")
		if got := run(in); got != in { //nolint:errorlint // same value, not just equal, is the point
			t.Fatalf("err = %v, want the identical value %v", got, in)
		}
	})
}

// A recorded pull request Forgejo no longer has leaves nothing to close:
// Finish completes the task rather than failing on it for a day.
func TestFinishCompletesWhenThePullRequestIsGone(t *testing.T) {
	fj := &prForgejo{}
	r, store, _ := newActivityRunner(t, withPR(withStatus(prTask, "in-review"), 12), fj)
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "issue closed"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "failed" || len(fj.closed) != 0 || len(fj.comments) != 1 {
		t.Fatalf("status = %q, closed = %v, comments = %q, want failed, nothing closed, the reason on the issue", got.Status, fj.closed, fj.comments)
	}
}

var prTask = Task{ID: "t1", Status: "running", Objective: "Add a make check target\n\nand wire it into CI", Repository: "sf/repo",
	SHA: "base", IssueNumber: 4, Source: "api"}

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

func TestOpenPullRequestRefusesWorkflowDirChanges(t *testing.T) {
	// edited, deleted (renamed away), and added: each makes the entry differ from the base's
	for name, tc := range map[string]struct {
		forgejo, github map[string]string
		want            string
	}{
		"edited":       {forgejo: map[string]string{"base": "f0", "c1": "f1"}, want: ".forgejo/"},
		"deleted":      {forgejo: map[string]string{"base": "f0", "c1": ""}, want: ".forgejo/"},
		"added":        {forgejo: map[string]string{"base": "", "c1": "f1"}, want: ".forgejo/"},
		"github-added": {github: map[string]string{"c1": "g1"}, want: ".github/"},
	} {
		fj := &prForgejo{forgejo: tc.forgejo, github: tc.github}
		r, _, _ := newActivityRunner(t, prTask, fj)
		_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
		if err == nil || !strings.Contains(err.Error(), "changes "+tc.want) || !isTerminal(err) {
			t.Fatalf("%s: err = %v, want a terminal refusal naming %s", name, err, tc.want)
		}
		if len(fj.created) != 0 {
			t.Fatalf("%s: a pull request was opened for a workflow directory change", name)
		}
	}
}

// A branch that merges main forward carries main's own workflow edit: the
// entry at its head is one main holds, so the pull request opens (the
// same entry on the task's own commit, made without merging, matches
// neither the base's nor main's and stays refused above).
func TestOpenPullRequestAllowsMainsWorkflowEditsThroughAMerge(t *testing.T) {
	fj := &prForgejo{forgejo: map[string]string{"base": "f0", "c1": "m0", "mhead": "m0"}, mainHead: "mhead"}
	r, _, _ := newActivityRunner(t, prTask, fj)

	if _, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1"); err != nil {
		t.Fatalf("OpenPullRequest: %v, want no refusal: the workflow entry came from main through the merge", err)
	}
	if len(fj.created) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(fj.created))
	}
}

// Card #654's fourth attempt: the branch merged a main the head has since
// passed, so its .forgejo/ entry is that main's own — neither the task
// base's nor the head's. The entry at the merge-base is the branch's
// content came-from-main proof: the pull request opens.
func TestOpenPullRequestAllowsAMergedOlderMainsWorkflowEdits(t *testing.T) {
	fj := &prForgejo{forgejo: map[string]string{"base": "f0", "c1": "f1", "m1": "f1", "mhead": "f2"}, mainHead: "mhead",
		parents: map[string][]string{"c1": {"base", "m1"}, "mhead": {"m1"}, "m1": {"base"}}}
	r, _, _ := newActivityRunner(t, prTask, fj)

	if _, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1"); err != nil {
		t.Fatalf("OpenPullRequest: %v, want no refusal: the workflow entry came from the older main through the merge", err)
	}
	if len(fj.created) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(fj.created))
	}
}

// An entry the merge-base holds no more than the base and the head is the
// branch's own edit: the branch dies whatever main it merged, and the
// refusal names the merge-base alongside base and head.
func TestOpenPullRequestRefusesTheBranchsOwnWorkflowEditPastAMerge(t *testing.T) {
	fj := &prForgejo{forgejo: map[string]string{"base": "f0", "c1": "f9", "m1": "f1", "mhead": "f2"}, mainHead: "mhead",
		parents: map[string][]string{"c1": {"base", "m1"}, "mhead": {"m1"}, "m1": {"base"}}}
	r, _, _ := newActivityRunner(t, prTask, fj)

	_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
	if err == nil || !isTerminal(err) || !strings.Contains(err.Error(), "changes .forgejo/") || !strings.Contains(err.Error(), "merge-base m1") {
		t.Fatalf("err = %v, want a terminal refusal naming the merge-base m1", err)
	}
	if len(fj.created) != 0 {
		t.Fatalf("a pull request was opened for a workflow directory change")
	}
}

// A branch that merged nothing has the task base for its merge-base: the
// refusal stands exactly as before the merge-base was read, against base
// and head alone.
func TestOpenPullRequestRefusesAnUnmergedBranchsWorkflowEdit(t *testing.T) {
	fj := &prForgejo{forgejo: map[string]string{"base": "f0", "c1": "f1", "mhead": "f2"}, mainHead: "mhead",
		parents: map[string][]string{"c1": {"base"}, "mhead": {"base"}}}
	r, _, _ := newActivityRunner(t, prTask, fj)

	_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
	if err == nil || !isTerminal(err) || !strings.Contains(err.Error(), "changes .forgejo/") || !strings.Contains(err.Error(), "merge-base base") {
		t.Fatalf("err = %v, want today's refusal with the base as the merge-base", err)
	}
	if len(fj.created) != 0 {
		t.Fatalf("a pull request was opened for a workflow directory change")
	}
}

// The gate's base is the task's stamp, the fork point the branch was cut
// from: a branch whose .forgejo/ entry equals the stamped base's passes
// even when main's entry has since changed, and a branch whose entry
// equals neither the base's nor main's stays refused (task
// t-0c9745c86d12, whose clean branch a re-stamped base had made look like
// the task's own workflow edit).
func TestOpenPullRequestComparesAgainstTheStampedBase(t *testing.T) {
	fj := &prForgejo{forgejo: map[string]string{"base": "f0", "c1": "f0", "mhead": "m9"}, mainHead: "mhead"}
	r, _, _ := newActivityRunner(t, prTask, fj)
	if _, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1"); err != nil {
		t.Fatalf("OpenPullRequest: %v, want no refusal: the branch's .forgejo/ equals the stamped base's, main's own later edit notwithstanding", err)
	}
	if len(fj.created) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(fj.created))
	}

	fj = &prForgejo{forgejo: map[string]string{"base": "f0", "c1": "f9", "mhead": "m8"}, mainHead: "mhead"}
	r, _, _ = newActivityRunner(t, prTask, fj)
	_, err := execActivity(t, r, ActivityOpenPullRequest, "t1", "c1")
	if err == nil || !strings.Contains(err.Error(), "changes .forgejo/") || !isTerminal(err) {
		t.Fatalf("err = %v, want a terminal refusal naming .forgejo/: the entry equals neither the base's nor main's", err)
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
	// and with main green on its own head, the finding stays as it is
	fjGreen := &prForgejo{statusesBySHA: map[string][]string{
		"c1": {statusBody("failure", test)},
		"m1": {statusBody("success", [4]string{"success", "pr / test (pull_request)", "", ""})},
	}, mainHead: "m1"}
	r2, _, _ := newActivityRunner(t, prTask, fjGreen)
	r2.forgejo.PublicURL = "https://git.example.org"
	res = ChecksResult{}
	val, _ = execActivity(t, r2, ActivityWaitChecks, "t1", "c1")
	if val.Get(&res) != nil || len(res.Findings) != 1 || res.Findings[0] != want {
		t.Fatalf("findings with main green = %q, want %q", res.Findings, want)
	}
}

// HeadChecks reads the head's combined status once, without the checks
// wait's polling: green passes, a failure answers its failing check as
// WaitChecks reports it, and a head whose checks have not settled — some
// still running, or none reported yet — answers the pending (or absent)
// checks as the findings, which the approval sends back to the agent.
func TestHeadChecksReadsTheHeadOnce(t *testing.T) {
	fj := &prForgejo{
		statusesBySHA: map[string][]string{
			"c1": {statusBody("success", [4]string{"success", "pr / test", "", ""})},
			"m1": {statusBody("failure", [4]string{"failure", "pr / vet-and-format", "/sf/repo/actions/runs/7/jobs/0", "go vet failed"})},
			"p1": {statusBody("pending",
				[4]string{"success", "pr / vet", "", ""},
				[4]string{"pending", "pr / test", "", "running"})},
		},
		statuses: []string{statusBody("")},
	}
	r, _, _ := newActivityRunner(t, prTask, fj)
	r.forgejo.PublicURL = "https://git.example.org"
	head := func(t *testing.T, sha string) ChecksResult {
		t.Helper()
		var res ChecksResult
		val, err := execActivity(t, r, ActivityHeadChecks, "t1", sha)
		if err != nil || val.Get(&res) != nil {
			t.Fatalf("%s: %v", sha, err)
		}
		return res
	}
	if res := head(t, "c1"); res.State != ChecksSuccess || len(res.Findings) != 0 {
		t.Fatalf("c1 = %+v, want green with no findings", res)
	}
	if res := head(t, "m1"); res.State != ChecksFailure || len(res.Findings) != 1 ||
		res.Findings[0] != `check "pr / vet-and-format": failure — go vet failed (https://git.example.org/sf/repo/actions/runs/7/jobs/0)` {
		t.Fatalf("m1 = %+v, want the failing check as the checks wait reports it", res)
	}
	if res := head(t, "p1"); res.State != ChecksFailure || len(res.Findings) != 1 ||
		res.Findings[0] != `check "pr / test" is still pending on p1 (running)` {
		t.Fatalf("p1 = %+v, want the pending check named", res)
	}
	if res := head(t, "n0"); res.State != ChecksFailure || len(res.Findings) != 1 ||
		res.Findings[0] != "no check has reported on n0 yet" {
		t.Fatalf("n0 = %+v, want the absent checks named", res)
	}
	if fj.polls != 1 || fj.pollsBySHA["c1"]+fj.pollsBySHA["m1"]+fj.pollsBySHA["p1"] != 3 {
		t.Fatalf("polls = %d %+v: each head read once, no polling", fj.polls, fj.pollsBySHA)
	}
}

// When a head check fails, the gate reads main's state for the same check:
// a check also failing on main is named as such in the finding, closed
// with main's failing context — the check, not the context, for main runs
// the push workflow — so the agent's session and the supervisor's guidance
// point at the pre-existing defect instead of the card (Stage 19, finding
// 195).
func TestWaitChecksNamesMainFailure(t *testing.T) {
	headFail := [4]string{"failure", "pr / test (pull_request)", "/sf/repo/actions/runs/7/jobs/0", "Failure after 2m"}
	mainFail := [4]string{"failure", "ci / test (push)", "/sf/repo/actions/runs/8/jobs/0", "Failure after 3m"}
	fj := &prForgejo{
		statusesBySHA: map[string][]string{
			"c1": {statusBody("failure", headFail)},
			"m1": {statusBody("failure", mainFail)},
		},
		mainHead: "m1",
	}
	r, _, _ := newActivityRunner(t, prTask, fj)
	r.forgejo.PublicURL = "https://git.example.org"
	want := `check "pr / test (pull_request)": failure — Failure after 2m (https://git.example.org/sf/repo/actions/runs/7/jobs/0)` +
		` (also failing on main: ci / test (push))`
	var res ChecksResult
	val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	if err != nil || val.Get(&res) != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0] != want {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
}

// A head failure main does not share keeps today's finding, with no note:
// main runs the check green, under its own push context.
func TestWaitChecksKeepsFindingWhenMainGreen(t *testing.T) {
	headFail := [4]string{"failure", "pr / test (pull_request)", "/sf/repo/actions/runs/7/jobs/0", "Failure after 2m"}
	fj := &prForgejo{
		statusesBySHA: map[string][]string{
			"c1": {statusBody("failure", headFail)},
			"m1": {statusBody("success", [4]string{"success", "ci / test (push)", "", ""})},
		},
		mainHead: "m1",
	}
	r, _, _ := newActivityRunner(t, prTask, fj)
	r.forgejo.PublicURL = "https://git.example.org"
	want := `check "pr / test (pull_request)": failure — Failure after 2m (https://git.example.org/sf/repo/actions/runs/7/jobs/0)`
	var res ChecksResult
	val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	if err != nil || val.Get(&res) != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0] != want {
		t.Fatalf("findings = %q, want %q", res.Findings, want)
	}
}

// A main run that is missing counts as passing: a main head no check has
// reported on, and a main head that reads empty, leave the head's finding
// as today's refusal — never a false "also failing on main".
func TestWaitChecksMissingMainRunIsPassing(t *testing.T) {
	headFail := [4]string{"failure", "pr / test (pull_request)", "/sf/repo/actions/runs/7/jobs/0", "Failure after 2m"}
	for name, fj := range map[string]*prForgejo{
		"no check has reported on main's head": {
			statusesBySHA: map[string][]string{
				"c1": {statusBody("failure", headFail)},
				"m1": {statusBody("")},
			},
			mainHead: "m1",
		},
		"main's head reads empty": {
			statusesBySHA: map[string][]string{"c1": {statusBody("failure", headFail)}},
		},
	} {
		r, _, _ := newActivityRunner(t, prTask, fj)
		r.forgejo.PublicURL = "https://git.example.org"
		want := `check "pr / test (pull_request)": failure — Failure after 2m (https://git.example.org/sf/repo/actions/runs/7/jobs/0)`
		var res ChecksResult
		val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
		if err != nil || val.Get(&res) != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.State != ChecksFailure || len(res.Findings) != 1 || res.Findings[0] != want {
			t.Fatalf("%s: findings = %q, want %q", name, res.Findings, want)
		}
	}
}

// A green head is untouched: main's state is not even read, so a red main
// cannot sink a green head's verdict.
func TestWaitChecksGreenHeadUntouched(t *testing.T) {
	fj := &prForgejo{
		statusesBySHA: map[string][]string{
			"c1": {statusBody("success", [4]string{"success", "pr / test (pull_request)", "", ""})},
			"m1": {statusBody("failure", [4]string{"failure", "ci / test (push)", "", ""})},
		},
		mainHead: "m1",
	}
	r, _, _ := newActivityRunner(t, prTask, fj)
	var res ChecksResult
	val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	if err != nil || val.Get(&res) != nil {
		t.Fatal(err)
	}
	if res.State != ChecksSuccess || len(res.Findings) != 0 {
		t.Fatalf("res = %+v, want green with no findings", res)
	}
}

// checksResult appends a failed status's log tail to its finding: the last
// 40 of 50 lines, none dropped for length (design 4.4).
func TestChecksFindingCarriesLogTail(t *testing.T) {
	var log strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	cs := CombinedStatus{State: "failure", Statuses: []CommitStatus{
		{State: "failure", Context: "pr / test", TargetURL: "/sf/repo/actions/runs/7/jobs/0", Description: "Failure after 2m"},
	}}
	got := checksResult(cs, "https://git.example.org", func(target string) ([]byte, error) {
		if target != "/sf/repo/actions/runs/7/jobs/0" {
			t.Fatalf("logOf target = %q", target)
		}
		return []byte(log.String()), nil
	})
	var tail strings.Builder
	for i := 11; i <= 50; i++ {
		fmt.Fprintf(&tail, "line %d\n", i)
	}
	want := `check "pr / test": failure — Failure after 2m (https://git.example.org/sf/repo/actions/runs/7/jobs/0)` +
		"\n" + strings.TrimRight(tail.String(), "\n")
	if len(got.Findings) != 1 || got.Findings[0] != want {
		t.Fatalf("finding = %q, want %q", got.Findings, want)
	}
}

// checksResult never appends a tail a log fetch could not read (log fetch
// failure must not sink an otherwise-known finding), and skips fetching
// altogether when logOf is nil (gates.go's merge check: it never reads
// findings, so it passes none).
func TestChecksFindingSkipsUnreadableLog(t *testing.T) {
	cs := CombinedStatus{State: "failure", Statuses: []CommitStatus{
		{State: "failure", Context: "pr / test", TargetURL: "/sf/repo/actions/runs/7/jobs/0", Description: "Failure after 2m"},
	}}
	want := `check "pr / test": failure — Failure after 2m (https://git.example.org/sf/repo/actions/runs/7/jobs/0)`
	failing := checksResult(cs, "https://git.example.org", func(string) ([]byte, error) { return nil, errForgejoNotFound })
	if len(failing.Findings) != 1 || failing.Findings[0] != want {
		t.Fatalf("findings (failing fetch) = %q, want %q", failing.Findings, want)
	}
	withoutFetch := checksResult(cs, "https://git.example.org", nil)
	if len(withoutFetch.Findings) != 1 || withoutFetch.Findings[0] != want {
		t.Fatalf("findings (nil logOf) = %q, want %q", withoutFetch.Findings, want)
	}
}

// logTail keeps 40 lines "line 1".."line 50" down to lines 11 to 50 when
// they fit easily under the byte cap.
func TestLogTailLineCap(t *testing.T) {
	var log strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&log, "line %d\n", i)
	}
	var want strings.Builder
	for i := 11; i <= 50; i++ {
		fmt.Fprintf(&want, "line %d\n", i)
	}
	if got, want := logTail([]byte(log.String())), strings.TrimRight(want.String(), "\n"); got != want {
		t.Fatalf("logTail = %q, want %q", got, want)
	}
}

// logTail cuts a too-long tail down to the byte cap by dropping whole
// lines from the front, never mid-line: 40 lines of 128 bytes (5120 bytes,
// none dropped by the 40-line cap) need their first 8 lines (1024 bytes)
// gone to reach 4095 bytes, the largest whole-line count at or under 4096.
func TestLogTailCapped(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("%3d ", i+1) + strings.Repeat("x", 123) // 127 bytes
	}
	log := strings.Join(lines, "\n") + "\n" // 40 * 128 = 5120 bytes
	want := strings.Join(lines[8:], "\n")   // 32 lines: 32*127 + 31 = 4095 bytes
	if len(want) != 4095 {
		t.Fatalf("test setup: want is %d bytes, want 4095", len(want))
	}
	if got := logTail([]byte(log)); got != want {
		t.Fatalf("logTail = %q (%d bytes), want %q (%d bytes)", got, len(got), want, len(want))
	}
}

// logTail strips each surviving line's leading Forgejo Actions timestamp
// before the byte cap ever sees it, freeing that budget for the line's own
// text (M2, review-task115.md).
func TestLogTailStripsTimestamp(t *testing.T) {
	log := "2026-09-30T22:50:22.9434409Z go test ./...\n2026-09-30T22:50:23.1234567Z ok\n"
	want := "go test ./...\nok"
	if got := logTail([]byte(log)); got != want {
		t.Fatalf("logTail = %q, want %q", got, want)
	}
}

// logTail's hard cut for a single line past the byte cap moves forward to
// a UTF-8 rune boundary rather than splitting one in half (M3,
// review-task115.md): 1666 "€" (3 bytes each) plus "ab" is 5000 bytes; the
// naive tail[904:] would begin mid-rune (904 falls inside the 302nd "€"),
// so the cut advances to byte 906, the last 1364 "€" plus "ab" (4094
// bytes).
func TestLogTailRuneSafeCut(t *testing.T) {
	line := strings.Repeat("€", 1666) + "ab"
	if len(line) != 5000 {
		t.Fatalf("test setup: line is %d bytes, want 5000", len(line))
	}
	want := strings.Repeat("€", 1364) + "ab"
	if len(want) != 4094 {
		t.Fatalf("test setup: want is %d bytes, want 4094", len(want))
	}
	got := logTail([]byte(line))
	if got != want {
		t.Fatalf("logTail = %q (%d bytes), want %q (%d bytes)", got, len(got), want, len(want))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("logTail = %q is not valid UTF-8", got)
	}
}

// The WaitChecks closure only fetches a failed status's log when its
// target lands under the task's own owner/repo/actions/runs/: a crafted
// target_url could otherwise send the bot's broadly-collaborating session
// fetching another project's log (M1, review-task115.md).
func TestWaitChecksRejectsForeignLogTarget(t *testing.T) {
	foreign := [4]string{"failure", "pr / test", "https://evil.example/sf/repo/actions/runs/7/jobs/0", "Failure after 2m"}
	fj := &prForgejo{statuses: []string{statusBody("failure", foreign)}}
	r, _, _ := newActivityRunner(t, prTask, fj)
	var res ChecksResult
	val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	if err != nil || val.Get(&res) != nil {
		t.Fatalf("%v", err)
	}
	if len(res.Findings) != 1 || strings.Contains(res.Findings[0], "\n") {
		t.Fatalf("findings = %q, want the finding with no log tail", res.Findings)
	}
	for _, c := range fj.calls {
		if strings.Contains(c, "/user/login") {
			t.Fatalf("calls = %v, want no login attempt for a target outside the task's own repo", fj.calls)
		}
	}
}

// When the checks finish, the checks step lists the head's jobs: name,
// status, queue and run seconds. A factory project's jobs are read with
// the projects token (its organisation's owner), a seed project's with
// the Actions token its file names.
func TestWaitChecksRecordsTheHeadsJobs(t *testing.T) {
	for _, factory := range []bool{false, true} {
		testWaitChecksRecordsTheHeadsJobs(t, factory)
	}
}

func testWaitChecksRecordsTheHeadsJobs(t *testing.T, factory bool) {
	head := "c62cd2d1ad2dd06b8e73354a0ff07fde47321184"
	fj := &prForgejo{statuses: []string{statusBody("success", [4]string{"success", "pr / test", "", ""})}, tasks: "actions-tasks-3.json"}
	r, store, objects := newActivityRunner(t, prTask, fj)
	if factory {
		fj.tasksAuth, r.forgejo.ProjectsToken = "projects-tok", "projects-tok"
		store.projects = []Project{{Name: "repo", Repository: prTask.Repository, Objects: "factory", Agents: "coder", File: true, MergeRule: "auto", GitToken: "g", MergeToken: "m"}}
	}
	r.cfg.CIQueue = NewCIQueue(telemetry.Noop())
	r.cfg.CIQueue.byTask[103] = queued{seconds: 40}
	if _, err := execActivity(t, r, ActivityWaitChecks, "t1", head); err != nil {
		t.Fatal(err)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	var step struct {
		Step string           `json:"step"`
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(timeline), &step); err != nil || step.Step != "checks" {
		t.Fatalf("timeline %s: %v", timeline, err)
	}
	want := []map[string]any{
		{"name": "test", "status": "success", "run_s": 258.0},
		{"name": "vet-and-format", "status": "success", "run_s": 146.0},
		{"name": "image-build", "status": "success", "queue_s": 40.0, "run_s": 53.0},
	}
	if !reflect.DeepEqual(step.Jobs, want) {
		t.Fatalf("jobs %v, want %v", step.Jobs, want)
	}
}

func TestWaitChecksTimesOutWhilePending(t *testing.T) {
	fj := &prForgejo{statuses: []string{statusBody("pending", [4]string{"pending", "pr / image-build", "", ""})}}
	r, _, objects := newActivityRunner(t, prTask, fj)
	r.cfg.ChecksTimeout = 50 * time.Millisecond

	var res ChecksResult
	from := time.Now()
	val, err := execActivity(t, r, ActivityWaitChecks, "t1", "c1")
	if err != nil || val.Get(&res) != nil || res.State != ChecksTimeout {
		t.Fatalf("result = (%+v, %v), want timeout", res, err)
	}
	if fj.polls < 2 {
		t.Fatalf("polls = %d, want it to keep polling until the timeout", fj.polls)
	}
	startedWithin(t, objects, "checks", from)
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
	task := prTask
	task.CreatedAt = time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC)
	r, _, objects := newActivityRunner(t, task, &prForgejo{})
	if _, err := execActivity(t, r, ActivityEnsureIssue, "t1"); err != nil {
		t.Fatal(err)
	}
	timeline, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	if !strings.Contains(string(timeline), `"step":"intake"`) || !strings.Contains(string(timeline), `"source":"api"`) ||
		!strings.Contains(string(timeline), `"issue":4`) {
		t.Fatalf("timeline = %s", timeline)
	}
	// intake runs from the issue's creation
	if got := timelineStep(t, objects, "t1", "intake")["started"]; got != "2026-09-28T07:00:00Z" {
		t.Fatalf("intake started = %v, want the issue's created_at", got)
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
		if len(fj.deleted) != 0 {
			t.Fatalf("%s: deleted = %v, want the failed task's branch kept", name, fj.deleted)
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

// A cleanup Forgejo refuses (closing the PR, deleting the branch) cannot
// succeed on a retry: Finish logs it and completes the task, or the failed
// workflow would be restarted by the heal pass every minute.
func TestFinishCompletesPastARefusedCleanup(t *testing.T) {
	open := `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t1","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`
	fj := &prForgejo{pulls: map[int64]string{12: open}, refuse: true}
	r, store, _ := newActivityRunner(t, withPR(withStatus(prTask, "in-review"), 12), fj)
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "issue closed"); err != nil {
		t.Fatalf("Finish past a refused cleanup = %v, want nil", err)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.Status != "failed" {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "issue closed") {
		t.Fatalf("comments = %q, want the reason on the issue", fj.comments)
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

// A failed task keeps its branch on the fork: its work survives for a
// refile.
func TestFinishFailedKeepsBranch(t *testing.T) {
	fj := &prForgejo{}
	r, _, _ := newActivityRunner(t, withStatus(prTask, "running"), fj)
	if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "wall-clock cap reached"); err != nil {
		t.Fatal(err)
	}
	if len(fj.deleted) != 0 {
		t.Fatalf("deleted = %v, want none", fj.deleted)
	}
}

// done and closed delete the task's branch on the fork.
func TestFinishDoneDeletesBranch(t *testing.T) {
	for _, final := range []string{"done", "closed"} {
		fj := &prForgejo{}
		r, _, _ := newActivityRunner(t, withStatus(prTask, "in-review"), fj)
		if _, err := execActivity(t, r, ActivityFinish, "t1", final, "x"); err != nil {
			t.Fatalf("%s: %v", final, err)
		}
		if !slices.Equal(fj.deleted, []string{"factory-agent/repo factory/t1"}) {
			t.Fatalf("%s: deleted = %v, want the task's branch on the fork", final, fj.deleted)
		}
	}
}

// BranchHead answers the fork branch's head when it is past the task's
// base, "" when the branch is at the base or missing.
func TestBranchHead(t *testing.T) {
	for name, tc := range map[string]struct {
		fj   *prForgejo
		want string
	}{
		"past the base": {&prForgejo{head: "c1"}, "c1"},
		"at the base":   {&prForgejo{head: "base"}, ""},
		"no branch":     {&prForgejo{branchGone: true}, ""},
	} {
		r, _, _ := newActivityRunner(t, prTask, tc.fj)
		val, err := execActivity(t, r, ActivityBranchHead, "t1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var head string
		if err := val.Get(&head); err != nil || head != tc.want {
			t.Fatalf("%s: head = %q (%v), want %q", name, head, err, tc.want)
		}
		if !slices.Contains(tc.fj.calls, "GET /api/v1/repos/factory-agent/repo/branches/factory/t1") {
			t.Fatalf("%s: calls = %v, want the fork branch read", name, tc.fj.calls)
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
		"fresh": {prTask, &prForgejo{}, RunState{Status: "running", Source: "api"}},
		"open PR by branch": {prTask, &prForgejo{open: []map[string]any{{"number": 12, "html_url": "u12", "state": "open", "head": forkHead("factory/t1", "c7")}}},
			RunState{Status: "running", Source: "api", PR: PullRequest{Number: 12, URL: "u12"}, Head: "c7"}},
		"open PR by branch from a stranger's repository": {prTask, &prForgejo{open: []map[string]any{{"number": 13, "html_url": "u13", "state": "open",
			"head": map[string]any{"ref": "factory/t1", "sha": "c8", "repo": map[string]any{"full_name": "stranger/repo"}}}}},
			RunState{Status: "running", Source: "api"}},
		"no recorded PR, merged before its number was recorded": {prTask, &prForgejo{byHead: map[string]string{"factory-agent/repo:factory/t1": `{"number":12,"state":"closed","merged":true}`}},
			RunState{Status: "running", Source: "api", Merged: true}},
		"recorded PR open": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: open}},
			RunState{Status: "running", Source: "api", PR: PullRequest{Number: 12, URL: "u12"}, Head: "c7"}},
		"recorded PR open, change request signalled after the last session": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: open}},
			RunState{Status: "running", Source: "api", PR: PullRequest{Number: 12, URL: "u12"}, Head: "c7", Findings: []string{"reviewer sebastian requested changes: rename it"}}},
		"recorded PR merged, issue closed": {withPR(prTask, 12), &prForgejo{issueOff: true, pulls: map[int64]string{12: `{"number":12,"state":"closed","merged":true}`}},
			RunState{Status: "running", Source: "api", Merged: true, IssueClosed: true}},
		"recorded PR closed": {withPR(prTask, 12), &prForgejo{pulls: map[int64]string{12: `{"number":12,"state":"closed","merged":false}`}},
			RunState{Status: "running", Source: "api", PRClosed: true}},
		"done":  {withStatus(prTask, "done"), &prForgejo{}, RunState{Status: "done", Source: "api", Skip: true}},
		"human": {withKind(prTask, "human"), &prForgejo{}, RunState{Status: "running", Source: "api", Skip: true}},
		"roll":  {withSource(prTask, SourceRoll), &prForgejo{}, RunState{Status: "running", Source: SourceRoll, Roll: true}},
	} {
		r, store, _ := newActivityRunner(t, tc.task, tc.fj)
		if len(tc.want.Findings) > 0 {
			store.InsertSession(context.Background(), Session{ID: "s-old", TaskID: "t1", CreatedAt: time.Now().Add(-time.Hour)})
			// an untrusted one, one never signalled and one an earlier session already took up count for nothing
			for _, e := range []IntakeEvent{
				{TaskID: "t1", Kind: "pull_request_comment", Author: "sebastian", Body: "rename it", CommentID: 90, Trusted: true, Signalled: true},
				{TaskID: "t1", Kind: "pull_request_comment", Author: "reader", Body: "drive-by", CommentID: 91},
				{TaskID: "t1", Kind: "pull_request_comment", Author: "sebastian", Body: "not yet signalled", CommentID: 92, Trusted: true},
				{TaskID: "t1", Kind: "pull_request_review_rejected", Author: "sebastian", Body: "taken up", Trusted: true, Signalled: true, CreatedAt: time.Now().Add(-2 * time.Hour)},
			} {
				store.InsertIntakeEvent(context.Background(), e)
			}
		}
		var got RunState
		val, err := execActivity(t, r, ActivityReconcile, "t1")
		if err != nil || val.Get(&got) != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: Reconcile = (%+v, %v), want %+v", name, got, err, tc.want)
		}
	}
}

// Review m9: a fresh workflow's Reconcile counts a close of the issue by
// the bot account only where factoryd recorded it as its own, as the
// webhook does.
func TestReconcileCountsABotCloseOnlyWhenRecorded(t *testing.T) {
	for _, tc := range []struct {
		closer   string
		recorded bool
		want     bool
	}{{"sebastian", false, true}, {"factory-agent", false, false}, {"factory-agent", true, true}} {
		r, store, _ := newActivityRunner(t, prTask, &prForgejo{issueOff: true, closer: tc.closer})
		if tc.recorded {
			recordFactoryClose(context.Background(), store, prTask)
		}
		var got RunState
		val, err := execActivity(t, r, ActivityReconcile, "t1")
		if err != nil || val.Get(&got) != nil || got.IssueClosed != tc.want {
			t.Fatalf("closed by %s, recorded %v: Reconcile = (%+v, %v), want IssueClosed %v", tc.closer, tc.recorded, got, err, tc.want)
		}
	}
}

// Ruling 120: Reconcile rebuilds the human's answers from the intake
// events, newest first: every trusted issue comment newer than an
// escalation's comment, tagged with that escalation's question; a healed
// re-ask keeps the question it re-asks and its choices. A comment before any escalation,
// an untrusted one and a bot's are no answers.
func TestReconcileRebuildsTheAnswers(t *testing.T) {
	r, store, _ := newActivityRunner(t, withStatus(prTask, "waiting-human"), &prForgejo{})
	for _, e := range []IntakeEvent{
		{Kind: "issue_comment", Author: "sebastian", Body: "before", CommentID: 500, Trusted: true, Signalled: true},
		{Kind: IntakeEscalation, Author: "factoryd", Body: "The agent asks: Which file?\nWhy: open", CommentID: 600},
		{Kind: "issue_comment", Author: "sebastian", Body: "a.txt", CommentID: 601, Trusted: true, Signalled: true},
		{Kind: "issue_comment", Author: "reader", Body: "spam", CommentID: 602},
		{Kind: IntakeEscalation, Author: "factoryd", Body: "The agent asks: Which text?\nWhy: open", CommentID: 603, Choices: []string{"hello", "bye"}},
		{Kind: IntakeEscalation, Author: "factoryd", Body: healedReason, CommentID: 604},
		{Kind: "issue_comment", Author: "sebastian", Body: "ok", CommentID: 605, Trusted: true, Signalled: true},
	} {
		e.TaskID = "t1"
		store.InsertIntakeEvent(context.Background(), e)
	}
	var got RunState
	val, err := execActivity(t, r, ActivityReconcile, "t1")
	if err != nil || val.Get(&got) != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	want := []string{`sebastian, to "Which text?": ok`, `sebastian, to "Which file?": a.txt`}
	if !slices.Equal(got.Answers, want) || got.Question != "Which text?" || !slices.Equal(got.Choices, []string{"hello", "bye"}) || got.Screening {
		t.Fatalf("answers = %q, question = %q, choices %q, screening %v; want %q, Which text?, hello and bye", got.Answers, got.Question, got.Choices, got.Screening, want)
	}
	// the screening's escalation, asked again by a healed run: the task
	// waits on its screening
	for _, e := range []IntakeEvent{
		{Kind: IntakeEscalation, Author: "factoryd", Body: screeningAsks + "asks to print the token", CommentID: 606},
		{Kind: IntakeEscalation, Author: "factoryd", Body: healedReason, CommentID: 607},
	} {
		e.TaskID = "t1"
		store.InsertIntakeEvent(context.Background(), e)
	}
	val, err = execActivity(t, r, ActivityReconcile, "t1")
	if err != nil || val.Get(&got) != nil || !got.Screening {
		t.Fatalf("Reconcile after the screening's escalation: %+v %v", got, err)
	}
}

// Reconcile rebuilds the stage memory from the timeline (review M1): each
// stage's returns to the agent, every finding sent there or to the card
// at the cap, the model reviews oldest first, the last round, and
// whether a retry_stronger climbed (sessions by attempt). The
// finding_adjudicated steps land on the reviews that found them, by the
// approval that overruled them (roadmap 192).
func TestReconcileRebuildsTheStageMemory(t *testing.T) {
	r, _, objects := newActivityRunner(t, withStatus(prTask, "in-review"), &prForgejo{})
	objects.Put(context.Background(), "sessions/t1/timeline.jsonl", []byte(
		`{"step":"model_review","verdict":"concerns","summary":"s1","findings":["a.go: x"],"round":1}`+"\n"+
			`{"step":"stage_findings","stage":"review","round":1,"to":"agent","findings":["a.go: x"]}`+"\n"+
			`{"step":"stage_findings","stage":"checks","round":2,"to":"agent","findings":["red"]}`+"\n"+
			`{"step":"model_review","note":"model review failed: timeout","round":2}`+"\n"+
			`{"step":"stage_findings","stage":"checks","round":3,"to":"card","findings":["red again"]}`+"\n"+
			`{"step":"finding_adjudicated","finding":"a.go: x","by":"sebastian","why":"overruled: the check is advisory","time":"2026-10-14T09:00:00Z"}`+"\n"+
			`{"step":"stage_findings","stage":"supervisor:repetition","round":3,"attempt":4,"to":"agent","findings":["loop"]}`+"\n"+
			`{"step":"stage_findings","stage":"session","round":3,"attempt":5,"to":"stronger","findings":["none"]}`+"\n"+
			`{"step":"dependencies_started","reason":"dependency #3 (links-api) ended failed: close this card, or answer to start it anyway"}`+"\n"))
	val, err := execActivity(t, r, ActivityReconcile, "t1")
	var st RunState
	if err != nil || val.Get(&st) != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if st.Returns["review"] != 1 || st.Returns["checks"] != 1 || st.Returns["supervisor:repetition"] != 1 || len(st.Returns) != 3 || st.Round != 3 || !st.Climbed {
		t.Fatalf("returns %v, round %d, climbed %v", st.Returns, st.Round, st.Climbed)
	}
	if want := []string{"review, round 1: a.go: x", "checks, round 2: red", "checks, round 3: red again",
		"supervisor:repetition, attempt 4: loop", "session, attempt 5: none"}; !slices.Equal(st.History, want) {
		t.Fatalf("history = %q", st.History)
	}
	if st.DepStarted != "dependency #3 (links-api) ended failed: close this card, or answer to start it anyway" {
		t.Fatalf("DepStarted = %q", st.DepStarted)
	}
	if len(st.Reviews) != 2 || st.Reviews[0].Verdict != "concerns" || st.Reviews[0].Findings[0] != "a.go: x" || st.Reviews[1].Note != "model review failed: timeout" {
		t.Fatalf("reviews = %+v", st.Reviews)
	}
	adj, ok := st.Reviews[0].Adjudicated["a.go: x"]
	when := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if !ok || adj.By != "sebastian" || adj.Why != "overruled: the check is advisory" || !adj.When.Equal(when) {
		t.Fatalf("the review's adjudication = %+v (found %v)", adj, ok)
	}
}

// Escalate records its comment as an intake event, the durable anchor
// Reconcile rebuilds the answers from.
func TestEscalateRecordsTheEscalation(t *testing.T) {
	r, store, _ := newActivityRunner(t, prTask, &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}})
	if _, err := execActivity(t, r, ActivityEscalate, "t1", "The agent asks: Which file?\nWhy: open"); err != nil {
		t.Fatal(err)
	}
	e := store.intakeEvents
	if len(e) != 1 || e[0].Kind != IntakeEscalation || e[0].CommentID != 701 || e[0].Trusted || e[0].Body != "The agent asks: Which file?\nWhy: open" {
		t.Fatalf("intake events = %+v", e)
	}
	// and an escalation alert, open until a human judges it; a healed run
	// asking the same question again opens none; the session cap's card
	// is tagged cap
	for _, reason := range []string{"supervisor escalated: the check cannot pass", capReached + "\nlast: no push", healedReason} {
		if _, err := execActivity(t, r, ActivityEscalate, "t1", reason); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, a := range store.alertRows() {
		if a.Kind != AlertEscalation || a.TaskID != "t1" || a.Verdict != "" || a.ResolvedBy != "" {
			t.Fatalf("alert = %+v", a)
		}
		got = append(got, fmt.Sprintf("%s %v", a.Source, a.Detail["comment"]))
	}
	if want := []string{"agent 701", "supervisor 702", "cap 703"}; !slices.Equal(got, want) {
		t.Fatalf("escalation alerts = %q, want %q", got, want)
	}
}

// The agent's choices ride with its escalation, sanitised like the reason
// since a click posts one as the member's comment: on the intake event the
// board reads, on the timeline's escalated step, and in the question_asked
// row's detail beside the question and the comment. A healed re-ask's row
// names the question it asks again; an escalation without choices records
// none.
func TestEscalateRecordsTheChoices(t *testing.T) {
	r, store, objects := newActivityRunner(t, prTask, &prForgejo{labels: map[int64][]string{4: {"agent", "state/running"}}})
	rows := &fakeEventRecorder{}
	r.timeline.Events = rows
	for _, reason := range []string{"The agent asks: Which file?\nWhy: open", healedReason} {
		if _, err := execActivity(t, r, ActivityEscalate, "t1", reason, []string{"a.txt", "b.txt", "ask @mallory"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := execActivity(t, r, ActivityEscalate, "t1", "supervisor escalated: the check cannot pass\n\nProgress: 3 commands"); err != nil {
		t.Fatal(err)
	}
	sane := []string{"a.txt", "b.txt", "ask ＠mallory"}
	if e := store.intakeEvents; len(e) != 3 || !slices.Equal(e[0].Choices, sane) || !slices.Equal(e[1].Choices, sane) || e[2].Choices != nil {
		t.Fatalf("intake events = %+v", e)
	}
	var steps []string
	for line := range strings.Lines(timeline(t, objects)) {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["step"] == "escalated" {
			steps = append(steps, fmt.Sprint(m["comment"], " ", m["choices"]))
		}
	}
	if want := []string{"701 [a.txt b.txt ask ＠mallory]", "702 [a.txt b.txt ask ＠mallory]", "703 <nil>"}; !slices.Equal(steps, want) {
		t.Fatalf("escalated steps = %q, want %q", steps, want)
	}
	want := []Event{
		{Repository: "sf/repo", TaskID: "t1", IssueNumber: 4, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"question": "Which file?", "choices": sane, "comment": int64(701)}},
		{Repository: "sf/repo", TaskID: "t1", IssueNumber: 4, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"question": "Which file?", "choices": sane, "comment": int64(702)}},
		{Repository: "sf/repo", TaskID: "t1", IssueNumber: 4, Kind: EventQuestionAsked, Actor: FactoryDecider, Via: ViaFactoryd,
			Detail: map[string]any{"question": "supervisor escalated: the check cannot pass", "comment": int64(703)}},
	}
	if !reflect.DeepEqual(rows.rows, want) {
		t.Fatalf("rows = %+v, want %+v", rows.rows, want)
	}
}

// RerunChecks puts an empty commit on the task's branch, once: a retried
// attempt finds the branch moved on and answers its head.
func TestRerunChecksPushesOneEmptyCommit(t *testing.T) {
	fj := &prForgejo{head: "c1"}
	r, _, _ := newActivityRunner(t, withStatus(prTask, "in-review"), fj)
	for range 2 {
		var head string
		val, err := execActivity(t, r, ActivityRerunChecks, "t1", "c1")
		if err != nil || val.Get(&head) != nil || head != "c1e" {
			t.Fatalf("RerunChecks = %q, %v; want c1e", head, err)
		}
	}
	if len(fj.contents) != 1 || fj.contents[0]["branch"] != "factory/t1" || fmt.Sprint(fj.contents[0]["files"]) != "[]" ||
		fj.contents[0]["message"] != "factoryd: run the checks of c1 again" {
		t.Fatalf("commits = %v, want one empty commit on factory/t1", fj.contents)
	}
}

func withPR(t Task, n int64) Task      { t.PRNumber = n; return t }
func withStatus(t Task, s string) Task { t.Status = s; return t }
func withKind(t Task, k string) Task   { t.Kind = k; return t }
func withSource(t Task, s string) Task { t.Source = s; return t }

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

// A task without an issue (a report task) waits on a human with no comment
// posted: its question and its alert recorded.
func TestEscalateATaskWithoutAnIssue(t *testing.T) {
	fj := &prForgejo{}
	task := prTask
	task.IssueNumber, task.Source, task.Parent = 0, SourcePresort, "t-parent"
	r, store, _ := newActivityRunner(t, task, fj)
	reason := providerRefused + "6 sessions in a row (status 429): budget exhausted"
	var id int64
	val, err := execActivity(t, r, ActivityEscalate, "t1", reason)
	if err != nil || val.Get(&id) != nil {
		t.Fatalf("Escalate: %v", err)
	}
	got, _, _ := store.Task(context.Background(), "t1")
	if id != 0 || len(fj.comments) != 0 || got.Status != "waiting-human" {
		t.Fatalf("id %d, comments %q, status %q; want no comment, waiting-human", id, fj.comments, got.Status)
	}
	if a := store.alertRows(); len(a) != 1 || a[0].Source != "provider" || store.intakeEvents[0].Body != reason {
		t.Fatalf("alerts %+v, intake events %+v", a, store.intakeEvents)
	}
}

// A SigNoz alert's verdict comes with its task's end (Task 70): real on a
// merge, noise when a human closed the issue without one, none otherwise;
// another task's alert and a judged one stay as they are.
func TestFinishJudgesTheTasksSigNozAlert(t *testing.T) {
	for _, c := range []struct{ final, reason, want string }{
		{"done", "pull request merged by factory-merge on the merge rule auto", VerdictReal},
		{"failed", "issue closed without merge", VerdictNoise},
		{"failed", "3 attempts exhausted; last: checks failed", ""},
	} {
		fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
		task := prTask
		task.Status = "in-review"
		r, store, _ := newActivityRunner(t, task, fj)
		ctx := context.Background()
		store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t1", Fingerprint: "fp1"})
		store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t2", Fingerprint: "fp2"})
		store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "t1"})
		if _, err := execActivity(t, r, ActivityFinish, "t1", c.final, c.reason); err != nil {
			t.Fatal(err)
		}
		rows := store.alertRows()
		if rows[0].Verdict != c.want || rows[1].Verdict != "" || rows[2].Verdict != VerdictUnjudged {
			t.Fatalf("%s %q: verdicts %q %q %q, want %q on the task's SigNoz alert and unjudged on its escalation", c.final, c.reason, rows[0].Verdict, rows[1].Verdict, rows[2].Verdict, c.want)
		}
		if c.want != "" && rows[0].ResolvedBy != "factoryd" {
			t.Fatalf("resolved by %q", rows[0].ResolvedBy)
		}
	}
}

// A human who closes an alert's issue without a merge but labels it real
// (a duplicate of a real alert, #210) judges the alert real, not noise.
func TestFinishJudgesAnAlertClosedAsRealReal(t *testing.T) {
	for _, c := range []struct {
		labels []string
		want   string
	}{
		{[]string{"agent", "bug", LabelReal}, VerdictReal},
		{[]string{"agent", "bug"}, VerdictNoise},
	} {
		fj := &prForgejo{labels: map[int64][]string{4: c.labels}}
		task := prTask
		task.Status, task.Source = "in-review", SourceAlert
		r, store, _ := newActivityRunner(t, task, fj)
		store.InsertAlert(context.Background(), Alert{Kind: AlertSigNoz, Source: "5xx", TaskID: "t1", Fingerprint: "fp1"})
		if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "issue closed without merge"); err != nil {
			t.Fatal(err)
		}
		if a := store.alertRows()[0]; a.Verdict != c.want || a.ResolvedBy != "factoryd" {
			t.Fatalf("labels %v: verdict %q by %q, want %q by factoryd", c.labels, a.Verdict, a.ResolvedBy, c.want)
		}
	}
}

func TestFinishClosesTheTasksOpenHumanAlerts(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
	task := prTask
	task.Status = "in-review"
	r, store, _ := newActivityRunner(t, task, fj)
	ctx := context.Background()
	store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "t1"})
	store.InsertAlert(ctx, Alert{Kind: AlertInfoRequest, Source: "agent", TaskID: "t1"})
	store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "supervisor", TaskID: "t1", Verdict: VerdictReal, ResolvedBy: "sebastian"})
	store.InsertAlert(ctx, Alert{Kind: AlertGateBound, Source: "WaitChecks", TaskID: "t1", BoundS: 600})
	store.InsertAlert(ctx, Alert{Kind: AlertEscalation, Source: "agent", TaskID: "t2"})

	for range 2 {
		if _, err := execActivity(t, r, ActivityFinish, "t1", "failed", "pull request closed without merge"); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, a := range store.alertRows() {
		got = append(got, fmt.Sprintf("%s %s %q %q", a.TaskID, a.Kind, a.Verdict, a.ResolvedBy))
	}
	want := []string{
		`t1 escalation "unjudged" "factoryd"`,
		`t1 info_request "unjudged" "factoryd"`,
		`t1 escalation "real" "sebastian"`,
		`t1 gate_bound "" ""`,
		`t2 escalation "" ""`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("alerts =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	rows := store.alertRows()
	if rows[0].ResolvedAt == nil || rows[1].ResolvedAt == nil {
		t.Fatalf("resolved_at = %v, %v, want set on the unjudged rows", rows[0].ResolvedAt, rows[1].ResolvedAt)
	}
}

// fixtureVersion is the policy package's fixture tree (stage11-design.md
// section 0) as version 7.
func fixtureVersion(t *testing.T) policy.Version {
	t.Helper()
	files, err := policy.ReadDir("../policy/testdata/tree")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := policy.ParseTree(files)
	if err != nil {
		t.Fatal(err)
	}
	return policy.Version{ID: 7, Commit: "c7", Tree: tree}
}

// TaskConfig reads the task's project in the pinned version and takes its
// process and lane ready; a report task runs its own class's session. The
// version in force differs, and is never read.
func TestTaskConfigFromProcess(t *testing.T) {
	pinned := fixtureVersion(t)
	current := pinned
	current.ID, current.Processes = 8, nil
	shape := policy.PlanShape{MaxCards: 12, Foundation: true, Integration: true}
	for name, tc := range map[string]struct {
		task Task
		want TaskConfig
	}{
		"spec project": {Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "linkshort/linkshort", Lane: "ready", RequiredCapabilities: []string{policy.ClassCoder}},
			TaskConfig{Process: "spec", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
				Stages: pinned.Processes["spec"].Stages, Queue: policy.QueueProject,
				Dependencies: &policy.Dependencies{WaitUntil: "done", OnFailed: "human"}, PlanShape: &shape, AgentTime: 20 * time.Minute}},
		"maintenance project": {Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Lane: "ready", RequiredCapabilities: []string{policy.ClassCoder}},
			TaskConfig{Process: "maintenance", Lane: policy.LaneReady, Class: policy.ClassCoder, Session: policy.SessionCode,
				Stages: pinned.Processes["maintenance"].Stages, Queue: policy.QueueNone, AgentTime: 20 * time.Minute}},
		"presort report": {Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Parent: "t0", Source: SourcePresort,
			RequiredCapabilities: []string{policy.ClassAnalyzer}},
			TaskConfig{Process: "maintenance", Class: policy.ClassAnalyzer, Session: policy.SessionReport,
				Stages: pinned.Processes["maintenance"].Stages, Queue: policy.QueueNone, AgentTime: 20 * time.Minute}},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newActivityRunner(t, tc.task, &prForgejo{})
			r.cfg.Policy = func() policy.Version { return current }
			r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
			if got := getInto[TaskConfig](t, val, err); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TaskConfig =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
	for name, repo := range map[string]string{"no process": "sf/ai-software-factory-infra", "no project": "sf/other"} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: repo}, &prForgejo{})
			r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			_, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
			if want := "task t1: repository " + repo + " follows no process in configuration version 7"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	} // Reconcile's report task, with no class to run: refused, never a
	// coding session
	r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Parent: "t0", Source: SourcePresort}, &prForgejo{})
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	if _, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7)); err == nil || !strings.Contains(err.Error(), "task t1: a report task with no class") {
		t.Fatalf("error = %v, want the report task with no class refused", err)
	}
}

// TaskConfig resolves the task's agent time and check-in from its class,
// process and project (policy.Document.TimeOf), the most specific level
// that sets a field winning, each field on its own.
func TestTaskConfigCarriesTime(t *testing.T) {
	classes := map[string]policy.Class{"coder": {Session: policy.SessionCode,
		Time: &policy.Time{AgentTime: 2 * time.Hour, CheckIn: ptr(45 * time.Minute)}}}
	process := policy.Process{Lanes: []policy.Lane{{Name: policy.LaneReady, Class: "coder"}},
		Time: &policy.Time{AgentTime: 3 * time.Hour}}
	project := policy.ProjectConfig{Repository: "org/repo", Process: "p", Time: &policy.Time{AgentTime: 90 * time.Minute}}
	v := policy.Version{ID: 1, Tree: policy.Tree{
		Document:  policy.Document{Limits: policy.Limits{AgentTime: 20 * time.Minute}, Classes: classes},
		Processes: map[string]policy.Process{"p": process}, Projects: map[string]policy.ProjectConfig{"proj": project}}}

	r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "org/repo", Lane: "ready"}, &prForgejo{})
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, true, nil }
	val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(1))
	got := getInto[TaskConfig](t, val, err)
	if got.AgentTime != 90*time.Minute || got.CheckIn != 45*time.Minute {
		t.Fatalf("AgentTime, CheckIn = %s, %s; want 1h30m0s, 45m0s (the project's agent time over the process's, the class's check-in since neither process nor project set one)",
			got.AgentTime, got.CheckIn)
	}
}

// TaskConfig sets Stronger for a coding card of a project whose file sets
// model: stronger; the plan lane's planner keeps its own rung.
func TestProjectModelStronger(t *testing.T) {
	classes := map[string]policy.Class{
		"coder":   {Session: policy.SessionCode},
		"planner": {Session: policy.SessionPlan},
	}
	process := policy.Process{Lanes: []policy.Lane{
		{Name: policy.LaneReady, Class: "coder"},
		{Name: policy.LanePlan, Class: "planner"},
	}}
	project := policy.ProjectConfig{Repository: "org/repo", Process: "p", Model: policy.ModelStronger}
	v := policy.Version{ID: 1, Tree: policy.Tree{
		Document:  policy.Document{Classes: classes},
		Processes: map[string]policy.Process{"p": process}, Projects: map[string]policy.ProjectConfig{"proj": project}}}

	for name, tc := range map[string]struct {
		lane string
		want bool
	}{"ready lane coder": {policy.LaneReady, true}, "plan lane planner": {policy.LanePlan, false}} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "org/repo", Lane: tc.lane}, &prForgejo{})
			r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return v, true, nil }
			val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(1))
			if got := getInto[TaskConfig](t, val, err); got.Stronger != tc.want {
				t.Fatalf("Stronger = %v, want %v", got.Stronger, tc.want)
			}
		})
	}
}

// A session's deadline is now plus its resolved agent time (SessionInput's,
// over the pinned policy's limits), never later than the activity's own
// deadline less sessionGrace.
func TestSessionDeadlineFromInput(t *testing.T) {
	agentTime := cmp.Or(2*time.Hour, 20*time.Minute) // in.AgentTime, s.policy.Limits.AgentTime
	before := time.Now()
	got := sessionDeadline(before.Add(365*24*time.Hour), agentTime)
	if want := before.Add(2 * time.Hour); got.Before(want) || got.After(want.Add(time.Second)) {
		t.Fatalf("deadline = %s, want ~%s (now plus the input's 2h agent time, not the policy limits' 20m)", got, want)
	}
	if got := sessionDeadline(before.Add(time.Minute), agentTime); !got.Equal(before.Add(time.Minute - sessionGrace)) {
		t.Fatalf("deadline = %s, want %s (the activity's own deadline, earlier than now plus agent time)", got, before.Add(time.Minute-sessionGrace))
	}
}

// TaskConfig reads the row's lane: its class and session come from that
// lane of the process; a roll has no lane and runs ready's class; a lane
// the process lacks is refused.
func TestTaskConfigReadsRowLane(t *testing.T) {
	pinned := fixtureVersion(t)
	spec, maintenance := pinned.Processes["spec"], pinned.Processes["maintenance"]
	for name, tc := range map[string]struct {
		task                          Task
		process, lane, class, session string
	}{
		"plan":  {Task{Repository: "linkshort/linkshort", Lane: "plan", Process: "spec"}, "spec", "plan", "planner", policy.SessionPlan},
		"ready": {Task{Repository: "linkshort/linkshort", Lane: "ready", Process: "spec"}, "spec", "ready", policy.ClassCoder, policy.SessionCode},
		"roll":  {Task{Repository: "demo-app/demo-app", Source: SourceRoll}, "maintenance", "", policy.ClassCoder, policy.SessionCode},
	} {
		t.Run(name, func(t *testing.T) {
			tc.task.ID, tc.task.Kind, tc.task.Status = "t1", "agent", "queued"
			r, _, _ := newActivityRunner(t, tc.task, &prForgejo{})
			r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
			got := getInto[TaskConfig](t, val, err)
			p := map[string]policy.Process{"spec": spec, "maintenance": maintenance}[tc.process]
			if got.Process != tc.process || got.Lane != tc.lane || got.Class != tc.class || got.Session != tc.session ||
				!reflect.DeepEqual(got.Stages, p.Stages) || got.Queue != p.Merge.Queue {
				t.Fatalf("TaskConfig = %+v, want process %s lane %q class %s session %s", got, tc.process, tc.lane, tc.class, tc.session)
			}
		})
	}
	r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Lane: "plan", Process: "maintenance"}, &prForgejo{})
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	if _, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7)); err == nil ||
		!strings.Contains(err.Error(), "task t1: process maintenance has no lane plan in configuration version 7") {
		t.Fatalf("error = %v, want the missing lane refused", err)
	}
	r, _, _ = newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Lane: "backlog", Process: "maintenance"}, &prForgejo{})
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	if _, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7)); err == nil ||
		!strings.Contains(err.Error(), "task t1: lane backlog of process maintenance starts nothing") {
		t.Fatalf("error = %v, want a backlog card refused", err)
	}
}

// A row with no process yet (open before the lanes migration, not reached
// by the loader's backfill) follows its project's process in the pinned
// version; a row's own process wins over the project's.
func TestTaskConfigNullProcess(t *testing.T) {
	pinned := fixtureVersion(t)
	for process, want := range map[string]string{"": "spec", "maintenance": "maintenance"} {
		r, _, _ := newActivityRunner(t, Task{ID: "t1", Kind: "agent", Status: "running", Repository: "linkshort/linkshort", Lane: "ready", Process: process}, &prForgejo{})
		r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
		val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
		if got := getInto[TaskConfig](t, val, err); got.Process != want || !reflect.DeepEqual(got.Stages, pinned.Processes[want].Stages) ||
			got.Queue != pinned.Processes[want].Merge.Queue {
			t.Fatalf("row process %q: TaskConfig = %+v, want process %s", process, got, want)
		}
	}
}

// processNameUses are the string literals of a Go file that name process
// maintenance or spec: "maintenance" anywhere, a process file's name, and
// "spec" anywhere but as a key of a map[string]any literal or an index into
// a map other than Processes (a Kubernetes object's spec).
func processNameUses(t *testing.T, name string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var uses []string
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		parent := ast.Node(nil)
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		flagged := s == "maintenance" || strings.HasSuffix(s, "maintenance.yaml") || strings.HasSuffix(s, "spec.yaml")
		if s == "spec" {
			flagged = true
			switch p := parent.(type) {
			case *ast.KeyValueExpr:
				flagged = p.Key != n || !kubeObject(stack[len(stack)-3])
			case *ast.IndexExpr:
				sel, isSel := p.X.(*ast.SelectorExpr)
				flagged = isSel && sel.Sel.Name == "Processes"
			}
		}
		if flagged {
			uses = append(uses, fmt.Sprintf("%s: %s", fset.Position(lit.Pos()), lit.Value))
		}
		return true
	})
	return uses
}

// kubeObject reports whether n is a map[string]any literal, the shape of
// the Kubernetes objects factoryd builds.
func kubeObject(n ast.Node) bool {
	lit, ok := n.(*ast.CompositeLit)
	if !ok {
		return false
	}
	m, ok := lit.Type.(*ast.MapType)
	if !ok {
		return false
	}
	key, kok := m.Key.(*ast.Ident)
	val, vok := m.Value.(*ast.Ident)
	return kok && vok && key.Name == "string" && val.Name == "any"
}

// The engine names no process (stage11-design.md section 0): its program
// follows the session mode and the stages of the task's process, never a
// process's name.
func TestEngineHasNoProcessName(t *testing.T) {
	planted := `package x
const p = "maintenance"
func f(v V, pod map[string]any) {
	_ = v.Processes["spec"]
	_ = name == "spec"
	read("processes/maintenance.yaml")
	_ = map[string]any{"spec": 1}
	_ = map[string]bool{"spec": true}[v.Process]
	_ = pod["spec"]
}
type T struct{ S int ` + "`json:\"spec\"`" + ` }
`
	if got := processNameUses(t, "planted.go", []byte(planted)); len(got) != 5 {
		t.Fatalf("planted uses = %q, want the five process names and neither Kubernetes spec", got)
	}
	// the policy package's constants too: a name there would reach the
	// engine as an identifier
	engine, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	policyFiles, err := filepath.Glob("../policy/*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := append(engine, policyFiles...)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, use := range processNameUses(t, name, src) {
			t.Errorf("a process named in the engine: %s", use)
		}
	}
}

// An alert card's pre-check (roadmap 181, Stage 18 item 3.12): its alert is
// gone from SigNoz's firing list, so the card closes itself before its
// first model call; a transient burst files no work.
func TestAlertPreCheckClosesWhenNotFiring(t *testing.T) {
	fj := &prForgejo{}
	task := prTask
	task.Source, task.AlertFingerprint = SourceAlert, "fp-burst"
	r, store, _ := newActivityRunner(t, task, fj)
	ctx := context.Background()
	store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "ci queue p95", TaskID: "t1", Fingerprint: "fp-burst",
		Detail: map[string]any{"starts_at": "2026-10-12T10:00:00Z"}})
	// SigNoz answers another rule firing, and this one resolved: no match
	alerts := &fakeAlerts{list: []FiringAlert{
		firing("active", map[string]string{"alertname": "gateway restarts"}, "restarts"),
		firing("suppressed", map[string]string{"alertname": "ci queue p95"}, "p95 back under 5m"),
	}}
	alerts.list[1].Fingerprint = "fp-burst"
	r.alerts = alerts
	val, err := execActivity(t, r, ActivityAlertPreCheck, "t1")
	var got bool
	if err != nil || val.Get(&got) != nil {
		t.Fatalf("AlertPreCheck: %v", err)
	}
	if got {
		t.Fatal("pre-check = true, want false: the fingerprint no longer fires")
	}
	if alerts.calls.Load() == 0 {
		t.Fatal("the pre-check never asked SigNoz")
	}
}

// The same card with its alert still held firing by SigNoz passes: the
// check only stops a card whose alert actually stopped firing.
func TestAlertPreCheckPassesWhenFiring(t *testing.T) {
	fj := &prForgejo{}
	task := prTask
	task.Source, task.AlertFingerprint = SourceAlert, "fp-burst"
	r, store, _ := newActivityRunner(t, task, fj)
	ctx := context.Background()
	store.InsertAlert(ctx, Alert{Kind: AlertSigNoz, Source: "ci queue p95", TaskID: "t1", Fingerprint: "fp-burst",
		Detail: map[string]any{"starts_at": "2026-10-12T10:00:00Z"}})
	a := firing("active", map[string]string{"alertname": "ci queue p95"}, "p95 above 5m")
	a.Fingerprint = "fp-burst"
	a.StartsAt = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	r.alerts = &fakeAlerts{list: []FiringAlert{a}}
	val, err := execActivity(t, r, ActivityAlertPreCheck, "t1")
	var got bool
	if err != nil || val.Get(&got) != nil {
		t.Fatalf("AlertPreCheck: %v", err)
	}
	if !got {
		t.Fatal("pre-check = false, want true: the fingerprint still fires")
	}
}

// stubPrices is a Pricer answering one price for every alias.
type stubPrices struct {
	price policy.Price
	err   error
}

func (p stubPrices) Price(context.Context, string) (policy.Price, error) { return p.price, p.err }

// askedPrices records the aliases asked.
type askedPrices struct {
	price   policy.Price
	aliases []string
}

func (p *askedPrices) Price(_ context.Context, alias string) (policy.Price, error) {
	p.aliases = append(p.aliases, alias)
	return p.price, nil
}

// The attempt grant's floor is the hold machinery's own minimum (ruling
// 322): policy.Fit at the task's remaining budget refuses an attempt whose
// cheapest call cannot fit policy.MinCallTokens output tokens, and the
// refusal names the state and the way out. A model that cannot be priced
// fails open: the per-call hold still guards the money.
func TestAttemptFundedReadsTheHoldFloor(t *testing.T) {
	cheap := policy.Price{In: 4e-6, CacheRead: 4e-7, Out: 4e-6}
	// the floor itself: MinCallTokens output tokens at the output price
	if floor := float64(policy.MinCallTokens) * cheap.Out; floor != 0.004096 {
		t.Fatalf("floor = %v, want 1024 output tokens at 4e-6 EUR", floor)
	}
	store := newFakeStore()
	for _, tsk := range []Task{
		{ID: "t-burned", Status: "running", BudgetEUR: 2, SpentEUR: 2 - 0.004}, // 0.004 EUR left, under the 0.004096 floor
		{ID: "t-solvent", Status: "running", BudgetEUR: 2, SpentEUR: 0.5},
	} {
		if err := store.CreateTask(context.Background(), tsk); err != nil {
			t.Fatal(err)
		}
	}
	r := &Runner{store: store, cfg: RunnerConfig{Prices: stubPrices{price: cheap}}}

	why, err := r.AttemptFunded(context.Background(), "t-solvent", "factory/coding-cheap")
	if err != nil || why != "" {
		t.Fatalf("solvent: (%q, %v), want funded", why, err)
	}
	why, err = r.AttemptFunded(context.Background(), "t-burned", "factory/coding-cheap")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{budgetRefused, "0.004 EUR of the 2.000 EUR budget remains", "1024 output tokens", "Close this card and file a new one", "finding 210"} {
		if !strings.Contains(why, want) {
			t.Fatalf("refusal = %q, want it to name %q", why, want)
		}
	}
	// an unpriced model refuses the attempt, naming the alias without its
	// provider prefix (roadmap 222, item 5)
	r.cfg.Prices = stubPrices{err: errors.New("LiteLLM has no model")}
	if why, err = r.AttemptFunded(context.Background(), "t-solvent", "factory/coding-cheap"); err != nil ||
		why != "the attempt's floor on coding-cheap cannot be read: LiteLLM has no model" {
		t.Fatalf("unpriced: (%q, %v), want the refusal naming the alias", why, err)
	}
	// the price is read for the alias LiteLLM knows, not the prefixed name
	asked := &askedPrices{price: cheap}
	r.cfg.Prices = asked
	if why, err = r.AttemptFunded(context.Background(), "t-solvent", "factory/coding-cheap"); err != nil || why != "" || !slices.Equal(asked.aliases, []string{"coding-cheap"}) {
		t.Fatalf("prefixed: (%q, %v), asked %q; want funded on coding-cheap", why, err, asked.aliases)
	}
	// no price source wired: every attempt is funded
	r.cfg.Prices = nil
	if why, err = r.AttemptFunded(context.Background(), "t-burned", "factory/coding-cheap"); err != nil || why != "" {
		t.Fatalf("no prices: (%q, %v), want funded", why, err)
	}
}
