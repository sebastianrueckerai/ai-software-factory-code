package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// confirmText is the confirmation's stored text, factoryd's multi-line
// shape: the decision line, then the spec, the criteria and the later
// rounds, each on its own line.
func confirmText() string {
	return "Confirmation of the spec and the criteria of plan revision 2: Confirm fixes the spec and the criteria; Request changes sends your words to the next planner round. The criteria are fixed only when this is confirmed.\n" +
		"Outcome: A door opens only for a key that fits.\n" +
		"Glossary: key: the token that opens a door; log: the record of every key turn\n" +
		"Criteria: door_open (test), key_log (invariant).\n" +
		"Still to plan, in later rounds: the console's own use of the API.\n" +
		"The whole plan is in the plan's comment on this issue."
}

// holdoutsRig is planRig (plan t-plan #1 of linkshort waiting at revision 2)
// with revision 2's confirmation open — its text factoryd's stored shape —
// the plan's hold-out rows, one kept out in round 1 and two proposed in
// round 2, and a revision carrying the spec, two criteria and later rounds,
// so the card's layers hold their lines.
func holdoutsRig(r *rig, confirmation factoryd.PlanQuestion) {
	planRig(r)
	plan, _ := json.Marshal(factoryd.Plan{
		Summary: "The foundation lands first, with its migrations.\n\nOn it, the links API and the acceptance tests close the round.",
		Spec:    &factoryd.Spec{Outcome: "A door opens only for a key that fits.", Glossary: []string{"key: the token that opens a door", "log: the record of every key turn"}},
		Outline: []string{"the console's own use of the API"},
		Criteria: []factoryd.PlanCriterion{
			{ID: "door_open", Kind: factoryd.CriterionTest, Claim: "A door opens only for a key that fits", Inputs: "POST /doors/1/open with key 9", Expected: "403 and the door row still shut"},
			{ID: "key_log", Kind: factoryd.CriterionInvariant, Suite: "./proofs/key_log"},
		},
		Cards: []factoryd.PlanCard{
			{Key: "integration", Role: "integration", Title: "Acceptance tests", Size: "small"},
			{Key: "foundation", Role: "foundation", Title: "Foundation", Size: "medium"},
			{Key: "links-api", Role: "feature", Title: "Links API", Size: "medium"},
		},
	})
	r.tasks.plans["t-plan"][1].Plan = plan
	confirmation.Text = confirmText()
	r.tasks.questions = map[string][]factoryd.PlanQuestion{"t-plan 2": {confirmation}}
	r.tasks.proofs = map[string][]factoryd.PlanProof{"t-plan": {
		{Idx: 1, Round: 1, ID: "old_door", Origin: factoryd.ProofOriginHoldout, Claim: "The old round's door stays shut", State: factoryd.ProofOkayed},
		{Idx: 2, Round: 2, ID: "door_state", Origin: factoryd.ProofOriginHoldout, Claim: "The door stays shut until a key that fits turns the lock",
			Inputs: "POST /doors/1/open with key 9", Expected: "403 and the door row still shut", State: factoryd.ProofProposed,
			Name: "TestSecret", Code: "func TestSecret(t *testing.T) {}"},
		{Idx: 3, Round: 2, ID: "key_log", Origin: factoryd.ProofOriginHoldout, Claim: "Every key turn lands in the log",
			Inputs: "three turns of key 7", Expected: "three log rows", State: factoryd.ProofProposed},
	}}
}

