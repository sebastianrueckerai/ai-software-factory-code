package console

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// caseCard files agent card t-e on sf/code, waiting on a human for the
// agent's ask on #13, with its live cases: 5, at the human, and the merge
// case 9, which its own door answers.
func caseCard(r *rig) {
	r.tasks.set(factoryd.AgentAsks+"Use a replica?", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
		IssueNumber: 13, BudgetEUR: 2})
	r.openCases = map[string][]factoryd.Case{"t-e": {
		{ID: 5, TaskID: "t-e", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman},
		{ID: 9, TaskID: "t-e", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseOpen},
	}}
}

// actedOnce is the rig's one Act call with its Post dropped, so it
// compares, and whether it carried a Post.
func actedOnce(t *testing.T, r *rig) (factoryd.ActInput, bool) {
	t.Helper()
	if len(r.acted) != 1 {
		t.Fatalf("Act calls = %+v, want one", r.acted)
	}
	in := r.acted[0]
	posts := in.Post != nil || in.Work != nil
	in.Post, in.Work = nil, nil
	return in, posts
}

// runTool runs the chat tool name as a turn of sebastian's chat on sf/code,
// decided by decider: the agent unconfirmed, or the human.
func runTool(t *testing.T, r *rig, name, decider string, in toolInput) (string, error) {
	t.Helper()
	_, s := r.loggedIn(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, s))
	tool, ok := r.c.chatTool(name)
	if !ok {
		t.Fatalf("there is no %s tool", name)
	}
	note, _, err := tool.run(&turn{c: r.c, r: req, a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: decider},
		p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}, in)
	return note, err
}

// The Grant button of a card at its session cap answers the card's newest
// live case through Act, the merge case aside: an extend of one session on
// rung 3 by the human, the choice's label its note, which Act has the
// console post as the human's comment.
func TestAnswerGoesThroughActOnTheCardsCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
	r.openCases["t-e"][0].Kind = factoryd.CaseSessionCap
	r.tasks.tasks[0].Question = &factoryd.HumanQuestion{Ask: "Grant a session?",
		Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerGrant, Label: "Grant one session"}}}
	if rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerGrant}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	got, posts := actedOnce(t, r)
	want := factoryd.ActInput{TaskID: "t-e", CaseID: 5, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "Grant one session"}}
	if !reflect.DeepEqual(got, want) || !posts {
		t.Fatalf("Act took %+v (post %v), want %+v with a post", got, posts, want)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "Grant one session" {
		t.Fatalf("comment = %q, want the label as the human's", got)
	}
}

// An answer in words alone is continue, the words its note and the
// comment.
func TestAnswerTextAloneIsContinue(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
	if rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("answer = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	want := factoryd.ActInput{TaskID: "t-e", CaseID: 5, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActContinue, Note: "use the replica"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
	}
	if got := r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"]; got != "use the replica" {
		t.Fatalf("comment = %q", got)
	}
}

// Act's refusal reaches the human whole as a 409, and nothing is posted.
func TestAnswerRefusalNamesTheOfferedActions(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
	const why = "the session cap of 3 is reached: extend or close, and the human may take extend, fund, raise_ceiling, successor, replan and close"
	r.actErr = factoryd.ErrActRefused(why)
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), why) {
		t.Fatalf("answer = %d: %s, want 409 with the refusal", rec.Code, rec.Body)
	}
	if len(r.fg.comments) != 0 {
		t.Fatalf("comments %v: the refused answer was posted", r.fg.comments)
	}
}

// The chat's answer carries its action and the money beside it: a human's
// re-verify with 1 EUR drawn beside it, on rung 3.
func TestChatAnswerCarriesActionAndFund(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", Text: "check it again", Action: factoryd.ActReverify, FundEUR: 1}); err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	want := factoryd.ActInput{TaskID: "t-e", CaseID: 5, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActReverify, Fund: &policy.Draw{EUR: 1}, Note: "check it again"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
	}
}

// The chat's fund is the agent's own decision on rung 2, and a human's on
// rung 3 once they confirmed it.
func TestAgentFundIsRungTwo(t *testing.T) {
	for decider, rung := range map[string]int{"agent": 2, "member": 3} {
		r := newRig(t)
		caseCard(r)
		if _, err := runTool(t, r, "fund", decider, toolInput{TaskID: "t-e", EUR: 1, Text: "the replica is worth it"}); err != nil {
			t.Fatalf("%s: %v", decider, err)
		}
		got, _ := actedOnce(t, r)
		want := factoryd.ActInput{TaskID: "t-e", CaseID: 5, By: "sebastian", Via: factoryd.ViaChat, Rung: rung,
			Res: factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": 1.0}, Note: "the replica is worth it"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: Act took %+v, want %+v", decider, got, want)
		}
	}
}

// set_pool sets escalation.pool.eur in the project file, the pool's other
// parts and every other line kept, and the loaded policy's escalation
// pool says it landed.
func TestSetPoolEditsTheEscalationPool(t *testing.T) {
	r, _ := newWritesRig(t)
	infra := withInfra(t, r)
	const head = "# demo\nrepository: demo-app/demo-app\nmerge_rule: human\nescalation:\n  # what every rung draws from\n  pool:\n    sessions: 10\n"
	infra.files = map[string]string{"factory/projects/demo-app.yaml": head + "    eur: 20\n"}
	r.c.cfg.CheckChange = func(context.Context, map[string][]byte) ([]string, error) { return nil, nil }
	r.c.cfg.PolicyReload = func(context.Context) error {
		r.ledger.files["demo-app"] = policy.ProjectConfig{Repository: "demo-app/demo-app", Escalation: &policy.Escalation{Pool: policy.Draw{Sessions: 10, EUR: 30}}}
		return nil
	}
	_, s := r.loggedIn(t)
	res, err := r.c.doSetPool(context.Background(), "demo-app", poolChange{action: poolSet, eur: 30, reason: "two more follow-ups", source: "blob1"},
		actor{s: s, via: factoryd.ViaChat})
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "the set", res, poolResult{from: 20, confirmed: 30, to: 30, pr: 12, loaded: true})
	if len(infra.contents) != 1 {
		t.Fatalf("%d commits, want one", len(infra.contents))
	}
	if got := committed(t, infra, 0); got != head+"    eur: 30\n" {
		t.Fatalf("the committed file =\n%s", got)
	}
}

