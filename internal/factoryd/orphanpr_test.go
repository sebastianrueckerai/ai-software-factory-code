package factoryd

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/converter"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func activityInto[T any](t *testing.T, val converter.EncodedValue, err error) T {
	t.Helper()
	return getInto[T](t, val, err)
}

// A pull request factoryd opened outside any card's workflow becomes a
// card of its own (roadmap 187): FileFactoryPRTask files the issue, links
// the open pull request to the row and starts the workflow.
func TestFileFactoryPRTaskCreatesCardAndLinksPR(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	got, ref, err := FileFactoryPRTask(ctx, store, f, wf, nil, seedProject("sf/r"), "Schema copies for abc123", "body", []string{"improvement"},
		Task{ID: "t-schema", Source: SourceSchemaCopy}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRNumber != 9 || got.IssueNumber <= 0 {
		t.Fatalf("task = %+v, want PRNumber 9 and an issue above 0", got)
	}
	if ref.Number != got.IssueNumber {
		t.Fatalf("ref #%d, row #%d: want the same issue", ref.Number, got.IssueNumber)
	}
	row := store.tasks["t-schema"]
	if row.PRNumber != 9 || row.IssueNumber != got.IssueNumber {
		t.Fatalf("stored row = %+v, want PRNumber 9 and the filed issue linked", row)
	}
	if row.Kind != "agent" || row.Status != "queued" || row.Source != SourceSchemaCopy ||
		!slices.Equal(row.RequiredCapabilities, []string{policy.ClassCoder}) {
		t.Fatalf("stored row = %+v, want a queued agent card of ready's class", row)
	}
	if !slices.Equal(*labels, []string{"improvement", "state/queued"}) {
		t.Fatalf("labels %v", *labels)
	}
	if !slices.Equal(wf.started, []string{"t-schema"}) {
		t.Fatalf("started %v, want the workflow started once", wf.started)
	}
	// a project whose ready lane the approvers move files the card in the
	// backlog instead, with the pull request linked all the same
	p := seedProject("sf/r")
	p.Process, p.Lanes = "gated", approversLanes
	store, wf = newFakeStore(), &fakeWorkflows{}
	got, _, err = FileFactoryPRTask(ctx, store, f, wf, nil, p, "Sync from GitHub abc1234", "body", nil, Task{ID: "t-sync", Source: SourceGitHubSync}, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "backlog" || got.Lane != "backlog" || store.tasks["t-sync"].PRNumber != 11 || len(wf.started) != 0 {
		t.Fatalf("task = %+v, stored %+v, started %v; want a backlog card with pull request #11 linked and no workflow", got, store.tasks["t-sync"], wf.started)
	}
}

// TaskConfig gives a card for a pull request factoryd opened outside any
// card's workflow ready's process and class with no lane, like a roll; a
// row that does name a lane keeps it.
func TestTaskConfigForFactorySources(t *testing.T) {
	pinned := fixtureVersion(t)
	maintenance := pinned.Processes["maintenance"]
	for name, source := range map[string]string{
		"github sync": SourceGitHubSync, "schema copy": SourceSchemaCopy, "template update": SourceTemplateUpdate,
	} {
		t.Run(name, func(t *testing.T) {
			task := Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Source: source}
			r, _, _ := newActivityRunner(t, task, &prForgejo{})
			r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
			val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
			if err != nil {
				t.Fatal(err)
			}
			got := getInto[TaskConfig](t, val, nil)
			if got.Process != "maintenance" || got.Class != policy.ClassCoder || got.Session != policy.SessionCode ||
				!reflect.DeepEqual(got.Stages, maintenance.Stages) || got.Queue != maintenance.Merge.Queue {
				t.Fatalf("TaskConfig = %+v, want maintenance's ready lane class %s", got, policy.ClassCoder)
			}
		})
	}
	// a row the loader moved into a lane keeps that lane's class
	task := Task{ID: "t1", Kind: "agent", Status: "queued", Repository: "demo-app/demo-app", Source: SourceTemplateUpdate, Lane: "ready", Process: "maintenance"}
	r, _, _ := newActivityRunner(t, task, &prForgejo{})
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	val, err := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
	if err != nil {
		t.Fatal(err)
	}
	if got := getInto[TaskConfig](t, val, nil); got.Lane != policy.LaneReady || got.Class != policy.ClassCoder {
		t.Fatalf("TaskConfig = %+v, want the row's ready lane", got)
	}
}

// The run of a card whose pull request factoryd opened before the card
// existed verifies that pull request: it adopts the recorded one and goes
// to the checks without opening one (roadmap 187).
func TestWorkflowSkipsOpenPullRequestWhenPRSet(t *testing.T) {
	h := newWorkflowHarness(t)
	h.state = RunState{Status: "queued", FactoryPR: true, PR: PullRequest{Number: 9, URL: "https://git.example/sf/repo/pulls/9"}, Head: "c7"}
	var checked []string
	h.env.OnActivity(ActivityWaitChecks, mock.Anything, "t1", mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (ChecksResult, error) {
			checked = append(checked, sha)
			return ChecksResult{State: ChecksSuccess}, nil
		})
	h.env.OnActivity(ActivityOpenPullRequest, mock.Anything, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, taskID, sha string) (PullRequest, error) {
			t.Errorf("OpenPullRequest called for head %s: the card's pull request was already open", sha)
			return PullRequest{Number: 9}, nil
		})
	h.signalAfter(time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})

	if final := h.run(t); final != "done" {
		t.Fatalf("final = %q, want done", final)
	}
	if len(h.sessions) != 0 {
		t.Fatalf("sessions = %v, want none: the card's work is the open pull request", h.sessions)
	}
	if !slices.Contains(h.states, "in-review") {
		t.Fatalf("states = %v, want the card in review", h.states)
	}
	if !slices.Equal(checked, []string{"c7"}) {
		t.Fatalf("checks ran on %v, want the adopted head c7 once", checked)
	}
}

