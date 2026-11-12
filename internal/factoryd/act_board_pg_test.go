package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// boardStub is the console's side of a board action: it records its calls
// and decides the merge request as the console's decision does.
type boardStub struct {
	mu       sync.Mutex
	calls    []ActInput
	err      error
	s        *PGStore
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
		case ActDenyCriteria:
			return 0, b.s.DecideApproval(ctx, b.approval, ApprovalCriteria, "denied", in.By, in.Res.Note)
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

// The human's approve_merge on a merge case whose head h2 has green checks
// runs the console's work once, and the request's decision ends the case:
// resolved by the human on rung 3 with the action approve_merge, and the
// answer's own step, resolved, names it.
func TestApproveMergeAtRung3WithGreenChecksRunsAndResolves(t *testing.T) {
	g, stub, _ := mergeRig(t, "success")
	c := g.liveCase(t, CaseMergeApproval)
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActApproveMerge, Note: "reads well"}}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(stub.calls) != 1 || stub.calls[0].Res.Action != ActApproveMerge {
		t.Fatalf("board calls = %+v, want one approve_merge", stub.calls)
	}
	got := caseByID(t, g.s, c.ID)
	if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Action != ActApproveMerge || got.Resolution.By != "sebastian" || got.Resolution.Rung != 3 {
		t.Fatalf("case = %+v, want resolved by sebastian on rung 3 with approve_merge", got)
	}
	var own *CaseStep
	for _, s := range g.steps(t, c.ID) {
		if s.Decision == ActApproveMerge {
			own = &s
		}
	}
	if own == nil || own.Outcome != StepResolved || own.Grounds != "reads well" || len(own.Did) != 1 || own.Did[0].Result != "approve_merge" {
		t.Fatalf("steps = %+v, want a resolved step that decided approve_merge with the console's result", g.steps(t, c.ID))
	}
}

// Red checks on the head refuse the approval at once, naming the checks;
// the console's work never runs and the case stays at the human.
func TestApproveMergeWithRedChecksIsRefusedNamingThem(t *testing.T) {
	g, stub, _ := mergeRig(t, "failure")
	c := g.liveCase(t, CaseMergeApproval)
	_, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "the checks on h2 are red, not green") {
		t.Fatalf("Act = %v, want a refusal naming the red checks", err)
	}
	if len(stub.calls) != 0 || caseByID(t, g.s, c.ID).Status != CaseAtHuman {
		t.Fatalf("board calls %d, case %s: a refused approval runs nothing and ends nothing", len(stub.calls), caseByID(t, g.s, c.ID).Status)
	}
}

// Rung 2's approval is refused with 2.8's text when a gate did not run on
// the card (the rig holds a review and no audit row, and the process lists
// both), and rung 1 may not pick approve_merge at all.
func TestApproveMergeAtRung2NeedsTheGates(t *testing.T) {
	g, stub, _ := mergeRig(t, "success")
	g.atAutonomy(policy.AutonomyManual)
	// a push after the review: the review of the new head did not run
	g.record(t, EventSessionPushed, map[string]any{"head": "h2"})
	c := g.liveCase(t, CaseMergeApproval)
	for _, tc := range []struct {
		rung int
		by   string
		want string
	}{
		{2, ChatUser, "The review did not run, so the project agent cannot approve this merge."},
	} {
		_, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: tc.by, Via: ViaChat, Rung: tc.rung, Res: Resolution{Action: ActApproveMerge}})
		var refused ErrActRefused
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("rung %d: Act = %v, want a refusal with %q", tc.rung, err, tc.want)
		}
	}
	if len(stub.calls) != 0 {
		t.Fatalf("board calls = %+v, want none", stub.calls)
	}
}

// At autonomous rung 2's approve_merge is a proposal: nothing is decided and
// the case stays at the human.
func TestApproveMergeAtAutonomousRung2IsAProposal(t *testing.T) {
	g, stub, _ := mergeRig(t, "success")
	g.atAutonomy(policy.AutonomyAutonomous)
	c := g.liveCase(t, CaseMergeApproval)
	res, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActApproveMerge, Note: "ok"}})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if !res.Proposed || len(stub.calls) != 0 {
		t.Fatalf("result %+v, board calls %d, want a proposal and no call", res, len(stub.calls))
	}
}

