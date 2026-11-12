package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// askedQuestions are a plan revision's two open questions, of 3 and 2
// choices.
func askedQuestions() []factoryd.PlanQuestion {
	return []factoryd.PlanQuestion{
		{Text: "How often does the checker probe a site?", Choices: []string{"30 seconds", "60 seconds", "5 minutes"}},
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
	}
}

// questionsRig is planRig (plan t-plan #1 of linkshort waiting at
// revision 2) with revision 2's two questions open.
func questionsRig(r *rig) {
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": askedQuestions()}
}

// The plan card asks the plan's open questions above its
// Request changes, one button per choice and a text box each, its meta
// line counting the open ones, and offers no Accept; an answer posts "Answer <n>:
// <answer>" on the issue as the member, signals factoryd and shows on the
// card; a second answer to it, one to a stale revision or to no question,
// and an empty or long one are refused.
func TestPlanCardAsksTheQuestions(t *testing.T) {
	r := newRig(t)
	questionsRig(r)
	ck, s := r.loggedIn(t)
	board := func() string {
		return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
	}
	card := board()
	for _, want := range []string{`<p class="ask">How often does the checker probe a site?</p>`, `<span class="pos" aria-live="polite">1 of 2</span>`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="1">`,
		`<input type="radio" name="choice" value="2">`, `<input type="text" id="own-t-plan-1" name="own" maxlength="1000" hx-preserve>`,
		`<p class="meta">2 questions open. The answers go to the planner; its next revision with no open question files the cards.</p>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	if strings.Count(card, `action="/tasks/t-plan/plan/answer"`) != 1 || strings.Count(card, `<input type="radio"`) != 3 || strings.Contains(card, " checked>") ||
		strings.Index(card, `action="/tasks/t-plan/plan/answer"`) > strings.Index(card, `action="/tasks/t-plan/plan/changes"`) || strings.Contains(card, "Accept plan") {
		t.Errorf("want one answer form with 3 unchecked choices above Request changes, and no Accept:\n%s", card)
	}
	const comments = "/api/v1/repos/linkshort/linkshort/issues/1/comments"
	answer := func(revision, index, text string) *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {revision}, "index": {index}, "answer": {text},
			"project": {"linkshort"}}), ck)
	}
	if rec := answer("2", "1", "60 seconds"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" ||
		r.fg.comments[comments] != "Answer 1: 60 seconds" || !slices.Contains(r.fg.calls, "SIGNAL plan_answered t-plan 2 1 by sebastian") ||
		!slices.Contains(r.fg.calls, "POST "+comments+" Bearer access-1") {
		t.Fatalf("answer: %d to %q %s; comment %q, calls %v", rec.Code, rec.Header().Get("Location"), rec.Body, r.fg.comments[comments], r.fg.calls)
	}
	if card := board(); strings.Contains(card, "How often does the checker probe a site?") || !strings.Contains(card, `<p class="ask">How long are the results kept?</p>`) ||
		!strings.Contains(card, `<p class="meta">1 question open. The answers go to the planner`) || strings.Count(card, `<input type="radio"`) != 2 || strings.Contains(card, `class="carousel"`) {
		t.Errorf("the card after one answer:\n%s", card)
	}
	r.fg.calls = nil
	for _, tc := range []struct {
		revision, index, text string
		status                int
		want                  string
	}{
		{"2", "1", "5 minutes", http.StatusConflict, "Question 1 of plan #1 is answered already."},
		{"1", "2", "30 days", http.StatusConflict, "the plan changed to revision 2; reload"},
		{"2", "3", "30 days", http.StatusConflict, "Plan #1 revision 2 has no question 3."},
		{"2", "2", " \n ", http.StatusBadRequest, "an answer of 1 to 1000 characters is required"},
		{"2", "2", strings.Repeat("a", 1001), http.StatusBadRequest, "an answer of 1 to 1000 characters is required"},
	} {
		if rec := answer(tc.revision, tc.index, tc.text); rec.Code != tc.status || rec.Body.String() != tc.want+"\n" || len(r.fg.calls) != 0 {
			t.Errorf("revision %s question %s %q: %d %q, calls %v; want %d %q", tc.revision, tc.index, tc.text, rec.Code, rec.Body, r.fg.calls, tc.status, tc.want)
		}
	}
	// the last answer, typed on two lines, is one
	if rec := answer("2", "2", " 30\n days "); rec.Code != http.StatusSeeOther || r.fg.comments[comments] != "Answer 2: 30 days" {
		t.Fatalf("the second answer: %d, comment %q", rec.Code, r.fg.comments[comments])
	}
	if card := board(); strings.Contains(card, " open. ") || !strings.Contains(card, `<p class="meta">The answers go to the planner;`) || strings.Contains(card, `action="/tasks/t-plan/plan/answer"`) {
		t.Errorf("the card once answered:\n%s", card)
	}
	// an answer stored whose signal failed: 502, the answer recorded,
	// request_changes the way on
	r.c.cfg.AnswerPlan = func(context.Context, string, int, int, string, string, string) error {
		return fmt.Errorf("%w: temporal down", factoryd.ErrAnswerUnsignalled)
	}
	if rec := answer("2", "1", "5 minutes"); rec.Code != http.StatusBadGateway || rec.Body.String() !=
		"The answer is recorded, but factoryd could not pass it to the plan; request_changes sends the plan to the planner with the answers.\n" ||
		r.fg.comments[comments] != "Answer 2: 30 days" {
		t.Errorf("an unsignalled answer: %d %q, comment %q", rec.Code, rec.Body, r.fg.comments[comments])
	}
}