// planDrawer is the drawer of plan t-plan: the face holds the hold-outs
// only while its confirmation waits, so the full rows sit here.
func planDrawer(t *testing.T, r *rig, ck *http.Cookie) string {
	t.Helper()
	rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?project=linkshort", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("drawer of t-plan = %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The plan's drawer reads as the confirmation's layers: the ask's five
// parts and its one decide block — Confirm and Request changes in one
// form, one fund control, one note box, one send — then the hold-outs
// zone beside the decision and ahead of the other questions whatever the
// confirmation's index — this round's rows only, each with its claim on
// its own line, its inputs and expected numbers and its state's one line,
// and one form on a proposed one whose Okay and Drop are its two sends,
// carrying the row's own index, the number the gate and factoryd know it
// by. Under the questions the drawer alone holds the summary as markup,
// the criteria and the closed fold with the outcome, each glossary term,
// the later rounds and the cards. A proof's code never reaches the card,
// an okayed or stored row takes no click, and a state without a line
// shows none.
func TestPlanCardHoldsTheHoldoutZone(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	r.tasks.questions["t-plan 2"] = []factoryd.PlanQuestion{
		{Text: "How long are the results kept?", Choices: []string{"7 days", "30 days"}},
		{Kind: factoryd.QuestionConfirmation, Text: confirmText()}}
	r.tasks.proofs["t-plan"][2].State = factoryd.ProofOkayed
	r.tasks.proofs["t-plan"] = append(r.tasks.proofs["t-plan"], factoryd.PlanProof{Idx: 4, Round: 2, ID: "lock_log",
		Origin: factoryd.ProofOriginHoldout, Claim: "Every lock turn lands in the log", Inputs: "one turn", Expected: "one row",
		State: factoryd.ProofStored, Path: "proofs/lock_log/lock_log_test.go", Suite: "./proofs/lock_log", Name: "TestLockLog", Code: "func TestLockLog(t *testing.T) {}"})
	ck, _ := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	for _, want := range []string{
		`<p class="ask">Confirm the spec and the criteria, or request changes.</p>`,
		`<p class="why">Plan #1, revision 2: Confirm fixes the spec and the criteria, Request changes sends your words to the next planner round.</p>`,
		`<div class="holdouts" id="holdouts-t-plan-d">`,
		`<p class="meta">A hold-out is a behaviour the plan promises to keep intact, kept out of the plan the agent sees and proved at the gate.</p>`,
		`<legend>Hold-out 1</legend>`,
		`<p class="claim">The door stays shut until a key that fits turns the lock</p>`,
		`<p class="meta">Inputs: POST /doors/1/open with key 9<br>Expected: 403 and the door row still shut</p>`,
		"proposed: okay keeps it out of the plan. The prover writes its test, hidden from the agent, unless it filed before the okay.",
		`<legend>Hold-out 2</legend>`,
		`<p class="claim">Every key turn lands in the log</p>`,
		"kept out: once the plan&#39;s foundation is on main, or at once when the plan has none, the prover writes its test, hidden from the agent. A hold-out okayed after the prover files gets no test.",
		`<div class="choices" role="group" aria-label="Keep the hold-out out or drop it">`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2">`,
		`<button type="submit" name="verb" value="okay" class="primary">Okay</button>`,
		`<button type="submit" name="verb" value="drop" class="deny">Drop</button>`,
		`<div class="info"><p>The foundation lands first, with its migrations.</p>`,
		`<p>On it, the links API and the acceptance tests close the round.</p>`,
		`<p class="meta">door_open (test): A door opens only for a key that fits</p>`,
		`<p class="meta">key_log (invariant): ./proofs/key_log</p>`,
		`<details class="fold"><summary>The spec and the whole revision</summary>`,
		`<p class="meta">A door opens only for a key that fits.</p>`,
		`<ul class="glossary"><li>key: the token that opens a door</li><li>log: the record of every key turn</li></ul>`,
		`<p class="meta">Still to plan:</p><ul><li>the console&#39;s own use of the API</li></ul>`,
		`<p class="meta">The plan's cards:</p><ul><li>foundation: Foundation</li><li>links-api: Links API</li><li>integration: Acceptance tests</li></ul>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	for _, leak := range []string{"TestSecret", "TestLockLog", "The old round", `name="index" value="4"`, `<p class="meta"></p>`, "<details class=\"fold\" open>"} {
		if strings.Contains(card, leak) {
			t.Errorf("the card holds %q:\n%s", leak, card)
		}
	}
	if strings.Count(card, `action="/tasks/t-plan/plan/holdout"`) != 1 {
		t.Errorf("want one hold-out form, the proposal's Okay and Drop its two sends:\n%s", card)
	}
	if n := strings.Count(card, `action="/tasks/t-plan/plan/answer"`); n != 1 {
		t.Errorf("the drawer holds the confirmation's one decide form and asks no other question beside it:\n%s", card)
	}
	ask, forms := strings.Index(card, `<p class="ask">Confirm the spec`), strings.Index(card, `name="decision" value="confirm"`)
	zone := strings.Index(card, `id="holdouts-t-plan-d"`)
	question := strings.Index(card, `1. How long are the results kept?`)
	summary := strings.Index(card, `<div class="info"><p>The foundation lands first`)
	criteria := strings.Index(card, `<p class="meta">door_open (test)`)
	fold := strings.Index(card, `<details class="fold">`)
	if !(ask < forms && forms < zone && zone < question && question < summary && summary < criteria && criteria < fold) {
		t.Errorf("the layers out of order: ask %d, forms %d, zone %d, question %d, summary %d, criteria %d, fold %d:\n%s",
			ask, forms, zone, question, summary, criteria, fold, card)
	}
	if strings.Contains(card, `action="/tasks/t-plan/plan/changes"`) {
		t.Errorf("the open confirmation's Request changes is the card's only one:\n%s", card)
	}
}

// The board face of a confirming plan holds the decision's layers beside
// the slot's forms: the lead line, then the hold-outs zone with its clicks
// — its Confirm waits on them, so the okay and the drop sit beside it, not
// only in the drawer. The face holds no raw text of the revision: no
// summary, no spec, no criteria, and the proof's code stays off it.
func TestPlanCardFaceHoldsConfirmingHoldouts(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	card := col(t, r.do(httptest.NewRequest(http.MethodGet, "/?project=linkshort", nil), ck).Body.String(), "Needs you")
	for _, want := range []string{
		`<p class="lead">Confirm fixes the spec and the criteria; Request changes sends your words back to the planner.</p>`,
		`<div class="holdouts" id="holdouts-t-plan">`,
		`<legend>Hold-out 1</legend>`,
		`<p class="claim">The door stays shut until a key that fits turns the lock</p>`,
		`<button type="submit" name="verb" value="okay" class="primary">Okay</button>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the face lacks %q:\n%s", want, card)
		}
	}
	if n := strings.Count(card, `action="/tasks/t-plan/plan/holdout"`); n != 2 {
		t.Errorf("want one hold-out form per proposed row on the face:\n%s", card)
	}
	if n := strings.Count(card, `action="/tasks/t-plan/plan/answer"`); n != 1 {
		t.Errorf("the face holds the confirmation's one decide form, the slot's, and the card holds none more:\n%s", card)
	}
	for _, raw := range []string{"The foundation lands first, with its migrations.", "On it, the links API and the acceptance tests close the round.",
		"A door opens only for a key that fits.", "key: the token that opens a door", "door_open (test)", "Still to plan",
		"Outcome:", "Glossary:", "The whole plan is in the plan", "TestSecret"} {
		if strings.Contains(card, raw) {
			t.Errorf("the face holds the revision's raw %q:\n%s", raw, card)
		}
	}
}