// The board's pool line reads what the project's cards drew this month
// against the escalation pool its file sets, in three parts, and says
// overdrawn, naming the part, once a part is past its pool.
func TestBoardShowsThePoolInThreeParts(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.ledger.files = map[string]policy.ProjectConfig{"ai-software-factory-code": {Repository: "sf/code", MergeRule: "human", Objects: "factory",
		Agents: "coder", Process: "maintenance", Escalation: &policy.Escalation{Pool: policy.Draw{Sessions: 10, AgentTime: 5 * time.Hour, EUR: 20}}}}
	r.drawn = policy.Draw{Sessions: 3, AgentTime: 80 * time.Minute, EUR: 12.5}
	board := func() string {
		t.Helper()
		rec := r.do(httptest.NewRequest(http.MethodGet, "/board?project=ai-software-factory-code", nil), ck)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /board = %d: %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	const within = "3 of 10 sessions, 1 h 20 min of 5 h agent time and €12.50 of €20.00 drawn this UTC month"
	if page := board(); !strings.Contains(page, within) || strings.Contains(page, "overdrawn") {
		t.Fatalf("the board lacks %q or says overdrawn:\n%s", within, page)
	}
	r.drawn.Sessions = 11
	const over = "11 of 10 sessions, 1 h 20 min of 5 h agent time and €12.50 of €20.00 drawn this UTC month, overdrawn in sessions: only the human draws more until the month turns"
	if page := board(); !strings.Contains(page, over) {
		t.Fatalf("the board lacks %q:\n%s", over, page)
	}
}

// committed is the content of the one file of infra's commit i.
func committed(t *testing.T, infra *infraFake, i int) string {
	t.Helper()
	change := infra.contents[i].body["files"].([]any)[0].(map[string]any)
	b, err := base64.StdEncoding.DecodeString(change["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Words on a card whose newest case is its session cap grant it one
// session, and at its agent time cap the card draw's hour, as the cap's
// own answer does: continue would find no room. Words on a card whose
// newest case is an ask go on, whatever its session count says.
func TestWordsGrantASessionOnlyAtASessionCap(t *testing.T) {
	for kind, want := range map[factoryd.CaseKind]factoryd.Resolution{
		factoryd.CaseSessionCap:    {Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "use the replica"},
		factoryd.CaseAgentTimeCap:  {Action: factoryd.ActExtend, Args: map[string]any{"agent_time_s": 3600}, Note: "use the replica"},
		factoryd.CaseAgentEscalate: {Action: factoryd.ActContinue, Note: "use the replica"},
	} {
		r := newRig(t)
		ck, s := r.loggedIn(t)
		caseCard(r)
		r.openCases["t-e"][0].Kind = kind
		r.tasks.tasks[0].Usage.Attempts = 3 // the card used its three sessions either way
		if rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: answer = %d: %s", kind, rec.Code, rec.Body)
		}
		if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, want) {
			t.Errorf("%s: Act took %+v, want %+v", kind, got.Res, want)
		}
	}
}

// An answer with an action meets every answer's gates before Act, on
// every entry: the chat's close on a human card runs the close's work, its
// label posted and the issue closed, and resolves the card's case with the
// choice, handing Act the comment the close posted. An action on a running
// agent card meets the wall the button meets, and nothing reaches Act.
func TestAnActionMeetsTheAnswersGates(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerGo, Label: "Have the factory do it"},
		factoryd.QuestionChoice{Value: factoryd.AnswerClose, Label: "Done"})}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", Action: factoryd.ActClose}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"), []string{"Done"}) ||
		!slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Fatalf("comments %q, patched %v; want the label posted and the issue closed", r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"), r.fg.patched)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the close", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-hq", CaseID: 1, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActClose, Note: "Done"}}, true})

	r = newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 11}}}
	var steered []string
	r.c.cfg.Steer = func(_ context.Context, taskID, _, text string) error {
		steered = append(steered, taskID+": "+text)
		return nil
	}
	ck, s := r.loggedIn(t)
	const wall = "Card #11 runs an agent session, not a question or a session cap to answer: steer sends its session a message; its pull request comes when the session ends."
	rec := r.do(post("/tasks/t-run/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerGrant}}), ck)
	_, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-run", Action: factoryd.ActExtend, Text: "one more"})
	var no *refusal
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != wall || !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != wall {
		t.Fatalf("the button = %d %q, the chat's action = %v; want both %q", rec.Code, rec.Body, err, wall)
	}
	if len(r.acted) != 0 || len(steered) != 0 {
		t.Fatalf("Act took %+v, steered %v; want neither", r.acted, steered)
	}
}

// A human card's choice posts its comment by its own work, which Act runs
// once it accepts the answer: the work's comment is the answer's, so the
// webhook's copy of it finds the case answered, and posts nothing more. An
// answer Act refuses because the case ended already runs none of the work:
// the issue stays open and the human reads the refusal.
func TestAHumanChoiceAndItsWebhookCopyInEitherOrder(t *testing.T) {
	const comments = "/api/v1/repos/sf/code/issues/51/comments"
	press := func(r *rig) *httptest.ResponseRecorder {
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerClose, Label: "Done"})}
		ck, s := r.loggedIn(t)
		return r.do(post("/tasks/t-hq/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerClose}}), ck)
	}

	r := newRig(t)
	if rec := press(r); rec.Code != http.StatusSeeOther {
		t.Fatalf("the close = %d: %s", rec.Code, rec.Body)
	}
	if len(r.fg.commentBodies(comments)) != 1 || len(r.fg.patched) != 1 || r.acted[0].Work == nil {
		t.Fatalf("comments %q, patched %v; want the close's comment posted once and the issue closed by Act's Work", r.fg.commentBodies(comments), r.fg.patched)
	}

	r = newRig(t)
	at := time.Date(2026, 11, 3, 10, 4, 0, 0, time.UTC)
	r.actErr = factoryd.ErrCaseEnded{Case: factoryd.Case{ID: 1, Status: factoryd.CaseResolved, ResolvedAt: &at,
		Resolution: &factoryd.CaseResolution{By: "sebastian", Action: "answer"}}}
	if rec := press(r); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "has no live case, so a new answer goes on the issue or on a new card") ||
		len(r.fg.patched) != 0 || len(r.fg.commentBodies(comments)) != 0 {
		t.Fatalf("the close the webhook answered first = %d %q, patched %v; want the refusal and no work", rec.Code, rec.Body, r.fg.patched)
	}
}

