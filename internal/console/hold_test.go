package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

const holdProject = "ai-software-factory-code"

type deployCall struct{ repository, by, via, decider string }

// holdRig is a rig whose project sf/code is held 7 h 30 min by plan #12
// (3 of 5 merged); the returned slice records each Deploy now.
func holdRig(t *testing.T) (*rig, *[]deployCall) {
	t.Helper()
	r := newRig(t)
	r.repoWriters["sf/code"] = []string{"sebastian", "morgan"}
	r.forgetWriters()
	r.c.cfg.Hold = func(_ context.Context, repository string) (factoryd.Hold, error) {
		if repository != "sf/code" {
			return factoryd.Hold{}, nil
		}
		return factoryd.Hold{Since: time.Now().Add(-7*time.Hour - 30*time.Minute), Groups: []factoryd.Group{
			{Plan: "t-pl", Issue: 12, Children: map[string]int{"merged": 2, "done": 1, "queued": 2}}}}, nil
	}
	calls := &[]deployCall{}
	r.c.cfg.DeployNow = func(_ context.Context, repository, by, via, decider string) (string, error) {
		*calls = append(*calls, deployCall{repository, by, via, decider})
		return "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d", nil
	}
	return r, calls
}

func TestBoardShowsTheProductionHold(t *testing.T) {
	r, _ := holdRig(t)
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	for _, want := range []string{`production held · 7 h</span>`, `plan #12 · 3 of 5 merged`, `action="/projects/` + holdProject + `/deploy-now"`,
		`<p class="sp-note warn">Held for 7 h: plan #12 is open on main.`, "Deploy now</button>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the approver's board lacks %q", want)
		}
	}

	wck, _ := memberLogin(t, r, "morgan")
	body = r.do(httptest.NewRequest(http.MethodGet, "/", nil), wck).Body.String()
	for _, want := range []string{`production held · 7 h</span>`, `plan #12 · 3 of 5 merged`} {
		if !strings.Contains(body, want) {
			t.Errorf("the writer's board lacks %q", want)
		}
	}
	if strings.Contains(body, "deploy-now") {
		t.Error("the writer's board carries the Deploy now form")
	}

	r.c.cfg.Hold = func(context.Context, string) (factoryd.Hold, error) { return factoryd.Hold{}, nil }
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); strings.Contains(body, "production held") {
		t.Error("a free production shows the hold")
	}
}

func TestBoardShowsTheHoldWithoutMirrors(t *testing.T) {
	r, _ := holdRig(t)
	r.c.cfg.Mirrors = nil
	ck, _ := r.loggedIn(t)
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, "production held · 7 h") {
		t.Error("the board without mirrors lacks the hold cell")
	}
}

func TestBoardNamesAHoldItCannotRead(t *testing.T) {
	r, _ := holdRig(t)
	r.c.cfg.Hold = func(context.Context, string) (factoryd.Hold, error) { return factoryd.Hold{}, context.DeadlineExceeded }
	ck, _ := r.loggedIn(t)
	if body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(); !strings.Contains(body, "the production hold could not be read") {
		t.Error("an unreadable hold goes unnamed")
	}
}

func TestBoardTextNamesTheHold(t *testing.T) {
	d := boardData{}
	d.Status.Hold = &holdLine{For: "7 h", Reason: "plan #12 is open on main", Groups: []string{"plan #12 · 3 of 5 merged"}}
	if got := boardWords(d); !strings.HasPrefix(got, "Production held for 7 h: plan #12 is open on main.\n") {
		t.Errorf("boardText = %q, want it to start with the hold", got)
	}
	if got := boardWords(boardData{}); strings.Contains(got, "Production held") {
		t.Errorf("a free production reads %q", got)
	}
}

