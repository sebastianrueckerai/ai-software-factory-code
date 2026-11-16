package factoryd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The replan stop guards: a card whose open pull request is its plan's
// criteria-gate trigger is never stopped by a machine origin, and a
// check-in's cut closes a plan child only once the plan's replan has
// filed the revision naming the change. The rigs seed the pull requests
// through the fake forge, the read the merge press does.

// gateOpenPR is the re-cut integration card's open pull request 12 at
// head c7, and gateGreenPR the forge that serves it with green checks.
const gateOpenPR = `{"number":12,"state":"open","merged":false,"html_url":"u12","head":{"ref":"factory/t-c","sha":"c7","repo":{"full_name":"factory-agent/repo"}}}`

func gateGreenForge() *prForgejo {
	return &prForgejo{
		pulls:         map[int64]string{12: gateOpenPR},
		statusesBySHA: map[string][]string{"c7": {statusBody("success", [4]string{"success", "pr / test", "", ""})}},
		labels:        map[int64][]string{10: {LabelPlan, "state/done"}},
	}
}

// gateReplanRig is plan p (issue 10, one session used, one revision of
// criteriaPlan filed, criteria unsettled) in status, its re-cut
// integration card c (issue 11, PlanRevision 1) in review holding pull
// request 12, open and green through the fake forge.
func gateReplanRig(t *testing.T, status string) (*Runner, *fakeStore, *prForgejo) {
	t.Helper()
	fj := gateGreenForge()
	r, store, _ := newActivityRunner(t, Task{ID: "p", Kind: "agent", Track: policy.TrackPlan, Source: "webhook", Repository: "sf/repo",
		IssueNumber: 10, Status: status, BudgetEUR: 5, Usage: TaskUsage{Attempts: 1}}, fj)
	b, err := json.Marshal(criteriaPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(context.Background(), "p", b, "sha-1"); err != nil {
		t.Fatal(err)
	}
	c := Task{ID: "c", Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "p", Repository: "sf/repo",
		PlanKey: "integration", PlanRevision: 1, IssueNumber: 11, Status: "in-review", PRNumber: 12, SHA: "c7"}
	if err := store.CreateTask(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return r, store, fj
}

// gateReplanRigAt is gateReplanRig with the card's own id, so the
// workflow harness's task and the guarded card are one.
func gateReplanRigAt(t *testing.T, cardID, status string) (*Runner, *fakeStore, *prForgejo) {
	t.Helper()
	fj := gateGreenForge()
	r, store, _ := newActivityRunner(t, Task{ID: "p", Kind: "agent", Track: policy.TrackPlan, Source: "webhook", Repository: "sf/repo",
		IssueNumber: 10, Status: status, BudgetEUR: 5, Usage: TaskUsage{Attempts: 1}}, fj)
	b, err := json.Marshal(criteriaPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(context.Background(), "p", b, "sha-1"); err != nil {
		t.Fatal(err)
	}
	c := Task{ID: cardID, Kind: "agent", Track: policy.TrackDirect, Source: SourcePlan, Parent: "p", Repository: "sf/repo",
		PlanKey: "integration", PlanRevision: 1, IssueNumber: 11, Status: "in-review", PRNumber: 12, SHA: "c7"}
	if err := store.CreateTask(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return r, store, fj
}

func gateCardAlive(t *testing.T, store *fakeStore, fj *prForgejo) {
	t.Helper()
	if c := store.tasks["c"]; c.Status != "in-review" {
		t.Fatalf("the card %s, want still in review", c.Status)
	}
	for _, closed := range fj.closed {
		if closed == 12 {
			t.Fatal("pull request 12 closed")
		}
	}
}

// TestReplanSparesGateTriggerCard: no replan origin but a person's stops
// a card that holds the gate-trigger pull request. A person's Replan
// press on the plan leaves the card alive in review with its pull request
// open, the plan's answer naming the pull request and the gate; the
// check-in's ReplanChild cut refuses the same way; a person-origin replan
// that names the card still stops it.
func TestReplanSparesGateTriggerCard(t *testing.T) {
	ctx := context.Background()

	r, store, fj := gateReplanRig(t, "done")
	note, err := r.Replan(ctx, "p", ReplanAsk{By: "sebastian", Comment: "the round changed"})
	if err != nil {
		t.Fatalf("the plan's replan: %v", err)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(note, want) {
			t.Fatalf("the plan's replan answer lacks %q: %s", want, note)
		}
	}
	if !strings.Contains(note, "no card stops") {
		t.Fatalf("the plan's replan answer stopped a card: %s", note)
	}
	gateCardAlive(t, store, fj)
	if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 0 {
		t.Fatalf("the spared card's replan_stop steps: %d, want none", n)
	}

	r, store, fj = gateReplanRig(t, "done")
	got := replanChild(t, r, "c", "the round changed")
	if got.Reason != "" {
		t.Fatalf("ReplanChild = %q, want a refusal", got.Reason)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(got.Refused, want) {
			t.Fatalf("ReplanChild's refusal lacks %q: %s", want, got.Refused)
		}
	}
	gateCardAlive(t, store, fj)
	if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 0 {
		t.Fatalf("the refused cut's replan_stop steps: %d, want none", n)
	}

	r, store, _ = gateReplanRig(t, "done")
	note, err = r.Replan(ctx, "c", ReplanAsk{By: "sebastian", Comment: "stop this card for the new round"})
	if err != nil {
		t.Fatalf("the person's replan naming the card: %v", err)
	}
	if c := store.tasks["c"]; c.Status != "closed" {
		t.Fatalf("the named card %s, want closed at the person's ask", c.Status)
	}
	if !strings.Contains(note, "criteria-gate trigger") {
		t.Fatalf("the person's stop note does not name the gate: %s", note)
	}
}

// TestReplanCutFilesItsChange: the check-in's cut closes a plan child
// only once the plan's replan has filed its next revision naming the
// change. A plan that accepts the replan refuses the cut until the
// revision files, and the card closes with replan_stop recorded only
// after it; a plan that cannot take the replan refuses naming the state,
// the pull request and the gate consequence, and the card is not closed.
func TestReplanCutFilesItsChange(t *testing.T) {
	ctx := context.Background()

	r, store, fj := gateReplanRig(t, "done")
	got := replanChild(t, r, "c", "the round changed")
	if got.Reason != "" {
		t.Fatalf("the cut before the revision = %q, want a refusal", got.Reason)
	}
	for _, want := range []string{"revision", "pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(got.Refused, want) {
			t.Fatalf("the cut's refusal lacks %q: %s", want, got.Refused)
		}
	}
	gateCardAlive(t, store, fj)
	if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 0 {
		t.Fatalf("the refused cut left a replan_stop strand: %d, want none", n)
	}
	if steps := replanSteps(t, r, "p"); len(steps) != 1 || steps[0].Origin != ReplanCut {
		t.Fatalf("the plan's replan steps %+v, want the cut's alone", steps)
	}
	if p := store.tasks["p"]; p.Status != "queued" {
		t.Fatalf("the plan %s, want queued for its planner", p.Status)
	}

	// the planner files the next revision naming the change: the card's
	// key stays in it
	b, err := json.Marshal(criteriaPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "p", b, "sha-2"); err != nil {
		t.Fatal(err)
	}
	got = replanChild(t, r, "c", "the round changed")
	if got.Refused != "" {
		t.Fatalf("the cut after the revision refused: %s", got.Refused)
	}
	for _, want := range []string{"revision 2", "pull request #12"} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("the cut's reason lacks %q: %s", want, got.Reason)
		}
	}
	if err := r.Finish(ctx, "c", "closed", got.Reason); err != nil {
		t.Fatalf("close the card as its workflow's cut would: %v", err)
	}
	if c := store.tasks["c"]; c.Status != "closed" {
		t.Fatalf("the card %s, want closed after the revision filed", c.Status)
	}
	stops := stepsNamed(t, r, "c", "replan_stop")
	if len(stops) != 1 {
		t.Fatalf("the card's replan_stop steps: %d, want 1", len(stops))
	}
	var stop struct {
		Work string `json:"work"`
		PR   int64  `json:"pr"`
		Gate string `json:"gate"`
	}
	if err := json.Unmarshal(stops[0], &stop); err != nil || stop.PR != 12 || !strings.Contains(stop.Gate, "merge press") {
		t.Fatalf("replan_stop = %s (%v), want the pull request and the gate consequence", stops[0], err)
	}

	r, store, fj = gateReplanRig(t, "done")
	if _, err := store.Record(ctx, Event{Repository: "sf/repo", TaskID: "p", IssueNumber: 10, Kind: EventPlanClosed,
		Actor: FactoryDecider, Via: ViaFactoryd}); err != nil {
		t.Fatal(err)
	}
	got = replanChild(t, r, "c", "the round changed")
	if got.Reason != "" {
		t.Fatalf("the cut on a closed plan = %q, want a refusal", got.Reason)
	}
	for _, want := range []string{"Plan #10 is closed", "pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(got.Refused, want) {
			t.Fatalf("the closed plan's refusal lacks %q: %s", want, got.Refused)
		}
	}
	gateCardAlive(t, store, fj)
	if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 0 {
		t.Fatalf("the closed plan left a replan_stop strand: %d, want none", n)
	}
}

