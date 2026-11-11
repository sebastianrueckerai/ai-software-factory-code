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
// out in round 1, two proposed in round 2.
func holdoutsRig(r *rig, confirmation factoryd.PlanQuestion) {
	planRig(r)
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

// planDrawer is the drawer of plan t-plan: the plan card's face on the board
// holds no raw text, so the hold-outs and the summary sit here.
func planDrawer(t *testing.T, r *rig, ck *http.Cookie) string {
	t.Helper()
	rec := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-plan/drawer?project=linkshort", nil), ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("drawer of t-plan = %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

// The plan's drawer holds the round's hold-outs after its questions, this
// round's rows only, each with its claim, inputs and expected numbers and
// its state's one line, and an Okay and a Drop on a proposed one; the
// forms carry the row's own index, the number the gate and factoryd know
// it by, and the confirmation stays the card's only open question. A
// proof's code never reaches the card, an okayed or stored row takes no
// click, and a state without a line shows none.
func TestPlanCardHoldsTheHoldoutZone(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"})
	r.tasks.proofs["t-plan"][2].State = factoryd.ProofOkayed
	r.tasks.proofs["t-plan"] = append(r.tasks.proofs["t-plan"], factoryd.PlanProof{Idx: 4, Round: 2, ID: "lock_log",
		Origin: factoryd.ProofOriginHoldout, Claim: "Every lock turn lands in the log", Inputs: "one turn", Expected: "one row",
		State: factoryd.ProofStored, Path: "proofs/lock_log/lock_log_test.go", Suite: "./proofs/lock_log", Name: "TestLockLog", Code: "func TestLockLog(t *testing.T) {}"})
	ck, _ := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	for _, want := range []string{
		`<div class="holdouts">`,
		`<p class="meta">Hold-out criteria, kept out of the plan and run at the gate, hidden from the agent.</p>`,
		`<legend>Hold-out 1: The door stays shut until a key that fits turns the lock</legend>`,
		`<p class="meta">Inputs: POST /doors/1/open with key 9<br>Expected: 403 and the door row still shut</p>`,
		"proposed: okay keeps it out of the plan. The prover writes its test, hidden from the agent, unless it filed before the okay.",
		`<legend>Hold-out 2: Every key turn lands in the log</legend>`,
		"kept out: once the plan&#39;s foundation is on main, or at once when the plan has none, the prover writes its test, hidden from the agent. A hold-out okayed after the prover files gets no test.",
		`<div class="choices" role="group" aria-label="Keep the hold-out out or drop it">`,
		`<input type="hidden" name="revision" value="2"><input type="hidden" name="index" value="2">`,
		`<button type="submit" name="verb" value="okay">Okay</button>`,
		`<button type="submit" name="verb" value="drop" class="deny">Drop</button>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("plan card lacks %q:\n%s", want, card)
		}
	}
	for _, leak := range []string{"TestSecret", "TestLockLog", "The old round", `name="index" value="3"`, `name="index" value="4"`, `<p class="meta"></p>`} {
		if strings.Contains(card, leak) {
			t.Errorf("the card holds %q:\n%s", leak, card)
		}
	}
	if strings.Count(card, `action="/tasks/t-plan/plan/holdout"`) != 2 {
		t.Errorf("want 2 hold-out forms, the proposal's okay and drop:\n%s", card)
	}
	if i, q := strings.Index(card, `class="holdouts"`), strings.LastIndex(card, `action="/tasks/t-plan/plan/answer"`); !(q < i) || strings.Contains(card, `action="/tasks/t-plan/plan/changes"`) {
		t.Errorf("the hold-out zone sits at %d, the last answer form %d; want the zone after the answers and the confirmation's Request changes the only one:\n%s", i, q, card)
	}
}

// Once the round's confirmation is answered, its hold-out rows are settled:
// the card still lists them with their state's one line, but no click works.
func TestPlanCardHoldoutsSettleWithTheConfirmation(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?",
		Decision: factoryd.DecisionConfirm, AnsweredBy: "sebastian"})
	ck, s := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	if !strings.Contains(card, `<legend>Hold-out 1: The door stays shut until a key that fits turns the lock</legend>`) ||
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
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation, Text: "Does the plan match the spec?"})
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
		strings.Count(card, `action="/tasks/t-plan/plan/holdout"`) != 2 {
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