func TestDeployNowButton(t *testing.T) {
	r, calls := holdRig(t)
	ck, s := r.loggedIn(t)
	path := "/projects/" + holdProject + "/deploy-now"

	req := post(path, url.Values{"csrf": {s.csrf}})
	req.Header.Set("HX-Request", "true")
	rec := r.do(req, ck)
	want := "Deploy now: main of " + holdProject + " at 9d9d9d9 goes to production. A merge after it waits for the open plans again."
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("Deploy now = %d, want 200 with %q: %s", rec.Code, want, rec.Body)
	}
	if len(*calls) != 1 || (*calls)[0] != (deployCall{"sf/code", "sebastian", factoryd.ViaButton, "member"}) {
		t.Fatalf("DeployNow calls = %+v, want one for sf/code by sebastian via the button", *calls)
	}

	if rec := r.do(post(path, url.Values{}), ck); rec.Code != http.StatusForbidden || len(*calls) != 1 {
		t.Errorf("without csrf = %d with %d calls, want 403 and still 1", rec.Code, len(*calls))
	}

	wck, ws := memberLogin(t, r, "morgan")
	if rec := r.do(post(path, url.Values{"csrf": {ws.csrf}}), wck); rec.Code != http.StatusForbidden || len(*calls) != 1 {
		t.Errorf("writer = %d with %d calls, want 403 and still 1", rec.Code, len(*calls))
	}
}

func TestDeployNowRefusesAFreeProduction(t *testing.T) {
	r, _ := holdRig(t)
	r.c.cfg.DeployNow = func(context.Context, string, string, string, string) (string, error) { return "", factoryd.ErrNotHeld }
	ck, s := r.loggedIn(t)
	rec := r.do(post("/projects/"+holdProject+"/deploy-now", url.Values{"csrf": {s.csrf}}), ck)
	want := "Production of " + holdProject + " is not held: every merge goes to production on its own."
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("Deploy now on a free production = %d %q, want 409 %q", rec.Code, rec.Body, want)
	}
}

func TestDeployNowRefusesARedMain(t *testing.T) {
	r, calls := holdRig(t)
	r.c.cfg.DeployNow = func(_ context.Context, repository, by, via, decider string) (string, error) {
		*calls = append(*calls, deployCall{repository, by, via, decider})
		return "", factoryd.ErrMainRed{Commit: "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"}
	}
	ck, s := r.loggedIn(t)
	rec := r.do(post("/projects/"+holdProject+"/deploy-now", url.Values{"csrf": {s.csrf}}), ck)
	want := "Main of " + holdProject + " at e2e2e2e is red: Deploy now waits for a green run on main. Press it again once main is green."
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("Deploy now on a red main = %d %q, want 409 %q", rec.Code, rec.Body, want)
	}
}

type replanCall struct {
	id, by, comment string
	offMain         bool
}

// holdAskRig is holdRig with the ask t-ask (#77) as the release keeper
// files it for the plan t-pl (#12), the plan card itself, and a Replan
// recorder.
func holdAskRig(t *testing.T) (*rig, *fakeEvents, *[]deployCall, *[]replanCall) {
	t.Helper()
	r, deploys := holdRig(t)
	replans := &[]replanCall{}
	r.c.cfg.Replan = func(_ context.Context, id, by, comment string, offMain bool) (string, error) {
		*replans = append(*replans, replanCall{id, by, comment, offMain})
		if offMain {
			return "Card #90 reverts the merged cards, then the planner replans.", nil
		}
		return "The planner replans on top of what is merged.", nil
	}
	r.tasks.tasks = []factoryd.BoardTask{holdAsk(),
		{Task: factoryd.Task{ID: "t-pl", Kind: "agent", Status: "done", Track: policy.TrackPlan, Process: "spec",
			Repository: "sf/code", IssueNumber: 12, Objective: "A plan."}},
	}
	return r, r.events, deploys, replans
}

