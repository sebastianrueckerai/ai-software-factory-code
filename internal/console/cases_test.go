package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	at := func(id int64, task, problem string, d time.Duration) factoryd.Case {
		return factoryd.Case{ID: id, TaskID: task, Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: problem, OpenedAt: opened.Add(d)}
	}
	r.openCases = map[string][]factoryd.Case{
		"t-a": {at(1, "t-a", "ask one", 0), at(2, "t-a", "ask two", time.Hour), at(3, "t-a", "ask three", 2*time.Hour)},
		"t-b": {at(4, "t-b", "ask b", 150*time.Minute)},
	}
}

func getAs(r *rig, ck *http.Cookie, path string, htmx bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	return r.do(req, ck)
}

func needsOf(t *testing.T, r *rig, ck *http.Cookie, path string) string {
	t.Helper()
	return col(t, getAs(r, ck, path, false).Body.String(), "Needs you")
}

func caseURL(card string, id int) string {
	return "/projects/" + casesProject + "/cards/" + card + "/cases/" + string(rune('0'+id))
}

// A card that holds cases at the human is one row of Needs you, the card
// with the oldest face first, showing its newest case with its place among
// them; the carousel appears for more than one case and its arrows address
// the neighbours by case id.
func TestNeedsYouShowsOneRowPerCardAndACarouselForMany(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	if n := strings.Count(needs, `class="card case"`); n != 2 {
		t.Fatalf("Needs you holds %d case rows, want one per card (2):\n%s", n, needs)
	}
	a, b := strings.Index(needs, `data-card="t-a"`), strings.Index(needs, `data-card="t-b"`)
	if a < 0 || b < 0 || a > b {
		t.Errorf("t-a (oldest case 3 h) at %d, t-b (30 min) at %d; want the oldest first", a, b)
	}
	rowA := needs[a:b]
	for _, want := range []string{`<p class="ask">ask three</p>`, `<span class="pos" aria-live="polite">1 of 3</span>`, `id="case-prev-t-a" aria-label="Previous case" disabled`,
		`id="case-next-t-a" aria-label="Next case" hx-get="/projects/` + casesProject + `/cards/t-a/cases/2?project=` + casesProject + `"`, ` data-place="case `, ` data-case="3"`} {
		if !strings.Contains(rowA, want) {
			t.Errorf("t-a's row lacks %q:\n%s", want, rowA)
		}
	}
	if strings.Count(rowA, `<p class="ask">`) != 1 || strings.Contains(rowA, "ask one") {
		t.Errorf("t-a's row shows more than its one case:\n%s", rowA)
	}
	if rowB := needs[b:]; !strings.Contains(rowB, `<p class="ask">ask b</p>`) || strings.Contains(rowB, `class="carousel"`) || strings.Contains(rowB, `class="pos"`) {
		t.Errorf("t-b's single case shows a count or arrows:\n%s", rowB)
	}

	next := getAs(r, ck, caseURL("t-a", 2), true)
	if next.Code != http.StatusOK {
		t.Fatalf("case 2 = %d %s", next.Code, next.Body)
	}
	for _, want := range []string{`<p class="ask">ask two</p>`, `<span class="pos" aria-live="polite">2 of 3</span>`,
		`id="case-prev-t-a" aria-label="Previous case" hx-get="/projects/` + casesProject + `/cards/t-a/cases/3?`,
		`id="case-next-t-a" aria-label="Next case" hx-get="/projects/` + casesProject + `/cards/t-a/cases/1?`} {
		if !strings.Contains(next.Body.String(), want) {
			t.Errorf("case 2 lacks %q:\n%s", want, next.Body)
		}
	}
	if strings.Contains(next.Body.String(), "<html") {
		t.Errorf("case 2 is not the card's body alone:\n%s", next.Body)
	}
	for _, path := range []string{caseURL("t-a", 4), caseURL("t-a", 0), "/projects/" + casesProject + "/cards/t-a/cases/x", caseURL("t-zz", 1)} {
		if rec := getAs(r, ck, path, true); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

// A card of another project is no case of the project the route names.
func TestCaseRouteRefusesAnotherProjectsCard(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	withLinkshort(r)
	ck, _ := r.loggedIn(t)
	if rec := getAs(r, ck, "/projects/linkshort/cards/t-a/cases/1", true); rec.Code != http.StatusNotFound {
		t.Errorf("t-a under linkshort = %d, want 404", rec.Code)
	}
}

// A board read that names the case a card showed keeps it while it is at
// the human, and falls back to the newest once it is not; an event's card
// read does the same, and the wide view's card read is the plain face.
func TestBoardReadKeepsTheCaseInView(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/?case=t-a:2&case=t-b:4"); !strings.Contains(needs, `<p class="ask">ask two</p>`) || !strings.Contains(needs, `2 of 3`) {
		t.Errorf("the board dropped the case in view:\n%s", needs)
	}
	if needs := needsOf(t, r, ck, "/?case=t-a:99"); !strings.Contains(needs, `<p class="ask">ask three</p>`) {
		t.Errorf("a case no longer at the human kept its place:\n%s", needs)
	}
	frag := getAs(r, ck, "/cards/t-a?case=t-a:1", false).Body.String()
	if !strings.HasPrefix(frag, `<article class="card case"`) || !strings.Contains(frag, `<p class="ask">ask one</p>`) || !strings.Contains(frag, ` data-place="case `) {
		t.Errorf("the card fragment is not the case in view with its place:\n%s", frag)
	}
	if wide := getAs(r, ck, "/cards/t-a?view=wide", false).Body.String(); !strings.HasPrefix(wide, `<article class="card face`) {
		t.Errorf("the wide view's card fragment is not the plain face:\n%s", wide)
	}
}

// The wide view lists its Escalations from the same case rows as the
// column, and keeps the card's face in its state column.
func TestWideViewListsTheCaseRows(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	body := getAs(r, ck, "/?view=wide", false).Body.String()
	esc := col(t, body, "Escalations")
	if strings.Count(esc, `class="card case"`) != 2 || !strings.Contains(esc, `<p class="ask">ask three</p>`) || strings.Contains(esc, ` data-place="`) {
		t.Errorf("the wide view's Escalations are not the case rows:\n%s", esc)
	}
	if waiting := col(t, body, "Waiting on a human"); strings.Count(waiting, `<article class="card face`) != 2 {
		t.Errorf("the state column lost the cards' faces:\n%s", waiting)
	}
}

// The answer forms sit on the case that is the card's live ask, the newest
// live case an answer ends, and nowhere else; a live ask still with the
// ladder leaves every row without forms, and a post that names another
// case is refused with the case in view.
func TestAnswerFormsSitOnTheLiveAsksCaseOnly(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.openCases["t-a"] = append(r.openCases["t-a"], factoryd.Case{ID: 9, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseOpen, Problem: "still with the ladder"})
	ck, s := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/"); strings.Contains(needs[strings.Index(needs, `data-card="t-a"`):strings.Index(needs, `data-card="t-b"`)], `action="/tasks/t-a/resolve"`) {
		t.Errorf("a row carries answer forms while the card's live ask is open with the ladder:\n%s", needs)
	}
	r.openCases["t-a"] = r.openCases["t-a"][:3]
	forms := func(path string) bool {
		needs := needsOf(t, r, ck, path)
		return strings.Contains(needs[strings.Index(needs, `data-card="t-a"`):strings.Index(needs, `data-card="t-b"`)], `action="/tasks/t-a/resolve"`)
	}
	if !forms("/") {
		t.Error("the newest case, the card's live ask, carries no answer form")
	}
	if forms("/?case=t-a:2") {
		t.Error("case 2 carries answer forms though case 3 is the live ask")
	}
	if frag := getAs(r, ck, caseURL("t-a", 3), true).Body.String(); !strings.Contains(frag, `<input type="hidden" name="case_id" value="3">`) {
		t.Errorf("the live ask's forms do not name its case:\n%s", frag)
	}
	rec := r.do(post("/tasks/t-a/answer", url.Values{"csrf": {s.csrf}, "project": {casesProject}, "answer": {"x"}, "case": {"2"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Case 2 is not the card's live ask: the card's live ask is case 3") {
		t.Errorf("an answer from another case = %d %q, want 409 naming case 3", rec.Code, rec.Body)
	}
}

// A board read reads the cases in one call and builds no digest.
func TestBoardReadMakesOneCaseReadAndNoDigest(t *testing.T) {
	r := newRig(t)
	var tasks []factoryd.Task
	r.openCases = map[string][]factoryd.Case{}
	for i := range 10 {
		id := "t-" + string(rune('a'+i))
		tasks = append(tasks, factoryd.Task{ID: id, Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: int64(30 + i)})
		r.openCases[id] = []factoryd.Case{{ID: int64(2*i + 1), TaskID: id, Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "p"},
			{ID: int64(2*i + 2), TaskID: id, Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "q"}}
	}
	r.tasks.set("asks", tasks...)
	var digests, desks, steps int
	cfg := &r.c.cfg
	digest, desk, step := cfg.CardDigest, cfg.CaseDesk, cfg.CaseSteps
	cfg.CardDigest = func(ctx context.Context, id string) (factoryd.CardDigest, error) { digests++; return digest(ctx, id) }
	cfg.CaseDesk = func(ctx context.Context, repo string) (factoryd.CaseDesk, error) { desks++; return desk(ctx, repo) }
	cfg.CaseSteps = func(ctx context.Context, id int64) ([]factoryd.CaseStep, error) { steps++; return step(ctx, id) }
	ck, _ := r.loggedIn(t)
	if n := strings.Count(needsOf(t, r, ck, "/"), `class="card case"`); n != 10 {
		t.Fatalf("%d rows, want 10", n)
	}
	if digests != 0 || desks != 1 || steps != 0 {
		t.Errorf("a board read of 10 rows of 2 cases made %d digests, %d case-desk reads and %d step reads, want 0, 1 and 0", digests, desks, steps)
	}
}

// A card is counted once: a merge card with its case reads as many need
// you as the column holds, and a running card with a case at the human
// shows in Needs you alone.
func TestACardWithACaseCountsOnceAndShowsOnce(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-mc", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10, BudgetEUR: 2},
		factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 9, BudgetEUR: 2})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 61, Kind: factoryd.ApprovalMerge, TaskID: "t-mc", Project: "sf/code",
		PRNumber: 10, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	r.openCases = map[string][]factoryd.Case{
		"t-mc":  {{ID: 5, TaskID: "t-mc", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman, Problem: "Pull request #10 waits for a merge approval", Facts: map[string]any{"approval": 61.0}}},
		"t-run": {{ID: 6, TaskID: "t-run", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman, Problem: "The monitor flagged this card."}},
	}
	ck, _ := r.loggedIn(t)
	body := getAs(r, ck, "/", false).Body.String()
	needs := col(t, body, "Needs you")
	n := regexp.MustCompile(`Needs you <span class="count">(\d+)</span>`).FindStringSubmatch(body)
	if n == nil || !regexp.MustCompile(`<b class="n">`+n[1]+`</b> need you`).MatchString(body) {
		t.Errorf("the status line does not count the column's %v cards once", n)
	}
	if strings.Contains(col(t, body, "In progress"), `data-card="t-run"`) || !strings.Contains(needs, `data-card="t-run"`) {
		t.Errorf("the running card with a case shows outside Needs you alone")
	}
	// the merge approval sits inside its case's slot, once, with the writer's link
	merge, run := strings.Index(needs, `data-card="t-mc"`), strings.Index(needs, `data-card="t-run"`)
	at := strings.Index(needs, `action="/approvals/61/approve"`)
	if strings.Count(needs, `action="/approvals/61/approve"`) != 1 || at < merge || at > run || !strings.Contains(needs, `<a href="#card=t-mc" class="open">Merge</a>`) {
		t.Errorf("the merge approval is not once in its case's slot, with the writer's link:\n%s", needs)
	}
}

// A plan's confirmation case shows its own slot: the plan's one line and
// Confirm with Request changes; the plan's approval card sits under the
// carousel once and holds the decision's layers — the lead line and the
// hold-outs the Confirm waits on, with their clicks — and no other raw
// text of the revision: not its summary, its spec, its criteria.
func TestAPlansCaseHoldsItsApprovalOnce(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/?project=linkshort")
	if strings.Count(needs, `class="card case"`) != 1 || strings.Count(needs, `class="card approval plan"`) != 1 {
		t.Fatalf("want one case row holding one plan card:\n%s", needs)
	}
	if n := strings.Count(needs[strings.Index(needs, `<article class="card approval plan"`):], "</article>"); n != 2 {
		t.Errorf("the plan card does not sit inside its case's article (%d closings after it, want 2):\n%s", n, needs)
	}
	for _, want := range []string{`<p class="ask">Confirm the spec and the criteria, or request changes.</p>`, `<p class="why">Plan #1, revision 2: `,
		`<p class="lead">Confirm fixes the spec and the criteria; Request changes sends your words back to the planner.</p>`,
		`<div class="holdouts" id="holdouts-t-plan">`,
		`<p class="claim">The door stays shut until a key that fits turns the lock</p>`,
		`action="/tasks/t-plan/plan/holdout"`} {
		if !strings.Contains(needs, want) {
			t.Errorf("the plan's case lacks %q:\n%s", want, needs)
		}
	}
	if n := strings.Count(needs, `action="/tasks/t-plan/plan/answer"`); n != 2 {
		t.Errorf("the slot holds the confirmation's two forms and the card holds none more (%d):\n%s", n, needs)
	}
	for _, raw := range []string{"The foundation lands first, with its migrations.", "On it, the links API and the acceptance tests close the round.",
		"A door opens only for a key that fits.", "key: the token that opens a door", "door_open (test)", "Still to plan"} {
		if strings.Contains(needs, raw) {
			t.Errorf("the plan's face holds %q:\n%s", raw, needs)
		}
	}
}

// Every text the face reads is scrubbed: URL userinfo and a bearer value
// come out masked in the finding (Why), the recommendation the ask carries
// and the ask's info.
func TestCaseFaceMasksEveryTextItReads(t *testing.T) {
	const url, bearer = "https://user:pass@git.example/x", "Authorization: Bearer abc123def456ghi789jkl012mno345"
	r := newRig(t)
	seatCases(r)
	r.caseSteps = map[int64][]factoryd.CaseStep{3: {
		{CaseID: 3, Rung: 1, Outcome: factoryd.StepHandedUp, Found: "It failed pulling " + url + ". It sent " + bearer + "."}}}
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{
		"info":           "The log says " + url + " and " + bearer,
		"recommendation": "handover: the fetch of " + url + " sent " + bearer, "recommended_by": "the factory agent"}}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	for _, want := range []string{"It failed pulling", "handover: the fetch of", "The log says"} {
		if !strings.Contains(needs, want) {
			t.Fatalf("the face lacks %q:\n%s", want, needs)
		}
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(needs, leak) {
			t.Errorf("the face holds %q:\n%s", leak, needs)
		}
	}
}

