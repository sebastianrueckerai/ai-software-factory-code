package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

const casesProject = "ai-software-factory-code"

func seatCases(r *rig) {
	r.tasks.set("the agent asks?", factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Card A"},
		factoryd.Task{ID: "t-b", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 22, Objective: "Card B"})
	opened := time.Now().Add(-3 * time.Hour)
	r.openCases = map[string][]factoryd.Case{
		"t-a": {
			{ID: 1, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "ask one", OpenedAt: opened},
			{ID: 2, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "ask two", OpenedAt: opened.Add(time.Hour)},
			{ID: 3, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "ask three", OpenedAt: opened.Add(2 * time.Hour)},
			{ID: 9, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseOpen, Problem: "still with the ladder", OpenedAt: opened},
		},
		"t-b": {{ID: 4, TaskID: "t-b", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "ask b", OpenedAt: opened.Add(30 * time.Minute)}},
	}
}

func getAs(r *rig, ck *http.Cookie, path string, htmx bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	return r.do(req, ck)
}

// A card that holds cases at the human is one row of Needs you, oldest
// case first, showing its newest case with its place among them; the
// carousel appears for more than one case, and its arrows fetch the
// neighbour.
func TestNeedsYouShowsOneRowPerCardAndACarouselForMany(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	needs := col(t, getAs(r, ck, "/", false).Body.String(), "Needs you")
	if n := strings.Count(needs, `class="card case"`); n != 2 {
		t.Fatalf("Needs you holds %d case rows, want one per card (2):\n%s", n, needs)
	}
	a, b := strings.Index(needs, `data-card="t-a"`), strings.Index(needs, `data-card="t-b"`)
	if a < 0 || b < 0 || a > b {
		t.Errorf("t-a (oldest case 3 h) at %d, t-b (30 min) at %d; want the oldest first", a, b)
	}
	rowA := needs[a:b]
	for _, want := range []string{`<p class="ask">ask three</p>`, `<span class="pos">1 of 3</span>`,
		`aria-label="Previous case" disabled`, `aria-label="Next case" hx-get="/projects/` + casesProject + `/cards/t-a/cases/2"`} {
		if !strings.Contains(rowA, want) {
			t.Errorf("t-a's row lacks %q:\n%s", want, rowA)
		}
	}
	if strings.Contains(rowA, "ask one") || strings.Contains(rowA, "still with the ladder") {
		t.Errorf("t-a's row shows another case than its newest at the human:\n%s", rowA)
	}
	if rowB := needs[b:]; !strings.Contains(rowB, `<p class="ask">ask b</p>`) || strings.Contains(rowB, `class="carousel"`) || strings.Contains(rowB, `class="pos"`) {
		t.Errorf("t-b's single case shows a count or arrows:\n%s", rowB)
	}

	next := getAs(r, ck, "/projects/"+casesProject+"/cards/t-a/cases/2", true)
	if next.Code != http.StatusOK {
		t.Fatalf("case 2 = %d %s", next.Code, next.Body)
	}
	for _, want := range []string{`<p class="ask">ask two</p>`, `<span class="pos">2 of 3</span>`,
		`aria-label="Previous case" hx-get="/projects/` + casesProject + `/cards/t-a/cases/1"`, `aria-label="Next case" hx-get="/projects/` + casesProject + `/cards/t-a/cases/3"`} {
		if !strings.Contains(next.Body.String(), want) {
			t.Errorf("case 2 lacks %q:\n%s", want, next.Body)
		}
	}
	if strings.Contains(next.Body.String(), "<html") {
		t.Errorf("case 2 is not the card's body alone:\n%s", next.Body)
	}
	for _, path := range []string{"/cases/4", "/cases/0", "/cases/x"} {
		if rec := getAs(r, ck, "/projects/"+casesProject+"/cards/t-a"+path, true); rec.Code != http.StatusNotFound {
			t.Errorf("t-a%s = %d, want 404", path, rec.Code)
		}
	}
	if rec := getAs(r, ck, "/projects/"+casesProject+"/cards/t-zz/cases/1", true); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown card's case = %d, want 404", rec.Code)
	}
}

// Every text the face reads from a step is scrubbed: URL userinfo and a
// bearer value come out masked.
func TestCaseFaceMasksWhatItsStepsCarry(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseSteps = map[int64][]factoryd.CaseStep{3: {{CaseID: 3, Rung: 1, Outcome: factoryd.StepHandedUp,
		Found: "It failed pulling https://user:pass@git.example/x. It sent Authorization: Bearer abc123def456ghi789jkl012mno345."}}}
	ck, _ := r.loggedIn(t)
	needs := col(t, getAs(r, ck, "/", false).Body.String(), "Needs you")
	if !strings.Contains(needs, "It failed pulling") {
		t.Fatalf("the step's finding is not on the face:\n%s", needs)
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(needs, leak) {
			t.Errorf("the face holds %q:\n%s", leak, needs)
		}
	}
}

// A plan's case shows its five parts and the plan's answer forms, and
// nothing raw of the revision: not its summary, not a hold-out.
func TestPlanCaseFaceHoldsNoRawText(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"})
	r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 7, TaskID: "t-plan", Kind: factoryd.CasePlanConfirmation, Status: factoryd.CaseAtHuman,
		Problem: "Does the plan match the spec?"}}}
	ck, _ := r.loggedIn(t)
	needs := col(t, getAs(r, ck, "/?project=linkshort", false).Body.String(), "Needs you")
	for _, want := range []string{`class="card case"`, `<p class="ask">Does the plan match the spec?</p>`, `<p class="why">`, `action="/tasks/t-plan/plan/answer"`} {
		if !strings.Contains(needs, want) {
			t.Errorf("the plan's case lacks %q:\n%s", want, needs)
		}
	}
	for _, raw := range []string{"Foundation first, then the API, then the acceptance tests.", "Hold-out criteria", "The door stays shut until a key that fits turns the lock",
		"Inputs: POST /doors/1/open"} {
		if strings.Contains(needs, raw) {
			t.Errorf("the plan's face holds %q:\n%s", raw, needs)
		}
	}
}