// holdAsk is the ask t-ask (#77) as the release keeper files it for the
// plan t-pl (#12).
func holdAsk() factoryd.BoardTask {
	q := &factoryd.HumanQuestion{Ask: "Production has waited 7 hours. Wait, deploy anyway, or replan?", Choices: []factoryd.QuestionChoice{
		{Value: factoryd.AnswerHoldWait, Label: "Wait"},
		{Value: factoryd.AnswerDeployAnyway, Label: "Deploy anyway"},
		{Value: factoryd.AnswerReplanOnTop + ":t-pl", Label: "Replan plan #12 on top"},
		{Value: factoryd.AnswerTakeOffMain + ":t-pl", Label: "Take plan #12 off main and replan"}}}
	return factoryd.BoardTask{Task: factoryd.Task{ID: "t-ask", Kind: "human", Status: "waiting-human", Source: factoryd.SourceProductionHeld,
		Repository: "sf/code", IssueNumber: 77, Objective: "needs you: production is held"}, Escalation: q.Ask, Question: q}
}

func answerAsk(r *rig, ck *http.Cookie, csrf, value, words string) *httptest.ResponseRecorder {
	req := post("/tasks/t-ask/answer", url.Values{"csrf": {csrf}, "value": {value}, "answer": {words}})
	req.Header.Set("HX-Request", "true")
	return r.do(req, ck)
}

// askStatus is the status of the ask t-ask.
func askStatus(r *rig) string {
	r.tasks.mu.Lock()
	defer r.tasks.mu.Unlock()
	for _, bt := range r.tasks.tasks {
		if bt.ID == "t-ask" {
			return bt.Status
		}
	}
	return ""
}

func askRows(ev *fakeEvents, kind string) []factoryd.Event {
	var out []factoryd.Event
	for _, e := range ev.all() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestHoldAskDeployAnyway(t *testing.T) {
	r, ev, deploys, _ := holdAskRig(t)
	ck, s := r.loggedIn(t)
	rec := answerAsk(r, ck, s.csrf, factoryd.AnswerDeployAnyway, "")
	want := "Deploy now: main of " + holdProject + " at 9d9d9d9 goes to production. A merge after it waits for the open plans again."
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("deploy anyway = %d, want 200 with %q: %s", rec.Code, want, rec.Body)
	}
	if len(*deploys) != 1 || (*deploys)[0] != (deployCall{"sf/code", "sebastian", factoryd.ViaButton, "member"}) {
		t.Fatalf("DeployNow calls = %+v, want one for sf/code by sebastian via the button", *deploys)
	}
	rows := askRows(ev, factoryd.EventQuestionAnswered)
	if len(rows) != 1 || rows[0].Detail["value"] != factoryd.AnswerDeployAnyway || rows[0].TaskID != "t-ask" {
		t.Fatalf("question_answered rows = %+v, want one on t-ask with value deploy_anyway", rows)
	}

	r, ev, deploys, _ = holdAskRig(t)
	wck, ws := memberLogin(t, r, "morgan")
	rec = answerAsk(r, wck, ws.csrf, factoryd.AnswerDeployAnyway, "")
	if rec.Code != http.StatusForbidden || len(*deploys) != 0 || len(askRows(ev, factoryd.EventQuestionAnswered)) != 0 {
		t.Errorf("writer = %d with %d calls and %d rows, want 403, 0, 0", rec.Code, len(*deploys), len(askRows(ev, factoryd.EventQuestionAnswered)))
	}

	r, ev, _, _ = holdAskRig(t)
	r.c.cfg.DeployNow = func(context.Context, string, string, string, string) (string, error) { return "", factoryd.ErrNotHeld }
	ck, s = r.loggedIn(t)
	rec = answerAsk(r, ck, s.csrf, factoryd.AnswerDeployAnyway, "")
	want = "Production of " + holdProject + " is not held: every merge goes to production on its own."
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("a free production = %d %q, want 409 %q", rec.Code, rec.Body, want)
	}
	if n := len(askRows(ev, factoryd.EventQuestionAnswered)); n != 0 || askStatus(r) != "waiting-human" {
		t.Errorf("a refused Deploy anyway left %d answers, status %s; want none, waiting-human", n, askStatus(r))
	}
}

