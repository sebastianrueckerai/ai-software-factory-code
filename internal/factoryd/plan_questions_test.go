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
// 5 distinct choices of 1 to 60; a breach is a finding like a card's. The
// guide gives the field's format and one sentence on it, nothing about
// when to ask (ruling 263).
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
		"text at the cap":   {change(func(p *Plan) { p.Questions[0].Text = strings.Repeat("q", 300) }), nil},
		"long text":         {change(func(p *Plan) { p.Questions[0].Text = strings.Repeat("q", 301) }), []string{"plan: question 1: text over 300 characters"}},
		"unknown field":     {"```json\n{\"summary\": \"s\", \"cards\": [], \"questions\": [{\"text\": \"q\", \"choices\": [\"a\", \"b\"], \"answer\": \"a\"}]}\n```", []string{`plan: json: unknown field "answer"`}},
	} {
		if got := ValidatePlan(tc.text, testShape, testLimits); !slices.Equal(got, tc.want) {
			t.Errorf("%s: ValidatePlan = %q, want %q", name, got, tc.want)
		}
	}
	g := planGuide(testShape, testLimits)
	if !strings.Contains(g, `"size": "small|medium|large"}], "questions": [{"text": "...", "choices": ["...", "..."]}]}. `) ||
		!strings.Contains(g, "questions: 0 to 5 open points the spec leaves to the member, each with 2 to 5 short choices; the member's answers come back to you "+
			"as a block of answers with your last plan, and a plan with questions cannot be accepted until they are answered.") || strings.Count(g, "question") != 3 {
		t.Fatalf("planGuide names questions %d times, want 3 (the format's key, the sentence's two):\n%s", strings.Count(g, "question"), g)
	}
}

