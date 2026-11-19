package pg

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The heal never ends a case Act is answering: while a grant's work holds the
// case lock after it decided the request, the heal skips that case and goes on,
// and Act resolves it under the grant's own name.
func TestTheHealSkipsTheCaseActIsAnswering(t *testing.T) {
	g, _, c := criteriaRig(t)
	g.s.TryLockCase = g.r.TryLockCase
	ctx := context.Background()
	decided, release := make(chan struct{}), make(chan struct{})
	var actErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, actErr = g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActGrantCriteria},
			Work: func(ctx context.Context) (int64, error) {
				if err := g.s.DecideApproval(LeaveCaseToAct(ctx), mustApproval(t, g), ApprovalCriteria, "approved", "sebastian", "one more bounded card"); err != nil {
					return 0, err
				}
				close(decided)
				<-release
				return 0, nil
			}})
	}()
	<-decided
	// the heal returns at once with the busy case left to Act, and the offers pass runs on
	healed := make(chan CaseHealReport)
	go func() {
		rep, _ := g.s.ReconcileCases(ctx, "demo-app/demo-app")
		g.r.StoreMissedOffers(ctx)
		healed <- rep
	}()
	select {
	case rep := <-healed:
		if rep.Resolved != 0 {
			t.Errorf("the heal ended the case Act was answering: %+v", rep)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the heal pass waited on the case Act was answering")
	}
	close(release)
	wg.Wait()
	got := caseByID(t, g.s, c.ID)
	if actErr != nil || got.Status != CaseResolved || got.Resolution == nil || got.Resolution.Action != ActGrantCriteria {
		t.Fatalf("Act = %v, case %s %+v; want it resolved as grant_criteria", actErr, got.Status, got.Resolution)
	}
}