// Act's failure reaches the human as what it means: a retry factoryd
// refuses is a 409 with its sentence, and any other failure a 500 that
// names the error and the next step, not the factoryd log.
func TestActFailuresNameTheErrorAndTheNextStep(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		body   string
	}{
		{factoryd.ErrNoRetry("Card #7 was already retried."), http.StatusConflict, "Card #7 was already retried."},
		{errors.New(`pq: relation "cases" does not exist`), http.StatusInternalServerError,
			"The answer failed: pq: relation \"cases\" does not exist. Reload the board and press the button again."},
	} {
		r := newRig(t)
		ck, s := r.loggedIn(t)
		caseCard(r)
		r.actErr = tc.err
		rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck)
		if rec.Code != tc.status || strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Errorf("%v = %d %q, want %d %q", tc.err, rec.Code, rec.Body, tc.status, tc.body)
		}
	}
}

// retryRig is the plan board rig with failed card t-c7 (#7) and two cards
// that wait on it, t-d1 (#21) and t-d2 (#22), each at the human on its
// dependency case about t-c7: 5 and 6.
func retryRig(t *testing.T) *rig {
	r := newRig(t)
	withLinkshort(r)
	r.tasks.tasks = append(planTasks(), failedCard())
	r.tasks.deps = map[string][]string{}
	for _, d := range []struct {
		id    string
		issue int64
	}{{"t-d1", 21}, {"t-d2", 22}} {
		r.tasks.tasks = append(r.tasks.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: d.id, Kind: "agent", Status: "waiting-human",
			Repository: "linkshort/linkshort", IssueNumber: d.issue}})
		r.tasks.deps[d.id] = []string{"t-c7"}
	}
	r.openCases = map[string][]factoryd.Case{
		"t-d1": {{ID: 5, TaskID: "t-d1", Kind: factoryd.CaseDependencyFailed, StateKey: "t-c7", Status: factoryd.CaseAtHuman}},
		"t-d2": {{ID: 6, TaskID: "t-d2", Kind: factoryd.CaseDependencyFailed, StateKey: "t-c7", Status: factoryd.CaseAtHuman}},
	}
	return r
}

// One Retry of a failed card answers every dependency case on it through
// Act and files nothing itself. card_retried is written once, by the
// answer whose retry filed the replacement (its step names #9), even when
// a later dependent's answer stops or the filing answer itself fails after
// the filing; a press whose answers only named the replacement a retry
// filed before writes none.
func TestRetryAnswersEveryDependentsCase(t *testing.T) {
	retry := factoryd.Resolution{Action: factoryd.ActRetry, Args: map[string]any{"task_id": "t-c7"}}
	filed := []factoryd.CaseAction{{Action: factoryd.ActRetry, Args: map[string]any{"task_id": "t-c7", "replacement": int64(9)}, Result: "Filed #9 in place of #7."}}
	named := []factoryd.CaseAction{{Action: factoryd.ActRetry, Args: map[string]any{"task_id": "t-c7"}, Result: "#7 was retried as #9 already."}}
	answered := func(id string, caseID int64) wantRow {
		return wantRow{kind: factoryd.EventQuestionAnswered, repo: "linkshort/linkshort", taskID: id,
			detail: map[string]any{"action": factoryd.ActRetry, "case_id": caseID, "answer": factoryd.ActRetry}}
	}
	retried := wantRow{kind: factoryd.EventCardRetried, repo: "linkshort/linkshort", taskID: "t-c7", detail: map[string]any{"replacement": int64(9)}}
	press := func(r *rig) (string, error) {
		_, s := r.loggedIn(t)
		return r.c.doRetry(t.Context(), actor{s: s, repository: "linkshort/linkshort", via: factoryd.ViaButton}, "t-c7")
	}

	r := retryRig(t)
	r.actDid = func(in factoryd.ActInput) []factoryd.CaseAction {
		if in.TaskID == "t-d1" {
			return filed
		}
		return named
	}
	before := r.events.count()
	said, err := press(r)
	if err != nil {
		t.Fatal(err)
	}
	want := []factoryd.ActInput{
		{TaskID: "t-d1", CaseID: 5, By: "sebastian", Via: factoryd.ViaButton, Rung: 3, Res: retry},
		{TaskID: "t-d2", CaseID: 6, By: "sebastian", Via: factoryd.ViaButton, Rung: 3, Res: retry},
	}
	var got []factoryd.ActInput
	for _, in := range r.acted {
		in.Post, in.Detail = nil, nil
		got = append(got, in)
	}
	if !reflect.DeepEqual(got, want) || len(r.retried) != 0 || said != "Case 5 took retry. Case 6 took retry." {
		t.Fatalf("Act took %+v, RetryCard %v, said %q; want both cases answered and no direct retry", got, r.retried, said)
	}
	checkRows(t, r.events, before, answered("t-d1", 5), retried, answered("t-d2", 6))

	r = retryRig(t)
	r.actDid = func(in factoryd.ActInput) []factoryd.CaseAction {
		if in.TaskID == "t-d1" {
			return filed
		}
		return nil
	}
	r.actErrs = map[int]error{2: factoryd.ErrActRefused("the card does not depend on t-c7")}
	before = r.events.count()
	if _, err := press(r); err == nil || err.Error() != "Case 5 took retry. The next answer stopped: the card does not depend on t-c7" {
		t.Fatalf("the stopped press = %v", err)
	}
	checkRows(t, r.events, before, answered("t-d1", 5), retried)

	r = retryRig(t)
	r.actDid = func(factoryd.ActInput) []factoryd.CaseAction { return filed }
	r.actErrs = map[int]error{1: errors.New("the store is down")}
	before = r.events.count()
	if _, err := press(r); err == nil {
		t.Fatal("the press whose filing answer failed went through")
	}
	checkRows(t, r.events, before, retried)

	r = retryRig(t)
	r.actDid = func(factoryd.ActInput) []factoryd.CaseAction { return named }
	before = r.events.count()
	if said, err := press(r); err != nil || said != "Case 5 took retry. Case 6 took retry." {
		t.Fatalf("the naming press = %q, %v", said, err)
	}
	checkRows(t, r.events, before, answered("t-d1", 5), answered("t-d2", 6))
}

