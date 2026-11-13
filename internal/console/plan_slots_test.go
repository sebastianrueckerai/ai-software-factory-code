package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

const slotsComments = "/api/v1/repos/linkshort/linkshort/issues/1/comments"

// slotsRig is plan t-plan #1 at revision 2 with three open questions, each
// with the planner's recommendation filed on its ask, and the confirmation.
func slotsRig(r *rig) {
	planRig(r)
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {
		{Text: "How often does the checker probe a site?", Choices: []string{"30 seconds", "60 seconds", "5 minutes"}, Recommended: "60 seconds", Grounds: "a minute catches an outage before a user does"},
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}, Recommended: "30 days", Grounds: "a month shows a trend"},
		{Text: "Who may add a site?", Choices: []string{"anyone", "the owner"}, Recommended: "the owner", Grounds: "a stranger cannot probe a private host"},
		{Kind: factoryd.QuestionConfirmation, Text: "Confirmation of the spec and the criteria of plan revision 2. Outcome: raw text of the revision."},
	}}
	r.tasks.plans["t-plan"][1].Plan = []byte(`{"summary":"Probe each site, then show the history.","spec":{"outcome":"A site owner sees an outage within a minute."}}`)
	r.caseAsks = map[int64]factoryd.CaseAsk{}
	for i, q := range r.tasks.questions["t-plan 2"][:3] {
		r.caseAsks[int64(500+i)] = factoryd.CaseAsk{At: time.Now().Add(-time.Hour), Detail: map[string]any{
			"recommendation": q.Recommended + ": " + q.Grounds, "recommended_by": "the planner"}}
	}
}

func slotsNeeds(t *testing.T, r *rig, ck *http.Cookie) string {
	t.Helper()
	return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
}