// The face's info is the ask's, with the last answer's time under it, and
// the card's life loads on demand behind a fold.
func TestCaseFaceInfoIsTheAsksAndTheLifeLoadsOnDemand(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{"info": "Postgres is shared."}}}
	r.caseAnswered = map[string]time.Time{"t-a": time.Date(2026, 11, 1, 21, 40, 0, 0, time.UTC)}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	for _, want := range []string{"Postgres is shared.", "Last answer processed: 2026-11-01 21:40 UTC",
		`<details class="fold" hx-get="/tasks/t-a/digest" hx-trigger="toggle once"`, "◷ 1 h 0 min"} {
		if !strings.Contains(needs, want) {
			t.Errorf("the face lacks %q:\n%s", want, needs)
		}
	}
}

// A card's row waits since its oldest case was faced: the ask's time, not
// the case's opening.
func TestNeedsYouOrdersRowsByTheOldestAsk(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	// t-b opened later but was asked longest ago
	r.caseAsks = map[int64]factoryd.CaseAsk{4: {At: time.Now().Add(-10 * time.Hour)}, 1: {At: time.Now().Add(-2 * time.Hour)}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	if a, b := strings.Index(needs, `data-card="t-a"`), strings.Index(needs, `data-card="t-b"`); b > a {
		t.Errorf("t-b (asked 10 h ago) at %d, t-a (2 h) at %d; want the oldest ask first", b, a)
	}
}

// A case row and a plain card face never share a place, so a card swap
// replaces like with like; the wide view's arrows keep the view, and the
// row they fetch has no place of its own there.
func TestCaseRowsHaveTheirOwnPlace(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	row := getAs(r, ck, "/cards/t-a", false).Body.String()
	r.openCases = nil
	r.tasks.set("the agent asks?", factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Card A"})
	r.openCases = map[string][]factoryd.Case{}
	face := getAs(r, ck, "/cards/t-a", false).Body.String()
	place := regexp.MustCompile(`data-place="([^"]*)"`)
	rp, fp := place.FindStringSubmatch(row), place.FindStringSubmatch(face)
	if rp == nil || fp == nil || rp[1] == fp[1] || !strings.HasPrefix(rp[1], "case ") {
		t.Errorf("the row's place %v and the face's %v must differ", rp, fp)
	}
	seatCases(r)
	wide := getAs(r, ck, "/projects/"+casesProject+"/cards/t-a/cases/2?view=wide", true).Body.String()
	if strings.Contains(wide, ` data-place=`) || !strings.Contains(wide, `<input type="hidden" name="view" value="wide">`) || !strings.Contains(wide, `/cases/3?`) ||
		!strings.Contains(wide, `view=wide`) {
		t.Errorf("the wide view's case fragment has a place or loses the view:\n%s", wide)
	}
}

// The answer forms need the case's comment to be the card's question's:
// a case asked on another comment carries none.
func TestAnswerFormsNeedTheQuestionsComment(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Card A"},
		Escalation: "the agent asks", QuestionComment: 42, Question: &factoryd.HumanQuestion{Ask: "Which?", Choices: []factoryd.QuestionChoice{{Value: "park", Label: "Park it"}}}}}
	r.openCases = map[string][]factoryd.Case{"t-a": {{ID: 1, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "Which?", CommentID: 41}}}
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/"); strings.Contains(needs, `action="/tasks/t-a/resolve"`) {
		t.Errorf("a case asked on comment 41 carries the forms of the question on comment 42:\n%s", needs)
	}
	r.openCases["t-a"][0].CommentID = 42
	if needs := needsOf(t, r, ck, "/"); !strings.Contains(needs, `action="/tasks/t-a/resolve"`) {
		t.Errorf("the case asked on the question's comment carries no forms:\n%s", needs)
	}
}

