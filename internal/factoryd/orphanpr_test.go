package factoryd

import (
	"context"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
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
	got, ref, err := FileFactoryPRTask(ctx, store, f, wf, nil, testCardBudget, seedProject("sf/r"), "Schema copies for abc123", "body", []string{"improvement"},
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
	// a project whose ready lane the approvers moves files the card with no
	// lane all the same, like a roll: ready's class, workflow started
	p := seedProject("sf/r")
	p.Process, p.Lanes = "gated", approversLanes
	store, wf = newFakeStore(), &fakeWorkflows{}
	got, _, err = FileFactoryPRTask(ctx, store, f, wf, nil, testCardBudget, p, "Sync from GitHub abc1234", "body", nil, Task{ID: "t-sync", Source: SourceGitHubSync}, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || got.Lane != "" || got.Process != "" ||
		!slices.Equal(got.RequiredCapabilities, []string{policy.ClassCoder}) ||
		store.tasks["t-sync"].PRNumber != 11 || !slices.Equal(wf.started, []string{"t-sync"}) {
		t.Fatalf("task = %+v, stored %+v, started %v; want a no-lane card of ready's class with pull request #11 linked and the workflow started", got, store.tasks["t-sync"], wf.started)
	}
}

// Requires a disposable Postgres (FACTORYD_TEST_DATABASE_URL): a factory
// pull request card runs no session, only its model gates, so it is filed
// with the policy's task default, 2 EUR of testCardBudget. On the database
// the gateway grants from, the review's largest call, a whole diff part
// and its prompt (64 KiB, a token a byte at 3e-6 EUR) at the gates' output
// limit of 18432 tokens at 1.5e-5 EUR, takes its hold of 0.473088 EUR, and
// the audit's like call beside it takes one too.
func TestFactoryPRCardFundsItsGates(t *testing.T) {
	s, err := OpenStore(t.Context(), schematest.DSN(t, "factoryd_factory_card"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	f, _ := fileStub(t)
	for _, source := range []string{SourceGitHubSync, SourceSchemaCopy, SourceTemplateUpdate, SourceRevert} {
		if _, err := s.db.ExecContext(ctx, `TRUNCATE tasks, holds, ledger, audit CASCADE`); err != nil {
			t.Fatal(err)
		}
		card, _, err := FileFactoryPRTask(ctx, s, f, &fakeWorkflows{}, nil, testCardBudget, seedProject("sf/r"), "title", "body", nil,
			Task{ID: "t-gates", Source: source}, 9)
		if err != nil {
			t.Fatal(err)
		}
		if row, _, _ := s.Task(ctx, "t-gates"); card.BudgetEUR != 2 || row.BudgetEUR != 2 {
			t.Fatalf("%s: card %v EUR, row %v EUR, want the policy's task default of 2", source, card.BudgetEUR, row.BudgetEUR)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = 'in-review' WHERE id = 't-gates'`); err != nil {
			t.Fatal(err)
		}
		call := policy.HoldRequest{TaskID: "t-gates", Kind: policy.KindModel, Ref: "big", Bytes: reviewDiffMax + 16<<10,
			Price: policy.Price{In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}, WantOut: 2048 + 16384, DayEUR: 20, MonthEUR: 50, ProjectEUR: math.Inf(1)}
		review, err := policy.TakeHold(ctx, s.db, call)
		if err != nil || review.MaxOut != call.WantOut || math.Abs(review.EUR-0.473088) > 1e-9 {
			t.Fatalf("%s: the review's hold = (%+v, %v), want 0.473088 EUR for all %d output tokens", source, review, err, call.WantOut)
		}
		call.Ref = "big"
		if audit, err := policy.TakeHold(ctx, s.db, call); err != nil || audit.MaxOut != call.WantOut {
			t.Fatalf("%s: the audit's hold beside the review's = (%+v, %v), want all %d output tokens", source, audit, err, call.WantOut)
		}
	}
}

// FileFactoryPRTask inserts the card's row with its pull request linked,
// one statement: a store that refuses the insert leaves no row and starts
// no run, and no card stands queued without its pull request, which a link
// written after the row once could leave when it failed. A card asked for
// no pull request at all is refused before its issue is filed.
func TestFileFactoryPRTaskInsertsThePullRequestWithTheRow(t *testing.T) {
	ctx := context.Background()
	f, labels := fileStub(t)
	store, wf := newFakeStore(), &fakeWorkflows{}
	if _, _, err := FileFactoryPRTask(ctx, store, f, wf, nil, testCardBudget, seedProject("sf/r"), "title", "body", nil, Task{ID: "t0", Source: SourceSchemaCopy}, 0); err == nil ||
		err.Error() != "a schema-copy card needs its pull request" || *labels != nil {
		t.Fatalf("err %v, labels %v: want the refusal before any issue", err, *labels)
	}
	store.createFails = 1
	if _, _, err := FileFactoryPRTask(ctx, store, f, wf, nil, testCardBudget, seedProject("sf/r"), "title", "body", nil, Task{ID: "t1", Source: SourceTemplateUpdate}, 9); err == nil {
		t.Fatal("want the refused insert's error")
	}
	if len(store.tasks) != 0 || len(wf.started) != 0 {
		t.Fatalf("tasks %v, started %v: a refused insert leaves no card", store.tasks, wf.started)
	}
	if _, _, err := FileFactoryPRTask(ctx, store, f, wf, nil, testCardBudget, seedProject("sf/r"), "title", "body", nil, Task{ID: "t1", Source: SourceTemplateUpdate}, 9); err != nil {
		t.Fatal(err)
	}
	if store.createCalls != 2 || store.tasks["t1"].PRNumber != 9 || !slices.Equal(wf.started, []string{"t1"}) {
		t.Fatalf("inserts %d, row %+v, started %v: want the row inserted with pull request #9", store.createCalls, store.tasks["t1"], wf.started)
	}
}

// fileEntry gives every factory pull request source the roll's entry:
// ready's class, queued, no lane, the workflow started at once. The lane
// the writers' ready move gave these cards before (finding 224) queued
// them behind a session start their run never takes: its verify finds the
// pull request linked and moves in-review straight from queued, which the
// task state machine refuses.
func TestFactoryPRSourcesEnterLikeARoll(t *testing.T) {
	ctx := context.Background()
	for name, source := range map[string]string{
		"github sync": SourceGitHubSync, "schema copy": SourceSchemaCopy, "template update": SourceTemplateUpdate,
	} {
		t.Run(name, func(t *testing.T) {
			f, labels := fileStub(t)
			store, wf := newFakeStore(), &fakeWorkflows{}
			got, _, err := FileFactoryPRTask(ctx, store, f, wf, nil, testCardBudget, seedProject("sf/r"), "title", "body", []string{"improvement"},
				Task{ID: "t1", Source: source}, 9)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "queued" || got.Lane != "" || got.Process != "" ||
				!slices.Equal(got.RequiredCapabilities, []string{policy.ClassCoder}) ||
				store.tasks["t1"].PRNumber != 9 || !slices.Equal(wf.started, []string{"t1"}) {
				t.Fatalf("task = %+v, stored %+v, started %v; want the roll's entry with pull request #9 linked", got, store.tasks["t1"], wf.started)
			}
			if !slices.Equal(*labels, []string{"improvement", "state/queued"}) {
				t.Fatalf("labels %v", *labels)
			}
		})
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
	// a factory pull request card without its number adopts no branch's
	r4, _, _ := newActivityRunner(t, withSource(withStatus(prTask, "queued"), SourceTemplateUpdate), forked)
	if st4 := reconcileInto(t, r4); !st4.FactoryPR || st4.PR.Number != 0 || st4.Head != "" {
		t.Fatalf("RunState = %+v, want no pull request", st4)
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
		SHA: "base", IssueNumber: 4, Source: SourceSchemaCopy, PRNumber: 9, RequiredCapabilities: []string{policy.ClassCoder}}
	r, store, _ := newActivityRunner(t, card, &fj.prForgejo)
	ll := &fakeLiteLLM{content: `{"verdict":"approve","summary":"the copy matches upstream","findings":[]}`}
	r.supervisor = testSupervisorModel(ll.srv(t).URL)
	pinned := fixtureVersion(t)
	r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	r.cfg.Policy = func() policy.Version { return pinned }

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
	resVal, resErr := execActivity(t, r, ActivityMerge, "t1", int64(9), "h9", "sebastian", "")
	res := activityInto[MergeResult](t, resVal, resErr)
	if !res.Merged {
		t.Fatalf("MergeResult = %+v, want the approved merge of the card's pull request", res)
	}
	if _, err := execActivity(t, r, ActivityFinish, "t1", "done", "pull request merged"); err != nil {
		t.Fatal(err)
	}
	row, _, _ := store.Task(context.Background(), "t1")
	if row.Status != "merged" {
		t.Fatalf("row status = %q, want merged: its chain has not run the change yet", row.Status)
	}
	calls := fj.callList()
	for _, call := range calls {
		if strings.HasPrefix(call, "POST /api/v1/repos/sf/repo/pulls ") || strings.Contains(call, "/pulls/main/") {
			t.Fatalf("calls %v: the card's pull request was adopted, never opened nor looked up by branch", calls)
		}
	}
}

// The card of each factory pull request source runs its linked pull
// request through the gates the way a roll does: running first, then
// in-review past the stages. The state machine refuses in-review from
// queued (finding 224), so the run's states say running before in-review.
// No session, no RollCommit: nothing writes the card's branch, the pull
// request exists; the adopted head reaches the gates as it is.
func TestFactoryPRCardRunsItsGatesFromRunning(t *testing.T) {
	for name, source := range map[string]string{
		"github sync": SourceGitHubSync, "schema copy": SourceSchemaCopy, "template update": SourceTemplateUpdate,
	} {
		t.Run(name, func(t *testing.T) {
			g := newGateHarness(t)
			g.state = RunState{Status: "queued", FactoryPR: true, Source: source,
				PR: PullRequest{Number: 9, URL: "https://git.example/sf/repo/pulls/9"}, Head: "c7"}
			g.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, in SessionInput) (SessionResult, error) {
					t.Errorf("a factory pull request card ran a session: %+v", in)
					return SessionResult{}, nil
				})
			g.env.OnActivity(ActivityRollCommit, mock.Anything, "t1").Return(
				func(ctx context.Context, taskID string) (string, error) {
					t.Errorf("a factory pull request card ran RollCommit: nothing writes its branch")
					return "", nil
				})
			g.env.OnActivity(ActivityOpenPullRequest, mock.Anything, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, taskID, sha string) (PullRequest, error) {
					t.Errorf("OpenPullRequest called for head %s: the card's pull request was already open", sha)
					return PullRequest{Number: 9}, nil
				})
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			if final := g.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, g.finals)
			}
			if want := []string{"policy", "repro c7", "review c7", "criteria c7", "approval c7", "merge"}; !slices.Equal(g.steps, want) {
				t.Fatalf("steps = %v, want %v: the linked pull request runs the gates", g.steps, want)
			}
			if !slices.Equal(g.states, []string{"running", "in-review"}) {
				t.Fatalf("states = %v, want running before in-review: in-review from queued is refused", g.states)
			}
			if !slices.Equal(g.merges, []string{"9 c7 sebastian"}) {
				t.Fatalf("merges = %v", g.merges)
			}
			if len(g.sessions) != 0 {
				t.Fatalf("sessions = %v, want none: the card's work is the open pull request", g.sessions)
			}
		})
	}
}

// A factory pull request card the heal pass restarts in a waiting state
// (a gate's escalation, an information request) is known by its source
// first: its run takes the gates on its linked pull request again, asks
// no one again and starts no session, which the waiting state's answer
// would otherwise buy it.
func TestFactoryPRCardRestartedWhileWaitingRunsNoSession(t *testing.T) {
	for _, st := range []RunState{
		{Status: "waiting-human", Question: "the review's supervisor asks whether the copy may merge"},
		{Status: "waiting-poster", InfoRequest: 651, PosterInfo: []string{"visitor: Linux"}, InfoAnswered: true},
	} {
		t.Run(st.Status, func(t *testing.T) {
			g := newGateHarness(t)
			st.FactoryPR, st.Source, st.PR, st.Head = true, SourceSchemaCopy, PullRequest{Number: 9}, "c7"
			g.state = st
			g.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
				func(ctx context.Context, in SessionInput) (SessionResult, error) {
					t.Errorf("a factory pull request card ran a session: %+v", in)
					return SessionResult{}, nil
				})
			g.onChecks(ChecksResult{State: ChecksSuccess})
			g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
			if final := g.run(t); final != "done" {
				t.Fatalf("final = %q, finals %v", final, g.finals)
			}
			if !slices.Equal(g.states, []string{"running", "in-review"}) || len(g.escalated) != 0 || !slices.Equal(g.merges, []string{"9 c7 sebastian"}) {
				t.Fatalf("states %v, escalations %q, merges %v: want the gates again from running and no question", g.states, g.escalated, g.merges)
			}
		})
	}
}

// A factory pull request card with no pull request linked, which
// FileFactoryPRTask no longer files, fails at once when a run meets one:
// no session can do its work.
func TestFactoryPRCardWithoutItsPullRequestFails(t *testing.T) {
	g := newGateHarness(t)
	g.state = RunState{Status: "queued", FactoryPR: true, Source: SourceSchemaCopy}
	g.env.OnActivity(ActivityRunSession, mock.Anything, mock.Anything).Return(
		func(ctx context.Context, in SessionInput) (SessionResult, error) {
			t.Errorf("a factory pull request card ran a session: %+v", in)
			return SessionResult{}, nil
		})
	if final := g.run(t); final != "failed" || g.finals[0][1] != "factory pull request: none is linked to the card" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.states) != 0 || len(g.steps) != 0 {
		t.Fatalf("states %v, steps %v: want no gate", g.states, g.steps)
	}
}

// A factory pull request card has no agent (versionFactoryPRFindings): the
// findings a stage would send an agent's card back with, red checks and
// the review's concerns, go to a human's approval, whose card names each
// once; no session runs and the card never fails for want of an agent.
// The approval merges it. A run from before the change fails on them.
func TestFactoryPRCardTakesItsFindingsToAHuman(t *testing.T) {
	red := "check \"pr / test (pull_request)\": failure"
	concern := "ci.yml: the smoke job has no trigger"
	factoryCard := func(g *gateHarness) {
		g.review = Review{Verdict: "concerns", Summary: "the smoke job never runs", Findings: []string{concern}}
		g.state = RunState{Status: "queued", FactoryPR: true, Source: SourceTemplateUpdate, PR: PullRequest{Number: 9}, Head: "c7"}
		g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}})
		g.onHeadChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}})
	}
	g := newGateHarness(t, factoryCard)
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	want := []string{noAgent, "checks, round 1: " + red, "review, round 1: " + concern}
	if len(g.approvals) != 1 || !slices.Equal(g.approvals[0].WhyHuman, want) {
		t.Fatalf("approvals %+v, want one whose card is %q", g.approvals, want)
	}
	if !slices.Equal(g.merges, []string{"9 c7 sebastian"}) || len(g.sessions) != 0 {
		t.Fatalf("merges %v, sessions %v: want the approved merge and no session", g.merges, g.sessions)
	}

	old := newGateHarness(t, factoryCard)
	old.env.OnGetVersion("factory-pr-findings", workflow.DefaultVersion, versionFactoryPRFindings).Return(workflow.DefaultVersion)
	if final := old.run(t); final != "failed" || old.finals[0][1] != "factory pull request: checks findings went back to the agent: "+red {
		t.Fatalf("a run from before the change: final = %q, finals %v", final, old.finals)
	}
}

// A factory pull request card's approval fits its head (roadmap 264): on
// red checks its request says so, which the console turns into a re-run
// and a close in place of a merge; a re-run runs them again on an empty
// commit, waits for them and asks again, and on green the approval merges
// the new head.
func TestFactoryPRCardRerunsItsRedChecks(t *testing.T) {
	red := "check \"pr / test (pull_request)\": failure"
	g := newGateHarness(t, func(g *gateHarness) {
		g.state = RunState{Status: "queued", FactoryPR: true, Source: SourceTemplateUpdate, PR: PullRequest{Number: 9}, Head: "c7"}
	})
	g.onChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}}, ChecksResult{State: ChecksSuccess})
	g.onHeadChecks(ChecksResult{State: ChecksFailure, Findings: []string{red}}, ChecksResult{State: ChecksSuccess})
	var reruns []string
	g.env.OnActivity(ActivityRerunChecks, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, sha string) (string, error) {
		reruns = append(reruns, sha)
		return "c8", nil
	})
	g.signalAfter(time.Hour, SignalRerunChecks, TaskSignal{Author: "anna", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) != 2 || g.approvals[0].Commit != "c7" || !g.approvals[0].RedChecks || !g.approvals[0].Factory ||
		g.approvals[1].Commit != "c8" || g.approvals[1].RedChecks || !g.approvals[1].Factory {
		t.Fatalf("approvals %+v: want c7's on red checks, then c8's on green", g.approvals)
	}
	if !slices.Equal(reruns, []string{"c7"}) || !slices.Equal(g.merges, []string{"9 c8 sebastian"}) || len(g.sessions) != 0 {
		t.Fatalf("re-runs %v, merges %v, sessions %v: want c7's checks run again once and c8 merged, no session", reruns, g.merges, g.sessions)
	}
}

// A factory pull request card whose head's checks have not started when
// the approval reads it is pending, not red: a request opened on that
// read would record red checks, which the console turns into a refusal
// to merge — the card parks un-approvable. The approval waits the checks
// out (versionApprovalPending) and asks on their green conclusion, which
// the approval merges.
func TestFactoryPRCardWaitsOutItsPendingHead(t *testing.T) {
	pending := `no check has reported on c7 yet`
	g := newGateHarness(t, func(g *gateHarness) {
		g.state = RunState{Status: "queued", FactoryPR: true, Source: SourceTemplateUpdate, PR: PullRequest{Number: 9}, Head: "c7"}
	})
	g.onChecks(ChecksResult{State: ChecksSuccess})
	g.onHeadChecks(ChecksResult{State: ChecksPending, Findings: []string{pending}})
	g.signalAfter(time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) != 1 || g.approvals[0].Commit != "c7" || g.approvals[0].RedChecks || !g.approvals[0].Factory {
		t.Fatalf("approvals %+v: want one factory request on c7 that says nothing of red checks", g.approvals)
	}
	if !slices.Equal(g.merges, []string{"9 c7 sebastian"}) || len(g.sessions) != 0 {
		t.Fatalf("merges %v, sessions %v: want the approved merge, no session", g.merges, g.sessions)
	}
}

// A factory pull request card whose pending head waits out red asks on
// the red conclusion, not the read: the request records red checks, which
// the console answers with the re-run and the close in place of a merge,
// and a re-run asks again on the green head it makes.
func TestFactoryPRCardWaitsOutItsPendingHeadToRed(t *testing.T) {
	red := `check "pr / test": failure — 1 test failed (https://git.example/sf/r/actions/runs/95)`
	pending := `no check has reported on c7 yet`
	g := newGateHarness(t, func(g *gateHarness) {
		g.state = RunState{Status: "queued", FactoryPR: true, Source: SourceTemplateUpdate, PR: PullRequest{Number: 9}, Head: "c7"}
	})
	g.onChecks(ChecksResult{State: ChecksSuccess}, ChecksResult{State: ChecksFailure, Findings: []string{red}}, ChecksResult{State: ChecksSuccess})
	g.onHeadChecks(ChecksResult{State: ChecksPending, Findings: []string{pending}}, ChecksResult{State: ChecksSuccess})
	var reruns []string
	g.env.OnActivity(ActivityRerunChecks, mock.Anything, "t1", mock.Anything).Return(func(ctx context.Context, taskID, sha string) (string, error) {
		reruns = append(reruns, sha)
		return "c8", nil
	})
	g.signalAfter(time.Hour, SignalRerunChecks, TaskSignal{Author: "anna", PR: 9})
	g.signalAfter(2*time.Hour, SignalApproved, TaskSignal{Author: "sebastian", PR: 9})
	if final := g.run(t); final != "done" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) != 2 || g.approvals[0].Commit != "c7" || !g.approvals[0].RedChecks || !g.approvals[0].Factory ||
		g.approvals[1].Commit != "c8" || g.approvals[1].RedChecks || !g.approvals[1].Factory {
		t.Fatalf("approvals %+v: want c7's on the wait's red conclusion, then c8's on green", g.approvals)
	}
	if !slices.Equal(reruns, []string{"c7"}) || !slices.Equal(g.merges, []string{"9 c8 sebastian"}) || len(g.sessions) != 0 {
		t.Fatalf("re-runs %v, merges %v, sessions %v: want c7's checks run again once and c8 merged, no session", reruns, g.merges, g.sessions)
	}
}

// A change request on a factory pull request card closes it with the
// human's words, since no agent answers it (roadmap 264): Finish closes
// the pull request and the issue with them. A run from before the change
// fails it as before.
func TestFactoryPRCardClosesOnAChangeRequest(t *testing.T) {
	factoryCard := func(g *gateHarness) {
		g.state = RunState{Status: "queued", FactoryPR: true, Source: SourceSchemaCopy, PR: PullRequest{Number: 9}, Head: "c7"}
		g.onChecks(ChecksResult{State: ChecksSuccess})
		g.signalAfter(time.Hour, SignalChangesRequested, TaskSignal{Author: "anna", PR: 9, Body: "the copy is of the wrong build"})
	}
	g := newGateHarness(t, factoryCard)
	if final := g.run(t); final != "closed" ||
		g.finals[0][1] != factoryPRClosed+": reviewer anna requested changes: the copy is of the wrong build" {
		t.Fatalf("final = %q, finals %v", final, g.finals)
	}
	if len(g.approvals) != 1 || g.approvals[0].RedChecks || len(g.merges) != 0 {
		t.Fatalf("approvals %+v, merges %v: want one request on green checks and no merge", g.approvals, g.merges)
	}

	old := newGateHarness(t, factoryCard)
	old.env.OnGetVersion("factory-pr-actions", workflow.DefaultVersion, versionFactoryPRActions).Return(workflow.DefaultVersion)
	if final := old.run(t); final != "failed" ||
		old.finals[0][1] != "factory pull request: changes requested: reviewer anna requested changes: the copy is of the wrong build" {
		t.Fatalf("a run from before the change: final = %q, finals %v", final, old.finals)
	}
	if len(old.approvals) != 1 || old.approvals[0].Factory {
		t.Fatalf("a run from before the change asked %+v: want a request that offers no factory actions, which it does not hear", old.approvals)
	}
}

// The task state machine refuses in-review from queued, the move the
// factory pull request card's old lane entry asked of its run (finding
// 224); the gate path enters from running.
func TestSetStateRefusesInReviewFromQueued(t *testing.T) {
	r, _, _ := newActivityRunner(t, withStatus(prTask, "queued"), &prForgejo{})
	_, err := execActivity(t, r, ActivitySetState, "t1", "in-review")
	if err == nil || !strings.Contains(err.Error(), "task t1 is queued, cannot move to in-review") {
		t.Fatalf("err = %v, want the queued card's refusal", err)
	}
	if _, err := execActivity(t, r, ActivitySetState, "t1", "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := execActivity(t, r, ActivitySetState, "t1", "in-review"); err != nil {
		t.Fatal(err)
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