// Once the round's confirmation is answered, its hold-out rows are settled:
// the card still lists them with their state's one line, but no click
// works, and the answered confirmation reads its first line only.
func TestPlanCardHoldoutsSettleWithTheConfirmation(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation,
		Decision: factoryd.DecisionConfirm, AnsweredBy: "sebastian"})
	ck, s := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	if !strings.Contains(card, `<legend>Hold-out 1</legend>`) ||
		!strings.Contains(card, `<p class="claim">The door stays shut until a key that fits turns the lock</p>`) ||
		strings.Contains(card, `action="/tasks/t-plan/plan/holdout"`) {
		t.Errorf("the settled round's card holds the rows and no forms:\n%s", card)
	}
	if !strings.Contains(card, `<p class="answered">Confirmation of the spec and the criteria of plan revision 2: Confirm fixes the spec and the criteria; Request changes sends your words to the next planner round. The criteria are fixed only when this is confirmed.: confirm (sebastian)</p>`) {
		t.Errorf("the answered confirmation reads its first line only:\n%s", card)
	}
	rec := r.do(post("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"2"}, "verb": {"okay"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusConflict || rec.Body.String() != "The confirmation of plan #1 is answered already; its hold-outs are settled.\n" {
		t.Fatalf("okay after the confirmation = %d %q", rec.Code, rec.Body)
	}
}

