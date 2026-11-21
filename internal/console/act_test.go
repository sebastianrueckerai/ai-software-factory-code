package console

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// reportCard files report card t-r on sf/code, which has no issue,
// waiting on a human at its gate stop, case 7.
func reportCard(r *rig) {
	r.tasks.set("The gate supervisor stopped RunReport (attempt 1) past its bound of 10m0s: the session made no call",
		factoryd.Task{ID: "t-r", Kind: "agent", Status: "waiting-human", Repository: "sf/code", Source: factoryd.SourceSizing, Parent: "t-p", BudgetEUR: 2})
	r.openCases = map[string][]factoryd.Case{"t-r": {{ID: 7, TaskID: "t-r", Kind: factoryd.CaseGateStop, Status: factoryd.CaseAtHuman}}}
}

// An answer in words alone is continue, the words its note and the
// comment. A card with no issue, a report card, has no issue to post on:
// Act takes the answer with no Post, and the case resolves with the note
// alone, which the card's run takes as the answer.
func TestAnswerTextAloneIsContinue(t *testing.T) {
	for _, tc := range []struct {
		card     func(*rig)
		id       string
		caseID   int64
		posts    bool
		comments map[string]string
	}{
		{card: caseCard, id: "t-e", caseID: 5, posts: true, comments: map[string]string{"/api/v1/repos/sf/code/issues/13/comments": "use the replica"}},
		{card: reportCard, id: "t-r", caseID: 7, comments: map[string]string{}},
	} {
		r := newRig(t)
		ck, s := r.loggedIn(t)
		tc.card(r)
		before := r.events.count()
		if rec := r.do(post("/tasks/"+tc.id+"/answer", url.Values{"csrf": {s.csrf}, "answer": {"use the replica"}}), ck); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: answer = %d: %s", tc.id, rec.Code, rec.Body)
		}
		got, posts := actedOnce(t, r)
		want := factoryd.ActInput{TaskID: tc.id, CaseID: tc.caseID, By: "sebastian", Via: factoryd.ViaButton, Rung: 3,
			Res: factoryd.Resolution{Action: factoryd.ActContinue, Note: "use the replica"}}
		if !reflect.DeepEqual(got, want) || posts != tc.posts {
			t.Fatalf("%s: Act took %+v (post %v), want %+v (post %v)", tc.id, got, posts, want, tc.posts)
		}
		equal(t, tc.id+"'s comments", r.fg.comments, tc.comments)
		if tc.posts {
			continue
		}
		checkRows(t, r.events, before, wantRow{kind: factoryd.EventQuestionAnswered, repo: "sf/code", taskID: "t-r",
			detail: map[string]any{"action": factoryd.ActContinue, "answer": "use the replica", "case_id": int64(7)}})
	}
}

// Act's refusal reaches the human whole as a 409, and nothing is posted.
func TestAnswerRefusalNamesTheOfferedActions(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
	const why = "the session cap of 3 is reached, and the human may take extend, successor, replan and close"
	r.actErr = factoryd.ErrActRefused(why)
	rec := r.do(post("/tasks/t-e/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), why) {
		t.Fatalf("answer = %d: %s, want 409 with the refusal", rec.Code, rec.Body)
	}
	if len(r.fg.comments) != 0 {
		t.Fatalf("comments %v: the refused answer was posted", r.fg.comments)
	}
}

// A press Act refuses shows the human each answer by its label, never by
// the key an agent types.
func TestARefusedPressNamesTheAnswersByTheirLabels(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
	r.actErr = factoryd.ErrActRefused("the session cap of 3 is reached (the card's answers at this stop: `extend`, `close` and `note`), and the human may take `extend` and `close`")
	rec := r.do(post("/tasks/t-e/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"5"}, "action": {"continue"}, "continue.note": {"go on"}}), ck)
	want := "the session cap of 3 is reached (the card's answers at this stop: “Give more room”, “End the card” and “Not now”), and the human may take “Give more room” and “End the card”\n"
	if rec.Code != http.StatusConflict || rec.Body.String() != want {
		t.Fatalf("the refused press = %d %q, want 409 %q", rec.Code, rec.Body, want)
	}
}

// The chat's answer carries its action: a human's re-verify, on rung 3.
func TestChatAnswerCarriesItsAction(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-e", Text: "check it again", Action: factoryd.ActReverify}); err != nil {
		t.Fatal(err)
	}
	got, _ := actedOnce(t, r)
	want := factoryd.ActInput{TaskID: "t-e", CaseID: 5, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActReverify, Note: "check it again"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Act took %+v, want %+v", got, want)
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
	const within = "3 of 10 sessions, 1 h 20 min of 5 h agent time and 12.50 of 20.00 EUR drawn this UTC month"
	if page := board(); !strings.Contains(page, within) || strings.Contains(page, "overdrawn") {
		t.Fatalf("the board lacks %q or says overdrawn:\n%s", within, page)
	}
	r.drawn.Sessions = 11
	const over = "11 of 10 sessions, 1 h 20 min of 5 h agent time and 12.50 of 20.00 EUR drawn this UTC month, overdrawn in sessions: only the human draws more of that part until the month turns"
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

// Words on a card whose newest case is its session cap or its agent time
// cap give it a session's worth with its money, as the cap's own answer
// does: continue would find no room. Words on a card whose newest case is
// an ask or a check-in's share stop go on, whatever its session count
// says.
func TestWordsGiveASessionsWorthOnlyAtACap(t *testing.T) {
	room := factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1, "money": true}, Note: "use the replica"}
	for kind, want := range map[factoryd.CaseKind]factoryd.Resolution{
		factoryd.CaseSessionCap:    room,
		factoryd.CaseAgentTimeCap:  room,
		factoryd.CaseCheckinShare:  {Action: factoryd.ActContinue, Note: "use the replica"},
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
// every entry: the chat's close on a human card runs the close's work, the
// human's words posted and the issue closed, and resolves the card's case
// with the close, handing Act the comment the close posted. The real Act is the
// only gate: it takes the chat's room on a running card with no case
// open, a draw, and refuses the button's words on a card that holds no
// case, a continue.
func TestAnActionMeetsTheAnswersGates(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerGo, Label: "Have the factory do it"},
		factoryd.QuestionChoice{Value: factoryd.AnswerDone, Label: "I did it"})}
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-hq", Action: factoryd.ActClose, Text: "Moved to the ops board."}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"), []string{"Moved to the ops board."}) ||
		!slices.Equal(r.fg.patched, []string{`/api/v1/repos/sf/code/issues/51 {"state":"closed"}`}) {
		t.Fatalf("comments %q, patched %v; want the words posted and the issue closed", r.fg.commentBodies("/api/v1/repos/sf/code/issues/51/comments"), r.fg.patched)
	}
	got, posts := actedOnce(t, r)
	equal(t, "the close", []any{got, posts}, []any{factoryd.ActInput{TaskID: "t-hq", CaseID: 1, By: "sebastian", Via: factoryd.ViaChat, Rung: 3,
		Res: factoryd.Resolution{Action: factoryd.ActClose, Note: "Moved to the ops board."}}, true})

	r = newRig(t)
	run := factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 11, Track: policy.TrackDirect, BudgetEUR: 5}
	rev := factoryd.Task{ID: "t-rev", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 12, Track: policy.TrackDirect, BudgetEUR: 5}
	r.tasks.tasks = []factoryd.BoardTask{{Task: run}, {Task: rev}}
	ra := newRealRunner(t, policy.AutonomyManual)
	ra.seed(t, run)
	ra.seed(t, rev)
	ra.wire(r, "t-run", false)
	var steered []string
	r.c.cfg.Steer = func(_ context.Context, taskID, _, text string) error {
		steered = append(steered, taskID+": "+text)
		return nil
	}
	// a room is a draw, which Act takes with no case open, and its words
	// steer nothing. By hand: the card ran no session, so a session's worth
	// is a quarter of its 20 minutes, 5 minutes, and 20 minutes hold 4,
	// more than its 3 starts: the room now is 3, and 1 session's worth aims
	// at 4, 1 start and no time
	if _, err := runTool(t, r, "give_room", "member", toolInput{TaskID: "t-run", Sessions: 1, Money: true, Text: "one more"}); err != nil {
		t.Fatalf("the chat's room on a running card: %v", err)
	}
	if got, _, _ := ra.pg.Task(context.Background(), "t-run"); got.Usage.RaisedSessions != 1 || got.Usage.RaisedTime != 0 || len(steered) != 0 {
		t.Errorf("raised sessions %d and time %s, steered %v; want 1, none and none", got.Usage.RaisedSessions, got.Usage.RaisedTime, steered)
	}
	// the button's words on a card that holds no case reach the real Act
	// as a continue, which it refuses
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-rev/answer", url.Values{"csrf": {s.csrf}, "answer": {"go on"}}), ck)
	if want := "no ask is open on card #12, so “Go on with my note” has nothing to answer"; rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Errorf("words on a card with no case = %d %q; want 409 naming %q", rec.Code, rec.Body, want)
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
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerDone, Label: "I did it"})}
		ck, s := r.loggedIn(t)
		return r.do(post("/tasks/t-hq/answer", url.Values{"csrf": {s.csrf}, "value": {factoryd.AnswerDone}}), ck)
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
	if rec := press(r); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "has no open ask, so a new answer goes on the issue or on a new card") ||
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

