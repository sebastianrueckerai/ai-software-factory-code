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
	// a card holds one live ask, and beside it the monitor's flags, which
	// are answered by their own number
	flag := func(id int64, task, name string, d time.Duration) factoryd.Case {
		k := at(id, task, "", d)
		k.Kind, k.Facts = factoryd.CaseMonitorFlag, map[string]any{"flag": name}
		return k
	}
	r.openCases = map[string][]factoryd.Case{
		"t-a": {flag(1, "t-a", "session_no_push", 0), flag(2, "t-a", "wait_over_bound", time.Hour), at(3, "t-a", "ask three", 2*time.Hour)},
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
// with the oldest face first, showing its oldest case with its place among
// them; the carousel appears for more than one case and its arrows address
// the neighbours by case id.
func TestNeedsYouShowsOneRowPerCardAndACarouselForMany(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	if n := strings.Count(needs, ` data-case="`); n != 2 {
		t.Fatalf("Needs you holds %d case rows, want one per card (2):\n%s", n, needs)
	}
	a, b := strings.Index(needs, `data-card="t-a"`), strings.Index(needs, `data-card="t-b"`)
	if a < 0 || b < 0 || a > b {
		t.Fatalf("t-a (oldest case 3 h) at %d, t-b (30 min) at %d; want the oldest first", a, b)
	}
	rowA := needs[a:b]
	for _, want := range []string{`<span class="why">A session that changes no code spends money and moves the work no closer to done.</span>`, `<span class="pos" aria-live="polite">1 of 3</span>`, `id="ask-prev-t-a" aria-label="The ask before" disabled`,
		`id="ask-next-t-a" aria-label="The ask after" hx-get="/projects/` + casesProject + `/cards/t-a/cases/2?project=` + casesProject + `"`, ` data-place="case `, ` data-case="1"`} {
		if !strings.Contains(rowA, want) {
			t.Errorf("t-a's row lacks %q:\n%s", want, rowA)
		}
	}
	if strings.Count(rowA, `class="title"`) != 1 || strings.Contains(rowA, "Ask three.") {
		t.Errorf("t-a's row shows more than its one case:\n%s", rowA)
	}
	if rowB := needs[b:]; !strings.Contains(rowB, `<span class="why">Ask b.</span>`) || strings.Contains(rowB, `class="carousel"`) || strings.Contains(rowB, `class="pos"`) {
		t.Errorf("t-b's single case shows a count or arrows:\n%s", rowB)
	}

	next := getAs(r, ck, caseURL("t-a", 2), true)
	if next.Code != http.StatusOK {
		t.Fatalf("case 2 = %d %s", next.Code, next.Body)
	}
	for _, want := range []string{`<span class="why">A card that waits this long may be stuck.</span>`, `<span class="pos" aria-live="polite">2 of 3</span>`,
		`id="ask-prev-t-a" aria-label="The ask before" hx-get="/projects/` + casesProject + `/cards/t-a/cases/1?`,
		`id="ask-next-t-a" aria-label="The ask after" hx-get="/projects/` + casesProject + `/cards/t-a/cases/3?`} {
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
// the human, and falls back to the oldest once it is not; an event's card
// read does the same.
func TestBoardReadKeepsTheCaseInView(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/?case=t-a:2&case=t-b:4"); !strings.Contains(needs, `<span class="why">A card that waits this long may be stuck.</span>`) || !strings.Contains(needs, `2 of 3`) {
		t.Errorf("the board dropped the case in view:\n%s", needs)
	}
	if needs := needsOf(t, r, ck, "/?case=t-a:99"); !strings.Contains(needs, `<span class="why">A session that changes no code spends money and moves the work no closer to done.</span>`) {
		t.Errorf("a case no longer at the human kept its place:\n%s", needs)
	}
	frag := getAs(r, ck, "/cards/t-a?case=t-a:3", false).Body.String()
	if !strings.HasPrefix(frag, `<article class="card face need`) || !strings.Contains(frag, `<span class="why">Ask three.</span>`) || !strings.Contains(frag, ` data-place="case `) {
		t.Errorf("the card fragment is not the case in view with its place:\n%s", frag)
	}
}

// The suggested answer's press sits on the case that is the card's live
// ask, the newest live case an answer ends, and nowhere else. The board
// shows the asks the store holds at the human: an older ask the minute pass
// has not withdrawn yet is a page with no press, and so is an at-human ask
// while a newer live ask is still with the ladder. A post that names
// another case is refused with the live ask named.
func TestAnswerFormsSitOnTheLiveAsksCaseOnly(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	suggest := map[string]any{"preselected": map[string]any{"action": factoryd.ActClose}, "recommended_by": "the project agent", "recommendation": "the work is done elsewhere"}
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now(), Detail: suggest}, 7: {At: time.Now(), Detail: suggest}}
	r.openCases["t-a"][2].Facts = roomyFacts(nil)
	newer := factoryd.Case{ID: 7, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "ask seven", Facts: roomyFacts(nil),
		OpenedAt: time.Now().Add(-10 * time.Minute)}
	r.openCases["t-a"] = append(r.openCases["t-a"], newer, factoryd.Case{ID: 9, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseOpen, Problem: "still with the ladder"})
	ck, s := r.loggedIn(t)
	row := func(path string) string {
		needs := needsOf(t, r, ck, path)
		return needs[strings.Index(needs, `data-card="t-a"`):strings.Index(needs, `data-card="t-b"`)]
	}
	if a := row("/?case=t-a:7"); strings.Contains(a, `action="/tasks/t-a/resolve"`) || !strings.Contains(a, "Ask seven.") || !strings.Contains(a, "4 of 4") {
		t.Errorf("an at-human ask carries a press while the card's live ask is with the ladder:\n%s", a)
	}
	// the opened card says why a page's answers wait
	note := func(id string) string {
		ask := askBlockOf(t, drawer(t, r, ck, "t-a", "overview&case=t-a:"+id))
		if i := strings.Index(ask, `<p class="o-note">`); i >= 0 {
			return ask[i : i+strings.Index(ask[i:], "</p>")+4]
		}
		return ""
	}
	if got, want := note("7"), `<p class="o-note">A newer ask on this card waits on the factory first.</p>`; got != want {
		t.Errorf("an at-human ask behind a newer one with the factory notes %q, want %q", got, want)
	}
	r.openCases["t-a"] = r.openCases["t-a"][:4]
	if got, want := note("3"), `<p class="o-note">A newer ask on the card replaced this one.</p>`; got != want {
		t.Errorf("a replaced ask notes %q, want %q", got, want)
	}
	if got := note("7"); got != "" {
		t.Errorf("the live ask notes %q, want no note", got)
	}
	if a := row("/?case=t-a:7"); !strings.Contains(a, `action="/tasks/t-a/resolve"`) || !strings.Contains(a, "4 of 4") {
		t.Errorf("the live ask carries no press:\n%s", a)
	}
	if a := row("/?case=t-a:3"); !strings.Contains(a, "Ask three.") || strings.Contains(a, `action="/tasks/t-a/resolve"`) {
		t.Errorf("the older ask is no page, or carries a press:\n%s", a)
	}
	if frag := getAs(r, ck, caseURL("t-a", 7), true).Body.String(); !strings.Contains(frag, `<input type="hidden" name="case_id" value="7">`) {
		t.Errorf("the live ask's press does not name its case:\n%s", frag)
	}
	if frag := getAs(r, ck, caseURL("t-a", 3), true).Body.String(); strings.Contains(frag, `name="case_id" value="3"`) {
		t.Errorf("the older ask's page holds a press of its case:\n%s", frag)
	}
	rec := r.do(post("/tasks/t-a/resolve", url.Values{"csrf": {s.csrf}, "project": {casesProject}, "action": {factoryd.ActClose}, "case_id": {"3"}}), ck)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Case 3 is not the card's live ask: the card's live ask is case 7") {
		t.Errorf("an answer from another case = %d %q, want 409 naming case 7", rec.Code, rec.Body)
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
	if n := strings.Count(needsOf(t, r, ck, "/"), ` data-case="`); n != 10 {
		t.Fatalf("%d rows, want 10", n)
	}
	if digests != 0 || desks != 1 || steps != 0 {
		t.Errorf("a board read of 10 rows of 2 cases made %d digests, %d case-desk reads and %d step reads, want 0, 1 and 0", digests, desks, steps)
	}
}

