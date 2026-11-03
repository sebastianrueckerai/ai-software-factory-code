package console

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

// caseCard files agent card t-e on sf/code, waiting on a human at its
// session cap on #13, with its live cases: 5, at the human, and the merge
// case 9, which its own door answers.
func caseCard(r *rig) {
	r.tasks.set("Card #13 used its sessions", factoryd.Task{ID: "t-e", Kind: "agent", Status: "waiting-human", Repository: "sf/code",
		IssueNumber: 13, BudgetEUR: 2})
	r.openCases = map[string][]factoryd.Case{"t-e": {
		{ID: 5, TaskID: "t-e", Kind: factoryd.CaseSessionCap, Status: factoryd.CaseAtHuman},
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
	posts := in.Post != nil
	in.Post = nil
	return in, posts
}

// runTool runs the chat tool name as a turn of sebastian's chat on sf/code
// runs it, decided by decider: the agent unconfirmed, or the member.
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

// The Grant button answers the card's newest live case through Act, the
// merge case aside: an extend of one session on rung 3 by the human, the
// choice's label its note, which Act has the console post as the human's
// comment.
func TestAnswerGoesThroughActOnTheCardsCase(t *testing.T) {
	r := newRig(t)
	ck, s := r.loggedIn(t)
	caseCard(r)
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