// A human card's own Close ends its case through Act as the human's close
// once its work went through: the note posted, the issue closed, and the
// note's comment handed to Act.
func TestCloseOfAHumanCardResolvesItsCase(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerPark, Label: "Wait for it"})}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/tasks/t-hq/close", url.Values{"csrf": {s.csrf}, "note": {"done by hand"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body)
	}
	if !slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Fatalf("patched %v; want the issue closed", r.fg.patched)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the close", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-hq", CaseID: 1, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActClose, Note: "done by hand"}}, true})
	if len(r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments")) != 1 {
		t.Fatalf("comments %q; want the note's comment posted once by Act's Work", r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"))
	}
}

// A ceiling refusal names the control the human holds where they are: the
// Fund form's ceiling field on the board, the answer's raise_ceiling in
// the chat.
func TestCeilingRefusalNamesTheActorsControl(t *testing.T) {
	for via, want := range map[string]string{
		factoryd.ViaButton: "Card #13 holds €4.50 now, and €0.90 more passes its ceiling of €5.00: raise the ceiling in the Fund form's ceiling field as you fund.",
		factoryd.ViaChat:   "Card #13 holds €4.50 now, and €0.90 more passes its ceiling of €5.00: raise the ceiling first (answer with action raise_ceiling), then fund.",
	} {
		r := needRig(t)
		_, s := r.loggedIn(t)
		r.actErr = &factoryd.CardCapError{Budget: 4.5}
		_, err := r.c.doFund(t.Context(), actor{s: s, repository: "sf/code", via: via}, "t-e", 0.9, "go", 0)
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != want {
			t.Errorf("%s: doFund = %v, want %q", via, err, want)
		}
	}
}

// The board's Fund form raises the ceiling as it funds: its ceiling field
// is a raise_ceiling with the fund drawn beside it.
func TestFundFormRaisesTheCeiling(t *testing.T) {
	r := needRig(t)
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/tasks/t-e/fund", url.Values{"csrf": {s.csrf}, "eur": {"0.9"}, "ceiling": {"1"}, "reason": {"worth it"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("fund = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	if want := (factoryd.Resolution{Action: factoryd.ActRaiseCeiling, Args: map[string]any{"eur": 1.0}, Fund: &policy.Draw{EUR: 0.9}, Note: "worth it"}); !reflect.DeepEqual(got.Res, want) {
		t.Fatalf("Act took %+v, want %+v", got.Res, want)
	}
}

// The chat's answer passes its args to Act whole: a handover to a
// stronger model names its model.
func TestChatAnswerPassesItsArgs(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", Text: "try the bigger one", Action: factoryd.ActHandover,
		Args: json.RawMessage(`{"model":"stronger"}`)}); err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	if want := (factoryd.Resolution{Action: factoryd.ActHandover, Args: map[string]any{"model": "stronger"}, Note: "try the bigger one"}); !reflect.DeepEqual(got.Res, want) {
		t.Fatalf("Act took %+v, want %+v", got.Res, want)
	}
}