// TestGateReadFailureRefusesTheReplan: a pull request read that fails at
// the guard's decision refuses the replan naming the unreadable pull
// request and stops no card; a retry with the read landing proceeds on
// facts.
func TestGateReadFailureRefusesTheReplan(t *testing.T) {
	ctx := context.Background()
	r, store, fj := gateReplanRig(t, "done")
	fj.pullsDown = true

	if _, err := r.Replan(ctx, "p", ReplanAsk{By: "sebastian", Comment: "the round changed"}); err == nil || !strings.Contains(err.Error(), "pull request #12") {
		t.Fatalf("the plan's replan on a failed read = %v, want the refusal naming the unreadable pull request", err)
	}
	got := replanChild(t, r, "c", "the round changed")
	if got.Reason != "" || !strings.Contains(got.Refused, "pull request #12") {
		t.Fatalf("ReplanChild on a failed read = %+v, want a refusal naming the unreadable pull request", got)
	}
	gateCardAlive(t, store, fj)
	if n := len(stepsNamed(t, r, "c", "replan_stop")); n != 0 {
		t.Fatalf("a failed read left replan_stop steps: %d, want none", n)
	}
	if steps := replanSteps(t, r, "p"); len(steps) != 0 {
		t.Fatalf("a failed read wrote the plan's replan steps %+v, want none", steps)
	}

	fj.pullsDown = false
	note, err := r.Replan(ctx, "p", ReplanAsk{By: "sebastian", Comment: "the round changed"})
	if err != nil || !strings.Contains(note, "pull request #12") || !strings.Contains(note, "merge press") {
		t.Fatalf("the retry = %q, %v; want the spare note naming the pull request and the gate", note, err)
	}
	gateCardAlive(t, store, fj)
}

