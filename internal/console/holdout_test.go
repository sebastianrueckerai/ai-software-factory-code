package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// holdoutsRig is planRig (plan t-plan #1 of linkshort waiting at revision 2)
// with revision 2's confirmation open and the plan's hold-out rows: one kept
// out in round 1, one drafted and one unrunnable in round 2.
func holdoutsRig(r *rig, confirmation factoryd.PlanQuestion) {
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {confirmation}}
	r.tasks.holdouts = map[string][]factoryd.Holdout{"t-plan": {
		{Idx: 1, Round: 1, ID: "old_door", Criterion: "The old round's door stays shut", State: factoryd.HoldoutStored},
		{Idx: 2, Round: 2, ID: "door_state", Criterion: "The door opens only for a key that fits",
			State: factoryd.HoldoutProposed, DryVerdict: "red"},
		{Idx: 3, Round: 2, ID: "key_log", Criterion: "Every key turn lands in the log", State: factoryd.HoldoutUnrunnable},
	}}
}

// Piece 6: the plan card holds the round's hold-out criteria between its
// questions and its accept form, this round's rows only, each with its
// state's one line, an Okay on a drafted one and a Drop on a drafted or
// unrunnable one; the forms carry the row's own index, the number the gate
// and factoryd know it by, and the confirmation stays the card's only open
// question.
func TestPlanCardHoldsTheHoldoutZone(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?", Choices: []string{"It does"}})
	ck, _ := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
	for _, want := range []string{
		`<div class="holdouts">`,
		`<p class="meta">Hold-out criteria, kept out of the plan and run at the gate, hidden from the agent.</p>`,
		`<legend>Hold-out 1: The door opens only for a key that fits</legend>`,
		`<legend>Hold-out 2: Every key turn lands in the log</legend>`,
		"drafted; okay keeps it out of the plan and runs it at the gate, hidden from the agent",
		"its draft did not run; drop discards it",
		`<div class="choices" role="group" aria-label="Keep the hold-out out or drop it">`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2">`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="3">`,
		`<button type="submit" name="verb" value="okay">Okay</button>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "The old round's door stays shut") {
		t.Errorf("the card shows round 1's hold-out:\n%s", card)
	}
	if strings.Count(card, `action="/tasks/t-plan/plan/holdout"`) != 3 || strings.Count(card, `value="drop" class="deny">Drop`) != 2 {
		t.Errorf("want 3 hold-out forms, one okay and two drops:\n%s", card)
	}
	if i, q, a := strings.Index(card, `class="holdouts"`), strings.LastIndex(card, `action="/tasks/t-plan/plan/answer"`), strings.Index(card, `action="/tasks/t-plan/plan/accept"`); !(q < i && i < a) {
		t.Errorf("the hold-out zone sits at %d, the last answer form %d, the accept form %d; want answer < zone < accept", q, i, a)
	}
}

// Once the round's confirmation is answered, its hold-out rows are settled:
// the card still lists them with their state's one line, but no click works.
func TestPlanCardHoldoutsSettleWithTheConfirmation(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?",
		Choices: []string{"It does"}, Answer: "It does", AnsweredBy: "sebastian"})
	ck, s := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
	if !strings.Contains(card, `<legend>Hold-out 1: The door opens only for a key that fits</legend>`) ||
		strings.Contains(card, `action="/tasks/t-plan/plan/holdout"`) {
		t.Errorf("the settled round's card holds the rows and no forms:\n%s", card)
	}
	rec := r.do(post("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"2"}, "verb": {"okay"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusConflict || rec.Body.String() != "The confirmation of plan #1 is answered already; its hold-outs are settled.\n" {
		t.Fatalf("okay after the confirmation = %d %q", rec.Code, rec.Body)
	}
}

// The okay and the drop store the decision and show the board again: the
// row's line turns, nothing is posted on the issue, and the event feed
// carries the click. A second click on the row, one on no row, one on a
// stale revision and a verb that is neither okay nor drop are refused.
func TestPlanHoldoutClicksDecideTheRound(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?", Choices: []string{"It does"}})
	ck, s := r.loggedIn(t)
	board := func() string {
		return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
	}
	click := func(revision, index, verb string) *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {revision}, "index": {index}, "verb": {verb},
			"project": {"linkshort"}}), ck)
	}
	if rec := click("2", "2", "okay"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" ||
		!slices.Contains(r.fg.calls, "SIGNAL holdout_decided t-plan 2 2 okay by sebastian") || len(r.fg.comments) != 0 ||
		!slices.ContainsFunc(r.events.rows, func(e factoryd.Event) bool { return e.Kind == factoryd.EventHoldoutDecided }) {
		t.Fatalf("okay: %d to %q, calls %v, comments %v; body %s", rec.Code, rec.Header().Get("Location"), r.fg.calls, r.fg.comments, rec.Body)
	}
	if card := board(); !strings.Contains(card, "kept out; it runs at the gate, hidden from the agent") ||
		strings.Count(card, `action="/tasks/t-plan/plan/holdout"`) != 1 {
		t.Errorf("the card after the okay:\n%s", card)
	}
	for _, tc := range []struct {
		revision, index, verb string
		status                int
		want                  string
	}{
		{"2", "2", "okay", http.StatusConflict, "the hold-out takes no such click now"},
		{"2", "9", "drop", http.StatusConflict, "no such hold-out"},
		{"1", "1", "drop", http.StatusConflict, "the plan changed to revision 2; reload"},
		{"2", "3", "keep", http.StatusBadRequest, "okay or drop is required"},
	} {
		if rec := click(tc.revision, tc.index, tc.verb); rec.Code != tc.status || rec.Body.String() != tc.want+"\n" {
			t.Errorf("click %s %s %s = %d %q; want %d %q", tc.verb, tc.revision, tc.index, rec.Code, rec.Body, tc.status, tc.want)
		}
	}
	drop := r.do(htmxPost("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"3"}, "verb": {"drop"},
		"project": {"linkshort"}}), ck)
	if drop.Code != http.StatusOK || !strings.HasPrefix(drop.Body.String(), `<div id="board"`) ||
		!strings.Contains(drop.Body.String(), `<p class="note" role="status">Hold-out 3 of plan #1 dropped.</p>`) {
		t.Fatalf("drop by htmx: %d %s", drop.Code, drop.Body)
	}
	answer := r.do(htmxPost("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"1"}, "answer": {"It does"},
		"project": {"linkshort"}}), ck)
	if answer.Code != http.StatusOK || !strings.Contains(answer.Body.String(), `<p class="note" role="status">Answered question 1 of plan #1.</p>`) {
		t.Fatalf("the confirmation's answer by htmx: %d %s", answer.Code, answer.Body)
	}
	if card := board(); strings.Contains(card, `action="/tasks/t-plan/plan/holdout"`) || !strings.Contains(card, ">dropped<") {
		t.Errorf("the card after the drop:\n%s", card)
	}
}