// A human card's "End the card" ends its case through Act as the human's
// close once its work went through: the note posted, the issue closed, and
// the note's comment handed to Act.
func TestCloseOfAHumanCardResolvesItsCase(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerPark, Label: "Wait for it"})}
	ck, _ := r.loggedIn(t)
	end := answerForm(t, drawer(t, r, ck, "t-hq", "overview"), factoryd.ActClose)
	end.Values.Set("close.note", "done by hand")
	if rec := r.do(post(end.Action, end.Values), ck); rec.Code != http.StatusSeeOther {
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

// A spend limit the room's money passes, which the room raised by what it
// needed when it was worked out, means the card's money moved since: the
// refusal asks for the room again, on every door.
func TestASpendLimitPassedAsksForTheRoomAgain(t *testing.T) {
	const want = "Card #13 holds 4.50 EUR now, more than when its room was worked out, so the room's money passes its spend limit. Give the room again: its raises follow the card as it is now."
	for _, via := range []string{factoryd.ViaButton, factoryd.ViaChat} {
		r := needRig(t)
		_, s := r.loggedIn(t)
		r.actErr = &factoryd.CardCapError{Budget: 4.5}
		_, err := r.c.doExtend(t.Context(), actor{s: s, repository: "sf/code", via: via}, "t-e", 1, true, "go")
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != want {
			t.Errorf("%s: the room = %v, want %q", via, err, want)
		}
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

// The console's press of every answer the register's face offers fits the
// action table on the facts of its case. For each case kind, each answer
// of its face, pressed with its fields' defaults and a note where one is
// required, reaches Act on rung 3 as an action the real table gives the
// human on hand-built facts of the kind: factoryd.Offered holds it, and its
// own rule takes the answer's arguments. A note, a continue and a settle
// carry what their rule reads, which Offered's probes do not, so their rule
// alone judges them. A human card's choice is its question's to take.
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
		case res.Action != factoryd.ActNote && res.Action != factoryd.ActContinue && res.Action != factoryd.ActSettle &&
			!slices.ContainsFunc(factoryd.Offered(f, 3), func(o factoryd.Action) bool { return o.Name == res.Action }):
			return "not offered"
		}
		return a.Applies(f, res)
	}
	sample := map[string]string{"note": "the review misreads the spec", "depends_on": "#2190", "title": "Fix the importer",
		"why": "So a round trip keeps the accent.", "summary": "The importer keeps the accent.", "criterion": "size_bound_escape"}
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
		{factoryd.CaseCheckinShare, "check-in: " + factoryd.CheckSpend + ": the card's spend is above the policy's bound", nil, room},
		{factoryd.CaseCheckinShare, "check-in: " + factoryd.CheckSpend + ": check-in call cap reached (3 calls)", nil, room},
		{factoryd.CaseGateStop, "The gate supervisor stopped WaitChecks (attempt 2) past its bound of 30 min: checks still red after 3 runs.", nil, checksWait},
		{factoryd.CaseGateStop, "The gate supervisor stopped review: review: concerns on the head", nil, pr},
		{factoryd.CaseFindingsRepeated, "No reply can change this: the head and the card's limits are unchanged since the last stop.", nil, pr},
		{factoryd.CaseAgentEscalate, factoryd.AgentAsks + "Use a replica?", []string{"Yes", "No"}, room},
		{factoryd.CaseOther, "the pod never started", nil, room},
		{factoryd.CaseWaitExpired, "The card was quiet for 3 days.", nil, room},
	} {
		tc.facts.Kind = tc.kind
		facts := map[string]any{}
		raw, _ := json.Marshal(tc.facts)
		_ = json.Unmarshal(raw, &facts)
		facts["reason"] = tc.reason
		if tc.choices != nil {
			facts["choices"] = tc.choices
		}
		k := factoryd.Case{ID: 1, TaskID: "t-k", Kind: tc.kind, Status: factoryd.CaseAtHuman, Facts: facts}
		card := factoryd.Task{ID: "t-k", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, BudgetEUR: 2}
		face := factoryd.FaceOf(k, factoryd.Facts{CaseFacts: tc.facts, Card: card})
		if len(face.Answers) < 2 {
			t.Errorf("%s: the face offers %d answers", tc.kind, len(face.Answers))
		}
		for _, answer := range face.Answers {
			r := newRig(t)
			_, s := r.loggedIn(t)
			r.tasks.set(tc.reason, card)
			r.openCases = map[string][]factoryd.Case{"t-k": {k}}
			a := actor{s: s, repository: "sf/code", via: factoryd.ViaButton}
			if _, err := r.c.doPressCase(t.Context(), a, "t-k", 1, answer, sample); err != nil || len(r.acted) != 1 {
				t.Errorf("%s, %s: %v, Act took %+v", tc.kind, answer.Key(), err, r.acted)
				continue
			}
			in := r.acted[0]
			if why := fits(tc.facts, in.Res); in.Rung != 3 || why != "" {
				t.Errorf("%s, %s: Act took %+v on rung %d, which the table refuses: %s", tc.kind, answer.Key(), in.Res, in.Rung, why)
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

	if got, want := fits(capped, factoryd.Resolution{Action: factoryd.ActContinue, Note: "go on"}), "the session cap of 3 is reached (the card's answers at this stop: `extend`, `close` and `note`)"; got != want {
		t.Errorf("a continue at the cap = %q, want the table's refusal %q", got, want)
	}
	ask := room
	ask.Kind = factoryd.CaseAgentEscalate
	if slices.ContainsFunc(factoryd.Offered(ask, 3), func(o factoryd.Action) bool { return o.Name == factoryd.ActExtend }) {
		t.Errorf("the human on an ask is offered an extend, want none: a grant there answers no question")
	}
}

// give_room reaches Act on any live card: on an ask its room meets the
// table's refusal, which the console does not judge before it.
func TestGiveRoomLeavesTheAskToAct(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	if _, err := runTool(t, r, "give_room", "member", toolInput{TaskID: "t-e", Sessions: 1, Money: true, Text: "yes"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := actedOnce(t, r); !reflect.DeepEqual(got.Res, factoryd.Resolution{Action: factoryd.ActExtend, Args: map[string]any{"sessions": 1, "money": true}, Note: "yes"}) {
		t.Fatalf("Act took %+v, want a room of 1 session's worth", got.Res)
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

// A human card's typed answer is one of its choices: args on it reach Act,
// which refuses them (humanChoice), and nothing is posted or closed before
// it does.
func TestHumanCardsTypedAnswerMeetsActsRefusal(t *testing.T) {
	for _, in := range []toolInput{
		{TaskID: "t-hq", Action: factoryd.ActClose, Args: json.RawMessage(`{"model":"stronger"}`)},
	} {
		r := newRig(t)
		r.tasks.tasks = []factoryd.BoardTask{humanAsk("t-hq", 51, factoryd.QuestionChoice{Value: factoryd.AnswerClose, Label: "Done"})}
		r.actErr = factoryd.ErrActRefused("Act's refusal")
		_, err := runTool(t, r, "answer", "member", in)
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != "Act's refusal" || len(r.acted) != 1 || r.acted[0].Res.Args["model"] != "stronger" ||
			len(r.fg.comments) != 0 || len(r.fg.patched) != 0 {
			t.Errorf("%+v = %v, Act took %+v, comments %v, patched %v; want Act's refusal after one call and no side effect", in, err, r.acted, r.fg.comments, r.fg.patched)
		}
	}
}

// A room on a card that ended is refused at the door with the card's state
// and what fits it, and nothing reaches Act.
func TestGiveRoomOnAnEndedCardNamesWhatFits(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-d", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 13})
	_, err := runTool(t, r, "give_room", "member", toolInput{TaskID: "t-d", Sessions: 1, Money: true, Text: "one more"})
	var no *refusal
	if want := "Card #13 " + factoryd.StatusMeaning("done") + ", not a live card to give room: a new card continues the work."; !errors.As(err, &no) ||
		no.status != http.StatusConflict || no.msg != want || len(r.acted) != 0 {
		t.Fatalf("give_room on a done card = %v, Act took %+v; want 409 %q", err, r.acted, want)
	}
}

// A card at its agent time cap leads with the room, and a wrong press
// names what fits.
func TestTimeCappedCardNamesItsGrant(t *testing.T) {
	r := newRig(t)
	ck, _ := r.loggedIn(t)
	r.tasks.set("the agent time is used up: every finding sent to the agent so far goes to a human", factoryd.Task{ID: "t-tc", Kind: "agent",
		Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "Trim the logs."})
	seatAsk(r, "t-tc", factoryd.CaseAgentTimeCap, roomyFacts(func(f *factoryd.CaseFacts) { f.Attempts, f.AgentTimeS = 1, 7200 }))
	esc := escalationOf(t, r, ck, "t-tc")
	if first := strings.Index(esc, `data-answer="`); first < 0 || !strings.HasPrefix(esc[first:], `data-answer="extend"`) || !strings.Contains(esc, `name="extend.sessions"`) {
		t.Fatalf("the time-capped card's answer form =\n%s\nwant the room first, with its count", esc)
	}
	if got, want := fitting(factoryd.Task{Status: "waiting-human", IssueNumber: 13}, capFacts{capped: true, timeCap: true}),
		"more room answers it (`give_room`), as does an answer with the findings (answer). Close ends it"; got != want {
		t.Fatalf("fitting = %q, want %q", got, want)
	}
}

// give_room says what it gives, in its sentence and in the title its
// proposal leads with: sessions' worth of time and money, at a stop and
// ahead of time.
func TestGiveRoomSaysWhatItGives(t *testing.T) {
	r := newRig(t)
	tool, _ := r.c.chatTool("give_room")
	if want := "Give a card more sessions' worth of time and money, at its stop or ahead of time."; tool.about != want {
		t.Fatalf("give_room's about = %q, want %q", tool.about, want)
	}
	if want := "Give a card more room"; tool.title != want {
		t.Fatalf("give_room's title = %q, want %q", tool.title, want)
	}
}

// The chat's answer tool names the actions rung 2 or the human may take
// but the room, which give_room gives: withdraw, the factory's own, is
// none of them.
func TestChatAnswerOffersNoWithdraw(t *testing.T) {
	i := slices.IndexFunc(tools, func(t chatTool) bool { return t.name == "answer" })
	action, _ := tools[i].params["action"].(map[string]any)
	enum, _ := action["enum"].([]string)
	if slices.Contains(enum, factoryd.ActWithdraw) || slices.Contains(enum, factoryd.ActExtend) || !slices.Contains(enum, factoryd.ActRouteApproval) {
		t.Fatalf("the answer tool's actions = %q, want rung 2's and the human's, no withdraw and no extend", enum)
	}
}

// A press with no words posts its resolution's sentence as the human's
// own comment, the comment a run from before the cases gate wakes on,
// where factoryd's record of it wakes nothing: Give more room at 1
// session's worth with no note posts "“Give more room”: 1 session's
// worth" as sebastian.
func TestAPressWithNoWordsPostsItsSentence(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
	r.openCases["t-e"][0].Kind = factoryd.CaseSessionCap
	if rec := r.do(post("/tasks/t-e/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"5"}, "action": {factoryd.ActExtend},
		"extend.sessions": {"1"}, "extend.money": {"yes"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the press = %d: %s", rec.Code, rec.Body)
	}
	if _, posts := actedOnce(t, r); !posts {
		t.Fatal("Act took no Post: factoryd would post the record, which no run from before the gate wakes on")
	}
	r.fg.mu.Lock()
	defer r.fg.mu.Unlock()
	if !slices.Contains(r.fg.calls, "POST /api/v1/repos/sf/code/issues/13/comments Bearer access-1") ||
		r.fg.comments["/api/v1/repos/sf/code/issues/13/comments"] != "“Give more room”: 1 session's worth" {
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
	const live = " Card #11's open asks, by `case_id`: 7 (monitor flag)."
	refused("t-run", 8, "Card #11 has no ask by that number."+live)
	refused("t-run", 3, "Card #11 has no ask by that number."+live)
	refused("t-run", 6, "That ask ended."+live)
	refused("t-run", 5, "That ask is #14's."+live)
	refused("t-idle", 7, "That ask is #11's. Card #15 asks nothing now.")
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
	if !errors.As(err, &no) || no.status != http.StatusConflict || no.msg != "Approval request 0 waits for no answer any more. Reload the board." ||
		len(r.acted) != calls+1 || r.acted[calls].CaseID != 9 || r.acted[calls].Res.Action != factoryd.ActApproveMerge {
		t.Fatalf("approve_merge on case 9 = %v, Act calls %+v; want one call on case 9 whose work finds no request", err, r.acted[calls:])
	}
	refused("t-e", 10, "`case_id` 10 is the plan question of #13: answer it with `revision` 2 and `question` 1, and no `case_id`.")
}

// A card with a live case closes through Act, which removes its dependencies
// itself (factoryd's close): the console sends the close as typed and removes
// nothing first (its only write is the note's comment). A refusal of Act's close, whose error carries
// URL userinfo and a bearer value, reaches the human with both masked.
func TestCloseThroughActLeavesTheDependenciesToAct(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	r.fg.deps = map[string][]map[string]any{"/api/v1/repos/sf/code/issues/13/dependencies": dependsOn(7)}
	ck, _ := r.loggedIn(t)
	end := answerForm(t, drawer(t, r, ck, "t-e", "overview"), factoryd.ActClose)
	end.Values.Set("close.note", "Done.")
	if rec := r.do(post(end.Action, end.Values), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("close = %d: %s", rec.Code, rec.Body)
	}
	got, _ := actedOnce(t, r)
	equal(t, "the close", []any{got.Res, writesOf(r.fg)}, []any{factoryd.Resolution{Action: factoryd.ActClose, Note: "Done."}, []string{"POST /api/v1/repos/sf/code/issues/13/comments"}})

	r = newRig(t)
	caseCard(r)
	r.actErr = errors.New("forgejo PATCH https://user:pass@git.example/x: edge refused, Authorization: Bearer abcdef0123456789")
	ck, _ = r.loggedIn(t)
	end = answerForm(t, drawer(t, r, ck, "t-e", "overview"), factoryd.ActClose)
	rec := r.do(post(end.Action, end.Values), ck)
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

// By hand: in a human's turn the agent's decision goes out as factory-chat,
// with the project agent's token: its card answer's comment, its comment on
// a card and its edit of a card's issue. The same tools decided by the
// human go out with the human's own token.
func TestTheAgentsCardWritesGoOutAsTheChatBot(t *testing.T) {
	answer := toolInput{TaskID: "t-e", Text: "Go on with the replica."}
	comment := toolInput{TaskID: "t-e", Text: "The replica is fine."}
	edit := toolInput{TaskID: "t-e", Title: "Use the replica", Why: "So reads stay fast.", Summary: "The card moves the reads to the replica."}
	for _, c := range []struct {
		tool, decider string
		in            toolInput
		want          string
	}{
		{"answer", "agent", answer, "POST /api/v1/repos/sf/code/issues/13/comments Bearer chat-token"},
		{"answer", "member", answer, "POST /api/v1/repos/sf/code/issues/13/comments Bearer access-1"},
		{"comment", "agent", comment, "POST /api/v1/repos/sf/code/issues/13/comments Bearer chat-token"},
		{"comment", "member", comment, "POST /api/v1/repos/sf/code/issues/13/comments Bearer access-1"},
		{"edit", "agent", edit, "PATCH /api/v1/repos/sf/code/issues/13 Bearer chat-token"},
		{"edit", "member", edit, "PATCH /api/v1/repos/sf/code/issues/13 Bearer access-1"},
	} {
		t.Run(c.tool+" by the "+c.decider, func(t *testing.T) {
			r := newRig(t)
			caseCard(r)
			r.openCases["t-e"][0].Kind = factoryd.CaseSessionCap
			if _, err := runTool(t, r, c.tool, c.decider, c.in); err != nil {
				t.Fatal(err)
			}
			r.fg.mu.Lock()
			defer r.fg.mu.Unlock()
			writes := slices.DeleteFunc(slices.Clone(r.fg.calls), func(s string) bool { return strings.HasPrefix(s, "GET ") })
			if !slices.Equal(writes, []string{c.want}) {
				t.Errorf("Forgejo writes %q, want %q", writes, c.want)
			}
		})
	}
}

// The agent's own acts outside Act carry the chat user, never the login
// of the human whose turn woke it: its steer reaches the session as the
// project agent's, a human's steer as theirs.
func TestTheAgentsSteerIsTheChatUsers(t *testing.T) {
	r, _ := newWritesRig(t)
	_, s := r.loggedIn(t)
	var by []string
	r.c.cfg.Steer = func(_ context.Context, _, user, _ string) error {
		by = append(by, user)
		return nil
	}
	for _, decider := range []string{"agent", "member"} {
		a := actor{s: s, repository: "sf/code", via: factoryd.ViaChat, decider: decider, level: policy.AutonomyAutonomous}
		if _, err := r.c.doSteer(t.Context(), a, "t-r", "rebase first"); err != nil {
			t.Fatalf("%s's steer: %v", decider, err)
		}
	}
	equal(t, "the steers' senders", by, []string{factoryd.ChatUser, "sebastian"})
}

// roomCard is an agent card of sf/code on the room's rig: its row, the
// sessions and the agent time its runs left on it, and the stop its case
// waits at, none for a card waiting on no case.
type roomCard struct {
	task      factoryd.Task
	attempts  int
	agentTime time.Duration
	stop      factoryd.CaseKind
}

// roomRig is the console rig on the real Act and the real store, with
// linkshort at level, and cards in both, each a direct card of the
// maintenance process, on sf/code unless it names its repository, whose
// policy sets 3 sessions, 20 minutes of agent time, a spend limit of 5.00
// EUR, a gate reserve of 0.40 EUR, no session floor and a month of 50.00
// EUR. Each stop's case stands at the human after its trip's step;
// roomRig answers the cases by card.
func roomRig(t *testing.T, level string, cards ...roomCard) (*rig, *realAct, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	r := newRig(t)
	ra := newRealRunner(t, level)
	cases := map[string]int64{}
	var tasks []factoryd.Task
	for _, k := range cards {
		card := k.task
		card.Kind, card.Track, card.Process, card.Objective = "agent", policy.TrackDirect, "maintenance", "Speed up the board."
		card.Repository = cmp.Or(card.Repository, "sf/code")
		if err := ra.pg.CreateTask(ctx, card); err != nil {
			t.Fatal(err)
		}
		if _, err := ra.pg.DB().ExecContext(ctx, `UPDATE tasks SET attempts = $2, agent_time_ns = $3 WHERE id = $1`, card.ID, k.attempts, int64(k.agentTime)); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, card)
		if k.stop == "" {
			continue
		}
		c, _, err := ra.pg.OpenCase(ctx, factoryd.Case{TaskID: card.ID, Repository: card.Repository, Kind: k.stop, StateKey: "cap",
			Problem: fmt.Sprintf("Card #%d stopped at its cap", card.IssueNumber)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ra.pg.DB().ExecContext(ctx, `UPDATE cases SET status = 'at_human', owner_rung = 3, next_wake_at = NULL WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		// the trip's own step, which tells Act the run reads its raise from the card's row
		if _, err := ra.pg.AppendCaseStep(ctx, factoryd.CaseStep{CaseID: c.ID, Rung: 0, Actor: factoryd.FactoryDecider, Found: "no resolver fits",
			Outcome: factoryd.StepHandedUp}); err != nil {
			t.Fatal(err)
		}
		cases[card.ID] = c.ID
	}
	r.tasks.set("", tasks...)
	ra.wire(r, "", false)
	return r, ra, cases
}

// stored is card id as the real store holds it.
func (ra *realAct) stored(t *testing.T, id string) factoryd.Task {
	t.Helper()
	k, ok, err := ra.pg.Task(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("card %s: %v %v", id, ok, err)
	}
	return k
}

// drawRows are card id's draw rows, oldest first.
func (ra *realAct) drawRows(t *testing.T, id string) []factoryd.Event {
	t.Helper()
	events, err := ra.pg.TaskEvents(context.Background(), id, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []factoryd.Event
	for _, e := range slices.Backward(events) {
		if e.Kind == factoryd.EventCardExtended || e.Kind == factoryd.EventCardFunded {
			out = append(out, e)
		}
	}
	return out
}

// give_room answers a stop and gives room ahead of time, each raise the
// one RoomFor works out, and its result names them. By hand:
//
// At the session cap, #31 ran 3 sessions in 15 minutes for 0.90 EUR of
// its 2.00: a session's worth is 5 minutes (15 / 3, the floor of 5 holds)
// and 0.30 EUR (0.90 / 3, no session floor). Starts left 3 - 3 = 0, time
// left 20 - 15 = 5 minutes, money left 2.00 - 0.90 - 0.40 = 0.70 EUR: the
// room now is min(0, 1, 2) = 0, so 1 session's worth needs 1 start, 1 x
// 5 - 5 = 0 minutes and no money (0.30 <= 0.70).
//
// Running #32 ran 3 sessions in 42 minutes for 3.60 EUR of 6.00, with 1
// session and 40 minutes raised before, under its own spend limit of
// 10.00: a session's worth is 14 minutes and 1.20 EUR. Starts left 4 - 3 =
// 1, time left 60 - 42 = 18 minutes, money left 6.00 - 3.60 - 0.40 = 2.00
// EUR: the room now is min(1, 1, 1) = 1, so 2 sessions' worth aim at 3:
// 3 - 1 = 2 starts, 3 x 14 - 18 = 24 minutes and 3 x 1.20 - 2.00 = 1.60
// EUR, the budget to 7.60 under the limit of 10.00. A count of 0 or 6
// before it is Act's to refuse, and changes nothing.
func TestGiveRoomAtAStopAndAheadOfTime(t *testing.T) {
	r, ra, cases := roomRig(t, policy.AutonomyManual,
		roomCard{task: factoryd.Task{ID: "t-cap", Status: "waiting-human", IssueNumber: 31, BudgetEUR: 2, SpentEUR: 0.9},
			attempts: 3, agentTime: 15 * time.Minute, stop: factoryd.CaseSessionCap},
		roomCard{task: factoryd.Task{ID: "t-run", Status: "running", IssueNumber: 32, BudgetEUR: 6, SpentEUR: 3.6, CeilingEUR: 10,
			Usage: factoryd.TaskUsage{RaisedSessions: 1, RaisedTime: 40 * time.Minute}}, attempts: 3, agentTime: 42 * time.Minute})

	said, err := runTool(t, r, "give_room", "member", toolInput{TaskID: "t-cap", Sessions: 1, Money: true, Text: "Finish the cache."})
	if err != nil {
		t.Fatalf("give_room at the session cap: %v", err)
	}
	k, err := ra.pg.CaseByID(t.Context(), cases["t-cap"])
	if err != nil || k.Status != factoryd.CaseResolved || k.Resolution == nil || k.Resolution.Action != factoryd.ActExtend || k.Resolution.Rung != 3 {
		t.Fatalf("the session cap's case = %+v, %v; want it resolved by the human's extend", k, err)
	}
	if card := ra.stored(t, "t-cap"); card.Usage.RaisedSessions != 1 || card.Usage.RaisedTime != 0 || factoryd.Cents(card.BudgetEUR) != 200 {
		t.Fatalf("#31 after the room: %d raised sessions, %s raised time, %.2f EUR; want 1, none and 2.00", card.Usage.RaisedSessions, card.Usage.RaisedTime, card.BudgetEUR)
	}
	if !strings.HasSuffix(said, " Added 1 session.") {
		t.Fatalf("give_room at the cap said %q, want the start it added", said)
	}

	for _, n := range []int{0, 6} {
		_, err := runTool(t, r, "give_room", "member", toolInput{TaskID: "t-run", Sessions: n, Money: true, Text: "The cache needs more."})
		var no *refusal
		if !errors.As(err, &no) || no.status != http.StatusConflict || !strings.Contains(no.msg, "a room counts 1 to 5 sessions' worth") {
			t.Fatalf("give_room of %d = %v, want Act's 409 naming the count's range", n, err)
		}
	}
	said, err = runTool(t, r, "give_room", "member", toolInput{TaskID: "t-run", Sessions: 2, Money: true, Text: "The cache needs more."})
	if err != nil {
		t.Fatalf("give_room ahead of time: %v", err)
	}
	card := ra.stored(t, "t-run")
	if card.Usage.RaisedSessions != 3 || card.Usage.RaisedTime != 64*time.Minute || factoryd.Cents(card.BudgetEUR) != 760 || factoryd.Cents(card.CeilingEUR) != 1000 || card.Status != "running" {
		t.Fatalf("#32 after the room: %d raised sessions, %s raised time, %.2f EUR under %.2f, %s; want 3, 1h4m, 7.60 under 10.00, running",
			card.Usage.RaisedSessions, card.Usage.RaisedTime, card.BudgetEUR, card.CeilingEUR, card.Status)
	}
	if !strings.HasSuffix(said, " Added 2 sessions, 24 minutes of agent time and 1.60 EUR.") {
		t.Fatalf("give_room ahead of time said %q, want the raises it applied", said)
	}
}

// The chat's answer gives no room: an extend, and a fund or a ceiling
// raise as they were once typed, are refused with the tool that gives it,
// on the card's ask and on a case alike, and nothing reaches Act. The
// answer tool offers none of them, nor a room's old arguments.
func TestAnswerRefusesARoom(t *testing.T) {
	r := newRig(t)
	caseCard(r)
	for _, action := range []string{factoryd.ActExtend, "fund", "raise_ceiling"} {
		for _, in := range []toolInput{{TaskID: "t-e", Text: "more room", Action: action}, {TaskID: "t-e", CaseID: 5, Text: "more room", Action: action}} {
			_, err := runTool(t, r, "answer", "member", in)
			var no *refusal
			if !errors.As(err, &no) || no.status != http.StatusBadRequest || !strings.Contains(no.msg, "use `give_room`") {
				t.Errorf("answer with %s on case %d = %v, want 400 naming give_room", action, in.CaseID, err)
			}
		}
	}
	if len(r.acted) != 0 {
		t.Fatalf("a refused room reached Act: %+v", r.acted)
	}
	tool, ok := r.c.chatTool("answer")
	if !ok {
		t.Fatal("there is no answer tool")
	}
	actions, _ := tool.params["action"].(map[string]any)["enum"].([]string)
	for _, name := range []string{factoryd.ActExtend, "fund", "raise_ceiling"} {
		if slices.Contains(actions, name) {
			t.Errorf("the answer tool offers the action %s", name)
		}
	}
	args, _ := tool.params["args"].(map[string]any)["properties"].(map[string]any)
	equal(t, "the answer tool's parameters", slices.Sorted(maps.Keys(tool.params)),
		[]string{"action", "answers", "args", "case_id", "decision", "question", "revision", "task_id", "text"})
	equal(t, "the answer tool's args", slices.Sorted(maps.Keys(args)),
		[]string{"action", "body", "criterion", "depends_on", "model", "size", "summary", "task_id", "title", "verdict", "why"})
}

// A human's room for a card is an approver's on every door: the opened
// card's Give more room and the chat's give_room give one verdict for the
// same human. The chat's door admits a writer, whose turn the factory agent
// may take on its own; the press on the human's rung meets the approver's
// rule. A writer is refused on both doors and Act takes nothing; an
// approver reaches Act on both, on rung 3.
func TestGiveRoomNeedsAnApprover(t *testing.T) {
	const words = "Only the approvers of project ai-software-factory-code may give a card room."
	for _, tc := range []struct {
		who  string
		acts int
	}{{"alice", 0}, {"sebastian", 2}} {
		r := newRig(t)
		r.openCases = map[string][]factoryd.Case{"t-g": {{ID: 91, TaskID: "t-g", Kind: factoryd.CaseGateStop, Status: factoryd.CaseAtHuman,
			Facts: roomyFacts(func(f *factoryd.CaseFacts) { f.Kind, f.Gate, f.Attempts = factoryd.CaseGateStop, "review", 1 })}}}
		r.tasks.tasks = append(r.tasks.tasks,
			factoryd.BoardTask{Task: factoryd.Task{ID: "t-g", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 24, BudgetEUR: 2}})
		r.fg.user, r.repoWriters["sf/code"] = tc.who, []string{"sebastian", "alice"}
		r.forgetWriters()
		ck, s := r.loggedIn(t)
		press := r.do(post("/tasks/t-g/resolve", url.Values{"csrf": {s.csrf}, "case_id": {"91"}, "action": {factoryd.ActExtend},
			"extend.sessions": {"1"}, "extend.money": {"yes"}}), ck)
		_, err := runTool(t, r, "give_room", "member", toolInput{TaskID: "t-g", Sessions: 1, Money: true, Text: "Go on"})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		trn := &turn{c: r.c, r: req.WithContext(context.WithValue(req.Context(), ctxKey{}, s)),
			a: actor{s: s, repository: "sf/code", via: factoryd.ViaChat}, p: factoryd.Project{Name: "ai-software-factory-code", Repository: "sf/code"}}
		if door := trn.admitTool("give_room", toolInput{TaskID: "t-g"}); door != nil {
			t.Errorf("%s, a writer of the card: the chat's door = %v, want it admitted", tc.who, door)
		}
		var no *refusal
		switch {
		case tc.acts == 0 && (press.Code != http.StatusForbidden || strings.TrimSpace(press.Body.String()) != words ||
			!errors.As(err, &no) || no.status != http.StatusForbidden || no.msg != words):
			t.Errorf("%s, a writer: the press = %d %q, the chat's give_room = %v; want both refused with 403 %q", tc.who, press.Code, press.Body, err, words)
		case tc.acts > 0 && (press.Code != http.StatusSeeOther || err != nil):
			t.Errorf("%s, an approver: the press = %d: %s, the chat's give_room = %v; want both taken", tc.who, press.Code, press.Body, err)
		}
		if len(r.acted) != tc.acts {
			t.Fatalf("%s: Act took %+v, want %d answers", tc.who, r.acted, tc.acts)
		}
		for _, in := range r.acted {
			if in.Rung != 3 || in.CaseID != 91 || in.Res.Action != factoryd.ActExtend || in.Res.Args["sessions"] != 1 || in.Res.Args["money"] != true {
				t.Errorf("%s: Act took %+v, want the human's room of 1 session's worth with its money on case 91", tc.who, in)
			}
		}
	}
}

// The factory agent's room comes from the policy and the pool, not from
// the role of the human whose turn it takes: the Waker, woken on a card's
// session cap as alice, who writes linkshort and approves nothing, gives
// it 1 session's worth without money on rung 2, past the chat's confirm
// list, and the pool counts it. By hand: #36 ran 3 sessions of 3 in 15 of
// its 20 minutes, so a session's worth is 5 minutes; with no start and 5
// minutes left, the room now is 0, and 1 session's worth needs 1 start
// and no time, and no money, left out.
func TestTheWakerAsAWriterGivesRoomFromThePool(t *testing.T) {
	r, ra, cases := roomRig(t, policy.AutonomyAutonomous, roomCard{task: factoryd.Task{ID: "t-wk", Status: "waiting-human", Repository: "linkshort/linkshort",
		IssueNumber: 36, BudgetEUR: 2, SpentEUR: 0.9}, attempts: 3, agentTime: 15 * time.Minute, stop: factoryd.CaseSessionCap})
	withLinkshort(r)
	r.repoWriters["linkshort/linkshort"] = []string{"sebastian", "alice"}
	r.forgetWriters()
	p := factoryd.Project{Name: "linkshort", Repository: "linkshort/linkshort"}
	s, err := r.c.wakeSession(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if r.c.isApprover(t.Context(), "alice", p.Repository) {
		t.Fatal("alice approves linkshort, want a writer alone")
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/linkshort/chat", nil)
	trn := &turn{c: r.c, r: req.WithContext(context.WithValue(req.Context(), ctxKey{}, s)), p: p,
		a:   actor{s: s, repository: p.Repository, via: factoryd.ViaChat, wake: true, birth: factoryd.EventCaseClimbed, level: policy.AutonomyAutonomous},
		cfg: policy.ChatConfig{Confirm: policy.ConfirmFor(policy.AutonomyManual, policy.MergeHuman, policy.DefaultChat.Confirm)}}
	var events []string
	trn.stream(func(line string) error { events = append(events, line); return nil }, nil)
	result, proposal, status := trn.call(factoryd.OperatorCall{Name: "give_room",
		Arguments: `{"task_id":"t-wk","sessions":1,"money":false,"text":"The last test needs one more run, and the money holds."}`})
	if proposal != "" || status != 0 || !strings.Contains(result, " Added 1 session.") {
		t.Fatalf("the Waker's room = %q, proposal %q, status %d; want it given on rung 2", result, proposal, status)
	}
	k, err := ra.pg.CaseByID(t.Context(), cases["t-wk"])
	if err != nil || k.Status != factoryd.CaseResolved || k.Resolution == nil || k.Resolution.Rung != 2 || k.Resolution.By != factoryd.ChatUser {
		t.Fatalf("the session cap's case = %+v, %v; want it resolved by the factory agent on rung 2", k.Resolution, err)
	}
	rows := ra.drawRows(t, "t-wk")
	if len(rows) != 1 || rows[0].Kind != factoryd.EventCardExtended || rows[0].Detail["rung"] != 2.0 || rows[0].Detail["pool"] != true || rows[0].Detail["sessions"] != 1.0 {
		t.Fatalf("the draw rows = %+v, want one rung 2 pool row of 1 start", rows)
	}
	if d, err := ra.pg.PoolDrawn(t.Context(), p.Repository, time.Now()); err != nil || d != (policy.Draw{Sessions: 1}) {
		t.Fatalf("the pool counts %+v, %v; want the Waker's 1 session", d, err)
	}
}

// The project agent's own give_room is rung 2's, and its rows count in the
// escalation pool; the human's confirmed one is rung 3's, and its rows do
// not. By hand: each card ran 3 sessions in 42 minutes for 3.60 EUR of
// 6.00, with 1 session and 40 minutes raised before, under its own spend
// limit of 10.00, so a session's worth is 14 minutes and 1.20 EUR and the
// room now is min(4 - 3, 18 / 14, 2.00 / 1.20) = 1. One session's worth
// aims at 2: 2 - 1 = 1 start, 2 x 14 - 18 = 10 minutes and 2 x 1.20 - 2.00
// = 0.40 EUR. The pool counts the agent's 1 session, 10 minutes and 0.40
// EUR, and the human's draw leaves it there.
func TestTheChatAgentsRoomIsRungTwo(t *testing.T) {
	ran := func(id string, issue int64) roomCard {
		return roomCard{task: factoryd.Task{ID: id, Status: "running", IssueNumber: issue, BudgetEUR: 6, SpentEUR: 3.6, CeilingEUR: 10,
			Usage: factoryd.TaskUsage{RaisedSessions: 1, RaisedTime: 40 * time.Minute}}, attempts: 3, agentTime: 42 * time.Minute}
	}
	r, ra, _ := roomRig(t, policy.AutonomyManual, ran("t-agent", 33), ran("t-human", 34))
	want := policy.Draw{Sessions: 1, AgentTime: 10 * time.Minute, EUR: 0.4}
	pool := func() policy.Draw {
		d, err := ra.pg.PoolDrawn(t.Context(), "sf/code", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		d.EUR = float64(factoryd.Cents(d.EUR)) / 100
		return d
	}
	for _, tc := range []struct {
		id, decider string
		rung        float64
	}{{"t-agent", "agent", 2}, {"t-human", "member", 3}} {
		if _, err := runTool(t, r, "give_room", tc.decider, toolInput{TaskID: tc.id, Sessions: 1, Money: true, Text: "The cache needs another session."}); err != nil {
			t.Fatalf("%s's give_room: %v", tc.decider, err)
		}
		rows := ra.drawRows(t, tc.id)
		if len(rows) != 2 {
			t.Fatalf("%s's draw rows = %+v, want the start and time, then the money", tc.decider, rows)
		}
		for _, e := range rows {
			if e.Detail["rung"] != tc.rung || e.Detail["pool"] != true {
				t.Errorf("%s's row %s = %+v, want rung %v in the pool's rows", tc.decider, e.Kind, e.Detail, tc.rung)
			}
		}
		if got := pool(); got != want {
			t.Errorf("after %s's room the pool counts %+v, want the agent's %+v alone", tc.decider, got, want)
		}
	}
}

// Words typed at an agent-time cap give one session's worth with its
// money, as RoomFor works it out, where they once gave the card draw's
// hour. By hand: #35 ran 2 sessions in its whole 20 minutes for 0.60 EUR
// of 2.00, so a session's worth is 10 minutes and 0.30 EUR. Starts left
// 3 - 2 = 1, time left 0, money left 2.00 - 0.60 - 0.40 = 1.00 EUR: the
// room now is 0, and 1 session's worth needs no start, 10 minutes and no
// money (0.30 <= 1.00).
func TestWordsAloneAtACapGiveOneSessionsWorth(t *testing.T) {
	r, ra, cases := roomRig(t, policy.AutonomyManual, roomCard{task: factoryd.Task{ID: "t-time", Status: "waiting-human", IssueNumber: 35, BudgetEUR: 2, SpentEUR: 0.6},
		attempts: 2, agentTime: 20 * time.Minute, stop: factoryd.CaseAgentTimeCap})
	if _, err := runTool(t, r, "answer", "member", toolInput{TaskID: "t-time", Text: "The last test needs ten more minutes."}); err != nil {
		t.Fatalf("words at the agent-time cap: %v", err)
	}
	k, err := ra.pg.CaseByID(t.Context(), cases["t-time"])
	if err != nil || k.Status != factoryd.CaseResolved || k.Resolution == nil || k.Resolution.Action != factoryd.ActExtend ||
		k.Resolution.Args["sessions"] != 1.0 || k.Resolution.Args["money"] != true {
		t.Fatalf("the agent-time cap's case = %+v, %v; want it resolved by an extend of 1 session's worth with its money", k.Resolution, err)
	}
	if card := ra.stored(t, "t-time"); card.Usage.RaisedTime != 10*time.Minute || card.Usage.RaisedSessions != 0 || factoryd.Cents(card.BudgetEUR) != 200 {
		t.Fatalf("#35 after the words: %s raised time, %d raised sessions, %.2f EUR; want 10m, none and 2.00", card.Usage.RaisedTime, card.Usage.RaisedSessions, card.BudgetEUR)
	}
}
