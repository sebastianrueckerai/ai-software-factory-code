package factoryd

import (
	"cmp"
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
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/harness"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// gateForgejo plays the pull request files, diff, merge, state and
// comment endpoints of sf/repo#9.
type gateForgejo struct {
	mu          sync.Mutex
	files       []string // ChangedFile JSON objects
	diff        string
	mergeStatus []int // answers in order, the last repeating
	merges      []map[string]any
	merged      bool   // GET /pulls/9 reports merged
	mergedBy    string // and by whom; "" = factory-merge
	head        string // GET /pulls/9's head sha; "" = c1
	base        string // and its base; "" = main
	stateFail   int    // GET /pulls/9 answers 502 this many times first
	diffDelay   time.Duration
	comments    []string
}

func (f *gateForgejo) srv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch p := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/sf/repo"); {
		case r.Method == "GET" && p == "/pulls/9/files":
			servePage(w, r, 2, f.files...)
		case r.Method == "GET" && p == "/pulls/9.diff":
			time.Sleep(f.diffDelay)
			io.WriteString(w, f.diff)
		case r.Method == "POST" && p == "/pulls/9/merge":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.merges = append(f.merges, body)
			status := f.mergeStatus[min(len(f.merges), len(f.mergeStatus))-1]
			if status == http.StatusOK {
				f.merged = true
			}
			w.WriteHeader(status)
			io.WriteString(w, `{"message":"not allowed to merge"}`)
		case r.Method == "GET" && p == "/pulls/9":
			if f.stateFail > 0 {
				f.stateFail--
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			pr := map[string]any{"number": 9, "state": map[bool]string{true: "closed", false: "open"}[f.merged], "merged": f.merged,
				"head": map[string]any{"sha": cmp.Or(f.head, "c1")}, "base": map[string]any{"ref": cmp.Or(f.base, "main")}}
			if f.merged {
				pr["merged_by"] = map[string]any{"login": cmp.Or(f.mergedBy, "factory-merge")}
			}
			json.NewEncoder(w).Encode(pr)
		case r.Method == "POST" && p == "/issues/9/comments":
			var body struct{ Body string }
			json.NewDecoder(r.Body).Decode(&body)
			f.comments = append(f.comments, body.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%d}`, 800+len(f.comments))
		default:
			t.Errorf("unexpected forgejo call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

var gateTask = Task{ID: "t1", Status: "in-review", Objective: "Add a make check target", Repository: "sf/repo",
	SHA: "base", IssueNumber: 4, Source: "api", PRNumber: 9, Kind: "agent"}

func newGateRunner(t *testing.T, task Task, fj *gateForgejo) (*Runner, *fakeStore, *fakeObjectStore) {
	t.Helper()
	r, store, objects := newActivityRunner(t, task, &prForgejo{})
	r.forgejo = &Forgejo{BaseURL: fj.srv(t).URL, BotUser: "factory-agent", BotPassword: "bot-pw", MergeToken: "merge-tok"}
	return r, store, objects
}

func timeline(t *testing.T, objects *fakeObjectStore) string {
	t.Helper()
	b, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl")
	return string(b)
}

func getInto[T any](t *testing.T, val converter.EncodedValue, err error) T {
	t.Helper()
	var out T
	if err != nil {
		t.Fatal(err)
	}
	if err := val.Get(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGatePolicyClassifiesAndRecords(t *testing.T) {
	fj := &gateForgejo{files: []string{
		`{"filename":"internal/factoryd/store.go"}`,
		`{"filename":"internal/factoryd/migrations/0016_x.sql"}`,
		`{"filename":"k8s/factoryd.yaml"}`,
	}}
	r, store, objects := newGateRunner(t, gateTask, fj)
	val, err := execActivity(t, r, ActivityGatePolicy, "t1", int64(9), "c1")
	g := getInto[PolicyGate](t, val, err)
	want := PolicyGate{Class: "migration", Outcome: policy.OutcomeRequiresApproval, Paths: []string{"internal/factoryd/migrations/0016_x.sql"},
		Migrations: []string{"internal/factoryd/migrations/0016_x.sql"}, Changed: 3, PolicyVersion: 7}
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Fatalf("gate = %+v, want %+v", g, want)
	}
	if got, _, _ := store.Task(context.Background(), "t1"); got.RiskClass != "migration" {
		t.Fatalf("risk class = %q", got.RiskClass)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "policy version 7") || !strings.Contains(fj.comments[0], "**migration**") {
		t.Fatalf("comments = %q", fj.comments)
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"step":"gate_policy"`) || !strings.Contains(tl, `"class":"migration"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// A rename out of .forgejo counts under its old name: forbidden, and the
// comment names the path.
func TestGatePolicyForbidsARenameOutOfAWorkflowDir(t *testing.T) {
	fj := &gateForgejo{files: []string{`{"filename":"ci.yml","previous_filename":".forgejo/workflows/ci.yml","status":"renamed"}`}}
	r, _, _ := newGateRunner(t, gateTask, fj)
	val, err := execActivity(t, r, ActivityGatePolicy, "t1", int64(9), "c1")
	g := getInto[PolicyGate](t, val, err)
	if g.Outcome != policy.OutcomeForbidden || !slices.Equal(g.Paths, []string{".forgejo/workflows/ci.yml"}) {
		t.Fatalf("gate = %+v", g)
	}
	if !strings.Contains(fj.comments[0], "Agents may not change these paths") || !strings.Contains(fj.comments[0], "`.forgejo/workflows/ci.yml`") {
		t.Fatalf("comment = %q", fj.comments[0])
	}
}

func TestGateMigrationComments(t *testing.T) {
	fj := &gateForgejo{}
	r, _, objects := newGateRunner(t, gateTask, fj)
	if _, err := execActivity(t, r, ActivityGateMigration, "t1", int64(9), []string{"internal/factoryd/migrations/0016_x.sql"}); err != nil {
		t.Fatal(err)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "schema migration: human review required") || !strings.Contains(fj.comments[0], "0016_x.sql") {
		t.Fatalf("comments = %q", fj.comments)
	}
	if !strings.Contains(timeline(t, objects), `"step":"gate_migration"`) {
		t.Fatal("no gate_migration step")
	}
}

func TestParseReview(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want Review
		note string
	}{
		"approve":          {`{"verdict":"approve","summary":"fine","findings":[]}`, Review{Verdict: "approve", Summary: "fine"}, ""},
		"concerns, fenced": {"```json\n{\"verdict\":\"concerns\",\"summary\":\"s\",\"findings\":[\"a.go: x\",\" \"]}\n```", Review{Verdict: "concerns", Summary: "s", Findings: []string{"a.go: x"}}, ""},
		"not json":         {"looks good to me", Review{}, "model review answer invalid"},
		"unknown verdict":  {`{"verdict":"merge","summary":"s"}`, Review{}, `unknown verdict "merge"`},
		"extra field":      {`{"verdict":"approve","summary":"s","merge":true}`, Review{}, `unknown field "merge"`},
		"no summary":       {`{"verdict":"approve","summary":" "}`, Review{}, "no summary"},
		"trailing text":    {`{"verdict":"approve","summary":"s"} ok`, Review{}, "text after the JSON object"},
		"a note smuggled":  {`{"verdict":"approve","summary":"s","note":"n"}`, Review{}, "note is not an answer field"},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseReview(tc.in)
			if tc.note != "" {
				if got.Verdict != "" || !strings.Contains(got.Note, tc.note) {
					t.Fatalf("parseReview = %+v, want a note containing %q", got, tc.note)
				}
				return
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("parseReview = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The review is one metered call of the review alias on the task's
// account, over the brief, the capped diff and the gates, posted on the
// pull request.
func TestModelReviewPostsTheVerdict(t *testing.T) {
	fj := &gateForgejo{diff: "diff --git a/Makefile b/Makefile\n+check:\n"}
	r, _, objects := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"concerns","summary":"the target skips vet","findings":["Makefile: check does not run go vet"]}`, cost: "0.03"}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	gates := GateResults{Policy: PolicyGate{Class: "normal", Outcome: policy.OutcomeRequiresApproval}}
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", gates)
	rv := getInto[Review](t, val, err)
	if rv.Verdict != "concerns" || rv.Findings[0] != "Makefile: check does not run go vet" {
		t.Fatalf("review = %+v", rv)
	}
	if len(ll.chats) != 1 || ll.chats[0]["model"] != "review-frontier" {
		t.Fatalf("chats = %v", ll.chats)
	}
	meta, _ := ll.mints[0]["metadata"].(map[string]any)
	if meta["task_id"] != "t1" || meta["purpose"] != "review" {
		t.Fatalf("mint = %v", ll.mints[0])
	}
	msgs, _ := ll.chats[0]["messages"].([]any)
	user, _ := msgs[1].(map[string]any)["content"].(string)
	for _, want := range []string{"Add a make check target", "+check:", `"Class": "normal"`} {
		if !strings.Contains(user, want) {
			t.Fatalf("review input lacks %q: %s", want, user)
		}
	}
	meter := r.supervisor.Meter.(*memPolicy)
	if len(meter.released) != 1 || *meter.released[0].cost != 0.03 || meter.released[0].hold.Ref != "review-frontier" {
		t.Fatalf("released = %+v, want the review booked at 0.03", meter.released)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "Verdict: **concerns**") || !strings.Contains(fj.comments[0], "not a decision") {
		t.Fatalf("comments = %q", fj.comments)
	}
	if !strings.Contains(timeline(t, objects), `"step":"model_review"`) {
		t.Fatal("no model_review step")
	}
}

// No headroom: no call, a note; an invalid answer: a note. Neither fails.
func TestModelReviewNotes(t *testing.T) {
	fj := &gateForgejo{diff: "d"}
	r, _, _ := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: "LGTM"}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if rv := getInto[Review](t, val, err); rv.Verdict != "" || !strings.Contains(rv.Note, "answer invalid") {
		t.Fatalf("review = %+v", rv)
	}
	r.supervisor.Meter = &memPolicy{taskFree: 0.0001, dayFree: 20}
	val, err = execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if rv := getInto[Review](t, val, err); !strings.Contains(rv.Note, "model review skipped: the task's headroom does not pay for it") {
		t.Fatalf("review = %+v", rv)
	}
	if len(ll.chats) != 1 || len(fj.comments) != 2 || !strings.Contains(fj.comments[1], "skipped") {
		t.Fatalf("chats = %d, comments = %q", len(ll.chats), fj.comments)
	}
}

func TestRequestApprovalOpensTheCard(t *testing.T) {
	r, store, objects := newGateRunner(t, gateTask, &gateForgejo{})
	in := ApprovalInput{PR: PullRequest{Number: 9, URL: "https://git.example/sf/repo/pulls/9"}, Commit: "c1",
		Gates:  GateResults{Policy: PolicyGate{Class: "high", PolicyVersion: 7}},
		Review: Review{Note: "model review skipped"}}
	for range 2 {
		if _, err := execActivity(t, r, ActivityRequestApproval, "t1", in); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := store.ApprovalRequests(context.Background(), "pending")
	if len(pending) != 1 || len(store.approvals) != 2 {
		t.Fatalf("approvals = %+v, want one pending, the retried one superseded", store.approvals)
	}
	a := pending[0]
	var spec MergeSpec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if a.Kind != "merge" || a.PRNumber != 9 || a.Project != "sf/repo" || !strings.Contains(a.Reason, "risk class high") ||
		spec.HeadSHA != "c1" || spec.RiskClass != "high" || spec.Migration || spec.VerdictSummary != "model review skipped" ||
		spec.PRURL != in.PR.URL || spec.PolicyVersion != 7 {
		t.Fatalf("approval = %+v, spec %+v", a, spec)
	}
	if first := store.approvals[0]; first.Status != "denied" || first.DecidedBy != "factoryd" {
		t.Fatalf("superseded request = %+v", first)
	}
	if !strings.Contains(timeline(t, objects), `"step":"approval_requested"`) {
		t.Fatal("no approval_requested step")
	}
}

func TestMergeAsFactoryMerge(t *testing.T) {
	fj := &gateForgejo{mergeStatus: []int{http.StatusOK}}
	r, store, objects := newGateRunner(t, gateTask, fj)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian")
	if res := getInto[MergeResult](t, val, err); !res.Merged {
		t.Fatalf("result = %+v", res)
	}
	if fj.merges[0]["head_commit_id"] != "c1" || !strings.Contains(fj.merges[0]["MergeMessageField"].(string), "approved by sebastian") {
		t.Fatalf("merge = %v", fj.merges[0])
	}
	if a := store.approvals[0]; a.Status != "approved" || a.DecidedBy != "sebastian" {
		t.Fatalf("approval = %+v", a)
	}
	if !strings.Contains(timeline(t, objects), `"step":"merged"`) {
		t.Fatal("no merged step")
	}
}

// Forgejo's refusal (no whitelisted approval) is a result with a comment,
// not an error: the workflow keeps waiting. A refusal on a pull request
// that is merged after all is a merge.
func TestMergeRefusal(t *testing.T) {
	fj := &gateForgejo{mergeStatus: []int{http.StatusMethodNotAllowed}}
	r, store, objects := newGateRunner(t, gateTask, fj)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian")
	res := getInto[MergeResult](t, val, err)
	if res.Merged || !strings.Contains(res.Refusal, "405") {
		t.Fatalf("result = %+v", res)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "was refused: forgejo POST") || store.approvals[0].Status != "pending" {
		t.Fatalf("comments = %q, approval = %+v", fj.comments, store.approvals[0])
	}
	// the console's card says why the approve did not merge
	if note := store.approvals[0].DecisionNote; !strings.HasPrefix(note, "waiting: Forgejo refused the merge after sebastian's approval: ") ||
		!strings.Contains(note, "405") {
		t.Fatalf("approval note = %q", note)
	}
	if !strings.Contains(timeline(t, objects), `"step":"merge_refused"`) {
		t.Fatal("no merge_refused step")
	}
	fj.merged = true
	val, err = execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian")
	if res := getInto[MergeResult](t, val, err); !res.Merged || len(fj.comments) != 1 {
		t.Fatalf("result on a merged PR = %+v, comments %d", res, len(fj.comments))
	}
}

// A new session denies the task's pending merge approval as superseded but
// leaves its resource request waiting for the human; Finish denies both
// with the task's end.
func TestApprovalsFollowTheTask(t *testing.T) {
	fj := &prForgejo{labels: map[int64][]string{4: {"agent", "state/in-review"}}}
	task := prTask
	task.Status = "in-review"
	r, store, _ := newActivityRunner(t, task, fj)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "resource", TaskID: "t1"})
	if _, err := execActivity(t, r, ActivitySetState, "t1", "running"); err != nil {
		t.Fatal(err)
	}
	if res := store.approvals[1]; res.Status != "pending" {
		t.Fatalf("a new attempt decided the resource request: %+v", res)
	}
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"superseded: a new attempt changes the pull request", "the task ended done: pull request merged",
		"the task ended done: pull request merged"} {
		if a := store.approvals[i]; a.Status != "denied" || a.DecisionNote != want {
			t.Fatalf("approval %d = %+v, want denied: %s", i, a, want)
		}
	}
}

type recordedSignals struct {
	Workflows
	sent []string
}

func (w *recordedSignals) Signal(ctx context.Context, taskID, name string, sig TaskSignal) error {
	w.sent = append(w.sent, fmt.Sprintf("%s %s %s %d", taskID, name, sig.Author, sig.PR))
	return nil
}

// The console's approve signals only a pending merge request of that task
// and pull request.
func TestApproveMerge(t *testing.T) {
	store := newFakeStore()
	wf := &recordedSignals{}
	ctx := context.Background()
	if err := ApproveMerge(ctx, store, wf, "t1", 9, "sebastian"); err == nil {
		t.Fatal("approved without a pending request")
	}
	store.OpenApproval(ctx, ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	if err := ApproveMerge(ctx, store, wf, "t1", 8, "sebastian"); err == nil {
		t.Fatal("approved another pull request")
	}
	if err := ApproveMerge(ctx, store, wf, "t1", 9, "sebastian"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wf.sent, []string{"t1 approved sebastian 9"}) {
		t.Fatalf("signals = %v", wf.sent)
	}
}

// No reproduction command: nothing runs.
func TestGateReproductionWithoutACommand(t *testing.T) {
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, _ := newTestRunner(t, gateTask, kube, objects)
	val, err := execActivity(t, r, ActivityGateReproduction, "t1", int64(9), "c1")
	if res := getInto[ReproResult](t, val, err); res.Ran || len(kube.podOrder) != 0 || len(hub.minted()) != 0 {
		t.Fatalf("result = %+v, pods %d, mints %v", res, len(kube.podOrder), hub.minted())
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"step":"gate_reproduction"`) || !strings.Contains(tl, `"skipped":"no reproduction command"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// The reproduction heartbeats from its start: a slow namespace setup (a
// previous namespace still terminating) never runs into the heartbeat
// timeout before the pod exists.
func TestGateReproductionHeartbeatsThroughASlowNamespace(t *testing.T) {
	defer func(d time.Duration) { gateHeartbeatEvery = d }(gateHeartbeatEvery)
	gateHeartbeatEvery = 20 * time.Millisecond
	defer func(f func(context.Context, ...any)) { recordGateHeartbeat = f }(recordGateHeartbeat)
	var beats atomic.Int32
	recordGateHeartbeat = func(ctx context.Context, d ...any) { beats.Add(1); activity.RecordHeartbeat(ctx, d...) }
	task := gateTask
	task.ReproCommand = "true"
	kube := newFakeKube()
	kube.nsDelay = 300 * time.Millisecond
	var atPod atomic.Int32
	kube.onCreatePod = func() { atPod.Store(beats.Load()) }
	r, _, _ := newTestRunner(t, task, kube, &fakeObjectStore{})
	done := make(chan error)
	go func() {
		_, err := execActivity(t, r, ActivityGateReproduction, "t1", int64(9), "c1")
		done <- err
	}()
	for {
		kube.mu.Lock()
		n := len(kube.podOrder)
		kube.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	kube.scriptPods(0, reproLine(0, "ok"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := atPod.Load(); n < 5 {
		t.Fatalf("%d heartbeats before the pod after a 300 ms namespace setup at 20 ms, want several", n)
	}
}

func reproLine(exit int, output string) string {
	b, _ := json.Marshal(harness.WorkerEnvelope{Type: "repro", Repro: &harness.Repro{Exit: exit, Output: output}})
	return string(b)
}

// The command runs in a fresh sandbox pod with no agent: the worker in its
// repro mode, a read token on the task's repository only, the sandbox's
// egress label, no gateway token; the namespace and the token go after.
func TestGateReproductionRunsTheCommandInASandbox(t *testing.T) {
	task := gateTask
	task.ReproCommand = "go test ./internal/x -run TestY"
	kube := newFakeKube()
	objects := &fakeObjectStore{}
	r, hub, _ := newTestRunner(t, task, kube, objects)
	var res ReproResult
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		var val converter.EncodedValue
		if val, err = execActivity(t, r, ActivityGateReproduction, "t1", int64(9), "c1"); err == nil {
			err = val.Get(&res)
		}
	}()
	for {
		kube.mu.Lock()
		n := len(kube.podOrder)
		kube.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	kube.scriptPods(0, "agent-worker: fetching\n"+reproLine(1, "--- FAIL: TestY\nFAIL"))
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if res != (ReproResult{Ran: true, Command: task.ReproCommand, Exit: 1, Output: "--- FAIL: TestY\nFAIL"}) {
		t.Fatalf("result = %+v", res)
	}
	pod := kube.podAt(0)
	if !strings.HasPrefix(pod.name, "repro-") || pod.ns != "factory-agents-t1" {
		t.Fatalf("pod = %s/%s", pod.ns, pod.name)
	}
	spec, _ := json.Marshal(pod.spec)
	for _, want := range []string{`"app.kubernetes.io/name":"opencode-worker"`, `"runtimeClassName":"gvisor"`,
		`{"name":"MODE","value":"repro"}`, `{"name":"PR_REF","value":"refs/pull/9/head"}`, `{"name":"REPO_SHA","value":"c1"}`,
		`{"name":"REPRO_COMMAND","value":"go test ./internal/x -run TestY"}`, `{"name":"REPRO_TIMEOUT","value":"600"}`,
		`"REPO_URL","value":"http://forgejo-http.forgejo.svc.cluster.local:3000/sf/repo.git"`} {
		if !strings.Contains(string(spec), want) {
			t.Fatalf("pod spec lacks %s: %s", want, spec)
		}
	}
	for _, not := range []string{"gateway-token", "PUSH_TOKEN", "GATEWAY_URL"} {
		if strings.Contains(string(spec), not) {
			t.Fatalf("pod spec carries %s: %s", not, spec)
		}
	}
	// the read token is the fetch init container's alone: the container
	// that runs the agent-written command has none
	var parsed struct {
		Spec struct {
			InitContainers, Containers []struct {
				Name string
				Env  []struct{ Name string }
			}
		}
	}
	json.Unmarshal(spec, &parsed)
	envOf := func(cs []struct {
		Name string
		Env  []struct{ Name string }
	}) string {
		var out []string
		for _, c := range cs {
			var names []string
			for _, e := range c.Env {
				names = append(names, e.Name)
			}
			out = append(out, c.Name+": "+strings.Join(names, " "))
		}
		return strings.Join(out, "; ")
	}
	if got := envOf(parsed.Spec.InitContainers); got != "fetch: MODE REPO_URL REPO_TOKEN PR_REF REPO_SHA" {
		t.Fatalf("init containers = %s", got)
	}
	if got := envOf(parsed.Spec.Containers); got != "worker: MODE TASK_ID REPRO_COMMAND REPRO_TIMEOUT" {
		t.Fatalf("containers = %s", got)
	}
	if calls := strings.Join(hub.calls, "\n"); !strings.Contains(calls, "mint task-t1-base read:repository sf/repo") || strings.Contains(calls, "write:repository") {
		t.Fatalf("forgejo calls = %s", calls)
	}
	if len(hub.tokens) != 0 || !slices.Contains(kube.deletedNs, "factory-agents-t1") {
		t.Fatalf("tokens left %v, namespaces deleted %v", hub.tokens, kube.deletedNs)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0], "**failed** (exit 1)") || !strings.Contains(hub.comments[0], "--- FAIL: TestY") {
		t.Fatalf("comments = %q", hub.comments)
	}
	if tl, _ := objects.Get(context.Background(), "sessions/t1/timeline.jsonl"); !strings.Contains(string(tl), `"step":"gate_reproduction"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// A head that moved after the checks is not gated: GatePolicy refuses it
// terminally, ModelReview notes it without a model call.
func TestGatesRefuseAMovedHead(t *testing.T) {
	fj := &gateForgejo{head: "c2", files: []string{`{"filename":"README.md"}`}}
	r, _, _ := newGateRunner(t, gateTask, fj)
	if _, err := execActivity(t, r, ActivityGatePolicy, "t1", int64(9), "c1"); err == nil || !isTerminal(err) || !strings.Contains(err.Error(), "head moved to c2") {
		t.Fatalf("GatePolicy on a moved head = %v, want a terminal refusal", err)
	}
	// a workflow from before versionGatesPinned passes no commit
	if _, err := execActivity(t, r, ActivityGatePolicy, "t1", int64(9)); err != nil {
		t.Fatalf("GatePolicy without a commit = %v", err)
	}
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"s"}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	val, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{})
	if rv := getInto[Review](t, val, err); !strings.Contains(rv.Note, "head moved to c2") || len(ll.chats) != 0 {
		t.Fatalf("review = %+v after %d model calls", rv, len(ll.chats))
	}
}

// A pull request into another branch than main is not merged.
func TestMergeOnlyIntoMain(t *testing.T) {
	fj := &gateForgejo{base: "release", mergeStatus: []int{http.StatusOK}}
	r, _, _ := newGateRunner(t, gateTask, fj)
	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "sebastian")
	if res := getInto[MergeResult](t, val, err); res.Merged || !strings.Contains(res.Refusal, `"release", not main`) || len(fj.merges) != 0 {
		t.Fatalf("result = %+v, merges %d", res, len(fj.merges))
	}
}

// A human's merge in Forgejo, of another head, is credited to the human:
// the approval row and the result name them and the head they merged.
func TestMergeCreditsAHumanMerge(t *testing.T) {
	fj := &gateForgejo{merged: true, mergedBy: "sebastian", head: "c2"}
	r, store, _ := newGateRunner(t, gateTask, fj)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	val, err := execActivity(t, r, ActivityMerge, "t1", int64(9), "c1", "alice")
	if res := getInto[MergeResult](t, val, err); !res.Merged || res.By != "sebastian" || len(fj.merges) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if a := store.approvals[0]; a.Status != "approved" || a.DecidedBy != "sebastian" || a.DecisionNote != "merged in Forgejo by sebastian at c2, not the gated c1" {
		t.Fatalf("approval = %+v", a)
	}
}

// A merge that fails other than by refusal is an error for the retry; the
// last attempt reports it on the timeline and the pull request instead.
func TestMergeFailureOnTheLastAttempt(t *testing.T) {
	fj := &gateForgejo{stateFail: 2}
	r, _, objects := newGateRunner(t, gateTask, fj)
	ctx := context.Background()
	if _, err := r.mergeAttempt(ctx, gateTask, Repo{"sf", "repo"}, 9, "c1", "sebastian", false); err == nil {
		t.Fatal("a transient failure before the last attempt was not an error")
	}
	res, err := r.mergeAttempt(ctx, gateTask, Repo{"sf", "repo"}, 9, "c1", "sebastian", true)
	if err != nil || res.Merged || !strings.Contains(res.Failed, "502") {
		t.Fatalf("last attempt = %+v, %v", res, err)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "The approval stands") || !strings.Contains(timeline(t, objects), `"step":"merge_failed"`) {
		t.Fatalf("comments = %q", fj.comments)
	}
}

func TestFencedCannotBeClosedEarly(t *testing.T) {
	if got := fenced("a\n"); got != "```\na\n```" {
		t.Fatalf("fenced = %q", got)
	}
	if got := fenced("x ```` y"); got != "`````\nx ```` y\n`````" {
		t.Fatalf("fenced = %q", got)
	}
}

// The review heartbeats while its model call runs, so the workflow's
// cancel (a terminal signal during the review) reaches the call; a call
// cut by the cancel books its hold and leaves none open.
func TestModelReviewHeartbeatsAndACancelCutsTheCall(t *testing.T) {
	defer func(d time.Duration) { gateHeartbeatEvery = d }(gateHeartbeatEvery)
	gateHeartbeatEvery = 20 * time.Millisecond
	fj := &gateForgejo{diff: "d"}
	r, _, _ := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"s"}`, delay: 300 * time.Millisecond}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	env := suite.NewTestActivityEnvironment()
	var beats atomic.Int32
	defer func(f func(context.Context, ...any)) { recordGateHeartbeat = f }(recordGateHeartbeat)
	recordGateHeartbeat = func(ctx context.Context, d ...any) { beats.Add(1); activity.RecordHeartbeat(ctx, d...) }
	r.RegisterActivities(env)
	if _, err := env.ExecuteActivity(ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err != nil {
		t.Fatal(err)
	}
	if beats.Load() < 3 {
		t.Fatalf("%d heartbeats during a 300 ms call at 20 ms, want several", beats.Load())
	}

	ll.delay = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	env = suite.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})
	r.RegisterActivities(env)
	time.AfterFunc(100*time.Millisecond, cancel)
	began := time.Now()
	if _, err := env.ExecuteActivity(ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err == nil {
		t.Fatal("a cancelled review succeeded")
	}
	meter := r.supervisor.Meter.(*memPolicy)
	if time.Since(began) > 10*time.Second || len(meter.holds) != 2 || len(meter.released) != 2 {
		t.Fatalf("after %s: holds %d, released %d; want the cut call's hold released", time.Since(began), len(meter.holds), len(meter.released))
	}
}

func TestReportMergeFailure(t *testing.T) {
	fj := &gateForgejo{}
	r, store, objects := newGateRunner(t, gateTask, fj)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	if _, err := execActivity(t, r, ActivityReportMergeFailure, "t1", int64(9), "c1", "sebastian", "forgejo: not found"); err != nil {
		t.Fatal(err)
	}
	if note := store.approvals[0].DecisionNote; note != "waiting: the merge after sebastian's approval failed and is retried: forgejo: not found" {
		t.Fatalf("approval note = %q", note)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "failed: forgejo: not found") || !strings.Contains(fj.comments[0], "The approval stands") {
		t.Fatalf("comments = %q", fj.comments)
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"step":"merge_failed"`) || !strings.Contains(tl, `"error":"forgejo: not found"`) {
		t.Fatalf("timeline = %s", tl)
	}
}