// A plan at its session cap has a session cap case and a pending plan
// approval that case does not answer: the card is one row, the approval
// inside it under the carousel, counted once.
func TestACardsApprovalWithNoCaseOfItsOwnSitsInItsRow(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 5, TaskID: "t-plan", Kind: factoryd.CaseSessionCap, Status: factoryd.CaseAtHuman, Problem: "The plan hit its session cap."}}}
	ck, _ := r.loggedIn(t)
	body := getAs(r, ck, "/?project=linkshort", false).Body.String()
	needs := col(t, body, "Needs you")
	row := needs[strings.Index(needs, `class="card case"`):]
	if strings.Count(needs, `class="card case"`) != 1 || strings.Count(needs, `class="card approval plan"`) != 1 || strings.Count(row, "</article>") != 2 {
		t.Fatalf("want one row holding the plan card once:\n%s", needs)
	}
	if strings.Index(row, `class="card approval plan"`) < strings.Index(row, `class="carousel"`) && strings.Contains(row, `class="carousel"`) {
		t.Errorf("the approval sits above the carousel")
	}
	if !regexp.MustCompile(`Needs you <span class="count">1</span>`).MatchString(body) || !regexp.MustCompile(`<b class="n">1</b> need you`).MatchString(body) {
		t.Errorf("the card is not counted once:\n%s", needs)
	}
}