// Reconcile of a factory pull-request card reads its state from the row's
// PRNumber: the open pull request is adopted straight from the number,
// whatever branch it stands on.
func TestReconcileAdoptsFactoryPRCard(t *testing.T) {
	fj := &prForgejo{pulls: map[int64]string{9: `{"number":9,"state":"open","html_url":"https://git.example/sf/repo/pulls/9","head":{"sha":"h9","ref":"factory/schema-abc","repo":{"full_name":"factory-agent/repo"}},"base":{"ref_name":"main"},"mergeable":true}`}}
	r, _, _ := newActivityRunner(t, withPR(withSource(withStatus(prTask, "queued"), SourceSchemaCopy), 9), fj)
	st := reconcileInto(t, r)
	if !st.FactoryPR || st.PR.Number != 9 || st.Head != "h9" || st.Merged || st.PRClosed || st.Roll {
		t.Fatalf("RunState = %+v, want the schema-copy card's pull request #9 at h9 adopted", st)
	}
	merged := &prForgejo{pulls: map[int64]string{9: `{"number":9,"state":"closed","merged":true,"head":{"sha":"h9"},"base":{"ref_name":"main"}}`}}
	r2, _, _ := newActivityRunner(t, withPR(withSource(withStatus(prTask, "in-review"), SourceTemplateUpdate), 9), merged)
	if st2 := reconcileInto(t, r2); !st2.Merged || st2.PR.Number != 0 {
		t.Fatalf("RunState = %+v, want the merged pull request reported merged", st2)
	}
	// a coding card takes none of this: its pull request comes from the
	// fork's branch, not the row's number
	forked := &prForgejo{open: []map[string]any{{"number": 12, "html_url": "u",
		"head": map[string]any{"ref": "factory/t1", "sha": "h12", "label": "factory-agent/repo:factory/t1",
			"repo": map[string]any{"full_name": "factory-agent/repo"}}}}}
	r3, _, _ := newActivityRunner(t, withStatus(prTask, "running"), forked)
	if st3 := reconcileInto(t, r3); st3.FactoryPR || st3.PR.Number != 12 {
		t.Fatalf("RunState = %+v, want the coding card's branch pull request found as before", st3)
	}
}

func reconcileInto(t *testing.T, r *Runner) RunState {
	t.Helper()
	val, err := execActivity(t, r, ActivityReconcile, "t1")
	return activityInto[RunState](t, val, err)
}