// A plan with three questions and its confirmation is one row of Needs you,
// "1 of 4": slot 1 is question 1 with its five parts, the planner's
// recommended choice checked and the planner as its source, and no text of
// the revision.
func TestAPlansQuestionsAreSlotsOfOneRow(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	needs := slotsNeeds(t, r, ck)
	if n := strings.Count(needs, `class="card case"`); n != 1 || !strings.Contains(needs, `<span class="pos" aria-live="polite">1 of 4</span>`) {
		t.Fatalf("want one row, 1 of 4 (%d rows):\n%s", n, needs)
	}
	for _, want := range []string{
		`<p class="ask">How often does the checker probe a site?</p>`,
		`<p class="why">Plan #1, revision 2: 3 questions open;`,
		`<p class="recommendation">Recommendation (the planner): 60 seconds: a minute catches an outage before a user does</p>`,
		`<input type="radio" name="choice" value="2" checked> 60 seconds`,
		`<input type="radio" name="choice" value="1"> 30 seconds`,
		`<input type="radio" name="choice" value="3"> 5 minutes`,
		`name="own"`, `name="note"`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="1">`,
	} {
		if !strings.Contains(needs, want) {
			t.Errorf("slot 1 lacks %q:\n%s", want, needs)
		}
	}
	if strings.Count(needs, " checked>") != 1 || strings.Contains(needs, "Probe each site, then show the history.") || strings.Contains(needs, "raw text of the revision") ||
		strings.Contains(needs, "A site owner sees an outage within a minute.") || strings.Contains(needs, "Who may add a site?") {
		t.Errorf("slot 1 holds a second checked choice, the revision's text or another slot's question:\n%s", needs)
	}
	next := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/501?project=linkshort", true).Body.String()
	if !strings.Contains(next, `<p class="ask">How long are the results kept?</p>`) || !strings.Contains(next, `name="choice" value="2" checked>`) || !strings.Contains(next, "2 of 4") {
		t.Errorf("slot 2:\n%s", next)
	}
	last := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/503?project=linkshort", true).Body.String()
	for _, want := range []string{`<p class="ask">Confirm the spec and the criteria, or request changes.</p>`, `name="decision" value="confirm"`, `name="decision" value="revise"`, "4 of 4"} {
		if !strings.Contains(last, want) {
			t.Errorf("the confirmation's slot lacks %q:\n%s", want, last)
		}
	}
}

// Slot 2 answered alone answers question 2 with the chosen words and the
// note, records one row for index 2, and leaves the row at "1 of 3".
func TestAnsweringOneSlotLeavesTheOthers(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"2"},
		"choice": {"2"}, "note": {"trends matter"}}), ck)
	if rec.Code != http.StatusSeeOther || r.fg.comments[slotsComments] != "Answer 2: 30 days — trends matter" {
		t.Fatalf("answer: %d %q, comment %q", rec.Code, rec.Body, r.fg.comments[slotsComments])
	}
	if got := r.tasks.questions["t-plan 2"]; got[0].Answered() || !got[1].Answered() || got[2].Answered() || got[1].Answer != "30 days — trends matter" {
		t.Errorf("questions after: %+v", got)
	}
	rows := 0
	for _, e := range r.events.all() {
		if e.Kind == factoryd.EventPlanQuestionAnswered {
			rows++
			if e.Detail["index"] != 2 {
				t.Errorf("the answer row names index %v, want 2", e.Detail["index"])
			}
		}
	}
	needs := slotsNeeds(t, r, ck)
	if rows != 1 || !strings.Contains(needs, `1 of 3</span>`) || strings.Contains(needs, "How long are the results kept?") {
		t.Errorf("%d answer rows; the row after:\n%s", rows, needs)
	}
	// free words override the choice
	rec = r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"1"},
		"choice": {"2"}, "own": {"every 45 seconds"}}), ck)
	if rec.Code != http.StatusSeeOther || r.fg.comments[slotsComments] != "Answer 1: every 45 seconds" {
		t.Errorf("own words: %d %q", rec.Code, r.fg.comments[slotsComments])
	}
}

// A question stored before the planner filed recommendations shows no
// Recommendation and no checked choice.
func TestAQuestionWithoutARecommendationChecksNothing(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	q := &r.tasks.questions["t-plan 2"][0]
	q.Recommended, q.Grounds = "", ""
	delete(r.caseAsks, 500)
	ck, _ := r.loggedIn(t)
	needs := slotsNeeds(t, r, ck)
	if !strings.Contains(needs, `<p class="ask">How often does the checker probe a site?</p>`) || strings.Contains(needs, "Recommendation") || strings.Contains(needs, " checked>") ||
		strings.Count(needs, `<input type="radio"`) != 3 {
		t.Errorf("an unrecommended question:\n%s", needs)
	}
}

// What the slot shows of model-written text is masked, URL userinfo and a
// bearer value in the outcome, a choice's label and a take-off reason; the
// choice's value, which a send posts, stays the planner's own.
func TestPlanSlotMasksModelText(t *testing.T) {
	const rawURL, bearer = "https://user:pass@git.example/x", "Authorization: Bearer abc123def456ghi789jkl012mno345"
	r := newRig(t)
	slotsRig(r)
	r.tasks.plans["t-plan"][1].Plan = []byte(`{"spec":{"outcome":"See ` + rawURL + ` and ` + bearer + `"}}`)
	r.tasks.questions["t-plan 2"][0].Choices = []string{"use " + rawURL, bearer}
	ck, s := r.loggedIn(t)
	needs := slotsNeeds(t, r, ck)
	if !strings.Contains(needs, `name="choice" value="1"> use `) {
		t.Fatalf("the label is missing:\n%s", needs)
	}
	for _, leak := range []string{"user:pass", "abc123def456", "See "} {
		if strings.Contains(needs, leak) {
			t.Errorf("the face holds %q:\n%s", leak, needs)
		}
	}
	drawer := planDrawer(t, r, ck)
	if !strings.Contains(drawer, "See ") || !strings.Contains(drawer, `name="choice" value="1">use `) {
		t.Fatalf("the drawer's masked outcome or choice buttons are missing:\n%s", drawer)
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(drawer, leak) {
			t.Errorf("the drawer holds %q:\n%s", leak, drawer)
		}
	}
	// a send names the choice by its place and records the planner's own value
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"1"}, "choice": {"1"}}), ck)
	if got := r.tasks.questions["t-plan 2"][0].Answer; rec.Code != http.StatusSeeOther || got != "use "+rawURL {
		t.Errorf("choice 1: %d, recorded %q, want %q", rec.Code, got, "use "+rawURL)
	}
	for _, pick := range []string{"3", "-1"} {
		rec = r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"2"}, "choice": {pick}}), ck)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Question 2 of plan #1 has 2 choices; there is no choice "+pick+".") {
			t.Errorf("choice %s: %d %q", pick, rec.Code, rec.Body)
		}
	}
	// the confirmation's take-off reasons
	r.tasks.plans["t-plan"][1].Plan = []byte(`{"takeOffMain":["k2"],"take_off_main":["k2"],"take_off_reasons":{"k2":"it fetched ` + rawURL + ` with ` + bearer + `"}}`)
	last := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/503?project=linkshort", true).Body.String()
	if !strings.Contains(last, "k2: it fetched") || strings.Contains(last, "user:pass") || strings.Contains(last, "abc123def456") {
		t.Errorf("the confirmation's take-off reason:\n%s", last)
	}
}

// Each slot's own and note boxes carry their question's index in their ids,
// so htmx's id match keeps no text from one slot in the next.
func TestSlotInputIDsDifferPerSlot(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	one := slotsNeeds(t, r, ck)
	two := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/501?project=linkshort", true).Body.String()
	for _, want := range []string{`id="own-t-plan-1"`, `id="note-t-plan-1"`} {
		if !strings.Contains(one, want) {
			t.Errorf("slot 1 lacks %s", want)
		}
	}
	for _, want := range []string{`id="own-t-plan-2"`, `id="note-t-plan-2"`} {
		if !strings.Contains(two, want) {
			t.Errorf("slot 2 lacks %s", want)
		}
	}
	if strings.Contains(two, `id="own-t-plan-1"`) || strings.Contains(two, `id="note-t-plan-1"`) {
		t.Errorf("slot 2 carries slot 1's ids")
	}
}

// A note with no choice and no words of its own is no answer.
func TestANoteAloneIsRefused(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"1"},
		"note": {"because"}}), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "A note is not an answer: pick one of the choices") || r.tasks.questions["t-plan 2"][0].Answered() {
		t.Errorf("a note alone: %d %q", rec.Code, rec.Body)
	}
}

// A recommended choice with no grounds files no Recommendation, and so
// checks nothing.
func TestARecommendedChoiceWithoutGroundsChecksNothing(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	r.tasks.questions["t-plan 2"][0].Grounds = ""
	delete(r.caseAsks, 500)
	ck, _ := r.loggedIn(t)
	needs := slotsNeeds(t, r, ck)
	if strings.Contains(needs, "Recommendation") || strings.Contains(needs, " checked>") || strings.Count(needs, `<input type="radio"`) != 3 {
		t.Errorf("a recommendation without grounds:\n%s", needs)
	}
}

// The confirmation's Why claims nothing about the questions beside it.
func TestTheConfirmationWhyClaimsNoAnsweredQuestions(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	last := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/503?project=linkshort", true).Body.String()
	if strings.Contains(last, "are answered") || !strings.Contains(last, "Confirm fixes the spec and the criteria") {
		t.Errorf("the confirmation's why:\n%s", last)
	}
}