// TestRecutCardReachesMergePress: end to end at the workflow level, a
// re-cut card with an open green pull request reaches its merge press.
// A plan replan pressed while the pull request stands and the card's own
// session check-in answering replan both fail to close it — the workflow
// holds it for a person, whose answer buys the session that pushes, and
// the merge press lands; only a person's replan naming the card closes
// it.
func TestRecutCardReachesMergePress(t *testing.T) {
	ctx := context.Background()
	r, store, fj := gateReplanRigAt(t, "t1", "done")

	note, err := r.Replan(ctx, "p", ReplanAsk{By: "sebastian", Comment: "the round changed"})
	if err != nil {
		t.Fatalf("the plan's replan press: %v", err)
	}
	if !strings.Contains(note, "pull request #12") || !strings.Contains(note, "merge press") {
		t.Fatalf("the plan's replan answer names neither the pull request nor the gate: %s", note)
	}
	if c := store.tasks["t1"]; c.Status != "in-review" {
		t.Fatalf("the card %s after the plan's replan, want still in review", c.Status)
	}

	cut := SessionResult{Outcome: OutcomeReplan, SessionID: "s1", Trip: "size", Reason: "two packages", Replan: "R"}
	guarded, err := r.ReplanChild(ctx, "t1", cut.Replan)
	if err != nil {
		t.Fatalf("the check-in's cut through the guard: %v", err)
	}
	if guarded.Reason != "" {
		t.Fatalf("the check-in's cut = %q, want a refusal", guarded.Reason)
	}
	for _, want := range []string{"pull request #12", "criteria-gate trigger", "merge press"} {
		if !strings.Contains(guarded.Refused, want) {
			t.Fatalf("the check-in's cut refusal lacks %q: %s", want, guarded.Refused)
		}
	}
	h := newWorkflowHarness(t)
	h.state.Source = SourcePlan
	h.onReplanChild(guarded)
	h.onSessions(cut, pushed("s2", "c2"))
	h.onChecks(ChecksResult{State: ChecksSuccess})
	h.signalAfter(time.Hour, SignalHumanComment, TaskSignal{Author: "sebastian", Body: "let it run to its merge press", Comment: 602})
	// the harness's pull request opens as number 9 (onChecks); the merge
	// press presses it — the guarded card's own pull request 12 stays in
	// the store world, asserted below
	h.signalAfter(2*time.Hour, SignalPRMerged, TaskSignal{Author: "sebastian", PR: 9})
	if final := h.run(t); final != "done" || h.finals[0][1] != "pull request merged" {
		t.Fatalf("finals %v, want the card done at its merge press", h.finals)
	}
	if len(h.escalated) != 1 || !strings.Contains(h.escalated[0], "pull request #12") || !strings.Contains(h.escalated[0], "merge press") {
		t.Fatalf("escalated %q, want the check-in's replan refused naming the pull request and the gate", h.escalated)
	}
	if c := store.tasks["t1"]; c.Status != "in-review" {
		t.Fatalf("the card %s after the check-in's replan, want still in review with its merge press reached", c.Status)
	}
	for _, closed := range fj.closed {
		if closed == 12 {
			t.Fatal("pull request 12 closed before the merge press")
		}
	}

	// the plan's replan lands before the person's press, as its run would
	// see it through: the planner files the next revision naming the
	// change — the card's key stays in it — and the filing ends the
	// pending replan and the plan's run. The harness drives the
	// activities, so it stands in for the plan's run here
	b, err := json.Marshal(criteriaPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddPlanRevision(ctx, "p", b, "sha-2"); err != nil {
		t.Fatal(err)
	}
	if err := r.timeline.Append(ctx, "p", "replanned", map[string]any{"revision": 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionTask(ctx, "p", []string{"queued"}, "done"); err != nil {
		t.Fatal(err)
	}

	// the person's replan naming the card follows the first two presses
	// on the same run and still stops it: the guard exists for machinery
	// and the check-in model, never for the human
	note, err = r.Replan(ctx, "t1", ReplanAsk{By: "sebastian", Comment: "stop this card for the new round"})
	if err != nil {
		t.Fatalf("the person's replan naming the card: %v", err)
	}
	if !strings.Contains(note, "criteria-gate trigger") {
		t.Fatalf("the person's stop note does not name the gate: %s", note)
	}
	if c := store.tasks["t1"]; c.Status != "closed" {
		t.Fatalf("the card %s after the person's replan, want closed", c.Status)
	}
}
