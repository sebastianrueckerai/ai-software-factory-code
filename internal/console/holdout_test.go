package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sebastianrueckerai/ai-software-factory-code/internal/factoryd"
)

// confirmText is the confirmation's stored text, factoryd's multi-line
// shape: the decision line, then the spec, the criteria and the later
// rounds, each on its own line.
func confirmText() string {
	return "Confirmation of the spec and the criteria of plan revision 2: Confirm the plan fixes the spec and the criteria; Ask for changes sends your words to the next planner round. The criteria are fixed only when this is confirmed.\n" +
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

// pages are the opened card of plan t-plan at each page of its carousel,
// in order, by its arrows.
func planPages(t *testing.T, r *rig, ck *http.Cookie) []string {
	t.Helper()
	return cardPages(t, r, ck, "t-plan", "linkshort")
}

// Each extra promise of the round waits on a page of its own, before the
// confirmation: the small card opens on the first of 3 pages and asks to
// keep it by its claim; on the opened card each page asks for its own
// claim, tells what an extra promise is,
// names its claim whole and what keeping it costs, and offers Keep it and
// Drop it, which post the row's own index with no field to fill. The
// confirmation's page comes last and offers Ask for changes and Not now,
// its why naming the 2 extra promises it waits on, and no text names
// Confirm. The full brief lists the extra
// promises with their state and holds no form; a proof's code, an earlier
// round's row and the old zone with its script reach no page.
func TestEachExtraPromiseAsksOnItsOwnPage(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	small := needsOf(t, r, ck, "/?project=linkshort")
	for _, want := range []string{`data-case="500.1"`, `<strong class="title">Keep the extra promise “The door stays shut until a key that fits turns the lock” for plan “Link shortener”?</strong>`,
		`<span class="why">The plan&#39;s cards start only once each extra promise is kept or dropped.</span>`, `<span class="pos" aria-live="polite">1 of 3</span>`,
		`hx-get="/projects/linkshort/cards/t-plan/cases/500.2?project=linkshort"`} {
		if !strings.Contains(small, want) {
			t.Errorf("the small card lacks %s:\n%s", want, small)
		}
	}
	if strings.Contains(small, "<form") {
		t.Errorf("the small card holds a form:\n%s", small)
	}
	pages := planPages(t, r, ck)
	if len(pages) != 3 {
		t.Fatalf("the card holds %d pages, want 3", len(pages))
	}
	for i, c := range []struct{ ask, claim string }{
		{`<p class="o-question">Keep the extra <span class="term">promise</span> “The door stays shut until a key that fits turns the lock” for plan “Link shortener”?</p>`,
			"The door stays shut until a key that fits turns the lock."},
		{`<p class="o-question">Keep the extra <span class="term">promise</span> “Every key turn lands in the log” for plan “Link shortener”?</p>`, "Every key turn lands in the log."},
	} {
		page := pages[i]
		for _, want := range []string{
			fmt.Sprintf("Your call · %d of 3", i+1),
			c.ask,
			"<li>An extra promise is one the plan&#39;s own list missed, and a test the agent never sees checks it.</li>",
			"<li>The extra promise: " + c.claim + "</li>",
			"<li>Keeping it adds one more test, which the plan&#39;s last card must pass.</li>",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("page %d lacks %q:\n%s", i+1, want, page)
			}
		}
		for key, verb := range map[string]string{factoryd.AnswerKeepHoldout: "okay", factoryd.AnswerDropHoldout: "drop"} {
			p := answerForm(t, page, key)
			if want := (url.Values{"revision": {"2"}, "index": {strconv.Itoa(i + 2)}, "verb": {verb}}); p.Action != "/tasks/t-plan/plan/holdout" ||
				p.Values.Get("revision") != want.Get("revision") || p.Values.Get("index") != want.Get("index") || p.Values.Get("verb") != verb || p.Values.Has("note") {
				t.Errorf("page %d: %s posts %s %v; want the hold-out door with %v", i+1, key, p.Action, p.Values, want)
			}
		}
	}
	confirm := pages[2]
	for _, want := range []string{"Your call · 3 of 3", `<p class="o-question">Does plan “Link shortener” need changes before its 2 extra <span class="term">promises</span> are settled?</p>`,
		`<p class="o-askwhy">The plan&#39;s cards start only once its 2 extra promises are kept or dropped.</p>`, `data-answer="revise"`,
		"<h4>The extra promises</h4>", `<p class="claim">Every key turn lands in the log <span class="meta">waits for your answer</span></p>`} {
		if !strings.Contains(confirm, want) {
			t.Errorf("the confirmation's page lacks %q:\n%s", want, confirm)
		}
	}
	if strings.Contains(askBlockOf(t, confirm), `data-answer="confirm"`) || strings.Contains(askBlockOf(t, confirm), "Confirm the plan") {
		t.Errorf("the confirmation offers Confirm while extra promises wait:\n%s", askBlockOf(t, confirm))
	}
	// Not now keeps the plan as it is: a note on its confirmation's case
	if p := answerForm(t, confirm, factoryd.ActNote); p.Label != "Not now" || p.Action != "/tasks/t-plan/resolve" || p.Values.Get("action") != factoryd.ActNote ||
		p.Values.Get("case_id") == "" || !strings.Contains(confirm, `<span class="effect">The plan&#39;s cards wait until its 2 extra promises are kept or dropped.</span>`) {
		t.Errorf("the confirmation's Not now posts %s %v, label %q; want a note on its case and the effect that names what waits:\n%s", p.Action, p.Values, p.Label, askBlockOf(t, confirm))
	}
	for i, page := range pages {
		for _, leak := range []string{"TestSecret", "The old round", `id="holdouts-`, `action="/tasks/t-plan/plan/holdout"><input type="hidden" name="csrf"`} {
			if strings.Contains(page, leak) && !(leak == `action="/tasks/t-plan/plan/holdout"><input type="hidden" name="csrf"` && i < 2) {
				t.Errorf("page %d holds %q:\n%s", i+1, leak, page)
			}
		}
	}
	if js := r.do(httptest.NewRequest(http.MethodGet, "/static/console.js", nil), ck).Body.String(); strings.Contains(js, "holdouts") {
		t.Error("the served script still reads the old hold-out zone")
	}
}