// The project agent reads plan t-p #20's open questions on its card,
// numbered with their choices, and answers one by its revision and number:
// the proposal names the question, Do it answers it as the member, one row
// via chat. An answer without its revision, or to another one, is refused;
// with no question open, an answer to one, or a decision, is refused with
// the plan's actions.
func TestChatAnswersAPlanQuestion(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(k factoryd.BoardTask) bool { return k.ID == "t-p" })].Status = "waiting-human"
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": askedQuestions()}
	r.tasks.mu.Unlock()
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: "t-p", Project: "sf/code",
		Status: "pending", Spec: []byte(`{"revision":1,"sha256":"5e1f"}`)})
	read := reads(t, r, g, "card", `{"task_id":"t-p"}`, "plan", `{"task_id":"t-p"}`)
	q1, q2 := "1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)", "2. How long are the results kept? (choices: 7 days / 30 days)"
	if card, plan := read[0], read[1]; !strings.Contains(card, "\nQuestions:\n"+q1+"\n"+q2) ||
		!strings.Contains(plan, "\nsummary: An API, then its web UI.\n"+q1+"\n"+q2+"\napi foundation") {
		t.Fatalf("the card read =\n%s\nthe plan read =\n%s\nwant both to hold the questions", card, plan)
	}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	args := `{"task_id":"t-p","revision":1,"question":1,"text":"60 seconds"}`
	g.replies = [][]string{{call(1, "answer", args)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"probe every minute"}})
	equal(t, "stream", events, []string{`proposal {"arguments":` + args + `,"description":"Answer a card, #20 t-p \"Build the link shortener.\", text 60 seconds, revision 1, ` +
		`question 1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)","expires":"EXPIRES","id":"TOKEN","tool":"answer"}`, `done {}`})
	_, events, _ = chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	equal(t, "Do it", events, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"answer","result":"Answered question 1 of plan #20."}`,
		`applied {"result":"Answered question 1 of plan #20.","tool":"answer"}`, `done {}`})
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/code/issues/20/comments"], "Answer 1: 60 seconds")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20,
		Kind: factoryd.EventPlanQuestionAnswered, Detail: map[string]any{"revision": 1, "index": 1, "answer": "60 seconds"}})
	if plan := reads(t, r, g, "plan", `{"task_id":"t-p"}`)[0]; !strings.Contains(plan, "\n"+q1+" answered: 60 seconds (sebastian)\n"+q2+"\napi") {
		t.Fatalf("the plan read after the answer =\n%s", plan)
	}
	for _, tc := range []struct {
		in   toolInput
		want []any
	}{
		{toolInput{TaskID: "t-p", Question: 2, Text: "7 days"}, []any{http.StatusBadRequest, "A plan's question is answered with its revision (revision), from the plan tool."}},
		{toolInput{TaskID: "t-p", Revision: 2, Question: 2, Text: "7 days"}, []any{http.StatusConflict, "the plan changed to revision 1; reload"}},
	} {
		status, msg := refused(t, r, "answer", tc.in)
		equal(t, "an answer without its revision or of another", []any{status, msg}, tc.want)
	}
	r.tasks.answer("t-p", 1, 2, "", "30 days", "sebastian")
	for _, in := range []toolInput{{TaskID: "t-p", Revision: 1, Question: 2, Text: "7 days"}, {TaskID: "t-p", Decision: factoryd.DecisionConfirm, Text: "fine"}} {
		status, msg := refused(t, r, "answer", in)
		equal(t, "an answer with no question open", []any{status, msg}, []any{http.StatusConflict,
			"Card #20 waits on a human: an answer, an approval or a decision, not an open plan question: answer its open questions (answer). Its next revision with none files the cards, request_changes sends it back to the planner."})
	}
}

// batchAnswerRig is the reads rig with plan t-p #20 waiting on a human at
// revision 1, whose two questions are open and which holds its pending
// plan request.
func batchAnswerRig(t *testing.T) (*rig, *chatGateway) {
	t.Helper()
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(k factoryd.BoardTask) bool { return k.ID == "t-p" })].Status = "waiting-human"
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": askedQuestions()}
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: "t-p", Project: "sf/code",
		Status: "pending", Spec: []byte(`{"revision":1,"sha256":"5e1f"}`)})
	return r, g
}

// answerAs runs the answer tool on in as the rig's member of the chat's
// project, outside a turn, and answers what the action noted.
func (r *rig) answerAs(t *testing.T, s *session, in toolInput) (string, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/chat", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	p, ok, err := r.c.projectOf(req.Context(), "ai-software-factory-code")
	if err != nil || !ok {
		t.Fatalf("the chat's project: %v, %v", ok, err)
	}
	trn := &turn{c: r.c, r: req, a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat}, p: p}
	tool, _ := r.c.chatTool("answer")
	note, _, err := tool.run(trn, in)
	return note, err
}

// chatTurnOf is the member's turn on the chat project, as the route builds
// it; its stream events land in events.
func (r *rig) chatTurnOf(t *testing.T, s *session, events *[]string) *turn {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/projects/ai-software-factory-code/chat", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	p, ok, err := r.c.projectOf(req.Context(), "ai-software-factory-code")
	if err != nil || !ok {
		t.Fatalf("the chat's project: %v, %v", ok, err)
	}
	v, _ := r.c.inForce(req.Context())
	trn := &turn{c: r.c, r: req, a: actor{s: s, repository: p.Repository, via: factoryd.ViaChat}, p: p,
		cfg: v.Document.ChatOf(v.Projects[p.Name])}
	trn.stream(func(line string) error {
		event, data := "", ""
		for _, f := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
			switch {
			case strings.HasPrefix(f, "event: "):
				event = strings.TrimPrefix(f, "event: ")
			case strings.HasPrefix(f, "data: "):
				data = strings.TrimPrefix(f, "data: ")
			}
		}
		*events = append(*events, event+" "+durationField.ReplaceAllString(data, `"duration_ms":0`))
		return nil
	}, nil)
	return trn
}

// The answer tool takes every open question of a plan revision in one
// call: answers as a list of {question, text}. One call stores all
// answers, posts one comment each on the issue as the member, records one
// event each and returns the combined note; the proposal names each
// question.
func TestChatAnswersAllPlanQuestionsAtOnce(t *testing.T) {
	r, g := batchAnswerRig(t)
	r.tasks.questions["t-p 1"] = append(r.tasks.questions["t-p 1"], factoryd.PlanQuestion{Text: "Who may add a site?", Choices: []string{"anyone", "the owner"}})
	ck, s := r.loggedIn(t)
	args := `{"task_id":"t-p","revision":1,"answers":[{"question":1,"text":"60 seconds"},{"question":2,"text":"30 days"},{"question":3,"text":"the owner"}]}`
	g.replies = [][]string{{call(1, "answer", args)}}
	_, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"probe every minute, keep them for a month"}})
	equal(t, "stream", events, []string{`proposal {"arguments":` + args + `,"description":"Answer a card, #20 t-p \"Build the link shortener.\", revision 1, ` +
		`question 1: 60 seconds, question 2: 30 days, question 3: the owner","expires":"EXPIRES","id":"TOKEN","tool":"answer"}`, `done {}`})
	before := r.events.count()
	note, err := r.answerAs(t, s, toolInput{TaskID: "t-p", Revision: 1, Answers: []PlanAnswerInput{{Question: 1, Text: "60 seconds"}, {Question: 2, Text: "30 days"}, {Question: 3, Text: "the owner"}}})
	if err != nil {
		t.Fatalf("one call = %q, %v", note, err)
	}
	equal(t, "the combined note", note, "Answered question 1: 60 seconds, question 2: 30 days, question 3: the owner of plan #20.")
	const comments = "/api/v1/repos/sf/code/issues/20/comments"
	equal(t, "the comments", r.fg.commentBodies(comments), []string{"Answer 1: 60 seconds", "Answer 2: 30 days", "Answer 3: the owner"})
	for _, want := range []string{"SIGNAL plan_answered t-p 1 1 by sebastian", "SIGNAL plan_answered t-p 1 2 by sebastian", "SIGNAL plan_answered t-p 1 3 by sebastian"} {
		if !slices.Contains(r.fg.calls, want) {
			t.Errorf("the calls = %v, want it to hold %s", r.fg.calls, want)
		}
	}
	posts := 0
	for _, call := range r.fg.calls {
		if call == "POST "+comments+" Bearer access-1" {
			posts++
		}
	}
	equal(t, "the comment posts", posts, 3)
	qs, err := r.tasks.PlanQuestions(context.Background(), "t-p", 1)
	if err != nil || qs[0].Answer != "60 seconds" || qs[0].AnsweredBy != "sebastian" || qs[1].Answer != "30 days" || qs[1].AnsweredBy != "sebastian" || qs[2].Answer != "the owner" || qs[2].AnsweredBy != "sebastian" {
		t.Errorf("the questions after one call = %+v, %v; want both answered", qs, err)
	}
	chatRows(t, r.events, before, "sebastian",
		factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20, Kind: factoryd.EventPlanQuestionAnswered,
			Detail: map[string]any{"revision": 1, "index": 1, "answer": "60 seconds"}},
		factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20, Kind: factoryd.EventPlanQuestionAnswered,
			Detail: map[string]any{"revision": 1, "index": 2, "answer": "30 days"}},
		factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20, Kind: factoryd.EventPlanQuestionAnswered,
			Detail: map[string]any{"revision": 1, "index": 3, "answer": "the owner"}})
	if plan := reads(t, r, g, "plan", `{"task_id":"t-p"}`)[0]; !strings.Contains(plan,
		"\n"+q1Line()+" answered: 60 seconds (sebastian)\n"+q2Line()+" answered: 30 days (sebastian)\n3. Who may add a site? (choices: anyone / the owner) answered: the owner (sebastian)\n") {
		t.Errorf("the plan read after one call =\n%s", plan)
	}
}

