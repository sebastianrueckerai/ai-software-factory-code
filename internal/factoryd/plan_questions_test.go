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
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/schema/schematest"
)

// questionPlan is testPlan with two open questions, of 3 and 2 choices.
func questionPlan() Plan {
	p := testPlan()
	p.Questions = []PlanQuestion{
		{Text: "How often does the checker probe a site?", Choices: []string{"30 seconds", "60 seconds", "5 minutes"}},
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
	}
	return p
}

// A plan asks up to five questions, each of 1 to 300 characters with 2 to
// 5 distinct choices of 1 to 60, an origin of self_attack or none (an
// attack on the planner's own draft, piece 5); a breach is a finding like
// a card's. The guide gives the field's format and one sentence on it,
// nothing about when to ask (ruling 263).
func TestPlanQuestionsFormat(t *testing.T) {
	p, findings := readPlan(fencePlan(questionPlan()), testShape, testLimits)
	rawWith, with, _ := planSHA(p)
	rawWithout, without, _ := planSHA(testPlan())
	if len(findings) != 0 || len(p.Questions) != 2 || len(p.Questions[0].Choices) != 3 || len(p.Questions[1].Choices) != 2 || with == without ||
		!strings.Contains(string(rawWith), `"questions":[{"text":"How often`) || strings.Contains(string(rawWithout), "questions") {
		t.Fatalf("questions %+v, findings %q; stored %s and %s", p.Questions, findings, rawWith, rawWithout)
	}
	change := func(edit func(p *Plan)) string {
		p := questionPlan()
		edit(&p)
		return fencePlan(p)
	}
	for name, tc := range map[string]struct {
		text string
		want []string
	}{
		"five questions":    {change(func(p *Plan) { p.Questions = slices.Repeat(p.Questions[:1], 5) }), nil},
		"six questions":     {change(func(p *Plan) { p.Questions = slices.Repeat(p.Questions[:1], 6) }), []string{"plan: 6 questions; want 0 to 5"}},
		"one choice":        {change(func(p *Plan) { p.Questions[0].Choices = []string{"60 seconds"} }), []string{"plan: question 1: 1 choices; want 2 to 5"}},
		"six choices":       {change(func(p *Plan) { p.Questions[1].Choices = []string{"a", "b", "c", "d", "e", "f"} }), []string{"plan: question 2: 6 choices; want 2 to 5"}},
		"choice at the cap": {change(func(p *Plan) { p.Questions[1].Choices[1] = strings.Repeat("c", 60) }), nil},
		"long choice":       {change(func(p *Plan) { p.Questions[1].Choices[1] = strings.Repeat("c", 61) }), []string{"plan: question 2: choice 2 is over 60 characters"}},
		"empty choice":      {change(func(p *Plan) { p.Questions[0].Choices[0] = "" }), []string{"plan: question 1: choice 1 is empty"}},
		"duplicate choices": {change(func(p *Plan) { p.Questions[0].Choices[2] = "30 seconds" }), []string{`plan: question 1: choice "30 seconds" is offered twice`}},
		"no text":           {change(func(p *Plan) { p.Questions[0].Text = "" }), []string{"plan: question 1: text empty"}},
		"an attack":         {change(func(p *Plan) { p.Questions[0].Origin = CatchSelfAttack }), nil},
		"unknown origin":    {change(func(p *Plan) { p.Questions[0].Origin = "attack" }), []string{`plan: question 1: origin "attack", want self_attack or none`}},
		"unknown field":     {"```json\n{\"summary\": \"s\", \"cards\": [], \"questions\": [{\"text\": \"q\", \"choices\": [\"a\", \"b\"], \"answer\": \"a\"}]}\n```", []string{`plan: json: unknown field "answer"`}},
	} {
		if got := ValidatePlan(tc.text, testShape, testLimits); !slices.Equal(got, tc.want) {
			t.Errorf("%s: ValidatePlan = %q, want %q", name, got, tc.want)
		}
	}
	// The bounds (0 to 5 questions, 2 to 5 choices) are questionFindings'
	// findings, pinned by the table above, not the prose (roadmap 186).
	g := planGuide(testShape, testLimits)
	if !strings.Contains(g, `"size": "small|medium|large", "rung": "cheap|frontier", "rung_reason": "one line"}], "questions": [{"text": "...", "choices": ["...", "..."]}]}. `) ||
		!strings.Contains(g, "questions: open points the spec leaves to the member; their answers come back to you with your last plan, "+
			"and a plan with open questions cannot be accepted until they are answered.") || strings.Count(g, "question") != 4 {
		t.Fatalf("planGuide names questions %d times, want 4 (the format's key, the attack sentence's one, the sentence's two):\n%s", strings.Count(g, "question"), g)
	}
}