// The row's waiting time is the case's, once, in its head.
func TestACaseRowShowsOneWaitingTime(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-28 * time.Minute)}, 1: {At: time.Now().Add(-95 * time.Minute)}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	row := needs[strings.Index(needs, `data-card="t-a"`):strings.Index(needs, `data-card="t-b"`)]
	if strings.Count(row, "◷") != 1 || !strings.Contains(row, "◷ 28 min") {
		t.Errorf("t-a's row does not show the case's one waiting time, 28 min:\n%s", row)
	}
}

// The ask and the money line are scrubbed too, and a recommendation shows
// with its writer or not at all.
func TestCaseFaceScrubsAskAndMoneyAndNamesTheRecommendationsWriter(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.openCases["t-a"][2].Problem = "Pulling https://user:pass@git.example/x failed"
	r.openCases["t-a"][2].Money = &factoryd.CaseMoney{Kind: "session_cap", BudgetEUR: 2, SpentEUR: 1, NeedEUR: 0.5, Buys: "a call with Authorization: Bearer abc123def456ghi789jkl012mno345"}
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now(), Detail: map[string]any{"recommendation": "park: wait", "recommended_by": "the check-in"}},
		4: {At: time.Now(), Detail: map[string]any{"recommendation": "no writer words"}}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	for _, leak := range []string{"user:pass", "abc123def456", "no writer words"} {
		if strings.Contains(needs, leak) {
			t.Errorf("the face holds %q:\n%s", leak, needs)
		}
	}
	if !strings.Contains(needs, `<p class="recommendation">Recommendation (the check-in): park: wait</p>`) || !strings.Contains(needs, "Pulling ") || !strings.Contains(needs, "1.00 of 2.00 EUR spent") {
		t.Errorf("the face lacks its recommendation with its writer, its ask or its money line:\n%s", needs)
	}
}