// A refusal whose error factoryd names carries that error and the next
// step to the chat, not the factoryd log: the agent reads why the answers
// did not land and answers again from the plan.
func TestAnswerToolRefusalNamesTheError(t *testing.T) {
	r, _ := batchAnswerRig(t)
	_, s := r.loggedIn(t)
	r.c.cfg.AnswerPlanQuestions = func(context.Context, string, int, []factoryd.PlanAnswer, string) error {
		return errors.New("plan t-x revision 1: idx 2 out of range")
	}
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	tool, _ := r.c.chatTool("answer")
	args := `{"task_id":"t-p","revision":1,"answers":[{"question":1,"text":"60 seconds"},{"question":2,"text":"30 days"}]}`
	in := toolInput{TaskID: "t-p", Revision: 1, Answers: []PlanAnswerInput{{Question: 1, Text: "60 seconds"}, {Question: 2, Text: "30 days"}}}
	got, _ := trn.runTool(tool, args, in)
	equal(t, "the tool result", got,
		"Refused: The answers could not be recorded: plan t-x revision 1: idx 2 out of range. "+
			"Read the plan again and answer the open questions of its newest revision.")
}

// q1Line and q2Line are the rig's two questions as the plan read numbers
// them with their choices.
func q1Line() string {
	return "1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)"
}

func q2Line() string {
	return "2. How long are the results kept? (choices: 7 days / 30 days)"
}

// The answer tool's params hold answers: an array whose items each carry
// a question and a text, the question alone required, since a confirm
// carries no words; task_id alone is required, since only a plan's
// question takes a revision, which its answer refuses without.
func TestChatAnswerToolSchemaHasAnswersArray(t *testing.T) {
	r := newRig(t)
	tool, ok := r.c.chatTool("answer")
	if !ok {
		t.Fatal("the agent is offered no answer tool")
	}
	list, ok := tool.params["answers"].(map[string]any)
	if !ok || list["type"] != "array" {
		t.Fatalf("params answers = %#v, want an array", tool.params["answers"])
	}
	items, ok := list["items"].(map[string]any)
	if !ok || items["type"] != "object" {
		t.Fatalf("answers items = %#v, want objects", list["items"])
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("answers items' properties = %#v", items["properties"])
	}
	for _, name := range []string{"question", "text"} {
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Errorf("answers items lack property %q", name)
			continue
		}
		if want := map[string]string{"question": "integer", "text": "string"}[name]; p["type"] != want {
			t.Errorf("answers item %q is %v, want %s", name, p["type"], want)
		}
	}
	equal(t, "an item's required arguments", items["required"], []string{"question"})
	equal(t, "the required arguments", tool.required, []string{"task_id"})
	def := tool.def(false)
	schema, _ := def.Parameters.(map[string]any)
	params, ok := schema["properties"].(map[string]any)
	if !ok || params["answers"] == nil {
		t.Fatalf("the tool's parameters = %#v, they name no answers", def.Parameters)
	}
	if !strings.Contains(def.Description, "the plan's confirmation by your own decision") {
		t.Errorf("the tool's about = %q", def.Description)
	}
	if d := params["decision"].(map[string]any)["description"].(string); !strings.Contains(d, "confirm accepts the plan") {
		t.Errorf("the decision parameter = %v", params["decision"])
	}
}