// The card's verify path on the real activities end to end: Reconcile
// adopts the pull request recorded on the row, the card moves to
// in-review, its checks are green on the adopted head, and a human's
// approval merges that very pull request as factory-merge — no session,
// no OpenPullRequest, no branch lookup (roadmap 187).
func TestFactoryPRCardRealActivitiesMergeTheAdoptedPR(t *testing.T) {
	fj := newOrphanForgejo()
	card := Task{ID: "t1", Kind: "agent", Status: "queued", Objective: "Copy the schemas of abc123", Repository: "demo-app/demo-app",
		SHA: "base", IssueNumber: 4, Source: SourceSchemaCopy, PRNumber: 9}
	r, store, _ := newActivityRunner(t, card, &fj.prForgejo)
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"the copy matches upstream","findings":[]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	pinned := fixtureVersion(t)
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }

	st := reconcileInto(t, r)
	if !st.FactoryPR || st.PR.Number != 9 || st.Head != "h9" {
		t.Fatalf("RunState = %+v, want pull request #9 adopted at h9", st)
	}
	cfgVal, cfgErr := execActivity(t, r, ActivityTaskConfig, "t1", int64(7))
	cfg := activityInto[TaskConfig](t, cfgVal, cfgErr)
	if cfg.Class != policy.ClassCoder || cfg.Process != "maintenance" || cfg.Lane != "" {
		t.Fatalf("TaskConfig = %+v, want maintenance's coder with no lane", cfg)
	}
	if _, err := execActivity(t, r, ActivitySetState, "t1", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := execActivity(t, r, ActivitySetState, "t1", "in-review"); err != nil {
		t.Fatal(err)
	}
	checksVal, checksErr := execActivity(t, r, ActivityWaitChecks, "t1", "h9")
	checks := activityInto[ChecksResult](t, checksVal, checksErr)
	if checks.State != ChecksSuccess {
		t.Fatalf("checks = %+v, want green on the adopted head", checks)
	}
	gateVal, gateErr := execActivity(t, r, ActivityGatePolicy, "t1", int64(9), "h9", int64(7))
	gate := activityInto[PolicyGate](t, gateVal, gateErr)
	if gate.Outcome != policy.OutcomeRequiresApproval || gate.MergeRule != policy.MergeHuman {
		t.Fatalf("PolicyGate = %+v, want requires-approval on the human merge rule", gate)
	}
	reproVal, reproErr := execActivity(t, r, ActivityGateReproduction, "t1", int64(9), "h9")
	repro := activityInto[ReproResult](t, reproVal, reproErr)
	if repro.Ran {
		t.Fatalf("ReproResult = %+v, want the gate skipped: the card carries no reproduction command", repro)
	}
	rvVal, rvErr := execActivity(t, r, ActivityModelReview, "t1", st.PR, "h9", GateResults{Pin: 7, Policy: gate, Reproduction: repro}, nil)
	rv := activityInto[Review](t, rvVal, rvErr)
	if rv.Verdict != "approve" {
		t.Fatalf("Review = %+v, want the model review's approval", rv)
	}
	fj.setMerged()
	resVal, resErr := execActivity(t, r, ActivityMerge, "t1", int64(9), "h9", "sebastian")
	res := activityInto[MergeResult](t, resVal, resErr)
	if !res.Merged {
		t.Fatalf("MergeResult = %+v, want the approved merge of the card's pull request", res)
	}
	if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
		t.Fatal(err)
	}
	row, _, _ := store.Task(context.Background(), "t1")
	if row.Status != "done" {
		t.Fatalf("row status = %q, want done", row.Status)
	}
	calls := fj.callList()
	for _, call := range calls {
		if strings.HasPrefix(call, "POST /api/v1/repos/sf/repo/pulls ") || strings.Contains(call, "/pulls/main/") {
			t.Fatalf("calls %v: the card's pull request was adopted, never opened nor looked up by branch", calls)
		}
	}
}

// orphanForgejo plays the endpoints a factory pull-request card's real
// activities call: pull request #9 (open, then merged) and its combined
// status. The embedded prForgejo answers the rest.
type orphanForgejo struct {
	prForgejo
	pullMu sync.Mutex
}

func newOrphanForgejo() *orphanForgejo {
	o := &orphanForgejo{}
	o.pulls = map[int64]string{9: `{"number":9,"state":"open","html_url":"https://git.example/sf/repo/pulls/9","base":{"ref_name":"main"},"mergeable":true,"head":{"ref":"factory/schema-abc","sha":"h9","repo":{"full_name":"factory-agent/repo"}}}`}
	o.statuses = []string{`{"state":"success","statuses":[{"status":"success","context":"ci/check","target_url":"","description":"ok"}]}`}
	return o
}

func (o *orphanForgejo) setMerged() {
	o.pullMu.Lock()
	defer o.pullMu.Unlock()
	o.pulls[9] = `{"number":9,"state":"closed","merged":true,"merged_by":{"login":"factory-merge"},"html_url":"https://git.example/sf/repo/pulls/9","base":{"ref_name":"main"},"head":{"ref":"factory/schema-abc","sha":"h9","repo":{"full_name":"factory-agent/repo"}}}`
}

func (o *orphanForgejo) callList() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.calls)
}