// A card is counted once: a merge card with its case reads as many need
// you as the column holds, and a running card with a case at the human
// shows in Needs you alone. The merge's answer is the opened card's,
// once.
func TestACardWithACaseCountsOnceAndShowsOnce(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-mc", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10, BudgetEUR: 2},
		factoryd.Task{ID: "t-run", Kind: "agent", Status: "running", Repository: "sf/code", IssueNumber: 9, BudgetEUR: 2})
	ap := factoryd.ApprovalRequest{ID: 61, Kind: factoryd.ApprovalMerge, TaskID: "t-mc", Project: "sf/code",
		PRNumber: 10, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal","head_sha":"h1"}`)}
	r.approvals.rows = append(r.approvals.rows, ap)
	merge, err := factoryd.RequestCase(ap)
	if err != nil {
		t.Fatal(err)
	}
	merge.ID = 5
	r.openCases = map[string][]factoryd.Case{
		"t-mc":  {merge},
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
	if strings.Count(needs, `data-card="t-mc"`) != 1 || strings.Contains(needs, `action="/approvals/61/approve"`) {
		t.Errorf("the merge card is not one row, or its small card holds the approval's form:\n%s", needs)
	}
	if drawer := getAs(r, ck, "/cards/t-mc/drawer?tab=overview", false).Body.String(); strings.Count(drawer, `data-answer="approve_merge"`) != 1 ||
		strings.Count(drawer, `<h4>The pull request</h4>`) != 1 {
		t.Errorf("the opened card does not hold the merge's answer and its evidence once:\n%s", drawer)
	}
}

// A plan's confirmation case with extra promises waiting opens on the first
// of them on its small card; the plan's approval is the opened card's,
// once: the extra promise's answers in its ask and, in its full brief, the
// extra promises with their state. The small card holds no other raw text
// of the revision: not its summary, its spec, its criteria.
func TestAPlansCaseHoldsItsApprovalOnce(t *testing.T) {
	r := newRig(t)
	holdoutsRig(r, factoryd.PlanQuestion{Kind: factoryd.QuestionConfirmation})
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/?project=linkshort")
	if strings.Count(needs, ` data-case="`) != 1 || strings.Contains(needs, `class="card approval plan"`) {
		t.Fatalf("want one small card and no plan card in it:\n%s", needs)
	}
	if want := `<strong class="title">Keep this extra promise for plan “Link shortener”?</strong> <span class="why">The plan&#39;s cards start only once each extra promise is kept or dropped.</span>`; !strings.Contains(needs, want) {
		t.Errorf("the plan's small card lacks %s:\n%s", want, needs)
	}
	for _, raw := range []string{"The foundation lands first, with its migrations.", "On it, the links API and the acceptance tests close the round.",
		"A door opens only for a key that fits.", "key: the token that opens a door", "door_open (test)", "Still to plan"} {
		if strings.Contains(needs, raw) {
			t.Errorf("the plan's small card holds %q:\n%s", raw, needs)
		}
	}
	drawer := getAs(r, ck, "/cards/t-plan/drawer?tab=overview", false).Body.String()
	if strings.Count(drawer, `<h4>The plan</h4>`) != 1 {
		t.Fatalf("the opened card does not hold the plan's approval once:\n%s", drawer)
	}
	for _, want := range []string{`<p class="claim">The door stays shut until a key that fits turns the lock <span class="meta">waits for your answer</span></p>`,
		`action="/tasks/t-plan/plan/holdout"`} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the opened card lacks %q:\n%s", want, drawer)
		}
	}
}

