package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// ghosts_wide_lane_once: the one board stands its routines' ghosts in the
// column titled Queued, and a human task that asks renders once, as its
// ask in Needs you with its answers on the opened card; a human task that
// asks nothing stands there once too, asking whether it is done.
func TestProofGhostsAndAskInTheWideLane(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{audit(), soonBump()}
	r.tasks.tasks = []factoryd.BoardTask{
		{Task: factoryd.Task{ID: "t-ask", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 13, Objective: "pick the region"},
			Question: &factoryd.HumanQuestion{Ask: "which region?", Choices: []factoryd.QuestionChoice{{Value: "fsn1", Label: "fsn1"}}}},
		{Task: factoryd.Task{ID: "t-do", Kind: "human", Status: "waiting-human", Repository: "sf/code", IssueNumber: 14, Objective: "rotate the key"}},
	}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	queued := col(t, body, "Queued")
	for _, ghost := range []string{"audit", "weekly-bump"} {
		if !strings.Contains(queued, `data-ghost="ai-software-factory-code/`+ghost+`"`) {
			t.Errorf("Queued holds no ghost of %s:\n%s", ghost, queued)
		}
	}
	if n := strings.Count(body, `data-ghost=`); n != 2 {
		t.Errorf("the board stands %d ghosts, want the two in Queued", n)
	}
	needs := col(t, body, "Needs you")
	for id, ask := range map[string]string{"t-ask": "which region?", "t-do": "Is “rotate the key” done?"} {
		if n := strings.Count(body, `data-card="`+id+`"`); n != 1 {
			t.Errorf("the human task %s renders %d times, want once:\n%s", id, n, body)
		}
		if face := faceOf(t, needs, id); !strings.Contains(face, `<strong class="title">`+ask+`</strong>`) {
			t.Errorf("the human task %s does not ask %q in Needs you:\n%s", id, ask, face)
		}
	}
	if ask := askBlockOf(t, drawer(t, r, ck, "t-ask", "overview")); !strings.Contains(ask, `<button type="submit" class="pick">fsn1</button>`) {
		t.Errorf("the asking human task's opened card lacks its answer:\n%s", ask)
	}
}
