package console

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

const slotsComments = "/api/v1/repos/linkshort/linkshort/issues/1/comments"

// slotsRig is plan t-plan #1 at revision 2 with three open questions, each
// with the planner's recommendation on its case and asked an hour ago, and
// the confirmation.
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
	for i := range 3 {
		r.caseAsks[int64(500+i)] = factoryd.CaseAsk{At: time.Now().Add(-time.Hour), Detail: map[string]any{}}
	}
}

func slotsNeeds(t *testing.T, r *rig, ck *http.Cookie) string {
	t.Helper()
	return col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
}

// slotOf is the plan card's opened ask on slot caseID, 0 its first.
func slotOf(t *testing.T, r *rig, ck *http.Cookie, caseID int64) string {
	t.Helper()
	path := "/cards/t-plan/drawer?project=linkshort"
	if caseID != 0 {
		path += "&case=t-plan:" + strconv.FormatInt(caseID, 10)
	}
	opened := r.do(httptest.NewRequest(http.MethodGet, path, nil), ck).Body.String()
	if strings.Contains(opened, `<article class="need" id="need-t-plan-d">`) {
		return askBlockOf(t, opened)
	}
	return approvalFaceOf(t, opened, "t-plan")
}

// A plan with three questions and its confirmation is one row of Needs you,
// "1 of 4": slot 1 is question 1, the planner's recommended choice
// suggested with its grounds and the planner as its writer, an answer per
// choice and the human's own words, and no text of the revision. Its next
// arrow opens slot 2, whose own recommendation leads, and the last slot is
// the confirmation's two answers.
func TestAPlansQuestionsAreSlotsOfOneRow(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	row := slotsNeeds(t, r, ck)
	if n := strings.Count(row, ` data-case="`); n != 1 || !strings.Contains(row, `<span class="pos" aria-live="polite">1 of 4</span>`) ||
		!strings.Contains(row, `<strong class="title">How often does the checker probe a site?</strong>`) {
		t.Fatalf("want one row, 1 of 4, asking question 1 (%d rows):\n%s", n, row)
	}
	needs := slotOf(t, r, ck, 0)
	for _, want := range []string{
		`<p class="o-label">Your call · 1 of 4`,
		`<p class="o-question">How often does the checker probe a site?</p>`,
		`class="answer suggested" data-answer="plan:60 seconds"`,
		`<p class="o-by">suggested by the planner: <span class="quote">a minute catches an outage before a user does</span></p>`,
		`<input type="hidden" name="choice" value="1">` + "\n" + `<button type="submit" class="pick">30 seconds</button>`,
		`<input type="hidden" name="choice" value="3">` + "\n" + `<button type="submit" class="pick">5 minutes</button>`,
		`name="own"`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="1">`,
	} {
		if !strings.Contains(needs, want) {
			t.Errorf("slot 1 lacks %q:\n%s", want, needs)
		}
	}
	if strings.Count(needs, "suggested") != 2 {
		t.Errorf("slot 1 suggests other than one answer:\n%s", needs)
	}
	if strings.Contains(row, "Probe each site, then show the history.") || strings.Contains(row, "raw text of the revision") ||
		strings.Contains(row, "A site owner sees an outage within a minute.") || strings.Contains(row, "Who may add a site?") {
		t.Errorf("the small card holds the revision's text or another slot's question:\n%s", row)
	}
	if next := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/501?project=linkshort", true).Body.String(); !strings.Contains(next, `<strong class="title">How long are the results kept?</strong>`) ||
		!strings.Contains(next, "2 of 4") {
		t.Errorf("slot 2's small card:\n%s", next)
	}
	link := regexp.MustCompile(`<a class="next" href="([^"]+)"`).FindStringSubmatch(needs)
	if link == nil {
		t.Fatalf("slot 1 has no next arrow:\n%s", needs)
	}
	next := askBlockOf(t, getAs(r, ck, html.UnescapeString(link[1]), true).Body.String())
	if !strings.Contains(next, `<p class="o-question">How long are the results kept?</p>`) || !strings.Contains(next, `class="answer suggested" data-answer="plan:30 days"`) {
		t.Errorf("slot 2:\n%s", next)
	}
	if last := getAs(r, ck, "/projects/linkshort/cards/t-plan/cases/503?project=linkshort", true).Body.String(); !strings.Contains(last, "4 of 4") {
		t.Errorf("the confirmation's small card:\n%s", last)
	}
	last := slotOf(t, r, ck, 503)
	for _, want := range []string{`<p class="o-question">Confirm plan “Link shortener” so its cards can start?</p>`, `name="decision" value="confirm"`, `name="decision" value="revise"`} {
		if !strings.Contains(last, want) {
			t.Errorf("the confirmation's slot lacks %q:\n%s", want, last)
		}
	}
}