// A merge case's brief shows once on the board, as the face's info, and
// whole in the drawer, rendered as paragraphs with its secrets masked.
func TestAMergeCasesBriefShowsOnceOnTheBoardAndWholeInTheDrawer(t *testing.T) {
	const url, bearer = "https://user:pass@git.example/x", "Authorization: Bearer abc123def456ghi789jkl012mno345"
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-mc", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10, BudgetEUR: 2})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 61, Kind: factoryd.ApprovalMerge, TaskID: "t-mc", Project: "sf/code",
		PRNumber: 10, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	r.openCases = map[string][]factoryd.Case{"t-mc": {{ID: 5, TaskID: "t-mc", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman,
		Facts: map[string]any{"approval": 61.0}, Brief: "The card adds the retry.\n\nIts review approved the head. It cloned " + url + " with " + bearer}}}
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/"); strings.Count(needs, "The card adds the retry.") != 1 {
		t.Errorf("the board prints the brief's first line %d times, want once:\n%s", strings.Count(needs, "The card adds the retry."), needs)
	}
	drawer := r.do(httptest.NewRequest(http.MethodGet, "/cards/t-mc/drawer?tab=overview", nil), ck).Body.String()
	for _, want := range []string{"<p>The card adds the retry.</p>", "<p>Its review approved the head."} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the drawer lacks %q", want)
		}
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(drawer, leak) {
			t.Errorf("the drawer holds %q", leak)
		}
	}
}