// criteriaRig is the rung rig whose card t1 is a done plan, the only state
// in which a criteria request opens, with its request's plan_criteria case at
// the human.
func criteriaRig(t *testing.T) (rungRig, *boardStub, Case) {
	t.Helper()
	g, stub, _ := mergeRig(t, "success")
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET status = 'done', track = 'plan', finished_at = now() WHERE id = 't1'`); err != nil {
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

// A done plan's criteria request is answered through Act (the ended-card
// check lets the criteria's actions and the restart through): a verdict keeps
// the case at the human; a grant, a confirmation and a denial each resolve it
// under their own name; restart_plan resolves it too.
func TestCriteriaActionsRunOnADonePlansCase(t *testing.T) {
	for _, tc := range []struct {
		action  string
		args    map[string]any
		note    string
		status  string
		resolve string
	}{
		{ActAnswerCriteria, map[string]any{"criterion": "c1", "verdict": "waive"}, "out of scope", CaseAtHuman, ""},
		{ActGrantCriteria, nil, "", CaseResolved, ActGrantCriteria},
		{ActConfirmCriteria, nil, "the cards deliver it", CaseResolved, ActConfirmCriteria},
		{ActDenyCriteria, nil, "not now", CaseResolved, ActDenyCriteria},
		{ActRestartPlan, nil, "split it in two", CaseResolved, ActRestartPlan},
	} {
		t.Run(tc.action, func(t *testing.T) {
			g, stub, c := criteriaRig(t)
			if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
				Res: Resolution{Action: tc.action, Args: tc.args, Note: tc.note}}); err != nil {
				t.Fatalf("Act: %v", err)
			}
			got := caseByID(t, g.s, c.ID)
			if len(stub.calls) != 1 || got.Status != tc.status || tc.resolve != "" && got.Resolution.Action != tc.resolve {
				t.Fatalf("board calls %d, case %+v; want one call and status %s resolved with %q", len(stub.calls), got, tc.status, tc.resolve)
			}
			var step *CaseStep
			for _, s := range g.steps(t, c.ID) {
				if s.Decision == tc.action {
					step = &s
				}
			}
			if step == nil || step.Grounds != tc.note {
				t.Fatalf("steps = %+v, want one that decided %s with the note %q", g.steps(t, c.ID), tc.action, tc.note)
			}
		})
	}
}

// A merge case is about one head (#1091). The red head's case refuses the
// approval; once the branch moves, the new head's request withdraws the old
// case and opens its own, whose green head takes the approval, while the old
// case, withdrawn, takes none.
func TestAMergeCaseIsAboutOneHead(t *testing.T) {
	g, stub, _ := mergeRig(t, "failure")
	ctx := context.Background()
	old := g.liveCase(t, CaseMergeApproval)
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: old.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}}); err == nil ||
		!strings.Contains(err.Error(), "the checks on h2 are red, not green") {
		t.Fatalf("approve on the red head = %v, want the refusal naming the red checks", err)
	}
	g.forge.checks["h3"] = "success"
	spec, _ := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h3"})
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
	if got := caseByID(t, g.s, old.ID); got.Status != CaseWithdrawn {
		t.Fatalf("the red head's case is %s, want withdrawn by the new request", got.Status)
	}
	fresh := g.liveCase(t, CaseMergeApproval)
	if fresh.Facts["head"] != "h3" {
		t.Fatalf("the new case's head = %v, want h3", fresh.Facts["head"])
	}
	stub.approval = id
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: fresh.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}}); err != nil {
		t.Fatalf("approve on the green head: %v", err)
	}
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: old.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}}); err == nil {
		t.Fatal("the withdrawn case took an approval")
	}
}

// One send: a session cap answered with handover (model same), a draw of
// 1.50 EUR and a note is one Act call: one pool card_funded row of 1.50 on
// rung 3, the case resolved with handover, the note on the step.
func TestOneSendCarriesTheDecisionTheFundAndTheNote(t *testing.T) {
	g := newRungRig(t, "success")
	g.atAutonomy(policy.AutonomyManual)
	ctx := context.Background()
	if _, err := g.s.db.ExecContext(ctx, `UPDATE tasks SET ceiling_eur = 10 WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	out := g.trip(t, tripCap)
	g.settle()
	if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: out.CaseID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActHandover, Args: map[string]any{"model": "same"}, Fund: &policy.Draw{EUR: 1.5}, Note: "go on with the cache"}}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	var rows int
	var eur float64
	var rung float64
	if err := g.s.db.QueryRowContext(ctx, `SELECT count(*), COALESCE(max((detail->>'amount_eur')::float), 0), COALESCE(max((detail->>'rung')::float), 0)
		FROM events WHERE task_id = 't1' AND kind = $1 AND detail->>'pool' = 'true'`, EventCardFunded).Scan(&rows, &eur, &rung); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || eur != 1.5 || rung != 3 {
		t.Fatalf("card_funded rows %d, amount %v, rung %v; want one of 1.50 on rung 3", rows, eur, rung)
	}
	c := caseByID(t, g.s, out.CaseID)
	if c.Status != CaseResolved || c.Resolution.Action != ActHandover || c.Resolution.Reason != "go on with the cache" {
		t.Fatalf("case = %+v, want resolved with handover and the note", c)
	}
	var onStep bool
	for _, st := range g.steps(t, out.CaseID) {
		onStep = onStep || st.Decision == ActHandover && st.Grounds == "go on with the cache"
	}
	if !onStep {
		t.Fatalf("steps = %+v, want the note on the step that decided handover", g.steps(t, out.CaseID))
	}
}

