package pg

// The rig family the round tests need, moved with round3_pg_test.go and
// round4_pg_test.go: copies of factoryd's test-file helpers (scriptedChat,
// rungRig, boardStub, the rigs), reached only inside this package. The
// factoryd copies stay for the act and rung files that remain there.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
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
// at head h2, whose case is at the human.
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
// in which a criteria request opens, with its request's plan_criteria case at
// the human.
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
	if c.Status != CaseAtHuman || c.Problem != "criteria not met: c1" {
		t.Fatalf("case = %+v, want at the human with the request's reason", c)
	}
	return g, stub, c
}

// recordAsk writes the ask row of card t as a human card is born with it,
// for the tests that seed an ask on a card they made themselves.
func recordAsk(ctx context.Context, tl *Timeline, t Task, q HumanQuestion, by, asker, via, decider string, facts map[string]any) error {
	return tl.WriteRow(ctx, t.ID, EventQuestionAsked, FactoryDecider, via, HumanAsk{Q: q, By: by, Asker: asker, Via: via, Decider: decider, Facts: facts}.Event(t).Detail)
}