// The face shows the case's brief as its info, the writer's words as the
// recommendation's grounds, and masks URL userinfo and a bearer value in both.
func TestCaseFaceShowsTheBriefMasked(t *testing.T) {
	const url, bearer = "https://user:pass@git.example/x", "Authorization: Bearer abc123def456ghi789jkl012mno345"
	r := newRig(t)
	seatCases(r)
	r.openCases["t-a"][2].Brief = "The fetch of " + url + " sent " + bearer + "."
	r.openCases["t-a"][2].BriefDetail = "Another session fixes both findings."
	r.caseSteps = map[int64][]factoryd.CaseStep{3: {{CaseID: 3, Rung: 2, Outcome: factoryd.StepHandedUp, Decision: "handover", Grounds: "old grounds"}}}
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{"info": "the digest's info",
		"recommendation": "handover: old grounds", "recommended_by": "the factory agent"}}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	for _, want := range []string{"The fetch of", "handover: Another session fixes both findings."} {
		if !strings.Contains(needs, want) {
			t.Errorf("the face lacks %q:\n%s", want, needs)
		}
	}
	for _, leak := range []string{"user:pass", "abc123def456", "the digest's info", "old grounds"} {
		if strings.Contains(needs, leak) {
			t.Errorf("the face holds %q:\n%s", leak, needs)
		}
	}
}