// Every text the face reads is scrubbed: URL userinfo and a bearer value
// come out masked in the writer's story, the recommendation the ask
// carries and the ask's info, which the opened card shows.
func TestCaseFaceMasksEveryTextItReads(t *testing.T) {
	const url, bearer = "https://user:pass@git.example/x", "Authorization: Bearer abc123def456ghi789jkl012mno345"
	r := newRig(t)
	seatCases(r)
	r.openCases["t-a"][2].Brief = "It failed pulling " + url + ". It sent " + bearer + "."
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{
		"info":           "The log says " + url + " and " + bearer,
		"recommendation": "close: the fetch of " + url + " sent " + bearer, "recommended_by": "the factory agent",
		"preselected": map[string]any{"action": "close"}}}}
	ck, _ := r.loggedIn(t)
	drawer := getAs(r, ck, "/cards/t-a/drawer?tab=overview&case=t-a:3", false).Body.String()
	for _, want := range []string{"It failed pulling", "close: the fetch of", "The log says"} {
		if !strings.Contains(drawer, want) {
			t.Fatalf("the opened card lacks %q:\n%s", want, drawer)
		}
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(drawer, leak) || strings.Contains(needsOf(t, r, ck, "/"), leak) {
			t.Errorf("a face holds %q:\n%s", leak, drawer)
		}
	}
}

