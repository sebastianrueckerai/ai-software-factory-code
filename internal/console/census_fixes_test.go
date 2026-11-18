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

// A case answered and re-tripped renders no processed-at text: the case's
// state carries the confirmation, the face carries no confirmation line.
func TestCensusAnsweredCaseFaceHasNoProcessedLine(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{"info": "Postgres is shared."}}}
	r.caseAnswered = map[string]time.Time{"t-a": time.Date(2026, 11, 1, 21, 40, 0, 0, time.UTC)}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	if strings.Contains(needs, "Last answer processed") {
		t.Errorf("the answered case's face carries a processed-at line:\n%s", needs)
	}
	if !strings.Contains(needs, "Postgres is shared.") {
		t.Errorf("the face lost the ask's info:\n%s", needs)
	}
}

// The gate artifact folds: the criterion row holds no bare pre, its
// details' summary names the artifact and its size in KiB.
func TestCensusGateArtifactFolds(t *testing.T) {
	r := newRig(t)
	humanRig(r)
	ck, _ := r.loggedIn(t)
	column := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=ai-software-factory-code", nil), ck).Body.String(), "Needs you")
	const fold = `<details class="fold"><summary>pages_ok.txt at 8e3a1b0, 1 KiB</summary>`
	if !strings.Contains(column, fold) {
		t.Fatalf("the artifact's fold or its summary is missing:\n%s", column)
	}
	artifact := column[strings.Index(column, fold):]
	artifact = artifact[:strings.Index(artifact, "</details>")]
	if !strings.Contains(artifact, `<pre class="artifact" tabindex="0">sizes: 3 rows`) {
		t.Errorf("the fold holds no artifact:\n%s", artifact)
	}
	i := strings.Index(column, `<pre class="artifact"`)
	if i > 0 && !strings.Contains(column[:i], fold) {
		t.Errorf("a bare artifact pre rides the face before the fold:\n%s", column[max(0, i-300):i+100])
	}
}

// A plan case without revision and index facts still renders one question
// per face: no face holds every open question at once.
func TestCensusSlotlessPlanCaseRendersOneQuestion(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
		{Text: "Which store keeps the snapshots?", Choices: []string{"Postgres", "S3"}},
	}}
	r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 5, TaskID: "t-plan", Kind: factoryd.CasePlanQuestion,
		Status: factoryd.CaseAtHuman, Problem: "The plan's question."}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort&view=wide", nil), ck).Body.String()
	if n := strings.Count(body, `class="decide plan-answer"`); n != 1 {
		t.Errorf("the slotless plan face holds %d decide forms, want one question per face:\n%s", n, body)
	}
	if strings.Count(body, "How long are the results kept?")+strings.Count(body, "Which store keeps the snapshots?") > 2 {
		t.Errorf("the face holds more than the one ask:\n%s", body)
	}
}

// The extend form rides no asking face: the drawer's extend fold alone
// holds it.
func TestCensusAskingFaceHoldsNoExtendInputs(t *testing.T) {
	r := newRig(t)
	r.openCases = map[string][]factoryd.Case{}
	r.tasks.set("the session cap is reached: every finding sent to a human returns unacted",
		factoryd.Task{ID: "t-q", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 15, Objective: "add the feed"})
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String()
	if esc := escalationOf(t, body, "t-q"); strings.Contains(esc, "/extend") || strings.Contains(esc, `id="extend-`) {
		t.Errorf("the asking face holds the extend form:\n%s", esc)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-q/drawer", nil), ck).Body.String()
	i := strings.Index(drawer, `<details class="sec" data-sec="extend">`)
	if i < 0 || strings.Count(drawer, `action="/tasks/t-q/extend"`) != 1 || !strings.Contains(drawer[i:], `action="/tasks/t-q/extend"`) {
		t.Errorf("the drawer's extend fold does not hold the one extend form:\n%s", drawer)
	}
}

// The wide lane holds each card once, ghosts included: a human task that
// asks renders through its escalation alone, once with its actions, and
// the queue group carries the routines' ghosts.
func TestCensusWideLaneHoldsEachCardOnceGhostsIncluded(t *testing.T) {
	r := newRig(t)
	r.openCases = map[string][]factoryd.Case{}
	r.routines.views = []factoryd.RoutineView{audit()}
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-e", Kind: "human", Status: "waiting-human",
		Repository: "sf/code", IssueNumber: 13, Objective: "asks"},
		Question: &factoryd.HumanQuestion{Ask: "which region?", Choices: []factoryd.QuestionChoice{{Value: "fsn1", Label: "fsn1"}}}}}
	ck, _ := r.loggedIn(t)
	body := r.do(httptest.NewRequest(http.MethodGet, "/?view=wide", nil), ck).Body.String()
	if n := strings.Count(body, `id="need-t-e"`); n != 1 {
		t.Errorf("the wide lane renders the asking human task %d times, want once:\n%s", n, body)
	}
	if strings.Contains(col(t, body, "Human tasks"), `data-card="t-e"`) {
		t.Errorf("the wide lane holds the asking human task twice, in Human tasks and Escalations:\n%s", body)
	}
	escalations := col(t, body, "Escalations")
	if !strings.Contains(escalations, `data-card="t-e"`) || !strings.Contains(escalations, `>fsn1</button>`) {
		t.Errorf("the asking human task's one render is not its escalation with its actions:\n%s", escalations)
	}
	queued := col(t, body, "Queued")
	if !strings.Contains(queued, `class="card ghost`) || !strings.Contains(queued, `data-ghost="ai-software-factory-code/audit"`) {
		t.Errorf("the wide lane's queue group holds no ghosts:\n%s", queued)
	}
}