func TestHoldAskReplan(t *testing.T) {
	const ask, plan = "/api/v1/repos/sf/code/issues/77/comments", "/api/v1/repos/sf/code/issues/12/comments"
	comment := "Chosen on #77, the ask about held production."
	for _, c := range []struct {
		value, label, note, said string
		offMain                  bool
	}{
		{factoryd.AnswerReplanOnTop + ":t-pl", "Replan plan #12 on top", "Replanning plan #12 on top; #77 is closed. The planner replans on top of what is merged.", "", false},
		{factoryd.AnswerTakeOffMain + ":t-pl", "Take plan #12 off main and replan", "Taking plan #12 off main and replanning; #77 is closed. Card #90 reverts the merged cards, then the planner replans.", "", true},
	} {
		r, ev, _, replans := holdAskRig(t)
		ck, s := r.loggedIn(t)
		rec := answerAsk(r, ck, s.csrf, c.value, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.note) {
			t.Fatalf("%s = %d, want 200 with %q: %s", c.value, rec.Code, c.note, rec.Body)
		}
		if len(*replans) != 1 || (*replans)[0] != (replanCall{"t-pl", "sebastian", comment, c.offMain}) {
			t.Fatalf("%s: Replan calls = %+v, want t-pl by sebastian off main %v", c.value, *replans, c.offMain)
		}
		if got := r.fg.commentBodies(ask); len(got) != 1 || got[0] != c.label {
			t.Errorf("%s: comments on #77 = %q, want the label %q", c.value, got, c.label)
		}
		if got := r.fg.commentBodies(plan); len(got) != 1 || !strings.Contains(got[0], comment) {
			t.Errorf("%s: comments on the plan = %q, want the replan note", c.value, got)
		}
		if n := len(askRows(ev, factoryd.EventCardReplanned)); n != 1 {
			t.Errorf("%s: %d card_replanned rows, want 1", c.value, n)
		}
		if n := len(askRows(ev, factoryd.EventCardClosed)); n != 1 {
			t.Errorf("%s: %d card_closed rows, want 1", c.value, n)
		}
	}

	r, ev, _, _ := holdAskRig(t)
	r.c.cfg.Replan = func(context.Context, string, string, string, bool) (string, error) {
		return "", factoryd.ErrNoReplan("Plan #12 is being replanned: wait for its revision.")
	}
	ck, s := r.loggedIn(t)
	rec := answerAsk(r, ck, s.csrf, factoryd.AnswerReplanOnTop+":t-pl", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Plan #12 is being replanned: wait for its revision.") {
		t.Fatalf("a plan in replan = %d %q, want 409 with its text", rec.Code, rec.Body)
	}
	if askStatus(r) != "waiting-human" || len(askRows(ev, factoryd.EventCardClosed)) != 0 || len(r.fg.commentBodies(ask)) != 0 {
		t.Errorf("a refused replan closed or commented the ask: status %s", askStatus(r))
	}

	r, _, _, replans := holdAskRig(t)
	wck, ws := memberLogin(t, r, "morgan")
	if rec := answerAsk(r, wck, ws.csrf, factoryd.AnswerReplanOnTop+":t-pl", ""); rec.Code != http.StatusForbidden || len(*replans) != 0 {
		t.Errorf("writer = %d with %d replans, want 403, 0", rec.Code, len(*replans))
	}
}

