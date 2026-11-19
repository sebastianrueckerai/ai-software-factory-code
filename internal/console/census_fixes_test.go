package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// The census fixes (the one-face plan, task 8): every card kind's face
// exact, judged against the five violations the other tasks do not cover.

// A case that asks again renders no processed-at text: the case's
// state carries the confirmation, neither the small card nor the opened
// card carries a confirmation line, and the opened card keeps the ask's
// info.
func TestCensusAnsweredCaseFaceHasNoProcessedLine(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{"info": "Postgres is shared."}}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	opened := getAs(r, ck, "/cards/t-a/drawer?tab=overview&case=t-a:3", false).Body.String()
	if strings.Contains(needs, "Last answer processed") || strings.Contains(opened, "Last answer processed") {
		t.Errorf("the answered case's face carries a processed-at line:\n%s\n%s", needs, opened)
	}
	if !strings.Contains(opened, "Postgres is shared.") {
		t.Errorf("the opened card lost the ask's info:\n%s", opened)
	}
}

// The gate artifact folds: the criterion row holds no bare pre, its
// details' summary names the artifact and its size in KiB.
func TestCensusGateArtifactFolds(t *testing.T) {
	r := newRig(t)
	humanRig(r)
	ck, _ := r.loggedIn(t)
	opened := drawer(t, r, ck, "t-int", "overview")
	const fold = `<details class="fold"><summary>pages_ok.txt at 8e3a1b0, 1 KiB</summary>`
	if !strings.Contains(opened, fold) {
		t.Fatalf("the artifact's fold or its summary is missing:\n%s", opened)
	}
	artifact := opened[strings.Index(opened, fold):]
	artifact = artifact[:strings.Index(artifact, "</details>")]
	if !strings.Contains(artifact, `<pre class="artifact" tabindex="0">sizes: 3 rows`) {
		t.Errorf("the fold holds no artifact:\n%s", artifact)
	}
	i := strings.Index(opened, `<pre class="artifact"`)
	if i > 0 && !strings.Contains(opened[:i], fold) {
		t.Errorf("a bare artifact pre rides the card before the fold:\n%s", opened[max(0, i-300):i+100])
	}
}

// A plan case without revision and index facts, keyed on its revision as
// every plan case is, still renders one question per face: no face holds
// every open question at once.
func TestCensusSlotlessPlanCaseRendersOneQuestion(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
		{Text: "Which store keeps the snapshots?", Choices: []string{"Postgres", "S3"}},
	}}
	r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 5, TaskID: "t-plan", Kind: factoryd.CasePlanQuestion, StateKey: "4c1d2e0f9a8b7c6d@r2",
		Status: factoryd.CaseAtHuman, Problem: "The plan's question."}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if n := strings.Count(body, "How long are the results kept?") + strings.Count(body, "Which store keeps the snapshots?"); n != 1 {
		t.Errorf("the board holds %d asks of the slotless plan case, want the one first open question:\n%s", n, body)
	}
	ask := askBlockOf(t, planDrawer(t, r, ck))
	if strings.Count(ask, `<p class="o-question">`) != 1 || !strings.Contains(ask, `<p class="o-question">How long are the results kept?</p>`) ||
		strings.Contains(ask, "Which store keeps the snapshots?") || strings.Contains(ask, "Postgres") {
		t.Errorf("the opened card does not ask the one first open question:\n%s", ask)
	}
}

// The extend form rides no asking face: the opened card alone holds it,
// once.
func TestCensusAskingFaceHoldsNoExtendInputs(t *testing.T) {
	r := newRig(t)
	r.openCases = map[string][]factoryd.Case{}
	r.tasks.set("the session cap is reached: every finding sent to a human returns unacted",
		factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if face := faceOf(t, body, "t-q"); strings.Contains(face, "extend") || strings.Contains(face, "<input") {
		t.Errorf("the small card holds the extend form:\n%s", face)
	}
	opened := drawer(t, r, ck, "t-q", "overview")
	if more := otherActionsOf(t, opened); strings.Count(opened, `data-answer="extend"`) != 1 || !strings.Contains(more, `data-answer="extend"`) ||
		answerForm(t, opened, factoryd.ActExtend).Action != "/tasks/t-q/resolve" {
		t.Errorf("the opened card's other actions do not hold the one extend form:\n%s", opened)
	}
}

// The board holds each card once, ghosts included: a human task that asks
// renders once, as its ask with its answers on its opened card, and Queued
// carries the routines' ghosts.
func TestCensusWideLaneHoldsEachCardOnceGhostsIncluded(t *testing.T) {
	r := newRig(t)
	r.routines.views = []factoryd.RoutineView{audit()}
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-e", Kind: "human", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "asks"},
		Question: &factoryd.HumanQuestion{Ask: "which region?", Choices: []factoryd.QuestionChoice{{Value: "fsn1", Label: "fsn1"}}}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, codeBoard, nil), ck).Body.String()
	if n := strings.Count(body, `data-card="t-e"`); n != 1 {
		t.Errorf("the board renders the asking human task %d times, want once:\n%s", n, body)
	}
	if face := faceOf(t, col(t, body, "Needs you"), "t-e"); !strings.Contains(face, `<strong class="title">which region?</strong>`) {
		t.Errorf("the asking human task's one render is not its ask in Needs you:\n%s", face)
	}
	if ask := escalationOf(t, r, ck, "t-e"); !strings.Contains(ask, `<button type="submit" class="pick">fsn1</button>`) {
		t.Errorf("the asking human task's opened card lacks its answer:\n%s", ask)
	}
	queued := col(t, body, "Queued")
	if !strings.Contains(queued, `class="card face ghost"`) || !strings.Contains(queued, `data-ghost="ai-software-factory-code/audit"`) {
		t.Errorf("Queued holds no ghosts:\n%s", queued)
	}
}