// A card that has ended keeps no row, so its pending approval, which the
// end missed, shows on its own.
func TestAFinalCardsApprovalStillShows(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-done", Kind: "agent", Status: "done", Repository: "sf/code", IssueNumber: 8, PRNumber: 10})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 62, Kind: factoryd.ApprovalMerge, TaskID: "t-done", Project: "sf/code",
		PRNumber: 10, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	r.openCases = map[string][]factoryd.Case{"t-done": {{ID: 6, TaskID: "t-done", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman, Problem: "flag"}}}
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/"); !strings.Contains(needs, `action="/approvals/62/approve"`) || strings.Contains(needs, `class="card case"`) {
		t.Errorf("the ended card's pending approval is not on the board alone:\n%s", needs)
	}
}

// A case's approval shows in the row only while its own case is in view,
// not under a sibling case.
func TestAnApprovalShowsOnlyUnderItsOwnCase(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-mc", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10, BudgetEUR: 2})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 61, Kind: factoryd.ApprovalMerge, TaskID: "t-mc", Project: "sf/code",
		PRNumber: 10, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	r.openCases = map[string][]factoryd.Case{"t-mc": {
		{ID: 5, TaskID: "t-mc", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman, Problem: "merge?", Facts: map[string]any{"approval": 61.0}},
		{ID: 6, TaskID: "t-mc", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman, Problem: "flag"}}}
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/"); strings.Contains(needs, `action="/approvals/61/approve"`) || !strings.Contains(needs, `<p class="ask">flag</p>`) {
		t.Errorf("the merge approval shows under the flag's case:\n%s", needs)
	}
	if needs := needsOf(t, r, ck, "/?case=t-mc:5"); !strings.Contains(needs, `action="/approvals/61/approve"`) {
		t.Errorf("the merge approval does not show under its own case:\n%s", needs)
	}
}

// An answer naming a case when the card has no live ask is refused with
// that said.
func TestAnAnswerNamingACaseWithNoLiveAskSaysSo(t *testing.T) {
	r := newRig(t)
	r.tasks.set("asks", factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21})
	r.openCases = map[string][]factoryd.Case{}
	ck, s := r.loggedIn(t)
	rec := r.do(post("/tasks/t-a/answer", url.Values{"csrf": {s.csrf}, "project": {casesProject}, "answer": {"x"}, "case": {"7"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "the card has no live ask") {
		t.Errorf("an answer naming case 7 = %d %q, want 409 saying the card has no live ask", rec.Code, rec.Body)
	}
}