// A claim the prover wrote across lines renders whole, as stored: the card
// never rewrites it to its first line.
func TestACompoundHoldoutClaimRendersWhole(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	r.tasks.proofs["t-plan"][1].Claim = "The door stays shut.\nThe key that fits turns the lock."
	ck, _ := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	if !strings.Contains(card, "<p class=\"claim\">The door stays shut.\nThe key that fits turns the lock.</p>") {
		t.Errorf("the compound claim does not render whole:\n%s", card)
	}
}

// The okay and the drop store the decision and show the board again: the
// row's line turns, nothing is posted on the issue, and the event feed
// carries the click. A second click on the row, one on no row, one on a
// stale revision and a verb that is neither okay nor drop are refused.
func TestPlanHoldoutClicksDecideTheRound(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, s := r.loggedIn(t)
	board := func() string { return planDrawer(t, r, ck) }
	click := func(revision, index, verb string) *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {revision}, "index": {index}, "verb": {verb},
			"project": {"linkshort"}}), ck)
	}
	if rec := click("2", "2", "okay"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" ||
		!slices.Contains(r.fg.calls, "SIGNAL holdout_decided t-plan 2 2 okay by sebastian") || len(r.fg.comments) != 0 ||
		!slices.ContainsFunc(r.events.rows, func(e factoryd.Event) bool { return e.Kind == factoryd.EventHoldoutDecided }) {
		t.Fatalf("okay: %d to %q, calls %v, comments %v; body %s", rec.Code, rec.Header().Get("Location"), r.fg.calls, r.fg.comments, rec.Body)
	}
	if card := board(); !strings.Contains(card, "kept out: once the plan&#39;s foundation is on main, or at once when the plan has none, the prover writes its test, hidden from the agent. A hold-out okayed after the prover files gets no test.") ||
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
	if !pressRefreshes(drop) {
		t.Fatalf("drop by htmx: %d %s", drop.Code, drop.Body)
	}
	answer := r.do(htmxPost("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"1"}, "decision": {"confirm"},
		"project": {"linkshort"}}), ck)
	if !pressRefreshes(answer) {
		t.Fatalf("the confirmation's answer by htmx: %d %s", answer.Code, answer.Body)
	}
	if card := board(); strings.Contains(card, `action="/tasks/t-plan/plan/holdout"`) || !strings.Contains(card, ">dropped<") {
		t.Errorf("the card after the drop:\n%s", card)
	}
}