// The confirmation question is two controls, not a choice list and not a
// free-text form: a Confirm press stores the decision, carries the
// revision's cards to the gate with no planner session and names it;
// Request changes takes required words to the next planner round, and
// bare text names the two controls and stores nothing. While the
// confirmation is open its Request changes is the card's only one. The
// answered line shows the decision with its findings, and the night's
// words ride a confirm as findings.
func TestPlanCardDecidesTheConfirmation(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {
		{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}}}
	ck, s := r.loggedIn(t)
	board := func() string {
		return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
	}
	press := func(kv ...string) *httptest.ResponseRecorder {
		v := url.Values{"csrf": {s.csrf}, "project": {"linkshort"}}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return r.do(htmxPost("/tasks/t-plan/plan/answer", v), ck)
	}
	const comments = "/api/v1/repos/linkshort/linkshort/issues/1/comments"
	night := "okay — confirmed. Revision 15 is fine."
	card := board()
	for _, want := range []string{
		`<p class="ask">Confirm the spec and the criteria, or request changes.</p>`,
		`<input type="hidden" name="decision" value="confirm">`,
		"<button type=\"submit\" class=\"primary\">Confirm</button>",
		`<input type="hidden" name="decision" value="revise">`,
		"Request changes: your words go to the next planner round",
		`name="answer" maxlength="1000" required`,
		"<p class=\"meta\">1 question open. The answers go to the planner",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	if strings.Count(card, `action="/tasks/t-plan/plan/answer"`) != 2 || strings.Contains(card, `name="choice"`) {
		t.Errorf("the confirmation is two forms, no choice buttons:\n%s", card)
	}
	if strings.Contains(card, `action="/tasks/t-plan/plan/changes"`) {
		t.Errorf("the open confirmation's Request changes is the card's only one:\n%s", card)
	}
	// Request changes without words: refused by name, nothing stored
	if rec := press("revision", "2", "index", "1", "decision", "revise"); rec.Code != http.StatusBadRequest ||
		rec.Body.String() != "Request changes needs its words: they go to the next planner round.\n" ||
		slices.Contains(r.fg.calls, "SIGNAL plan_answered t-plan 2 1 by sebastian") || len(r.fg.comments) != 0 {
		t.Fatalf("words-less revise: %d %q, calls %v", rec.Code, rec.Body, r.fg.calls)
	}
	// bare text names the controls
	if rec := press("revision", "2", "index", "1", "answer", "It does"); rec.Code != http.StatusConflict || rec.Body.String() !=
		"Question 1 of plan #1 is its confirmation: Confirm fixes the spec and the criteria, Request changes sends your words to the planner.\n" {
		t.Fatalf("bare text: %d %q", rec.Code, rec.Body)
	}
	// the night's answer: confirm with the words as findings
	if rec := press("revision", "2", "index", "1", "decision", "confirm", "answer", night); !pressRefreshes(rec) ||
		!slices.Contains(r.fg.calls, "SIGNAL plan_answered t-plan 2 1 by sebastian") ||
		r.fg.comments[comments] != "Answer 1: confirm · "+night {
		t.Fatalf("the night's confirm: %d %q, calls %v, comment %q", rec.Code, rec.Body, r.fg.calls, r.fg.comments[comments])
	}
	if card = board(); strings.Contains(card, `action="/tasks/t-plan/plan/answer"`) || !strings.Contains(card, `action="/tasks/t-plan/plan/changes"`) {
		t.Errorf("the card after the confirm:\n%s", card)
	}
	if d := planDrawer(t, r, ck); !strings.Contains(d, `<p class="answered">Does the plan match the spec?: confirm · `+night+` (sebastian)</p>`) {
		t.Errorf("the drawer after the confirm lacks the answered line:\n%s", d)
	}
	// Request changes with words, on a fresh round
	r.tasks.questions["t-plan 2"][0] = factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}
	if rec := press("revision", "2", "index", "1", "decision", "revise", "answer", "keep the slate clean"); !pressRefreshes(rec) ||
		r.fg.comments[comments] != "Answer 1: revise · keep the slate clean" {
		t.Fatalf("the revise: %d %q, comment %q", rec.Code, rec.Body, r.fg.comments[comments])
	}
}