// The console's answer to every choice the five-part questions offer fits
// the action table on the facts of its case. For each case kind with a
// question, each choice value that question offers reaches Act on rung 3
// as an action the real table gives the human on hand-built facts of the
// kind: factoryd.Offered holds it, and its own rule takes the answer's
// arguments. A note and a continue carry the note their rule reads, which
// Offered's probes do not, so their rule alone judges them. A human card's
// choice is its question's to take, and its note (park) is a note on rung
// 3 that the table takes. The pairs the table refuses stay away from Act:
// words at a session cap would be a continue with no room, which the
// console turns into a grant, and a grant on an ask is an extend the
// table does not offer there.
func TestEveryChoiceFitsTheActionTable(t *testing.T) {
	room := factoryd.CaseFacts{Attempts: 1, SessionCap: 3, AgentTimeS: 600, AgentTimeCapS: 7200, BudgetEUR: 2, LeftEUR: 1.5, FloorEUR: 0.5, CeilingEUR: 5}
	with := func(edit func(*factoryd.CaseFacts)) factoryd.CaseFacts {
		f := room
		edit(&f)
		return f
	}
	capped := with(func(f *factoryd.CaseFacts) { f.Attempts = 3 })
	pr := with(func(f *factoryd.CaseFacts) { f.PR, f.PROpen, f.Head, f.Checks = 9, true, "h1", "red" })
	checksWait := with(func(f *factoryd.CaseFacts) { *f = pr; f.Gate = factoryd.ActivityWaitChecks })
	fits := func(f factoryd.CaseFacts, res factoryd.Resolution) string {
		a, ok := factoryd.ActionByName(res.Action)
		switch {
		case !ok:
			return "no action " + res.Action
		case !a.Rungs[3]:
			return "not the human's"
		case res.Action != factoryd.ActNote && res.Action != factoryd.ActContinue &&
			!slices.ContainsFunc(factoryd.Offered(f, 3), func(o factoryd.Action) bool { return o.Name == res.Action }):
			return "not offered"
		}
		return a.Applies(f, res)
	}
	for _, tc := range []struct {
		kind    factoryd.CaseKind
		reason  string
		choices []string
		facts   factoryd.CaseFacts
	}{
		{factoryd.CaseSessionCap, "the session cap is reached: every finding sent to the agent so far goes to a human", nil, capped},
		{factoryd.CaseAgentTimeCap, "the agent time is used up: every finding sent to the agent so far goes to a human", nil,
			with(func(f *factoryd.CaseFacts) { f.AgentTimeS = 7200 })},
		{factoryd.CaseBudgetFloor, needRefusal, nil, with(func(f *factoryd.CaseFacts) { f.LeftEUR, f.FloorEUR = 0.17, 0.9 })},
		{factoryd.CaseMonthCap, "the month cap is reached", nil, room},
		{factoryd.CaseCheckinShare, "check-in: " + factoryd.CheckSpend + ": the check-in's share of the budget is spent", nil, room},
		{factoryd.CaseCheckinShare, "check-in: " + factoryd.CheckSpend + ": check-in call cap reached (3 calls)", nil, room},
		{factoryd.CaseGateStop, "The gate supervisor stopped WaitChecks (attempt 2) past its bound of 30 min: checks still red after 3 runs.", nil, checksWait},
		{factoryd.CaseGateStop, "The gate supervisor stopped review: review: concerns on the head", nil, pr},
		{factoryd.CaseFindingsRepeated, "No reply can change this: the head and the card's limits are unchanged since the last stop.", nil, pr},
		{factoryd.CaseAgentEscalate, factoryd.AgentAsks + "Use a replica?", []string{"Yes", "No"}, room},
		{factoryd.CaseOther, "the pod never started", nil, room},
		{factoryd.CaseWaitExpired, "The card was quiet for 3 days.", nil, room},
	} {
		q := factoryd.ShapedQuestion(tc.reason, factoryd.QuestionFacts{}, tc.choices)
		tc.facts.Kind = tc.kind
		for _, c := range q.Choices {
			r := newRig(t)
			_, s := r.loggedIn(t)
			r.tasks.set(tc.reason, factoryd.Task{ID: "t-k", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, BudgetEUR: 2})
			r.tasks.tasks[0].Question = q
			facts := map[string]any{"reason": tc.reason}
			if strings.Contains(tc.reason, factoryd.CheckinShareSpent) {
				facts["checkin_stop"] = factoryd.CheckinStopShare // as the trip names the share stop
			}
			r.openCases = map[string][]factoryd.Case{"t-k": {{ID: 1, TaskID: "t-k", Kind: tc.kind, Status: factoryd.CaseAtHuman, Facts: facts}}}
			a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat}
			var err error
			if c.Value == factoryd.AnswerExtend {
				_, err = r.c.doExtend(t.Context(), a, "t-k", 1, 0, 0, 0, c.Label) // the Extend form gives its amounts
			} else {
				_, err = r.c.doAnswerValue(t.Context(), a, "t-k", "", c.Value, "")
			}
			if err != nil || len(r.acted) != 1 {
				t.Errorf("%s, %s: %v, Act took %+v", tc.kind, c.Value, err, r.acted)
				continue
			}
			in := r.acted[0]
			if why := fits(tc.facts, in.Res); in.Rung != 3 || why != "" {
				t.Errorf("%s, %s: Act took %+v on rung %d, which the table refuses: %s", tc.kind, c.Value, in.Res, in.Rung, why)
			}
		}
	}

	for _, card := range []factoryd.BoardTask{stallCard(), humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerGo, Label: "Have the factory do it"},
		factoryd.QuestionChoice{Value: factoryd.AnswerPark, Label: "Wait for it"}, factoryd.QuestionChoice{Value: factoryd.AnswerClose, Label: "Done"})} {
		for _, c := range card.Question.Choices {
			r := newRig(t)
			_, s := r.loggedIn(t)
			r.tasks.tasks = []factoryd.BoardTask{card}
			if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, card.ID, "", c.Value, ""); err != nil || len(r.acted) != 1 {
				t.Errorf("%s, %s: %v, Act took %+v", card.ID, c.Value, err, r.acted)
				continue
			}
			res := r.acted[0].Res
			switch {
			case res.Action == factoryd.ActNote && fits(room, res) != "":
				t.Errorf("%s, %s: the note %+v is no note the table takes: %s", card.ID, c.Value, res, fits(room, res))
			case res.Action != factoryd.ActNote && !card.Question.Offers(res.Action):
				t.Errorf("%s, %s: the choice %+v is not its question's", card.ID, c.Value, res)
			}
		}
	}

	if got, want := fits(capped, factoryd.Resolution{Action: factoryd.ActContinue, Note: "go on"}), "the session cap of 3 is reached: extend or close"; got != want {
		t.Errorf("a continue at the cap = %q, want the table's refusal %q", got, want)
	}
	ask := room
	ask.Kind = factoryd.CaseAgentEscalate
	if slices.ContainsFunc(factoryd.Offered(ask, 3), func(o factoryd.Action) bool { return o.Name == factoryd.ActExtend }) {
		t.Errorf("the human on an ask is offered an extend, want none: a grant there answers no question")
	}
}