// set_dependency on a dependency case of a card that has not started runs the
// console's work once and resolves the case with set_dependency, the edge's
// card among its args; on a case of a started card it is refused.
func TestSetDependencyResolvesTheDependencyCase(t *testing.T) {
	g, stub, _ := mergeRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "t2", Kind: "agent", Status: "queued", Objective: "y", Repository: "demo-app/demo-app", IssueNumber: 13, Track: policy.TrackDirect}); err != nil {
		t.Fatal(err)
	}
	if err := g.s.AddTaskDeps(ctx, "t2", []string{"t1"}); err != nil {
		t.Fatal(err)
	}
	c, _, err := g.s.OpenCase(ctx, Case{TaskID: "t2", Repository: "demo-app/demo-app", Kind: CaseDependencyFailed, StateKey: "d", Status: CaseAtHuman, Problem: "a dependency failed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stub.act(g.r, ActInput{TaskID: "t2", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActRemoveDependency, Args: map[string]any{"depends_on": "t1"}}}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	got := caseByID(t, g.s, c.ID)
	if len(stub.calls) != 1 || got.Status != CaseResolved || got.Resolution.Action != ActRemoveDependency || got.Resolution.Args["depends_on"] != "t1" {
		t.Fatalf("board calls %d, case %+v; want one call and the case resolved with remove_dependency of t1", len(stub.calls), got)
	}
}

// Rung 2's approve_merge on a merge approval of class migration is refused
// through Act, on the policy version the class came from: the case's
// facts carry the human classes of the pinned version, a version that marks
// migration, while the live policy no longer marks it.
func TestRung2ApproveOfAMigrationMergeIsRefusedOnThePinnedPolicy(t *testing.T) {
	g := newRungRig(t, "success")
	pinned := policy.Version{ID: 9, Tree: projectTree(t)}
	live := policy.Version{ID: 11, Tree: projectTree(t)}
	live.Document.Risk.Classes = slices.Clone(pinned.Document.Risk.Classes)
	i := slices.IndexFunc(live.Document.Risk.Classes, func(c policy.RiskClass) bool { return c.Name == "migration" })
	if i < 0 || !pinned.Document.Risk.Classes[i].Migration {
		t.Fatal("the test policy has no migration class marked migration")
	}
	live.Document.Risk.Classes[i].Migration = false
	g.r.cfg.Policy = func() policy.Version { return live }
	g.r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == pinned.ID, nil }
	spec, err := json.Marshal(MergeSpec{RiskClass: "migration", HeadSHA: "h2", PolicyVersion: pinned.ID})
	if err != nil {
		t.Fatal(err)
	}
	id, err := g.s.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
	stub := &boardStub{s: g.s, approval: id}
	c := g.liveCase(t, CaseMergeApproval)
	_, err = stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaFactoryd, Rung: 2, Res: Resolution{Action: ActApproveMerge}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "risk class migration keeps a human on its merge") {
		t.Fatalf("Act = %v, want a refusal naming the class migration", err)
	}
	if len(stub.calls) != 0 || caseByID(t, g.s, c.ID).Status != CaseAtHuman {
		t.Fatalf("board calls %d, case %s: a refused approval runs nothing and ends nothing", len(stub.calls), caseByID(t, g.s, c.ID).Status)
	}
}

