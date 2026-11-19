package pg

// The rig family the round tests need, moved with round3_pg_test.go and
// round4_pg_test.go: copies of factoryd's test-file helpers (scriptedChat,
// rungRig, boardStub, the rigs), reached only inside this package. The
// factoryd copies stay for the act and rung files that remain there.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// scriptedChat answers a check-in's conversations in order. Fails makes
// the first calls error before the answers apply; delay holds each call,
// honouring the caller's context, so a session's deadline cuts it; onCall
// runs at each call's start.
type scriptedChat struct {
	answers []CheckinResponse
	calls   int
	got     CheckinRequest
	gots    []CheckinRequest
	aliases []string
	fails   int
	delay   time.Duration
	onCall  func()
}

func (c *scriptedChat) Call(ctx context.Context, _, alias string, in CheckinRequest) (CheckinResponse, error) {
	c.got = in
	c.gots = append(c.gots, in)
	c.aliases = append(c.aliases, alias)
	c.calls++
	if c.onCall != nil {
		c.onCall()
	}
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return CheckinResponse{}, ctx.Err()
		}
	}
	if n := c.calls - c.fails; n < 1 || n > len(c.answers) {
		return CheckinResponse{}, errors.New("provider refused")
	}
	return c.answers[c.calls-c.fails-1], nil
}

const rungFindingText = "TasksByPR is undefined"

// rungRig is tripRig's card t1 with a check-in model: a medium card of the
// coder class, whose head h2 on pull request 9 is green and not approved,
// and the review's one finding, rungFindingText, on h2. The check-in's
// model is a script that notes the task each call booked on.
type rungRig struct {
	tripRig
	chat *idChat
	id   string // the finding's id
}

type idChat struct {
	scriptedChat
	mu    sync.Mutex
	ids   []string
	cards []string // each call's X-Factory-Card, as the check-in client sends it
}

func (c *idChat) Call(ctx context.Context, taskID, alias string, in CheckinRequest) (CheckinResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids, c.cards = append(c.ids, taskID), append(c.cards, factoryd.PgCardHeader(ctx))
	return c.scriptedChat.Call(ctx, taskID, alias, in)
}

func newRungRig(t *testing.T, checks string, answers ...CheckinResponse) rungRig {
	t.Helper()
	ctx := context.Background()
	g := rungRig{tripRig: newTripRig(t, checks), chat: &idChat{scriptedChat: scriptedChat{answers: answers}}, id: FindingID(rungFindingText)}
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET required_capabilities = '["coder"]'::jsonb, size = 'medium', budget_eur = 5 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.atHead(t, false)
	if err := g.r.PgTimeline().Append(ctx, "t1", "model_review", map[string]any{"commit": "h2", "verdict": "concerns", "findings": []string{rungFindingText}}); err != nil {
		t.Fatal(err)
	}
	g.r.Checkin = g.chat
	g.r.PgSetPrices(aliasPrices{"big": {In: 3e-6, Out: 1.5e-5, MaxOutput: 32768}})
	return g
}