// The face's info is the ask's, which the opened card shows; the small
// card shows the row's one waiting time in its band and names no last
// answer.
func TestCaseFaceInfoIsTheAsksAndTheLifeLoadsOnDemand(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now().Add(-time.Hour), Detail: map[string]any{"info": "Postgres is shared."}}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	if !strings.Contains(needs, `<span class="since">waits for you, 1 h 0 min</span>`) || strings.Contains(needs, "Last answer processed") {
		t.Errorf("the small card's band lacks the wait, or names the last answer:\n%s", needs)
	}
	if drawer := getAs(r, ck, "/cards/t-a/drawer?tab=overview", false).Body.String(); !strings.Contains(drawer, "Postgres is shared.") {
		t.Errorf("the opened card lacks the ask's info:\n%s", drawer)
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
// replaces like with like; the arrows keep the board's filter, and the row
// they fetch keeps the row's place.
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
	next := getAs(r, ck, "/projects/"+casesProject+"/cards/t-a/cases/2?q=card", true).Body.String()
	if !strings.Contains(next, ` data-place="`+rp[1]+`"`) || !strings.Contains(next, `/cases/3?`) || !strings.Contains(next, `q=card`) {
		t.Errorf("the arrows' case fragment has not the row's place or loses the find:\n%s", next)
	}
}

// The press needs the case's comment to be the card's question's: a case
// asked on another comment carries none.
func TestAnswerFormsNeedTheQuestionsComment(t *testing.T) {
	r := newRig(t)
	r.tasks.tasks = []factoryd.BoardTask{{Task: factoryd.Task{ID: "t-a", Kind: "agent", Status: "waiting-human", Repository: "sf/code", IssueNumber: 21, Objective: "Card A"},
		Escalation: "the agent asks", QuestionComment: 42, Question: &factoryd.HumanQuestion{Ask: "Which?"}}}
	r.openCases = map[string][]factoryd.Case{"t-a": {{ID: 1, TaskID: "t-a", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman, Problem: "Which?", CommentID: 41,
		Facts: roomyFacts(nil)}}}
	r.caseAsks = map[int64]factoryd.CaseAsk{1: {At: time.Now(), Detail: map[string]any{"preselected": map[string]any{"action": factoryd.ActClose},
		"recommended_by": "the project agent", "recommendation": "the work is done elsewhere"}}}
	ck, _ := r.loggedIn(t)
	if needs := needsOf(t, r, ck, "/"); strings.Contains(needs, `action="/tasks/t-a/resolve"`) {
		t.Errorf("a case asked on comment 41 carries the press of the question on comment 42:\n%s", needs)
	}
	r.openCases["t-a"][0].CommentID = 42
	if needs := needsOf(t, r, ck, "/"); !strings.Contains(needs, `action="/tasks/t-a/resolve"`) {
		t.Errorf("the case asked on the question's comment carries no press:\n%s", needs)
	}
}