// The heartbeats start with the activity: a slow diff fetch before the
// model call heartbeats too, so it never runs into the heartbeat timeout.
func TestModelReviewHeartbeatsThroughASlowDiffFetch(t *testing.T) {
	defer func(d time.Duration) { gateHeartbeatEvery = d }(gateHeartbeatEvery)
	gateHeartbeatEvery = 20 * time.Millisecond
	defer func(f func(context.Context, ...any)) { recordGateHeartbeat = f }(recordGateHeartbeat)
	var beats atomic.Int32
	recordGateHeartbeat = func(ctx context.Context, d ...any) { beats.Add(1); activity.RecordHeartbeat(ctx, d...) }
	fj := &gateForgejo{diff: "d", diffDelay: 300 * time.Millisecond}
	r, _, _ := newGateRunner(t, gateTask, fj)
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"s"}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	if _, err := execActivity(t, r, ActivityModelReview, "t1", PullRequest{Number: 9}, "c1", GateResults{}); err != nil {
		t.Fatal(err)
	}
	if beats.Load() < 5 {
		t.Fatalf("%d heartbeats during a 300 ms diff fetch at 20 ms, want several", beats.Load())
	}
}

func TestReportMergeRetriesExhausted(t *testing.T) {
	fj := &gateForgejo{}
	r, store, objects := newGateRunner(t, gateTask, fj)
	store.OpenApproval(context.Background(), ApprovalRequest{Kind: "merge", TaskID: "t1", PRNumber: 9})
	if _, err := execActivity(t, r, ActivityReportMergeRetriesExhausted, "t1", int64(9), "c1", "sebastian"); err != nil {
		t.Fatal(err)
	}
	if note := store.approvals[0].DecisionNote; note != "waiting: the merge after sebastian's approval failed 6 times; approve again to retry" {
		t.Fatalf("approval note = %q", note)
	}
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0], "merge retries exhausted") || !strings.Contains(fj.comments[0], "A new approval restarts the merge") {
		t.Fatalf("comments = %q", fj.comments)
	}
	if tl := timeline(t, objects); !strings.Contains(tl, `"step":"merge_retries_exhausted"`) || !strings.Contains(tl, `"retries":6`) {
		t.Fatalf("timeline = %s", tl)
	}
}