// The store keeps a revision's questions in order and each answer once;
// the confirmation question (piece 5) appends after them once, and its
// answer ends the asking for every later revision.
func TestPGStorePlanQuestions(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plan_questions"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(ctx, Task{ID: "t-plan", Status: "waiting-human", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Lane: "plan", IssueNumber: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary": "one"}`), "s1"); err != nil {
		t.Fatal(err)
	}
	asked := questionPlan().Questions
	for i := range asked { // the store keeps every planner's question as kind question
		asked[i].Kind = "question"
	}
	for i, want := range []bool{true, false} { // a healed RequestPlan puts them again
		if inserted, err := s.PutPlanQuestions(ctx, "t-plan", 1, asked); inserted != want || err != nil {
			t.Fatalf("put %d: inserted %v (%v), want %v", i+1, inserted, err, want)
		}
	}
	got, err := s.PlanQuestions(ctx, "t-plan", 1)
	if open, _ := s.OpenPlanQuestions(ctx, "t-plan", 1); err != nil || fmt.Sprint(got) != fmt.Sprint(asked) || open != 2 {
		t.Fatalf("questions %+v (%v), %d open; want %+v, 2 open", got, err, open, asked)
	}
	before := time.Now()
	if err := s.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "60 seconds", "alice"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.PlanQuestions(ctx, "t-plan", 1)
	if q, open := got[0], must(s.OpenPlanQuestions(ctx, "t-plan", 1)); q.Answer != "60 seconds" || q.AnsweredBy != "alice" || open != 1 ||
		q.AnsweredAt.Before(before.Add(-time.Minute)) || q.AnsweredAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("answered %+v, %d open", q, open)
	}
	for _, tc := range []struct {
		revision, idx int
		want          error
	}{{1, 1, ErrAnswered}, {1, 3, ErrNotFound}, {2, 1, ErrNotFound}, {1, 2, nil}} {
		if err := s.AnswerPlanQuestion(ctx, "t-plan", tc.revision, tc.idx, "30 days", "bob"); !errors.Is(err, tc.want) {
			t.Fatalf("revision %d question %d: %v, want %v", tc.revision, tc.idx, err, tc.want)
		}
	}
	got, _ = s.PlanQuestions(ctx, "t-plan", 1)
	if open := must(s.OpenPlanQuestions(ctx, "t-plan", 1)); open != 0 || got[0].Answer != "60 seconds" || got[1].Answer != "30 days" || got[1].AnsweredBy != "bob" {
		t.Fatalf("questions %+v, %d open", got, open)
	}
	// The confirmation (piece 5): appended after the revision's questions,
	// once however often it runs, and its answer ends the asking for every
	// later revision.
	confirm := PlanQuestion{Text: "Confirmation of the spec and the criteria of plan revision 1: answer okay.", Choices: []string{"okay"}}
	for i, want := range []bool{true, false} {
		if idx, inserted, err := s.PutPlanConfirmation(ctx, "t-plan", 1, confirm); inserted != want || err != nil || (inserted && idx != 3) {
			t.Fatalf("confirmation put %d: idx %d inserted %v (%v), want inserted %v at idx 3", i+1, idx, inserted, err, want)
		}
	}
	if answered, err := s.PlanConfirmationAnswered(ctx, "t-plan"); err != nil || answered {
		t.Fatalf("answered %v (%v), want not yet", answered, err)
	}
	if err := s.AnswerPlanQuestion(ctx, "t-plan", 1, 3, "okay", "alice"); err != nil {
		t.Fatal(err)
	}
	if answered, err := s.PlanConfirmationAnswered(ctx, "t-plan"); err != nil || !answered {
		t.Fatalf("answered %v (%v), want yes", answered, err)
	}
	if got, _ := s.PlanQuestions(ctx, "t-plan", 1); len(got) != 3 || got[2].Answer != "okay" {
		t.Fatalf("questions with the confirmation %+v", got)
	}
	if open := must(s.OpenPlanQuestions(ctx, "t-plan", 1)); open != 0 {
		t.Fatalf("%d open questions, want 0", open)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// RequestPlan of a revision with two questions lists them in the plan
// comment after the table (the confirmation question never reaches the
// comment; it waits in Needs you, its own test), stores them sanitised as
// the comment is (a clicked choice is posted as the member's), records
// one question_asked row each and writes them to the recording rule's
// ledger as the planner's catches, once however often it runs; the
// approval request waits.
func TestRequestPlanAsksTheQuestions(t *testing.T) {
	p := questionPlan()
	p.Questions[1].Choices[1] = "30 days, see https://evil.example"
	r, store, _, fj := newPlanRunner(t, p)
	rec := &fakeEventRecorder{}
	r.timeline.Events = rec
	for range 2 {
		if _, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1); err != nil {
			t.Fatal(err)
		}
	}
	want := "| 3 | all_green | invariant | go test ./... passes | foundation, integration |\n\nQuestions:\n" +
		"1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)\n" +
		"2. How long are the results kept? (choices: 7 days / 30 days, see hxxps://evil[.]example)\n\n<details>"
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0].body, want) {
		t.Fatalf("comments %d, the first:\n%s\nwant it to contain:\n%s", len(fj.comments), fj.comments[0].body, want)
	}
	var rows []string
	for _, e := range rec.rows {
		if e.TaskID == "t-plan" && e.Kind == EventQuestionAsked && e.Via == ViaFactoryd && e.Detail["kind"] == nil {
			rows = append(rows, fmt.Sprintf("%v %v %v %v", e.Detail["revision"], e.Detail["index"], e.Detail["question"], e.Detail["choices"]))
		}
	}
	if !slices.Equal(rows, []string{"1 1 How often does the checker probe a site? [30 seconds 60 seconds 5 minutes]",
		"1 2 How long are the results kept? [7 days 30 days, see hxxps://evil[.]example]"}) {
		t.Fatalf("question_asked rows %q", rows)
	}
	if qs, _ := store.PlanQuestions(context.Background(), "t-plan", 1); len(qs) != 3 || qs[1].Choices[1] != "30 days, see hxxps://evil[.]example" ||
		!strings.HasPrefix(qs[2].Text, "Confirmation of the spec and the criteria of plan revision 1:") || !slices.Equal(qs[2].Choices, []string{"okay"}) {
		t.Fatalf("stored questions %+v", qs)
	}
	if got := store.catches["t-plan 1 "+CatchQuestion+" "+FinderPlanner]; !slices.Equal(got,
		[]string{"How often does the checker probe a site?", "How long are the results kept?"}) {
		t.Fatalf("the recording rule's catches %q", got)
	}
	if len(store.approvals) != 1 || store.approvals[0].Kind != ApprovalPlan || store.approvals[0].Status != "pending" {
		t.Fatalf("approvals %+v", store.approvals)
	}
}

// The confirmation question (the spec-intake concept, piece 5) shows the
// spec and the criteria to the members in Needs you: factoryd appends it
// to a revision with criteria, after the planner's questions, one
// question_asked row of kind confirmation, once however often the
// revision runs. Asked until answered once: the answer ends the asking,
// a later revision asks nothing; a plan without criteria carries no spec
// to confirm.
func TestRequestPlanAsksTheConfirmation(t *testing.T) {
	ctx := context.Background()
	r, store, _, _ := newPlanRunner(t, questionPlan())
	rec := &fakeEventRecorder{}
	r.timeline.Events = rec
	for range 2 {
		if _, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1); err != nil {
			t.Fatal(err)
		}
	}
	want := "Confirmation of the spec and the criteria of plan revision 1: answer okay to confirm them, or answer with what changes; " +
		"the criteria are fixed only when this is answered.\n" +
		"Outcome: A link shortener: links with slugs that redirect and count clicks.\n" +
		"Glossary: slug: the short key a link gets; click: one redirect served\n" +
		"Criteria: links_crud (test), redirect_302 (test), all_green (invariant).\n" +
		"The whole plan is in the plan's comment on this issue."
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if len(qs) != 3 || qs[2].Text != want || !slices.Equal(qs[2].Choices, []string{"okay"}) {
		t.Fatalf("stored questions %+v, want the confirmation third:\n%s", qs, want)
	}
	var rows []string
	for _, e := range rec.rows {
		if e.TaskID == "t-plan" && e.Kind == EventQuestionAsked && e.Detail["kind"] == "confirmation" {
			rows = append(rows, fmt.Sprintf("%v %v %v %v", e.Detail["revision"], e.Detail["index"], e.Detail["kind"], e.Detail["choices"]))
		}
	}
	if !slices.Equal(rows, []string{"1 3 confirmation [okay]"}) {
		t.Fatalf("confirmation rows %q", rows)
	}
	if answered, err := store.PlanConfirmationAnswered(ctx, "t-plan"); err != nil || answered {
		t.Fatalf("answered %v (%v), want not yet", answered, err)
	}
	// the answer ends the asking: revision 2, filed with the answers, asks
	// its questions again but no confirmation
	if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 3, "okay", "alice"); err != nil {
		t.Fatal(err)
	}
	if answered, err := store.PlanConfirmationAnswered(ctx, "t-plan"); err != nil || !answered {
		t.Fatalf("answered %v (%v), want yes", answered, err)
	}
	p2 := questionPlan()
	p2.Summary = "Answered, the same plan again."
	raw, sha, _ := planSHA(p2)
	if _, err := store.AddPlanRevision(ctx, "t-plan", raw, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 2); err != nil {
		t.Fatal(err)
	}
	if qs, _ = store.PlanQuestions(ctx, "t-plan", 2); len(qs) != 2 {
		t.Fatalf("revision 2's questions %+v, want the planner's two only: the confirmation was answered once", qs)
	}
	for _, e := range rec.rows {
		if e.TaskID == "t-plan" && e.Kind == EventQuestionAsked && e.Detail["kind"] == "confirmation" && e.Detail["revision"] == 2 {
			t.Fatalf("revision 2 asked a confirmation: %+v", e.Detail)
		}
	}
	// no criteria, no confirmation: a plan this process gives no spec
	if err := r.askConfirmation(ctx, store.tasks["t-plan"], 3, Plan{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	if qs, _ = store.PlanQuestions(ctx, "t-plan", 3); len(qs) != 0 {
		t.Fatalf("a plan without criteria asked %+v", qs)
	}
}

// The planner's one attack on its own draft (piece 5) reaches the
// recording rule's ledger under its own kind: a question with origin
// self_attack is stored with its origin and written as a catch of kind
// self_attack, the plain questions as kind question, both finder
// planner.
func TestRequestPlanSplitsTheCatches(t *testing.T) {
	p := questionPlan()
	p.Questions[0].Origin = CatchSelfAttack
	r, store, _, _ := newPlanRunner(t, p)
	if _, err := execActivity(t, r, ActivityRequestPlan, "t-plan", 1); err != nil {
		t.Fatal(err)
	}
	if got := store.catches["t-plan 1 "+CatchSelfAttack+" "+FinderPlanner]; !slices.Equal(got, []string{"How often does the checker probe a site?"}) {
		t.Fatalf("the attack's catches %q", got)
	}
	if got := store.catches["t-plan 1 "+CatchQuestion+" "+FinderPlanner]; !slices.Equal(got, []string{"How long are the results kept?"}) {
		t.Fatalf("the plain catches %q", got)
	}
	if qs, _ := store.PlanQuestions(context.Background(), "t-plan", 1); qs[0].Origin != CatchSelfAttack || qs[1].Origin != "" {
		t.Fatalf("stored questions' origins %+v", qs)
	}
}

// signalLog keeps every signal with its payload; err fails each.
type signalLog struct {
	got []string
	err error
}

func (w *signalLog) Start(context.Context, string) error                    { return nil }
func (w *signalLog) StartRoutineRun(context.Context, RoutineRunInput) error { return nil }
func (w *signalLog) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	if w.err != nil {
		return w.err
	}
	w.got = append(w.got, fmt.Sprintf("%s %s %s revision %d index %d", taskID, name, sig.Author, sig.Revision, sig.Index))
	return nil
}

// AnswerPlan stores a member's answer to a question of the newest revision,
// then signals the plan's workflow; the last answer resolves the plan's
// approval as superseded, so Accept cannot file the revision the planner
// revises with the answers. A second answer, an unknown question or a
// stale revision is refused before any signal; a failed signal after the
// stored answer is ErrAnswerUnsignalled. Accept plan refuses a revision
// with an open question before it signals, and CreateChildren's judgement
// refuses it the same way.
func TestAnswerPlanAndAcceptance(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	wf := &signalLog{}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 1, "60 seconds", "alice"); err != nil || store.approvals[0].Status != "pending" {
		t.Fatalf("the first answer: %v, approval %+v", err, store.approvals[0])
	}
	for idx, want := range map[int]error{1: ErrAnswered, 3: ErrNotFound} {
		if err := AnswerPlan(ctx, store, wf, "t-plan", 1, idx, "5 minutes", "bob"); !errors.Is(err, want) {
			t.Fatalf("question %d: %v, want %v", idx, err, want)
		}
	}
	var open ErrOpenQuestions
	if err := AcceptPlan(ctx, store, wf, "t-plan", 1, sha, "sebastian"); !errors.As(err, &open) ||
		err.Error() != "plan #20 revision 1 has 1 unanswered questions; answer them first" {
		t.Fatalf("accept with an open question: %v", err)
	}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 2, "30 days", "bob"); err != nil {
		t.Fatal(err)
	}
	if a := store.approvals[0]; a.Status != "denied" || a.DecidedBy != FactoryDecider || a.DecisionNote != "superseded: the planner revises revision 1 with the answers" {
		t.Fatalf("the approval after the last answer %+v", a)
	}
	if err := AcceptPlan(ctx, store, wf, "t-plan", 1, sha, "sebastian"); err == nil || err.Error() != "task t-plan has no plan waiting for acceptance" {
		t.Fatalf("accept once the planner revises: %v", err)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Answer != "60 seconds" || qs[0].AnsweredBy != "alice" || !slices.Equal(wf.got, []string{"t-plan plan_answered alice revision 1 index 1",
		"t-plan plan_answered bob revision 1 index 2"}) {
		t.Fatalf("questions %+v, signals %q", qs, wf.got)
	}
	// revision 2 asks one question and waits: an answer to revision 1 is
	// stale; one whose signal fails is recorded all the same
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "sha-2")
	store.PutPlanQuestions(ctx, "t-plan", 2, questionPlan().Questions[:1])
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 2, "30 days", "bob"); err == nil || err.Error() != "the plan changed to revision 2; reload" || len(wf.got) != 2 {
		t.Fatalf("a stale revision: %v, signals %q", err, wf.got)
	}
	wf.err = errors.New("temporal down")
	err := AnswerPlan(ctx, store, wf, "t-plan", 2, 1, "60 seconds", "alice")
	if qs, _ := store.PlanQuestions(ctx, "t-plan", 2); !errors.Is(err, ErrAnswerUnsignalled) ||
		err.Error() != "the answer is recorded, but the plan's workflow was not signalled: temporal down" || qs[0].Answer != "60 seconds" || store.approvals[1].Status != "pending" {
		t.Fatalf("a failed signal: %v, questions %+v, approval %+v", err, qs, store.approvals[1])
	}

	r, rstore, _, fj := newPlanRunner(t, questionPlan())
	rstore.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	rstore.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	refused := "plan #1 revision 1 has 2 unanswered questions; answer them first"
	if res := createChildren(t, r); !slices.Equal(res.Refused, []string{refused}) || fj.next != 1 ||
		rstore.approvals[0].DecisionNote != "accepting revision 1 was refused: "+refused {
		t.Fatalf("result %+v, issues up to #%d, approval %+v", res, fj.next, rstore.approvals[0])
	}
}

// The planner session after the members' answers gets them as the brief's
// answers, "<question>: <answer>" in the questions' order, beside its last
// plan; its plan_revision step names them.
func TestRunPlanTakesTheAnswers(t *testing.T) {
	ctx := context.Background()
	plan := Task{ID: "t-plan", Kind: "agent", Status: "running", Lane: policy.LanePlan, Process: "spec", Repository: "linkshort/linkshort",
		SHA: "abc123", IssueNumber: 1, Author: "alice", AuthorWriter: true, RequiredCapabilities: []string{"planner"}, BudgetEUR: 2, Objective: "Uptime checker"}
	kube, objects := newFakeKube(), &fakeObjectStore{}
	runner, _, store := newTestRunner(t, plan, kube, objects)
	pinned := fixtureVersion(t)
	runner.cfg.PolicyVersion = func(_ context.Context, id int64) (policy.Version, bool, error) { return pinned, id == 7, nil }
	runner.cfg.Policy = func() policy.Version { return pinned }
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.AnswerPlanQuestion(ctx, "t-plan", 1, 2, "30 days", "bob")
	store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "60 seconds", "alice")
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/big", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	answered := questionPlan()
	answered.Questions = nil // the answers are in; the plan is answered whole, changed
	answered.Summary = "Probe every 60 seconds, keep 30 days, then the plan as filed."
	b, err := json.Marshal(answered)
	if err != nil {
		t.Fatal(err)
	}
	if f, err := runner.FilePlan(ctx, "t-plan", string(b)); err != nil || f.Revision != 2 {
		t.Fatalf("the session's filing: %+v, %v", f, err)
	}
	kube.scriptPods(0, resultLine(true, "done"))
	if err := <-done; err != nil || res.Revision != 2 {
		t.Fatalf("RunPlan: %+v, %v", res, err)
	}
	env := podEnv(kube.podAt(0))
	want := `["How often does the checker probe a site?: 60 seconds","How long are the results kept?: 30 days"]`
	if env["ANSWERS"] != want || !strings.Contains(env["FINDINGS"], "your last plan, revision 1") {
		t.Fatalf("ANSWERS %s, want %s; FINDINGS %s", env["ANSWERS"], want, env["FINDINGS"])
	}
	if step := timelineStep(t, objects, "t-plan", "plan_revision"); step["revision"] != float64(2) {
		t.Fatalf("plan_revision %v", step)
	}
}

// On Postgres: the batch stores every answer in one statement; a second
// answer (ErrAnswered) or an unknown idx (ErrNotFound) in a batch stores
// none of it.
func TestPGStoreAnswerPlanQuestions(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(ctx, schematest.DSN(t, "factoryd_plan_answers"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, id := range []string{"t-plan", "t-plan-bad"} {
		if err := s.CreateTask(ctx, Task{ID: id, Status: "waiting-human", Kind: "agent", Repository: "sf/r", BudgetEUR: 1, Lane: "plan", IssueNumber: int64(20 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary": "one"}`), "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := s.AnswerPlanQuestions(ctx, "t-plan", 1, []PlanAnswer{{Idx: 2, Text: "30 days"}, {Idx: 1, Text: "60 seconds"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.PlanQuestions(ctx, "t-plan", 1)
	if open, _ := s.OpenPlanQuestions(ctx, "t-plan", 1); open != 0 || got[0].Answer != "60 seconds" || got[0].AnsweredBy != "alice" ||
		got[1].Answer != "30 days" || got[1].AnsweredAt.Before(before.Add(-time.Minute)) {
		t.Fatalf("questions %+v, %d open", got, open)
	}
	for name, tc := range map[string]struct {
		id       string
		revision int
		answers  []PlanAnswer
		want     error
	}{
		"answered already": {"t-plan", 1, []PlanAnswer{{Idx: 1, Text: "5 minutes"}}, ErrAnswered},
		"unknown idx":      {"t-plan", 1, []PlanAnswer{{Idx: 2, Text: "7 days"}, {Idx: 4, Text: "no"}}, ErrNotFound},
		"no such revision": {"t-plan", 2, []PlanAnswer{{Idx: 1, Text: "5 minutes"}}, ErrNotFound},
	} {
		if err := s.AnswerPlanQuestions(ctx, tc.id, tc.revision, tc.answers, "bob"); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
	}
	got, _ = s.PlanQuestions(ctx, "t-plan", 1)
	if got[0].Answer != "60 seconds" || got[0].AnsweredBy != "alice" || got[1].Answer != "30 days" || got[1].AnsweredBy != "alice" {
		t.Fatalf("questions after the refusals %+v", got)
	}
	// a partial batch — one question answered, one still open — stores
	// neither, and a bad entry leaves no row of its own behind
	if _, err := s.AddPlanRevision(ctx, "t-plan-bad", json.RawMessage(`{"summary": "one"}`), "sb1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPlanQuestions(ctx, "t-plan-bad", 1, questionPlan().Questions); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPlanQuestion(ctx, "t-plan-bad", 1, 1, "60 seconds", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerPlanQuestions(ctx, "t-plan-bad", 1, []PlanAnswer{{Idx: 1, Text: "5 minutes"}, {Idx: 2, Text: "7 days"}, {Idx: 9, Text: "no"}}, "carol"); !errors.Is(err, ErrAnswered) {
		t.Fatalf("a partial batch with a bad idx: %v, want ErrAnswered", err)
	}
	if got, _ := s.PlanQuestions(ctx, "t-plan-bad", 1); got[1].Answer != "" || got[0].Answer != "60 seconds" || got[0].AnsweredBy != "alice" {
		t.Fatalf("questions after the partial batch %+v", got)
	}
	if err := s.AnswerPlanQuestions(ctx, "t-plan-bad", 1, []PlanAnswer{{Idx: 2, Text: "7 days"}}, "carol"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PlanQuestions(ctx, "t-plan-bad", 1); got[1].Answer != "7 days" || got[1].AnsweredBy != "carol" {
		t.Fatalf("the open question after the batch %+v", got[1])
	}
}