// The Grant value buys what the case's stop asks for: one session at a
// session cap, the card draw's agent time at an agent time cap (an hour,
// one and a half for a large card), one more check-in share at the
// check-in's share stop, which the case's checkin_stop fact names even on
// a repeat whose ask names no share, one session at the check-in's other
// spend stops and where the case names none, and on a stop that asked a
// question it goes on with its label.
func TestGrantBuysWhatTheStopAsksFor(t *testing.T) {
	const repeat = "check-in: spend_above_policy: The restart produced no new commit, the head is still c1. What should the agent do?"
	for _, tc := range []struct {
		kind factoryd.CaseKind
		stop string
		size string
		want factoryd.Resolution
	}{
		{factoryd.CaseSessionCap, "", "", factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "Go on"}},
		{factoryd.CaseAgentTimeCap, "", "", factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"agent_time_s": 3600}, Note: "Go on"}},
		{factoryd.CaseAgentTimeCap, "", "large", factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"agent_time_s": 5400}, Note: "Go on"}},
		{factoryd.CaseCheckinShare, factoryd.CheckinStopShare, "", factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"checkin_shares": 1}, Note: "Go on"}},
		{factoryd.CaseCheckinShare, factoryd.CheckinStopCallCap, "", factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "Go on"}},
		{factoryd.CaseCheckinShare, "", "", factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "Go on"}},
		{factoryd.CaseOther, "", "", factoryd.Resolution{Action: factoryd.ActContinue, Note: "Go on"}},
	} {
		r := newRig(t)
		_, s := r.loggedIn(t)
		r.tasks.set("a stop", factoryd.Task{ID: "t-k", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Size: tc.size})
		r.tasks.tasks[0].Question = &factoryd.HumanQuestion{Ask: "May it go on?", Choices: []factoryd.QuestionChoice{{Value: factoryd.AnswerGrant, Label: "Go on"}}}
		facts := map[string]any{"reason": repeat}
		if tc.stop != "" {
			facts["checkin_stop"] = tc.stop
		}
		r.openCases = map[string][]factoryd.Case{"t-k": {{ID: 1, TaskID: "t-k", Kind: tc.kind, Status: factoryd.CaseAtHuman, Facts: facts}}}
		if _, err := r.c.doAnswerValue(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, "t-k", "", factoryd.AnswerGrant, ""); err != nil {
			t.Fatalf("%s %s: %v", tc.kind, tc.size, err)
		}
		if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, tc.want) {
			t.Errorf("%s %q %s: Act took %+v, want %+v", tc.kind, tc.stop, tc.size, got.Res, tc.want)
		}
	}
}

// grant_session reaches Act on any card: on an ask its extend of one
// session meets the table's refusal, which the console does not judge
// before it.
func TestGrantSessionLeavesTheAskToAct(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	if _, err := runTool(t, r, "grant_session", "member", toolInput{TaskID: "t-e", Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1}, Note: "yes"}) {
		t.Fatalf("Act took %+v, want an extend of one session", got.Res)
	}
}

// The chat's args reach Act with the keys its schema names alone: a
// "comment" or a key of no action is dropped.
func TestChatArgsKeepTheSchemasKeys(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", Text: "try the bigger one", Action: factoryd.ActHandover,
		Args: json.RawMessage(`{"model":"stronger","comment":5,"evil":1}`)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res.Args, map[string]any{"model": "stronger"}) {
		t.Fatalf("Act took args %+v, want the model alone", got.Res.Args)
	}
}

// A human card's typed answer is one of its choices: args or fund_eur on
// it are refused with 400, and nothing reaches Act or Forgejo.
func TestHumanCardsTypedAnswerTakesNoArgs(t *testing.T) {
	for _, in := range []toolInput{
		{TaskID: "t-hq", Action: factoryd.ActClose, Args: json.RawMessage(`{"sessions":1}`)},
		{TaskID: "t-hq", Action: factoryd.ActClose, FundEUR: 1},
	} {
		r := newRig(t)
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerClose, Label: "Done"})}
		_, err := runTool(t, r, "answer", "member", in)
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusBadRequest ||
			no.msg != "Card #51 is a human card: its answer is one of its choices, which draws nothing and takes no args." || len(r.acted) != 0 ||
			len(r.fg.comments) != 0 || len(r.fg.patched) != 0 {
			t.Errorf("%+v = %v, Act took %+v, comments %v, patched %v; want the 400 alone", in, err, r.acted, r.fg.comments, r.fg.patched)
		}
	}
}

// A fund on a card that ended reaches Act, whose fund rule refuses it
// naming the end: the console carries that refusal whole as a 409.
func TestFundOnAnEndedCardMeetsTheTable(t *testing.T) {
	r := needRig(t)
	r.tasks.tasks[0].Status = "done"
	res := factoryd.Resolution{Action: factoryd.ActFund, Args: map[string]any{"eur": 0.9}, Note: "go"}
	fund, _ := factoryd.ActionByName(factoryd.ActFund)
	why := fund.Applies(factoryd.CaseFacts{Final: "done"}, res)
	if why != "this card ended done: a fund buys nothing, file a new card" {
		t.Fatalf("the table's fund rule on a done card = %q", why)
	}
	r.actErr = factoryd.ErrActRefused(why)
	_, s := r.loggedIn(t)
	_, err := r.c.doFund(t.Context(), actor{s: s, repository: "sf/code", via: factoryd.ViaButton}, "t-e", 0.9, "go", 0)
	var no *refusal
	if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != why {
		t.Fatalf("doFund = %v, want 409 %q", err, why)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, res) || got.CaseID != 0 {
		t.Fatalf("Act took %+v, want the fund on no case", got)
	}
}

