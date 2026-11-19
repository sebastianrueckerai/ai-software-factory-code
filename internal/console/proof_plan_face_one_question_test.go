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
// facts name neither, whose slot is derived from the state key's head and
// the ask's place among the revision's questions, each show exactly one
// question, in the questions' order, with the carousel; a case whose ask
// matches no question asks the plan's first open question, never every
// open question at once.
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
	board := func() string {
		return r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String()
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
		body := board()
		face := faceOf(t, body, "t-plan")
		for _, want := range []string{
			`data-case="8"`, `<strong class="title">Which store keeps the snapshots?</strong>`,
			`<div class="carousel" role="group" aria-label="The card's asks">`, `<span class="pos" aria-live="polite">1 of 2</span>`,
		} {
			if !strings.Contains(face, want) {
				t.Errorf("%s: the plan's face lacks %q:\n%s", name, want, face)
			}
		}
		ask := askBlockOf(t, planDrawer(t, r, ck))
		for _, want := range []string{
			`<p class="o-question">Which store keeps the snapshots?</p>`,
			`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2"><input type="hidden" name="choice" value="1">` + "\n" + `<button type="submit" class="pick">Postgres</button>`,
			`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2"><input type="hidden" name="choice" value="2">` + "\n" + `<button type="submit" class="pick">S3</button>`,
		} {
			if !strings.Contains(ask, want) {
				t.Errorf("%s: the plan's ask lacks %q:\n%s", name, want, ask)
			}
		}
		if n := strings.Count(ask, `<p class="o-question">`); n != 1 {
			t.Errorf("%s: the opened card asks %d questions, want the slot's one:\n%s", name, n, ask)
		}
		for _, other := range []string{"How long are the results kept?", "Who may add a site?", "the owner"} {
			if strings.Contains(face, other) || strings.Contains(ask, other) {
				t.Errorf("%s: the face holds another question's %q:\n%s\n%s", name, other, face, ask)
			}
		}
	}

	r.openCases = map[string][]factoryd.Case{"t-plan": {plan(9, "c3@r2", "The plan's question.", nil)}}
	face, ask := faceOf(t, board(), "t-plan"), askBlockOf(t, planDrawer(t, r, ck))
	if n := strings.Count(ask, `<p class="o-question">`); n != 1 || !strings.Contains(ask, `<p class="o-question">How long are the results kept?</p>`) ||
		!strings.Contains(face, `<strong class="title">How long are the results kept?</strong>`) {
		t.Errorf("a slotless plan case asks %d questions, want its plan's one ask, the first open question:\n%s\n%s", n, face, ask)
	}
}
