package factoryd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// The confirmation is a decision field with closed values: confirm files
// the revision, revise sends the planner back with the words as findings,
// and no string anywhere decides.
// The night of 2026-10-31 answered "okay — confirmed. Revision 15 is
// fine." and the choice-word rule filed nothing: the words are findings
// now, the decision is the field.

func TestConfirmedByDecision(t *testing.T) {
	confirm := PlanQuestion{Kind: QuestionConfirmation, Decision: DecisionConfirm}
	if !confirmedByAnswers([]PlanQuestion{confirm}) {
		t.Fatal("a confirmation decided confirm does not confirm")
	}
	if confirmedByAnswers([]PlanQuestion{{Kind: QuestionConfirmation, Decision: DecisionRevise, Answer: "make the retry rule exponential"}}) {
		t.Fatal("a revise confirms")
	}
	if confirmedByAnswers([]PlanQuestion{{Kind: QuestionConfirmation, Answer: "okay"}}) {
		t.Fatal("a bare okay with no decision confirms")
	}
	if confirmedByAnswers([]PlanQuestion{confirm, {Answer: "postgres"}}) {
		t.Fatal("a real question beside the confirmation files the revision")
	}
	if confirmedByAnswers(nil) {
		t.Fatal("no questions confirm")
	}
}

// decisionRig runs a plan card whose spec is unconfirmed, so revision 1
// asks its confirmation, with a signal log the answers can reach.
func decisionRig(t *testing.T) (*Runner, *fakeStore) {
	t.Helper()
	r, store, _, _ := newPlanRunner(t, testPlan())
	plan := store.tasks["t-plan"]
	plan.Status = "running"
	store.tasks["t-plan"] = plan
	r.cfg.Workflows = &signalLog{}
	return r, store
}

