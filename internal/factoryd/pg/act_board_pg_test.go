package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/redact"
)

// Moved out of factoryd with the board action tests; the boardStub, the
// merge and criteria rigs and rungRig's record and liveCase helpers it
// shares with the round tests moved before it into helpers_2e_test.go.

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
// the console's work never runs and the case stays open where it was.
func TestApproveMergeWithRedChecksIsRefusedNamingThem(t *testing.T) {
	g, stub, _ := mergeRig(t, "failure")
	c := g.liveCase(t, CaseMergeApproval)
	_, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "the checks on h2 are red, not green") {
		t.Fatalf("Act = %v, want a refusal naming the red checks", err)
	}
	if len(stub.calls) != 0 || caseByID(t, g.s, c.ID).Status != CaseOpen {
		t.Fatalf("board calls %d, case %s: a refused approval runs nothing and ends nothing", len(stub.calls), caseByID(t, g.s, c.ID).Status)
	}
}

// Rung 2's approval at full auto, where the merge is rung 2's to decide,
// is refused with 2.8's text when a gate did not run on the card (the rig
// holds a review and no audit row, and the process lists both).
func TestApproveMergeAtRung2NeedsTheGates(t *testing.T) {
	g, stub, _ := mergeRig(t, "success")
	g.atAutonomy(policy.AutonomyFullAuto)
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

// criteriaRig is the rung rig helpers_2e_test.go holds for these tests.
//
// A done plan's criteria request is answered through Act (the ended-card
// check lets the criteria's actions and the restart through): a verdict keeps
// the case open where it stands; a grant, a confirmation and a denial each
// resolve it under their own name; restart_plan resolves it too.
func TestCriteriaActionsRunOnADonePlansCase(t *testing.T) {
	for _, tc := range []struct {
		action  string
		args    map[string]any
		note    string
		status  string
		resolve string
	}{
		{ActAnswerCriteria, map[string]any{"criterion": "c1", "verdict": "waive"}, "out of scope", CaseOpen, ""},
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

// Settling a plan's criteria by word is a human's act, on every path. At
// autonomous, where rung 2's answers run and propose nothing, its waiver,
// denial and confirmation through Act are each refused, naming the grant
// and the hand-up, and run no work; the store refuses the project agent's denial of a
// criteria request too. The human's press takes each, and rung 2's grant,
// which waives nothing, runs.
func TestRung2SettlesNoCriteriaByWord(t *testing.T) {
	const want = "settling a plan's criteria by word is the human's: take grant_criteria for one more bounded card, or hand the case up with attach_analysis and recommend answer_criteria (waive), deny_criteria or confirm_criteria"
	words := []struct {
		name string
		res  Resolution
	}{
		{"waive", Resolution{Action: ActAnswerCriteria, Args: map[string]any{"criterion": "c1", "verdict": "waive"}, Note: "out of scope"}},
		{"deny", Resolution{Action: ActDenyCriteria, Note: "not now"}},
		{"confirm", Resolution{Action: ActConfirmCriteria, Note: "main holds every page"}},
	}
	for _, w := range words {
		t.Run(w.name, func(t *testing.T) {
			g, stub, c := criteriaRig(t)
			g.atAutonomy(policy.AutonomyAutonomous)
			_, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: w.res})
			var refused ErrActRefused
			if !errors.As(err, &refused) || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("rung 2's %s = %v, want a refusal starting %q", w.name, err, want)
			}
			if len(stub.calls) != 0 || caseByID(t, g.s, c.ID).Status != CaseOpen {
				t.Fatalf("board calls %d, case %s: a refused answer runs nothing and ends nothing", len(stub.calls), caseByID(t, g.s, c.ID).Status)
			}
			if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: w.res}); err != nil {
				t.Fatalf("the human's %s: %v", w.name, err)
			}
			if len(stub.calls) != 1 {
				t.Fatalf("board calls %d after the human's %s, want 1", len(stub.calls), w.name)
			}
		})
	}
	t.Run("the store's denial", func(t *testing.T) {
		g, stub, _ := criteriaRig(t)
		ctx := context.Background()
		if err := g.s.DecideApproval(ctx, stub.approval, ApprovalCriteria, "denied", ChatUser, "not now"); !errors.Is(err, ErrNotPerson) {
			t.Fatalf("the project agent's denial = %v, want ErrNotPerson", err)
		}
		if pending, _ := g.s.ApprovalRequests(ctx, "pending"); !slices.ContainsFunc(pending, func(q ApprovalRequest) bool { return q.ID == stub.approval }) {
			t.Fatal("a refused denial decided the request")
		}
	})
	t.Run("grant", func(t *testing.T) {
		g, stub, c := criteriaRig(t)
		g.atAutonomy(policy.AutonomyAutonomous)
		if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActGrantCriteria}}); err != nil {
			t.Fatalf("rung 2's grant: %v", err)
		}
		if len(stub.calls) != 1 || caseByID(t, g.s, c.ID).Status != CaseResolved {
			t.Fatalf("board calls %d, case %s: rung 2's grant runs and resolves the case", len(stub.calls), caseByID(t, g.s, c.ID).Status)
		}
	})
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
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET ceiling_eur = 10 WHERE id = 't1'`); err != nil {
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
	if err := g.s.PgDB().QueryRowContext(ctx, `SELECT count(*), COALESCE(max((detail->>'amount_eur')::float), 0), COALESCE(max((detail->>'rung')::float), 0)
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

