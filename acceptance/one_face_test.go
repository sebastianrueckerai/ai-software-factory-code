package acceptance

// One face every asking kind (plan #1850, criterion
// one_face_every_asking_kind_suite): each asking face the board holds —
// a merge request's, a resource request's, an escalation's ask, a plan's
// question — renders exactly one decide block in the five-part shape,
// built from the shared case-decide partial with the table's offers as
// buttons, exactly one fund control and one note box, and its one send
// lands the chosen answer, the fund beside it and the note in one
// request. The merge and resource faces seat their board fixtures, as
// the internal pinning loop does (face_test.go), so their ask and why
// assert what approvalDecide builds from the request's own spec, never
// the case's empty problem or its kind's placeholder.

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
	// the escalation's ask on the standing waiting card, and a plan's
	// question at the human on a plan card, as caseFromAsk opens them
	r.mu.Lock()
	r.caseNext++
	escalationID := r.caseNext
	r.cases["t-e"] = []factoryd.Case{{ID: escalationID, TaskID: "t-e", Repository: "sf/code",
		Kind: factoryd.CaseAgentEscalate, Status: factoryd.CaseAtHuman, Problem: "Use a replica for the reads?",
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
	body := r.request(http.MethodGet, "/", nil, ck).Body.String()
	faceOf := func(id string) string {
		t.Helper()
		at := strings.Index(body, `data-card="`+id+`"`)
		if at < 0 {
			t.Fatalf("the board holds no face of %s:\n%s", id, body)
		}
		start := strings.LastIndex(body[:at], "<article")
		return body[start : start+strings.Index(body[start:], "</article>")]
	}
	// the one decide of each asking face: one send (the form's one
	// action), one fund control, one note box — the shared partial, never
	// a second form beside it
	for _, tc := range []struct {
		id, action string
	}{
		{mfx.Card.TaskID, "/approvals/41/approve"},
		{rfx.Card.TaskID, "/approvals/42/approve"},
		{"t-e", "/tasks/t-e/resolve"},
		{"t-bp", "/tasks/t-bp/plan/answer"},
	} {
		face := faceOf(tc.id)
		for what, want := range map[string]int{
			`name="fund_eur"`:            1,
			`<textarea`:                  1,
			`action="` + tc.action + `"`: 1,
		} {
			if got := strings.Count(face, what); got != want {
				t.Errorf("%s: the face holds %s %d times, want %d:\n%s", tc.id, what, got, want, face)
			}
		}
		if strings.Count(face, `class="decide`) != 1 {
			t.Errorf("%s: the face holds more than one decide block:\n%s", tc.id, face)
		}
	}
	// the approval faces carry their own five parts — the ask with its
	// numbers and the why the request's spec builds — never the case's
	// empty problem or its kind's placeholder
	for _, fx := range []factoryd.Fixture{mfx, rfx} {
		face := faceOf(fx.Card.TaskID)
		for _, want := range []string{
			`<p class="ask">` + fx.Asks[0].Ask + `</p>`,
			`<p class="why">` + fx.Asks[0].Why + `</p>`,
		} {
			if !strings.Contains(face, want) {
				t.Errorf("%s: the face lacks %q:\n%s", fx.Card.TaskID, want, face)
			}
		}
		if strings.Contains(face, `<p class="ask"></p>`) || strings.Contains(face, "The case is the") {
			t.Errorf("%s: the face still shows the empty ask or the placeholder:\n%s", fx.Card.TaskID, face)
		}
	}
	// the escalation's ask and the plan's question keep theirs: the case's
	// own words, the plan's one line, the register's answers as buttons
	if face := faceOf("t-e"); !strings.Contains(face, `<p class="ask">Use a replica for the reads?</p>`) ||
		!strings.Contains(face, `<p class="why">The agent stopped because it needs a decision only a human can make.</p>`) ||
		!strings.Contains(face, `<span class="effect">The card stays as it is.</span>`) {
		t.Errorf("the escalation face lacks its ask, why or offers:\n%s", face)
	}
	if face := faceOf("t-bp"); !strings.Contains(face, `<p class="ask">Which cache holds the reads?</p>`) ||
		!strings.Contains(face, `<p class="why">Plan #531, revision 2: 1 question open;`) ||
		!strings.Contains(face, `name="choice" value="1" checked`) || !strings.Contains(face, `name="choice" value="2"`) ||
		!strings.Contains(face, `<p class="recommendation">Recommendation (the planner): it already holds the board keys</p>`) {
		t.Errorf("the plan face lacks its ask, why, choices or the seated recommendation:\n%s", face)
	}

	// each face's one send: the chosen answer, the fund and the note ride
	// one POST and land as one row on the card (the escalation's fund its
	// own draw row, as the store lands it)
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
	send("/approvals/41/approve", url.Values{"action": {"approve"}, "fund_eur": {"1.50"}, "note": {"the shard is sound"}})
	if q := r.Approval(41); q.Status != "approved" {
		t.Errorf("the merge request is %s, want approved", q.Status)
	}
	if got, want := budget(mfx.Card.TaskID), mfx.Card.BudgetEUR+1.5; got != want {
		t.Errorf("the merge card's budget is %v, want %v raised by the fund", got, want)
	}
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventMergeApproved || rs[0].TaskID != mfx.Card.TaskID ||
		fmt.Sprint(rs[0].Detail["fund_eur"]) != "1.5" || fmt.Sprint(rs[0].Detail["reason"]) != "the shard is sound" {
		t.Errorf("the merge send landed %v, want the one approval row with the fund and the note", rs)
	}

	before = len(r.Events())
	send("/approvals/42/approve", url.Values{"action": {"approve"}, "fund_eur": {"1.50"}, "note": {"the lease is worth it"}})
	if q := r.Approval(42); q.Status != "approved" {
		t.Errorf("the resource request is %s, want approved", q.Status)
	}
	if got, want := budget(rfx.Card.TaskID), rfx.Card.BudgetEUR+1.5; got != want {
		t.Errorf("the resource card's budget is %v, want %v raised by the fund", got, want)
	}
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventMergeApproved || rs[0].TaskID != rfx.Card.TaskID ||
		fmt.Sprint(rs[0].Detail["fund_eur"]) != "1.5" || fmt.Sprint(rs[0].Detail["reason"]) != "the lease is worth it" {
		t.Errorf("the resource send landed %v, want the one approval row with the fund and the note", rs)
	}

	before = len(r.Events())
	send("/tasks/t-e/resolve", url.Values{"case_id": {fmt.Sprint(escalationID)}, "action": {factoryd.ActContinue},
		"fund_eur": {"1.50"}, "note": {"the replica holds"}})
	var funded, answered int
	for _, e := range rows(before) {
		switch {
		case e.Kind == factoryd.EventCardFunded && fmt.Sprint(e.Detail["amount_eur"]) == "1.5":
			funded++
		case e.Kind == factoryd.EventQuestionAnswered && e.TaskID == "t-e" && fmt.Sprint(e.Detail["answer"]) == "the replica holds":
			answered++
		}
	}
	if funded != 1 || answered != 1 || budget("t-e") != 1.5 {
		t.Errorf("the escalation send landed %d fund rows and %d answer rows at budget %v, want one of each at 1.50",
			funded, answered, budget("t-e"))
	}

	before = len(r.Events())
	// the press of the recommended button: the send's guard reads the
	// seated question and answers with its choice's own words
	send("/tasks/t-bp/plan/answer", url.Values{"revision": {"2"}, "index": {"1"}, "choice": {"1"},
		"fund_eur": {"1.50"}, "note": {"worth it"}})
	if rs := rows(before); len(rs) != 1 || rs[0].Kind != factoryd.EventPlanQuestionAnswered || rs[0].TaskID != "t-bp" ||
		fmt.Sprint(rs[0].Detail["fund_eur"]) != "1.5" ||
		fmt.Sprint(rs[0].Detail["answer"]) != "Redis — worth it" {
		t.Errorf("the plan send landed %v, want the one answer row with the fund and the note", rs)
	}
	if got := budget("t-bp"); got != 1.5 {
		t.Errorf("the plan card's budget is %v, want 1.50 raised by the fund", got)
	}
}
