package acceptance

// One face every asking kind (plan #1850, criterion
// one_face_every_asking_kind_suite): each asking card's opened card — a
// merge request's, a resource request's, an escalation's ask, a plan's
// question — renders exactly one ask block: the register's question and
// why, built from the request's own spec, never the case's empty problem
// or its kind's placeholder, and the register's answers, each its own
// form with its own fields and no fund beside it. One answer's send lands
// that answer and its words in one request. The merge and resource faces
// seat their board fixtures, as the internal pinning loop does
// (face_test.go).

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
	"github.com/sebastianrueckerai/ai-software-factory-code/internal/policy"
)

func TestOneFaceEveryAskingKind(t *testing.T) {
	r, _, _ := NewAcceptanceRig(t, false, nil)
	// the two approval fixtures: each one's card on the board with its
	// request pending, the request's spec the fixture case's facts — the
	// merge card in review while its request pends, the resource card
	// waiting on the human with its exported case at the human beside the
	// request riding its card
	seat := func(name string, id int64) factoryd.Fixture {
		t.Helper()
		fx, err := factoryd.LoadFixture(filepath.Join("..", "internal", "factoryd", "testdata", "fixtures", name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		c := fx.Card
		status := "in-review"
		if fx.Asks[0].CaseKind == factoryd.ApprovalResource {
			status = "waiting-human"
		}
		spec, err := json.Marshal(fx.Cases[0].Facts)
		if err != nil {
			t.Fatalf("marshal %s's spec: %v", name, err)
		}
		q := factoryd.ApprovalRequest{ID: id, Kind: fx.Asks[0].CaseKind, TaskID: c.TaskID, Project: c.Repository,
			Status: "pending", Spec: spec, CreatedAt: time.Now().Add(-time.Minute)}
		if q.Kind == factoryd.ApprovalMerge {
			q.PRNumber = 31
		}
		if m, ok := fx.Cases[0].Facts["monthly_eur"].(float64); ok {
			q.MonthlyEUR = &m
		}
		task := factoryd.Task{ID: c.TaskID, Kind: cmp.Or(c.Kind, "agent"), Track: cmp.Or(c.Track, policy.TrackDirect),
			Status: status, Repository: c.Repository, IssueNumber: c.Issue, Objective: c.Objective,
			BudgetEUR: c.BudgetEUR, CreatedAt: time.Now().Add(-time.Minute)}
		r.mu.Lock()
		r.tasks = append(r.tasks, factoryd.BoardTask{Task: task})
		r.approvals = append(r.approvals, q)
		if q.Kind == factoryd.ApprovalResource {
			r.caseNext++
			r.cases[c.TaskID] = []factoryd.Case{{ID: r.caseNext, TaskID: c.TaskID, Repository: c.Repository,
				Kind: factoryd.CaseKind(fx.Cases[0].Kind), Status: factoryd.CaseAtHuman,
				Problem: fx.Cases[0].Problem, Facts: fx.Cases[0].Facts, OpenedAt: time.Now().Add(-70 * time.Second)}}
		}
		r.mu.Unlock()
		return fx
	}
	mfx := seat("board-merge.json", 41)
	rfx := seat("board-resource.json", 42)
	// the escalation's ask on the standing waiting card, with the room a
	// session needs as the Runner's facts carry it, and a plan's question
	// at the human on a plan card, as caseFromAsk opens them
	room, err := json.Marshal(factoryd.CaseFacts{Kind: factoryd.CaseAgentEscalate, SessionCap: 3, AgentTimeCapS: 3600, BudgetEUR: 2, LeftEUR: 2,
		FloorEUR: 0.3, CardDrawLeft: policy.Draw{Sessions: 2, AgentTime: time.Hour, EUR: 2}, PoolLeft: policy.Draw{Sessions: 30, AgentTime: 30 * time.Hour, EUR: 60}})
	if err != nil {
		t.Fatal(err)
	}
	var facts map[string]any
	if err := json.Unmarshal(room, &facts); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.caseNext++
	escalationID := r.caseNext
	r.cases["t-e"] = []factoryd.Case{{ID: escalationID, TaskID: "t-e", Repository: "sf/code",
		Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman, Problem: "Use a replica for the reads?", Facts: facts,
		OpenedAt: time.Now().Add(-90 * time.Second)}}
	r.caseNext++
	planID := r.caseNext
	r.tasks = append(r.tasks, factoryd.BoardTask{Task: factoryd.Task{ID: "t-bp", Kind: "agent", Status: "waiting-human",
		Track: policy.TrackPlan, Repository: "sf/code", IssueNumber: 531, Objective: "Plan the board's cache"}})
	r.cases["t-bp"] = []factoryd.Case{{ID: planID, TaskID: "t-bp", Repository: "sf/code",
		Kind: factoryd.CasePlanQuestion, Status: factoryd.CaseAtHuman, Problem: "Which cache holds the reads?",
		Facts: map[string]any{"revision": float64(2), "index": float64(1), "choices": []any{"Redis", "In memory"}, "recommended": "Redis",
			"grounds": "it already holds the board keys"},
		OpenedAt: time.Now().Add(-80 * time.Second)}}
	// the plan's own seatings, as the plan store and the ask row write
	// them in production: the revision's table the slot's planTable
	// reads (plans), the question with its choices the send's choice
	// guard reads (questions), and the ask on the case (caseAsks); the
	// planner's recommendation and grounds are the case's facts
	plan, err := json.Marshal(factoryd.Plan{Summary: "Round one lands the cache; round two the reads."})
	if err != nil {
		t.Fatalf("marshal the plan's table: %v", err)
	}
	r.plans["t-bp"] = []factoryd.PlanRevision{{TaskID: "t-bp", Revision: 2, Plan: plan, SHA256: "sha-531-2",
		CreatedAt: time.Now().Add(-2 * time.Hour)}}
	r.questions["t-bp 2"] = []factoryd.PlanQuestion{{Text: "Which cache holds the reads?",
		Choices: []string{"Redis", "In memory"}, Recommended: "Redis", Grounds: "one standing cost buys both"}}
	r.caseAsks[planID] = factoryd.CaseAsk{At: time.Now().Add(-80 * time.Second), Detail: map[string]any{}}
	r.mu.Unlock()

	ck, csrf := r.Login(t)
	// each asking card decides on its opened card: its case's ask (a merge
	// request's case, as the store opens it), else the request it waits on
	askOf := func(id string) string {
		t.Helper()
		opened := r.request(http.MethodGet, "/cards/"+id+"/drawer?tab=overview", nil, ck).Body.String()
		at := strings.Index(opened, `<section class="o-ask"`)
		if at < 0 {
			t.Fatalf("the opened card of %s holds no ask:\n%s", id, opened)
		}
		if strings.Count(opened, `<section class="o-ask"`) != 1 {
			t.Errorf("the opened card of %s holds more than one ask:\n%s", id, opened)
		}
		return opened[at : at+strings.Index(opened[at:], "</section>")]
	}
	// each answer its own send: no fund control, at most one note box a
	// form, and the door the kind answers through
	for _, tc := range []struct {
		id, action string
	}{
		{mfx.Card.TaskID, "/tasks/" + mfx.Card.TaskID + "/resolve"},
		{rfx.Card.TaskID, "/approvals/42/approve"},
		{"t-e", "/tasks/t-e/resolve"},
		{"t-bp", "/tasks/t-bp/plan/answer"},
	} {
		ask := askOf(tc.id)
		if strings.Contains(ask, `name="fund_eur"`) || !strings.Contains(ask, `action="`+tc.action+`"`) {
			t.Errorf("%s: the ask holds a fund or no answer through %s:\n%s", tc.id, tc.action, ask)
		}
		for _, form := range strings.Split(ask, "<form ")[1:] {
			if strings.Count(form, "<textarea") > 1 || strings.Count(form, `type="submit"`) != 1 {
				t.Errorf("%s: an answer is no send of its own:\n%s", tc.id, form)
			}
		}
	}
	for id, want := range map[string][]string{
		mfx.Card.TaskID: {`<p class="o-question">Merge this card&#39;s pull request?</p>`,
			`<p class="o-askwhy">The factory&#39;s rules let only a human merge this change.</p>`},
		rfx.Card.TaskID: {`<p class="o-question">Rent a cx33 server for 5 hours at about 0.07 EUR?</p>`,
			`<p class="o-askwhy">The card says what the server is for: load-test the feed cache.</p>`},
		"t-e": {`<p class="o-question">Use a replica for the reads?</p>`,
			`<p class="o-askwhy">The agent stopped because it needs a decision only a human can make.</p>`,
			`data-press="continue/own_words">Answer in my words</button>`, `<span class="effect">The card keeps waiting for your answer.</span>`},
		"t-bp": {`<p class="o-question">Which cache holds the reads?</p>`,
			`<p class="o-askwhy">The planner needs your answer before it can finish the plan.</p>`,
			`<p class="o-by">suggested by the planner: <span class="quote">it already holds the board keys</span></p>`,
			`<input type="hidden" name="choice" value="1">`, `<input type="hidden" name="choice" value="2">`},
	} {
		ask := askOf(id)
		for _, w := range want {
			if !strings.Contains(ask, w) {
				t.Errorf("%s: the ask lacks %q:\n%s", id, w, ask)
			}
		}
		if strings.Contains(ask, `<p class="o-question"></p>`) || strings.Contains(ask, "The case is the") {
			t.Errorf("%s: the ask shows the empty question or the placeholder:\n%s", id, ask)
		}
	}

	// each send lands its one answer as one row on the card, and no fund
	send := func(path string, form url.Values) {
		t.Helper()
		form.Set("csrf", csrf)
		form.Set("project", accProjectName)
		if rec := r.Post(path, ck, form); rec.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body)
		}
	}
	budget := func(id string) float64 {
		t.Helper()
		k, ok, err := r.Task(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("the card %s: %v, %v", id, ok, err)
		}
		return k.BudgetEUR
	}
	rows := func(before int) []factoryd.Event { return r.Events()[before:] }

	before := len(r.Events())
	send("/tasks/"+mfx.Card.TaskID+"/resolve", url.Values{"case_id": {"7041"}, "action": {factoryd.ActApproveMerge}})
	if q := r.Approval(41); q.Status != "approved" {
		t.Errorf("the merge request is %s, want approved", q.Status)
	}
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventMergeApproved || rs[0].TaskID != mfx.Card.TaskID || budget(mfx.Card.TaskID) != mfx.Card.BudgetEUR {
		t.Errorf("the merge send landed %v, want the one approval row and the budget as it was", rs)
	}

	before = len(r.Events())
	send("/approvals/42/approve", url.Values{"action": {"approve"}})
	if q := r.Approval(42); q.Status != "approved" {
		t.Errorf("the resource request is %s, want approved", q.Status)
	}
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventMergeApproved || rs[0].TaskID != rfx.Card.TaskID || budget(rfx.Card.TaskID) != rfx.Card.BudgetEUR {
		t.Errorf("the resource send landed %v, want the one approval row and the budget as it was", rs)
	}

	before = len(r.Events())
	own := factoryd.ActContinue + "/own_words"
	send("/tasks/t-e/resolve", url.Values{"case_id": {fmt.Sprint(escalationID)}, "action": {own}, own + ".note": {"the replica holds"}})
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventQuestionAnswered || rs[0].TaskID != "t-e" ||
		fmt.Sprint(rs[0].Detail["answer"]) != "the replica holds" || budget("t-e") != 0 {
		t.Errorf("the escalation send landed %v at budget %v, want the one answer row and no fund", rs, budget("t-e"))
	}

	before = len(r.Events())
	// the press of the suggested answer: the send's guard reads the seated
	// question and answers with its choice's own words
	send("/tasks/t-bp/plan/answer", url.Values{"revision": {"2"}, "index": {"1"}, "choice": {"1"}})
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventPlanQuestionAnswered || rs[0].TaskID != "t-bp" ||
		fmt.Sprint(rs[0].Detail["answer"]) != "Redis" || budget("t-bp") != 0 {
		t.Errorf("the plan send landed %v at budget %v, want the one answer row and no fund", rs, budget("t-bp"))
	}
}