func mustApproval(t *testing.T, g rungRig) int64 {
	t.Helper()
	var id int64
	if err := g.s.PgDB().QueryRowContext(context.Background(), `SELECT id FROM approval_requests WHERE task_id = 't1' AND kind = $1`, ApprovalCriteria).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A merge request whose case an answer resolved without deciding the request
// gets its case again from the heal; only a confirmation's stays resolved.
func TestAMergeRequestWhoseCaseWasAnsweredGetsItsCaseAgain(t *testing.T) {
	for _, res := range []Resolution{
		{Action: ActContinue, Note: "look at the flaky test first"},
		{Action: ActHandover, Args: map[string]any{"model": "same"}},
		{Action: ActResume},
	} {
		t.Run(res.Action, func(t *testing.T) {
			g, _, _ := mergeRig(t, "success")
			ctx := context.Background()
			c := g.liveCase(t, CaseMergeApproval)
			if _, err := g.r.Act(ctx, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaChat, Rung: 3, Res: res}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE approval_requests SET created_at = now() - interval '5 minutes'`); err != nil {
				t.Fatal(err)
			}
			rep, err := g.s.ReconcileCases(ctx, "demo-app/demo-app")
			open, _ := g.s.OpenCasesOf(ctx, "t1")
			if err != nil || rep.Opened != 1 || !slices.ContainsFunc(open, func(k Case) bool { return k.Kind == CaseMergeApproval }) {
				t.Fatalf("heal after %s = %+v, %v, live %+v; want the request's case opened again", res.Action, rep, err, open)
			}
		})
	}
}

// The case writer tells the story after the ask stored the face: its lines
// stand beside the composed numbers on the stored face from then on.
func TestTheWritersStoryShowsOnTheStoredFace(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.PgFaceStop(ctx, "t1", "the importer cannot read the snapshot", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if len(cases) != 1 || cases[0].Kind != CaseOther {
		t.Fatalf("cases %+v, want one case of kind other", cases)
	}
	happened, left := "The session found that the importer drops the accent.", "A fix of the importer is left."
	if err := g.s.SetCaseBrief(ctx, cases[0].ID, happened, left); err != nil {
		t.Fatal(err)
	}
	c := caseByID(t, g.s, cases[0].ID)
	ask, _ := g.r.CaseAsk(ctx, c)
	if _, stored := ask.Detail["story"]; !stored {
		t.Fatalf("the ask stored no face: %+v", ask.Detail)
	}
	got := CaseFace(c, Task{ID: "t1"}, ask).Story
	if want := "No session has run on its 3.00 EUR budget yet. " + happened; got.Happened != want {
		t.Errorf("happened %q, want %q", got.Happened, want)
	}
	if want := left + " All of its work is still to do."; got.Left != want {
		t.Errorf("left %q, want %q", got.Left, want)
	}
}

// An ask stores the face's story, so the offers pass finds the face stored
// and leaves it.
func TestAnAskStoresItsStorySoTheOffersPassLeavesIt(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.PgFaceStop(ctx, "t1", CapReached+"\nlast: no session produced a result", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.asks(t)[0].Detail["story"].(map[string]any); !ok {
		t.Fatalf("story = %#v, want the face's story stored", g.asks(t)[0].Detail["story"])
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if err := g.s.SetCaseFace(ctx, cases[0].ID, map[string]any{"offers": []AskOffer{{Value: ActNote, Label: "Not now", Effect: "marker"}}}); err != nil {
		t.Fatal(err)
	}
	g.r.StoreMissedOffers(ctx)
	if got := offerValues(storedOffers(t, g.asks(t)[0].Detail)); !slices.Equal(got, []string{ActNote}) {
		t.Fatalf("offers after the pass = %v, want the stored face left as it is", got)
	}
}

// A merge request opened on red checks is refused while its recorded head is
// red and approved once the same head is green again (a re-run).
func TestARedRequestApprovesOnceItsHeadTurnsGreen(t *testing.T) {
	g := newRungRig(t, "failure")
	ctx := context.Background()
	spec, _ := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h2", RedChecks: true})
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
	stub := &boardStub{s: g.s, approval: id}
	c := g.liveCase(t, CaseMergeApproval)
	in := ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}}
	if _, err := stub.act(g.r, in); err == nil || !strings.Contains(err.Error(), "red, not green") || len(stub.calls) != 0 {
		t.Fatalf("approve on the red head = %v, work ran %d; want the red refusal and no work", err, len(stub.calls))
	}
	g.forge.checks["h2"] = "success"
	if _, err := stub.act(g.r, in); err != nil || len(stub.calls) != 1 {
		t.Fatalf("approve on the healed head = %v, work ran %d; want it taken once", err, len(stub.calls))
	}
	if got := caseByID(t, g.s, c.ID); got.Resolution == nil || got.Resolution.Action != ActApproveMerge {
		t.Fatalf("case = %+v, want it resolved by approve_merge", got.Resolution)
	}
}

// The red-checks refusal names the remedies the approval card shows: a factory
// pull request's checks run again, an agent's pull request goes back.
func TestTheRedChecksRefusalNamesTheCardsOwnRemedies(t *testing.T) {
	for _, tc := range []struct {
		factory bool
		want    string
	}{
		{false, "the checks on h2 are red, not green: `approve_merge` waits for green checks on the head, and the human may return it to the agent with what to change (`deny_merge` with `request_changes`) or close it (`deny_merge` with `reject`)"},
		{true, "the checks on h2 are red, not green: `approve_merge` waits for green checks on the head, and the human may run them again (`rerun_checks`) or close it with your words (`deny_merge` with `reject`)"},
	} {
		g := newRungRig(t, "failure")
		ctx := context.Background()
		spec, _ := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h2", RedChecks: true, Factory: tc.factory})
		id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
		stub := &boardStub{s: g.s, approval: id}
		c := g.liveCase(t, CaseMergeApproval)
		_, err = stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}})
		if err == nil || err.Error() != tc.want {
			t.Errorf("factory %v: refusal %v, want %q", tc.factory, err, tc.want)
		}
	}
}

// grant_criteria on a criteria request that never judged them resolves its
// case as a grant, as on a gap's request: the work runs once.
func TestActGrantsCriteriaNeverJudged(t *testing.T) {
	g, stub, c := criteriaRig(t)
	ctx := context.Background()
	if _, err := g.s.PgDB().ExecContext(ctx, `UPDATE cases SET facts = facts || '{"approval_kind": "criteria_rerun"}'::jsonb WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActGrantCriteria}}); err != nil {
		t.Fatalf("grant on criteria never judged = %v, want Act to take it", err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("work ran %d times, want once", len(stub.calls))
	}
	if got := caseByID(t, g.s, c.ID); got.Status != CaseResolved || got.Resolution.Action != ActGrantCriteria {
		t.Errorf("case = %+v, want it resolved by the grant", got)
	}
}

// SetCaseFace writes to the newest ask that names the case.
func TestSetCaseFaceTakesTheNewestAsk(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.PgFaceStop(ctx, "t1", CapReached+"\nlast: no session produced a result", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if _, err := g.r.PgFaceStop(ctx, "t1", HealedReason, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.s.SetCaseFace(ctx, cases[0].ID, map[string]any{"offers": []AskOffer{{Value: ActNote, Label: "Not now", Effect: "marker"}}}); err != nil {
		t.Fatal(err)
	}
	asks := g.asks(t)
	if len(asks) != 2 {
		t.Fatalf("asks = %d, want 2", len(asks))
	}
	if first, newest := offerValues(storedOffers(t, asks[0].Detail)), offerValues(storedOffers(t, asks[1].Detail)); slices.Equal(first, []string{ActNote}) || !slices.Equal(newest, []string{ActNote}) {
		t.Fatalf("the older ask holds %v and the newest %v; want the marker on the newest alone", first, newest)
	}
}

// SetCaseFace replaces the suggestion the ask held: a face that suggests
// nothing leaves no stale preselected answer, writer or grounds behind.
func TestSetCaseFaceDropsTheSuggestionItNoLongerHolds(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if _, err := g.r.PgFaceStop(ctx, "t1", CapReached+"\nlast: no session produced a result", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "t1")
	if err := g.s.SetQuestionProposal(ctx, "t1", cases[0].ID, "one session fixes it", "the factory agent", map[string]any{"action": ActExtend}); err != nil {
		t.Fatal(err)
	}
	if asks := g.asks(t); len(asks) != 1 || asks[0].Detail["preselected"] == nil {
		t.Fatalf("asks %+v, want one that preselects an answer", asks)
	}
	if err := g.s.SetCaseFace(ctx, cases[0].ID, map[string]any{"offers": []AskOffer{{Value: ActNote, Label: "Not now", Effect: "marker"}}}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"preselected", "recommended_by", "recommendation"} {
		if v, ok := g.asks(t)[0].Detail[key]; ok {
			t.Errorf("the ask keeps %s = %v after a face that suggests nothing", key, v)
		}
	}
}

// Free words alone answer no card whose question offers choices: Act refuses
// them naming the choices, the card's "Not now" born beside the lone "Done",
// and a choice takes the note beside it.
func TestActRefusesFreeWordsOnAChoiceCard(t *testing.T) {
	g := newTripRig(t, "success")
	ctx := context.Background()
	if err := g.s.CreateTask(ctx, Task{ID: "h1", Kind: "human", Status: "waiting-human", Objective: "x", Repository: "demo-app/demo-app", IssueNumber: 51, Source: SourceRoutine}); err != nil {
		t.Fatal(err)
	}
	h, _, _ := g.r.PgTask(ctx, "h1")
	q := HumanQuestion{Ask: "Which store?", Choices: []QuestionChoice{{Value: AnswerClose, Label: "Done"}}}
	if err := recordAsk(ctx, g.r.PgTimeline(), h, q, "factoryd", SourceRoutine, ViaFactoryd, "", nil); err != nil {
		t.Fatal(err)
	}
	cases, _ := g.s.OpenCasesOf(ctx, "h1")
	_, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: cases[0].ID, By: "sebastian", Via: ViaButton, Rung: 3, Res: Resolution{Action: ActNote, Note: "retry it next week"}})
	if want := "card #51 takes one of its choices, with your note beside it: Done (close), Not now (park)"; err == nil || err.Error() != want {
		t.Fatalf("free words = %v, want %q", err, want)
	}
	if _, err := g.r.Act(ctx, ActInput{TaskID: "h1", CaseID: cases[0].ID, By: "sebastian", Via: ViaButton, Rung: 3, Res: Resolution{Action: AnswerClose, Note: "retry it next week"}}); err != nil {
		t.Fatalf("a choice with a note = %v", err)
	}
}

// Pending checks are not red: the refusal says to wait for them, and "run them
// again" belongs to red checks alone.
func TestThePendingChecksRefusalSaysToWait(t *testing.T) {
	g := newRungRig(t, "pending")
	ctx := context.Background()
	spec, _ := json.Marshal(MergeSpec{RiskClass: "standard", HeadSHA: "h2", Factory: true})
	id, err := g.s.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalMerge, TaskID: "t1", Project: "demo-app/demo-app", PRNumber: 9, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	g.record(t, EventApprovalRequested, map[string]any{"approval": id, "kind": ApprovalMerge, "pr": int64(9)})
	stub := &boardStub{s: g.s, approval: id}
	c := g.liveCase(t, CaseMergeApproval)
	_, err = stub.act(g.r, ActInput{TaskID: "t1", CaseID: c.ID, By: "sebastian", Via: ViaConsole, Rung: 3, Res: Resolution{Action: ActApproveMerge}})
	const want = "the checks on h2 are pending, not green: `approve_merge` waits for green checks on the head, and the human may wait for them to settle or close it (`deny_merge` with `reject`)"
	if err == nil || err.Error() != want {
		t.Fatalf("approve on pending checks = %v, want %q", err, want)
	}
}
