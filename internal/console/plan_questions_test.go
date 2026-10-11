package console

import (
	"context"
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

// Ruling 264: the plan card asks the plan's open questions above its
// accept form, one button per choice and a text box each, the accept
// button disabled while any is open; an answer posts "Answer <n>:
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
	for _, want := range []string{"<legend>1. How often does the checker probe a site?</legend>", "<legend>2. How long are the results kept?</legend>",
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="1">`,
		`<button type="submit" name="answer" value="60 seconds">60 seconds</button>`,
		`<input type="text" id="answer-20-2" name="answer" maxlength="1000" required>`, `<p class="meta">2 questions open</p>`, ` disabled>Accept plan</button>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	if strings.Count(card, `action="/tasks/t-plan/plan/answer"`) != 7 || strings.Count(card, `<button type="submit" name="answer"`) != 5 ||
		strings.Count(card, `<input type="text"`) != 2 || strings.Index(card, "<legend>2.") > strings.Index(card, `action="/tasks/t-plan/plan/accept"`) {
		t.Errorf("want 7 answer forms, 5 choice buttons and 2 text boxes above the accept form:\n%s", card)
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
	if card := board(); !strings.Contains(card, `<p class="answered">How often does the checker probe a site?: 60 seconds (sebastian)</p>`) ||
		!strings.Contains(card, `<p class="meta">1 question open</p>`) || strings.Count(card, `<button type="submit" name="answer"`) != 2 {
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
	// the last answer, typed on two lines, is one; the plan may be accepted
	if rec := answer("2", "2", " 30\n days "); rec.Code != http.StatusSeeOther || r.fg.comments[comments] != "Answer 2: 30 days" {
		t.Fatalf("the second answer: %d, comment %q", rec.Code, r.fg.comments[comments])
	}
	if card := board(); strings.Contains(card, " open</p>") || strings.Contains(card, " disabled>Accept plan") ||
		!strings.Contains(card, `<p class="answered">How long are the results kept?: 30 days (sebastian)</p>`) {
		t.Errorf("the card once answered:\n%s", card)
	}
}

// factoryd's refusal of a revision with open questions is the member's
// sentence: the button answers it with 409, the chat's Do it as its
// refusal.
func TestAcceptPlanOpenQuestions(t *testing.T) {
	r, _, g := newChatRig(t)
	r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-plan", Kind: "agent", Status: "waiting-human",
		Lane: policy.LanePlan, Process: "maintenance", Objective: "Uptime", Repository: "sf/code", IssueNumber: 20}})
	r.c.cfg.AcceptPlan = func(context.Context, string, int, string, string) error {
		return factoryd.ErrOpenQuestions{Issue: 20, Revision: 1, Open: 1}
	}
	refused := "plan #20 revision 1 has 1 unanswered questions; answer them first"
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/tasks/t-plan/plan/accept", url.Values{"csrf": {s.csrf}, "revision": {"1"}, "sha256": {"sha-1"}}), ck); rec.Code != http.StatusConflict ||
		rec.Body.String() != refused+"\n" || len(r.fg.comments) != 0 {
		t.Fatalf("the button: %d %q, comments %v", rec.Code, rec.Body, r.fg.comments)
	}
	g.replies = [][]string{{call(1, "accept_plan", `{"task_id":"t-plan","revision":1,"sha256":"sha-1"}`)}}
	_, _, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"accept the plan"}})
	_, events, _ := chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	equal(t, "Do it", events, []string{
		`tool {"arguments":{"task_id":"t-plan","revision":1,"sha256":"sha-1"},"duration_ms":0,"name":"accept_plan","result":"Refused: ` + refused + `"}`, `done {}`})
}

// The project agent reads plan t-p #20's open questions on its card,
// numbered with their choices, and answers one by its number: the proposal
// names the question, Do it answers it as the member, one row via chat.
// With no question open, an answer to one is refused with the plan's
// actions.
func TestChatAnswersAPlanQuestion(t *testing.T) {
	r, g := newReadsRig(t)
	r.tasks.mu.Lock()
	r.tasks.tasks[slices.IndexFunc(r.tasks.tasks, func(k factoryd.BoardTask) bool { return k.ID == "t-p" })].Status = "waiting-human"
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-p 1": askedQuestions()}
	r.tasks.mu.Unlock()
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 30, Kind: factoryd.ApprovalPlan, TaskID: "t-p", Project: "sf/code",
		Status: "pending", Spec: []byte(`{"revision":1,"sha256":"5e1f"}`)})
	card := reads(t, r, g, "card", `{"task_id":"t-p"}`)[0]
	if want := "\nQuestions:\n1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)\n" +
		"2. How long are the results kept? (choices: 7 days / 30 days)"; !strings.Contains(card, want) {
		t.Fatalf("the card read =\n%s\nwant it to hold\n%s", card, want)
	}
	ck, s := r.loggedIn(t)
	before := r.events.count()
	args := `{"task_id":"t-p","question":1,"text":"60 seconds"}`
	g.replies = [][]string{{call(1, "answer", args)}}
	_, events, tokens := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"probe every minute"}})
	equal(t, "stream", events, []string{`proposal {"arguments":` + args + `,"description":"Answer a card, #20 t-p \"Build the link shortener\", text 60 seconds, ` +
		`question 1. How often does the checker probe a site? (choices: 30 seconds / 60 seconds / 5 minutes)","id":"TOKEN","tool":"answer"}`, `done {}`})
	_, events, _ = chatPost(t, r, ck, s, chatPath+"/proposals/"+tokens[0], nil)
	equal(t, "Do it", events, []string{`tool {"arguments":` + args + `,"duration_ms":0,"name":"answer","result":"Answered question 1 of plan #20."}`,
		`applied {"result":"Answered question 1 of plan #20.","tool":"answer"}`, `done {}`})
	equal(t, "the comment", r.fg.comments["/api/v1/repos/sf/code/issues/20/comments"], "Answer 1: 60 seconds")
	chatRows(t, r.events, before, "sebastian", factoryd.Event{Repository: "sf/code", TaskID: "t-p", IssueNumber: 20,
		Kind: factoryd.EventPlanQuestionAnswered, Detail: map[string]any{"revision": 1, "index": 1, "answer": "60 seconds"}})
	r.tasks.answer("t-p", 1, 2, "30 days", "sebastian")
	status, msg := refused(t, r, "answer", toolInput{TaskID: "t-p", Question: 2, Text: "7 days"})
	equal(t, "an answer with no question open", []any{status, msg}, []any{http.StatusConflict,
		"Card #20 waits on a human: an answer, an approval or a decision, not an open plan question: accept_plan, request_changes or reject_plan decide the plan."})
}