// Once the round's confirmation is answered, its hold-outs ask nothing:
// the card holds no page of them, the full brief lists them with their
// state, and the answered confirmation reads its first line only. A late
// okay is refused.
func TestPlanCardHoldoutsSettleWithTheConfirmation(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation,
		Decision: factoryd.DecisionConfirm, AnsweredBy: "sebastian"})
	ck, s := r.loggedIn(t)
	card := planDrawer(t, r, ck)
	if !strings.Contains(card, `<p class="claim">The door stays shut until a key that fits turns the lock <span class="meta">waits for your answer</span></p>`) ||
		strings.Contains(card, `action="/tasks/t-plan/plan/holdout"`) || strings.Contains(card, "Keep this extra promise") {
		t.Errorf("the settled round's card lists the extra promises and asks nothing of them:\n%s", card)
	}
	if !strings.Contains(card, `<p class="answered">Confirmation of the spec and the criteria of plan revision 2: Confirm the plan fixes the spec and the criteria; Ask for changes sends your words to the next planner round. The criteria are fixed only when this is confirmed.: confirm (sebastian)</p>`) {
		t.Errorf("the answered confirmation reads its first line only:\n%s", card)
	}
	rec := r.do(post("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {"2"}, "index": {"2"}, "verb": {"okay"},
		"project": {"linkshort"}}), ck)
	if rec.Code != http.StatusConflict || rec.Body.String() != "The confirmation of plan #1 is answered already, and its extra promises are settled.\n" {
		t.Fatalf("okay after the confirmation = %d %q", rec.Code, rec.Body)
	}
}

// A claim the prover wrote across lines reads whole on its page, its lines
// joined into the story's one line.
func TestACompoundHoldoutClaimRendersWhole(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	r.tasks.proofs["t-plan"][1].Claim = "The door stays shut.\nThe key that fits turns the lock."
	ck, _ := r.loggedIn(t)
	if card := planDrawer(t, r, ck); !strings.Contains(card, "<li>The extra promise: The door stays shut. The key that fits turns the lock.</li>") {
		t.Errorf("the compound claim does not read whole:\n%s", card)
	}
}

// Keep it and Drop it store the decision from their page and show the
// board again: nothing is posted on the issue, the event feed carries the
// press, and the kept promise leaves the pages. A second press on the row,
// one on no row, one on a stale revision and a verb that is neither okay
// nor drop are refused. Once both are decided and the plan confirmed, no
// page of them stands and the full brief names them kept and dropped.
func TestPlanHoldoutClicksDecideTheRound(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, s := r.loggedIn(t)
	click := func(revision, index, verb string) *httptest.ResponseRecorder {
		return r.do(post("/tasks/t-plan/plan/holdout", url.Values{"csrf": {s.csrf}, "revision": {revision}, "index": {index}, "verb": {verb},
			"project": {"linkshort"}}), ck)
	}
	keep := answerForm(t, planDrawer(t, r, ck), factoryd.AnswerKeepHoldout)
	keep.Values.Set("csrf", s.csrf)
	keep.Values.Set("project", "linkshort")
	if rec := r.do(post(keep.Action, keep.Values), ck); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?project=linkshort" ||
		!slices.Contains(r.fg.calls, "SIGNAL holdout_decided t-plan 2 2 okay by sebastian") || len(r.fg.comments) != 0 ||
		!slices.ContainsFunc(r.events.rows, func(e factoryd.Event) bool { return e.Kind == factoryd.EventHoldoutDecided }) {
		t.Fatalf("Keep it: %d to %q, calls %v, comments %v; body %s", rec.Code, rec.Header().Get("Location"), r.fg.calls, r.fg.comments, rec.Body)
	}
	if pages := planPages(t, r, ck); len(pages) != 2 || !strings.Contains(pages[0], "<li>The extra promise: Every key turn lands in the log.</li>") ||
		!strings.Contains(pages[1], `<p class="claim">The door stays shut until a key that fits turns the lock <span class="meta">kept</span></p>`) {
		t.Errorf("after Keep it the card holds %d pages:\n%s", len(pages), strings.Join(pages, "\n----\n"))
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
	if card := planDrawer(t, r, ck); strings.Contains(card, `action="/tasks/t-plan/plan/holdout"`) || !strings.Contains(card, `<span class="meta">dropped</span>`) {
		t.Errorf("the card after the drop:\n%s", card)
	}
}

// A Confirm answered while the round's extra promises wait is refused with
// their count and where they are, on the card's press and in a chat batch:
// the rig's two proposed hold-outs of round 2 name two, a rig with one
// names one.
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
	refuse(t, r, ck, s, "2 extra promises of plan #1 wait for Keep it or Drop it on their pages before the confirmation.")
	one := newRig(t)
	holdoutsRig(one, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	one.tasks.proofs["t-plan"][2].State = factoryd.ProofDropped
	ck1, s1 := one.loggedIn(t)
	refuse(t, one, ck1, s1, "1 extra promise of plan #1 waits for Keep it or Drop it on its page before the confirmation.")
}