// The confirmation view reads in its layers, the drawer's whole read of
// the rig: the decision's ask and why, then its one decide form whose
// Confirm and Request changes are its answers, then the hold-outs zone —
// the round's two proposed rows, each with its claim on its own line,
// its inputs and expected numbers, its proposed line and its one form
// whose Okay and Drop are its two sends carrying the row's own index —
// and under the questions the summary, the criteria and the closed fold
// with the outcome, the glossary, the later rounds and the cards. A
// proof's code and an earlier round's row never reach the card.
func TestConfirmationViewLayers(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	for _, want := range []string{
		`<p class="ask">Confirm the spec and the criteria, or request changes.</p>`,
		`<p class="why">Plan #1, revision 2: Confirm fixes the spec and the criteria, Request changes sends your words to the next planner round.</p>`,
		`name="decision" value="confirm"`,
		`name="decision" value="revise"`,
		`<div class="holdouts" id="holdouts-t-plan-d">`,
		`<legend>Hold-out 1</legend>`,
		`<p class="claim">The door stays shut until a key that fits turns the lock</p>`,
		`<p class="meta">Inputs: POST /doors/1/open with key 9<br>Expected: 403 and the door row still shut</p>`,
		"proposed: okay keeps it out of the plan. The prover writes its test, hidden from the agent, unless it filed before the okay.",
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2">`,
		`<legend>Hold-out 2</legend>`,
		`<p class="claim">Every key turn lands in the log</p>`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="3">`,
		`<button type="submit" name="verb" value="okay" class="primary">Okay</button>`,
		`<button type="submit" name="verb" value="drop" class="deny">Drop</button>`,
		`<div class="info"><p>The foundation lands first, with its migrations.</p>`,
		`<p class="meta">door_open (test): A door opens only for a key that fits</p>`,
		`<p class="meta">key_log (invariant): ./proofs/key_log</p>`,
		`<details class="fold"><summary>The spec and the whole revision</summary>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the drawer lacks %q:\n%s", want, card)
		}
	}
	for _, leak := range []string{"TestSecret", "The old round"} {
		if strings.Contains(card, leak) {
			t.Errorf("the drawer holds %q:\n%s", leak, card)
		}
	}
	if n := strings.Count(card, `action="/tasks/t-plan/plan/holdout"`); n != 2 {
		t.Errorf("want a hold-out form per proposed row, its Okay and Drop the two sends, got %d:\n%s", n, card)
	}
	ask := strings.Index(card, `<p class="ask">Confirm the spec`)
	forms := strings.Index(card, `name="decision" value="confirm"`)
	zone := strings.Index(card, `id="holdouts-t-plan-d"`)
	summary := strings.Index(card, `<div class="info"><p>The foundation lands first`)
	criteria := strings.Index(card, `<p class="meta">door_open (test)`)
	fold := strings.Index(card, `<details class="fold">`)
	if !(ask < forms && forms < zone && zone < summary && summary < criteria && criteria < fold) {
		t.Errorf("the layers out of order: ask %d, forms %d, zone %d, summary %d, criteria %d, fold %d:\n%s",
			ask, forms, zone, summary, criteria, fold, card)
	}
}

// The confirmation view leads with the decision's ask and explains
// the hold-out term where it first appears: above the hold-outs the card
// carries the decision's ask, and the hold-out zone opens with the
// term's explanation ahead of its first row.
func TestConfirmationLead(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	const lead = "Confirm the spec and the criteria, or request changes."
	const explains = "A hold-out is a behaviour the plan promises to keep intact, kept out of the plan the agent sees and proved at the gate."
	for _, want := range []string{lead, explains} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	zone, why, row := strings.Index(card, `id="holdouts-t-plan-d"`), strings.Index(card, explains), strings.Index(card, `<legend>Hold-out 1</legend>`)
	if !(strings.Index(card, lead) < zone && zone < why && why < row) {
		t.Errorf("the ask must sit above the zone and the zone open with its explanation: ask %d, zone %d, explanation %d, first row %d:\n%s",
			strings.Index(card, lead), zone, why, row, card)
	}
}

// A Confirm answered while the round's hold-outs wait is refused with
// the count of the round's proposed hold-outs and the word below, on the
// card's press and in a chat batch: the rig's two proposed hold-outs of
// round 2 name two, a rig with one names one.
func TestRefusalNamesTheCount(t *testing.T) {
	refuse := func(t *testing.T, r *rig, ck *http.Cookie, s *session, want string) {
		t.Helper()
		press := r.do(post("/tasks/t-plan/plan/answer", url.Values{"csrf": {s.csrf}, "project": {"linkshort"}, "revision": {"2"},
			"index": {"1"}, "decision": {"confirm"}}), ck)
		if press.Code != http.StatusConflict || press.Body.String() != want+"\n" {
			t.Errorf("the press: %d %q; want 409 %q", press.Code, press.Body, want)
		}
		_, err := r.c.doAnswerPlanQuestions(context.Background(), actor{s: s, via: "chat"}, "t-plan", 2,
			[]PlanAnswerInput{{Question: 1, Decision: factoryd.DecisionConfirm}})
		var ref *refusal
		if !errors.As(err, &ref) || ref.status != http.StatusConflict || ref.msg != want {
			t.Errorf("the batch: %v; want 409 %q", err, want)
		}
	}
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, s := r.loggedIn(t)
	refuse(t, r, ck, s, "2 hold-outs wait for your okay or drop below.")
	one := newRig(t)
	holdoutsRig(one, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	one.tasks.proofs["t-plan"][2].State = factoryd.ProofDropped
	ck1, s1 := one.loggedIn(t)
	refuse(t, one, ck1, s1, "1 hold-out waits for your okay or drop below.")
}

// The hold-outs zone carries a stable anchor and the console's script
// brings it into view when the flash shows the hold-outs refusal: the
// served script holds the handler that, on the hold-outs sentence, loads
// the pressed card's drawer when it is closed and scrolls the zone into
// view, asserted as the suite asserts the script's other handlers, by
// the marker strings the served file holds.
func TestRefusalScrolls(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	if card := planDrawer(t, r, ck); !strings.Contains(card, `<div class="holdouts" id="holdouts-t-plan-d">`) {
		t.Errorf("the drawer's zone carries no anchor id:\n%s", card)
	}
	js := r.do(httptest.NewRequest(http.MethodGet, "/static/console.js", nil), ck).Body.String()
	for _, want := range []string{
		`function holdoutsRefused(e)`,
		`xhr.responseText.indexOf("okay or drop below") < 0`,
		`document.getElementById("holdouts-" + id + "-d") || document.getElementById("holdouts-" + id)`,
		`zone.scrollIntoView({ block: "center" })`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the served script lacks %q", want)
		}
	}
}