// atHead puts pull request #9 on t1 with head h2, whose newest review
// approved it when approved.
func (g tripRig) atHead(t *testing.T, approved bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET pr_number = 9 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	verdict := "concerns"
	if approved {
		verdict = "approve"
	}
	for _, e := range []Event{
		{Kind: EventGateHead, Detail: map[string]any{"gate": GateMerge, "head": "h2"}},
		{Kind: EventModelReview, Detail: map[string]any{"commit": "h2", "round": 1, "reviewed": true, "verdict": verdict}},
	} {
		e.Repository, e.TaskID, e.IssueNumber, e.Actor, e.Via = "demo-app/demo-app", "t1", 12, FactoryDecider, ViaFactoryd
		if _, err := g.s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
}

// boardStub is the console's side of a board action: it records its calls
// and decides the merge request as the console's decision does.
type boardStub struct {
	mu       sync.Mutex
	calls    []ActInput
	err      error
	s        *factoryd.PGStore
	approval int64
}

// act answers through Act with the stub's work.
func (b *boardStub) act(r *Runner, in ActInput) (ActResult, error) {
	in.Work = func(ctx context.Context) (int64, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.calls = append(b.calls, in)
		if b.err != nil {
			return 0, b.err
		}
		switch in.Res.Action {
		case ActApproveMerge, ActDenyMerge:
			status := map[string]string{ActApproveMerge: "approved", ActDenyMerge: "denied"}[in.Res.Action]
			return 0, b.s.DecideApproval(ctx, b.approval, ApprovalMerge, status, in.By, in.Res.Note)
		case ActGrantCriteria:
			return 0, b.s.DecideApproval(LeaveCaseToAct(ctx), b.approval, ApprovalCriteria, "approved", in.By, "one more bounded card")
		case ActRestartPlan:
			return 0, b.s.ClosePlanWithWaivers(LeaveCaseToAct(ctx), in.TaskID, in.By, func(context.Context) (PlanClose, error) {
				return PlanClose{Home: in.TaskID, Words: "Closed by " + in.By + ": " + in.Res.Note}, nil
			})
		}
		return 0, nil
	}
	return r.Act(context.Background(), in)
}

// mergeRig is the rung rig with a pending merge request on t1's pull request 9
// at head h2, whose case is open at rung 0.
func mergeRig(t *testing.T, checks string) (rungRig, *boardStub, int64) {
	t.Helper()
	g := newRungRig(t, checks)
	spec, err := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h2"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := g.s.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
	stub := &boardStub{s: g.s, approval: id}
	return g, stub, id
}

// mergeWakeRig is the rung 2 rig at autonomy state whose card t1 is in
// review, its pull request 9 at head h2 reviewed and audited, with a
// pending merge request of class standard at h2, whose case is open at
// rung 0 and due at the request's opening, with no step yet; the stub
// decides the request.
func mergeWakeRig(t *testing.T, state string) (rungRig, *Waker, *fakeWakeStarter, *boardStub, Case) {
	t.Helper()
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	g.atAutonomy(state)
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'in-review' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.record(t, EventGateAudit, map[string]any{"commit": "h2", "ran": true})
	spec, err := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h2"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9), "class": "standard", "factory": false, "red_checks": false})
	c := g.liveCase(t, CaseMergeApproval)
	var opened time.Time
	if err := g.s.PgDB().QueryRowContext(ctx, `SELECT created_at FROM approval_requests WHERE id = $1`, id).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	if c.Status != CaseOpen || c.OwnerRung != 0 || c.NextWakeAt == nil || !c.NextWakeAt.Equal(opened) || len(g.steps(t, c.ID)) != 0 {
		t.Fatalf("case = %+v with steps %+v, want it open at rung 0, due at the request's opening %s, with no step", c, g.steps(t, c.ID), opened)
	}
	return g, w, starter, &boardStub{s: g.s, approval: id}, c
}

// approveWork is the console's approve of t1's pull request 9 as approver,
// whose decision signals wf: Act's work for approve_merge.
func (g rungRig) approveWork(wf Workflows, approver string) func(context.Context) (int64, error) {
	return func(ctx context.Context) (int64, error) {
		at := func(context.Context, int64) (policy.Version, bool) { return g.r.PgCfg().Policy(), true }
		return 0, ApproveMerge(ctx, g.s, at, wf, "t1", 9, approver)
	}
}

func (g rungRig) record(t *testing.T, kind string, detail map[string]any) {
	t.Helper()
	if _, err := g.s.Record(context.Background(), Event{Repository: "demo-app/demo-app", TaskID: "t1", IssueNumber: 12, Kind: kind,
		Actor: FactoryDecider, Via: ViaFactoryd, Detail: detail}); err != nil {
		t.Fatalf("Record %s: %v", kind, err)
	}
}

func (g rungRig) liveCase(t *testing.T, kind CaseKind) Case {
	t.Helper()
	open, err := g.s.OpenCasesOf(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range open {
		if c.Kind == kind {
			return c
		}
	}
	t.Fatalf("t1 holds no live %s case: %+v", kind, open)
	return Case{}
}

// criteriaRig is the rung rig whose card t1 is a done plan, the only state
// in which a criteria request opens, with its request's plan_criteria case
// open at rung 0, due at the request's opening, with no step yet.
func criteriaRig(t *testing.T) (rungRig, *boardStub, Case) {
	t.Helper()
	g, stub, _ := mergeRig(t, "success")
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'done', track = 'plan', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(CriteriaSpec{Task: "t1"})
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalCriteria, TaskID: "t1", Project: "demo-app/demo-app", Spec: spec, Reason: "criteria not met: c1"})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalCriteria})
	stub.approval = id
	c := g.liveCase(t, CasePlanCriteria)
	var opened time.Time
	if err := g.s.PgDB().QueryRowContext(ctx, `SELECT created_at FROM approval_requests WHERE id = $1`, id).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	if c.Status != CaseOpen || c.OwnerRung != 0 || c.NextWakeAt == nil || !c.NextWakeAt.Equal(opened) || c.Problem != "criteria not met: c1" || len(g.steps(t, c.ID)) != 0 {
		t.Fatalf("case = %+v with steps %+v, want it open at rung 0, due at the request's opening %s, with the request's reason and no step", c, g.steps(t, c.ID), opened)
	}
	return g, stub, c
}