// A grant on a card that is at no cap is refused before Act: a done card
// with no case has nothing to grant, and the Extend form raises a running
// card.
func TestGrantSessionNeedsACap(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-d", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 13})
	_, err := runTool(t, r, "grant_session", "member", toolInput{TaskID: "t-d", Text: "one more"})
	var no *refusal
	if want := "this card is not at a cap, so there is nothing to grant: the Extend form raises a running card"; !errors.As(err, &no) ||
		no.status != http.StatusConflict || no.msg != want || len(r.acted) != 0 {
		t.Fatalf("grant_session on a done card = %v, Act took %+v; want 409 %q", err, r.acted, want)
	}
}

// The answer's args take one more check-in share at most, as the table
// does.
func TestAnswerArgsTakeOneCheckinShare(t *testing.T) {
	r := newRig(t)
	tool, _ := r.c.chatTool("answer")
	shares := tool.params["args"].(map[string]any)["properties"].(map[string]any)["checkin_shares"].(map[string]any)
	if shares["maximum"] != 1 || shares["minimum"] != 0 {
		t.Fatalf("checkin_shares = %v, want minimum 0 and maximum 1", shares)
	}
}

// At an agent time cap a policy the console cannot read still grants the
// default card draw's agent time, one and a half hours for a large card,
// never a session the time cap does not take.
func TestCardTimeFallsBackToTheDefaultDraw(t *testing.T) {
	r := newRig(t)
	r.ledger.down = true
	if got, want := r.c.cardTime(t.Context(), factoryd.Task{Size: "large"}), 90*time.Minute; got != want {
		t.Fatalf("cardTime = %v, want %v", got, want)
	}
}

// A card at its agent time cap leads with the extend, whose form asks the
// minutes of agent time first, and a wrong press names what fits.
func TestTimeCappedCardNamesItsGrant(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("the agent time is used up: every finding sent to the agent so far goes to a human", factoryd.Task{ID: "t-tc", Kind: "agent",
		Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "Trim the logs."})
	seatAsk(r, "t-tc", factoryd.CaseAgentTimeCap, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.AgentTimeS = 1, 7200 }))
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code&view=wide", nil), ck).Body.String()
	esc := escalationOf(t, body, "t-tc")
	minutes, sessions, button := strings.Index(esc, `name="extend.minutes"`), strings.Index(esc, `name="extend.sessions"`), strings.Index(esc, `name="action" value="extend"`)
	if minutes < 0 || sessions < minutes || button < sessions {
		t.Fatalf("the time-capped card's answer form =\n%s\nwant the extend first, its minutes before its sessions", esc)
	}
	if got, want := fitting(factoryd.Task{Status: "waiting-human", IssueNumber: 13}, capFacts{capped: true, timeCap: true}),
		"an answer with the findings grants more agent time (answer). Close ends it"; got != want {
		t.Fatalf("fitting = %q, want %q", got, want)
	}
}

// grant_session says what it grants, in its sentence and in the title its
// proposal leads with: what the cap asks for.
func TestGrantSessionSaysWhatItGrants(t *testing.T) {
	r := newRig(t)
	tool, _ := r.c.chatTool("grant_session")
	if want := "Grant a card at a cap what the cap asks for, with an answer to its findings."; tool.about != want {
		t.Fatalf("grant_session's about = %q, want %q", tool.about, want)
	}
	if want := "Grant what the stop asks for"; tool.title != want {
		t.Fatalf("grant_session's title = %q, want %q", tool.title, want)
	}
}

// The chat's answer tool names the actions rung 2 or the human may take:
// withdraw, the factory's own, is none of them.
func TestChatAnswerOffersNoWithdraw(t *testing.T) {
	i := slices.IndexFunc(tools, func(t chatTool) bool { return t.name == "answer" })
	action, _ := tools[i].params["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if slices.Contains(enum, factoryd.ActWithdraw) || !slices.Contains(enum, factoryd.ActExtend) || !slices.Contains(enum, factoryd.ActRouteApproval) {
		t.Fatalf("the answer tool's actions = %q, want rung 2's and the human's, no withdraw", enum)
	}
}

// A typed answer with no words posts its resolution's sentence as the
// human's own comment, the comment a run from before the cases gate wakes
// on, where factoryd's record of it wakes nothing: the chat's extend of one
// session with no text posts "extend: 1 session from the pool" as
// sebastian.
func TestATypedAnswerWithNoWordsPostsItsSentence(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	r.openCases["t-e"][0].Kind = factoryd.CaseSessionCap
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", Action: factoryd.ActExtend, Args: json.RawMessage(`{"sessions":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, posts := actedOnce(t, r); !posts {
		t.Fatal("Act took no Post: factoryd would post the record, which no run from before the gate wakes on")
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if !slices.Contains(r.fg.calls, "POST /api/v1/repos/sf/code/issues/13/comments Bearer access-1") ||
		r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"] != "extend: 1 session from the pool" {
		t.Fatalf("Forgejo calls %q, comments %q; want the sentence posted on #13 as sebastian", r.fg.calls, r.fg.comments)
	}
}