// A plan at its session cap has a session cap case and a pending plan
// approval that case does not answer: the card is one row, counted once,
// and the plan's approval is the opened card's, once.
func TestACardsApprovalWithNoCaseOfItsOwnSitsInItsRow(t *testing.T) {
	r := newRig(t)
	planRig(r)
	r.openCases = map[string][]factoryd.Case{"t-plan": {{ID: 5, TaskID: "t-plan", Kind: factoryd.CaseSessionCap, Status: factoryd.CaseAtHuman, Problem: "The plan hit its session cap."}}}
	ck, _ := r.loggedIn(t)
	body := getAs(r, ck, "/?project=linkshort", false).Body.String()
	needs := col(t, body, "Needs you")
	if strings.Count(needs, ` data-case="`) != 1 || strings.Contains(needs, `class="card approval plan"`) {
		t.Fatalf("want one row and no plan card in it:\n%s", needs)
	}
	if !regexp.MustCompile(`Needs you <span class="count">1</span>`).MatchString(body) || !regexp.MustCompile(`<b class="n">1</b> need you`).MatchString(body) {
		t.Errorf("the card is not counted once:\n%s", needs)
	}
	if drawer := getAs(r, ck, "/cards/t-plan/drawer?tab=overview", false).Body.String(); strings.Count(drawer, `<h4>The plan</h4>`) != 1 {
		t.Errorf("the opened card does not hold the plan's approval once:\n%s", drawer)
	}
}

// The row's waiting time is its shown case's, the oldest, once, in its
// band.
func TestACaseRowShowsOneWaitingTime(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-28 * time.Minute)}, 1: {At: time.Now().Add(-95 * time.Minute)}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/")
	row := needs[strings.Index(needs, `data-card="t-a"`):strings.Index(needs, `data-card="t-b"`)]
	if strings.Count(row, `class="since"`) != 1 || !strings.Contains(row, `<span class="since">waits for you, 1 h 35 min</span>`) {
		t.Errorf("t-a's row does not show the case's one waiting time, 1 h 35 min:\n%s", row)
	}
}

// The agent's question is scrubbed on the opened card, and the small
// card's suggestion names its writer: words no one signs show nowhere,
// whatever action they open with.
func TestCaseFaceScrubsTheQuestionAndNamesTheRecommendationsWriter(t *testing.T) {
	r := newRig(t)
	seatCases(r)
	r.openCases["t-a"][2].Kind = factoryd.CaseAgentEscalate
	r.openCases["t-a"][2].Facts = map[string]any{"reason": factoryd.AgentAsks + "the pull from https://user:pass@git.example/x failed: go on?"}
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now(), Detail: map[string]any{"recommendation": "wait", "recommended_by": "the check-in",
		"preselected": map[string]any{"action": "close"}}},
		4: {At: time.Now(), Detail: map[string]any{"recommendation": "close: no writer words"}}}
	ck, _ := r.loggedIn(t)
	needs := needsOf(t, r, ck, "/?case=t-a:3")
	for _, leak := range []string{"user:pass", "no writer words"} {
		if strings.Contains(needs, leak) {
			t.Errorf("the face holds %q:\n%s", leak, needs)
		}
	}
	rowA, rowB := needs[strings.Index(needs, `data-card="t-a"`):strings.Index(needs, `data-card="t-b"`)], needs[strings.Index(needs, `data-card="t-b"`):]
	if !strings.Contains(rowA, `<span class="by">check-in&#39;s pick</span>`) {
		t.Errorf("the face lacks its suggestion's writer:\n%s", rowA)
	}
	if drawer := getAs(r, ck, "/cards/t-a/drawer?tab=overview&case=t-a:3", false).Body.String(); !strings.Contains(drawer, "The pull from") || strings.Contains(drawer, "user:pass") {
		t.Errorf("the opened card lacks the agent's question, or holds it unmasked:\n%s", drawer)
	}
	if strings.Contains(rowB, `class="by"`) {
		t.Errorf("words no one signs suggest an answer:\n%s", rowB)
	}
	if drawer := getAs(r, ck, "/cards/t-b/drawer?tab=overview", false).Body.String(); strings.Contains(drawer, "no writer words") {
		t.Errorf("the opened card shows words no one signs:\n%s", drawer)
	}
}