// A pinned policy version that cannot be read stops no answer: the case's
// facts name the failed read in Partial, the version once and the error's
// own message scrubbed, rung 2's approve_merge is refused with that state
// and the hand-up, the human's approve and deny at rung 3 each end their
// case, and rung 2's deny_merge is not refused. Version 9 does not exist;
// reading version 10 fails.
func TestAnUnreadablePinnedPolicyRefusesRung2ApproveAndLeavesTheHumanTheirs(t *testing.T) {
	g := newRungRig(t, "success")
	g.r.Scrub = redact.New()
	g.r.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) {
		if id == 10 {
			return policy.Version{}, false, errors.New("GET https://user:pass@db.example/v: Authorization: Bearer abcdefghijklmnopqrstuvwx")
		}
		return policy.Version{}, false, nil
	}
	stub := &boardStub{s: g.s}
	open := func(version int64) Case {
		t.Helper()
		spec, err := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h2", PolicyVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		id, err := g.s.OpenApproval(context.Background(), ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
		stub.approval = id
		return g.liveCase(t, CaseMergeApproval)
	}
	unread := func(c Case, entry string) {
		t.Helper()
		task, _, _ := g.s.Task(context.Background(), "t1")
		f, err := g.r.factsAtCase(context.Background(), task, c)
		if err != nil || !slices.Equal(f.Partial, []string{entry}) {
			t.Fatalf("facts: Partial %q, %v; want [%q]", f.Partial, err, entry)
		}
		_, err = stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaFactoryd, Rung: 2, Res: Resolution{Action: ActApproveMerge}})
		want := "the classes that keep a human on a merge could not be read (" + strings.TrimPrefix(entry, "human_classes: ") +
			"), so rung 2 cannot tell whether class standard keeps one: hand_up with approve_merge as the recommendation"
		var refused ErrActRefused
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), want) || strings.Count(err.Error(), "policy version") != 1 {
			t.Fatalf("rung 2 Act = %v, want a refusal holding %q, naming the version once", err, want)
		}
	}
	ended := func(who string, c Case, action, by string, rung, calls int) {
		t.Helper()
		got := caseByID(t, g.s, c.ID)
		if got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Action != action || got.Resolution.By != by || got.Resolution.Rung != rung || len(stub.calls) != calls {
			t.Fatalf("%s: case %s, resolution %+v, board calls %d; want resolved with %s by %s on rung %d, board calls %d", who, got.Status, got.Resolution, len(stub.calls), action, by, rung, calls)
		}
	}

	c := open(10)
	unread(c, "human_classes: read policy version 10: GET https://[redacted]@db.example/v: Authorization: Bearer [redacted]")
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge, Note: "reads well"}}); err != nil {
		t.Fatalf("the human's approve: %v", err)
	}
	ended("the human's approve", c, ActApproveMerge, "sebastian", 3, 1)

	c = open(9)
	unread(c, "human_classes: policy version 9, which the task started under, does not exist")
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3,
		Res: Resolution{Action: ActDenyMerge, Args: map[string]any{"action": "request_changes"}, Note: "split the migration"}}); err != nil {
		t.Fatalf("the human's deny: %v", err)
	}
	ended("the human's deny", c, ActDenyMerge, "sebastian", 3, 2)

	c = open(9)
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaFactoryd, Rung: 2,
		Res: Resolution{Action: ActDenyMerge, Args: map[string]any{"action": "request_changes"}, Note: "the tests do not cover the rename"}}); err != nil {
		t.Fatalf("rung 2's deny: %v", err)
	}
	ended("rung 2's deny", c, ActDenyMerge, ChatUser, 2, 3)
}
