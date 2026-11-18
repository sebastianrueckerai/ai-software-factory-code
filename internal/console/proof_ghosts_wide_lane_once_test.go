package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// ghosts_wide_lane_once: a wide board stands its routines' ghosts in the
// column titled Queued, and a human task that asks renders once, as its
// escalation with its actions, never again among the human tasks, where a
// human task that asks nothing stays.
func TestProofGhostsAndAskInTheWideLane(t *testing.T) {
	r := newRig(t)
	r.openCases = map[string][]factoryd.Case{}
	r.routines.views = []factoryd.RoutineView{audit(), soonBump()}
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-ask", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "pick the region"},
			Question: &factoryd.HumanQuestion{Ask: "which region?", Choices: []factoryd.QuestionChoice{{Value: "fsn1", Label: "fsn1"}}}},
		{Task: factoryd.Task{ID: "t-do", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14, Objective: "rotate the key"}},
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard+"&view=wide", nil), ck).Body.String()
	queued := col(t, body, "Queued")
	for _, ghost := range []string{"audit", "weekly-bump"} {
		if !strings.Contains(queued, `data-ghost="ai-software-factory-code/`+ghost+`"`) {
			t.Errorf("the wide lane's Queued holds no ghost of %s:\n%s", ghost, queued)
		}
	}
	if n := strings.Count(body, `data-ghost=`); n != 2 {
		t.Errorf("the wide board stands %d ghosts, want the two in Queued", n)
	}
	if n := strings.Count(body, `id="need-t-ask"`); n != 1 {
		t.Errorf("the asking human task renders %d times, want once:\n%s", n, body)
	}
	escalations, humans := col(t, body, "Escalations"), col(t, body, "Human tasks")
	if !strings.Contains(escalations, `data-card="t-ask"`) || !strings.Contains(escalations, `>fsn1</button>`) {
		t.Errorf("the asking human task is not its escalation with its actions:\n%s", escalations)
	}
	if strings.Contains(humans, `data-card="t-ask"`) || !strings.Contains(humans, `data-card="t-do"`) {
		t.Errorf("Human tasks holds the asking task, or not the task that asks nothing:\n%s", humans)
	}
}