// AnswerPlanQuestions is the batch answer: one call stores every answer of
// the newest revision, signals the plan's workflow once per answer and,
// when no question stays open, resolves the plan's approval as superseded.
func TestAnswerPlanQuestionsBatch(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	wf := &signalLog{}
	if err := AnswerPlanQuestions(ctx, store, wf, "t-plan", 1, []PlanAnswer{{Idx: 1, Text: "60 seconds"}, {Idx: 2, Text: "30 days"}}, "alice"); err != nil {
		t.Fatal(err)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Answer != "60 seconds" || qs[0].AnsweredBy != "alice" || qs[1].Answer != "30 days" || qs[1].AnsweredBy != "alice" ||
		!slices.Equal(wf.got, []string{"t-plan plan_answered alice revision 1 index 1", "t-plan plan_answered alice revision 1 index 2"}) {
		t.Fatalf("questions %+v, signals %q", qs, wf.got)
	}
	if a := store.approvals[0]; a.Status != "denied" || a.DecidedBy != FactoryDecider || a.DecisionNote != "superseded: the planner revises revision 1 with the answers" {
		t.Fatalf("the approval after the batch %+v", a)
	}
	if open, err := store.OpenPlanQuestions(ctx, "t-plan", 1); err != nil || open != 0 {
		t.Fatalf("%d open questions (%v), want 0", open, err)
	}
}

// A batch whose entries hold one already-answered question refuses the
// whole call with ErrAnswered and stores none.
func TestAnswerPlanQuestionsPartialError(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	if err := store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "60 seconds", "alice"); err != nil {
		t.Fatal(err)
	}
	wf := &signalLog{}
	err := AnswerPlanQuestions(ctx, store, wf, "t-plan", 1, []PlanAnswer{{Idx: 1, Text: "5 minutes"}, {Idx: 2, Text: "30 days"}}, "bob")
	if !errors.Is(err, ErrAnswered) || len(wf.got) != 0 {
		t.Fatalf("%v, signals %q, want ErrAnswered and none", err, wf.got)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Answer != "60 seconds" || qs[0].AnsweredBy != "alice" || qs[1].Answer != "" || store.approvals[0].Status != "pending" {
		t.Fatalf("questions %+v, approval %+v", qs, store.approvals[0])
	}
	if open, err := store.OpenPlanQuestions(ctx, "t-plan", 1); err != nil || open != 1 {
		t.Fatalf("%d open questions (%v), want 1", open, err)
	}
}

// A batch with an idx no question of the revision has refuses the whole
// call with ErrNotFound and stores none.
func TestAnswerPlanQuestionsNotFound(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	wf := &signalLog{}
	err := AnswerPlanQuestions(ctx, store, wf, "t-plan", 1, []PlanAnswer{{Idx: 2, Text: "30 days"}, {Idx: 3, Text: "weekly"}}, "bob")
	if !errors.Is(err, ErrNotFound) || len(wf.got) != 0 {
		t.Fatalf("%v, signals %q, want ErrNotFound and none", err, wf.got)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Answer != "" || qs[1].Answer != "" || store.approvals[0].Status != "pending" {
		t.Fatalf("questions %+v, approval %+v", qs, store.approvals[0])
	}
	if open, err := store.OpenPlanQuestions(ctx, "t-plan", 1); err != nil || open != 2 {
		t.Fatalf("%d open questions (%v), want 2", open, err)
	}
}

// The members' answers send the planner back once none is open: an answer
// that leaves a question open waits on, the last one starts exactly one
// planner session (RunPlan reads the answers), whose new revision is
// accepted; an answer to a revision no longer waiting is recorded and
// ignored. Intake signals no change request for an answer's comment
// (TestIntakeKnowsAPlanAnswer).
func TestPlanLoopAnswersSendThePlannerBack(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"}, PlanResult{Revision: 2, SHA256: "sha-2"})
	h.open = []int{1, 0}
	h.signalAfter(time.Hour, SignalPlanAnswered, TaskSignal{Author: "alice", Revision: 1, Index: 1})
	h.signalAfter(2*time.Hour, SignalPlanAnswered, TaskSignal{Author: "bob", Revision: 1, Index: 2})
	h.signalAfter(3*time.Hour, SignalPlanAnswered, TaskSignal{Author: "carol", Revision: 1, Index: 2})
	h.signalAfter(4*time.Hour, SignalPlanAccepted, TaskSignal{Author: "sebastian", Revision: 2, SHA256: "sha-2"})
	if final := h.run(t); final != "done" || h.finals[0][1] != "plan revision 2 accepted: 3 cards filed" {
		t.Fatalf("finals %v", h.finals)
	}
	if len(h.plans) != 2 || h.plans[1] != nil || !slices.Equal(h.requests, []int{1, 2}) || !slices.Equal(h.created, []string{"2 sebastian 7"}) ||
		!slices.Equal(h.asked, []int{1, 1}) {
		t.Fatalf("plans %q requests %v created %v asked %v", h.plans, h.requests, h.created, h.asked)
	}
	var answered []string
	for _, s := range h.recorded {
		if s.step == "signal" && s.detail["signal"] == SignalPlanAnswered {
			answered = append(answered, fmt.Sprintf("%v %v %v %v", s.detail["author"], s.detail["revision"], s.detail["index"], cmpOrAny(s.detail["ignored"])))
		}
	}
	if !slices.Equal(answered, []string{"alice 1 1 ", "bob 1 2 ", "carol 1 2 the plan changed to revision 2"}) {
		t.Fatalf("recorded %q", answered)
	}
}

// An answer that arrives with the Reject stays unread by the plan's wait
// and is drained when the run ends: no unhandled signal.
func TestPlanLoopDrainsAnAnswerAtTheEnd(t *testing.T) {
	h := newPlanHarness(t, PlanResult{Revision: 1, SHA256: "sha-1"})
	h.signalAfter(time.Hour, SignalPlanRejected, TaskSignal{Author: "alice", Body: "not this quarter"})
	h.signalAfter(time.Hour, SignalPlanAnswered, TaskSignal{Author: "bob", Revision: 1, Index: 1})
	if final := h.run(t); final != "failed" || strings.Contains(h.logs.String(), "unhandled signals") {
		t.Fatalf("finals %v, logs:\n%s", h.finals, h.logs)
	}
}

// A member's answer to a plan's question, which the console posts as
// "Answer <n>: <answer>" once it stored the answer, is no change request:
// intake records it as signalled (the console signalled the workflow),
// sends no human_comment and records no question_answered row, but counts
// as an escalation answer, in whichever revision it answered (a late,
// healed one included). A comment naming an open question or another
// answer, or any other comment on the plan, is a change request.
func TestIntakeKnowsAPlanAnswer(t *testing.T) {
	ctx := context.Background()
	in, store, _ := newTestIntake(t)
	m, reader := testMetrics(t)
	in.Metrics = m
	store.tasks["t-plan"] = Task{ID: "t-plan", Repository: "sf/ai-software-factory-code", IssueNumber: 20, Status: "waiting-human", Kind: "agent", Lane: policy.LanePlan}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "60 seconds", "sebastian")
	rec := &fakeEventRecorder{}
	in.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: rec, Store: store}
	comment := func(id int, body string) {
		b, _ := json.Marshal(map[string]any{"action": "created", "issue": map[string]any{"number": 20}, "repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"comment": map[string]any{"id": id, "user": map[string]string{"login": "sebastian"}, "body": body}})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("comment %d: status %d", id, rr.Code)
		}
	}
	for i, body := range []string{PlanAnswerComment(1, "60 seconds"), "Answer 2: 30 days", "Answer 1: 5 minutes", "split the web card"} {
		comment(91+i, body)
	}
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "sha-2")
	comment(95, PlanAnswerComment(1, "60 seconds"))
	want := []string{"t-plan human_comment sebastian #0 c92: Answer 2: 30 days", "t-plan human_comment sebastian #0 c93: Answer 1: 5 minutes",
		"t-plan human_comment sebastian #0 c94: split the web card"}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) || len(store.intakeEvents) != 5 || !store.intakeEvents[0].Signalled || !store.intakeEvents[4].Signalled ||
		!slices.Equal(answerEvents(rec, "t-plan"), []string{"sebastian webhook 92", "sebastian webhook 93", "sebastian webhook 94"}) {
		t.Fatalf("signals %q, want %q; intake events %+v; question_answered rows %q", got, want, store.intakeEvents, answerEvents(rec, "t-plan"))
	}
	wantSums(t, sums(t, reader, "factory.human.intervention"), map[string]float64{"kind=escalation_answer,lane=plan,process=none": 5})
}