// A hold-out decided by its inline button records the decision and the
// view shows the new state without a page reload: the okay answers the
// board-refresh alone, nothing is posted on the issue, the event feed
// carries the click, and the row's line turns where it sits while the
// other proposed row keeps its clicks.
func TestHoldoutInlineUpdate(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, s := r.loggedIn(t)
	press := r.do(htmxPost("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"2"}, "verb": {"okay"},
		"project": {"linkshort"}}), ck)
	if !pressRefreshes(press) {
		t.Fatalf("the inline okay: %d %s", press.Code, press.Body)
	}
	if !slices.Contains(r.fg.calls, "SIGNAL holdout_decided t-plan 2 2 okay by sebastian") || len(r.fg.comments) != 0 ||
		!slices.ContainsFunc(r.events.rows, func(e factoryd.Event) bool { return e.Kind == factoryd.EventHoldoutDecided }) {
		t.Fatalf("the decision's records: calls %v, comments %v", r.fg.calls, r.fg.comments)
	}
	card := planDrawer(t, r, ck)
	for _, want := range []string{
		`<legend>Hold-out 1</legend>`,
		`<p class="claim">The door stays shut until a key that fits turns the lock</p>`,
		"kept out: once the plan&#39;s foundation is on main, or at once when the plan has none, the prover writes its test, hidden from the agent. A hold-out okayed after the prover files gets no test.",
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="3">`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the card after the okay lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, `<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2">`) ||
		strings.Count(card, `action="/tasks/t-plan/plan/holdout"`) != 1 {
		t.Errorf("the okayed row keeps no click and one proposed row stays:\n%s", card)
	}
}

// The hold-outs zone carries its own anchor on every copy that holds it,
// and the served script moves a refused confirm there: the pressed card's
// zone scrolls into view, a card the page no longer holds loads its drawer
// first and scrolls the zone it lands on.
func TestTheHoldoutsZoneAnchorAndItsScript(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	if face := needsOf(t, r, ck, "/?project=linkshort"); !strings.Contains(face, `<div class="holdouts" id="holdouts-t-plan">`) {
		t.Errorf("the face's zone lacks its anchor:\n%s", face)
	}
	if card := planDrawer(t, r, ck); !strings.Contains(card, `<div class="holdouts" id="holdouts-t-plan-d">`) {
		t.Errorf("the drawer's zone lacks its anchor:\n%s", card)
	}
	js := r.do(httptest.NewRequest(http.MethodGet, "/static/console.js", nil), ck).Body.String()
	for _, want := range []string{
		`function holdoutsRefused(e)`,
		`xhr.responseText.indexOf("okay or drop below") < 0`,
		`card.querySelector('[id^="holdouts-"]')`,
		`zone.scrollIntoView({ block: "center" })`,
		`document.getElementById("holdouts-" + id + "-d") || document.getElementById("holdouts-" + id)`,
		`location.hash = "#card=" + encodeURIComponent(id)`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the served script lacks %q", want)
		}
	}
}