// Rung 2's approve_merge at full auto on a merge approval of class
// migration is refused through Act, on the policy version the class came
// from: the case's facts carry the human classes of the pinned version, a
// version that marks migration, while the live policy no longer marks it.
// The request opens the case at rung 0, the case-wake pass hands it to
// rung 2, and rung 2's hand-up faces it: at the human with rung 2's
// finding, one poke, and the card still in review.
func TestRung2ApproveOfAMigrationMergeIsRefusedOnThePinnedPolicy(t *testing.T) {
	g := newRungRig(t, "success")
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE tasks SET status = 'in-review' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	pinned := policy.Version{ID: 9, Tree: projectTree(t)}
	live := policy.Version{ID: 11, Tree: projectTree(t)}
	live.Document.Risk.Classes = slices.Clone(pinned.Document.Risk.Classes)
	i := slices.IndexFunc(live.Document.Risk.Classes, func(c policy.RiskClass) bool { return c.Name == "migration" })
	if i < 0 || !pinned.Document.Risk.Classes[i].Migration {
		t.Fatal("the test policy has no migration class marked migration")
	}
	live.Document.Risk.Classes[i].Migration = false
	p := live.Projects["demo-app"]
	p.Notify = []string{"sebastian"}
	live.Projects = map[string]policy.ProjectConfig{"demo-app": p}
	g.r.PgSetPolicy(func() policy.Version { return live })
	g.r.PgSetPolicyVersion(func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == pinned.ID, nil })
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
	if c.Status != CaseOpen {
		t.Fatalf("the request's case is %s, want it open at rung 0", c.Status)
	}
	g.wakePass(t)
	_, err = stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: ChatUser, Via: ViaFactoryd, Rung: 2, Res: Resolution{Action: ActApproveMerge}})
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "risk class migration keeps a human on its merge") {
		t.Fatalf("Act = %v, want a refusal naming the class migration", err)
	}
	if len(stub.calls) != 0 || caseByID(t, g.s, c.ID).Status != CaseOpen {
		t.Fatalf("board calls %d, case %s: a refused approval runs nothing and ends nothing", len(stub.calls), caseByID(t, g.s, c.ID).Status)
	}
	if _, err := g.r.HandUpRung2(ctx, "t1", c.ID, "the migration adds a column with its default",
		RungTwoRecommendation{Action: ActApproveMerge, Grounds: "the migration is additive"}, nil); err != nil {
		t.Fatalf("HandUpRung2: %v", err)
	}
	task, _, err := g.s.Task(ctx, "t1")
	if got := caseByID(t, g.s, c.ID); err != nil || got.Status != CaseAtHuman || task.Status != "in-review" {
		t.Fatalf("case %s, card %s (%v); want the case at the human and the card in review", got.Status, task.Status, err)
	}
	if asks := g.asks(t); len(asks) != 1 || !strings.Contains(asks[0].Detail["info"].(string), "The factory agent found: the migration adds a column with its default") {
		t.Fatalf("asks = %+v, want one carrying rung 2's finding", asks)
	}
	if pokes := g.pokeRows(t); !slices.Equal(pokes, []string{EventQuestionAsked}) {
		t.Fatalf("pokes = %v, want the face's ask alone", pokes)
	}
}

// Rung 2 approves a head once. Its approve_merge of h2 on case 1 decided
// the request, Forgejo refused the merge, and the request's reopening
// opens case 2 for the same head, where rung 2's approve_merge is refused
// naming case 1 and what fits, while the human's approve passes.
func TestRung2ApprovesAHeadOnce(t *testing.T) {
	g, stub, _ := mergeRig(t, "success")
	g.record(t, EventGateAudit, map[string]any{"commit": "h2", "ran": true})
	ctx := context.Background()
	first := g.liveCase(t, CaseMergeApproval)
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: first.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActApproveMerge}}); err != nil {
		t.Fatalf("rung 2's approve on case 1: %v", err)
	}
	if err := g.s.ReopenApproval(ctx, "t1", ApprovalMerge, "waiting: Forgejo refused the merge"); err != nil {
		t.Fatal(err)
	}
	second := g.liveCase(t, CaseMergeApproval)
	if second.ID == first.ID || second.StateKey != first.StateKey || second.Status != CaseOpen {
		t.Fatalf("case 2 = %+v, want a new open case of case 1's key %s", second, first.StateKey)
	}
	_, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: second.ID, By: ChatUser, Via: ViaChat, Rung: 2, Res: Resolution{Action: ActApproveMerge}})
	want := fmt.Sprintf("rung 2 approved head h2 of pull request #9 on case %d and its merge did not land: hand_up with what the merge said", first.ID)
	var refused ErrActRefused
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), want) {
		t.Fatalf("rung 2's approve on case 2 = %v, want the refusal %q", err, want)
	}
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: second.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}}); err != nil {
		t.Fatalf("the human's approve on case 2: %v", err)
	}
	if got := caseByID(t, g.s, second.ID); got.Status != CaseResolved || got.Resolution == nil || got.Resolution.By != "sebastian" {
		t.Fatalf("case 2 = %+v, want it resolved by sebastian", got)
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
	g.r.PgSetPolicyVersion(func(_ context.Context, id int64) (policy.Version, bool, error) {
		if id == 10 {
			return policy.Version{}, false, errors.New("GET https://user:pass@db.example/v: Authorization: Bearer abcdefghijklmnopqrstuvwx")
		}
		return policy.Version{}, false, nil
	})
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
		f, err := g.r.FactsAtCase(context.Background(), task, c)
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