// The night's answer, words and all, files when it carries the decision:
// the words land as findings, the field decides, the approval resolves as
// confirmed. The same words without a decision are refused and nothing is
// stored, so the answered-but-unconfirmed limbo cannot be written.
func TestFlavouredConfirmationFilesThroughDecision(t *testing.T) {
	ctx := context.Background()
	r, store := decisionRig(t)
	if ref := requestPlan(t, r); ref.Answered || ref.Comment == 0 {
		t.Fatalf("the ask: %+v", ref)
	}
	night := "okay — confirmed. Revision 15 is fine."
	if err := AnswerPlan(ctx, store, r.cfg.Workflows, "t-plan", 1, 1, "", night, "sebastian"); !errors.Is(err, ErrDecisionRequired) {
		t.Fatalf("words without a decision: %v", err)
	}
	if qs, _ := store.PlanQuestions(ctx, "t-plan", 1); qs[0].Answer != "" || qs[0].Decision != "" {
		t.Fatalf("the refusal stored a half answer: %+v", qs[0])
	}
	if err := AnswerPlan(ctx, store, r.cfg.Workflows, "t-plan", 1, 1, DecisionConfirm, night, "sebastian"); err != nil {
		t.Fatal(err)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Decision != DecisionConfirm || qs[0].Answer != night || qs[0].AnsweredBy != "sebastian" {
		t.Fatalf("the answered confirmation: %+v", qs[0])
	}
	if rev, ok, _ := store.PlanConfirmed(ctx, "t-plan"); !ok || rev != 1 {
		t.Fatalf("the answered confirmation's revision %d %v", rev, ok)
	}
	if n, _ := store.OpenPlanQuestions(ctx, "t-plan", 1); n != 0 {
		t.Fatalf("%d questions still open after the confirm", n)
	}
	if a := store.approvals[0]; a.Status != "denied" || !strings.Contains(a.DecisionNote, "revision 1 is confirmed") {
		t.Fatalf("the approval after the confirm: %+v", a)
	}
}

// A revise is a decision with required words: the words reach the next
// planner round as the answer, the revision does not file.
func TestReviseCarriesTheWords(t *testing.T) {
	ctx := context.Background()
	r, store := decisionRig(t)
	requestPlan(t, r)
	if err := AnswerPlan(ctx, store, r.cfg.Workflows, "t-plan", 1, 1, DecisionRevise, "", "sebastian"); err == nil {
		t.Fatal("a revise without words is stored")
	}
	words := "make the retry rule exponential"
	if err := AnswerPlan(ctx, store, r.cfg.Workflows, "t-plan", 1, 1, DecisionRevise, words, "sebastian"); err != nil {
		t.Fatal(err)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Decision != DecisionRevise || qs[0].Answer != words {
		t.Fatalf("the revised confirmation: %+v", qs[0])
	}
	if a := store.approvals[0]; a.Status != "denied" || !strings.Contains(a.DecisionNote, "the planner revises revision 1") {
		t.Fatalf("the approval after the revise: %+v", a)
	}
}

// A decision on a real question is refused: the field is the
// confirmation's alone.
func TestDecisionIsTheConfirmationsAlone(t *testing.T) {
	ctx := context.Background()
	r, store := decisionRig(t)
	requestPlan(t, r)
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if len(qs) != 1 {
		t.Fatalf("revision 1's questions %+v, want the confirmation alone", qs)
	}
	store.kinds["t-plan 1"] = []string{"question", "question"}
	store.questions["t-plan 1"] = []PlanQuestion{{Text: "Which store?"}, {Text: "Which cache?"}}
	if err := AnswerPlanQuestions(ctx, store, r.cfg.Workflows, "t-plan", 1, []PlanAnswer{{Idx: 1, Text: "postgres", Decision: DecisionConfirm}}, "alice"); !errors.Is(err, ErrDecisionReserved) {
		t.Fatalf("a decision on a real question: %v", err)
	}
	if err := AnswerPlanQuestions(ctx, store, r.cfg.Workflows, "t-plan", 1, []PlanAnswer{{Idx: 1, Text: "postgres"}, {Idx: 2, Text: "redis"}}, "alice"); err != nil {
		t.Fatal(err)
	}
}

// The asked confirmation carries no choice word and teaches the two
// decisions instead.
func TestTheAskTeachesTheDecisions(t *testing.T) {
	r, store, _, _ := newPlanRunner(t, testPlan())
	if err := r.askConfirmation(context.Background(), store.tasks["t-plan"], 1, testPlan()); err != nil {
		t.Fatal(err)
	}
	qs, _ := store.PlanQuestions(context.Background(), "t-plan", 1)
	if len(qs) != 1 || len(qs[0].Choices) != 0 {
		t.Fatalf("the confirmation offers choices: %+v", qs)
	}
	if strings.Contains(qs[0].Text, "answer okay") {
		t.Fatalf("the ask still teaches the word: %s", qs[0].Text)
	}
	for _, want := range []string{"Confirm", "Request changes"} {
		if !strings.Contains(qs[0].Text, want) {
			t.Fatalf("the ask lacks %q: %s", want, qs[0].Text)
		}
	}
}

// The filing names what it reached: storing a revision files no cards,
// and FilePlan reports whether the human's confirmation waits on it. A
// plan with criteria waits; one whose spec sits unchanged under an
// answered confirmation files with no further gate.
func TestFilePlanNamesTheGateItReached(t *testing.T) {
	ctx := context.Background()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort"}
	runner, _, store := newTestRunner(t, plan, newFakeKube(), &fakeObjectStore{})
	runner.cfg.Policy = func() policy.Version { return fixtureVersion(t) }
	b, err := json.Marshal(testPlan())
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil || res.Revision != 1 || !res.Gate {
		t.Fatalf("a first filing = %+v, %v; want revision 1 with the confirmation waiting", res, err)
	}
	if _, _, err := store.PutPlanConfirmation(ctx, "t-plan", 1, PlanQuestion{Text: "Confirm the plan?"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, DecisionConfirm, "", "sebastian"); err != nil {
		t.Fatal(err)
	}
	res, err = runner.FilePlan(ctx, "t-plan", string(b))
	if err != nil || res.Revision != 1 || res.Gate {
		t.Fatalf("the spec unchanged under a confirmed revision = %+v, %v; want no gate", res, err)
	}
}

// The console's comment of a decided confirmation is its echo, whatever
// the decision carries: a bare confirm, a confirm with words, a revise
// with words. Intake records each as signalled and sends no change
// request; a comment that only looks like one, the old choice word on a
// confirmed row, is a change request.
func TestIntakeKnowsADecidedConfirmation(t *testing.T) {
	ctx := context.Background()
	in, store, _ := newTestIntake(t)
	store.tasks["t-plan"] = Task{ID: "t-plan", Repository: "sf/ai-software-factory-code", IssueNumber: 20, Status: "waiting-human", Kind: "agent", Track: policy.TrackPlan}
	for n, d := range []struct{ decision, words string }{{DecisionConfirm, ""}, {DecisionRevise, "keep the slate clean"}, {DecisionConfirm, "fine as is"}} {
		store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"one"}`), fmt.Sprintf("sha-%d", n+1))
		store.PutPlanConfirmation(ctx, "t-plan", n+1, PlanQuestion{Text: "Confirm the plan?"})
		if err := store.AnswerPlanQuestion(ctx, "t-plan", n+1, 1, d.decision, d.words, "sebastian"); err != nil {
			t.Fatal(err)
		}
	}
	in.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: &fakeEventRecorder{}, Store: store}
	for i, body := range []string{"Answer 1: confirm", "Answer 1: revise · keep the slate clean", "Answer 1: confirm · fine as is", "Answer 1: okay"} {
		b, _ := json.Marshal(map[string]any{"action": "created", "issue": map[string]any{"number": 20}, "repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"comment": map[string]any{"id": 91 + i, "user": map[string]string{"login": "sebastian"}, "body": body}})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("comment %q: status %d", body, rr.Code)
		}
	}
	want := []string{"t-plan human_comment sebastian #0 c94: Answer 1: okay"}
	if got, ev := workflowsOf(in).signals, store.intakeEvents; !slices.Equal(got, want) || len(ev) != 4 || !ev[0].Signalled || !ev[1].Signalled || !ev[2].Signalled {
		t.Fatalf("signals %q, want %q; intake events %+v", got, want, ev)
	}
}

// Request changes fixes nothing: the spec and the criteria are fixed only
// by a confirm, so a revision after a revise that keeps the spec asks the
// confirmation again.
func TestAReviseFixesNoSpec(t *testing.T) {
	ctx := context.Background()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Track: policy.TrackPlan, Process: "spec", Repository: "linkshort/linkshort"}
	runner, _, store := newTestRunner(t, plan, newFakeKube(), &fakeObjectStore{})
	runner.cfg.Policy = func() policy.Version { return fixtureVersion(t) }
	p := testPlan()
	b, _ := json.Marshal(p)
	if res, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || res.Revision != 1 || !res.Gate {
		t.Fatalf("revision 1: %+v, %v", res, err)
	}
	store.PutPlanConfirmation(ctx, "t-plan", 1, PlanQuestion{Text: "Confirm the plan?"})
	if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, DecisionRevise, "keep the slate clean", "sebastian"); err != nil {
		t.Fatal(err)
	}
	p.Summary = "The same spec, the cards cut finer."
	b, _ = json.Marshal(p)
	if res, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || res.Revision != 2 || !res.Gate {
		t.Fatalf("revision 2 after the revise: %+v, %v; want the confirmation to wait", res, err)
	}
}