// criteriaFace is the words planCriteriaRig's request asks its face to post.
const criteriaFace = "Every card of the plan has ended with a criterion still unmet.\n\n- k1 (test): the page k1 loads\n- k2 (test): the page k2 loads\n- k3 (test): the page k3 loads\n\nGrant one more bounded card, or waive a criterion with your reason."

// planCriteriaRig is the rung 2 rig whose card t1 is a done plan with three
// test criteria, k1 to k3, which the gate rows of its integration card
// t1-int, done at 09:00 on 17 November, found unmet, on a main whose head
// is head. The gate's runs on record are one cut run at each of cuts,
// attempt after attempt, for the cards as they stand. The plan's criteria
// request waits, as the close check opens it, with the three promises it
// asks about and criteriaFace as its face's words, and its case is open;
// the stub decides the request.
func planCriteriaRig(t *testing.T, head string, cuts ...string) (rungRig, *Waker, *fakeWakeStarter, *boardStub, Case) {
	t.Helper()
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'done', track = 'plan', finished_at = now() WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	g.forge.mu.Lock()
	g.forge.mainHead = head
	g.forge.mu.Unlock()
	p := Plan{Summary: "the pages", Cards: []PlanCard{{Key: "integration", Role: RoleIntegration, Title: "Integration", Directories: []string{"a"}}}}
	var ids []string
	for _, id := range []string{"k1", "k2", "k3"} {
		p.Criteria = append(p.Criteria, PlanCriterion{ID: id, Kind: CriterionTest, Claim: "the page " + id + " loads", Inputs: "GET /" + id, Expected: "200"})
		ids = append(ids, id)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.AddPlanRevision(ctx, "t1", b, "sha"); err != nil {
		t.Fatal(err)
	}
	if err := g.s.CreateTask(ctx, Task{ID: "t1-int", Kind: "agent", Status: "done", Objective: "x", Repository: "demo-app/demo-app",
		Parent: "t1", Source: SourcePlan, PlanKey: "integration", Track: policy.TrackPlan, Size: "large", BudgetEUR: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET finished_at = '2026-11-17T09:00:00Z' WHERE id = 't1-int'`); err != nil {
		t.Fatal(err)
	}
	var rows []CriterionVerdict
	for _, id := range ids {
		rows = append(rows, CriterionVerdict{Criterion: id, Verdict: "unmet", Kind: CriterionTest})
	}
	if err := g.s.PutCriteriaVerdicts(ctx, "t1-int", CriteriaSourceGate, FactoryDecider, rows); err != nil {
		t.Fatal(err)
	}
	for i, commit := range cuts {
		g.record(t, EventCriteriaGateRun, map[string]any{"task": "t1-int", "pr": 0, "commit": commit, "outcome": "cut", "attempt": fmt.Sprint(i + 1),
			"cards": "rev 1 t1-int:done@2026-11-17T09:00:00Z"})
	}
	spec, err := json.Marshal(CriteriaSpec{Task: "t1-int", Criteria: ids, Open: ids, Of: len(ids), Face: criteriaFace})
	if err != nil {
		t.Fatal(err)
	}
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalCriteria, TaskID: "t1", Project: "demo-app/demo-app", Spec: spec,
		Reason: "criteria not met once every card of the plan ended: k1, k2, k3"})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalCriteria, "criteria": ids})
	return g, w, starter, &boardStub{s: g.s, approval: id}, g.liveCase(t, CasePlanCriteria)
}

// planWakeRig is the rung 2 rig at autonomy state whose card t1 is a plan
// waiting for its answers at revision, under plan: its questions qs come
// through AskQuestions, a confirmation among them as AskConfirmation's row
// asks it, and its plan request is pending. Each question's case is open
// at rung 0 with no step. It answers the request's id.
func planWakeRig(t *testing.T, state string, revision int, plan Plan, qs ...PlanQuestion) (rungRig, *Waker, *fakeWakeStarter, int64) {
	t.Helper()
	g, w, starter := rungTwoRig(t, policy.JudgmentAlways)
	g.atAutonomy(state)
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'waiting-human', track = 'plan' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= revision; n++ {
		if _, err := g.s.AddPlanRevision(ctx, "t1", raw, fmt.Sprint("s", n)); err != nil {
			t.Fatal(err)
		}
	}
	task, _, err := g.r.PgTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var asked []PlanQuestion
	for _, q := range qs {
		if q.Kind != QuestionConfirmation {
			asked = append(asked, q)
		}
	}
	if err := g.r.AskQuestions(ctx, task, revision, asked); err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		if q.Kind != QuestionConfirmation {
			continue
		}
		idx, _, err := g.s.PutPlanConfirmation(ctx, "t1", revision, q)
		if err != nil {
			t.Fatal(err)
		}
		g.record(t, EventQuestionAsked, map[string]any{"question": q.Text, "revision": revision, "index": idx, "kind": QuestionConfirmation})
	}
	spec, err := json.Marshal(factoryd.PlanSpec{Revision: revision, SHA256: fmt.Sprint("s", revision)})
	if err != nil {
		t.Fatal(err)
	}
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: factoryd.ApprovalPlan, TaskID: "t1", Project: "demo-app/demo-app", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": factoryd.ApprovalPlan, "revision": revision, "cards": len(plan.Cards)})
	return g, w, starter, id
}

// commentBodies are the bodies of the comments on t1's issue, oldest first,
// as the last write left each.
func (g rungRig) commentBodies(t *testing.T) []string {
	t.Helper()
	g.forge.mu.Lock()
	defer g.forge.mu.Unlock()
	out := make([]string, len(g.forge.comments))
	for i, raw := range g.forge.comments {
		var c struct{ Body string }
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
		out[i] = c.Body
	}
	return out
}

// wakePass runs the case-wake pass on g's store and runner a second past
// now, so the case its request opened is due.
func (g rungRig) wakePass(t *testing.T) {
	t.Helper()
	v := g.r.PgCfg().Policy()
	m := &Monitor{Store: g.s, Runner: g.r, Policy: func() policy.Tree { return v.Tree }, Now: func() time.Time { return time.Now().Add(time.Second) }}
	if err := m.WakeCases(context.Background()); err != nil {
		t.Fatalf("WakeCases: %v", err)
	}
}

// recordAsk writes the ask row of card t as a human card is born with it,
// for the tests that seed an ask on a card they made themselves.
func recordAsk(ctx context.Context, tl *Timeline, t Task, q HumanQuestion, by, asker, via, decider string, facts map[string]any) error {
	return tl.WriteRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, via, HumanAsk{Q: q, By: by, Asker: asker, Via: via, Decider: decider, Facts: facts}.Event(t).Detail)
}