// Slot 2 answered alone answers question 2 with the chosen words, records
// one row for index 2, and leaves the row at "1 of 3".
func TestAnsweringOneSlotLeavesTheOthers(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"2"},
		"choice": {"2"}}), ck)
	if rec.Code != http.StatusSeeOther || r.fg.comments[slotsComments] != "Answer 2: 30 days" {
		t.Fatalf("answer: %d %q, comment %q", rec.Code, rec.Body, r.fg.comments[slotsComments])
	}
	if got := r.tasks.questions["t-plan 2"]; got[0].Answered() || !got[1].Answered() || got[2].Answered() || got[1].Answer != "30 days" {
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

// A question stored before the planner filed recommendations suggests no
// answer.
func TestAQuestionWithoutARecommendationChecksNothing(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	q := &r.tasks.questions["t-plan 2"][0]
	q.Recommended, q.Grounds = "", ""
	delete(r.caseAsks, 500)
	ck, _ := r.loggedIn(t)
	needs := slotOf(t, r, ck, 0)
	if !strings.Contains(needs, `<p class="o-question">How often does the checker probe a site?</p>`) || strings.Contains(needs, "suggested") ||
		strings.Count(needs, `name="choice"`) != 3 {
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
	slot, face := slotOf(t, r, ck, 0), slotsNeeds(t, r, ck)
	if !strings.Contains(slot, `<input type="hidden" name="choice" value="1">`+"\n"+`<button type="submit" class="pick">use `) {
		t.Fatalf("the label is missing:\n%s", slot)
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(slot+face, leak) {
			t.Errorf("the slot or the small card holds %q:\n%s\n%s", leak, slot, face)
		}
	}
	if strings.Contains(face, "See ") {
		t.Errorf("the small card holds the outcome:\n%s", face)
	}
	drawer := planDrawer(t, r, ck)
	if !strings.Contains(drawer, "See ") || !strings.Contains(drawer, `<button type="submit" class="pick">use `) {
		t.Fatalf("the drawer's masked outcome or the slot's choices are missing:\n%s", drawer)
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
	last := slotOf(t, r, ck, 503)
	if !strings.Contains(last, "k2: it fetched") || strings.Contains(last, "user:pass") || strings.Contains(last, "abc123def456") {
		t.Errorf("the confirmation's take-off reason:\n%s", last)
	}
}

// Each slot's own-words box carries its case in its id and keeps its
// text across a swap, so htmx's id match keeps no text from one slot in
// the next.
func TestSlotInputIDsDifferPerSlot(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	for id, slot := range map[string]string{"500": slotOf(t, r, ck, 0), "501": slotOf(t, r, ck, 501)} {
		if !strings.Contains(slot, `<textarea id="f-t-plan-`+id+`-plan-answer-own-words-note" name="own" rows="2" maxlength="1000" placeholder="The answer the planner needs" required hx-preserve></textarea>`) {
			t.Errorf("slot %s's own words box lacks its case or its preserve:\n%s", id, slot)
		}
	}
}

// A send with no choice and no words is no answer.
func TestANoteAloneIsRefused(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"1"}}), ck)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "an answer of 1 to 1000 characters is required") || r.tasks.questions["t-plan 2"][0].Answered() {
		t.Errorf("an empty answer: %d %q", rec.Code, rec.Body)
	}
}

// A recommended choice with no grounds files no recommendation, and so
// suggests nothing.
func TestARecommendedChoiceWithoutGroundsChecksNothing(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	r.tasks.questions["t-plan 2"][0].Grounds = ""
	delete(r.caseAsks, 500)
	ck, _ := r.loggedIn(t)
	needs := slotOf(t, r, ck, 0)
	if strings.Contains(needs, "suggested") || strings.Count(needs, `name="choice"`) != 3 {
		t.Errorf("a recommendation without grounds:\n%s", needs)
	}
}

// The confirmation's Why claims nothing about the questions beside it.
func TestTheConfirmationWhyClaimsNoAnsweredQuestions(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, _ := r.loggedIn(t)
	last := slotOf(t, r, ck, 503)
	if strings.Contains(last, "are answered") || !strings.Contains(last, `<p class="o-askwhy">The plan&#39;s cards and promises are fixed only once a human confirms them.</p>`) {
		t.Errorf("the confirmation's why:\n%s", last)
	}
}

// The plan's two asks each send one answer alone: the question's answer
// ask, a choice per answer to /tasks/{id}/plan/answer and no decision; the
// confirmation's, Confirm and Ask for changes, its decision and no choice;
// no fund beside either. Each send lands one row of the card.
func TestPlanFacesDecideThroughTheSharedPartial(t *testing.T) {
	r := newRig(t)
	slotsRig(r)
	ck, s := r.loggedIn(t)
	answer, confirm := slotOf(t, r, ck, 0), slotOf(t, r, ck, 503)
	if n := strings.Count(answer, `action="/tasks/t-plan/plan/answer"`); n != 4 || strings.Contains(answer, `name="decision"`) {
		t.Errorf("the answer ask holds %d answers or a decision:\n%s", n, answer)
	}
	if n := strings.Count(confirm, `action="/tasks/t-plan/plan/answer"`); n != 2 || strings.Contains(confirm, `name="choice"`) {
		t.Errorf("the confirm ask holds %d answers or a choice:\n%s", n, confirm)
	}
	if strings.Contains(answer+confirm, "fund") {
		t.Errorf("an ask holds a fund beside its answers")
	}
	if rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"1"},
		"choice": {"2"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the answer's send: %d %q", rec.Code, rec.Body)
	}
	if got := r.fg.comments[slotsComments]; got != "Answer 1: 60 seconds" {
		t.Errorf("the answer's comment = %q", got)
	}
	if rec := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"}, "index": {"4"},
		"decision": {"confirm"}, "answer": {"the spec is sound"}}), ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("the confirm's send: %d %q", rec.Code, rec.Body)
	}
	if got := r.fg.comments[slotsComments]; got != "Answer 4: confirm · the spec is sound" {
		t.Errorf("the confirm's comment = %q", got)
	}
	rows := 0
	for _, e := range r.events.all() {
		if e.Kind == factoryd.EventPlanQuestionAnswered {
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("the two sends wrote %d answer rows, want one each", rows)
	}
}