// A monitor case is answered by its number. The chat's answer with case_id
// on running card t-run reaches Act on case 7, past the gates of the run's
// own ask, with no comment of the human's for the run to read, and the run
// is not steered, a close with no words among them. A number that names no
// live case of the card is refused before Act, saying why and naming the
// card's live cases with their kinds: a case unknown or another project's,
// a case of the card that ended, a case of another card, and on a card
// with none, that it has none. Without case_id, waiting card t-e's answer
// finds its own ask, case 5, the newer monitor case 12 beside it aside.
// Case 5 named meets the answer's gates and posts the human's comment,
// and a merge's or a plan question's case named is refused with its own
// door.
func TestTheChatAnswersAMonitorCaseByItsNumber(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 11}},
		{Task: factoryd.Task{ID: "t-other", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14}},
		{Task: factoryd.Task{ID: "t-idle", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 15}}}
	r.openCases = map[string][]factoryd.Case{"t-run": {{ID: 7, TaskID: "t-run", Repository: "sf/code", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman}},
		"t-other": {{ID: 5, TaskID: "t-other", Repository: "sf/code", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman}},
		"t-far":   {{ID: 3, TaskID: "t-far", Repository: "sf/else", Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman}}}
	r.endedCases = []factoryd.Case{{ID: 6, TaskID: "t-run", Repository: "sf/code", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseWithdrawn}}
	var steered []string
	r.c.cfg.Steer = func(_ context.Context, taskID, _, text string) error {
		steered = append(steered, taskID+": "+text)
		return nil
	}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-run", CaseID: 7, Action: factoryd.ActNote, Text: "seen it"}); err != nil {
		t.Fatal(err)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the monitor case's answer", []any{got, posts, steered}, []any{factoryd.ActInput{TaskID: "t-run", CaseID: 7, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActNote, Note: "seen it"}}, false, []string(nil)})

	refused := func(task string, id int64, want string) {
		t.Helper()
		calls := len(r.acted)
		_, err := runTool(t, r, "answer", "member", toolInput{TaskID: task, CaseID: id, Action: factoryd.ActNote, Text: "seen it"})
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != want || len(r.acted) != calls {
			t.Fatalf("an answer on case %d = %v, Act calls %d; want %q and no call", id, err, len(r.acted)-calls, want)
		}
	}
	const live = " Card #11's live cases: 7 (monitor_flag)."
	refused("t-run", 8, "Card #11 has no case by that number."+live)
	refused("t-run", 3, "Card #11 has no case by that number."+live)
	refused("t-run", 6, "That case ended."+live)
	refused("t-run", 5, "That case is #14's."+live)
	refused("t-idle", 7, "That case is #11's. Card #15 has no live case.")
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-run", CaseID: 7, Action: factoryd.ActClose}); err != nil {
		t.Fatal(err)
	}
	if len(r.acted) != 2 || r.acted[1].CaseID != 7 || !reflect.DeepEqual(r.acted[1].Res, factoryd.Resolution{Action: factoryd.ActClose}) || r.acted[1].Post != nil {
		t.Fatalf("Act calls = %+v, want the close of case 7 with no words and no post", r.acted)
	}

	r = newRig(t)
	caseCard(r)
	r.openCases["t-e"] = append(r.openCases["t-e"],
		factoryd.Case{ID: 10, TaskID: "t-e", Kind: factoryd.CasePlanQuestion, Status: factoryd.CaseAtHuman, Facts: map[string]any{"revision": float64(2), "index": float64(1)}},
		factoryd.Case{ID: 12, TaskID: "t-e", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman})
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", Action: factoryd.ActNote, Text: "later"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := actedOnce(t, r); got.CaseID != 5 {
		t.Fatalf("Act took case %d, want the card's own ask, case 5", got.CaseID)
	}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", CaseID: 5, Action: factoryd.ActNote, Text: "later"}); err != nil {
		t.Fatal(err)
	}
	if len(r.acted) != 2 || r.acted[1].CaseID != 5 || r.acted[1].Post == nil {
		t.Fatalf("Act calls = %+v, want case 5 answered with the human's comment", r.acted)
	}
	// a merge's case is answered by its id as any case is: Act takes the
	// decision, whose work finds no request behind the rig's case
	calls := len(r.acted)
	_, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", CaseID: 9, Action: factoryd.ActApproveMerge})
	var no *refusal
	if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != "approval request 0 is not pending (any more); reload the board" ||
		len(r.acted) != calls+1 || r.acted[calls].CaseID != 9 || r.acted[calls].Res.Action != factoryd.ActApproveMerge {
		t.Fatalf("approve_merge on case 9 = %v, Act calls %+v; want one call on case 9 whose work finds no request", err, r.acted[calls:])
	}
	refused("t-e", 10, "Case 10 is #13's plan question: answer revision 2, question 1 without a case number.")
}

// A card with a live case closes through Act, which removes its dependencies
// itself (factoryd's close): the console sends the close as typed and removes
// nothing first (its only write is the note's comment). A refusal of Act's close, whose error carries
// URL userinfo and a bearer value, reaches the human with both masked.
func TestCloseThroughActLeavesTheDependenciesToAct(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	r.fg.deps = map[string][]map[string]any{"/api/v1/repos/sf/code/issues/13/dependencies": dependsOn(7)}
	ck, s := r.loggedIn(t)
	if rec := r.do(post("/tasks/t-e/close", url.Values{"csrf": {s.csrf}, "note": {"Done."}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the close", []any{got.Res, writesOf(r.fg)}, []any{factoryd.Resolution{Action: factoryd.ActClose, Note: "Done."}, []string{"POST /api/v1/repos/sf/code/issues/13/comments"}})

	r = newRig(t)
	caseCard(r)
	r.actErr = errors.New("forgejo PATCH https://user:pass@git.example/x: edge refused, Authorization: Bearer abcdef0123456789")
	ck, s = r.loggedIn(t)
	rec := r.do(post("/tasks/t-e/close", url.Values{"csrf": {s.csrf}}), ck)
	equal(t, "the status", rec.Code, http.StatusInternalServerError)
	for _, leak := range []string{"user:pass", "abcdef0123456789"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the refusal %q leaks %q", rec.Body, leak)
		}
	}
	if !strings.Contains(rec.Body.String(), "edge refused") {
		t.Errorf("the refusal %q lost Forgejo's reason", rec.Body)
	}
}
