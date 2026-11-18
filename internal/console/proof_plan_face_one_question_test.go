package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// plan_face_one_question_proven: every plan case takes the one-question
// slot. Cases whose facts name their revision and index, and cases whose
// facts name neither, which planSlotFacts derives from the state key's head
// and the ask's place among the revision's questions, each show exactly one
// question with the carousel; a case whose ask matches no question falls
// to the plan's own ask, which holds one answer form too, never every open
// question's.
func TestProofPlanFaceHoldsOneQuestion(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
		{Text: "Which store keeps the snapshots?", Choices: []string{"Postgres", "S3"}},
		{Text: "Who may add a site?", Choices: []string{"anyone", "the owner"}},
	}}
	ck, _ := r.loggedIn(t)
	plan := func(id int64, key, problem string, facts map[string]any) factoryd.Case {
		return factoryd.Case{ID: id, TaskID: "t-plan", Kind: factoryd.CasePlanQuestion, Status: factoryd.CaseAtHuman,
			StateKey: key, Problem: problem, Facts: facts}
	}
	for name, cases := range map[string][]factoryd.Case{
		"facts": {
			plan(7, "b2@r2", "Who may add a site?", map[string]any{"revision": 2.0, "index": 3.0, "choices": []any{"anyone", "the owner"}}),
			plan(8, "a1@r2", "Which store keeps the snapshots?", map[string]any{"revision": 2.0, "index": 2.0, "choices": []any{"Postgres", "S3"}}),
		},
		"derived": {
			plan(7, "b2@r2", "Who may add a site?", nil),
			plan(8, "a1@r2", "Which store  keeps\nthe snapshots?", nil),
		},
	} {
		r.openCases = map[string][]factoryd.Case{"t-plan": cases}
		body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
		face := escalationOf(t, body, "t-plan")
		for _, want := range []string{
			`<p class="ask">Which store `,
			`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2">`,
			`<input type="radio" name="choice" value="1"> Postgres`, `<input type="radio" name="choice" value="2"> S3`,
			`<p class="why">Plan #1, revision 2: 2 questions open;`,
			`<div class="carousel" role="group" aria-label="The card's cases">`, `<span class="pos" aria-live="polite">1 of 2</span>`,
		} {
			if !strings.Contains(face, want) {
				t.Errorf("%s: the plan's face lacks %q:\n%s", name, want, face)
			}
		}
		if n := strings.Count(body, `class="decide plan-answer"`); n != 1 {
			t.Errorf("%s: the board holds %d plan answer forms, want the slot's one:\n%s", name, n, body)
		}
		for _, other := range []string{"How long are the results kept?", "Who may add a site?", "the owner"} {
			if strings.Contains(face, other) {
				t.Errorf("%s: the face holds another question's %q:\n%s", name, other, face)
			}
		}
	}

	r.openCases = map[string][]factoryd.Case{"t-plan": {plan(9, "c3@r2", "The plan's question.", nil)}}
	body := r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
	if n := strings.Count(body, `class="decide plan-answer"`); n != 1 || !strings.Contains(escalationOf(t, body, "t-plan"), `<p class="ask">How long are the results kept?</p>`) {
		t.Errorf("a slotless plan case holds %d plan answer forms, want its plan's one ask, the first open question:\n%s", n, body)
	}
}