// The chat's answer tool decides the plan's confirmation itself: it reads
// the human's words, sets confirm or revise and carries the reasons as
// the text. The night's answer, confirm with the prose as findings, files
// the revision; revise carries its words to the next planner round; text
// with no decision is refused by name.
func TestChatDecidesTheConfirmation(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(k factoryd.BoardTask) bool { return k.ID == "t-p" })].Status = "waiting-human"
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": {
		{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}}}
	r.tasks.mu.Unlock()
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: "t-p", Project: "sf/code",
		Status: "pending", Spec: []byte(`{"revision":1,"sha256":"5e1f"}`)})
	ck, s := r.loggedIn(t)
	before := r.events.count()
	night := "okay — confirmed. Revision 15 is fine."
	args := `{"task_id":"t-p","revision":1,"question":1,"decision":"confirm","text":"` + night + `"}`
	g.replies = [][]string{{call(1, "answer", args)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"the human confirmed it"}})
	equal(t, "stream", events, []string{`proposal {"arguments":` + args + `,"description":"Answer a card, #20 t-p \"Build the link shortener.\", text ` + night +
		`, revision 1, question 1. Does the plan match the spec? (decide: confirm or revise), decision confirm","expires":"EXPIRES","id":"TOKEN","tool":"answer"}`, `done {}`})
	_, events, _ = chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	equal(t, "Do it", events, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"answer","result":"Confirmed plan #20; its cards file."}`,
		`applied {"result":"Confirmed plan #20; its cards file.","tool":"answer"}`, `done {}`})
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/code/issues/20/comments"], "Answer 1: confirm · "+night)
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20,
		Kind: factoryd.EventPlanQuestionAnswered, Detail: map[string]any{"revision": 1, "index": 1, "answer": night, "decision": "confirm"}})
	// a fresh round: revise carries its words
	r.tasks.mu.Lock()
	r.tasks.questions["t-p 1"][0] = factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}
	r.tasks.mu.Unlock()
	rargs := `{"task_id":"t-p","revision":1,"question":1,"decision":"revise","text":"keep the slate clean"}`
	g.replies = [][]string{{call(1, "answer", rargs)}}
	_, _, rtokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"the human wants less"}})
	if _, events, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+rtokens[0], nil); !slices.Contains(events,
		`applied {"result":"Your changes are on plan #20; the planner answers a new revision.","tool":"answer"}`) {
		t.Fatalf("the revise's events: %v", events)
	}
	equal(t, "the revise's comment", r.fg.comments["/api/v1/repos/sf/code/issues/20/comments"], "Answer 1: revise · keep the slate clean")
	// text with no decision names the two controls; revise without words
	// names its rule
	r.tasks.mu.Lock()
	r.tasks.questions["t-p 1"][0] = factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}
	r.tasks.mu.Unlock()
	for _, tc := range []struct {
		in   toolInput
		want []any
	}{
		{toolInput{TaskID: "t-p", Revision: 1, Question: 1, Text: "It does"}, []any{http.StatusConflict,
			"Question 1 of plan #20 is its confirmation: Confirm fixes the spec and the criteria, Request changes sends your words to the planner."}},
		{toolInput{TaskID: "t-p", Revision: 1, Question: 1, Decision: factoryd.DecisionRevise}, []any{http.StatusBadRequest,
			"Request changes needs its words: they go to the next planner round."}},
	} {
		status, msg := refused(t, r, "answer", tc.in)
		equal(t, "the confirmation answered wrong", []any{status, msg}, tc.want)
	}
}

// A Confirm on a revision that also asked real questions files nothing
// yet: the answers go to the planner's next revision. A bare confirm in a
// batch is the decision alone: no words, the note and the comment name
// the decision.
func TestConfirmBesideRealQuestions(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}, Answer: "30 days", AnsweredBy: "sebastian"},
		{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}}}
	ck, s := r.loggedIn(t)
	rec := r.do(htmxPost("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"2"},
		"decision": {"confirm"}}), ck)
	if !pressRefreshes(rec) ||
		r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"] != "Answer 2: confirm" {
		t.Fatalf("the confirm beside a real question: %d %q, comment %q", rec.Code, rec.Body, r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"])
	}
	r.tasks.questions["t-plan 2"] = []factoryd.PlanQuestion{{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
		{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}}
	a := actor{s: s, via: "chat"}
	note, err := r.c.doAnswerPlanQuestions(context.Background(), a, "t-plan", 2, []PlanAnswerInput{{Question: 1, Text: "7 days"}, {Question: 2, Decision: factoryd.DecisionConfirm}})
	if err != nil || note != "Answered question 1: 7 days, question 2: confirm of plan #1." ||
		r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"] != "Answer 2: confirm" {
		t.Fatalf("the batch with a bare confirm: %q, %v, comment %q", note, err, r.fg.comments["/api/v1/repos/linkshort/linkshort/issues/1/comments"])
	}
}

// The project agent reads a decided confirmation as answered: the card
// lists it under no open question, and the plan read shows its decision.
func TestChatReadsADecidedConfirmation(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(k factoryd.BoardTask) bool { return k.ID == "t-p" })].Status = "waiting-human"
	qs := askedQuestions()
	qs[0].Answer, qs[0].AnsweredBy = "60 seconds", "sebastian"
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": append(qs,
		factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?", Decision: factoryd.DecisionConfirm, AnsweredBy: "sebastian"})}
	r.tasks.mu.Unlock()
	read := reads(t, r, g, "card", `{"task_id":"t-p"}`, "plan", `{"task_id":"t-p"}`)
	if card, plan := read[0], read[1]; !strings.Contains(card, "\nQuestions:\n"+q2Line()+"\n") || strings.Contains(card, "Does the plan match") ||
		!strings.Contains(plan, "\n3. Does the plan match the spec? (decide: confirm or revise) answered: confirm (sebastian)\n") {
		t.Fatalf("the card read =\n%s\nthe plan read =\n%s", card, plan)
	}
}

// A confirmation answered while the round's hold-outs wait is refused by
// name, on the card's press and in a batch: the refusal says what waits
// and what to do, not where the log is.
func TestTheConfirmationNamesTheOpenHoldouts(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}}}
	r.c.cfg.AnswerPlan = func(context.Context, string, int, int, string, string, string) error { return factoryd.ErrHoldoutsOpen }
	r.c.cfg.AnswerPlanQuestions = func(context.Context, string, int, []factoryd.PlanAnswer, string) error {
		return factoryd.ErrHoldoutsOpen
	}
	ck, s := r.loggedIn(t)
	const want = "The hold-outs of plan #1 wait for your okay or drop: decide each, then the confirmation."
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"1"},
		"decision": {"confirm"}}), ck)
	if rec.Code != http.StatusConflict || rec.Body.String() != want+"\n" {
		t.Errorf("the press: %d %q", rec.Code, rec.Body)
	}
	_, err := r.c.doAnswerPlanQuestions(context.Background(), actor{s: s, via: "chat"}, "t-plan", 2, []PlanAnswerInput{{Question: 1, Decision: factoryd.DecisionConfirm}})
	var ref *refusal
	if !errors.As(err, &ref) || ref.status != http.StatusConflict || ref.msg != want {
		t.Errorf("the batch: %v", err)
	}
}

// planGuardRig is the reads rig's plan t-p #20 waiting at revision 1 with
// the confirmation, or the two questions, open; takeOff names merged cards
// the revision takes off main.
func planGuardRig(t *testing.T, confirmation bool, takeOff ...string) (*rig, *turn, *[]string) {
	t.Helper()
	r, _ := newReadsRig(t)
	plan, _ := json.Marshal(factoryd.Plan{Summary: "An API.", TakeOffMain: takeOff})
	qs := askedQuestions()
	if confirmation {
		qs = []factoryd.PlanQuestion{{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"}}
	}
	r.tasks.mu.Lock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(k factoryd.BoardTask) bool { return k.ID == "t-p" })].Status = "waiting-human"
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": qs}
	r.tasks.plans = map[string][]factoryd.PlanRevision{"t-p": {{TaskID: "t-p", Revision: 1, Plan: plan, SHA256: "5e1f"}}}
	r.tasks.mu.Unlock()
	_, s := r.loggedIn(t)
	var events []string
	trn := r.chatTurnOf(t, s, &events)
	trn.cfg.Confirm = nil
	return r, trn, &events
}

const (
	refusedOffMain = "Refused: This revision takes merged work off main, or its reverts could not be read, so its confirmation is the human's, made with the plan's Confirm button."
	refusedBelowC  = "Refused: A plan confirmation below full auto is the human's, made with the plan's Confirm button."
	refusedBelowA  = "Refused: A plan's answer below full auto is the human's, made on the plan's card with its choices or its Request changes button."
	agentStep      = " Record your analysis and recommended option with attach_analysis."
)

// By hand: with answer off the confirm list a wake's answer runs at once
// as the agent, and the guard decides it, whether it answers one question
// or all in one call. At full auto a revision that takes nothing off main
// confirms and an answer to a plan question passes; one that takes merged
// work off main is refused naming the Confirm button, and below full auto
// the confirmation and the answers are refused, each with the agent's next
// step. request_changes is the same rule: the agent's passes at full auto
// on any revision and is refused below it.
func TestAWakeAnswersAPlanOnlyWhereTheGuardPasses(t *testing.T) {
	const confirm = `{"task_id":"t-p","revision":1,"question":1,"decision":"confirm","text":"fine"}`
	const answer = `{"task_id":"t-p","revision":1,"question":1,"text":"60 seconds"}`
	const batchConfirm = `{"task_id":"t-p","revision":1,"answers":[{"question":1,"decision":"confirm","text":"fine"}]}`
	const batchAnswers = `{"task_id":"t-p","revision":1,"answers":[{"question":1,"text":"60 seconds"},{"question":2,"text":"7 days"}]}`
	const changes = `{"task_id":"t-p","text":"split the API card"}`
	for _, c := range []struct {
		name, level, tool, args string
		confirmation            bool
		takeOff                 []string
		want                    string
	}{
		{"full auto confirms", policy.AutonomyFullAuto, "answer", confirm, true, nil, ""},
		{"full auto, work off main", policy.AutonomyFullAuto, "answer", confirm, true, []string{"api"}, refusedOffMain + agentStep},
		{"autonomous confirm", policy.AutonomyAutonomous, "answer", confirm, true, nil, refusedBelowC + agentStep},
		{"full auto answers a question", policy.AutonomyFullAuto, "answer", answer, false, nil, ""},
		{"autonomous question", policy.AutonomyAutonomous, "answer", answer, false, nil, refusedBelowA + agentStep},
		{"full auto batch confirm", policy.AutonomyFullAuto, "answer", batchConfirm, true, nil, ""},
		{"full auto batch confirm, work off main", policy.AutonomyFullAuto, "answer", batchConfirm, true, []string{"api"}, refusedOffMain + agentStep},
		{"autonomous batch confirm", policy.AutonomyAutonomous, "answer", batchConfirm, true, nil, refusedBelowC + agentStep},
		{"full auto batch of answers", policy.AutonomyFullAuto, "answer", batchAnswers, false, nil, ""},
		{"autonomous batch of answers", policy.AutonomyAutonomous, "answer", batchAnswers, false, nil, refusedBelowA + agentStep},
		{"full auto request_changes, work off main", policy.AutonomyFullAuto, "request_changes", changes, true, []string{"api"}, ""},
		{"autonomous request_changes", policy.AutonomyAutonomous, "request_changes", changes, true, nil, refusedBelowA + agentStep},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, trn, _ := planGuardRig(t, c.confirmation, c.takeOff...)
			trn.a.level, trn.a.wake, trn.a.birth = c.level, true, factoryd.WakeAsk
			res, proposal, _ := trn.call(factoryd.OperatorCall{Name: c.tool, Arguments: c.args})
			wrote := len(r.fg.comments) > 0
			if c.want == "" && (proposal != "" || !wrote || strings.HasPrefix(res, "Refused")) ||
				c.want != "" && (res != c.want || wrote || slices.ContainsFunc(r.fg.calls, func(s string) bool { return strings.HasPrefix(s, "SIGNAL") })) {
				t.Errorf("result %q, comments %v, calls %v; want %q", res, r.fg.comments, r.fg.calls, c.want)
			}
		})
	}
}

// A human's own message that carries the confirmation decides it as the
// human at every level, in a thread with history; a human's turn that
// absorbed a timer's or a watch's row, queued before or after the human's
// line, decides as the agent, so it cannot confirm a revision that takes
// work off main at autonomous.
func TestAPlanAnswerIsTheHumansOnlyOnTheLineTheyTyped(t *testing.T) {
	const confirm = `{"task_id":"t-p","revision":1,"question":1,"decision":"confirm","text":"fine"}`
	const changes = `{"task_id":"t-p","text":"split the API card"}`
	for _, c := range []struct {
		name, level, birth, tool, args, want string
		absorbed                             int // 0 none, 1 and 2 a wake's row queued before and after the human's line, 3 a human's row landed after it, 4 a wake turn with the line, 5 no line of its own, 6 a line whose row left the thread
	}{
		{"the human's line at autonomous", policy.AutonomyAutonomous, "", "answer", confirm, "Confirmed plan #20", 0},
		{"the human's line at full auto, work off main", policy.AutonomyFullAuto, "", "answer", confirm, "Confirmed plan #20", 0},
		{"the human's line, request_changes at autonomous", policy.AutonomyAutonomous, "", "request_changes", changes, "Your change request is on plan #20", 0},
		{"a timer's row absorbed", policy.AutonomyAutonomous, "wake_timer", "answer", confirm, refusedBelowC + agentStep, 1},
		{"a watch's row absorbed", policy.AutonomyAutonomous, "wake_card", "answer", confirm, refusedBelowC + agentStep, 2},
		{"a timer's row absorbed at full auto, work off main", policy.AutonomyFullAuto, "wake_timer", "answer", confirm, refusedOffMain + agentStep, 1},
		{"a watch's row absorbed, request_changes at autonomous", policy.AutonomyAutonomous, "wake_card", "request_changes", changes, refusedBelowA + agentStep, 2},
		{"a queued human row landed after the turn's own row", policy.AutonomyAutonomous, "", "answer", confirm, "Confirmed plan #20", 3},
		{"a wake turn, whatever rows it holds", policy.AutonomyAutonomous, "", "answer", confirm, refusedBelowC + agentStep, 4},
		{"a turn with no row of its own that lands a human's", policy.AutonomyAutonomous, "", "answer", confirm, refusedBelowC + agentStep, 5},
		{"a line whose row left the thread before a human's next", policy.AutonomyAutonomous, "", "answer", confirm, refusedBelowC + agentStep, 6},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, trn, _ := planGuardRig(t, true, "api")
			trn.a.level = c.level
			trn.owner = "sebastian"
			conv, err := r.chat.NewConversation(t.Context(), "sf/code", "sebastian")
			if err != nil {
				t.Fatal(err)
			}
			trn.conv = conv
			add := func(m factoryd.ChatMessage) {
				m.Conversation = conv
				if _, err := r.chat.AddChatMessage(t.Context(), "sf/code", "sebastian", m); err != nil {
					t.Fatal(err)
				}
			}
			add(factoryd.ChatMessage{Role: "user", Content: "hello"})
			add(factoryd.ChatMessage{Role: "assistant", Content: "Hello."})
			wake := factoryd.ChatMessage{Role: "user", Content: "The timer you set on #20 is due.", Queued: true, BirthKind: c.birth}
			human := factoryd.ChatMessage{Role: "user", Content: "confirm it: the plan matches", Queued: c.absorbed == 1 || c.absorbed == 2}
			switch c.absorbed {
			case 0:
				add(human)
			case 1:
				add(wake)
				add(human)
				trn.landUnread()
			case 2:
				add(human)
				add(wake)
				trn.landUnread()
			case 3:
				add(human)
				add(factoryd.ChatMessage{Role: "user", Content: "and what about #12?", Queued: true})
				trn.landUnread()
			case 4:
				add(human)
				trn.a.wake = true
			case 5:
				add(factoryd.ChatMessage{Role: "user", Content: "yes, confirm", Queued: true})
				trn.landUnread()
			case 6:
				add(human)
				add(factoryd.ChatMessage{Role: "user", Content: "yes, confirm"})
			}
			rows, _ := r.chat.ChatMessages(t.Context(), "sf/code", "sebastian", conv, 50)
			if i := slices.IndexFunc(rows, func(m factoryd.ChatMessage) bool { return m.Content == human.Content }); i >= 0 {
				trn.line = rows[i].ID
			}
			if c.absorbed == 6 {
				if _, err := r.chat.AbandonChatMessage(t.Context(), "sf/code", "sebastian", conv, trn.line); err != nil {
					t.Fatal(err)
				}
			}
			if err := trn.begin(); err != nil {
				t.Fatal(err)
			}
			res, _, _ := trn.call(factoryd.OperatorCall{Name: c.tool, Arguments: c.args})
			if !strings.HasPrefix(res, c.want) || strings.HasPrefix(c.want, "Refused") && res != c.want {
				t.Errorf("result %q, want %q", res, c.want)
			}
		})
	}
}

// The record of an agent's plan answer names the agent: the stored answer
// and the workflow signal's author are the chat's agent, the event reads
// decider agent, the wake that made it and the thread; a human's own line
// leaves the human's login.
func TestAnAgentsPlanAnswerRecordsTheAgentAndItsWake(t *testing.T) {
	const confirm = `{"task_id":"t-p","revision":1,"question":1,"decision":"confirm","text":"fine"}`
	const changes = `{"task_id":"t-p","text":"split the API card"}`
	r, trn, _ := planGuardRig(t, true)
	trn.a.level, trn.a.wake, trn.a.birth, trn.conv = policy.AutonomyFullAuto, true, factoryd.WakeAsk, 7
	before := r.events.count()
	if res, _, _ := trn.call(factoryd.OperatorCall{Name: "answer", Arguments: confirm}); !strings.HasPrefix(res, "Confirmed plan #20") {
		t.Fatalf("result %q", res)
	}
	if !slices.Contains(r.fg.calls, "SIGNAL plan_answered t-p 1 1 by "+factoryd.ChatUser) {
		t.Errorf("the signal's author: %v", r.fg.calls)
	}
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20, Kind: factoryd.EventPlanQuestionAnswered,
		Detail: map[string]any{"revision": 1, "index": 1, "answer": "fine", "decision": "confirm", "birth": factoryd.WakeAsk,
			"autonomy": policy.AutonomyFullAuto, "decider": "agent", "thread": int64(7)}})
	before = r.events.count()
	if res, _, _ := trn.call(factoryd.OperatorCall{Name: "request_changes", Arguments: changes}); !strings.HasPrefix(res, "Your change request is on plan #20") {
		t.Fatalf("result %q", res)
	}
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20, Kind: factoryd.EventPlanChangesRequested,
		Detail: map[string]any{"birth": factoryd.WakeAsk, "autonomy": policy.AutonomyFullAuto, "decider": "agent", "thread": int64(7)}})
}

// The batch path stores the agent's answers as factory-chat's too.
func TestAnAgentsBatchAnswerNamesTheAgent(t *testing.T) {
	r, trn, _ := planGuardRig(t, false)
	trn.a.level, trn.a.wake, trn.a.birth = policy.AutonomyFullAuto, true, factoryd.WakeAsk
	res, _, _ := trn.call(factoryd.OperatorCall{Name: "answer", Arguments: `{"task_id":"t-p","revision":1,"answers":[{"question":1,"text":"60 seconds"},{"question":2,"text":"7 days"}]}`})
	if !strings.HasPrefix(res, "Answered") || !slices.ContainsFunc(r.fg.calls, func(c string) bool {
		return strings.HasPrefix(c, "SIGNAL plan_answered t-p 1") && strings.HasSuffix(c, " by "+factoryd.ChatUser)
	}) {
		t.Errorf("result %q, calls %v; want the batch signalled by %s", res, r.fg.calls, factoryd.ChatUser)
	}
}

// A human's turn that absorbed a wake's row gives the agent's answer: the
// stored answer and the signal name the agent, the event names the thread.
// A human's own line stores the human's login.
func TestAHumansLineStoresTheHumanAndAnAbsorbedTurnTheAgent(t *testing.T) {
	const confirm = `{"task_id":"t-p","revision":1,"question":1,"decision":"confirm","text":"fine"}`
	for _, c := range []struct{ name, by string }{{"the human's line", "sebastian"}, {"a wake's row absorbed", factoryd.ChatUser}} {
		t.Run(c.name, func(t *testing.T) {
			r, trn, _ := planGuardRig(t, true)
			trn.a.level = policy.AutonomyFullAuto
			humanTurn(t, r, trn, c.by == factoryd.ChatUser)
			before := r.events.count()
			if res, _, _ := trn.call(factoryd.OperatorCall{Name: "answer", Arguments: confirm}); !strings.HasPrefix(res, "Confirmed plan #20") {
				t.Fatalf("result %q", res)
			}
			if !slices.Contains(r.fg.calls, "SIGNAL plan_answered t-p 1 1 by "+c.by) {
				t.Errorf("the signal's author: %v, want %s", r.fg.calls, c.by)
			}
			token := "access-1"
			if c.by == factoryd.ChatUser {
				token = r.chatToken
			}
			if !slices.Contains(r.fg.calls, "POST /api/v1/repos/sf/code/issues/20/comments Bearer "+token) {
				t.Errorf("the comment's poster: %v, want the token %q", r.fg.calls, token)
			}
			if c.by != "sebastian" {
				chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20, Kind: factoryd.EventPlanQuestionAnswered,
					Detail: map[string]any{"revision": 1, "index": 1, "answer": "fine", "decision": "confirm", "autonomy": policy.AutonomyFullAuto, "decider": "agent", "thread": trn.conv}})
			}
		})
	}
}

// humanTurn makes trn sebastian's turn on a new thread, run on their own
// row "confirm it"; absorbed queues a watch's row before it, which the turn
// lands, so the turn decides as the agent.
func humanTurn(t *testing.T, r *rig, trn *turn, absorbed bool) {
	t.Helper()
	trn.owner = "sebastian"
	conv, err := r.chat.NewConversation(t.Context(), "sf/code", "sebastian")
	if err != nil {
		t.Fatal(err)
	}
	trn.conv = conv
	if absorbed {
		if _, err := r.chat.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{Conversation: conv, Role: "user",
			Content: "The watch fired.", Queued: true, BirthKind: "wake_card"}); err != nil {
			t.Fatal(err)
		}
	}
	id, err := r.chat.AddChatMessage(t.Context(), "sf/code", "sebastian", factoryd.ChatMessage{Conversation: conv, Role: "user", Content: "confirm it"})
	if err != nil {
		t.Fatal(err)
	}
	trn.line = id
	trn.landUnread()
	if err := trn.begin(); err != nil {
		t.Fatal(err)
	}
}

// rowsAfterDown is the chat store whose read of a thread's rows after an
// id fails.
type rowsAfterDown struct{ Chat }

func (rowsAfterDown) ChatMessagesAfter(context.Context, string, string, int64, int64, int) ([]factoryd.ChatMessage, error) {
	return nil, errors.New("the store is down")
}

// A turn whose own row cannot be read decides nothing: the human's line
// confirms no plan, and the agent's word does not stand in for it.
func TestAnUnreadLineDecidesNoPlanAnswer(t *testing.T) {
	r, trn, _ := planGuardRig(t, true)
	trn.a.level = policy.AutonomyFullAuto
	humanTurn(t, r, trn, false)
	r.c.cfg.Chat = rowsAfterDown{r.c.cfg.Chat}
	res, _, _ := trn.call(factoryd.OperatorCall{Name: "answer", Arguments: `{"task_id":"t-p","revision":1,"question":1,"decision":"confirm","text":"fine"}`})
	if want := "Refused: The thread could not be read, so nothing was decided. Try again."; res != want ||
		slices.ContainsFunc(r.fg.calls, func(s string) bool { return strings.HasPrefix(s, "SIGNAL") || strings.HasPrefix(s, "POST") }) {
		t.Errorf("result %q, calls %v; want %q and nothing signalled or posted", res, r.fg.calls, want)
	}
}

// By hand: the plan path's comments go out as their decider. In a human's
// turn that absorbed a wake's row the agent decides, so its request_changes,
// each answer of its batch and its replan's note post with the project
// agent's token, as factory-chat; on the human's own line they post with
// the human's. A
// replan names the same login as its person: the human's on their line,
// factory-chat's in an absorbed turn and in a wake, whose own session the
// chat token is. With no chat token to read, the agent's change request and
// replan are refused before anything is posted or replanned.
func TestThePlanPathPostsAsItsDecider(t *testing.T) {
	const comments = "POST /api/v1/repos/sf/code/issues/20/comments Bearer "
	const changes, replan = `{"task_id":"t-p","text":"split the API card"}`, `{"task_id":"t-p","note":"split the API card"}`
	const batch = `{"task_id":"t-p","revision":1,"answers":[{"question":1,"text":"60 seconds"},{"question":2,"text":"7 days"}]}`
	for _, c := range []struct {
		name, tool, args string
		turn             string // line: the human's own, absorbed: a wake's row landed, wake: a wake's turn
		token, by        string
	}{
		{"request_changes on the human's line", "request_changes", changes, "line", "access-1", ""},
		{"request_changes in an absorbed turn", "request_changes", changes, "absorbed", "chat-token", ""},
		{"a replan on the human's line", "replan", replan, "line", "access-1", "sebastian"},
		{"a replan in an absorbed turn", "replan", replan, "absorbed", "chat-token", factoryd.ChatUser},
		{"a replan in a wake", "replan", replan, "wake", "chat-token", factoryd.ChatUser},
		{"a batch of answers on the human's line", "answer", batch, "line", "access-1", ""},
		{"a batch of answers in an absorbed turn", "answer", batch, "absorbed", "chat-token", ""},
		{"request_changes in an absorbed turn without a chat token", "request_changes", changes, "absorbed", "", ""},
		{"a replan in an absorbed turn without a chat token", "replan", replan, "absorbed", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, trn, _ := planGuardRig(t, c.args != batch)
			if c.token == "" {
				r.chatToken = ""
			}
			var replans []string
			r.c.cfg.Replan = func(_ context.Context, _, by, _ string, _ bool) (string, error) {
				replans = append(replans, by)
				return "Plan #20 goes to the planner.", nil
			}
			trn.a.level = policy.AutonomyFullAuto
			if c.turn == "wake" {
				s, err := r.c.wakeSession(trn.p, "sebastian")
				if err != nil {
					t.Fatal(err)
				}
				trn.a.s, trn.a.wake, trn.a.birth = s, true, factoryd.WakeAsk
			}
			humanTurn(t, r, trn, c.turn == "absorbed")
			res, _, _ := trn.call(factoryd.OperatorCall{Name: c.tool, Arguments: c.args})
			posts := slices.DeleteFunc(slices.Clone(r.fg.calls), func(s string) bool { return !strings.HasPrefix(s, comments) })
			if c.token == "" {
				if want := "Refused: The project agent's token could not be read, so nothing was done. Try again."; res != want || len(posts) != 0 || len(replans) != 0 {
					t.Errorf("result %q, comments %v, replans %v; want %q and none", res, posts, replans, want)
				}
				return
			}
			n := 1
			if c.args == batch {
				n = 2
			}
			if want := slices.Repeat([]string{comments + c.token}, n); strings.HasPrefix(res, "Refused") || !slices.Equal(posts, want) {
				t.Errorf("result %q, comments %v; want %v", res, posts, want)
			}
			if c.by != "" && !slices.Equal(replans, []string{c.by}) {
				t.Errorf("replans asked by %v, want %s", replans, c.by)
			}
		})
	}
}

// A revision whose reverts cannot be read counts as taking work off main:
// the agent's confirmation is refused, a human's press passes.
func TestAnUnreadableRevisionIsNotConfirmedByTheAgent(t *testing.T) {
	r, trn, _ := planGuardRig(t, true)
	r.tasks.mu.Lock()
	r.tasks.plans = map[string][]factoryd.PlanRevision{}
	r.tasks.mu.Unlock()
	a := actor{s: trn.a.s, repository: "sf/code", via: factoryd.ViaChat, decider: "agent", level: policy.AutonomyFullAuto}
	_, err := r.c.doAnswerPlanQuestion(t.Context(), a, "t-p", 1, 1, factoryd.DecisionConfirm, "fine")
	refusedAs(t, err, http.StatusConflict, strings.TrimPrefix(refusedOffMain, "Refused: ")+agentStep)
}

// The plan buttons' own actor (button, no decider and no level) passes the
// guard on a revision that takes work off main: the confirmation, an answer
// and Request changes.
func TestAPlanButtonPressPassesTheGuard(t *testing.T) {
	r, trn, _ := planGuardRig(t, true, "api")
	req := httptest.NewRequest(http.MethodPost, "/tasks/t-p/plan/answer", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, trn.a.s))
	a := button(req)
	if _, err := r.c.doAnswerPlanQuestion(t.Context(), a, "t-p", 1, 1, factoryd.DecisionConfirm, "fine"); err != nil {
		t.Errorf("the Confirm button: %v", err)
	}
	if _, err := r.c.doRequestChanges(t.Context(), a, "t-p", "split the API card"); err != nil {
		t.Errorf("the Request changes button: %v", err)
	}
}