func TestHoldAskWait(t *testing.T) {
	r, ev, deploys, replans := holdAskRig(t)
	ck, s := r.loggedIn(t)
	rec := answerAsk(r, ck, s.csrf, factoryd.AnswerHoldWait, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Closed #77") {
		t.Fatalf("wait = %d, want 200 closing #77: %s", rec.Code, rec.Body)
	}
	if got := r.fg.commentBodies("/api/v1/repos/sf/code/issues/77/comments"); len(got) != 1 || got[0] != "Wait" {
		t.Errorf("comments on #77 = %q, want [Wait]", got)
	}
	if len(*deploys) != 0 || len(*replans) != 0 || len(askRows(ev, factoryd.EventCardClosed)) != 1 {
		t.Errorf("wait: %d deploys, %d replans, %d closes; want 0, 0, 1", len(*deploys), len(*replans), len(askRows(ev, factoryd.EventCardClosed)))
	}

	r, _, _, _ = holdAskRig(t)
	ck, s = r.loggedIn(t)
	rec = answerAsk(r, ck, s.csrf, "", "do whatever")
	want := "Card #77 takes one of its choices: Wait, Deploy anyway, Replan plan #12 on top, Take plan #12 off main and replan."
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
		t.Errorf("typed words = %d %q, want 400 %q", rec.Code, rec.Body, want)
	}
}

func TestHoldAskReplanNamesAFailedClose(t *testing.T) {
	r, ev, _, replans := holdAskRig(t)
	r.fg.failEdit = true
	ck, s := r.loggedIn(t)
	rec := answerAsk(r, ck, s.csrf, factoryd.AnswerReplanOnTop+":t-pl", "")
	want := "Replanning plan #12 on top; #77 could not be closed, close it by hand. The planner replans on top of what is merged."
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("a failed close = %d, want 200 with %q: %s", rec.Code, want, rec.Body)
	}
	if len(*replans) != 1 || len(askRows(ev, factoryd.EventCardClosed)) != 0 || askStatus(r) != "waiting-human" {
		t.Errorf("%d replans, %d closes, status %s; want 1, 0, waiting-human", len(*replans), len(askRows(ev, factoryd.EventCardClosed)), askStatus(r))
	}
}

// The project agent never presses Deploy anyway: the chat's answer on the
// held-production ask refuses it at every autonomy level, run unconfirmed
// at autonomous and before any proposal at manual. Nothing deploys and
// nothing is answered.
func TestHoldAskDeployAnywayRefusedInChat(t *testing.T) {
	refused := `tool {"arguments":{"task_id":"#77","text":"Deploy anyway"},"duration_ms":0,"name":"answer","result":` +
		`"Refused: Deploy anyway is a person's press: use Deploy now in the status panel, or answer card #77 in the console."}`
	for _, level := range []string{policy.AutonomyAutonomous, policy.AutonomyManual} {
		t.Run(level, func(t *testing.T) {
			r, _, g := newChatRig(t)
			r.ledger.files = map[string]policy.ProjectConfig{holdProject: {Repository: "sf/code", Autonomy: level, MergeRule: policy.MergeHuman}}
			deploys := &[]deployCall{}
			r.c.cfg.DeployNow = func(_ context.Context, repository, by, via, decider string) (string, error) {
				*deploys = append(*deploys, deployCall{repository, by, via, decider})
				return "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d", nil
			}
			r.tasks.tasks = append(r.tasks.tasks, holdAsk())
			g.replies = [][]string{{call(1, "answer", `{"task_id":"#77","text":"Deploy anyway"}`)}, {say("Use Deploy now in the status panel.")}}
			ck, s := r.loggedIn(t)
			code, events, _ := chatPost(t, r, ck, s, chatPath, url.Values{"message": {"deploy anyway on the held ask"}})
			equal(t, "code", code, 200)
			if !slices.Contains(events, refused) {
				t.Errorf("the stream lacks the refusal %s: %v", refused, events)
			}
			if slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, "proposal ") }) {
				t.Errorf("the stream proposes the answer: %v", events)
			}
			if len(*deploys) != 0 || len(askRows(r.events, factoryd.EventQuestionAnswered)) != 0 || askStatus(r) != "waiting-human" {
				t.Errorf("DeployNow calls %+v, %d answers, status %s; want none, none, waiting-human",
					*deploys, len(askRows(r.events, factoryd.EventQuestionAnswered)), askStatus(r))
			}
		})
	}
}