// The store keeps a revision's questions in order and each answer once.
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
	for range 2 { // a healed RequestPlan puts them again
		if err := s.PutPlanQuestions(ctx, "t-plan", 1, asked); err != nil {
			t.Fatal(err)
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
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// RequestPlan of a revision with two questions lists them in the plan
// comment after the table, stores them sanitised as the comment is (a
// clicked choice is posted as the member's) and records one
// question_asked row each, once however often it runs; the approval
// request waits.
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
	want := "| 4 | integration | integration | Integration: acceptance tests | medium | foundation, links-api, redirects |  |\n\nQuestions:\n" +
		"1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)\n" +
		"2. How long are the results kept? (choices: 7 days / 30 days, see hxxps://evil[.]example)\n\n<details>"
	if len(fj.comments) != 1 || !strings.Contains(fj.comments[0].body, want) {
		t.Fatalf("comments %d, the first:\n%s\nwant it to contain:\n%s", len(fj.comments), fj.comments[0].body, want)
	}
	var rows []string
	for _, e := range rec.rows {
		if e.TaskID == "t-plan" && e.Kind == EventQuestionAsked && e.Via == ViaFactoryd {
			rows = append(rows, fmt.Sprintf("%v %v %v %v", e.Detail["revision"], e.Detail["index"], e.Detail["question"], e.Detail["choices"]))
		}
	}
	if !slices.Equal(rows, []string{"1 1 How often does the checker probe a site? [30 seconds 60 seconds 5 minutes]",
		"1 2 How long are the results kept? [7 days 30 days, see hxxps://evil[.]example]"}) {
		t.Fatalf("question_asked rows %q", rows)
	}
	if qs, _ := store.PlanQuestions(context.Background(), "t-plan", 1); len(qs) != 2 || qs[1].Choices[1] != "30 days, see hxxps://evil[.]example" {
		t.Fatalf("stored questions %+v", qs)
	}
	if len(store.approvals) != 1 || store.approvals[0].Kind != ApprovalPlan || store.approvals[0].Status != "pending" {
		t.Fatalf("approvals %+v", store.approvals)
	}
}

// signalLog keeps every signal with its payload.
type signalLog struct{ got []string }

func (w *signalLog) Start(context.Context, string) error { return nil }
func (w *signalLog) Signal(_ context.Context, taskID, name string, sig TaskSignal) error {
	w.got = append(w.got, fmt.Sprintf("%s %s %s revision %d index %d", taskID, name, sig.Author, sig.Revision, sig.Index))
	return nil
}

// AnswerPlan stores a member's answer to a question of the newest revision,
// then signals the plan's workflow; a second answer, an unknown question
// or a stale revision is refused before any signal. Accept plan refuses a
// revision with an open question before it signals, and CreateChildren's
// judgement refuses it the same way.
func TestAnswerPlanAndAcceptance(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tasks["t-plan"] = Task{ID: "t-plan", Kind: "agent", Status: "waiting-human", Lane: "plan", IssueNumber: 20, Repository: "linkshort/linkshort"}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.OpenApproval(ctx, ApprovalRequest{Kind: ApprovalPlan, TaskID: "t-plan"})
	wf := &signalLog{}
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 1, "60 seconds", "alice"); err != nil {
		t.Fatal(err)
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
	if err := AcceptPlan(ctx, store, wf, "t-plan", 1, sha, "sebastian"); err != nil {
		t.Fatal(err)
	}
	qs, _ := store.PlanQuestions(ctx, "t-plan", 1)
	if qs[0].Answer != "60 seconds" || qs[0].AnsweredBy != "alice" || !slices.Equal(wf.got, []string{"t-plan plan_answered alice revision 1 index 1",
		"t-plan plan_answered bob revision 1 index 2", "t-plan plan_accepted sebastian revision 1 index 0"}) {
		t.Fatalf("questions %+v, signals %q", qs, wf.got)
	}
	store.AddPlanRevision(ctx, "t-plan", json.RawMessage(`{"summary":"two"}`), "sha-2")
	if err := AnswerPlan(ctx, store, wf, "t-plan", 1, 2, "30 days", "bob"); err == nil || err.Error() != "the plan changed to revision 2; reload" || len(wf.got) != 3 {
		t.Fatalf("a stale revision: %v, signals %q", err, wf.got)
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
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.AnswerPlanQuestion(ctx, "t-plan", 1, 2, "30 days", "bob")
	store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "60 seconds", "alice")
	var res PlanResult
	done := make(chan error)
	go func() {
		val, err := execActivity(t, runner, ActivityRunPlan, "t-plan", "factory/coding-frontier", int64(7), []string(nil))
		if err == nil {
			err = val.Get(&res)
		}
		done <- err
	}()
	awaitPod(t, kube, done)
	kube.scriptPods(0, resultLine(true, fencePlan(testPlan())))
	if err := <-done; err != nil || res.Revision != 2 {
		t.Fatalf("RunPlan: %+v, %v", res, err)
	}
	env := podEnv(kube.podAt(0))
	want := `["How often does the checker probe a site?: 60 seconds","How long are the results kept?: 30 days"]`
	if env["ANSWERS"] != want || !strings.Contains(env["FINDINGS"], "your last plan, revision 1") {
		t.Fatalf("ANSWERS %s, want %s; FINDINGS %s", env["ANSWERS"], want, env["FINDINGS"])
	}
	if step := timelineStep(t, objects, "t-plan", "plan_revision"); fmt.Sprint(step["answers"]) !=
		"[How often does the checker probe a site?: 60 seconds How long are the results kept?: 30 days]" {
		t.Fatalf("plan_revision %v", step)
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

// A member's answer to a plan's question, which the console posts as
// "Answer <n>: <answer>" once it stored the answer, is no change request:
// intake records it as signalled (the console signalled the workflow),
// sends no human_comment and counts no question_answered. A comment
// naming an open question or another answer, or any other comment on the
// plan, is one.
func TestIntakeKnowsAPlanAnswer(t *testing.T) {
	ctx := context.Background()
	in, store, _ := newTestIntake(t)
	store.tasks["t-plan"] = Task{ID: "t-plan", Repository: "sf/ai-software-factory-code", IssueNumber: 20, Status: "waiting-human", Kind: "agent", Lane: policy.LanePlan}
	raw, sha, _ := planSHA(questionPlan())
	store.AddPlanRevision(ctx, "t-plan", raw, sha)
	store.PutPlanQuestions(ctx, "t-plan", 1, questionPlan().Questions)
	store.AnswerPlanQuestion(ctx, "t-plan", 1, 1, "60 seconds", "sebastian")
	rec := &fakeEventRecorder{}
	in.Timeline = &Timeline{Objects: &fakeObjectStore{}, Events: rec, Store: store}
	for i, body := range []string{PlanAnswerComment(1, "60 seconds"), "Answer 2: 30 days", "Answer 1: 5 minutes", "split the web card"} {
		b, _ := json.Marshal(map[string]any{"action": "created", "issue": map[string]any{"number": 20}, "repository": map[string]any{"full_name": "sf/ai-software-factory-code"},
			"comment": map[string]any{"id": 91 + i, "user": map[string]string{"login": "sebastian"}, "body": body}})
		if rr := postWebhook(in, "issue_comment", b, sign(webhookSecret, b)); rr.Code != http.StatusOK {
			t.Fatalf("comment %d: status %d", 91+i, rr.Code)
		}
	}
	want := []string{"t-plan human_comment sebastian #0 c92: Answer 2: 30 days", "t-plan human_comment sebastian #0 c93: Answer 1: 5 minutes",
		"t-plan human_comment sebastian #0 c94: split the web card"}
	if got := workflowsOf(in).signals; !slices.Equal(got, want) || len(store.intakeEvents) != 4 || !store.intakeEvents[0].Signalled ||
		!slices.Equal(answerEvents(rec, "t-plan"), []string{"sebastian webhook 92", "sebastian webhook 93", "sebastian webhook 94"}) {
		t.Fatalf("signals %q, want %q; intake events %+v; question_answered rows %q", got, want, store.intakeEvents, answerEvents(rec, "t-plan"))
	}
}