// The opened card shows the case writer's two lines in its story, masked,
// beside the numbers the card composes, and the ask's info beside them.
func TestCaseFaceShowsTheBriefMasked(t *testing.T) {
	const url, bearer = "https://user:pass@git.example/x", "Authorization: Bearer abc123def456ghi789jkl012mno345"
	r := newRig(t)
	seatCases(r)
	r.openCases["t-a"][2].Brief = "The fetch of " + url + " sent " + bearer + "."
	r.openCases["t-a"][2].BriefDetail = "Another session fixes both findings."
	r.caseAsks = map[int64]factoryd.CaseAsk{3: {At: time.Now().Add(-time.Hour), Detail: map[string]any{"info": "the digest's info"}}}
	ck, _ := r.loggedIn(t)
	drawer := getAs(r, ck, "/cards/t-a/drawer?tab=overview&case=t-a:3", false).Body.String()
	for _, want := range []string{"<li>No session has run on this card. The fetch of", "<li>Another session fixes both findings. All of its work is still to do.</li>", "<p>the digest's info</p>"} {
		if !strings.Contains(drawer, want) {
			t.Errorf("the opened card lacks %q:\n%s", want, drawer)
		}
	}
	for _, leak := range []string{"user:pass", "abc123def456"} {
		if strings.Contains(drawer, leak) {
			t.Errorf("the opened card holds %q:\n%s", leak, drawer)
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
	needs := needsOf(t, r, ck, "/")
	if face := faceOf(t, needs, "t-done"); strings.Count(needs, `data-card="t-done"`) != 1 || !strings.Contains(face, `<strong class="title">Merge this card&#39;s pull request?</strong>`) || strings.Contains(face, `data-case=`) {
		t.Errorf("the ended card's pending approval is not on the board alone:\n%s", needs)
	}
}

// An ended card whose issue stayed open stands in Needs you with the
// flag's ask, and its opened card offers the flag's three answers.
func TestAnEndedCardsOpenIssueStandsInNeedsYou(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-end", Kind: "agent", Status: "failed", Repository: "sf/code", IssueNumber: 8, BudgetEUR: 2})
	r.openCases = map[string][]factoryd.Case{"t-end": {{ID: 6, TaskID: "t-end", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman,
		StateKey: "issue_open_after_end|", Facts: map[string]any{"flag": "issue_open_after_end"}}}}
	ck, _ := r.loggedIn(t)
	if face := faceOf(t, needsOf(t, r, ck, "/"), "t-end"); !strings.Contains(face, `<strong class="title">This card ended, and its issue is still open. Close the issue?</strong>`) {
		t.Errorf("the ended card's open issue does not stand in Needs you with its ask:\n%s", face)
	}
	opened := getPage(r, ck, "/cards/t-end/drawer?tab=overview").Body.String()
	for _, key := range []string{factoryd.ActClose + "/issue", factoryd.ActSettle + "/open", factoryd.ActNote} {
		if !strings.Contains(opened, `action="/tasks/t-end/resolve" hx-post="/tasks/t-end/resolve" class="answer" data-answer="`+key+`"`) {
			t.Errorf("the ended card's opened card offers no %s:\n%s", key, opened)
		}
	}
}

// A card whose cases are a merge approval and a monitor's flag shows the
// one in view, the merge first, oldest, the flag under its own number.
func TestAnApprovalShowsOnlyUnderItsOwnCase(t *testing.T) {
	r := newRig(t)
	r.tasks.set("", factoryd.Task{ID: "t-mc", Kind: "agent", Status: "in-review", Repository: "sf/code", IssueNumber: 8, PRNumber: 10, BudgetEUR: 2})
	r.approvals.rows = append(r.approvals.rows, factoryd.ApprovalRequest{ID: 61, Kind: factoryd.ApprovalMerge, TaskID: "t-mc", Project: "sf/code",
		PRNumber: 10, Status: "pending", Spec: json.RawMessage(`{"risk_class":"normal"}`)})
	r.openCases = map[string][]factoryd.Case{"t-mc": {
		{ID: 5, TaskID: "t-mc", Kind: factoryd.CaseMergeApproval, Status: factoryd.CaseAtHuman, Problem: "merge?", Facts: map[string]any{"approval": 61.0}},
		{ID: 6, TaskID: "t-mc", Kind: factoryd.CaseMonitorFlag, Status: factoryd.CaseAtHuman, Problem: "flag"}}}
	ck, _ := r.loggedIn(t)
	if face := faceOf(t, needsOf(t, r, ck, "/?case=t-mc:6"), "t-mc"); strings.Contains(face, "Merge this card") || !strings.Contains(face, `<strong class="title">The factory flagged something odd on this card. Is that fine?</strong>`) {
		t.Errorf("the merge's ask shows under the flag's case:\n%s", face)
	}
	if face := faceOf(t, needsOf(t, r, ck, "/"), "t-mc"); !strings.Contains(face, `<strong class="title">Merge this card&#39;s pull request?</strong>`) {
		t.Errorf("the merge's ask does not show under its own case:\n%s", face)
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

// A card that holds a case and a pending resource request asks both as
// pages of one carousel, the request first as it leads the card's asks, on
// the board and on the opened card alike: the small card and the opened
// card open on the same page and count 2 of them, and the next page asks
// the case.
func TestACasesCardAsksItsResourceRequestAsAPage(t *testing.T) {
	r := newRig(t)
	withRequestCards(r)
	r.openCases = map[string][]factoryd.Case{"t-res": {{ID: 5, TaskID: "t-res", Kind: factoryd.CaseOther, Status: factoryd.CaseAtHuman,
		Problem: "the load test needs a decision", Facts: roomyFacts(nil)}}}
	r.tasks.mu.Lock()
	for i := range r.tasks.tasks {
		if r.tasks.tasks[i].ID == "t-res" {
			r.tasks.tasks[i].Status = "waiting-human"
		}
	}
	r.tasks.mu.Unlock()
	ck, _ := r.loggedIn(t)
	const caseAsk, serverAsk = "The card stopped on a problem it cannot fix by itself. Run one more session, or send it back to the planner?",
		"Rent a cx33 server for 5 hours at about 0.07 EUR?"
	small := faceOf(t, r.do(httptest.NewRequest(http.MethodGet, "/", nil), ck).Body.String(), "t-res")
	if !strings.Contains(small, serverAsk) || !strings.Contains(small, "1 of 2") || !strings.Contains(small, "/cards/t-res/cases/5?") {
		t.Errorf("the small card does not open on the request with the case as its next page:\n%s", small)
	}
	ask := askBlockOf(t, drawer(t, r, ck, "t-res", "overview"))
	for _, want := range []string{`<p class="o-question">` + serverAsk + `</p>`, "Your call · 1 of 2", "case=t-res:5", `action="/approvals/3/approve"`, `action="/approvals/3/deny"`} {
		if !strings.Contains(ask, want) {
			t.Errorf("the opened card's first page lacks %s:\n%s", want, ask)
		}
	}
	ask = askBlockOf(t, drawer(t, r, ck, "t-res", "overview&case=t-res:5"))
	if !strings.Contains(ask, `<p class="o-question">`+strings.Replace(caseAsk, " session,", ` <span class="term">session</span>,`, 1)+`</p>`) ||
		!strings.Contains(ask, "Your call · 2 of 2") || !strings.Contains(ask, "case=t-res:-3") {
		t.Errorf("the case's page:\n%s", ask)
	}
	if frag := getAs(r, ck, "/projects/"+casesProject+"/cards/t-res/cases/-3", true); frag.Code != http.StatusOK || !strings.Contains(frag.Body.String(), serverAsk) {
		t.Errorf("the request's page as a fragment = %d:\n%s", frag.Code, frag.Body)
	}
}

// A case names its request by the request's id however its facts decode
// the number: a resource request's case as its merge and criteria
// siblings do, so an id of a million matches as it reads.
func TestACaseNamesItsRequestByItsID(t *testing.T) {
	q := factoryd.ApprovalRequest{ID: 1000000, Kind: factoryd.ApprovalResource}
	for _, id := range []any{int64(1000000), float64(1000000)} {
		if !approvalOf(q, factoryd.Case{Kind: factoryd.CaseKind(factoryd.ApprovalResource), Facts: map[string]any{"approval": id}}) {
			t.Errorf("a resource case of approval %v does not name request 1000000", id)
		}
	}
	if approvalOf(q, factoryd.Case{Kind: factoryd.CaseKind(factoryd.ApprovalResource), Facts: map[string]any{"approval": float64(7)}}) {
		t.Error("a resource case of approval 7 names request 1000000")
	}
}
